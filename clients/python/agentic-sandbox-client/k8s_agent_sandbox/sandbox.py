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

import atexit
import logging
import requests
from .trace_manager import create_tracer_manager, trace_span, trace
from .core.core_execution import CoreExecution
from .files.filesystem import Filesystem
from .models import (
    SandboxConnectionConfig, 
    SandboxLocalTunnelConnectionConfig, 
    SandboxTracerConfig
)
from .k8s_helper import K8sHelper
from .connector import SandboxConnector

class Sandbox:
    """
    A persistent handle to a Sandbox resource.
    """
    def __init__(
        self,
        sandbox_id: str,
        namespace: str = "default",
        connection_config: SandboxConnectionConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
        k8s_helper: K8sHelper | None = None,
    ):
        # Sandbox Related Configuration
        self.id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config or SandboxLocalTunnelConnectionConfig()
        
        # Sandbox Management downstream dependency
        self.k8s_helper = k8s_helper or K8sHelper()
        
        # Establish Sandbox Connection
        self.connector = SandboxConnector(
            sandbox_id=self.id,
            namespace=self.namespace,
            connection_config=self.connection_config,
            k8s_helper=self.k8s_helper
        )

        # Tracer initialization
        self.tracer_config = tracer_config or SandboxTracerConfig()
        self.trace_service_name = self.tracer_config.trace_service_name
        self.tracing_manager, self.tracer = create_tracer_manager(self.tracer_config)

        # Initialisation of namespaced engines
        self._core = CoreExecution(self.connector, self.tracer, self.trace_service_name)
        self._files = Filesystem(self.connector, self.tracer, self.trace_service_name)
        
        # Internal state tracking
        self._is_closed = False
        
    @property
    def core(self) -> CoreExecution | None:
        return self._core

    @property
    def files(self) -> Filesystem | None:
        return self._files

    @property
    def is_active(self) -> bool:
        """
        Returns True if the connection hasn't been explicitly closed 
        and engines are still initialized.
        """
        return not self._is_closed and self._core is not None and self._files is not None

    def _close_connection(self):
        """Closes the client-side connection and disables execution engines."""
        if self._is_closed:
            return
        # Close client side connection
        self.connector.close()
        
        # Don't allow anymore further executions.
        self._core = None
        self._files = None
        
        # Cleanup Trace if it exists
        if self.tracing_manager:
            try:
                self.tracing_manager.end_lifecycle_span()
            except Exception as e:
                logging.error(f"Failed to end tracing span: {e}")
        
        self._is_closed = True
        logging.info(f"Connection to sandbox '{self.id}' has been closed.")
        
    @trace_span("terminate")
    def terminate(self):
        """Permanent deletion of all server side infrastructure and client side connection."""
        # Close the client side connection and trace manager lifecycle
        self._close_connection()
        
        # Delete Sandboxes
        self.k8s_helper.delete_sandbox_claim(self.id, self.namespace)

 
