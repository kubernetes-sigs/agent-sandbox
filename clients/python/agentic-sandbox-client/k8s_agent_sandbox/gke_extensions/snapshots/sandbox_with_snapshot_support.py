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
from kubernetes.client import ApiException
from k8s_agent_sandbox.exceptions import SnapshotNotFoundError
from .snapshot_engine import SnapshotEngine, SnapshotResponse
from k8s_agent_sandbox.sandbox import Sandbox
from k8s_agent_sandbox.constants import (
    SANDBOX_API_GROUP,
    SANDBOX_API_VERSION,
    SANDBOX_PLURAL_NAME,
    PODSNAPSHOT_NAME_ANNOTATION,
)
from .utils import (
    check_pod_restored_from_snapshot,
    RestoreCheckResult,
    wait_for_pod_termination,
    wait_for_pod_ready,
    wait_for_sandbox_propagation,
)
from pydantic import BaseModel

SUCCESS_CODE = 0
ERROR_CODE = 1
INTERNAL_ERROR_CODE = 2

logger = logging.getLogger(__name__)

OPERATING_MODE_RUNNING = "Running"
OPERATING_MODE_SUSPENDED = "Suspended"

class SuspendResponse(BaseModel):
    """Result of a suspend operation."""
    success: bool
    snapshot_response: SnapshotResponse | None = None
    error_reason: str = ""
    error_code: int = 0

class ResumeResponse(BaseModel):
    """Result of a resume operation."""
    success: bool
    restored_from_snapshot: bool | None = None
    snapshot_uid: str | None = None
    error_reason: str = ""
    error_code: int = 0

class RestorationResponse(BaseModel):
    """Result of a restore operation."""
    success: bool
    restored_from_snapshot: bool | None = None
    snapshot_uid: str | None = None
    error_reason: str = ""
    error_code: int = 0

