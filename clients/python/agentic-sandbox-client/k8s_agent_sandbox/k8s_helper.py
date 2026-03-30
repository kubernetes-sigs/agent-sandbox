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
from typing import List
from kubernetes import client, config, watch

# Constants for API Groups and Resources
CLAIM_API_GROUP = "extensions.agents.x-k8s.io"
CLAIM_API_VERSION = "v1alpha1"
CLAIM_PLURAL_NAME = "sandboxclaims"

SANDBOX_API_GROUP = "agents.x-k8s.io"
SANDBOX_API_VERSION = "v1alpha1"
SANDBOX_PLURAL_NAME = "sandboxes"

GATEWAY_API_GROUP = "gateway.networking.k8s.io"
GATEWAY_API_VERSION = "v1"
GATEWAY_PLURAL = "gateways"

class K8sHelper:
    """Helper class for Kubernetes API interactions."""

    def __init__(self):
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
        self.custom_objects_api = client.CustomObjectsApi()
        self.core_v1_api = client.CoreV1Api()

    def create_sandbox_claim(self, name: str, template: str, namespace: str, annotations: dict | None = None):
        """Creates a SandboxClaim custom resource."""
        manifest = {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": {
                "name": name,
                "annotations": annotations or {}
            },
            "spec": {
                "sandboxTemplateRef": {
                    "name": template
                }
            }
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
        w = watch.Watch()
        logging.info(f"Resolving sandbox name from claim '{claim_name}'...")
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=namespace,
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            plural=CLAIM_PLURAL_NAME,
            field_selector=f"metadata.name={claim_name}",
            timeout_seconds=timeout
        ):
            if event["type"] == "DELETED":
                w.stop()
                raise RuntimeError(
                    f"SandboxClaim '{claim_name}' was deleted while resolving sandbox name")
            if event["type"] in ["ADDED", "MODIFIED"]:
                claim_object = event['object']
                sandbox_status = claim_object.get(
                    'status', {}).get('sandbox', {})
                name = sandbox_status.get('name', '')
                if name:
                    logging.info(
                        f"Resolved sandbox name '{name}' from claim status")
                    w.stop()
                    return name
        raise TimeoutError(
            f"Could not resolve sandbox name from claim "
            f"'{claim_name}' within {timeout} seconds.")

    def wait_for_sandbox_ready(self, name: str, namespace: str, timeout: int):
        """Waits for the Sandbox custom resource to have a 'Ready' status."""
        logging.info(f"Watching for Sandbox {name} to become ready...")
        w = watch.Watch()
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=namespace,
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            plural=SANDBOX_PLURAL_NAME,
            field_selector=f"metadata.name={name}",
            timeout_seconds=timeout
        ):
            if event is None:
                continue
            if event["type"] in ["ADDED", "MODIFIED"]:
                sandbox_object = event['object']
                status = sandbox_object.get('status', {})
                conditions = status.get('conditions', [])
                for cond in conditions:
                    if cond.get('type') == 'Ready' and cond.get('status') == 'True':
                        logging.info(f"Sandbox {name} is ready.")
                        w.stop()
                        return
            elif event["type"] == "DELETED":
                logging.error(f"Sandbox {name} was deleted before becoming ready.")
                w.stop()
                raise RuntimeError(f"Sandbox {name} was deleted before becoming ready.")
        raise TimeoutError(f"Sandbox {name} did not become ready within {timeout} seconds.")

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
        logging.info(f"Waiting for Gateway '{gateway_name}' in namespace '{namespace}'...")
        w = watch.Watch()
        for event in w.stream(
            func=self.custom_objects_api.list_namespaced_custom_object,
            namespace=namespace,
            group=GATEWAY_API_GROUP,
            version=GATEWAY_API_VERSION,
            plural=GATEWAY_PLURAL,
            field_selector=f"metadata.name={gateway_name}",
            timeout_seconds=timeout,
        ):
            if event is None:
                continue
            if event["type"] in ["ADDED", "MODIFIED"]:
                gateway_object = event['object']
                status = gateway_object.get('status', {})
                addresses = status.get('addresses', [])
                if addresses:
                    ip_address = addresses[0].get('value')
                    if ip_address:
                        logging.info(f"Gateway ready. IP: {ip_address}")
                        w.stop()
                        return ip_address
        raise TimeoutError(f"Gateway '{gateway_name}' did not get an IP.")
