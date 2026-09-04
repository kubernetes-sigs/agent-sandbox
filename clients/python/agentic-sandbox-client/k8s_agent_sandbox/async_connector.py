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

"""Async HTTP and pod-tunnel transports used by the Python SDK.

Router-based connections share an ``httpx`` client. The sandboxd runtime uses
one direct ``kubectl port-forward`` with separate local endpoints for its REST
and gRPC listeners, both owned by the connector lifecycle.
"""

import asyncio
import inspect
import logging
import math
import socket
from typing import Callable, Awaitable

import httpx

logger = logging.getLogger(__name__)

from .async_k8s_helper import AsyncK8sHelper
from .exceptions import SandboxPortForwardError, SandboxRequestError
from .models import (
    SandboxConnectionConfig,
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxdPodTunnelConnectionConfig,
)

RETRYABLE_STATUS_CODES = {500, 502, 503, 504}
# POST endpoints include command execution, so replaying them can duplicate
# side effects after the server handled a request but returned a 5xx response.
RETRYABLE_METHODS = {"GET", "PUT", "DELETE"}
MAX_RETRIES = 5
BACKOFF_FACTOR = 0.5


def _router_timeout_header_value(timeout) -> str | None:
    """Return a positive read timeout suitable for the router header."""
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

    Supports DirectConnection, GatewayConnection, InCluster, and sandboxd pod
    tunnel modes. LocalTunnel mode is not supported because it relies on a
    router subprocess; use the sync SandboxConnector for local development.
    """

    def __init__(
        self,
        sandbox_id: str,
        namespace: str,
        connection_config: SandboxConnectionConfig,
        k8s_helper: AsyncK8sHelper,
        get_pod_ip: Callable[[], Awaitable[str | None]] | None = None,
        get_pod_name: Callable[[], Awaitable[str | None]] | None = None,
    ):
        if isinstance(connection_config, SandboxLocalTunnelConnectionConfig):
            raise ValueError(
                "AsyncSandboxConnector does not support SandboxLocalTunnelConnectionConfig. "
                "Use SandboxDirectConnectionConfig, SandboxGatewayConnectionConfig, "
                "SandboxInClusterConnectionConfig, or "
                "SandboxdPodTunnelConnectionConfig instead. "
                "For local development, use the synchronous SandboxClient."
            )
        self.id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config
        self.k8s_helper = k8s_helper
        self._get_pod_ip = get_pod_ip
        self._grpc_channel = None
        self._grpc_channel_target: str | None = None
        # Command and filesystem calls may arrive together on first use. The
        # lock keeps channel creation and replacement single-owner.
        self._grpc_lock = asyncio.Lock()
        # Serialize endpoint resolution and teardown so close cannot race with
        # a new tunnel or gRPC channel being published.
        self._lifecycle_lock = asyncio.Lock()
        self._closed = False
        self.grpc_target: str | None = None
        self._sandboxd_strategy = None
        if isinstance(connection_config, SandboxdPodTunnelConnectionConfig):
            self._sandboxd_strategy = AsyncSandboxdPodTunnelStrategy(
                sandbox_id=sandbox_id,
                namespace=namespace,
                config=connection_config,
                get_pod_name=get_pod_name,
            )

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
            connection_config,
            (SandboxInClusterConnectionConfig, SandboxdPodTunnelConnectionConfig),
        )

        transport = httpx.AsyncHTTPTransport(retries=3)
        self.client = httpx.AsyncClient(
            transport=transport, timeout=httpx.Timeout(60.0)
        )

    async def _resolve_base_url(self) -> str:
        """Resolve the HTTP endpoint for the configured connection mode."""
        if self._sandboxd_strategy is not None:
            base_url, self.grpc_target = await self._sandboxd_strategy.connect()
            return base_url

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
                is explicitly popped and overridden. `allowed_statuses` may be
                supplied as a set of status codes that should be returned without
                raising an HTTP status error.

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
        async with self._lifecycle_lock:
            self._ensure_open()
            base_url = await self._resolve_base_url()
        url = f"{base_url.rstrip('/')}/{endpoint.lstrip('/')}"

        allowed_statuses = kwargs.pop("allowed_statuses", None)
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
                if (
                    method.upper() in RETRYABLE_METHODS
                    and response.status_code in RETRYABLE_STATUS_CODES
                    and attempt < MAX_RETRIES
                ):
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
                if allowed_statuses and response.status_code in allowed_statuses:
                    return response
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

    async def connect(self) -> str:
        """Establish the configured transport and return its HTTP base URL."""
        async with self._lifecycle_lock:
            self._ensure_open()
            return await self._resolve_base_url()

    def _ensure_open(self) -> None:
        """Raise when an operation is attempted after connection teardown."""
        if self._closed:
            raise RuntimeError("sandbox connector is closed")

    def _close_for_atexit(self) -> None:
        """Release loop-independent local resources during interpreter shutdown.

        The regular async close path cannot be used from an ``atexit`` handler:
        its objects may have been created by an event loop that is already
        closed. Only the sandboxd subprocess has a synchronous termination
        operation, so leave async HTTP and gRPC objects to interpreter teardown.
        """
        self._closed = True
        try:
            if self._sandboxd_strategy is not None:
                self._sandboxd_strategy._close_for_atexit()
        finally:
            self.grpc_target = None
            self._grpc_channel = None
            self._grpc_channel_target = None
            self._base_url = None
            self._pod_ip_resolved = False
            self._cached_pod_ip_url = None
            self._pod_ip = None

    def is_sandboxd(self) -> bool:
        """Return whether this connector targets the sandboxd runtime."""
        return self._sandboxd_strategy is not None

    def should_inject_router_headers(self) -> bool:
        """Return whether requests pass through the sandbox router."""
        return self._inject_router_headers

    async def grpc_channel(self):
        """Return the reusable ``grpc.aio`` channel for sandboxd commands.

        Dependency validation happens before tunnel setup so a missing optional
        extra never leaves a ``kubectl`` process behind.
        """
        self._ensure_open()
        if not self.is_sandboxd():
            raise RuntimeError("grpc_channel() is only available for the sandboxd runtime")
        try:
            import grpc
        except ImportError as e:
            raise ImportError(
                "the sandboxd runtime requires gRPC support; install the "
                "'grpc' extra: pip install k8s-agent-sandbox[grpc]"
            ) from e

        async with self._lifecycle_lock:
            self._ensure_open()
            async with self._grpc_lock:
                if not self.grpc_target:
                    _, self.grpc_target = await self._sandboxd_strategy.connect()
                if (
                    self._grpc_channel is not None
                    and self._grpc_channel_target == self.grpc_target
                ):
                    return self._grpc_channel
                if self._grpc_channel is not None:
                    result = self._grpc_channel.close()
                    if inspect.isawaitable(result):
                        await result
                    self._grpc_channel = None
                    self._grpc_channel_target = None
                self._grpc_channel = grpc.aio.insecure_channel(self.grpc_target)
                self._grpc_channel_target = self.grpc_target
                return self._grpc_channel

    async def close(self):
        """Close HTTP, gRPC, and port-forward resources owned by the connector."""
        async with self._lifecycle_lock:
            if self._closed:
                return
            self._closed = True
            await self.client.aclose()
            async with self._grpc_lock:
                if self._grpc_channel is not None:
                    result = self._grpc_channel.close()
                    if inspect.isawaitable(result):
                        await result
                    self._grpc_channel = None
                    self._grpc_channel_target = None
            if self._sandboxd_strategy is not None:
                await self._sandboxd_strategy.close()
                self.grpc_target = None
            if isinstance(self.connection_config, SandboxGatewayConnectionConfig):
                self._base_url = None
            self._pod_ip_resolved = False
            self._cached_pod_ip_url = None
            self._pod_ip = None


class AsyncSandboxdPodTunnelStrategy:
    """Own the direct Pod tunnel used by sandboxd's REST and gRPC clients.

    A single ``kubectl`` process forwards both ports. Establishment and teardown
    are serialized because file and command operations can race on first use.
    """

    def __init__(
        self,
        sandbox_id: str,
        namespace: str,
        config: SandboxdPodTunnelConnectionConfig,
        get_pod_name: Callable[[], Awaitable[str | None]] | None = None,
    ):
        self.sandbox_id = sandbox_id
        self.namespace = namespace
        self.config = config
        self._get_pod_name = get_pod_name
        self.port_forward_process: asyncio.subprocess.Process | None = None
        self.base_url: str | None = None
        self.grpc_target: str | None = None
        # Only one task may publish or tear down the shared subprocess state.
        self._lifecycle_lock = asyncio.Lock()
        self._closed = False

    @staticmethod
    def _get_free_port() -> int:
        """Return an unused loopback port for ``kubectl`` to bind."""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            return sock.getsockname()[1]

    @staticmethod
    async def _is_port_open(port: int) -> bool:
        """Check whether a forwarded local port accepts connections."""
        try:
            _reader, writer = await asyncio.wait_for(
                asyncio.open_connection("127.0.0.1", port), timeout=0.1
            )
            writer.close()
            await writer.wait_closed()
            return True
        except (asyncio.TimeoutError, ConnectionRefusedError, OSError):
            return False

    async def connect(self) -> tuple[str, str]:
        """Establish or reuse the tunnel and return REST and gRPC endpoints."""
        async with self._lifecycle_lock:
            if self._closed:
                raise SandboxPortForwardError("sandboxd port-forward strategy is closed")
            return await self._connect_locked()

    async def _connect_locked(self) -> tuple[str, str]:
        """Establish the tunnel while the caller holds ``_lifecycle_lock``."""
        if (
            self.base_url
            and self.grpc_target
            and self.port_forward_process
            and self.port_forward_process.returncode is None
        ):
            return self.base_url, self.grpc_target

        if self.port_forward_process:
            await self._close_locked()

        pod_name = self._get_pod_name() if self._get_pod_name else None
        if inspect.isawaitable(pod_name):
            pod_name = await pod_name
        if not pod_name:
            raise SandboxPortForwardError(
                "sandbox pod name not resolved yet; cannot port-forward to sandboxd"
            )

        rest_local = self._get_free_port()
        grpc_local = self._get_free_port()
        try:
            try:
                self.port_forward_process = await asyncio.create_subprocess_exec(
                    "kubectl",
                    "port-forward",
                    f"pod/{pod_name}",
                    f"{rest_local}:{self.config.rest_port}",
                    f"{grpc_local}:{self.config.grpc_port}",
                    "-n",
                    self.namespace,
                    # kubectl logs each forwarded connection. Discard the
                    # long-lived subprocess output because this SDK has no log
                    # consumer for the forwarding process.
                    stdout=asyncio.subprocess.DEVNULL,
                    stderr=asyncio.subprocess.DEVNULL,
                )
            except OSError as e:
                raise SandboxPortForwardError(
                    "failed to start sandboxd port-forward for pod "
                    f"{self.namespace}/{pod_name}: {e}"
                ) from e

            deadline = (
                asyncio.get_running_loop().time()
                + self.config.port_forward_ready_timeout
            )
            while asyncio.get_running_loop().time() < deadline:
                if self.port_forward_process.returncode is not None:
                    await self._close_locked()
                    raise SandboxPortForwardError(
                        "sandboxd port-forward exited before it was ready for pod "
                        f"{self.namespace}/{pod_name}"
                    )
                if await self._is_port_open(rest_local) and await self._is_port_open(
                    grpc_local
                ):
                    self.base_url = f"http://127.0.0.1:{rest_local}"
                    self.grpc_target = f"127.0.0.1:{grpc_local}"
                    return self.base_url, self.grpc_target
                await asyncio.sleep(0.05)

            await self._close_locked()
            raise SandboxPortForwardError(
                "timed out waiting for sandboxd port-forward for pod "
                f"{self.namespace}/{pod_name}"
            )
        except asyncio.CancelledError:
            await self._close_locked()
            raise

    async def close(self):
        """Stop the port-forward and clear its published endpoints."""
        async with self._lifecycle_lock:
            if self._closed:
                return
            self._closed = True
            await self._close_locked()

    def _close_for_atexit(self) -> None:
        """Terminate the port-forward without awaiting loop-bound state."""
        self._closed = True
        process = self.port_forward_process
        self.port_forward_process = None
        self.base_url = None
        self.grpc_target = None
        if process is None:
            return

        # ``asyncio.subprocess.Process.wait()`` is tied to the loop that
        # created the process.  ``terminate``/``kill`` dispatch directly to
        # the child process and are safe for this last-resort shutdown path.
        try:
            if process.returncode is None:
                process.terminate()
        except (OSError, ProcessLookupError):
            pass
        try:
            if process.returncode is None:
                process.kill()
        except (OSError, ProcessLookupError):
            pass

    async def _close_locked(self):
        """Tear down subprocess state while holding ``_lifecycle_lock``."""
        process = self.port_forward_process
        self.port_forward_process = None
        self.base_url = None
        self.grpc_target = None
        if process is None:
            return
        if process.returncode is None:
            process.terminate()
            try:
                await asyncio.wait_for(process.wait(), timeout=2)
            except asyncio.TimeoutError:
                process.kill()
                await process.wait()
