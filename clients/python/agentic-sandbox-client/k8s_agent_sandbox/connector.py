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
import socket
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
    SandboxdPodTunnelConnectionConfig,
)
from .k8s_helper import K8sHelper
from .metrics import sandbox_client_discovery_latency_ms
from .exceptions import (
    SandboxPortForwardError,
    SandboxRequestError,
)

ROUTER_SERVICE_NAME = "svc/sandbox-router-svc"
# POST endpoints include command execution, so replaying them can duplicate
# side effects after the server handled a request but returned a 5xx response.
RETRYABLE_METHODS = frozenset({"GET", "PUT", "DELETE"})


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
            host = f"[{ip_address}]" if ":" in ip_address else ip_address
            self.base_url = f"http://{host}"
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
                    self.base_url = f"http://127.0.0.1:{local_port}"
                    logging.info(f"Tunnel ready at {self.base_url}")
                    return self.base_url

                # Poll the local port at 50ms: this is a cheap localhost socket
                # probe, and a coarser interval (e.g. 500ms) adds a uniform
                # 0-500ms of avoidable latency to the first sandbox request.
                time.sleep(0.05)

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

class SandboxdPodTunnelStrategy(ConnectionStrategy):
    """Port-forwards directly to the sandbox pod for the sandboxd runtime.

    sandboxd binds loopback-only inside the pod (KEP-539.2), so it cannot be
    reached through the sandbox-router. This strategy forwards both sandboxd
    listeners from the pod: the REST filesystem port and the gRPC
    ProcessService port. ``connect()`` returns the REST base URL; the gRPC
    target is exposed via ``grpc_target``.
    """

    def __init__(
        self,
        sandbox_id: str,
        namespace: str,
        config: SandboxdPodTunnelConnectionConfig,
        get_pod_name: Callable[[], str | None] | None = None,
    ):
        self.sandbox_id = sandbox_id
        self.namespace = namespace
        self.config = config
        self._get_pod_name = get_pod_name
        self.port_forward_process: subprocess.Popen | None = None
        self.base_url: str | None = None
        self.grpc_target: str | None = None

    def _get_free_port(self) -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(('127.0.0.1', 0))
            return s.getsockname()[1]

    def _is_port_open(self, port: int) -> bool:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                return True
        except (socket.timeout, ConnectionRefusedError, OSError):
            return False

    def connect(self) -> str:
        if (
            self.base_url
            and self.port_forward_process
            and self.port_forward_process.poll() is None
        ):
            return self.base_url
        if self.port_forward_process:
            self.close()

        pod_name = self._get_pod_name() if self._get_pod_name else None
        if not pod_name:
            raise SandboxPortForwardError(
                "sandbox pod name not resolved yet; cannot port-forward to sandboxd"
            )

        start_time = time.monotonic()
        status = "success"
        try:
            rest_local = self._get_free_port()
            grpc_local = self._get_free_port()
            logging.info(f"Starting sandboxd pod tunnel for {self.sandbox_id}")
            self.port_forward_process = subprocess.Popen(
                [
                    "kubectl", "port-forward",
                    f"pod/{pod_name}",
                    f"{rest_local}:{self.config.rest_port}",
                    f"{grpc_local}:{self.config.grpc_port}",
                    "-n", self.namespace,
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            while time.monotonic() - start_time < self.config.port_forward_ready_timeout:
                if self.port_forward_process.poll() is not None:
                    _, stderr = self.port_forward_process.communicate()
                    raise SandboxPortForwardError(
                        f"Tunnel crashed: {stderr.decode(errors='replace')}")
                if self._is_port_open(rest_local) and self._is_port_open(grpc_local):
                    self.base_url = f"http://127.0.0.1:{rest_local}"
                    self.grpc_target = f"127.0.0.1:{grpc_local}"
                    logging.info(
                        f"sandboxd pod tunnel ready (rest={self.base_url}, grpc={self.grpc_target})")
                    return self.base_url
                time.sleep(0.05)
            self.close()
            raise TimeoutError("Failed to establish sandboxd pod tunnel.")
        except Exception:
            status = "failure"
            raise
        finally:
            latency = (time.monotonic() - start_time) * 1000
            sandbox_client_discovery_latency_ms.labels(
                mode="sandboxd_pod_tunnel", status=status).observe(latency)

    def close(self):
        if self.port_forward_process:
            try:
                self.port_forward_process.terminate()
                try:
                    self.port_forward_process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    self.port_forward_process.kill()
            except Exception as e:
                logging.error(f"Failed to stop sandboxd pod tunnel: {e}")
            finally:
                self.port_forward_process = None
                self.base_url = None
                self.grpc_target = None

    def verify_connection(self):
        if self.port_forward_process and self.port_forward_process.poll() is not None:
            _, stderr = self.port_forward_process.communicate()
            raise SandboxPortForwardError(
                f"sandboxd pod tunnel crashed!\nStderr: {stderr.decode(errors='replace')}")

    def should_inject_router_headers(self) -> bool:
        return False


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
        self._dns_url = (
            f"http://{sandbox_id}.{namespace}"
            f".svc.cluster.local:{config.server_port}"
        )
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
                host = f"[{pod_ip}]" if ":" in pod_ip else pod_ip
                self._cached_pod_ip_url = f"http://{host}:{self._server_port}"
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
        get_pod_name: Callable[[], str | None] | None = None,
    ):
        # Parameter initialization
        self.id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config
        self.k8s_helper = k8s_helper
        self._get_pod_ip = get_pod_ip
        self._get_pod_name = get_pod_name
        self._pod_ip: str | None = None
        self._pod_ip_resolved = False
        self._pod_ip_auth_failed = False
        self._grpc_channel = None
        self._grpc_channel_target: str | None = None

        # Connection strategy initialization
        self.strategy = self._connection_strategy()
        
        # HTTP Session setup
        self.session = requests.Session()
        retries = Retry(
            total=5,
            backoff_factor=0.5,
            status_forcelist=[500, 502, 503, 504],
            allowed_methods=RETRYABLE_METHODS,
        )
        self.session.mount("http://", HTTPAdapter(max_retries=retries))
        self.session.mount("https://", HTTPAdapter(max_retries=retries))
        

    def _connection_strategy(self):
        if isinstance(self.connection_config, SandboxDirectConnectionConfig):
            return DirectConnectionStrategy(self.connection_config)
        elif isinstance(self.connection_config, SandboxGatewayConnectionConfig):
            return GatewayConnectionStrategy(self.connection_config, self.k8s_helper)
        elif isinstance(self.connection_config, SandboxLocalTunnelConnectionConfig):
            return LocalTunnelConnectionStrategy(self.id, self.namespace, self.connection_config)
        elif isinstance(self.connection_config, SandboxInClusterConnectionConfig):
            return InClusterConnectionStrategy(self.id, self.namespace, self.connection_config, self._get_pod_ip)
        elif isinstance(self.connection_config, SandboxdPodTunnelConnectionConfig):
            return SandboxdPodTunnelStrategy(self.id, self.namespace, self.connection_config, self._get_pod_name)
        else:
            raise ValueError("Unknown connection configuration type")

    def is_sandboxd(self) -> bool:
        """Return True when this connector speaks the sandboxd runtime API."""
        return isinstance(self.connection_config, SandboxdPodTunnelConnectionConfig)

    def grpc_channel(self):
        """Return a lazily created gRPC channel to sandboxd's ProcessService.

        The channel is plaintext: it only ever traverses the port-forward
        tunnel to the pod's loopback listener. Requires the ``grpc`` extra.
        """
        if not self.is_sandboxd():
            raise RuntimeError("grpc_channel() is only available for the sandboxd runtime")
        target = getattr(self.strategy, "grpc_target", None)
        if not target:
            raise SandboxRequestError(
                "sandboxd gRPC endpoint not connected; call connect() first")
        # Invalidate the cached channel if the tunnel was re-established on a
        # new local port (connect() allocates a fresh port each time), so we
        # never return a channel pointing at a closed port.
        if self._grpc_channel is not None:
            if self._grpc_channel_target == target:
                return self._grpc_channel
            try:
                self._grpc_channel.close()
            except Exception:
                pass
            self._grpc_channel = None
            self._grpc_channel_target = None
        try:
            import grpc
        except ImportError as e:
            raise ImportError(
                "the sandboxd runtime requires gRPC support; install the "
                "'grpc' extra: pip install k8s-agent-sandbox[grpc]"
            ) from e
        self._grpc_channel = grpc.insecure_channel(target)
        self._grpc_channel_target = target
        return self._grpc_channel

    def connect(self) -> str:
        return self.strategy.connect()

    def close(self):
        self._pod_ip_resolved = False
        self._pod_ip = None
        if self._grpc_channel is not None:
            try:
                self._grpc_channel.close()
            except Exception:
                pass
            self._grpc_channel = None
            self._grpc_channel_target = None
        self.strategy.close()
        if self.session:
            self.session.close()

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
        # allowed_statuses lets a caller treat specific non-2xx codes as a
        # normal outcome (e.g. HEAD 404 for exists()): the response is
        # returned as-is instead of raising — which is important because the
        # raise path also calls self.close() and tears down the connection.
        allowed_statuses = kwargs.pop("allowed_statuses", None)
        try:
            # Establish connection (re-establishes if closed/dead)
            base_url = self.connect()

            # Verify if the connection is active before sending the request
            self.strategy.verify_connection()

            # Prepare the request
            url = f"{base_url.rstrip('/')}/{endpoint.lstrip('/')}"

            headers = kwargs.get("headers", {}).copy()
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
            # Return caller-tolerated statuses without raising (and thus
            # without closing the connection). Redirects are still rejected
            # above regardless of allowed_statuses.
            if allowed_statuses and response.status_code in allowed_statuses:
                return response
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
