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
    validate_claim_identity,
    validate_claim_name,
)
from .claim_ownership import (
    ClaimLookupOperation,
    ClaimOwnership,
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
    ):
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
                managed sandboxes when the program terminates. This includes
                internally named and reattached claims; explicitly named claims
                created by this client remain caller-owned. Defaults to False.
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
        self._active_claim_uids: Dict[Tuple[str, str], str | None] = {}
        self._claim_ownership = ClaimOwnership()
        self._automatic_cleanup_claims = (
            self._claim_ownership.automatic_cleanup_claims
        )
        self._automatic_cleanup_claim_uids = (
            self._claim_ownership.automatic_cleanup_claim_uids
        )
        self._caller_owned_claims = self._claim_ownership.caller_owned_claims
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
        cleanup_generated_claim = generated_claim_name
        claim_uid = None
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
            expected_handle = self._active_connection_sandboxes.get(key)
            if not generated_claim_name:
                self._claim_ownership.mark_caller_owned(key)
        sandbox: T | None = None
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
                        metadata = created_claim.get("metadata") or {}
                        claim_rv = metadata.get("resourceVersion")
                        uid = metadata.get("uid")
                        if isinstance(uid, str) and uid:
                            claim_uid = uid
                else:
                    claim_identity = validate_expected_claim(created_claim)
                    claim_uid = claim_identity.uid
                    claim_rv = claim_identity.resource_version
                    claim_validator = partial(
                        validate_expected_claim,
                        expected_uid=claim_identity.uid,
                    )
            except ApiException as error:
                if generated_claim_name and error.status == 409:
                    cleanup_generated_claim = False
                if not (adopt_existing and error.status == 409):
                    raise
                existing_claim = self.k8s_helper.get_sandbox_claim(
                    claim_name, namespace
                )
                if existing_claim is None:
                    raise SandboxNotFoundError(
                        f"SandboxClaim '{claim_name}' disappeared after the "
                        "create conflict; retry the request."
                    )
                assert validate_expected_claim is not None
                claim_identity = validate_expected_claim(existing_claim)
                claim_uid = claim_identity.uid
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
            return self._register_created_handle(
                key,
                sandbox,
                generated_claim_name,
                expected_handle,
                claim_uid,
            )
        except Exception:
            self._rollback_failed_creation(
                sandbox,
                key,
                claim_uid,
                cleanup_generated_claim,
            )
            raise

    def _register_created_handle(
        self,
        key: Tuple[str, str],
        sandbox: T,
        generated_claim_name: bool,
        expected_handle: T | None,
        claim_uid: str | None,
    ) -> T:
        """Register one handle and close superseded handles outside the lock."""
        stale_handles: list[T] = []
        result: T | None = None
        concurrent_change = False
        with self._lock:
            current_handle = self._active_connection_sandboxes.get(key)
            if current_handle is not expected_handle:
                if expected_handle is not None:
                    stale_handles.append(expected_handle)
                if self._handle_matches_claim(
                    key, current_handle, sandbox, claim_uid
                ):
                    stale_handles.append(sandbox)
                    result = current_handle
                else:
                    concurrent_change = True
            elif self._handle_matches_claim(
                key, current_handle, sandbox, claim_uid
            ):
                stale_handles.append(sandbox)
                result = current_handle
            else:
                if current_handle is not None:
                    stale_handles.append(current_handle)
                self._active_connection_sandboxes[key] = sandbox
                self._active_claim_uids[key] = claim_uid
                if generated_claim_name:
                    self._claim_ownership.register_automatic(key, claim_uid)
                result = sandbox

        for stale_handle in stale_handles:
            self._close_handle_best_effort(stale_handle, retire=True)
        if concurrent_change:
            raise self._concurrent_claim_change(key)
        assert result is not None
        return result

    def _handle_matches_claim(
        self, key: Tuple[str, str], current: T | None, candidate: T, uid: str | None
    ) -> bool:
        """Return whether a handle belongs to the same Claim incarnation."""
        return (
            self._active_handle_has_claim_uid(key, current, uid)
            and current is not None
            and current.sandbox_id == candidate.sandbox_id
        )

    def _active_handle_has_claim_uid(
        self, key: Tuple[str, str], handle: T | None, uid: str | None
    ) -> bool:
        """Return whether an active handle has the observed Claim UID."""
        return (
            handle is not None
            and handle.is_active
            and self._active_claim_uids.get(key) == uid
        )

    def _delete_failed_generated_claim_if_owned(
        self, key: Tuple[str, str], expected_uid: str | None
    ) -> None:
        """Roll back a generated Claim unless explicit ownership superseded it."""
        with self._lock:
            should_delete = self._claim_ownership.failed_generated_needs_delete(
                key,
                has_registered_handle=key in self._active_connection_sandboxes,
                claim_uid=expected_uid,
            )
            if not should_delete:
                return
        namespace, claim_name = key
        self._delete_claim_with_optional_uid(
            claim_name, namespace, expected_uid
        )
        assert expected_uid
        with self._lock:
            self._claim_ownership.discard_automatic_if_uid(
                key, expected_uid
            )

    def _rollback_failed_creation(
        self,
        sandbox: T | None,
        key: Tuple[str, str],
        expected_uid: str | None,
        cleanup_generated_claim: bool,
    ) -> None:
        """Best-effort rollback that cannot replace the original failure."""
        if sandbox is not None:
            with self._lock:
                if self._active_connection_sandboxes.get(key) is sandbox:
                    self._active_connection_sandboxes.pop(key, None)
                    self._active_claim_uids.pop(key, None)
            try:
                self._retire_stale_handle(sandbox)
            except Exception as error:
                logging.error(f"Failed to close candidate sandbox: {error}")
        if not cleanup_generated_claim:
            return
        try:
            self._delete_failed_generated_claim_if_owned(key, expected_uid)
        except Exception as error:
            logging.error(f"Failed to delete generated SandboxClaim: {error}")

    @staticmethod
    def _concurrent_claim_change(key: Tuple[str, str]) -> RuntimeError:
        namespace, claim_name = key
        return RuntimeError(
            f"SandboxClaim '{claim_name}' in namespace '{namespace}' "
            "changed concurrently; retry the operation."
        )

    def get_sandbox(
        self,
        claim_name: str,
        namespace: str = "default",
        resolve_timeout: int = 30,
    ) -> T:
        """
        Retrieves an existing sandbox handle given a sandbox claim name.
        If the handle is closed or missing, it re-attaches to the infrastructure.
        Reattached handles preserve the client's historical automatic cleanup
        behavior. Cleanup is constrained to the exact observed Claim UID.

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
            lookup_operation = self._claim_ownership.begin_lookup(key)

        try:
            try:
                claim_object = self.k8s_helper.get_sandbox_claim(
                    claim_name, namespace
                )
                claim_identity = validate_claim_identity(
                    claim_object,
                    claim_name=claim_name,
                    namespace=namespace,
                )
                claim_validator = partial(
                    validate_claim_identity,
                    claim_name=claim_name,
                    namespace=namespace,
                    expected_uid=claim_identity.uid,
                )
                sandbox_id = self.k8s_helper.resolve_sandbox_name(
                    claim_name,
                    namespace,
                    timeout=resolve_timeout,
                    claim_validator=claim_validator,
                )
                sandbox_object = self.k8s_helper.get_sandbox(
                    sandbox_id, namespace
                )
                if not sandbox_object:
                    raise SandboxNotFoundError(
                        f"Underlying Sandbox '{sandbox_id}' not found."
                    )
            except Exception as error:
                self._detach_failed_lookup(key, existing)
                message = (
                    f"Sandbox claim '{claim_name}' not found or resolution "
                    f"failed in namespace '{namespace}': {error}"
                )
                raise SandboxNotFoundError(message) from error

            return self._reuse_or_replace_resolved_handle(
                key,
                existing,
                lookup_operation,
                claim_name,
                sandbox_id,
                namespace,
                claim_identity.uid,
            )
        finally:
            with self._lock:
                self._claim_ownership.finish_lookup(key, lookup_operation)

    def _detach_failed_lookup(
        self, key: Tuple[str, str], expected_handle: T | None
    ) -> None:
        """Detach only the handle observed by the failed lookup."""
        if expected_handle is None:
            return
        should_delete = False
        expected_uid = None
        with self._lock:
            current_handle = self._active_connection_sandboxes.get(key)
            if current_handle is not expected_handle:
                return
            automatic_cleanup = self._claim_ownership.should_retire_handle(
                key
            )
            self._active_connection_sandboxes.pop(key, None)
            self._active_claim_uids.pop(key, None)
            should_delete, expected_uid = (
                self._claim_ownership.take_automatic_cleanup(key)
            )

        self._close_handle_best_effort(
            expected_handle, retire=automatic_cleanup
        )
        if should_delete:
            namespace, claim_name = key
            try:
                self._delete_claim_with_optional_uid(
                    claim_name, namespace, expected_uid
                )
            except Exception as error:
                with self._lock:
                    self._claim_ownership.register_automatic(
                        key, expected_uid
                    )
                logging.error(f"Failed to delete stale SandboxClaim: {error}")

    def _close_handle_best_effort(
        self, sandbox: T, *, retire: bool
    ) -> None:
        """Close a detached handle while preserving the preceding outcome."""
        try:
            if retire:
                self._retire_stale_handle(sandbox)
            else:
                sandbox.close_connection()
        except Exception as error:
            logging.error(f"Failed to close stale sandbox handle: {error}")

    @staticmethod
    def _retire_stale_handle(sandbox: T) -> None:
        """Disable name-only deletion from a superseded handle."""
        if sandbox.claim_name is None:
            return
        sandbox.claim_name = None
        sandbox.close_connection()

    def _reuse_or_replace_resolved_handle(
        self,
        key: Tuple[str, str],
        expected_handle: T | None,
        lookup_operation: ClaimLookupOperation,
        claim_name: str,
        sandbox_id: str,
        namespace: str,
        claim_uid: str,
    ) -> T:
        """Install a resolved handle without overwriting a concurrent replacement."""
        stale_handles: list[T] = []
        result: T | None = None
        concurrent_change = False
        with self._lock:
            if not self._claim_ownership.lookup_is_valid(
                key, lookup_operation
            ):
                raise self._concurrent_claim_change(key)
            current_handle = self._active_connection_sandboxes.get(key)
            if current_handle is not expected_handle:
                if expected_handle is not None:
                    stale_handles.append(expected_handle)
                if self._resolved_handle_matches_claim(
                    key, current_handle, sandbox_id, claim_uid
                ):
                    self._claim_ownership.register_automatic(key, claim_uid)
                    result = current_handle
                else:
                    concurrent_change = True
            elif self._resolved_handle_matches_claim(
                key, current_handle, sandbox_id, claim_uid
            ):
                self._claim_ownership.register_automatic(key, claim_uid)
                result = current_handle
            else:
                if current_handle is not None:
                    stale_handles.append(current_handle)
                new_handle = self.sandbox_class(
                    claim_name=claim_name,
                    sandbox_id=sandbox_id,
                    namespace=namespace,
                    connection_config=self.connection_config,
                    tracer_config=self.tracer_config,
                    k8s_helper=self.k8s_helper,
                )
                self._active_connection_sandboxes[key] = new_handle
                self._active_claim_uids[key] = claim_uid
                self._claim_ownership.register_automatic(key, claim_uid)
                result = new_handle

        for stale_handle in stale_handles:
            self._close_handle_best_effort(stale_handle, retire=True)
        if concurrent_change:
            raise self._concurrent_claim_change(key)
        assert result is not None
        return result

    def _resolved_handle_matches_claim(
        self, key: Tuple[str, str], handle: T | None, sandbox_id: str, uid: str
    ) -> bool:
        """Return whether a resolved identity matches a registered handle."""
        return (
            handle is not None
            and handle.is_active
            and handle.sandbox_id == sandbox_id
            and self._active_claim_uids.get(key) == uid
        )
    
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
                    if self._claim_ownership.should_retire_handle(key):
                        obj.claim_name = None
                    self._active_connection_sandboxes.pop(key, None)
                    self._active_claim_uids.pop(key, None)
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

    def delete_sandbox(self, claim_name: str, namespace: str = "default"):
        """Stops the client side connection and deletes the Kubernetes resources.
        
        Example:
        
            >>> client = SandboxClient()
            >>> sandbox = client.create_sandbox("python-sandbox-pool")
            >>> client.delete_sandbox(sandbox.claim_name)
        """
        key = (namespace, claim_name)
        with self._lock:
            sandbox = self._active_connection_sandboxes.pop(key, None)
            active_uid = self._active_claim_uids.pop(key, None)
            automatic_owned = key in self._automatic_cleanup_claims
            automatic_uid = self._claim_ownership.automatic_cleanup_uid(key)
            caller_owned = key in self._caller_owned_claims
            self._claim_ownership.discard(key)
        try:
            if sandbox:
                self._retire_stale_handle(sandbox)
                if active_uid:
                    self._delete_claim_with_optional_uid(
                        claim_name, namespace, active_uid
                    )
                else:
                    self._delete_claim(claim_name, namespace)
            else:
                self._delete_claim(claim_name, namespace)
        except Exception as e:
            with self._lock:
                if (
                    sandbox is not None
                    and key not in self._active_connection_sandboxes
                ):
                    self._active_connection_sandboxes[key] = sandbox
                    self._active_claim_uids[key] = active_uid
                if caller_owned:
                    self._claim_ownership.mark_caller_owned(key)
                elif automatic_owned:
                    self._claim_ownership.register_automatic(
                        key, automatic_uid
                    )
            logging.error(f"Failed to delete sandbox '{claim_name}' in namespace '{namespace}': {e}")
            
    def delete_all(self):
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
        """Best-effort cleanup of automatically managed claims."""
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
            should_delete, expected_uid = (
                self._claim_ownership.take_automatic_cleanup(key)
            )
            if not should_delete:
                return
            namespace, claim_name = key
            sandbox = self._active_connection_sandboxes.pop(key, None)
            self._active_claim_uids.pop(key, None)

        if sandbox is not None:
            self._close_handle_best_effort(sandbox, retire=True)
        try:
            self._delete_claim_with_optional_uid(
                claim_name, namespace, expected_uid
            )
        except Exception:
            with self._lock:
                self._claim_ownership.register_automatic(key, expected_uid)
            raise

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
        return self.k8s_helper.wait_for_claim_ready(
            claim_name,
            namespace,
            timeout,
            resource_version=resource_version,
            claim_validator=claim_validator,
        )

    @trace_span("wait_for_sandbox_ready")
    def _wait_for_sandbox_ready(self, sandbox_id: str, namespace: str, timeout: int):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        self.k8s_helper.wait_for_sandbox_ready(sandbox_id, namespace, timeout)

    @trace_span("delete_claim")
    def _delete_claim(self, claim_name: str, namespace: str):
        """Deletes the SandboxClaim custom resource from the Kubernetes cluster."""
        self.k8s_helper.delete_sandbox_claim(claim_name, namespace)

    def _delete_claim_with_optional_uid(
        self,
        claim_name: str,
        namespace: str,
        expected_uid: str | None,
    ) -> None:
        """Delete a Claim only when its exact identity is known."""
        if not expected_uid:
            return
        self.k8s_helper.delete_sandbox_claim(
            claim_name, namespace, expected_uid=expected_uid
        )

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
