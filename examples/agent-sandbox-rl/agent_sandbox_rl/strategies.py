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

"""Warm-pool provisioning strategies + bounded-parallel task execution.

Each strategy decides *when* image pools exist; all of them process the actual
claim+exec in parallel (up to ``concurrency`` threads — claiming and exec are
blocking IO, so threads parallelize them well). The async variant in a later
phase does the same with asyncio.
"""

from __future__ import annotations

import logging
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from . import sizing
from .observability import repo_family

logger = logging.getLogger("agent_sandbox_rl.strategies")


def process_parallel(fleet, tasks, process_fn, concurrency):
  """Acquire → process → release each task, up to ``concurrency`` at once.

  Returns one entry per task (in input order); a per-task failure is captured as
  the exception object rather than aborting the batch.
  """
  results = [None] * len(tasks)

  def _one(task):
    fam = repo_family(task)
    t0 = time.monotonic()
    status = "ok"
    cluster = "-"
    try:
      handle = fleet.acquire(task)
      cluster = handle.cluster_name
      try:
        with fleet._obs.phase("process", cluster=cluster, family=fam):
          return process_fn(task, handle)
      finally:
        fleet.release(handle)
    except BaseException:
      status = "error"
      raise
    finally:
      fleet._obs.task_done(cluster, fam, status, time.monotonic() - t0)

  if concurrency <= 1:
    for i, t in enumerate(tasks):
      try:
        results[i] = _one(t)
      except Exception as e:  # noqa: BLE001
        logger.error("task %s failed: %s", t.id, e)
        results[i] = e
    return results

  with ThreadPoolExecutor(max_workers=concurrency) as ex:
    futs = {ex.submit(_one, t): i for i, t in enumerate(tasks)}
    for fut in as_completed(futs):
      i = futs[fut]
      try:
        results[i] = fut.result()
      except Exception as e:  # noqa: BLE001
        logger.error("task %s failed: %s", tasks[i].id, e)
        results[i] = e
  return results


def run_naive(fleet, process_fn, concurrency):
  """Pre-warm every image up front, process all tasks in parallel, tear down."""
  try:
    fleet.setup()              # inside try: a setup failure still triggers teardown
    return process_parallel(fleet, fleet.tasks, process_fn, concurrency)
  finally:
    fleet.teardown()


def _run_windowed(fleet, process_fn, concurrency, window, replicas_override=None):
  """Process unique images in batches of ``window``: warm the batch, process its
  tasks in parallel, tear the batch down, then advance."""
  fleet.preflight()
  fleet.plan()
  images = list(fleet.image_counts().keys())
  by_image = {}
  for i, t in enumerate(fleet.tasks):
    by_image.setdefault(t.image, []).append((i, t))   # keep original index

  # Results are written back at each task's original position, so the returned
  # list matches fleet.tasks order (the documented "one result per task" contract)
  # even though tasks are processed grouped by image/window.
  results = [None] * len(fleet.tasks)
  try:
    for start in range(0, len(images), window):
      batch = images[start:start + window]
      for img in batch:
        fleet.warm_image(img, replicas_override=replicas_override, wait=True)
      batch_pairs = [(i, t) for img in batch for (i, t) in by_image[img]]
      batch_tasks = [t for _i, t in batch_pairs]
      logger.info("window [%d..%d): %d image(s), %d task(s)",
                  start, start + len(batch), len(batch), len(batch_tasks))
      batch_results = process_parallel(fleet, batch_tasks, process_fn, concurrency)
      for (i, _t), r in zip(batch_pairs, batch_results, strict=True):
        results[i] = r
      for img in batch:
        fleet.unwarm_image(img)
  finally:
    fleet.teardown()
  return results


def run_sliding(fleet, process_fn, concurrency):
  """Keep only a window of image pools warm at a time (footprint-bounded)."""
  window = fleet.config.window_size
  if window is None:
    window = sizing.recommend_window(
        fleet.image_counts(), fleet.config.max_concurrent,
        fleet.config.max_warmpool_size)
  return _run_windowed(fleet, process_fn, concurrency, max(1, window))


def run_none(fleet, process_fn, concurrency):
  """No pre-warming: one size-1 pool per image, on demand, torn down after."""
  return _run_windowed(fleet, process_fn, concurrency, window=1,
                       replicas_override=1)


STRATEGIES = {
    "none": run_none,
    "naive": run_naive,
    "sliding": run_sliding,
}
