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

"""Test doubles: `FakeFleet` and `FakeHandle`, the `SandboxFleet` surface with no
cluster behind it.

An RL framework that drives a fleet (warms pools ahead of its batches, acquires
a handle per episode, execs, releases, tears down) needs to test that loop
without Kubernetes. `FakeFleet` keeps the fleet's bookkeeping in memory, records
every call, and raises the same exceptions the real fleet would where that
matters (`FleetOvercommitError`, `KeyError` for an unplanned image, injected
failures), so the loop under test is exercised, not mocked away::

    from agent_sandbox_rl.testing import FakeFleet

    fleet = FakeFleet(FleetConfig(warm_per_task=True, max_warmpool_size=4),
                      exec_outputs={"pytest": "3 passed\\n"})
    fleet.load_tasks(["img:a", "img:a", "img:b"]); fleet.setup()
    h = fleet.acquire(fleet.tasks[0]); out = h.exec("pytest -q"); fleet.release(h)
    assert fleet.active_pools == {"img:a": 2, "img:b": 1}
    assert fleet.warm_calls and h.commands == ["pytest -q"]

Importing this module pulls nothing beyond the package itself.
"""

from __future__ import annotations

import collections
import contextlib
import itertools
import threading
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass, field
from typing import Optional

from . import constants
from .config import FleetConfig
from .exceptions import FleetError, FleetOvercommitError
from .fleet import FleetPlan, PlanEntry
from .handles import SandboxHandle
from .observability import Observer
from .sources import Task, to_tasks

FAKE_CLUSTER = "fake"


@dataclass
class FakeSession:
  """Stands in for `SandboxSession`: open until closed, runs through the handle."""
  handle: "FakeHandle"
  container: Optional[str] = None
  is_open: bool = True

  def run(self, command, timeout: float | None = None) -> str:
    return self.handle._run(command, self.container)

  def close(self) -> None:
    self.is_open = False


@dataclass
class FakeHandle(SandboxHandle):
  """A `SandboxHandle` whose exec is programmable and recorded.

  ``exec_outputs`` maps a substring of the command to its output (first match
  wins) or to a callable ``command -> output`` / ``command -> Exception`` to
  raise; unmatched commands return ``""``. ``commands`` lists every command
  run, ``released`` flips on release, ``suspended`` tracks suspend / resume.
  """
  exec_outputs: dict = field(default_factory=dict, repr=False)
  commands: list = field(default_factory=list, repr=False)
  released: bool = False
  suspended: bool = False
  snapshots: list = field(default_factory=list, repr=False)

  def _run(self, command, container=None) -> str:
    text = command if isinstance(command, str) else " ".join(map(str, command))
    self.commands.append(text)
    for key, out in self.exec_outputs.items():
      if key in text:
        if callable(out):
          out = out(text)
        if isinstance(out, BaseException):
          raise out
        return out
    return ""

  def exec(self, command, timeout: float | None = None, *, container: str | None = None) -> str:
    if self.released:
      raise FleetError(f"handle {self.sandbox_id} was released")
    return self._run(command, container or getattr(self, "target_container", None))

  def open_session(self, *, container: str | None = None) -> FakeSession:
    if self._session is None or not self._session.is_open:
      self._session = FakeSession(self, container or getattr(self, "target_container", None))
    return self._session

  def close_session(self) -> None:
    if self._session is not None:
      self._session.close()
      self._session = None

  def is_alive(self) -> bool:
    return not self.released and not self.suspended

  def release(self) -> None:
    self.close_session()
    self.released = True


