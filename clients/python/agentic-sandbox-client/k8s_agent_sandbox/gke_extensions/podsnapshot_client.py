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
from kubernetes import client, watch
from kubernetes.client import ApiException
from ..sandbox_client import SandboxClient, ExecutionResult
from ..constants import *

logger = logging.getLogger(__name__)


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

    def __enter__(self) -> "PodSnapshotSandboxClient":
        self.controller_ready = self.snapshot_controller_ready()
        super().__enter__()
        return self

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

    def __exit__(self, exc_type, exc_val, exc_tb):
        """
        Automatically cleans up the Sandbox.
        """
        super().__exit__(exc_type, exc_val, exc_tb)
