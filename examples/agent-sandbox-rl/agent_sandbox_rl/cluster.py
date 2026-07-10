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

"""Multi-cluster wiring.

A `Cluster` owns its own kube `ApiClient` (so we can target many contexts at
once) and exposes per-cluster `CustomObjectsApi`/`CoreV1Api`/`AppsV1Api`, a
`Resources` manager, and — pointed at this context via attribute injection — the
SDK's `K8sHelper` / `SandboxClient` (built lazily). No SDK fork required.
"""

from __future__ import annotations

import threading
from collections import OrderedDict

from kubernetes import client
from kubernetes import config as k8s_config

from .config import ClusterConfig, TemplateSpec
from .resources import Resources


def build_api_client(cfg: ClusterConfig, *, pool_maxsize: int = 1000):
  """Build a kube ApiClient for one cluster's context (mockable in tests).

  Sizes the urllib3 connection pool to ``pool_maxsize`` (default 1000). The
  kubernetes client default (~60) throttles high-concurrency claim fan-out: a
  500–1000-wide ``process_parallel`` otherwise serializes on the pool ("Connection
  pool is full, discarding connection"), inflating claim latency and stalling large
  runs. Sizing it to the concurrency budget keeps claims parallel."""
  configuration = client.Configuration()
  configuration.connection_pool_maxsize = pool_maxsize
  if cfg.in_cluster:
    k8s_config.load_incluster_config(client_configuration=configuration)
  else:
    k8s_config.load_kube_config(config_file=cfg.kubeconfig, context=cfg.context,
                                client_configuration=configuration)
  return client.ApiClient(configuration)


class Cluster:
  """A single target cluster and its API clients."""

  def __init__(self, config: ClusterConfig, *, api_client=None,
               labels: dict | None = None):
    self.config = config
    self.name = config.name
    self.namespace = config.namespace
    self.api_client = api_client if api_client is not None else build_api_client(config)
    self.custom_api = client.CustomObjectsApi(self.api_client)
    self.core_api = client.CoreV1Api(self.api_client)
    self.apps_api = client.AppsV1Api(self.api_client)
    self.resources = Resources(
        self.custom_api, self.core_api, self.namespace, labels=labels)
    self._k8s_helper = None
    self._sandbox_client = None
    self.tracer_config = None                 # optional SDK SandboxTracerConfig
    # Placement / capacity bookkeeping. Mutated from parallel claim workers, so
    # guard with a per-cluster lock and only touch via the methods below.
    self._count_lock = threading.Lock()
    self.active_replicas = 0
    self.active_claims = 0
    # One CoreV1Api per thread for router-free pod exec (see exec_core_api).
    self._exec_local = threading.local()

  def exec_core_api(self):
    """A thread-local `CoreV1Api` for router-free pod exec.

    The kubernetes ``stream()`` (websocket) exec is not thread-safe across a
    shared client, so each thread gets its own client — but cached per thread
    rather than rebuilt per call. A fresh ``ApiClient`` per exec would leak a
    urllib3 connection pool + thread pool every time (GC-reliant), which at this
    package's scale (a sandbox per trajectory → many execs) exhausts fds.
    """
    core = getattr(self._exec_local, "core_api", None)
    if core is None:
      core = client.CoreV1Api(client.ApiClient(self.api_client.configuration))
      self._exec_local.core_api = core
    return core

  @property
  def k8s_helper(self):
    """SDK K8sHelper pointed at this cluster's context (lazy, injected)."""
    if self._k8s_helper is None:
      from k8s_agent_sandbox.k8s_helper import K8sHelper
      helper = K8sHelper()
      helper.custom_objects_api = self.custom_api
      helper.core_v1_api = self.core_api
      self._k8s_helper = helper
    return self._k8s_helper

  @property
  def sandbox_client(self):
    """SDK SandboxClient using this cluster's K8sHelper (lazy, injected)."""
    if self._sandbox_client is None:
      from k8s_agent_sandbox import SandboxClient
      c = (SandboxClient(tracer_config=self.tracer_config)
           if self.tracer_config is not None else SandboxClient())
      c.k8s_helper = self.k8s_helper
      self._sandbox_client = c
    return self._sandbox_client

  def template_spec(self, base: TemplateSpec) -> TemplateSpec:
    """Merge per-cluster overrides onto a base TemplateSpec."""
    data = base.model_dump()
    if self.config.node_selector is not None:
      data["node_selector"] = self.config.node_selector
    if self.config.runtime_class is not None:
      data["runtime_class"] = self.config.runtime_class
    if self.config.image_pull_secret is not None:
      data["image_pull_secret"] = self.config.image_pull_secret
    return TemplateSpec(**data)

  @property
  def capacity(self) -> int | None:
    return self.config.max_replicas

  def has_capacity(self, additional: int) -> bool:
    with self._count_lock:
      if self.config.max_replicas is None:
        return True
      return self.active_replicas + additional <= self.config.max_replicas

  # --- atomic capacity bookkeeping (thread-safe) ------------------------- #
  def reserve_replicas(self, n: int) -> None:
    with self._count_lock:
      self.active_replicas += n

  def release_replicas(self, n: int) -> None:
    with self._count_lock:
      self.active_replicas = max(0, self.active_replicas - n)

  def reserve_claim(self) -> None:
    with self._count_lock:
      self.active_claims += 1

  def release_claim(self) -> None:
    with self._count_lock:
      if self.active_claims > 0:
        self.active_claims -= 1

  def reset_counts(self) -> None:
    with self._count_lock:
      self.active_replicas = 0
      self.active_claims = 0

  def __repr__(self) -> str:
    return (f"Cluster(name={self.name!r}, namespace={self.namespace!r}, "
            f"active_replicas={self.active_replicas}, "
            f"active_claims={self.active_claims})")


class ClusterRegistry:
  """An ordered set of clusters, keyed by name."""

  def __init__(self, clusters: list[Cluster] | None = None):
    self._by_name: "OrderedDict[str, Cluster]" = OrderedDict()
    for c in clusters or []:
      self.add(c)

  def add(self, cluster: Cluster) -> None:
    if cluster.name in self._by_name:
      raise ValueError(f"duplicate cluster name: {cluster.name!r}")
    self._by_name[cluster.name] = cluster

  def get(self, name: str) -> Cluster:
    return self._by_name[name]

  def names(self) -> list[str]:
    return list(self._by_name.keys())

  def __iter__(self):
    return iter(self._by_name.values())

  def __len__(self) -> int:
    return len(self._by_name)

  @classmethod
  def from_configs(cls, configs: list[ClusterConfig],
                   *, labels: dict | None = None) -> "ClusterRegistry":
    return cls([Cluster(c, labels=labels) for c in configs])
