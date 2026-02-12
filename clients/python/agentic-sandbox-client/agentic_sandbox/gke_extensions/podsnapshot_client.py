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
from pathlib import Path
import json
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
class SnapshotResponse:
    """Structured response for snapshot operations."""

    success: bool
    trigger_name: str
    error_reason: str
    error_code: int


class SnapshotPersistenceManager:
    """
    Manages local persistence of snapshot metadata in a secure directory.
    Stores metadata as a dictionary keyed by trigger_name.
    """

    def __init__(self):
        """Initializes the persistence manager and ensures the secure directory exists."""
        self.secure_dir = Path.home() / ".snapshot_metadata"
        self._ensure_secure_dir()
        self.metadata_file = self.secure_dir / ".snapshots.json"

    def _ensure_secure_dir(self):
        """Ensures the directory exists with 700 permissions."""
        if not self.secure_dir.exists():
            self.secure_dir.mkdir(parents=True)
        self.secure_dir.chmod(0o700)

    def _load_metadata(self) -> dict[str, Any]:
        """Loads metadata. Returns an empty dict if file doesn't exist or is invalid."""
        if not self.metadata_file.exists():
            return {}
        try:
            with open(self.metadata_file, "r") as f:
                data = json.load(f)
                if isinstance(data, list):
                    # Handle legacy list format by clearing it (or converting if preferred, but identifying key is tricky if not consistent)
                    logging.warning("Found legacy list-format metadata. Resetting to empty dict.")
                    return {}
                return data
        except (json.JSONDecodeError, IOError) as e:
            logging.warning(f"Failed to load snapshot metadata: {e}")
            return {}

    def save_snapshot_metadata(self, record: dict[str, Any]):
        """Saves a snapshot record to the local registry."""
        trigger_name = record.get("trigger_name")
        if not trigger_name:
            logging.error("Cannot save metadata: missing 'trigger_name'.")
            return

        snapshots = self._load_metadata()
        snapshots[trigger_name] = record
        
        try:
            with open(self.metadata_file, "w") as f:
                json.dump(snapshots, f, indent=4)
            self.metadata_file.chmod(0o600)
            logging.info(f"Snapshot metadata saved to {self.metadata_file}")
        except IOError as e:
            logging.error(f"Failed to save snapshot metadata: {e}")

    def delete_snapshot_metadata(self, trigger_name: str):
        """Deletes a snapshot record from the local registry."""
        snapshots = self._load_metadata()
        if trigger_name in snapshots:
            del snapshots[trigger_name]
            try:
                with open(self.metadata_file, "w") as f:
                    json.dump(snapshots, f, indent=4)
                self.metadata_file.chmod(0o600)
                logging.info(f"Snapshot metadata deleted for '{trigger_name}'")
            except IOError as e:
                logging.error(f"Failed to save metadata after deletion: {e}")
        else:
            logging.info(f"No local metadata found for trigger '{trigger_name}' to delete.")


