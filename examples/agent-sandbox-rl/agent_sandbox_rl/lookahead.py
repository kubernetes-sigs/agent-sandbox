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
"""`LookaheadWarmer`: keep warm pools sized for a *stream* of batches.

The fleet's strategies (`naive`, `sliding`, `pipelined`) drive ``fleet.run()``
over a finite task list. An RL trainer is the other way round: its own loop
dispatches batch after batch from a prompt stream, knows the next one or two
batches ahead of time, and may still be claiming for a batch two steps back
(staleness). What it needs from the fleet is a warmer that, given the image
mix of every batch as it becomes known, keeps each image's pool sized for the
batches still in flight and tears pools down once their batches are done.

::

    warmer = LookaheadWarmer(fleet, keep_finished=max_staleness)
    warmer.add(0, counts_of(batch0), wait=True)     # start-up barrier
    warmer.add(1, counts_of(batch1))                # lookahead: warm in the background
    ... dispatch batch 0 ...
    warmer.add(2, counts_of(batch2))
    warmer.finish(0)                                # batch 0's trajectories are released
    ...
    warmer.close()

``counts`` maps a task image to the number of claims that batch will make
(prompts of that image times generations). Desired replicas per image are the
sum over the batches still live, capped at ``cap`` (``max_warmpool_size`` by
default). Each `add` / `finish` applies the difference to the fleet: new images
are warmed (``wait=False``), changed counts are scaled, and images no live batch
uses are unwarmed, all concurrently. Warm errors are collected in the returned
`WarmReport` and logged rather than raised, unless ``fail_fast`` is set, in which
case a `FleetError` from a pool create is re-raised after the whole phase. A pool
that exists but is slow to become ready is never an error: ``wait`` runs a
best-effort readiness barrier (`SandboxFleet.wait_pools_ready`) and reports which
pools were ready in time.
"""

from __future__ import annotations

import collections
import logging
import threading
from collections.abc import Iterable, Mapping
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field

from .exceptions import FleetError
from .sources import Task

logger = logging.getLogger("agent_sandbox_rl.lookahead")


@dataclass
class WarmReport:
  """What one `LookaheadWarmer.add` / `finish` did to the fleet."""

  created: list[str] = field(default_factory=list)    # images warmed anew
  scaled: dict[str, int] = field(default_factory=dict)  # image -> new replicas
  removed: list[str] = field(default_factory=list)    # images unwarmed
  errors: dict[str, str] = field(default_factory=dict)  # image -> error text
  ready: dict[str, bool] = field(default_factory=dict)  # from the readiness barrier

  @property
  def ok(self) -> bool:
    return not self.errors

  def summary(self) -> str:
    parts = [f"created={len(self.created)}", f"scaled={len(self.scaled)}",
             f"removed={len(self.removed)}"]
    if self.errors:
      parts.append(f"errors={len(self.errors)}")
    if self.ready:
      n_ready = sum(1 for v in self.ready.values() if v)
      parts.append(f"ready={n_ready}/{len(self.ready)}")
    return " ".join(parts)


