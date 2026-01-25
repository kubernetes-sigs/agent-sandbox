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
file I/O) with the sandbox environment, including optional OpenTelemetry tracing.
"""

import os
import sys
import time
import socket
import subprocess
import logging
import asyncio
from dataclasses import dataclass

import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from kubernetes import client, config, watch

# Import all tracing components from the trace_manager module
from .trace_manager import (
    initialize_tracer, TracerManager, trace_span, trace, OPENTELEMETRY_AVAILABLE
)

# httpx is an optional dependency used by AsyncSandboxClient.
try:
    import httpx
except ImportError:  # pragma: no cover
    httpx = None

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

logging.basicConfig(level=logging.INFO,
                    format='%(asctime)s - %(levelname)s - %(message)s',
                    stream=sys.stdout)


@dataclass
class ExecutionResult:
    """A structured object for holding the result of a command execution."""
    stdout: str
    stderr: str
    exit_code: int


class SandboxClient:
    """
    A client for creating and interacting with a stateful Sandbox via a router.
    """

    def __init__(
        self,
        template_name: str,
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

        self.template_name = template_name
        self.namespace = namespace
        self.gateway_name = gateway_name
        self.gateway_namespace = gateway_namespace
        self.base_url = api_url  # If provided, we skip discovery
        self.server_port = server_port
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.gateway_ready_timeout = gateway_ready_timeout
        self.port_forward_ready_timeout = port_forward_ready_timeout

        self.port_forward_process: subprocess.Popen | None = None

        self.claim_name: str | None = None
        self.sandbox_name: str | None = None
        self.pod_name: str | None = None
        self.annotations: dict | None = None

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

    def is_ready(self) -> bool:
        """Returns True if the sandbox is ready and the Gateway IP has been found."""
        return self.base_url is not None

    @trace_span("create_claim")
    def _create_claim(self, trace_context_str: str = ""):
        """Creates the SandboxClaim custom resource in the Kubernetes cluster."""
        self.claim_name = f"sandbox-claim-{os.urandom(4).hex()}"

        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.claim.name", self.claim_name)

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
            f"Creating SandboxClaim '{self.claim_name}' "
            f"in namespace '{self.namespace}' "
            f"using template '{self.template_name}'..."
        )
        self.custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=self.namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest
        )

    @trace_span("wait_for_sandbox_ready")
    def _wait_for_sandbox_ready(self):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        if not self.claim_name:
            raise RuntimeError(
                "Cannot wait for sandbox; a sandboxclaim has not been created.")

        w = watch.Watch()
        logging.info("Watching for Sandbox to become ready...")
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=self.namespace,
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            plural=SANDBOX_PLURAL_NAME,
            field_selector=f"metadata.name={self.claim_name}",
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
                    metadata = sandbox_object.get(
                        "metadata", {})
                    self.sandbox_name = metadata.get(
                        "name")
                    if not self.sandbox_name:
                        raise RuntimeError(
                            "Could not determine sandbox name from sandbox object.")
                    logging.info(f"Sandbox {self.sandbox_name} is ready.")

                    self.annotations = sandbox_object.get(
                        'metadata', {}).get('annotations', {})
                    pod_name = self.annotations.get(POD_NAME_ANNOTATION)
                    if pod_name:
                        self.pod_name = pod_name
                        logging.info(
                            f"Found pod name from annotation: {self.pod_name}")
                    else:
                        self.pod_name = self.sandbox_name
                    w.stop()
                    return

        self.__exit__(None, None, None)
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

        self.__exit__(None, None, None)
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

    def __enter__(self) -> 'SandboxClient':
        trace_context_str = ""
        # We can't use the "with trace..." context management. This is the equivalent.
        # https://github.com/open-telemetry/opentelemetry-python/issues/2787
        if self.tracing_manager:
            self.tracing_manager.start_lifecycle_span()
            trace_context_str = self.tracing_manager.get_trace_context_json()

        self._create_claim(trace_context_str)
        self._wait_for_sandbox_ready()

        # STRATEGY SELECTION
        if self.base_url:
            # Case 1: API URL provided manually (DNS / Internal) -> Do nothing, just use it.
            logging.info(f"Using configured API URL: {self.base_url}")

        elif self.gateway_name:
            # Case 2: Gateway Name provided -> Production Mode (Discovery)
            self._wait_for_gateway_ip()

        else:
            # Case 3: No Gateway, No URL -> Developer Mode (Port Forward to Router)
            self._start_and_wait_for_port_forward()

        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        # Cleanup Port Forward if it exists
        if self.port_forward_process:
            try:
                logging.info("Stopping port-forwarding...")
                self.port_forward_process.terminate()
                try:
                    self.port_forward_process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    self.port_forward_process.kill()
            # Unlikely to fail, but catch just in case.
            except Exception as e:
                logging.error(f"Failed to stop port-forwarding: {e}")

        # Delete the SandboxClaim
        if self.claim_name:
            logging.info(f"Deleting SandboxClaim: {self.claim_name}")
            try:
                self.custom_objects_api.delete_namespaced_custom_object(
                    group=CLAIM_API_GROUP,
                    version=CLAIM_API_VERSION,
                    namespace=self.namespace,
                    plural=CLAIM_PLURAL_NAME,
                    name=self.claim_name
                )
            except client.ApiException as e:
                if e.status != 404:
                    logging.error(
                        f"Error deleting sandbox claim: {e}", exc_info=True)
            except Exception as e:
                logging.error(
                    f"Unexpected error deleting sandbox claim: {e}", exc_info=True)

        # Cleanup Trace if it exists
        if self.tracing_manager:
            try:
                self.tracing_manager.end_lifecycle_span()
            except Exception as e:
                logging.error(f"Failed to end tracing span: {e}")

    def _request(self, method: str, endpoint: str, **kwargs) -> requests.Response:
        if not self.is_ready():
            raise RuntimeError("Sandbox is not ready for communication.")

        # Check if port-forward died silently
        if self.port_forward_process and self.port_forward_process.poll() is not None:
            _, stderr = self.port_forward_process.communicate()
            raise RuntimeError(
                f"Kubectl Port-Forward crashed BEFORE request!\n"
                f"Stderr: {stderr.decode(errors='ignore')}"
            )

        url = f"{self.base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        headers = kwargs.get("headers", {})
        headers["X-Sandbox-ID"] = self.claim_name
        headers["X-Sandbox-Namespace"] = self.namespace
        headers["X-Sandbox-Port"] = str(self.server_port)
        kwargs["headers"] = headers

        try:
            response = self.session.request(method, url, **kwargs)
            response.raise_for_status()
            return response
        except requests.exceptions.RequestException as e:
            # Check if port-forward died DURING request
            if self.port_forward_process and self.port_forward_process.poll() is not None:
                _, stderr = self.port_forward_process.communicate()
                raise RuntimeError(
                    f"Kubectl Port-Forward crashed DURING request!\n"
                    f"Stderr: {stderr.decode(errors='ignore')}"
                ) from e

            logging.error(f"Request to gateway router failed: {e}")
            raise RuntimeError(
                f"Failed to communicate with the sandbox via the gateway at {url}.") from e

    @trace_span("run")
    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.command", command)

        payload = {"command": command}
        response = self._request(
            "POST", "execute", json=payload, timeout=timeout)

        response_data = response.json()
        result = ExecutionResult(
            stdout=response_data.get('stdout', ''),
            stderr=response_data.get('stderr', ''),
            exit_code=response_data.get('exit_code', -1)
        )

        if span.is_recording():
            span.set_attribute("sandbox.exit_code", result.exit_code)
        return result

    @trace_span("write")
    def write(self, path: str, content: bytes | str, timeout: int = 60):
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
            span.set_attribute("sandbox.file.size", len(content))

        if isinstance(content, str):
            content = content.encode('utf-8')

        filename = os.path.basename(path)
        files_payload = {'file': (filename, content)}
        self._request("POST", "upload",
                      files=files_payload, timeout=timeout)
        logging.info(f"File '{filename}' uploaded successfully.")

    @trace_span("read")
    def read(self, path: str, timeout: int = 60) -> bytes:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        response = self._request(
            "GET", f"download/{path}", timeout=timeout)
        content = response.content

        if span.is_recording():
            span.set_attribute("sandbox.file.size", len(content))

        return content


class AsyncSandboxClient:
    """
    An async client for creating and interacting with a stateful Sandbox via a router.

    Lifecycle operations (claiming, waiting, port-forwarding) are performed using the
    same Kubernetes APIs as SandboxClient. Sandbox I/O calls (execute/upload/download)
    are performed asynchronously using httpx.
    """

    def __init__(
        self,
        template_name: str,
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
        http_retries: int = 5,
        http_backoff_factor: float = 0.5,
        http_retry_statuses: tuple[int, ...] = (500, 502, 503, 504),
    ):
        if httpx is None:
            raise ImportError(
                "AsyncSandboxClient requires 'httpx'. Install with: pip install 'agentic_sandbox[async]'"
            )

        self.trace_service_name = trace_service_name
        self.tracing_manager: TracerManager | None = None
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

        self.template_name = template_name
        self.namespace = namespace
        self.gateway_name = gateway_name
        self.gateway_namespace = gateway_namespace
        self.base_url = api_url  # If provided, we skip discovery
        self.server_port = server_port
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.gateway_ready_timeout = gateway_ready_timeout
        self.port_forward_ready_timeout = port_forward_ready_timeout

        self.http_retries = http_retries
        self.http_backoff_factor = http_backoff_factor
        self.http_retry_statuses = set(http_retry_statuses)

        self.port_forward_process: subprocess.Popen | None = None

        self.claim_name: str | None = None
        self.sandbox_name: str | None = None
        self.pod_name: str | None = None
        self.annotations: dict | None = None

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        self.custom_objects_api = client.CustomObjectsApi()

        self._http = httpx.AsyncClient(follow_redirects=True)

    def is_ready(self) -> bool:
        """Returns True if the sandbox is ready and the Gateway IP has been found."""
        return self.base_url is not None

    def _create_claim(self, trace_context_str: str = ""):
        """Creates the SandboxClaim custom resource in the Kubernetes cluster."""
        self.claim_name = f"sandbox-claim-{os.urandom(4).hex()}"

        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.claim.name", self.claim_name)

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
            f"Creating SandboxClaim '{self.claim_name}' "
            f"in namespace '{self.namespace}' "
            f"using template '{self.template_name}'..."
        )
        self.custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=self.namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest
        )

    def _wait_for_sandbox_ready(self):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        if not self.claim_name:
            raise RuntimeError(
                "Cannot wait for sandbox; a sandboxclaim has not been created.")

        w = watch.Watch()
        logging.info("Watching for Sandbox to become ready...")
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=self.namespace,
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            plural=SANDBOX_PLURAL_NAME,
            field_selector=f"metadata.name={self.claim_name}",
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
                    metadata = sandbox_object.get(
                        "metadata", {})
                    self.sandbox_name = metadata.get(
                        "name")
                    if not self.sandbox_name:
                        raise RuntimeError(
                            "Could not determine sandbox name from sandbox object.")
                    logging.info(f"Sandbox {self.sandbox_name} is ready.")

                    self.annotations = sandbox_object.get(
                        'metadata', {}).get('annotations', {})
                    pod_name = self.annotations.get(POD_NAME_ANNOTATION)
                    if pod_name:
                        self.pod_name = pod_name
                        logging.info(
                            f"Found pod name from annotation: {self.pod_name}")
                    else:
                        self.pod_name = self.sandbox_name
                    w.stop()
                    return

        self._close_sync()
        raise TimeoutError(
            f"Sandbox did not become ready within {self.sandbox_ready_timeout} seconds.")

    def _get_free_port(self):
        """Finds a free port on localhost."""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(('', 0))
            return s.getsockname()[1]

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

        self._close_sync()
        raise TimeoutError("Failed to establish tunnel to Router Service.")

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

    def _open_sync(self, trace_context_str: str = ""):
        self._create_claim(trace_context_str)
        self._wait_for_sandbox_ready()

        # STRATEGY SELECTION
        if self.base_url:
            # Case 1: API URL provided manually (DNS / Internal) -> Do nothing, just use it.
            logging.info(f"Using configured API URL: {self.base_url}")

        elif self.gateway_name:
            # Case 2: Gateway Name provided -> Production Mode (Discovery)
            self._wait_for_gateway_ip()

        else:
            # Case 3: No Gateway, No URL -> Developer Mode (Port Forward to Router)
            self._start_and_wait_for_port_forward()

    def open(self) -> 'AsyncSandboxClient':
        trace_context_str = ""
        if self.tracing_manager:
            self.tracing_manager.start_lifecycle_span()
            trace_context_str = self.tracing_manager.get_trace_context_json()

        self._open_sync(trace_context_str)
        return self

    async def aopen(self) -> 'AsyncSandboxClient':
        trace_context_str = ""
        if self.tracing_manager:
            self.tracing_manager.start_lifecycle_span()
            trace_context_str = self.tracing_manager.get_trace_context_json()

        loop = asyncio.get_running_loop()
        await loop.run_in_executor(None, self._open_sync, trace_context_str)
        return self

    def _close_sync(self):
        # Cleanup Port Forward if it exists
        if self.port_forward_process:
            try:
                logging.info("Stopping port-forwarding...")
                self.port_forward_process.terminate()
                try:
                    self.port_forward_process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    self.port_forward_process.kill()
            # Unlikely to fail, but catch just in case.
            except Exception as e:
                logging.error(f"Failed to stop port-forwarding: {e}")

        # Delete the SandboxClaim
        if self.claim_name:
            logging.info(f"Deleting SandboxClaim: {self.claim_name}")
            try:
                self.custom_objects_api.delete_namespaced_custom_object(
                    group=CLAIM_API_GROUP,
                    version=CLAIM_API_VERSION,
                    namespace=self.namespace,
                    plural=CLAIM_PLURAL_NAME,
                    name=self.claim_name
                )
            except client.ApiException as e:
                if e.status != 404:
                    logging.error(
                        f"Error deleting sandbox claim: {e}", exc_info=True)
            except Exception as e:
                logging.error(
                    f"Unexpected error deleting sandbox claim: {e}", exc_info=True)

    def close(self):
        self._close_sync()

        # Cleanup Trace if it exists
        if self.tracing_manager:
            try:
                self.tracing_manager.end_lifecycle_span()
            except Exception as e:
                logging.error(f"Failed to end tracing span: {e}")

    async def aclose(self):
        loop = asyncio.get_running_loop()
        await loop.run_in_executor(None, self._close_sync)
        if self.tracing_manager:
            try:
                self.tracing_manager.end_lifecycle_span()
            except Exception as e:
                logging.error(f"Failed to end tracing span: {e}")
        await self._http.aclose()

    def __enter__(self) -> 'AsyncSandboxClient':
        return self.open()

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.close()
        try:
            loop = asyncio.get_event_loop()
            if loop.is_running():
                loop.create_task(self._http.aclose())
            else:
                loop.run_until_complete(self._http.aclose())
        except Exception:
            # Best-effort: if no loop is available, httpx will close on GC.
            pass

    async def __aenter__(self) -> 'AsyncSandboxClient':
        return await self.aopen()

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        await self.aclose()

    async def _request(self, method: str, endpoint: str, **kwargs) -> 'httpx.Response':
        if not self.is_ready():
            raise RuntimeError("Sandbox is not ready for communication.")

        # Check if port-forward died silently
        if self.port_forward_process and self.port_forward_process.poll() is not None:
            _, stderr = self.port_forward_process.communicate()
            raise RuntimeError(
                f"Kubectl Port-Forward crashed BEFORE request!\n"
                f"Stderr: {stderr.decode(errors='ignore')}"
            )

        url = f"{self.base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        headers = dict(kwargs.get("headers", {}) or {})
        headers["X-Sandbox-ID"] = self.claim_name
        headers["X-Sandbox-Namespace"] = self.namespace
        headers["X-Sandbox-Port"] = str(self.server_port)
        kwargs["headers"] = headers

        allowed_retry_methods = {"GET", "POST", "PUT", "DELETE"}
        method_upper = method.upper()

        last_exc: Exception | None = None
        for attempt in range(max(1, int(self.http_retries))):
            try:
                response = await self._http.request(method_upper, url, **kwargs)
                response.raise_for_status()
                return response
            except httpx.HTTPStatusError as e:
                last_exc = e
                status = e.response.status_code if e.response else None
                should_retry = (
                    status in self.http_retry_statuses
                    and method_upper in allowed_retry_methods
                    and attempt < self.http_retries - 1
                )
                if should_retry:
                    await asyncio.sleep(self.http_backoff_factor * (2 ** attempt))
                    continue

                logging.error(f"Request to gateway router failed: {e}")
                raise RuntimeError(
                    f"Failed to communicate with the sandbox via the gateway at {url}.") from e
            except httpx.RequestError as e:
                last_exc = e
                if attempt < self.http_retries - 1:
                    await asyncio.sleep(self.http_backoff_factor * (2 ** attempt))
                    continue

                # Check if port-forward died DURING request
                if self.port_forward_process and self.port_forward_process.poll() is not None:
                    _, stderr = self.port_forward_process.communicate()
                    raise RuntimeError(
                        f"Kubectl Port-Forward crashed DURING request!\n"
                        f"Stderr: {stderr.decode(errors='ignore')}"
                    ) from e

                logging.error(f"Request to gateway router failed: {e}")
                raise RuntimeError(
                    f"Failed to communicate with the sandbox via the gateway at {url}.") from e

        if last_exc is not None:
            raise last_exc
        raise RuntimeError(f"Failed to communicate with the sandbox via the gateway at {url}.")

    async def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        if self.tracer:
            with self.tracer.start_as_current_span(f"{self.trace_service_name}.run"):
                return await self._run_inner(command, timeout)
        return await self._run_inner(command, timeout)

    async def _run_inner(self, command: str, timeout: int) -> ExecutionResult:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.command", command)

        payload = {"command": command}
        response = await self._request(
            "POST", "execute", json=payload, timeout=timeout)
        await response.aread()

        response_data = response.json()
        result = ExecutionResult(
            stdout=response_data.get('stdout', ''),
            stderr=response_data.get('stderr', ''),
            exit_code=response_data.get('exit_code', -1)
        )

        if span.is_recording():
            span.set_attribute("sandbox.exit_code", result.exit_code)
        return result

    async def write(self, path: str, content: bytes | str, timeout: int = 60):
        if self.tracer:
            with self.tracer.start_as_current_span(f"{self.trace_service_name}.write"):
                await self._write_inner(path, content, timeout)
                return
        await self._write_inner(path, content, timeout)

    async def _write_inner(self, path: str, content: bytes | str, timeout: int):
        if isinstance(content, str):
            content_bytes = content.encode('utf-8')
        else:
            content_bytes = content

        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
            span.set_attribute("sandbox.file.size", len(content_bytes))

        filename = os.path.basename(path)
        files_payload = {'file': (filename, content_bytes)}
        response = await self._request("POST", "upload",
                                       files=files_payload, timeout=timeout)
        await response.aread()
        logging.info(f"File '{filename}' uploaded successfully.")

    async def read(self, path: str, timeout: int = 60) -> bytes:
        if self.tracer:
            with self.tracer.start_as_current_span(f"{self.trace_service_name}.read"):
                return await self._read_inner(path, timeout)
        return await self._read_inner(path, timeout)

    async def _read_inner(self, path: str, timeout: int) -> bytes:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        response = await self._request(
            "GET", f"download/{path}", timeout=timeout)
        content = await response.aread()

        if span.is_recording():
            span.set_attribute("sandbox.file.size", len(content))

        return content
