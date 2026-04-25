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
import time
from typing import List
from kubernetes import client, config, watch
from .exceptions import SandboxMetadataError, SandboxNotFoundError, SandboxTemplateNotFoundError
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

class K8sHelper:
    """Helper class for Kubernetes API interactions."""

    def __init__(self):
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
        self.custom_objects_api = client.CustomObjectsApi()
        self.core_v1_api = client.CoreV1Api()

    def create_sandbox_claim(self, name: str, template: str, namespace: str, annotations: dict | None = None, labels: dict | None = None, lifecycle: dict | None = None):
        """Creates a SandboxClaim custom resource."""
        metadata = {
            "name": name,
            "annotations": annotations or {},
        }
        if labels:
            metadata["labels"] = labels

        spec = {
            "sandboxTemplateRef": {
                "name": template
            }
        }
        if lifecycle:
            spec["lifecycle"] = lifecycle

        manifest = {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": metadata,
            "spec": spec,
        }
        logging.info(f"Creating SandboxClaim '{name}' in namespace '{namespace}' using template '{template}'...")
        self.custom_objects_api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest
        )
    
    def resolve_sandbox_name(self, claim_name: str, namespace: str, timeout: int) -> str:
        """Resolves the actual Sandbox name from the SandboxClaim status.
        With warm pool adoption, the sandbox name may differ from the claim
        name. This method watches the SandboxClaim until the sandbox name
        appears in the claim's status, then returns it.
        """
        deadline = time.monotonic() + timeout
        logging.info(f"Resolving sandbox name from claim '{claim_name}'...")
        while True:
            remaining = int(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(
                    f"Could not resolve sandbox name from claim "
                    f"'{claim_name}' within {timeout} seconds.")
            w = watch.Watch()
            for event in w.stream(
                func=self.custom_objects_api.list_namespaced_custom_object,
                namespace=namespace,
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                plural=CLAIM_PLURAL_NAME,
                field_selector=f"metadata.name={claim_name}",
                timeout_seconds=remaining
            ):
                if event is None:
                    continue
                if event["type"] == "DELETED":
                    w.stop()
                    raise SandboxMetadataError(
                        f"SandboxClaim '{claim_name}' was deleted while resolving sandbox name")
                if event["type"] in ["ADDED", "MODIFIED"]:
                    claim_object = event['object']
                    status = claim_object.get('status') or {}
                    
                    for cond in status.get('conditions', []):
                        if (
                            cond.get('type') == 'Ready'
                            and cond.get('status') == 'False'
                            and cond.get('reason') == 'TemplateNotFound'
                        ):
                            w.stop()
                            raise SandboxTemplateNotFoundError(
                                f"SandboxTemplate requested does not exist: {cond.get('message', 'Template not found')}"
                            )

                    sandbox_status = status.get('sandbox', {})
                    # Support both 'name' (standard) and 'Name' (legacy, before CRD rename in #440)
                    name = sandbox_status.get('name', '') or sandbox_status.get('Name', '')
                    if name:
                        logging.info(
                            f"Resolved sandbox name '{name}' from claim status")
                        w.stop()
                        return name

    def wait_for_sandbox_ready(self, name: str, namespace: str, timeout: int) -> str | None:
        """Waits for the Sandbox custom resource to have a 'Ready' status.

        Returns the first pod IP from the sandbox status when ready, or None if
        no IPs are present (e.g. on older controllers that don't populate podIPs).
        """
        deadline = time.monotonic() + timeout
        logging.info(f"Watching for Sandbox {name} to become ready...")
        while True:
            remaining = int(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(f"Sandbox {name} did not become ready within {timeout} seconds.")
            w = watch.Watch()
            for event in w.stream(
                func=self.custom_objects_api.list_namespaced_custom_object,
                namespace=namespace,
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                plural=SANDBOX_PLURAL_NAME,
                field_selector=f"metadata.name={name}",
                timeout_seconds=remaining
            ):
                if event is None:
                    continue
                if event["type"] in ["ADDED", "MODIFIED"]:
                    sandbox_object = event['object']
                    status = sandbox_object.get('status') or {}
                    conditions = status.get('conditions', [])
                    for cond in conditions:
                        if cond.get('type') == 'Ready' and cond.get('status') == 'True':
                            logging.info(f"Sandbox {name} is ready.")
                            w.stop()
                            pod_ips = status.get('podIPs', [])
                            return pod_ips[0] if pod_ips else None
                elif event["type"] == "DELETED":
                    logging.error(f"Sandbox {name} was deleted before becoming ready.")
                    w.stop()
                    raise SandboxNotFoundError(f"Sandbox {name} was deleted before becoming ready.")

    def delete_sandbox_claim(self, name: str, namespace: str):
        """Deletes a SandboxClaim custom resource."""
        try:
            self.custom_objects_api.delete_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
                name=name
            )
            logging.info(f"Terminated SandboxClaim: {name}")
        except client.ApiException as e:
            if e.status != 404:
                logging.error(f"Error terminating sandbox {name}: {e}")
                raise

    def get_sandbox(self, name: str, namespace: str):
        """Gets a Sandbox custom resource."""
        try:
            return self.custom_objects_api.get_namespaced_custom_object(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=name
            )
        except client.ApiException as e:
            if e.status == 404:
                return None
            raise

    def list_sandbox_claims(self, namespace: str) -> List[str]:
        """Lists all SandboxClaim custom resources in a namespace."""
        try:
            response = self.custom_objects_api.list_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME
            )
            return [
                item.get("metadata", {}).get("name") 
                for item in response.get("items", []) 
                if item.get("metadata", {}).get("name")
            ]
        except client.ApiException as e:
            logging.error(f"Error listing sandbox claims in namespace {namespace}: {e}")
            raise

    def wait_for_gateway_ip(self, gateway_name: str, namespace: str, timeout: int) -> str:
        """Waits for the Gateway to be assigned an external IP."""
        deadline = time.monotonic() + timeout
        logging.info(f"Waiting for Gateway '{gateway_name}' in namespace '{namespace}'...")
        while True:
            remaining = int(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(f"Gateway '{gateway_name}' did not get an IP.")
            w = watch.Watch()
            for event in w.stream(
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
                    gateway_object = event['object']
                    status = gateway_object.get('status') or {}
                    addresses = status.get('addresses', [])
                    if addresses:
                        ip_address = addresses[0].get('value')
                        if ip_address:
                            logging.info(f"Gateway ready. IP: {ip_address}")
                            w.stop()
                            return ip_address
