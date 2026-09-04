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
from collections import defaultdict
from dataclasses import asdict, dataclass, field
from typing import Any

from pydantic import BaseModel, Field, field_validator

from . import budget, inventory as _inventory, placement, sizing
from .inventory import InventoryProvider
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
         generation: int = 0) -> Assignments:
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
    for cname in registry.clusters:
        if cname not in clusters:
            if registry.clusters[cname].report_age_s > registry.max_report_age_s:
                logger.warning(
                    "cluster %s has no fresh capacity report (age %.0fs) — assigning "
                    "empty, which DROPS any warm pools it currently holds",
                    cname, registry.clusters[cname].report_age_s,
                )
            clusters[cname] = ClusterAssignment(pools=[])

    now_iso = _dt.datetime.now(_dt.timezone.utc).isoformat().replace("+00:00", "Z")
    return Assignments(schema_version=SCHEMA_VERSION, generation=generation,
                       updated_at=now_iso, clusters=clusters)


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
    paths = paths or Paths()
    raw, store_gen = gcs.get_json_with_generation(paths.assignments)
    if raw is None:
        return 0, 0
    published_schema = raw.get("schema_version", SCHEMA_VERSION)
    if published_schema != SCHEMA_VERSION:
        raise ValueError(
            f"published {paths.assignments} has schema_version "
            f"{published_schema}, which this fleetctl ({SCHEMA_VERSION}) does "
            f"not understand — it was written by a different version. Refusing "
            f"to overwrite it; upgrade fleetctl."
        )
    return int(raw.get("generation", 0)), store_gen


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
    published_gen, store_gen = read_published(gcs, paths)
    gen = next_generation(published_gen, generation)
    reg = provider.load(spec.cluster_weights)
    assn = plan(spec, reg, generation=gen)
    publish(gcs, assn, paths, if_generation_match=store_gen)
    archived = spec.model_dump(exclude={"generation"})
    # What was applied, for humans; not an input. Stamped under its own key:
    # cmd_status re-validates this archive as a FleetSpec, and writing the
    # deprecated `generation` field made every status run warn about a value
    # apply() itself wrote.
    archived["applied_generation"] = gen
    gcs.put_json(paths.spec, archived)
    return assn
