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

"""Publish a fleet-member's capacity into its own ClusterProfile status.

The write half of the ClusterProfile integration. `inventory.py` reads; this
writes. Both use the same `PROP_*` constants so the two halves cannot drift.

Each member applies ONLY its own ClusterProfile, using Server-Side Apply with
a distinct field manager. That is the pattern the ClusterProfile spec calls for
when several actors write one status: the cluster manager (GKE Fleet) owns
identity and health, we own `agents.x-k8s.io/*` capacity, and neither takes
fields from the other.

Deliberately NOT written here: `conditions`. `ControlPlaneHealthy` and `Joined`
belong to the cluster manager. A member asserting its own control plane is
healthy is worth nothing anyway — a partitioned member would happily keep
claiming it. We publish a heartbeat instead and let the reader decide.

NON-OBVIOUS BEHAVIOR OF THE PYTHON SSA PATH
-------------------------------------------
1. **Content type.** The Python client offers both `merge-patch+json` and
   `apply-patch+yaml` and picks the FIRST one, so a plain
   `patch_namespaced_custom_object_status(..., field_manager=...)` is a
   merge-patch that *accepts* `field_manager` and silently gives you no SSA
   ownership at all. It has to be forced via a default header on a dedicated
   ApiClient. `_apply_api()` does that; `assert_ssa_configured()` proves it.

2. **List merge semantics — checked, and we got lucky.** `status.properties`
   is a list, and under SSA a list without `x-kubernetes-list-type: map` is
   *atomic*: one manager's apply replaces the whole list and silently drops
   properties owned by another. That would have made per-member SSA unsafe
   next to a cluster manager publishing its own properties. Upstream marks it
   `x-kubernetes-list-type: map` keyed on `name`, so it merges per property —
   verified in the real CRD and then empirically, by watching a `gke-fleet`
   owned `location` property survive a member applying seven
   `agents.x-k8s.io/*` ones. Do NOT re-derive this from the fixture CRD in
   `deploy/`: it is preserve-unknown-fields, therefore atomic, and will
   cheerfully pass a test that the real hub would fail.

3. **Conflicts surface as 409 rather than being forced.** See `force` in
   `__init__`.
"""

from __future__ import annotations

import logging
from typing import Any, Mapping

from .hubauth import TOKEN_SOURCE_KUBECONFIG, load_hub_configuration
from .inventory import (
    CLUSTERPROFILE_GROUP,
    CLUSTERPROFILE_NAMESPACE,
    CLUSTERPROFILE_PLURAL,
    CLUSTERPROFILE_VERSION,
    PROP_ACTIVE_CLAIMS,
    PROP_CLAIM_P90_MS,
    PROP_HEARTBEAT,
    PROP_MAX_REPLICAS,
    PROP_NODE_PRESSURE,
    PROP_SANDBOX_CAPACITY,
    PROP_WARMPOOL_DEPTH,
    PROP_WARMPOOL_READY,
)

logger = logging.getLogger("agent_sandbox_fleet.publisher")

APPLY_CONTENT_TYPE = "application/apply-patch+yaml"
DEFAULT_FIELD_MANAGER = "agent-sandbox-fleet-member"


