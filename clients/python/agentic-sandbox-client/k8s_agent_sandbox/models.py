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

"""Configuration and wire-format models for the Python SDK."""

import re
import warnings
from datetime import datetime, timezone
from typing import Annotated, Any, Literal, Optional, Union
from pydantic import AfterValidator, BaseModel, ConfigDict, Field, ModelWrapValidatorHandler, SecretStr, ValidationError, field_validator, model_validator
from pydantic_core import InitErrorDetails

_ENV_VAR_NAME_RE = re.compile(r"^[-._a-zA-Z][-._a-zA-Z0-9]*$")
_RESERVED_HEADER_PREFIX = "x-sandbox-"
_DEPRECATED_IN_CLUSTER_MODES = {
    "service-dns": "in-cluster-service",
    "pod-ip": "in-cluster-pod-ip",
}


def _validate_sandboxd_auth_token(token: SecretStr) -> SecretStr:
    if not re.fullmatch(r"[\x21-\x7e]+", token.get_secret_value()):
        raise ValueError("bearer token must be non-empty printable ASCII with no spaces")
    return token


_SandboxdAuthToken = Annotated[
    SecretStr, Field(strict=True), AfterValidator(_validate_sandboxd_auth_token)
]


def _redact_sandboxd_validation_errors(
    cls: type[BaseModel], value: Any, handler: ModelWrapValidatorHandler[Any]
) -> Any:
    try:
        return handler(value)
    except ValidationError as error:
        # hide_input_in_errors only affects text; structured errors also retain
        # raw input, including whole configs when a model validator fails.
        details = [
            InitErrorDetails(type=item["type"], loc=item["loc"], input=None, ctx=item.get("ctx", {}))
            for item in error.errors(include_input=False, include_url=False)
        ]
        raise ValidationError.from_exception_data(cls.__name__, details, hide_input=True) from None


class ExecutionResult(BaseModel):
    """A structured object for holding the result of a command execution."""
    stdout: str = ""  # Standard output from the command.
    stderr: str = ""  # Standard error from the command.
    exit_code: int = -1  # Exit code of the command.
    timed_out: bool = False  # True if the runtime killed the command for exceeding its time limit.

class ExecutionLimits(BaseModel):
    """Execution budgets for commands.run_with_limits().

    Unset limits inherit sandboxd's deployment defaults or maxima. When a
    deployment leaves a limit unconfigured, that resource remains unlimited.
    Explicit limits must be positive and within the deployment maximum.
    """

    model_config = ConfigDict(validate_assignment=True)

    time_limit: float | None = Field(
        default=None, strict=True, ge=1e-9, le=(2**63 - 1) / 1e9, allow_inf_nan=False,
    )  # Execution seconds, including launch and output collection, rounded to nanoseconds.
    stdout_limit_bytes: int | None = Field(
        default=None, strict=True, gt=0, le=2**64 - 1,
    )  # Maximum retained stdout prefix in bytes.
    stderr_limit_bytes: int | None = Field(
        default=None, strict=True, gt=0, le=2**64 - 1,
    )  # Maximum retained stderr prefix in bytes.

    @field_validator("time_limit")
    @classmethod
    def validate_time_limit(cls, value: float | None) -> float | None:
        # Float rounding at the upper boundary can exceed Go's signed duration.
        if value is not None and round(value * 1e9) > 2**63 - 1:
            raise ValueError("time_limit must fit in a Go duration")
        return value

class BoundedExecutionResult(ExecutionResult):
    """Outcome returned by commands.run_with_limits().

    stdout and stderr contain the retained prefixes, decoded as UTF-8 with
    replacement for invalid or incomplete characters. Byte limits apply before
    decoding, so re-encoding the text can exceed those limits.
    exit_code is sandboxd's actual result, including 128+signal for termination
    by signal. timed_out is true only for a server-reported execution time limit.
    Transport failures raise an exception instead of producing a result.
    """

    termination_reason: str  # "exited", "time_limit_exceeded", "unspecified", or a future enum value as a decimal string.
    elapsed_duration: float  # Wall-clock seconds from launch preparation through termination and I/O cleanup.
    stdout_truncated: bool  # True when sandboxd discarded stdout beyond its byte limit.
    stderr_truncated: bool  # True when sandboxd discarded stderr beyond its byte limit.

