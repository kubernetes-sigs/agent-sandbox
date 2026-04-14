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

from typing import Literal, Union
from pydantic import BaseModel

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
    api_url: str  # Direct URL to the router.
    server_port: int = 8888  # Port the sandbox container listens on.

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

SandboxConnectionConfig = Union[SandboxDirectConnectionConfig, SandboxGatewayConnectionConfig, SandboxLocalTunnelConnectionConfig]

class SandboxTracerConfig(BaseModel):
    """Configuration for tracer level information"""
    # Required because tracer_provider accepts a TracerProvider instance.
    model_config = {"arbitrary_types_allowed": True}
    enable_tracing: bool = False  # Whether to enable OpenTelemetry tracing.
    trace_service_name: str = "sandbox-client"  # Service name used for traces.
    tracer_provider: object = None  # Optional TracerProvider instance.
    