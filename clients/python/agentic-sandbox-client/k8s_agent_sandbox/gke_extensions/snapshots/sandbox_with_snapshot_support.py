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
from .snapshot_engine import SnapshotEngine
from k8s_agent_sandbox.sandbox import Sandbox
from .utils import check_pod_restored_from_snapshot, RestoreCheckResult
from pydantic import BaseModel

SNAPSHOT_SUCCESS_CODE = 0
SNAPSHOT_ERROR_CODE = 1

logger = logging.getLogger(__name__)

class SandboxWithSnapshotSupport(Sandbox):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._snapshots = SnapshotEngine(
            namespace=self.namespace,
            k8s_helper=self.k8s_helper,
            pod_name=self.pod_name,
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
                error_code=SNAPSHOT_ERROR_CODE,
            )

        if not self.pod_name:
            logger.warning("Cannot check restore status: pod_name is unknown.")
            return RestoreCheckResult(
                success=False,
                error_reason="Pod name not found. Ensure sandbox is created.",
                error_code=SNAPSHOT_ERROR_CODE,
            )

        return check_pod_restored_from_snapshot(
            k8s_helper=self.k8s_helper,
            namespace=self.namespace,
            pod_name=self.pod_name,
            snapshot_uid=snapshot_uid,
        )
    
    def terminate(self):
        """
        Cleans up the manually generated trigger resources and terminates the Sandbox.
        """
        try:
            if self._snapshots:
                self._snapshots.delete_manual_triggers()
        finally:
            self._snapshots = None
            super().terminate()
        
