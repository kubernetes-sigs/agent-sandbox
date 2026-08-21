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

"""Warm-pool replica sizing.

Harvested from `agent_sandbox_rl/sizing.py`. Only `compute_replicas` is kept —
the sliding-window heuristics (`recommend_window*`) are strategy-layer concerns
that the planner does not need.
"""

from __future__ import annotations


def compute_replicas(
    tasks_image: int,
    tasks_total: int,
    max_concurrent: int,
    max_pool: int,
    *,
    buffer: int = 0,
    per_task: bool = False,
) -> int:
    """Replicas to pre-warm for one image on one cluster.

    Default (concurrency-proportional):
      clamp(round(max_concurrent * tasks_image / tasks_total),
            1, min(tasks_image, max_pool)) + buffer
    Then re-clamped by (tasks_image, max_pool).

    `per_task=True` warms one replica per task (RL instant-claim sizing), still
    capped by max_pool.
    """
    if tasks_image <= 0:
        return 0
    if per_task:
        return min(tasks_image, max_pool)
    if tasks_total <= 0:
        tasks_total = tasks_image
    share = max_concurrent * tasks_image / tasks_total
    replicas = max(1, round(share)) + max(0, buffer)
    return int(min(replicas, tasks_image, max_pool))