class ClusterProfilePublisher:
    """Applies one cluster's capacity onto its ClusterProfile status."""

    def __init__(
        self,
        cluster_name: str,
        *,
        api: Any = None,
        namespace: str = CLUSTERPROFILE_NAMESPACE,
        kubeconfig: str | None = None,
        context: str | None = None,
        token_source: str = TOKEN_SOURCE_KUBECONFIG,
        field_manager: str = DEFAULT_FIELD_MANAGER,
        force: bool = False,
        sandbox_capacity: int | None = None,
        max_replicas: int | None = None,
        status_subresource: bool = True,
        version: str = CLUSTERPROFILE_VERSION,
        request_timeout: float | tuple[float, float] | None = (5.0, 30.0),
    ):
        self.cluster_name = cluster_name
        self._api = api
        # Must match the reader's version. Mismatched versions do not error --
        # with no conversion webhook the apiserver serves the same object under
        # either -- but they DO produce separate managedFields entries, so a
        # version flip can silently hand ownership to a phantom second manager.
        self._version = version
        # (connect, read). The publisher runs on a timer inside the member pod,
        # and the hub is the one endpoint in this system that is routinely
        # unreachable -- separate VPC, private endpoint, no master-global-access.
        # urllib3's default is no timeout at all, so an unreachable hub parks the
        # publish thread until the OS gives up on the SYN, which is minutes. That
        # thread is shared with capacity collection, so the member stops
        # publishing to GCS too -- the hub takes down the path that does not
        # depend on it.
        self._request_timeout = request_timeout
        self._namespace = namespace
        self._kubeconfig = kubeconfig
        self._context = context
        self._token_source = token_source
        self._field_manager = field_manager
        # Opt-in, and per publisher rather than a retry the code performs on its
        # own. When another manager already owns a field we apply, the apiserver
        # returns 409; that propagates. force=True takes the field instead, which
        # is occasionally what you want and never what you want by accident -- so
        # the caller has to say so before the write, not after seeing the error.
        self._force = force
        # A member cannot infer how many sandboxes its cluster can hold, so the
        # operator supplies it. This is what lets the planner derive weights with
        # no weight map at all.
        self._sandbox_capacity = sandbox_capacity
        self._max_replicas = max_replicas
        self._status_subresource = status_subresource

    # -- API plumbing --------------------------------------------------------- #

    def _apply_api(self) -> Any:
        """A CustomObjectsApi whose PATCHes are Server-Side Applies.

        The content type is pinned with a default header because the generated
        client picks `merge-patch+json` on its own and offers no per-call override.
        `__call_api` does `header_params.update(self.default_headers)`, so a
        default header wins over the per-call choice. Dedicated client: pinning
        this globally would corrupt every other request that carries a body.
        """
        if self._api is not None:
            return self._api

        try:
            from kubernetes import client as k8s_client
        except ImportError as e:  # pragma: no cover - dependency guard
            raise RuntimeError(
                "publishing to a hub needs the kubernetes client; "
                "`pip install kubernetes`"
            ) from e

        # The hub is a DIFFERENT cluster from the one this member runs in, so
        # in-cluster config is wrong here by construction — the member needs
        # explicit hub credentials. See hubauth for why gke-metadata exists.
        cfg = load_hub_configuration(
            kubeconfig=self._kubeconfig, context=self._context,
            token_source=self._token_source,
        )
        api_client = k8s_client.ApiClient(configuration=cfg)
        api_client.set_default_header("Content-Type", APPLY_CONTENT_TYPE)
        self._api = k8s_client.CustomObjectsApi(api_client)
        return self._api

    def assert_ssa_configured(self) -> None:
        """Fail loudly if PATCHes would go out as merge-patch instead of apply.

        Worth calling once at startup. The failure this guards is silent: a
        merge-patch accepts `field_manager` without error and simply does not
        establish SSA ownership, so the damage only shows up later as two managers
        trampling each other.
        """
        api = self._apply_api()
        client = getattr(api, "api_client", None)
        headers = getattr(client, "default_headers", {}) or {}
        got = headers.get("Content-Type")
        if got != APPLY_CONTENT_TYPE:
            raise RuntimeError(
                f"ClusterProfile publishing requires Content-Type "
                f"{APPLY_CONTENT_TYPE!r}, got {got!r}. Without it the request is a "
                f"merge-patch and field_manager is accepted but meaningless."
            )

    # -- Body construction ---------------------------------------------------- #

    def build_status(self, report: Mapping[str, Any]) -> dict[str, Any]:
        """Map a CapacityReport-shaped mapping onto ClusterProfile properties."""
        props: list[dict[str, str]] = []

        def put(name: str, value: Any) -> None:
            props.append({"name": name, "value": str(value)})

        # The heartbeat the API does not have. Everything else is capacity.
        put(PROP_HEARTBEAT, report.get("updated_at", ""))
        put(PROP_WARMPOOL_DEPTH, int(report.get("warmpool_depth", 0) or 0))
        put(PROP_WARMPOOL_READY, int(report.get("warmpool_ready", 0) or 0))
        put(PROP_CLAIM_P90_MS, float(report.get("claim_p90_ms", 0.0) or 0.0))

        # Same omit-don't-zero rule as pressure below: 0 in-flight claims is the
        # most attractive value LeastLoaded can see.
        claims = report.get("active_claims")
        if claims is not None:
            put(PROP_ACTIVE_CLAIMS, int(claims))

        # OMIT pressure when it could not be measured. Publishing 0.0 would read
        # as "completely idle" and CapacityAware would then prefer the cluster
        # that failed — the exact bug the CapacityReport docstring records from
        # a density fleet, where the calc failed every cycle at 200k pods.
        pressure = report.get("node_pressure_score")
        if pressure is not None:
            put(PROP_NODE_PRESSURE, float(pressure))

        if self._sandbox_capacity is not None:
            put(PROP_SANDBOX_CAPACITY, int(self._sandbox_capacity))
        if self._max_replicas is not None:
            put(PROP_MAX_REPLICAS, int(self._max_replicas))

        return {"properties": props}

    def build_body(self, report: Mapping[str, Any]) -> dict[str, Any]:
        """The full apply body. Must carry apiVersion/kind/name for SSA."""
        return {
            "apiVersion": f"{CLUSTERPROFILE_GROUP}/{self._version}",
            "kind": "ClusterProfile",
            "metadata": {"name": self.cluster_name, "namespace": self._namespace},
            "status": self.build_status(report),
        }

    # -- The write ------------------------------------------------------------ #

    def publish(self, report: Mapping[str, Any]) -> Any:
        """Server-Side Apply this member's capacity onto its ClusterProfile."""
        api = self._apply_api()
        body = self.build_body(report)
        kwargs = dict(
            group=CLUSTERPROFILE_GROUP,
            version=self._version,
            namespace=self._namespace,
            plural=CLUSTERPROFILE_PLURAL,
            name=self.cluster_name,
            body=body,
            field_manager=self._field_manager,
        )
        if self._request_timeout is not None:
            kwargs["_request_timeout"] = self._request_timeout
        if self._force:
            kwargs["force"] = True

        if self._status_subresource:
            resp = api.patch_namespaced_custom_object_status(**kwargs)
        else:
            # Fixture CRDs that do not declare the status subresource. Applying to
            # the main resource is the only way to land a status on those.
            resp = api.patch_namespaced_custom_object(**kwargs)

        logger.debug("published ClusterProfile status for %s (%d properties)",
                     self.cluster_name, len(body["status"]["properties"]))
        return resp