class PodSnapshotSandboxClient(SandboxClient):
    """
    A specialized Sandbox client for interacting with the gke pod snapshot controller.
    Currently supports manual triggering via PodSnapshotManualTrigger.
    """

    def __init__(
        self,
        template_name: str,
        podsnapshot_timeout: int = 180,
        server_port: int = 8080,
        **kwargs,
    ):
        super().__init__(template_name, server_port=server_port, **kwargs)

        self.controller_ready = False
        self.podsnapshot_timeout = podsnapshot_timeout
        self.persistence_manager = SnapshotPersistenceManager()

    def __enter__(self) -> "PodSnapshotSandboxClient":
        self.controller_ready = self.snapshot_controller_ready()
        super().__enter__()
        return self
    
    def _get_policy_labels(self, snapshot_uid: str) -> dict:
        """
        Retrieves the policy labels of the specified PodSnapshot resource.
        """
        try:
            snapshot = self.custom_objects_api.get_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOT_PLURAL,
                name=snapshot_uid
            )
            policy_name = snapshot.get("spec", {}).get("policyName", {})
            policy = self.custom_objects_api.get_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOTPOLICY_PLURAL,
                name=policy_name
            )
            return policy.get("spec", {}).get("selector", {}).get("matchLabels", {})
        except ApiException as e:
            logging.error(f"Failed to retrieve PodSnapshot '{snapshot_uid}': {e}")
            raise

    def _wait_for_snapshot_processed(self, trigger_name: str) -> SnapshotResult:
        """
        Waits for the PodSnapshotManualTrigger to be processed and returns SnapshotResult.
        """
        w = watch.Watch()
        logging.info(
            f"Waiting for snapshot manual trigger '{trigger_name}' to be processed..."
        )

        try:
            for event in w.stream(
                func=self.custom_objects_api.list_namespaced_custom_object,
                namespace=self.namespace,
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                field_selector=f"metadata.name={trigger_name}",
                timeout_seconds=self.podsnapshot_timeout,
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
                            snapshot_uid = status.get("snapshotCreated", {}).get("name")
                            snapshot_timestamp = condition.get("lastTransitionTime")
                            logging.info(
                                f"Snapshot manual trigger '{trigger_name}' processed successfully. Created Snapshot UID: {snapshot_uid}"
                            )
                            w.stop()
                            return SnapshotResult(
                                snapshot_uid=snapshot_uid,
                                snapshot_timestamp=snapshot_timestamp,
                            )
        except Exception as e:
            logging.error(f"Error watching snapshot: {e}")
            raise

        raise TimeoutError(
            f"Snapshot manual trigger '{trigger_name}' was not processed within {self.podsnapshot_timeout} seconds."
        )

    def snapshot_controller_ready(self) -> bool:
        """
        Checks if the snapshot agent pods are running in a GKE-managed pod snapshot cluster.
        """

        if self.controller_ready:
            return True

        core_v1_api = client.CoreV1Api()

        def check_namespace(namespace: str, required_components: list[str]) -> bool:
            try:
                pods = core_v1_api.list_namespaced_pod(namespace)
                found_components = {
                    component: False for component in required_components
                }

                for pod in pods.items:
                    if pod.status.phase == "Running":
                        name = pod.metadata.name
                        for component in required_components:
                            if component in name:
                                found_components[component] = True

                return all(found_components.values())
            except ApiException:
                return False

        # Check managed: requires only agent in gke-managed-pod-snapshots
        if check_namespace(SNAPSHOT_NAMESPACE_MANAGED, [SNAPSHOT_AGENT]):
            self.controller_ready = True
            return True

        self.controller_ready = False
        return self.controller_ready

    def snapshot(self, trigger_name: str) -> SnapshotResponse:
        """
        Triggers a snapshot of the specified pod by creating a PodSnapshotManualTrigger resource.
        The trigger_name will be suffixed with the current datetime.
        Returns:
            tuple[ExecutionResult, str]: The result of the operation and the final trigger name (with suffix).
        """
        trigger_name = f"{trigger_name}-{os.urandom(4).hex()}"

        if not self.controller_ready:
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                error_reason="Snapshot controller is not ready. Ensure it is installed and running.",
                error_code=1,
            )
        if not self.pod_name:
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                error_reason="Sandbox pod name not found. Ensure sandbox is created.",
                error_code=1,
            )

        manifest = {
            "apiVersion": f"{PODSNAPSHOT_API_GROUP}/{PODSNAPSHOT_API_VERSION}",
            "kind": f"{PODSNAPSHOT_API_KIND}",
            "metadata": {"name": trigger_name, "namespace": self.namespace},
            "spec": {"targetPod": self.pod_name},
        }

        try:
            self.custom_objects_api.create_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                body=manifest,
            )
            snapshot_result = self._wait_for_snapshot_processed(trigger_name)

            policy_labels = self._get_policy_labels(snapshot_result.snapshot_uid)
            # Save metadata locally
            try:
                record = {
                    "trigger_name": trigger_name,
                    "uid": snapshot_result.snapshot_uid,
                    "template_name": self.template_name,
                    "policy_labels": policy_labels,
                    "namespace": self.namespace,
                    "claim_name": self.claim_name,
                    "timestamp": snapshot_result.snapshot_timestamp
                }
                self.persistence_manager.save_snapshot_metadata(record)
            except Exception as e:
                logging.warning(f"Failed to save snapshot metadata locally: {e}")

            return SnapshotResponse(
                success=True, trigger_name=trigger_name, error_reason="", error_code=0
            )
        except ApiException as e:
            logging.exception(
                f"Failed to create PodSnapshotManualTrigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                error_reason=f"Failed to create PodSnapshotManualTrigger: {e}",
                error_code=1,
            )
        except TimeoutError as e:
            logging.exception(
                f"Snapshot creation timed out for trigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                error_reason=f"Snapshot creation timed out: {e}",
                error_code=1,
            )

    def list_snapshots(self, policy_name: str, ready_only: bool = True) -> list | None:
        """
        Checks for existing snapshots matching the label selector and optional policy name.
        Returns a list of valid snapshots sorted by creation timestamp (newest first).
        
        policy_name: Filters snapshots by their spec.policyName.
        ready_only: If True, filters out snapshots that are only in 'Ready' state.
        """
        local_meta = self.persistence_manager._load_metadata()
        if not local_meta:
            logging.info("No local snapshot metadata found.")
            return None

        valid_snapshots = []
        
        for trigger_name, record in local_meta.items():
            uid = record.get("uid")
            if not uid:
                continue

            try:
                # Fetch the actual PodSnapshot resource to verify status and details
                snapshot = self.custom_objects_api.get_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOT_PLURAL,
                    name=uid
                )
            except ApiException as e:
                # If 404, it means it's in metadata but deleted from K8s. Skip it.
                if e.status != 404:
                    logging.warning(f"Failed to fetch PodSnapshot '{uid}' (trigger: {trigger_name}): {e}")
                continue

            spec = snapshot.get("spec", {})
            status = snapshot.get("status", {})
            conditions = status.get("conditions", [])
            metadata = snapshot.get("metadata", {})

            # Filter by policy_name if provided
            if policy_name and spec.get("policyName") != policy_name:
                continue
            
            # Check for Ready=True
            is_ready = False
            for cond in conditions:
                if cond.get("type") == "Ready" and cond.get("status") == "True":
                    is_ready = True
                    break

            # Skip if only ready snapshots are requested 
            if ready_only and not is_ready:
                continue
            
            valid_snapshots.append({
                "snapshot_id": metadata.get("name"),
                "trigger_name": trigger_name,
                "source_pod": metadata.get("labels", {}).get("podsnapshot.gke.io/pod-name", "Unknown"),
                "uid": metadata.get("uid"),
                "creationTimestamp": metadata.get("creationTimestamp", ""),
                "status": "Ready" if is_ready else "NotReady",
                "policy_name": spec.get("policyName")
            })

        if not valid_snapshots:
            logging.info("No Ready snapshots found matching criteria in local metadata.")
            return None
        
        # Sort snapshots by creation timestamp descending
        valid_snapshots.sort(key=lambda x: x["creationTimestamp"], reverse=True)
        logging.info(f"Found {len(valid_snapshots)} ready snapshots from local metadata.")
        return valid_snapshots

    def delete_snapshots(self, trigger_name: str | None = None) -> int:
        """
        Deletes snapshots matching the provided trigger name and the PSMT resources.
        Returns the count of successfully deleted snapshots.
        """
        local_meta = self.persistence_manager._load_metadata()
        
        triggers_to_delete = []

        if trigger_name:
            if trigger_name in local_meta:
                triggers_to_delete.append(trigger_name)
            else:
                logging.warning(f"Trigger '{trigger_name}' not found in local metadata. No deletion performed.")
                return 0
        else:
            logging.info("No trigger_name provided. Deleting ALL snapshots found in local metadata.")
            triggers_to_delete = list(local_meta.keys())

        if not triggers_to_delete:
            logging.info("No local snapshot metadata found to process.")
            return 0

        delete_count = 0
        for trig in triggers_to_delete:
            record = local_meta.get(trig)
            if not record:
                continue

            uid = record.get("uid")
            
            # Delete PodSnapshot (if UID is known)
            if uid:
                try:
                    logging.info(f"Deleting PodSnapshot '{uid}' associated with trigger '{trig}'...")
                    self.custom_objects_api.delete_namespaced_custom_object(
                        group=PODSNAPSHOT_API_GROUP,
                        version=PODSNAPSHOT_API_VERSION,
                        namespace=self.namespace,
                        plural=PODSNAPSHOT_PLURAL,
                        name=uid
                    )
                    logging.info(f"PodSnapshot '{uid}' deleted.")
                except ApiException as e:
                    if e.status == 404:
                        logging.info(f"PodSnapshot '{uid}' not found in K8s (already deleted?).")
                    else:
                        logging.error(f"Failed to delete PodSnapshot '{uid}': {e}")
            else:
                logging.warning(f"No UID associated with trigger '{trig}' in metadata. Skipping PodSnapshot deletion.")

            # Delete PodSnapshotManualTrigger
            try:
                logging.info(f"Deleting PodSnapshotManualTrigger '{trig}'...")
                self.custom_objects_api.delete_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                    name=trig
                )
                logging.info(f"PodSnapshotManualTrigger '{trig}' deleted.")
            except ApiException as e:
                if e.status == 404:
                    logging.info(f"PodSnapshotManualTrigger '{trig}' not found in K8s (already deleted?).")
                else:
                    logging.error(f"Failed to delete PodSnapshotManualTrigger '{trig}': {e}")

            # Cleanup Local Metadata
            self.persistence_manager.delete_snapshot_metadata(trig)
            delete_count += 1

        logging.info(f"Snapshot deletion process completed. Deleted {delete_count} snapshots.")
        return delete_count

    def __exit__(self, exc_type, exc_val, exc_tb):
        """
        Automatically cleans up the Sandbox.
        """
        super().__exit__(exc_type, exc_val, exc_tb)
