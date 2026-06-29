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
) -> int:
  """Replicas to pre-warm for one image.

  ``clamp(round(max_concurrent * tasks_image / tasks_total), 1,
  min(tasks_image, max_pool)) + buffer`` (re-clamped).
  """
  if tasks_image <= 0:
    return 0
  if tasks_total <= 0:
    tasks_total = tasks_image
  share = max_concurrent * tasks_image / tasks_total
  replicas = max(1, round(share)) + max(0, buffer)
  return int(min(replicas, tasks_image, max_pool))


def recommend_window(
    image_totals: "OrderedDict[str, int]",
    max_concurrent: int,
    max_pool: int,
) -> int:
  """For the sliding strategy: how many image pools to keep warm so the total
  warm footprint stays ~ ``max_concurrent``."""
  total = sum(image_totals.values()) or 1
  budget = max(1, max_concurrent)
  used = 0
  window = 0
  for cnt in image_totals.values():
    r = compute_replicas(cnt, total, max_concurrent, max_pool)
    if window >= 1 and used + r > budget:
      break
    used += r
    window += 1
  return max(1, window)


def plan(image_totals, max_concurrent, max_pool, *, buffer=0):
  """Returns ``(per_image_replicas: OrderedDict, total_warm_footprint: int)``."""
  total = sum(image_totals.values()) or 1
  per = OrderedDict(
      (img, compute_replicas(c, total, max_concurrent, max_pool, buffer=buffer))
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
