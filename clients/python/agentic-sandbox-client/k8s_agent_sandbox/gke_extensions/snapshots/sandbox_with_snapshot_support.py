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
import os
from datetime import datetime, timezone
from typing import Any
from dataclasses import dataclass
from kubernetes import client, watch
from kubernetes.client import ApiException
from .snapshot_engine import SnapshotEngine
from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.sandbox import Sandbox
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOTMANUALTRIGGER_API_KIND,
    PODSNAPSHOTMANUALTRIGGER_PLURAL,
    POD_NAME_ANNOTATION,
)
from .utils import SnapshotResult, wait_for_snapshot_to_be_completed

SNAPSHOT_SUCCESS_CODE = 0
SNAPSHOT_ERROR_CODE = 1

logger = logging.getLogger(__name__)

class SandboxWithSnapshotSupport(Sandbox):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._snapshots = SnapshotEngine(
            sandbox_id=self.id,
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
        
    
    def terminate(self):
        """
        Closes the client side connections and cleans up the user generated trigger resources.
        """
        super()._close_connection()
        
        if self._snapshots:
            self._snapshots.delete_manual_triggers()
            self._snapshots = None
        super().terminate()
        
