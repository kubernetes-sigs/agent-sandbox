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
an asyncio loop. It reuses the tested sync core, running each blocking k8s call on
a **dedicated thread pool sized to ``max_concurrent``** (see `_thread_pool` — *not*
``asyncio.to_thread``'s shared default pool, which is tied to the CPU count and
would starve/deadlock the `pipelined` strategy's overlapping warm/unwarm) and
driving real concurrency via ``asyncio.gather`` + a `Semaphore`. ``process_fn`` may
be sync or async.

(Design note: this is a thread-backed asyncio wrapper rather than a native
``kubernetes_asyncio`` rewrite — it delivers the same awaitable API + concurrency
with far less risk, reusing the cluster/resources/strategies logic verbatim. A
native async I/O backend can replace the internals later without changing this
API.)
"""

from __future__ import annotations

import asyncio
import functools
import inspect
import logging
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Awaitable, Callable

from .observability import repo_family

from .config import FleetConfig
from .fleet import SandboxFleet
from .handles import SandboxHandle
from .sources import Task

logger = logging.getLogger("agent_sandbox_rl.async_fleet")


class AsyncSandboxFleet:
  """Awaitable wrapper over `SandboxFleet`."""

  def __init__(self, config: FleetConfig | None = None, registry=None,
               *, sync_fleet: SandboxFleet | None = None):
    self._fleet = sync_fleet or SandboxFleet(config, registry)
    self._executor: ThreadPoolExecutor | None = None

  # --- dedicated thread pool (NOT asyncio's default) --------------------- #
  def _thread_pool(self) -> ThreadPoolExecutor:
    """A thread pool sized to the fleet's concurrency, used for all blocking k8s
    calls — instead of ``asyncio.to_thread``'s shared default pool
    (``min(32, cpu+4)``). That default is tied to the driver's CPU count, not
    ``max_concurrent``: the ``pipelined`` strategy overlaps prefetch + process +
    unwarm, and ``wait_for_pool_ready`` holds its thread, so the default pool
    starves teardown and deadlocks. Size for ~process (max_concurrent) + two
    windows (prefetch + unwarm) + headroom."""
    if self._executor is None:
      mc = max(1, self._fleet.config.max_concurrent)
      win = self._fleet.config.window_size or mc
      workers = min(1024, max(64, mc + 2 * win + 16))
      self._executor = ThreadPoolExecutor(
          max_workers=workers, thread_name_prefix="asrl-async")
      logger.debug("async fleet thread pool: %d workers", workers)
    return self._executor

  async def _to_thread(self, fn, *args, **kwargs):
    """Run a blocking call on the dedicated pool (drop-in for asyncio.to_thread)."""
    loop = asyncio.get_running_loop()
    return await loop.run_in_executor(
        self._thread_pool(), functools.partial(fn, *args, **kwargs))

  def close(self, *, wait: bool = True) -> None:
    """Shut down the dedicated thread pool (idempotent). Prefer ``async with`` /
    ``close()``; the fleet is reusable, so the pool persists across ``run()`` calls
    until then.

    ``wait=True`` (default, the explicit-close path) blocks until outstanding work
    finishes and cancels queued-but-unstarted work, so no non-daemon worker thread
    outlives the close. ``__del__`` passes ``wait=False`` to avoid hanging during
    GC/interpreter finalization.
    """
    if self._executor is not None:
      self._executor.shutdown(wait=wait, cancel_futures=True)
      self._executor = None

  def __del__(self):
    # Backstop for the no-context-manager path (`fleet.run()` then discard): don't
    # leak the pool's threads if close()/__aexit__ was never called. Non-blocking
    # so finalization can't hang on in-flight work.
    try:
      self.close(wait=False)
    except Exception:  # pragma: no cover - best-effort at finalization
      pass

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

  def load_tasks(self, source, *, image_rewrite=None) -> list[Task]:
    return self._fleet.load_tasks(source, image_rewrite=image_rewrite)

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
    return await self._to_thread(self._fleet.preflight)

  async def plan(self):
    return await self._to_thread(self._fleet.plan)

  async def ensure_templates(self):
    return await self._to_thread(self._fleet.ensure_templates)

  async def start_warmpools(self, wait: bool = True):
    return await self._to_thread(self._fleet.start_warmpools, wait)

  async def prepull(self, wait: bool = True):
    return await self._to_thread(self._fleet.prepull, wait)

  async def prepull_delete(self):
    return await self._to_thread(self._fleet.prepull_delete)

  async def setup(self, prepull: bool = False) -> "AsyncSandboxFleet":
    await self._to_thread(self._fleet.setup, prepull)
    return self

  async def acquire(self, task: Task) -> SandboxHandle:
    return await self._to_thread(self._fleet.acquire, task)

  async def acquire_batch(self, tasks: list[Task]) -> list[SandboxHandle]:
    return list(await asyncio.gather(*(self.acquire(t) for t in tasks)))

  async def release(self, handle: SandboxHandle):
    return await self._to_thread(self._fleet.release, handle)

  async def release_all(self):
    await asyncio.gather(*(self.release(h) for h in self.handles()))

  async def teardown(self, delete_namespace: bool = False):
    return await self._to_thread(self._fleet.teardown, delete_namespace)

  async def __aenter__(self) -> "AsyncSandboxFleet":
    return await self.setup()

  async def __aexit__(self, *exc):
    try:
      await self.teardown()
    finally:
      self.close()

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
    result = await self._to_thread(process_fn, task, handle)
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
                          replicas_override=None, *, teardown=True):
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
        # Route through the bounded batch warmer (capped by max_concurrent) rather
        # than one _to_thread per image, so a large window can't fan out hundreds
        # of concurrent API watches at once.
        await self._to_thread(self._fleet.warm_images, batch,
                              replicas_override=replicas_override, wait=True)
        batch_pairs = [(i, t) for img in batch for (i, t) in by_image[img]]
        batch_tasks = [t for _i, t in batch_pairs]
        batch_results = await self._process_parallel(batch_tasks, process_fn, concurrency)
        for (i, _t), r in zip(batch_pairs, batch_results, strict=True):
          results[i] = r
        for img in batch:
          await self._to_thread(self._fleet.unwarm_image, img)
    finally:
      if teardown:
        await self.teardown()
    return results

  async def _run_pipelined(self, process_fn, concurrency, window,
                           replicas_override=None, *, teardown=True):
    """Async double-buffered sliding window: prefetch window N+1's pools (an
    ``asyncio.Task``) while window N's tasks run. Footprint ≤ 2 windows — the
    current window is unwarmed before awaiting the next prefetch, and only one
    prefetch task is ever in flight."""
    await self.preflight()
    await self.plan()
    images = list(self.image_counts().keys())
    by_image: dict[str, list] = {}
    for i, t in enumerate(self.tasks):
      by_image.setdefault(t.image, []).append((i, t))   # keep original index
    results = [None] * len(self.tasks)
    batches = [images[s:s + window] for s in range(0, len(images), window)]

    async def _warm(batch):
      # Bounded batch warm (capped by max_concurrent), not one task per image.
      await self._to_thread(self._fleet.warm_images, batch,
                            replicas_override=replicas_override, wait=True)

    async def _prefetch(batch):              # background warm, timed as "prefetch"
      with self._fleet._obs.phase("prefetch"):
        await _warm(batch)

    pending = None
    try:
      if batches:
        await _warm(batches[0])              # prime window 0 in the foreground
      for n, batch in enumerate(batches):
        pending = (asyncio.create_task(_prefetch(batches[n + 1]))
                   if n + 1 < len(batches) else None)
        batch_pairs = [(i, t) for img in batch for (i, t) in by_image[img]]
        batch_tasks = [t for _i, t in batch_pairs]
        batch_results = await self._process_parallel(batch_tasks, process_fn, concurrency)
        for (i, _t), r in zip(batch_pairs, batch_results, strict=True):
          results[i] = r
        for img in batch:
          await self._to_thread(self._fleet.unwarm_image, img)
        if pending is not None:
          await pending                      # surface prefetch errors inside try
          pending = None
    finally:
      if pending is not None:
        # Let the in-flight prefetch finish before teardown: cancelling the task
        # would not stop its warm_image already running on the thread pool, which
        # could create a pool *after* teardown's sweep and leak it. wait_pool_ready
        # is bounded by ready_timeout, so this can't hang indefinitely.
        await asyncio.gather(pending, return_exceptions=True)
      if teardown:
        await self.teardown()
    return results

  async def _run_once(self, strategy, process_fn, conc, *, teardown):
    if strategy == "naive":
      try:
        await self.setup()
        return await self._process_parallel(self.tasks, process_fn, conc)
      finally:
        if teardown:
          await self.teardown()
    if strategy == "sliding":
      return await self._run_windowed(
          process_fn, conc, self._fleet.recommended_window(), teardown=teardown)
    if strategy == "pipelined":
      return await self._run_pipelined(
          process_fn, conc, self._fleet.recommended_window(pipelined=True),
          teardown=teardown)
    if strategy == "none":
      return await self._run_windowed(
          process_fn, conc, 1, replicas_override=1, teardown=teardown)
    raise ValueError(f"unknown strategy '{strategy}'")

  async def run(self, process_fn: Callable[[Task, SandboxHandle], object | Awaitable],
                strategy: str = "naive", concurrency: int | None = None,
                *, epochs: int = 1, keep_warm: bool = False) -> list:
    """Run all loaded tasks under ``strategy`` with up to ``concurrency``
    concurrent claim+exec. ``process_fn`` may be sync or a coroutine function.

    ``epochs``/``keep_warm`` mirror `SandboxFleet.run`: ``epochs>1`` runs N passes
    keeping pools resident between them (returns ``list[list]``); ``keep_warm=True``
    skips the final teardown for caller-driven reuse.
    """
    if epochs < 1:
      raise ValueError("epochs must be >= 1")
    conc = concurrency or self.config.max_concurrent
    obs = self._fleet._obs
    with obs.run(strategy) as report:
      self._fleet.report = report
      try:
        report.environment = await self._to_thread(self._fleet.describe_environment)
      except Exception:  # noqa: BLE001 — environment is best-effort
        logger.debug("could not collect environment", exc_info=True)
      if epochs == 1:
        results = await self._run_once(strategy, process_fn, conc,
                                       teardown=not keep_warm)
      else:
        results = []
        for e in range(epochs):
          last = e == epochs - 1
          logger.info("epoch %d/%d", e + 1, epochs)
          try:
            results.append(await self._run_once(
                strategy, process_fn, conc, teardown=last and not keep_warm))
          except BaseException:               # a mid-run epoch never tore down
            if not keep_warm and not last:
              await self.teardown()
            raise
    logger.info("\n%s", report.summary())
    return results