class LegacyExecuteRequest(BaseModel):
    """Request body for the legacy python-runtime /execute endpoint."""
    command: str  # Shell command to run.
    timeout_seconds: Optional[float] = None  # Limit on how long the command may run; omitted when unset.

class ProcessOutputEvent(BaseModel):
    """A chunk of stdout or stderr from a process started with commands.start().

    ``data`` is raw bytes, not text: a multi-byte character can be split
    across two chunks, so callers decode the stream rather than each chunk.
    A PTY process never produces ``stderr`` events, because the terminal
    merges stderr into stdout.
    """
    type: Literal["stdout", "stderr"]
    data: bytes

class ProcessExitEvent(BaseModel):
    """Terminal event of a process started with commands.start().

    ``exit_code`` is -1 when the process was killed by a signal: sandboxd
    reports Go's ExitCode(), not the shell's 128+N convention.
    """
    type: Literal["exit"] = "exit"
    exit_code: int

# Discriminated union of events ProcessHandle.events() yields. The init event
# sandboxd sends first is consumed by commands.start() to populate the
# handle's process_id, so it is not part of the public event stream. This
# mirrors the Go SDK's ProcessEvent and the TypeScript SDK's ProcessEvent.
ProcessEvent = Union[ProcessOutputEvent, ProcessExitEvent]

class PtySize(BaseModel):
    """Pseudo-terminal geometry for commands.start().

    Mirrors the TypeScript SDK's ``PtySize``.
    """
    cols: int = 80
    rows: int = 24

    @field_validator("cols", "rows")
    @classmethod
    def _validate_positive(cls, v: int) -> int:
        if v < 1 or v > 65535:
            raise ValueError("cols/rows must be between 1 and 65535")
        return v

class FileEntry(BaseModel):
    """Represents a file or directory entry in the sandbox.

    Runtime-neutral: the SDK decodes both the legacy python-runtime wire
    format (``mod_time`` as a float POSIX timestamp) and the sandboxd wire
    format (``modified_at`` as an RFC 3339 string, plus ``mode``) into this
    one shape. ``modified`` is always a timezone-aware datetime.
    """
    name: str  # Name of the file.
    size: int  # Size of the file in bytes.
    type: Literal["file", "directory"]  # Type of the entry (file or directory).
    modified: datetime  # Last modification time (timezone-aware).
    mode: Optional[str] = None  # Octal permission bits (sandboxd only), e.g. "0644".

    @classmethod
    def from_legacy(cls, entry: dict[str, Any]) -> "FileEntry":
        """Build from the legacy python-runtime listing entry."""
        return cls(
            name=entry["name"],
            size=entry["size"],
            type=entry["type"],
            modified=datetime.fromtimestamp(entry.get("mod_time", 0), tz=timezone.utc),
        )

    @classmethod
    def from_sandboxd(cls, entry: dict[str, Any]) -> "FileEntry":
        """Build from a sandboxd DirectoryListing entry."""
        return cls(
            name=entry["name"],
            size=entry["size"],
            type=entry["type"],
            modified=datetime.fromisoformat(entry["modified_at"].replace("Z", "+00:00")),
            mode=entry.get("mode"),
        )


class FileEvent(BaseModel):
    """A filesystem change observed by :meth:`FileWatcher.watch`.

    Mirrors the ``watcher.v1.FileEvent`` protobuf message with SDK-idiomatic
    field names. ``type`` is a string matching the proto names (``create``,
    ``write``, ``remove``, ``rename``, ``chmod``, ``error``); future proto
    versions may introduce additional values, so consumers should tolerate
    unknown types rather than treating the set as closed.

    For ``rename`` events, ``path`` carries the OLD (source) path; when the
    new name lands inside the watched tree it arrives as a separate
    ``create`` event. Pair the two to reconstruct renames.
    """

    type: str  # Kind of change observed (e.g. "create", "write", "rename").
    path: str = ""  # Sandbox-relative path; for ``rename`` events the OLD path.
    error: str = ""  # For ``error`` events, a human-readable message; empty otherwise.

