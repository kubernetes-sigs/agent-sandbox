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

import asyncio

from kubernetes_asyncio.client import ApiException

from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
from k8s_agent_sandbox.constants import (
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_KIND,
    PODSNAPSHOT_API_VERSION,
)
from .async_sandbox_with_snapshot_support import AsyncSandboxWithSnapshotSupport


class AsyncPodSnapshotSandboxClient(
    AsyncSandboxClient[AsyncSandboxWithSnapshotSupport]
):
    """
    Async client for managing Sandboxes with Pod Snapshot support.

    This class enables users to take snapshots of Sandboxes via GKE Pod Snapshot:
    https://docs.cloud.google.com/kubernetes-engine/docs/concepts/pod-snapshots

    The CRD check is performed lazily on first use (``__aenter__``,
    ``create_sandbox``, or ``get_sandbox``) because ``__init__`` cannot be async.
    """

    sandbox_class = AsyncSandboxWithSnapshotSupport

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._snapshot_crd_verified = False
        self._crd_check_lock = asyncio.Lock()

    async def _ensure_snapshot_ready(self) -> None:
        """Ensures the PodSnapshot CRD is available. Thread-safe via asyncio.Lock."""
        if self._snapshot_crd_verified:
            return
        async with self._crd_check_lock:
            if self._snapshot_crd_verified:
                return
            await self.k8s_helper._ensure_initialized()
            if not await self._check_snapshot_crd_installed():
                raise RuntimeError(
                    "Pod Snapshot Controller is not ready. "
                    "Ensure the PodSnapshot CRD is installed."
                )
            self._snapshot_crd_verified = True

    async def _check_snapshot_crd_installed(self) -> bool:
        try:
            resource_list = await self.k8s_helper.custom_objects_api.get_api_resources(
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
            if e.status in [403, 404]:
                return False
            raise

    async def __aenter__(self) -> "AsyncPodSnapshotSandboxClient":
        try:
            await self._ensure_snapshot_ready()
        except Exception:
            await self.close()
            raise
        return self

    async def create_sandbox(self, *args, **kwargs) -> AsyncSandboxWithSnapshotSupport:
        await self._ensure_snapshot_ready()
        return await super().create_sandbox(*args, **kwargs)

    async def get_sandbox(self, *args, **kwargs) -> AsyncSandboxWithSnapshotSupport:
        await self._ensure_snapshot_ready()
        return await super().get_sandbox(*args, **kwargs)
