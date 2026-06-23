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

"""`AsyncSandboxFleet` — an awaitable, event-loop-native fleet.

Same surface as `SandboxFleet`, for RL frameworks (TorchRL/SkyRL/etc.) that own
an asyncio loop. It reuses the tested sync core, running each blocking k8s call
in a worker thread (``asyncio.to_thread``) and driving real concurrency via
``asyncio.gather`` + a `Semaphore`. ``process_fn`` may be sync or async.

(Design note: this is a thread-backed asyncio wrapper rather than a native
``kubernetes_asyncio`` rewrite — it delivers the same awaitable API + concurrency
with far less risk, reusing the cluster/resources/strategies logic verbatim. A
native async I/O backend can replace the internals later without changing this
API.)
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import time
from typing import Awaitable, Callable

from .observability import repo_family

from .config import FleetConfig
from .fleet import SandboxFleet
from .handles import SandboxHandle
from .sizing import recommend_window
from .sources import Task, to_tasks

logger = logging.getLogger("agent_sandbox_rl.async_fleet")


class AsyncSandboxFleet:
  """Awaitable wrapper over `SandboxFleet`."""

  def __init__(self, config: FleetConfig | None = None, registry=None,
               *, sync_fleet: SandboxFleet | None = None):
    self._fleet = sync_fleet or SandboxFleet(config, registry)

  # --- sync passthroughs (cheap, no I/O) --------------------------------- #
  @property
  def config(self) -> FleetConfig:
    return self._fleet.config

  @property
  def registry(self):
    return self._fleet.registry

  @property
  def tasks(self) -> list[Task]:
    return self._fleet.tasks

  @property
  def plan_(self):
    return self._fleet.plan_

  @property
  def report(self):
    return self._fleet.report

  def load_tasks(self, source) -> list[Task]:
    return self._fleet.load_tasks(source)

  def image_counts(self):
    return self._fleet.image_counts()

  def handles(self) -> list[SandboxHandle]:
    return self._fleet.handles()

  def hostnames(self) -> list[str]:
    return self._fleet.hostnames()

  def endpoints(self, port: int = 8888) -> list[str]:
    return self._fleet.endpoints(port)

  # --- awaitable lifecycle (blocking calls offloaded to threads) --------- #
  async def preflight(self):
    return await asyncio.to_thread(self._fleet.preflight)

  async def plan(self):
    return await asyncio.to_thread(self._fleet.plan)

  async def ensure_templates(self):
    return await asyncio.to_thread(self._fleet.ensure_templates)

  async def start_warmpools(self, wait: bool = True):
    return await asyncio.to_thread(self._fleet.start_warmpools, wait)

  async def prepull(self, wait: bool = True):
    return await asyncio.to_thread(self._fleet.prepull, wait)

  async def prepull_delete(self):
    return await asyncio.to_thread(self._fleet.prepull_delete)

  async def setup(self, prepull: bool = False) -> "AsyncSandboxFleet":
    await asyncio.to_thread(self._fleet.setup, prepull)
    return self

  async def acquire(self, task: Task) -> SandboxHandle:
    return await asyncio.to_thread(self._fleet.acquire, task)

  async def acquire_batch(self, tasks: list[Task]) -> list[SandboxHandle]:
    return list(await asyncio.gather(*(self.acquire(t) for t in tasks)))

  async def release(self, handle: SandboxHandle):
    return await asyncio.to_thread(self._fleet.release, handle)

  async def release_all(self):
    await asyncio.gather(*(self.release(h) for h in self.handles()))

  async def teardown(self, delete_namespace: bool = False):
    return await asyncio.to_thread(self._fleet.teardown, delete_namespace)

  async def __aenter__(self) -> "AsyncSandboxFleet":
    return await self.setup()

  async def __aexit__(self, *exc):
    await self.teardown()

  # --- parallel processing + strategies ---------------------------------- #
  async def _call(self, process_fn, task, handle):
    # Fast path: plain coroutine functions and callable objects with `async
    # __call__` are awaited directly on the loop.
    if inspect.iscoroutinefunction(process_fn) or \
        inspect.iscoroutinefunction(getattr(process_fn, "__call__", None)):
      return await process_fn(task, handle)
    # Otherwise run in a worker thread — but some callables that aren't
    # coroutine-functions still RETURN an awaitable (e.g. functools.partial of an
    # async fn, or a sync fn returning a coroutine). Await the result if so,
    # rather than handing back an un-awaited coroutine.
    result = await asyncio.to_thread(process_fn, task, handle)
    if inspect.isawaitable(result):
      return await result
    return result

  async def _process_parallel(self, tasks, process_fn, concurrency):
    sem = asyncio.Semaphore(max(1, concurrency))
    results = [None] * len(tasks)
    obs = self._fleet._obs

    async def _one(i, task):
      fam = repo_family(task)
      t0 = time.monotonic()
      status = "ok"
      cluster = "-"
      async with sem:
        try:
          handle = await self.acquire(task)
          cluster = handle.cluster_name
          try:
            with obs.phase("process", cluster=cluster, family=fam):
              results[i] = await self._call(process_fn, task, handle)
          finally:
            await self.release(handle)
        except Exception as e:  # noqa: BLE001
          status = "error"
          logger.error("task %s failed: %s", task.id, e)
          results[i] = e
        finally:
          obs.task_done(cluster, fam, status, time.monotonic() - t0)

    await asyncio.gather(*(_one(i, t) for i, t in enumerate(tasks)))
    return results

  async def _run_windowed(self, process_fn, concurrency, window,
                          replicas_override=None):
    await self.preflight()
    await self.plan()
    images = list(self.image_counts().keys())
    by_image: dict[str, list] = {}
    for i, t in enumerate(self.tasks):
      by_image.setdefault(t.image, []).append((i, t))   # keep original index
    # Write results back at each task's original index so the returned list
    # matches self.tasks order (the "one result per task" contract), matching
    # the sync sliding implementation.
    results = [None] * len(self.tasks)
    try:
      for start in range(0, len(images), window):
        batch = images[start:start + window]
        await asyncio.gather(*(
            asyncio.to_thread(self._fleet.warm_image, img,
                              replicas_override=replicas_override, wait=True)
            for img in batch))
        batch_pairs = [(i, t) for img in batch for (i, t) in by_image[img]]
        batch_tasks = [t for _i, t in batch_pairs]
        batch_results = await self._process_parallel(batch_tasks, process_fn, concurrency)
        for (i, _t), r in zip(batch_pairs, batch_results, strict=True):
          results[i] = r
        await asyncio.gather(*(
            asyncio.to_thread(self._fleet.unwarm_image, img) for img in batch))
    finally:
      await self.teardown()
    return results

  async def run(self, process_fn: Callable[[Task, SandboxHandle], object | Awaitable],
                strategy: str = "naive", concurrency: int | None = None) -> list:
    """Run all loaded tasks under ``strategy`` with up to ``concurrency``
    concurrent claim+exec. ``process_fn`` may be sync or a coroutine function.
    """
    conc = concurrency or self.config.max_concurrent
    obs = self._fleet._obs
    with obs.run(strategy) as report:
      self._fleet.report = report
      try:
        report.environment = await asyncio.to_thread(self._fleet.describe_environment)
      except Exception:  # noqa: BLE001 — environment is best-effort
        logger.debug("could not collect environment", exc_info=True)
      if strategy == "naive":
        try:
          await self.setup()
          results = await self._process_parallel(self.tasks, process_fn, conc)
        finally:
          await self.teardown()
      elif strategy == "sliding":
        window = self.config.window_size or recommend_window(
            self.image_counts(), self.config.max_concurrent,
            self.config.max_warmpool_size)
        results = await self._run_windowed(process_fn, conc, max(1, window))
      elif strategy == "none":
        results = await self._run_windowed(process_fn, conc, 1, replicas_override=1)
      else:
        raise ValueError(f"unknown strategy '{strategy}'")
    logger.info("\n%s", report.summary())
    return results