class SandboxHealth(BaseModel):
    """Sandbox liveness/readiness reported by sandboxd's /v1/health endpoint.

    Mirrors the Go SDK's ``sandbox.Health`` and the TypeScript SDK's
    ``SandboxHealth``.
    """
    status: Literal["ok"]  # HealthResponse.status enum in the sandboxd spec.
    # Required rather than optional because sandboxd always serves it
    # (HealthResponse.UptimeSeconds is a plain int64), so a body without it is
    # malformed rather than a sandbox of unknown uptime.
    uptime_seconds: int = Field(ge=0)  # Seconds sandboxd has been running.

    @classmethod
    def from_sandboxd(cls, payload: dict[str, Any]) -> "SandboxHealth":
        """Build from a sandboxd /v1/health response body."""
        # model_validate so a malformed body surfaces as one pydantic
        # ValidationError naming every bad field, not a bare KeyError.
        return cls.model_validate(payload)

class SandboxMetadata(BaseModel):
    """Runtime environment metadata reported by sandboxd's /v1/metadata endpoint.

    Only non-sensitive orchestrator-injected configuration is exposed here;
    the sandboxd spec forbids serving credentials from /v1/metadata. Mirrors
    the Go SDK's ``sandbox.Metadata`` and the TypeScript SDK's
    ``SandboxMetadata``.
    """
    env: dict[str, str] = Field(default_factory=dict)  # Orchestrator-injected environment variables; empty when none apply.

    @classmethod
    def from_sandboxd(cls, payload: dict[str, Any]) -> "SandboxMetadata":
        """Build from a sandboxd /v1/metadata response body."""
        return cls(env=payload.get("env") or {})

class SandboxClaimEnvVar(BaseModel):
    """Represents an environment variable entry in a SandboxClaim spec."""
    name: str  # Name of the environment variable.
    value: str  # Value of the environment variable.
    container_name: str | None = Field(default=None, serialization_alias="containerName")

    @field_validator("name")
    @classmethod
    def validate_name(cls, v: str) -> str:
        if not _ENV_VAR_NAME_RE.match(v):
            raise ValueError(
                "Invalid environment variable name: must consist of alphabetic "
                "characters, digits, '_', '-', or '.', and must not start with a digit"
            )
        if v == "." or v == ".." or v.startswith(".."):
            raise ValueError(
                "Invalid environment variable name: must not be '.', '..', or start with '..'"
            )
        return v

class SandboxDirectConnectionConfig(BaseModel):
    """Configuration for connecting directly to a Sandbox URL.

    ``extra_headers`` and ``client_cert``/``ca_cert`` (mTLS) support a router
    behind an authenticating gateway.
    """
    api_url: str  # Direct URL to the router.
    server_port: int = 8888  # Port the sandbox container listens on.
    # Hidden from repr to keep credentials out of logs.
    extra_headers: dict[str, str] = Field(default_factory=dict, repr=False)  # Sent on every request.
    client_cert: Optional[tuple[str, str]] = None  # (certificate path, private key path) for mTLS.
    ca_cert: Optional[str] = None  # CA bundle path used to verify the router; default trust store if unset.

    @field_validator("extra_headers")
    @classmethod
    def validate_extra_headers(cls, v: dict[str, str]) -> dict[str, str]:
        for name in v:
            if name.lower().startswith(_RESERVED_HEADER_PREFIX):
                raise ValueError(
                    f"header {name!r} is reserved: the SDK sets {_RESERVED_HEADER_PREFIX}* headers itself"
                )
        return v

    @model_validator(mode="after")
    def validate_tls_requires_https(self) -> "SandboxDirectConnectionConfig":
        # TLS options are ignored on http://, so fail loudly instead.
        if (self.client_cert or self.ca_cert) and not self.api_url.lower().startswith("https://"):
            raise ValueError("client_cert and ca_cert require an https:// api_url")
        return self

class SandboxGatewayConnectionConfig(BaseModel):
    """Configuration for connecting via Kubernetes Gateway API."""
    gateway_name: str  # Name of the Gateway resource.
    gateway_namespace: str = "default"  # Namespace where the Gateway resource resides.
    gateway_ready_timeout: int = 180  # Timeout in seconds to wait for Gateway IP.
    server_port: int = 8888  # Port the sandbox container listens on.

