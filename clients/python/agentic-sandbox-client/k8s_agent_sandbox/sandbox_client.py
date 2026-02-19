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

import json
import os
import sys
import time
import socket
import subprocess
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
        result = ExecutionResult(**response_data)

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

        encoded_path = urllib.parse.quote(path, safe='')
        response = self._request(
            "GET", f"download/{encoded_path}", timeout=timeout)
        content = response.content

        if span.is_recording():
            span.set_attribute("sandbox.file.size", len(content))

        return content
    
    @trace_span("list")
    def list(self, path: str, timeout: int = 60) -> List[FileEntry]:
        """
        Lists the contents of a directory in the sandbox.
        Returns a list of FileEntry objects containing name, size, type, and mod_time.
        """
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        encoded_path = urllib.parse.quote(path, safe='')
        response = self._request("GET", f"list/{encoded_path}", timeout=timeout)
        
        entries = response.json()
        if not entries:
            return []

        file_entries = [FileEntry(**e) for e in entries]
        
        if span.is_recording():
            span.set_attribute("sandbox.file.count", len(file_entries))
        return file_entries

    @trace_span("exists")
    def exists(self, path: str, timeout: int = 60) -> bool:
        """
        Checks if a file or directory exists at the given path.
        """
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        encoded_path = urllib.parse.quote(path, safe='')
        response = self._request("GET", f"exists/{encoded_path}", timeout=timeout)
        exists = response.json().get("exists", False)
        if span.is_recording():
            span.set_attribute("sandbox.file.exists", exists)
        return exists
    
