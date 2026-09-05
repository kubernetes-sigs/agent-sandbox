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
"""
This module provides the SandboxClient for interacting with the Agentic Sandbox.
It handles lifecycle management (claiming, waiting) and interaction (execution,
file I/O) via the Sandbox resource handle.
"""

import uuid
import atexit
import sys
import logging
from functools import partial
from threading import RLock
from typing import List, Dict, Tuple, TypeVar, Generic, Type

from kubernetes.client import ApiException

# Import all tracing components from the trace_manager module
from .trace_manager import (
    create_tracer_manager, initialize_tracer, trace_span, trace
)
from .sandbox import Sandbox
from .models import (
    SandboxConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxTracerConfig,
)
from .k8s_helper import K8sHelper
from .claim_adoption import (
    get_ready_sandbox_name,
    validate_claim_for_adoption,
    validate_claim_name,
)
from .pod_metadata import build_pod_metadata, validate_labels
from .utils import construct_sandbox_claim_lifecycle_spec
from .exceptions import SandboxNotFoundError

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
        cleanup: bool = False,
    ) -> None:
        """
        Initializes the SandboxClient.

        Args:
            connection_config: Configuration for connecting to the sandboxes. 
                Defaults to SandboxLocalTunnelConnectionConfig() which uses 
                kubectl port-forwarding. Can also be SandboxDirectConnectionConfig 
                or SandboxGatewayConnectionConfig.
            tracer_config: Configuration for OpenTelemetry tracing. 
                Defaults to an empty SandboxTracerConfig (tracing disabled).
            cleanup: If True, registers an atexit hook to automatically delete
                internally named sandboxes when the program terminates. Explicitly
                named claims remain caller-owned. Defaults to False.
        """
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
        self._automatic_cleanup_claims: set[Tuple[str, str]] = set()
        self._lock = RLock()
        
        # Optional automatic cleanup of sandboxes on program termination
        if cleanup:
            atexit.register(self._delete_automatic_cleanup_claims)

    def create_sandbox(
        self,
        warmpool: str,
        namespace: str = "default",
        sandbox_ready_timeout: int = 180,
        labels: dict[str, str] | None = None,
        *,
        claim_name: str | None = None,
        adopt_existing: bool = False,
        shutdown_after_seconds: int | None = None,
        volume_claim_templates: list[dict] | None = None,
        pod_labels: dict[str, str] | None = None,
        pod_annotations: dict[str, str] | None = None,
        env: dict[str, str] | None = None,
    ) -> T:
        """Provisions new Sandbox claim and returns a Sandbox handle which tracks
           the underlying infrastructure.

        Args:
            warmpool: Name of the SandboxWarmPool to use.
            namespace: Kubernetes namespace for the claim.
            sandbox_ready_timeout: Seconds to wait for the sandbox to be ready.
            labels: Optional Kubernetes labels to attach to the claim object
                (``SandboxClaim.metadata.labels``).
            claim_name: Optional deterministic SandboxClaim name. When omitted,
                the client preserves its random-name behavior and owns automatic
                cleanup. Explicitly named claims remain caller-owned.
            adopt_existing: On a create conflict, adopt the exact existing
                ``claim_name`` only after validating its immutable request
                contract. Requires ``claim_name`` and cannot be combined with
                ``shutdown_after_seconds``.
            shutdown_after_seconds: Optional TTL in seconds. When set, the
                claim's ``spec.lifecycle`` is populated with a ``shutdownTime``
                of *now + shutdown_after_seconds* (UTC) and a ``shutdownPolicy``
                of ``"Delete"``, so the controller auto-deletes the claim on
                expiry. Must be a positive integer.
            volume_claim_templates: Optional list of volume claim templates
                to override/merge with the sandbox template.
            pod_labels: Optional labels stamped onto the running Sandbox **Pod**
                via ``spec.additionalPodMetadata.labels``. Unlike ``labels``
                (which land on the claim object), these are readable from inside
                the sandbox through the Downward API.
            pod_annotations: Optional annotations stamped onto the running
                Sandbox **Pod** via ``spec.additionalPodMetadata.annotations``.
            env: Optional environment variables to inject into the SandboxClaim.
                Setting this populates ``spec.env`` and forces a cold start
                from the warm pool template instead of adopting a pre-warmed
                pod, which may increase startup latency.

        Example:

            >>> client = SandboxClient()
            >>> sandbox = client.create_sandbox(warmpool="python-sandbox-pool")
            >>> sandbox.commands.run("echo 'Hello World'")
        """
        if not warmpool:
            raise ValueError("Warmpool name cannot be empty.")

        if labels:
            validate_labels(labels)

        if adopt_existing and claim_name is None:
            raise ValueError("adopt_existing requires an explicit claim_name.")
        if adopt_existing and shutdown_after_seconds is not None:
            raise ValueError(
                "adopt_existing cannot be combined with shutdown_after_seconds "
                "because each retry computes a different shutdownTime."
            )

        pod_metadata = build_pod_metadata(pod_labels, pod_annotations)

        lifecycle = construct_sandbox_claim_lifecycle_spec(shutdown_after_seconds) if shutdown_after_seconds is not None else None

        generated_claim_name = claim_name is None
        if generated_claim_name:
            claim_name = f"sandbox-claim-{uuid.uuid4().hex[:8]}"
        else:
            validate_claim_name(claim_name)

        key = (namespace, claim_name)
        adopted_sandbox_id = None
        claim_validator = None
        validate_expected_claim = None
        if not generated_claim_name:
            validate_expected_claim = partial(
                validate_claim_for_adoption,
                claim_name=claim_name,
                namespace=namespace,
                warmpool=warmpool,
                labels=labels,
                lifecycle=lifecycle,
                volume_claim_templates=volume_claim_templates,
                pod_metadata=pod_metadata,
                env=env,
            )
            with self._lock:
                self._automatic_cleanup_claims.discard(key)
        try:
            try:
                created_claim = self._create_claim(
                    claim_name,
                    warmpool,
                    namespace,
                    labels=labels,
                    lifecycle=lifecycle,
                    volume_claim_templates=volume_claim_templates,
                    pod_metadata=pod_metadata,
                    env=env,
                )
                if validate_expected_claim is None:
                    claim_rv = None
                    if isinstance(created_claim, dict):
                        claim_rv = (created_claim.get("metadata") or {}).get("resourceVersion")
                else:
                    claim_identity = validate_expected_claim(created_claim)
                    claim_rv = claim_identity.resource_version
                    claim_validator = partial(
                        validate_expected_claim,
                        expected_uid=claim_identity.uid,
                    )
            except ApiException as error:
                if not (adopt_existing and error.status == 409):
                    raise
                existing_claim = self.k8s_helper.get_sandbox_claim(
                    claim_name, namespace
                )
                claim_identity = validate_expected_claim(existing_claim)
                claim_rv = claim_identity.resource_version
                claim_validator = partial(
                    validate_expected_claim,
                    expected_uid=claim_identity.uid,
                )
                adopted_sandbox_id = get_ready_sandbox_name(
                    existing_claim, claim_name
                )
            # Wait for the claim to be bound and Ready in a single watch.
            # The claim status carries the sandbox name (which differs from
            # the claim name with warm pools) and the forwarded Ready
            # condition in the same status update, so no second watch on the
            # Sandbox resource is needed. The watch starts from the create
            # response's resourceVersion so the apiserver serves it from the
            # watch cache instead of a quorum etcd read per wait.
            sandbox_id = adopted_sandbox_id
            if sandbox_id is None:
                if claim_validator is None:
                    sandbox_id = self._wait_for_claim_ready(
                        claim_name,
                        namespace,
                        sandbox_ready_timeout,
                        resource_version=claim_rv,
                    )
                else:
                    sandbox_id = self._wait_for_claim_ready(
                        claim_name,
                        namespace,
                        sandbox_ready_timeout,
                        resource_version=claim_rv,
                        claim_validator=claim_validator,
                    )

            sandbox = self.sandbox_class(
                claim_name=claim_name,
                sandbox_id=sandbox_id,
                namespace=namespace,
                connection_config=self.connection_config,
                tracer_config=self.tracer_config,
                k8s_helper=self.k8s_helper,
            )
        except Exception:
            if generated_claim_name:
                self._delete_claim(claim_name, namespace)
            raise

        with self._lock:
            self._active_connection_sandboxes[key] = sandbox
            if generated_claim_name:
                self._automatic_cleanup_claims.add(key)
        return sandbox

    def get_sandbox(
        self,
        claim_name: str,
        namespace: str = "default",
        resolve_timeout: int = 30,
    ) -> T:
        """
        Retrieves an existing sandbox handle given a sandbox claim name.
        If the handle is closed or missing, it re-attaches to the infrastructure.
        Reattached claims remain caller-owned: automatic cleanup never deletes
        them. Call ``delete_sandbox`` or ``delete_all`` for deliberate deletion.

        Args:
            claim_name: Name of the SandboxClaim to attach to.
            namespace: Kubernetes namespace the claim lives in.
            resolve_timeout: Seconds to wait while resolving the sandbox
                name from the claim status.
        Example:

            >>> client = SandboxClient()
            >>> sandbox = client.get_sandbox(
            ...     "sandbox-claim-1234abcd",
            ... )
            >>> sandbox.commands.run("ls -la")
        """
        key = (namespace, claim_name)
        with self._lock:
            existing = self._active_connection_sandboxes.get(key)

        # Check if the sandbox actually exists in Kubernetes
        try:
            sandbox_id = self.k8s_helper.resolve_sandbox_name(claim_name, namespace, timeout=resolve_timeout)
            sandbox_object = self.k8s_helper.get_sandbox(sandbox_id, namespace)
            if not sandbox_object:
                raise SandboxNotFoundError(f"Underlying Sandbox '{sandbox_id}' not found.")
        except Exception as e:
            self._detach_failed_lookup(key, existing)
            raise SandboxNotFoundError(f"Sandbox claim '{claim_name}' not found or resolution failed in namespace '{namespace}': {e}") from e

        return self._reuse_or_replace_resolved_handle(
            key, existing, claim_name, sandbox_id, namespace
        )

    def _detach_failed_lookup(
        self, key: Tuple[str, str], expected_handle: T | None
    ) -> None:
        """Detach only the handle observed by the failed lookup."""
        if expected_handle is None:
            return
        with self._lock:
            current_handle = self._active_connection_sandboxes.get(key)
            if current_handle is not expected_handle:
                expected_handle.close_connection()
                return
            if key in self._automatic_cleanup_claims:
                expected_handle.terminate()
                self._automatic_cleanup_claims.discard(key)
            else:
                expected_handle.close_connection()
            if self._active_connection_sandboxes.get(key) is expected_handle:
                self._active_connection_sandboxes.pop(key, None)

    def _reuse_or_replace_resolved_handle(
        self,
        key: Tuple[str, str],
        expected_handle: T | None,
        claim_name: str,
        sandbox_id: str,
        namespace: str,
    ) -> T:
        """Install a resolved handle without overwriting a concurrent replacement."""
        with self._lock:
            current_handle = self._active_connection_sandboxes.get(key)
            if current_handle is not expected_handle and expected_handle is not None:
                expected_handle.close_connection()
            if current_handle is not None and current_handle.is_active:
                return current_handle
            if current_handle is not None:
                current_handle.close_connection()
            new_handle = self.sandbox_class(
                claim_name=claim_name,
                sandbox_id=sandbox_id,
                namespace=namespace,
                connection_config=self.connection_config,
                tracer_config=self.tracer_config,
                k8s_helper=self.k8s_helper,
            )
            self._active_connection_sandboxes[key] = new_handle
            return new_handle
    
    def list_active_sandboxes(self) -> List[Tuple[str, str]]:
        """Returns a list of tuples containing (namespace, claim_name) currently managed by this client.
        
        Example:
        
            >>> client = SandboxClient()
            >>> client.create_sandbox("python-sandbox-pool")
            >>> print(client.list_active_sandboxes())
            [('default', 'sandbox-claim-1234abcd')]
        """
        # We only return IDs that are still active/initialized, and clean up inactive ones.
        with self._lock:
            for key, obj in list(self._active_connection_sandboxes.items()):
                if not obj.is_active:
                    self._active_connection_sandboxes.pop(key, None)
            return list(self._active_connection_sandboxes.keys())
      
    def list_all_sandboxes(self, namespace: str = "default", label_selector: str | None = None) -> List[str]:
        """
        Lists all SandboxClaim names currently existing in the Kubernetes cluster
        for the given namespace.

        Args:
            namespace: Kubernetes namespace to list claims in.
            label_selector: Optional Kubernetes label selector string
                (e.g. ``"app=myapp"``). When set, only claims matching
                the selector are returned.

        Example:

            >>> client = SandboxClient()
            >>> print(client.list_all_sandboxes(namespace="default"))
            ['sandbox-claim-1234abcd', 'sandbox-claim-5678efgh']
        """
        return self.k8s_helper.list_sandbox_claims(namespace, label_selector=label_selector)

    def delete_sandbox(self, claim_name: str, namespace: str = "default") -> None:
        """Stops the client side connection and deletes the Kubernetes resources.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.create_sandbox("python-sandbox-pool")
            >>> client.delete_sandbox(sandbox.claim_name)
        """
        key = (namespace, claim_name)
        try:
            with self._lock:
                sandbox = self._active_connection_sandboxes.get(key)
                if sandbox:
                    sandbox.terminate()
                    if self._active_connection_sandboxes.get(key) is sandbox:
                        self._active_connection_sandboxes.pop(key, None)
                else:
                    self._delete_claim(claim_name, namespace)
                self._automatic_cleanup_claims.discard(key)
        except Exception as e:
            logging.error(f"Failed to delete sandbox '{claim_name}' in namespace '{namespace}': {e}")
            
    def delete_all(self) -> None:
        """
        Deliberately delete every sandbox tracked by this client.
        
        Example:
        
            >>> client = SandboxClient()
            >>> client.create_sandbox("python-sandbox-pool")
            >>> client.create_sandbox("python-sandbox-pool")
            >>> client.delete_all()
        """
        with self._lock:
            claims = list(self._active_connection_sandboxes)
        for ns, claim_name in claims:
            try:
                self.delete_sandbox(claim_name, namespace=ns)
            except Exception as e:
                logging.error(
                    f"Cleanup failed for {claim_name} in namespace {ns}: {e}"
                )

    def _delete_automatic_cleanup_claims(self):
        """Best-effort cleanup of internally named claims only."""
        with self._lock:
            claims = list(self._automatic_cleanup_claims)
        for ns, claim_name in claims:
            try:
                self._delete_automatic_cleanup_claim((ns, claim_name))
            except Exception as e:
                logging.error(
                    f"Cleanup failed for {claim_name} in namespace {ns}: {e}"
                )

    def _delete_automatic_cleanup_claim(self, key: Tuple[str, str]) -> None:
        """Delete a claim only while this client still owns its cleanup."""
        with self._lock:
            if key not in self._automatic_cleanup_claims:
                return
            namespace, claim_name = key
            sandbox = self._active_connection_sandboxes.get(key)
            if sandbox is None:
                self._delete_claim(claim_name, namespace)
            else:
                sandbox.terminate()
            if self._active_connection_sandboxes.get(key) is sandbox:
                self._active_connection_sandboxes.pop(key, None)
            self._automatic_cleanup_claims.discard(key)

    @trace_span("create_claim")
    def _create_claim(
        self,
        claim_name: str,
        warmpool_name: str,
        namespace: str,
        labels: dict[str, str] | None = None,
        lifecycle: dict | None = None,
        volume_claim_templates: list[dict] | None = None,
        pod_metadata: dict | None = None,
        env: dict[str, str] | None = None,
    ):
        """Creates the SandboxClaim custom resource in the Kubernetes cluster."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.claim.name", claim_name)
            if lifecycle:
                span.set_attribute("sandbox.lifecycle.shutdown_time", lifecycle["shutdownTime"])
                span.set_attribute("sandbox.lifecycle.shutdown_policy", lifecycle["shutdownPolicy"])

        annotations = {}
        if self.tracing_manager:
            trace_context_str = self.tracing_manager.get_trace_context_json()
            if trace_context_str:
                annotations["opentelemetry.io/trace-context"] = trace_context_str

        return self.k8s_helper.create_sandbox_claim(
            claim_name,
            warmpool_name,
            namespace,
            annotations=annotations,
            labels=labels,
            lifecycle=lifecycle,
            volume_claim_templates=volume_claim_templates,
            pod_metadata=pod_metadata,
            env=env,
        )

    @trace_span("wait_for_claim_ready")
    def _wait_for_claim_ready(
        self,
        claim_name: str,
        namespace: str,
        timeout: int,
        resource_version: str | None = None,
        claim_validator=None,
    ) -> str:
        """Waits for the SandboxClaim to be bound and Ready, returning the sandbox name."""
        if claim_validator is None:
            return self.k8s_helper.wait_for_claim_ready(
                claim_name,
                namespace,
                timeout,
                resource_version=resource_version,
            )
        return self.k8s_helper.wait_for_claim_ready(
            claim_name,
            namespace,
            timeout,
            resource_version=resource_version,
            claim_validator=claim_validator,
        )

    @trace_span("wait_for_sandbox_ready")
    def _wait_for_sandbox_ready(
        self, sandbox_id: str, namespace: str, timeout: int
    ) -> None:
        self.k8s_helper.wait_for_sandbox_ready(sandbox_id, namespace, timeout)

    @trace_span("delete_claim")
    def _delete_claim(self, claim_name: str, namespace: str) -> None:
        """Deletes the SandboxClaim custom resource from the Kubernetes cluster."""
        self.k8s_helper.delete_sandbox_claim(claim_name, namespace)

    def get_sandbox_claim_warmpool_name(self, claim_name: str, namespace: str) -> str:
        """Get warmpool name of a sandbox claim."""
        claim_object = self.k8s_helper.get_sandbox_claim(claim_name, namespace)
        if not claim_object:
            raise SandboxNotFoundError(
                f"SandboxClaim '{claim_name}' not found in namespace '{namespace}'."
            )
        warmpool_name = (
            claim_object.get("spec", {})
            .get("warmPoolRef", {})
            .get("name")
        )
        return warmpool_name
