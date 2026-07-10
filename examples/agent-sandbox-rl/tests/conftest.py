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

"""Shared test fakes: a FakeCluster that mimics agent_sandbox_rl.cluster.Cluster
without any real Kubernetes access."""

import threading
import types
from unittest.mock import MagicMock

import pytest

from agent_sandbox_rl import ClusterRegistry


class FakeCluster:
  def __init__(self, name, namespace="ns", weight=1.0, max_replicas=None):
    self.name = name
    self.namespace = namespace
    self.active_claims = 0
    self.active_replicas = 0
    self._lock = threading.Lock()      # mirror Cluster: counters mutated concurrently
    self.config = types.SimpleNamespace(weight=weight, max_replicas=max_replicas)
    self.api_client = MagicMock()
    self.core_api = MagicMock()
    self.apps_api = MagicMock()
    self.resources = MagicMock()
    self.resources.managed_selector.return_value = "app=agent-sandbox-rl"
    self.resources.list_warmpools.return_value = []
    self.resources.list_templates.return_value = []
    self.resources.list_claims.return_value = []
    self.resources.ensure_template.return_value = True
    self.resources.wait_for_pool_ready.return_value = True
    self._seq = 0
    self.sandbox_client = MagicMock()
    self.sandbox_client.create_sandbox.side_effect = self._make_sandbox

  def has_capacity(self, additional: int = 1) -> bool:
    with self._lock:
      if self.config.max_replicas is None:
        return True
      return self.active_replicas + additional <= self.config.max_replicas

  # atomic capacity bookkeeping (mirrors agent_sandbox_rl.cluster.Cluster)
  def reserve_replicas(self, n):
    with self._lock:
      self.active_replicas += n

  def release_replicas(self, n):
    with self._lock:
      self.active_replicas = max(0, self.active_replicas - n)

  def reserve_claim(self):
    with self._lock:
      self.active_claims += 1

  def release_claim(self):
    with self._lock:
      if self.active_claims > 0:
        self.active_claims -= 1

  def reset_counts(self):
    with self._lock:
      self.active_replicas = 0
      self.active_claims = 0

  def template_spec(self, base):
    return base

  def _make_sandbox(self, warmpool=None, namespace=None,
                    sandbox_ready_timeout=None, labels=None, **_):
    with self._lock:                   # claims run concurrently -> unique seq
      self._seq += 1
      seq = self._seq
    s = MagicMock()
    s.claim_name = f"claim-{self.name}-{seq}"
    s.sandbox_id = f"sb-{self.name}-{seq}"
    s.get_pod_name.return_value = f"pod-{self.name}-{seq}"
    s.get_pod_ip.return_value = "10.0.0.1"
    return s


@pytest.fixture
def make_cluster():
  return FakeCluster


@pytest.fixture
def two_cluster_registry():
  return ClusterRegistry([FakeCluster("a"), FakeCluster("b")])
