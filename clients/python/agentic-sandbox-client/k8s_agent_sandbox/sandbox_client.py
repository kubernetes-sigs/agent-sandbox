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
import time
import atexit
import logging
from typing import List, Literal, Dict, Tuple, TypeVar, Generic, Type
from pydantic import BaseModel
from kubernetes import client

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
    ):
        # Sandbox related configuration
        self.connection_config = connection_config or SandboxLocalTunnelConnectionConfig()
        
        # Tracer configuration
        self.tracer_config = tracer_config or SandboxTracerConfig()
        if self.tracer_config.enable_tracing:
            initialize_tracer(self.tracer_config.trace_service_name)
        self.tracing_manager, self.tracer = create_tracer_manager(self.tracer_config)

        # Downstream Kubernetes Configuration
        self.k8s_helper = K8sHelper()
        
        # Tracks all the active client side connections to the created sandbox claims
        self._active_connection_sandboxes: Dict[Tuple[str, str], T] = {}
        
        # Register global cleanup for all tracked sandboxes.
        # Deletes all the sandboxes on program termination
        atexit.register(self.delete_all)

    def create_sandbox(self, template: str, namespace: str = "default", sandbox_ready_timeout: int = 180) -> T:
        """Provisions new Sandbox claim and returns a Sandbox handle which tracks 
           the underlying infrastructure.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.create_sandbox(template="python-sandbox-template")
            >>> sandbox.commands.run("echo 'Hello World'")
        """
        claim_name = f"sandbox-claim-{os.urandom(4).hex()}"
        
        try:
            self._create_claim(claim_name, template, namespace)
            # Resolve the sandbox id from the sandbox claim object.
            # In case of warmpool, sandbox id is not the same as claim name.
            start_time = time.monotonic()
            sandbox_id = self.k8s_helper.resolve_sandbox_name(
                claim_name, namespace, sandbox_ready_timeout
            )
            elapsed_time = time.monotonic() - start_time
            remaining_timeout = max(0, int(sandbox_ready_timeout - elapsed_time))
            if remaining_timeout <= 0:
                raise TimeoutError("Sandbox resolution exceeded the ready timeout.")
            self._wait_for_sandbox_ready(sandbox_id, namespace, remaining_timeout)
            
            # Resolve the pod name from the sandbox id
            pod_name = self._get_sandbox_pod_name(sandbox_id, namespace)

            sandbox = self.sandbox_class(
                claim_name=claim_name,
                sandbox_id=sandbox_id,
                namespace=namespace,
                connection_config=self.connection_config,
                tracer_config=self.tracer_config,
                k8s_helper=self.k8s_helper,
                pod_name=pod_name
            )
        except Exception:
            # If creation or waiting fails, ensure we don't leave an orphaned claim
            self._delete_claim(claim_name, namespace)
            raise

        self._active_connection_sandboxes[(namespace, claim_name)] = sandbox
        return sandbox

    def get_sandbox(self, claim_name: str, namespace: str = "default", resolve_timeout: int = 30) -> T:
        """
        Retrieves an existing sandbox handle given a sandbox claim name. 
        If the handle is closed or missing, it re-attaches to the infrastructure.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.get_sandbox("sandbox-claim-1234abcd")
            >>> sandbox.commands.run("ls -la")
        """
        key = (namespace, claim_name)
        existing = self._active_connection_sandboxes.get(key)

        # Check if the sandbox actually exists in Kubernetes
        try:
            sandbox_id = self.k8s_helper.resolve_sandbox_name(claim_name, namespace, timeout=resolve_timeout)
            sandbox_object = self.k8s_helper.get_sandbox(sandbox_id, namespace)
            if not sandbox_object:
                raise RuntimeError(f"Underlying Sandbox '{sandbox_id}' not found.")
        except Exception as e:
            if existing:
                existing.terminate()
            self._active_connection_sandboxes.pop(key, None)
            raise RuntimeError(f"Sandbox claim '{claim_name}' not found or resolution failed in namespace '{namespace}': {e}") from e

        # If it's already in the registry and active (and verified on K8s), return the existing object
        if existing and existing.is_active:
            return existing
            
        # If the sandbox is not active, pop it out from the tracking list
        if existing and not existing.is_active:
            self._active_connection_sandboxes.pop(key, None)

        pod_name = self._get_sandbox_pod_name(sandbox_id, namespace)

        # Re-attach: Create a fresh handle for the existing ID
        new_handle = self.sandbox_class(
            claim_name=claim_name,
            sandbox_id=sandbox_id,
            namespace=namespace,
            connection_config=self.connection_config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper,
            pod_name=pod_name
        )
        
        self._active_connection_sandboxes[key] = new_handle
        return new_handle
    
    def list_active_sandboxes(self) -> List[Tuple[str, str]]:
        """Returns a list of tuples containing (namespace, claim_name) currently managed by this client.
        
        Example:
        
            >>> client = SandboxClient()
            >>> client.create_sandbox("python-sandbox-template")
            >>> print(client.list_active_sandboxes())
            [('default', 'sandbox-claim-1234abcd')]
        """
        # We only return IDs that are still active/initialized, and clean up inactive ones.
        for key, obj in list(self._active_connection_sandboxes.items()):
            if not obj.is_active:
                self._active_connection_sandboxes.pop(key, None)
        return list(self._active_connection_sandboxes.keys())
      
    def list_all_sandboxes(self, namespace: str = "default") -> List[str]:
        """
        Lists all SandboxClaim names currently existing in the Kubernetes cluster 
        for the given namespace.
        
        Example:
        
            >>> client = SandboxClient()
            >>> print(client.list_all_sandboxes(namespace="default"))
            ['sandbox-claim-1234abcd', 'sandbox-claim-5678efgh']
        """
        return self.k8s_helper.list_sandbox_claims(namespace)

    def delete_sandbox(self, claim_name: str, namespace: str = "default"):
        """Stops the client side connection and deletes the Kubernetes resources.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.create_sandbox("python-sandbox-template")
            >>> client.delete_sandbox(sandbox.claim_name)
        """
        key = (namespace, claim_name)
        sandbox = self._active_connection_sandboxes.get(key)
        try:
            if sandbox:
                sandbox.terminate()
                self._active_connection_sandboxes.pop(key, None)
            else:
                self._delete_claim(claim_name, namespace)
        except Exception as e:
            logging.error(f"Failed to delete sandbox '{claim_name}' in namespace '{namespace}': {e}")
            
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
        for ns, claim_name in list(self._active_connection_sandboxes.keys()):
            try:
                self.delete_sandbox(claim_name, namespace=ns)
            except Exception as e:
                logging.error(
                    f"Cleanup failed for {claim_name} in namespace {ns}: {e}"
                )

    def _get_sandbox_pod_name(self, sandbox_id: str, namespace: str) -> str:
        """Fetches the Sandbox object from Kubernetes and retrieves its pod name."""
        sandbox_object = self.k8s_helper.get_sandbox(sandbox_id, namespace) or {}
        metadata = sandbox_object.get('metadata') or {}
        annotations = metadata.get('annotations') or {}
        return annotations.get(POD_NAME_ANNOTATION) or sandbox_id
    
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
    def _wait_for_sandbox_ready(self, sandbox_id: str, namespace: str, timeout: int):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        self.k8s_helper.wait_for_sandbox_ready(sandbox_id, namespace, timeout)
        
    @trace_span("delete_claim")
    def _delete_claim(self, claim_name: str, namespace: str):
        """Deletes the SandboxClaim custom resource from the Kubernetes cluster."""
        try:
            self.k8s_helper.delete_sandbox_claim(claim_name, namespace)
        except Exception as e:
            logging.error(f"Failed to cleanup SandboxClaim '{claim_name}': {e}")
