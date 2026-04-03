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

import asyncio
import logging
import re
import time
import uuid
from typing import Generic, TypeVar

from .async_k8s_helper import AsyncK8sHelper
from .async_sandbox import AsyncSandbox
from .exceptions import SandboxNotFoundError
from .models import SandboxConnectionConfig, SandboxTracerConfig
from .trace_manager import async_trace_span, create_tracer_manager, initialize_tracer, trace

logger = logging.getLogger(__name__)

T = TypeVar("T", bound=AsyncSandbox)


class AsyncSandboxClient(Generic[T]):
    """
    Async registry-based client for managing Sandbox lifecycles.

    Use as an async context manager for automatic cleanup::

        async with AsyncSandboxClient(connection_config=config) as client:
            sandbox = await client.create_sandbox("template")
            result = await sandbox.commands.run("echo hello")

    ``connection_config`` is required — the async client does not support
    ``SandboxLocalTunnelConnectionConfig``.
    """

    sandbox_class: type[T] = AsyncSandbox  # type: ignore

    def __init__(
        self,
        connection_config: SandboxConnectionConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
    ):
        if connection_config is None:
            raise ValueError(
                "connection_config is required for AsyncSandboxClient. "
                "Use SandboxDirectConnectionConfig or SandboxGatewayConnectionConfig. "
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
        template: str,
        namespace: str = "default",
        sandbox_ready_timeout: int = 180,
        labels: dict[str, str] | None = None,
    ) -> T:
        """Provisions a new Sandbox claim and returns an async Sandbox handle.

        Example::

            async with AsyncSandboxClient(connection_config=config) as client:
                sandbox = await client.create_sandbox("python-sandbox-template")
                result = await sandbox.commands.run("echo 'Hello'")
        """
        if not template:
            raise ValueError("Template name cannot be empty.")

        if labels:
            self._validate_labels(labels)

        claim_name = f"sandbox-claim-{uuid.uuid4().hex[:8]}"

        try:
            await self._create_claim(claim_name, template, namespace, labels=labels)
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
        self, claim_name: str, namespace: str = "default", resolve_timeout: int = 30
    ) -> T:
        """Retrieves an existing sandbox handle given a sandbox claim name.

        Example::

            sandbox = await client.get_sandbox("sandbox-claim-1234abcd")
            result = await sandbox.commands.run("ls -la")
        """
        key = (namespace, claim_name)

        async with self._lock:
            existing = self._active_connection_sandboxes.get(key)

        try:
            sandbox_id = await self.k8s_helper.resolve_sandbox_name(
                claim_name, namespace, timeout=resolve_timeout
            )
            sandbox_object = await self.k8s_helper.get_sandbox(sandbox_id, namespace)
            if not sandbox_object:
                raise SandboxNotFoundError(f"Underlying Sandbox '{sandbox_id}' not found.")
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

    async def list_all_sandboxes(self, namespace: str = "default") -> list[str]:
        """Lists all SandboxClaim names in the Kubernetes cluster for a namespace."""
        return await self.k8s_helper.list_sandbox_claims(namespace)

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

    # --- Label validation (shared with sync client) ---

    _LABEL_NAME_RE = re.compile(r"^[A-Za-z0-9][-A-Za-z0-9_.]*[A-Za-z0-9]$|^[A-Za-z0-9]$")
    _LABEL_PREFIX_RE = re.compile(r"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")
    _LABEL_NAME_MAX_LENGTH = 63
    _LABEL_PREFIX_MAX_LENGTH = 253

    @staticmethod
    def _validate_label_name(name: str, context: str):
        if len(name) > AsyncSandboxClient._LABEL_NAME_MAX_LENGTH:
            raise ValueError(
                f"Label {context} '{name}' exceeds max length of "
                f"{AsyncSandboxClient._LABEL_NAME_MAX_LENGTH} characters."
            )
        if not AsyncSandboxClient._LABEL_NAME_RE.match(name):
            raise ValueError(
                f"Label {context} '{name}' contains invalid characters. "
                f"Must start and end with alphanumeric, and contain only [-A-Za-z0-9_.]."
            )

    @staticmethod
    def _validate_labels(labels: dict[str, str]):
        for key, value in labels.items():
            if not key:
                raise ValueError("Label key cannot be empty.")

            if "/" in key:
                prefix, name = key.split("/", 1)
                if not prefix or len(prefix) > AsyncSandboxClient._LABEL_PREFIX_MAX_LENGTH:
                    raise ValueError(
                        f"Label key prefix '{prefix}' is invalid or exceeds "
                        f"{AsyncSandboxClient._LABEL_PREFIX_MAX_LENGTH} characters."
                    )
                if not AsyncSandboxClient._LABEL_PREFIX_RE.match(prefix):
                    raise ValueError(
                        f"Label key prefix '{prefix}' must be a valid DNS subdomain."
                    )
                if not name:
                    raise ValueError(f"Label key '{key}' has an empty name after prefix.")
                AsyncSandboxClient._validate_label_name(name, f"key name in '{key}'")
            else:
                AsyncSandboxClient._validate_label_name(key, f"key '{key}'")

            if value:
                AsyncSandboxClient._validate_label_name(
                    value, f"value '{value}' for key '{key}'"
                )

    @async_trace_span("create_claim")
    async def _create_claim(
        self,
        claim_name: str,
        template_name: str,
        namespace: str,
        labels: dict[str, str] | None = None,
    ):
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.claim.name", claim_name)

        annotations = {}
        if self.tracing_manager:
            trace_context_str = self.tracing_manager.get_trace_context_json()
            if trace_context_str:
                annotations["opentelemetry.io/trace-context"] = trace_context_str

        await self.k8s_helper.create_sandbox_claim(
            claim_name, template_name, namespace, annotations=annotations, labels=labels
        )

    @async_trace_span("wait_for_sandbox_ready")
    async def _wait_for_sandbox_ready(self, sandbox_id: str, namespace: str, timeout: int):
        await self.k8s_helper.wait_for_sandbox_ready(sandbox_id, namespace, timeout)

    @async_trace_span("delete_claim")
    async def _delete_claim(self, claim_name: str, namespace: str):
        try:
            await self.k8s_helper.delete_sandbox_claim(claim_name, namespace)
        except Exception as e:
            logger.error(f"Failed to cleanup SandboxClaim '{claim_name}': {e}")
