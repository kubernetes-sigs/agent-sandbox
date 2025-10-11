import os
from dataclasses import dataclass

import requests
from kubernetes import client, config, watch

API_GROUP = "karo.io"
API_VERSION = "v1"
PLURAL_NAME = "agenticsandboxes"

@dataclass
class ExecutionResult:
    """A structured object for holding the result of a command execution."""
    stdout: str
    stderr: str
    exit_code: int

class _Commands:
    """Helper class for executing commands, accessed via sandbox.commands"""
    def __init__(self, sandbox: 'Sandbox'):
        self._sandbox = sandbox

    def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        """Executes a shell command inside the running sandbox via RPC."""
        if not self._sandbox.is_ready():
            raise ConnectionError("Sandbox is not ready. Cannot execute commands.")

        service_dns_name = f"{self._sandbox.instance_name}.{self._sandbox.namespace}.svc.cluster.local"
        url = f"http://{service_dns_name}:{self._sandbox.server_port}/execute"
        payload = {"command": command}

        response = requests.post(url, json=payload, timeout=timeout)
        response.raise_for_status()

        response_data = response.json()
        return ExecutionResult(
            stdout=response_data['stdout'],
            stderr=response_data['stderr'],
            exit_code=response_data['exit_code']
        )

class _Files:
    """Helper class for file operations, accessed via sandbox.files"""
    def __init__(self, sandbox: 'Sandbox'):
        self._sandbox = sandbox

    def write(self, path: str, content: bytes | str):
        """Uploads content to a file inside the sandbox."""
        if not self._sandbox.is_ready():
            raise ConnectionError("Sandbox is not ready. Cannot write files.")

        service_dns_name = f"{self._sandbox.instance_name}.{self._sandbox.namespace}.svc.cluster.local"
        url = f"http://{service_dns_name}:{self._sandbox.server_port}/upload"
        filename = os.path.basename(path)
        files_payload = {'file': (filename, content)}

        response = requests.post(url, files=files_payload)
        response.raise_for_status()
        print(f"File '{filename}' uploaded successfully.")

    def read(self, path: str) -> bytes:
        """Downloads a file from the sandbox."""
        if not self._sandbox.is_ready():
            raise ConnectionError("Sandbox is not ready. Cannot read files.")

        service_dns_name = f"{self._sandbox.instance_name}.{self._sandbox.namespace}.svc.cluster.local"
        url = f"http://{service_dns_name}:{self._sandbox.server_port}/download/{path}"
        response = requests.get(url)
        response.raise_for_status()
        return response.content

class Sandbox:
    """
    The main client for creating and interacting with a stateful AgenticSandbox.
    This class is a context manager, designed to be used with a `with` statement.
    """
    def __init__(self, class_name: str, namespace: str = "default"):
        self.class_name = class_name
        self.namespace = namespace

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        self.custom_objects_api = client.CustomObjectsApi()

        self.commands = _Commands(self)
        self.files = _Files(self)

        # Internal state
        self.instance_name: str | None = None
        self.sandbox_ip: str | None = None
        self.server_port: int | None = None

    def is_ready(self) -> bool:
        """Returns True if the sandbox is created and ready for communication."""
        return self.instance_name is not None and self.sandbox_ip is not None

    def __enter__(self) -> 'Sandbox':
        """Creates the AgenticSandbox resource and waits for it to become ready."""
        manifest = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "AgenticSandbox",
            "metadata": {"generateName": "agentic-sandbox-"},
            "spec": {"className": self.class_name}
        }

        print("Creating AgenticSandbox resource on the cluster...")
        created_sandbox = self.custom_objects_api.create_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self.namespace,
            plural=PLURAL_NAME,
            body=manifest
        )
        self.instance_name = created_sandbox['metadata']['name']
        print(f"Created AgenticSandbox instance: {self.instance_name}")

        w = watch.Watch()
        print("Watching for sandbox to become ready...")
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=self.namespace,
            group=API_GROUP,
            version=API_VERSION,
            plural=PLURAL_NAME,
            field_selector=f"metadata.name={self.instance_name}",
            timeout_seconds=180
        ):
            sandbox_object = event['object']
            status = sandbox_object.get('status', {})
            # Check the 'conditions' array instead of the 'phase' field.
            conditions = status.get('conditions', [])
            is_ready = False
            for cond in conditions:
                if cond.get('type') == 'Ready' and cond.get('status') == 'True':
                    is_ready = True
                    break

            if is_ready:
                # The IP and Port might not be set in the very first "Ready" status update.
                # We also need to get them from the Service, not the sandbox status.
                # NOTE: This is a placeholder for the logic to get the Service IP and Port.
                # In a real scenario, the operator should populate these into the sandbox status.
                # For this PoC, we assume the operator will eventually add them.
                self.sandbox_ip = status.get('sandboxIP')
                self.server_port = status.get('serverPort')

                if self.sandbox_ip and self.server_port:
                    w.stop()
                    print(f"Sandbox is ready at http://{self.sandbox_ip}:{self.server_port}")
                    break

        if not self.is_ready():
            self.__exit__(None, None, None)
            raise TimeoutError("Sandbox did not become ready within the 180-second timeout period.")

        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """Deletes the AgenticSandbox resource, triggering cleanup."""
        if self.instance_name:
            print(f"Deleting AgenticSandbox instance: {self.instance_name}")
            try:
                self.custom_objects_api.delete_namespaced_custom_object(
                    group=API_GROUP,
                    version=API_VERSION,
                    namespace=self.namespace,
                    plural=PLURAL_NAME,
                    name=self.instance_name
                )
            except client.ApiException as e:
                if e.status != 404:
                    print(f"Error deleting sandbox: {e}")
            self.instance_name = None
