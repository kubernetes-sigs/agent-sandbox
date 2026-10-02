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

import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Literal, Optional, Union
from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

_ENV_VAR_NAME_RE = re.compile(r"^[-._a-zA-Z][-._a-zA-Z0-9]*$")
_RESERVED_HEADER_PREFIX = "x-sandbox-"

class ExecutionResult(BaseModel):
    """A structured object for holding the result of a command execution."""
    stdout: str = ""  # Standard output from the command.
    stderr: str = ""  # Standard error from the command.
    exit_code: int = -1  # Exit code of the command.

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
    def from_legacy(cls, entry: dict) -> "FileEntry":
        """Build from the legacy python-runtime listing entry."""
        return cls(
            name=entry["name"],
            size=entry["size"],
            type=entry["type"],
            modified=datetime.fromtimestamp(entry.get("mod_time", 0), tz=timezone.utc),
        )

    @classmethod
    def from_sandboxd(cls, entry: dict) -> "FileEntry":
        """Build from a sandboxd DirectoryListing entry."""
        return cls(
            name=entry["name"],
            size=entry["size"],
            type=entry["type"],
            modified=datetime.fromisoformat(entry["modified_at"].replace("Z", "+00:00")),
            mode=entry.get("mode"),
        )

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
    """
    rest_port: int = 8080  # sandboxd REST filesystem port on the pod.
    grpc_port: int = 9090  # sandboxd gRPC ProcessService port on the pod.
    port_forward_ready_timeout: int = 30  # Seconds to wait for port-forward readiness.

    @field_validator("rest_port", "grpc_port")
    @classmethod
    def validate_port(cls, v: int) -> int:
        if v < 1 or v > 65535:
            raise ValueError("port must be between 1 and 65535")
        return v

class SandboxdInClusterConnectionConfig(BaseModel):
    """Connect to sandboxd directly over the selected in-cluster address.

    ``service-dns`` requires ``Sandbox.status.serviceFQDN`` and a Service
    enabled on the Sandbox template. ``pod-ip`` uses ``Sandbox.status.podIPs``.
    Neither mode falls back to the other. sandboxd's REST filesystem listener
    defaults to port 8080 and its gRPC ProcessService listener to port 9090.
    """
    mode: Literal["service-dns", "pod-ip"]
    rest_port: int = 8080
    grpc_port: int = 9090

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
    SandboxdInClusterConnectionConfig,
]

class SandboxTracerConfig(BaseModel):
    """Configuration for tracer level information"""
    enable_tracing: bool = False  # Whether to enable OpenTelemetry tracing.
    trace_service_name: str = "sandbox-client"  # Service name used for traces.


class BatchGroup(BaseModel):
    """Represents one warmpool in a batch for multi-pool batch claiming."""
    # frozen=True so callers can't mutate a group returned from SandboxBatch.groups() and corrupt later snapshots
    model_config = ConfigDict(frozen=True)

    warmpool: str
    size: int = Field(ge=0)
    min_ready: int | None = None  # Defaults to ``size``.

    @model_validator(mode="before")
    @classmethod
    def _fill_min_ready_default(cls, data: Any) -> Any:
        if isinstance(data, dict) and data.get("min_ready") is None:
            data = {**data, "min_ready": data.get("size")}
        return data

    @model_validator(mode="after")
    def _validate_min_ready(self) -> "BatchGroup":
        min_ready = self.min_ready
        if min_ready is None or not (0 <= min_ready <= self.size):
            raise ValueError(
                f"min_ready ({self.min_ready}) must be between 0 and size ({self.size})"
            )
        return self


class MemberState(str, Enum):
    """Where a batch member's claim is in its lifecycle.

    ``FAILED`` is terminal, so the claim will never become Ready. A member stays ``FAILED`` if its
    claim is deleted afterwards, so its state keeps saying why it can't become Ready. ``LOST``
    means the claim was deleted before it failed.
    """
    PENDING = "pending"
    READY = "ready"
    FAILED = "failed"
    LOST = "lost"


class Member(BaseModel):
    """Represents the identity of a single claim in a batch."""
    # frozen=True blocks field reassignment; pod_ips is declared as a tuple instead of a list
    # so it can't be mutated in place either, making Member fully immutable and hashable.
    model_config = ConfigDict(frozen=True)

    claim_name: str
    sandbox_name: str | None = None
    warmpool: str  # The group this member belongs to.
    pod_ips: tuple[str, ...] = ()
    service_fqdn: str | None = None
    state: MemberState = MemberState.PENDING
    reason: str | None = None
    message: str | None = None


class BatchEventType(str, Enum):
    """The kind of change a ``BatchEvent`` reports."""
    MEMBER_READY = "member_ready"
    MEMBER_LOST = "member_lost"
    MEMBER_FAILED = "member_failed"
    LEASE_DEGRADED = "lease_degraded"


class BatchEvent(BaseModel):
    """One change yielded by ``SandboxBatch.events()``. ``member`` is ``None`` for ``LEASE_DEGRADED``."""
    model_config = ConfigDict(frozen=True)

    type: BatchEventType
    member: Member | None = None


@dataclass(frozen=True)
class GroupReady:
    """One group's quorum outcome, yielded by ``SandboxBatch.iter_ready_groups()``.

    On success, ``members`` holds exactly ``min_ready`` Ready members and ``error`` is ``None``.
    On failure, ``error`` is a ``QuorumUnreachableError`` or ``TimeoutError`` and ``members`` is empty.

    This is a dataclass rather than a Pydantic model because ``error`` holds a raw Python
    ``Exception`` object (preserving traceback and causes) rather than serializable data.
    """
    warmpool: str
    members: list[Member] = field(default_factory=list)
    error: Exception | None = None
