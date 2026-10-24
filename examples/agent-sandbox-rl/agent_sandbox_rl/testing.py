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
every call, runs the real strategies and `RunReport`, and raises what the real
fleet raises where a loop can observe it (`FleetOvercommitError` at the cap,
`KeyError` for an unplanned image, a torn-down fleet on `acquire`), so the loop
under test is exercised, not mocked away::

    from agent_sandbox_rl.testing import FakeFleet

    fleet = FakeFleet(FleetConfig(warm_per_task=True, max_warmpool_size=4),
                      exec_outputs={"pytest": "3 passed\\n"})
    fleet.load_tasks(["img:a", "img:a", "img:b"]); fleet.setup()
    h = fleet.acquire(fleet.tasks[0]); out = h.exec("pytest -q"); fleet.release(h)
    assert fleet.active_pools == {"img:a": 2, "img:b": 1}
    assert fleet.warm_calls and h.commands == ["pytest -q"]

The fake offers the methods `SandboxFleet` has, with the same signatures, so a
test that passes here does not call something the real fleet lacks. Beyond
that surface it adds inspection only: the ``*_calls`` records, `active_pools`,
`FakeHandle.commands`, and `FakeSandboxGone` (exec on a released or suspended
handle, a guard the real handle does not have).

Importing this module pulls nothing beyond the package itself.
"""

from __future__ import annotations

import collections
import contextlib
import itertools
import threading
import types
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from typing import Optional

from . import constants, sizing
from .config import FleetConfig, run_namespace
from .exceptions import FleetError, FleetOvercommitError
from .fleet import FleetPlan, PlanEntry, SandboxFleet
from .handles import SandboxHandle
from .observability import Observer
from .sources import Task, to_tasks

FAKE_CLUSTER = "fake"


class FakeSandboxGone(RuntimeError):
  """Exec on a released or suspended `FakeHandle`.

  A test aid, not fleet behavior: the real handle has no such guard (the exec
  API returns an error, or output from a pod that is still terminating), so
  this sits outside the `FleetError` hierarchy on purpose."""


@dataclass
class FakeSession:
  """Stands in for `SandboxSession`: open until closed, runs through the handle."""
  handle: "FakeHandle"
  is_open: bool = True

  def run(self, command, timeout: float | None = None) -> str:
    return self.handle._run(command)

  def close(self) -> None:
    self.is_open = False


@dataclass
class FakeHandle(SandboxHandle):
  """A `SandboxHandle` whose exec is programmable and recorded.

  ``exec_outputs`` maps a substring of the command to its output (the longest
  matching key wins) or to a callable ``command -> output`` / an exception to
  raise; unmatched commands return ``""``. ``commands`` lists every command
  run, ``released`` / ``suspended`` track the lifecycle, ``snapshots`` the uids
  taken. The snapshot methods keep state only; ``refresh()`` renames the pod as
  a resume does.
  """
  exec_outputs: dict = field(default_factory=dict, repr=False)
  commands: list = field(default_factory=list, repr=False)
  released: bool = False
  suspended: bool = False
  snapshots: list = field(default_factory=list, repr=False)
  _generation: int = field(default=0, repr=False)

  def _run(self, command) -> str:
    text = command if isinstance(command, str) else " ".join(map(str, command))
    self.commands.append(text)
    for key in sorted(self.exec_outputs, key=len, reverse=True):
      if key in text:
        out = self.exec_outputs[key]
        if callable(out):
          out = out(text)
        if isinstance(out, BaseException):
          raise out
        return out
    return ""

  def exec(self, command, timeout: float | None = None) -> str:
    if self.released:
      raise FakeSandboxGone(f"handle {self.sandbox_id} was released")
    if self.suspended:
      raise FakeSandboxGone(f"sandbox {self.sandbox_id} is suspended (no pod)")
    if self._session is not None and self._session.is_open:
      return self._session.run(command, timeout=timeout)
    return self._run(command)

  def open_session(self) -> FakeSession:
    if self._session is None or not self._session.is_open:
      self._session = FakeSession(self)
    return self._session

  def close_session(self) -> None:
    if self._session is not None:
      self._session.close()
      self._session = None

  def refresh(self) -> None:
    """A resumed or restored sandbox is a new pod: new name, session dropped."""
    self.close_session()
    self._generation += 1
    self.pod_name = f"{self.sandbox_id}-pod{self._generation}"

  # --- snapshots (state only; the fleet wrappers below call these) --------- #
  def snapshot(self, name: str | None = None, *, timeout: int = 180) -> str:
    if self.released or self.suspended:                   # no pod to snapshot
      raise FakeSandboxGone(f"sandbox {self.sandbox_id} has no running pod")
    uid = f"{name or self.sandbox_id}-snap{len(self.snapshots) + 1}"
    self.snapshots.append(uid)
    return uid

  def list_snapshots(self) -> list:
    return list(self.snapshots)

  def snapshot_deleter(self) -> Callable[..., None]:
    def _delete(uid=None, *_, **__):
      if uid in self.snapshots:
        self.snapshots.remove(uid)
    return _delete

  def suspend(self, *, snapshot: bool = True, timeout: int = 180) -> Optional[str]:
    if self.suspended:
      raise FakeSandboxGone(f"sandbox {self.sandbox_id} is already suspended")
    self.close_session()
    uid = self.snapshot() if snapshot else None
    self.suspended = True
    return uid

  def resume(self, *, timeout: int = 180) -> bool:
    self.suspended = False
    self.refresh()
    return bool(self.snapshots)

  def restore(self, snapshot_uid: str, *, timeout: int = 180) -> None:
    if snapshot_uid not in self.snapshots:
      raise FleetError(f"unknown snapshot {snapshot_uid} for {self.sandbox_id}")
    self.suspended = False
    self.refresh()

  @property
  def is_suspended(self) -> bool:
    return self.suspended

  def release(self) -> None:
    self.close_session()
    self.released = True
    self.pod_ip = None


class FakeFleet:
  """In-memory `SandboxFleet`: the same methods and signatures, no API calls.

  Args:
    config: the `FleetConfig` the real fleet would get. It is copied and
      resolved the way `SandboxFleet` does it (run id label, ``{run_id}``
      names, ``run_isolation``), so pool and template names match the real ones.
    exec_outputs: default programmed outputs for every handle's ``exec``.
    fail_warm: ``image -> Exception`` raised by ``warm_image`` for that image.
    fail_acquire: ``image -> Exception`` raised by ``acquire`` for that image.

  Plans size pools with the real `sizing.compute_replicas` on one cluster;
  ``run()`` executes the real strategies (naive / sliding / pipelined) over
  this fleet, so warming order, teardown and the `RunReport` are the fleet's.
  """

  # The real window arithmetic: it only reads config and image counts.
  recommended_window = SandboxFleet.recommended_window
  _disk_spec = SandboxFleet._disk_spec

  def __init__(self, config: FleetConfig | None = None, *, exec_outputs=None,
               fail_warm: Mapping[str, BaseException] | None = None,
               fail_acquire: Mapping[str, BaseException] | None = None):
    self.config = (config or FleetConfig()).model_copy(deep=True)
    self.exec_outputs = dict(exec_outputs or {})
    self.fail_warm = dict(fail_warm or {})
    self.fail_acquire = dict(fail_acquire or {})
    self.run_id = self.config.run_id or "fake-run"
    self._shared_run_id = self.config.run_id is not None
    self.config.labels = {**self.config.labels, constants.RUN_ID_LABEL: self.run_id}
    self.config.apply_run_isolation(self.run_id)
    if self.config.clusters:
      self.namespace = self.config.clusters[0].namespace
    elif self.config.run_isolation == "namespace":
      self.namespace = run_namespace("default", self.run_id)
    else:
      self.namespace = "default"
    # What handles see as their cluster: enough for endpoint() and cluster_name.
    self._cluster = types.SimpleNamespace(name=FAKE_CLUSTER, namespace=self.namespace)
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
    self.skipped_images: list[str] = []     # warm_images: not in the plan
    self.acquire_calls: list[tuple] = []    # (image, snapshot_uid, pod_annotations)
    self.release_calls: list[str] = []      # sandbox ids
    self.prepull_calls: list[bool] = []     # wait flags
    self.on_demand: set[str] = set()
    self.teardowns = 0

  # --- identity / inputs --------------------------------------------------- #
  def run_selector(self) -> str:
    return f"{constants.RUN_ID_LABEL}={self.run_id}"

  def pod_run_selector(self) -> str:
    return f"{constants.POD_RUN_ID_LABEL}={self.run_id}"

  def load_tasks(self, source, *, image_rewrite=None) -> list[Task]:
    tasks = to_tasks(source)
    if image_rewrite is not None:
      out = []
      for t in tasks:
        new = image_rewrite(t.image)
        if new != t.image:
          t = t.model_copy(update={"image": new,
                                   "metadata": {**t.metadata, "original_image": t.image}})
        out.append(t)
      tasks = out
    self.tasks = tasks
    return self.tasks

  def image_counts(self) -> "collections.OrderedDict[str, int]":
    counts: "collections.OrderedDict[str, int]" = collections.OrderedDict()
    for t in self.tasks:
      counts[t.image] = counts.get(t.image, 0) + 1
    return counts

  # --- plan / setup ---------------------------------------------------------- #
  def _entry(self, image: str, n: int, total: int) -> PlanEntry:
    replicas = sizing.compute_replicas(
        n, total, self.config.max_concurrent, self.config.max_warmpool_size,
        per_task=self.config.warm_per_task)
    return PlanEntry(cluster=FAKE_CLUSTER, image=image,
                     template=self.config.template_name(image),
                     pool=self.config.pool_name(image), replicas=replicas, tasks=n)

  def preflight(self) -> dict:
    return {}

  def plan(self) -> FleetPlan:
    with self._obs.phase("plan"):
      counts = self.image_counts()
      total = sum(counts.values())
      self.plan_ = FleetPlan([self._entry(img, n, total) for img, n in counts.items()])
    return self.plan_

  def ensure_templates(self) -> None:
    self.plan_ or self.plan()

  def start_warmpools(self, wait: bool = True, create_budget: int | None = None) -> None:
    plan = self.plan_ or self.plan()
    for e in plan.entries:
      self.warm_image(e.image, wait=wait)

  def prepull(self, wait: bool = True) -> None:
    self.prepull_calls.append(wait)

  def prepull_delete(self) -> None:
    pass

  def setup(self, prepull: bool = False, create_budget: int | None = None) -> "FakeFleet":
    self._torndown = False
    with self._lock:
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
    """Inspection only (no real-fleet counterpart): replicas per warmed image."""
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
    plan = self.plan_ or self.plan()
    for img in dict.fromkeys(images):
      if plan.for_image(img) is None:       # the real fleet skips these with a warning
        self.skipped_images.append(img)
        continue
      self.warm_image(img, replicas_override=replicas_override, wait=wait)

  def set_pool_replicas(self, image: str, replicas: int) -> None:
    if (self.plan_ or self.plan()).for_image(image) is None:
      return                                # unplanned: a silent no-op, as the real one
    self.scale_calls.append((image, replicas))
    with self._lock:
      self._warmed[image] = max(0, replicas)

  def unwarm_image(self, image: str) -> None:
    with self._lock:
      if self._warmed.pop(image, None) is None:
        return
    self.unwarm_calls.append(image)

  def unwarm_images(self, images) -> None:
    for img in dict.fromkeys(images):
      self.unwarm_image(img)

  # --- claims ---------------------------------------------------------------- #
  def acquire(self, task: Task, *, snapshot_uid: str | None = None,
              pod_annotations: dict | None = None) -> FakeHandle:
    self.acquire_calls.append((task.image, snapshot_uid, pod_annotations))
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
          sandbox_id=f"sb-{n}", pod_name=f"sb-{n}-pod0", hostname=f"sb-{n}",
          pod_ip=f"10.0.0.{n % 250 + 1}", exec_outputs=dict(self.exec_outputs),
          _cluster=self._cluster)
      self._claims_reserved += 1
      self._handles.append(handle)
    self._obs.claim(FAKE_CLUSTER, "ok")
    return handle

  def acquire_batch(self, tasks) -> list[FakeHandle]:
    return [self.acquire(t) for t in tasks]

  def release(self, handle: FakeHandle, *, delete_snapshots: bool = False) -> None:
    with self._lock:
      if handle not in self._handles:
        return
      self._handles.remove(handle)
      if not handle._slot_released:         # a suspended handle gave its slot back already
        self._claims_reserved = max(0, self._claims_reserved - 1)
    handle.release()
    if delete_snapshots:
      handle.snapshots.clear()
    self.release_calls.append(handle.sandbox_id)

  def release_all(self) -> None:
    for h in list(self._handles):
      self.release(h)

  def handles(self) -> list[FakeHandle]:
    return list(self._handles)

  def hostnames(self) -> list[str]:
    return [h.hostname for h in self._handles]

  def endpoints(self, port: int = 8888) -> list[str]:
    return [h.endpoint(port) for h in self._handles]

  # --- snapshots: the fleet's slot bookkeeping over the handle methods ------- #
  def _held(self, handle: FakeHandle) -> None:
    """A snapshot call on a handle this fleet no longer holds (released) fails
    on the real fleet because the claim is gone; here it must not touch the
    slot count either."""
    with self._lock:
      if handle not in self._handles:
        raise FakeSandboxGone(f"handle {handle.sandbox_id} is not held by this fleet")

  def _free_slot_for_suspend(self, handle: FakeHandle) -> None:
    with self._lock:
      if handle not in self._handles or handle._slot_released:
        return
      handle._slot_released = True
      self._claims_reserved = max(0, self._claims_reserved - 1)

  def _retake_slot_for_wake(self, handle: FakeHandle) -> bool:
    with self._lock:
      if handle not in self._handles or not handle._slot_released:
        return False
      hard = self.config.max_live_sandboxes
      if hard and self._claims_reserved >= hard:
        raise FleetOvercommitError(
            f"max_live_sandboxes={hard} reached: {self._claims_reserved} claims live, "
            f"so {handle.sandbox_id} stays suspended")
      self._claims_reserved += 1
      handle._slot_released = False
    return True

  def snapshot(self, handle: FakeHandle, name: str | None = None, *, timeout: int = 180) -> str:
    self._held(handle)
    with self._obs.phase("snapshot", cluster=FAKE_CLUSTER):
      return handle.snapshot(name, timeout=timeout)

  def suspend(self, handle: FakeHandle, *, snapshot: bool = True,
              timeout: int = 180) -> Optional[str]:
    self._held(handle)
    with self._obs.phase("suspend", cluster=FAKE_CLUSTER):
      uid = handle.suspend(snapshot=snapshot, timeout=timeout)
    self._free_slot_for_suspend(handle)
    return uid

  def resume(self, handle: FakeHandle, *, timeout: int = 180) -> bool:
    self._held(handle)
    retaken = self._retake_slot_for_wake(handle)
    try:
      with self._obs.phase("resume", cluster=FAKE_CLUSTER):
        restored = handle.resume(timeout=timeout)
    except Exception:
      if retaken:
        self._free_slot_for_suspend(handle)
      raise
    self._obs.restored(FAKE_CLUSTER, restored)
    return restored

  def restore(self, handle: FakeHandle, snapshot_uid: str, *, timeout: int = 180) -> None:
    self._held(handle)
    retaken = self._retake_slot_for_wake(handle)
    try:
      with self._obs.phase("restore", cluster=FAKE_CLUSTER):
        handle.restore(snapshot_uid, timeout=timeout)
    except Exception:
      if retaken:
        self._free_slot_for_suspend(handle)
      raise
    self._obs.restored(FAKE_CLUSTER, True)

  # --- runs ------------------------------------------------------------------ #
  @contextlib.contextmanager
  def recording(self, label: str = "manual"):
    with self._obs.run(label) as report:
      self.report = report
      yield report

  def run(self, process_fn: Callable[[Task, FakeHandle], object],
          strategy: str = "naive", concurrency: int | None = None,
          *, epochs: int = 1, keep_warm: bool = False, recycle: bool = False,
          **recycle_options) -> list:
    """`SandboxFleet.run` over this fleet: the real strategy (naive / sliding /
    pipelined) warms, acquires, processes, releases and tears down, and the
    `RunReport` records it. ``recycle=True`` is not supported by the fake."""
    from .strategies import STRATEGIES, process_parallel
    if strategy not in STRATEGIES:
      raise ValueError(f"unknown strategy '{strategy}'; choose from {sorted(STRATEGIES)}")
    if epochs < 1:
      raise ValueError("epochs must be >= 1")
    if recycle:
      raise NotImplementedError("FakeFleet.run does not model recycle=True "
                                "(git-restore reuse needs a real sandbox)")
    conc = concurrency or self.config.max_concurrent
    fn = STRATEGIES[strategy]
    if self.plan_ is None:
      self.plan()
    with self._obs.run(strategy) as report:
      self.report = report
      report.environment = self.describe_environment()
      if epochs == 1:
        return fn(self, process_fn, conc, teardown=not keep_warm, executor=process_parallel)
      results = []
      for e in range(epochs):
        last = e == epochs - 1
        try:
          results.append(fn(self, process_fn, conc, teardown=last and not keep_warm,
                            executor=process_parallel))
        except BaseException:
          if not keep_warm and not last:
            self.teardown()
          raise
      return results

  def describe_environment(self) -> dict:
    return {FAKE_CLUSTER: {"fake": True, "namespace": self.namespace}}

  # --- teardown -------------------------------------------------------------- #
  def teardown(self, delete_namespace: bool = False) -> None:
    self.teardowns += 1
    self.release_all()
    with self._lock:
      for img in list(self._warmed):
        self.unwarm_calls.append(img)
      self._warmed.clear()
      self._torndown = True
      self.plan_ = None                       # as the real _teardown does
