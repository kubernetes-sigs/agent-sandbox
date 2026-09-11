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

"""Inventory providers — where the planner learns which clusters exist.

The planner is a pure function of (FleetSpec, inventory snapshot). This module
is the inventory half: everything that turns "what clusters are out there and
how loaded are they" into a `PlannerRegistry` of `PlannerCluster` snapshots.

Two providers:

* :class:`GCSInventory` — the original. Reads `fleet/capacity/<cluster>.json`
  reports published by each fleet-member. Default; no hub cluster required.
* :class:`ClusterProfileInventory` — reads SIG-Multicluster `ClusterProfile`
  CRs (``multicluster.x-k8s.io/v1alpha1``, KEP-4322) from a hub cluster.

Both produce the identical `PlannerRegistry`, so `planner.plan()` and every
selector in `placement.py` are untouched by the choice. That is the whole point
of the split — see "ClusterProfile integration" in perf_goals/FLEET.md.

Known gap: ClusterProfile has no heartbeat field
------------------------------------------------
Our failover rule is "capacity report older than 90s → drop the cluster from
placement". ClusterProfile has no equivalent. The nearest thing is
``status.conditions[ControlPlaneHealthy]``, but a condition's
``lastTransitionTime`` only moves on *transition*: a cluster that went
``True`` an hour ago and then died silently still reads ``True`` with an
hour-old timestamp. Health is not liveness.

Until that is settled upstream we publish a vendor heartbeat property
(:data:`PROP_HEARTBEAT`) and read it in preference to anything else. When it is
absent the provider falls back to trusting the condition and logs a warning;
set ``require_heartbeat=True`` to treat a missing heartbeat as stale instead.
"""

from __future__ import annotations

import datetime as _dt
import logging
from typing import Any, Protocol

from .objectstore import GCS, Paths
from .placement import PlannerCluster, PlannerRegistry

logger = logging.getLogger("agent_sandbox_fleet.inventory")

# --------------------------------------------------------------------------- #
# ClusterProfile API coordinates and our property names.
# --------------------------------------------------------------------------- #

CLUSTERPROFILE_GROUP = "multicluster.x-k8s.io"

# Default served version. Upstream is mid-migration: as of the 2026-08 CRD,
# v1alpha1 is still `storage: true` but ALREADY emits a deprecation warning
# pointing at v1alpha2, which is served with `storage: false`. So:
#
#   v1alpha1 — works on every hub, is the storage version, warns on every call.
#   v1alpha2 — quiet and forward-looking, absent on hubs running an older CRD.
#
# The default stays v1alpha1 because a missing version is a hard failure while
# a deprecation warning is noise, and a managed hub (GKE Fleet) may well ship
# the older CRD. Override per-call once your hub serves v1alpha2; there is no
# conversion webhook, so the two differ only in the apiVersion string unless
# upstream changes the schema.
CLUSTERPROFILE_VERSION = "v1alpha1"
CLUSTERPROFILE_PLURAL = "clusterprofiles"
CLUSTERPROFILE_NAMESPACE = "fleet-system"
CLUSTER_MANAGER_LABEL = "x-k8s.io/cluster-manager"

# Well-known conditions from the ClusterProfile spec.
COND_CONTROL_PLANE_HEALTHY = "ControlPlaneHealthy"
COND_JOINED = "Joined"

# Our properties. The well-known set (`clusterset.k8s.io`, `location`) covers
# identity and topology only — nothing we can schedule on — so capacity signals
# need a vendor prefix. Candidates to propose upstream as well-known capacity
# properties; see FLEET.md.
PROP_PREFIX = "agents.x-k8s.io/"
PROP_HEARTBEAT = PROP_PREFIX + "heartbeat"
PROP_WARMPOOL_DEPTH = PROP_PREFIX + "warmpool-depth"
PROP_WARMPOOL_READY = PROP_PREFIX + "warmpool-ready"
PROP_ACTIVE_CLAIMS = PROP_PREFIX + "active-claims"
PROP_CLAIM_P90_MS = PROP_PREFIX + "claim-p90-ms"
PROP_NODE_PRESSURE = PROP_PREFIX + "node-pressure-score"
PROP_MAX_REPLICAS = PROP_PREFIX + "max-replicas"
PROP_SANDBOX_CAPACITY = PROP_PREFIX + "sandbox-capacity"

