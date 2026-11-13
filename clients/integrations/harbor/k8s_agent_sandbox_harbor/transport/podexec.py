# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Exec and tar transfer through the Kubernetes pod exec API.

The image needs no agent: the kubelet runs ``sh -c`` in the ``main`` container
over a websocket, as ``kubectl exec`` does. The websocket client is blocking,
so every call runs on a thread of a pool shared by the process, and each
thread holds its own API client because the client is not thread-safe.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import contextlib
import functools
import logging
import shlex
import threading
import time
import uuid
from typing import Any, AsyncIterator, Callable

import websocket
from harbor.environments.base import ExecResult
from kubernetes import client
from kubernetes.client.rest import ApiException
from kubernetes.stream import stream, ws_client

_IgnoredIO = getattr(ws_client, "_IgnoredIO", None)

# A command that outlives its in-sandbox ``timeout`` by this much is cut from
# the client side too (the sandbox-side timeout is the normal path).
_LOCAL_TIMEOUT_GRACE_S = 15
_UPLOAD_SETTLE_S = 30
_TRUNCATED_MARK = "\n[output truncated by k8s-agent-sandbox-harbor]\n"
# The kubernetes client opens the exec socket with ``WebSocket.connect()`` and
# passes no timeout, and that path ignores websocket-client's global default
# (only ``create_connection`` reads it), so the open blocks for good when the
# API server does not answer. A stalled open cannot be cancelled from the event
# loop and holds an executor thread, so ``connect`` gets a default deadline.
_CONNECT_TIMEOUT_S = 60

# Indirection so tests can drive the deadlines without touching time.monotonic,
# which the event loop uses too.
_now = time.monotonic


_connect_wrapped = False
# Set on a thread while this transport opens a stream, so the default connect
# timeout applies to its sockets only and not to another user of
# websocket-client in the same process.
_connect_scope = threading.local()


def ensure_connect_timeout(seconds: float = _CONNECT_TIMEOUT_S) -> None:
    """Give ``websocket.WebSocket.connect`` a default socket timeout.

    Installed once per process, and applied only to a socket this transport
    opens (see ``_connect_scope``). A caller that passes its own ``timeout``
    to ``connect`` or set one on the socket keeps it; only the "no timeout at
    all" case changes, and only to a deadline. The timeout also bounds a
    blocked read, which the exec loop never does: it reads after ``select``
    reports data.
    """
    global _connect_wrapped
    if _connect_wrapped:
        return
    original = websocket.WebSocket.connect

    @functools.wraps(original)
    def connect(self: Any, url: str, **options: Any) -> Any:
        if (getattr(_connect_scope, "active", False) and "timeout" not in options
                and getattr(self.sock_opt, "timeout", None) is None):
            options["timeout"] = seconds
        return original(self, url, **options)

    websocket.WebSocket.connect = connect  # type: ignore[method-assign]
    _connect_wrapped = True


class ExecStreamClosedError(RuntimeError):
    """The exec stream ended without an exit status; the pod is likely gone."""


class PodGoneError(RuntimeError):
    """The pod of this sandbox no longer exists."""


def build_command(script: str, *, cwd: str | None = None, env: dict[str, str] | None = None,
                  user: str | int | None = None, timeout_sec: int | None = None) -> list[str]:
    """The argv for one Harbor exec, mirroring Harbor's own Kubernetes provider.

    ``script`` runs under ``bash -c``; ``env`` is prefixed as ``K=V`` words;
    ``cwd`` becomes ``cd <cwd> &&``; ``user`` wraps the whole thing in ``su``
    (``su`` without ``-`` keeps the working directory; a numeric user is
    resolved through ``getent``); ``timeout_sec`` wraps it in coreutils
    ``timeout`` when the image has it, so the process is killed in the sandbox
    and exits 124.
    """
    full = f"bash -c {shlex.quote(script)}"
    if env:
        prefix = " ".join(f"{key}={shlex.quote(str(value))}" for key, value in env.items())
        full = f"{prefix} {full}"
    if cwd:
        full = f"cd {shlex.quote(cwd)} && {full}"
    if user is not None:
        user_arg = (f"$(getent passwd {int(user)} | cut -d: -f1)" if isinstance(user, int)
                    else shlex.quote(str(user)))
        full = f"su {user_arg} -s /bin/bash -c {shlex.quote(full)}"
    if timeout_sec:
        quoted = shlex.quote(full)
        full = (f"if command -v timeout >/dev/null 2>&1; then exec timeout -k 5 {int(timeout_sec)} "
                f"sh -c {quoted}; else exec sh -c {quoted}; fi")
    return ["sh", "-c", full]


