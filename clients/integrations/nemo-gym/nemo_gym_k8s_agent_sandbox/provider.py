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
"""Agent Sandbox provider for NVIDIA NeMo Gym (kubernetes-sigs/agent-sandbox).

Sandboxes are checked out of a SandboxWarmPool through a SandboxClaim, so create
latency is claim-bind latency rather than pod-start latency. The pool's
SandboxTemplate — not ``SandboxSpec.image`` — decides the image; images are routed
to pools through ``create.image_warmpools`` (see ``AgentSandboxCreateConfig``).

The provider wraps the async ``k8s-agent-sandbox`` client. Two properties of the
in-sandbox runtime API shape everything here:

- The runtime's ``/execute`` endpoint tokenizes the command with ``shlex.split``
  and runs it WITHOUT a shell, so every command is sent as
  ``<exec_shell> -c '<script>'``; ``cwd``/``env`` are folded into the script.
- The file API only accepts ``..``-free relative paths, rooted at the runtime
  server's working directory (the same directory ``/execute`` runs in). Absolute
  ``upload_file``/``download_file`` paths are honored by staging through a
  relative temp name and copying with ``exec``.
"""

import asyncio
import logging
import math
import posixpath
import re
import shlex
import uuid
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from nemo_gym.sandbox.providers.base import (
    SandboxCreateError,
    SandboxCreateVerificationError,
    SandboxExecResult,
    SandboxHandle,
    SandboxSpec,
    SandboxStatus,
)


LOGGER = logging.getLogger(__name__)

MANAGED_BY_LABEL = "app.kubernetes.io/managed-by"
MANAGED_BY_VALUE = "nemo-gym"
READY_PROBE_COMMAND = "printf agent-sandbox-ready"
READY_PROBE_EXPECTED = "agent-sandbox-ready"
# Non-process sentinel for runtime failures (transport errors, timeouts), mirroring
# the convention of the other NeMo Gym providers. Also used as the `cd` guard exit
# code inside wrapped scripts so a bad cwd cannot fall through to the command.
SANDBOX_RUNTIME_RETURN_CODE = 125
# Staging names are flat (no subdirectory): the reference runtime's upload handler
# does not create parent directories, so a nested staging path would 500.
STAGING_PREFIX = ".nemo-gym-"

_ENV_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")

_CONNECTION_MODES = ("in_cluster", "gateway", "direct")


class AgentSandboxCreateError(SandboxCreateError):
    """Raised when a SandboxClaim cannot be created or bound."""


class AgentSandboxCreateVerificationError(SandboxCreateVerificationError):
    """Raised when a claimed sandbox fails its exec readiness probe."""


def _require_client() -> None:
    try:
        import k8s_agent_sandbox.async_sandbox_client  # noqa: F401
    except ImportError as e:
        # ImportError, not just ModuleNotFoundError: a failed `from ... import`
        # inside the async client module (missing [async] extras) raises the former.
        raise ImportError(
            "The k8s-agent-sandbox async client is required for the agent_sandbox "
            "sandbox provider. Install k8s-agent-sandbox[async] (it ships with the "
            "nemo-gym-k8s-agent-sandbox distribution) before using "
            "sandbox provider name agent_sandbox."
        ) from e


def _coerce_config(value: Any, config_cls: type[Any]) -> Any:
    if value is None:
        return config_cls()
    if isinstance(value, config_cls):
        return value
    if isinstance(value, Mapping):
        return config_cls(**value)
    raise TypeError(f"{config_cls.__name__} must be a mapping or {config_cls.__name__} instance")


def _is_timeout_failure(exc: BaseException) -> bool:
    import httpx

    return isinstance(exc, (httpx.TimeoutException, TimeoutError))


