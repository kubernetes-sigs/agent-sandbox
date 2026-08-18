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

"""Cluster placement policies.

Harvested from `examples/agent-sandbox-rl/agent_sandbox_rl/placement.py` and
rebased on a lightweight `PlannerCluster` (a snapshot of a cluster's live state
read from GCS capacity reports) instead of the k8s-connected `Cluster` in
that PoC.

Adds `CapacityAware` — the new selector that reads live per-cluster capacity
from object storage and weights placement by (weight × ready_ratio /
(1 + load × pressure)). Replaces the static-weight `CapacityWeighted`.
"""

from __future__ import annotations

import hashlib
import itertools
import threading
from dataclasses import dataclass, field
from typing import Iterable, Protocol


class NoClusterAvailableError(RuntimeError):
  """No cluster in the registry has fresh capacity for a placement."""


# --------------------------------------------------------------------------- #
# PlannerCluster and registry — the planner-side stand-ins for the RL PoC's Cluster
# and ClusterRegistry (`agent_sandbox_rl/cluster.py:35-76`). Built from the
# CapacityReport JSON pulled from GCS; no live k8s connection.
# --------------------------------------------------------------------------- #

@dataclass
class PlannerCluster:
  """Snapshot of one member cluster, built from its GCS capacity report."""

  name: str
  weight: float = 1.0
  max_replicas: int | None = None
  # Live signals published by the agent
  warmpool_depth: int = 0
  warmpool_ready: int = 0
  # None == the member could not measure it. Distinct from 0 == no claims.
  active_claims: int | None = None
  claim_p90_ms: float = 0.0
  # None == the member could not measure it. Distinct from 0.0 == idle.
  node_pressure_score: float | None = None
  # Metadata
  report_age_s: float = 0.0
  # Bookkeeping populated during a single planner run
  planned_replicas: int = 0

  @property
  def active_replicas(self) -> int:
    """For selector-compat with the RL PoC's `Cluster.active_replicas`."""
    return self.warmpool_depth + self.planned_replicas

  @property
  def ready_ratio(self) -> float:
    if self.warmpool_depth == 0:
      return 1.0
    return self.warmpool_ready / self.warmpool_depth

  def has_capacity(self, need: int = 1) -> bool:
    """Same semantic as the RL PoC's Cluster.has_capacity."""
    if self.max_replicas is None:
      return True
    return self.active_replicas + need <= self.max_replicas


@dataclass
class PlannerRegistry:
  clusters: dict[str, PlannerCluster] = field(default_factory=dict)
  # A capacity report older than this is treated as absent. Matches the alert
  # threshold in ARCHITECTURE.md.
  max_report_age_s: float = 90.0

  def __iter__(self) -> Iterable[PlannerCluster]:
    return iter(self.fresh())

  def names(self) -> list[str]:
    return [c.name for c in self.fresh()]

  def get(self, name: str) -> PlannerCluster:
    return self.clusters[name]

  def fresh(self) -> list[PlannerCluster]:
    """Only clusters with a recent capacity report are placement-eligible."""
    return [c for c in self.clusters.values()
            if c.report_age_s <= self.max_report_age_s]


# --------------------------------------------------------------------------- #
# Selectors — harvested verbatim from the RL PoC's placement.py, rebased on the
# planner-side types above. The signatures match, so the algorithms port 1:1.
# --------------------------------------------------------------------------- #

def _eligible(registry: PlannerRegistry, need: int = 1) -> list[PlannerCluster]:
  elig = [c for c in registry if c.has_capacity(need)]
  if not elig:
    raise NoClusterAvailableError(
        "no cluster has capacity (check max_replicas, load, or capacity-report freshness)")
  return elig


class Placement(Protocol):
  def select(self, image: str, registry: PlannerRegistry) -> PlannerCluster: ...


