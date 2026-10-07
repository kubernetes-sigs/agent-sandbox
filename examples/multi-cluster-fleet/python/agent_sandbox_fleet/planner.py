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

"""The planner — turns a FleetSpec + live capacity reports into ClusterAssignments.

Composes the harvested selectors (placement.py), the Hamilton budget split
(budget.py), and the concurrency-proportional sizing (sizing.py) with the new
CapacityAware selector. Writes the result to GCS.

Runs as a batch on `fleetctl apply`. Not a controller. A future alpha would
promote this into a hub controller reconciling a `SandboxFleet` CRD.
"""

from __future__ import annotations

import datetime as _dt
import logging
import math
import re
from collections import defaultdict
from dataclasses import asdict, dataclass, field
from typing import Any

from pydantic import BaseModel, Field, field_validator

from . import budget, inventory as _inventory, placement, sizing
from .inventory import STALE_AGE_S, InventoryProvider
from .objectstore import GCS, Paths
from .placement import PlannerCluster, PlannerRegistry

logger = logging.getLogger("agent_sandbox_fleet.planner")

# The payload shape this code writes and understands. Three DIFFERENT questions
# used to share one integer (`generation`), which is why they are now separate:
#
#   schema_version  "can I parse this?"   -- compatibility gate. A member that
#                                            does not know the version refuses
#                                            the payload and keeps serving its
#                                            current pools, rather than reading
#                                            an unparseable plan as empty and
#                                            tearing the cluster down.
#   generation      "is this newer?"      -- ordering. Derived here, compared by
#                                            members. See `next_generation`.
#   store generation "did someone else    -- concurrency. Owned by the object
#                    write since I read?"   store; see objectstore.put_json.
#
# Bump this ONLY for a change an older member cannot safely ignore. Adding a
# field is not one: members drop unknown fields (see AssignmentPool.from_json in
# fleet_member.py), so additive changes ship without a bump and without a
# lockstep rollout.
SCHEMA_VERSION = 1

# --------------------------------------------------------------------------- #
# Wire types — matched byte-for-byte with the Go structs in pkg/fleet/types.go.
# --------------------------------------------------------------------------- #

class ModelSpec(BaseModel):
    # Image is optional. The fleet-member does NOT use it (templates are
    # operator-managed and hold the actual pod image). It's still needed by
    # the `image-affinity` placement policy (hashes MD5(image) mod N for
    # image-pull locality) and by the optional `registry_rewrite` step. For
    # capacity-aware / least-loaded / round-robin / capacity-weighted
    # placement, the field is decorative and can be omitted.
    image: str | None = None
    template_name: str
    target_tasks: int = Field(gt=0)
    # Explicit placement for the `pinned` policy: the name of the cluster this
    # model MUST land on (e.g. the cluster whose secondary-boot-disk shard
    # holds the image). Required on every model when placement_policy=pinned;
    # ignored by every other policy.
    cluster: str | None = None

    @field_validator("template_name")
    @classmethod
    def _template_name_renders_a_valid_pool_name(cls, v: str) -> str:
        """Reject at spec load a template_name whose rendered pool name
        ("<template_name>-pool") cannot be a Kubernetes object name — where
        the error can name the offending model, instead of surfacing as a
        create failure on every member. Names longer than 63 chars are valid
        objects but exceed the label-value cap; the member truncates+hashes
        the pool label in that case, so only warn.
        """
        pool = f"{v}-pool"
        if len(pool) > 253 or not _DNS1123_SUBDOMAIN.match(pool):
            raise ValueError(
                f"template_name {v!r} renders warm-pool name {pool!r}, which "
                "is not a valid Kubernetes object name (DNS-1123 subdomain, "
                "max 253 chars); every member's pool create would fail")
        if len(pool) > 63:
            logger.warning(
                "template_name %r renders a %d-char pool name; the member "
                "will truncate+hash its pool label (values cap at 63)",
                v, len(pool))
        return v


