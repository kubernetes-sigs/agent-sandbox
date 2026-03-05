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
file I/O) via the Sandbox resource handle.
"""

import json
import os
import sys
import subprocess
import atexit
import logging
from typing import List, Literal, Dict
from pydantic import BaseModel

# Import all tracing components from the trace_manager module
from .trace_manager import (
    create_tracer_manager, trace_span, trace
)
from .sandbox import Sandbox
from .models import (
    SandboxConnectionConfig, 
    SandboxLocalTunnelConnectionConfig, 
    SandboxTracerConfig
)
from .k8s_helper import K8sHelper

logging.basicConfig(level=logging.INFO,
                    format='%(asctime)s - %(levelname)s - %(message)s',
                    stream=sys.stdout)

class SandboxClient:
    """
    A registry-based client for managing Sandbox lifecycles.
    Tracks all active handles to ensure flat code structure and safe cleanup.
    """

    def __init__(
        self,
        connection_config: SandboxConnectionConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
        sandbox_ready_timeout: int = 180,
    ):
        # Sandbox related configuration
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.connection_config = connection_config or SandboxLocalTunnelConnectionConfig()
        
        # Tracer configuration
        self.tracer_config = tracer_config or SandboxTracerConfig()
        self.tracing_manager, self.tracer = create_tracer_manager(self.tracer_config)

        # Downstream Kubernetes Configuration
        self.k8s_helper = K8sHelper()
        
        # Tracks all the active connections to the Sandbox
        self._active_connection_sandboxes: Dict[str, Sandbox] = {}
        
        # Register global cleanup for all tracked sandboxes
        atexit.register(self.delete_all)


    def create_sandbox(self, template: str, namespace: str = "default") -> Sandbox:
        """Provisions new infra and returns a tracked Sandbox handle."""
        claim_name = f"sandbox-claim-{os.urandom(4).hex()}"
        
        try:
            self._create_claim(claim_name, template, namespace)
            self._wait_for_sandbox_ready(claim_name, namespace)
        except Exception:
            # If creation or waiting fails, ensure we don't leave an orphaned claim
            self.k8s_helper.delete_sandbox_claim(claim_name, namespace)
            raise

        sandbox = Sandbox(
            sandbox_id=claim_name,
            namespace=namespace,
            connection_config=self.connection_config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper
        )
        
        self._active_connection_sandboxes[claim_name] = sandbox
        return sandbox

    def get_sandbox(self, sandbox_id: str, namespace: str = "default") -> Sandbox:
        """
        Retrieves an existing sandbox handle. 
        If the handle is closed or missing, it re-attaches to the infrastructure.
        """
        existing = self._active_connection_sandboxes.get(sandbox_id)

        # If it's already in the registry and active, return the existing object
        if existing and existing.is_active:
            return existing

        # Check if the sandbox actually exists in Kubernetes
        if not self.k8s_helper.get_sandbox(sandbox_id, namespace):
            self._active_connection_sandboxes.pop(sandbox_id, None)
            raise RuntimeError(f"Sandbox '{sandbox_id}' not found in namespace '{namespace}'")

        # Re-attach: Create a fresh handle for the existing ID
        new_handle = Sandbox(
            sandbox_id=sandbox_id,
            namespace=namespace,
            connection_config=self.connection_config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper
        )
        
        self._active_connection_sandboxes[sandbox_id] = new_handle
        return new_handle

    def delete_sandbox(self, sandbox_id: str, namespace: str = "default"):
        """Stops the client side connection and deletes the Kubernetes resources."""
        sandbox = self._active_connection_sandboxes.get(sandbox_id)
        if sandbox:
            sandbox.terminate()
            self._active_connection_sandboxes.pop(sandbox_id, None)
        else:
            # If not in registry, attempt a blind delete via K8s helper
            self.k8s_helper.delete_sandbox_claim(sandbox_id, namespace)
    
    def list_active_sandboxes(self) -> List[str]:
        """Returns a list of all Sandbox IDs currently managed by this client."""
        # We only return IDs that are still active/initialized
        return [sb_id for sb_id, obj in self._active_connection_sandboxes.items() if obj.is_active]
    
    def delete_all(self):
        """
        Cleanup all tracked sandboxes, respecting their individual namespaces.
        Triggered automatically on script exit via atexit.
        """
        # We iterate over items to get access to the sandbox object's metadata
        for sb_id, sandbox in list(self._active_connection_sandboxes.items()):
            try:
                # We pass the specific namespace stored in the Sandbox handle
                self.delete_sandbox(sb_id, namespace=sandbox.namespace)
            except Exception as e:
                # We use sandbox.namespace in the log for better debugging
                logging.error(
                    f"Cleanup failed for {sb_id} in namespace {sandbox.namespace}: {e}"
                )
    
    @trace_span("create_claim")
    def _create_claim(self, claim_name: str, template_name: str, namespace: str):
        """Creates the SandboxClaim custom resource in the Kubernetes cluster."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.claim.name", claim_name)

        annotations = {}
        if self.tracing_manager:
            trace_context_str = self.tracing_manager.get_trace_context_json()
            if trace_context_str:
                annotations["opentelemetry.io/trace-context"] = trace_context_str

        self.k8s_helper.create_sandbox_claim(claim_name, template_name, namespace, annotations)

    @trace_span("wait_for_sandbox_ready")
    def _wait_for_sandbox_ready(self, claim_name: str, namespace: str):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        self.k8s_helper.wait_for_sandbox_ready(claim_name, namespace, self.sandbox_ready_timeout)
