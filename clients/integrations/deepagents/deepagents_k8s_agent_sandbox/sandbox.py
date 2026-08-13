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

import logging
import posixpath
import textwrap
import shlex
from typing import Literal

from deepagents.backends.protocol import (
    ExecuteResponse,
    FileDownloadResponse,
    FileUploadResponse,
    FileOperationError,
    FILE_NOT_FOUND,
    PERMISSION_DENIED,
    IS_DIRECTORY,
)
from deepagents.backends.sandbox import (
    BaseSandbox,
)
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.sandbox import Sandbox

from .settings import (
    K8sAgentSandboxSettings,
)
from .lifecycle_manager import (
    K8sAgentSandboxLifecycleManager,
    ExistingSandboxInstanceLifecycleManager,
    ExistingSandboxClaimLifecycleManager,
    LabelScopedLifecycleManager,
)


logger = logging.getLogger(__name__)


class K8sAgentSandbox(BaseSandbox):
    """
    DeepAgents backend for k8s_agent_sandbox.

    Args:
        lifecycle_manager: the instance of the `K8sAgentSandboxLifecycleManager`
            which is responsible for managing the sandbox instance.
        root_dir: Sandbox's working directory.
        default_timeout_seconds: Default timeout for various operations.
        sandbox_api_accepts_relative_paths: When True, all paths that are passed to
            sandbox operations are converted to paths relative to `sandbox_api_cwd`.
            Since the DeepAgents backends normally use absolute paths, this option can be
            used to convert them into relative paths in case when sandbox operations only
            accept relative paths. This is a temporary workaround until the sandbox SDK provides
            this as a built-in feature.
        sandbox_api_cwd: The directory that the sandbox's file API (upload/download)
            resolves relative paths against. This is a property of the sandbox runtime
            image, unrelated to `root_dir`.
    """

    def __init__(
        self,
        lifecycle_manager: K8sAgentSandboxLifecycleManager,
        root_dir: str = "/app/work",
        default_timeout_seconds: int = 30 * 60,
        sandbox_api_cwd: str = "/app",
        sandbox_api_accepts_relative_paths: bool = True,

    ) -> None:
        self._lifecycle_manager = lifecycle_manager
        self._root_dir = root_dir
        self._default_timeout_seconds = default_timeout_seconds

        self._sandbox_api_accepts_relative_paths = sandbox_api_accepts_relative_paths
        self._sandbox_api_cwd = sandbox_api_cwd
        self._root_dir_initialized = False
        self._last_used_sandbox: Sandbox | None = None


    @classmethod
    def from_labels_scope(
        cls,
        client: SandboxClient,
        sandbox_settings: K8sAgentSandboxSettings,
        scope: dict[str, str],
        scope_labels_prefix: str = "deepagents.agents.x-k8s.io",
        root_dir: str = "/app/work",
        sandbox_api_cwd: str = "/app",
        sandbox_api_accepts_relative_paths: bool = True,
    ):
        """
        Create DeepAgents backend that re-uses sandbox with matching "scope" labels
        or creates a new one with these labels, so it can be reused later.

        Args:
            client: SandboxClient instance.
            sandbox_settings: Instance with sandbox settings.
            scope: Dictionary that represents labels that are applied to a sandbox claim.
                This can be used in a graph factory to specify user, thread or 
                assistant specific labels to isolate sandboxes from different runs.
            scope_labels_prefix: Prefix for scope label keys.
            root_dir: See :meth:`__init__` for details.
            sandbox_api_accepts_relative_paths: See :meth:`__init__` for details.
            sandbox_api_cwd: See :meth:`__init__` for details.
        """

        lifecycle_manager = LabelScopedLifecycleManager(
            client,
            sandbox_settings,
            scope,
            scope_labels_prefix,
        )

        return cls(
            lifecycle_manager,
            root_dir=root_dir,
            sandbox_api_cwd=sandbox_api_cwd,
            sandbox_api_accepts_relative_paths=sandbox_api_accepts_relative_paths,
        )
     
    @classmethod
    def from_existing_sandbox(
        cls,
        sandbox: Sandbox,
        root_dir: str = "/app/work",
        sandbox_api_cwd: str = "/app",
        sandbox_api_accepts_relative_paths: bool = True,
    ):
        """
        Create Sandbox backend from existing sandbox instance.

        Args:
            sandbox: Existing k8s_agent_sandbox.sandbox.Sandbox instance to use.
            root_dir: See :meth:`__init__` for details.
            sandbox_api_accepts_relative_paths: See :meth:`__init__` for details.
            sandbox_api_cwd: See :meth:`__init__` for details.
        """

        lifecycle_manager = ExistingSandboxInstanceLifecycleManager(sandbox)

        return cls(
            lifecycle_manager,
            root_dir=root_dir,
            sandbox_api_cwd=sandbox_api_cwd,
            sandbox_api_accepts_relative_paths=sandbox_api_accepts_relative_paths,
        )


    @classmethod
    def from_existing_claim_name(
        cls,
        client: SandboxClient,
        claim_name: str,
        namespace: str,
        root_dir: str = "/app/work",
        sandbox_api_cwd: str = "/app",
        sandbox_api_accepts_relative_paths: bool = True,
    ):
        """
        Create Sandbox backend from existing sandbox by finding it by its claim name.
        Args:
            client: SandboxClient instance.
            claim_name: Name of an existing sandbox claim to use.
            namespace: Namespace with a target sandbox claim.
            root_dir: See :meth:`__init__` for details.
            sandbox_api_accepts_relative_paths: See :meth:`__init__` for details.
            sandbox_api_cwd: See :meth:`__init__` for details.
        """


        lifecycle_manager = ExistingSandboxClaimLifecycleManager(
            client,
            claim_name,
            namespace,
        )
 
        return cls(
            lifecycle_manager,
            root_dir=root_dir,
            sandbox_api_cwd=sandbox_api_cwd,
            sandbox_api_accepts_relative_paths=sandbox_api_accepts_relative_paths,
        )

    def execute(
        self,
        command: str,
        *,
        timeout: int | None = None,
    ) -> ExecuteResponse:
        """
        Execute a shell command in the sandbox.
        """

        inner_shell_command = f"cd {shlex.quote(self._root_dir)} && {command}"
        wrapped = f"sh -c {shlex.quote(inner_shell_command)}"

        effective_timeout = timeout or self._default_timeout_seconds

        try:
            result = self._sandbox.commands.run(wrapped, timeout=effective_timeout)
        except Exception as e:
            logger.error("execute failed: %s", e)
            return ExecuteResponse(
                output=f"Error: {e}",
                exit_code=-1,
                truncated=False,
            )
        combined = result.stdout
        if result.stderr:
            combined = f"{combined}\n<stderr>\n{result.stderr}\n</stderr>" if combined else result.stderr
        return ExecuteResponse(
            output=combined,
            exit_code=result.exit_code,
            truncated=False,
        )

    def upload_files(self, files: list[tuple[str, bytes]]) -> list[FileUploadResponse]:
        """Upload multiple files to the sandbox.

        Args:
            files: Dict or iterable of (path, content) pairs.

        Returns:
            List of FileUploadResponse for each file.
        """
        responses = []
        for path, content in files:
            responses.append(self._upload_file(path, content))

        return responses

    def download_files(self, paths: list[str]) -> list[FileDownloadResponse]:
        """
        Download multiple files from the sandbox.
        """
        responses = []
        for path in paths:
            responses.append(self._download_file(path))
        return responses

    def _upload_file(self, path: str, content: bytes):
        try:
            self._ensure_parent_dir(path)
            try:
                self._assert_file_valid_state(path, "w")
            except FileNotFoundError:
                pass
            rel_path = self._get_path_relative_to_cwd_if_needed(path)
            self._sandbox.files.write(rel_path, content, allow_unsafe_paths=True)
            error = None
        except Exception as e:
            error = _map_file_error(e)
 
        return FileUploadResponse(path, error=error)

    def _download_file(self, path: str):
        try:
            self._assert_file_valid_state(path, "r")
            rel_path = self._get_path_relative_to_cwd_if_needed(path)
            content = self._sandbox.files.read(rel_path, allow_unsafe_paths=True)
            error = None
        except Exception as e:
            content = None
            error = _map_file_error(e)
 
        return FileDownloadResponse(path, content=content, error=error)

    def _ensure_parent_dir(self, path: str) -> None:
        parent = posixpath.dirname(path)
        if parent == "":
            return
        command = shlex.join(["mkdir", "-p", parent])
        result = self._sandbox.commands.run(command)
        if result.exit_code != 0:
            error_msg = result.stderr.strip() if result.stderr else f"mkdir failed with exit code {result.exit_code}"
            raise RuntimeError(f"Cannot create parent directory '{parent}': {error_msg}")

    def _assert_file_valid_state(
        self,
        path: str,
        access_mode: Literal["r", "w"],
    ):
        """Run the shell command to validate the state of a target file."""
    
        quoted_path = shlex.quote(path)
        cmd = textwrap.dedent(
            f"""
            if [ ! -e {quoted_path} ]; then echo missing; exit 0; fi;
            if [ -d {quoted_path} ]; then echo directory; exit 0; fi;
            if [ -{access_mode} {quoted_path} ]; then echo file; else echo denied; fi
            """
        )

        result = self._sandbox.commands.run(f"sh -c {shlex.quote(cmd)}")
        output = result.stdout.strip()

        if result.exit_code != 0:
            raise RuntimeError(f"Cannot get file state. Error: {result.stderr}")

        if output == "file":
            return

        if output == "missing":
            raise FileNotFoundError(f"File {path} is not found.")
        elif output == "directory":
            raise IsADirectoryError(f"Path {path} is a directory.")
        elif output == "denied":
            raise PermissionError(f"Cannot access file {path}.")
        else:
            raise RuntimeError(f"Unknown file state: {output}")

    @property
    def _sandbox(self):
        sandbox = self._lifecycle_manager.get_sandbox()
        self._initialize_root_dir(sandbox)
        self._last_used_sandbox = sandbox
        return sandbox

    def _initialize_root_dir(self, sandbox: Sandbox):
        """
        Create a root directory in case it does not exist.
        """
        if self._root_dir_initialized and self._last_used_sandbox is sandbox:
            return

        command = f"mkdir -p {shlex.quote(self._root_dir)}"
        result = sandbox.commands.run(f"sh -c {shlex.quote(command)}")

        if result.exit_code == 0:
            self._root_dir_initialized = True
            return

        raise RuntimeError(
            f"Cannot create working directory {self._root_dir}. Error: {result.stderr}"
        )

    @property
    def id(self) -> str:
        """
        Return a namespace-qualified sandbox identifier.
        """

        sandbox = self._lifecycle_manager.get_sandbox()
        return f"{sandbox.namespace}/{sandbox.claim_name}"

    def _get_path_relative_to_cwd_if_needed(self, path: str) -> str:
        if self._sandbox_api_accepts_relative_paths and posixpath.isabs(path):
            return posixpath.relpath(path, self._sandbox_api_cwd)

        return path


def _map_file_error(error: Exception) -> FileOperationError | str:
    """
    Map a provider filesystem failure to a Deep Agents file error.
    """
    if isinstance(error, PermissionError):
        return PERMISSION_DENIED
    if isinstance(error, IsADirectoryError):
        return IS_DIRECTORY
    if isinstance(error, FileNotFoundError):
        return FILE_NOT_FOUND

    return str(error)

