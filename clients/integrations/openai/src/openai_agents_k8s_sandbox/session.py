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

"""``BaseSandboxSession`` implementation backed by a Kubernetes sandbox pod.

Only six methods are abstract on ``BaseSandboxSession``; the ~1300 lines above them
(``ls``/``rm``/``mkdir``/``extract``/``apply_patch``, manifest materialization, snapshot
fingerprinting) run unchanged on top of them.
"""

from __future__ import annotations

import base64
import io
import logging
import tarfile
import uuid
from pathlib import Path

from agents.sandbox.errors import (
    ExposedPortUnavailableError,
    WorkspaceArchiveReadError,
    WorkspaceArchiveWriteError,
)
from agents.sandbox.session.base_sandbox_session import BaseSandboxSession
from agents.sandbox.session.runtime_helpers import (
    RESOLVE_WORKSPACE_PATH_HELPER,
    RuntimeHelperScript,
)
from agents.sandbox.session.workspace_payloads import coerce_write_payload
from agents.sandbox.types import ExecResult, ExposedPortEndpoint, User
from agents.sandbox.util.tar_utils import UnsafeTarMemberError, validate_tarfile
from agents.sandbox.workspace_paths import (
    coerce_posix_path,
    posix_path_as_path,
    posix_path_for_error,
    sandbox_path_str,
)

from .options import K8sSandboxSessionState
from .transport import _EXEC_CHUNK_CHARS, SandboxTransport

logger = logging.getLogger(__name__)


