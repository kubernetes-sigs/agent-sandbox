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

import os
import uuid
import functools
import time
from typing import Optional, Dict
from test.e2e.clients.python.framework.predicates import (
    deployment_ready,
    pod_ready,
)

import kubernetes
import yaml


DEFAULT_KUBECONFIG_PATH = "bin/KUBECONFIG"


class TestContext:
    """Context for E2E tests, managing Kubernetes interactions"""

    def __init__(self, kubeconfig_path: Optional[str] = None):
        self.kubeconfig_path = kubeconfig_path or os.environ.get(
            "KUBECONFIG", DEFAULT_KUBECONFIG_PATH
        )
        self._api_client = None
        self.namespace = None

    def get_api_client(self):
        """Returns a Kubernetes API client"""
        if not self._api_client:
            self._api_client = kubernetes.config.new_client_from_config(
                self.kubeconfig_path
            )
        return self._api_client

    def get_core_v1_api(self):
        """Returns the CoreV1Api client"""
        return kubernetes.client.CoreV1Api(self.get_api_client())

    def get_apps_v1_api(self):
        """Returns the AppsV1Api client"""
        return kubernetes.client.AppsV1Api(self.get_api_client())

    def get_custom_objects_api(self):
        """Returns the CustomObjectsApi client"""
        return kubernetes.client.CustomObjectsApi(self.get_api_client())

    def create_temp_namespace(self, prefix="test-"):
        """Creates a temporary namespace for testing"""
        core_v1 = self.get_core_v1_api()
        namespace_name = f"{prefix}{uuid.uuid4().hex[:8]}"
        namespace_manifest = {
            "apiVersion": "v1",
            "kind": "Namespace",
            "metadata": {"name": namespace_name},
        }
        core_v1.create_namespace(body=namespace_manifest)
        self.namespace = namespace_name
        print(f"Created namespace: {self.namespace}")
        return self.namespace

    def delete_namespace(self, namespace: Optional[str] = None):
        """Deletes the specified namespace"""
        if namespace is None:
            namespace = self.namespace
        if namespace:
            core_v1 = self.get_core_v1_api()
            try:
                core_v1.delete_namespace(name=namespace)
                print(f"Deleted namespace: {namespace}")
                if self.namespace == namespace:
                    self.namespace = None
            except kubernetes.client.rest.ApiException as e:
                if e.status == 404:
                    print(f"Namespace {namespace} not found, skipping deletion.")
                else:
                    raise

    def apply_manifest_text(self, manifest_text: str, namespace: Optional[str] = None):
        """Applies the given manifest text to the cluster"""
        if namespace is None:
            namespace = self.namespace
        if not namespace:
            raise ValueError(
                "Namespace must be provided or created before applying manifests."
            )

        manifests = yaml.safe_load_all(manifest_text)
        for manifest in manifests:
            if not manifest:
                continue
            self._apply_single_manifest(manifest, namespace)

    def _apply_single_manifest(self, manifest: Dict, namespace: str):
        api_version = manifest.get("apiVersion")
        kind = manifest.get("kind")
        metadata = manifest.get("metadata", {})
        manifest_name = metadata.get("name")

        # Set namespace if not present
        if "namespace" not in metadata:
            manifest["metadata"]["namespace"] = namespace

        print(f"Applying {kind} '{manifest_name}' in namespace '{namespace}'")

        if api_version == "v1":
            core_v1 = self.get_core_v1_api()
            if kind == "Service":
                try:
                    core_v1.create_namespaced_service(
                        namespace=namespace, body=manifest
                    )
                except kubernetes.client.rest.ApiException as e:
                    if e.status == 409:  # Conflict
                        print(f"Service {manifest_name} already exists. Patching...")
                        core_v1.patch_namespaced_service(
                            name=manifest_name, namespace=namespace, body=manifest
                        )
                    else:
                        raise
        elif api_version == "apps/v1":
            apps_v1 = self.get_apps_v1_api()
            if kind == "Deployment":
                try:
                    apps_v1.create_namespaced_deployment(
                        namespace=namespace, body=manifest
                    )
                except kubernetes.client.rest.ApiException as e:
                    if e.status == 409:  # Conflict
                        print(f"Deployment {manifest_name} already exists. Patching...")
                        apps_v1.patch_namespaced_deployment(
                            name=manifest_name, namespace=namespace, body=manifest
                        )
                    else:
                        raise
        else:
            custom_objects_api = self.get_custom_objects_api()
            group, version = api_version.split("/")
            plural = self._get_plural_name(kind)  # Helper needed for this

            try:
                custom_objects_api.create_namespaced_custom_object(
                    group=group,
                    version=version,
                    namespace=namespace,
                    plural=plural,
                    body=manifest,
                )
            except kubernetes.client.rest.ApiException as e:
                if e.status == 409:  # Conflict
                    print(f"{kind} {manifest_name} already exists. Replacing...")
                    # Need to get the resource version for replace
                    try:
                        existing = custom_objects_api.get_namespaced_custom_object(
                            group=group,
                            version=version,
                            namespace=namespace,
                            plural=plural,
                            name=manifest_name,
                        )
                        manifest["metadata"]["resourceVersion"] = existing["metadata"][
                            "resourceVersion"
                        ]
                        custom_objects_api.replace_namespaced_custom_object(
                            group=group,
                            version=version,
                            namespace=namespace,
                            plural=plural,
                            name=manifest_name,
                            body=manifest,
                        )
                    except kubernetes.client.rest.ApiException as get_e:
                        print(f"Error getting existing object for replace: {get_e}")
                        raise
                else:
                    raise

    def _get_plural_name(self, kind: str) -> str:
        # Basic pluralization, may need refinement
        if kind == "SandboxWarmPool":
            return "sandboxwarmpools"
        if kind == "SandboxClaim":
            return "sandboxclaims"
        if kind == "SandboxTemplate":
            return "sandboxtemplates"
        if kind == "Sandbox":
            return "sandboxes"
        if kind.endswith("s"):
            return kind.lower() + "es"
        return kind.lower() + "s"

    def wait_for_object(
        self, watch_func, name: str, namespace: str, predicate_func, timeout=120
    ):
        """Waits for a Kubernetes object to satisfy a given predicate function"""
        w = kubernetes.watch.Watch()
        try:
            for event in w.stream(
                watch_func,
                namespace=namespace,
                field_selector=f"metadata.name={name}",
                timeout_seconds=timeout,
            ):
                obj = event["object"]
                if predicate_func(obj):
                    print(
                        f"Object {name} satisfied predicate on event type {event['type']}."
                    )
                    w.stop()
                    return True
            # Fallthrough means timeout
            raise TimeoutError(
                f"Object {name} did not satisfy predicate within {timeout} seconds."
            )
        except Exception as e:
            print(f"Error during watch: {e}")
            # Check if the error is due to a timeout in the stream
            if "timeout" in str(e).lower():
                raise TimeoutError(
                    f"Object {name} did not satisfy predicate within {timeout} seconds."
                )
            raise

    def wait_for_deployment_ready(
        self,
        name: str,
        namespace: Optional[str] = None,
        min_ready: int = 1,
        timeout=120,
    ):
        """Waits for a Deployment to have at least min_ready available replicas"""
        if namespace is None:
            namespace = self.namespace
        if not namespace:
            raise ValueError("Namespace must be provided.")

        apps_v1 = self.get_apps_v1_api()

        return self.wait_for_object(
            apps_v1.list_namespaced_deployment,
            name,
            namespace,
            deployment_ready(min_ready),
            timeout,
        )

    def wait_for_warmpool_ready(
        self,
        name: str,
        namespace: Optional[str] = None,
        min_ready: int = 1,
        timeout=120,
    ):
        """Waits for a SandboxWarmPool to have at least min_ready ready sandboxes"""
        if namespace is None:
            namespace = self.namespace
        if not namespace:
            raise ValueError("Namespace must be provided.")

        custom_objects_api = self.get_custom_objects_api()
        core_v1 = self.get_core_v1_api()
        is_pod_ready = pod_ready()

        try:
            warmpool = custom_objects_api.get_namespaced_custom_object(
                group="extensions.agents.x-k8s.io",
                version="v1alpha1",
                namespace=namespace,
                plural="sandboxwarmpools",
                name=name,
            )
            warmpool_uid = warmpool["metadata"]["uid"]
        except kubernetes.client.rest.ApiException as e:
            print(f"Error fetching SandboxWarmPool {name}: {e}")
            raise

        w = kubernetes.watch.Watch()
        ready_pods = set()
        try:
            for event in w.stream(
                core_v1.list_namespaced_pod,
                namespace=namespace,
                timeout_seconds=timeout,
            ):
                pod = event["object"]
                pod_name = pod.metadata.name

                owner_references = pod.metadata.owner_references or []
                is_owned = False
                for owner_ref in owner_references:
                    if (
                        owner_ref.kind == "SandboxWarmPool"
                        and owner_ref.uid == warmpool_uid
                    ):
                        is_owned = True
                        break

                if not is_owned:
                    continue

                event_type = event["type"]
                if event_type == "DELETED":
                    if pod_name in ready_pods:
                        ready_pods.remove(pod_name)
                    continue

                if is_pod_ready(pod):
                    ready_pods.add(pod_name)
                elif pod_name in ready_pods:
                    ready_pods.remove(pod_name)

                print(
                    f"WarmPool {name}: {len(ready_pods)}/{min_ready} pods ready. Current ready set: {ready_pods}"
                )
                if len(ready_pods) >= min_ready:
                    print(
                        f"SandboxWarmPool {name} is ready with {len(ready_pods)} pods."
                    )
                    w.stop()
                    return True

            # Fallthrough means timeout
            raise TimeoutError(
                f"SandboxWarmPool {name} did not become ready within {timeout} seconds."
            )
        except Exception as e:
            print(f"Error during watch: {e}")
            if "timeout" in str(e).lower():
                raise TimeoutError(
                    f"SandboxWarmPool {name} did not become ready within {timeout} seconds."
                )
            raise


if __name__ == "__main__":
    # Example Usage
    tc = None
    try:
        tc = TestContext()
        ns = tc.create_temp_namespace()

        print("TestContext example finished.")
    except Exception as e:
        print(f"An error occurred: {e}")
    finally:
        if tc and tc.namespace:
            print(f"Cleaning up namespace: {tc.namespace}")
            # tc.delete_namespace()
