# Copyright 2026 The Kubernetes Authors.
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

import atexit
import logging
import requests
from .trace_manager import create_tracer_manager, trace_span, trace
from .commands.command_executor import CommandExecutor
from .files.filesystem import Filesystem
from .models import (
    SandboxConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxTracerConfig,
)
from .k8s_helper import K8sHelper
from .connector import SandboxConnector
from .constants import POD_NAME_ANNOTATION, SANDBOX_NAME_HASH_LABEL

class Sandbox:
    """
    Represents a connection to a specific running Sandbox instance.

    This class provides the interface for interacting with the Sandbox, including:
    - Executing commands via the `commands` property.
    - Managing files via the `files` property.
    - Handling the underlying connection.
    - Integrating with OpenTelemetry for tracing operations.
    """
    def __init__(
        self,
        claim_name: str,
        sandbox_id: str,
        namespace: str = "default",
        connection_config: SandboxConnectionConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
        k8s_helper: K8sHelper | None = None,
    ):
        # Sandbox Related Configuration
        self.claim_name = claim_name
        self.sandbox_id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config or SandboxLocalTunnelConnectionConfig()

        # Sandbox Management downstream dependency
        self.k8s_helper = k8s_helper or K8sHelper()

        # Establish Sandbox Connection
        use_pod_ip = (
            isinstance(self.connection_config, SandboxInClusterConnectionConfig)
            and self.connection_config.use_pod_ip
        )
        self.connector = SandboxConnector(
            sandbox_id=self.sandbox_id,
            namespace=self.namespace,
            connection_config=self.connection_config,
            k8s_helper=self.k8s_helper,
            get_pod_ip=self.get_pod_ip if use_pod_ip else None,
        )

        # Tracer initialization
        self.tracer_config = tracer_config or SandboxTracerConfig()
        self.trace_service_name = self.tracer_config.trace_service_name
        self.tracing_manager, self.tracer = create_tracer_manager(self.tracer_config)

        # Initialisation of namespaced engines
        self._commands = CommandExecutor(self.connector, self.tracer, self.trace_service_name)
        self._files = Filesystem(self.connector, self.tracer, self.trace_service_name)
        
        # Internal state tracking
        self._is_closed = False
        self._pod_name = None
        self._sandbox_name_hash = None
        
    def get_pod_name(self) -> str:
        """Fetches the Sandbox object from Kubernetes and retrieves its current pod name."""
        if self._pod_name is not None:
            return self._pod_name

        sandbox_object = self.k8s_helper.get_sandbox(self.sandbox_id, self.namespace) or {}
        metadata = sandbox_object.get('metadata') or {}
        annotations = metadata.get('annotations') or {}
        pod_name = annotations.get(POD_NAME_ANNOTATION)
        self._pod_name = pod_name if pod_name is not None else self.sandbox_id
        return self._pod_name


    def get_sandbox_name_hash(self) -> str | None:
        """Fetches the Sandbox object from Kubernetes and retrieves its name hash from selector.
        Caches the result to avoid repeated API calls.
        """
        # Return cached value if available
        if self._sandbox_name_hash is not None:
            return self._sandbox_name_hash

        sandbox_object = self.k8s_helper.get_sandbox(self.sandbox_id, self.namespace) or {}
        status = sandbox_object.get('status') or {}
        selector = status.get('selector') or ""
        if "=" in selector:
            key, value = selector.split("=")
            if key == SANDBOX_NAME_HASH_LABEL:
                self._sandbox_name_hash = value
                return value
                
        return None

    def get_pod_ip(self) -> str | None:
        """Fetches the first pod IP from the Sandbox status.

        Always queries the K8s API for the latest IP — the pod IP can change
        after a pod restart (e.g. when spec.replicas is scaled to 0 and back).
        Returns None if the controller does not populate podIPs.
        """
        sandbox_object = self.k8s_helper.get_sandbox(self.sandbox_id, self.namespace) or {}
        pod_ips = sandbox_object.get('status', {}).get('podIPs', [])
        return pod_ips[0] if pod_ips else None

    def status(self) -> tuple[str, str]:
        """
        Retrieves the current status of the Sandbox by inspecting its Kubernetes conditions.
        
        Returns a tuple of (status, message).
        status can be 'SandboxReady', 'SandboxNotFound', or 'SandboxNotReady'.
        message contains the Kubernetes condition message if available.
        """
        sandbox_object = self.k8s_helper.get_sandbox(self.sandbox_id, self.namespace)
        if not sandbox_object:
            return "SandboxNotFound", "Sandbox object not found in Kubernetes."

        status_data = sandbox_object.get("status", {})
        for cond in status_data.get("conditions", []):
            if cond.get("type") == "Ready":
                message = cond.get("message", "")
                if cond.get("status") == "True":
                    return "SandboxReady", message
                else:
                    return "SandboxNotReady", message

        return "SandboxNotReady", "Unknown message"

    @property
    def commands(self) -> CommandExecutor | None:
        return self._commands

    @property
    def files(self) -> Filesystem | None:
        return self._files

    @property
    def is_active(self) -> bool:
        """
        Returns True if the connection hasn't been explicitly closed 
        and engines are still initialized.
        """
        return not self._is_closed and self._commands is not None and self._files is not None

    def close_connection(self):
        """
        Closes the client-side connection and disables execution engines locally,
        but leaves the remote Kubernetes Sandbox infrastructure running.
        
        Use this to free up local resources (like port-forwards or HTTP sessions).
        """
        if self._is_closed:
            return
        # Close client side connection
        self.connector.close()
        
        # Don't allow anymore further executions.
        self._commands = None
        self._files = None
        
        # Cleanup Trace if it exists
        if self.tracing_manager:
            try:
                self.tracing_manager.end_lifecycle_span()
            except Exception as e:
                logging.error(f"Failed to end tracing span: {e}")
        
        self._is_closed = True
        logging.info(f"Connection to sandbox claim '{self.claim_name}' has been closed.")
    
    def terminate(self):
        """Permanent deletion of all server side infrastructure and client side connection.

        Idempotent: calling ``terminate()`` repeatedly is a no-op after the
        first successful delete. ``self.claim_name`` is cleared after the
        claim is deleted so a subsequent call does not issue a second DELETE
        that would return 404.
        """
        # Close the client side connection and trace manager lifecycle
        self.close_connection()

        if not self.claim_name:
            # Already deleted (or never successfully created a claim).
            return

        # Delete this Sandbox
        claim_name = self.claim_name
        self.k8s_helper.delete_sandbox_claim(claim_name, self.namespace)
        # Clear after successful delete so a retry does not 404.
        self.claim_name = None

 