# Sentinel age for "no usable report". Matches the value the GCS provider has
# always used, and `cli.py` prints anything over 1e8 as STALE.
STALE_AGE_S = 1e9


class InventoryProvider(Protocol):
    """Turns cluster inventory into a PlannerRegistry.

    `weights` is the operator-authored `cluster_weights` map from the FleetSpec.
    A provider may treat it as authoritative (GCS) or as an override on top of
    weights it derives from real capacity (ClusterProfile).
    """

    def load(self, weights: dict[str, float]) -> PlannerRegistry: ...


# --------------------------------------------------------------------------- #
# Shared helpers.
# --------------------------------------------------------------------------- #

def _now() -> _dt.datetime:
    return _dt.datetime.now(_dt.timezone.utc)


def age_seconds(ts: str | None, now: _dt.datetime | None = None) -> float:
    """Age of an RFC3339 timestamp in seconds. Unparseable or absent → STALE."""
    if not ts:
        return STALE_AGE_S
    now = now or _now()
    try:
        t = _dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except ValueError:
        return STALE_AGE_S
    if t.tzinfo is None:
        t = t.replace(tzinfo=_dt.timezone.utc)
    return max(0.0, (now - t).total_seconds())


def _stale_placeholders(reg: PlannerRegistry, weights: dict[str, float]) -> None:
    """Any cluster the operator named but that never reported gets a stale entry.

    Shared by both providers: placement should shift away from a silent cluster,
    not pretend it doesn't exist (that would hide it from `fleetctl status`).
    """
    for name, w in weights.items():
        if name not in reg.clusters:
            reg.clusters[name] = PlannerCluster(
                name=name, weight=w, report_age_s=STALE_AGE_S,
            )


#: Per-cluster detail entries in the inventory log line. The line exists to
#: discriminate source and freshness, not to be an inventory dump -- the counts
#: in the same line carry the totals.
_LOG_DETAIL_MAX = 12


def _log_registry(reg: PlannerRegistry, source: str) -> None:
    """State which inventory a registry came from, and what it contained.

    Both providers were previously silent on the happy path: they logged only
    exclusions and malformed data. A healthy cycle therefore printed a cluster
    COUNT and nothing about where it came from, so `--inventory=clusterprofile`
    taking no effect and working perfectly produced identical output. Proving
    which source was live needed a falsification run against a live fleet, which
    is an absurd price for a fact the process already knows.

    Staleness is included because the two failure modes look alike from the
    count alone: a cluster present because the hub reported it, and a cluster
    present only as a _stale_placeholders() entry synthesised from the operator's
    configured weights, are both "1 cluster".
    """
    if not reg.clusters:
        logger.info("inventory %s: no clusters", source)
        return

    # Freshness is registry.max_report_age_s (90s), NOT STALE_AGE_S. The latter
    # is a 1e9 SENTINEL meaning "no usable report at all" -- comparing against it
    # calls everything fresh right up to the heat death of the universe. Getting
    # this wrong made the log claim a cluster was fresh in the same cycle the
    # planner excluded it for a stale heartbeat, which is worse than not logging
    # it: placement.fresh() is the function that decides, so this line has to ask
    # the same question it does.
    def _label(c) -> str:
        if c.report_age_s >= STALE_AGE_S:
            return f"{c.name}(NO REPORT)"
        stale = "" if c.report_age_s <= reg.max_report_age_s else ", STALE"
        return f"{c.name}(age={c.report_age_s:.0f}s{stale})"

    fresh = reg.fresh()
    fresh_names = {c.name for c in fresh}
    ordered = sorted(reg.clusters.values(), key=lambda c: c.name)
    eligible = [c for c in ordered if c.name in fresh_names]
    excluded = [c for c in ordered if c.name not in fresh_names]

    # Sample eligible clusters FIRST, then backfill with excluded ones. Measured
    # at 507 clusters on a real hub: enumerating every one emits a ~15KB single
    # line each cycle, which at a 60s loop is most of the planner's log volume.
    # Truncating alphabetically instead would be worse than truncating at all --
    # the first dozen names there were _stale_placeholders() synthesised from
    # the spec's cluster_weights plus synthetic load-test entries, while the one
    # cluster placement actually used sorted past the cut. A sample containing
    # no eligible cluster answers no useful question.
    shown = eligible[:_LOG_DETAIL_MAX]
    shown += excluded[:_LOG_DETAIL_MAX - len(shown)]
    shown.sort(key=lambda c: c.name)
    detail = ", ".join(_label(c) for c in shown)

    omitted = len(ordered) - len(shown)
    if omitted:
        n_no_report = sum(1 for c in excluded if c.report_age_s >= STALE_AGE_S)
        detail += (f", …+{omitted} more (excluded overall: "
                   f"{len(excluded) - n_no_report} stale, "
                   f"{n_no_report} no-report)")

    logger.info("inventory %s: %d clusters, %d placement-eligible (max age %.0fs) — %s",
                source, len(reg.clusters), len(fresh), reg.max_report_age_s, detail)