def _is_runtime_failure(exc: BaseException) -> bool:
    """Whether an exception came from the transport/cluster rather than caller code.

    Covers the HTTP path to the in-sandbox runtime (httpx), the Kubernetes API
    (kubernetes_asyncio), and the client's SandboxError hierarchy. Deliberately
    does NOT match bare RuntimeError: a programming bug must propagate, not be
    misclassified as a sandbox failure. The one known producer of bare
    RuntimeError (the command executor's malformed-response error) is handled at
    its call site in ``_run_wrapped``.
    """
    import httpx
    from k8s_agent_sandbox.exceptions import SandboxError
    from kubernetes_asyncio.client.rest import ApiException

    return isinstance(exc, (httpx.HTTPError, ApiException, SandboxError, ConnectionError, TimeoutError))


@dataclass(frozen=True)
class AgentSandboxConnectionConfig:
    """How to reach the Kubernetes API and the runtime server inside each sandbox.

    ``mode`` selects the client's connection config class:

    - ``in_cluster``: talk to the sandbox pod IP directly (NeMo Gym resource
      servers running inside the same cluster — the recommended RL setup).
    - ``gateway``: route through a Gateway API resource fronting the sandbox
      router (out-of-cluster clients).
    - ``direct``: a fixed URL to an already-reachable sandbox router.

    Kubernetes API credentials come from the standard kubeconfig / in-cluster
    service account resolution done by the client's K8s helper.
    """

    mode: str = "in_cluster"
    server_port: int = 8888
    api_url: str | None = None
    gateway_name: str | None = None
    gateway_namespace: str = "default"
    gateway_ready_timeout_s: int = 180

    def __post_init__(self) -> None:
        if self.mode not in _CONNECTION_MODES:
            raise ValueError(f"connection.mode must be one of {_CONNECTION_MODES}, got {self.mode!r}")
        if self.server_port < 1 or self.server_port > 65535:
            raise ValueError("connection.server_port must be a valid TCP port")
        if self.mode == "direct" and not self.api_url:
            raise ValueError("connection.api_url is required when connection.mode is 'direct'")
        if self.mode == "gateway" and not self.gateway_name:
            raise ValueError("connection.gateway_name is required when connection.mode is 'gateway'")

    def build(self) -> Any:
        from k8s_agent_sandbox.models import (
            SandboxDirectConnectionConfig,
            SandboxGatewayConnectionConfig,
            SandboxInClusterConnectionConfig,
        )

        if self.mode == "direct":
            return SandboxDirectConnectionConfig(api_url=self.api_url, server_port=self.server_port)
        if self.mode == "gateway":
            return SandboxGatewayConnectionConfig(
                gateway_name=self.gateway_name,
                gateway_namespace=self.gateway_namespace,
                gateway_ready_timeout=self.gateway_ready_timeout_s,
                server_port=self.server_port,
            )
        return SandboxInClusterConnectionConfig(server_port=self.server_port)


@dataclass(frozen=True)
class AgentSandboxCreateConfig:
    """Claim creation settings.

    Warm pool resolution order for each sandbox: ``provider_options.warmpool`` >
    ``image_warmpools[spec.image]`` > ``warmpool``. ``image_warmpools`` keys are
    exact image references as agents request them (the pool's SandboxTemplate must
    actually run that image — the provider cannot verify this).
    """

    warmpool: str | None = None
    namespace: str = "default"
    image_warmpools: dict[str, str] = field(default_factory=dict)
    ready_timeout_s: float = 180

    def __post_init__(self) -> None:
        if self.ready_timeout_s <= 0:
            raise ValueError("create.ready_timeout_s must be > 0")
        if not isinstance(self.image_warmpools, Mapping):
            raise TypeError("create.image_warmpools must be a mapping of image -> warmpool name")
        object.__setattr__(
            self,
            "image_warmpools",
            {str(k): str(v) for k, v in self.image_warmpools.items()},
        )


@dataclass(frozen=True)
class AgentSandboxExecConfig:
    default_timeout_s: float | None = 180
    # File staging execs (mkdir/cp/rm) and file API reads/writes use this timeout
    # instead of default_timeout_s so large artifacts are not capped by the exec default.
    transfer_timeout_s: float = 300
    exec_shell: str = "/bin/sh"

    def __post_init__(self) -> None:
        if self.default_timeout_s is not None and self.default_timeout_s <= 0:
            raise ValueError("exec.default_timeout_s must be > 0")
        if self.transfer_timeout_s <= 0:
            raise ValueError("exec.transfer_timeout_s must be > 0")
        if not self.exec_shell:
            raise ValueError("exec.exec_shell must be a non-empty shell name/path")


