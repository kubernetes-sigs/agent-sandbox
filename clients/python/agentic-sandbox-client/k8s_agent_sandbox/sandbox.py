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

import logging
import requests
import socket
import subprocess
import time
import atexit
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from .trace_manager import initialize_tracer, TracerManager, OPENTELEMETRY_AVAILABLE, trace_span, trace
from .core_execution import CoreExecution
from .filesystem import Filesystem
from .models import SandboxRouterConfig, SandboxTracerConfig
from .k8s_helper import K8sHelper

class Sandbox:
    """
    A persistent handle to a Sandbox resource.
    """
    def __init__(
        self,
        sandbox_id: str,
        namespace: str = "default",
        router_config: SandboxRouterConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
        k8s_helper: K8sHelper | None = None,
    ):
        # Sandbox ID and Namespace instantiation
        self.id = sandbox_id
        self.namespace = namespace
        
        # Router initialization
        self.router_config = router_config or SandboxRouterConfig()
        self.base_url = self.router_config.api_url
        
        # Tracer initialization
        self.tracer_config = tracer_config or SandboxTracerConfig()
        self.trace_service_name = self.tracer_config.trace_service_name
        self.tracing_manager = None
        self.tracer = None
        if self.tracer_config.enable_tracing:
            if not OPENTELEMETRY_AVAILABLE:
                logging.error(
                    "OpenTelemetry not installed; skipping tracer initialization.")
            else:
                initialize_tracer(service_name=self.trace_service_name)
                self.tracing_manager = TracerManager(
                    service_name=self.trace_service_name)
                self.tracer = self.tracing_manager.tracer

        # The creation of Sandbox starts a session with resuable connection (pooling)
        self.session = requests.Session()
        retries = Retry(
            total=5,
            backoff_factor=0.5,
            status_forcelist=[500, 502, 503, 504],
            allowed_methods=["GET", "POST", "PUT", "DELETE"]
        )
        self.session.mount("http://", HTTPAdapter(max_retries=retries))
        self.session.mount("https://", HTTPAdapter(max_retries=retries))

        # Close the port forward on program termination
        self.port_forward_process: subprocess.Popen | None = None
        atexit.register(self.close)
    
        # Initialisation of namespaced engines
        self.core = CoreExecution(self)
        self.files = Filesystem(self)
        
        # Sandbox Management downstream dependency
        self.k8s_helper = k8s_helper or K8sHelper()

    def close(self):
        """Clean up resources like port-forwarding processes."""
        if self.port_forward_process:
            try:
                logging.info(f"Stopping port-forwarding for Sandbox {self.id}...")
                self.port_forward_process.terminate()
                try:
                    self.port_forward_process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    self.port_forward_process.kill()
            except Exception as e:
                logging.error(f"Failed to stop port-forwarding: {e}")
            finally:
                self.port_forward_process = None

    def _get_free_port(self):
        """Finds a free port on localhost."""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(('', 0))
            return s.getsockname()[1]

    def _start_and_wait_for_port_forward(self):
        """Starts 'kubectl port-forward' to the Router Service."""
        local_port = self._get_free_port()
        router_svc = "svc/sandbox-router-svc"

        logging.info(
            f"Starting tunnel for Sandbox {self.id}: localhost:{local_port} -> {router_svc}:8080...")

        self.port_forward_process = subprocess.Popen(
            [
                "kubectl", "port-forward",
                router_svc,
                f"{local_port}:8080",
                "-n", self.namespace
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE
        )

        logging.info("Waiting for port-forwarding to be ready...")
        start_time = time.monotonic()
        while time.monotonic() - start_time < self.router_config.port_forward_ready_timeout:
            if self.port_forward_process.poll() is not None:
                _, stderr = self.port_forward_process.communicate()
                raise RuntimeError(
                    f"Tunnel crashed: {stderr.decode(errors='ignore')}")

            try:
                with socket.create_connection(("127.0.0.1", local_port), timeout=0.1):
                    self.base_url = f"http://127.0.0.1:{local_port}"
                    logging.info(f"Tunnel ready at {self.base_url}")
                    time.sleep(0.5)
                    return
            except (socket.timeout, ConnectionRefusedError):
                time.sleep(0.5)

        raise TimeoutError("Failed to establish tunnel to Router Service.")

    def _wait_for_gateway_ip(self):
        """Waits for the Gateway to be assigned an external IP."""
        ip_address = self.k8s_helper.wait_for_gateway_ip(
            self.router_config.gateway_name,
            self.router_config.gateway_namespace,
            self.router_config.gateway_ready_timeout
        )
        self.base_url = f"http://{ip_address}"

    def _ensure_connection(self):
        """Ensures that the base_url is resolved (via Gateway or Port Forward)."""
        if self.base_url:
            return

        if self.router_config.gateway_name:
            self._wait_for_gateway_ip()
        else:
            self._start_and_wait_for_port_forward()

    def _request(self, method: str, endpoint: str, **kwargs) -> requests.Response:
        self._ensure_connection()

        # Check if port-forward died silently
        if self.port_forward_process and self.port_forward_process.poll() is not None:
            _, stderr = self.port_forward_process.communicate()
            raise RuntimeError(
                f"Kubectl Port-Forward crashed BEFORE request!\n"
                f"Stderr: {stderr.decode(errors='ignore')}"
            )

        url = f"{self.base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        headers = kwargs.get("headers", {})
        headers["X-Sandbox-ID"] = self.id
        headers["X-Sandbox-Namespace"] = self.namespace
        headers["X-Sandbox-Port"] = str(self.router_config.server_port)
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

    def terminate(self):
        """Permanent deletion of all infrastructure and state."""
        self.k8s_helper.delete_sandbox_claim(self.id, self.namespace)
