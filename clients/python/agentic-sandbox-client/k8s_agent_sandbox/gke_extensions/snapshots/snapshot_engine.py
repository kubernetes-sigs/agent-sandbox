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
import uuid
import time
from typing import Callable
from datetime import datetime, timezone
from kubernetes.client import ApiException
from pydantic import BaseModel

from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_PLURAL,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOTMANUALTRIGGER_API_KIND,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
)
from .utils import wait_for_snapshot_to_be_completed, wait_for_snapshot_deletion

SNAPSHOT_SUCCESS_CODE = 0
SNAPSHOT_ERROR_CODE = 1

logger = logging.getLogger(__name__)


class SnapshotResponse(BaseModel):
    """Structured response for snapshot operations."""

    success: bool
    trigger_name: str
    snapshot_uid: str | None
    snapshot_timestamp: str | None
    error_reason: str
    error_code: int


class SnapshotDetail(BaseModel):
    """Detailed information about a snapshot."""

    snapshot_uid: str
    source_pod: str
    creation_timestamp: str | None
    status: str


class ListSnapshotResult(BaseModel):
    """Result of a list snapshots operation."""

    success: bool
    snapshots: list[SnapshotDetail]
    error_reason: str
    error_code: int


class DeleteSnapshotResult(BaseModel):
    """Result of a delete snapshot operation."""

    success: bool
    deleted_snapshots: list[str]
    error_reason: str
    error_code: int


