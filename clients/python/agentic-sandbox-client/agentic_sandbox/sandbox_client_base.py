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
"""
Shared, non-I/O logic for sandbox clients.
"""

import logging
import os
import socket
import sys
from dataclasses import dataclass
from typing import Any

from .trace_manager import OPENTELEMETRY_AVAILABLE, TracerManager, initialize_tracer

StrDict = dict[str, Any]

# Constants for API Groups and Resources
GATEWAY_API_GROUP = "gateway.networking.k8s.io"
GATEWAY_API_VERSION = "v1"
GATEWAY_PLURAL = "gateways"

CLAIM_API_GROUP = "extensions.agents.x-k8s.io"
CLAIM_API_VERSION = "v1alpha1"
CLAIM_PLURAL_NAME = "sandboxclaims"

SANDBOX_API_GROUP = "agents.x-k8s.io"
SANDBOX_API_VERSION = "v1alpha1"
SANDBOX_PLURAL_NAME = "sandboxes"

POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name"

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(levelname)s - %(message)s",
    stream=sys.stdout,
)


@dataclass
class ExecutionResult:
    """A structured object for holding the result of a command execution."""

    stdout: str
    stderr: str
    exit_code: int


class SandboxClientBase:
    """
    Base class for Sandbox clients with shared non-I/O behavior.
    """

    def __init__(
        self,
        template_name: str,
        namespace: str = "default",  # Where Sandbox lives
        gateway_name: str | None = None,  # Name of the Gateway
        gateway_namespace: str = "default",  # Where Gateway lives
        api_url: str | None = None,  # Allow custom URL (DNS or Localhost)
        server_port: int = 8888,  # The port the runtime inside the sandbox listens on
        sandbox_ready_timeout: int = 180,
        gateway_ready_timeout: int = 180,
        port_forward_ready_timeout: int = 30,
        enable_tracing: bool = False,
        trace_service_name: str = "sandbox-client",
    ):
        self.trace_service_name = trace_service_name
        self.tracing_manager = None
        self.tracer = None
        if enable_tracing:
            if not OPENTELEMETRY_AVAILABLE:
                logging.error(
                    "OpenTelemetry not installed; skipping tracer initialization."
                )
            else:
                initialize_tracer(service_name=trace_service_name)
                self.tracing_manager = TracerManager(service_name=trace_service_name)
                self.tracer = self.tracing_manager.tracer

        self.template_name = template_name
        self.namespace = namespace
        self.gateway_name = gateway_name
        self.gateway_namespace = gateway_namespace
        self.base_url = api_url  # If provided, we skip discovery
        self.server_port = server_port
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.gateway_ready_timeout = gateway_ready_timeout
        self.port_forward_ready_timeout = port_forward_ready_timeout

        self.port_forward_process = None

        self.claim_name: str | None = None
        self.sandbox_name: str | None = None
        self.pod_name: str | None = None
        self.annotations: StrDict | None = None

    def is_ready(self) -> bool:
        """Returns True if the sandbox is ready and the Gateway IP has been found."""
        return self.base_url is not None

    def _start_lifecycle_span(self) -> str:
        trace_context_str = ""
        if self.tracing_manager:
            self.tracing_manager.start_lifecycle_span()
            trace_context_str = self.tracing_manager.get_trace_context_json()
        return trace_context_str

    def _end_lifecycle_span(self):
        if self.tracing_manager:
            try:
                self.tracing_manager.end_lifecycle_span()
            except Exception as exc:
                logging.error(f"Failed to end tracing span: {exc}")

    def _get_free_port(self) -> int:
        """Finds a free port on localhost."""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as socket_obj:
            socket_obj.bind(("", 0))
            return socket_obj.getsockname()[1]

    def _build_claim_manifest(self, trace_context_str: str) -> StrDict:
        self.claim_name = f"sandbox-claim-{os.urandom(4).hex()}"
        annotations = {}
        if trace_context_str:
            annotations["opentelemetry.io/trace-context"] = trace_context_str

        return {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": {
                "name": self.claim_name,
                "annotations": annotations,
            },
            "spec": {"sandboxTemplateRef": {"name": self.template_name}},
        }

    def _set_sandbox_metadata(self, sandbox_object: StrDict):
        metadata = sandbox_object.get("metadata", {})
        self.sandbox_name = metadata.get("name")
        if not self.sandbox_name:
            raise RuntimeError("Could not determine sandbox name from sandbox object.")
        self.annotations = metadata.get("annotations", {}) or {}

        pod_name = self.annotations.get(POD_NAME_ANNOTATION)
        if pod_name:
            self.pod_name = pod_name
            logging.info(f"Found pod name from annotation: {self.pod_name}")
        else:
            self.pod_name = self.sandbox_name

    def _set_base_url_from_gateway(self, gateway_object: dict[str, Any]) -> bool:
        status = gateway_object.get("status", {})
        addresses = status.get("addresses", [])
        if addresses:
            ip_address = addresses[0].get("value")
            if ip_address:
                self.base_url = f"http://{ip_address}"
                logging.info(f"Gateway is ready. Base URL set to: {self.base_url}")
                return True
        return False

    def _build_request_headers(self, headers: StrDict | None) -> StrDict:
        request_headers = dict(headers or {})
        request_headers["X-Sandbox-ID"] = self.claim_name
        request_headers["X-Sandbox-Namespace"] = self.namespace
        request_headers["X-Sandbox-Port"] = str(self.server_port)
        return request_headers

    def _build_url(self, endpoint: str) -> str:
        return f"{self.base_url.rstrip('/')}/{endpoint.lstrip('/')}"
