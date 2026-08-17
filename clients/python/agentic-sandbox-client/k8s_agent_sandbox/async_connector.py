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
import math
from typing import Callable, Awaitable

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


def _router_timeout_header_value(timeout) -> str | None:
    value = None
    if isinstance(timeout, bool):
        return None
    if isinstance(timeout, (int, float)):
        value = timeout
    elif isinstance(timeout, httpx.Timeout):
        value = timeout.read
    else:
        return None

    if value is None or not math.isfinite(value) or value <= 0:
        return None
    return str(value)


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
        get_pod_ip: Callable[[], Awaitable[str | None]] | None = None,
    ):
        if isinstance(connection_config, SandboxLocalTunnelConnectionConfig):
            raise ValueError(
                "AsyncSandboxConnector does not support SandboxLocalTunnelConnectionConfig. "
                "Use SandboxDirectConnectionConfig, SandboxGatewayConnectionConfig, "
                "or SandboxInClusterConnectionConfig instead. "
                "For local development, use the synchronous SandboxClient."
            )

        self.id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config
        self.k8s_helper = k8s_helper
        self._get_pod_ip = get_pod_ip

        self._base_url: str | None = None
        self._pod_ip: str | None = None
        self._pod_ip_resolved = False
        self._pod_ip_auth_failed = False
        self._cached_pod_ip_url: str | None = None
        if isinstance(connection_config, SandboxInClusterConnectionConfig):
            self._dns_url = (
                f"http://{sandbox_id}.{namespace}"
                f".svc.cluster.local:{connection_config.server_port}"
            )
            self._server_port = connection_config.server_port
        else:
            self._dns_url = None
            self._server_port = None

        self._inject_router_headers = not isinstance(
            connection_config, SandboxInClusterConnectionConfig
        )

        transport = httpx.AsyncHTTPTransport(retries=3)
        self.client = httpx.AsyncClient(
            transport=transport, timeout=httpx.Timeout(60.0)
        )

    async def _resolve_base_url(self) -> str:
        if isinstance(self.connection_config, SandboxInClusterConnectionConfig):
            if self._get_pod_ip:
                if self._pod_ip_resolved:
                    return self._cached_pod_ip_url or self._dns_url
                pod_ip = await self._get_pod_ip()
                if pod_ip:
                    self._pod_ip = pod_ip
                    host = f"[{pod_ip}]" if ":" in pod_ip else pod_ip
                    self._cached_pod_ip_url = f"http://{host}:{self._server_port}"
                    self._pod_ip_resolved = True
                    return self._cached_pod_ip_url
            return self._dns_url

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
            host = f"[{ip_address}]" if ":" in ip_address else ip_address
            self._base_url = f"http://{host}"
        else:
            raise ValueError(
                f"AsyncSandboxConnector does not support {type(self.connection_config).__name__}."
            )

        return self._base_url

    async def send_request(self, method: str, endpoint: str, **kwargs) -> httpx.Response:
        """Sends an HTTP request asynchronously to the sandbox with standard parameters.

        This method automatically resolves the gateway connection, appends the router/sandbox
        identity headers, overrides redirect options to disable client-side automatic
        redirection (for security/SSRF mitigation), and raises appropriate exceptions on errors.

        Args:
            method: The HTTP method (e.g., "GET", "POST").
            endpoint: The API endpoint path.
            **kwargs: Extra keyword arguments passed directly to the underlying
                `httpx.AsyncClient.request` invocation. Note that 'follow_redirects'
                is explicitly popped and overridden.

        Returns:
            The `httpx.Response` object representing the response from the sandbox.

        Raises:
            SandboxRequestError: If a connection/HTTP status error occurs, or if a redirect is
                returned (status codes 301, 302, 303, 307, 308).

        Note on Redirect Handling:
            Automatic redirection (SSRF risk mitigation) is explicitly disabled in the
            HTTP client. If a redirect status code recognized by httpx (301, 302,
            303, 307, 308) is returned, a SandboxRequestError wrapping HTTPStatusError is
            raised. Non-redirect 3xx status codes, such as 300 (Multiple Choices), 304
            (Not Modified), 305 (Use Proxy), and 306 (Switch Proxy), do not trigger
            automatic client redirection or raise redirect errors; they are returned
            directly to the caller because httpx does not consider them redirects
            and raise_for_status only raises for status codes 400 and above.
        """
        base_url = await self._resolve_base_url()
        url = f"{base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        headers = kwargs.pop("headers", {}).copy()
        # For security and SSRF mitigation, the SDK explicitly mandates blocking all HTTP redirects
        # to the internal sandbox endpoints. Any user-provided redirect settings are overridden and
        # ignored. We pop 'follow_redirects' here to prevent a TypeError due to duplicate keyword
        # arguments when calling httpx.AsyncClient.request.
        kwargs.pop("follow_redirects", None)

        if self._inject_router_headers:
            headers["X-Sandbox-ID"] = self.id
            headers["X-Sandbox-Namespace"] = self.namespace
            headers["X-Sandbox-Port"] = str(self.connection_config.server_port)
            timeout_header = _router_timeout_header_value(kwargs.get("timeout"))
            if timeout_header is not None:
                headers["X-Sandbox-Timeout"] = timeout_header
            if self._get_pod_ip and not self._pod_ip_auth_failed:
                if not self._pod_ip_resolved:
                    try:
                        pod_ip = await self._get_pod_ip()
                        if pod_ip:
                            self._pod_ip = pod_ip
                            self._pod_ip_resolved = True
                    except Exception as e:
                        status_code = getattr(getattr(e, "response", None), "status_code", None)
                        if status_code in (401, 403):
                            self._pod_ip_auth_failed = True
                            logger.debug(f"K8s API auth failed ({status_code}). Permanently disabling direct pod IP routing for this client instance.")
                        else:
                            logger.debug(f"Transient failure resolving pod IP for direct routing: {e}")
                if self._pod_ip:
                    headers["X-Sandbox-Pod-IP"] = self._pod_ip

        last_response: httpx.Response | None = None
        for attempt in range(MAX_RETRIES + 1):
            try:
                response = await self.client.request(
                    method, url, headers=headers, follow_redirects=False, **kwargs
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
                if response.is_redirect:
                    raise httpx.HTTPStatusError(
                        f"Redirection is not allowed (status code {response.status_code}).",
                        request=response.request,
                        response=response,
                    )
                response.raise_for_status()
                return response
            except httpx.HTTPStatusError as e:
                logger.error(f"Request to sandbox failed: {e}")
                # Clear cached URLs that may have gone stale.
                if isinstance(self.connection_config, SandboxGatewayConnectionConfig):
                    self._base_url = None
                self._pod_ip_resolved = False
                self._cached_pod_ip_url = None
                self._pod_ip = None
                raise SandboxRequestError(
                    f"Failed to communicate with the sandbox at {url}.",
                    status_code=e.response.status_code,
                    response=e.response,
                ) from e
            except httpx.HTTPError as e:
                logger.error(f"Request to sandbox failed: {e}")
                # Clear cached URLs that may have gone stale.
                if isinstance(self.connection_config, SandboxGatewayConnectionConfig):
                    self._base_url = None
                self._pod_ip_resolved = False
                self._cached_pod_ip_url = None
                self._pod_ip = None
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
        if isinstance(self.connection_config, SandboxGatewayConnectionConfig):
            self._base_url = None
        self._pod_ip_resolved = False
        self._cached_pod_ip_url = None
        self._pod_ip = None
