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
from datetime import datetime, timezone
from typing import Any
from dataclasses import dataclass
from kubernetes import client, watch
from kubernetes.client import ApiException
from ..sandbox_client import SandboxClient
from ..constants import (
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOTMANUALTRIGGER_API_KIND,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
)

SNAPSHOT_SUCCESS_CODE = 0
SNAPSHOT_ERROR_CODE = 1

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
    snapshot_timestamp: str
    error_reason: str
    error_code: int


@dataclass
class RestoreCheckResult:
    """Result of a restore check operation."""

    success: bool
    error_reason: str
    error_code: int


class PodSnapshotSandboxClient(SandboxClient):
    """
    A specialized Sandbox client for interacting with the GKE Pod Snapshot Controller.
    This class enables users to take a manual trigger snapshot of their sandbox and restore from the taken snapshot.
    """

    def __init__(
        self,
        template_name: str,
        podsnapshot_timeout: int = 180,
        **kwargs,
    ):
        super().__init__(template_name, **kwargs)

        self.snapshot_crd_installed = False
        self.core_v1_api = client.CoreV1Api()
        self.podsnapshot_timeout = podsnapshot_timeout

        self.created_manual_triggers = []

    def __enter__(self) -> "PodSnapshotSandboxClient":
        try:
            self.snapshot_crd_installed = self._check_snapshot_crd_installed()
            if not self.snapshot_crd_installed:
                raise RuntimeError(
                    "Pod Snapshot Controller is not ready. "
                    "Ensure the PodSnapshot CRD is installed."
                )
            super().__enter__()
            return self
        except Exception as e:
            self.__exit__(None, None, None)
            raise RuntimeError(
                f"Failed to initialize PodSnapshotSandboxClient. Ensure that you are connected to a GKE cluster "
                f"with the Pod Snapshot Controller enabled. Error details: {e}"
            ) from e

    def _check_snapshot_crd_installed(self) -> bool:
        """
        Checks if the PodSnapshot CRD is installed and available in the cluster.
        Returns:
            bool: True if the CRD is installed, False otherwise.
        """

        if self.snapshot_crd_installed:
            return True

        try:
            # Check if the API resource exists using CustomObjectsApi
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

    def _parse_created_snapshot_info(self, obj: dict[str, Any]) -> SnapshotResult:
        """Parses the object to extract snapshot details."""
        status = obj.get("status", {})
        conditions = status.get("conditions") or []
        for condition in conditions:
            if (
                condition.get("type") == "Triggered"
                and condition.get("status") == "True"
                and condition.get("reason") == "Complete"
            ):
                snapshot_created = status.get("snapshotCreated") or {}
                snapshot_uid = snapshot_created.get("name")
                snapshot_timestamp = condition.get("lastTransitionTime")
                return SnapshotResult(
                    snapshot_uid=snapshot_uid,
                    snapshot_timestamp=snapshot_timestamp,
                )
            elif condition.get("status") == "False" and condition.get("reason") in [
                "Failed",
                "Error",
            ]:
                raise RuntimeError(
                    f"Snapshot failed. Condition: {condition.get('message', 'Unknown error')}"
                )
        raise ValueError("Snapshot is not yet complete.")

    def _wait_for_snapshot_to_be_completed(
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
                    try:
                        result = self._parse_created_snapshot_info(obj)
                        logger.info(
                            f"Snapshot manual trigger '{trigger_name}' processed successfully. Created Snapshot UID: {result.snapshot_uid}"
                        )
                        return result
                    except ValueError:
                        # Continue watching if snapshot is not yet complete
                        continue
                elif event["type"] == "ERROR":
                    logger.error(
                        f"Snapshot watch received error event: {event['object']}"
                    )
                    raise RuntimeError(f"Snapshot watch error: {event['object']}")
                elif event["type"] == "DELETED":
                    logger.error(
                        f"Snapshot manual trigger '{trigger_name}' was deleted before completion."
                    )
                    raise RuntimeError(
                        f"Snapshot manual trigger '{trigger_name}' was deleted."
                    )
        except Exception as e:
            logger.error(f"Error watching snapshot: {e}")
            raise
        finally:
            w.stop()

        raise TimeoutError(
            f"Snapshot manual trigger '{trigger_name}' was not processed within {self.podsnapshot_timeout} seconds."
        )

    def snapshot(self, trigger_name: str) -> SnapshotResponse:
        """
        Triggers a snapshot of the specified pod by creating a PodSnapshotManualTrigger resource.
        The trigger_name will be suffixed with a timestamp and random hex string.
        Returns:
            SnapshotResponse: The result of the operation.
        """
        timestamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
        suffix = uuid.uuid4().hex[:8]
        trigger_name = f"{trigger_name}-{timestamp}-{suffix}"

        if not self.snapshot_crd_installed:
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason="Snapshot CRD is not installed. Ensure it is installed and running.",
                error_code=SNAPSHOT_ERROR_CODE,
            )
        if not self.pod_name:
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason="Sandbox pod name not found. Ensure sandbox is created.",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        manifest = {
            "apiVersion": f"{PODSNAPSHOT_API_GROUP}/{PODSNAPSHOT_API_VERSION}",
            "kind": f"{PODSNAPSHOTMANUALTRIGGER_API_KIND}",
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
            self.created_manual_triggers.append(trigger_name)

            # Start watching from the version we just created to avoid missing updates
            resource_version = created_obj.get("metadata", {}).get("resourceVersion")
            snapshot_result = self._wait_for_snapshot_to_be_completed(
                trigger_name, resource_version
            )

            return SnapshotResponse(
                success=True,
                trigger_name=trigger_name,
                snapshot_uid=snapshot_result.snapshot_uid,
                snapshot_timestamp=snapshot_result.snapshot_timestamp,
                error_reason="",
                error_code=SNAPSHOT_SUCCESS_CODE,
            )
        except ApiException as e:
            logger.exception(
                f"Failed to create PodSnapshotManualTrigger '{trigger_name}': {e}"
            )
            return SnapshotResponse(
                success=False,
                trigger_name=trigger_name,
                snapshot_uid=None,
                snapshot_timestamp=None,
                error_reason=f"Failed to create PodSnapshotManualTrigger: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
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
                error_reason=f"Unexpected error: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )

    def is_restored_from_snapshot(self, snapshot_uid: str) -> RestoreCheckResult:
        """
        Checks if the sandbox pod was restored from the specified snapshot.

        Returns:
            RestoreCheckResult: The result of the restore check operation.
        """
        if not snapshot_uid:
            return RestoreCheckResult(
                success=False,
                error_reason="Snapshot UID cannot be empty.",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        if not self.pod_name:
            logger.warning("Cannot check restore status: pod_name is unknown.")
            return RestoreCheckResult(
                success=False,
                error_reason="Pod name not found. Ensure sandbox is created.",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        try:
            pod = self.core_v1_api.read_namespaced_pod(self.pod_name, self.namespace)

            if not pod.status or not pod.status.conditions:
                return RestoreCheckResult(
                    success=False,
                    error_reason="Pod status or conditions not found.",
                    error_code=SNAPSHOT_ERROR_CODE,
                )

            for condition in pod.status.conditions:
                if condition.type == "PodRestored":
                    if condition.status == "True":
                        # Check if Snapshot UID is present in the condition.message
                        if condition.message and snapshot_uid in condition.message:
                            return RestoreCheckResult(
                                success=True,
                                error_reason="",
                                error_code=SNAPSHOT_SUCCESS_CODE,
                            )
                        else:
                            return RestoreCheckResult(
                                success=False,
                                error_reason="Pod was not restored from the given snapshot",
                                error_code=SNAPSHOT_ERROR_CODE,
                            )
                    else:
                        reason = f" reason: '{condition.get('reason') or ""}'"
                        msg = f" message: '{condition.get('message') or ""}'"
                        return RestoreCheckResult(
                            success=False,
                            error_reason=f"Restore attempted but pending or failed (status: '{condition.status}'{reason}{msg})",
                            error_code=SNAPSHOT_ERROR_CODE,
                        )

            return RestoreCheckResult(
                success=False,
                error_reason="Pod was started as a fresh instance",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        except ApiException as e:
            logger.error(f"Failed to check pod restore status: {e}")
            return RestoreCheckResult(
                success=False,
                error_reason=f"Failed to check pod restore status: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )
        except Exception as e:
            logger.exception(
                f"Unexpected error during restore check for snapshot UID '{snapshot_uid}': {e}"
            )
            return RestoreCheckResult(
                success=False,
                error_reason=f"Unexpected error: {e}",
                error_code=SNAPSHOT_ERROR_CODE,
            )

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
                if e.status == 404:
                    # Ignore if the resource is already deleted
                    continue
                logger.error(
                    f"Failed to delete PodSnapshotManualTrigger '{trigger_name}': {e}"
                )
        super().__exit__(exc_type, exc_val, exc_tb)
