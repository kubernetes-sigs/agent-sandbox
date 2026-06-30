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
  # Escape hatch: extra keys merged into the pod spec (e.g. tolerations).
  extra_pod_spec: dict = Field(default_factory=dict)


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
  window_size: int | None = None            # sliding: None = auto from max_concurrent
  ready_timeout: int = 900
  template: TemplateSpec = Field(default_factory=TemplateSpec)
  template_name_prefix: str = "r2e-img-"
  labels: dict[str, str] = Field(default_factory=lambda: dict(constants.DEFAULT_LABELS))
  observability: ObservabilityConfig = Field(default_factory=ObservabilityConfig)

  @field_validator("max_concurrent", "max_warmpool_size")
  @classmethod
  def _positive(cls, v: int) -> int:
    if v < 1:
      raise ValueError("must be >= 1")
    return v

  @field_validator("window_size")
  @classmethod
  def _window_positive(cls, v: int | None) -> int | None:
    if v is not None and v < 1:
      raise ValueError("window_size must be >= 1 or None (auto)")
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
