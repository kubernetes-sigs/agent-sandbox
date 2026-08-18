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

"""Regression proof of the harvest: our hamilton_split matches the RL PoC's
_split_budget for a battery of inputs. Kept as a standalone test so we can
detect drift if either implementation is edited later.
"""

from __future__ import annotations

import math
import pytest

from agent_sandbox_fleet.budget import hamilton_split


def _reference_split_budget(total, weights):
  """Verbatim copy of agent_sandbox_rl.fleet._split_budget."""
  if not weights:
    return {}
  if len(weights) == 1:
    return {next(iter(weights)): total}
  tw = sum(weights.values())
  ideal = {k: total * (w / tw) for k, w in weights.items()}
  alloc = {k: int(math.floor(v)) for k, v in ideal.items()}
  remainder = total - sum(alloc.values())
  for k in sorted(weights, key=lambda k: ideal[k] - alloc[k], reverse=True)[:remainder]:
    alloc[k] += 1
  return alloc


@pytest.mark.parametrize("total, weights", [
    (100, {"a": 1.0, "b": 1.0}),
    (100, {"a": 1.0, "b": 2.0, "c": 3.0}),
    (7, {"a": 1.0, "b": 1.0, "c": 1.0}),
    (0, {"a": 1.0, "b": 1.0}),
    (1, {"a": 0.5}),
    (10, {}),
    (1000, {f"c{i}": (i + 1) * 0.7 for i in range(20)}),
])
def test_matches_tomer(total, weights):
  assert hamilton_split(total, weights) == _reference_split_budget(total, weights)
  # Sums to exactly `total` (unless weights empty).
  if weights:
    assert sum(hamilton_split(total, weights).values()) == total


def test_single_cluster_shortcut():
  assert hamilton_split(42, {"only": 1.0}) == {"only": 42}


def test_empty_weights():
  assert hamilton_split(50, {}) == {}
