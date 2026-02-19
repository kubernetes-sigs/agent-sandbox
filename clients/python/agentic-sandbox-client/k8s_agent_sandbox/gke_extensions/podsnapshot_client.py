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
        self.core_v1_api = client.CoreV1Api()

        self.created_manual_triggers = []

    def __enter__(self) -> "PodSnapshotSandboxClient":
        self.controller_ready = self.snapshot_controller_ready()
        super().__enter__()
        return self

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

    def __exit__(self, exc_type, exc_val, exc_tb):
        """
        Cleans up the PodSnapshotManualTrigger Resources.
        Automatically cleans up the Sandbox.

        TODO: Add cleanup for PodSnapshot resources.
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