# --------------------------------------------------------------------------- #
# GCS provider — the original behavior, unchanged.
# --------------------------------------------------------------------------- #

class GCSInventory:
    """Reads `fleet/capacity/<cluster>.json` reports from the fleet bucket.

    This is the default provider and needs no hub cluster: each fleet-member
    publishes its own report and the planner reads them all.
    """

    def __init__(self, gcs: GCS, paths: Paths | None = None):
        self._gcs = gcs
        self._paths = paths or Paths()

    def load(self, weights: dict[str, float]) -> PlannerRegistry:
        reg = PlannerRegistry()
        reports = {
            n: self._gcs.get_json(n)
            for n in self._gcs.list_prefix(self._paths.capacity_prefix)
        }
        now = _now()
        for path, r in reports.items():
            if r is None:
                continue
            # One member writing a bad report must not take the whole planner down.
            # These objects come from N independently-deployed pods; a rolling
            # upgrade mid-write, a truncated object, or an older member schema all
            # land here. Skipping the report leaves the cluster to
            # _stale_placeholders(), which is exactly the "present but unusable"
            # state we want — ineligible for placement, still visible in
            # show-registry — rather than an unplaceable fleet.
            if not isinstance(r, dict):
                logger.warning("skipping capacity report %s: not a JSON object", path)
                continue
            cluster = r.get("cluster")
            if not isinstance(cluster, str) or not cluster:
                logger.warning("skipping capacity report %s: no 'cluster' field", path)
                continue
            try:
                reg.clusters[cluster] = PlannerCluster(
                    name=cluster,
                    weight=weights.get(cluster, 1.0),
                    warmpool_depth=int(r.get("warmpool_depth", 0)),
                    warmpool_ready=int(r.get("warmpool_ready", 0)),
                    # Absent or null stays None — same rule as pressure below.
                    active_claims=(
                        None if r.get("active_claims") is None
                        else int(r["active_claims"])
                    ),
                    claim_p90_ms=float(r.get("claim_p90_ms", 0.0)),
                    # None (or absent) means the member could not measure pressure.
                    # Do NOT coerce to 0.0 — that reads as "idle" and CapacityAware
                    # would then prefer the cluster that failed to report.
                    node_pressure_score=(
                        None if r.get("node_pressure_score") is None
                        else float(r["node_pressure_score"])
                    ),
                    report_age_s=age_seconds(r.get("updated_at"), now),
                )
            except (TypeError, ValueError) as exc:
                # No cleanup needed: PlannerCluster is built in a single expression,
                # so a raise while evaluating any field means the assignment into
                # reg.clusters never ran. Keep it that way -- building the object
                # field by field would let a rejected report leave partial state
                # behind, which the planner would then treat as measured.
                logger.warning("skipping capacity report %s for cluster %s: %s",
                               path, cluster, exc)
        _stale_placeholders(reg, weights)
        _log_registry(reg, f"gcs://{self._gcs.bucket_name}/{self._paths.capacity_prefix}")
        return reg


# --------------------------------------------------------------------------- #
# ClusterProfile provider.
# --------------------------------------------------------------------------- #

