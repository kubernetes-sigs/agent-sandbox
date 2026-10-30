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
import functools
import logging
import shlex
import threading
import time
import uuid
from dataclasses import dataclass
from typing import Any, Callable

from harbor.environments.base import ExecResult
from kubernetes import client
from kubernetes.client.rest import ApiException
from kubernetes.stream import stream

# A command that outlives its in-sandbox ``timeout`` by this much is cut from
# the client side too (the sandbox-side timeout is the normal path).
_LOCAL_TIMEOUT_GRACE_S = 15
_UPLOAD_SETTLE_S = 30
_TRUNCATED_MARK = "\n[output truncated by k8s-agent-sandbox-harbor]\n"

# Indirection so tests can drive the deadlines without touching time.monotonic,
# which the event loop uses too.
_now = time.monotonic


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


@dataclass
class _Streams:
    stdout: str = ""
    stderr: str = ""


class PodExecTransport:
    """Exec and tar transfer for one pod container."""

    def __init__(self, *, api_factory: Callable[[], client.CoreV1Api], pod: str, namespace: str,
                 container: str, executor: concurrent.futures.Executor,
                 logger: logging.Logger | None = None, max_output_bytes: int = 0):
        self._api_factory = api_factory
        self.pod = pod
        self.namespace = namespace
        self.container = container
        self._executor = executor
        self._logger = logger or logging.getLogger(__name__)
        self._max_output = max_output_bytes
        self.exec_count = 0

    async def _run(self, fn: Callable[..., Any], *args: Any) -> Any:
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(self._executor, functools.partial(fn, *args))

    def _open(self, command: list[str], *, stdin: bool, binary: bool) -> Any:
        api = self._api_factory()
        return stream(api.connect_get_namespaced_pod_exec, self.pod, self.namespace,
                      command=command, container=self.container, stderr=True, stdin=stdin,
                      stdout=True, tty=False, _preload_content=False, binary=binary)

    def _cap(self, text: str) -> str:
        if self._max_output and len(text) > self._max_output:
            return text[: self._max_output] + _TRUNCATED_MARK
        return text

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
        while True:
            try:
                await self._run(self._probe)
                return
            except ApiException as exc:
                if exc.status == 404:
                    raise self._translate(exc) from exc
                last = f"{exc.status} {exc.reason}"
            except Exception as exc:  # noqa: BLE001 - retried until the deadline
                last = repr(exc)
            attempt += 1
            if _now() >= deadline:
                raise TimeoutError(f"container {self.container} of pod {self.pod} did not accept "
                                   f"exec within {timeout_s:.0f}s: {last}")
            if attempt % 10 == 1:
                self._logger.debug("exec not ready on %s (%s); retrying", self.pod, last)
            await asyncio.sleep(2)

    # --- exec -----------------------------------------------------------------
    def _exec_blocking(self, command: list[str], holder: dict[str, Any]) -> tuple[str, str, int]:
        resp = self._open(command, stdin=False, binary=False)
        holder["resp"] = resp
        out = _Streams()
        try:
            while resp.is_open():
                resp.update(timeout=1)
                if resp.peek_stdout():
                    out.stdout += resp.read_stdout()
                if resp.peek_stderr():
                    out.stderr += resp.read_stderr()
        finally:
            resp.close()
        return out.stdout, out.stderr, self._return_code(resp)

    async def exec(self, script: str, *, cwd: str | None = None, env: dict[str, str] | None = None,
                   user: str | int | None = None, timeout_sec: int | None = None) -> ExecResult:
        command = build_command(script, cwd=cwd, env=env, user=user, timeout_sec=timeout_sec)
        holder: dict[str, Any] = {}
        self.exec_count += 1
        deadline = (timeout_sec + _LOCAL_TIMEOUT_GRACE_S) if timeout_sec else None
        try:
            stdout, stderr, code = await asyncio.wait_for(
                self._run(self._exec_blocking, command, holder), timeout=deadline)
        except asyncio.TimeoutError:
            resp = holder.get("resp")
            if resp is not None:
                await self._run(resp.close)
            return ExecResult(stdout=None, stderr=f"Command timed out after {timeout_sec} seconds",
                              return_code=124)
        except ApiException as exc:
            raise self._translate(exc) from exc
        return ExecResult(stdout=self._cap(stdout), stderr=self._cap(stderr), return_code=code)

    # --- files ----------------------------------------------------------------
    def _upload_blocking(self, data: bytes, target_dir: str) -> str:
        command = ["sh", "-c", f"mkdir -p {shlex.quote(target_dir)} && tar xf - -C {shlex.quote(target_dir)}"]
        resp = self._open(command, stdin=True, binary=True)
        err = b""
        try:
            view = memoryview(data)
            for start in range(0, len(view), 1 << 20):
                resp.write_stdin(bytes(view[start:start + (1 << 20)]))
            # The websocket has no stdin EOF; tar gets it when the stream closes.
            resp.run_forever(timeout=1)
            if resp.peek_stderr():
                err = resp.read_stderr()
        finally:
            resp.close()
        return err.decode("utf-8", "replace") if isinstance(err, bytes) else str(err)

    async def upload_tar(self, data: bytes, target_dir: str) -> None:
        """Extract a tar stream under ``target_dir`` and confirm it landed.

        The exec websocket cannot half-close stdin, so ``tar`` sees EOF only
        when the stream closes and its exit status is lost. A marker member
        appended to the archive proves the extraction completed.
        """
        marker = f".k8s-agent-sandbox-harbor-{uuid.uuid4().hex}"
        import io
        import tarfile
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as tar:
            with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as src:
                for member in src:
                    tar.addfile(member, src.extractfile(member) if member.isfile() else None)
            info = tarfile.TarInfo(marker)
            info.size = 0
            tar.addfile(info)
        try:
            stderr = await self._run(self._upload_blocking, buf.getvalue(), target_dir)
        except ApiException as exc:
            raise self._translate(exc) from exc
        marker_path = f"{target_dir.rstrip('/')}/{marker}"
        deadline = _now() + _UPLOAD_SETTLE_S
        while True:
            result = await self.exec(f"test -e {shlex.quote(marker_path)} && rm -f {shlex.quote(marker_path)}")
            if result.return_code == 0:
                return
            if _now() >= deadline:
                raise RuntimeError(f"upload to {target_dir} on pod {self.pod} did not complete"
                                   + (f": {stderr.strip()}" if stderr.strip() else ""))
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
