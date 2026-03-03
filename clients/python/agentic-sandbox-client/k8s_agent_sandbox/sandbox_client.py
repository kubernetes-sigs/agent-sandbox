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
This module provides the SandboxClient for interacting with the Agentic Sandbox.
It handles lifecycle management (claiming, waiting) and interaction (execution,
file I/O) via the Sandbox resource handle.
"""

import json
import os
import sys
import logging
import urllib.parse
from typing import List, Literal

import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from kubernetes import client, config, watch
from pydantic import BaseModel

# Import all tracing components from the trace_manager module
from .trace_manager import (
    initialize_tracer, TracerManager, trace_span, trace, OPENTELEMETRY_AVAILABLE
)
from .sandbox import Sandbox

# Constants for API Groups and Resources
CLAIM_API_GROUP = "extensions.agents.x-k8s.io"
CLAIM_API_VERSION = "v1alpha1"
CLAIM_PLURAL_NAME = "sandboxclaims"

SANDBOX_API_GROUP = "agents.x-k8s.io"
SANDBOX_API_VERSION = "v1alpha1"
SANDBOX_PLURAL_NAME = "sandboxes"

POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name"

logging.basicConfig(level=logging.INFO,
                    format='%(asctime)s - %(levelname)s - %(message)s',
                    stream=sys.stdout)

class SandboxClient:
    """
    A client for creating and interacting with a stateful Sandbox via a router.
    """

    def __init__(
        self,
        namespace: str = "default",  # Where Sandbox lives
        gateway_name: str | None = None,  # Name of the Gateway
        gateway_namespace: str = "default",  # Where Gateway lives
        api_url: str | None = None,  # Allow custom URL (DNS or Localhost)
        server_port: int = 8888,     # The port the runtime inside the sandbox listens on
        sandbox_ready_timeout: int = 180,
        gateway_ready_timeout: int = 180,
        port_forward_ready_timeout: int = 30,
        enable_tracing: bool = False,
        trace_service_name: str = "sandbox-client",
    ):
        self.trace_service_name = trace_service_name
        self.enable_tracing = enable_tracing
        self.tracing_manager = None
        self.tracer = None
        if enable_tracing:
            if not OPENTELEMETRY_AVAILABLE:
                logging.error(
                    "OpenTelemetry not installed; skipping tracer initialization.")
            else:
                initialize_tracer(service_name=trace_service_name)
                self.tracing_manager = TracerManager(
                    service_name=trace_service_name)
                self.tracer = self.tracing_manager.tracer

        self.namespace = namespace
        self.gateway_name = gateway_name
        self.gateway_namespace = gateway_namespace
        self.base_url = api_url  # If provided, we skip discovery
        self.server_port = server_port
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.gateway_ready_timeout = gateway_ready_timeout
        self.port_forward_ready_timeout = port_forward_ready_timeout

        self.port_forward_process: subprocess.Popen | None = None

        atexit.register(self.close)

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        self.custom_objects_api = client.CustomObjectsApi()

        # HTTP session with retries
        self.session = requests.Session()
        retries = Retry(
            total=5,
            backoff_factor=0.5,
            status_forcelist=[500, 502, 503, 504],
            allowed_methods=["GET", "POST", "PUT", "DELETE"]
        )
        self.session.mount("http://", HTTPAdapter(max_retries=retries))
        self.session.mount("https://", HTTPAdapter(max_retries=retries))

    def create_sandbox(self, template: str, namespace: str | None = None) -> Sandbox:
        """Provisions a new sandbox and returns a Resource Handle."""
        target_namespace = namespace or self.namespace
        claim_name = f"sandbox-claim-{os.urandom(4).hex()}"

        self._create_claim(claim_name, template, target_namespace)
        self._wait_for_sandbox_ready(claim_name, target_namespace)

        return Sandbox(
            sandbox_id=claim_name,
            base_url=self.api_url,
            namespace=target_namespace,
            gateway_name=self.gateway_name,
            gateway_namespace=self.gateway_namespace,
            server_port=self.server_port,
            enable_tracing=self.enable_tracing,
            trace_service_name=self.trace_service_name
            gateway_ready_timeout=self.gateway_ready_timeout,
            port_forward_ready_timeout=self.port_forward_ready_timeout
        )

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        """Re-attaches to an existing sandbox by ID."""
        return Sandbox(
            sandbox_id=sandbox_id,
            base_url=self.api_url,
            namespace=self.namespace,
            gateway_name=self.gateway_name,
            gateway_namespace=self.gateway_namespace,
            server_port=self.server_port,
            enable_tracing=self.enable_tracing,
            trace_service_name=self.trace_service_name
            gateway_ready_timeout=self.gateway_ready_timeout,
            port_forward_ready_timeout=self.port_forward_ready_timeout
        )

    @trace_span("create_claim")
    def _create_claim(self, claim_name: str, template_name: str, namespace: str):
        """Creates the SandboxClaim custom resource in the Kubernetes cluster."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.claim.name", claim_name)

        annotations = {}
        if trace_context_str:
            annotations["opentelemetry.io/trace-context"] = trace_context_str

        manifest = {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": {"name": self.claim_name,
                         "annotations": annotations
                         },
            "spec": {"sandboxTemplateRef": {"name": self.template_name}}
        }
        logging.info(
            f"Creating SandboxClaim '{claim_name}' "
            f"in namespace '{namespace}' "
            f"using template '{template_name}'..."
        )
        self.custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest
        )

    @trace_span("wait_for_sandbox_ready")
    def _wait_for_sandbox_ready(self, claim_name: str, namespace: str):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        w = watch.Watch()
        logging.info("Watching for Sandbox to become ready...")
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=namespace,
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            plural=SANDBOX_PLURAL_NAME,
            field_selector=f"metadata.name={claim_name}",
            timeout_seconds=self.sandbox_ready_timeout
        ):
            if event["type"] in ["ADDED", "MODIFIED"]:
                sandbox_object = event['object']
                status = sandbox_object.get('status', {})
                conditions = status.get('conditions', [])
                is_ready = False
                for cond in conditions:
                    if cond.get('type') == 'Ready' and cond.get('status') == 'True':
                        is_ready = True
                        break

                if is_ready:
                    logging.info(f"Sandbox {claim_name} is ready.")
                    w.stop()
                    return

        raise TimeoutError(
            f"Sandbox did not become ready within {self.sandbox_ready_timeout} seconds.")

    def _get_free_port(self):
        """Finds a free port on localhost."""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(('', 0))
            return s.getsockname()[1]

    @trace_span("dev_mode_tunnel")
    def _start_and_wait_for_port_forward(self):
        """
        Starts 'kubectl port-forward' to the Router Service.
        This allows 'Dev Mode' without needing a public Gateway IP.
        """
        local_port = self._get_free_port()

        # Assumes the router service name from sandbox_router.yaml
        router_svc = "svc/sandbox-router-svc"

        logging.info(
            f"Starting Dev Mode tunnel: localhost:{local_port} -> {router_svc}:8080...")

        self.port_forward_process = subprocess.Popen(
            [
                "kubectl", "port-forward",
                router_svc,
                # Tunnel to Router (8080), not Sandbox (8888)
                f"{local_port}:8080",
                # The router lives in the sandbox NS (no gateway)
                "-n", self.namespace
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE
        )

        logging.info("Waiting for port-forwarding to be ready...")
        start_time = time.monotonic()
        while time.monotonic() - start_time < self.port_forward_ready_timeout:
            if self.port_forward_process.poll() is not None:
                _, stderr = self.port_forward_process.communicate()
                raise RuntimeError(
                    f"Tunnel crashed: {stderr.decode(errors='ignore')}")

            try:
                # Connect to localhost
                with socket.create_connection(("127.0.0.1", local_port), timeout=0.1):
                    self.base_url = f"http://127.0.0.1:{local_port}"
                    logging.info(
                        f"Dev Mode ready. Tunneled to Router at {self.base_url}")
                    # No need for huge sleeps; the Router service is stable.
                    time.sleep(0.5)
                    return
            except (socket.timeout, ConnectionRefusedError):
                time.sleep(0.5)

        raise TimeoutError("Failed to establish tunnel to Router Service.")

    @trace_span("wait_for_gateway")
    def _wait_for_gateway_ip(self):
        """Waits for the Gateway to be assigned an external IP."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.gateway.name", self.gateway_name)
            span.set_attribute(
                "sandbox.gateway.namespace", self.gateway_namespace)

        # Check if we already have a manually provided URL
        if self.base_url:
            logging.info(f"Using configured API URL: {self.base_url}")
            return

        logging.info(
            f"Waiting for Gateway '{self.gateway_name}' in namespace '{self.gateway_namespace}'...")

        w = watch.Watch()
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=self.gateway_namespace, group=GATEWAY_API_GROUP,
            version=GATEWAY_API_VERSION, plural=GATEWAY_PLURAL,
            field_selector=f"metadata.name={self.gateway_name}",
            timeout_seconds=self.gateway_ready_timeout,
        ):
            if event["type"] in ["ADDED", "MODIFIED"]:
                gateway_object = event['object']
                status = gateway_object.get('status', {})
                addresses = status.get('addresses', [])
                if addresses:
                    ip_address = addresses[0].get('value')
                    if ip_address:
                        self.base_url = f"http://{ip_address}"
                        logging.info(
                            f"Gateway is ready. Base URL set to: {self.base_url}")
                        w.stop()
                        return

        if not self.base_url:
            raise TimeoutError(
                f"Gateway '{self.gateway_name}' in namespace '{self.gateway_namespace}' did not get"
                f" an IP within {self.gateway_ready_timeout} seconds."
            )
