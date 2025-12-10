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
from typing import Optional
from test.e2e.clients.python.framework.predicates import (
    deployment_ready,
    warmpool_ready,
)
import subprocess
from urllib3.exceptions import ReadTimeoutError
import kubernetes


DEFAULT_KUBECONFIG_PATH = "bin/KUBECONFIG"
DEFAULT_TIMEOUT_SECONDS = 120


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
        """Applies the given manifest text to the cluster using kubectl."""
        if namespace is None:
            namespace = self.namespace
        if not namespace:
            raise ValueError(
                "Namespace must be provided or created before applying manifests."
            )

        cmd = ["kubectl", "apply", "-f", "-", "-n", namespace]
        try:
            result = subprocess.run(
                cmd, input=manifest_text, text=True, capture_output=True, check=True
            )
            print(result.stdout)
            if result.stderr:
                print(result.stderr)
        except subprocess.CalledProcessError as e:
            error_msg = e.stderr.strip()
            print(f"kubectl apply failed with exit code {e.returncode}")
            print(f"stdout: {e.stdout}")
            print(f"stderr: {error_msg}")
            raise RuntimeError(f"Failed to apply manifest: {error_msg}") from e

    def wait_for_object(
        self,
        watch_func,
        name: str,
        namespace: str,
        predicate_func,
        timeout=DEFAULT_TIMEOUT_SECONDS,
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
        except ReadTimeoutError:
            raise TimeoutError(
                f"Object {name} did not satisfy predicate within {timeout} seconds."
            )
        except Exception as e:
            print(f"Error during watch: {e}")
            raise

    def wait_for_deployment_ready(
        self,
        name: str,
        namespace: Optional[str] = None,
        min_ready: int = 1,
        timeout=DEFAULT_TIMEOUT_SECONDS,
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
        timeout=DEFAULT_TIMEOUT_SECONDS,
    ):
        """Waits for a SandboxWarmPool to have at least min_ready ready sandboxes"""
        if namespace is None:
            namespace = self.namespace
        if not namespace:
            raise ValueError("Namespace must be provided.")

        custom_objects_api = self.get_custom_objects_api()

        return self.wait_for_object(
            functools.partial(
                custom_objects_api.list_namespaced_custom_object,
                group="extensions.agents.x-k8s.io",
                version="v1alpha1",
                plural="sandboxwarmpools",
            ),
            name,
            namespace,
            warmpool_ready(),
            timeout,
        )


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
