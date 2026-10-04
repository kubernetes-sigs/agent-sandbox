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

"""An in-process stand-in for ``k8s_agent_sandbox``'s async client.

The fake speaks the same surface the provider actually uses — ``commands.run``,
``files.read``/``files.write``, ``status()``, plus the client's create/get/delete — and
backs it with the local machine: "the pod is this host". That keeps the real
:class:`K8sHttpTransport` and :class:`K8sSandboxSession` code under test, including the
shell probes the SDK runs during ``start()``, without needing a cluster.

Deliberate fidelity choices:

* ``commands.run`` takes a command *string*, ``shlex.split``s it and execs the argv
  **without a shell** — exactly what the in-pod server does
  (``examples/python-runtime-sandbox/main.py``). The provider supplies its own ``sh -lc``
  wrapper when it wants a shell, so this round-trips the provider's ``shlex.join``.
* ``max_command_chars`` models ``MAX_ARG_STRLEN``: the kernel caps a *single* execve
  argument at 128 KiB, which is why base64 payloads have to be chunked.
* ``ExecutionResult`` carries ``str``, not ``bytes`` — the same lossiness as the JSON
  ``execute`` endpoint.
* ``files.write`` does **not** create parent directories, so a provider that forgets to
  ``mkdir`` first fails here the way it would against a real server.
* Failure injection (``fail_file_reads``, ``fail_file_writes``, ``fail_commands_matching``,
  ``fail_get_sandbox``, ``status_value``) exists so error-mapping paths can be driven
  deterministically instead of relying on incidental I/O failures.
"""

from __future__ import annotations

import asyncio
import errno
import os
import shlex
import uuid
from pathlib import Path

from k8s_agent_sandbox.exceptions import SandboxNotFoundError
from k8s_agent_sandbox.models import ExecutionResult


class FakeCommands:
    def __init__(self, sandbox: "FakeAsyncSandbox") -> None:
        self._sandbox = sandbox

    async def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        self._sandbox.assert_live()
        self._sandbox.commands_run.append(command)
        self._sandbox.commands_timeouts.append(timeout)

        if self._sandbox.fail_commands_matching is not None and (
            self._sandbox.fail_commands_matching in command
        ):
            return ExecutionResult(stdout="", stderr="injected command failure", exit_code=1)

        # The in-pod server splits the command string and execs the argv directly.
        argv = shlex.split(command)
        self._sandbox.assert_argv_within_limits(argv)

        proc = await asyncio.create_subprocess_exec(
            *argv,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd="/",
            env={**os.environ, "HOME": str(self._sandbox.home)},
        )
        try:
            stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=timeout)
        except (asyncio.TimeoutError, TimeoutError):
            proc.kill()
            await proc.wait()
            raise

        return ExecutionResult(
            stdout=stdout.decode("utf-8", errors="replace"),
            stderr=stderr.decode("utf-8", errors="replace"),
            exit_code=proc.returncode if proc.returncode is not None else -1,
        )


class FakeFilesystem:
    def __init__(self, sandbox: "FakeAsyncSandbox") -> None:
        self._sandbox = sandbox

    async def read(
        self, path: str, timeout: int = 60, allow_unsafe_paths: bool = False
    ) -> bytes:
        self._sandbox.assert_live()
        _reject_absolute_without_allow_unsafe(path, allow_unsafe_paths)
        self._sandbox.files_read.append(path)
        if self._sandbox.fail_file_reads is not None:
            raise self._sandbox.fail_file_reads
        return Path(path).read_bytes()

    async def write(
        self,
        path: str,
        content: bytes | str,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,
    ) -> None:
        self._sandbox.assert_live()
        _reject_absolute_without_allow_unsafe(path, allow_unsafe_paths)
        self._sandbox.files_written.append(path)
        if self._sandbox.fail_file_writes is not None:
            raise self._sandbox.fail_file_writes
        if isinstance(content, str):
            content = content.encode("utf-8")
        target = Path(path)
        if not target.parent.is_dir():
            # Matches a server that will not implicitly create directories.
            raise FileNotFoundError(f"no such directory: {target.parent}")
        target.write_bytes(content)


