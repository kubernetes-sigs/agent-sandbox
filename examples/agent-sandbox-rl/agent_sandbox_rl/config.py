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

from pydantic import BaseModel, Field, field_validator

from . import constants

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
  template: TemplateSpec = Field(default_factory=TemplateSpec)
  template_name_prefix: str = "r2e-img-"
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

  @field_validator("template_name_prefix")
  @classmethod
  def _valid_prefix(cls, v: str) -> str:
    # Validate the *final* generated name `<prefix><md5[:12]>`, not just the
    # prefix: a permissive prefix regex still allows names that aren't valid
    # DNS-1123 subdomains (e.g. consecutive dots "r2e..img-", or a segment
    # ending in '-' before a dot), which fail with a 422 only at create time.
    # The 12-char md5 suffix is hex, so "0"*12 is a representative stand-in.
    sample = f"{v}{'0' * 12}"
    dns1123 = r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
    if len(sample) > 253 or not re.match(dns1123, sample):
      raise ValueError(
          "template_name_prefix must yield a DNS-1123 subdomain when combined "
          f"with the 12-char image hash; got a name like {sample!r}")
    return v

  @field_validator("placement")
  @classmethod
  def _known_placement(cls, v: str) -> str:
    if v not in PLACEMENTS:
      raise ValueError(f"unknown placement '{v}'; choose from {sorted(PLACEMENTS)}")
    return v

  def template_name(self, image: str) -> str:
    """Stable, DNS-compliant SandboxTemplate name for an image (same scheme as
    the rl-sandbox-scripts example: ``<prefix><md5[:12]>``)."""
    h = hashlib.md5(image.encode(), usedforsecurity=False).hexdigest()[:12]
    return f"{self.template_name_prefix}{h}"
