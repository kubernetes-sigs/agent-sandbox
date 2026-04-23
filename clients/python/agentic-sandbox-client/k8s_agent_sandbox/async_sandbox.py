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

import logging

from .async_connector import AsyncSandboxConnector
from .async_k8s_helper import AsyncK8sHelper
from .commands.async_command_executor import AsyncCommandExecutor
from .constants import POD_NAME_ANNOTATION
from .files.async_filesystem import AsyncFilesystem
from .models import SandboxConnectionConfig, SandboxInClusterConnectionConfig, SandboxTracerConfig
from .trace_manager import create_tracer_manager


class AsyncSandbox:
    """
    Represents an async connection to a specific running Sandbox instance.

    This class provides the async interface for interacting with the Sandbox:
    - Executing commands via the ``commands`` property.
    - Managing files via the ``files`` property.
    - Handling the underlying connection lifecycle.
    - Integrating with OpenTelemetry for tracing operations.

    Unlike the sync ``Sandbox``, ``connection_config`` is required because the
    async client does not support ``SandboxLocalTunnelConnectionConfig``.
    """

    def __init__(
        self,
        claim_name: str,
        sandbox_id: str,
        namespace: str = "default",
        connection_config: SandboxConnectionConfig | None = None,
        tracer_config: SandboxTracerConfig | None = None,
        k8s_helper: AsyncK8sHelper | None = None,
    ):
        if connection_config is None:
            raise ValueError(
                "connection_config is required for AsyncSandbox. "
                "Use SandboxDirectConnectionConfig, SandboxGatewayConnectionConfig, "
                "or SandboxInClusterConnectionConfig."
            )

        self.claim_name = claim_name
        self.sandbox_id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config

        self.k8s_helper = k8s_helper or AsyncK8sHelper()

        use_pod_ip = (
            isinstance(self.connection_config, SandboxInClusterConnectionConfig)
            and self.connection_config.use_pod_ip
        )
        self.connector = AsyncSandboxConnector(
            sandbox_id=self.sandbox_id,
            namespace=self.namespace,
            connection_config=self.connection_config,
            k8s_helper=self.k8s_helper,
            get_pod_ip=self.get_pod_ip if use_pod_ip else None,
        )

        self.tracer_config = tracer_config or SandboxTracerConfig()
        self.trace_service_name = self.tracer_config.trace_service_name
        self.tracing_manager, self.tracer = create_tracer_manager(self.tracer_config)

        self._commands = AsyncCommandExecutor(
            self.connector, self.tracer, self.trace_service_name
        )
        self._files = AsyncFilesystem(
            self.connector, self.tracer, self.trace_service_name
        )

        self._is_closed = False
        self._pod_name = None

    async def get_pod_name(self) -> str:
        """Fetches the Sandbox object from Kubernetes and retrieves its current pod name."""
        if self._pod_name is not None:
            return self._pod_name

        sandbox_object = await self.k8s_helper.get_sandbox(self.sandbox_id, self.namespace) or {}
        metadata = sandbox_object.get("metadata") or {}
        annotations = metadata.get("annotations") or {}
        pod_name = annotations.get(POD_NAME_ANNOTATION)
        self._pod_name = pod_name if pod_name is not None else self.sandbox_id
        return self._pod_name

    async def get_pod_ip(self) -> str | None:
        """Fetches the first pod IP from the Sandbox status.

        Always queries the K8s API for the latest IP — the pod IP can change
        after a pod restart (e.g. when spec.replicas is scaled to 0 and back).
        Returns None if the controller does not populate podIPs.
        """
        sandbox_object = await self.k8s_helper.get_sandbox(self.sandbox_id, self.namespace) or {}
        pod_ips = sandbox_object.get("status", {}).get("podIPs", [])
        return pod_ips[0] if pod_ips else None

    @property
    def commands(self) -> AsyncCommandExecutor | None:
        return self._commands

    @property
    def files(self) -> AsyncFilesystem | None:
        return self._files

    @property
    def is_active(self) -> bool:
        return not self._is_closed and self._commands is not None and self._files is not None

    async def _close_connection(self):
        """Closes the client-side connection and disables execution engines."""
        if self._is_closed:
            return

        await self.connector.close()

        self._commands = None
        self._files = None

        if self.tracing_manager:
            try:
                self.tracing_manager.end_lifecycle_span()
            except Exception as e:
                logging.error(f"Failed to end tracing span: {e}")

        self._is_closed = True
        logging.info(f"Connection to sandbox claim '{self.claim_name}' has been closed.")

    async def terminate(self):
        """Permanent deletion of all server side infrastructure and client side connection."""
        await self._close_connection()
        await self.k8s_helper.delete_sandbox_claim(self.claim_name, self.namespace)
