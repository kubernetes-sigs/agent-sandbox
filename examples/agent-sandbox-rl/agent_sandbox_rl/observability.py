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

"""Observability: per-phase performance tracking for the fleet.

Three layers (mirroring the k8s-agent-sandbox SDK so they interoperate):

1. **RunReport** — always-on, dependency-free per-phase timing + counts.
2. **Prometheus metrics** — `asrl_*` series on the default registry (optional
   ``metrics`` extra; opt-in via ``ObservabilityConfig.enable_metrics``, default
   True). Collectors are registered lazily on first use, never at import.
3. **OpenTelemetry spans** — reuse the SDK's tracer/provider so fleet spans nest
   with the SDK's own (opt-in via ``enable_tracing``; no-op without OTel).

Everything is guarded so disabled layers are cheap no-ops.
"""

from __future__ import annotations

import contextlib
import logging
import threading
import time
from dataclasses import dataclass, field

logger = logging.getLogger("agent_sandbox_rl.observability")

# --- Prometheus (optional `metrics` extra; import guarded) ----------------- #
try:
  from prometheus_client import REGISTRY as _REGISTRY
  from prometheus_client import Counter, Gauge, Histogram, start_http_server
  _PROM = True
except Exception:  # pragma: no cover
  _PROM = False

_SEC_BUCKETS = (0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600)


def _metric(cls, name, *args, **kwargs):
  """Create a metric, or reuse the existing collector if ``name`` is already
  registered (idempotent across module re-import/reload, which otherwise raises
  ``Duplicated timeseries``)."""
  try:
    return cls(name, *args, **kwargs)
  except ValueError:
    reg = getattr(_REGISTRY, "_names_to_collectors", {})
    for cand in (name, name + "_total", name + "_count", name + "_sum",
                 name + "_bucket", name + "_created"):
      if cand in reg:
        return reg[cand]
    raise


# The asrl_* collectors are created lazily (see _ensure_metrics) the first time
# an Observer with metrics enabled is built — NOT at import. This keeps merely
# importing the package free of global-registry side effects (prometheus is an
# optional `metrics` extra; the always-on RunReport needs none of it).
PHASE_LATENCY = TASK_LATENCY = RUN_LATENCY = CLAIMS = TASKS = WARM_REPLICAS = None
_METRICS_READY = False


def _ensure_metrics() -> bool:
  """Register the ``asrl_*`` collectors on first use. Returns True if metrics are
  available (prometheus installed); idempotent and safe across re-import."""
  global _METRICS_READY, PHASE_LATENCY, TASK_LATENCY, RUN_LATENCY
  global CLAIMS, TASKS, WARM_REPLICAS
  if _METRICS_READY:
    return True
  if not _PROM:
    return False
  PHASE_LATENCY = _metric(
      Histogram, "asrl_phase_latency_seconds", "Fleet phase latency",
      ["phase", "cluster", "family", "strategy", "status"], buckets=_SEC_BUCKETS)
  TASK_LATENCY = _metric(
      Histogram, "asrl_task_latency_seconds",
      "Per-task acquire+process+release latency",
      ["strategy", "cluster", "family", "status"], buckets=_SEC_BUCKETS)
  RUN_LATENCY = _metric(
      Histogram, "asrl_run_latency_seconds", "Whole-run latency",
      ["strategy", "status"], buckets=_SEC_BUCKETS)
  CLAIMS = _metric(Counter, "asrl_claims_total", "SandboxClaims started",
                   ["cluster", "status"])
  TASKS = _metric(Counter, "asrl_tasks_total", "Tasks processed",
                  ["strategy", "status"])
  WARM_REPLICAS = _metric(Gauge, "asrl_warm_replicas", "Current warm replicas",
                          ["cluster"])
  _METRICS_READY = True
  return True


# --- config --------------------------------------------------------------- #
# (ObservabilityConfig lives in config.py to avoid an import cycle; imported here
#  lazily where needed.)


