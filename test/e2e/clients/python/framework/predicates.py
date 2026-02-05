# Copyright 2025 The Kubernetes Authors.
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

import kubernetes
from typing import Dict, Any


def deployment_ready(min_ready: int = 1):
    """Predicate to check if a Deployment has at least min_ready available replicas."""

    def check(obj: kubernetes.client.V1Deployment) -> bool:
        if obj.status:
            available_replicas = obj.status.available_replicas or 0
            return available_replicas >= min_ready
        return False

    return check


def pod_ready():
    """Predicate to check if a Pod is ready."""

    def check(obj: kubernetes.client.V1Pod) -> bool:
        if not obj.status:
            return False
        for condition in obj.status.conditions or []:
            if condition.type == "Ready" and condition.status == "True":
                return True
        return False

    return check


def warmpool_ready():
    """
    Predicate to check if a SandboxWarmPool (CR) has all the required number of ready sandboxes.
    """

    def check(obj: Dict[str, Any]) -> bool:
        # Safety check: ensure obj is a dict (just in case)
        if not isinstance(obj, dict):
            return False

        status = obj.get("status") or {}
        ready_replicas = status.get("readyReplicas", 0)
        replicas = obj.get("spec", {}).get("replicas", 0)

        return ready_replicas == replicas

    return check


def gateway_address_ready():
    """Predicate to check if a Gateway has an address."""

    def check(obj: Dict[str, Any]) -> bool:
        if not isinstance(obj, dict):
            return False

        status = obj.get("status") or {}
        addresses = status.get("addresses") or []
        return len(addresses) > 0

    return check
