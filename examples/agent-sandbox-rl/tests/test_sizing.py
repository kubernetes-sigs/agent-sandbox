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
from agent_sandbox_rl.sizing import (
    recommend_window_disk,
    recommend_window_pipelined,
)

# 8 images, 1 task each -> 1 replica per image regardless of concurrency.
_EIGHT = OrderedDict((f"img{i}", 1) for i in range(8))


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


def test_per_task_warms_one_replica_per_task():
  # ignores the concurrency share: replicas == tasks_image (capped by max_pool)
  assert compute_replicas(10, 100, 40, 16, per_task=True) == 10
  assert compute_replicas(2, 100, 40, 8, per_task=True) == 2     # was 1 by share
  assert compute_replicas(10, 100, 40, 8, per_task=True) == 8    # clamped to max_pool
  assert compute_replicas(1, 1000, 1, 8, per_task=True) == 1
  assert compute_replicas(0, 100, 8, 32, per_task=True) == 0     # no tasks -> none


def test_disk_window_is_node_aware():
  # distinct images spread across the pool: a single node's disk caps small, but the
  # cluster-wide budget (nodes x per-node) uses the whole pool.
  totals = OrderedDict((f"img{i}", 1) for i in range(500))
  assert recommend_window_disk(totals, 500, 64, avg_image_gb=10, usable_disk_gb=254) == 25
  assert recommend_window_disk(totals, 500, 64, avg_image_gb=10, usable_disk_gb=254,
                               nodes=30) == 500       # capped only by concurrency now
  # pipelined halves the concurrency window (250), node-aware disk no longer the limit
  assert recommend_window_pipelined(totals, 500, 64, avg_image_gb=10,
                                    usable_disk_gb=254, nodes=30) == 250


def test_per_task_window_packs_fewer_images():
  totals = OrderedDict((f"img{i}", 4) for i in range(8))   # 4 tasks each
  # share path: each image rounds to 1 replica at budget 8 -> window 8.
  assert recommend_window(totals, 8, 32) == 8
  # per-task: each image now costs 4 replicas, budget 8 -> only 2 fit.
  assert recommend_window(totals, 8, 32, per_task=True) == 2
  # disk sizing also reflects the bigger per-image footprint.
  assert recommend_window_disk(
      totals, 8, 32, avg_image_gb=1, usable_disk_gb=10, per_task=True) == 2


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


# --- disk-aware sizing ---------------------------------------------------- #
def test_disk_window_caps_to_fit_node_disk():
  # 4 GB each, 10 GB usable -> only 2 fit (2*4=8 <= 10, 3rd would be 12).
  assert recommend_window_disk(_EIGHT, 8, 32, avg_image_gb=4, usable_disk_gb=10) == 2


def test_disk_window_allows_all_when_disk_large():
  assert recommend_window_disk(_EIGHT, 8, 32, avg_image_gb=4, usable_disk_gb=1000) == 8


def test_disk_window_at_least_one_even_if_image_exceeds_disk():
  # A single image bigger than the whole disk still yields a window of 1.
  assert recommend_window_disk(_EIGHT, 8, 32, avg_image_gb=100, usable_disk_gb=10) == 1


def test_disk_window_disabled_is_unbounded():
  # avg/usable falsy -> disk doesn't constrain (returns all images).
  assert recommend_window_disk(_EIGHT, 8, 32, avg_image_gb=0, usable_disk_gb=10) == 8


def test_disk_window_respects_replicas_on_skewed_totals():
  totals = OrderedDict([("a", 40), ("b", 20), ("c", 12)])  # replicas 18,9,5 @ mc=32
  assert [compute_replicas(c, 72, 32, 32) for c in totals.values()] == [18, 9, 5]
  # a alone = 18 GB > 12 -> window 1 (the first image always fits).
  assert recommend_window_disk(totals, 32, 32, avg_image_gb=1, usable_disk_gb=12) == 1
  # 30 GB fits a(18)+b(9)=27; c(+5)=32 > 30 -> window 2.
  assert recommend_window_disk(totals, 32, 32, avg_image_gb=1, usable_disk_gb=30) == 2
  # 40 GB fits all three (32) -> window 3.
  assert recommend_window_disk(totals, 32, 32, avg_image_gb=1, usable_disk_gb=40) == 3


def test_pipelined_window_halves_without_disk():
  # recommend_window(_EIGHT, 8, 32) == 8 -> halved to 4.
  assert recommend_window(_EIGHT, 8, 32) == 8
  assert recommend_window_pipelined(_EIGHT, 8, 32) == 4


def test_pipelined_window_disk_min_wins():
  # halved conc window 4, but 4 GB each / 10 GB usable with 2x factor (budget 5)
  # -> only 1 fits -> min(4, 1) == 1.
  assert recommend_window_pipelined(
      _EIGHT, 8, 32, avg_image_gb=4, usable_disk_gb=10) == 1


def test_pipelined_window_disk_none_is_noop():
  assert recommend_window_pipelined(_EIGHT, 8, 32, avg_image_gb=None) == 4
