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
from typing import Literal, Optional, Union
from pydantic import BaseModel, field_validator, model_validator


class TLSConfig(BaseModel):
    """Optional TLS settings for HTTPS connections."""
    # PEM-encoded CA bundle content OR an absolute file path. The SDK detects
    # which form was passed by looking for a "-----BEGIN" header.
    ca_cert: Optional[str] = None
    # Disable certificate verification. Intended for development and self-signed
    # certificates only. Must not be combined with ca_cert.
    insecure_skip_verify: bool = False
    # SNI / Host header override. Required when connecting to a TLS endpoint via
    # an address that does not match the server certificate's CN/SAN (most
    # notably LocalTunnel, which connects via 127.0.0.1).
    server_name_override: Optional[str] = None

    @model_validator(mode="after")
    def _check_exclusive(self) -> "TLSConfig":
        if self.insecure_skip_verify and self.ca_cert:
            raise ValueError(
                "TLSConfig: ca_cert and insecure_skip_verify are mutually exclusive"
            )
        return self


class ExecutionResult(BaseModel):
    """A structured object for holding the result of a command execution."""
    stdout: str = ""  # Standard output from the command.
    stderr: str = ""  # Standard error from the command.
    exit_code: int = -1  # Exit code of the command.

class FileEntry(BaseModel):
    """Represents a file or directory entry in the sandbox."""
    name: str # Name of the file.
    size: int  # Size of the file in bytes.
    type: Literal["file", "directory"]  # Type of the entry (file or directory).
    mod_time: float # Last modification time of the file. (POSIX timestamp)

class SandboxDirectConnectionConfig(BaseModel):
    """Configuration for connecting directly to a Sandbox URL."""
    api_url: str  # Direct URL to the router (must include scheme).
    server_port: int = 8888  # Port the sandbox container listens on.
    tls: Optional[TLSConfig] = None  # Honored only when api_url uses https://.

    @model_validator(mode="after")
    def _check_tls_matches_scheme(self) -> "SandboxDirectConnectionConfig":
        if self.tls is not None and not self.api_url.lower().startswith("https://"):
            raise ValueError(
                "SandboxDirectConnectionConfig: tls config requires api_url to use https://"
            )
        return self

class SandboxGatewayConnectionConfig(BaseModel):
    """Configuration for connecting via Kubernetes Gateway API."""
    gateway_name: str  # Name of the Gateway resource.
    gateway_namespace: str = "default"  # Namespace where the Gateway resource resides.
    gateway_ready_timeout: int = 180  # Timeout in seconds to wait for Gateway IP.
    # Port the sandbox container listens on. The SDK does NOT use this to build
    # the Gateway URL (Gateways listen on standard 80/443); it is forwarded as
    # the X-Sandbox-Port router header for backend routing.
    server_port: int = 8888
    scheme: Literal["http", "https"] = "http"
    tls: Optional[TLSConfig] = None

    @model_validator(mode="after")
    def _check_tls_matches_scheme(self) -> "SandboxGatewayConnectionConfig":
        if self.tls is not None and self.scheme != "https":
            raise ValueError(
                "SandboxGatewayConnectionConfig: tls config requires scheme='https'"
            )
        return self

class SandboxLocalTunnelConnectionConfig(BaseModel):
    """Configuration for connecting via kubectl port-forward."""
    port_forward_ready_timeout: int = 30  # Timeout in seconds to wait for port-forward to be ready.
    server_port: int = 8888  # Port the sandbox container listens on.
    router_namespace: str = "agent-sandbox-system"  # Namespace where the Router service resides.
    scheme: Literal["http", "https"] = "http"
    # Note: connecting over https to 127.0.0.1 typically requires either
    # tls.server_name_override (to match the server cert) or
    # tls.insecure_skip_verify.
    tls: Optional[TLSConfig] = None

    @field_validator("router_namespace")
    @classmethod
    def validate_namespace(cls, v: str) -> str:
        if not re.match(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", v):
            raise ValueError("Invalid Kubernetes namespace name format")
        return v

    @model_validator(mode="after")
    def _check_tls_matches_scheme(self) -> "SandboxLocalTunnelConnectionConfig":
        if self.tls is not None and self.scheme != "https":
            raise ValueError(
                "SandboxLocalTunnelConnectionConfig: tls config requires scheme='https'"
            )
        return self

class SandboxInClusterConnectionConfig(BaseModel):
    """Configuration for direct in-cluster connection to the sandbox pod, bypassing the router.

    By default, connects via stable K8s DNS:
        http://{sandbox_id}.{namespace}.svc.cluster.local:{server_port}

    When use_pod_ip=True, connects directly to the pod IP from the Sandbox status,
    avoiding DNS resolution at the cost of needing a K8s API call to retrieve the IP.
    """
    server_port: int = 8888  # Port the sandbox container listens on.
    use_pod_ip: bool = False  # If True, connect via pod IP instead of cluster DNS.
    scheme: Literal["http", "https"] = "http"
    tls: Optional[TLSConfig] = None

    @model_validator(mode="after")
    def _check_tls_matches_scheme(self) -> "SandboxInClusterConnectionConfig":
        if self.tls is not None and self.scheme != "https":
            raise ValueError(
                "SandboxInClusterConnectionConfig: tls config requires scheme='https'"
            )
        return self

SandboxConnectionConfig = Union[
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxInClusterConnectionConfig,
]

class SandboxTracerConfig(BaseModel):
    """Configuration for tracer level information"""
    enable_tracing: bool = False  # Whether to enable OpenTelemetry tracing.
    trace_service_name: str = "sandbox-client"  # Service name used for traces.
