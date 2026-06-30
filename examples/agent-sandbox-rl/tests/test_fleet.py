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

import pytest

from agent_sandbox_rl import ClusterRegistry, FleetConfig, SandboxFleet
from agent_sandbox_rl.preflight import PreflightReport


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  """Fleet/strategy tests use FakeClusters; the real preflight is tested in
  test_preflight.py. Here we stub it to always pass."""
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _fleet(registry, **cfg):
  return SandboxFleet(FleetConfig(**cfg), registry=registry)


def test_naive_epochs_reuse_pools(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["i1", "i2"])
  res = f.run(lambda t, h: t.image, strategy="naive", epochs=3)
  # epochs>1 -> one task-ordered list per pass
  assert len(res) == 3
  assert all(sorted(r) == ["i1", "i2"] for r in res)
  # pools created once and REUSED across epochs (2 images, not 2*3)
  assert c.resources.create_warmpool.call_count == 2
  assert f.handles() == []
  assert c.active_replicas == 0          # torn down once, at the end


def test_keep_warm_persists_then_explicit_teardown(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["i1", "i2"])
  f.run(lambda t, h: t.image, strategy="naive", keep_warm=True)
  # pools left resident (no final teardown) for caller-driven reuse
  assert c.active_replicas == 2
  assert set(f._warmed) == {"i1", "i2"}
  # a second keep_warm run reuses them — no new pools, no double-reserve
  f.run(lambda t, h: t.image, strategy="naive", keep_warm=True)
  assert c.resources.create_warmpool.call_count == 2
  assert c.active_replicas == 2
  # explicit teardown fully cleans up
  f.teardown()
  assert c.active_replicas == 0
  assert f._warmed == {}


def test_epochs_must_be_positive(make_cluster):
  f = _fleet(ClusterRegistry([make_cluster("solo")]))
  f.load_tasks(["i1"])
  with pytest.raises(ValueError):
    f.run(lambda t, h: t.image, strategy="naive", epochs=0)


def test_warm_entry_reserves_only_delta_on_scale_up(make_cluster):
  # Re-warming an image with MORE replicas must reserve only the delta (the pool
  # is upserted, not recreated) so active_replicas isn't double-counted.
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=8)
  f.load_tasks(["i1"])
  f.warm_image("i1", replicas_override=2)
  assert c.active_replicas == 2 and f._warmed["i1"] == 2
  f.warm_image("i1", replicas_override=4)        # scale up 2 -> 4
  assert c.active_replicas == 4 and f._warmed["i1"] == 4
  # already satisfied -> no further reservation
  f.warm_image("i1", replicas_override=4)
  assert c.active_replicas == 4


def test_explicit_empty_registry_is_honored_no_kubeconfig(monkeypatch):
  # An explicit (even empty) registry must NOT be replaced by a default ambient
  # Cluster — ClusterRegistry defines __len__, so `registry or default` would
  # treat ClusterRegistry([]) as falsy and load kube-config (fails in CI).
  import agent_sandbox_rl.fleet as fleet_mod

  def _boom(*a, **k):
    raise AssertionError("must not build a default Cluster / load kube-config")
  monkeypatch.setattr(fleet_mod, "Cluster", _boom)   # the name _default_registry uses
  f = SandboxFleet(FleetConfig(), registry=ClusterRegistry([]))
  assert len(f.registry) == 0           # honored as-is, no fallback


def test_warm_entry_waits_on_reuse(make_cluster):
  # Re-warming an already-warm image with wait=True must still wait for readiness
  # (a prior warm may have used wait=False), not silently skip the check.
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["i1"])
  f.warm_image("i1", wait=False)                 # warm without waiting
  assert c.resources.wait_for_pool_ready.call_count == 0
  f.warm_image("i1", wait=True)                  # reuse — must wait now
  assert c.resources.wait_for_pool_ready.call_count == 1
  # no re-create / no extra reservation on reuse
  assert c.resources.create_warmpool.call_count == 1


