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

"""Transport seam between the SDK session and a running Kubernetes sandbox.

Everything the session needs from the pod is expressed as three operations. Today the
only implementation talks to the in-pod HTTP server that ``k8s-agent-sandbox`` targets
(``POST execute``, ``POST upload``, ``GET download/{path}``). When the ``sandboxd`` gRPC
``ProcessService`` lands (streaming stdout/stderr, ``WriteStdin``, ``ResizeTTY``), it can
be dropped in here without touching :mod:`openai_agents_k8s_sandbox.session`.
"""

from __future__ import annotations

import asyncio
import base64
import logging
import math
import shlex
import uuid
from collections.abc import Sequence
from typing import Any, Protocol, runtime_checkable

from agents.sandbox.errors import ExecTimeoutError, ExecTransportError
from agents.sandbox.types import ExecResult

from .options import FileTransfer

try:  # httpx ships with k8s-agent-sandbox[async]; keep the import soft anyway.
    import httpx
except ImportError:  # pragma: no cover - exercised only in stripped installs
    httpx = None  # type: ignore[assignment]

logger = logging.getLogger(__name__)

# Base64 payloads are pushed through the JSON exec endpoint one chunk per command.
# 32 KiB of base64 keeps each rendered command well under a typical ARG_MAX.
_EXEC_CHUNK_CHARS = 32 * 1024


@runtime_checkable
class SandboxTransport(Protocol):
    """The pod-facing operations the session depends on."""

    async def exec_command(
        self, argv: Sequence[str], *, timeout: float | None = None
    ) -> ExecResult: ...

    async def read_file(self, path: str) -> bytes: ...

    async def write_file(self, path: str, data: bytes) -> None: ...

    async def is_ready(self) -> bool: ...


