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
import sys
import os
from typing import Any
from dataclasses import dataclass
from kubernetes import client, watch
from kubernetes.client import ApiException
from ..sandbox_client import SandboxClient, ExecutionResult
from ..constants import *

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(levelname)s - %(message)s",
    stream=sys.stdout,
)


@dataclass
class SnapshotResult:
    """Result of a snapshot processing operation."""
    snapshot_uid: str
    snapshot_timestamp: str

@dataclass
class CheckpointResponse:
    """Structured response for checkpoint operations."""
    execution_result: ExecutionResult
    trigger_name: str

class SnapshotPersistenceManager:
    """
    Manages local persistence of snapshot metadata in a secure directory.
    Stores metadata as a dictionary keyed by trigger_name.
    """
    def __init__(self):
        """Initializes the persistence manager and ensures the secure directory exists."""
        pass

    def _ensure_secure_dir(self):
        """Ensures the directory exists with 700 permissions."""
        pass

    def _load_metadata(self) -> dict[str, Any]:
        """Loads metadata. Returns an empty dict if file doesn't exist or is invalid."""
        pass

    def save_snapshot_metadata(self, record: dict[str, Any]):
        """Saves a snapshot record to the local registry."""
        pass
    
    def delete_snapshot_metadata(self, trigger_name: str):
        """Deletes a snapshot record from the local registry."""
        pass


class PodSnapshotSandboxClient(SandboxClient):
    """
    A specialized Sandbox client for interacting with the gke pod snapshot controller.
    Handles the case only when triggerConfig is type manual.
    """

    def __init__(
        self,
        template_name: str,
        podsnapshot_timeout: int = 180,
        server_port: int = 8080,
        **kwargs,
    ):
        super().__init__(
            template_name, server_port=server_port, **kwargs
        )

        self.controller_ready = False
        self.podsnapshot_timeout = podsnapshot_timeout

    def __enter__(self) -> 'PodSnapshotSandboxClient':
        self.controller_ready = self.snapshot_controller_ready()
        super().__enter__()
        return self

    def _wait_for_snapshot_processed(self, trigger_name: str) -> SnapshotResult:
        """
        Waits for the PodSnapshotManualTrigger to be processed and returns SnapshotResult.
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
                            snapshot_uid = status.get('snapshotCreated', {}).get('name')
                            snapshot_timestamp = condition.get('lastTransitionTime')
                            logging.info(f"Snapshot manual trigger '{trigger_name}' processed successfully. Created Snapshot UID: {snapshot_uid}")
                            w.stop()
                            return SnapshotResult(snapshot_uid=snapshot_uid, snapshot_timestamp=snapshot_timestamp)
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

        core_v1_api = client.CoreV1Api()

        def check_namespace(namespace: str, required_components: list[str]) -> bool:
            try:
                pods = core_v1_api.list_namespaced_pod(namespace)
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


    def checkpoint(self, trigger_name: str) -> CheckpointResponse:
        """
        Triggers a snapshot of the specified pod by creating a PodSnapshotManualTrigger resource.
        The trigger_name will be suffixed with the current datetime.
        Returns:
            tuple[ExecutionResult, str]: The result of the operation and the final trigger name (with suffix).
        """
        trigger_name = f"{trigger_name}-{os.urandom(4).hex()}"

        if not self.controller_ready:
            return ExecutionResult(
                stdout="",
                stderr="Snapshot controller is not ready. Ensure it is installed and running.",
                exit_code=1
            ), trigger_name
        if not self.pod_name:
            return ExecutionResult(
                stdout="",
                stderr="Sandbox pod name not found. Ensure sandbox is created.",
                exit_code=1
            ), trigger_name

        manifest = {
            "apiVersion": f"{PODSNAPSHOT_API_GROUP}/{PODSNAPSHOT_API_VERSION}",
            "kind": f"{PODSNAPSHOT_API_KIND}",
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
            snapshot_result = self._wait_for_snapshot_processed(trigger_name)
            
            # TODO: Add snapshot metadata persistence logic here using SnapshotPersistenceManager

            return CheckpointResponse(
                execution_result=ExecutionResult(
                    stdout=f"PodSnapshotManualTrigger '{trigger_name}' created successfully. Snapshot UID: {snapshot_result.snapshot_uid}",
                    stderr="",
                    exit_code=0
                ),
                trigger_name=trigger_name
            )
        except ApiException as e:
            logging.exception(f"Failed to create PodSnapshotManualTrigger '{trigger_name}': {e}")
            return CheckpointResponse(
                execution_result=ExecutionResult(
                    stdout="",
                    stderr=f"Failed to create PodSnapshotManualTrigger: {e}",
                    exit_code=1
                ),
                trigger_name=trigger_name
            )
        except TimeoutError as e:
            logging.exception(f"Snapshot creation timed out for trigger '{trigger_name}': {e}")
            return CheckpointResponse(
                execution_result=ExecutionResult(
                    stdout="",
                    stderr=f"Snapshot creation timed out: {e}",
                    exit_code=1
                ),
                trigger_name=trigger_name
            )
  
    def list_snapshots(self, policy_name: str, ready_only: bool = True) -> list | None:
        """
        Checks for existing snapshots matching the label selector and optional policy name.
        Returns a list of valid snapshots sorted by creation timestamp (newest first).
        policy_name: Filters snapshots by their spec.policyName.
        ready_only: If True, filters out snapshots that are only in 'Ready' state.
        """
        pass


    def delete_snapshots(self, trigger_name: str) -> int:
        """
        Deletes snapshots matching the provided trigger name and the PSMT resources.
        Returns the count of successfully deleted snapshots.
        """
        pass


    def __exit__(self, exc_type, exc_val, exc_tb):
        """
        Automatically cleans up the Sandbox.
        """
        super().__exit__(exc_type, exc_val, exc_tb)