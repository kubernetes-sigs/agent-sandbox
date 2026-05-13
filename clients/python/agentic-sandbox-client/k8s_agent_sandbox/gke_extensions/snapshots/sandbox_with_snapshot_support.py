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
import copy
from kubernetes.client import ApiException
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

logger = logging.getLogger(__name__)

class SuspendResponse(BaseModel):
    """Result of a suspend operation."""
    success: bool
    snapshot_response: SnapshotResponse | None = None
    error_reason: str = ""
    error_code: int = 0

class RestorationResponse(BaseModel):
    """Result of a restore/resume operation."""
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
    
    def is_restored_from_snapshot(self, snapshot_uid: str) -> RestoreCheckResult:
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
        A sandbox is considered suspended if its spec.replicas is 0 and it has no podIPs assigned.
        """
        try:
            sandbox_cr = self.k8s_helper.custom_objects_api.get_namespaced_custom_object(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=self.namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=self.sandbox_id
            )
            spec_replicas = sandbox_cr.get("spec", {}).get("replicas", 1)
            pod_ips = sandbox_cr.get("status", {}).get("podIPs")
            
            is_spec_suspended = spec_replicas == 0
            
            # TODO: Replace this with Suspended status when it's available
            if is_spec_suspended and pod_ips:
                logger.info(f"Sandbox '{self.sandbox_id}' is in the process of suspending (spec.replicas=0 but podIPs still present).")
            elif not is_spec_suspended and not pod_ips:
                logger.info(f"Sandbox '{self.sandbox_id}' is in the process of resuming/starting (spec.replicas={spec_replicas} but no podIPs assigned).")
                
            return is_spec_suspended
        except Exception as e:
            logger.error(f"Failed to check if Sandbox '{self.sandbox_id}' is suspended: {e}")
            return False

    def _set_replicas(self, replicas: int):
        self.k8s_helper.custom_objects_api.patch_namespaced_custom_object(
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            namespace=self.namespace,
            plural=SANDBOX_PLURAL_NAME,
            name=self.sandbox_id,
            body={"spec": {"replicas": replicas}}
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
            self._set_replicas(0)
            logger.info(f"Sandbox '{self.sandbox_id}' suspended (scaled down to 0 replicas).")
        except Exception as e:
            logger.error(f"Failed to suspend Sandbox '{self.sandbox_id}': {e}")
            return SuspendResponse(
                success=False,
                snapshot_response=snapshot_response,
                error_reason=f"Failed to scale down sandbox: {e}",
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
            self._set_replicas(1)
            logger.info(f"Sandbox '{self.sandbox_id}' activated (scaled up to 1 replica).")
        except Exception as e:
            logger.error(f"Failed to activate Sandbox '{self.sandbox_id}': {e}")
            return RestorationResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=target_snapshot_uid,
                error_reason=f"Failed to patch replicas: {e}",
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

            restore_check = self.is_restored_from_snapshot(target_snapshot_uid)
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

    def resume(self, wait_timeout: int = 180) -> RestorationResponse:
        """
        Resumes the sandbox from the latest available snapshot.

        Args:
            wait_timeout: The maximum time in seconds to wait for the pod to become ready. Defaults to 180.

        Returns:
            RestorationResponse: An object containing the success status, restoration details, and any error information.
        """
        if not self.is_suspended():
            logger.info(f"Sandbox '{self.sandbox_id}' is already running (not suspended).")
            return RestorationResponse(
                success=True,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason="",
                error_code=SUCCESS_CODE
            )

        try:
            latest_snapshot_uid = self._get_latest_snapshot_uid()
        except Exception as e:
            logger.error(f"Failed to get target snapshot UID before resuming: {e}")
            return RestorationResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason=f"Failed to get target snapshot UID: {e}",
                error_code=ERROR_CODE
            )

        return self._restore_internal(latest_snapshot_uid, wait_timeout)

    def _verify_snapshot_exists(self, snapshot_uid: str) -> None:
        """Verifies that a snapshot exists for this sandbox."""
        list_result = self.snapshots.list()
        if not list_result.success:
            raise RuntimeError(f"Failed to list snapshots: {list_result.error_reason}")
        if not any(snap.snapshot_uid == snapshot_uid for snap in list_result.snapshots):
            raise RuntimeError(f"Snapshot '{snapshot_uid}' does not exist for this sandbox.")

    def restore(self, snapshot_uid: str | None = None, sandbox_ready_timeout: int = 180) -> RestorationResponse:
        """Restores this sandbox from a specific or the latest snapshot."""
        try:
            if not snapshot_uid:
                snapshot_uid = self._get_latest_snapshot_uid()
                if not snapshot_uid:
                    return RestorationResponse(
                        success=False,
                        restored_from_snapshot=False,
                        snapshot_uid=None,
                        error_reason=f"No snapshots found for sandbox '{self.claim_name}' to restore from.",
                        error_code=ERROR_CODE
                    )
            else:
                self._verify_snapshot_exists(snapshot_uid)

            # Suspend first to ensure the pod is deleted before we mutate the claim
            suspend_resp = self.suspend(snapshot_before_suspend=False, wait_timeout=sandbox_ready_timeout)
            if not suspend_resp.success:
                return RestorationResponse(
                    success=False,
                    restored_from_snapshot=False,
                    snapshot_uid=snapshot_uid,
                    error_reason=f"Failed to suspend sandbox before restore: {suspend_resp.error_reason}",
                    error_code=ERROR_CODE
                )

            claim = self.k8s_helper.get_sandbox_claim(self.claim_name, self.namespace)
            if not claim:
                return RestorationResponse(
                    success=False,
                    restored_from_snapshot=False,
                    snapshot_uid=snapshot_uid,
                    error_reason=f"SandboxClaim '{self.claim_name}' not found in namespace '{self.namespace}'",
                    error_code=ERROR_CODE
                )

            spec = claim.get("spec", {}) or {}
            additional_pod_metadata = copy.deepcopy(spec.get("additionalPodMetadata", {})) or {}            
            if "annotations" not in additional_pod_metadata:
                additional_pod_metadata["annotations"] = {}
            else:
                additional_pod_metadata["annotations"] = dict(additional_pod_metadata["annotations"])

            additional_pod_metadata["annotations"][PODSNAPSHOT_NAME_ANNOTATION] = snapshot_uid

            body = {
                "spec": {
                    "additionalPodMetadata": additional_pod_metadata
                }
            }
            self.k8s_helper.patch_sandbox_claim(self.claim_name, self.namespace, body)

            if not wait_for_sandbox_propagation(self.k8s_helper, self.namespace, self.sandbox_id, snapshot_uid):
                return RestorationResponse(
                    success=False,
                    restored_from_snapshot=False,
                    snapshot_uid=snapshot_uid,
                    error_reason="Timed out waiting for snapshot UID to propagate to Sandbox spec.",
                    error_code=ERROR_CODE
                )

            logger.info(f"Resuming Sandbox '{self.sandbox_id}' to trigger restore.")
            return self._restore_internal(snapshot_uid, sandbox_ready_timeout)

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
        