def test_warm_images_dedupes_input(make_cluster):
  # Duplicates in the public helper must not warm the same image twice (unsafe).
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["i1", "i2"])
  f.warm_images(["i1", "i1", "i2", "i1"], wait=True)
  assert c.resources.create_warmpool.call_count == 2


def test_epoch_failure_tears_down_when_not_keep_warm(make_cluster):
  # A non-final epoch that raises (teardown=False) must still clean up, so warm
  # pools / reserved replicas don't leak when keep_warm=False.
  from agent_sandbox_rl.exceptions import FleetError
  c = make_cluster("solo")
  c.resources.wait_for_pool_ready.return_value = False    # warm never ready -> FleetError
  f = _fleet(ClusterRegistry([c]), max_concurrent=4, ready_timeout=0)
  f.load_tasks(["i1", "i2"])
  with pytest.raises(FleetError):
    f.run(lambda t, h: t.image, strategy="naive", epochs=2)
  assert c.active_replicas == 0          # epoch-1 failure still tore down
  assert f._warmed == {}


def test_load_tasks_and_counts(two_cluster_registry):
  f = _fleet(two_cluster_registry)
  f.load_tasks(["imgA", "imgA", "imgB"])
  assert len(f.tasks) == 3
  assert dict(f.image_counts()) == {"imgA": 2, "imgB": 1}


def test_warm_images_warms_in_parallel(make_cluster):
  import time
  c = make_cluster("solo")

  def _slow_ready(*a, **k):
    time.sleep(0.1)
    return True
  c.resources.wait_for_pool_ready.side_effect = _slow_ready

  f = _fleet(ClusterRegistry([c]), max_concurrent=10)
  imgs = [f"img{i}" for i in range(10)]
  f.load_tasks(imgs)
  start = time.monotonic()
  f.warm_images(imgs, wait=True)            # 10 pools, each 0.1s ready
  elapsed = time.monotonic() - start
  assert elapsed < 0.6                       # parallel; serial would be ~1.0s
  assert c.resources.create_warmpool.call_count == 10


def test_warm_images_surfaces_warm_error(make_cluster):
  from agent_sandbox_rl.exceptions import FleetError
  c = make_cluster("solo")
  c.resources.wait_for_pool_ready.return_value = False   # never ready -> FleetError
  f = _fleet(ClusterRegistry([c]), max_concurrent=4, ready_timeout=0)
  f.load_tasks(["i1", "i2", "i3"])
  with pytest.raises(FleetError):
    f.warm_images(["i1", "i2", "i3"], wait=True)


def test_recommended_window_uses_cluster_nodes(make_cluster):
  # disk-aware window should span the whole pool when cluster_nodes is set
  imgs = [f"img{i}" for i in range(100)]
  f1 = _fleet(ClusterRegistry([make_cluster("a")]), max_concurrent=500,
              avg_image_gb=10, node_ephemeral_gb=339)            # nodes unknown -> 1 node
  f1.load_tasks(imgs)
  f2 = _fleet(ClusterRegistry([make_cluster("b")]), max_concurrent=500,
              avg_image_gb=10, node_ephemeral_gb=339, cluster_nodes=30)
  f2.load_tasks(imgs)
  assert f1.recommended_window() < f2.recommended_window()       # 25 (1 node) < 100 (pool)
  assert f2.recommended_window() == 100                          # all fit across the pool


def test_warm_per_task_sizes_replicas_to_task_count(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=1, warm_per_task=True)
  f.load_tasks(["i1", "i1", "i1", "i2"])           # i1: 3 tasks, i2: 1 task
  reps = {e.image: e.replicas for e in f.plan().entries}
  assert reps == {"i1": 3, "i2": 1}                # one replica per task


def test_warm_per_task_clamps_to_max_pool_and_warns(make_cluster, caplog):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=1, warm_per_task=True,
             max_warmpool_size=2)
  f.load_tasks(["i1", "i1", "i1"])                  # 3 tasks, cap 2
  with caplog.at_level("WARNING"):
    reps = {e.image: e.replicas for e in f.plan().entries}
  assert reps == {"i1": 2}                          # clamped to max_warmpool_size
  assert any("warm_per_task" in r.message for r in caplog.records)