class ClusterProfileInventory:
    """Reads `ClusterProfile` CRs from a hub cluster.

    Mapping onto `PlannerCluster`:

    ==========================  ===========================================
    PlannerCluster              ClusterProfile
    ==========================  ===========================================
    ``name``                    ``metadata.name``
    ``weight``                  ``cluster_weights`` override, else derived
                                from ``PROP_SANDBOX_CAPACITY``, else 1.0
    ``warmpool_depth`` etc.     ``status.properties[]`` (vendor-prefixed)
    ``report_age_s``            ``PROP_HEARTBEAT``; see module docstring
    ``max_replicas``            ``PROP_MAX_REPLICAS`` when published
    ==========================  ===========================================

    A cluster whose ``ControlPlaneHealthy`` or ``Joined`` condition is not
    ``True`` is marked stale, which drops it from `PlannerRegistry.fresh()` and
    therefore from placement — the same outcome as a missing capacity report.
    """

    def __init__(
        self,
        api: Any | None = None,
        *,
        namespace: str = CLUSTERPROFILE_NAMESPACE,
        kubeconfig: str | None = None,
        context: str | None = None,
        token_source: str = "kubeconfig",
        cluster_manager: str | None = None,
        require_heartbeat: bool = False,
        version: str = CLUSTERPROFILE_VERSION,
    ):
        """
        Parameters
        ----------
        api
            A ``kubernetes.client.CustomObjectsApi`` bound to the hub. Built lazily
            from ``kubeconfig``/``context`` when omitted. Injectable for tests.
        version
            Served ClusterProfile API version. See
            :data:`CLUSTERPROFILE_VERSION` for why the default is the deprecated
            one. Must match whatever the publisher writes.
        namespace
            Namespace holding the ClusterProfile CRs. ClusterProfile is a
            namespaced resource; the spec's example uses ``fleet-system``.
        token_source
            ``kubeconfig`` to authenticate with whatever the file carries, or
            ``gke-metadata`` to authenticate as the pod's Workload Identity
            service account. The latter lets the hub kubeconfig be a
            credential-free ConfigMap; see :mod:`agent_sandbox_fleet.hubauth`.
        cluster_manager
            When set, only profiles labelled with this cluster manager are read
            (``x-k8s.io/cluster-manager``). Use it when one hub inventories several
            fleets.
        require_heartbeat
            Treat a profile with no :data:`PROP_HEARTBEAT` property as stale rather
            than trusting ``ControlPlaneHealthy``. Off by default so that a
            standards-pure hub still works; turn it on once members publish
            heartbeats. See the module docstring for why this switch exists.
        """
        self._api = api
        self._namespace = namespace
        self._kubeconfig = kubeconfig
        self._context = context
        self._token_source = token_source
        self._cluster_manager = cluster_manager
        self._require_heartbeat = require_heartbeat
        self._version = version
        self._warned_no_heartbeat: set[str] = set()

    @property
    def clusters_without_heartbeat(self) -> frozenset[str]:
        """Clusters whose freshness came from a condition, not a heartbeat.

        Their `report_age_s` is a synthetic 0.0, which means "not measured" and
        NOT "measured a moment ago" — so such a cluster reads as the freshest
        thing in the fleet precisely because it tells us the least. Harmless while
        `fresh()` only threshold-tests the value; actively wrong for anything that
        ranks or sorts on it. Same trap as coercing an absent node-pressure to
        0.0, which is why that one is kept as None.
        """
        return frozenset(self._warned_no_heartbeat)

    # -- API plumbing ------------------------------------------------------- #

    def _get_api(self) -> Any:
        if self._api is not None:
            return self._api
        try:
            from kubernetes import client as k8s_client
        except ImportError as e:  # pragma: no cover - dependency guard
            raise RuntimeError(
                "the clusterprofile inventory needs the kubernetes client; "
                "`pip install kubernetes`"
            ) from e
        from .hubauth import load_hub_configuration

        cfg = load_hub_configuration(
            kubeconfig=self._kubeconfig, context=self._context,
            token_source=self._token_source,
        )
        self._api = k8s_client.CustomObjectsApi(k8s_client.ApiClient(configuration=cfg))
        return self._api

    def list_profiles(self) -> list[dict[str, Any]]:
        """Raw ClusterProfile objects from the hub. Split out for `fleetctl`."""
        kwargs: dict[str, Any] = {}
        if self._cluster_manager:
            kwargs["label_selector"] = f"{CLUSTER_MANAGER_LABEL}={self._cluster_manager}"
        resp = self._get_api().list_namespaced_custom_object(
            group=CLUSTERPROFILE_GROUP,
            version=self._version,
            namespace=self._namespace,
            plural=CLUSTERPROFILE_PLURAL,
            **kwargs,
        )
        return list(resp.get("items", []))

    # -- InventoryProvider -------------------------------------------------- #

    def load(self, weights: dict[str, float]) -> PlannerRegistry:
        reg = PlannerRegistry()
        now = _now()
        for profile in self.list_profiles():
            cluster = self._to_planner_cluster(profile, weights, now)
            if cluster is not None:
                reg.clusters[cluster.name] = cluster
        _stale_placeholders(reg, weights)
        selector = (f"{CLUSTER_MANAGER_LABEL}={self._cluster_manager}"
                    if self._cluster_manager else "<no cluster-manager selector>")
        _log_registry(
            reg,
            f"clusterprofile {self._version} ns={self._namespace} "
            f"selector={selector}")
        return reg

    def _to_planner_cluster(
        self, profile: dict[str, Any], weights: dict[str, float],
        now: _dt.datetime,
    ) -> PlannerCluster | None:
        name = (profile.get("metadata") or {}).get("name")
        if not name:
            logger.warning("skipping ClusterProfile with no metadata.name")
            return None

        status = profile.get("status") or {}
        props = _properties(status)
        conds = _conditions(status)

        pressure = _float_prop(props, PROP_NODE_PRESSURE)
        max_replicas = _int_prop(props, PROP_MAX_REPLICAS)

        return PlannerCluster(
            name=name,
            weight=self._weight_for(name, weights, props),
            max_replicas=max_replicas if max_replicas and max_replicas > 0 else None,
            warmpool_depth=_int_prop(props, PROP_WARMPOOL_DEPTH) or 0,
            warmpool_ready=_int_prop(props, PROP_WARMPOOL_READY) or 0,
            # Absent stays None, never 0 — see GCSInventory for why.
            active_claims=_int_prop(props, PROP_ACTIVE_CLAIMS),
            claim_p90_ms=_float_prop(props, PROP_CLAIM_P90_MS) or 0.0,
            # Absent stays None, never 0.0 — see GCSInventory for why.
            node_pressure_score=pressure,
            report_age_s=self._freshness(name, props, conds, now),
        )

    def _weight_for(
        self, name: str, weights: dict[str, float], props: dict[str, str],
    ) -> float:
        """Operator override wins; otherwise derive from published capacity.

        Deriving is the point of this provider: `cluster_weights` is a human
        multiplying node counts by hand, which is exactly what missed cluster F's
        undersized control plane during a density run.
        """
        if name in weights:
            return weights[name]
        capacity = _float_prop(props, PROP_SANDBOX_CAPACITY)
        if capacity and capacity > 0:
            return capacity
        # A cluster that is Joined, ControlPlaneHealthy and heartbeating but not
        # publishing capacity is the WORST case, not a benign one: it stays
        # placement-eligible and takes a weight of 1.0, so hamilton_split hands it
        # a rounding error instead of a share. Measured on the 1M spec, one such
        # cluster took 500 sandboxes where 129,000 were intended and the fleet came
        # in 12% short -- with a full plan, six placed clusters, and no error.
        # Silence is what makes it dangerous, hence the warning; it is not fatal
        # here because a weighted-1.0 cluster is still a legitimate operator choice
        # when weights are pinned rather than derived.
        logger.warning(
            "cluster %s publishes no %s -- defaulting weight to 1.0. In a "
            "hub-driven spec (cluster_weights empty) this all but removes it from "
            "the budget split while leaving it placement-eligible.",
            name, PROP_SANDBOX_CAPACITY)
        return 1.0

    def _freshness(
        self, name: str, props: dict[str, str], conds: dict[str, dict],
        now: _dt.datetime,
    ) -> float:
        """Report age in seconds, or STALE_AGE_S to drop the cluster.

        Order of authority:
          1. An explicitly unhealthy/unjoined condition is fatal.
          2. Our heartbeat property, when published.
          3. Otherwise trust ControlPlaneHealthy and warn (the API gap).
        """
        for cond_type in (COND_CONTROL_PLANE_HEALTHY, COND_JOINED):
            cond = conds.get(cond_type)
            if cond is not None and str(cond.get("status")) != "True":
                logger.info("cluster %s: %s=%s → excluded from placement",
                            name, cond_type, cond.get("status"))
                return STALE_AGE_S

        heartbeat = props.get(PROP_HEARTBEAT)
        if heartbeat:
            return age_seconds(heartbeat, now)

        if self._require_heartbeat:
            logger.info("cluster %s: no %s property and require_heartbeat is set "
                        "→ excluded from placement", name, PROP_HEARTBEAT)
            return STALE_AGE_S

        if COND_CONTROL_PLANE_HEALTHY not in conds:
            logger.warning("cluster %s: no %s property and no %s condition; "
                           "cannot establish liveness → excluded",
                           name, PROP_HEARTBEAT, COND_CONTROL_PLANE_HEALTHY)
            return STALE_AGE_S

        # ControlPlaneHealthy=True but no heartbeat. lastTransitionTime is NOT an
        # age — it stops moving once the condition settles — so we cannot compute a
        # real staleness here. Trust the condition and say so, once per cluster.
        if name not in self._warned_no_heartbeat:
            self._warned_no_heartbeat.add(name)
            logger.warning(
                "cluster %s publishes no %s property; falling back to "
                "%s=True. This detects an unhealthy cluster but NOT a silently dead "
                "one, because a condition's lastTransitionTime does not refresh. "
                "Publish a heartbeat property, or pass require_heartbeat=True.",
                name, PROP_HEARTBEAT, COND_CONTROL_PLANE_HEALTHY,
            )
        return 0.0


