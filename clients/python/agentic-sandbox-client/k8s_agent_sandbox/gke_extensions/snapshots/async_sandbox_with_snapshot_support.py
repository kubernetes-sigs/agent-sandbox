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

from kubernetes_asyncio.client import ApiException

from k8s_agent_sandbox.async_sandbox import AsyncSandbox
from k8s_agent_sandbox.constants import (
    POD_NAME_ANNOTATION,
    SANDBOX_API_GROUP,
    SANDBOX_API_VERSION,
    SANDBOX_PLURAL_NAME,
)
from .async_snapshot_engine import AsyncSnapshotEngine
from .snapshot_engine import SnapshotResponse
from .async_utils import (
    RestoreCheckResult,
    async_check_pod_restored_from_snapshot,
    async_wait_for_pod_termination,
    async_wait_for_pod_ready,
)
from .sandbox_with_snapshot_support import SuspendResponse, ResumeResponse

SUCCESS_CODE = 0
ERROR_CODE = 1

logger = logging.getLogger(__name__)


class AsyncSandboxWithSnapshotSupport(AsyncSandbox):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._snapshots = AsyncSnapshotEngine(
            namespace=self.namespace,
            k8s_helper=self.k8s_helper,
            get_pod_name_func=self.get_pod_name,
        )

    @property
    def snapshots(self) -> AsyncSnapshotEngine | None:
        return self._snapshots

    @property
    def is_active(self) -> bool:
        return super().is_active and self._snapshots is not None

    async def _resolve_pod_name(self) -> str | None:
        """Always fetches the pod name from the Sandbox CR, bypassing cache."""
        sandbox_object = (
            await self.k8s_helper.get_sandbox(self.sandbox_id, self.namespace) or {}
        )
        metadata = sandbox_object.get("metadata") or {}
        annotations = metadata.get("annotations") or {}
        pod_name = annotations.get(POD_NAME_ANNOTATION)
        return pod_name if pod_name is not None else self.sandbox_id

    async def is_restored_from_snapshot(self, snapshot_uid: str) -> RestoreCheckResult:
        """Checks if this sandbox was restored from the provided snapshot."""
        if not snapshot_uid:
            return RestoreCheckResult(
                success=False,
                error_reason="Snapshot UID cannot be empty.",
                error_code=ERROR_CODE,
            )

        pod_name = await self.get_pod_name()
        if not pod_name:
            logger.warning("Cannot check restore status: pod_name is unknown.")
            return RestoreCheckResult(
                success=False,
                error_reason="Pod name not found. Ensure sandbox is created.",
                error_code=ERROR_CODE,
            )

        return await async_check_pod_restored_from_snapshot(
            k8s_helper=self.k8s_helper,
            namespace=self.namespace,
            pod_name=pod_name,
            snapshot_uid=snapshot_uid,
        )

    async def is_suspended(self) -> bool:
        """
        Checks if the sandbox is currently suspended by inspecting the Sandbox CR.
        A sandbox is considered suspended if its spec.replicas is 0.
        """
        await self.k8s_helper._ensure_initialized()
        try:
            sandbox_cr = (
                await self.k8s_helper.custom_objects_api.get_namespaced_custom_object(
                    group=SANDBOX_API_GROUP,
                    version=SANDBOX_API_VERSION,
                    namespace=self.namespace,
                    plural=SANDBOX_PLURAL_NAME,
                    name=self.sandbox_id,
                )
            )
            spec_replicas = sandbox_cr.get("spec", {}).get("replicas", 1)
            pod_ips = sandbox_cr.get("status", {}).get("podIPs")

            is_spec_suspended = spec_replicas == 0

            if is_spec_suspended and pod_ips:
                logger.info(
                    f"Sandbox '{self.sandbox_id}' is in the process of suspending (spec.replicas=0 but podIPs still present)."
                )
            elif not is_spec_suspended and not pod_ips:
                logger.info(
                    f"Sandbox '{self.sandbox_id}' is in the process of resuming/starting (spec.replicas={spec_replicas} but no podIPs assigned)."
                )

            return is_spec_suspended
        except Exception as e:
            logger.error(
                f"Failed to check if Sandbox '{self.sandbox_id}' is suspended: {e}"
            )
            return False

    async def _set_replicas(self, replicas: int):
        await self.k8s_helper._ensure_initialized()
        await self.k8s_helper.custom_objects_api.patch_namespaced_custom_object(
            group=SANDBOX_API_GROUP,
            version=SANDBOX_API_VERSION,
            namespace=self.namespace,
            plural=SANDBOX_PLURAL_NAME,
            name=self.sandbox_id,
            body={"spec": {"replicas": replicas}},
        )

    async def _get_latest_snapshot_uid(self) -> str | None:
        if self.snapshots:
            list_result = await self.snapshots.list()
            if not list_result.success:
                raise RuntimeError(
                    f"Snapshot list request failed: {list_result.error_reason}"
                )
            if list_result.snapshots:
                return list_result.snapshots[0].snapshot_uid
        return None

    async def suspend(
        self, snapshot_before_suspend: bool = True, wait_timeout: int = 180
    ) -> SuspendResponse:
        """
        Suspends the sandbox.

        Args:
            snapshot_before_suspend: Whether to take a snapshot before suspending. Defaults to True.
            wait_timeout: Maximum time in seconds to wait for termination. Defaults to 180.

        Returns:
            SuspendResponse: An object containing the success status, potential snapshot response, and any error details.
        """
        if await self.is_suspended():
            logger.info(f"Sandbox '{self.sandbox_id}' is already suspended.")
            return SuspendResponse(
                success=True,
                snapshot_response=None,
                error_reason="",
                error_code=SUCCESS_CODE,
            )

        snapshot_response: SnapshotResponse | None = None
        if snapshot_before_suspend and self.snapshots:
            trigger_name = f"suspend-{self.sandbox_id}"
            snapshot_response = await self.snapshots.create(trigger_name)
            if not snapshot_response.success:
                logger.error(
                    f"Snapshot before suspend failed: {snapshot_response.error_reason}"
                )
                return SuspendResponse(
                    success=False,
                    snapshot_response=snapshot_response,
                    error_reason=f"Snapshot failed: {snapshot_response.error_reason}",
                    error_code=ERROR_CODE,
                )

        pod_name_to_wait = await self.get_pod_name()
        pod_uid_to_wait = None
        if pod_name_to_wait:
            try:
                await self.k8s_helper._ensure_initialized()
                pod = await self.k8s_helper.core_v1_api.read_namespaced_pod(
                    pod_name_to_wait, self.namespace
                )
                pod_uid_to_wait = pod.metadata.uid
            except ApiException as e:
                if e.status != 404:
                    logger.error(f"Error getting pod UID before suspend: {e}")

        try:
            await self._set_replicas(0)
            logger.info(
                f"Sandbox '{self.sandbox_id}' suspended (scaled down to 0 replicas)."
            )
        except Exception as e:
            logger.error(f"Failed to suspend Sandbox '{self.sandbox_id}': {e}")
            return SuspendResponse(
                success=False,
                snapshot_response=snapshot_response,
                error_reason=f"Failed to scale down sandbox: {e}",
                error_code=ERROR_CODE,
            )

        if await async_wait_for_pod_termination(
            self.k8s_helper,
            self.namespace,
            pod_name_to_wait,
            pod_uid_to_wait,
            wait_timeout,
        ):
            logger.info(f"Sandbox '{self.sandbox_id}' pod successfully terminated.")
            return SuspendResponse(
                success=True,
                snapshot_response=snapshot_response,
                error_reason="",
                error_code=SUCCESS_CODE,
            )

        logger.warning(
            f"Timed out waiting for Sandbox '{self.sandbox_id}' pod to terminate."
        )
        return SuspendResponse(
            success=False,
            snapshot_response=snapshot_response,
            error_reason="Timed out waiting for pod to terminate.",
            error_code=ERROR_CODE,
        )

    async def resume(self, wait_timeout: int = 180) -> ResumeResponse:
        """
        Resumes the sandbox from the latest available snapshot.

        Args:
            wait_timeout: Maximum time in seconds to wait for the pod to become ready. Defaults to 180.

        Returns:
            ResumeResponse: An object containing the success status, restoration details, and any error information.
        """
        if not await self.is_suspended():
            logger.info(
                f"Sandbox '{self.sandbox_id}' is already running (not suspended)."
            )
            return ResumeResponse(
                success=True,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason="",
                error_code=SUCCESS_CODE,
            )

        try:
            latest_snapshot_uid = await self._get_latest_snapshot_uid()
        except Exception as e:
            logger.error(f"Failed to get latest snapshot UID before resuming: {e}")
            return ResumeResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason=f"Failed to get latest snapshot UID: {e}",
                error_code=ERROR_CODE,
            )

        # Invalidate cached pod name so fresh lookups pick up the new pod
        self._pod_name = None

        try:
            await self._set_replicas(1)
            logger.info(
                f"Sandbox '{self.sandbox_id}' resumed (scaled up to 1 replica)."
            )
        except Exception as e:
            logger.error(f"Failed to resume Sandbox '{self.sandbox_id}': {e}")
            return ResumeResponse(
                success=False,
                restored_from_snapshot=False,
                snapshot_uid=None,
                error_reason=f"Failed to patch replicas: {e}",
                error_code=ERROR_CODE,
            )

        if await async_wait_for_pod_ready(
            self.k8s_helper, self.namespace, self._resolve_pod_name, wait_timeout
        ):
            if not latest_snapshot_uid:
                logger.info(
                    f"No previous snapshots found for Sandbox '{self.sandbox_id}'. Skipping restore verification."
                )
                return ResumeResponse(
                    success=True,
                    restored_from_snapshot=False,
                    snapshot_uid=None,
                    error_reason="",
                    error_code=SUCCESS_CODE,
                )

            # Refresh pod name cache now that the pod is ready
            self._pod_name = await self._resolve_pod_name()

            restore_check = await self.is_restored_from_snapshot(latest_snapshot_uid)
            if restore_check.success:
                logger.info(
                    f"Sandbox '{self.sandbox_id}' successfully restored from snapshot '{latest_snapshot_uid}'."
                )
                return ResumeResponse(
                    success=True,
                    restored_from_snapshot=True,
                    snapshot_uid=latest_snapshot_uid,
                    error_reason="",
                    error_code=SUCCESS_CODE,
                )
            else:
                logger.error(
                    f"Sandbox '{self.sandbox_id}' was not restored from snapshot '{latest_snapshot_uid}': {restore_check.error_reason}"
                )
                return ResumeResponse(
                    success=False,
                    restored_from_snapshot=False,
                    snapshot_uid=latest_snapshot_uid,
                    error_reason=f"Pod ready but not restored from snapshot: {restore_check.error_reason}",
                    error_code=ERROR_CODE,
                )

        logger.warning(
            f"Timed out waiting for Sandbox '{self.sandbox_id}' pod to become ready."
        )
        return ResumeResponse(
            success=False,
            restored_from_snapshot=False,
            snapshot_uid=latest_snapshot_uid,
            error_reason="Timed out waiting for pod to become ready.",
            error_code=ERROR_CODE,
        )

    async def terminate(self):
        """Cleans up the manually generated trigger resources and terminates the Sandbox."""
        try:
            if self._snapshots:
                await self._snapshots.delete_manual_triggers()
        except Exception as e:
            logger.error(f"Failed to cleanup snapshot triggers during terminate: {e}")
        finally:
            await super().terminate()
            self._snapshots = None