class FakeFleet:
  """In-memory `SandboxFleet`: same methods, no API calls.

  Args:
    config: the `FleetConfig` the real fleet would get (sizing, cap, names).
    exec_outputs: default programmed outputs for every handle's ``exec``.
    fail_warm: ``image -> Exception`` raised by ``warm_image`` for that image.
    fail_acquire: ``image -> Exception`` raised by ``acquire`` for that image.
    slow_images: images whose pools never report ready in `wait_pools_ready`.
  """

  def __init__(self, config: FleetConfig | None = None, *, exec_outputs=None,
               fail_warm: Mapping[str, BaseException] | None = None,
               fail_acquire: Mapping[str, BaseException] | None = None,
               slow_images: Iterable[str] = ()):
    self.config = config or FleetConfig()
    self.exec_outputs = dict(exec_outputs or {})
    self.fail_warm = dict(fail_warm or {})
    self.fail_acquire = dict(fail_acquire or {})
    self.slow_images = set(slow_images)
    self.run_id = self.config.run_id or "fake-run"
    self.tasks: list[Task] = []
    self.plan_: FleetPlan | None = None
    self.report = None
    self._obs = Observer(self.config.observability)
    self._lock = threading.RLock()
    self._warmed: dict[str, int] = {}
    self._handles: list[FakeHandle] = []
    self._claims_reserved = 0
    self._seq = itertools.count(1)
    self._torndown = False
    # Call records, in order.
    self.warm_calls: list[tuple] = []       # (image, replicas, wait)
    self.scale_calls: list[tuple] = []      # (image, replicas)
    self.unwarm_calls: list[str] = []
    self.acquire_calls: list[tuple] = []    # (image, kwargs)
    self.release_calls: list[str] = []      # sandbox ids
    self.prepull_calls: list[dict] = []
    self.on_demand: set[str] = set()
    self.teardowns = 0

  # --- identity / inputs --------------------------------------------------- #
  def run_selector(self) -> str:
    return f"{constants.RUN_ID_LABEL}={self.run_id}"

  def _rewrite(self, tasks, image_rewrite):
    if image_rewrite is None:
      return list(tasks)
    out = []
    for t in tasks:
      new = image_rewrite(t.image)
      if new != t.image:
        t = t.model_copy(update={"image": new,
                                 "metadata": {**t.metadata, "original_image": t.image}})
      out.append(t)
    return out

  def load_tasks(self, source, *, image_rewrite=None) -> list[Task]:
    self.tasks = self._rewrite(to_tasks(source), image_rewrite)
    return self.tasks

  def add_tasks(self, source, *, image_rewrite=None) -> list[Task]:
    new = self._rewrite(to_tasks(source), image_rewrite)
    with self._lock:
      self.tasks = list(self.tasks) + new
      if self.plan_ is not None:
        for t in new:
          entry = self.plan_.for_image(t.image)
          if entry is None:
            self.plan_ = FleetPlan(self.plan_.entries + [self._entry(t.image, 1)])
          else:
            entry.tasks += 1
    return new

  def image_counts(self) -> "collections.OrderedDict[str, int]":
    counts: "collections.OrderedDict[str, int]" = collections.OrderedDict()
    for t in self.tasks:
      counts[t.image] = counts.get(t.image, 0) + 1
    return counts

  # --- plan / setup ---------------------------------------------------------- #
  def _entry(self, image: str, n: int) -> PlanEntry:
    cap = self.config.max_warmpool_size
    replicas = min(n, cap) if self.config.warm_per_task else max(1, min(cap, self.config.max_concurrent))
    return PlanEntry(cluster=FAKE_CLUSTER, image=image,
                     template=self.config.template_name(image),
                     pool=self.config.pool_name(image), replicas=replicas, tasks=n)

  def preflight(self) -> dict:
    return {}

  def plan(self) -> FleetPlan:
    with self._obs.phase("plan"):
      self.plan_ = FleetPlan([self._entry(img, n) for img, n in self.image_counts().items()])
    return self.plan_

  def start_warmpools(self, wait: bool = True, create_budget: int | None = None) -> None:
    plan = self.plan_ or self.plan()
    for e in plan.entries:
      self.warm_image(e.image, wait=wait)

  def prepull(self, wait: bool = True, **kw) -> bool:
    self.prepull_calls.append({"wait": wait, **kw})
    return True

  def prepull_delete(self) -> None:
    pass

  def setup(self, prepull: bool = False, create_budget: int | None = None) -> "FakeFleet":
    self._torndown = False
    self._claims_reserved = 0
    self.preflight()
    self.plan()
    if prepull:
      self.prepull(wait=True)
    self.start_warmpools(wait=True, create_budget=create_budget)
    return self

  def __enter__(self):
    return self.setup()

  def __exit__(self, *exc):
    self.teardown()

  # --- pools ----------------------------------------------------------------- #
  @property
  def active_pools(self) -> dict[str, int]:
    with self._lock:
      return dict(self._warmed)

  def warm_image(self, image: str, *, replicas_override: int | None = None,
                 wait: bool = True) -> None:
    entry = (self.plan_ or self.plan()).for_image(image)
    if entry is None:
      raise KeyError(f"image not in plan: {image}")
    reps = replicas_override if replicas_override is not None else entry.replicas
    self.warm_calls.append((image, reps, wait))
    if image in self.fail_warm:
      raise self.fail_warm[image]
    with self._lock:
      self._warmed[image] = max(self._warmed.get(image, 0), reps)

  def warm_images(self, images, *, replicas_override: int | None = None,
                  wait: bool = True) -> None:
    for img in dict.fromkeys(images):
      self.warm_image(img, replicas_override=replicas_override, wait=wait)

  def set_pool_replicas(self, image: str, replicas: int) -> None:
    self.scale_calls.append((image, replicas))
    with self._lock:
      if image in self._warmed:
        self._warmed[image] = max(0, replicas)

  def unwarm_image(self, image: str) -> None:
    with self._lock:
      if self._warmed.pop(image, None) is None:
        return
    self.unwarm_calls.append(image)

  def unwarm_images(self, images) -> None:
    for img in dict.fromkeys(images):
      self.unwarm_image(img)

  def wait_pools_ready(self, images, *, timeout: float | None = None) -> dict[str, bool]:
    plan = self.plan_ or self.plan()
    return {img: plan.for_image(img) is not None and img not in self.slow_images
            for img in dict.fromkeys(images)}

  # --- claims ---------------------------------------------------------------- #
  def acquire(self, task: Task, **kwargs) -> FakeHandle:
    self.acquire_calls.append((task.image, dict(kwargs)))
    with self._lock:
      hard = self.config.max_live_sandboxes
      if hard and self._claims_reserved >= hard:
        raise FleetOvercommitError(
            f"max_live_sandboxes={hard} reached: {self._claims_reserved} claims live")
      if self._torndown:
        raise FleetError("fleet was torn down; call setup() before acquiring again")
      if task.image in self.fail_acquire:
        raise self.fail_acquire[task.image]
      plan = self.plan_
      if plan is None or plan.for_image(task.image) is None:
        self.on_demand.add(task.image)
      n = next(self._seq)
      handle = FakeHandle(
          task=task, cluster_name=FAKE_CLUSTER, claim_name=f"claim-{n}",
          sandbox_id=f"sb-{n}", pod_name=f"pod-{n}", hostname=f"sb-{n}",
          pod_ip=f"10.0.0.{n % 250 + 1}", exec_outputs=dict(self.exec_outputs))
      self._claims_reserved += 1
      self._handles.append(handle)
    self._obs.claim(FAKE_CLUSTER, "ok")
    return handle

  def acquire_batch(self, tasks) -> list[FakeHandle]:
    return [self.acquire(t) for t in tasks]

  def release(self, handle: FakeHandle, **_) -> None:
    with self._lock:
      if handle not in self._handles:
        return
      self._handles.remove(handle)
      self._claims_reserved = max(0, self._claims_reserved - 1)
    handle.release()
    self.release_calls.append(handle.sandbox_id)

  def release_all(self) -> None:
    for h in list(self._handles):
      self.release(h)

  def handles(self) -> list[FakeHandle]:
    return list(self._handles)

  def hostnames(self) -> list[str]:
    return [h.hostname for h in self._handles]

  def endpoints(self, port: int = 8888) -> list[str]:
    return [f"{h.hostname}.{self.config.clusters[0].namespace if self.config.clusters else 'default'}:{port}"
            for h in self._handles]

  # --- snapshots (state only) ------------------------------------------------ #
  def snapshot(self, handle: FakeHandle, name: str | None = None, *, timeout: int = 180) -> str:
    uid = f"snap-{len(handle.snapshots) + 1}"
    handle.snapshots.append(uid)
    return uid

  def suspend(self, handle: FakeHandle, *, snapshot: bool = True, timeout: int = 180):
    handle.close_session()
    handle.suspended = True
    return self.snapshot(handle) if snapshot else None

  def resume(self, handle: FakeHandle, *, timeout: int = 180) -> bool:
    handle.suspended = False
    return bool(handle.snapshots)

  def restore(self, handle: FakeHandle, snapshot_uid: str, *, timeout: int = 180) -> None:
    if snapshot_uid not in handle.snapshots:
      raise FleetError(f"unknown snapshot {snapshot_uid}")
    handle.suspended = False

  # --- runs ------------------------------------------------------------------ #
  @contextlib.contextmanager
  def recording(self, label: str = "manual"):
    with self._obs.run(label) as report:
      self.report = report
      yield report

  def run(self, process_fn: Callable[[Task, FakeHandle], object], strategy: str = "naive",
          concurrency: int | None = None, **_) -> list:
    """Acquire → ``process_fn(task, handle)`` → release for every task, in task
    order, recording a `RunReport`. A per-task exception is captured as that
    task's result, as the real fleet does."""
    results = []
    with self.recording(strategy):
      for task in list(self.tasks):
        h = self.acquire(task)
        try:
          results.append(process_fn(task, h))
        except Exception as exc:  # noqa: BLE001 — mirrors fleet.run's capture
          results.append(exc)
        finally:
          self.release(h)
    return results

  def describe_environment(self) -> dict:
    return {FAKE_CLUSTER: {"fake": True, "namespace": "default"}}

  # --- teardown -------------------------------------------------------------- #
  def teardown(self, delete_namespace: bool = False, *, verify: bool = False):
    self.teardowns += 1
    self.release_all()
    with self._lock:
      for img in list(self._warmed):
        self.unwarm_calls.append(img)
      self._warmed.clear()
      self._torndown = True
    return {FAKE_CLUSTER: {"claims": 0, "warmpools": 0, "sandboxes": 0,
                           "templates": 0}} if verify else None
