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

import asyncio
import logging

import httpx

logger = logging.getLogger(__name__)

from .async_k8s_helper import AsyncK8sHelper
from .exceptions import SandboxRequestError
from .models import (
    SandboxConnectionConfig,
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)

RETRYABLE_STATUS_CODES = {500, 502, 503, 504}
MAX_RETRIES = 5
BACKOFF_FACTOR = 0.5


class AsyncSandboxConnector:
    """
    Async connector for communicating with a Sandbox over HTTP using httpx.

    Supports DirectConnection, GatewayConnection, and InCluster modes. LocalTunnel
    mode is not supported because it relies on a long-running subprocess; use the
    sync SandboxConnector for local development.
    """

    def __init__(
        self,
        sandbox_id: str,
        namespace: str,
        connection_config: SandboxConnectionConfig,
        k8s_helper: AsyncK8sHelper,
        pod_ip: str | None = None,
    ):
        if isinstance(connection_config, SandboxLocalTunnelConnectionConfig):
            raise ValueError(
                "AsyncSandboxConnector does not support SandboxLocalTunnelConnectionConfig. "
                "Use SandboxDirectConnectionConfig or SandboxGatewayConnectionConfig instead. "
                "For local development, use the synchronous SandboxClient."
            )

        self.id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config
        self.k8s_helper = k8s_helper

        self._base_url: str | None = None
        if isinstance(connection_config, SandboxInClusterConnectionConfig):
            if pod_ip:
                self._base_url = f"http://{pod_ip}:{connection_config.server_port}"
            else:
                self._base_url = (
                    f"http://{sandbox_id}.{namespace}"
                    f".svc.cluster.local:{connection_config.server_port}"
                )

        self._inject_router_headers = not isinstance(
            connection_config, SandboxInClusterConnectionConfig
        )

        transport = httpx.AsyncHTTPTransport(retries=3)
        self.client = httpx.AsyncClient(
            transport=transport, timeout=httpx.Timeout(60.0)
        )

    async def _resolve_base_url(self) -> str:
        if self._base_url:
            return self._base_url

        if isinstance(self.connection_config, SandboxDirectConnectionConfig):
            self._base_url = self.connection_config.api_url
        elif isinstance(self.connection_config, SandboxGatewayConnectionConfig):
            ip_address = await self.k8s_helper.wait_for_gateway_ip(
                self.connection_config.gateway_name,
                self.connection_config.gateway_namespace,
                self.connection_config.gateway_ready_timeout,
            )
            self._base_url = f"http://{ip_address}"
        else:
            raise ValueError(
                f"AsyncSandboxConnector does not support {type(self.connection_config).__name__}."
            )

        return self._base_url

    async def send_request(self, method: str, endpoint: str, **kwargs) -> httpx.Response:
        base_url = await self._resolve_base_url()
        url = f"{base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        headers = kwargs.pop("headers", {}).copy()
        if self._inject_router_headers:
            headers["X-Sandbox-ID"] = self.id
            headers["X-Sandbox-Namespace"] = self.namespace
            headers["X-Sandbox-Port"] = str(self.connection_config.server_port)

        last_response: httpx.Response | None = None
        for attempt in range(MAX_RETRIES + 1):
            try:
                response = await self.client.request(
                    method, url, headers=headers, **kwargs
                )
                if response.status_code in RETRYABLE_STATUS_CODES and attempt < MAX_RETRIES:
                    delay = BACKOFF_FACTOR * (2 ** attempt)
                    logger.warning(
                        f"Retryable status {response.status_code} from {url}, "
                        f"attempt {attempt + 1}/{MAX_RETRIES + 1}, retrying in {delay:.1f}s"
                    )
                    last_response = response
                    await asyncio.sleep(delay)
                    continue
                response.raise_for_status()
                return response
            except httpx.HTTPStatusError as e:
                logger.error(f"Request to sandbox failed: {e}")
                raise SandboxRequestError(
                    f"Failed to communicate with the sandbox at {url}.",
                    status_code=e.response.status_code,
                    response=e.response,
                ) from e
            except httpx.HTTPError as e:
                logger.error(f"Request to sandbox failed: {e}")
                # Only clear the cached URL for Gateway config — the IP may have changed.
                # Direct and InCluster URLs are stable and must not be cleared.
                if isinstance(self.connection_config, SandboxGatewayConnectionConfig):
                    self._base_url = None
                raise SandboxRequestError(
                    f"Failed to communicate with the sandbox at {url}.",
                    status_code=None,
                    response=None,
                ) from e

        logger.error(f"All {MAX_RETRIES + 1} attempts failed for {url}")
        raise SandboxRequestError(
            f"Failed to communicate with the sandbox at {url} after {MAX_RETRIES + 1} attempts.",
            status_code=last_response.status_code if last_response else None,
            response=last_response,
        )

    async def close(self):
        await self.client.aclose()
        # Only clear volatile URLs (Gateway). Direct and InCluster URLs are stable.
        if isinstance(self.connection_config, SandboxGatewayConnectionConfig):
            self._base_url = None