@dataclass(frozen=True)
class AgentSandboxProbeConfig:
    """Exec readiness probe run after the claim binds.

    A bound claim means the pod is Ready, not that the runtime HTTP server is
    accepting requests (or that a gateway route has propagated), so the provider
    polls a cheap exec until it succeeds ``stable_count`` times. ``deadline_s``
    bounds the whole poll; ``None`` polls without a time limit.
    """

    command: str | None = READY_PROBE_COMMAND
    expected_stdout: str | None = READY_PROBE_EXPECTED
    timeout_s: int = 30
    deadline_s: float | None = 60
    stable_count: int = 1
    stable_delay_s: float = 1.0

    def __post_init__(self) -> None:
        if self.command is not None and self.timeout_s <= 0:
            raise ValueError("probe.timeout_s must be > 0")
        if self.deadline_s is not None and self.deadline_s <= 0:
            raise ValueError("probe.deadline_s must be > 0")
        if self.stable_count < 1:
            raise ValueError("probe.stable_count must be >= 1")
        if self.stable_delay_s < 0:
            raise ValueError("probe.stable_delay_s must be >= 0")


@dataclass(frozen=True)
class AgentSandboxOperationsConfig:
    # Deleting a claim returns once the apiserver accepts it; pod teardown (and the
    # warm pool backfilling the freed slot) continues asynchronously. Waiting is off
    # by default to keep RL rollout churn fast — enable it when tests need
    # deterministic teardown.
    close_wait_deleted: bool = False
    close_timeout_s: float = 60
    poll_interval_s: float = 1.0
    # When True, the underlying client registers an atexit hook that deletes any
    # still-tracked claims on interpreter exit. Off by default: NeMo Gym owns the
    # sandbox lifecycle, and claim TTLs (spec.ttl_s) are the crash-safety net.
    atexit_cleanup: bool = False

    def __post_init__(self) -> None:
        if self.close_timeout_s <= 0:
            raise ValueError("operations.close_timeout_s must be > 0")
        if self.poll_interval_s <= 0:
            raise ValueError("operations.poll_interval_s must be > 0")


@dataclass(frozen=True)
class AgentSandboxProviderOptions:
    """Validated per-sandbox options carried in ``SandboxSpec.provider_options``."""

    warmpool: str | None = None
    namespace: str | None = None
    pod_labels: dict[str, str] = field(default_factory=dict)
    pod_annotations: dict[str, str] = field(default_factory=dict)
    volume_claim_templates: list[dict] = field(default_factory=list)

    @classmethod
    def from_mapping(cls, options: Mapping[str, Any]) -> "AgentSandboxProviderOptions":
        allowed = set(cls.__dataclass_fields__)
        unknown = set(options) - allowed
        if unknown:
            raise ValueError(
                f"Unknown agent_sandbox provider_options keys: {sorted(unknown)}. Allowed keys: {sorted(allowed)}"
            )
        for key in ("pod_labels", "pod_annotations"):
            value = options.get(key)
            if value is not None and not isinstance(value, Mapping):
                raise TypeError(f"provider_options[{key!r}] must be a mapping, got {type(value).__name__}")
        vcts = options.get("volume_claim_templates") or []
        if not isinstance(vcts, (list, tuple)):
            raise TypeError(
                f"provider_options['volume_claim_templates'] must be a list, got {type(vcts).__name__}"
            )
        return cls(
            warmpool=str(options["warmpool"]) if options.get("warmpool") else None,
            namespace=str(options["namespace"]) if options.get("namespace") else None,
            pod_labels={str(k): str(v) for k, v in (options.get("pod_labels") or {}).items()},
            pod_annotations={str(k): str(v) for k, v in (options.get("pod_annotations") or {}).items()},
            volume_claim_templates=[dict(v) for v in vcts],
        )


