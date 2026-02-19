# Copyright 2025 The Kubernetes Authors.
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

from __future__ import annotations

import asyncio
import logging
import posixpath
import shlex
from pathlib import PurePosixPath
from typing import Callable, Iterable, Optional, Dict, List, Union, Tuple, Any

from k8s_agent_sandbox import SandboxClient

try:
    from deepagents.backends.protocol import (
        EditResult,
        ExecuteResponse,
        FileDownloadResponse,
        FileInfo,
        FileUploadResponse,
        GrepMatch,
        SandboxBackendProtocol,
        WriteResult,
    )
except (ImportError, ModuleNotFoundError) as exc:
    raise ImportError(
        "deepagents is required to use langchain-agent-sandbox"
    ) from exc

logger = logging.getLogger(__name__)


class AgentSandboxBackend(SandboxBackendProtocol):
    """DeepAgents backend adapter for agent-sandbox runtimes.

    Implements the DeepAgents SandboxBackendProtocol by wrapping a SandboxClient.
    All file operations are virtualized under `root_dir` (default: /app).

    Requirements:
        - Sandbox image must have POSIX utilities: sh, grep, find, mkdir, ls, test, printf
        - SandboxClient must be configured and connected before use

    Note:
        The backend can optionally manage the client lifecycle when created via from_template().
        When manage_client=True, the backend must be used as a context manager.
    """

    def __init__(
        self,
        client: SandboxClient,
        root_dir: str = "/app",
        manage_client: bool = False,
        allow_absolute_paths: bool = False,
    ) -> None:
        """Initialize the backend with a SandboxClient.

        Args:
            client: A configured SandboxClient instance.
            root_dir: Virtual root for file operations. Must be an absolute path.
            manage_client: If True, the backend manages the client lifecycle via context manager.
            allow_absolute_paths: If True, write/upload operations may target
                absolute paths outside root_dir.

        Raises:
            ValueError: If root_dir is not an absolute path.
        """
        if not root_dir.startswith("/"):
            raise ValueError(f"root_dir must be an absolute path, got: {root_dir}")
        self._client = client
        self._root_dir = posixpath.normpath(root_dir)
        self._manage_client = manage_client
        self._allow_absolute_paths = allow_absolute_paths

    def __enter__(self) -> "AgentSandboxBackend":
        if self._manage_client:
            self._client.__enter__()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        if self._manage_client:
            self._client.__exit__(exc_type, exc, tb)

    async def __aenter__(self) -> "AgentSandboxBackend":
        return self.__enter__()

    async def __aexit__(self, exc_type, exc, tb) -> None:
        self.__exit__(exc_type, exc, tb)

    @classmethod
    def from_template(
        cls,
        template_name: str,
        namespace: str = "default",
        gateway_name: Optional[str] = None,
        gateway_namespace: str = "default",
        api_url: Optional[str] = None,
        server_port: int = 8888,
        root_dir: str = "/app",
        allow_absolute_paths: bool = False,
        **kwargs: Any,
    ) -> "AgentSandboxBackend":
        """Create a backend with an internally-managed SandboxClient.

        The client will be automatically entered/exited when the backend
        is used as a context manager.

        Args:
            template_name: Name of the SandboxTemplate to claim.
            namespace: Kubernetes namespace for the sandbox.
            gateway_name: Optional gateway for production mode.
            gateway_namespace: Namespace of the gateway.
            api_url: Direct URL to bypass gateway discovery.
            server_port: Port the sandbox runtime listens on.
            root_dir: Virtual root for file operations.
            allow_absolute_paths: If True, write/upload operations may target
                absolute paths outside root_dir.
            **kwargs: Additional arguments passed to SandboxClient.

        Returns:
            AgentSandboxBackend with manage_client=True.
        """
        client = SandboxClient(
            template_name=template_name,
            namespace=namespace,
            gateway_name=gateway_name,
            gateway_namespace=gateway_namespace,
            api_url=api_url,
            server_port=server_port,
            **kwargs,
        )
        return cls(
            client,
            root_dir=root_dir,
            manage_client=True,
            allow_absolute_paths=allow_absolute_paths,
        )

    def execute(self, command: str) -> ExecuteResponse:
        """Execute a shell command in the sandbox.

        Args:
            command: Shell command to execute.

        Returns:
            ExecuteResponse with combined stdout/stderr output and exit code.
        """
        result = self._client.run(command)
        combined = result.stdout
        if result.stderr:
            combined = f"{combined}\n{result.stderr}" if combined else result.stderr
        return ExecuteResponse(
            output=combined,
            exit_code=result.exit_code,
            truncated=False,
        )

    async def aexecute(self, command: str) -> ExecuteResponse:
        """Async version of execute()."""
        return await asyncio.to_thread(self.execute, command)

    def ls_info(self, path: str) -> List[FileInfo]:
        """List directory contents.

        Args:
            path: Directory path to list.

        Returns:
            List of FileInfo entries. Returns empty list on error.
        """
        internal_path = self._to_internal(path)
        command = shlex.join(["ls", "-a", "-p", internal_path])
        result = self._client.run(command)
        if result.exit_code != 0:
            logger.warning(
                "ls_info failed for path '%s': exit_code=%d, stderr=%s",
                path, result.exit_code, result.stderr
            )
            return []

        entries: List[FileInfo] = []
        public_dir = self._normalize_public_dir(path)
        for entry in result.stdout.splitlines():
            if not entry or entry.rstrip("/") in (".", ".."):
                continue
            is_dir = entry.endswith("/")
            name = entry[:-1] if is_dir else entry
            public_path = self._join_public(public_dir, name)
            entries.append(FileInfo(path=public_path, is_dir=is_dir))

        entries.sort(key=lambda item: item["path"])
        return entries

    async def als_info(self, path: str) -> List[FileInfo]:
        """Async version of ls_info()."""
        return await asyncio.to_thread(self.ls_info, path)

    def read(self, file_path: str, offset: int = 0, limit: int = 2000) -> str:
        """Read file content with line numbers.

        Args:
            file_path: Path to the file to read.
            offset: 0-based line index to start reading from.
            limit: Maximum number of lines to return.

        Returns:
            File content with 1-based line numbers, or error message string.
        """
        internal_path = self._to_internal(file_path)
        if not self._exists(internal_path):
            logger.warning("Attempted to read non-existent file: %s", file_path)
            return f"Error: File '{file_path}' not found"

        content = self._client.read(self._to_relative(file_path))
        decoded = content.decode("utf-8", errors="replace")
        lines = decoded.splitlines()
        start = max(0, offset)
        end = min(len(lines), start + limit)
        numbered = [
            f"{idx + 1}: {self._truncate_line(lines[idx])}" for idx in range(start, end)
        ]
        return "\n".join(numbered)

    async def aread(self, file_path: str, offset: int = 0, limit: int = 2000) -> str:
        """Async version of read()."""
        return await asyncio.to_thread(self.read, file_path, offset, limit)

    def grep_raw(
        self,
        pattern: str,
        path: Optional[str] = None,
        glob: Optional[str] = None,
    ) -> Union[List[GrepMatch], str]:
        """Search for pattern in files.

        Args:
            pattern: Fixed string pattern to search for.
            path: Base path to search in (default: /).
            glob: Optional glob pattern to filter files (e.g., "*.py").

        Returns:
            List of GrepMatch entries, or error message string if path not found.
        """
        base_path = path or "/"
        internal_path = self._to_internal(base_path)
        if not self._exists(internal_path):
            return f"Error: Path '{base_path}' not found"

        grep_opts = "-rHnF"
        glob_part = f"--include={shlex.quote(glob)}" if glob else ""
        command = (
            f"grep {grep_opts} {glob_part} -e {shlex.quote(pattern)} {shlex.quote(internal_path)} 2>/dev/null || true"
        )
        result = self._client.run(command)
        if not result.stdout.strip():
            return []

        matches: List[GrepMatch] = []
        for line in result.stdout.splitlines():
            parts = line.split(":", 2)
            if len(parts) != 3:
                logger.debug("Skipping malformed grep output line: %s", line)
                continue
            raw_path, line_no, text = parts
            public_path = self._to_public(raw_path)
            try:
                line_int = int(line_no)
            except ValueError:
                logger.debug("Skipping grep line with invalid line number: %s", line)
                continue
            matches.append(GrepMatch(path=public_path, line=line_int, text=text))

        return matches

    async def agrep_raw(
        self,
        pattern: str,
        path: Optional[str] = None,
        glob: Optional[str] = None,
    ) -> Union[List[GrepMatch], str]:
        """Async version of grep_raw()."""
        return await asyncio.to_thread(self.grep_raw, pattern, path, glob)

    def glob_info(self, pattern: str, path: str = "/") -> List[FileInfo]:
        """Find files matching a glob pattern.

        Args:
            pattern: Glob pattern to match (e.g., "*.py", "**/*.txt").
            path: Base path to search in.

        Returns:
            List of FileInfo entries for matching files. Returns empty list on error.
        """
        internal_path = self._to_internal(path)
        command = shlex.join(["find", internal_path, "-mindepth", "1", "-print"])
        result = self._client.run(command)
        if result.exit_code != 0:
            logger.warning(
                "glob_info failed for path '%s': exit_code=%d, stderr=%s",
                path, result.exit_code, result.stderr
            )
            return []

        normalized_pattern = pattern.lstrip("/")
        entries: List[FileInfo] = []
        base_internal = self._to_internal(path)

        for raw in result.stdout.splitlines():
            rel_path = posixpath.relpath(raw, base_internal)
            if PurePosixPath(rel_path).match(normalized_pattern):
                public_path = self._to_public(raw)
                is_dir = self._is_dir(raw)
                entries.append(FileInfo(path=public_path, is_dir=is_dir))

        entries.sort(key=lambda item: item["path"])
        return entries

    async def aglob_info(self, pattern: str, path: str = "/") -> List[FileInfo]:
        """Async version of glob_info()."""
        return await asyncio.to_thread(self.glob_info, pattern, path)

    def write(self, file_path: str, content: str) -> WriteResult:
        """Create a new file with the given content.

        Args:
            file_path: Path for the new file.
            content: Content to write.

        Returns:
            WriteResult with error=None on success, or error message on failure.
            Fails if file already exists.
        """
        try:
            internal_path = self._resolve_write_path(file_path)
        except ValueError as e:
            return WriteResult(
                error=f"Error: Invalid path '{file_path}': {e}",
                path=file_path,
                files_update=None,
            )
        if self._exists(internal_path):
            return WriteResult(
                error=f"File '{file_path}' already exists",
                path=file_path,
                files_update=None,
            )

        self._ensure_parent_dir(internal_path)
        payload = content.encode("utf-8") if isinstance(content, str) else content
        self._upload_bytes(internal_path, payload)
        return WriteResult(error=None, path=file_path, files_update=None)

    async def awrite(self, file_path: str, content: str) -> WriteResult:
        """Async version of write()."""
        return await asyncio.to_thread(self.write, file_path, content)

    def edit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> EditResult:
        """Replace a string in a file.

        Args:
            file_path: Path to the file to edit.
            old_string: String to find and replace.
            new_string: Replacement string.
            replace_all: If True, replace all occurrences. If False, requires
                         exactly one occurrence.

        Returns:
            EditResult with error=None on success, or error message on failure.
        """
        internal_path = self._to_internal(file_path)
        if not self._exists(internal_path):
            return EditResult(
                error=f"Error: File '{file_path}' not found",
                path=file_path,
                files_update=None,
                occurrences=0,
            )

        content = self._client.read(self._to_relative(file_path)).decode("utf-8", errors="replace")
        occurrences = content.count(old_string)
        if occurrences == 0:
            return EditResult(
                error=f"Error: String not found in file: '{old_string}'",
                path=file_path,
                files_update=None,
                occurrences=0,
            )
        if not replace_all and occurrences > 1:
            return EditResult(
                error=(
                    f"Error: String '{old_string}' appears multiple times. "
                    "Use replace_all=True to replace all occurrences."
                ),
                path=file_path,
                files_update=None,
                occurrences=occurrences,
            )

        if replace_all:
            updated = content.replace(old_string, new_string)
        else:
            updated = content.replace(old_string, new_string, 1)
        self._upload_bytes(internal_path, updated.encode("utf-8"))
        return EditResult(
            error=None,
            path=file_path,
            files_update=None,
            occurrences=occurrences if replace_all else 1,
        )

    async def aedit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> EditResult:
        """Async version of edit()."""
        return await asyncio.to_thread(self.edit, file_path, old_string, new_string, replace_all)

    def upload_files(
        self, files: Union[Dict[str, bytes], Iterable[Tuple[str, bytes]]]
    ) -> List[FileUploadResponse]:
        """Upload multiple files to the sandbox.

        Args:
            files: Dict or iterable of (path, content) pairs.

        Returns:
            List of FileUploadResponse for each file.
        """
        items = files.items() if isinstance(files, dict) else files
        responses: List[FileUploadResponse] = []
        for path, payload in items:
            try:
                internal_path = self._resolve_write_path(path)
            except ValueError as e:
                logger.warning("Invalid path '%s': %s", path, e)
                responses.append(FileUploadResponse(path=path, error="invalid_path"))
                continue

            state = self._file_state(internal_path)
            if state == "dir":
                responses.append(FileUploadResponse(path=path, error="is_directory"))
                continue
            if state == "denied":
                responses.append(FileUploadResponse(path=path, error="permission_denied"))
                continue

            parent_state = self._dir_state(posixpath.dirname(internal_path))
            if parent_state == "missing" or parent_state == "not_dir":
                responses.append(FileUploadResponse(path=path, error="invalid_path"))
                continue
            if parent_state == "denied":
                responses.append(FileUploadResponse(path=path, error="permission_denied"))
                continue

            try:
                self._upload_bytes(internal_path, payload)
                responses.append(FileUploadResponse(path=path, error=None))
            except (RuntimeError, ConnectionError, TimeoutError) as e:
                logger.error("Upload failed for '%s': %s: %s", path, type(e).__name__, e)
                responses.append(FileUploadResponse(path=path, error="upload_failed"))
        return responses

    async def aupload_files(
        self, files: Union[Dict[str, bytes], Iterable[Tuple[str, bytes]]]
    ) -> List[FileUploadResponse]:
        """Async version of upload_files()."""
        return await asyncio.to_thread(self.upload_files, files)

    def download_files(self, paths: Iterable[str]) -> List[FileDownloadResponse]:
        """Download multiple files from the sandbox.

        Args:
            paths: Iterable of file paths to download.

        Returns:
            List of FileDownloadResponse for each file.
        """
        responses: List[FileDownloadResponse] = []
        for path in paths:
            try:
                internal_path = self._to_internal(path)
            except ValueError as e:
                logger.warning("Invalid path '%s': %s", path, e)
                responses.append(FileDownloadResponse(path=path, content=None, error="invalid_path"))
                continue
            state = self._file_state(internal_path)
            if state == "missing":
                responses.append(FileDownloadResponse(path=path, content=None, error="file_not_found"))
                continue
            if state == "dir":
                responses.append(FileDownloadResponse(path=path, content=None, error="is_directory"))
                continue
            if state == "denied":
                responses.append(FileDownloadResponse(path=path, content=None, error="permission_denied"))
                continue
            try:
                content = self._client.read(self._to_relative(path))
                responses.append(FileDownloadResponse(path=path, content=content, error=None))
            except (RuntimeError, ConnectionError, TimeoutError) as e:
                logger.error("Download failed for '%s': %s: %s", path, type(e).__name__, e)
                responses.append(FileDownloadResponse(path=path, content=None, error="download_failed"))
        return responses

    async def adownload_files(self, paths: Iterable[str]) -> List[FileDownloadResponse]:
        """Async version of download_files()."""
        return await asyncio.to_thread(self.download_files, paths)

    def _exists(self, internal_path: str) -> bool:
        command = shlex.join(["sh", "-c", f"test -e {shlex.quote(internal_path)}"])
        result = self._client.run(command)
        return result.exit_code == 0

    def _is_dir(self, internal_path: str) -> bool:
        command = shlex.join(["sh", "-c", f"test -d {shlex.quote(internal_path)}"])
        result = self._client.run(command)
        return result.exit_code == 0

    def _ensure_parent_dir(self, internal_path: str) -> None:
        parent = posixpath.dirname(internal_path)
        command = shlex.join(["mkdir", "-p", parent])
        result = self._client.run(command)
        if result.exit_code != 0:
            error_msg = result.stderr.strip() if result.stderr else f"mkdir failed with exit code {result.exit_code}"
            raise RuntimeError(f"Cannot create parent directory '{parent}': {error_msg}")

    def _upload_bytes(self, internal_path: str, payload: bytes) -> None:
        try:
            # Bypass SandboxClient.write basename behavior by calling router upload directly.
            files_payload = {"file": (internal_path, payload)}
            self._client._request("POST", "upload", files=files_payload)
        except Exception as e:
            raise RuntimeError(f"Upload failed for {internal_path}: {e}") from e

    def _resolve_write_path(self, path: str) -> str:
        """Resolve a write path while allowing absolute paths outside root_dir."""
        candidate = path.strip()
        if not candidate:
            raise ValueError("empty path")
        normalized = posixpath.normpath(candidate)
        if (
            self._allow_absolute_paths
            and normalized.startswith("/")
            and normalized != self._root_dir
            and not normalized.startswith(self._root_dir + "/")
        ):
            return normalized
        return self._to_internal(candidate)

    def _to_internal(self, path: str) -> str:
        """Convert a public virtual path to an internal filesystem path.

        All paths are treated as relative to root_dir after normalization:
        - Leading slashes are stripped: '/file.txt' -> '{root_dir}/file.txt'
        - Paths already containing root_dir prefix are normalized
        - Path traversal attacks (../) that escape root_dir raise ValueError

        Args:
            path: Public virtual path.

        Returns:
            Internal filesystem path under root_dir.

        Raises:
            ValueError: If the path escapes root_dir.
        """
        normalized = path.strip() or "/"
        if normalized.startswith(self._root_dir):
            normalized = normalized[len(self._root_dir):]
            normalized = normalized.lstrip("/")
        normalized = normalized.lstrip("/")
        internal_path = posixpath.normpath(posixpath.join(self._root_dir, normalized))
        # When root_dir is "/" every absolute path is valid; the naive
        # ``startswith(root_dir + "/")`` check becomes ``startswith("//")``
        # which incorrectly rejects all paths.
        if self._root_dir == "/":
            if not internal_path.startswith("/"):
                raise ValueError(f"Path '{path}' escapes root_dir '{self._root_dir}'")
        elif internal_path != self._root_dir and not internal_path.startswith(self._root_dir + "/"):
            raise ValueError(f"Path '{path}' escapes root_dir '{self._root_dir}'")
        return internal_path

    def _to_relative(self, path: str) -> str:
        internal_path = self._to_internal(path)
        rel = posixpath.relpath(internal_path, self._root_dir)
        return "." if rel == "." else rel

    def _to_public(self, internal_path: str) -> str:
        rel = posixpath.relpath(internal_path, self._root_dir)
        if rel == ".":
            return "/"
        return "/" + rel

    def _normalize_public_dir(self, path: str) -> str:
        if not path or path == "/":
            return "/"
        if path.startswith(self._root_dir):
            path = path[len(self._root_dir):]
        return "/" + path.strip("/")

    def _join_public(self, base: str, name: str) -> str:
        if base == "/":
            return "/" + name
        return posixpath.join(base, name)

    def _file_state(self, internal_path: str) -> str:
        check = (
            f"if [ ! -e {shlex.quote(internal_path)} ]; then echo missing; exit 0; fi; "
            f"if [ -d {shlex.quote(internal_path)} ]; then echo dir; exit 0; fi; "
            f"if [ -r {shlex.quote(internal_path)} ]; then echo file; else echo denied; fi"
        )
        result = self._client.run(f"sh -c {shlex.quote(check)}")
        output = result.stdout.strip()
        if not output and result.exit_code != 0:
            logger.warning(
                "_file_state command failed: exit_code=%d, stderr=%s",
                result.exit_code, result.stderr
            )
        return output or "missing"

    def _dir_state(self, internal_path: str) -> str:
        check = (
            f"if [ ! -e {shlex.quote(internal_path)} ]; then echo missing; exit 0; fi; "
            f"if [ -d {shlex.quote(internal_path)} ]; then "
            f"if [ -w {shlex.quote(internal_path)} ]; then echo writable; else echo denied; fi; "
            f"exit 0; fi; "
            f"echo not_dir"
        )
        result = self._client.run(f"sh -c {shlex.quote(check)}")
        output = result.stdout.strip()
        if not output and result.exit_code != 0:
            logger.warning(
                "_dir_state command failed: exit_code=%d, stderr=%s",
                result.exit_code, result.stderr
            )
        return output or "missing"

    def _truncate_line(self, line: str, max_len: int = 2000) -> str:
        if len(line) <= max_len:
            return line
        return line[:max_len]

    @property
    def id(self) -> str:
        """Return the sandbox/claim name for identification."""
        if getattr(self._client, "claim_name", None):
            return str(self._client.claim_name)
        if getattr(self._client, "sandbox_name", None):
            return str(self._client.sandbox_name)
        return "agent-sandbox"


