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

from collections import OrderedDict

from agent_sandbox_rl import compute_replicas, plan, recommend_window


def test_verified_one_to_one_is_one():
  # 1 task per image, any concurrency -> exactly 1 replica.
  for mc in (1, 8, 256):
    assert compute_replicas(1, 500, mc, 8) == 1


def test_proportional_share():
  # 40/100 of an 8-wide budget -> round(3.2) = 3, under the cap.
  assert compute_replicas(40, 100, 8, 32) == 3


def test_never_exceeds_task_count():
  assert compute_replicas(2, 100, 256, 32) == 2


def test_capped_by_max_pool():
  assert compute_replicas(100, 100, 256, 32) == 32


def test_at_least_one_when_tasks_present():
  assert compute_replicas(1, 1000, 1, 8) == 1


def test_zero_tasks_zero_replicas():
  assert compute_replicas(0, 100, 8, 32) == 0


def test_tasks_total_fallback():
  # tasks_total <= 0 falls back to tasks_image (share == max_concurrent capped).
  assert compute_replicas(5, 0, 4, 32) == 4


def test_buffer_adds_then_clamps():
  assert compute_replicas(10, 100, 1, 32, buffer=2) == 3   # max(1,round(0.1))+2
  assert compute_replicas(2, 100, 1, 32, buffer=5) == 2    # clamped to tasks_image


def test_plan_footprint_and_window_verified():
  totals = OrderedDict((f"img{i}", 1) for i in range(8))
  per, foot = plan(totals, max_concurrent=8, max_pool=32)
  assert all(v == 1 for v in per.values())
  assert foot == 8
  # 8 single-replica images, budget 8 -> window 8.
  assert recommend_window(totals, 8, 32) == 8
  # budget 1 -> keep only 1 warm at a time.
  assert recommend_window(totals, 1, 32) == 1


def test_plan_skewed_footprint_drops_with_concurrency():
  totals = OrderedDict([("a", 40), ("b", 20), ("c", 12), ("d", 8),
                        ("e", 6), ("f", 6), ("g", 4), ("h", 4)])
  baseline = sum(min(c, 32) for c in totals.values())  # 92
  _, foot_low = plan(totals, max_concurrent=8, max_pool=32)
  _, foot_full = plan(totals, max_concurrent=256, max_pool=32)
  assert baseline == 92
  assert foot_low < baseline       # concurrency-aware sizing is smaller
  assert foot_full == baseline     # unlimited concurrency -> warm everything
