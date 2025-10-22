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
import subprocess
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

@dataclass
class ExecutionResult:
    """A structured object for holding the result of a command execution."""
    stdout: str
    stderr: str
    exit_code: int

class Sandbox:
    """
    The main client for creating and interacting with a stateful Sandbox.
    This class is a context manager, designed to be used with a `with` statement.
    """
    def __init__(self, template_name: str, namespace: str = "default"):
        self.template_name = template_name
        self.namespace = namespace
        self.claim_name: str | None = None
        self.sandbox_name: str | None = None
        self.port_forward_process: subprocess.Popen | None = None
        self.server_port: int = 8888  # Assuming a default port

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        self.custom_objects_api = client.CustomObjectsApi()

    def is_ready(self) -> bool:
        """Returns True if the sandbox is created and ready for communication."""
        return self.port_forward_process is not None

    def __enter__(self) -> 'Sandbox':
        """Creates the SandboxClaim resource and waits for the Sandbox to become ready."""
        self.claim_name = f"sandbox-claim-{os.urandom(4).hex()}"
        manifest = {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": {"name": self.claim_name},
            "spec": {"sandboxTemplateRef": {"name": self.template_name}}
        }

        print(f"Creating SandboxClaim: {self.claim_name}...")
        self.custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=self.namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest
        )

        w = watch.Watch()
        print("Watching for Sandbox to become ready...")
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=self.namespace,
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            plural=SANDBOX_PLURAL_NAME,
            field_selector=f"metadata.name={self.claim_name}",
            timeout_seconds=180
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
                print(f"Sandbox {self.sandbox_name} is ready.")
                break

        if not self.sandbox_name:
            self.__exit__(None, None, None)
            raise TimeoutError("Sandbox did not become ready within the 180-second timeout period.")

        print(f"Starting port-forwarding for sandbox {self.sandbox_name}...")
        self.port_forward_process = subprocess.Popen(
            [
                "kubectl", "port-forward",
                f"pod/{self.sandbox_name}",
                f"{self.server_port}:{self.server_port}",
                "-n", self.namespace
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE
        )
        time.sleep(3)  # Give port-forward a moment to establish

        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """Deletes the SandboxClaim resource and stops port-forwarding."""
        if self.port_forward_process:
            print("Stopping port-forwarding...")
            self.port_forward_process.terminate()
            self.port_forward_process.wait()

        if self.claim_name:
            print(f"Deleting SandboxClaim: {self.claim_name}")
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
                    print(f"Error deleting sandbox claim: {e}")

    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        """Executes a shell command inside the running sandbox."""
        if not self.is_ready():
            raise ConnectionError("Sandbox is not ready. Cannot execute commands.")

        url = f"http://127.0.0.1:{self.server_port}/execute"
        payload = {"command": command}

        response = requests.post(url, json=payload, timeout=timeout)
        response.raise_for_status()

        response_data = response.json()
        return ExecutionResult(
            stdout=response_data['stdout'],
            stderr=response_data['stderr'],
            exit_code=response_data['exit_code']
        )

    def write(self, path: str, content: bytes | str):
        """Uploads content to a file inside the sandbox."""
        if not self.is_ready():
            raise ConnectionError("Sandbox is not ready. Cannot write files.")

        url = f"http://127.0.0.1:{self.server_port}/upload"
        filename = os.path.basename(path)
        files_payload = {'file': (filename, content)}

        response = requests.post(url, files=files_payload)
        response.raise_for_status()
        print(f"File '{filename}' uploaded successfully.")

    def read(self, path: str) -> bytes:
        """Downloads a file from the sandbox."""
        if not self.is_ready():
            raise ConnectionError("Sandbox is not ready. Cannot read files.")

        url = f"http://127.0.0.1:{self.server_port}/download/{path}"
        response = requests.get(url)
        response.raise_for_status()
        return response.content