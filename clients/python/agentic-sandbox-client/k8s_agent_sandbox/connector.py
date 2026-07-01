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
import math
import os
import socket
import ssl
import subprocess
import time
from typing import Callable
import requests
from abc import ABC, abstractmethod
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from .models import (
    SandboxConnectionConfig,
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    TLSConfig,
)
from .k8s_helper import K8sHelper
from .metrics import sandbox_client_discovery_latency_ms
from .exceptions import (
    SandboxPortForwardError,
    SandboxRequestError,
)
from .utils import (
    apply_tls_to_requests_session,
    build_base_url,
    build_ssl_context,
)

ROUTER_SERVICE_NAME = "svc/sandbox-router-svc"


class _TLSAdapter(HTTPAdapter):
    """HTTPAdapter that injects a custom SSL context and optional SNI override.

    Used when ``TLSConfig.ca_cert`` is supplied (inline PEM materialized into an
    SSLContext) or when ``server_name_override`` is set (e.g. LocalTunnel +
    https where the cert's CN does not match 127.0.0.1).
    """

    def __init__(
        self,
        ssl_context: ssl.SSLContext | None = None,
        server_name: str | None = None,
        **kwargs,
    ):
        self._ssl_context = ssl_context
        self._server_name = server_name
        super().__init__(**kwargs)

    def init_poolmanager(self, *args, **kwargs):
        if self._ssl_context is not None:
            kwargs["ssl_context"] = self._ssl_context
        if self._server_name is not None:
            kwargs["server_hostname"] = self._server_name
            kwargs["assert_hostname"] = self._server_name
        return super().init_poolmanager(*args, **kwargs)


def _maybe_warn_plaintext_http(config: SandboxConnectionConfig) -> None:
    """Log a notice when http:// is used for a strategy that should normally
    be encrypted.

    LocalTunnel is exempt because traffic is loopback-only. DirectConnection
    is also exempt because the scheme comes from the user-supplied URL.
    """
    if isinstance(config, (SandboxGatewayConnectionConfig, SandboxInClusterConnectionConfig)):
        if getattr(config, "scheme", "http") == "http":
            logging.getLogger(__name__).warning(
                "%s is using plaintext http:// scheme. Set scheme='https' and "
                "configure tls=TLSConfig(...) to encrypt traffic.",
                type(config).__name__,
            )


def _router_timeout_header_value(timeout) -> str | None:
    value = None
    if isinstance(timeout, bool):
        return None
    if isinstance(timeout, (int, float)):
        value = timeout
    elif isinstance(timeout, tuple):
        if len(timeout) == 0:
            return None
        value = timeout[-1]
    else:
        return None

    if value is None or not math.isfinite(value) or value <= 0:
        return None
    return str(value)


class ConnectionStrategy(ABC):
    """Abstract base class for connection strategies."""
    
    @abstractmethod
    def connect(self) -> str:
        """Establishes the connection and returns the base URL."""
        pass

    @abstractmethod
    def close(self):
        """Cleans up any resources associated with the connection."""
        pass

    @abstractmethod
    def verify_connection(self):
        """Checks if the connection is healthy. Raises SandboxPortForwardError if not."""
        pass

    @abstractmethod
    def should_inject_router_headers(self) -> bool:
        """Returns True if X-Sandbox-* router headers should be injected into requests."""
        pass

class DirectConnectionStrategy(ConnectionStrategy):
    def __init__(self, config: SandboxDirectConnectionConfig):
        self.config = config

    def connect(self) -> str:
        return self.config.api_url

    def close(self):
        pass

    def verify_connection(self):
        pass

    def should_inject_router_headers(self) -> bool:
        return True

class GatewayConnectionStrategy(ConnectionStrategy):
    def __init__(self, config: SandboxGatewayConnectionConfig, k8s_helper: K8sHelper):
        self.config = config
        self.k8s_helper = k8s_helper
        self.base_url = None

    def connect(self) -> str:
        if self.base_url:
            return self.base_url
            
        start_time = time.monotonic()
        status = "success"
        try:
            ip_address = self.k8s_helper.wait_for_gateway_ip(
                self.config.gateway_name,
                self.config.gateway_namespace,
                self.config.gateway_ready_timeout
            )
            self.base_url = build_base_url(self.config.scheme, ip_address)
            return self.base_url
        except Exception:
            status = "failure"
            raise
        finally:
            latency = (time.monotonic() - start_time) * 1000
            sandbox_client_discovery_latency_ms.labels(mode="gateway", status=status).observe(latency)

    def close(self):
        self.base_url = None

    def verify_connection(self):
        pass

    def should_inject_router_headers(self) -> bool:
        return True