def create_sandbox_backend_factory(
    template_name: str,
    namespace: str = "default",
    **kwargs: Any,
) -> Callable[[Any], AgentSandboxBackend]:
    """Create a BackendFactory for use with create_deep_agent().

    This factory function returns a callable that creates an AgentSandboxBackend
    when invoked with a ToolRuntime. The ToolRuntime provides state and store,
    but our backend doesn't need them since execution happens in the sandbox.

    Usage:
        from deepagents import create_deep_agent
        from langchain_agent_sandbox import create_sandbox_backend_factory

        agent = create_deep_agent(
            backend=create_sandbox_backend_factory("my-template")
        )

    Args:
        template_name: Name of the SandboxTemplate to claim.
        namespace: Kubernetes namespace for the sandbox.
        **kwargs: Additional arguments passed to AgentSandboxBackend.from_template().

    Returns:
        A factory callable that accepts a ToolRuntime and returns an AgentSandboxBackend.
    """
    def factory(_runtime: Any) -> AgentSandboxBackend:
        return AgentSandboxBackend.from_template(
            template_name=template_name,
            namespace=namespace,
            **kwargs,
        )
    return factory


class SandboxPolicyWrapper:
    """Wraps AgentSandboxBackend with policy enforcement.

    Provides enterprise-grade restrictions on sandbox operations:
    - deny_prefixes: Block writes/edits under certain paths (e.g., /etc, /sys)
    - deny_commands: Block commands containing specific patterns (e.g., rm -rf)
    - audit_log: Optional callable for logging all operations

    The wrapper delegates all read operations directly to the backend,
    while guarding write/edit/execute operations against policy rules.

    Audit log callback signature:
        def audit_log(operation: str, target: str, metadata: dict) -> None:
            # operation: "write", "edit", "execute", "upload"
            # target: file path or command string
            # metadata: {"size": int} for write/upload, {"replace_all": bool} for edit

    Usage:
        backend = AgentSandboxBackend.from_template("my-template")
        wrapped = SandboxPolicyWrapper(
            backend,
            deny_prefixes=["/etc", "/sys", "/proc"],
            deny_commands=["rm -rf", "shutdown", "reboot"],
            audit_log=lambda op, target, meta: print(f"{op}: {target}")
        )
    """

    def __init__(
        self,
        backend: AgentSandboxBackend,
        deny_prefixes: Optional[List[str]] = None,
        deny_commands: Optional[List[str]] = None,
        audit_log: Optional[Callable[[str, str, dict], None]] = None,
    ) -> None:
        self._backend = backend
        self._deny_prefixes = []
        for prefix in (deny_prefixes or []):
            canonical_prefix = self._canonicalize_path(prefix)
            if canonical_prefix == "/":
                self._deny_prefixes.append("/")
            else:
                self._deny_prefixes.append(canonical_prefix.rstrip("/") + "/")
        self._deny_commands = deny_commands or []
        self._audit_log = audit_log

    def __enter__(self) -> "SandboxPolicyWrapper":
        self._backend.__enter__()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self._backend.__exit__(exc_type, exc, tb)

    async def __aenter__(self) -> "SandboxPolicyWrapper":
        return self.__enter__()

    async def __aexit__(self, exc_type, exc, tb) -> None:
        self.__exit__(exc_type, exc, tb)

    @staticmethod
    def _canonicalize_path(path: str) -> str:
        normalized = posixpath.normpath(path.strip() or "/")
        return "/" + normalized.lstrip("/")

    def _is_denied_path(self, path: str) -> bool:
        canonical_path = self._canonicalize_path(path)
        normalized = canonical_path.rstrip("/") + "/" if canonical_path != "/" else "/"
        return any(normalized.startswith(prefix) for prefix in self._deny_prefixes)

    def _is_denied_command(self, cmd: str) -> bool:
        return any(pattern in cmd for pattern in self._deny_commands)

    def ls_info(self, path: str) -> List[FileInfo]:
        """List directory contents. Delegates to backend."""
        return self._backend.ls_info(path)

    async def als_info(self, path: str) -> List[FileInfo]:
        """Async version of ls_info()."""
        return await self._backend.als_info(path)

    def read(self, file_path: str, offset: int = 0, limit: int = 2000) -> str:
        """Read file content. Delegates to backend."""
        return self._backend.read(file_path, offset, limit)

    async def aread(self, file_path: str, offset: int = 0, limit: int = 2000) -> str:
        """Async version of read()."""
        return await self._backend.aread(file_path, offset, limit)

    def grep_raw(
        self,
        pattern: str,
        path: Optional[str] = None,
        glob: Optional[str] = None,
    ) -> Union[List[GrepMatch], str]:
        """Search for pattern in files. Delegates to backend."""
        return self._backend.grep_raw(pattern, path, glob)

    async def agrep_raw(
        self,
        pattern: str,
        path: Optional[str] = None,
        glob: Optional[str] = None,
    ) -> Union[List[GrepMatch], str]:
        """Async version of grep_raw()."""
        return await self._backend.agrep_raw(pattern, path, glob)

    def glob_info(self, pattern: str, path: str = "/") -> List[FileInfo]:
        """Find files matching glob pattern. Delegates to backend."""
        return self._backend.glob_info(pattern, path)

    async def aglob_info(self, pattern: str, path: str = "/") -> List[FileInfo]:
        """Async version of glob_info()."""
        return await self._backend.aglob_info(pattern, path)

    def download_files(self, paths: Iterable[str]) -> List[FileDownloadResponse]:
        """Download files. Delegates to backend."""
        return self._backend.download_files(paths)

    async def adownload_files(self, paths: Iterable[str]) -> List[FileDownloadResponse]:
        """Async version of download_files()."""
        return await self._backend.adownload_files(paths)

    def write(self, file_path: str, content: str) -> WriteResult:
        """Write file with policy check."""
        if self._is_denied_path(file_path):
            return WriteResult(
                error=f"Policy denied: writes not allowed under '{file_path}'",
                path=file_path,
                files_update=None,
            )
        if self._audit_log:
            try:
                self._audit_log("write", file_path, {"size": len(content)})
            except Exception as e:
                logger.warning("Audit log callback failed (continuing): %s", e)
        return self._backend.write(file_path, content)

    async def awrite(self, file_path: str, content: str) -> WriteResult:
        """Async version of write()."""
        return await asyncio.to_thread(self.write, file_path, content)

    def edit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> EditResult:
        """Edit file with policy check."""
        if self._is_denied_path(file_path):
            return EditResult(
                error=f"Policy denied: edits not allowed under '{file_path}'",
                path=file_path,
                files_update=None,
                occurrences=0,
            )
        if self._audit_log:
            try:
                self._audit_log("edit", file_path, {"replace_all": replace_all})
            except Exception as e:
                logger.warning("Audit log callback failed (continuing): %s", e)
        return self._backend.edit(file_path, old_string, new_string, replace_all)

    async def aedit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> EditResult:
        """Async version of edit()."""
        return await asyncio.to_thread(
            self.edit, file_path, old_string, new_string, replace_all
        )

    def execute(self, command: str) -> ExecuteResponse:
        """Execute command with policy check."""
        if self._is_denied_command(command):
            return ExecuteResponse(
                output="Policy denied: command blocked by policy",
                exit_code=1,
                truncated=False,
            )
        if self._audit_log:
            try:
                self._audit_log("execute", command, {})
            except Exception as e:
                logger.warning("Audit log callback failed (continuing): %s", e)
        return self._backend.execute(command)

    async def aexecute(self, command: str) -> ExecuteResponse:
        """Async version of execute()."""
        return await asyncio.to_thread(self.execute, command)

    def upload_files(
        self, files: Union[Dict[str, bytes], Iterable[Tuple[str, bytes]]]
    ) -> List[FileUploadResponse]:
        """Upload files with policy check."""
        items = list(files.items() if isinstance(files, dict) else files)
        responses: List[FileUploadResponse] = []
        allowed_files: List[Tuple[str, bytes]] = []

        for path, payload in items:
            if self._is_denied_path(path):
                responses.append(
                    FileUploadResponse(path=path, error="policy_denied")
                )
            else:
                if self._audit_log:
                    try:
                        self._audit_log("upload", path, {"size": len(payload)})
                    except Exception as e:
                        logger.warning("Audit log callback failed (continuing): %s", e)
                allowed_files.append((path, payload))

        if allowed_files:
            backend_responses = self._backend.upload_files(allowed_files)
            responses.extend(backend_responses)

        return responses

    async def aupload_files(
        self, files: Union[Dict[str, bytes], Iterable[Tuple[str, bytes]]]
    ) -> List[FileUploadResponse]:
        """Async version of upload_files()."""
        return await asyncio.to_thread(self.upload_files, files)

    @property
    def id(self) -> str:
        """Return the backend's sandbox/claim ID."""
        return self._backend.id


