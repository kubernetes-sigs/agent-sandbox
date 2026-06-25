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

import pytest

from agent_sandbox_rl import (
    CapacityWeighted,
    ClusterRegistry,
    ImageAffinity,
    LeastLoaded,
    NoClusterAvailableError,
    RoundRobin,
    get_placement,
)


def test_round_robin_cycles(two_cluster_registry):
  reg = two_cluster_registry
  p = RoundRobin()
  picks = [p.select(f"img{i}", reg).name for i in range(4)]
  assert picks == ["a", "b", "a", "b"]


def test_least_loaded_picks_min(two_cluster_registry):
  reg = two_cluster_registry
  reg.get("a").active_claims = 5
  assert LeastLoaded().select("img", reg).name == "b"


def test_capacity_weighted_prefers_higher_weight(make_cluster):
  reg = ClusterRegistry([make_cluster("a", weight=1.0),
                         make_cluster("big", weight=4.0)])
  assert CapacityWeighted().select("img", reg).name == "big"


def test_image_affinity_is_stable(two_cluster_registry):
  reg = two_cluster_registry
  p = ImageAffinity()
  first = p.select("django__django-101", reg).name
  for _ in range(5):
    assert p.select("django__django-101", reg).name == first


def test_image_affinity_falls_back_when_full(make_cluster):
  # Force the affinity target to be full; expect fallback to the other.
  a = make_cluster("a", max_replicas=0)
  b = make_cluster("b", max_replicas=0)
  # one with capacity
  c = make_cluster("c")
  reg = ClusterRegistry([a, b, c])
  # whichever the hash picks, if it's full it must fall back to 'c'.
  for img in ("x", "y", "z", "django", "astropy"):
    chosen = ImageAffinity().select(img, reg)
    assert chosen.has_capacity(1)


def test_no_capacity_raises(make_cluster):
  reg = ClusterRegistry([make_cluster("a", max_replicas=0)])
  with pytest.raises(NoClusterAvailableError):
    LeastLoaded().select("img", reg)


def test_get_placement_factory():
  assert isinstance(get_placement("image-affinity"), ImageAffinity)
  assert isinstance(get_placement("round-robin"), RoundRobin)
  with pytest.raises(ValueError):
    get_placement("nope")