class SnapshotEngine:
    """Engine for managing Sandbox snapshots."""

    def __init__(
        self,
        namespace: str,
        k8s_helper,
        get_pod_name_func: Callable[[], str],
    ):
        self.namespace = namespace
        self.k8s_helper = k8s_helper
        self.get_pod_name_func = get_pod_name_func
        self.created_manual_triggers = []

    def create(
        self, trigger_name: str, podsnapshot_timeout: int = 180
    ) -> SnapshotResponse:
        """
        Creates a snapshot of the Sandbox.
        """
        timestamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
        suffix = uuid.uuid4().hex[:8]
        # Sanitize to comply with Kubernetes resource name rules
        safe_trigger_name = trigger_name.lower().replace("_", "-")

        # Truncate to avoid exceeding Kubernetes 63-character limit for resource names
        # "-{timestamp}-{suffix}" is 25 chars long, leaving a max of 38 chars for safe_trigger_name
        safe_trigger_name = safe_trigger_name[:38].strip("-")
        if not safe_trigger_name:
            safe_trigger_name = "snap"

        trigger_name = f"{safe_trigger_name}-{timestamp}-{suffix}"

        manifest = {
            "apiVersion": f"{PODSNAPSHOT_API_GROUP}/{PODSNAPSHOT_API_VERSION}",
            "kind": f"{PODSNAPSHOTMANUALTRIGGER_API_KIND}",
            "metadata": {"name": trigger_name, "namespace": self.namespace},
            "spec": {"targetPod": self.get_pod_name_func()},
        }

        try:
            pod_snapshot_manual_trigger_cr = (
                self.k8s_helper.custom_objects_api.create_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                    body=manifest,
                )
            )
            self.created_manual_triggers.append(trigger_name)
        except ApiException as e:
            error_message = f"Failed to create PodSnapshotManualTrigger: {e}"
            if e.status == 403:
                error_message += " Check if the service account has RBAC permissions to create PodSnapshotManualTrigger resources."

            logger.exception(
                f"Failed to create PodSnapshotManualTrigger '{trigger_name}': {e}"
            )

            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason=error_message,
                error_code=SNAPSHOT_ERROR_CODE,
            )

        try:
            # Start watching from the version we just created to avoid missing updates
            resource_version = pod_snapshot_manual_trigger_cr.get("metadata", {}).get(
                "resourceVersion"
            )
            snapshot_result = wait_for_snapshot_to_be_completed(
                k8s_helper=self.k8s_helper,
                namespace=self.namespace,
                trigger_name=trigger_name,
                podsnapshot_timeout=podsnapshot_timeout,
                resource_version=resource_version,
            )

            return SnapshotResponse(
                success=True,
                trigger_name=trigger_name,
                snapshot_uid=snapshot_result.snapshot_uid,
                snapshot_timestamp=snapshot_result.snapshot_timestamp,
                error_reason="",
                error_code=SNAPSHOT_SUCCESS_CODE,
            )
        except TimeoutError as e:
            logger.exception(
                f"Snapshot creation timed out for trigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason=f"Snapshot creation timed out: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )
        except RuntimeError as e:
            logger.exception(
                f"Snapshot creation failed for trigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason=f"Snapshot creation failed: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )
        except Exception as e:
            logger.exception(
                f"Unexpected error during snapshot creation for trigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason=f"Server error: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )

    def delete_manual_triggers(self, max_retries: int = 3):
        """Cleans up the manual trigger related resources created by this Sandbox."""
        remaining_triggers = list(self.created_manual_triggers)

        for attempt in range(1, max_retries + 1):
            if not remaining_triggers:
                break

            current_batch = remaining_triggers
            remaining_triggers = []

            for trigger_name in current_batch:
                try:
                    self.k8s_helper.custom_objects_api.delete_namespaced_custom_object(
                        group=PODSNAPSHOT_API_GROUP,
                        version=PODSNAPSHOT_API_VERSION,
                        namespace=self.namespace,
                        plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                        name=trigger_name,
                    )
                    logger.info(f"Deleted PodSnapshotManualTrigger '{trigger_name}'")
                except ApiException as e:
                    if e.status == 404:
                        # Ignore if the resource is already deleted
                        continue
                    logger.error(
                        f"Attempt {attempt}/{max_retries}: Failed to delete PodSnapshotManualTrigger '{trigger_name}': {e}"
                    )
                    remaining_triggers.append(trigger_name)
                except Exception as e:
                    logger.error(
                        f"Attempt {attempt}/{max_retries}: Unexpected error while deleting PodSnapshotManualTrigger '{trigger_name}': {e}"
                    )
                    remaining_triggers.append(trigger_name)

            if remaining_triggers and attempt < max_retries:
                time.sleep(1)  # Brief pause before retrying

        self.created_manual_triggers = remaining_triggers

        if self.created_manual_triggers:
            logger.warning(
                f"Failed to delete {len(self.created_manual_triggers)} PodSnapshotManualTrigger(s) "
                f"after {max_retries} attempts: {', '.join(self.created_manual_triggers)}. "
                "These resources may be leaked in Kubernetes and require manual cleanup."
            )

    def list(
        self, grouping_labels: dict[str, str] | None = None, ready_only: bool = True
    ) -> ListSnapshotResult:
        """
        Checks for existing snapshots matching the grouping labels associated with the sandbox.
        Returns a ListSnapshotResult containing valid snapshots sorted by creation timestamp (newest first).

        grouping_labels: Filters snapshots by their metadata labels.
        ready_only: If True, only returns snapshots that are in the 'Ready' state.
        """

        valid_snapshots = []
        pod_name = self.get_pod_name_func()

        selectors = []
        if not pod_name:
            logger.warning("Pod name not found. Ensure sandbox is created.")
            return ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason="Pod name not found. Ensure sandbox is created.",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        selectors.append(f"podsnapshot.gke.io/pod-name={pod_name}")

        if grouping_labels:
            for k, v in grouping_labels.items():
                selectors.append(f"{k}={v}")

        label_selector = ",".join(selectors)

        logger.info(f"Listing snapshots with label selector: {label_selector}")
        try:
            # Fetch the PodSnapshots using label selector directly
            response = self.k8s_helper.custom_objects_api.list_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOT_PLURAL,
                label_selector=label_selector,
            )
        except ApiException as e:
            logger.error(f"Failed to list PodSnapshots: {e}")
            return ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason=f"Failed to list PodSnapshots: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )
        except Exception as e:
            logger.exception(
                f"Unexpected error during list snapshots for grouping labels '{grouping_labels}': {e}"
            )
            return ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason=f"Unexpected error: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        for snapshot in response.get("items") or []:
            status = snapshot.get("status", {})
            conditions = status.get("conditions") or []
            metadata = snapshot.get("metadata", {})

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
                SnapshotDetail(
                    snapshot_uid=metadata.get("name"),
                    source_pod=metadata.get("labels", {}).get(
                        "podsnapshot.gke.io/pod-name", "Unknown"
                    ),
                    creation_timestamp=metadata.get("creationTimestamp"),
                    status="Ready" if is_ready else "NotReady",
                )
            )

        if not valid_snapshots:
            logger.info("No snapshots found matching criteria.")
            return ListSnapshotResult(
                success=True,
                snapshots=[],
                error_reason="",
                error_code=SNAPSHOT_SUCCESS_CODE,
            )

        # Sort snapshots by creation timestamp descending
        valid_snapshots.sort(key=lambda x: x.creation_timestamp or "", reverse=True)
        logger.info(f"Found {len(valid_snapshots)} snapshots.")
        return ListSnapshotResult(
            success=True,
            snapshots=valid_snapshots,
            error_reason="",
            error_code=SNAPSHOT_SUCCESS_CODE,
        )

    def delete(
        self,
        grouping_labels: dict[str, str] | None = None,
        snapshot_uid: str | None = None,
    ) -> DeleteSnapshotResult:
        """
        Deletes snapshots.
        - If snapshot_uid is provided, deletes that specific snapshot.
        - If grouping_labels is provided, deletes all snapshots matching the grouping labels.
        - If not provided, deletes ALL snapshots for this pod.

        Note: snapshot_uid and grouping_labels are mutually exclusive.

        Returns a DeleteSnapshotResult containing the list of successfully deleted snapshots.
        """
        if snapshot_uid and grouping_labels:
            raise ValueError(
                "snapshot_uid and grouping_labels are mutually exclusive. "
                "Provide only one of them."
            )

        snapshots_to_delete = []

        if snapshot_uid:
            snapshots_to_delete.append(snapshot_uid)
        else:
            if grouping_labels:
                logger.info(
                    f"No snapshot_uid provided. Deleting snapshots based on pod name and grouping_labels: {grouping_labels}"
                )
            else:
                logger.info("No filters provided. Deleting ALL snapshots for this pod.")

            # Fetch all snapshots using list without filtering by ready status
            snapshots_result = self.list(
                grouping_labels=grouping_labels, ready_only=False
            )
            if not snapshots_result.success:
                return DeleteSnapshotResult(
                    success=False,
                    deleted_snapshots=[],
                    error_reason=f"Failed to list snapshots before deletion: {snapshots_result.error_reason}",
                    error_code=SNAPSHOT_ERROR_CODE,
                )
            if snapshots_result.snapshots:
                snapshots_to_delete = [
                    s.snapshot_uid for s in snapshots_result.snapshots
                ]
        logger.info(f"Snapshots to delete: {snapshots_to_delete}")

        if not snapshots_to_delete:
            logger.info("No snapshots found matching criteria to delete.")
            return DeleteSnapshotResult(
                success=True,
                deleted_snapshots=[],
                error_reason="",
                error_code=SNAPSHOT_SUCCESS_CODE,
            )

        deleted_snapshots = []
        errors = []
        for uid in snapshots_to_delete:
            # Delete PodSnapshot
            try:
                logger.info(f"Deleting PodSnapshot '{uid}'...")
                self.k8s_helper.custom_objects_api.delete_namespaced_custom_object(
                    group=PODSNAPSHOT_API_GROUP,
                    version=PODSNAPSHOT_API_VERSION,
                    namespace=self.namespace,
                    plural=PODSNAPSHOT_PLURAL,
                    name=uid,
                )
                logger.info(
                    f"PodSnapshot '{uid}' deletion requested. Waiting for confirmation..."
                )

                # Wait for completion of deletion
                wait_for_snapshot_deletion(
                    k8s_helper=self.k8s_helper,
                    namespace=self.namespace,
                    snapshot_uid=uid,
                )

                deleted_snapshots.append(uid)
            except ApiException as e:
                if e.status == 404:
                    logger.info(
                        f"PodSnapshot '{uid}' not found in K8s (already deleted?)."
                    )
                else:
                    msg = f"Failed to delete PodSnapshot '{uid}': {e}"
                    logger.error(msg)
                    errors.append(msg)
            except Exception as e:
                msg = f"Unexpected error deleting PodSnapshot '{uid}': {e}"
                logger.exception(msg)
                errors.append(msg)

        logger.info(
            f"Snapshot deletion process completed. Deleted {len(deleted_snapshots)} snapshots."
        )

        if errors:
            error_msg = "; ".join(errors)
            if deleted_snapshots:
                error_msg = f"Partial failure: deleted {len(deleted_snapshots)}/{len(snapshots_to_delete)} snapshots. Errors: {error_msg}"
            return DeleteSnapshotResult(
                success=False,
                deleted_snapshots=deleted_snapshots,
                error_reason=error_msg,
                error_code=SNAPSHOT_ERROR_CODE,
            )

        return DeleteSnapshotResult(
            success=True,
            deleted_snapshots=deleted_snapshots,
            error_reason="",
            error_code=SNAPSHOT_SUCCESS_CODE,
        )
