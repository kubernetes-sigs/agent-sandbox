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

logger = logging.getLogger(__name__)


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
    snapshot_uid: str
    error_reason: str
    error_code: int


@dataclass
class RestoreResult:
    """Result of a restore operation."""

    success: bool
    error_reason: str
    error_code: int


@dataclass
class PolicyMetadata:
    policy_name: str
    policy_labels: dict[str, str]


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
                    logging.warning(
                        "Found legacy list-format metadata. Resetting to empty dict."
                    )
                    return {}
                return data
        except (json.JSONDecodeError, IOError) as e:
            logging.warning(f"Failed to load snapshot metadata: {e}")
            return {}

    def save_snapshot_metadata(self, record: dict[str, Any]):
        """Saves a snapshot record to the local registry keyed by snapshot_uid."""
        snapshot_uid = record.get("snapshot_uid")
        if not snapshot_uid:
            logging.error("Cannot save metadata: missing 'snapshot_uid'.")
            return

        snapshots = self._load_metadata()
        snapshots[snapshot_uid] = record

        try:
            with open(self.metadata_file, "w") as f:
                json.dump(snapshots, f, indent=4)
            self.metadata_file.chmod(0o600)
            logging.info(f"Snapshot metadata saved to {self.metadata_file}")
        except IOError as e:
            logging.error(f"Failed to save snapshot metadata: {e}")

    def delete_snapshot_metadata(self, snapshot_uid: str):
        """Deletes a snapshot record from the local registry."""
        snapshots = self._load_metadata()
        if snapshot_uid in snapshots:
            del snapshots[snapshot_uid]
            try:
                with open(self.metadata_file, "w") as f:
                    json.dump(snapshots, f, indent=4)
                self.metadata_file.chmod(0o600)
                logging.info(f"Snapshot metadata deleted for '{snapshot_uid}'")
            except IOError as e:
                logging.error(f"Failed to save metadata after deletion: {e}")
        else:
            logging.info(
                f"No local metadata found for snapshot '{snapshot_uid}' to delete."
            )


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
        self.core_v1_api = client.CoreV1Api()

        self.created_manual_triggers = []

    def __enter__(self) -> "PodSnapshotSandboxClient":
        self.controller_ready = self.snapshot_controller_ready()
        super().__enter__()
        return self

    def _get_policy_info(self, snapshot_uid: str) -> PolicyMetadata:
        """
        Retrieves the policy name and labels of the specified PodSnapshot resource.
        """
        try:
            snapshot = self.custom_objects_api.get_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOT_PLURAL,
                name=snapshot_uid,
            )
            policy_name = snapshot.get("spec", {}).get("policyName", "")
            if not policy_name:
                logging.warning(f"No policyName found for snapshot {snapshot_uid}")
                return PolicyMetadata(policy_name="", policy_labels={})

            policy = self.custom_objects_api.get_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOTPOLICY_PLURAL,
                name=policy_name,
            )
            policy_metadata = PolicyMetadata(
                policy_name=policy_name,
                policy_labels=policy.get("spec", {})
                .get("selector", {})
                .get("matchLabels", {}),
            )
            return policy_metadata
        except ApiException as e:
            logging.error(f"Failed to retrieve PodSnapshot '{snapshot_uid}': {e}")
            raise

    def _parse_snapshot_result(self, obj) -> SnapshotResult | None:
        """Parses the object to see if snapshot is complete."""
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
                return SnapshotResult(
                    snapshot_uid=snapshot_uid,
                    snapshot_timestamp=snapshot_timestamp,
                )
        return None

    def _wait_for_snapshot_processed(
        self, trigger_name: str, resource_version: str | None = None
    ) -> SnapshotResult:
        """
        Waits for the PodSnapshotManualTrigger to be processed and returns SnapshotResult.
        """
        w = watch.Watch()
        logger.info(
            f"Waiting for snapshot manual trigger '{trigger_name}' to be processed..."
        )

        kwargs = {}
        if resource_version:
            kwargs["resource_version"] = resource_version

        try:
            for event in w.stream(
                func=self.custom_objects_api.list_namespaced_custom_object,
                namespace=self.namespace,
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                field_selector=f"metadata.name={trigger_name}",
                timeout_seconds=self.podsnapshot_timeout,
                **kwargs,
            ):
                if event["type"] in ["ADDED", "MODIFIED"]:
                    obj = event["object"]
                    result = self._parse_snapshot_result(obj)
                    if result:
                        logger.info(
                            f"Snapshot manual trigger '{trigger_name}' processed successfully. Created Snapshot UID: {result.snapshot_uid}"
                        )
                        w.stop()
                        return result
        except Exception as e:
            logger.error(f"Error watching snapshot: {e}")
            raise

        raise TimeoutError(
            f"Snapshot manual trigger '{trigger_name}' was not processed within {self.podsnapshot_timeout} seconds."
        )

    def snapshot_controller_ready(self) -> bool:
        """
        Checks if the snapshot agent pods are running in a GKE-managed pod snapshot cluster.
        Falls back to checking CRD existence if pod listing is forbidden.
        """

        if self.controller_ready:
            return True

        def check_crd_installed() -> bool:
            try:
                # Check directly if the API resource exists using CustomObjectsApi
                resource_list = self.custom_objects_api.get_api_resources(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                )

                if not resource_list or not resource_list.resources:
                    return False

                for resource in resource_list.resources:
                    if resource.kind == PODSNAPSHOT_API_KIND:
                        return True
                return False
            except ApiException as e:
                # If discovery fails with 403/404, we assume not ready/accessible
                if e.status == 403 or e.status == 404:
                    return False
                raise

        def check_namespace(namespace: str, required_components: list[str]) -> bool:
            try:
                pods = self.core_v1_api.list_namespaced_pod(namespace)
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
            except ApiException as e:
                if e.status == 403:
                    logger.info(
                        f"Permission denied listing pods in {namespace}. Checking CRD existence."
                    )
                    return check_crd_installed()
                if e.status == 404:
                    return False
                raise

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
                snapshot_uid=None,
                error_reason="Snapshot controller is not ready. Ensure it is installed and running.",
                error_code=1,
            )
        if not self.pod_name:
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
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
            created_obj = self.custom_objects_api.create_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                body=manifest,
            )

            # Start watching from the version we just created to avoid missing updates
            resource_version = created_obj.get("metadata", {}).get("resourceVersion")
            snapshot_result = self._wait_for_snapshot_processed(
                trigger_name, resource_version
            )

            self.created_manual_triggers.append(trigger_name)
            policy_metadata = self._get_policy_info(snapshot_result.snapshot_uid)
            # Save metadata locally
            try:
                record = {
                    "snapshot_uid": snapshot_result.snapshot_uid,
                    "template_name": self.template_name,
                    "policy_name": policy_metadata.policy_name,
                    "policy_labels": policy_metadata.policy_labels,
                    "namespace": self.namespace,
                    "claim_name": self.claim_name,
                    "timestamp": snapshot_result.snapshot_timestamp,
                }
                self.persistence_manager.save_snapshot_metadata(record)
            except Exception as e:
                logging.warning(f"Failed to save snapshot metadata locally: {e}")

            return SnapshotResponse(
                success=True,
                trigger_name=trigger_name,
                snapshot_uid=snapshot_result.snapshot_uid,
                error_reason="",
                error_code=0,
            )
        except ApiException as e:
            logger.exception(
                f"Failed to create PodSnapshotManualTrigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                error_reason=f"Failed to create PodSnapshotManualTrigger: {e}",
                error_code=1,
            )
        except TimeoutError as e:
            logger.exception(
                f"Snapshot creation timed out for trigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                error_reason=f"Snapshot creation timed out: {e}",
                error_code=1,
            )

    def is_restored_from_snapshot(self, snapshot_uid: str) -> RestoreResult:
        """
        Checks if the sandbox pod was restored from the specified snapshot.

        This is verified by inspecting the 'PodRestored' condition in the pod status
        and confirming that the condition's message contains the provided snapshot UID.

        Returns:
            RestoreResult: The result of the restore operation.
        """
        if not snapshot_uid:
            return RestoreResult(
                success=False,
                error_reason="Snapshot UID cannot be empty.",
                error_code=1,
            )

        if not self.pod_name:
            logger.warning("Cannot check restore status: pod_name is unknown.")
            return RestoreResult(
                success=False,
                error_reason="Pod name not found. Ensure sandbox is created.",
                error_code=1,
            )

        try:
            pod = self.core_v1_api.read_namespaced_pod(self.pod_name, self.namespace)

            if not pod.status or not pod.status.conditions:
                return RestoreResult(
                    success=False,
                    error_reason="Pod status or conditions not found.",
                    error_code=1,
                )

            for condition in pod.status.conditions:
                if condition.type == "PodRestored" and condition.status == "True":
                    # Check if Snapshot UUID is present in the condition.message
                    if condition.message and snapshot_uid in condition.message:
                        return RestoreResult(
                            success=True,
                            error_reason="",
                            error_code=0,
                        )
                    else:
                        return RestoreResult(
                            success=False,
                            error_reason="Pod was not restored from the given snapshot",
                            error_code=1,
                        )

            return RestoreResult(
                success=False,
                error_reason="Pod was not restored from any snapshot",
                error_code=1,
            )

        except ApiException as e:
            logger.error(f"Failed to check pod restore status: {e}")
            return RestoreResult(
                success=False,
                error_reason=f"Failed to check pod restore status: {e}",
                error_code=1,
            )

    def list_snapshots(self, policy_name: str, ready_only: bool = True) -> list | None:
        """
        Checks for existing snapshots matching the policy name.
        Returns a list of valid snapshots sorted by creation timestamp (newest first).

        policy_name: Filters snapshots by their spec.policyName.
        ready_only: If True, filters out snapshots that are only in 'Ready' state.
        """
        local_meta = self.persistence_manager._load_metadata()
        if not local_meta:
            logging.info("No local snapshot metadata found.")
            return None

        valid_snapshots = []

        for snapshot_id, record in local_meta.items():
            uid = record.get("snapshot_uid")
            if not uid:
                # Fallback if key is not uid (legacy) or uid missing in record
                uid = snapshot_id

            # Optimized Filtering: Check policy_name from metadata if available
            meta_policy_name = record.get("policy_name")
            if policy_name and meta_policy_name and meta_policy_name != policy_name:
                continue

            try:
                # Fetch the actual PodSnapshot resource to verify status and details
                snapshot = self.custom_objects_api.get_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOT_PLURAL,
                    name=uid,
                )
            except ApiException as e:
                # If 404, it means it's in metadata but deleted from K8s. Skip it.
                if e.status != 404:
                    logging.warning(f"Failed to fetch PodSnapshot '{uid}': {e}")
                else:
                    # delete from the local metadata
                    self.persistence_manager.delete_snapshot_metadata(uid)
                continue

            spec = snapshot.get("spec", {})
            status = snapshot.get("status", {})
            conditions = status.get("conditions", [])
            metadata = snapshot.get("metadata", {})

            # Filter by policy_name if provided and not already checked
            crd_policy_name = spec.get("policyName")
            if policy_name and crd_policy_name != policy_name:
                continue

            # Update metadata if policy_name was missing
            if not meta_policy_name and crd_policy_name:
                record["policy_name"] = crd_policy_name
                self.persistence_manager.save_snapshot_metadata(record)

            # Check for Ready=True
            is_ready = False
            for cond in conditions:
                if cond.get("type") == "Ready" and cond.get("status") == "True":
                    is_ready = True
                    break

            # Skip if only ready snapshots are requested
            if ready_only and not is_ready:
                continue

            valid_snapshots.append(
                {
                    "snapshot_id": metadata.get("name"),
                    "source_pod": metadata.get("labels", {}).get(
                        "podsnapshot.gke.io/pod-name", "Unknown"
                    ),
                    "uid": metadata.get("uid"),
                    "creationTimestamp": metadata.get("creationTimestamp", ""),
                    "status": "Ready" if is_ready else "NotReady",
                    "policy_name": spec.get("policyName"),
                }
            )

        if not valid_snapshots:
            logging.info(
                "No Ready snapshots found matching criteria in local metadata."
            )
            return None

        # Sort snapshots by creation timestamp descending
        valid_snapshots.sort(key=lambda x: x["creationTimestamp"], reverse=True)
        logging.info(
            f"Found {len(valid_snapshots)} ready snapshots from local metadata."
        )
        return valid_snapshots

    def delete_snapshots(
        self, snapshot_uid: str | None = None, policy_name: str | None = None
    ) -> int:
        """
        Deletes snapshots.
        - If snapshot_uid is provided, deletes that specific snapshot.
        - If policy_name is provided, deletes all snapshots matching that policy.
        - If neither is provided, deletes ALL snapshots in local metadata.
        Returns the count of successfully deleted snapshots.
        """
        local_meta = self.persistence_manager._load_metadata()

        snapshots_to_delete = []  # List of uids

        if snapshot_uid:
            if snapshot_uid in local_meta:
                snapshots_to_delete.append(snapshot_uid)
            else:
                logging.warning(
                    f"Snapshot '{snapshot_uid}' not found in local metadata. Checking other filters or skipping."
                )
                # We could try to delete from K8s even if not in metadata, but for now strict consistency

        if policy_name:
            for uid, record in local_meta.items():
                if record.get("policy_name") == policy_name:
                    if uid not in snapshots_to_delete:
                        snapshots_to_delete.append(uid)

        if not snapshot_uid and not policy_name:
            logging.info(
                "No snapshot_uid or policy_name provided. Deleting ALL snapshots found in local metadata."
            )
            snapshots_to_delete = list(local_meta.keys())

        if not snapshots_to_delete:
            logging.info("No snapshots found matching criteria to delete.")
            return 0

        delete_count = 0
        for uid in snapshots_to_delete:
            record = local_meta.get(uid)
            if not record:
                continue

            # Delete PodSnapshot
            try:
                logging.info(f"Deleting PodSnapshot '{uid}'...")
                self.custom_objects_api.delete_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOT_PLURAL,
                    name=uid,
                )
                logging.info(f"PodSnapshot '{uid}' deleted.")
            except ApiException as e:
                if e.status == 404:
                    logging.info(
                        f"PodSnapshot '{uid}' not found in K8s (already deleted?)."
                    )
                else:
                    logging.error(f"Failed to delete PodSnapshot '{uid}': {e}")

            # Cleanup Local Metadata
            self.persistence_manager.delete_snapshot_metadata(uid)
            delete_count += 1

        logging.info(
            f"Snapshot deletion process completed. Deleted {delete_count} snapshots."
        )
        return delete_count

    def __exit__(self, exc_type, exc_val, exc_tb):
        """
        Cleans up the PodSnapshotManualTrigger Resources.
        Automatically cleans up the Sandbox.
        """
        for trigger_name in self.created_manual_triggers:
            try:
                self.custom_objects_api.delete_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                    name=trigger_name,
                )
                logger.info(f"Deleted PodSnapshotManualTrigger '{trigger_name}'")
            except ApiException as e:
                logger.error(
                    f"Failed to delete PodSnapshotManualTrigger '{trigger_name}': {e}"
                )

        super().__exit__(exc_type, exc_val, exc_tb)
