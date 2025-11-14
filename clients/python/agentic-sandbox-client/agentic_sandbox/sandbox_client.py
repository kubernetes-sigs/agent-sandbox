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
import time
import sys
import subprocess
import socket
import logging
from dataclasses import dataclass

import requests
from kubernetes import client, config, watch

# Constants for SandboxClaim
CLAIM_API_GROUP = "extensions.agents.x-k8s.io"
CLAIM_API_VERSION = "v1alpha1"
CLAIM_PLURAL_NAME = "sandboxclaims"

# Constants for Sandbox
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
    The main client for creating and interacting with a stateful Sandbox (now named SandboxClient).
    This class is a context manager, designed to be used with a `with` statement.
    """

    def __init__(
        self,
        template_name: str,
        namespace: str = "default",
        server_port: int = 8888,
        sandbox_ready_timeout: int = 180,
        port_forward_ready_timeout: int = 30,
        pod_name_ready_timeout: int = 1
    ):
        self.template_name = template_name
        self.namespace = namespace
        self.server_port = server_port
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.port_forward_ready_timeout = port_forward_ready_timeout
        self.pod_name_ready_timeout = pod_name_ready_timeout
        self.claim_name: str | None = None
        self.sandbox_name: str | None = None
        self.pod_name: str | None = None
        self.base_url = f"http://127.0.0.1:{self.server_port}"
        self.port_forward_process: subprocess.Popen | None = None

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        self.custom_objects_api = client.CustomObjectsApi()

    def is_ready(self) -> bool:
        """Returns True if the sandbox is created and ready for communication."""
        return self.port_forward_process is not None

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
                "Cannot wait for sandbox, claim has not been created.")

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
                    self._wait_for_pod_name()
                    w.stop()
                    return

        self.__exit__(None, None, None)
        raise TimeoutError(
            f"Sandbox did not become ready within {self.sandbox_ready_timeout} seconds.")

    def _wait_for_pod_name(self, timeout: int = 30):
        """
        Waits for the pod-name annotation to be present on the sandbox object.
        This wait is only necessary when using SandboxWarmPool.
        """
        if self.pod_name_ready_timeout <= 0:
            logging.info(
                f"pod_name_ready_timeout {self.pod_name_ready_timeout} is <= 0. Defaulting pod to sandbox name {self.sandbox_name}.")
            self.pod_name = self.sandbox_name
            return
        w = watch.Watch()
        logging.info(
            f"Waiting for pod name annotation on sandbox {self.sandbox_name}...")
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=self.namespace,
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            plural=SANDBOX_PLURAL_NAME,
            field_selector=f"metadata.name={self.sandbox_name}",
            timeout_seconds=self.pod_name_ready_timeout
        ):
            if event["type"] in ["ADDED", "MODIFIED"]:
                sandbox_object = event['object']
                annotations = sandbox_object.get(
                    'metadata', {}).get('annotations', {})
                pod_name = annotations.get(POD_NAME_ANNOTATION)
                if pod_name:
                    self.pod_name = pod_name
                    logging.info(
                        f"Found pod name from annotation: {self.pod_name}")
                    w.stop()
                    return

        logging.warning(
            f"Pod name annotation not found after {self.pod_name_ready_timeout} seconds. Defaulting to sandbox name {self.sandbox_name}.")
        self.pod_name = self.sandbox_name

    def _start_and_wait_for_port_forward(self):
        """
        Starts the 'kubectl port-forward' subprocess and waits for the local port
        to be open and listening, ensuring the tunnel is ready for traffic.
        """
        if not self.pod_name:
            raise RuntimeError(
                "Cannot start port-forwarding, sandbox pod name is not known.")
        logging.info(
            f"Starting port-forwarding for sandbox {self.sandbox_name} in namespace {self.namespace} with sandbox pod {self.pod_name}...")
        self.port_forward_process = subprocess.Popen(
            [
                "kubectl", "port-forward",
                f"pod/{self.pod_name}",
                f"{self.server_port}:{self.server_port}",
                "-n", self.namespace
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE
        )

        logging.info("Waiting for port-forwarding to be ready...")
        start_time = time.monotonic()
        while time.monotonic() - start_time < self.port_forward_ready_timeout:
            # Check if the process has exited prematurely
            if self.port_forward_process.poll() is not None:
                stdout, stderr = self.port_forward_process.communicate()
                raise RuntimeError(
                    "Port-forward process exited unexpectedly.\n"
                    f"Stdout: {stdout.decode(errors='ignore')}\n"
                    f"Stderr: {stderr.decode(errors='ignore')}"
                )

            try:
                with socket.create_connection(("127.0.0.1", self.server_port), timeout=0.1):
                    logging.info(
                        f"Port-forwarding is ready on port {self.server_port}.")
                    return
            except (socket.timeout, ConnectionRefusedError):
                time.sleep(0.2)  # Wait before retrying

        # If the loop finishes, it timed out
        self.__exit__(None, None, None)
        raise TimeoutError(
            f"Port-forwarding did not become ready within {self.port_forward_ready_timeout} seconds.")

    def __enter__(self) -> 'SandboxClient':
        """Creates the SandboxClaim resource and waits for the Sandbox to become ready."""
        self._create_claim()
        self._wait_for_sandbox_ready()
        self._start_and_wait_for_port_forward()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """Deletes the SandboxClaim resource and stops port-forwarding."""
        if self.port_forward_process:
            logging.info("Stopping port-forwarding...")
            self.port_forward_process.terminate()
            self.port_forward_process.wait()

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
        """
        A helper method to make requests to the sandbox's server.
        Raises an exception if the sandbox is not ready or if the request fails.
        """
        if not self.is_ready():
            raise RuntimeError("Sandbox is not ready. Cannot send requests.")

        url = f"{self.base_url}/{endpoint}"
        try:
            response = requests.request(method, url, **kwargs)
            response.raise_for_status()  # Raise an exception for bad status codes (4xx or 5xx)
            return response
        except requests.exceptions.RequestException as e:
            logging.error(f"Request to sandbox failed: {e}")
            raise RuntimeError(
                f"Failed to communicate with the sandbox at {url}.") from e

    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        """
        Executes a shell command inside the running sandbox.
        """
        payload = {"command": command}
        response = self._request(
            "POST", "execute", json=payload, timeout=timeout)

        response_data = response.json()
        return ExecutionResult(
            stdout=response_data['stdout'],
            stderr=response_data['stderr'],
            exit_code=response_data['exit_code']
        )

    def write(self, path: str, content: bytes | str):
        """
        Uploads content to a file inside the sandbox.
        The basename of the provided path is used as the filename in the sandbox.
        """
        if isinstance(content, str):
            content = content.encode('utf-8')

        filename = os.path.basename(path)
        files_payload = {'file': (filename, content)}

        self._request("POST", "upload", files=files_payload)
        logging.info(f"File '{filename}' uploaded successfully.")

    def read(self, path: str) -> bytes:
        """
        Downloads a file from the sandbox.
        The base path for the download is the root of the sandbox's filesystem.
        """
        response = self._request("GET", f"download/{path}")
        return response.content