class SandboxLocalTunnelConnectionConfig(BaseModel):
    """Configuration for connecting via kubectl port-forward."""
    port_forward_ready_timeout: int = 30  # Timeout in seconds to wait for port-forward to be ready.
    server_port: int = 8888  # Port the sandbox container listens on.
    router_namespace: str = "agent-sandbox-system"  # Namespace where the Router service resides.

    @field_validator("router_namespace")
    @classmethod
    def validate_namespace(cls, v: str) -> str:
        if not re.match(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", v):
            raise ValueError("Invalid Kubernetes namespace name format")
        return v

class SandboxdPodTunnelConnectionConfig(BaseModel):
    """Configuration for the sandboxd runtime via a direct pod port-forward.

    sandboxd (KEP-539.2) exposes two listeners: the Filesystem & Runtime REST
    API and the gRPC ProcessService. This config port-forwards directly to the
    sandbox pod, reaching both.

    For Kata or gVisor, use SandboxdRouterTunnelConnectionConfig or
    SandboxdInClusterConnectionConfig.
    """
    model_config = ConfigDict(hide_input_in_errors=True)

    auth_token: _SandboxdAuthToken | None = Field(default=None, repr=False)  # Optional bearer token for sandboxd REST and gRPC.
    rest_port: int = 8080  # sandboxd REST filesystem port on the pod.
    grpc_port: int = 9090  # sandboxd gRPC ProcessService port on the pod.
    grpc_max_receive_message_bytes: int | None = Field(
        default=None, strict=True, gt=0, le=2**31 - 1, frozen=True,
    )  # Fixed per-channel receive capacity; None preserves gRPC's 4 MiB default.
    port_forward_ready_timeout: int = 30  # Seconds to wait for port-forward readiness.

    @field_validator("rest_port", "grpc_port")
    @classmethod
    def validate_port(cls, v: int) -> int:
        if v < 1 or v > 65535:
            raise ValueError("port must be between 1 and 65535")
        return v

    _redact_errors = model_validator(mode="wrap")(_redact_sandboxd_validation_errors)

class SandboxdRouterTunnelConnectionConfig(BaseModel):
    """Connect to sandboxd through a port-forward to sandbox-router-svc.

    REST and gRPC share the router's port 8080. Routing headers and metadata
    select the Sandbox and its REST or gRPC port. gRPC requires sandbox-router
    v1.0.9 or later. Set ``router_auth_token`` when the router requires
    authentication. The SDK sends only that token as ``Authorization``.
    The router strips that header before forwarding, so token-protected
    sandboxd is unsupported until credential pass-through is defined (issue
    2102). The SDK never sends ``auth_token`` in this mode.
    ``pod_ip_routing`` stays false so the router selects the Pod from its cache
    or DNS. Scoped tokens reject ``X-Sandbox-Pod-IP``.
    """
    model_config = ConfigDict(hide_input_in_errors=True)

    auth_token: _SandboxdAuthToken | None = Field(default=None, repr=False)  # sandboxd bearer token. Not sent in this mode because the router strips Authorization (issue 2102).
    router_auth_token: _SandboxdAuthToken | None = Field(default=None, repr=False)  # Bearer token for the router. Sent as Authorization on REST and gRPC. The router strips it.
    rest_port: int = Field(default=8080, ge=1, le=65535)  # sandboxd REST port, not the router port.
    grpc_port: int = Field(default=9090, ge=1, le=65535)  # sandboxd gRPC port, not the router port.
    grpc_max_receive_message_bytes: int | None = Field(
        default=None, strict=True, gt=0, le=2**31 - 1, frozen=True,
    )  # Fixed per-channel receive capacity; None preserves gRPC's 4 MiB default.
    router_namespace: str = Field(
        default="agent-sandbox-system", min_length=1, max_length=63,
        pattern=r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$",
    )  # Namespace of sandbox-router-svc. Defaults to agent-sandbox-system.
    port_forward_ready_timeout: int = Field(default=30, gt=0)  # Seconds to wait for the router tunnel. Must be positive.
    pod_ip_routing: bool = False  # Send X-Sandbox-Pod-IP on REST and gRPC. Set true only when the router cache is disabled and the Sandbox has no Service.

    @model_validator(mode="after")
    def validate_distinct_ports(self) -> "SandboxdRouterTunnelConnectionConfig":
        if self.rest_port == self.grpc_port:
            raise ValueError("REST and gRPC ports must be different")
        return self

    _redact_errors = model_validator(mode="wrap")(_redact_sandboxd_validation_errors)

class SandboxdInClusterConnectionConfig(BaseModel):
    """Connect to sandboxd directly over the selected in-cluster address.

    ``in-cluster-service`` requires ``Sandbox.status.serviceFQDN`` and a Service
    enabled on the Sandbox template. ``in-cluster-pod-ip`` uses
    ``Sandbox.status.podIPs``. Neither mode falls back to the other. sandboxd's
    REST filesystem listener defaults to port 8080 and its gRPC ProcessService
    listener to port 9090.
    """
    model_config = ConfigDict(hide_input_in_errors=True)

    auth_token: _SandboxdAuthToken | None = Field(default=None, repr=False)  # Optional bearer token for sandboxd REST and gRPC.
    mode: Literal["in-cluster-service", "in-cluster-pod-ip"]
    rest_port: int = 8080
    grpc_port: int = 9090
    grpc_max_receive_message_bytes: int | None = Field(
        default=None, strict=True, gt=0, le=2**31 - 1, frozen=True,
    )  # Fixed per-channel receive capacity; None preserves gRPC's 4 MiB default.

    @field_validator("mode", mode="before")
    @classmethod
    def map_deprecated_mode(cls, v: object) -> object:
        if isinstance(v, str) and v in _DEPRECATED_IN_CLUSTER_MODES:
            new = _DEPRECATED_IN_CLUSTER_MODES[v]
            warnings.warn(
                f"SandboxdInClusterConnectionConfig mode {v!r} is deprecated; use {new!r}",
                DeprecationWarning,
                stacklevel=3,
            )
            return new
        return v

    @field_validator("rest_port", "grpc_port")
    @classmethod
    def validate_port(cls, v: int) -> int:
        if v < 1 or v > 65535:
            raise ValueError("port must be between 1 and 65535")
        return v

    @model_validator(mode="after")
    def validate_distinct_ports(self) -> "SandboxdInClusterConnectionConfig":
        if self.rest_port == self.grpc_port:
            raise ValueError("REST and gRPC ports must be different")
        return self

    _redact_errors = model_validator(mode="wrap")(_redact_sandboxd_validation_errors)

class SandboxInClusterConnectionConfig(BaseModel):
    """Configuration for direct in-cluster connection to the sandbox pod, bypassing the router.

    The client first uses the pod IP reported in the Sandbox status. If the pod IP
    is unavailable, it falls back to the stable Kubernetes DNS endpoint:
        http://{sandbox_id}.{namespace}.svc.cluster.local:{server_port}
    """
    server_port: int = 8888  # Port the sandbox container listens on.

SandboxConnectionConfig = Union[
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxdPodTunnelConnectionConfig,
    SandboxdRouterTunnelConnectionConfig,
    SandboxdInClusterConnectionConfig,
]

class AdditionalPodMetadata(BaseModel):
    """Wire-format for SandboxClaim ``spec.additionalPodMetadata``.

    Labels and annotations stamped onto the running Sandbox Pod (not the
    SandboxClaim object). Omit empty sections so the manifest never carries
    an empty ``additionalPodMetadata``.
    """
    labels: dict[str, str] | None = None
    annotations: dict[str, str] | None = None

class SandboxFileConfig(BaseModel):
    """Client-side file download limits, shared by read() and read_to()."""

    model_config = ConfigDict(validate_assignment=True)

    max_download_size: int | None = Field(default=None, strict=True, gt=0)  # Maximum file bytes downloaded; None preserves unlimited reads.


class SandboxTracerConfig(BaseModel):
    """Configuration for tracer level information"""
    enable_tracing: bool = False  # Whether to enable OpenTelemetry tracing.
    trace_service_name: str = "sandbox-client"  # Service name used for traces.