_DNS1123_SUBDOMAIN = re.compile(
    r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$")


class FleetSpec(BaseModel):
    schema_version: int = SCHEMA_VERSION
    # DEPRECATED and ignored. `fleetctl apply` derives the generation from the
    # published assignments (see `next_generation`); an author-supplied value is
    # a silent-failure footgun, because forgetting to bump it makes every member
    # correctly ignore the apply while the operator sees a successful command
    # and no change in the fleet. Kept on the model rather than dropped so that
    # a spec that still carries one gets a warning instead of pydantic silently
    # discarding it as an extra field -- the whole failure mode being fixed here
    # is a generation that goes unnoticed. Use `fleetctl apply --generation` for
    # replay and disaster recovery.
    generation: int | None = None
    max_concurrent: int = Field(gt=0, default=100)
    max_pool: int = Field(gt=0, default=50)
    placement_policy: str = "capacity-aware"
    cluster_weights: dict[str, float] = Field(default_factory=dict)
    models: list[ModelSpec]
    # v1.5 anti-affinity floor. 0 = disabled (default: spread-first + scored
    # extras). >0 = require the assignment to use at least this many distinct
    # fresh clusters; ALL models placed round-robin across the first
    # min(min_clusters, len(fresh_clusters)) sorted-by-name clusters, ignoring
    # scored placement entirely. Kills the CapacityAware ping-pong that happens
    # when models > clusters and extras oscillate on re-apply.
    min_clusters: int = Field(ge=0, default=0)
    # How a STALE cluster (no fresh capacity report) is treated. Staleness
    # always excludes it from NEW placement. What happens to the pools it
    # already holds is a second, longer threshold: until a cluster's report is
    # older than this, its last published entry is carried forward unchanged
    # (frozen) so a transient reporting failure never tears a healthy cluster
    # down; past it, the entry is emptied and the member drops the pools. 0 =
    # never empty on staleness alone -- only an explicit drain (weight 0 or
    # models: []) does. An empty pool set is therefore only ever the result of
    # an explicit instruction or a prolonged, deliberate-length silence.
    stale_teardown_after_s: int = Field(ge=0, default=900)

    @field_validator("schema_version")
    @classmethod
    def _schema_known(cls, v: int) -> int:
        """Refuse a spec this planner cannot parse, at load, naming the file.

        Symmetric with the member-side gate: neither end should act on a payload
        whose shape it does not know. Rejecting forward is the conservative
        direction -- a newer spec may mean something different by a field this
        planner thinks it understands.
        """
        if v != SCHEMA_VERSION:
            raise ValueError(
                f"spec schema_version {v} is not supported by this planner "
                f"(understands {SCHEMA_VERSION}); upgrade fleetctl"
            )
        return v

    @field_validator("generation")
    @classmethod
    def _generation_deprecated(cls, v: int | None) -> None:
        if v is not None:
            logger.warning(
                "FleetSpec.generation=%d is deprecated and IGNORED: the "
                "generation is derived from the published assignments. Remove "
                "it from the spec; use `fleetctl apply --generation` to force a "
                "specific value for replay.", v,
            )
        # Normalised away so nothing downstream can read it by accident and
        # reintroduce the hand-authored path.
        return None

    @field_validator("cluster_weights")
    @classmethod
    def _weights_finite(cls, v: dict[str, float]) -> dict[str, float]:
        """Reject nan/inf/negative at spec load, where the error can name the
        file. Without this the failure surfaces from inside hamilton_split as an
        OverflowError with no indication of which cluster is at fault.
        """
        bad = {k: w for k, w in v.items() if not math.isfinite(w) or w < 0}
        if bad:
            raise ValueError(f"cluster_weights must be finite and >= 0; got {bad}")
        return v


class AssignmentPool(BaseModel):
    # `image` mirrors ModelSpec.image — optional, only populated when the
    # source spec included it (for image-affinity / registry-rewrite use).
    image: str | None = None
    template: str
    warmpool: str
    replicas: int


class ClusterAssignment(BaseModel):
    pools: list[AssignmentPool] = Field(default_factory=list)
    # Set by the planner when this cluster's capacity report is stale and the
    # entry is a carry-forward of the last published one rather than a fresh
    # placement. Members apply it unchanged; the resolver skips it. See
    # FleetSpec.stale_teardown_after_s.
    stale_since: str | None = None


class Assignments(BaseModel):
    schema_version: int = SCHEMA_VERSION
    generation: int
    updated_at: str
    clusters: dict[str, ClusterAssignment]


# --------------------------------------------------------------------------- #
# Registry construction — read capacity reports from GCS into PlannerClusters.
# --------------------------------------------------------------------------- #

def load_registry(gcs: GCS, weights: dict[str, float], paths: Paths | None = None) -> PlannerRegistry:
    """Read all capacity reports from GCS and hydrate a PlannerRegistry.

    Any cluster listed in `weights` but missing a fresh report is included with
    a stale report_age_s so it's filtered by `fresh()`. This matches the desired
    behavior: shift assignments away from silent clusters.

    Thin wrapper over `inventory.GCSInventory` — kept because callers and tests
    use it. For a non-GCS inventory (e.g. SIG-Multicluster ClusterProfile CRs),
    build the provider directly and pass it to `apply`.
    """
    return _inventory.GCSInventory(gcs, paths).load(weights)


# --------------------------------------------------------------------------- #
# The planner itself.
# --------------------------------------------------------------------------- #

def plan(spec: FleetSpec, registry: PlannerRegistry,
         generation: int = 0,
         previous: "Assignments | dict | None" = None) -> Assignments:
    """Produce ClusterAssignments from a FleetSpec + live registry.

    `generation` is passed in rather than read off the spec so that plan()
    stays a pure function of (spec, registry, generation) with no IO: deriving
    it requires reading the published assignments, which is `apply()`'s job.

    Algorithm (mirrors the RL PoC's `SandboxFleet.plan()` at `fleet.py:250-291`,
    rebased on the new registry):

      0. Reduce the registry to the ELIGIBLE clusters: fresh report AND weight
         > 0. Every step below sees only these, so a drained cluster is
         excluded from positional placement as well as from scoring.
      1. Pick a Placement selector by name.
      2. **Spread-first pre-pass** (v1.5): give each eligible cluster ONE model
         before doubling up. Prevents the CapacityAware oscillation where
         a cluster with leftover load at plan-time gets skipped indefinitely.
         Only kicks in for the first N models where N = number of fresh
         clusters; after that, honor the configured selector.
      3. For each remaining model, `selector.select(image, registry)` → cluster.
         Bookkeep `planned_replicas` so subsequent picks see the added load.
      4. Split `max_concurrent` across placed clusters via
         `budget.hamilton_split`.
      5. For each (cluster, image) pair, run `sizing.compute_replicas` on the
         cluster's slice of the budget.
      6. Emit `Assignments`.

    The spread-first pre-pass is a behavior change from the original
    algorithm. It exists because pure-greedy scoring produces oscillating
    placement whenever some clusters are transiently unavailable (their
    capacity report hasn't caught up to a wipe, or they had a burst of
    active_claims that outlasts the cleanup). This is a deliberate
    divergence from the original algorithm, not an accident.
    """
    selector = placement.get_placement(spec.placement_policy)
    # eligible(), not fresh(): a cluster weighted 0 is being drained and must be
    # excluded from the candidate set before any of the three placement paths
    # runs. Scoring it low is not enough -- see PlannerRegistry.eligible().
    eligible_clusters = sorted(registry.eligible(), key=lambda c: c.name)
    drained = sorted(c.name for c in registry.fresh() if c.weight <= 0)
    if drained:
        logger.info(
            "excluding %d drained cluster(s) from placement (weight 0): %s",
            len(drained), ", ".join(drained),
        )

    # image-affinity hashes model.image to pin the pool to a cluster; missing
    # images make that impossible. Fail fast rather than silently misplace.
    if spec.placement_policy == "image-affinity":
        missing = [m.template_name for m in spec.models if not m.image]
        if missing:
            raise ValueError(
                f"placement_policy=image-affinity requires an image on every model; "
                f"missing on templates: {missing}"
            )

    # pinned places each model on the cluster its spec names. Both halves are
    # validated up front: a model without a pin has no fallback (unlike
    # image-affinity there is nothing sensible to hash), and a pin naming a
    # non-eligible cluster must fail the PLAN, not silently strand the model —
    # for a pre-sharded fleet, "put it somewhere else" is data loss, not
    # placement. Lists are truncated: a spec can carry tens of thousands of
    # models and an exception is not a report.
    if spec.placement_policy == "pinned":
        unpinned = [m.template_name for m in spec.models if not m.cluster]
        if unpinned:
            raise ValueError(
                f"placement_policy=pinned requires a cluster on every model; "
                f"missing on {len(unpinned)} model(s), first few: {unpinned[:5]}"
            )
        eligible_names = {c.name for c in eligible_clusters}
        bad = sorted({m.cluster for m in spec.models} - eligible_names)
        if bad:
            raise placement.NoClusterAvailableError(
                f"placement_policy=pinned: {len(bad)} pinned cluster(s) are not "
                f"eligible (no fresh report, or drained at weight 0): {bad[:5]}; "
                f"eligible: {sorted(eligible_names)}"
            )

    # Step 1: pick placement mode.
    #
    # min_clusters > 0 → anti-affinity mode. ALL models placed round-robin
    # across the first N sorted-by-name fresh clusters (N = min(min_clusters,
    # len(fresh))). Ignores the configured selector entirely. Deterministic:
    # same spec + same fresh set produces byte-identical placement, so
    # re-apply cannot ping-pong.
    #
    # min_clusters == 0 → default mode: spread-first pre-pass (one model per
    # fresh cluster for the first N) then configured selector for extras.
    # Routing is per CLUSTER, not per model: a cluster can host several models,
    # so the two maps below are keyed by cluster name and are what Steps 3 and 4
    # actually consume. There used to be a third, `chosen`, keyed by
    # model.image -- never read by anything, and latently wrong: image is
    # Optional[str], so every model without one collided on the None key. Dead
    # and misleading, so it is gone rather than fixed.
    per_cluster_tasks: dict[str, int] = defaultdict(int)
    per_cluster_models: dict[str, list[ModelSpec]] = defaultdict(list)

    # "Nothing eligible" has two causes that need opposite handling, and both
    # produce an empty candidate list, so the reason has to be checked before
    # the count.
    #
    #   Nothing FRESH  -> the planner cannot see the fleet. Raise. Publishing an
    #                     empty assignment here would tear down every warm pool
    #                     on every cluster in response to what is most likely a
    #                     bucket read failure, a clock skew, or a planner that
    #                     started before any member did. A fleet-wide teardown
    #                     must never be the fallback for "I got no data".
    #   All fresh ones DRAINED -> the operator asked for exactly that teardown.
    #                     Plan empty and say so.
    #
    # This also keeps the all-zero weight map away from budget.hamilton_split,
    # which reads it as "no preference, split evenly" -- correct for a caller
    # who genuinely has no ranking, and an inversion of a full drain into a full
    # deployment if the planner ever reached it.
    if not registry.fresh() and spec.models:
        raise placement.NoClusterAvailableError(
            f"no cluster has a fresh capacity report "
            f"(0 of {len(registry.clusters)} known, max age "
            f"{registry.max_report_age_s:.0f}s); refusing to publish an empty "
            f"assignment, which would drop every warm pool in the fleet"
        )
    if not eligible_clusters:
        logger.warning(
            "every fresh cluster is drained (%d of %d known at weight 0) — "
            "planning an EMPTY assignment, which drops every warm pool in the "
            "fleet as each member reads it",
            len(drained), len(registry.clusters),
        )
    elif spec.placement_policy == "pinned":
        # Explicit mode: every model names its cluster and both were validated
        # above. Deterministic, independent of capacity scoring, and immune to
        # re-apply ping-pong by construction — the operator's shard layout IS
        # the placement. Takes precedence over min_clusters: an explicit pin is
        # a stronger statement than an anti-affinity floor.
        for model in spec.models:
            cluster = registry.get(model.cluster)
            cluster.planned_replicas += 1
            per_cluster_tasks[cluster.name] += model.target_tasks
            per_cluster_models[cluster.name].append(model)
    elif spec.min_clusters > 0:
        # Clamp rather than fail. min_clusters is a floor on SPREAD, not a
        # precondition on the fleet: refusing to plan would mean one cluster
        # going stale mid-incident takes the whole plan down with it, which is
        # exactly when re-planning matters most.
        rr_target_count = min(spec.min_clusters, len(eligible_clusters))
        if spec.min_clusters > len(eligible_clusters):
            logger.warning(
                "min_clusters=%d but only %d eligible clusters (fresh and not "
                "drained); spreading across %d instead",
                spec.min_clusters, len(eligible_clusters), rr_target_count,
            )
        for i, model in enumerate(spec.models):
            cluster = eligible_clusters[i % rr_target_count]
            cluster.planned_replicas += 1
            per_cluster_tasks[cluster.name] += model.target_tasks
            per_cluster_models[cluster.name].append(model)
    else:
        for i, model in enumerate(spec.models):
            if i < len(eligible_clusters):
                # First N models: deterministic one-per-cluster. Forces spread even
                # when the scored selector would prefer to double up.
                cluster = eligible_clusters[i]
            else:
                # After each cluster has at least one pool, honor the configured
                # selector (usually capacity-aware) for extras.
                cluster = selector.select(model.image, registry)
            cluster.planned_replicas += 1
            per_cluster_tasks[cluster.name] += model.target_tasks
            per_cluster_models[cluster.name].append(model)

    # Step 3: split global budget across CHOSEN clusters, weighted
    active_weights = {
        name: registry.get(name).weight for name in per_cluster_tasks
    }
    budget_per_cluster = budget.hamilton_split(spec.max_concurrent, active_weights)

    # Step 4: size each pool
    clusters: dict[str, ClusterAssignment] = {}
    for cname, models in per_cluster_models.items():
        tasks_total = per_cluster_tasks[cname]
        mc = budget_per_cluster.get(cname, 0)
        pools: list[AssignmentPool] = []
        for m in models:
            replicas = sizing.compute_replicas(
                tasks_image=m.target_tasks,
                tasks_total=tasks_total,
                max_concurrent=mc,
                max_pool=spec.max_pool,
            )
            pools.append(AssignmentPool(
                image=m.image,
                template=m.template_name,
                warmpool=_warmpool_name(m.template_name),
                replicas=replicas,
            ))
        clusters[cname] = ClusterAssignment(pools=pools)

        # max_concurrent is a TARGET, not a hard cap, and the gap is the min-1
        # floor in sizing.compute_replicas: every placed pool gets at least one
        # replica, because a template assigned 0 replicas can only be served cold
        # and the assignment is then pointless. When a cluster holds more models
        # than its budget slice, the floor wins and the slice is exceeded. Say so
        # rather than quietly overshooting an operator's stated ceiling.
        planned = sum(p.replicas for p in pools)
        if planned > mc:
            logger.warning(
                "cluster %s: %d replicas exceeds its budget slice of %d — %d pools "
                "at the 1-replica floor. Raise max_concurrent (%d) or place fewer "
                "models per cluster (min_clusters) to stay inside the budget.",
                cname, planned, mc, len(pools), spec.max_concurrent,
            )

    # Ensure every registry cluster has an entry. Empty pools means "drop
    # everything", and the fleet-member treats absent identically to empty, so
    # this is also how a drain spec works: a cluster weighted 0 was filtered out
    # of `eligible_clusters` above and lands here, getting an empty assignment
    # and shedding its pools. No warning for that case -- it was asked for, and
    # the exclusion is already logged once at the top of plan().
    #
    # NOTE the sharp edge: `registry.clusters` includes STALE clusters, so a
    # cluster whose capacity report went silent is assigned empty and tears its
    # pools down as soon as its member reads the file. That is intended when the
    # cluster is genuinely gone, but the trigger is a missing capacity report,
    # not a missing member — a cluster whose reconcile loop is perfectly healthy
    # and whose publish path is not will drop every warm pool it holds. Keeping
    # the behavior (a drain has to be able to empty a cluster that is not
    # reporting) but logging it, since the alternative is a silent teardown.
    #
    # UPDATE to the sharp edge above: a stale cluster is now FROZEN, not
    # emptied, until its report is older than spec.stale_teardown_after_s.
    # Freezing means carrying its last published entry forward verbatim with
    # `stale_since` set: the member sees the same pools it already runs and
    # changes nothing, the resolver stops routing claims there, and new
    # placement ignores it. Only an explicit drain (weight 0, models: []) or
    # a silence longer than the teardown window empties it. A report object
    # that is MISSING outright (age >= STALE_AGE_S) with pools previously
    # published is the most suspicious shape of all -- dead clusters leave
    # their last report behind; a vanished object means bucket-side trouble
    # -- so it freezes regardless of the window.
    prev = _previous_entries(previous)
    now = _dt.datetime.now(_dt.timezone.utc)
    now_iso = now.isoformat().replace("+00:00", "Z")
    window = spec.stale_teardown_after_s
    frozen_replicas = 0
    frozen_names: list[str] = []
    for cname in registry.clusters:
        if cname in clusters:
            continue
        c = registry.clusters[cname]
        stale = c.report_age_s > registry.max_report_age_s
        prev_pools, prev_since = prev.get(cname, ([], None))
        past_window = (window > 0 and c.report_age_s < STALE_AGE_S
                       and c.report_age_s > window)
        if (stale and c.weight > 0 and spec.models and prev_pools
                and not past_window):
            if prev_since:
                since = prev_since
            elif c.report_age_s >= STALE_AGE_S:
                since = now_iso
            else:
                since = (now - _dt.timedelta(seconds=c.report_age_s)
                         ).isoformat().replace("+00:00", "Z")
            pools = [AssignmentPool.model_validate(p) for p in prev_pools]
            replicas = sum(p.replicas for p in pools)
            frozen_replicas += replicas
            frozen_names.append(cname)
            logger.warning(
                "cluster %s has no fresh capacity report (%s) — FREEZING its "
                "published entry (%d pools, %d replicas) instead of emptying "
                "it: excluded from new placement, claims will not route "
                "there, pools torn down %s. Check the member's capacity "
                "publish path, or drain it explicitly with weight 0.",
                cname, _age_str(c.report_age_s), len(pools), replicas,
                (f"only after {window}s of silence" if window > 0
                 else "only on an explicit drain (stale_teardown_after_s=0)"),
            )
            clusters[cname] = ClusterAssignment(pools=pools, stale_since=since)
            continue
        if stale:
            if past_window:
                why = f"silent for longer than stale_teardown_after_s={window}s"
            elif c.weight <= 0:
                why = "drained at weight 0"
            elif not spec.models:
                why = "spec has no models (drain)"
            else:
                why = "it held no pools in the published assignment"
            logger.warning(
                "cluster %s has no fresh capacity report (%s) — assigning "
                "empty (%s), which DROPS any warm pools it currently holds",
                cname, _age_str(c.report_age_s), why,
            )
        clusters[cname] = ClusterAssignment(pools=[])
    if frozen_names:
        logger.warning(
            "%d frozen cluster(s) (%s) keep %d replicas outside the budget: the "
            "fleet total exceeds max_concurrent=%d by that much until they "
            "report again or are drained",
            len(frozen_names), ", ".join(frozen_names), frozen_replicas,
            spec.max_concurrent,
        )
    return Assignments(schema_version=SCHEMA_VERSION, generation=generation,
                       updated_at=now_iso, clusters=clusters)


def preview(
    gcs: GCS,
    spec: FleetSpec,
    paths: Paths | None = None,
    provider: InventoryProvider | None = None,
) -> str:
    """Plan against live inventory and return the delta against the currently
    published assignments — `fleetctl apply --dry-run`. Publishes nothing and
    archives nothing; the only store calls are reads.
    """
    paths = paths or Paths()
    provider = provider or _inventory.GCSInventory(gcs, paths)
    previous, _store_gen = read_published_payload(gcs, paths)
    published_gen = int(previous.get("generation", 0)) if previous else 0
    reg = provider.load(spec.cluster_weights)
    assn = plan(spec, reg, generation=next_generation(published_gen, None),
                previous=previous)
    return format_delta(previous, assn)


def format_delta(published: dict | None, planned: Assignments) -> str:
    """Human-readable diff between a published assignments payload and a
    planned one. Deletions are called out hard: a `- pool` line is a warm-pool
    teardown on that cluster the moment a member reads the new file, and the
    whole reason --dry-run exists is that `apply` can otherwise delete every
    pool in the fleet with nothing printed in advance.
    """
    old: dict[str, dict[str, int]] = {}
    for cname, body in ((published or {}).get("clusters") or {}).items():
        old[cname] = {
            p["warmpool"]: int(p.get("replicas", 0))
            for p in (body or {}).get("pools", []) if p.get("warmpool")
        }
    new = {cname: {p.warmpool: p.replicas for p in ca.pools}
           for cname, ca in planned.clusters.items()}
    lines: list[str] = []
    adds = dels = resized = 0
    for cname in sorted(set(old) | set(new)):
        o, n = old.get(cname, {}), new.get(cname, {})
        frozen = ""
        entry = planned.clusters.get(cname)
        if entry is not None and entry.stale_since:
            frozen = (f"  [FROZEN since {entry.stale_since}: no fresh capacity "
                      "report; carried forward, no new placement]")
        if o == n:
            lines.append(f"  {cname}: unchanged "
                         f"({sum(n.values())} replicas in {len(n)} pools){frozen}")
            continue
        lines.append(f"  {cname}:{frozen}")
        for pool in sorted(set(o) | set(n)):
            if pool not in o:
                adds += 1
                lines.append(f"    + {pool} ({n[pool]})")
            elif pool not in n:
                dels += 1
                lines.append(f"    - {pool} ({o[pool]})  [TEARDOWN]")
            elif o[pool] != n[pool]:
                resized += 1
                lines.append(f"    ~ {pool} {o[pool]} -> {n[pool]}")
    total_old = sum(sum(v.values()) for v in old.values())
    total_new = sum(sum(v.values()) for v in new.values())
    head = (f"DRY RUN — nothing was written. Would publish generation "
            f"{planned.generation}: +{adds} pool(s), -{dels} pool(s), "
            f"~{resized} resized; total replicas {total_old} -> {total_new}.")
    if dels:
        head += (" Lines marked [TEARDOWN] delete warm pools on that cluster "
                 "as soon as its member reads the new plan.")
    return "\n".join([head, *lines])


def _warmpool_name(template: str) -> str:
    return f"{template}-pool"


def publish(gcs: GCS, assignments: Assignments, paths: Paths | None = None,
            if_generation_match: int | None = None) -> None:
    paths = paths or Paths()
    gcs.put_json(paths.assignments, assignments.model_dump(),
                 if_generation_match=if_generation_match)
    logger.info("wrote %s (schema_version=%d generation=%d, %d clusters)",
                paths.assignments, assignments.schema_version,
                assignments.generation, len(assignments.clusters))


def read_published_payload(gcs: GCS, paths: Paths | None = None
                           ) -> tuple[dict | None, int]:
    """Return (published assignments payload or None, store generation).

    The payload is what `plan()` needs as `previous` -- a stale cluster's entry
    is carried forward from it -- and the store generation is the
    compare-and-set token for the next publish. See `read_published` for the
    schema guard.
    """
    paths = paths or Paths()
    raw, store_gen = gcs.get_json_with_generation(paths.assignments)
    if raw is None:
        return None, 0
    published_schema = raw.get("schema_version", SCHEMA_VERSION)
    if published_schema != SCHEMA_VERSION:
        raise ValueError(
            f"published {paths.assignments} has schema_version "
            f"{published_schema}, which this fleetctl ({SCHEMA_VERSION}) does "
            f"not understand — it was written by a different version. Refusing "
            f"to overwrite it; upgrade fleetctl."
        )
    return raw, store_gen


def read_published(gcs: GCS, paths: Paths | None = None) -> tuple[int, int]:
    """Return (payload generation, store generation) of the live assignments.

    Both are 0 when nothing has been published yet, which is the correct seed
    for both callers: the next payload generation is 1, and a store generation
    of 0 is the "must not exist" precondition, so the first apply of a fleet
    needs no special case.

    A published object whose schema_version this planner does not understand is
    a hard stop rather than a 0: overwriting a plan written by a newer fleetctl
    would silently downgrade the fleet, and the generation inside a payload this
    code cannot parse is not trustworthy input to an increment.
    """
    raw, store_gen = read_published_payload(gcs, paths)
    if raw is None:
        return 0, 0
    return int(raw.get("generation", 0)), store_gen


def _previous_entries(previous) -> dict[str, tuple[list[dict], str | None]]:
    """Normalise `plan(previous=...)` -- an Assignments, a raw payload dict, or
    None -- into {cluster: (pool dicts, stale_since)}."""
    if previous is None:
        return {}
    if not isinstance(previous, dict):
        previous = previous.model_dump()
    out: dict[str, tuple[list[dict], str | None]] = {}
    for cname, body in (previous.get("clusters") or {}).items():
        body = body or {}
        out[cname] = (list(body.get("pools") or []), body.get("stale_since"))
    return out


def _age_str(age_s: float) -> str:
    return "no report at all" if age_s >= STALE_AGE_S else f"age {age_s:.0f}s"


def next_generation(current: int, override: int | None = None) -> int:
    """Derive the generation to publish, or validate an explicit override.

    Monotonicity is enforced here rather than trusted, because a generation that
    does not advance is the one failure in this system with no symptom: members
    ignore the plan exactly as designed, nothing errors, and the operator sees a
    successful apply against an unchanged fleet.
    """
    if override is None:
        return current + 1
    if override <= current:
        raise ValueError(
            f"--generation {override} is not greater than the published "
            f"generation {current}; every member would ignore it and the apply "
            f"would silently do nothing"
        )
    return override


def apply(
    gcs: GCS,
    spec: FleetSpec,
    paths: Paths | None = None,
    provider: InventoryProvider | None = None,
    generation: int | None = None,
) -> Assignments:
    """One-shot: load registry, plan, publish, and also archive the spec.

    `provider` selects where cluster inventory comes from. Defaults to the GCS
    capacity reports. Note that GCS remains the transport for the spec and the
    assignments regardless — only the inventory source is pluggable.

    `generation` forces a specific value (replay, disaster recovery). Left None,
    it is derived from the published assignments.

    Write order is plan → publish → archive, and each step is load-bearing:

      plan first, because it raises on its own (NoClusterAvailableError,
      ValueError) and a bucket describing a plan that never ran is worse than
      one describing a stale plan that did.

      publish under a compare-and-set precondition on the store's generation, so
      two admins applying concurrently cannot both derive generation N+1 from
      the same base and have one silently overwrite the other. The loser gets
      CASConflict and must re-read -- retrying the same bytes would just
      reintroduce the lost update.

      archive last, because it is the only step whose failure is survivable. The
      spec copy is a human-readable record of what was applied; nothing reads it
      back to make a decision. Deriving the counter from it instead would make
      an archive write that failed after a successful publish desynchronise the
      fleet, and the next apply would reuse a generation members have passed.
    """
    paths = paths or Paths()
    provider = provider or _inventory.GCSInventory(gcs, paths)
    previous, store_gen = read_published_payload(gcs, paths)
    published_gen = int(previous.get("generation", 0)) if previous else 0
    gen = next_generation(published_gen, generation)
    reg = provider.load(spec.cluster_weights)
    assn = plan(spec, reg, generation=gen, previous=previous)
    publish(gcs, assn, paths, if_generation_match=store_gen)
    archived = spec.model_dump(exclude={"generation"})
    # What was applied, for humans; not an input. Stamped under its own key:
    # cmd_status re-validates this archive as a FleetSpec, and writing the
    # deprecated `generation` field made every status run warn about a value
    # apply() itself wrote.
    archived["applied_generation"] = gen
    gcs.put_json(paths.spec, archived)
    return assn