def test_pipelined_plus_warm_per_task_warns(make_cluster, caplog):
  # the documented anti-pattern (window shrinkage) is guarded at runtime
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4, warm_per_task=True)
  f.load_tasks(["i1", "i1", "i2", "i2"])
  with caplog.at_level("WARNING"):
    f.recommended_window(pipelined=True)
  assert any("pipelined" in r.message and "warm_per_task" in r.message
             for r in caplog.records)
  # sliding (non-pipelined) does NOT warn
  caplog.clear()
  with caplog.at_level("WARNING"):
    f.recommended_window(pipelined=False)
  assert not any("pipelined" in r.message for r in caplog.records)


def test_preflight_ok(two_cluster_registry):
  f = _fleet(two_cluster_registry)
  report = f.preflight()
  assert set(report) == {"a", "b"}
  assert all(r.ok for r in report.values())


def test_plan_routes_across_two_clusters(two_cluster_registry):
  # round-robin over 2 unique images -> one per cluster.
  f = _fleet(two_cluster_registry, placement="round-robin")
  f.load_tasks(["imgA", "imgB"])
  plan = f.plan()
  clusters = {e.cluster for e in plan.entries}
  assert clusters == {"a", "b"}
  assert plan.total_replicas == 2  # 1 task each, max_concurrent=1


def test_start_warmpools_provisions_each_entry(two_cluster_registry):
  f = _fleet(two_cluster_registry, placement="round-robin", max_concurrent=2)
  f.load_tasks(["imgA", "imgB"])
  f.plan()
  f.start_warmpools(wait=True)
  for c in two_cluster_registry:
    c.resources.create_warmpool.assert_called()
    c.resources.wait_for_pool_ready.assert_called()
    assert c.active_replicas >= 1


def test_acquire_returns_handle_on_right_cluster(two_cluster_registry):
  f = _fleet(two_cluster_registry, placement="image-affinity")
  tasks = f.load_tasks(["imgA", "imgB"])
  f.plan()
  h0 = f.acquire(tasks[0])
  h1 = f.acquire(tasks[1])
  # handle carries cluster + stable hostname; hostnames are unique.
  assert h0.cluster_name in ("a", "b")
  assert h0.hostname == h0.sandbox_id and h0.pod_name.startswith("pod-")
  assert h0.hostname != h1.hostname
  assert set(f.hostnames()) == {h0.hostname, h1.hostname}
  # the chosen cluster recorded an active claim
  assert f.registry.get(h0.cluster_name).active_claims >= 1


def test_endpoints_are_cluster_qualified(two_cluster_registry):
  f = _fleet(two_cluster_registry)
  t = f.load_tasks(["imgA"])[0]
  f.plan()
  h = f.acquire(t)
  ep = f.endpoints(port=9000)[0]
  assert ep == f"{h.hostname}.ns:9000"


def test_release_and_teardown(two_cluster_registry):
  f = _fleet(two_cluster_registry, placement="round-robin")
  tasks = f.load_tasks(["imgA", "imgB"])
  f.plan()
  hs = f.acquire_batch(tasks)
  for h in hs:
    h.sandbox.terminate.assert_not_called()
  f.teardown()
  # every claim released (terminate called) and bookkeeping reset
  for h in hs:
    h.sandbox.terminate.assert_called_once()
  assert f.handles() == []
  for c in two_cluster_registry:
    assert c.active_claims == 0 and c.active_replicas == 0


def test_run_managed_naive(two_cluster_registry):
  f = _fleet(two_cluster_registry, placement="round-robin")
  f.load_tasks(["imgA", "imgB"])
  seen = []
  results = f.run(lambda task, h: seen.append((task.image, h.cluster_name)) or h.pod_name)
  assert len(results) == 2
  assert {img for img, _ in seen} == {"imgA", "imgB"}
  assert f.handles() == []          # all released by teardown


