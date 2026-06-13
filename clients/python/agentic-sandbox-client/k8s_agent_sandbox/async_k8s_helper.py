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
import time

from kubernetes_asyncio import client, config, watch

logger = logging.getLogger(__name__)

from .constants import (
    CLAIM_API_GROUP,
    CLAIM_API_VERSION,
    CLAIM_PLURAL_NAME,
    GATEWAY_API_GROUP,
    GATEWAY_API_VERSION,
    GATEWAY_PLURAL,
    SANDBOX_API_GROUP,
    SANDBOX_API_VERSION,
    SANDBOX_PLURAL_NAME,
)
from .exceptions import SandboxMetadataError, SandboxNotFoundError, SandboxTemplateNotFoundError


class AsyncK8sHelper:
    """Async helper class for Kubernetes API interactions using kubernetes_asyncio."""

    def __init__(self):
        self._initialized = False
        self._init_lock = asyncio.Lock()
        self._api_client: client.ApiClient | None = None

    async def _ensure_initialized(self):
        if self._initialized:
            return
        async with self._init_lock:
            if self._initialized:
                return
            try:
                config.load_incluster_config()
            except config.ConfigException:
                await config.load_kube_config()
            self._api_client = client.ApiClient()
            self.custom_objects_api = client.CustomObjectsApi(self._api_client)
            self.core_v1_api = client.CoreV1Api(self._api_client)
            self._initialized = True

    async def create_sandbox_claim(
        self,
        name: str,
        template: str,
        namespace: str,
        annotations: dict | None = None,
        labels: dict | None = None,
        lifecycle: dict | None = None,
        warmpool: str | None = None,
    ):
        """Creates a SandboxClaim custom resource."""
        await self._ensure_initialized()

        metadata = {
            "name": name,
            "annotations": annotations or {},
        }
        if labels:
            metadata["labels"] = labels

        spec = {
            "sandboxTemplateRef": {
                "name": template,
            }
        }
        if lifecycle:
            spec["lifecycle"] = lifecycle
        if warmpool:
            spec["warmpool"] = warmpool

        manifest = {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": metadata,
            "spec": spec,
        }
        logger.info(
            f"Creating SandboxClaim '{name}' in namespace '{namespace}' using template '{template}'..."
        )
        await self.custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest,
        )

    async def resolve_sandbox_name(self, claim_name: str, namespace: str, timeout: int) -> str:
        """Resolves the actual Sandbox name from the SandboxClaim status.
        With warm pool adoption, the sandbox name may differ from the claim
        name. This method watches the SandboxClaim until the sandbox name
        appears in the claim's status, then returns it.
        """
        await self._ensure_initialized()

        deadline = time.monotonic() + timeout
        logger.info(f"Resolving sandbox name from claim '{claim_name}'...")
        while True:
            remaining = int(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(
                    f"Could not resolve sandbox name from claim "
                    f"'{claim_name}' within {timeout} seconds."
                )
            w = watch.Watch()
            try:
                async for event in w.stream(
                    func=self.custom_objects_api.list_namespaced_custom_object,
                    namespace=namespace,
                    group=CLAIM_API_GROUP,
                    version=CLAIM_API_VERSION,
                    plural=CLAIM_PLURAL_NAME,
                    field_selector=f"metadata.name={claim_name}",
                    timeout_seconds=remaining,
                ):
                    if event is None:
                        continue
                    if event["type"] == "DELETED":
                        raise SandboxMetadataError(
                            f"SandboxClaim '{claim_name}' was deleted while resolving sandbox name"
                        )
                    if event["type"] in ["ADDED", "MODIFIED"]:
                        claim_object = event["object"]
                        status = claim_object.get("status") or {}
                        
                        for cond in status.get("conditions", []):
                            if (
                                cond.get("type") == "Ready"
                                and cond.get("status") == "False"
                                and cond.get("reason") == "TemplateNotFound"
                            ):
                                raise SandboxTemplateNotFoundError(
                                    f"SandboxTemplate requested does not exist: {cond.get('message', 'Template not found')}"
                                )

                        sandbox_status = status.get("sandbox", {})
                        # Support both 'name' (standard) and 'Name' (legacy, before CRD rename in #440)
                        name = sandbox_status.get("name", "") or sandbox_status.get("Name", "")
                        if name:
                            logger.info(f"Resolved sandbox name '{name}' from claim status")
                            return name
            finally:
                await w.close()

    async def wait_for_sandbox_ready(self, name: str, namespace: str, timeout: int) -> str | None:
        """Waits for the Sandbox custom resource to have a 'Ready' status.

        Returns the first pod IP from the sandbox status when ready, or None if
        no IPs are present (e.g. on older controllers that don't populate podIPs).
        """
        await self._ensure_initialized()

        deadline = time.monotonic() + timeout
        logger.info(f"Watching for Sandbox {name} to become ready...")
        while True:
            remaining = int(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(f"Sandbox {name} did not become ready within {timeout} seconds.")
            w = watch.Watch()
            try:
                async for event in w.stream(
                    func=self.custom_objects_api.list_namespaced_custom_object,
                    namespace=namespace,
                    group=SANDBOX_API_GROUP,
                    version=SANDBOX_API_VERSION,
                    plural=SANDBOX_PLURAL_NAME,
                    field_selector=f"metadata.name={name}",
                    timeout_seconds=remaining,
                ):
                    if event is None:
                        continue
                    if event["type"] in ["ADDED", "MODIFIED"]:
                        sandbox_object = event["object"]
                        status = sandbox_object.get("status") or {}
                        conditions = status.get("conditions", [])
                        for cond in conditions:
                            if cond.get("type") == "Ready" and cond.get("status") == "True":
                                logger.info(f"Sandbox {name} is ready.")
                                pod_ips = status.get("podIPs", [])
                                return pod_ips[0] if pod_ips else None
                    elif event["type"] == "DELETED":
                        logger.error(f"Sandbox {name} was deleted before becoming ready.")
                        raise SandboxNotFoundError(
                            f"Sandbox {name} was deleted before becoming ready."
                        )
            finally:
                await w.close()

    async def delete_sandbox_claim(self, name: str, namespace: str):
        """Deletes a SandboxClaim custom resource."""
        await self._ensure_initialized()

        try:
            await self.custom_objects_api.delete_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
                name=name,
            )
            logger.info(f"Terminated SandboxClaim: {name}")
        except client.ApiException as e:
            if e.status != 404:
                logger.error(f"Error terminating sandbox {name}: {e}")
                raise

    async def get_sandbox(self, name: str, namespace: str):
        """Gets a Sandbox custom resource."""
        await self._ensure_initialized()

        try:
            return await self.custom_objects_api.get_namespaced_custom_object(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=name,
            )
        except client.ApiException as e:
            if e.status == 404:
                return None
            raise

    async def get_sandbox_claim(self, name: str, namespace: str):
        """Gets a SandboxClaim custom resource (or ``None`` if it doesn't exist)."""
        await self._ensure_initialized()

        try:
            return await self.custom_objects_api.get_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
                name=name,
            )
        except client.ApiException as e:
            if e.status == 404:
                return None
            raise

    async def list_sandbox_claims(self, namespace: str, label_selector: str | None = None) -> list[str]:
        """Lists all SandboxClaim custom resources in a namespace.

        Args:
            namespace: Kubernetes namespace to list claims in.
            label_selector: Optional Kubernetes label selector string
                (e.g. ``"app=myapp,env=prod"``). When set, only claims
                matching the selector are returned.
        """
        await self._ensure_initialized()

        try:
            kwargs: dict = dict(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
            )
            if label_selector is not None:
                kwargs["label_selector"] = label_selector
            response = await self.custom_objects_api.list_namespaced_custom_object(**kwargs)
            return [
                item.get("metadata", {}).get("name")
                for item in response.get("items", [])
                if item.get("metadata", {}).get("name")
            ]
        except client.ApiException as e:
            logger.error(f"Error listing sandbox claims in namespace {namespace}: {e}")
            raise

    async def wait_for_gateway_ip(self, gateway_name: str, namespace: str, timeout: int) -> str:
        """Waits for the Gateway to be assigned an external IP."""
        await self._ensure_initialized()

        deadline = time.monotonic() + timeout
        logger.info(f"Waiting for Gateway '{gateway_name}' in namespace '{namespace}'...")
        while True:
            remaining = int(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(f"Gateway '{gateway_name}' did not get an IP.")
            w = watch.Watch()
            try:
                async for event in w.stream(
                    func=self.custom_objects_api.list_namespaced_custom_object,
                    namespace=namespace,
                    group=GATEWAY_API_GROUP,
                    version=GATEWAY_API_VERSION,
                    plural=GATEWAY_PLURAL,
                    field_selector=f"metadata.name={gateway_name}",
                    timeout_seconds=remaining,
                ):
                    if event is None:
                        continue
                    if event["type"] in ["ADDED", "MODIFIED"]:
                        gateway_object = event["object"]
                        status = gateway_object.get("status") or {}
                        addresses = status.get("addresses", [])
                        if addresses:
                            ip_address = addresses[0].get("value")
                            if ip_address:
                                logger.info(f"Gateway ready. IP: {ip_address}")
                                return ip_address
            finally:
                await w.close()

    async def close(self):
        """Closes the shared Kubernetes API client session."""
        if self._api_client:
            await self._api_client.close()
            self._api_client = None
            self._initialized = False