class WarmPoolBackend(AgentSandboxBackend):
    """Backend optimized for warmpool usage.

    Provides faster startup by leveraging pre-warmed sandbox pods.
    When a SandboxClaim is created with a sandboxTemplateRef, the controller
    automatically adopts from any warmpool that uses the same template.

    This class provides explicit warmpool awareness and a clear intent that
    warmpool adoption is expected. It serves as a future extension point
    when the CRD supports explicit warmpool selection.

    Usage:
        with WarmPoolBackend.from_warmpool("my-template") as backend:
            result = backend.execute("python script.py")
    """

    def __init__(
        self,
        client: SandboxClient,
        root_dir: str = "/app",
        manage_client: bool = False,
        warmpool_name: Optional[str] = None,
    ) -> None:
        """Initialize the warmpool backend.

        Args:
            client: A configured SandboxClient instance.
            root_dir: Virtual root for file operations.
            manage_client: If True, the backend manages the client lifecycle.
            warmpool_name: Optional explicit warmpool name (reserved for future use).
        """
        super().__init__(client, root_dir, manage_client)
        self._warmpool_name = warmpool_name

    @classmethod
    def from_warmpool(
        cls,
        template_name: str,
        namespace: str = "default",
        warmpool_name: Optional[str] = None,
        **kwargs: Any,
    ) -> "WarmPoolBackend":
        """Create backend that expects warmpool adoption.

        Currently equivalent to from_template() since adoption is automatic
        when a warmpool exists for the template. The explicit warmpool_name
        parameter is reserved for future use when the CRD supports explicit
        warmpool selection.

        Args:
            template_name: SandboxTemplate name (must match warmpool's templateRef).
            namespace: Kubernetes namespace.
            warmpool_name: Optional explicit warmpool name (reserved for future use).
            **kwargs: Additional SandboxClient args.

        Returns:
            WarmPoolBackend configured for warmpool usage.
        """
        client = SandboxClient(
            template_name=template_name,
            namespace=namespace,
            **kwargs,
        )
        return cls(client, manage_client=True, warmpool_name=warmpool_name)

    def get_adoption_info(self) -> Dict[str, Optional[Union[str, bool]]]:
        """Get information about warmpool adoption.

        Returns a dict with warmpool-related metadata. Currently returns
        the warmpool name if specified; future versions will query the
        claim status for actual adoption details.

        Note: The 'from_warmpool' key indicates whether the warmpool_name
        parameter was specified, NOT whether actual warmpool adoption occurred.

        Returns:
            Dict with warmpool adoption information.
        """
        return {
            "warmpool_name": self._warmpool_name,
            "from_warmpool": self._warmpool_name is not None,
        }
