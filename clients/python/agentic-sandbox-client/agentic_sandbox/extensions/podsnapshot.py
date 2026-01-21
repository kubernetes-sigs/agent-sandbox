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

import logging
import sys
from kubernetes import client, config, watch
from kubernetes.client import ApiException
from ..sandbox_client import SandboxClient, ExecutionResult
from ..constants import *

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(levelname)s - %(message)s",
    stream=sys.stdout,
)


class PodSnapshotSandboxClient(SandboxClient):
    """
    A specialized Sandbox client for interacting with the snapshot controller.
    Handles the case only when triggerConfig is type manual.
    """

    def __init__(
        self,
        template_name: str,
        podsnapshot_timeout: int = 180,
        server_port: int = 8080,
        **kwargs,
    ):

        self.controller_ready = False
        self.podsnapshot_timeout = podsnapshot_timeout
        self.created_snapshots = []
        self.controller_ready = self.snapshot_controller_ready()

        super().__init__(
            template_name, server_port=server_port, **kwargs
        )

    def _wait_for_snapshot_processed(self, trigger_name: str):
        """
        Waits for the PodSnapshotManualTrigger to be processed and a snapshot created.
        """
        w = watch.Watch()
        logging.info(f"Waiting for snapshot manual trigger '{trigger_name}' to be processed...")

        try:
            for event in w.stream(
                func=self.custom_objects_api.list_namespaced_custom_object,
                namespace=self.namespace,
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                field_selector=f"metadata.name={trigger_name}",
                timeout_seconds=self.podsnapshot_timeout
            ):
                if event["type"] in ["ADDED", "MODIFIED"]:
                    obj = event["object"]
                    status = obj.get("status", {})
                    conditions = status.get("conditions", [])

                    for condition in conditions:
                        if (
                            condition.get("type") == "Triggered"
                            and condition.get("status") == "True"
                            and condition.get("reason") == "Complete"
                        ):
                            logging.info(f"Snapshot manual trigger '{trigger_name}' processed successfully. Created Snapshot UID: {status.get('snapshotCreated', {}).get('name')}")
                            w.stop()
                            return
        except Exception as e:
            logging.error(f"Error watching snapshot: {e}")
            raise

        raise TimeoutError(f"Snapshot manual trigger '{trigger_name}' was not processed within {self.podsnapshot_timeout} seconds.")

    def snapshot_controller_ready(self) -> bool:
        """
        Checks if the snapshot controller and agent pods are running.
        Checks both self-installed (gps-system) and GKE-managed pod snapshot systems.
        """

        if self.controller_ready:
            return True

        v1 = client.CoreV1Api()

        def check_namespace(namespace: str, required_components: list[str]) -> bool:
            try:
                pods = v1.list_namespaced_pod(namespace)
                found_components = {component: False for component in required_components}

                for pod in pods.items:
                    if pod.status.phase == "Running":
                        name = pod.metadata.name
                        for component in required_components:
                            if component in name:
                                found_components[component] = True

                return all(found_components.values())
            except ApiException:
                return False

        # Check self-installed: requires both controller and agent in gps-system
        if check_namespace(SNAPSHOT_NAMESPACE_SELF_INSTALLED, [SNAPSHOT_CONTROLLER_NAME, SNAPSHOT_AGENT]):
            self.controller_ready = True
            return True

        # Check managed: requires only agent in gke-managed-pod-snapshots
        if check_namespace(SNAPSHOT_NAMESPACE_MANAGED, [SNAPSHOT_AGENT]):
            self.controller_ready = True
            return True

        self.controller_ready = False
        return self.controller_ready

    def checkpoint(self, trigger_name: str) -> ExecutionResult:
        """
        Triggers a snapshot of the specified pod by creating a PodSnapshotManualTrigger resource.
        """
        if not self.controller_ready:
            return ExecutionResult(
                stdout="",
                stderr="Snapshot controller is not ready. Ensure it is installed and running.",
                exit_code=1
            )
        if not self.pod_name:
            return ExecutionResult(
                stdout="",
                stderr="Sandbox pod name not found. Ensure sandbox is created.",
                exit_code=1
            )

        manifest = {
            "apiVersion": f"{PODSNAPSHOT_API_GROUP}/{PODSNAPSHOT_API_VERSION}",
            "kind": "PodSnapshotManualTrigger",
            "metadata": {
                "name": trigger_name,
                "namespace": self.namespace
            },
            "spec": {
                "targetPod": self.pod_name
            }
        }

        try:
            self.custom_objects_api.create_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                body=manifest
            )
            self.created_snapshots.append(trigger_name)
            self._wait_for_snapshot_processed(trigger_name)
            return ExecutionResult(
                stdout=f"PodSnapshotManualTrigger '{trigger_name}' created successfully.",
                stderr="",
                exit_code=0
            )
        except ApiException as e:
            return ExecutionResult(
                stdout="",
                stderr=f"Failed to create PodSnapshotManualTrigger: {e}",
                exit_code=1
            )
        except TimeoutError as e:
             return ExecutionResult(
                stdout="",
                stderr=f"Snapshot creation timed out: {e}",
                exit_code=1
            )

    def list_snapshots(self, policy_name: str, ready_only: bool = True) -> list | None:
        """
        Checks for existing snapshots matching the label selector and optional policy name.
        Returns a list of valid snapshots sorted by creation timestamp (newest first).
        policy_name: Filters snapshots by their spec.policyName.
        ready_only: If True, filters out snapshots that are only in 'Ready' state.
        """
        pass

    def delete_snapshots(self, **filters) -> int:
        """
        Deletes snapshots matching the provided filters.
        Returns the count of successfully deleted snapshots.
        """
        pass

    def __exit__(self, exc_type, exc_val, exc_tb):
        """
        Automatically cleans up the Sandbox and the PSMT Trigger Requests.
        """
        for trigger_name in self.created_snapshots:
            try:
                logging.info(f"Cleaning up Trigger request: {trigger_name}")
                self.custom_objects_api.delete_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                    name=trigger_name
                )
            except ApiException as e:
                if e.status != 404:
                    logging.warning(f"Failed to cleanup trigger '{trigger_name}': {e}")
        super().__exit__(exc_type, exc_val, exc_tb)