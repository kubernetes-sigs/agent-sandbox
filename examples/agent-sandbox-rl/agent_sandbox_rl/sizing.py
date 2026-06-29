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

The right warm-pool depth for an image is the number of sandboxes likely to be
claimed *simultaneously* for it — its share of the global concurrency budget
(``max_concurrent``) — never more than its own task count, within the hard
per-pool cap. Replaces the naive ``min(tasks_image, max_pool)`` (which ignores
concurrency and over-provisions). Run ``python -m agent_sandbox_rl.sizing`` for
an old-vs-new demo.
"""

from collections import OrderedDict


def compute_replicas(
    tasks_image: int,
    tasks_total: int,
    max_concurrent: int,
    max_pool: int,
    *,
    buffer: int = 0,
    per_task: bool = False,
) -> int:
  """Replicas to pre-warm for one image.

  Default (concurrency-proportional): ``clamp(round(max_concurrent * tasks_image
  / tasks_total), 1, min(tasks_image, max_pool)) + buffer`` (re-clamped).

  ``per_task=True`` instead warms **one replica per task** for the image —
  ``min(tasks_image, max_pool)`` — so every task can claim a sandbox immediately
  (RL "instant-claim" sizing). ``max_pool`` still caps it; raise
  ``max_warmpool_size`` if an image has more tasks than the cap.
  """
  if tasks_image <= 0:
    return 0
  if per_task:                       # one replica per task (tasks_image >= 1 here)
    return min(tasks_image, max_pool)
  if tasks_total <= 0:
    tasks_total = tasks_image
  share = max_concurrent * tasks_image / tasks_total
  replicas = max(1, round(share)) + max(0, buffer)
  return int(min(replicas, tasks_image, max_pool))


def recommend_window(
    image_totals: "OrderedDict[str, int]",
    max_concurrent: int,
    max_pool: int,
    *,
    per_task: bool = False,
) -> int:
  """For the sliding strategy: how many image pools to keep warm so the total
  warm footprint stays ~ ``max_concurrent``."""
  total = sum(image_totals.values()) or 1
  budget = max(1, max_concurrent)
  used = 0
  window = 0
  for cnt in image_totals.values():
    r = compute_replicas(cnt, total, max_concurrent, max_pool, per_task=per_task)
    if window >= 1 and used + r > budget:
      break
    used += r
    window += 1
  return max(1, window)


def recommend_window_disk(
    image_totals: "OrderedDict[str, int]",
    max_concurrent: int,
    max_pool: int,
    *,
    avg_image_gb: float,
    usable_disk_gb: float,
    pipeline_factor: float = 1.0,
    buffer: int = 0,
    per_task: bool = False,
    nodes: int = 1,
) -> int:
  """Largest window whose resident image bytes fit the usable node disk.

  Disk budget = ``usable_disk_gb · nodes`` (the cluster's usable ephemeral disk):
  distinct images **spread across the node pool**, so the resident set is bounded by
  total cluster disk, not a single node's. With the default ``nodes=1`` this reduces
  to the conservative single-node bound (a window's replicas might all co-locate);
  pass the real node count to use the pool's full capacity.
  ``pipeline_factor`` accounts for keeping up to N windows resident at once (2 for
  the pipelined strategy). Returns ≥ 1 (always allow at least one image)."""
  if not image_totals:
    return 1
  if not avg_image_gb or not usable_disk_gb:
    return len(image_totals)               # disk-unbounded
  total = sum(image_totals.values()) or 1
  budget_gb = (usable_disk_gb * max(1, nodes)) / max(1e-9, pipeline_factor)
  used_gb = 0.0
  window = 0
  for cnt in image_totals.values():
    cost = compute_replicas(cnt, total, max_concurrent, max_pool,
                            buffer=buffer, per_task=per_task) * avg_image_gb
    if window >= 1 and used_gb + cost > budget_gb:
      break
    used_gb += cost
    window += 1
  return max(1, window)


def recommend_window_pipelined(
    image_totals: "OrderedDict[str, int]",
    max_concurrent: int,
    max_pool: int,
    *,
    avg_image_gb: float | None = None,
    usable_disk_gb: float | None = None,
    buffer: int = 0,
    per_task: bool = False,
    nodes: int = 1,
) -> int:
  """Window for the pipelined (double-buffered) strategy.

  The pipeline keeps up to **two** windows resident at once, so halve the
  concurrency window to keep peak ~ ``max_concurrent``; then, if disk hints are
  given, cap by disk with a 2x factor (each cap applies its factor exactly once).
  ``nodes`` lets the disk cap use the whole pool's disk (distinct images spread)."""
  conc_win = max(1, recommend_window(
      image_totals, max_concurrent, max_pool, per_task=per_task) // 2)
  if avg_image_gb is None or usable_disk_gb is None:
    return conc_win
  disk_win = recommend_window_disk(
      image_totals, max_concurrent, max_pool, avg_image_gb=avg_image_gb,
      usable_disk_gb=usable_disk_gb, pipeline_factor=2.0, buffer=buffer,
      per_task=per_task, nodes=nodes)
  return max(1, min(conc_win, disk_win))


def plan(image_totals, max_concurrent, max_pool, *, buffer=0, per_task=False):
  """Returns ``(per_image_replicas: OrderedDict, total_warm_footprint: int)``."""
  total = sum(image_totals.values()) or 1
  per = OrderedDict(
      (img, compute_replicas(c, total, max_concurrent, max_pool,
                             buffer=buffer, per_task=per_task))
      for img, c in image_totals.items()
  )
  return per, sum(per.values())


def _baseline(cnt, max_pool):
  return min(cnt, max_pool)


if __name__ == "__main__":
  dists = {
      "verified-like (1:1, 8 images)": OrderedDict((f"img{i}", 1) for i in range(8)),
      "skewed batch (8 images)": OrderedDict([
          ("django", 40), ("astropy", 20), ("sympy", 12),
          ("flask", 8), ("numpy", 6), ("scipy", 6), ("pandas", 4), ("pytest", 4),
      ]),
  }
  MAX_POOL = 32
  for name, totals in dists.items():
    tot = sum(totals.values())
    print(f"\n=== {name}  (tasks_total={tot}, MAX_WARMPOOL_SIZE={MAX_POOL}) ===")
    base_total = sum(_baseline(c, MAX_POOL) for c in totals.values())
    print(f"  baseline footprint (min(count,cap), all warm): {base_total} pods")
    for mc in (1, 8, 32, 256):
      per, foot = plan(totals, mc, MAX_POOL)
      win = recommend_window(totals, mc, MAX_POOL)
      sample = ", ".join(f"{k}:{v}" for k, v in list(per.items())[:4])
      print(f"  MAX_CONCURRENT={mc:>3}: naive footprint={foot:>3} pods "
            f"| sliding window={win:>2} | per-image[{sample}, ...]")