def repo_family(task_or_image) -> str:
  """Bounded cardinality label for a task/image (its repo family).

  Prefers ``task.metadata['repo']`` (``django/django`` -> ``django``); else
  parses a SWE-bench-style image name
  (``sweb.eval.x86_64.astropy__astropy-12907:latest`` -> ``astropy``); else
  ``"unknown"``.
  """
  repo = None
  if hasattr(task_or_image, "metadata"):
    repo = (getattr(task_or_image, "metadata", None) or {}).get("repo")
  image = getattr(task_or_image, "image", task_or_image)
  if repo:
    return repo.split("/")[-1]
  try:
    s = str(image)
    for arch in ("x86_64.", "arm64."):
      if arch in s:
        # ...<arch><repo>__<instance>:<tag> -> <repo>
        return s.split(arch)[-1].split("__")[0].split(":")[0]
  except Exception:  # pragma: no cover
    pass
  return "unknown"


# --- RunReport ------------------------------------------------------------ #
@dataclass
class RunReport:
  """Always-on per-run performance summary (like the e2e benchmark block)."""

  strategy: str
  total_s: float = 0.0
  phases: dict = field(default_factory=dict)   # name -> [count, total_s, max_s]
  claims: int = 0
  tasks_ok: int = 0
  tasks_err: int = 0
  warm_total: int = 0
  peak_warm: int = 0
  environment: dict = field(default_factory=dict)   # per-cluster details

  _ORDER = ("preflight", "plan", "prepull", "create_warmpool", "wait_pool_ready",
            "prefetch", "claim", "process", "release", "teardown")

  def add_phase(self, name: str, dur: float) -> None:
    c = self.phases.setdefault(name, [0, 0.0, 0.0])
    c[0] += 1
    c[1] += dur
    c[2] = max(c[2], dur)

  def add_task(self, status: str) -> None:
    if status == "ok":
      self.tasks_ok += 1
    else:
      self.tasks_err += 1

  def add_claim(self) -> None:
    self.claims += 1

  def _ordered(self):
    seen = list(self._ORDER) + [p for p in self.phases if p not in self._ORDER]
    return [(p, self.phases[p]) for p in seen if p in self.phases]

  def to_dict(self) -> dict:
    return {
        "strategy": self.strategy,
        "total_s": round(self.total_s, 2),
        "phases": {p: {"count": c[0], "total_s": round(c[1], 2),
                       "max_s": round(c[2], 2)} for p, c in self.phases.items()},
        "claims": self.claims,
        "tasks_ok": self.tasks_ok,
        "tasks_err": self.tasks_err,
        "warm_replicas_total": self.warm_total,
        "warm_replicas_peak": self.peak_warm,
        "environment": self.environment,
    }

  @staticmethod
  def _fmt_env(info: dict) -> str:
    order = ("context", "namespace", "k8s_version", "nodes", "node_pools",
             "instance_types", "region")
    parts = []
    for k in order:
      if k in info and info[k] not in (None, [], ""):
        v = info[k]
        if isinstance(v, list):
          v = "[" + ",".join(str(x) for x in v) + "]"
        parts.append(f"{k}={v}")
    return "  ".join(parts)

  def summary(self) -> str:
    lines = [f"── Run report (strategy={self.strategy}) ──"]
    if self.environment:
      lines.append("  environment:")
      for cname, info in self.environment.items():
        lines.append(f"    {cname}: {self._fmt_env(info)}")
      lines.append("  " + "-" * 40)
    for name, (count, total, mx) in self._ordered():
      lines.append(f"  {name:<18} {total:8.2f}s  (n={count}, max={mx:.2f}s)")
    lines.append("  " + "-" * 40)
    lines.append(f"  {'TOTAL':<18} {self.total_s:8.2f}s")
    lines.append(f"  claims={self.claims}  tasks={self.tasks_ok}ok/{self.tasks_err}err"
                 f"  warm_replicas total={self.warm_total} peak={self.peak_warm}")
    return "\n".join(lines)


# --- tracer (reuse the SDK's provider) ------------------------------------ #
def _init_tracer(service_name: str):
  try:
    from k8s_agent_sandbox.trace_manager import initialize_tracer
    from opentelemetry import trace
    initialize_tracer(service_name)
    return trace.get_tracer(service_name.replace("-", "_"))
  except Exception as e:  # pragma: no cover
    logger.warning("tracing requested but unavailable (%s); spans disabled", e)
    return None


