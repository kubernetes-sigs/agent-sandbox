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
from typing import List, Literal
from pydantic import BaseModel

# Import all tracing components from the trace_manager module
from .trace_manager import (
    initialize_tracer, TracerManager, trace_span, trace, OPENTELEMETRY_AVAILABLE
)
from .sandbox import Sandbox
from .models import SandboxRouterConfig, SandboxTracerConfig
from .k8s_helper import K8sHelper

POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name"

logging.basicConfig(level=logging.INFO,
                    format='%(asctime)s - %(levelname)s - %(message)s',
                    stream=sys.stdout)

class SandboxClient:
    """
    A client for creating and interacting with a stateful Sandbox via a router.
    """

    def __init__(
        self,
        config: SandboxRouterConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
        sandbox_ready_timeout: int = 180,
    ):
        self.config = config or SandboxRouterConfig()
        self.tracer_config = tracer_config or SandboxTracerConfig()
        self.sandbox_ready_timeout = sandbox_ready_timeout
        self.tracing_manager = None
        self.tracer = None
        
        if self.tracer_config.enable_tracing:
            if not OPENTELEMETRY_AVAILABLE:
                logging.error(
                    "OpenTelemetry not installed; skipping tracer initialization.")
            else:
                initialize_tracer(service_name=self.tracer_config.trace_service_name)
                self.tracing_manager = TracerManager(
                    service_name=self.tracer_config.trace_service_name)
                self.tracer = self.tracing_manager.tracer

        self.k8s_helper = K8sHelper()

    def create_sandbox(self, template: str, namespace: str = "default") -> Sandbox:
        """Provisions a new sandbox and returns a Resource Handle."""
        target_namespace = namespace
        claim_name = f"sandbox-claim-{os.urandom(4).hex()}"

        self._create_claim(claim_name, template, target_namespace)
        self._wait_for_sandbox_ready(claim_name, target_namespace)

        return Sandbox(
            sandbox_id=claim_name,
            namespace=target_namespace,
            router_config=self.config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper
        )

    def get_sandbox(self, sandbox_id: str, namespace: str = "default") -> Sandbox:
        """Re-attaches to an existing sandbox by ID."""
        return Sandbox(
            sandbox_id=sandbox_id,
            namespace=namespace,
            router_config=self.config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper
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
