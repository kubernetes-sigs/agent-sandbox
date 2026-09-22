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

"""Configuration models for agent-sandbox-rl (pydantic v2).

`FleetConfig` holds one or more `ClusterConfig`s plus orchestration knobs.
Single-cluster is just one entry (or the ambient kube context).
"""

from __future__ import annotations

import hashlib
import re
from typing import Any, Callable

from pydantic import BaseModel, Field, field_validator, model_validator

from . import constants

# A DNS-1123 subdomain: what the apiserver accepts as an object name. Applied to
# the *generated* template/pool names, not to the prefix/format on its own.
_DNS1123 = re.compile(
    r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$")

PLACEMENTS = ("round-robin", "least-loaded", "capacity-weighted", "image-affinity")


class ResourceSpec(BaseModel):
  """Per-sandbox container resource requests."""

  cpu: str = "250m"
  memory: str = "512Mi"


class TemplateSpec(BaseModel):
  """How to render each image's SandboxTemplate pod."""

  resources: ResourceSpec = Field(default_factory=ResourceSpec)
  keepalive_command: list[str] = Field(
      default_factory=lambda: list(constants.KEEPALIVE_COMMAND))
  runtime_class: str | None = None          # e.g. "gvisor"
  node_selector: dict[str, str] | None = None
  image_pull_secret: str | None = None
  # Default IfNotPresent so a node's containerd layer cache is reused across
  # runs/epochs (benchmark images are immutable/pinned). Set "Always" if a tag
  # mutates and you need to re-pull each time.
  image_pull_policy: str = "IfNotPresent"
  # Prefer scheduling a pool's replicas onto the *same* node (soft podAffinity on
  # the shared `sandbox=<template>` label) so only the first replica pulls the
  # image and the rest start from the node's containerd layer cache. Soft, so it
  # spills to other nodes instead of dead-locking when a node is full. Pairs with
  # image_pull_policy=IfNotPresent and warm_per_task (RL "instant-claim" mode).
  colocate_replicas: bool = False
  # Escape hatch: extra keys merged into the pod spec (e.g. tolerations).
  extra_pod_spec: dict = Field(default_factory=dict)

  @field_validator("image_pull_policy")
  @classmethod
  def _valid_pull_policy(cls, v: str) -> str:
    if v not in ("Always", "IfNotPresent", "Never"):
      raise ValueError("image_pull_policy must be Always|IfNotPresent|Never")
    return v


class ObservabilityConfig(BaseModel):
  """Observability toggles. RunReport is always on; metrics/tracing opt-in."""

  enable_metrics: bool = True              # Prometheus `asrl_*` on default registry
  enable_tracing: bool = False             # OpenTelemetry spans (needs the 'tracing' extra)
  trace_service_name: str = "agent-sandbox-rl"


class ClusterConfig(BaseModel):
  """A single target cluster. Defaults to the ambient kube context."""

  name: str = "default"
  kubeconfig: str | None = None             # path; None = default kubeconfig
  context: str | None = None                # kube context name; None = current
  in_cluster: bool = False
  namespace: str = "default"
  # Per-cluster overrides (fall back to FleetConfig.template if unset).
  node_selector: dict[str, str] | None = None
  runtime_class: str | None = None
  image_pull_secret: str | None = None
  weight: float = 1.0                        # for CapacityWeighted placement
  max_replicas: int | None = None           # optional hard capacity hint

  @field_validator("weight")
  @classmethod
  def _weight_positive(cls, v: float) -> float:
    if v <= 0:
      raise ValueError("cluster weight must be > 0")
    return v

  @field_validator("name")
  @classmethod
  def _name_nonempty(cls, v: str) -> str:
    if not v:
      raise ValueError("cluster name must be non-empty")
    return v


class FleetConfig(BaseModel):
  """Top-level fleet configuration."""

  clusters: list[ClusterConfig] = Field(default_factory=list)
  placement: str = "image-affinity"         # round-robin|least-loaded|capacity-weighted|image-affinity
  max_concurrent: int = 1                    # concurrency budget: sizes pools AND parallelizes claims
  max_warmpool_size: int = 8                 # hard cap on replicas per image pool
  # Warm one replica per task for each image (replicas = min(tasks_image,
  # max_warmpool_size)) instead of concurrency-proportional sizing, so every task
  # claims a sandbox immediately (RL "instant-claim"). Trades resources for claim
  # latency; raise max_warmpool_size for images with more tasks than the cap.
  warm_per_task: bool = False
  window_size: int | None = None            # sliding: None = auto from max_concurrent
  ready_timeout: int = 900
  # Stage the warm fill in waves of <= this many sandbox creates in flight,
  # waiting for each wave to reach Ready before the next. Bounds the controller's
  # concurrent create burst (Σ pools×replicas). On controllers <= v0.5.3 this
  # mitigates (but does NOT prevent) the SandboxWarmPool over-creation race
  # (issue 1215; fixed in v0.5.4 by PR 1266) — those controllers still need
  # --sandbox-warm-pool-concurrent-workers <= 10. 0 = warm all at once (old behavior).
  warm_create_budget: int = 1000
  # --- runaway safeguards (see plans/sdk-runaway-safeguards.md) ------------- #
  # Circuit breaker (#1, fail-safe): abort + teardown if live sandboxes this run
  # owns exceed the safe ceiling — min(expected × overcommit_factor,
  # max_live_sandboxes). Keys off *intent*, so it catches accidental over-creation
  # (runaway/orphan/#1215) not a large-but-intended run. 0/None on factor disables.
  overcommit_factor: float = 1.5
  max_live_sandboxes: int | None = None      # optional absolute hard ceiling
  breaker_poll_s: float = 5.0
  # Trip only after the ceiling is breached for this many *consecutive* polls, so a
  # transient spike (a claim briefly double-warms before scale-down; Terminating-pod
  # GC lag) can't tear down a healthy run on a single sample. One healthy poll resets
  # the counter. Sustained breach = breaker_trip_polls × breaker_poll_s.
  breaker_trip_polls: int = 3
  # Guaranteed teardown (#4): install atexit + SIGINT/SIGTERM handlers that tear
  # the fleet down on any exit path (orphan defense; pairs with the run-id label +
  # reaper). Set False if the host owns signal handling.
  install_teardown_hooks: bool = True
  template: TemplateSpec = Field(default_factory=TemplateSpec)
  template_name_prefix: str = "r2e-img-"
  # How a pool name is derived from its template. `{template}` is the full
  # SandboxTemplate name, `{image_hash}` the bare 12-char image digest.
  #
  # This is configurable because the pool name is an INTERFACE, not an internal
  # detail: whoever provisioned a warm pool chose its name, and a harness that
  # hardcodes a different one does not find pools that are standing right in
  # front of it. The multi-cluster fleet layer, for instance, names its pools
  # `{template}-pool` — set that here to line up with a fleet-provisioned
  # cluster. See `adopt_existing` for the stronger guarantee (match by image and
  # refuse to provision), which does not depend on getting this string right.
  pool_name_format: str = "pool-{template}"
  # Use warm pools that already exist instead of creating any. See the field-by-
  # field contract in `SandboxFleet._plan_adopt`; in short, planning matches each
  # task image against the pools already in the namespace (by the image in their
  # template, so any naming scheme works) and raises `PoolNotFoundError` for an
  # image nothing serves, rather than quietly building a size-1 pool for it.
  adopt_existing: bool = False
  # How concurrent runs are kept apart (constants.RUN_ISOLATION_MODES). "none"
  # (default, historical): stable per-image names in `clusters[].namespace` —
  # fine when nothing else runs there. "names": several runs share the namespace
  # and the run id is baked into every template/pool name, so runs on the same
  # image never share, resize, or delete each other's pools. "namespace": each run
  # gets `<namespace>-<run id>`, created on first use (labelled with this run's
  # labels + `run_namespace_labels`) and deleted at teardown if this fleet created
  # it. Anything a fresh namespace needs beyond labels (a Kueue LocalQueue, quotas,
  # a pull secret) is the caller's job — do it in `run_namespace_setup(cluster,
  # namespace)`, which must tolerate a re-run: a namespace kept after a failed
  # rollback delete gets the hook again until it succeeds once. Teardown only
  # ever sweeps this run's resources, in every mode.
  run_isolation: str = "none"
  run_namespace_labels: dict[str, str] = Field(default_factory=dict)
  run_namespace_setup: Callable[..., Any] | None = None
  labels: dict[str, str] = Field(default_factory=lambda: dict(constants.DEFAULT_LABELS))
  observability: ObservabilityConfig = Field(default_factory=ObservabilityConfig)
  # Disk-aware window sizing (all optional; when avg_image_gb is None it's a no-op
  # and sizing falls back to the concurrency-only window). node_ephemeral_gb is the
  # usable ephemeral storage per node; disk_headroom reserves a fraction of it.
  avg_image_gb: float | None = None
  node_ephemeral_gb: float | None = None
  disk_headroom: float = 0.25
  # Node count of the target pool. Lets disk-aware window sizing use the *cluster's*
  # usable disk (distinct images spread across nodes) instead of a single node's.
  # None => conservative single-node bound (safe when the pool size is unknown).
  cluster_nodes: int | None = None

  @field_validator("max_concurrent", "max_warmpool_size")
  @classmethod
  def _positive(cls, v: int) -> int:
    if v < 1:
      raise ValueError("must be >= 1")
    return v

  @field_validator("avg_image_gb", "node_ephemeral_gb")
  @classmethod
  def _disk_positive(cls, v: float | None) -> float | None:
    if v is not None and v <= 0:
      raise ValueError("disk size hints must be > 0 or None")
    return v

  @field_validator("disk_headroom")
  @classmethod
  def _headroom_fraction(cls, v: float) -> float:
    if not (0 <= v < 1):
      raise ValueError("disk_headroom must be in [0, 1)")
    return v

  @field_validator("window_size", "cluster_nodes")
  @classmethod
  def _window_positive(cls, v: int | None) -> int | None:
    if v is not None and v < 1:
      raise ValueError("must be >= 1 or None")
    return v

  @field_validator("run_isolation")
  @classmethod
  def _known_isolation(cls, v: str) -> str:
    if v not in constants.RUN_ISOLATION_MODES:
      raise ValueError(f"unknown run_isolation '{v}'; choose from "
                       f"{list(constants.RUN_ISOLATION_MODES)}")
    return v

  @field_validator("template_name_prefix")
  @classmethod
  def _valid_prefix(cls, v: str) -> str:
    # Validate the *final* generated name `<prefix><md5[:12]>`, not just the
    # prefix: a permissive prefix regex still allows names that aren't valid
    # DNS-1123 subdomains (e.g. consecutive dots "r2e..img-", or a segment
    # ending in '-' before a dot), which fail with a 422 only at create time.
    # The 12-char md5 suffix is hex, so "0"*12 is a representative stand-in; the
    # run id a `{run_id}` placeholder expands to is also 12 hex chars.
    sample = f"{v}{'0' * 12}".replace(constants.RUN_ID_PLACEHOLDER, _SAMPLE_RUN_ID)
    if len(sample) > 253 or not _DNS1123.match(sample):
      raise ValueError(
          "template_name_prefix must yield a DNS-1123 subdomain when combined "
          f"with the 12-char image hash; got a name like {sample!r}")
    return v

  @model_validator(mode="after")
  def _isolation_vs_adoption(self) -> "FleetConfig":
    # A fresh per-run namespace has nothing in it to adopt: plan() would create the
    # namespace and then raise PoolNotFoundError for every image. Say so up front.
    if self.adopt_existing and self.run_isolation == "namespace":
      raise ValueError(
          "adopt_existing=True cannot be combined with run_isolation='namespace': "
          "a per-run namespace holds no pools to adopt. Adopt from the shared "
          "namespace (run_isolation='none' or 'names') instead.")
    return self

  @model_validator(mode="after")
  def _valid_pool_name_format(self) -> "FleetConfig":
    # Validated here rather than as a field_validator because the rendered name
    # depends on template_name_prefix too.
    # Distinct stand-ins for the image hash and the run id: rendering both to the
    # same string would let a format with no per-image part (`pool-{run_id}`)
    # pass the uniqueness check below and map every image onto one pool.
    sample_hash = "0" * 12
    sample_template = (f"{self.template_name_prefix}{sample_hash}"
                       .replace(constants.RUN_ID_PLACEHOLDER, _SAMPLE_RUN_ID))
    try:
      sample = (self.pool_name_format
                .replace(constants.RUN_ID_PLACEHOLDER, _SAMPLE_RUN_ID)
                .format(template=sample_template, image_hash=sample_hash))
    except (KeyError, IndexError, ValueError) as e:
      # ValueError too: an unmatched brace ("pool-{template") raises it from
      # str.format, and it should get this actionable message, not escape raw.
      raise ValueError(
          "pool_name_format may only reference {template}, {image_hash} and "
          f"{{run_id}} (use {{{{ }}}} for a literal brace); got "
          f"{self.pool_name_format!r}") from e
    if sample_hash not in sample:
      # Not pedantry: a format with no per-image part maps EVERY image onto one
      # pool name, so the second image silently upserts the first one's pool and
      # its tasks claim sandboxes running the wrong image. There is no later
      # error — the run just produces garbage.
      raise ValueError(
          "pool_name_format must include {template} or {image_hash}, otherwise "
          "every image resolves to the same pool name and they collide; got "
          f"{self.pool_name_format!r}")
    if len(sample) > 253 or not _DNS1123.match(sample):
      raise ValueError(
          "pool_name_format must yield a DNS-1123 subdomain; "
          f"{self.pool_name_format!r} renders as {sample!r}")
    return self

  @field_validator("placement")
  @classmethod
  def _known_placement(cls, v: str) -> str:
    if v not in PLACEMENTS:
      raise ValueError(f"unknown placement '{v}'; choose from {sorted(PLACEMENTS)}")
    return v

  def image_hash(self, image: str) -> str:
    """The 12-char digest that makes template/pool names stable per image."""
    return hashlib.md5(image.encode(), usedforsecurity=False).hexdigest()[:12]

  def template_name(self, image: str) -> str:
    """Stable, DNS-compliant SandboxTemplate name for an image (same scheme as
    the rl-sandbox-scripts example: ``<prefix><md5[:12]>``)."""
    return f"{self.template_name_prefix}{self.image_hash(image)}"

  def pool_name(self, image: str) -> str:
    """Stable SandboxWarmPool name for an image, per ``pool_name_format``.

    Subclass `FleetConfig` and override this for a scheme the format string can't
    express (a per-image lookup table, a cluster-qualified name); everything in
    the SDK that names a pool goes through here."""
    h = self.image_hash(image)
    return self.pool_name_format.format(
        template=self.template_name(image), image_hash=h)

  def apply_run_isolation(self, run_id: str) -> None:
    """Resolve the run-dependent parts of this config for ``run_id``. Called once
    by `SandboxFleet.__init__` on its private copy, before the registry is built.

    ``{run_id}`` placeholders in `template_name_prefix` / `pool_name_format` are
    substituted in every mode. ``"names"`` additionally makes BOTH names unique to
    this run, independently: the template prefix gets the run id unless it already
    carries the placeholder, and the pool format gets it unless it carries the
    placeholder or `{template}` (which inherits the prefix's run id). Either name
    left shared would be reachable by the provisioning path — `ensure_template`
    reuses an existing template and `create_warmpool(reconcile=True)` resizes an
    existing pool. ``"namespace"`` rewrites each cluster's namespace to
    `run_namespace()`."""
    ph = constants.RUN_ID_PLACEHOLDER
    if self.run_isolation == "names":
      if ph not in self.template_name_prefix:
        self.template_name_prefix = f"{self.template_name_prefix}{ph}-"
      if ph not in self.pool_name_format and "{template}" not in self.pool_name_format:
        self.pool_name_format = f"{self.pool_name_format}-{ph}"
    self.template_name_prefix = self.template_name_prefix.replace(ph, run_id)
    self.pool_name_format = self.pool_name_format.replace(ph, run_id)
    self._check_resolved_names()
    if self.run_isolation == "namespace":
      for c in self.clusters:
        c.namespace = run_namespace(c.namespace, run_id)

  def _check_resolved_names(self) -> None:
    """Re-validate the rendered template and pool names once the run id is in.

    The field validators saw the placeholder form; ``"names"`` mode may have added
    13 characters to each name since, and a prefix that was valid at the 253-char
    limit no longer is. Better a ValueError here than a 422 on the first create."""
    sample_hash = "0" * 12
    template = f"{self.template_name_prefix}{sample_hash}"
    pool = self.pool_name_format.format(template=template, image_hash=sample_hash)
    for kind, name in (("template", template), ("pool", pool)):
      if len(name) > 253 or not _DNS1123.match(name):
        raise ValueError(
            f"resolved {kind} name {name!r} ({len(name)} chars) is not a valid "
            "DNS-1123 subdomain after the run id was added; shorten "
            "template_name_prefix / pool_name_format")


_DNS1123_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")
# Stand-in for a run id when validating name formats (12 hex chars, like the real
# thing, and deliberately different from the "0"*12 image-hash stand-in).
_SAMPLE_RUN_ID = "f" * 12


def run_namespace(base: str, run_id: str) -> str:
  """The per-run namespace for ``run_isolation="namespace"``: ``<base>-<run_id>``.

  Namespace names are DNS-1123 *labels* (63 chars, lowercase alphanumerics and
  '-'), stricter than the subdomain rule object names follow, so validate here
  rather than discover it as a 422 at create time."""
  ns = f"{base}-{run_id}"
  if not _DNS1123_LABEL.match(ns):
    raise ValueError(
        f"run namespace {ns!r} is not a valid DNS-1123 label (lowercase "
        f"alphanumerics and '-', 63 chars max); shorten the base namespace "
        f"{base!r} or use run_isolation='names'")
  return ns