@dataclass
class _AgentSandboxInstance:
    claim_name: str
    sandbox_name: str
    namespace: str
    sandbox: Any  # k8s_agent_sandbox.async_sandbox.AsyncSandbox
    env: dict[str, str] = field(default_factory=dict)
    workdir: str | None = None


class AgentSandboxProvider:
    """Sandbox provider backed by kubernetes-sigs/agent-sandbox warm pools."""

    name = "agent_sandbox"

    def __init__(
        self,
        *,
        connection: AgentSandboxConnectionConfig | Mapping[str, Any] | None = None,
        create: AgentSandboxCreateConfig | Mapping[str, Any] | None = None,
        # `exec` shadows the builtin, but the parameter name IS the provider config
        # contract: NeMo Gym passes the YAML `exec:` block by keyword, and NeMo
        # Gym's own providers (e.g. openshell) use the same name.
        exec: AgentSandboxExecConfig | Mapping[str, Any] | None = None,  # noqa: A002
        probe: AgentSandboxProbeConfig | Mapping[str, Any] | None = None,
        operations: AgentSandboxOperationsConfig | Mapping[str, Any] | None = None,
    ) -> None:
        self._connection = _coerce_config(connection, AgentSandboxConnectionConfig)
        self._create_config = _coerce_config(create, AgentSandboxCreateConfig)
        self._exec_config = _coerce_config(exec, AgentSandboxExecConfig)
        self._probe = _coerce_config(probe, AgentSandboxProbeConfig)
        self._operations = _coerce_config(operations, AgentSandboxOperationsConfig)
        _require_client()
        # Built lazily on first use: AsyncSandboxClient owns kubernetes_asyncio
        # resources that bind to the running event loop, so it cannot be shared at
        # module scope the way providers with loop-free SDK clients share theirs.
        self._client: Any = None
        self._client_lock = asyncio.Lock()
        self._closed = False

    async def _get_client(self) -> Any:
        if self._closed:
            raise RuntimeError("agent_sandbox provider is closed")
        async with self._client_lock:
            # Re-check under the lock: aclose() may have run while we waited, and
            # building a client past that point would leak it.
            if self._closed:
                raise RuntimeError("agent_sandbox provider is closed")
            if self._client is None:
                import inspect

                from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient

                kwargs: dict[str, Any] = {"connection_config": self._connection.build()}
                # The `cleanup` parameter landed after v0.5.4 (#859). Released clients
                # without it never register the atexit hook, which is exactly the
                # atexit_cleanup=False behavior; only an explicit opt-in needs it.
                if "cleanup" in inspect.signature(AsyncSandboxClient.__init__).parameters:
                    kwargs["cleanup"] = self._operations.atexit_cleanup
                elif self._operations.atexit_cleanup:
                    raise RuntimeError(
                        "operations.atexit_cleanup=true requires a k8s-agent-sandbox release "
                        "with AsyncSandboxClient(cleanup=...) support"
                    )
                self._client = AsyncSandboxClient(**kwargs)
            return self._client

    # ------------------------------------------------------------------ create

    def _resolve_warmpool(self, spec: SandboxSpec, options: AgentSandboxProviderOptions) -> str:
        if options.warmpool:
            return options.warmpool
        if spec.image:
            pool = self._create_config.image_warmpools.get(spec.image)
            if pool:
                return pool
        if self._create_config.warmpool:
            if spec.image:
                LOGGER.warning(
                    "spec.image=%r has no create.image_warmpools entry; using the default warm pool "
                    "%r, whose SandboxTemplate decides the actual image.",
                    spec.image,
                    self._create_config.warmpool,
                )
            return self._create_config.warmpool
        raise AgentSandboxCreateError(
            f"No warm pool for image={spec.image!r}: set provider_options.warmpool, add the image to "
            "create.image_warmpools, or configure create.warmpool as a default."
        )

    def _warn_unmapped_spec_fields(self, spec: SandboxSpec) -> None:
        resources = spec.resources
        ignored = [
            key
            for key, value in (
                ("cpu", resources.cpu),
                ("memory_mib", resources.memory_mib),
                ("disk_gib", resources.disk_gib),
                ("gpu", resources.gpu),
                ("gpu_type", resources.gpu_type),
            )
            if value is not None
        ]
        if ignored:
            LOGGER.warning(
                "%s resource requests are not mapped by the agent_sandbox provider; resources come "
                "from the warm pool's SandboxTemplate.",
                ", ".join(ignored),
            )

    async def create(self, spec: SandboxSpec) -> SandboxHandle:
        """Claim a sandbox from a warm pool, wait for the bind, then probe exec readiness.

        ``spec.ttl_s`` maps to the claim's lifecycle ``shutdownTime`` (ceil to whole
        seconds), so the controller deletes expired sandboxes even if this process
        dies. ``spec.metadata`` becomes SandboxClaim labels and must satisfy
        Kubernetes label syntax. ``spec.entrypoint`` is unsupported: the pool's
        SandboxTemplate owns the pod command. ``spec.files`` are not handled here —
        NeMo Gym's sandbox API uploads them via ``upload_file`` after ``create``
        returns. A claim that binds but fails verification is deleted before the
        error propagates.
        """
        if spec.entrypoint:
            raise AgentSandboxCreateError(
                "spec.entrypoint is not supported by the agent_sandbox provider; the warm pool's "
                "SandboxTemplate owns the pod command"
            )
        self._warn_unmapped_spec_fields(spec)
        if spec.ttl_s is not None and spec.ttl_s <= 0:
            raise AgentSandboxCreateError(f"spec.ttl_s must be > 0, got {spec.ttl_s!r}")
        if spec.ready_timeout_s is not None and spec.ready_timeout_s <= 0:
            raise AgentSandboxCreateError(f"spec.ready_timeout_s must be > 0, got {spec.ready_timeout_s!r}")
        options = AgentSandboxProviderOptions.from_mapping(spec.provider_options)
        warmpool = self._resolve_warmpool(spec, options)
        namespace = options.namespace or self._create_config.namespace
        ready_timeout_s = spec.ready_timeout_s or self._create_config.ready_timeout_s
        shutdown_after_seconds = math.ceil(spec.ttl_s) if spec.ttl_s is not None else None
        # Marker label goes last so user metadata cannot clobber it.
        labels = {**{str(k): str(v) for k, v in spec.metadata.items()}, MANAGED_BY_LABEL: MANAGED_BY_VALUE}

        client = await self._get_client()
        try:
            sandbox = await client.create_sandbox(
                warmpool,
                namespace=namespace,
                sandbox_ready_timeout=math.ceil(ready_timeout_s),
                labels=labels,
                shutdown_after_seconds=shutdown_after_seconds,
                volume_claim_templates=options.volume_claim_templates or None,
                pod_labels=options.pod_labels or None,
                pod_annotations=options.pod_annotations or None,
            )
        except ValueError as e:
            # Bad warm pool name / label syntax — caller error, keep it precise.
            raise AgentSandboxCreateError(f"invalid sandbox claim for warm pool {warmpool!r}: {e}") from e
        except Exception as e:
            if not _is_runtime_failure(e):
                raise
            raise AgentSandboxCreateError(
                f"SandboxClaim against warm pool {warmpool!r} in namespace {namespace!r} failed: {e}"
            ) from e

        handle = SandboxHandle(
            sandbox_id=sandbox.claim_name,
            provider_name=self.name,
            raw=_AgentSandboxInstance(
                claim_name=sandbox.claim_name,
                sandbox_name=sandbox.sandbox_id,
                namespace=namespace,
                sandbox=sandbox,
                env=dict(spec.env),
                workdir=spec.workdir,
            ),
        )
        try:
            # spec.files is NOT seeded here: NeMo Gym's sandbox API uploads them
            # through provider.upload_file() after create() returns.
            await self._verify_created_handle(handle)
        except (Exception, asyncio.CancelledError):
            await asyncio.shield(self._cleanup_failed_create_handle(handle))
            raise
        return handle

    async def _verify_created_handle(self, handle: SandboxHandle) -> None:
        """Poll the readiness probe until it passes ``stable_count`` times or the deadline elapses.

        The probe runs WITHOUT the cwd/env wrapping that ``exec`` applies: it tests
        runtime reachability only, and a ``spec.workdir`` that does not exist until
        the agent creates it must not fail an otherwise healthy sandbox.
        """
        probe = self._probe
        if probe.command is None:
            return
        inst: _AgentSandboxInstance = handle.raw
        wrapped = f"{self._exec_config.exec_shell} -c {shlex.quote(probe.command)}"
        loop = asyncio.get_running_loop()
        deadline = loop.time() + probe.deadline_s if probe.deadline_s is not None else None
        consecutive = 0
        attempts = 0
        last_detail = "no probe attempt completed"
        while True:
            result = await self._run_wrapped(inst, wrapped, probe.timeout_s)
            attempts += 1
            passed = result.return_code == 0 and (
                probe.expected_stdout is None or probe.expected_stdout in (result.stdout or "")
            )
            if passed:
                consecutive += 1
                last_detail = f"probe passed {consecutive}/{probe.stable_count} consecutive times"
                if consecutive >= probe.stable_count:
                    return
            else:
                consecutive = 0
                last_detail = f"return_code={result.return_code}, stderr={(result.stderr or '').strip()!r}"
                # Without a deadline the poll can run forever; surface progress so a
                # never-ready sandbox does not block create() silently.
                if deadline is None and attempts % 30 == 0:
                    LOGGER.warning(
                        "sandbox %r readiness probe still failing after %d attempts (no deadline configured): %s",
                        handle.sandbox_id,
                        attempts,
                        last_detail,
                    )
            if deadline is not None and loop.time() >= deadline:
                raise AgentSandboxCreateVerificationError(
                    f"sandbox {handle.sandbox_id!r} did not pass readiness probe within "
                    f"{probe.deadline_s:g}s: {last_detail}"
                )
            if probe.stable_delay_s > 0:
                await asyncio.sleep(probe.stable_delay_s)

    async def _cleanup_failed_create_handle(self, handle: SandboxHandle) -> None:
        inst: _AgentSandboxInstance = handle.raw
        try:
            await inst.sandbox.terminate()
        except Exception as e:
            LOGGER.warning(
                "Failed to delete half-created sandbox claim %r in namespace %r; "
                "the claim TTL (if set) is the remaining safety net: %s",
                inst.claim_name,
                inst.namespace,
                e,
            )

    # -------------------------------------------------------------------- exec

    def _wrap_command(self, command: str, *, cwd: str | None, env: Mapping[str, str]) -> str:
        """Fold cwd/env into a shell script and wrap it for the runtime's shlex+no-shell exec."""
        lines = []
        for key, value in env.items():
            if not _ENV_NAME_RE.match(key):
                raise ValueError(f"Invalid environment variable name for sandbox exec: {key!r}")
            lines.append(f"export {key}={shlex.quote(str(value))}")
        if cwd:
            lines.append(f"cd {shlex.quote(cwd)} || exit {SANDBOX_RUNTIME_RETURN_CODE}")
        lines.append(command)
        return f"{self._exec_config.exec_shell} -c {shlex.quote('\n'.join(lines))}"

    async def _run_wrapped(
        self, inst: _AgentSandboxInstance, wrapped_command: str, timeout_s: int | float | None
    ) -> SandboxExecResult:
        commands = inst.sandbox.commands
        if commands is None:
            return SandboxExecResult(
                stdout=None,
                stderr=f"sandbox connection for claim {inst.claim_name!r} is closed",
                return_code=SANDBOX_RUNTIME_RETURN_CODE,
                error_type="sandbox",
            )
        effective_timeout = timeout_s if timeout_s is not None else self._exec_config.default_timeout_s
        timeout = max(1, math.ceil(effective_timeout)) if effective_timeout is not None else None
        try:
            result = await commands.run(wrapped_command, timeout=timeout)
        except Exception as e:
            # The runtime server has no server-side timeout: a timed-out HTTP request
            # abandons the request but the process may keep running in the pod.
            if _is_timeout_failure(e):
                return SandboxExecResult(
                    stdout=None, stderr=str(e), return_code=SANDBOX_RUNTIME_RETURN_CODE, error_type="timeout"
                )
            # type(e) is checked exactly: the executor raises bare RuntimeError for
            # malformed runtime responses, which is a sandbox failure at this call
            # site only — RuntimeError subclasses from elsewhere still propagate.
            if _is_runtime_failure(e) or type(e) is RuntimeError:
                return SandboxExecResult(
                    stdout=None, stderr=str(e), return_code=SANDBOX_RUNTIME_RETURN_CODE, error_type="sandbox"
                )
            raise
        return SandboxExecResult(stdout=result.stdout, stderr=result.stderr, return_code=result.exit_code)

    async def exec(
        self,
        handle: SandboxHandle,
        command: str,
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        timeout_s: int | float | None = None,
        user: str | int | None = None,
    ) -> SandboxExecResult:
        """Run ``<exec_shell> -c <script>`` in the sandbox; never raises for command failure.

        ``cwd`` falls back to the sandbox spec's ``workdir``; a failing ``cd`` exits
        with the runtime sentinel code before the command runs. ``user`` is ignored
        with a warning — the runtime API always executes as the pod's user.
        """
        inst: _AgentSandboxInstance = handle.raw
        if user is not None:
            LOGGER.warning(
                "The agent_sandbox provider cannot run commands as user=%r; the sandbox "
                "runtime API has no user field. Running as the pod's user.",
                user,
            )
        merged_env = {str(k): str(v) for k, v in inst.env.items()}
        if env:
            merged_env.update({str(k): str(v) for k, v in env.items()})
        workdir = cwd if cwd is not None else inst.workdir
        wrapped = self._wrap_command(command, cwd=workdir, env=merged_env)
        return await self._run_wrapped(inst, wrapped, timeout_s)

    # ------------------------------------------------------------------- files

    async def _transfer_exec(self, inst: _AgentSandboxInstance, script: str) -> SandboxExecResult:
        # Staging paths are relative to the runtime server's working directory, which
        # is also where /execute runs — so no cd/env wrapping here, only shell quoting.
        wrapped = f"{self._exec_config.exec_shell} -c {shlex.quote(script)}"
        return await self._run_wrapped(inst, wrapped, self._exec_config.transfer_timeout_s)

    async def _put_bytes(self, handle: SandboxHandle, data: bytes, target_path: str) -> None:
        inst: _AgentSandboxInstance = handle.raw
        files = inst.sandbox.files
        if files is None:
            raise RuntimeError(f"sandbox connection for claim {inst.claim_name!r} is closed")
        staging = STAGING_PREFIX + uuid.uuid4().hex
        await files.write(staging, data, timeout=max(1, math.ceil(self._exec_config.transfer_timeout_s)))
        quoted_target = shlex.quote(target_path)
        parent = posixpath.dirname(target_path)
        script_parts = []
        if parent:
            script_parts.append(f"mkdir -p {shlex.quote(parent)}")
        # cp+rm rather than mv: the target may live on a different mount (e.g. a
        # volumeClaimTemplate volume), where rename(2) fails cross-device.
        script_parts.append(f"cp {shlex.quote(staging)} {quoted_target}")
        script_parts.append(f"rm -f {shlex.quote(staging)}")
        result = await self._transfer_exec(inst, " && ".join(script_parts))
        if result.return_code != 0:
            await self._transfer_exec(inst, f"rm -f {shlex.quote(staging)}")
            raise RuntimeError(
                f"agent_sandbox upload to {target_path!r} failed (code={result.return_code}): "
                f"{(result.stderr or '').strip()}"
            )

    async def upload_file(self, handle: SandboxHandle, source_path: Path, target_path: str) -> None:
        """Upload one local file, staging through a relative temp name to allow absolute targets."""
        data = await asyncio.to_thread(Path(source_path).read_bytes)
        await self._put_bytes(handle, data, target_path)

    async def download_file(self, handle: SandboxHandle, source_path: str, target_path: Path) -> None:
        """Download one sandbox file (staged copy first, so absolute source paths work)."""
        inst: _AgentSandboxInstance = handle.raw
        files = inst.sandbox.files
        if files is None:
            raise RuntimeError(f"sandbox connection for claim {inst.claim_name!r} is closed")
        staging = STAGING_PREFIX + uuid.uuid4().hex
        result = await self._transfer_exec(inst, f"cp {shlex.quote(source_path)} {shlex.quote(staging)}")
        if result.return_code != 0:
            raise RuntimeError(
                f"agent_sandbox download from {source_path!r} failed (code={result.return_code}): "
                f"{(result.stderr or '').strip()}"
            )
        try:
            data = await files.read(staging, timeout=max(1, math.ceil(self._exec_config.transfer_timeout_s)))
        finally:
            cleanup = await self._transfer_exec(inst, f"rm -f {shlex.quote(staging)}")
            if cleanup.return_code != 0:
                LOGGER.warning(
                    "Failed to remove staging file %r in sandbox %r: %s",
                    staging,
                    inst.claim_name,
                    (cleanup.stderr or "").strip(),
                )
        target_path = Path(target_path)
        target_path.parent.mkdir(parents=True, exist_ok=True)
        await asyncio.to_thread(target_path.write_bytes, data)

    # --------------------------------------------------------------- lifecycle

    async def status(self, handle: SandboxHandle) -> SandboxStatus:
        """Sandbox Ready condition via the Kubernetes API (missing -> STOPPED; API failure -> UNKNOWN)."""
        inst: _AgentSandboxInstance = handle.raw
        try:
            state, _message = await inst.sandbox.status()
        except Exception as e:
            if _is_runtime_failure(e):
                return SandboxStatus.UNKNOWN
            raise
        if state == "SandboxReady":
            return SandboxStatus.RUNNING
        if state == "SandboxNotFound":
            return SandboxStatus.STOPPED
        # "SandboxNotReady" covers both warm-up and degradation; the Ready condition
        # does not distinguish them, so report the optimistic phase.
        return SandboxStatus.STARTING

    async def close(self, handle: SandboxHandle) -> None:
        """Delete the SandboxClaim (already-gone counts as success)."""
        inst: _AgentSandboxInstance = handle.raw
        try:
            await inst.sandbox.terminate()
        except Exception as e:
            if not _is_runtime_failure(e):
                raise
            raise RuntimeError(
                f"agent_sandbox delete failed for claim {inst.claim_name!r} in namespace "
                f"{inst.namespace!r}: {e}"
            ) from e
        # terminate() deletes the claim, but the client keeps tracking the handle in
        # its active-sandbox registry; delete_sandbox pops that entry (its internal
        # second terminate is an idempotent no-op), keeping the registry bounded
        # across long RL runs. Guarded on the raw attribute so a close() racing
        # aclose() cannot resurrect the client.
        if self._client is not None:
            await self._client.delete_sandbox(inst.claim_name, inst.namespace)
        if self._operations.close_wait_deleted:
            await self._wait_deleted(inst)

    async def _wait_deleted(self, inst: _AgentSandboxInstance) -> None:
        client = await self._get_client()
        loop = asyncio.get_running_loop()
        deadline = loop.time() + self._operations.close_timeout_s
        while True:
            gone = False
            try:
                gone = await client.k8s_helper.get_sandbox_claim(inst.claim_name, inst.namespace) is None
            except Exception as e:
                # Transient API failure: keep polling until the deadline.
                if not _is_runtime_failure(e):
                    raise
                LOGGER.debug("get_sandbox_claim failed while waiting for %r deletion: %s", inst.claim_name, e)
            if gone:
                return
            if loop.time() >= deadline:
                raise RuntimeError(
                    f"agent_sandbox claim {inst.claim_name!r} was not deleted within "
                    f"{self._operations.close_timeout_s:g}s"
                )
            await asyncio.sleep(self._operations.poll_interval_s)

    async def aclose(self) -> None:
        """Close the Kubernetes client and all tracked sandbox connections (sandboxes keep running)."""
        if self._closed:
            return
        self._closed = True
        async with self._client_lock:
            if self._client is not None:
                await self._client.close()
                self._client = None