class K8sHttpTransport:
    """Talks to a sandbox pod through the ``k8s-agent-sandbox`` async client."""

    def __init__(
        self,
        sandbox: Any,
        *,
        file_transfer: FileTransfer = "http",
        default_timeout_s: float = 300.0,
    ) -> None:
        self._sandbox = sandbox
        self._file_transfer: FileTransfer = file_transfer
        self._default_timeout_s = default_timeout_s

    @property
    def file_transfer(self) -> FileTransfer:
        return self._file_transfer

    async def exec_command(
        self, argv: Sequence[str], *, timeout: float | None = None
    ) -> ExecResult:
        # The SDK hands us an argv list that it has already shell-wrapped
        # (``sh -lc "..."``). The in-pod API only accepts a command string, so join it
        # back with shell quoting: the server's own shell reconstructs the same argv.
        return await self._run(shlex.join(str(part) for part in argv), argv, timeout=timeout)

    async def read_file(self, path: str) -> bytes:
        if self._file_transfer == "exec":
            return await self._read_file_via_exec(path)

        try:
            data = await self._sandbox.files.read(path, allow_unsafe_paths=True)
        except Exception as e:
            raise ExecTransportError(
                command=["download", path],
                message="sandbox file download failed",
                cause=e,
            ) from e
        return bytes(data)

    async def write_file(self, path: str, data: bytes) -> None:
        if self._file_transfer == "exec":
            await self._write_file_via_exec(path, data)
            return

        try:
            await self._sandbox.files.write(path, data, allow_unsafe_paths=True)
        except Exception as e:
            raise ExecTransportError(
                command=["upload", path],
                message="sandbox file upload failed",
                cause=e,
            ) from e

    async def is_ready(self) -> bool:
        try:
            status, _message = await self._sandbox.status()
        except Exception:
            return False
        return status == "SandboxReady"

    # -- internals ---------------------------------------------------------------

    async def _run(
        self,
        command: str,
        argv: Sequence[str] | None = None,
        *,
        timeout: float | None = None,
    ) -> ExecResult:
        reported_command = list(argv) if argv is not None else [command]
        # There is no unbounded mode on the HTTP API, so `None` becomes the configured
        # default rather than "wait forever". The endpoint takes whole seconds.
        effective = self._default_timeout_s if timeout is None else timeout
        client_timeout_s = max(1, int(math.ceil(effective)))

        try:
            result = await self._sandbox.commands.run(command, timeout=client_timeout_s)
        except Exception as e:
            if self._is_timeout(e):
                # Bounds the HTTP request only: the execute payload carries no timeout, so
                # the command runs on in the pod under the server's own limit. Reported as
                # applied, since `None` would read as "no timeout".
                raise ExecTimeoutError(
                    command=reported_command,
                    timeout_s=float(client_timeout_s),
                    context={"enforced_by": "client"},
                    cause=e,
                ) from e
            raise ExecTransportError(command=reported_command, cause=e) from e

        # ExecutionResult carries text, not bytes. Binary payloads must never travel
        # this path — read_file()/write_file() exist for that.
        return ExecResult(
            stdout=_encode(result.stdout),
            stderr=_encode(result.stderr),
            exit_code=int(result.exit_code),
        )

    async def _run_script(
        self, script: str, *args: str, timeout: float | None = None
    ) -> ExecResult:
        argv = ["sh", "-lc", script, "sh", *args]
        return await self._run(shlex.join(argv), argv, timeout=timeout)

    async def _read_file_via_exec(self, path: str) -> bytes:
        argv = ["base64", "--", path]
        result = await self._run(shlex.join(argv), argv)
        if not result.ok():
            raise ExecTransportError(
                command=["base64", path],
                message="sandbox file read failed",
                context={"exit_code": result.exit_code},
            )
        return base64.b64decode(result.stdout)

    async def _write_file_via_exec(self, path: str, data: bytes) -> None:
        encoded = base64.b64encode(data).decode("ascii")
        # Unique per write: a fixed suffix would clobber a real file of that name, and two
        # writers to one path would interleave into the same staging file.
        staging = f"{path}.{uuid.uuid4().hex}.b64.part"

        # Chunks and destinations both travel as argv, never as script text. Standard
        # base64 has no shell metacharacters, but that is a property of the alphabet in
        # use today: switching to the URL-safe one would otherwise break this quietly.
        chunks = [
            encoded[i : i + _EXEC_CHUNK_CHARS] for i in range(0, len(encoded), _EXEC_CHUNK_CHARS)
        ] or [""]

        # Cleanup lives in `finally` rather than on the decode command: a failed chunk
        # write or a failed decode would otherwise strand a half-written base64 file next
        # to the target, where nothing ever collects it.
        written = False
        try:
            for index, chunk in enumerate(chunks):
                script = 'printf %s "$2" > "$1"' if index == 0 else 'printf %s "$2" >> "$1"'
                result = await self._run_script(script, staging, chunk)
                if not result.ok():
                    raise ExecTransportError(
                        command=["printf", staging],
                        message="sandbox file write failed",
                        context={"exit_code": result.exit_code, "chunk": index},
                    )

            result = await self._run_script('base64 -d < "$1" > "$2"', staging, path)
            if not result.ok():
                raise ExecTransportError(
                    command=["base64", "-d", staging, path],
                    message="sandbox file write failed",
                    context={"exit_code": result.exit_code},
                )
            written = True
        finally:
            # `written` is the last statement of the block above, so raising here can only
            # happen with nothing else in flight to mask.
            if written:
                await self._rm_staging_checked(staging)
            else:
                await self._rm_staging_best_effort(staging)

    async def _rm_staging_checked(self, staging: str) -> None:
        """Remove staging on a write that otherwise succeeded.

        No other error is on its way out to carry this one, and a stranded ".b64.part"
        sits next to the file the caller believes was written cleanly — inside the
        workspace a snapshot would sweep.
        """
        context, cause = await self._rm_staging(staging)
        if context is not None:
            raise ExecTransportError(
                command=["rm", "-f", staging],
                message="sandbox staging cleanup failed",
                context=context,
                cause=cause,
            )

    async def _rm_staging_best_effort(self, staging: str) -> None:
        """Remove staging while another failure is already on its way out.

        That failure is the one the caller needs, so a cleanup that fails too is logged
        rather than raised: it must never replace the write or decode error.
        """
        context, cause = await self._rm_staging(staging)
        if context is not None:
            logger.warning(
                "sandbox staging cleanup failed for %s: %s",
                staging,
                cause if cause is not None else context,
            )

    async def _rm_staging(self, staging: str) -> tuple[dict[str, object] | None, Exception | None]:
        """Remove the staging file, describing a failure instead of raising it.

        Returns ``(None, None)`` once the file is gone. Otherwise the context/cause pair
        lets the caller decide whether this failure is worth raising.
        """
        try:
            result = await self._run_script('rm -f "$1"', staging)
        except Exception as e:
            return {"reason": "cleanup command failed"}, e
        if result.ok():
            return None, None
        return {"exit_code": result.exit_code}, None

    @staticmethod
    def _is_timeout(exc: BaseException) -> bool:
        if isinstance(exc, (asyncio.TimeoutError, TimeoutError)):
            return True
        if httpx is not None and isinstance(exc, httpx.TimeoutException):
            return True
        return False


def _encode(value: str | bytes | None) -> bytes:
    if value is None:
        return b""
    if isinstance(value, bytes):
        return value
    return value.encode("utf-8", errors="replace")