# --------------------------------------------------------------------------- #
# Property parsing. Properties are stringly-typed {name, value} pairs, and a
# malformed one must not take down a whole planning pass.
# --------------------------------------------------------------------------- #

def _properties(status: dict[str, Any]) -> dict[str, str]:
    out: dict[str, str] = {}
    for p in status.get("properties") or []:
        name = p.get("name")
        if name is not None:
            out[str(name)] = str(p.get("value", ""))
    return out


def _conditions(status: dict[str, Any]) -> dict[str, dict]:
    out: dict[str, dict] = {}
    for c in status.get("conditions") or []:
        ctype = c.get("type")
        if ctype is not None:
            out[str(ctype)] = c
    return out


# ClusterProfile properties are free-form strings written by whatever publishes
# the profile, so treat them as untrusted. float() happily accepts "inf" and
# "nan": inf would win every least-loaded comparison in placement, nan loses
# every one of them, and int(float("inf")) raises OverflowError rather than the
# ValueError the obvious `except` catches. Reject non-finite values here so a
# malformed profile degrades to "unset" instead of poisoning the plan.
def _finite(raw: str, key: str) -> float | None:
    try:
        val = float(raw)
    except (ValueError, OverflowError):
        logger.warning("property %s=%r is not a number; ignoring", key, raw)
        return None
    if val != val or val in (float("inf"), float("-inf")):
        logger.warning("property %s=%r is not finite; ignoring", key, raw)
        return None
    return val


