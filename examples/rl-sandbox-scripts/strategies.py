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

"""Warm pool provisioning strategies for running a batch of SWE-bench tasks.

Each strategy decides *when* warm pools for each task image are created and
torn down, trading cluster resource usage against sandbox startup latency.
They are decoupled from any model/agent: callers pass a ``process(task,
pool_name)`` callback that claims a sandbox from ``pool_name`` and does the
work for ``task``.

Adapted from ``run_evaluation`` in the rl-tunix tunix fork
(``examples/deepswe/eval_deepswe.py``), ported to the v1beta1 API.
"""

import collections
import logging
from typing import Callable

import sizing
import warmpool as wp

logger = logging.getLogger("rl-tunix-swebench.strategies")

# A task is any mapping with at least a "docker_image" key.
Task = dict
ProcessFn = Callable[[Task, str], None]


class WarmPoolManager:
  """Convenience wrapper bundling the k8s clients + per-image config."""

  def __init__(
      self,
      custom_api,
      namespace: str,
      *,
      node_selector: dict | None = None,
      image_pull_secret: str | None = None,
      runtime_class: str | None = None,
      ready_timeout: int = 600,
  ):
    self.co = custom_api
    self.namespace = namespace
    self.node_selector = node_selector
    self.image_pull_secret = image_pull_secret
    self.runtime_class = runtime_class
    self.ready_timeout = ready_timeout

  def provision(self, docker_image: str, replicas: int) -> str:
    """Ensures a template + ready warm pool for ``docker_image``. Returns the
    warm pool name."""
    template = wp.get_template_name(docker_image)
    pool = wp.warmpool_name(template)
    wp.ensure_template(
        self.co,
        docker_image,
        template,
        self.namespace,
        node_selector=self.node_selector,
        image_pull_secret=self.image_pull_secret,
        runtime_class=self.runtime_class,
    )
    wp.create_warmpool(self.co, pool, template, replicas, self.namespace)
    wp.wait_for_pool_ready(
        self.co, pool, replicas, self.namespace, timeout=self.ready_timeout
    )
    return pool

  def teardown(self, docker_image: str, *, delete_template: bool = False):
    template = wp.get_template_name(docker_image)
    pool = wp.warmpool_name(template)
    wp.delete_warmpool(self.co, pool, self.namespace)
    if delete_template:
      wp.delete_template(self.co, template, self.namespace)


def _image_counts(entries: list[Task]) -> "collections.OrderedDict[str, int]":
  counts = collections.OrderedDict()
  for e in entries:
    counts[e["docker_image"]] = counts.get(e["docker_image"], 0) + 1
  return counts


def run_none(
    entries: list[Task],
    mgr: WarmPoolManager,
    process: ProcessFn,
) -> None:
  """No warm pools: provision a size-1 pool on demand per task.

  Simplest and lowest idle cost, but every task pays the cold-start (image
  pull + pod start) latency. Useful for debugging / tiny budgets.
  """
  for i, task in enumerate(entries):
    image = task["docker_image"]
    logger.info("[none %d/%d] provisioning on demand for %s",
                i + 1, len(entries), image)
    pool = mgr.provision(image, replicas=1)
    try:
      process(task, pool)
    finally:
      mgr.teardown(image, delete_template=True)


def run_naive(
    entries: list[Task],
    mgr: WarmPoolManager,
    process: ProcessFn,
    *,
    max_warmpool_size: int = 32,
    max_concurrent: int = 1,
) -> None:
  """Naive parallel: pre-warm a pool for *every* unique image up front.

  Fastest per-task startup, highest idle resource reservation. Best for small
  batches or when cluster capacity is ample. Per-image depth is sized to the
  image's share of ``max_concurrent`` (see ``sizing.compute_replicas``).
  """
  counts = _image_counts(entries)
  total = sum(counts.values())
  provisioned = set()
  try:
    for image, count in counts.items():
      replicas = sizing.compute_replicas(
          count, total, max_concurrent, max_warmpool_size)
      logger.info("[naive] pre-warming %s (replicas=%d)", image, replicas)
      mgr.provision(image, replicas=replicas)
      provisioned.add(image)
    for i, task in enumerate(entries):
      pool = wp.warmpool_name(wp.get_template_name(task["docker_image"]))
      logger.info("[naive %d/%d] %s", i + 1, len(entries),
                  task.get("instance_id", i))
      process(task, pool)
  finally:
    for image in provisioned:
      mgr.teardown(image, delete_template=True)


def run_sliding(
    entries: list[Task],
    mgr: WarmPoolManager,
    process: ProcessFn,
    *,
    window_size: int | None = None,
    max_warmpool_size: int = 32,
    max_concurrent: int = 1,
) -> None:
  """Sliding window: keep pools warm for only ``window_size`` images at a time.

  Tasks are grouped by sorting on ``docker_image`` to maximize node-cache and
  warm-pool reuse. As each image's tasks finish, its pool is torn down and the
  next image in line is pre-warmed. Balances startup latency against idle cost
  for large, image-diverse batches.

  ``window_size=None`` (default) auto-picks the window so the total warm
  footprint stays ~ ``max_concurrent`` (see ``sizing.recommend_window``).
  Per-image depth is sized by ``sizing.compute_replicas``.
  """
  entries = sorted(entries, key=lambda e: e["docker_image"])
  counts = _image_counts(entries)
  total = sum(counts.values())
  images = list(counts.keys())
  if window_size is None:
    window_size = sizing.recommend_window(counts, max_concurrent, max_warmpool_size)
    logger.info("[sliding] auto window_size=%d (max_concurrent=%d)",
                window_size, max_concurrent)
  finished = {img: 0 for img in images}
  active = set()
  next_idx = 0

  def warm(idx: int):
    image = images[idx]
    replicas = sizing.compute_replicas(
        counts[image], total, max_concurrent, max_warmpool_size)
    logger.info("[sliding] pre-warming %s (replicas=%d)", image, replicas)
    mgr.provision(image, replicas=replicas)
    active.add(image)

  try:
    for next_idx in range(min(window_size, len(images))):
      warm(next_idx)
    next_idx = min(window_size, len(images))

    for i, task in enumerate(entries):
      image = task["docker_image"]
      pool = wp.warmpool_name(wp.get_template_name(image))
      logger.info("[sliding %d/%d] %s", i + 1, len(entries),
                  task.get("instance_id", i))
      process(task, pool)

      finished[image] += 1
      if finished[image] == counts[image]:
        logger.info("[sliding] image %s done, sliding window", image)
        mgr.teardown(image, delete_template=True)
        active.discard(image)
        if next_idx < len(images):
          warm(next_idx)
          next_idx += 1
  finally:
    for image in list(active):
      mgr.teardown(image, delete_template=True)


STRATEGIES = {
    "none": run_none,
    "naive": run_naive,
    "sliding": run_sliding,
}
