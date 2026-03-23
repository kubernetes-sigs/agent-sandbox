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
from typing import List, Literal, Dict, TypeVar, Generic, Type
from pydantic import BaseModel

# Import all tracing components from the trace_manager module
from .trace_manager import (
    create_tracer_manager, initialize_tracer, trace_span, trace
)
from .sandbox import Sandbox
from .models import (
    SandboxConnectionConfig, 
    SandboxLocalTunnelConnectionConfig, 
    SandboxTracerConfig
)
from .k8s_helper import K8sHelper
from .constants import POD_NAME_ANNOTATION

logging.basicConfig(level=logging.INFO,
                    format='%(asctime)s - %(levelname)s - %(message)s',
                    stream=sys.stdout)

T = TypeVar('T', bound=Sandbox)

class SandboxClient(Generic[T]):
    """
    A registry-based client for managing Sandbox lifecycles.
    Tracks all active handles to ensure flat code structure and safe cleanup.
    """

    sandbox_class: Type[T] = Sandbox  # type: ignore

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
        if self.tracer_config.enable_tracing:
            initialize_tracer(self.tracer_config.trace_service_name)
        self.tracing_manager, self.tracer = create_tracer_manager(self.tracer_config)

        # Downstream Kubernetes Configuration
        self.k8s_helper = K8sHelper()
        
        # Tracks all the active client side connections to the Sandbox
        self._active_connection_sandboxes: Dict[str, T] = {}
        
        # Register global cleanup for all tracked sandboxes.
        # Deletes all the sandboxes on program termination
        atexit.register(self.delete_all)

    def create_sandbox(self, template: str, namespace: str = "default") -> T:
        """Provisions new infra and returns a tracked Sandbox handle.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.create_sandbox(template="python-sandbox-template")
            >>> sandbox.commands.run("echo 'Hello World'")
        """
        claim_name = f"sandbox-claim-{os.urandom(4).hex()}"
        
        try:
            self._create_claim(claim_name, template, namespace)
            self._wait_for_sandbox_ready(claim_name, namespace)
        except Exception:
            # If creation or waiting fails, ensure we don't leave an orphaned claim
            self.k8s_helper.delete_sandbox_claim(claim_name, namespace)
            raise

        sandbox_object = self.k8s_helper.get_sandbox(claim_name, namespace) or {}
        annotations = sandbox_object.get('metadata', {}).get('annotations', {})
        pod_name = annotations.get(POD_NAME_ANNOTATION)

        sandbox = self.sandbox_class(
            sandbox_id=claim_name,
            namespace=namespace,
            connection_config=self.connection_config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper,
            pod_name=pod_name
        )
        
        self._active_connection_sandboxes[claim_name] = sandbox
        return sandbox

    def get_sandbox(self, sandbox_id: str, namespace: str = "default") -> T:
        """
        Retrieves an existing sandbox handle. 
        If the handle is closed or missing, it re-attaches to the infrastructure.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.get_sandbox("sandbox-claim-1234abcd")
            >>> sandbox.commands.run("ls -la")
        """
        existing = self._active_connection_sandboxes.get(sandbox_id)

        # If it's already in the registry and active, return the existing object
        if existing and existing.is_active:
            return existing
        
        # If the sandbox is not active, pop it out from the tracking list
        if existing and not existing.is_active:
            self._active_connection_sandboxes.pop(sandbox_id, None)
            
        # Check if the sandbox actually exists in Kubernetes
        sandbox_object = self.k8s_helper.get_sandbox(sandbox_id, namespace)
        if not sandbox_object:
            self._active_connection_sandboxes.pop(sandbox_id, None)
            raise RuntimeError(f"Sandbox '{sandbox_id}' not found in namespace '{namespace}'")

        annotations = sandbox_object.get('metadata', {}).get('annotations', {})
        pod_name = annotations.get(POD_NAME_ANNOTATION)

        # Re-attach: Create a fresh handle for the existing ID
        new_handle = self.sandbox_class(
            sandbox_id=sandbox_id,
            namespace=namespace,
            connection_config=self.connection_config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper,
            pod_name=pod_name
        )
        
        self._active_connection_sandboxes[sandbox_id] = new_handle
        return new_handle
    
    def list_active_sandboxes(self) -> List[str]:
        """Returns a list of all Sandbox IDs currently managed by this client.
        
        Example:
        
            >>> client = SandboxClient()
            >>> client.create_sandbox("python-sandbox-template")
            >>> print(client.list_active_sandboxes())
            ['sandbox-claim-1234abcd']
        """
        # We only return IDs that are still active/initialized, and clean up inactive ones.
        for sb_id, obj in list(self._active_connection_sandboxes.items()):
            if not obj.is_active:
                self._active_connection_sandboxes.pop(sb_id, None)
        return list(self._active_connection_sandboxes.keys())
      
    def list_all_sandboxes(self, namespace: str = "default") -> List[str]:
        """
        Lists all Sandbox IDs currently existing in the Kubernetes cluster 
        for the given namespace.
        
        Example:
        
            >>> client = SandboxClient()
            >>> print(client.list_all_sandboxes(namespace="default"))
            ['sandbox-claim-1234abcd', 'sandbox-claim-5678efgh']
        """
        return self.k8s_helper.list_sandboxes(namespace)

    def delete_sandbox(self, sandbox_id: str, namespace: str = "default"):
        """Stops the client side connection and deletes the Kubernetes resources.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.create_sandbox("python-sandbox-template")
            >>> client.delete_sandbox(sandbox.id)
        """
        sandbox = self._active_connection_sandboxes.get(sandbox_id)
        if sandbox:
            sandbox.terminate()
            self._active_connection_sandboxes.pop(sandbox_id, None)
        else:
            # If not in registry, attempt a blind delete via K8s helper
            self.k8s_helper.delete_sandbox_claim(sandbox_id, namespace)
            
    def delete_all(self):
        """
        Cleanup all tracked sandboxes managed by this client.
        Triggered automatically on script exit via atexit.
        
        Example:
        
            >>> client = SandboxClient()
            >>> client.create_sandbox("python-sandbox-template")
            >>> client.create_sandbox("python-sandbox-template")
            >>> client.delete_all()
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