class LookaheadWarmer:
  """Size warm pools for the batches an external step loop has in flight.

  Args:
    fleet: the `SandboxFleet` to drive (an `AsyncSandboxFleet` is unwrapped).
    cap: hard cap on replicas per image; ``None`` uses the fleet's
      ``max_warmpool_size``.
    keep_finished: how many finished batches keep their pools (the trainer's
      staleness window): their claims may still arrive after `finish`.
    max_workers: thread pool width for the concurrent warm / scale / unwarm calls.
    fail_fast: re-raise a `FleetError` from a pool create (a name owned by another
      run, a run-namespace problem) after the phase, instead of only recording it.
    register_images: add an unknown image to the fleet's plan
      (`SandboxFleet.add_tasks`) before warming it, so the stream does not have to
      be loaded as tasks up front.
  """

  def __init__(self, fleet, *, cap: int | None = None, keep_finished: int = 0,
               max_workers: int = 32, fail_fast: bool = False,
               register_images: bool = True):
    inner = getattr(fleet, "_fleet", None)      # AsyncSandboxFleet wraps a sync fleet
    self.fleet = inner if inner is not None and hasattr(inner, "warm_image") else fleet
    self.cap = cap if cap is not None else int(self.fleet.config.max_warmpool_size)
    self.keep_finished = max(0, int(keep_finished))
    self.max_workers = max(1, int(max_workers))
    self.fail_fast = fail_fast
    self.register_images = register_images
    # _lock guards the batch tables and _active (short holds, taken by the
    # worker threads too); _reconcile_lock serializes whole reconciles and is
    # never taken by a worker, so a reconcile may fan out under it safely.
    self._lock = threading.RLock()
    self._reconcile_lock = threading.Lock()
    self._live: "collections.OrderedDict[object, dict[str, int]]" = collections.OrderedDict()
    self._finished: "collections.deque[dict[str, int]]" = collections.deque(
        maxlen=self.keep_finished or None)
    self._active: dict[str, int] = {}          # image -> replicas this warmer asked for
    self._executor: ThreadPoolExecutor | None = None

  # --- state -------------------------------------------------------------- #
  @property
  def active(self) -> dict[str, int]:
    """Replicas per image currently requested from the fleet."""
    with self._lock:
      return dict(self._active)

  @property
  def live_batches(self) -> list:
    with self._lock:
      return list(self._live)

  def desired(self) -> dict[str, int]:
    """Replicas per image for the live batches plus the kept finished ones."""
    with self._lock:
      totals: dict[str, int] = collections.OrderedDict()
      for counts in list(self._finished) + list(self._live.values()):
        for image, n in counts.items():
          if n > 0:
            totals[image] = totals.get(image, 0) + int(n)
      return {image: min(self.cap, n) for image, n in totals.items()}

  # --- batch lifecycle ---------------------------------------------------- #
  def add(self, batch_id, counts: Mapping[str, int], *, wait: bool = False,
          wait_timeout: float | None = None) -> WarmReport:
    """Register batch ``batch_id`` (``image -> claims``) and reconcile pools.

    With ``wait`` the call then blocks, best-effort, until this batch's pools are
    ready (or ``wait_timeout`` seconds, default the fleet's ``ready_timeout``)
    and records the outcome per image in ``report.ready``. Re-adding a known
    batch id replaces its counts.
    """
    clean = self._register_batch(batch_id, counts)
    return self._apply(clean, wait, wait_timeout)

  def add_async(self, batch_id, counts: Mapping[str, int], *, wait: bool = False,
                wait_timeout: float | None = None) -> "Future[WarmReport]":
    """`add` on a background thread; the future carries the `WarmReport` (or
    the `FleetError` a fail-fast add would raise). One batch at a time. The
    batch is registered now, so a `finish` that lands before the worker runs
    already counts it and does not unwarm an image the two batches share."""
    clean = self._register_batch(batch_id, counts)
    with self._lock:
      if self._executor is None:
        self._executor = ThreadPoolExecutor(max_workers=1,
                                            thread_name_prefix="lookahead-warm")
      ex = self._executor
    return ex.submit(self._apply, clean, wait, wait_timeout)

  def _register_batch(self, batch_id, counts: Mapping[str, int]) -> dict[str, int]:
    clean = {str(k): int(v) for k, v in counts.items() if int(v) > 0}
    with self._lock:
      self._live[batch_id] = clean
    return clean

  def _apply(self, clean: dict[str, int], wait: bool, wait_timeout) -> WarmReport:
    report = self._reconcile()
    if wait:
      # This batch's own share, not the sum over live batches: an earlier batch
      # is draining the same pool while this one warms, so readyReplicas never
      # reaches the sum under a steady claim stream.
      report.ready = self.wait_ready(
          clean.keys(), timeout=wait_timeout,
          replicas={img: min(self.cap, n) for img, n in clean.items()})
    return report

  def finish(self, batch_id) -> WarmReport:
    """Batch ``batch_id`` has no claims left to make: its pools stay for the
    next ``keep_finished`` finishes, then go. Unknown ids are a no-op."""
    with self._lock:
      counts = self._live.pop(batch_id, None)
      if counts is None:
        return WarmReport()
      if self.keep_finished:
        self._finished.append(counts)
    return self._reconcile()

  def wait_ready(self, images: Iterable[str] | None = None,
                 timeout: float | None = None, *,
                 replicas: Mapping[str, int] | None = None) -> dict[str, bool]:
    """Best-effort readiness barrier on ``images`` (default: every active image).
    ``replicas`` caps the ready count waited for per image (default: what the
    fleet asked of the pool)."""
    with self._lock:
      targets = list(images) if images is not None else list(self._active)
    if not targets:
      return {}
    return self.fleet.wait_pools_ready(targets, timeout=timeout, replicas=replicas)

  def close(self) -> WarmReport:
    """Forget every batch and unwarm everything this warmer asked for. A queued
    `add_async` is cancelled, a running one drained, before the tables are
    cleared, so nothing is warmed again behind the final reconcile."""
    with self._lock:
      ex, self._executor = self._executor, None
    if ex is not None:
      ex.shutdown(wait=True, cancel_futures=True)
    with self._lock:
      self._live.clear()
      self._finished.clear()
    return self._reconcile()

  def __enter__(self):
    return self

  def __exit__(self, *exc):
    self.close()

  # --- the diff ----------------------------------------------------------- #
  def _reconcile(self) -> WarmReport:
    with self._reconcile_lock:
      return self._reconcile_locked()

  def _reconcile_locked(self) -> WarmReport:
    # Serialized by _reconcile_lock; state reads/writes take _lock briefly so
    # the worker threads below never wait on a lock this thread holds.
    with self._lock:
      desired = self.desired()
      active = dict(self._active)
      n_live = len(self._live)
    to_create = [(img, n) for img, n in desired.items() if img not in active]
    to_scale = [(img, n) for img, n in desired.items()
                if img in active and active[img] != n]
    to_remove = [img for img in active if img not in desired]
    report = WarmReport()
    if not (to_create or to_scale or to_remove):
      return report
    if self.register_images:
      self._register([img for img, _ in to_create])
    if to_create and getattr(self.fleet, "plan_", None) is None:
      # One plan, built here, before the fan-out: a worker's warm_image would
      # otherwise build its own, and a stateful placement (round-robin) could
      # then put the pool on one cluster and a later acquire on another. A
      # failure here is not fatal to the warmer: each create below reports its
      # own error per image (the WarmReport contract), and fail_fast decides.
      plan = getattr(self.fleet, "plan", None)
      if callable(plan):
        try:
          plan()
        except Exception as exc:  # noqa: BLE001 — each warm below reports per image
          logger.warning("lookahead: could not plan the fleet (%s); warming per image", exc)
    fatal: list[BaseException] = []

    def _create(img, n):
      try:
        self.fleet.warm_image(img, replicas_override=n, wait=False)
        with self._lock:
          self._active[img] = n
        report.created.append(img)
      except Exception as exc:  # noqa: BLE001 — recorded; see fail_fast
        report.errors[img] = f"{type(exc).__name__}: {exc}"
        logger.warning("lookahead: could not warm %s (x%d): %s", img, n, exc)
        if self.fail_fast and isinstance(exc, FleetError):
          fatal.append(exc)

    def _scale(img, n):
      try:
        self.fleet.set_pool_replicas(img, n)
        with self._lock:
          self._active[img] = n
        report.scaled[img] = n
      except Exception as exc:  # noqa: BLE001
        report.errors[img] = f"{type(exc).__name__}: {exc}"
        logger.warning("lookahead: could not scale %s to %d: %s", img, n, exc)

    def _remove(img):
      try:
        self.fleet.unwarm_image(img)
      except Exception as exc:  # noqa: BLE001 — the fleet keeps its bookkeeping
        # on a failed delete so a retry can release the pool; keep ours too,
        # and the next reconcile tries again.
        report.errors[img] = f"{type(exc).__name__}: {exc}"
        logger.warning("lookahead: could not unwarm %s (kept for retry): %s", img, exc)
        return
      report.removed.append(img)
      with self._lock:
        self._active.pop(img, None)

    jobs = ([(_create, (img, n)) for img, n in to_create]
            + [(_scale, (img, n)) for img, n in to_scale]
            + [(_remove, (img,)) for img in to_remove])
    workers = min(self.max_workers, len(jobs))
    if workers <= 1:
      for fn, args in jobs:
        fn(*args)
    else:
      with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="lookahead") as ex:
        list(ex.map(lambda job: job[0](*job[1]), jobs))
    logger.info("lookahead: %s (desired %d image(s), live batches %d)",
                report.summary(), len(desired), n_live)
    if fatal:
      raise fatal[0]
    return report

  def _register(self, images: list[str]) -> None:
    plan = getattr(self.fleet, "plan_", None)
    unknown = [img for img in images
               if plan is None or plan.for_image(img) is None]
    if not unknown:
      return
    add = getattr(self.fleet, "add_tasks", None)
    if add is None:
      return
    try:
      add([Task(id=img, image=img) for img in unknown])
    except Exception as exc:  # noqa: BLE001 — the warm below reports per image
      logger.warning("lookahead: could not register %d image(s): %s", len(unknown), exc)
