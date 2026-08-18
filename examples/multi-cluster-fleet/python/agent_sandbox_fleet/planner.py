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
from collections import defaultdict
from dataclasses import asdict, dataclass, field
from typing import Any

from pydantic import BaseModel, Field

from . import budget, inventory as _inventory, placement, sizing
from .inventory import InventoryProvider
from .objectstore import GCS, Paths
from .placement import PlannerCluster, PlannerRegistry

logger = logging.getLogger("agent_sandbox_fleet.planner")

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


class FleetSpec(BaseModel):
  generation: int = 0
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

def plan(spec: FleetSpec, registry: PlannerRegistry) -> Assignments:
  """Produce ClusterAssignments from a FleetSpec + live registry.

  Algorithm (mirrors the RL PoC's `SandboxFleet.plan()` at `fleet.py:250-291`,
  rebased on the new registry):

    1. Pick a Placement selector by name.
    2. **Spread-first pre-pass** (v1.5): give each fresh cluster ONE model
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
  fresh_clusters = sorted(registry.fresh(), key=lambda c: c.name)

  # image-affinity hashes model.image to pin the pool to a cluster; missing
  # images make that impossible. Fail fast rather than silently misplace.
  if spec.placement_policy == "image-affinity":
    missing = [m.template_name for m in spec.models if not m.image]
    if missing:
      raise ValueError(
          f"placement_policy=image-affinity requires an image on every model; "
          f"missing on templates: {missing}"
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
  chosen: dict[str, PlannerCluster] = {}
  per_cluster_tasks: dict[str, int] = defaultdict(int)
  per_cluster_models: dict[str, list[ModelSpec]] = defaultdict(list)

  if spec.min_clusters > 0:
    if not fresh_clusters:
      rr_target_count = 0  # nothing to place; downstream produces empty assignment
    else:
      rr_target_count = min(spec.min_clusters, len(fresh_clusters))
      if spec.min_clusters > len(fresh_clusters):
        logger.warning(
            "min_clusters=%d but only %d fresh clusters available; using %d",
            spec.min_clusters, len(fresh_clusters), rr_target_count,
        )
    for i, model in enumerate(spec.models):
      if rr_target_count == 0:
        continue
      cluster = fresh_clusters[i % rr_target_count]
      chosen[model.image] = cluster
      cluster.planned_replicas += 1
      per_cluster_tasks[cluster.name] += model.target_tasks
      per_cluster_models[cluster.name].append(model)
  else:
    for i, model in enumerate(spec.models):
      if i < len(fresh_clusters):
        # First N models: deterministic one-per-cluster. Forces spread even
        # when the scored selector would prefer to double up.
        cluster = fresh_clusters[i]
      else:
        # After each cluster has at least one pool, honor the configured
        # selector (usually capacity-aware) for extras.
        cluster = selector.select(model.image, registry)
      chosen[model.image] = cluster
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

  # Ensure every registry cluster has an entry (empty pools = "drop everything")
  for cname in registry.clusters:
    clusters.setdefault(cname, ClusterAssignment(pools=[]))

  now_iso = _dt.datetime.now(_dt.timezone.utc).isoformat().replace("+00:00", "Z")
  return Assignments(generation=spec.generation, updated_at=now_iso, clusters=clusters)


def _warmpool_name(template: str) -> str:
  return f"{template}-pool"


def publish(gcs: GCS, assignments: Assignments, paths: Paths | None = None) -> None:
  paths = paths or Paths()
  gcs.put_json(paths.assignments, assignments.model_dump())
  logger.info("wrote %s (generation=%d, %d clusters)",
              paths.assignments, assignments.generation, len(assignments.clusters))


def apply(
    gcs: GCS,
    spec: FleetSpec,
    paths: Paths | None = None,
    provider: InventoryProvider | None = None,
) -> Assignments:
  """One-shot: load registry, plan, publish, and also persist the spec.

  `provider` selects where cluster inventory comes from. Defaults to the GCS
  capacity reports. Note that GCS remains the transport for the spec and the
  assignments regardless — only the inventory source is pluggable.
  """
  paths = paths or Paths()
  gcs.put_json(paths.spec, spec.model_dump())
  provider = provider or _inventory.GCSInventory(gcs, paths)
  reg = provider.load(spec.cluster_weights)
  assn = plan(spec, reg)
  publish(gcs, assn, paths)
  return assn
