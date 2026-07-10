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


def run_naive(fleet, process_fn, concurrency, *, teardown=True):
  """Pre-warm every image up front, process all tasks in parallel, tear down."""
  try:
    fleet.setup()              # inside try: a setup failure still triggers teardown
    return process_parallel(fleet, fleet.tasks, process_fn, concurrency)
  finally:
    if teardown:
      fleet.teardown()


def _run_windowed(fleet, process_fn, concurrency, window, replicas_override=None,
                  *, teardown=True):
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
      fleet.warm_images(batch, replicas_override=replicas_override, wait=True)
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
    if teardown:
      fleet.teardown()
  return results


def run_sliding(fleet, process_fn, concurrency, *, teardown=True):
  """Keep only a window of image pools warm at a time (footprint-bounded)."""
  return _run_windowed(fleet, process_fn, concurrency,
                       fleet.recommended_window(), teardown=teardown)


def _run_pipelined(fleet, process_fn, concurrency, window,
                   replicas_override=None, *, teardown=True):
  """Double-buffered sliding window: while window N's tasks run, prefetch window
  N+1's pools in the background so image pull overlaps execution.

  Footprint stays bounded at **≤ 2 windows**: a single-slot prefetcher never warms
  more than one future window, and the current window is unwarmed *before* awaiting
  the next prefetch, so we never hold three windows at once."""
  fleet.preflight()
  fleet.plan()
  images = list(fleet.image_counts().keys())
  by_image = {}
  for i, t in enumerate(fleet.tasks):
    by_image.setdefault(t.image, []).append((i, t))   # keep original index
  results = [None] * len(fleet.tasks)
  batches = [images[s:s + window] for s in range(0, len(images), window)]

  def _warm(batch):                         # warm the whole window in parallel
    fleet.warm_images(batch, replicas_override=replicas_override, wait=True)

  def _prefetch(batch):                     # background warm, timed as "prefetch"
    with fleet._obs.phase("prefetch"):
      _warm(batch)

  ex = ThreadPoolExecutor(max_workers=1)    # single slot: never warms >1 future window
  try:
    if batches:
      _warm(batches[0])                     # prime window 0 in the foreground
    for n, batch in enumerate(batches):
      nxt = ex.submit(_prefetch, batches[n + 1]) if n + 1 < len(batches) else None
      batch_pairs = [(i, t) for img in batch for (i, t) in by_image[img]]
      batch_tasks = [t for _i, t in batch_pairs]
      logger.info("pipelined window [%d/%d]: %d image(s), %d task(s)",
                  n + 1, len(batches), len(batch), len(batch_tasks))
      batch_results = process_parallel(fleet, batch_tasks, process_fn, concurrency)
      for (i, _t), r in zip(batch_pairs, batch_results, strict=True):
        results[i] = r
      for img in batch:
        fleet.unwarm_image(img)             # unwarm N before awaiting N+1 -> ≤2 windows
      if nxt is not None:
        nxt.result()                        # surface prefetch errors inside try
  finally:
    ex.shutdown(wait=True)
    if teardown:
      fleet.teardown()
  return results


def run_pipelined(fleet, process_fn, concurrency, *, teardown=True):
  """Pipelined sliding window (see `_run_pipelined`)."""
  return _run_pipelined(fleet, process_fn, concurrency,
                        fleet.recommended_window(pipelined=True), teardown=teardown)


def run_none(fleet, process_fn, concurrency, *, teardown=True):
  """No pre-warming: one size-1 pool per image, on demand, torn down after."""
  return _run_windowed(fleet, process_fn, concurrency, window=1,
                       replicas_override=1, teardown=teardown)


STRATEGIES = {
    "none": run_none,
    "naive": run_naive,
    "sliding": run_sliding,
    "pipelined": run_pipelined,
}
