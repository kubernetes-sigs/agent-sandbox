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
import logging
from dataclasses import dataclass

import requests
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

logging.basicConfig(level=logging.INFO,
                    format='%(asctime)s - %(levelname)s - %(message)s', stream=sys.stdout)


@dataclass
class ExecutionResult:
    """A structured object for holding the result of a command execution."""
    stdout: str
    stderr: str
    exit_code: int


class SandboxClient:
    """
    A client for creating and interacting with a stateful Sandbox via a router.
    This client dynamically discovers the Gateway IP address at runtime.
    """

    def __init__(
        self,
        template_name: str,
        namespace: str = "default",
        gateway_name: str = "external-http-gateway",
        sandbox_ready_timeout: int = 180,
        gateway_ready_timeout: int = 180,
    ):
        """
        Initializes the SandboxClient.

        Args:
            template_name: The name of the SandboxTemplate to use.
            namespace: The Kubernetes namespace to operate in.
            gateway_name: The name of the Gateway resource to discover the IP from.
            sandbox_ready_timeout: Timeout for the sandbox pod to become ready.
            gateway_ready_timeout: Timeout for the Gateway to get an external IP.
        """
        self.template_name = template_name
        self.namespace = namespace
        self.gateway_name = gateway_name
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.gateway_ready_timeout = gateway_ready_timeout

        self.base_url: str | None = None
        self.claim_name: str | None = None
        self.sandbox_name: str | None = None

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        self.custom_objects_api = client.CustomObjectsApi()

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

        logging.info(f"Creating SandboxClaim: {self.claim_name}...")
        self.custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=self.namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest
        )

    def _wait_for_sandbox_ready(self):
        """
        Waits for the Sandbox custom resource to have a 'Ready' status condition.
        This indicates that the underlying pod is running and has passed its checks.
        """
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
            sandbox_object = event['object']
            status = sandbox_object.get('status', {})
            conditions = status.get('conditions', [])
            is_ready = False
            for cond in conditions:
                if cond.get('type') == 'Ready' and cond.get('status') == 'True':
                    is_ready = True
                    break

            if is_ready:
                self.sandbox_name = sandbox_object['metadata']['name']
                w.stop()
                logging.info(f"Sandbox {self.sandbox_name} is ready.")
                break

        if not self.sandbox_name:
            self.__exit__(None, None, None)
            raise TimeoutError(
                f"Sandbox did not become ready within {self.sandbox_ready_timeout} seconds.")

    def _wait_for_gateway_ip(self):
        """Waits for the Gateway to be assigned an external IP and sets the base_url."""
        logging.info(
            f"Waiting for Gateway '{self.gateway_name}' to get an external IP...")

        w = watch.Watch()
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=self.namespace, group=GATEWAY_API_GROUP,
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
                f"Gateway '{self.gateway_name}' did not get an IP within {self.gateway_ready_timeout} seconds.")

    def __enter__(self) -> 'SandboxClient':
        """Creates SandboxClaim and Sanbdox resources, waits for them to be ready, and discovers the gateway IP."""
        self._create_claim()
        self._wait_for_sandbox_ready()
        self._wait_for_gateway_ip()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """Deletes the SandboxClaim resource."""
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
        """Helper method to make requests, injecting the sandbox ID header."""
        if not self.is_ready():
            raise RuntimeError("Sandbox is not ready for communication.")

        url = f"{self.base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        headers = kwargs.get("headers", {})
        headers["X-Sandbox-ID"] = self.claim_name
        kwargs["headers"] = headers

        try:
            response = requests.request(method, url, **kwargs)
            response.raise_for_status()
            return response
        except requests.exceptions.RequestException as e:
            logging.error(f"Request to gateway router failed: {e}")
            raise RuntimeError(
                f"Failed to communicate with the sandbox via the gateway at {url}.") from e

    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        """
        Executes a shell command inside the running sandbox.
        """
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
        """Uploads content to a file inside the sandbox."""
        if isinstance(content, str):
            content = content.encode('utf-8')

        filename = os.path.basename(path)
        files_payload = {'file': (filename, content)}

        self._request("POST", "upload", files=files_payload, timeout=timeout)
        logging.info(f"File '{filename}' uploaded successfully.")

    def read(self, path: str, timeout: int = 60) -> bytes:
        """Downloads a file from the sandbox."""
        response = self._request("GET", f"download/{path}", timeout=timeout)
        return response.content