class LocalTunnelConnectionStrategy(ConnectionStrategy):
    def __init__(self, sandbox_id: str, namespace: str, config: SandboxLocalTunnelConnectionConfig):
        self.sandbox_id = sandbox_id
        self.namespace = namespace
        self.config = config
        self.port_forward_process: subprocess.Popen | None = None
        self.base_url = None

    def _get_free_port(self):
        """Finds a free port on localhost."""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(('127.0.0.1', 0))
            return s.getsockname()[1]

    def _is_port_open(self, port: int) -> bool:
        """Checks if a port is open on localhost."""
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                return True
        except (socket.timeout, ConnectionRefusedError):
            return False

    def connect(self) -> str:
        if self.base_url and self.port_forward_process and self.port_forward_process.poll() is None:
             return self.base_url

        if self.port_forward_process:
             self.close()

        start_time = time.monotonic()
        status = "success"
        
        try:
            local_port = self._get_free_port()

            logging.info(
                f"Starting tunnel for Sandbox {self.sandbox_id}")
            
            self.port_forward_process = subprocess.Popen(
                [
                    "kubectl", "port-forward",
                    ROUTER_SERVICE_NAME,
                    f"{local_port}:8080",
                    "-n", self.config.router_namespace
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE
            )

            logging.info("Waiting for port-forwarding to be ready...")
            while time.monotonic() - start_time < self.config.port_forward_ready_timeout:
                if self.port_forward_process.poll() is not None:
                    _, stderr = self.port_forward_process.communicate()
                    raise SandboxPortForwardError(
                        f"Tunnel crashed: {stderr.decode(errors='replace')}")

                if self._is_port_open(local_port):
                    self.base_url = build_base_url(self.config.scheme, "127.0.0.1", local_port)
                    logging.info(f"Tunnel ready at {self.base_url}")
                    return self.base_url
                
                time.sleep(0.5)

            self.close()
            raise TimeoutError("Failed to establish tunnel to Router Service.")
        except Exception:
            status = "failure"
            raise
        finally:
            latency = (time.monotonic() - start_time) * 1000
            sandbox_client_discovery_latency_ms.labels(mode="port_forward", status=status).observe(latency)

    def close(self):
        if self.port_forward_process:
            try:
                logging.info(f"Stopping port-forwarding for Sandbox {self.sandbox_id}...")
                self.port_forward_process.terminate()
                try:
                    self.port_forward_process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    self.port_forward_process.kill()
            except Exception as e:
                logging.error(f"Failed to stop port-forwarding: {e}")
            finally:
                self.port_forward_process = None
                self.base_url = None

    def verify_connection(self):
        if self.port_forward_process and self.port_forward_process.poll() is not None:
            _, stderr = self.port_forward_process.communicate()
            raise SandboxPortForwardError(
                f"Kubectl Port-Forward crashed!\n"
                f"Stderr: {stderr.decode(errors='replace')}"
            )

    def should_inject_router_headers(self) -> bool:
        return True

class InClusterConnectionStrategy(ConnectionStrategy):
    """Provides direct in-cluster connectivity to a sandbox pod, bypassing the router.

    Requires the SDK to run inside the same Kubernetes cluster as the sandbox.
    Router-specific request headers are not injected.
    """

    def __init__(
        self,
        sandbox_id: str,
        namespace: str,
        config: SandboxInClusterConnectionConfig,
        get_pod_ip: Callable[[], str | None] | None = None,
    ):
        self._scheme = config.scheme
        host = f"{sandbox_id}.{namespace}.svc.cluster.local"
        self._dns_url = build_base_url(self._scheme, host, config.server_port)
        self._get_pod_ip = get_pod_ip
        self._server_port = config.server_port
        self._resolved = False
        self._cached_pod_ip_url: str | None = None

    def connect(self) -> str:
        if self._get_pod_ip:
            if self._resolved:
                return self._cached_pod_ip_url or self._dns_url
            pod_ip = self._get_pod_ip()
            if pod_ip:
                self._cached_pod_ip_url = build_base_url(self._scheme, pod_ip, self._server_port)
                self._resolved = True
                return self._cached_pod_ip_url
        return self._dns_url

    def verify_connection(self):
        pass

    def close(self):
        self._resolved = False
        self._cached_pod_ip_url = None

    def should_inject_router_headers(self) -> bool:
        return False

class SandboxConnector:
    """
    Manages the connection to the Sandbox, including auto-discovery and port-forwarding.
    """
    def __init__(
        self,
        sandbox_id: str,
        namespace: str,
        connection_config: SandboxConnectionConfig,
        k8s_helper: K8sHelper,
        get_pod_ip: Callable[[], str | None] | None = None,
    ):
        # Parameter initialization
        self.id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config
        self.k8s_helper = k8s_helper
        self._get_pod_ip = get_pod_ip
        self._pod_ip: str | None = None
        self._pod_ip_resolved = False
        self._pod_ip_auth_failed = False

        # Connection strategy initialization
        self.strategy = self._connection_strategy()

        # Warn when plaintext http is used on a mode that supports TLS
        # (Gateway, InCluster). LocalTunnel and DirectConnection are exempt.
        _maybe_warn_plaintext_http(connection_config)

        # HTTP Session setup
        self.session = requests.Session()
        retries = Retry(
            total=5,
            backoff_factor=0.5,
            status_forcelist=[500, 502, 503, 504],
            allowed_methods=["GET", "POST", "PUT", "DELETE"]
        )
        self.session.mount("http://", HTTPAdapter(max_retries=retries))

        # TLS plumbing for the https:// adapter. If a custom CA is provided
        # (either inline PEM or file path), we build an SSLContext directly;
        # otherwise we fall back to requests' native verify= path.
        # server_name_override is honored via the custom _TLSAdapter so
        # connections to addresses that don't match the cert CN (e.g.
        # LocalTunnel 127.0.0.1) can be validated.
        self._tls_temp_ca_path: str | None = None
        tls: TLSConfig | None = getattr(connection_config, "tls", None)
        self._sni_override = tls.server_name_override if tls else None

        ssl_ctx = build_ssl_context(tls)
        if isinstance(ssl_ctx, ssl.SSLContext) or self._sni_override is not None:
            self.session.mount(
                "https://",
                _TLSAdapter(
                    ssl_context=ssl_ctx if isinstance(ssl_ctx, ssl.SSLContext) else None,
                    server_name=self._sni_override,
                    max_retries=retries,
                ),
            )
            if ssl_ctx is False:
                self.session.verify = False
        else:
            self.session.mount("https://", HTTPAdapter(max_retries=retries))
            if ssl_ctx is False:
                self.session.verify = False
            else:
                # Fallback path when no custom CA or SNI override is provided.
                self._tls_temp_ca_path = apply_tls_to_requests_session(self.session, tls)


    def _connection_strategy(self):
        if isinstance(self.connection_config, SandboxDirectConnectionConfig):
            return DirectConnectionStrategy(self.connection_config)
        elif isinstance(self.connection_config, SandboxGatewayConnectionConfig):
            return GatewayConnectionStrategy(self.connection_config, self.k8s_helper)
        elif isinstance(self.connection_config, SandboxLocalTunnelConnectionConfig):
            return LocalTunnelConnectionStrategy(self.id, self.namespace, self.connection_config)
        elif isinstance(self.connection_config, SandboxInClusterConnectionConfig):
            return InClusterConnectionStrategy(self.id, self.namespace, self.connection_config, self._get_pod_ip)
        else:
            raise ValueError("Unknown connection configuration type")

    def get_conn_strategy(self):
        return self.strategy

    def connect(self) -> str:
        return self.strategy.connect()

    def close(self):
        self._pod_ip_resolved = False
        self._pod_ip = None
        self.strategy.close()
        if self.session:
            self.session.close()
        if self._tls_temp_ca_path:
            try:
                os.unlink(self._tls_temp_ca_path)
            except OSError:
                pass
            self._tls_temp_ca_path = None

    def send_request(self, method: str, endpoint: str, **kwargs) -> requests.Response:
        """Sends an HTTP request to the sandbox with standard parameters.

        This method automatically resolves the gateway or tunnel connection,
        appends the router/sandbox identity headers, overrides redirect options to
        disable client-side automatic redirection (for security/SSRF mitigation),
        and raises appropriate exceptions on errors.

        Args:
            method: The HTTP method (e.g., "GET", "POST").
            endpoint: The API endpoint path.
            **kwargs: Extra keyword arguments passed directly to the underlying
                `requests.Session.request` invocation. Note that 'allow_redirects'
                is explicitly popped and overridden.

        Returns:
            The `requests.Response` object representing the response from the sandbox.

        Raises:
            SandboxRequestError: If a connection error occurs, or if a redirect is
                returned (status codes 301, 302, 303, 307, 308).
            SandboxPortForwardError: If the local port-forward tunnel crashes.

        Note on Redirect Handling:
            Automatic redirection (SSRF risk mitigation) is explicitly disabled in the
            HTTP client. If a redirect status code recognized by requests (301, 302,
            303, 307, 308) is returned, a SandboxRequestError wrapping HTTPError is
            raised. Non-redirect 3xx status codes, such as 300 (Multiple Choices), 304
            (Not Modified), 305 (Use Proxy), and 306 (Switch Proxy), do not trigger
            automatic client redirection or raise redirect errors; they are returned
            directly to the caller because requests does not consider them redirects
            and raise_for_status only raises for status codes 400 and above.
        """
        try:
            # Establish connection (re-establishes if closed/dead)
            base_url = self.connect()

            # Verify if the connection is active before sending the request
            self.strategy.verify_connection()

            # Prepare the request
            url = f"{base_url.rstrip('/')}/{endpoint.lstrip('/')}"

            headers = kwargs.get("headers", {}).copy()
            if self._sni_override is not None and not any(
                k.lower() == "host" for k in headers
            ):
                headers["Host"] = self._sni_override
            if self.strategy.should_inject_router_headers():
                headers["X-Sandbox-ID"] = self.id
                headers["X-Sandbox-Namespace"] = self.namespace
                headers["X-Sandbox-Port"] = str(self.connection_config.server_port)
                timeout_header = _router_timeout_header_value(kwargs.get("timeout"))
                if timeout_header is not None:
                    headers["X-Sandbox-Timeout"] = timeout_header
                if self._get_pod_ip and not self._pod_ip_auth_failed:
                    if not self._pod_ip_resolved:
                        try:
                            pod_ip = self._get_pod_ip()
                            if pod_ip:
                                self._pod_ip = pod_ip
                                self._pod_ip_resolved = True
                        except Exception as e:
                            status_code = getattr(getattr(e, "response", None), "status_code", None)
                            if status_code in (401, 403):
                                self._pod_ip_auth_failed = True
                                logging.debug(f"K8s API auth failed ({status_code}). Permanently disabling direct pod IP routing for this client instance.")
                            else:
                                logging.debug(f"Transient failure resolving pod IP for direct routing: {e}")
                    if self._pod_ip:
                        headers["X-Sandbox-Pod-IP"] = self._pod_ip
            kwargs["headers"] = headers

            # For security and SSRF mitigation, the SDK explicitly mandates blocking all HTTP redirects
            # to the internal sandbox endpoints. Any user-provided redirect settings are overridden and
            # ignored. We pop 'allow_redirects' here to prevent a TypeError due to duplicate keyword
            # arguments when calling requests.Session.request.
            kwargs.pop("allow_redirects", None)

            # Send the request with redirections blocked
            response = self.session.request(method, url, allow_redirects=False, **kwargs)
            if response.is_redirect:
                raise requests.exceptions.HTTPError(
                    f"Redirection is not allowed (status code {response.status_code}).",
                    response=response,
                )
            response.raise_for_status()
            return response
        except SandboxPortForwardError:
            self.close()
            raise
        except requests.exceptions.RequestException as e:
            resp = getattr(e, "response", None)
            status_code = resp.status_code if resp is not None else None

            logging.error(f"Request to sandbox failed: {e}")
            self._pod_ip_resolved = False
            self._pod_ip = None
            self.close()
            raise SandboxRequestError(
                f"Failed to communicate with the sandbox at {url}.",
                status_code=status_code,
                response=resp,
            ) from e