def _int_prop(props: dict[str, str], key: str) -> int | None:
    raw = props.get(key)
    if raw is None or raw == "":
        return None
    val = _finite(raw, key)
    return None if val is None else int(val)


def _float_prop(props: dict[str, str], key: str) -> float | None:
    raw = props.get(key)
    if raw is None or raw == "":
        return None
    return _finite(raw, key)


# --------------------------------------------------------------------------- #
# Factory.
# --------------------------------------------------------------------------- #

def get_inventory(
    kind: str,
    *,
    gcs: GCS | None = None,
    paths: Paths | None = None,
    hub_kubeconfig: str | None = None,
    hub_context: str | None = None,
    hub_namespace: str = CLUSTERPROFILE_NAMESPACE,
    hub_token_source: str = "kubeconfig",
    cluster_manager: str | None = None,
    require_heartbeat: bool = False,
    version: str = CLUSTERPROFILE_VERSION,
) -> InventoryProvider:
    """Build a provider by name. `kind` is 'gcs' or 'clusterprofile'."""
    if kind == "gcs":
        if gcs is None:
            raise ValueError("the gcs inventory needs a GCS client")
        return GCSInventory(gcs, paths)
    if kind == "clusterprofile":
        return ClusterProfileInventory(
            namespace=hub_namespace,
            kubeconfig=hub_kubeconfig,
            context=hub_context,
            token_source=hub_token_source,
            cluster_manager=cluster_manager,
            require_heartbeat=require_heartbeat,
            version=version,
        )
    raise ValueError(f"unknown inventory {kind!r}; choose from gcs, clusterprofile")
