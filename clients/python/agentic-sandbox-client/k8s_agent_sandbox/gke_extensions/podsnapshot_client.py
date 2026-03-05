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
from kubernetes import client
from kubernetes.client import ApiException
from ..sandbox_client import SandboxClient
from ..constants import (
    PODSNAPSHOT_NAMESPACE_MANAGED,
    PODSNAPSHOT_AGENT,
    PODSNAPSHOT_API_GROUP,
    PODSNAPSHOT_API_VERSION,
    PODSNAPSHOT_API_KIND,
)

logger = logging.getLogger(__name__)


class PodSnapshotSandboxClient(SandboxClient):
    """
    A specialized Sandbox client for interacting with the gke pod snapshot controller.
    """

    def __init__(
        self,
        template_name: str,
        server_port: int = 8080,
        **kwargs,
    ):
        super().__init__(template_name, server_port=server_port, **kwargs)

        self.controller_ready = False
        self.core_v1_api = client.CoreV1Api()

    def __enter__(self) -> "PodSnapshotSandboxClient":
        try:
            self.controller_ready = self._check_snapshot_crd_installed()
            if not self.controller_ready:
                raise RuntimeError(
                    "Pod Snapshot Controller is not ready. "
                    "Ensure the PodSnapshotManualTrigger CRD is installed and the controller is running."
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
        Checks if the PodSnapshot CRD is installed in the cluster.
        """

        if self.controller_ready:
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

    def __exit__(self, exc_type, exc_val, exc_tb):
        """
        Automatically cleans up the Sandbox.
        """
        super().__exit__(exc_type, exc_val, exc_tb)
