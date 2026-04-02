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

from .async_k8s_helper import AsyncK8sHelper
from .exceptions import SandboxRequestError
from .models import (
    SandboxConnectionConfig,
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)

RETRYABLE_STATUS_CODES = {500, 502, 503, 504}
MAX_RETRIES = 5
BACKOFF_FACTOR = 0.5


class AsyncSandboxConnector:
    """
    Async connector for communicating with a Sandbox over HTTP using httpx.

    Supports DirectConnection and GatewayConnection modes. LocalTunnel mode
    is not supported because it relies on a long-running subprocess; use the
    sync SandboxConnector for local development.
    """

    def __init__(
        self,
        sandbox_id: str,
        namespace: str,
        connection_config: SandboxConnectionConfig,
        k8s_helper: AsyncK8sHelper,
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
        self.client = httpx.AsyncClient()

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
        headers["X-Sandbox-ID"] = self.id
        headers["X-Sandbox-Namespace"] = self.namespace
        headers["X-Sandbox-Port"] = str(self.connection_config.server_port)

        last_exc: Exception | None = None
        for attempt in range(MAX_RETRIES + 1):
            try:
                response = await self.client.request(
                    method, url, headers=headers, **kwargs
                )
                if response.status_code in RETRYABLE_STATUS_CODES and attempt < MAX_RETRIES:
                    delay = BACKOFF_FACTOR * (2 ** attempt)
                    logging.warning(
                        f"Retryable status {response.status_code} from {url}, "
                        f"attempt {attempt + 1}/{MAX_RETRIES + 1}, retrying in {delay:.1f}s"
                    )
                    await asyncio.sleep(delay)
                    continue
                response.raise_for_status()
                return response
            except httpx.HTTPStatusError as e:
                status_code = e.response.status_code
                if status_code in RETRYABLE_STATUS_CODES and attempt < MAX_RETRIES:
                    delay = BACKOFF_FACTOR * (2 ** attempt)
                    logging.warning(
                        f"Retryable status {status_code} from {url}, "
                        f"attempt {attempt + 1}/{MAX_RETRIES + 1}, retrying in {delay:.1f}s"
                    )
                    await asyncio.sleep(delay)
                    last_exc = e
                    continue
                logging.error(f"Request to sandbox failed: {e}")
                raise SandboxRequestError(
                    f"Failed to communicate with the sandbox at {url}.",
                    status_code=status_code,
                    response=e.response,
                ) from e
            except httpx.HTTPError as e:
                logging.error(f"Request to sandbox failed: {e}")
                self._base_url = None
                raise SandboxRequestError(
                    f"Failed to communicate with the sandbox at {url}.",
                    status_code=None,
                    response=None,
                ) from e

        # All retries exhausted
        logging.error(f"All {MAX_RETRIES + 1} attempts failed for {url}")
        raise SandboxRequestError(
            f"Failed to communicate with the sandbox at {url} after {MAX_RETRIES + 1} attempts.",
            status_code=getattr(getattr(last_exc, "response", None), "status_code", None),
            response=getattr(last_exc, "response", None),
        ) from last_exc

    async def close(self):
        await self.client.aclose()
        self._base_url = None
