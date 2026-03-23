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
from typing import Any
from kubernetes import watch
from pydantic import BaseModel
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
)

logger = logging.getLogger(__name__)


class SnapshotResult(BaseModel):
    """Result of a snapshot processing operation."""
    snapshot_uid: str
    snapshot_timestamp: str


def _get_snapshot_info(snapshot_obj: dict[str, Any]) -> SnapshotResult:
    """Get the details for Snapshot"""
    status = snapshot_obj.get("status", {})
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


def wait_for_snapshot_to_be_completed(
    k8s_helper,
    namespace: str,
    trigger_name: str,
    podsnapshot_timeout: int,
    resource_version: str | None = None,
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
            func=k8s_helper.custom_objects_api.list_namespaced_custom_object,
            namespace=namespace,
            group=PODSNAPSHOT_API_GROUP,
            version=PODSNAPSHOT_API_VERSION,
            plural=PODSNAPSHOTMANUALTRIGGER_PLURAL,
            field_selector=f"metadata.name={trigger_name}",
            timeout_seconds=podsnapshot_timeout,
            **kwargs,
        ):
            if event["type"] in ["ADDED", "MODIFIED"]:
                obj = event["object"]
                try:
                    result = _get_snapshot_info(obj)
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
        f"Snapshot manual trigger '{trigger_name}' was not processed within {podsnapshot_timeout} seconds."
    )