class RoundRobin:
  """Cycle over stable registry order, skipping ineligible."""

  def __init__(self):
    self._counter = itertools.count()
    self._lock = threading.Lock()

  def select(self, image: str, registry: PlannerRegistry) -> PlannerCluster:
    elig = {c.name for c in _eligible(registry)}
    ordered = list(registry)
    with self._lock:
      start = next(self._counter)
    n = len(ordered)
    for i in range(n):
      c = ordered[(start + i) % n]
      if c.name in elig:
        return c
    return ordered[start % n]


class LeastLoaded:
  def select(self, image: str, registry: PlannerRegistry) -> PlannerCluster:
    elig = _eligible(registry)
    # Unmeasured (None) sorts LAST, not as zero. A cluster whose claim list
    # failed is the one we know least about, so it must not win the tiebreak
    # by looking perfectly idle. When nothing measured it — `light` mode, say
    # — every cluster ties on the first key and this degrades to
    # active_replicas, which is what --capacity-detail advertises.
    return min(elig, key=lambda c: (c.active_claims is None,
                                    c.active_claims or 0,
                                    c.active_replicas))


class CapacityWeighted:
  """Static-weight variant kept for parity with the RL PoC."""

  def select(self, image: str, registry: PlannerRegistry) -> PlannerCluster:
    elig = _eligible(registry)
    return max(elig, key=lambda c: c.weight / (1 + c.active_replicas))


class ImageAffinity:
  """MD5-mod-#clusters affinity → same image, same cluster. Falls back to LeastLoaded."""

  def __init__(self):
    self._fallback = LeastLoaded()

  def select(self, image: str, registry: PlannerRegistry) -> PlannerCluster:
    _eligible(registry)
    names = sorted(registry.names())
    digest = hashlib.md5(image.encode(), usedforsecurity=False).hexdigest()
    target = registry.get(names[int(digest, 16) % len(names)])
    if target.has_capacity(1):
      return target
    return self._fallback.select(image, registry)


class CapacityAware:
  """New selector — weights placement by live capacity signals.

  Score:  weight × ready_ratio / (1 + load_factor × (1 + pressure))
    where load_factor = active_replicas
          pressure    = node_pressure_score in [0, 1]
          ready_ratio = warmpool_ready / warmpool_depth

  Falls back to `LeastLoaded` when all scores tie (fresh cluster, no signal yet).
  This is the placement policy the PoC recommends for production RL fleets and
  the direct replacement for the static `CapacityWeighted`.
  """

  def __init__(self):
    self._fallback = LeastLoaded()

  def select(self, image: str, registry: PlannerRegistry) -> PlannerCluster:
    elig = _eligible(registry)

    # A cluster that could not measure its pressure must not be treated as
    # idle — that would reward the failure by making it look most attractive.
    # Substitute the mean of the clusters that DID report, so an unmeasured
    # cluster is neither favoured nor penalised. If nobody reported, the term
    # is 0 for everyone, which cancels out of the comparison entirely.
    known = [c.node_pressure_score for c in elig
             if c.node_pressure_score is not None]
    default_pressure = sum(known) / len(known) if known else 0.0

    def score(c: PlannerCluster) -> float:
      load = c.active_replicas
      raw = (default_pressure if c.node_pressure_score is None
             else c.node_pressure_score)
      pressure = max(0.0, min(1.0, raw))
      return c.weight * c.ready_ratio / (1 + load * (1 + pressure))

    scores = {c.name: score(c) for c in elig}
    best = max(elig, key=lambda c: scores[c.name])
    # If everything ties (equal fresh clusters, no load yet), delegate.
    if len(set(scores.values())) == 1:
      return self._fallback.select(image, registry)
    return best


_REGISTRY: dict[str, type[Placement]] = {
    "round-robin": RoundRobin,
    "least-loaded": LeastLoaded,
    "capacity-weighted": CapacityWeighted,
    "image-affinity": ImageAffinity,
    "capacity-aware": CapacityAware,
}


def get_placement(name: str) -> Placement:
  try:
    return _REGISTRY[name]()
  except KeyError:
    raise ValueError(f"unknown placement '{name}'; choose from {sorted(_REGISTRY)}")