class SandboxWithSnapshotSupport(Sandbox):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._snapshots = SnapshotEngine(
            namespace=self.namespace,
            k8s_helper=self.k8s_helper,
            get_pod_name_func=self.get_pod_name,
            get_sandbox_name_hash_func=self.get_sandbox_name_hash,
        )

    @property
    def snapshots(self) -> SnapshotEngine | None:
        return self._snapshots

    @property
    def is_active(self) -> bool:
        return super().is_active and self._snapshots is not None
    
    def _is_restored_from_snapshot(self, snapshot_uid: str) -> RestoreCheckResult:
        """
        Checks if this sandbox was restored from the provided snapshot.

        Returns:
            RestoreCheckResult: The status of restoration check.
        """
        if not snapshot_uid:
            return RestoreCheckResult(
                success=False,
                error_reason="Snapshot UID cannot be empty.",
                error_code=ERROR_CODE,
            )

        pod_name = self.get_pod_name()
        if not pod_name:
            logger.warning("Cannot check restore status: pod_name is unknown.")
            return RestoreCheckResult(
                success=False,
                error_reason="Pod name not found. Ensure sandbox is created.",
                error_code=ERROR_CODE,
            )

        return check_pod_restored_from_snapshot(
            k8s_helper=self.k8s_helper,
            namespace=self.namespace,
            pod_name=pod_name,
            snapshot_uid=snapshot_uid,
        )

    def is_suspended(self) -> bool:
        """
        Checks if the sandbox is currently suspended by inspecting the Sandbox CR.
        A sandbox is considered suspended if the spec.operatingMode is set to Suspended. 
        """
        try:
            sandbox_cr = self.k8s_helper.custom_objects_api.get_namespaced_custom_object(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=self.namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=self.sandbox_id
            )
            spec_operating_mode = sandbox_cr.get("spec", {}).get("operatingMode", OPERATING_MODE_RUNNING)
            pod_ips = sandbox_cr.get("status", {}).get("podIPs")
            
            is_spec_suspended = spec_operating_mode == OPERATING_MODE_SUSPENDED
            
            # TODO: Replace this with Suspended status when it's available
            if is_spec_suspended and pod_ips:
                logger.info(f"Sandbox '{self.sandbox_id}' is in the process of suspending (spec.operatingMode='{OPERATING_MODE_SUSPENDED}' but podIPs still present).")
            elif not is_spec_suspended and not pod_ips:
                logger.info(f"Sandbox '{self.sandbox_id}' is in the process of resuming/starting (spec.operatingMode='{spec_operating_mode}' but no podIPs assigned).")
                
            return is_spec_suspended
        except Exception as e:
            logger.error(f"Failed to check if Sandbox '{self.sandbox_id}' is suspended: {e}")
            return False

    def _set_operating_mode(self, operating_mode: str):
        self.k8s_helper.custom_objects_api.patch_namespaced_custom_object(
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            namespace=self.namespace,
            plural=SANDBOX_PLURAL_NAME,
            name=self.sandbox_id,
            body={"spec": {"operatingMode": operating_mode}}
        )

    def _get_latest_snapshot_uid(self) -> str | None:
        if self.snapshots:
            list_result = self.snapshots.list()
            if not list_result.success:
                raise RuntimeError(f"Snapshot list request failed: {list_result.error_reason}")
            if list_result.snapshots:
                return list_result.snapshots[0].snapshot_uid
        return None

    def suspend(self, snapshot_before_suspend: bool = True, wait_timeout: int = 180) -> SuspendResponse:
        """
        Suspends the sandbox.

        Args:
            snapshot_before_suspend: Whether to take a snapshot of the sandbox before suspending it. Defaults to True.
            wait_timeout: The maximum time in seconds to wait for termination. Defaults to 180.

        Returns:
            SuspendResponse: An object containing the success status, potential snapshot response, and any error details.
        """
        if self.is_suspended():
            logger.info(f"Sandbox '{self.sandbox_id}' is already suspended.")
            return SuspendResponse(
                success=True,
                snapshot_response=None,
                error_reason="",
                error_code=SUCCESS_CODE
            )

        # Ensure the sandbox name hash is fetched and cached before we terminate the pod.
        try:
            sandbox_name_hash = self.get_sandbox_name_hash()
            if not sandbox_name_hash:
                raise ValueError(f"Sandbox name hash resolved to empty or None: {sandbox_name_hash}")
        except Exception as e:
            logger.error(f"Cannot suspend Sandbox: failed to retrieve required name hash label: {e}")
            return SuspendResponse(
                success=False,
                snapshot_response=None,
                error_reason=f"Failed to resolve sandbox name hash: {e}",
                error_code=ERROR_CODE
            )

        snapshot_response = None
        if snapshot_before_suspend and self.snapshots:
            # Generate a unique trigger name for this suspend action
            trigger_name = f"suspend-{self.sandbox_id}"
            snapshot_response = self.snapshots.create(trigger_name)
            if not snapshot_response.success:
                logger.error(f"Snapshot before suspend failed: {snapshot_response.error_reason}")
                return SuspendResponse(
                    success=False,
                    snapshot_response=snapshot_response,
                    error_reason=f"Snapshot failed: {snapshot_response.error_reason}",
                    error_code=ERROR_CODE
                )

        pod_name_to_wait = self.get_pod_name()
        pod_uid_to_wait = None
        if pod_name_to_wait:
            try:
                pod = self.k8s_helper.core_v1_api.read_namespaced_pod(pod_name_to_wait, self.namespace)
                pod_uid_to_wait = pod.metadata.uid
            except ApiException as e:
                if e.status != 404:
                    logger.error(f"Error getting pod UID before suspend: {e}")

        try:
            self._set_operating_mode(OPERATING_MODE_SUSPENDED)
            logger.info(f"Sandbox '{self.sandbox_id}' suspended (operatingMode set to {OPERATING_MODE_SUSPENDED}).")
        except Exception as e:
            logger.error(f"Failed to suspend Sandbox '{self.sandbox_id}': {e}")
            return SuspendResponse(
                success=False,
                snapshot_response=snapshot_response,
                error_reason=f"Failed to patch operatingMode: {e}",
                error_code=ERROR_CODE
            )

        if wait_for_pod_termination(self.k8s_helper, self.namespace, pod_name_to_wait, pod_uid_to_wait, wait_timeout):
            logger.info(f"Sandbox '{self.sandbox_id}' pod successfully terminated.")
            return SuspendResponse(
                success=True,
                snapshot_response=snapshot_response,
                error_reason="",
                error_code=SUCCESS_CODE
            )
        
        logger.warning(f"Timed out waiting for Sandbox '{self.sandbox_id}' pod to terminate.")
        return SuspendResponse(
            success=False,
            snapshot_response=snapshot_response,
            error_reason="Timed out waiting for pod to terminate.",
            error_code=ERROR_CODE
        )

    def _restore_internal(self, target_snapshot_uid: str | None, wait_timeout: int) -> RestorationResponse:
        """Internal restore logic shared by resume() and restore()."""
        # Clear cached pod name and connection before resuming to ensure we pick up the new pod
        self.connector.close()
        self._pod_name = None

        try:
            self._set_operating_mode(OPERATING_MODE_RUNNING)
            logger.info(f"Sandbox '{self.sandbox_id}' pod created successfully (operatingMode set to {OPERATING_MODE_RUNNING}).")
        except Exception as e:
            logger.error(f"Failed to create a new pod for Sandbox '{self.sandbox_id}': {e}")
            return RestorationResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason=f"Failed to patch operatingMode: {e}",
                error_code=ERROR_CODE
            )

        if wait_for_pod_ready(self.k8s_helper, self.namespace, self.get_pod_name, wait_timeout):
            if not target_snapshot_uid:
                logger.info(f"No previous snapshots found for Sandbox '{self.sandbox_id}'. Skipping restore verification.")
                return RestorationResponse(
                    success=True,
                    restored_from_snapshot=False,
                    snapshot_uid=None,
                    error_reason="",
                    error_code=SUCCESS_CODE
                )

            restore_check = self._is_restored_from_snapshot(target_snapshot_uid)
            if restore_check.success:
                logger.info(f"Sandbox '{self.sandbox_id}' successfully restored from snapshot '{target_snapshot_uid}'.")
                return RestorationResponse(
                    success=True,
                    restored_from_snapshot=True,
                    snapshot_uid=target_snapshot_uid,
                    error_reason="",
                    error_code=SUCCESS_CODE
                )
            else:
                logger.error(f"Sandbox '{self.sandbox_id}' was not restored from snapshot '{target_snapshot_uid}': {restore_check.error_reason}")
                return RestorationResponse(
                    success=False,
                    restored_from_snapshot=False,
                    snapshot_uid=target_snapshot_uid,
                    error_reason=f"Pod ready but not restored from snapshot: {restore_check.error_reason}",
                    error_code=ERROR_CODE
                )
        
        logger.warning(f"Timed out waiting for Sandbox '{self.sandbox_id}' pod to become ready.")
        return RestorationResponse(
            success=False,
            restored_from_snapshot=False,
            snapshot_uid=target_snapshot_uid,
            error_reason="Timed out waiting for pod to become ready.",
            error_code=ERROR_CODE
        )

    def resume(self, wait_timeout: int = 180) -> ResumeResponse:
        """
        Resumes the sandbox from the latest available snapshot.

        Args:
            wait_timeout: The maximum time in seconds to wait for the pod to become ready. Defaults to 180.

        Returns:
            ResumeResponse: An object containing the success status, restoration details, and any error information.
        """
        if not self.is_suspended():
            logger.info(f"Sandbox '{self.sandbox_id}' is already running (not suspended).")
            return ResumeResponse(
                success=True,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason="",
                error_code=SUCCESS_CODE
            )

        # Capture the snapshot UID before patching sandbox mode to guarantee we verify
        # against the exact state the controller will see.
        try:
            latest_snapshot_uid = self._get_latest_snapshot_uid()
        except Exception as e:
            logger.error(f"Failed to get latest snapshot UID before resuming: {e}")
            return ResumeResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason=f"Failed to get latest snapshot UID: {e}",
                error_code=ERROR_CODE
            )

        try:
            body = {
                "spec": {
                    "additionalPodMetadata": {
                        "annotations": {
                            PODSNAPSHOT_NAME_ANNOTATION: None
                        }
                    }
                }
            }
            self.k8s_helper.patch_sandbox_claim(self.claim_name, self.namespace, body)
        except Exception as e:
            logger.error(f"Failed to clean up restore annotation before resuming: {e}")
            return ResumeResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason=f"Failed to clean up restore annotation before resuming: {e}",
                error_code=ERROR_CODE,
            )

        if not wait_for_sandbox_propagation(self.k8s_helper, self.namespace, self.sandbox_id, None):
            logger.error("Timed out waiting for restore annotation cleanup to propagate to Sandbox spec.")
            return ResumeResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason="Internal Error: Timed out waiting for restore annotation cleanup.",
                error_code=INTERNAL_ERROR_CODE,
            )

        res = self._restore_internal(latest_snapshot_uid, wait_timeout)
        return ResumeResponse(
            success=res.success,
            restored_from_snapshot=res.restored_from_snapshot,
            snapshot_uid=res.snapshot_uid,
            error_reason=res.error_reason,
            error_code=res.error_code,
        )

    def _verify_snapshot_exists(self, snapshot_uid: str) -> None:
        """Verifies that a snapshot exists for this sandbox."""
        list_result = self.snapshots.list()
        if not list_result.success:
            raise RuntimeError(f"Failed to list snapshots: {list_result.error_reason}")
        if not any(snap.snapshot_uid == snapshot_uid for snap in list_result.snapshots):
            raise SnapshotNotFoundError(f"Snapshot '{snapshot_uid}' does not exist for this sandbox.")

    def restore(self, snapshot_uid: str, sandbox_ready_timeout: int = 180) -> RestorationResponse:
        """Restores this sandbox from a specific snapshot."""
        try:
            self._verify_snapshot_exists(snapshot_uid)

            if not self.is_suspended():
                return RestorationResponse(
                    success=False,
                    restored_from_snapshot=False,
                    snapshot_uid=snapshot_uid,
                    error_reason="Sandbox is currently running and cannot be restored.",
                    error_code=ERROR_CODE
                )

            body = {
                "spec": {
                    "additionalPodMetadata": {
                        "annotations": {
                            PODSNAPSHOT_NAME_ANNOTATION: snapshot_uid
                        }
                    }
                }
            }
            self.k8s_helper.patch_sandbox_claim(self.claim_name, self.namespace, body)

            if not wait_for_sandbox_propagation(self.k8s_helper, self.namespace, self.sandbox_id, snapshot_uid):
                logger.error(f"Timed out waiting for snapshot UID to propagate to Sandbox spec.")
                return RestorationResponse(
                    success=False,
                    restored_from_snapshot=False,
                    snapshot_uid=snapshot_uid,
                    error_reason="Internal Error: Timed out waiting for sandbox restoration.",
                    error_code=INTERNAL_ERROR_CODE
                )

            logger.info(f"Restoring sandbox '{self.sandbox_id}' from snapshot '{snapshot_uid}'.")
            return self._restore_internal(snapshot_uid, sandbox_ready_timeout)

        except SnapshotNotFoundError as e:
            logger.warning(f"Failed to restore Sandbox '{self.sandbox_id}': {e}")
            return RestorationResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=snapshot_uid,
                error_reason=str(e),
                error_code=ERROR_CODE
            )
        except Exception as e:
            logger.error(f"Unexpected error during restore for Sandbox '{self.sandbox_id}': {e}")
            return RestorationResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=snapshot_uid,
                error_reason=f"Unexpected error: {e}",
                error_code=ERROR_CODE
            )
    
    def terminate(self):
        """
        Cleans up the manually generated trigger resources and terminates the Sandbox.
        """
        try:
            if self._snapshots:
                self._snapshots.delete_manual_triggers()
        finally:
            super().terminate()
            self._snapshots = None
        
