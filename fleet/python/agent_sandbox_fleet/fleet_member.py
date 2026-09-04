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

"""Fleet-member — per-cluster Python daemon built on the agent-sandbox SDK.

Rewritten from the previous Go binary at cmd/fleet-member to reuse the
k8s-agent-sandbox SDK where it fits, so future SDK features come for free.

Concrete SDK use:
- `SandboxClient.list_all_sandboxes()` — used to count active SandboxClaims
  for the capacity report's active_claims signal.

Where the SDK has nothing (SandboxTemplate + SandboxWarmPool CRUD), we drop
to `kubernetes.client.CustomObjectsApi` — this matches the
`agent_sandbox_rl/resources.py` pattern.

Two concurrent loops:
1. reconcile — poll fleet/assignments.json every --reconcile-interval,
   ensure local SandboxWarmPool objects match (SandboxTemplates are
   operator-managed; the fleet-member only verifies existence).
2. capacity_report — publish fleet/capacity/<cluster>.json every
   --capacity-interval with warmpool depth/ready, active_claims,
   node_pressure_score.

Entrypoint:
  python -m agent_sandbox_fleet.fleet_member \\
      --cluster-name=$CLUSTER_NAME --bucket=$FLEET_BUCKET
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import logging
import os
import signal
import sys
import threading
import time
from dataclasses import asdict, dataclass, field
from typing import Any, Iterator, Mapping

from kubernetes import client as k8s, config as k8s_config
from kubernetes.client.exceptions import ApiException

# SDK — the whole reason for the rewrite. Import fails hard here (not lazy)
# because fleet-member cannot run without it.
from k8s_agent_sandbox import SandboxClient

from .argtypes import positive_float
from .objectstore import GCS, Paths

log = logging.getLogger("agent_sandbox_fleet.fleet_member")

# ----------------------------------------------------------------------------
# Constants matching the Go types in pkg/fleet/types.go. The Go wire schemas
# still live there for library consumers (external Go schedulers); this Python
# impl mirrors them for the system consumer.
# ----------------------------------------------------------------------------

CRD_GROUP = "extensions.agents.x-k8s.io"
CRD_VERSION = "v1beta1"
POOL_PLURAL = "sandboxwarmpools"
TEMPLATE_PLURAL = "sandboxtemplates"

# Page size for any list that scales with cluster size. Without a limit the
# kubernetes client materialises the WHOLE collection in memory: at 200k pods
# that measured ~60 KB/pod in Python objects (~12 GB), which OOMed the member
# on a 200k-pod density fleet. Paging bounds peak memory to one page.
PAGE_LIMIT = 1000

MANAGED_LABEL = "fleet.agent-sandbox.io/managed"
POOL_NAME_LABEL = "fleet.agent-sandbox.io/pool"
GENERATION_ANNOTATION = "fleet.agent-sandbox.io/assignment-generation"

# Payload shapes this member understands. Must stay in sync with
# planner.SCHEMA_VERSION; duplicated rather than imported because the member
# image deliberately does not depend on the planner's pydantic stack.
SCHEMA_VERSION = 1
SUPPORTED_SCHEMA_VERSIONS = frozenset({SCHEMA_VERSION})


# ----------------------------------------------------------------------------
# Wire types — mirror pkg/fleet/types.go. Kept as plain dataclasses (not
# pydantic) to keep runtime deps minimal on the fleet-member image.
# ----------------------------------------------------------------------------

@dataclass
class AssignmentPool:
    template: str
    warmpool: str
    replicas: int
    # `image` mirrors ModelSpec.image on the planner side — optional.
    # Only populated by the planner when the source FleetSpec set image
    # (needed for image-affinity placement + registry_rewrite). Set to
    # None or omitted from the JSON for capacity-aware / least-loaded /
    # round-robin / capacity-weighted policies.
    image: str | None = None

    @classmethod
    def from_json(cls, obj: Mapping[str, Any]) -> "AssignmentPool":
        """Build a pool from one entry of assignments.json, ignoring unknown keys.

        A dataclass __init__ raises TypeError on an unexpected keyword, so a
        plain AssignmentPool(**obj) means any field a NEWER planner adds breaks
        every OLDER member that reads the plan. That is the normal state of a
        rolling update -- planner and members are separate deployments and the
        planner is a single writer, so it is usually the one rolled first.

        The failure is worse than it looks: this parse runs inside
        _reconcile_once, whose caller catches Exception and retries next tick.
        So the member does not crash, it does not go unready, and it does not
        stop reporting capacity -- it just never applies another plan, while
        logging one traceback every interval. Dropping the unknown key keeps the
        member on the fields it does understand, which is strictly better than
        being pinned to the last generation it could parse.

        Unknown keys are logged once each, because silently ignoring a field the
        planner meant to act on is its own kind of wrong -- the member is
        obeying a plan it only partly understood, and the log line is the only
        signal that a member needs upgrading. A MISSING required field still
        raises: that is a malformed or truncated plan, not a newer one, and
        applying half of it would be worse than retrying.
        """
        extra = [k for k in obj if k not in cls.__dataclass_fields__]
        for key in extra:
            if key not in _UNKNOWN_POOL_FIELDS:
                _UNKNOWN_POOL_FIELDS.add(key)
                log.warning(
                    "assignments.json pool has unknown field %r; ignoring it. "
                    "This member is older than the planner that wrote the plan.",
                    key,
                )
        return cls(**{
            k: v for k, v in obj.items() if k in cls.__dataclass_fields__
        })


# Unknown-field names already logged, so a per-tick reconcile does not emit the
# same warning every interval for the life of the pod.
_UNKNOWN_POOL_FIELDS: set[str] = set()


@dataclass
class ClusterAssignment:
    pools: list[AssignmentPool] = field(default_factory=list)


@dataclass
class Assignments:
    schema_version: int = SCHEMA_VERSION
    generation: int = 0
    updated_at: str = ""
    clusters: dict[str, ClusterAssignment] = field(default_factory=dict)


@dataclass
class CapacityReport:
    cluster: str = ""
    updated_at: str = ""
    generation_observed: int = 0
    warmpool_depth: int = 0
    warmpool_ready: int = 0
    # None == NOT MEASURED (light mode, or the SDK list failed), which is NOT
    # the same as 0 == no in-flight claims. Same hazard as the pressure score
    # below: LeastLoaded ranks on active_claims first, so a cluster that
    # publishes 0 because its list blew up wins every tiebreak and attracts
    # the placement it is least able to serve.
    active_claims: int | None = None
    claim_p90_ms: float = 0.0  # v2 — Prometheus scrape deferred
    # None == NOT MEASURED, which is NOT the same as 0.0 == no pressure.
    # It was 0.0-on-failure, and that is actively dangerous: a cluster whose
    # pressure calc blew up published "completely idle" and CapacityAware
    # then preferred it. Measured on a density fleet — in `full` mode at 200k
    # pods the calc failed EVERY cycle (410 Gone) and reported 0.0 each time.
    node_pressure_score: float | None = None
    reported_pools: list[str] = field(default_factory=list)


# ----------------------------------------------------------------------------
# FleetMember — the orchestrator.
# ----------------------------------------------------------------------------

class FleetMember:
    """Per-cluster fleet member. Reuses SandboxClient for claim ops.

    Not safe for concurrent Run() calls; one instance per cluster process.
    """

    def __init__(
        self,
        cluster_name: str,
        bucket: str,
        namespace: str = "multi-cluster-fleet",
        reconcile_interval: float = 30.0,
        capacity_interval: float = 30.0,
        capacity_detail: str = "full",
        paths: Paths | None = None,
        hub_publisher: Any = None,
    ):
        self.cluster_name = cluster_name
        self.namespace = namespace
        self.reconcile_interval = reconcile_interval
        self.capacity_interval = capacity_interval
        self.capacity_detail = capacity_detail
        self.paths = paths or Paths()
        # Optional ClusterProfilePublisher. Additive: GCS stays the transport
        # the planner reads by default, so a broken hub degrades to exactly
        # today's behavior rather than taking the member down.
        self.hub_publisher = hub_publisher

        # K8s client init — in-cluster first, fall back to local kubeconfig.
        # Matches k8s_helper.py's own auto-detect logic.
        try:
            k8s_config.load_incluster_config()
            log.info("loaded in-cluster kube config")
        except k8s_config.ConfigException:
            k8s_config.load_kube_config()
            log.info("loaded local kube config")

        self.core_v1 = k8s.CoreV1Api()
        self.custom_objects = k8s.CustomObjectsApi()
        # SDK client — used only for list_all_sandboxes() in capacity_report.
        # Constructing here so the auto-config path runs once at startup.
        self.sandbox_client = SandboxClient()

        self.gcs = GCS(bucket)
        log.info(
            "fleet-member init cluster=%s namespace=%s bucket=%s",
            cluster_name, namespace, bucket,
        )

        # State tracked between reconciles.
        self._last_etag = ""
        self._last_assignment: Assignments | None = None
        # Armed whenever a reconcile pass does not fully apply, so the etag
        # short-circuit below does not strand a half-reconciled cluster.
        self._retry_pending = False
        self._stop = threading.Event()

    # -- Lifecycle -----------------------------------------------------------

    def run(self) -> None:
        """Start all loops. Blocks until SIGINT/SIGTERM."""
        for sig in (signal.SIGINT, signal.SIGTERM):
            signal.signal(sig, self._on_signal)

        threads = [
            threading.Thread(target=self._reconcile_loop, name="reconcile", daemon=True),
            threading.Thread(target=self._capacity_loop, name="capacity", daemon=True),
        ]
        for t in threads:
            t.start()

        log.info("fleet-member running; waiting for shutdown signal")
        self._stop.wait()
        log.info("shutdown signal received; loops will exit at next tick")

    def _on_signal(self, signum: int, frame: Any) -> None:
        self._stop.set()

    # -- Reconcile loop ------------------------------------------------------

    def _reconcile_loop(self) -> None:
        """Poll assignments.json; reconcile local templates + warmpools.

        Same shape as _capacity_loop, and for the same reason: every pass goes
        through the guard, INCLUDING the first. The first call used to sit
        outside the try/except, so a startup failure -- a GCS IAM denial, an
        apiserver error, a malformed assignments.json -- propagated out of the
        thread and killed it. run() keeps blocking on self._stop, so the pod
        stays 1/1 Running and Ready and never reconciles again.
        """
        # Fire once immediately so startup doesn't wait a full interval.
        while True:
            try:
                self._reconcile_once()
            except Exception:
                log.exception("reconcile pass failed; will retry next tick")
            if self._stop.wait(self.reconcile_interval):
                return

    def _reconcile_once(self) -> None:
        assignments, changed = self._fetch_assignments()
        if assignments is None:
            # Refused payload with no last-good assignment to fall back on
            # (fresh process start mid schema rollout). Reconciling would
            # treat the refusal as an empty plan -- the first-pass clause
            # below fires, the orphan sweep deletes every managed pool, and
            # the empty set gets cached as last-good. Skip the pass; whatever
            # the previous pod applied keeps serving.
            return
        # Re-run when the object changed, on the very first pass, or when the
        # previous pass did not fully apply. Without that last clause a pass
        # that fails partway -- a missing SandboxTemplate, an ApiException out
        # of _list_managed_pool_names -- leaves the cluster half-reconciled
        # and the stable etag then suppresses every retry until somebody
        # rewrites assignments.json. At fleet scale that is a cluster silently
        # serving the wrong pool set for as long as the plan holds steady.
        if (not changed and self._last_assignment is not None
                and not self._retry_pending):
            return

        # Assume failure. Cleared only after every pool has been applied and
        # every orphan deleted, so an exception anywhere between here and the
        # end of the method leaves the retry armed.
        self._retry_pending = True
        self._last_assignment = assignments

        local = assignments.clusters.get(self.cluster_name, ClusterAssignment())
        desired = {p.warmpool: p for p in local.pools}

        # 1. For each pool, verify the referenced SandboxTemplate exists
        #    (operator-managed, not created by the fleet-member — see
        #    ARCHITECTURE.md "Responsibility split"). If missing, skip pool
        #    reconcile and log loudly so the operator notices.
        applied = 0
        skipped = 0
        for pool in desired.values():
            if not self._template_exists(pool.template):
                log.error(
                    "SandboxTemplate %s/%s NOT FOUND — cannot create warmpool %s. "
                    "Operator must `kubectl apply` the template on this cluster "
                    "before fleetctl apply.",
                    self.namespace, pool.template, pool.warmpool,
                )
                skipped += 1
                continue
            try:
                self._ensure_warmpool(assignments.generation, pool)
                applied += 1
            except ApiException:
                log.exception("ensure warmpool=%s failed", pool.warmpool)
                skipped += 1

        # 2. Delete orphaned pools.
        for existing_name in self._list_managed_pool_names():
            if existing_name not in desired:
                log.info("deleting orphaned warmpool %s", existing_name)
                try:
                    self.custom_objects.delete_namespaced_custom_object(
                        group=CRD_GROUP, version=CRD_VERSION, namespace=self.namespace,
                        plural=POOL_PLURAL, name=existing_name,
                    )
                except ApiException as e:
                    if e.status != 404:
                        log.warning("delete warmpool %s: %s", existing_name, e.reason)

        # Skipped pools are a retryable condition, not a terminal one: the
        # usual cause is a SandboxTemplate the operator has not applied yet,
        # and once they do the next tick should pick it up on its own.
        self._retry_pending = skipped > 0

        log.info(
            "assignment applied generation=%d pools_applied=%d skipped=%d",
            assignments.generation, applied, skipped,
        )

    def _fetch_assignments(self) -> tuple[Assignments | None, bool]:
        """Return (assignments, changed). `changed` is False if etag matched.

        `assignments` is None only when the payload was refused AND no plan
        has been parsed since this process started, so there is nothing
        last-good to keep serving. The caller must skip the pass outright.
        """
        try:
            obj_bytes, etag = self.gcs.get_with_etag(self.paths.assignments)
        except FileNotFoundError:
            # Drop the cached etag too. Reporting changed=True while leaving
            # _last_etag set makes every subsequent tick report changed=True
            # again, so the member re-reconciles to empty forever; and a later
            # object that happens to hash back to the old etag would then be
            # read as unchanged and silently skipped.
            was_present = self._last_etag != ""
            self._last_etag = ""
            return Assignments(), was_present
        if etag == self._last_etag:
            return self._last_assignment or Assignments(), False
        raw = json.loads(obj_bytes.decode())

        # Compatibility gate, checked BEFORE anything else is read out of the
        # payload. An unknown schema_version is not an ordering problem, so a
        # higher `generation` says nothing about whether these bytes can be
        # acted on. Refusing is the safe direction and the reason this check
        # exists at all: without it, a payload this member cannot interpret
        # yields no clusters, an empty pool set for this cluster, and an empty
        # pool set means "drop everything" -- a schema bump would tear down the
        # fleet it was rolling out to. Keep serving the current pools instead.
        #
        # Unknown FIELDS are still ignored (AssignmentPool.from_json), so this
        # only fires on a deliberate, breaking bump.
        version = raw.get("schema_version", SCHEMA_VERSION)
        if version not in SUPPORTED_SCHEMA_VERSIONS:
            # Deliberately do NOT cache the etag: re-reading each tick keeps the
            # log loud for as long as the fleet is stuck, and picks up a
            # corrected plan on the next interval rather than after a restart.
            if self._last_assignment is None:
                # Fresh start (e.g. a pod restart mid schema rollout): there is
                # no last-good assignment to keep serving, and substituting an
                # empty Assignments() would read as "tear everything down" to
                # the caller -- the exact teardown this gate exists to prevent.
                log.error(
                    "REFUSING assignments.json: schema_version=%s, this member "
                    "understands %s. It was written by a newer planner, and no "
                    "plan has been parsed since this process started, so NOT "
                    "reconciling at all -- pools already on the cluster are "
                    "left untouched; upgrade this member's image.",
                    version, sorted(SUPPORTED_SCHEMA_VERSIONS),
                )
                return None, False
            log.error(
                "REFUSING assignments.json: schema_version=%s, this member "
                "understands %s. It was written by a newer planner. Keeping the "
                "%d pool(s) currently applied and NOT reconciling; upgrade this "
                "member's image.",
                version, sorted(SUPPORTED_SCHEMA_VERSIONS),
                len(self._last_assignment.clusters.get(
                    self.cluster_name, ClusterAssignment()).pools),
            )
            return self._last_assignment, False

        self._last_etag = etag
        clusters = {
            name: ClusterAssignment(
                pools=[
                    AssignmentPool.from_json(p) for p in body.get("pools", [])
                ]
            )
            for name, body in raw.get("clusters", {}).items()
        }
        return Assignments(
            schema_version=version,
            generation=raw.get("generation", 0),
            updated_at=raw.get("updated_at", ""),
            clusters=clusters,
        ), True

    # -- Reconcile helpers ---------------------------------------------------

    def _template_exists(self, template_name: str) -> bool:
        """Verify an operator-managed SandboxTemplate exists in the namespace.

        The fleet-member does NOT create or mutate SandboxTemplates — those
        are the operator's artifact (podspec, resources, env, volumes,
        security context — everything a real workload needs). The
        FleetSpec only references templates by name; if a referenced
        template isn't present on this cluster, we log and skip the pool
        (documented in ARCHITECTURE.md "Responsibility split").

        Returns True if present, False on 404. Any other API error raises.
        """
        try:
            self.custom_objects.get_namespaced_custom_object(
                group=CRD_GROUP, version=CRD_VERSION, namespace=self.namespace,
                plural=TEMPLATE_PLURAL, name=template_name,
            )
            return True
        except ApiException as e:
            if e.status == 404:
                return False
            raise

    def _ensure_warmpool(self, generation: int, pool: AssignmentPool) -> None:
        labels = self._labels_for(pool)
        annotations = {GENERATION_ANNOTATION: str(generation)}
        body = {
            "apiVersion": f"{CRD_GROUP}/{CRD_VERSION}",
            "kind": "SandboxWarmPool",
            "metadata": {
                "name": pool.warmpool,
                "namespace": self.namespace,
                "labels": labels,
                "annotations": annotations,
            },
            "spec": {
                "sandboxTemplateRef": {"name": pool.template},
                "replicas": int(pool.replicas),
            },
        }
        self._create_or_update(POOL_PLURAL, pool.warmpool, body)

    def _create_or_update(self, plural: str, name: str, body: dict) -> None:
        try:
            existing = self.custom_objects.get_namespaced_custom_object(
                group=CRD_GROUP, version=CRD_VERSION, namespace=self.namespace,
                plural=plural, name=name,
            )
        except ApiException as e:
            if e.status != 404:
                raise
            self.custom_objects.create_namespaced_custom_object(
                group=CRD_GROUP, version=CRD_VERSION, namespace=self.namespace,
                plural=plural, body=body,
            )
            return
        # Existing — preserve resourceVersion so update() is atomic.
        body["metadata"]["resourceVersion"] = existing["metadata"]["resourceVersion"]
        # Merge labels/annotations so we don't wipe unrelated keys someone else added.
        for key in ("labels", "annotations"):
            merged = dict(existing.get("metadata", {}).get(key) or {})
            merged.update(body["metadata"].get(key) or {})
            body["metadata"][key] = merged
        self.custom_objects.replace_namespaced_custom_object(
            group=CRD_GROUP, version=CRD_VERSION, namespace=self.namespace,
            plural=plural, name=name, body=body,
        )

    def _iter_managed_pools(self) -> Iterator[dict[str, Any]]:
        """Yield every managed SandboxWarmPool in the namespace, a page at a time.

        THE only place this collection is listed. Both readers -- the orphan
        sweep in _reconcile_once and the depth/ready rollup in _collect_capacity
        -- go through here, because two copies of a paged walk is how one of
        them ends up unpaged again.

        Paged for the same reason as the pod walk in _node_pressure, and NOT for
        the reason it looks like: an unbounded list is not truncated. Omitting
        `limit` makes the apiserver return the entire collection in one
        response, so both readers were always complete -- it is the single
        response that is the problem. One planner generation can assign a pool
        per model, and the density fleet ran 500 of them per cluster; that is an
        unbounded etcd range read plus a multi-megabyte body on every reconcile
        tick AND every capacity tick, on the same apiserver the sandbox creates
        are queued behind. On a starved control plane (see cluster F) it is also
        the shape of request that times out -- and a timeout DOES lose the work:
        the orphan sweep is skipped, or the capacity report goes unwritten and
        the planner ages this cluster out of placement entirely.

        A generator, not a list, so a caller that only needs an aggregate
        (_collect_capacity sums four scalars) holds one page rather than every
        pool body at once. That is the whole point of PAGE_LIMIT; returning
        list[dict] here would have paged the wire and then rebuilt the
        unbounded allocation in memory.

        Deliberately no 410-Gone restart, unlike the pod walk. That one pages
        through six figures of objects and can outlive a compaction window; this
        one is a few pages at worst, and both callers already treat a raised
        ApiException as "this tick did not finish" -- the sweep leaves
        _retry_pending armed, the capacity loop retries next interval.
        """
        cont: str | None = None
        while True:
            resp = self.custom_objects.list_namespaced_custom_object(
                group=CRD_GROUP, version=CRD_VERSION, namespace=self.namespace,
                plural=POOL_PLURAL,
                label_selector=f"{MANAGED_LABEL}=true",
                limit=PAGE_LIMIT,
                _continue=cont,
            )
            yield from resp.get("items", [])
            # Custom objects come back as plain dicts, so this is
            # metadata["continue"] -- not the `page.metadata._continue`
            # attribute the typed list calls return.
            cont = (resp.get("metadata") or {}).get("continue") or None
            if not cont:
                return

    def _list_managed_pool_names(self) -> list[str]:
        """Managed pool names, fully materialised before the caller acts on them.

        The orphan sweep deletes while iterating this, so it must not be lazy:
        a generator would interleave deletes with the paged walk, and a delete
        landing between two pages shifts the remaining items -- the classic
        skip-every-other-element bug, except silent, since the sweep would just
        leave some orphans behind and report success.
        """
        return [pool["metadata"]["name"] for pool in self._iter_managed_pools()]

    def _labels_for(self, pool: AssignmentPool) -> dict[str, str]:
        return {MANAGED_LABEL: "true", POOL_NAME_LABEL: pool.warmpool}

    # -- Capacity report loop -----------------------------------------------

    def _capacity_loop(self) -> None:
        # Every pass goes through the same guard, INCLUDING the first one.
        # Previously the first call sat outside the try/except, so a failure on
        # startup -- the most likely time to have one, since that is when fresh
        # IAM is least likely to be right -- propagated out of the thread and
        # killed it. The pod stayed 1/1 Running and Ready with a dead capacity
        # loop, publishing nothing to GCS or the hub, forever. A member that
        # has silently stopped reporting is exactly the failure the heartbeat
        # property exists to expose; it should not also be one we cause.
        while True:
            try:
                self._capacity_once()
            except Exception:
                log.exception("capacity pass failed; will retry next tick")
            if self._stop.wait(self.capacity_interval):
                return

    def _capacity_once(self) -> None:
        report = self._collect_capacity()
        payload = asdict(report)

        # The two sinks are independent, in BOTH directions. The hub write was
        # already guarded so a broken hub could not break GCS, but the GCS
        # write was not, so a broken bucket took the hub down with it -- and
        # since GCS goes first, an unwritable bucket meant the ClusterProfile
        # was never touched at all. Each failure is now contained to its own
        # sink.
        failed = []

        try:
            self.gcs.put_json(self.paths.capacity(self.cluster_name), payload)
        except Exception:
            log.exception(
                "writing the GCS capacity report for %s failed; continuing",
                self.cluster_name)
            failed.append("gcs")

        if self.hub_publisher is not None:
            try:
                self.hub_publisher.publish(payload)
            except Exception:
                log.exception(
                    "publishing ClusterProfile status for %s failed; continuing",
                    self.cluster_name)
                failed.append("clusterprofile")

        sinks = 2 if self.hub_publisher is not None else 1
        if len(failed) == sinks:
            # Partial delivery is survivable and already logged per sink. Total
            # failure means this cluster is invisible to the planner, which is
            # worth one unmissable line rather than only a pair of tracebacks.
            log.error(
                "capacity report for %s reached NO sink this tick (%s); the "
                "planner cannot see this cluster",
                self.cluster_name, ", ".join(failed))
        log.debug(
            "capacity published depth=%d ready=%d active_claims=%s pressure=%s",
            report.warmpool_depth, report.warmpool_ready, report.active_claims,
            "unknown" if report.node_pressure_score is None
            else f"{report.node_pressure_score:.3f}",
        )

    def _collect_capacity(self) -> CapacityReport:
        report = CapacityReport(
            cluster=self.cluster_name,
            updated_at=_now_iso(),
        )
        # Warmpool depth/ready + generation_observed — read CRs directly, paged.
        # This runs every capacity_interval, which is the more frequent of the
        # two loops; see _iter_managed_pools for why it is not a bare list.
        gen_observed = 0
        for pool in self._iter_managed_pools():
            report.reported_pools.append(pool["metadata"]["name"])
            status = pool.get("status") or {}
            report.warmpool_depth += int(status.get("replicas", 0) or 0)
            report.warmpool_ready += int(status.get("readyReplicas", 0) or 0)
            gen_annot = (pool["metadata"].get("annotations") or {}).get(GENERATION_ANNOTATION)
            if gen_annot:
                try:
                    gen_observed = max(gen_observed, int(gen_annot))
                except ValueError:
                    pass
        report.reported_pools.sort()
        report.generation_observed = gen_observed

        # Everything above is O(pools) — 500 CRs, and paged, so cheap at any
        # density in both wire and memory terms.
        # Everything below is O(cluster): a full Sandbox list and a full Pod
        # list. At density that is both an OOM risk and a real load on the
        # apiserver, competing for the very APF seats a density run measures.
        # `light` reports warmpool depth/ready only, which is what the
        # capacity-aware planner leans on anyway.
        if self.capacity_detail == "light":
            return report

        # active_claims — via SDK. NOTE: list_all_sandboxes() is unpaginated,
        # so this is the remaining O(cluster) allocation in `full` mode.
        try:
            claims = self.sandbox_client.list_all_sandboxes(namespace=self.namespace)
            report.active_claims = len(claims)
        except Exception as e:
            # Leave it None. Do NOT substitute 0 — see CapacityReport.
            log.warning("SDK list_all_sandboxes failed; active_claims "
                        "unmeasured (%s)", e)

        # node_pressure_score — direct list; SDK doesn't cover it.
        report.node_pressure_score = self._node_pressure()
        return report

    def _node_pressure(self) -> float | None:
        """Coarse [0,1] score: avg((cpu_req/alloc + mem_req/alloc)/2) across nodes.

        Returns None when the score could not be measured. Callers MUST NOT
        substitute 0.0 — see CapacityReport.node_pressure_score.

        KNOWN LIMIT, measured on a density fleet: this does not complete at
        200k pods. Walking the pod list takes ~5 min, which is longer than the
        apiserver keeps a continue token alive, so every attempt died with
        410 Gone partway through. Restarting (below) does not help once a
        single walk exceeds the token TTL — the cost is bytes-and-parsing,
        which scales with pod count, not with page count. At that density run
        `--capacity-detail=light` and accept a pressure-blind planner. The
        real fix is to stop deriving pressure from a full pod list at all and
        read metrics.k8s.io per node instead: O(nodes) (853) not O(pods)
        (200k), one request, no paging. Filed as follow-up work.
        """
        for attempt in (1, 2):
            node_req: dict[str, tuple[int, int]] = {}
            try:
                nodes = self.core_v1.list_node().items
                if not nodes:
                    return None
                # Page the pod list and fold each page into node_req
                # immediately, so peak memory is one page rather than the
                # whole cluster. The accumulator is O(nodes), not O(pods).
                cont: str | None = None
                while True:
                    page = self.core_v1.list_pod_for_all_namespaces(
                        field_selector="status.phase!=Failed,status.phase!=Succeeded",
                        limit=PAGE_LIMIT,
                        _continue=cont,
                    )
                    for p in page.items:
                        if not p.spec.node_name:
                            continue
                        cpu, mem = node_req.get(p.spec.node_name, (0, 0))
                        for ctr in p.spec.containers or []:
                            req = (ctr.resources.requests if ctr.resources else None) or {}
                            cpu += _parse_cpu_milli(req.get("cpu"))
                            mem += _parse_mem_bytes(req.get("memory"))
                        node_req[p.spec.node_name] = (cpu, mem)
                    cont = (page.metadata._continue or None) if page.metadata else None
                    if not cont:
                        break
                break  # walked the whole list
            except ApiException as e:
                # 410 Gone == the continue token expired mid-walk. The only
                # valid recovery is to start over; resuming is impossible.
                if e.status == 410 and attempt == 1:
                    log.warning("node pressure: continue token expired mid-walk, "
                                "restarting once (cluster too large for a paged "
                                "pod list at this interval?)")
                    continue
                log.warning("node pressure calc failed (%s %s); reporting UNKNOWN, "
                            "not zero", e.status, e.reason)
                return None
        acc = 0.0
        counted = 0
        for n in nodes:
            if n.spec.unschedulable:
                continue
            alloc = n.status.allocatable or {}
            alloc_cpu = _parse_cpu_milli(alloc.get("cpu"))
            alloc_mem = _parse_mem_bytes(alloc.get("memory"))
            if alloc_cpu == 0 and alloc_mem == 0:
                continue
            used_cpu, used_mem = node_req.get(n.metadata.name, (0, 0))
            cpu_r = min(1.0, max(0.0, used_cpu / alloc_cpu)) if alloc_cpu else 0.0
            mem_r = min(1.0, max(0.0, used_mem / alloc_mem)) if alloc_mem else 0.0
            acc += (cpu_r + mem_r) / 2
            counted += 1
        return acc / counted if counted else None


# ----------------------------------------------------------------------------
# Quantity parsing — kubernetes-client returns resource quantities as strings
# with SI suffixes. Minimal parser sufficient for the pressure signal.
# ----------------------------------------------------------------------------

_CPU_SUFFIX = {"m": 1, "": 1000}
_MEM_SUFFIX = {
    "": 1, "Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, "Ti": 1024**4,
    "K": 1000, "M": 1000**2, "G": 1000**3, "T": 1000**4,
}


def _parse_cpu_milli(s: str | None) -> int:
    if not s:
        return 0
    s = str(s).strip()
    for suf, mult in _CPU_SUFFIX.items():
        if suf and s.endswith(suf):
            return int(float(s[:-len(suf)]) * mult)
    try:
        return int(float(s) * 1000)
    except ValueError:
        return 0


def _parse_mem_bytes(s: str | None) -> int:
    if not s:
        return 0
    s = str(s).strip()
    # Order-longest-suffix-first so "Mi" beats "M".
    for suf in sorted(_MEM_SUFFIX, key=len, reverse=True):
        if suf and s.endswith(suf):
            try:
                return int(float(s[:-len(suf)]) * _MEM_SUFFIX[suf])
            except ValueError:
                return 0
    try:
        return int(float(s))
    except ValueError:
        return 0


def _now_iso() -> str:
    return _dt.datetime.now(_dt.timezone.utc).isoformat().replace("+00:00", "Z")


# ----------------------------------------------------------------------------
# Entrypoint
# ----------------------------------------------------------------------------

def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="fleet-member")
    p.add_argument("--cluster-name", default=os.environ.get("CLUSTER_NAME"),
                   help="This agent's cluster identity — must match fleet-spec.cluster_weights")
    p.add_argument("--bucket", default=os.environ.get("FLEET_BUCKET"),
                   help="GCS bucket serving as the fleet hub")
    p.add_argument("--namespace", default=os.environ.get("NAMESPACE", "multi-cluster-fleet"),
                   help="Namespace where fleet-managed CRs live")
    # positive_float, not float: 0 or a negative turns the loop into a spin
    # that hammers GCS and the apiserver with no delay between passes, and
    # this file already documents apiserver concurrency as a real hazard at
    # density.
    p.add_argument("--reconcile-interval", type=positive_float, default=30.0)
    p.add_argument("--capacity-interval", type=positive_float, default=30.0)
    p.add_argument(
        "--capacity-detail", choices=("full", "light"), default="full",
        help="full: also report active_claims + node_pressure_score, which "
             "cost one Sandbox list and one Pod list per tick. light: "
             "warmpool depth/ready only. Use light on density runs — at 200k "
             "pods the full lists OOM the member and steal apiserver "
             "concurrency from the controller being measured. TRADE-OFF: in "
             "light mode both fields are reported as unmeasured (omitted, "
             "not 0), so capacity-aware placement goes pressure-blind "
             "(degrades to weights + ready-ratio) and least-loaded degrades "
             "to active_replicas.",
    )
    # -- ClusterProfile publishing (SIG-Multicluster hub) ------------------- #
    # Off by default. When enabled the member ALSO applies its capacity onto
    # its own ClusterProfile status, so a planner running --inventory=
    # clusterprofile sees it. GCS keeps working either way.
    p.add_argument(
        "--publish-clusterprofile", action="store_true",
        default=os.environ.get("PUBLISH_CLUSTERPROFILE", "").lower()
                in ("1", "true", "yes"),
        help="Also publish capacity to this cluster's ClusterProfile status "
             "on the hub, via Server-Side Apply",
    )
    p.add_argument("--hub-kubeconfig", default=os.environ.get("HUB_KUBECONFIG"),
                   help="Kubeconfig granting access to the HUB cluster. The hub "
                        "is not this cluster, so in-cluster credentials do not "
                        "apply and this is required when publishing.")
    p.add_argument("--hub-context", default=os.environ.get("HUB_CONTEXT"))
    p.add_argument("--hub-token-source",
                   choices=["kubeconfig", "gke-metadata"],
                   default=os.environ.get("HUB_TOKEN_SOURCE", "kubeconfig"),
                   help="How to authenticate to the hub. 'kubeconfig' uses "
                        "the credentials in --hub-kubeconfig. 'gke-metadata' "
                        "ignores them and authenticates as this pod's "
                        "Workload Identity service account, which lets the "
                        "kubeconfig be a credential-free ConfigMap holding "
                        "only the hub's address and CA.")
    p.add_argument("--hub-namespace",
                   default=os.environ.get("HUB_NAMESPACE", "fleet-system"))
    p.add_argument("--hub-api-version",
                   default=os.environ.get("HUB_API_VERSION"),
                   help="Served ClusterProfile version. Upstream deprecates "
                        "v1alpha1 in favour of v1alpha2 while v1alpha1 is "
                        "still the storage version; must match the planner's "
                        "--hub-api-version or the two write separate "
                        "managedFields entries.")
    p.add_argument("--field-manager",
                   default=os.environ.get("FIELD_MANAGER", "agent-sandbox-fleet-member"),
                   help="SSA field manager. Must be distinct from the cluster "
                        "manager's so the two never take each other's fields.")
    p.add_argument("--force-conflicts", action="store_true",
                   help="Steal fields already owned by another field manager. "
                        "Off by default: a 409 is information, not noise.")
    p.add_argument("--sandbox-capacity", type=int,
                   default=(int(os.environ["SANDBOX_CAPACITY"])
                            if os.environ.get("SANDBOX_CAPACITY") else None),
                   help="How many sandboxes this cluster can hold. Published "
                        "so the planner can derive weights with no weight map. "
                        "A member cannot infer this, so you supply it.")
    p.add_argument("--status-subresource", dest="status_subresource",
                   action="store_true", default=True,
                   help="Apply to status/ (the upstream CRD declares it)")
    p.add_argument("--no-status-subresource", dest="status_subresource",
                   action="store_false",
                   help="Apply to the main object instead — for fixture CRDs "
                        "that do not declare a status subresource")
    p.add_argument("--log-level", default=os.environ.get("LOG_LEVEL", "INFO"))
    args = p.parse_args(argv)

    # `force=True` — critical. Without it, if any imported library
    # (google-cloud-storage, kubernetes, k8s_agent_sandbox) already attached a
    # root handler at import time, basicConfig silently no-ops and NONE of
    # our log.info() calls emit. Cost us a debugging cycle on the live fleet.
    logging.basicConfig(
        level=args.log_level.upper(),
        format="%(asctime)s %(name)s %(levelname)s %(message)s",
        stream=sys.stderr,
        force=True,
    )
    # And emit a proof-of-life line immediately so `kubectl logs` shows
    # SOMETHING even if reconcile hasn't happened yet.
    log.info("fleet-member starting (log_level=%s)", args.log_level.upper())

    if not args.cluster_name:
        p.error("--cluster-name / $CLUSTER_NAME is required")
    if not args.bucket:
        p.error("--bucket / $FLEET_BUCKET is required")

    hub_publisher = None
    if args.publish_clusterprofile:
        # Check the kubeconfig before building the publisher, so the error
        # names the flag. The hub is a DIFFERENT cluster from the one this
        # member runs in, so there is no in-cluster fallback: without a path,
        # load_kube_config() silently falls back to ~/.kube/config and either
        # fails deep inside the client or — worse — publishes this cluster's
        # capacity onto whatever hub that file happens to point at.
        if not args.hub_kubeconfig:
            p.error("--publish-clusterprofile requires --hub-kubeconfig / "
                    "$HUB_KUBECONFIG (the hub is a different cluster; there "
                    "is no in-cluster fallback)")
        if not os.path.isfile(args.hub_kubeconfig):
            p.error(f"--hub-kubeconfig {args.hub_kubeconfig!r} does not exist "
                    f"or is not a file")

        from .inventory import CLUSTERPROFILE_VERSION
        from .publisher import ClusterProfilePublisher

        hub_publisher = ClusterProfilePublisher(
            args.cluster_name,
            version=args.hub_api_version or CLUSTERPROFILE_VERSION,
            namespace=args.hub_namespace,
            kubeconfig=args.hub_kubeconfig,
            context=args.hub_context,
            token_source=args.hub_token_source,
            field_manager=args.field_manager,
            force=args.force_conflicts,
            sandbox_capacity=args.sandbox_capacity,
            status_subresource=args.status_subresource,
        )
        # Fail at startup, not silently forever. A merge-patch would accept
        # field_manager and establish no ownership at all.
        hub_publisher.assert_ssa_configured()
        log.info("publishing ClusterProfile status to hub ns=%s as field "
                 "manager %r", args.hub_namespace, args.field_manager)

    fm = FleetMember(
        cluster_name=args.cluster_name,
        bucket=args.bucket,
        namespace=args.namespace,
        reconcile_interval=args.reconcile_interval,
        capacity_interval=args.capacity_interval,
        capacity_detail=args.capacity_detail,
        hub_publisher=hub_publisher,
    )
    fm.run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
