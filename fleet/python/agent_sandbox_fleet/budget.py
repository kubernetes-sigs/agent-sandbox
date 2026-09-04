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

"""Global concurrency budget split across weighted clusters.

Harvested verbatim from `agent_sandbox_rl/fleet.py:48-63` — Hamilton
largest-remainder allocation, guaranteed to sum to exactly `total`.
"""

from __future__ import annotations

import math


def hamilton_split(total: int, weights: dict[str, float]) -> dict[str, int]:
    """Split an integer `total` across keys by `weights` using largest-remainder
    (Hamilton) allocation. Sums to exactly `total` (no rounding overshoot).

    Copy of `agent_sandbox_rl.fleet._split_budget`.
    """
    if not weights:
        return {}
    # Weights reach here from a user-authored spec and from ClusterProfile
    # properties, so neither source is trusted. Every bad value below fails
    # somewhere unhelpful otherwise: all-zero divides by zero, a negative
    # weight hands out a negative budget, inf/nan poison `ideal` and then
    # math.floor raises OverflowError/ValueError from inside the sort.
    bad = {k: w for k, w in weights.items()
           if not math.isfinite(w) or w < 0}
    if bad:
        raise ValueError(
            f"cluster weights must be finite and >= 0; got {bad}"
        )
    if len(weights) == 1:
        return {next(iter(weights)): total}
    tw = sum(weights.values())
    if tw == 0:
        # Every active cluster weighted 0. Treat as "no preference" and split
        # evenly rather than refusing to plan — the clusters are eligible, the
        # operator just expressed no ranking among them.
        return hamilton_split(total, {k: 1.0 for k in weights})
    ideal = {k: total * (w / tw) for k, w in weights.items()}
    alloc = {k: int(math.floor(v)) for k, v in ideal.items()}
    remainder = total - sum(alloc.values())
    for k in sorted(weights, key=lambda k: ideal[k] - alloc[k], reverse=True)[:remainder]:
        alloc[k] += 1
    return alloc