class FakeAsyncSandbox:
    def __init__(self, claim_name: str, sandbox_id: str, namespace: str, home: Path) -> None:
        self.claim_name: str | None = claim_name
        self.sandbox_id = sandbox_id
        self.namespace = namespace
        self.home = home
        self.commands = FakeCommands(self)
        self.files = FakeFilesystem(self)
        self.terminated = False

        self.commands_run: list[str] = []
        self.commands_timeouts: list[int] = []
        self.files_read: list[str] = []
        self.files_written: list[str] = []

        # -- knobs -------------------------------------------------------------
        self.status_value = "SandboxReady"
        """Reported by :meth:`status` while the sandbox is alive."""

        self.status_error: BaseException | None = None
        """When set, :meth:`status` raises it instead of returning."""

        self.fail_file_reads: BaseException | None = None
        self.fail_file_writes: BaseException | None = None

        self.fail_commands_matching: str | None = None
        """Substring; matching commands report exit 1 instead of running.

        A command that the pod runs but that fails — `rm` on a read-only mount, say — is
        not reachable by arranging the local filesystem, since the fake runs the argv for
        real.
        """

        self.max_command_chars: int | None = None
        """Per-argument ceiling, mirroring the kernel's ``MAX_ARG_STRLEN`` (128 KiB)."""

        self.fail_terminate: BaseException | None = None
        """When set, :meth:`terminate` raises it — models a refused claim deletion."""

    @property
    def is_active(self) -> bool:
        return not self.terminated

    def assert_live(self) -> None:
        if self.terminated:
            raise SandboxNotFoundError(f"sandbox '{self.sandbox_id}' has been terminated")

    def assert_argv_within_limits(self, argv: list[str]) -> None:
        if self.max_command_chars is None:
            return
        for arg in argv:
            if len(arg) > self.max_command_chars:
                raise OSError(errno.E2BIG, "Argument list too long")

    def longest_command_argument(self) -> int:
        """Longest single execve argument this sandbox has been asked to run."""

        return max(
            (len(arg) for command in self.commands_run for arg in shlex.split(command)),
            default=0,
        )

    async def status(self) -> tuple[str, str]:
        if self.status_error is not None:
            raise self.status_error
        if self.terminated:
            return "SandboxNotFound", "Sandbox object not found in Kubernetes."
        return self.status_value, ""

    async def close_connection(self) -> None:
        return None

    async def terminate(self) -> None:
        if self.fail_terminate is not None:
            raise self.fail_terminate
        self.terminated = True


class FakeAsyncSandboxClient:
    """Stands in for ``AsyncSandboxClient``."""

    def __init__(self, home: Path) -> None:
        self._home = home
        self._sandboxes: dict[tuple[str, str], FakeAsyncSandbox] = {}
        self._warm_pools: dict[tuple[str, str], str] = {}
        self.created: list[dict[str, object]] = []
        self.fail_get_sandbox: BaseException | None = None
        """When set, :meth:`get_sandbox` raises it — models an API blip, not a deletion."""

    async def create_sandbox(
        self,
        warmpool: str,
        namespace: str = "default",
        sandbox_ready_timeout: int = 180,
        labels: dict[str, str] | None = None,
        *,
        shutdown_after_seconds: int | None = None,
        volume_claim_templates: list[dict] | None = None,
        pod_labels: dict[str, str] | None = None,
        pod_annotations: dict[str, str] | None = None,
    ) -> FakeAsyncSandbox:
        claim_name = f"sandbox-claim-{uuid.uuid4().hex[:8]}"
        sandbox_id = f"{warmpool}-{uuid.uuid4().hex[:5]}"
        self.created.append(
            {
                "warmpool": warmpool,
                "namespace": namespace,
                "sandbox_ready_timeout": sandbox_ready_timeout,
                "labels": labels,
                "shutdown_after_seconds": shutdown_after_seconds,
                "pod_labels": pod_labels,
            }
        )
        sandbox = FakeAsyncSandbox(claim_name, sandbox_id, namespace, self._home)
        self._sandboxes[(namespace, claim_name)] = sandbox
        self._warm_pools[(namespace, claim_name)] = warmpool
        return sandbox

    async def get_sandbox(
        self,
        claim_name: str,
        namespace: str = "default",
        resolve_timeout: int = 30,
        warmpool_name: str | None = None,
    ) -> FakeAsyncSandbox:
        if self.fail_get_sandbox is not None:
            raise self.fail_get_sandbox
        sandbox = self._sandboxes.get((namespace, claim_name))
        if sandbox is None or sandbox.terminated:
            raise SandboxNotFoundError(f"claim '{claim_name}' not found in '{namespace}'")
        existing_pool = self._warm_pools.get((namespace, claim_name))
        if warmpool_name is not None and existing_pool != warmpool_name:
            raise ValueError(
                f"SandboxClaim '{claim_name}' references warmpool '{existing_pool}', "
                f"not '{warmpool_name}'. Refusing to reattach."
            )
        return sandbox

    async def delete_sandbox(self, claim_name: str, namespace: str = "default") -> None:
        sandbox = self._sandboxes.pop((namespace, claim_name), None)
        self._warm_pools.pop((namespace, claim_name), None)
        if sandbox is None:
            return
        try:
            await sandbox.terminate()
        except Exception:
            # Modelled, not a bug: the real client logs deletion failures and returns
            # normally, which is why the provider deletes via terminate() instead.
            pass

    def drop_claim(self, claim_name: str, namespace: str = "default") -> None:
        """Simulate the claim disappearing underneath us (TTL expiry, node loss)."""
        self._sandboxes.pop((namespace, claim_name), None)
        self._warm_pools.pop((namespace, claim_name), None)


def _reject_absolute_without_allow_unsafe(path: str, allow_unsafe_paths: bool) -> None:
    # The real client's sanitizer strips a leading "/", so an absolute workspace path
    # only survives with allow_unsafe_paths=True. Enforce that here so the provider
    # cannot silently regress to server-relative paths.
    if not allow_unsafe_paths and path.startswith("/"):
        raise ValueError(f"absolute path requires allow_unsafe_paths: {path}")