def upload_command(target_dir: str, marker_path: str) -> str:
    """The shell command an upload runs: extract stdin under ``target_dir``,
    then create ``marker_path`` only when ``tar`` exited 0."""
    target = shlex.quote(target_dir)
    return f"mkdir -p {target} && tar xf - -C {target} && : > {shlex.quote(marker_path)}"


class _Output:
    """One output stream of a command, kept up to a cap.

    Chunks past the cap are dropped while the stream is still drained, so the
    command does not block on a full pipe and memory stays near the cap. The
    chunks are joined once at the end; appending to a string per frame is
    quadratic for a large output.
    """

    def __init__(self, cap: int):
        self._cap = cap
        self._parts: list[str] = []
        self._size = 0

    def add(self, chunk: str) -> None:
        if self._cap and self._size > self._cap:
            return
        self._parts.append(chunk)
        self._size += len(chunk)

    def text(self) -> str:
        text = "".join(self._parts)
        if self._cap and len(text) > self._cap:
            return text[: self._cap] + _TRUNCATED_MARK
        return text


class PodExecTransport:
    """Exec and tar transfer for one pod container."""

    def __init__(self, *, api_factory: Callable[[], client.CoreV1Api], pod: str, namespace: str,
                 container: str, executor: concurrent.futures.Executor,
                 logger: logging.Logger | None = None, max_output_bytes: int = 0,
                 slots: asyncio.Semaphore | None = None):
        self._api_factory = api_factory
        self.pod = pod
        self.namespace = namespace
        self.container = container
        self._executor = executor
        ensure_connect_timeout()
        # One slot per executor thread. A call waits for a slot before its
        # timeout starts, so a queued command is never reported as timed out.
        self._slots = slots
        self._logger = logger or logging.getLogger(__name__)
        self._max_output = max_output_bytes
        self.exec_count = 0

    async def _run(self, fn: Callable[..., Any], *args: Any) -> Any:
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(self._executor, functools.partial(fn, *args))

    @contextlib.asynccontextmanager
    async def _slot(self) -> AsyncIterator[None]:
        if self._slots is None:
            yield
            return
        async with self._slots:
            yield

    def _open(self, command: list[str], *, stdin: bool, binary: bool) -> Any:
        api = self._api_factory()
        _connect_scope.active = True
        try:
            resp = stream(api.connect_get_namespaced_pod_exec, self.pod, self.namespace,
                          command=command, container=self.container, stderr=True, stdin=stdin,
                          stdout=True, tty=False, _preload_content=False, binary=binary)
        finally:
            _connect_scope.active = False
        # The client keeps a second copy of every frame for ``read_all``, which
        # this transport never calls, and ``stream`` has no option to turn that
        # off. Without this a large output or download is held twice.
        if _IgnoredIO is not None and hasattr(resp, "_all"):
            resp._all = _IgnoredIO()
        # Newer clients make one ``update()`` consume every frame that is
        # already available and append each to a string. A command that writes
        # faster than the frames are read then never returns from ``update()``:
        # the output cap cannot apply and the append is quadratic. Every loop
        # here reads the channels after each ``update()`` until the stream
        # closes, so one frame per call loses nothing.
        if hasattr(resp, "_frames_immediately_available"):
            resp._frames_immediately_available = lambda: False
        return resp

    @staticmethod
    def _return_code(resp: Any) -> int:
        try:
            code = resp.returncode
        except Exception as exc:  # noqa: BLE001 - the client raises on a missing status
            raise ExecStreamClosedError("exec stream closed without an exit status") from exc
        if code is None:
            raise ExecStreamClosedError("exec stream closed before the command exited")
        return int(code)

    def _translate(self, exc: ApiException) -> Exception:
        if exc.status == 404:
            return PodGoneError(f"pod {self.pod} in {self.namespace} no longer exists")
        return exc

    # --- readiness ------------------------------------------------------------
    def _probe(self) -> None:
        resp = self._open(["true"], stdin=False, binary=False)
        resp.run_forever(timeout=10)
        resp.close()

    async def wait_ready(self, timeout_s: float) -> None:
        """Wait until the container accepts exec (it can lag the pod's Ready)."""
        deadline = _now() + timeout_s
        attempt = 0
        last = "no exec slot was free"

        def expired() -> TimeoutError:
            return TimeoutError(f"container {self.container} of pod {self.pod} did not accept "
                                f"exec within {timeout_s:.0f}s: {last}")

        while True:
            # The probe takes a slot like any other call, so a burst of
            # starting trials cannot fill the pool and starve an exec whose
            # timeout is already running. The wait for the slot counts against
            # the deadline; the probe itself is not cut short, because its
            # thread would keep running after the slot was released.
            remaining = deadline - _now()
            if remaining <= 0:
                raise expired()
            if self._slots is not None:
                try:
                    await asyncio.wait_for(self._slots.acquire(), timeout=remaining)
                except asyncio.TimeoutError:
                    raise expired() from None
                # The slot can come free just before the deadline and this
                # task resume after it; a probe must not start past the budget.
                if _now() >= deadline:
                    self._slots.release()
                    raise expired()
            try:
                await self._run(self._probe)
                return
            except ApiException as exc:
                if exc.status == 404:
                    raise self._translate(exc) from exc
                last = f"{exc.status} {exc.reason}"
            except Exception as exc:  # noqa: BLE001 - retried until the deadline
                last = repr(exc)
            finally:
                if self._slots is not None:
                    self._slots.release()
            attempt += 1
            if _now() >= deadline:
                raise expired()
            if attempt % 10 == 1:
                self._logger.debug("exec not ready on %s (%s); retrying", self.pod, last)
            await asyncio.sleep(2)

    # --- exec -----------------------------------------------------------------
    def _exec_blocking(self, command: list[str], holder: dict[str, Any]) -> tuple[str, str, int]:
        resp = self._open(command, stdin=False, binary=False)
        holder["resp"] = resp
        abandoned: threading.Event = holder["abandoned"]
        stdout, stderr = _Output(self._max_output), _Output(self._max_output)
        try:
            # The caller sets ``abandoned`` when it stops waiting (timeout or
            # cancellation), so this thread returns to the pool within one
            # update interval even when the stream itself stays open.
            while resp.is_open() and not abandoned.is_set():
                resp.update(timeout=1)
                if resp.peek_stdout():
                    stdout.add(resp.read_stdout())
                if resp.peek_stderr():
                    stderr.add(resp.read_stderr())
        finally:
            resp.close()
        return stdout.text(), stderr.text(), self._return_code(resp)

    def _abandon(self, holder: dict[str, Any]) -> asyncio.Future[None]:
        """Stop the blocking call behind ``holder`` and close its stream.

        The close runs on the default executor, not the exec pool: the
        abandoned call still holds one of the pool's threads, and when every
        thread is held by a hung stream the close is the only thing that frees
        them. A close error is dropped so it cannot replace the caller's result.
        """
        holder["abandoned"].set()
        resp = holder.get("resp")

        def close() -> None:
            if resp is not None:
                with contextlib.suppress(Exception):
                    resp.close()

        return asyncio.get_running_loop().run_in_executor(None, close)

    async def exec(self, script: str, *, cwd: str | None = None, env: dict[str, str] | None = None,
                   user: str | int | None = None, timeout_sec: int | None = None) -> ExecResult:
        command = build_command(script, cwd=cwd, env=env, user=user, timeout_sec=timeout_sec)
        holder: dict[str, Any] = {"abandoned": threading.Event()}
        self.exec_count += 1
        deadline = (timeout_sec + _LOCAL_TIMEOUT_GRACE_S) if timeout_sec else None
        async with self._slot():
            try:
                stdout, stderr, code = await asyncio.wait_for(
                    self._run(self._exec_blocking, command, holder), timeout=deadline)
            except asyncio.TimeoutError:
                await self._abandon(holder)
                return ExecResult(stdout=None,
                                  stderr=f"Command timed out after {timeout_sec} seconds",
                                  return_code=124)
            except asyncio.CancelledError:
                # Harbor cancels a trial's exec when the trial times out or is
                # stopped. Without this the thread stays in the stream until
                # the command ends, while the slot is already free, and the
                # next call's timeout runs while it waits for a thread. The
                # close is not awaited: a cancelled task must not wait here.
                self._abandon(holder)
                raise
            except ApiException as exc:
                raise self._translate(exc) from exc
        return ExecResult(stdout=stdout, stderr=stderr, return_code=code)

    # --- files ----------------------------------------------------------------
    def _upload_blocking(self, data: bytes, target_dir: str,
                         marker_path: str) -> tuple[str, int | None]:
        """Stream ``data`` into ``tar``; return its stderr and, when the command
        ended while the stream was open, its exit status."""
        resp = self._open(["sh", "-c", upload_command(target_dir, marker_path)],
                          stdin=True, binary=True)
        err = b""
        code: int | None = None
        try:
            view = memoryview(data)
            for start in range(0, len(view), 1 << 20):
                resp.write_stdin(bytes(view[start:start + (1 << 20)]))
            # The websocket has no stdin EOF. A tar that stops at the archive's
            # end blocks exits here and reports its status; one that waits for
            # EOF gets it when the stream closes, and the marker tells.
            resp.run_forever(timeout=1)
            if resp.peek_stderr():
                err = resp.read_stderr()
            if not resp.is_open():
                with contextlib.suppress(Exception):
                    code = resp.returncode
        finally:
            resp.close()
        return (err.decode("utf-8", "replace") if isinstance(err, bytes) else str(err)), code

    async def upload_tar(self, data: bytes, target_dir: str) -> None:
        """Extract a tar stream under ``target_dir`` and confirm it landed.

        The exec websocket cannot half-close stdin, so the exit status of
        ``tar`` can be lost. The remote shell creates a marker file only after
        ``tar`` exits 0, and this method waits for that file. The marker is not
        an archive member, because ``tar`` extracts later members after an
        earlier one fails.
        """
        marker_path = f"{target_dir.rstrip('/')}/.k8s-agent-sandbox-harbor-{uuid.uuid4().hex}"
        try:
            async with self._slot():
                stderr, code = await self._run(self._upload_blocking, data, target_dir, marker_path)
        except ApiException as exc:
            raise self._translate(exc) from exc
        detail = f": {stderr.strip()}" if stderr.strip() else ""
        if code:
            raise RuntimeError(f"upload to {target_dir} on pod {self.pod} failed "
                               f"(exit {code}){detail}")
        deadline = _now() + _UPLOAD_SETTLE_S
        while True:
            result = await self.exec(f"test -e {shlex.quote(marker_path)} && rm -f {shlex.quote(marker_path)}")
            if result.return_code == 0:
                return
            if _now() >= deadline:
                raise RuntimeError(f"upload to {target_dir} on pod {self.pod} did not complete{detail}")
            await asyncio.sleep(0.5)

    def _download_blocking(self, source_dir: str, single: str | None) -> tuple[bytes, str]:
        what = shlex.quote(single) if single else "."
        command = ["sh", "-c", f"cd {shlex.quote(source_dir)} && tar cf - {what}"]
        resp = self._open(command, stdin=False, binary=True)
        chunks: list[bytes] = []
        err = b""
        try:
            while resp.is_open():
                resp.update(timeout=1)
                if resp.peek_stdout():
                    chunk = resp.read_stdout()
                    chunks.append(chunk if isinstance(chunk, bytes) else chunk.encode("utf-8", "surrogateescape"))
                if resp.peek_stderr():
                    e = resp.read_stderr()
                    err += e if isinstance(e, bytes) else e.encode("utf-8", "replace")
        finally:
            resp.close()
        code = self._return_code(resp)
        text = err.decode("utf-8", "replace")
        if code != 0:
            what = f"{source_dir}{'/' + single if single else ''}"
            if "No such file or directory" in text or "can't cd" in text or "cannot cd" in text:
                # Harbor collects some paths best-effort; give it the error it
                # expects instead of a generic failure.
                raise FileNotFoundError(f"{what} does not exist in pod {self.pod}: {text.strip()}")
            raise RuntimeError(f"tar of {what} on pod {self.pod} failed ({code}): {text.strip()}")
        return b"".join(chunks), text

    async def download_tar(self, source_dir: str, single: str | None = None) -> bytes:
        """A tar stream of ``source_dir`` (or of one entry ``single`` inside it)."""
        try:
            async with self._slot():
                data, _ = await self._run(self._download_blocking, source_dir, single)
        except ApiException as exc:
            raise self._translate(exc) from exc
        return data


class ThreadLocalCoreV1:
    """A ``CoreV1Api`` per thread over one shared configuration."""

    def __init__(self, configuration: client.Configuration):
        self._configuration = configuration
        self._local = threading.local()

    def __call__(self) -> client.CoreV1Api:
        api = getattr(self._local, "api", None)
        if api is None:
            api = client.CoreV1Api(client.ApiClient(self._configuration))
            self._local.api = api
        return api
