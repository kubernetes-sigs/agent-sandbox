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
Async version of :class:`SandboxClient` for use in async applications.

Requires the ``async`` optional dependencies::

    pip install k8s-agent-sandbox[async]
"""

import atexit
import asyncio
import logging
import sys
import uuid
from functools import partial
from typing import Generic, TypeVar

from kubernetes_asyncio.client import ApiException

from .async_k8s_helper import AsyncK8sHelper
from .async_sandbox import AsyncSandbox
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
from .exceptions import SandboxNotFoundError
from .k8s_helper import K8sHelper
from .pod_metadata import build_pod_metadata, validate_labels
from .utils import construct_sandbox_claim_lifecycle_spec
from .models import SandboxConnectionConfig, SandboxInClusterConnectionConfig, SandboxTracerConfig
from .trace_manager import async_trace_span, create_tracer_manager, initialize_tracer, trace

logger = logging.getLogger(__name__)

T = TypeVar("T", bound=AsyncSandbox)

# Bounds each per-claim delete issued by the atexit cleanup below. urllib3
# (used by the synchronous K8sHelper) has no default read timeout, so an
# unresponsive apiserver would otherwise hang process exit indefinitely.
_ATEXIT_DELETE_REQUEST_TIMEOUT_SECONDS = 300


class AsyncSandboxClient(Generic[T]):
    """
    Async registry-based client for managing Sandbox lifecycles.

    Use as an async context manager for automatic cleanup::

        async with AsyncSandboxClient(connection_config=config) as client:
            sandbox = await client.create_sandbox("python-sandbox-pool")
            result = await sandbox.commands.run("echo hello")

    ``connection_config`` is required — the async client does not support
    ``SandboxLocalTunnelConnectionConfig``.

    By default (``cleanup=True``) an atexit hook is registered that deletes
    automatically managed sandboxes on program termination. This includes
    internally named and reattached claims; explicitly named claims created by
    this client remain caller-owned. Pass ``cleanup=False`` to opt out::

        client = AsyncSandboxClient(connection_config=config, cleanup=False)

    Note that this default differs from the synchronous ``SandboxClient``,
    which defaults to ``cleanup=False``; the async client opts in to safer
    out-of-the-box cleanup.

    Alternatively, use the ``async with`` context manager or explicitly call
    ``await client.delete_all()`` followed by ``await client.close()`` to
    avoid orphaned claims.
    """

    sandbox_class: type[T] = AsyncSandbox  # type: ignore

    def __init__(
        self,
        connection_config: SandboxConnectionConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
        cleanup: bool = True,
    ):
        """
        Args:
            connection_config: Configuration for connecting to the sandboxes.
                Required — the async client does not support
                ``SandboxLocalTunnelConnectionConfig``.
            tracer_config: Configuration for OpenTelemetry tracing.
                Defaults to an empty SandboxTracerConfig (tracing disabled).
            cleanup: If True, registers an atexit hook to automatically delete
                managed sandboxes when the program terminates. This includes
                internally named and reattached claims; explicitly named claims
                created by this client remain caller-owned. The hook uses a
                snapshot of the tracked claim names and synchronous ``K8sHelper``,
                which has no event loop dependency, so it works correctly
                during interpreter shutdown. Cleanup is best-effort —
                per-claim and top-level failures emit warnings to
                ``sys.stderr`` rather than raising. Defaults to True so that
                sandboxes are not leaked when a caller forgets to clean up;
                pass ``cleanup=False`` to opt out. Note this differs from the
                synchronous ``SandboxClient``, which defaults to False.
        """
        if connection_config is None:
            raise ValueError(
                "connection_config is required for AsyncSandboxClient. "
                "Use SandboxDirectConnectionConfig, SandboxGatewayConnectionConfig, or "
                "SandboxInClusterConnectionConfig. "
                "For local development with kubectl port-forward, use the synchronous SandboxClient."
            )

        self.connection_config = connection_config

        self.tracer_config = tracer_config or SandboxTracerConfig()
        if self.tracer_config.enable_tracing:
            initialize_tracer(self.tracer_config.trace_service_name)
        self.tracing_manager, self.tracer = create_tracer_manager(self.tracer_config)

        self.k8s_helper = AsyncK8sHelper()

        self._active_connection_sandboxes: dict[tuple[str, str], T] = {}
        self._active_claim_uids: dict[tuple[str, str], str | None] = {}
        self._claim_ownership = ClaimOwnership()
        self._automatic_cleanup_claims = (
            self._claim_ownership.automatic_cleanup_claims
        )
        self._automatic_cleanup_claim_uids = (
            self._claim_ownership.automatic_cleanup_claim_uids
        )
        self._caller_owned_claims = self._claim_ownership.caller_owned_claims
        self._lock = asyncio.Lock()

        if cleanup:
            atexit.register(self._atexit_cleanup)

    async def __aenter__(self) -> "AsyncSandboxClient[T]":
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        try:
            await self._delete_automatic_cleanup_claims()
        finally:
            await self.close()

    async def close(self):
        """Shuts down all tracked sandbox connections and the K8s API client."""
        async with self._lock:
            sandboxes = list(self._active_connection_sandboxes.items())
            for key, sandbox in sandboxes:
                if self._claim_ownership.should_retire_handle(key):
                    sandbox.claim_name = None
            self._active_connection_sandboxes.clear()
            self._active_claim_uids.clear()
        for _, sandbox in sandboxes:
            try:
                await sandbox.close_connection()
            except Exception as e:
                logger.error(f"Failed to close sandbox connection: {e}")
        await self.k8s_helper.close()

    async def create_sandbox(
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
        """Provisions a new Sandbox claim and returns an async Sandbox handle.

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

        Example::

            async with AsyncSandboxClient(connection_config=config) as client:
                sandbox = await client.create_sandbox("python-sandbox-pool")
                result = await sandbox.commands.run("echo 'Hello'")
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
        async with self._lock:
            expected_handle = self._active_connection_sandboxes.get(key)
            if not generated_claim_name:
                self._claim_ownership.mark_caller_owned(key)
        sandbox: T | None = None
        try:
            try:
                created_claim = await self._create_claim(
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
                existing_claim = await self.k8s_helper.get_sandbox_claim(
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
                sandbox_id = await self._wait_for_claim_ready(
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
            return await self._register_created_handle(
                key,
                sandbox,
                generated_claim_name,
                expected_handle,
                claim_uid,
            )
        except (Exception, asyncio.CancelledError):
            await asyncio.shield(
                self._rollback_failed_creation(
                    sandbox,
                    key,
                    claim_uid,
                    cleanup_generated_claim,
                )
            )
            raise

    async def _register_created_handle(
        self,
        key: tuple[str, str],
        sandbox: T,
        generated_claim_name: bool,
        expected_handle: T | None,
        claim_uid: str | None,
    ) -> T:
        """Register one handle and close superseded handles outside the lock."""
        stale_handles: list[T] = []
        result: T | None = None
        concurrent_change = False
        async with self._lock:
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
            await self._close_handle_best_effort(stale_handle, retire=True)
        if concurrent_change:
            raise self._concurrent_claim_change(key)
        assert result is not None
        return result

    def _handle_matches_claim(
        self, key: tuple[str, str], current: T | None, candidate: T, uid: str | None
    ) -> bool:
        """Return whether a handle belongs to the same Claim incarnation."""
        return (
            self._active_handle_has_claim_uid(key, current, uid)
            and current is not None
            and current.sandbox_id == candidate.sandbox_id
        )

    def _active_handle_has_claim_uid(
        self, key: tuple[str, str], handle: T | None, uid: str | None
    ) -> bool:
        """Return whether an active handle has the observed Claim UID."""
        return (
            handle is not None
            and handle.is_active
            and self._active_claim_uids.get(key) == uid
        )

    async def _delete_failed_generated_claim_if_owned(
        self, key: tuple[str, str], expected_uid: str | None
    ) -> None:
        """Roll back a generated Claim unless explicit ownership superseded it."""
        async with self._lock:
            should_delete = self._claim_ownership.failed_generated_needs_delete(
                key,
                has_registered_handle=key in self._active_connection_sandboxes,
                claim_uid=expected_uid,
            )
            if not should_delete:
                return
        namespace, claim_name = key
        await self._delete_claim_with_optional_uid(
            claim_name, namespace, expected_uid
        )
        assert expected_uid
        async with self._lock:
            self._claim_ownership.discard_automatic_if_uid(
                key, expected_uid
            )

    async def _rollback_failed_creation(
        self,
        sandbox: T | None,
        key: tuple[str, str],
        expected_uid: str | None,
        cleanup_generated_claim: bool,
    ) -> None:
        """Best-effort rollback that cannot replace the original failure."""
        if sandbox is not None:
            async with self._lock:
                if self._active_connection_sandboxes.get(key) is sandbox:
                    self._active_connection_sandboxes.pop(key, None)
                    self._active_claim_uids.pop(key, None)
            try:
                await self._retire_stale_handle(sandbox)
            except (Exception, asyncio.CancelledError) as error:
                logger.error(f"Failed to close candidate sandbox: {error}")
        if not cleanup_generated_claim:
            return
        try:
            await self._delete_failed_generated_claim_if_owned(key, expected_uid)
        except (Exception, asyncio.CancelledError) as error:
            logger.error(f"Failed to delete generated SandboxClaim: {error}")

    @staticmethod
    def _concurrent_claim_change(key: tuple[str, str]) -> RuntimeError:
        namespace, claim_name = key
        return RuntimeError(
            f"SandboxClaim '{claim_name}' in namespace '{namespace}' "
            "changed concurrently; retry the operation."
        )

    async def get_sandbox(
        self,
        claim_name: str,
        namespace: str = "default",
        resolve_timeout: int = 30,
        warmpool_name: str | None = None,
    ) -> T:
        """Retrieves an existing sandbox handle given a sandbox claim name.

        Reattached handles preserve the client's historical automatic cleanup
        behavior. Cleanup is constrained to the exact observed Claim UID.

        Args:
            claim_name: Name of the SandboxClaim to attach to.
            namespace: Kubernetes namespace the claim lives in.
            resolve_timeout: Seconds to wait while resolving the sandbox
                name from the claim status.
            warmpool_name: Optional SandboxWarmPool name to validate against
                the existing claim's ``spec.warmPoolRef.name``.
                When supplied and the claim references a different
                warmpool, ``ValueError`` is raised before returning a
                handle. Mirrors the sync ``SandboxClient.get_sandbox``
                guard so async session-reattach callers get the same
                refuse-on-mismatch semantics.

        Example::

            sandbox = await client.get_sandbox("sandbox-claim-1234abcd")
            result = await sandbox.commands.run("ls -la")
        """
        key = (namespace, claim_name)

        async with self._lock:
            existing = self._active_connection_sandboxes.get(key)
            lookup_operation = self._claim_ownership.begin_lookup(key)

        try:
            try:
                claim_object = await self.k8s_helper.get_sandbox_claim(
                    claim_name, namespace
                )
                claim_identity = validate_claim_identity(
                    claim_object,
                    claim_name=claim_name,
                    namespace=namespace,
                )
            except Exception as error:
                await self._detach_failed_lookup(key, existing)
                message = (
                    f"Sandbox claim '{claim_name}' not found or resolution "
                    f"failed in namespace '{namespace}': {error}"
                )
                raise SandboxNotFoundError(message) from error

            if warmpool_name is not None:
                existing_warmpool = (
                    claim_object.get("spec", {})
                    .get("warmPoolRef", {})
                    .get("name")
                )
                if existing_warmpool != warmpool_name:
                    raise ValueError(
                        f"SandboxClaim '{claim_name}' in namespace '{namespace}' references "
                        f"warmpool '{existing_warmpool}', not '{warmpool_name}'. Refusing "
                        f"to reattach."
                    )

            claim_validator = partial(
                validate_claim_identity,
                claim_name=claim_name,
                namespace=namespace,
                expected_uid=claim_identity.uid,
            )
            try:
                sandbox_id = await self.k8s_helper.resolve_sandbox_name(
                    claim_name,
                    namespace,
                    timeout=resolve_timeout,
                    claim_validator=claim_validator,
                )
                sandbox_object = await self.k8s_helper.get_sandbox(
                    sandbox_id, namespace
                )
                if not sandbox_object:
                    raise SandboxNotFoundError(
                        f"Underlying Sandbox '{sandbox_id}' not found."
                    )
            except Exception as error:
                await self._detach_failed_lookup(key, existing)
                message = (
                    f"Sandbox claim '{claim_name}' not found or resolution "
                    f"failed in namespace '{namespace}': {error}"
                )
                raise SandboxNotFoundError(message) from error

            return await self._reuse_or_replace_resolved_handle(
                key,
                existing,
                lookup_operation,
                claim_name,
                sandbox_id,
                namespace,
                claim_identity.uid,
            )
        finally:
            async with self._lock:
                self._claim_ownership.finish_lookup(key, lookup_operation)

    async def _detach_failed_lookup(
        self, key: tuple[str, str], expected_handle: T | None
    ) -> None:
        """Detach only the handle observed by the failed lookup."""
        if expected_handle is None:
            return
        should_delete = False
        expected_uid = None
        async with self._lock:
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

        try:
            await self._close_handle_best_effort(
                expected_handle, retire=automatic_cleanup
            )
        except asyncio.CancelledError:
            if should_delete:
                async with self._lock:
                    self._claim_ownership.register_automatic(
                        key, expected_uid
                    )
            raise
        if should_delete:
            namespace, claim_name = key
            try:
                await self._delete_claim_with_optional_uid(
                    claim_name, namespace, expected_uid
                )
            except (Exception, asyncio.CancelledError) as error:
                async with self._lock:
                    self._claim_ownership.register_automatic(
                        key, expected_uid
                    )
                logger.error(f"Failed to delete stale SandboxClaim: {error}")

    async def _close_handle_best_effort(
        self, sandbox: T, *, retire: bool
    ) -> None:
        """Close a detached handle while preserving the preceding outcome."""
        try:
            if retire:
                await self._retire_stale_handle(sandbox)
            else:
                await sandbox.close_connection()
        except Exception as error:
            logger.error(f"Failed to close stale sandbox handle: {error}")

    @staticmethod
    async def _retire_stale_handle(sandbox: T) -> None:
        """Disable name-only deletion from a superseded handle."""
        if sandbox.claim_name is None:
            return
        sandbox.claim_name = None
        await sandbox.close_connection()

    async def _reuse_or_replace_resolved_handle(
        self,
        key: tuple[str, str],
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
        async with self._lock:
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
            await self._close_handle_best_effort(stale_handle, retire=True)
        if concurrent_change:
            raise self._concurrent_claim_change(key)
        assert result is not None
        return result

    def _resolved_handle_matches_claim(
        self, key: tuple[str, str], handle: T | None, sandbox_id: str, uid: str
    ) -> bool:
        """Return whether a resolved identity matches a registered handle."""
        return (
            handle is not None
            and handle.is_active
            and handle.sandbox_id == sandbox_id
            and self._active_claim_uids.get(key) == uid
        )

    async def list_active_sandboxes(self) -> list[tuple[str, str]]:
        """Returns a list of ``(namespace, claim_name)`` tuples currently managed."""
        async with self._lock:
            for key, obj in list(self._active_connection_sandboxes.items()):
                if not obj.is_active:
                    if self._claim_ownership.should_retire_handle(key):
                        obj.claim_name = None
                    self._active_connection_sandboxes.pop(key, None)
                    self._active_claim_uids.pop(key, None)
            return list(self._active_connection_sandboxes.keys())

    async def list_all_sandboxes(self, namespace: str = "default", label_selector: str | None = None) -> list[str]:
        """Lists all SandboxClaim names in the Kubernetes cluster for a namespace.

        Args:
            namespace: Kubernetes namespace to list claims in.
            label_selector: Optional Kubernetes label selector string
                (e.g. ``"app=myapp"``). When set, only claims matching
                the selector are returned.
        """
        return await self.k8s_helper.list_sandbox_claims(namespace, label_selector=label_selector)

    async def delete_sandbox(self, claim_name: str, namespace: str = "default"):
        """Stops the client side connection and deletes the Kubernetes resources."""
        key = (namespace, claim_name)
        async with self._lock:
            sandbox = self._active_connection_sandboxes.pop(key, None)
            active_uid = self._active_claim_uids.pop(key, None)
            automatic_owned = key in self._automatic_cleanup_claims
            automatic_uid = self._claim_ownership.automatic_cleanup_uid(key)
            caller_owned = key in self._caller_owned_claims
            self._claim_ownership.discard(key)
        try:
            if sandbox:
                await self._retire_stale_handle(sandbox)
                if active_uid:
                    await self._delete_claim_with_optional_uid(
                        claim_name, namespace, active_uid
                    )
                else:
                    await self._delete_claim(claim_name, namespace)
            else:
                await self._delete_claim(claim_name, namespace)
        except (Exception, asyncio.CancelledError) as e:
            async with self._lock:
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
            if isinstance(e, asyncio.CancelledError):
                raise
            logger.error(
                f"Failed to delete sandbox '{claim_name}' in namespace '{namespace}': {e}"
            )

    async def delete_all(self):
        """Deliberately delete every sandbox tracked by this client."""
        async with self._lock:
            claims = list(self._active_connection_sandboxes)

        for ns, claim_name in claims:
            try:
                await self.delete_sandbox(claim_name, namespace=ns)
            except Exception as e:
                logger.error(f"Cleanup failed for {claim_name} in namespace {ns}: {e}")

    async def _delete_automatic_cleanup_claims(self):
        """Best-effort cleanup of automatically managed claims."""
        async with self._lock:
            claims = list(self._automatic_cleanup_claims)

        for ns, claim_name in claims:
            try:
                await self._delete_automatic_cleanup_claim((ns, claim_name))
            except Exception as e:
                logger.error(f"Cleanup failed for {claim_name} in namespace {ns}: {e}")

    async def _delete_automatic_cleanup_claim(
        self, key: tuple[str, str]
    ) -> None:
        """Delete a claim only while this client still owns its cleanup."""
        async with self._lock:
            should_delete, expected_uid = (
                self._claim_ownership.take_automatic_cleanup(key)
            )
            if not should_delete:
                return
            namespace, claim_name = key
            sandbox = self._active_connection_sandboxes.pop(key, None)
            self._active_claim_uids.pop(key, None)

        try:
            if sandbox is not None:
                await self._close_handle_best_effort(sandbox, retire=True)
            await self._delete_claim_with_optional_uid(
                claim_name, namespace, expected_uid
            )
        except (Exception, asyncio.CancelledError):
            async with self._lock:
                self._claim_ownership.register_automatic(key, expected_uid)
            raise

    def _atexit_cleanup(self):
        """Best-effort atexit handler for automatically managed claims.

        Uses the synchronous :class:`K8sHelper` rather than kubernetes_asyncio,
        even though this class is otherwise fully async. atexit runs during
        interpreter shutdown, after Python has begun tearing down its
        process-wide thread pool; kubernetes_asyncio's aiohttp transport does a
        per-request netrc lookup via a background thread, which raises "cannot
        schedule new futures after interpreter shutdown" once that teardown has
        started. The synchronous client's urllib3 transport has no event loop or
        executor dependency, so it isn't affected. Per-claim failures emit
        warnings to ``sys.stderr`` rather than raising — atexit cleanup is
        best-effort.
        """
        try:
            claims_with_uids = [
                (key, expected_uid)
                for key in self._automatic_cleanup_claims
                if key not in self._caller_owned_claims
                if (expected_uid := self._automatic_cleanup_claim_uids.get(key))
            ]
            if not claims_with_uids:
                return

            helper = K8sHelper()
            for (ns, claim_name), expected_uid in claims_with_uids:
                try:
                    helper.delete_sandbox_claim(
                        claim_name,
                        ns,
                        _request_timeout=_ATEXIT_DELETE_REQUEST_TIMEOUT_SECONDS,
                        expected_uid=expected_uid,
                    )
                except Exception as e:
                    if sys.stderr is not None:
                        print(
                            f"[agent-sandbox] Warning: failed to delete sandbox claim "
                            f"'{claim_name}' in namespace '{ns}' during atexit cleanup: {e}",
                            file=sys.stderr,
                        )
        except Exception as e:
            if sys.stderr is not None:
                print(
                    f"[agent-sandbox] Warning: atexit cleanup failed: {e}",
                    file=sys.stderr,
                )

    @async_trace_span("create_claim")
    async def _create_claim(
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

        return await self.k8s_helper.create_sandbox_claim(
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

    @async_trace_span("wait_for_claim_ready")
    async def _wait_for_claim_ready(
        self,
        claim_name: str,
        namespace: str,
        timeout: int,
        resource_version: str | None = None,
        claim_validator=None,
    ) -> str:
        """Waits for the SandboxClaim to be bound and Ready, returning the sandbox name."""
        return await self.k8s_helper.wait_for_claim_ready(
            claim_name,
            namespace,
            timeout,
            resource_version=resource_version,
            claim_validator=claim_validator,
        )

    @async_trace_span("wait_for_sandbox_ready")
    async def _wait_for_sandbox_ready(self, sandbox_id: str, namespace: str, timeout: int):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        await self.k8s_helper.wait_for_sandbox_ready(sandbox_id, namespace, timeout)

    @async_trace_span("delete_claim")
    async def _delete_claim(self, claim_name: str, namespace: str):
        await self.k8s_helper.delete_sandbox_claim(claim_name, namespace)

    async def _delete_claim_with_optional_uid(
        self,
        claim_name: str,
        namespace: str,
        expected_uid: str | None,
    ) -> None:
        """Delete a Claim only when its exact identity is known."""
        if not expected_uid:
            return
        await self.k8s_helper.delete_sandbox_claim(
            claim_name, namespace, expected_uid=expected_uid
        )
