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
from typing import Callable, Literal
from datetime import datetime, timezone
from kubernetes.client import ApiException
from pydantic import BaseModel, ConfigDict, ValidationError

from k8s_agent_sandbox.constants import (
    SANDBOX_NAME_HASH_LABEL,
    PODSNAPSHOT_POD_NAME_ANNOTATION,
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


class SnapshotFilter(BaseModel):
    """Filter for listing snapshots."""

    model_config = ConfigDict(extra="forbid")

    ready_only: bool = True


class SnapshotEngine:
    """Engine for managing Sandbox snapshots."""

    def __init__(
        self,
        namespace: str,
        k8s_helper,
        get_pod_name_func: Callable[[], str],
        get_sandbox_name_hash_func: Callable[[], str | None],
    ):
        self.namespace = namespace
        self.k8s_helper = k8s_helper
        self.get_pod_name_func = get_pod_name_func
        self.get_sandbox_name_hash_func = get_sandbox_name_hash_func
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
        self, filter_by: SnapshotFilter | dict | None = None
    ) -> ListSnapshotResult:
        """
        Checks for existing snapshots associated with the sandbox.
        Returns a ListSnapshotResult containing valid snapshots sorted by creation timestamp (newest first).

        filter_by: Structure containing filters (ready_only).
        """
        if filter_by is None:
            filter_by = SnapshotFilter()
        elif isinstance(filter_by, dict):
            try:
                filter_by = SnapshotFilter(**filter_by)
            except ValidationError as e:
                logger.error(f"Invalid filter parameters: {e}")
                return ListSnapshotResult(
                    success=False,
                    snapshots=[],
                    error_reason=f"Invalid filter parameters: {e}",
                    error_code=SNAPSHOT_ERROR_CODE,
                )

        valid_snapshots = []
        pod_name = self.get_pod_name_func()

        selectors = []
        if not pod_name:
            logger.warning("Pod name not found.")
            return ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason="Pod name not found.",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        sandbox_name_hash = self.get_sandbox_name_hash_func()
        if not sandbox_name_hash:
            logger.warning(f"Sandbox name hash not found for pod {pod_name}.")
            return ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason="Sandbox name hash not found.",
                error_code=SNAPSHOT_ERROR_CODE,
            )
        
        selectors.append(f"{SANDBOX_NAME_HASH_LABEL}={sandbox_name_hash}")

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

            for snapshot in response.get("items") or []:
                status = snapshot.get("status") or {}
                conditions = status.get("conditions") or []
                metadata = snapshot.get("metadata") or {}

                # Check for Ready=True
                is_ready = False
                for cond in conditions:
                    if cond.get("type") == "Ready" and cond.get("status") == "True":
                        is_ready = True
                        break
                # Skip if only ready snapshots are requested
                if filter_by.ready_only and not is_ready:
                    continue

                try:
                    valid_snapshots.append(
                        SnapshotDetail(
                            snapshot_uid=metadata.get("name"),
                            source_pod=metadata.get("annotations", {}).get(
                                PODSNAPSHOT_POD_NAME_ANNOTATION, "Unknown"
                            ),
                            creation_timestamp=metadata.get("creationTimestamp"),
                            status="Ready" if is_ready else "NotReady",
                        )
                    )
                except ValidationError as e:
                    logger.warning(
                        f"Skipping malformed snapshot {metadata.get('name', 'Unknown')}: {e}"
                    )
                    continue
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
                f"Unexpected error during list snapshots for filter '{filter_by}': {e}"
            )
            return ListSnapshotResult(
                success=False,
                snapshots=[],
                error_reason=f"Unexpected error: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
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

    def _execute_deletion(
        self,
        snapshot_uid: str | None = None,
        scope: str | None = None,
        timeout: int = 180,
    ) -> DeleteSnapshotResult:
        """Helper method to execute deletion of snapshots."""
        snapshots_to_delete = []

        if snapshot_uid:
            snapshots_to_delete.append(snapshot_uid)
        elif scope == "global":
            logger.info("Deleting ALL snapshots for this pod.")
            snapshots_result = self.list(filter_by={"ready_only": False})
            if not snapshots_result.success:
                return DeleteSnapshotResult(
                    success=False,
                    deleted_snapshots=[],
                    error_reason=f"Failed to list snapshots before deletion: {snapshots_result.error_reason}",
                    error_code=SNAPSHOT_ERROR_CODE,
                )
            snapshots_to_delete = [s.snapshot_uid for s in snapshots_result.snapshots]

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
            try:
                logger.info(f"Deleting PodSnapshot '{uid}'...")
                delete_resp = (
                    self.k8s_helper.custom_objects_api.delete_namespaced_custom_object(
                        group=PODSNAPSHOT_API_GROUP,
                        version=PODSNAPSHOT_API_VERSION,
                        namespace=self.namespace,
                        plural=PODSNAPSHOT_PLURAL,
                        name=uid,
                    )
                )
                logger.info(
                    f"PodSnapshot '{uid}' deletion requested. Waiting for confirmation..."
                )

                resource_version = None
                if isinstance(delete_resp, dict):
                    resource_version = delete_resp.get("metadata", {}).get(
                        "resourceVersion"
                    )

                if wait_for_snapshot_deletion(
                    k8s_helper=self.k8s_helper,
                    namespace=self.namespace,
                    snapshot_uid=uid,
                    resource_version=resource_version,
                    timeout=timeout,
                ):
                    deleted_snapshots.append(uid)
                else:
                    msg = f"Timed out waiting for confirmation of deletion for snapshot '{uid}'"
                    logger.error(msg)
                    errors.append(msg)
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

    def delete(self, snapshot_uid: str, timeout: int = 180) -> DeleteSnapshotResult:
        """Delete a single snapshot by UID."""
        return self._execute_deletion(snapshot_uid=snapshot_uid, timeout=timeout)

    def delete_all(
        self,
        timeout: int = 180,
    ) -> DeleteSnapshotResult:
        """Deletes all snapshots for this pod."""
        logger.info("Deleting every snapshot for this pod...")
        return self._execute_deletion(scope="global", timeout=timeout)