# --- Observer ------------------------------------------------------------- #
class Observer:
  """Records phase/task/run timings to the RunReport, Prometheus, and spans."""

  def __init__(self, config=None):
    from .config import ObservabilityConfig
    self.config = config or ObservabilityConfig()
    # Registering the collectors is deferred to here (first metrics-enabled
    # Observer), so importing the package never touches the global registry.
    self.metrics = bool(self.config.enable_metrics and _PROM and _ensure_metrics())
    self.tracer = _init_tracer(self.config.trace_service_name) if self.config.enable_tracing else None
    self.report: RunReport | None = None
    self._strategy = "-"
    self._lock = threading.Lock()
    self._warm: dict[str, int] = {}

  # span helper
  def _span(self, name):
    if self.tracer is None:
      return contextlib.nullcontext()
    return self.tracer.start_as_current_span(name)

  @contextlib.contextmanager
  def phase(self, name: str, *, cluster: str = "-", family: str = "-"):
    start = time.monotonic()
    status = "ok"
    with self._span(f"asrl.{name}") as span:
      if span is not None:
        for k, v in (("cluster", cluster), ("family", family),
                     ("strategy", self._strategy)):
          try:
            span.set_attribute(k, v)
          except Exception:  # pragma: no cover
            pass
      try:
        yield span
      except BaseException:
        status = "error"
        raise
      finally:
        dur = time.monotonic() - start
        if self.metrics:
          PHASE_LATENCY.labels(phase=name, cluster=cluster, family=family,
                               strategy=self._strategy, status=status).observe(dur)
        rep = self.report
        if rep is not None:
          with self._lock:
            rep.add_phase(name, dur)

  @contextlib.contextmanager
  def run(self, strategy: str):
    self._strategy = strategy
    self.report = RunReport(strategy)
    start = time.monotonic()
    status = "ok"
    with self._span("asrl.run") as span:
      if span is not None:
        try:
          span.set_attribute("strategy", strategy)
        except Exception:  # pragma: no cover
          pass
      try:
        yield self.report
      except BaseException:
        status = "error"
        raise
      finally:
        self.report.total_s = time.monotonic() - start
        if self.metrics:
          RUN_LATENCY.labels(strategy=strategy, status=status).observe(self.report.total_s)

  def task_done(self, cluster: str, family: str, status: str, seconds: float) -> None:
    if self.metrics:
      TASK_LATENCY.labels(strategy=self._strategy, cluster=cluster,
                          family=family, status=status).observe(seconds)
      TASKS.labels(strategy=self._strategy, status=status).inc()
    if self.report is not None:
      with self._lock:
        self.report.add_task(status)

  def claim(self, cluster: str, status: str = "ok") -> None:
    if self.metrics:
      CLAIMS.labels(cluster=cluster, status=status).inc()
    if self.report is not None and status == "ok":
      with self._lock:
        self.report.add_claim()

  def warm_add(self, cluster: str, n: int) -> None:
    # Gauge set inside the lock so concurrent add/remove can't lose an update.
    with self._lock:
      self._warm[cluster] = self._warm.get(cluster, 0) + n
      current = sum(self._warm.values())
      if self.report is not None:
        self.report.warm_total += n
        self.report.peak_warm = max(self.report.peak_warm, current)
      if self.metrics:
        WARM_REPLICAS.labels(cluster=cluster).set(self._warm[cluster])

  def warm_remove(self, cluster: str, n: int) -> None:
    with self._lock:
      self._warm[cluster] = max(0, self._warm.get(cluster, 0) - n)
      if self.metrics:
        WARM_REPLICAS.labels(cluster=cluster).set(self._warm[cluster])

  def warm_reset(self) -> None:
    with self._lock:
      clusters = list(self._warm.keys())
      self._warm.clear()
    if self.metrics:
      for c in clusters:
        WARM_REPLICAS.labels(cluster=c).set(0)


def serve_metrics(port: int = 9095, addr: str = "0.0.0.0"):
  """Opt-in: expose the default Prometheus registry over HTTP (``/metrics``).

  Returns the (server, thread) tuple from ``prometheus_client.start_http_server``.
  The caller owns its lifetime.
  """
  if not _PROM:  # pragma: no cover
    raise RuntimeError("prometheus_client not installed")
  return start_http_server(port, addr)
