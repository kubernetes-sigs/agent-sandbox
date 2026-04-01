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
import socket
import subprocess
import time
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
)
from .k8s_helper import K8sHelper
from .exceptions import (
    SandboxPortForwardError,
    SandboxRequestError,
)

ROUTER_SERVICE_NAME = "svc/sandbox-router-svc"

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

    def should_inject_router_headers(self) -> bool:
        """Returns True if X-Sandbox-* router headers should be attached to requests.

        Defaults to True. InClusterConnectionStrategy overrides to False because
        requests go directly to the sandbox pod, which does not use those headers.
        """
        return True

class DirectConnectionStrategy(ConnectionStrategy):
    def __init__(self, config: SandboxDirectConnectionConfig):
        self.config = config

    def connect(self) -> str:
        return self.config.api_url

    def close(self):
        pass

    def verify_connection(self):
        pass

class GatewayConnectionStrategy(ConnectionStrategy):
    def __init__(self, config: SandboxGatewayConnectionConfig, k8s_helper: K8sHelper):
        self.config = config
        self.k8s_helper = k8s_helper
        self.base_url = None

    def connect(self) -> str:
        if self.base_url:
            return self.base_url
            
        ip_address = self.k8s_helper.wait_for_gateway_ip(
            self.config.gateway_name,
            self.config.gateway_namespace,
            self.config.gateway_ready_timeout
        )
        self.base_url = f"http://{ip_address}"
        return self.base_url

    def close(self):
        self.base_url = None

    def verify_connection(self):
        pass

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

    def connect(self) -> str:
        if self.base_url and self.port_forward_process and self.port_forward_process.poll() is None:
             return self.base_url

        if self.port_forward_process:
             self.close()

        local_port = self._get_free_port()

        logging.info(
            f"Starting tunnel for Sandbox {self.sandbox_id}")
        self.port_forward_process = subprocess.Popen(
            [
                "kubectl", "port-forward",
                ROUTER_SERVICE_NAME,
                f"{local_port}:8080",
                "-n", self.namespace
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE
        )

        logging.info("Waiting for port-forwarding to be ready...")
        start_time = time.monotonic()
        while time.monotonic() - start_time < self.config.port_forward_ready_timeout:
            if self.port_forward_process.poll() is not None:
                _, stderr = self.port_forward_process.communicate()
                raise SandboxPortForwardError(
                    f"Tunnel crashed: {stderr.decode(errors='replace')}")

            try:
                with socket.create_connection(("127.0.0.1", local_port), timeout=0.1):
                    self.base_url = f"http://127.0.0.1:{local_port}"
                    logging.info(f"Tunnel ready at {self.base_url}")
                    time.sleep(0.5)
                    return self.base_url
            except (socket.timeout, ConnectionRefusedError):
                time.sleep(0.5)

        self.close()
        raise TimeoutError("Failed to establish tunnel to Router Service.")

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
    ):
        self.sandbox_id = sandbox_id
        self.namespace = namespace
        self.config = config

    def connect(self) -> str:
        return (
            f"http://{self.sandbox_id}.{self.namespace}"
            f".svc.cluster.local:{self.config.server_port}"
        )

    def verify_connection(self):
        pass

    def close(self):
        pass

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
    ):
        # Parameter initialization
        self.id = sandbox_id
        self.namespace = namespace
        self.connection_config = connection_config
        self.k8s_helper = k8s_helper
        
        # Connection strategy initialization
        self.strategy = self._connection_strategy()
        
        # HTTP Session setup
        self.session = requests.Session()
        retries = Retry(
            total=5,
            backoff_factor=0.5,
            status_forcelist=[500, 502, 503, 504],
            allowed_methods=["GET", "POST", "PUT", "DELETE"]
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
            return InClusterConnectionStrategy(self.id, self.namespace, self.connection_config)
        else:
            raise ValueError("Unknown connection configuration type")

    def get_conn_strategy(self):
        return self.strategy

    def connect(self) -> str:
        return self.strategy.connect()

    def close(self):
        self.strategy.close()

    def send_request(self, method: str, endpoint: str, **kwargs) -> requests.Response:
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
            kwargs["headers"] = headers

            # Send the request
            response = self.session.request(method, url, **kwargs)
            response.raise_for_status()
            return response
        except SandboxPortForwardError:
            self.close()
            raise
        except requests.exceptions.RequestException as e:
            resp = getattr(e, "response", None)
            status_code = resp.status_code if resp is not None else None

            logging.error(f"Request to sandbox failed: {e}")
            self.close()
            raise SandboxRequestError(
                f"Failed to communicate with the sandbox at {url}.",
                status_code=status_code,
                response=resp,
            ) from e