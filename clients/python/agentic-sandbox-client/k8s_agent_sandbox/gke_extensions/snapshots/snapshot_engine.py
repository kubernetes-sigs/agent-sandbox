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
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOTMANUALTRIGGER_API_KIND,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
)
from .utils import wait_for_snapshot_to_be_completed

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

    def create(self, trigger_name: str, podsnapshot_timeout: int = 60) -> SnapshotResponse:
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
            pod_snapshot_manual_trigger_cr = self.k8s_helper.custom_objects_api.create_namespaced_custom_object(
                group=PODSNAPSHOT_API_GROUP,
                version=PODSNAPSHOT_API_VERSION,
                namespace=self.namespace,
                plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
                body=manifest,
            )
            self.created_manual_triggers.append(trigger_name)
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
            
        try:
            # Start watching from the version we just created to avoid missing updates
            resource_version = pod_snapshot_manual_trigger_cr.get("metadata", {}).get("resourceVersion")
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
