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

"""`SandboxFleet` — the synchronous orchestrator.

Drives the full lifecycle across one or many clusters: load tasks → preflight →
plan (compute replicas) → ensure templates → start warm pools → acquire (claim a
sandbox per task, returning a `SandboxHandle` with a hostname/endpoint) →
release → teardown. Use the primitives directly from an RL loop, or the managed
`run()`. (Strategies + parallelism land in phase 4; async in phase 6.)
"""

from __future__ import annotations

import collections
import logging
import math
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from typing import Callable, Optional

from kubernetes import client

from . import sizing
from .cluster import Cluster, ClusterRegistry
from .config import ClusterConfig, FleetConfig
from .exceptions import FleetError, PreflightError
from .handles import SandboxHandle
from .observability import Observer, repo_family
from .placement import get_placement
from .sources import Task, to_tasks

logger = logging.getLogger("agent_sandbox_rl.fleet")


def _split_budget(total: int, weights: "dict[str, float]") -> "dict[str, int]":
  """Split an integer ``total`` across keys by ``weights`` using largest-remainder
  (Hamilton) allocation, so the result sums to exactly ``total`` (no rounding
  overshoot)."""
  if not weights:
    return {}
  if len(weights) == 1:                         # common case — no allocation math
    return {next(iter(weights)): total}
  tw = sum(weights.values())                    # ClusterConfig.weight is > 0, so tw > 0
  ideal = {k: total * (w / tw) for k, w in weights.items()}
  alloc = {k: int(math.floor(v)) for k, v in ideal.items()}
  remainder = total - sum(alloc.values())
  # hand out the leftover units to the largest fractional parts
  for k in sorted(weights, key=lambda k: ideal[k] - alloc[k], reverse=True)[:remainder]:
    alloc[k] += 1
  return alloc


@dataclass
class PlanEntry:
  """One image's provisioning plan on a chosen cluster."""

  cluster: str
  image: str
  template: str
  pool: str
  replicas: int
  tasks: int


class FleetPlan:
  """The result of `SandboxFleet.plan()`: per-image placement + sizing."""

  def __init__(self, entries: list[PlanEntry]):
    self.entries = entries
    self._by_image = {e.image: e for e in entries}

  def for_image(self, image: str) -> Optional[PlanEntry]:
    return self._by_image.get(image)

  @property
  def total_replicas(self) -> int:
    return sum(e.replicas for e in self.entries)

  def by_cluster(self) -> "dict[str, list[PlanEntry]]":
    out: dict[str, list[PlanEntry]] = collections.defaultdict(list)
    for e in self.entries:
      out[e.cluster].append(e)
    return dict(out)