def test_default_registry_from_config_clusters(monkeypatch):
  # FleetConfig with clusters -> registry built without touching a real cluster.
  import agent_sandbox_rl.cluster as cl
  monkeypatch.setattr(cl, "build_api_client", lambda cfg: object())
  from agent_sandbox_rl import ClusterConfig
  f = SandboxFleet(FleetConfig(clusters=[ClusterConfig(name="c1"),
                                         ClusterConfig(name="c2")]))
  assert f.registry.names() == ["c1", "c2"]


def test_acquire_rolls_back_on_create_failure(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  c.sandbox_client.create_sandbox.side_effect = RuntimeError("boom")
  with pytest.raises(RuntimeError):
    f.acquire(f.tasks[0])
  # on-demand replica bump rolled back; nothing tracked/leaked
  assert c.active_replicas == 0
  assert c.active_claims == 0
  assert f.handles() == []


def test_acquire_terminates_sandbox_on_pod_name_failure(make_cluster):
  from unittest.mock import MagicMock
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  bad = MagicMock()
  bad.claim_name = "cx"
  bad.sandbox_id = "sx"
  bad.get_pod_name.side_effect = RuntimeError("nopod")
  c.sandbox_client.create_sandbox.side_effect = None
  c.sandbox_client.create_sandbox.return_value = bad
  with pytest.raises(RuntimeError):
    f.acquire(f.tasks[0])
  bad.terminate.assert_called_once()      # created sandbox cleaned up
  assert c.active_replicas == 0
  assert c.active_claims == 0
  assert f.handles() == []


def test_release_is_idempotent(make_cluster):
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  h = f.acquire(f.tasks[0])
  f.release(h)
  f.release(h)      # double release: remote delete + counter touched once only
  assert h.sandbox.terminate.call_count == 1
  assert c.active_claims == 0
  assert f.handles() == []


def test_start_warmpools_raises_on_pool_timeout(make_cluster):
  from agent_sandbox_rl.exceptions import FleetError
  c = make_cluster("solo")
  c.resources.wait_for_pool_ready.return_value = False   # pool never ready
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img"])
  f.preflight()
  f.plan()
  with pytest.raises(FleetError):
    f.start_warmpools(wait=True)


def test_plan_splits_budget_across_clusters(two_cluster_registry):
  # Global max_concurrent must be split across clusters, not applied per-cluster
  # (else the warm footprint would be max_concurrent x n_clusters).
  f = _fleet(two_cluster_registry, placement="round-robin",
             max_concurrent=8, max_warmpool_size=16)
  f.load_tasks(["imgA"] * 10 + ["imgB"] * 10)   # round-robin → one image per cluster
  plan = f.plan()
  reps = {e.image: e.replicas for e in plan.entries}
  assert reps == {"imgA": 4, "imgB": 4}         # 8 budget / 2 clusters = 4 each
  assert plan.total_replicas == 8               # not 16


def test_acquire_ondemand_reserves_pool_once(make_cluster):
  # Repeated on-demand acquire() of the same image (no plan()) must not grow
  # active_replicas unbounded — the size-1 pool is reserved once and reused.
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]))
  f.load_tasks(["img", "img", "img"])
  for t in f.tasks:
    f.release(f.acquire(t))
  assert c.active_replicas == 1          # reserved once, not 3
  assert c.active_claims == 0
  assert f.handles() == []


def test_plan_budget_no_overshoot_three_clusters(make_cluster):
  # 3 clusters, max_concurrent=8: largest-remainder gives 3+3+2=8 (not round()'s
  # 3+3+3=9). Total warm replicas must not exceed the global budget.
  reg = ClusterRegistry([make_cluster("a"), make_cluster("b"), make_cluster("c")])
  f = _fleet(reg, placement="round-robin", max_concurrent=8, max_warmpool_size=16)
  f.load_tasks(["i1"] * 10 + ["i2"] * 10 + ["i3"] * 10)  # 1 image per cluster
  plan = f.plan()
  assert plan.total_replicas == 8
  assert plan.total_replicas <= 8       # would have been 9 with round()