class K8sSandboxSession(BaseSandboxSession):
    state: K8sSandboxSessionState

    # Staging area for workspace archives. Outside the workspace root so it is never
    # swept into a snapshot.
    _ARCHIVE_STAGING_DIR: Path = posix_path_as_path(coerce_posix_path("/tmp/agents-k8s-sandbox"))

    def __init__(
        self,
        *,
        transport: SandboxTransport,
        state: K8sSandboxSessionState,
    ) -> None:
        self._transport = transport
        self.state = state

    # -- capabilities ------------------------------------------------------------

    def supports_pty(self) -> bool:
        # The in-pod HTTP API has no PTY channel. sandboxd's gRPC ProcessService
        # (Start/WriteStdin/ResizeTTY) is the path to flipping this on.
        return False

    def supports_docker_volume_mounts(self) -> bool:
        return False

    def _runtime_helpers(self) -> tuple[RuntimeHelperScript, ...]:
        return (RESOLVE_WORKSPACE_PATH_HELPER,)

    def _current_runtime_helper_cache_key(self) -> object | None:
        return self.state.sandbox_id

    async def _validate_path_access(self, path: Path | str, *, for_write: bool = False) -> Path:
        # Remote validation: the path checks run in the pod, not in this process.
        return await self._validate_remote_path_access(path, for_write=for_write)

    # -- exec --------------------------------------------------------------------

    async def _exec_internal(
        self, *command: str | Path, timeout: float | None = None
    ) -> ExecResult:
        return await self._transport.exec_command(
            [str(part) for part in command], timeout=timeout
        )

    async def running(self) -> bool:
        return await self._transport.is_ready()

    async def _resolve_exposed_port(self, port: int) -> ExposedPortEndpoint:
        host = self.state.exposed_port_host
        if not host:
            raise ExposedPortUnavailableError(
                port=port,
                exposed_ports=self.state.exposed_ports,
                reason="backend_unavailable",
                context={"backend": "k8s", "detail": "no_exposed_port_host"},
            )
        # The Sandbox CR gives the pod a stable hostname, so the container port is the
        # published port — there is no host-side remapping as with Docker.
        return ExposedPortEndpoint(host=host, port=port, tls=False)

    # -- file I/O ----------------------------------------------------------------

    async def read(self, path: Path, *, user: str | User | None = None) -> io.IOBase:
        workspace_path = await self._validate_path_access(path)

        # A `user` means the read must be subject to that user's permissions, which only
        # the exec path can express (the SDK prefixes `sudo -u`).
        if user is not None or self._uses_exec_file_transfer:
            return await self._read_via_exec(path, workspace_path, user=user)

        try:
            data = await self._transport.read_file(sandbox_path_str(workspace_path))
        except Exception as e:
            if not self.state.read_fallback:
                raise
            # Fall back to exec so a missing file is reported as WorkspaceReadNotFoundError
            # rather than an opaque transport error. Log it: a download endpoint that
            # always fails would otherwise look like a merely slow sandbox.
            logger.warning(
                "sandbox file download failed, falling back to exec for %s: %s",
                workspace_path,
                e,
            )
            return await self._read_via_exec(path, workspace_path, user=user)
        return io.BytesIO(data)

    async def write(
        self,
        path: Path,
        data: io.IOBase,
        *,
        user: str | User | None = None,
    ) -> None:
        payload = coerce_write_payload(path=path, data=data)
        workspace_path = await self._validate_path_access(path, for_write=True)
        # Prototype limitation: the whole payload is buffered. The HTTP upload endpoint
        # takes a single body, so streaming would need the sandboxd filesystem service.
        raw = payload.stream.read()

        if user is not None or self._uses_exec_file_transfer:
            await self._write_via_exec(workspace_path, raw, user=user)
            return

        await self.mkdir(workspace_path.parent, parents=True)
        try:
            await self._transport.write_file(sandbox_path_str(workspace_path), raw)
        except Exception as e:
            raise WorkspaceArchiveWriteError(path=workspace_path, cause=e) from e

    async def _read_via_exec(
        self,
        path: Path,
        workspace_path: Path,
        *,
        user: str | User | None,
    ) -> io.IOBase:
        # base64 keeps the bytes intact across an endpoint that returns JSON strings.
        # Pass the file as an argument rather than redirecting into stdin: a failed
        # redirect exits 2 under dash, and the SDK's not-found classifier keys on exit 1.
        path_arg = sandbox_path_str(workspace_path)
        command = ("base64", "--", path_arg)
        result = await self.exec(*command, shell=False, user=user)
        if not result.ok():
            await self._raise_read_error_from_exec(
                path=path,
                workspace_path=workspace_path,
                command=command,
                result=result,
                user=user,
            )

        try:
            decoded = base64.b64decode(result.stdout)
        except ValueError as e:
            raise WorkspaceArchiveReadError(path=path, cause=e) from e
        return io.BytesIO(decoded)

    async def _write_via_exec(
        self,
        workspace_path: Path,
        raw: bytes,
        *,
        user: str | User | None,
    ) -> None:
        encoded = base64.b64encode(raw).decode("ascii")
        path_arg = sandbox_path_str(workspace_path)
        # Unique per write, for the same reason K8sHttpTransport stages under a uuid.
        staging_arg = f"{path_arg}.{uuid.uuid4().hex}.b64.part"

        # The in-pod server splits the command and execs the argv directly, so the payload
        # lands as a single execve argument — capped at MAX_ARG_STRLEN (128 KiB), which a
        # base64 blob clears at roughly 96 KiB of input. Append it in chunks instead, the
        # same way K8sHttpTransport does. Every chunk goes through self.exec so a `user`
        # still gets its `sudo -u` prefix.
        chunks = [
            encoded[i : i + _EXEC_CHUNK_CHARS] for i in range(0, len(encoded), _EXEC_CHUNK_CHARS)
        ] or [""]

        written = False
        try:
            for index, chunk in enumerate(chunks):
                script = (
                    'mkdir -p "$(dirname "$1")" && printf %s "$2" > "$1"'
                    if index == 0
                    else 'printf %s "$2" >> "$1"'
                )
                result = await self.exec(
                    "sh", "-lc", script, "sh", staging_arg, chunk, shell=False, user=user
                )
                if not result.ok():
                    raise self._write_via_exec_error(workspace_path, result)

            result = await self.exec(
                "sh",
                "-lc",
                'base64 -d < "$1" > "$2"',
                "sh",
                staging_arg,
                path_arg,
                shell=False,
                user=user,
            )
            if not result.ok():
                raise self._write_via_exec_error(workspace_path, result)
            written = True
        finally:
            # Staging sits next to the target, inside the workspace: leaving it behind
            # after a write the caller believes succeeded would seed the next snapshot
            # with it. `written` is the last statement above, so the checked branch can
            # only raise with nothing else in flight to mask.
            if written:
                await self._rm_checked(
                    Path(staging_arg),
                    error=WorkspaceArchiveWriteError,
                    error_path=workspace_path,
                )
            else:
                await self._rm_best_effort(Path(staging_arg))

    @staticmethod
    def _write_via_exec_error(
        workspace_path: Path, result: ExecResult
    ) -> WorkspaceArchiveWriteError:
        return WorkspaceArchiveWriteError(
            path=workspace_path,
            context={
                "exit_code": result.exit_code,
                "stderr": result.stderr.decode("utf-8", errors="replace"),
            },
        )

    # -- workspace snapshots -----------------------------------------------------

    async def persist_workspace(self) -> io.IOBase:
        root = self._workspace_root_path()
        error_root = posix_path_for_error(root)
        staging = self._stage_path("workspace.tar")
        staging_arg = sandbox_path_str(staging)

        await self._exec_checked_nonzero("mkdir", "-p", sandbox_path_str(self._ARCHIVE_STAGING_DIR))

        # Tar inside the pod, then pull the archive over the bytes-clean file channel.
        # Streaming a tar through the JSON exec endpoint would corrupt it.
        excludes = [
            f"--exclude=./{rel.as_posix()}" for rel in sorted(self._persist_workspace_skip_relpaths())
        ]
        result = await self.exec(
            "tar",
            "-c",
            "-f",
            staging_arg,
            "-C",
            sandbox_path_str(root),
            *excludes,
            ".",
            shell=False,
        )
        if not result.ok():
            raise WorkspaceArchiveReadError(
                path=error_root,
                context={
                    "exit_code": result.exit_code,
                    "stderr": result.stderr.decode("utf-8", errors="replace"),
                },
            )

        read = False
        try:
            await self._reject_oversized_stage(staging_arg, error_root)
            data = await self._transport.read_file(staging_arg)
            read = True
        except WorkspaceArchiveReadError:
            raise
        except Exception as e:
            raise WorkspaceArchiveReadError(path=error_root, cause=e) from e
        finally:
            if read:
                await self._rm_checked(
                    staging, error=WorkspaceArchiveReadError, error_path=error_root
                )
            else:
                await self._rm_best_effort(staging)

        return io.BytesIO(data)

    async def hydrate_workspace(self, data: io.IOBase) -> None:
        root = self._workspace_root_path()
        error_root = posix_path_for_error(root)

        archive = _drain(data, error_path=error_root, max_bytes=self._max_archive_input_bytes())
        try:
            with tarfile.open(fileobj=io.BytesIO(archive), mode="r:*") as tar:
                validate_tarfile(tar, allow_external_symlink_targets=False)
        except UnsafeTarMemberError as e:
            raise WorkspaceArchiveWriteError(
                path=error_root,
                context={"reason": e.reason, "member": e.member},
                cause=e,
            ) from e
        except (tarfile.TarError, OSError) as e:
            raise WorkspaceArchiveWriteError(path=error_root, cause=e) from e

        staging = self._stage_path("hydrate.tar")
        staging_arg = sandbox_path_str(staging)
        await self._exec_checked_nonzero("mkdir", "-p", sandbox_path_str(self._ARCHIVE_STAGING_DIR))
        await self._exec_checked_nonzero("mkdir", "-p", sandbox_path_str(root))

        try:
            await self._transport.write_file(staging_arg, archive)
        except Exception as e:
            raise WorkspaceArchiveWriteError(path=error_root, cause=e) from e

        extracted = False
        try:
            result = await self.exec(
                "tar", "-x", "-f", staging_arg, "-C", sandbox_path_str(root), shell=False
            )
            if not result.ok():
                raise WorkspaceArchiveWriteError(
                    path=error_root,
                    context={
                        "exit_code": result.exit_code,
                        "stderr": result.stderr.decode("utf-8", errors="replace"),
                    },
                )
            extracted = True
        finally:
            if extracted:
                await self._rm_checked(
                    staging, error=WorkspaceArchiveWriteError, error_path=error_root
                )
            else:
                await self._rm_best_effort(staging)

    # -- helpers -----------------------------------------------------------------

    @property
    def _uses_exec_file_transfer(self) -> bool:
        return self.state.file_transfer == "exec"

    def _stage_path(self, name_hint: str) -> Path:
        return self._ARCHIVE_STAGING_DIR / f"{uuid.uuid4().hex}_{name_hint}"

    async def _reject_oversized_stage(self, staging_arg: str, error_path: Path) -> None:
        """Size the staged tar in the pod before it is pulled into this process.

        `read_file()` has no streaming form, so the whole archive lands in memory; and an
        archive above the input ceiling is one `hydrate_workspace()` would refuse anyway.
        """

        limit = self._max_archive_input_bytes()
        if limit is None:
            return

        result = await self.exec("sh", "-lc", 'wc -c < "$1"', "sh", staging_arg, shell=False)
        measured = result.stdout.decode("utf-8", errors="replace").strip()
        if not result.ok() or not measured.isdigit():
            raise WorkspaceArchiveReadError(
                path=error_path,
                context={"reason": "archive size probe failed", "exit_code": result.exit_code},
            )

        size = int(measured)
        if size > limit:
            raise WorkspaceArchiveReadError(
                path=error_path,
                context={"reason": "archive size exceeds limit", "limit": limit, "actual": size},
            )

    def _max_archive_input_bytes(self) -> int | None:
        # None (the default until a run opts in via SandboxArchiveLimits) means unbounded,
        # which keeps this a no-op for callers that never configured limits.
        limits = self._archive_limits
        return limits.max_input_bytes if limits is not None else None

    async def _rm_best_effort(self, path: Path) -> None:
        """Remove staging while another failure is already propagating.

        That failure is the one the caller needs, so a cleanup that fails too is logged
        rather than raised: it must never take the original error's place.
        """
        context, cause = await self._rm(path)
        if context is not None:
            logger.warning(
                "sandbox staging cleanup failed for %s: %s",
                path,
                cause if cause is not None else context,
            )

    async def _rm_checked(
        self,
        path: Path,
        *,
        error: type[WorkspaceArchiveReadError] | type[WorkspaceArchiveWriteError],
        error_path: Path,
    ) -> None:
        """Remove staging on a path where nothing else failed.

        No other error is on its way out to carry this one, so an archive that cannot be
        cleaned up is reported in the error class the surrounding operation already uses.
        """
        context, cause = await self._rm(path)
        if context is not None:
            raise error(path=error_path, context=context, cause=cause)

    async def _rm(self, path: Path) -> tuple[dict[str, object] | None, Exception | None]:
        """Remove a file, describing a failure instead of raising it.

        Returns ``(None, None)`` once the file is gone. Otherwise the context/cause pair
        lets the caller decide whether this failure is worth raising.
        """
        try:
            result = await self.exec("rm", "-f", "--", sandbox_path_str(path), shell=False)
        except Exception as e:
            return {"reason": "staging cleanup failed"}, e
        if result.ok():
            return None, None
        return {
            "reason": "staging cleanup failed",
            "exit_code": result.exit_code,
            "stderr": result.stderr.decode("utf-8", errors="replace"),
        }, None


def _drain(data: io.IOBase, *, error_path: Path, max_bytes: int | None) -> bytes:
    """Buffer a snapshot archive, refusing to grow past ``max_bytes``.

    The whole archive is held in memory, so the ceiling is enforced per chunk rather than
    after the fact — otherwise an oversized stream is already resident by the time anyone
    could reject it.
    """

    buf = io.BytesIO()
    total = 0
    while True:
        chunk = data.read(io.DEFAULT_BUFFER_SIZE)
        if chunk in ("", b"", None):
            break
        if isinstance(chunk, str):
            chunk = chunk.encode("utf-8")
        if not isinstance(chunk, (bytes, bytearray)):
            raise WorkspaceArchiveWriteError(
                path=error_path, context={"reason": "non_bytes_tar_payload"}
            )
        total += len(chunk)
        if max_bytes is not None and total > max_bytes:
            raise WorkspaceArchiveWriteError(
                path=error_path,
                context={
                    "reason": "archive input size exceeds limit",
                    "limit": max_bytes,
                    "actual": total,
                },
            )
        buf.write(chunk)
    return buf.getvalue()
