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
from kubernetes import client, config
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from .trace_manager import initialize_tracer, TracerManager, OPENTELEMETRY_AVAILABLE
from .core_execution import CoreExecution
from .filesystem import Filesystem

CLAIM_API_GROUP = "extensions.agents.x-k8s.io"
CLAIM_API_VERSION = "v1alpha1"
CLAIM_PLURAL_NAME = "sandboxclaims"

SANDBOX_API_GROUP = "agents.x-k8s.io"
SANDBOX_API_VERSION = "v1alpha1"
SANDBOX_PLURAL_NAME = "sandboxes"

class Sandbox:
    """
    A persistent handle to a Sandbox resource.
    """
    def __init__(
        self,
        sandbox_id: str,
        base_url: str,
        namespace: str = "default",
        server_port: int = 8888,
        enable_tracing: bool = False,
        trace_service_name: str = "sandbox-client",
    ):
        self.id = sandbox_id
        self.base_url = base_url
        self.namespace = namespace
        self.server_port = server_port

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

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
        self.custom_objects_api = client.CustomObjectsApi()

        self.core = CoreExecution(self)
        self.files = Filesystem(self)

    def _request(self, method: str, endpoint: str, **kwargs) -> requests.Response:
        url = f"{self.base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        headers = kwargs.get("headers", {})
        headers["X-Sandbox-ID"] = self.id
        headers["X-Sandbox-Namespace"] = self.namespace
        headers["X-Sandbox-Port"] = str(self.server_port)
        kwargs["headers"] = headers

        try:
            response = self.session.request(method, url, **kwargs)
            response.raise_for_status()
            return response
        except requests.exceptions.RequestException as e:
            logging.error(f"Request to gateway router failed: {e}")
            raise RuntimeError(
                f"Failed to communicate with the sandbox via the gateway at {url}.") from e

    def status(self):
        """Fetches the current lifecycle state from the manager."""
        try:
            resource = self.custom_objects_api.get_namespaced_custom_object(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=self.namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=self.id
            )
            return resource.get("status", {})
        except client.ApiException as e:
            logging.error(f"Error getting status for Sandbox {self.id}: {e}")
            raise

    def terminate(self):
        """Permanent deletion of all infrastructure and state."""
        try:
            self.custom_objects_api.delete_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=self.namespace,
                plural=CLAIM_PLURAL_NAME,
                name=self.id
            )
            logging.info(f"Terminated SandboxClaim: {self.id}")
        except client.ApiException as e:
            if e.status != 404:
                logging.error(f"Error terminating sandbox {self.id}: {e}")
                raise