class SandboxFleet:
  """Synchronous multi-cluster warm-pool orchestrator."""

  def __init__(self, config: FleetConfig | None = None,
               registry: ClusterRegistry | None = None):
    self.config = config or FleetConfig()
    # Honor an explicitly-passed registry even when empty — `ClusterRegistry`
    # defines `__len__`, so `registry or …` would treat `ClusterRegistry([])` as
    # falsy and build a default ambient Cluster (which loads kube-config). Only
    # fall back when no registry was given at all.
    self.registry = (registry if registry is not None
                     else self._default_registry(self.config))
    self.placement = get_placement(self.config.placement)
    self.tasks: list[Task] = []
    self.plan_: FleetPlan | None = None
    self._handles: list[SandboxHandle] = []
    self._warmed: dict[str, int] = {}        # image -> replicas currently warmed
    self._ondemand: set[tuple[str, str]] = set()   # (cluster, image) pools made via acquire()
    self._lock = threading.Lock()            # guards bookkeeping under parallel run
    self._obs = Observer(self.config.observability)
    self.report = None                       # set by run()/the Observer
    if self.config.observability.enable_tracing:
      self._enable_sdk_tracing()

  def _enable_sdk_tracing(self) -> None:
    """Point each cluster's SDK SandboxClient at our tracer/provider so the SDK's
    create_claim/wait_ready spans nest under our fleet spans."""
    try:
      from k8s_agent_sandbox.models import SandboxTracerConfig
      tc = SandboxTracerConfig(
          enable_tracing=True,
          trace_service_name=self.config.observability.trace_service_name)
      for c in self.registry:
        c.tracer_config = tc
    except Exception:  # noqa: BLE001  (SDK without tracing support)
      pass

  @property
  def observer(self):
    return self._obs

  @staticmethod
  def _default_registry(config: FleetConfig) -> ClusterRegistry:
    if config.clusters:
      return ClusterRegistry.from_configs(config.clusters, labels=config.labels)
    # Single cluster from the ambient kube context.
    return ClusterRegistry([Cluster(ClusterConfig(), labels=config.labels)])

  # --- inputs ------------------------------------------------------------ #
  def load_tasks(self, source, *, image_rewrite=None) -> list[Task]:
    """Load tasks from ``source``. ``image_rewrite`` is an optional
    ``image -> image`` hook (e.g. ``registry_rewrite.make_rewriter(...)``) applied
    to each task's image; the original is stashed in ``metadata['original_image']``."""
    tasks = to_tasks(source)
    if image_rewrite is not None:
      # Copy rather than mutate: to_tasks may hand back the caller's own Task
      # objects (e.g. a list[Task] / caching TaskSource), so rewriting in place
      # would alias and corrupt their images/metadata.
      rewritten = []
      for t in tasks:
        new = image_rewrite(t.image)
        if new != t.image:
          t = t.model_copy(update={
              "image": new,
              "metadata": {**t.metadata, "original_image": t.image}})
        rewritten.append(t)
      tasks = rewritten
    self.tasks = tasks
    logger.info("Loaded %d tasks (%d unique images)",
                len(self.tasks), len({t.image for t in self.tasks}))
    return self.tasks

  def image_counts(self) -> "collections.OrderedDict[str, int]":
    counts: "collections.OrderedDict[str, int]" = collections.OrderedDict()
    for t in self.tasks:
      counts[t.image] = counts.get(t.image, 0) + 1
    return counts

  def _disk_spec(self) -> "tuple[float | None, float | None]":
    """``(avg_image_gb, usable_disk_gb)`` for disk-aware window sizing. ``usable`` is
    ``None`` (disk cap disabled) unless **both** ``avg_image_gb`` and
    ``node_ephemeral_gb`` are set; ``avg`` is returned as-configured for reference.
    ``usable`` is per-node ephemeral storage minus headroom (conservative: a window's
    images may co-locate on one node)."""
    avg = self.config.avg_image_gb
    node_gb = self.config.node_ephemeral_gb
    if avg is None or node_gb is None:
      return (avg, None)                      # usable=None -> recommend_window_* skips the disk cap
    return (avg, node_gb * (1.0 - self.config.disk_headroom))

  def recommended_window(self, *, pipelined: bool = False) -> int:
    """Window size for sliding/pipelined: explicit ``window_size`` wins; otherwise
    the concurrency-aware window, capped by node disk when disk hints are set."""
    if self.config.window_size is not None:
      return max(1, self.config.window_size)
    counts = self.image_counts()
    avg, usable = self._disk_spec()
    per_task = self.config.warm_per_task
    nodes = self.config.cluster_nodes or 1   # spread distinct images across the pool
    if pipelined and per_task:
      logger.warning(
          "pipelined + warm_per_task: deep per-image replicas shrink the prefetch "
          "window and can serialize images (underfilling max_concurrent). Prefer "
          "strategy='naive' or 'sliding' with warm_per_task for RL rollouts.")
    if pipelined:
      return sizing.recommend_window_pipelined(
          counts, self.config.max_concurrent, self.config.max_warmpool_size,
          avg_image_gb=avg, usable_disk_gb=usable, per_task=per_task, nodes=nodes)
    win = sizing.recommend_window(
        counts, self.config.max_concurrent, self.config.max_warmpool_size,
        per_task=per_task)
    if avg is not None and usable is not None:
      win = min(win, sizing.recommend_window_disk(
          counts, self.config.max_concurrent, self.config.max_warmpool_size,
          avg_image_gb=avg, usable_disk_gb=usable, pipeline_factor=1.0,
          per_task=per_task, nodes=nodes))
    return max(1, win)

  # --- preflight / plan -------------------------------------------------- #
  def preflight(self) -> dict:
    """Run full per-cluster preflight (reachability, CRD versions, controller,
    runtime class, pull secret, namespace). Raises `PreflightError` on any hard
    failure; returns ``{cluster_name: PreflightReport}``."""
    with self._obs.phase("preflight"):
      return self._preflight()

  def _preflight(self) -> dict:
    from . import preflight as _pf
    reports = {}
    failed = {}
    sample_image = next(iter(self.image_counts()), "busybox:latest")
    for c in self.registry:
      ts = c.template_spec(self.config.template)
      rep = _pf.preflight_cluster(
          c, require_runtime_class=ts.runtime_class,
          image_pull_secret=ts.image_pull_secret, namespace=c.namespace,
          validate_template=ts, sample_image=sample_image)
      reports[c.name] = rep
      for w in rep.warnings:
        logger.warning("[%s] %s: %s", c.name, w.name, w.detail)
      if not rep.ok:
        failed[c.name] = rep
    if failed:
      detail = "; ".join(
          f"{n}: " + ", ".join(f"{ch.name}({ch.detail})" for ch in r.failures)
          for n, r in failed.items())
      raise PreflightError(f"preflight failed — {detail}")
    logger.info("Preflight OK on %d cluster(s): %s",
                len(reports), ", ".join(reports))
    return reports

  def plan(self) -> FleetPlan:
    """Assign each unique image to a cluster (placement) and size its pool."""
    with self._obs.phase("plan"):
      return self._plan()

  def _plan(self) -> FleetPlan:
    counts = self.image_counts()
    # image -> cluster (each unique image placed once).
    assigned: "collections.OrderedDict[str, Cluster]" = collections.OrderedDict()
    for image in counts:
      assigned[image] = self.placement.select(image, self.registry)
    # per-cluster totals for proportional sizing.
    cluster_totals: dict[str, int] = collections.defaultdict(int)
    for image, c in assigned.items():
      cluster_totals[c.name] += counts[image]

    # Split the global concurrency budget across the clusters in use, by weight,
    # so the total warm footprint stays ~max_concurrent rather than
    # max_concurrent x n_clusters. Use largest-remainder allocation so the
    # per-cluster budgets sum to *exactly* max_concurrent (no round()-induced
    # overshoot). compute_replicas still floors each pool at 1, so a 0 budget
    # never starves an image. (Single cluster → full budget, unchanged.)
    used = [self.registry.get(n) for n in cluster_totals]
    cluster_budget = _split_budget(self.config.max_concurrent,
                                   {c.name: c.config.weight for c in used})

    per_task = self.config.warm_per_task
    entries: list[PlanEntry] = []
    for image, c in assigned.items():
      replicas = sizing.compute_replicas(
          counts[image], cluster_totals[c.name],
          cluster_budget[c.name], self.config.max_warmpool_size,
          per_task=per_task)
      if per_task and replicas < counts[image]:   # clamped by max_warmpool_size
        logger.warning(
            "warm_per_task: image %s has %d tasks but max_warmpool_size=%d; "
            "warming only %d replicas (raise max_warmpool_size for one per task)",
            image, counts[image], self.config.max_warmpool_size, replicas)
      template = self.config.template_name(image)
      entries.append(PlanEntry(
          cluster=c.name, image=image, template=template,
          pool=f"pool-{template}", replicas=replicas, tasks=counts[image]))
    self.plan_ = FleetPlan(entries)
    logger.info("Plan: %d images across %d cluster(s), %d total warm replicas",
                len(entries), len(self.plan_.by_cluster()),
                self.plan_.total_replicas)
    return self.plan_

  # --- provisioning ------------------------------------------------------ #
  def _ensure_pool(self, cluster: Cluster, image: str, replicas: int) -> str:
    template = self.config.template_name(image)
    pool = f"pool-{template}"
    cluster.resources.ensure_template(
        image, template, cluster.template_spec(self.config.template))
    cluster.resources.create_warmpool(pool, template, replicas)
    return pool

  def ensure_templates(self) -> None:
    plan = self.plan_ or self.plan()
    for e in plan.entries:
      c = self.registry.get(e.cluster)
      c.resources.ensure_template(
          e.image, e.template, c.template_spec(self.config.template))

  def _warm_entry(self, e, wait: bool, replicas_override: int | None = None) -> None:
    """Warm one plan entry's pool (create template+pool, reserve, optionally wait
    for readiness). The single warm path shared by ``warm_image`` and
    ``start_warmpools``. Safe to run concurrently **across distinct images** (how
    start_warmpools/the windowed strategies call it): shared counters/observer use
    atomic helpers and ``_warmed`` writes hold the lock. It is NOT safe to warm the
    *same* image from two threads at once (the reuse check + record aren't atomic
    across the released lock); the callers never do that — one entry per image."""
    reps = replicas_override if replicas_override is not None else e.replicas
    c = self.registry.get(e.cluster)
    fam = repo_family(e.image)

    def _await_ready():
      with self._obs.phase("wait_pool_ready", cluster=e.cluster, family=fam):
        if not c.resources.wait_for_pool_ready(
            e.pool, reps, timeout=self.config.ready_timeout):
          raise FleetError(
              f"warm pool '{e.pool}' on cluster '{e.cluster}' did not become "
              f"ready within {self.config.ready_timeout}s")

    with self._lock:
      already = self._warmed.get(e.image, 0)
    if already >= reps:                       # already warm (cross-epoch / keep_warm reuse)
      if wait:                                # still honor the readiness contract on reuse
        _await_ready()
      return
    with self._obs.phase("create_warmpool", cluster=e.cluster, family=fam):
      c.resources.ensure_template(
          e.image, e.template, c.template_spec(self.config.template))
      c.resources.create_warmpool(e.pool, e.template, reps, reconcile=True)
    # Reserve only the delta when scaling an already-warm pool (create_warmpool
    # upserts replicas on 409 under reconcile), so reuse never double-counts.
    delta = reps - already
    c.reserve_replicas(delta)
    with self._lock:
      self._warmed[e.image] = reps
    self._obs.warm_add(e.cluster, delta)
    if wait:
      _await_ready()

  def _warm_entries(self, entries, wait: bool,
                    replicas_override: int | None = None) -> None:
    """Warm a set of plan entries **concurrently** (bounded by ``max_concurrent``).
    Each entry's ``wait_for_pool_ready`` blocks on the image pull, so serializing
    them is O(#images) slow; this fans out across a thread pool and raises the first
    error (teardown cleans up partial state). Shared by ``start_warmpools`` (all
    pools) and ``warm_images`` (a window) — both warm one entry per distinct image,
    which ``_warm_entry`` is safe for."""
    if not entries:
      return
    workers = max(1, min(len(entries), self.config.max_concurrent))
    if workers == 1 or len(entries) == 1:
      for e in entries:
        self._warm_entry(e, wait, replicas_override=replicas_override)
      return
    with ThreadPoolExecutor(max_workers=workers) as ex:
      futures = [ex.submit(self._warm_entry, e, wait, replicas_override)
                 for e in entries]
      err = None
      for f in as_completed(futures):
        try:
          f.result()
        except BaseException as exc:  # noqa: BLE001 — surface first; teardown cleans up
          err = err or exc
      if err is not None:
        raise err

  def start_warmpools(self, wait: bool = True) -> None:
    """Warm every planned pool, concurrently (bounded by ``max_concurrent``)."""
    self._warm_entries((self.plan_ or self.plan()).entries, wait)

  def warm_images(self, images, *, replicas_override: int | None = None,
                  wait: bool = True) -> None:
    """Warm a subset of images' pools **concurrently** (used by sliding/pipelined to
    warm a whole window in parallel instead of one image at a time)."""
    plan = self.plan_ or self.plan()
    # Dedupe while preserving order: warming the same image from two threads at
    # once is unsafe (see ``_warm_entry``), and this is a public helper.
    images = list(dict.fromkeys(images))
    resolved = [(img, plan.for_image(img)) for img in images]
    entries = [e for _img, e in resolved if e is not None]
    missing = [img for img, e in resolved if e is None]
    if missing:                              # callers pass planned images; None = a bug
      logger.warning("warm_images: %d image(s) not in the plan, skipped: %s",
                     len(missing), missing[:5])
    self._warm_entries(entries, wait, replicas_override=replicas_override)

  def warm_image(self, image: str, *, replicas_override: int | None = None,
                 wait: bool = True) -> None:
    """Warm one image's pool (used by sliding/none to bound the footprint)."""
    entry = (self.plan_ or self.plan()).for_image(image)
    if entry is None:
      raise KeyError(f"image not in plan: {image}")
    self._warm_entry(entry, wait, replicas_override=replicas_override)

  def unwarm_image(self, image: str) -> None:
    """Tear down one image's pool + template."""
    entry = (self.plan_ or self.plan()).for_image(image)
    if entry is None:
      return
    c = self.registry.get(entry.cluster)
    c.resources.delete_warmpool(entry.pool)
    c.resources.delete_template(entry.template)
    with self._lock:
      reps = self._warmed.pop(image, entry.replicas)
    c.release_replicas(reps)
    self._obs.warm_remove(entry.cluster, reps)

  def prepull(self, wait: bool = True) -> None:
    """Pre-pull each cluster's planned images via a DaemonSet (optional)."""
    from . import prepull as _pp
    plan = self.plan_ or self.plan()
    with self._obs.phase("prepull"):
      for cname, entries in plan.by_cluster().items():
        c = self.registry.get(cname)
        ts = c.template_spec(self.config.template)
        _pp.prepull(c, [e.image for e in entries],
                    node_selector=ts.node_selector,
                    image_pull_secret=ts.image_pull_secret,
                    labels=self.config.labels, wait=wait)

  def prepull_delete(self) -> None:
    from . import prepull as _pp
    for c in self.registry:
      _pp.prepull_delete(c)

  def setup(self, prepull: bool = False) -> "SandboxFleet":
    """preflight → plan → (optional pre-pull) → start (and wait for) warm pools."""
    self.preflight()
    self.plan()
    if prepull:
      self.prepull(wait=True)
    self.start_warmpools(wait=True)
    return self

  # --- claims ------------------------------------------------------------ #
  def acquire(self, task: Task) -> SandboxHandle:
    """Claim a sandbox for ``task`` and return a `SandboxHandle`.

    On any failure between claim creation and bookkeeping, the partially-created
    sandbox is terminated and the on-demand replica bump is rolled back, so a
    failed acquire leaks neither a remote sandbox nor capacity counters.
    """
    entry = self.plan_.for_image(task.image) if self.plan_ else None
    on_demand = entry is None
    if not on_demand:
      cluster = self.registry.get(entry.cluster)
      pool = entry.pool
    created_pool = False          # did THIS call create the on-demand pool?
    if on_demand:
      cluster = self.placement.select(task.image, self.registry)
      pool = self._ensure_pool(cluster, task.image, 1)
      # Reserve the size-1 pool's replica only the first time we create it for
      # this (cluster, image); repeated acquire()s reuse it (no unbounded growth).
      key = (cluster.name, task.image)
      with self._lock:
        created_pool = key not in self._ondemand
        if created_pool:
          self._ondemand.add(key)
      if created_pool:
        cluster.reserve_replicas(1)

    fam = repo_family(task)
    sandbox = None
    try:
      with self._obs.phase("claim", cluster=cluster.name, family=fam):
        sandbox = cluster.sandbox_client.create_sandbox(
            warmpool=pool, namespace=cluster.namespace,
            sandbox_ready_timeout=self.config.ready_timeout,
            labels=dict(self.config.labels))
        pod = sandbox.get_pod_name()
        try:
          pod_ip = sandbox.get_pod_ip()
        except Exception:  # noqa: BLE001
          pod_ip = None
    except Exception:  # noqa: BLE001 — roll back partial state, then re-raise
      if sandbox is not None:
        try:
          sandbox.terminate()
        except Exception:  # noqa: BLE001
          logger.warning("failed to terminate sandbox after acquire error",
                         exc_info=True)
      # If this call created the on-demand pool, undo it fully (delete pool +
      # template, release the replica, forget it) so a failed acquire leaves no
      # trace. A reused pool is left for the next acquire.
      if created_pool:
        try:
          cluster.resources.delete_warmpool(pool)
          cluster.resources.delete_template(self.config.template_name(task.image))
        except Exception:  # noqa: BLE001
          logger.warning("failed to remove on-demand pool after acquire error",
                         exc_info=True)
        cluster.release_replicas(1)
        with self._lock:
          self._ondemand.discard(key)
      self._obs.claim(cluster.name, "error")
      raise

    handle = SandboxHandle(
        task=task, cluster_name=cluster.name, claim_name=sandbox.claim_name,
        sandbox_id=sandbox.sandbox_id, pod_name=pod, hostname=sandbox.sandbox_id,
        pod_ip=pod_ip, sandbox=sandbox, _cluster=cluster)
    cluster.reserve_claim()
    with self._lock:
      self._handles.append(handle)
    self._obs.claim(cluster.name, "ok")
    return handle

  def acquire_batch(self, tasks: list[Task]) -> list[SandboxHandle]:
    return [self.acquire(t) for t in tasks]

  def handles(self) -> list[SandboxHandle]:
    return list(self._handles)

  def hostnames(self) -> list[str]:
    return [h.hostname for h in self._handles]

  def endpoints(self, port: int = 8888) -> list[str]:
    return [h.endpoint(port) for h in self._handles]

  def release(self, handle: SandboxHandle) -> None:
    # Claim the handle under the lock first, so a concurrent double-release of the
    # same handle issues the remote delete (and counter decrement) exactly once.
    with self._lock:
      if handle not in self._handles:
        return
      self._handles.remove(handle)
      c = self.registry.get(handle.cluster_name)
    c.release_claim()
    with self._obs.phase("release", cluster=handle.cluster_name):
      handle.release()

  def release_all(self) -> None:
    for h in list(self._handles):
      self.release(h)

  # --- teardown ---------------------------------------------------------- #
  def teardown(self, delete_namespace: bool = False) -> None:
    """Release all claims and delete every resource this fleet created."""
    with self._obs.phase("teardown"):
      self._teardown(delete_namespace)

  def _teardown(self, delete_namespace: bool) -> None:
    self.release_all()
    for c in self.registry:
      sel = c.resources.managed_selector()
      # Sweep any stray claims first (defensive: untracked/leaked claims keep
      # their adopted sandbox alive even after the pool is gone).
      for claim in c.resources.list_claims(label_selector=sel):
        c.resources.delete_claim(claim)
      for pool in c.resources.list_warmpools(label_selector=sel):
        c.resources.delete_warmpool(pool)
      for tmpl in c.resources.list_templates(label_selector=sel):
        c.resources.delete_template(tmpl)
      c.reset_counts()
      if delete_namespace:
        try:
          c.core_api.delete_namespace(c.namespace)
        except Exception:  # noqa: BLE001
          pass
    self._obs.warm_reset()
    with self._lock:
      self._warmed.clear()
      self._ondemand.clear()
    self.plan_ = None

  def __enter__(self) -> "SandboxFleet":
    return self.setup()

  def __exit__(self, *exc) -> None:
    self.teardown()

  # --- managed runner ---------------------------------------------------- #
  def run(self, process_fn: Callable[[Task, SandboxHandle], object],
          strategy: str = "naive", concurrency: int | None = None,
          *, epochs: int = 1, keep_warm: bool = False) -> list:
    """Run all loaded tasks under ``strategy`` (none|naive|sliding|pipelined) with
    up to ``concurrency`` parallel claim+exec (defaults to ``config.max_concurrent``).

    ``epochs>1`` runs that many passes over all tasks, keeping warm pools resident
    between epochs (so re-pulls hit the node layer cache) and tearing down once at
    the end; it returns ``list[list]`` (one task-ordered list per pass). ``epochs==1``
    returns the flat ``list`` (a per-task exception is captured, not raised).
    ``keep_warm=True`` skips the final teardown so a caller's own loop can reuse the
    warm pools; call ``fleet.teardown()`` when done.
    """
    from .strategies import STRATEGIES
    if strategy not in STRATEGIES:
      raise ValueError(f"unknown strategy '{strategy}'; choose from {sorted(STRATEGIES)}")
    if epochs < 1:
      raise ValueError("epochs must be >= 1")
    conc = concurrency or self.config.max_concurrent
    fn = STRATEGIES[strategy]
    with self._obs.run(strategy) as report:
      self.report = report
      try:
        report.environment = self.describe_environment()
      except Exception:  # noqa: BLE001 — environment is best-effort
        logger.debug("could not collect environment", exc_info=True)
      if epochs == 1:
        results = fn(self, process_fn, conc, teardown=not keep_warm)
      else:
        results = []
        for e in range(epochs):
          last = e == epochs - 1
          logger.info("epoch %d/%d", e + 1, epochs)
          try:
            results.append(fn(self, process_fn, conc,
                              teardown=last and not keep_warm))
          except BaseException:               # a mid-run epoch never tore down
            if not keep_warm and not last:
              self.teardown()
            raise
    logger.info("\n%s", report.summary())
    return results

  def describe_environment(self) -> dict:
    """Best-effort per-cluster details (context, namespace, k8s version, nodes,
    node pools, instance types, region) for the RunReport. Never raises."""
    def _lbl(node, key):
      return (node.metadata.labels or {}).get(key)

    env = {}
    for c in self.registry:
      info = {"context": c.config.context or "(ambient)", "namespace": c.namespace}
      try:
        info["k8s_version"] = client.VersionApi(c.api_client).get_code().git_version
      except Exception:  # noqa: BLE001
        pass
      try:
        nodes = c.core_api.list_node().items
        info["nodes"] = len(nodes)
        pools = sorted({_lbl(n, "cloud.google.com/gke-nodepool")
                        for n in nodes if _lbl(n, "cloud.google.com/gke-nodepool")})
        types = sorted({_lbl(n, "node.kubernetes.io/instance-type")
                        for n in nodes if _lbl(n, "node.kubernetes.io/instance-type")})
        regions = sorted({_lbl(n, "topology.kubernetes.io/region")
                          for n in nodes if _lbl(n, "topology.kubernetes.io/region")})
        if pools:
          info["node_pools"] = pools
        if types:
          info["instance_types"] = types
        if regions:
          info["region"] = regions[0] if len(regions) == 1 else regions
      except Exception:  # noqa: BLE001
        pass
      env[c.name] = info
    return env
