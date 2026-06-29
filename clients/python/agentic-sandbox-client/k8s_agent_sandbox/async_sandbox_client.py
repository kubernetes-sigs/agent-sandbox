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
import time
import uuid
from typing import Generic, TypeVar

from .async_k8s_helper import AsyncK8sHelper
from .async_sandbox import AsyncSandbox
from .exceptions import SandboxNotFoundError
from .pod_metadata import build_pod_metadata, validate_labels
from .utils import construct_sandbox_claim_lifecycle_spec
from .models import SandboxConnectionConfig, SandboxInClusterConnectionConfig, SandboxTracerConfig
from .trace_manager import async_trace_span, create_tracer_manager, initialize_tracer, trace

logger = logging.getLogger(__name__)

T = TypeVar("T", bound=AsyncSandbox)


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
    all tracked sandboxes on program termination, so sandboxes are not leaked
    if the program exits without explicit cleanup. Pass ``cleanup=False`` to
    opt out of this behavior::

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
                all tracked sandboxes when the program terminates. The hook
                snapshots the tracked claim names and opens fresh async
                resources in a new event loop, so it is safe to call after
                the main event loop has exited. Cleanup is best-effort —
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
        self._lock = asyncio.Lock()

        if cleanup:
            atexit.register(self._atexit_cleanup)

    async def __aenter__(self) -> "AsyncSandboxClient[T]":
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        try:
            await self.delete_all()
        finally:
            await self.close()

    async def close(self):
        """Shuts down all tracked sandbox connections and the K8s API client."""
        async with self._lock:
            for sandbox in self._active_connection_sandboxes.values():
                try:
                    await sandbox._close_connection()
                except Exception as e:
                    logger.error(f"Failed to close sandbox connection: {e}")
            self._active_connection_sandboxes.clear()
        await self.k8s_helper.close()

    async def create_sandbox(
        self,
        warmpool: str,
        namespace: str = "default",
        sandbox_ready_timeout: int = 180,
        labels: dict[str, str] | None = None,
        *,
        shutdown_after_seconds: int | None = None,
        pod_labels: dict[str, str] | None = None,
        pod_annotations: dict[str, str] | None = None,
    ) -> T:
        """Provisions a new Sandbox claim and returns an async Sandbox handle.

        Args:
            warmpool: Name of the SandboxWarmPool to use.
            namespace: Kubernetes namespace for the claim.
            sandbox_ready_timeout: Seconds to wait for the sandbox to be ready.
            labels: Optional Kubernetes labels to attach to the claim object
                (``SandboxClaim.metadata.labels``).
            shutdown_after_seconds: Optional TTL in seconds. When set, the
                claim's ``spec.lifecycle`` is populated with a ``shutdownTime``
                of *now + shutdown_after_seconds* (UTC) and a ``shutdownPolicy``
                of ``"Delete"``, so the controller auto-deletes the claim on
                expiry. Must be a positive integer.
            pod_labels: Optional labels stamped onto the running Sandbox **Pod**
                via ``spec.additionalPodMetadata.labels``. Unlike ``labels``
                (which land on the claim object), these are readable from inside
                the sandbox through the Downward API.
            pod_annotations: Optional annotations stamped onto the running
                Sandbox **Pod** via ``spec.additionalPodMetadata.annotations``.

        Example::

            async with AsyncSandboxClient(connection_config=config) as client:
                sandbox = await client.create_sandbox("python-sandbox-pool")
                result = await sandbox.commands.run("echo 'Hello'")
        """
        if not warmpool:
            raise ValueError("Warmpool name cannot be empty.")

        if labels:
            validate_labels(labels)

        pod_metadata = build_pod_metadata(pod_labels, pod_annotations)

        lifecycle = construct_sandbox_claim_lifecycle_spec(shutdown_after_seconds) if shutdown_after_seconds is not None else None

        claim_name = f"sandbox-claim-{uuid.uuid4().hex[:8]}"

        try:
            await self._create_claim(claim_name, warmpool, namespace, labels=labels, lifecycle=lifecycle, pod_metadata=pod_metadata)
            start_time = time.monotonic()
            sandbox_id = await self.k8s_helper.resolve_sandbox_name(
                claim_name, namespace, sandbox_ready_timeout
            )
            elapsed_time = time.monotonic() - start_time
            remaining_timeout = max(0, int(sandbox_ready_timeout - elapsed_time))
            if remaining_timeout <= 0:
                raise TimeoutError("Sandbox resolution exceeded the ready timeout.")
            await self._wait_for_sandbox_ready(sandbox_id, namespace, remaining_timeout)

            sandbox = self.sandbox_class(
                claim_name=claim_name,
                sandbox_id=sandbox_id,
                namespace=namespace,
                connection_config=self.connection_config,
                tracer_config=self.tracer_config,
                k8s_helper=self.k8s_helper,
            )
        except (Exception, asyncio.CancelledError):
            await asyncio.shield(self._delete_claim(claim_name, namespace))
            raise

        async with self._lock:
            self._active_connection_sandboxes[(namespace, claim_name)] = sandbox
        return sandbox

    async def get_sandbox(
        self,
        claim_name: str,
        namespace: str = "default",
        resolve_timeout: int = 30,
        warmpool_name: str | None = None,
    ) -> T:
        """Retrieves an existing sandbox handle given a sandbox claim name.

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

        try:
            if warmpool_name is not None:
                claim_object = await self.k8s_helper.get_sandbox_claim(
                    claim_name, namespace
                )
                if not claim_object:
                    raise SandboxNotFoundError(
                        f"SandboxClaim '{claim_name}' not found in namespace '{namespace}'."
                    )
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
            sandbox_id = await self.k8s_helper.resolve_sandbox_name(
                claim_name, namespace, timeout=resolve_timeout
            )
            sandbox_object = await self.k8s_helper.get_sandbox(sandbox_id, namespace)
            if not sandbox_object:
                raise SandboxNotFoundError(f"Underlying Sandbox '{sandbox_id}' not found.")
        except ValueError:
            # Warmpool mismatch is a signed-off refusal — propagate
            # untouched so the caller sees the security-relevant reason
            # rather than a generic "not found" wrap.
            raise
        except Exception as e:
            if existing:
                await existing.terminate()
            async with self._lock:
                self._active_connection_sandboxes.pop(key, None)
            raise SandboxNotFoundError(
                f"Sandbox claim '{claim_name}' not found or resolution failed "
                f"in namespace '{namespace}': {e}"
            ) from e

        if existing and existing.is_active:
            return existing

        if existing:
            async with self._lock:
                self._active_connection_sandboxes.pop(key, None)

        new_handle = self.sandbox_class(
            claim_name=claim_name,
            sandbox_id=sandbox_id,
            namespace=namespace,
            connection_config=self.connection_config,
            tracer_config=self.tracer_config,
            k8s_helper=self.k8s_helper,
        )

        async with self._lock:
            self._active_connection_sandboxes[key] = new_handle
        return new_handle

    async def list_active_sandboxes(self) -> list[tuple[str, str]]:
        """Returns a list of ``(namespace, claim_name)`` tuples currently managed."""
        async with self._lock:
            for key, obj in list(self._active_connection_sandboxes.items()):
                if not obj.is_active:
                    self._active_connection_sandboxes.pop(key, None)
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
            sandbox = self._active_connection_sandboxes.get(key)
        try:
            if sandbox:
                await sandbox.terminate()
                async with self._lock:
                    self._active_connection_sandboxes.pop(key, None)
            else:
                await self._delete_claim(claim_name, namespace)
        except Exception as e:
            logger.error(
                f"Failed to delete sandbox '{claim_name}' in namespace '{namespace}': {e}"
            )

    async def delete_all(self):
        """Cleanup all tracked sandboxes managed by this client."""
        async with self._lock:
            items = list(self._active_connection_sandboxes.items())

        for (ns, claim_name), _ in items:
            try:
                await self.delete_sandbox(claim_name, namespace=ns)
            except Exception as e:
                logger.error(f"Cleanup failed for {claim_name} in namespace {ns}: {e}")

    def _atexit_cleanup(self):
        """Best-effort atexit handler that deletes all tracked sandbox claims.

        Uses a snapshot of the tracked claims and a fresh :class:`AsyncK8sHelper`
        so that no loop-bound objects from the original client are reused across
        event loop boundaries. Per-claim failures and top-level errors emit
        warnings to ``sys.stderr`` rather than raising — atexit cleanup is
        best-effort.
        """
        claims = list(self._active_connection_sandboxes.keys())
        if not claims:
            return

        async def _do_cleanup():
            helper = AsyncK8sHelper()
            try:
                async def _delete_one(ns, claim_name):
                    try:
                        await helper.delete_sandbox_claim(claim_name, ns)
                    except Exception as e:
                        if sys.stderr is not None:
                            print(
                                f"[agent-sandbox] Warning: failed to delete sandbox claim "
                                f"'{claim_name}' in namespace '{ns}' during atexit cleanup: {e}",
                                file=sys.stderr,
                            )

                await asyncio.gather(*(_delete_one(ns, claim_name) for ns, claim_name in claims))
            finally:
                await helper.close()

        try:
            asyncio.run(_do_cleanup())
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
        pod_metadata: dict | None = None,
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

        await self.k8s_helper.create_sandbox_claim(
            claim_name, warmpool_name, namespace, annotations=annotations, labels=labels, lifecycle=lifecycle, pod_metadata=pod_metadata
        )

    @async_trace_span("wait_for_sandbox_ready")
    async def _wait_for_sandbox_ready(self, sandbox_id: str, namespace: str, timeout: int):
        await self.k8s_helper.wait_for_sandbox_ready(sandbox_id, namespace, timeout)

    @async_trace_span("delete_claim")
    async def _delete_claim(self, claim_name: str, namespace: str):
        await self.k8s_helper.delete_sandbox_claim(claim_name, namespace)
