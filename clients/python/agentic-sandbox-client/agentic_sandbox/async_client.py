# Copyright 2025 The Kubernetes Authors.
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
Async client for interacting with the Agentic Sandbox.
"""

import asyncio
import logging
import os
import time
from types import TracebackType
from typing import Any

try:
    import httpx
    from kubernetes_asyncio import client as k8s_client
    from kubernetes_asyncio import config as k8s_config
    from kubernetes_asyncio import watch as k8s_watch
except ImportError as exc:
    raise ImportError(
        "Async dependencies are not installed. Install agentic_sandbox[async]."
    ) from exc

from .sandbox_client_base import (
    CLAIM_API_GROUP,
    CLAIM_API_VERSION,
    CLAIM_PLURAL_NAME,
    GATEWAY_API_GROUP,
    GATEWAY_API_VERSION,
    GATEWAY_PLURAL,
    SANDBOX_API_GROUP,
    SANDBOX_API_VERSION,
    SANDBOX_PLURAL_NAME,
    ExecutionResult,
    SandboxClientBase,
)
from .trace_manager import trace, trace_span


class AsyncSandboxClient(SandboxClientBase):
    """
    Async client for creating and interacting with a stateful Sandbox via a router.
    """

    def __init__(
        self,
        template_name: str,
        namespace: str = "default",  # Where Sandbox lives
        gateway_name: str | None = None,  # Name of the Gateway
        gateway_namespace: str = "default",  # Where Gateway lives
        api_url: str | None = None,  # Allow custom URL (DNS or Localhost)
        server_port: int = 8888,  # The port the runtime inside the sandbox listens on
        sandbox_ready_timeout: int = 180,
        gateway_ready_timeout: int = 180,
        port_forward_ready_timeout: int = 30,
        enable_tracing: bool = False,
        trace_service_name: str = "sandbox-client",
    ):
        super().__init__(
            template_name=template_name,
            namespace=namespace,
            gateway_name=gateway_name,
            gateway_namespace=gateway_namespace,
            api_url=api_url,
            server_port=server_port,
            sandbox_ready_timeout=sandbox_ready_timeout,
            gateway_ready_timeout=gateway_ready_timeout,
            port_forward_ready_timeout=port_forward_ready_timeout,
            enable_tracing=enable_tracing,
            trace_service_name=trace_service_name,
        )
        self.custom_objects_api: k8s_client.CustomObjectsApi | None = None
        self.http_client: httpx.AsyncClient | None = None
        self.port_forward_process: asyncio.subprocess.Process | None = None
        self._kube_config_loaded: bool = False

    async def _load_config(self) -> None:
        if self._kube_config_loaded:
            return

        try:
            await k8s_config.load_incluster_config()
        except k8s_config.ConfigException:
            await k8s_config.load_kube_config()

        self.custom_objects_api = k8s_client.CustomObjectsApi()
        self._kube_config_loaded = True

    @trace_span("create_claim")
    async def _create_claim(self, trace_context_str: str = "") -> None:
        """Creates the SandboxClaim custom resource in the Kubernetes cluster."""
        await self._load_config()
        custom_objects_api = self.custom_objects_api
        if custom_objects_api is None:
            raise RuntimeError("Kubernetes client is not initialized.")
        manifest = self._build_claim_manifest(trace_context_str)

        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.claim.name", self.claim_name)

        logging.info(
            f"Creating SandboxClaim '{self.claim_name}' "
            f"in namespace '{self.namespace}' "
            f"using template '{self.template_name}'..."
        )
        await custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=self.namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest,
        )

    @trace_span("wait_for_sandbox_ready")
    async def _wait_for_sandbox_ready(self) -> None:
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        if not self.claim_name:
            raise RuntimeError(
                "Cannot wait for sandbox; a sandboxclaim has not been created."
            )

        await self._load_config()
        custom_objects_api = self.custom_objects_api
        if custom_objects_api is None:
            raise RuntimeError("Kubernetes client is not initialized.")
        watcher = k8s_watch.Watch()
        logging.info("Watching for Sandbox to become ready...")
        async for event in watcher.stream(
            func=custom_objects_api.list_namespaced_custom_object,
            namespace=self.namespace,
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            plural=SANDBOX_PLURAL_NAME,
            field_selector=f"metadata.name={self.claim_name}",
            timeout_seconds=self.sandbox_ready_timeout,
        ):
            if event["type"] in ["ADDED", "MODIFIED"]:
                sandbox_object = event["object"]
                status = sandbox_object.get("status", {})
                conditions = status.get("conditions", [])
                is_ready = False
                for cond in conditions:
                    if cond.get("type") == "Ready" and cond.get("status") == "True":
                        is_ready = True
                        break

                if is_ready:
                    self._set_sandbox_metadata(sandbox_object)
                    logging.info(f"Sandbox {self.sandbox_name} is ready.")
                    watcher.stop()
                    return

        await self.__aexit__(None, None, None)
        raise TimeoutError(
            f"Sandbox did not become ready within {self.sandbox_ready_timeout} seconds."
        )

    @trace_span("dev_mode_tunnel")
    async def _start_and_wait_for_port_forward(self) -> None:
        """
        Starts 'kubectl port-forward' to the Router Service.
        This allows 'Dev Mode' without needing a public Gateway IP.
        """
        local_port = self._get_free_port()

        # Assumes the router service name from sandbox_router.yaml
        router_svc = "svc/sandbox-router-svc"

        logging.info(
            f"Starting Dev Mode tunnel: localhost:{local_port} -> {router_svc}:8080..."
        )

        self.port_forward_process = await asyncio.create_subprocess_exec(
            "kubectl",
            "port-forward",
            router_svc,
            # Tunnel to Router (8080), not Sandbox (8888)
            f"{local_port}:8080",
            # The router lives in the sandbox NS (no gateway)
            "-n",
            self.namespace,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        port_forward_process = self.port_forward_process

        logging.info("Waiting for port-forwarding to be ready...")
        start_time = time.monotonic()
        while time.monotonic() - start_time < self.port_forward_ready_timeout:
            if port_forward_process.returncode is not None:
                _, stderr = await port_forward_process.communicate()
                raise RuntimeError(f"Tunnel crashed: {stderr.decode(errors='ignore')}")

            try:
                _, writer = await asyncio.wait_for(
                    asyncio.open_connection("127.0.0.1", local_port),
                    timeout=0.1,
                )
                writer.close()
                await writer.wait_closed()
                self.base_url = f"http://127.0.0.1:{local_port}"
                logging.info(f"Dev Mode ready. Tunneled to Router at {self.base_url}")
                await asyncio.sleep(0.5)
                return
            except (ConnectionRefusedError, asyncio.TimeoutError, OSError):
                await asyncio.sleep(0.5)

        await self.__aexit__(None, None, None)
        raise TimeoutError("Failed to establish tunnel to Router Service.")

    @trace_span("wait_for_gateway")
    async def _wait_for_gateway_ip(self) -> None:
        """Waits for the Gateway to be assigned an external IP."""
        await self._load_config()
        custom_objects_api = self.custom_objects_api
        if custom_objects_api is None:
            raise RuntimeError("Kubernetes client is not initialized.")
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.gateway.name", self.gateway_name)
            span.set_attribute("sandbox.gateway.namespace", self.gateway_namespace)

        # Check if we already have a manually provided URL
        if self.base_url:
            logging.info(f"Using configured API URL: {self.base_url}")
            return

        logging.info(
            f"Waiting for Gateway '{self.gateway_name}' in namespace '{self.gateway_namespace}'..."
        )

        watcher = k8s_watch.Watch()
        async for event in watcher.stream(
            func=custom_objects_api.list_namespaced_custom_object,
            namespace=self.gateway_namespace,
            group=GATEWAY_API_GROUP,
            version=GATEWAY_API_VERSION,
            plural=GATEWAY_PLURAL,
            field_selector=f"metadata.name={self.gateway_name}",
            timeout_seconds=self.gateway_ready_timeout,
        ):
            if event["type"] in ["ADDED", "MODIFIED"]:
                gateway_object = event["object"]
                if self._set_base_url_from_gateway(gateway_object):
                    watcher.stop()
                    return

        if not self.base_url:
            raise TimeoutError(
                f"Gateway '{self.gateway_name}' in namespace '{self.gateway_namespace}' did not get"
                f" an IP within {self.gateway_ready_timeout} seconds."
            )

    async def __aenter__(self) -> "AsyncSandboxClient":
        trace_context_str = self._start_lifecycle_span()

        if self.http_client is None:
            self.http_client = httpx.AsyncClient()

        await self._create_claim(trace_context_str)
        await self._wait_for_sandbox_ready()

        # STRATEGY SELECTION
        if self.base_url:
            # Case 1: API URL provided manually (DNS / Internal) -> Do nothing, just use it.
            logging.info(f"Using configured API URL: {self.base_url}")

        elif self.gateway_name:
            # Case 2: Gateway Name provided -> Production Mode (Discovery)
            await self._wait_for_gateway_ip()

        else:
            # Case 3: No Gateway, No URL -> Developer Mode (Port Forward to Router)
            await self._start_and_wait_for_port_forward()

        return self

    async def __aexit__(
        self,
        exc_type: type[Exception] | None,
        exc_val: Exception | None,
        exc_tb: TracebackType | None,
    ) -> None:
        # Cleanup Port Forward if it exists
        port_forward_process = self.port_forward_process
        if port_forward_process:
            try:
                logging.info("Stopping port-forwarding...")
                port_forward_process.terminate()
                try:
                    await asyncio.wait_for(
                        port_forward_process.wait(),
                        timeout=2,
                    )
                except asyncio.TimeoutError:
                    port_forward_process.kill()
            # Unlikely to fail, but catch just in case.
            except Exception as exc:
                logging.error(f"Failed to stop port-forwarding: {exc}")

        # Delete the SandboxClaim
        custom_objects_api = self.custom_objects_api
        if self.claim_name and custom_objects_api:
            logging.info(f"Deleting SandboxClaim: {self.claim_name}")
            try:
                await custom_objects_api.delete_namespaced_custom_object(
                    group=CLAIM_API_GROUP,
                    version=CLAIM_API_VERSION,
                    namespace=self.namespace,
                    plural=CLAIM_PLURAL_NAME,
                    name=self.claim_name,
                )
            except k8s_client.ApiException as exc:
                if exc.status != 404:
                    logging.error(f"Error deleting sandbox claim: {exc}", exc_info=True)
            except Exception as exc:
                logging.error(
                    f"Unexpected error deleting sandbox claim: {exc}",
                    exc_info=True,
                )

        if self.http_client:
            await self.http_client.aclose()

        # Cleanup Trace if it exists
        self._end_lifecycle_span()

    async def _request(
        self, method: str, endpoint: str, **kwargs: Any
    ) -> httpx.Response:
        if not self.is_ready():
            raise RuntimeError("Sandbox is not ready for communication.")

        # Check if port-forward died silently
        port_forward_process = self.port_forward_process
        if port_forward_process and port_forward_process.returncode is not None:
            _, stderr = await port_forward_process.communicate()
            raise RuntimeError(
                f"Kubectl Port-Forward crashed BEFORE request!\n"
                f"Stderr: {stderr.decode(errors='ignore')}"
            )

        http_client = self.http_client
        if http_client is None:
            http_client = httpx.AsyncClient()
            self.http_client = http_client

        url = self._build_url(endpoint)
        kwargs["headers"] = self._build_request_headers(kwargs.get("headers"))

        try:
            response = await http_client.request(method, url, **kwargs)
            response.raise_for_status()
            return response
        except httpx.HTTPError as exc:
            # Check if port-forward died DURING request
            if port_forward_process and port_forward_process.returncode is not None:
                _, stderr = await port_forward_process.communicate()
                raise RuntimeError(
                    f"Kubectl Port-Forward crashed DURING request!\n"
                    f"Stderr: {stderr.decode(errors='ignore')}"
                ) from exc

            logging.error(f"Request to gateway router failed: {exc}")
            raise RuntimeError(
                f"Failed to communicate with the sandbox via the gateway at {url}."
            ) from exc

    @trace_span("run")
    async def run(self, command: str, timeout: int = 60) -> ExecutionResult:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.command", command)

        payload = {"command": command}
        response = await self._request("POST", "execute", json=payload, timeout=timeout)

        response_data = response.json()
        result = ExecutionResult(
            stdout=response_data.get("stdout", ""),
            stderr=response_data.get("stderr", ""),
            exit_code=response_data.get("exit_code", -1),
        )

        if span.is_recording():
            span.set_attribute("sandbox.exit_code", result.exit_code)
        return result

    @trace_span("write")
    async def write(self, path: str, content: bytes | str, timeout: int = 60) -> None:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
            span.set_attribute("sandbox.file.size", len(content))

        if isinstance(content, str):
            content = content.encode("utf-8")

        filename = os.path.basename(path)
        files_payload = {"file": (filename, content)}
        await self._request("POST", "upload", files=files_payload, timeout=timeout)
        logging.info(f"File '{filename}' uploaded successfully.")

    @trace_span("read")
    async def read(self, path: str, timeout: int = 60) -> bytes:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        response = await self._request("GET", f"download/{path}", timeout=timeout)
        content = response.content

        if span.is_recording():
            span.set_attribute("sandbox.file.size", len(content))

        return content
