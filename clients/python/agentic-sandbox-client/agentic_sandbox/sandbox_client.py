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

import os
import sys
import time
import socket
import subprocess
import logging
from dataclasses import dataclass

import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from kubernetes import client, config, watch

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
    ):
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

    def _create_claim(self):
        """Creates the SandboxClaim custom resource in the Kubernetes cluster."""
        self.claim_name = f"sandbox-claim-{os.urandom(4).hex()}"
        manifest = {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": {"name": self.claim_name},
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

        self.__exit__(None, None, None)
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
                stdout, stderr = self.port_forward_process.communicate()
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

    def _wait_for_gateway_ip(self):
        """Waits for the Gateway to be assigned an external IP."""
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
                f"Gateway '{self.gateway_name}' in namespace '{self.gateway_namespace}' did not get an IP within {self.gateway_ready_timeout} seconds.")

    def __enter__(self) -> 'SandboxClient':
        self._create_claim()
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
            logging.info("Stopping port-forwarding...")
            self.port_forward_process.terminate()
            try:
                self.port_forward_process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.port_forward_process.kill()

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

    def _request(self, method: str, endpoint: str, **kwargs) -> requests.Response:
        if not self.is_ready():
            raise RuntimeError("Sandbox is not ready for communication.")

        # Check if port-forward died silently
        if self.port_forward_process and self.port_forward_process.poll() is not None:
            stdout, stderr = self.port_forward_process.communicate()
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
                stdout, stderr = self.port_forward_process.communicate()
                raise RuntimeError(
                    f"Kubectl Port-Forward crashed DURING request!\n"
                    f"Stderr: {stderr.decode(errors='ignore')}"
                ) from e

            logging.error(f"Request to gateway router failed: {e}")
            raise RuntimeError(
                f"Failed to communicate with the sandbox via the gateway at {url}.") from e

    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        payload = {"command": command}
        response = self._request(
            "POST", "execute", json=payload, timeout=timeout)

        response_data = response.json()
        return ExecutionResult(
            stdout=response_data.get('stdout', ''),
            stderr=response_data.get('stderr', ''),
            exit_code=response_data.get('exit_code', -1)
        )

    def write(self, path: str, content: bytes | str, timeout: int = 60):
        if isinstance(content, str):
            content = content.encode('utf-8')

        filename = os.path.basename(path)
        files_payload = {'file': (filename, content)}

        self._request("POST", "upload", files=files_payload, timeout=timeout)
        logging.info(f"File '{filename}' uploaded successfully.")

    def read(self, path: str, timeout: int = 60) -> bytes:
        response = self._request("GET", f"download/{path}", timeout=timeout)
        return response.content
