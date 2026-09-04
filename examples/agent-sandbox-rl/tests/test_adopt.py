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

"""Adopting warm pools someone else provisioned (issue #1533).

Two halves, matching the two defects: the pool name was hardcoded to
``pool-<template>`` (so a fleet-provisioned pool named ``<template>-pool`` was
invisible), and a miss fell silently through to the on-demand path.
"""

import logging
from unittest.mock import MagicMock

import pytest

from agent_sandbox_rl import (
    ClusterRegistry,
    FleetConfig,
    FleetError,
    PoolNotFoundError,
    SandboxFleet,
    Task,
    constants,
)
from agent_sandbox_rl.preflight import PreflightReport
from agent_sandbox_rl.resources import DiscoveredPool, Resources

IMG_A = "reg.example/bench:a"
IMG_B = "reg.example/bench:b"


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _adopt_fleet(registry, **cfg):
  cfg.setdefault("adopt_existing", True)
  return SandboxFleet(FleetConfig(**cfg), registry=registry)


def _fleet_style(cfg, image):
  """What the multi-cluster fleet layer would call this image's pool."""
  return f"{cfg.template_name(image)}-pool"


def _seed(cluster, cfg, *images, replicas=100):
  """Make `cluster` look like it already has fleet-provisioned pools."""
  cluster.resources.discover_pools.return_value = {
      img: DiscoveredPool(pool=_fleet_style(cfg, img),
                          template=cfg.template_name(img),
                          image=img, replicas=replicas)
      for img in images
  }


# --- 1. the pool name is derivable and configurable ---------------------- #
def test_pool_name_default_is_the_historical_scheme():
  cfg = FleetConfig()
  assert cfg.pool_name(IMG_A) == f"pool-{cfg.template_name(IMG_A)}"
  assert cfg.pool_name(IMG_A) == cfg.pool_name(IMG_A)          # stable
  assert cfg.pool_name(IMG_A) != cfg.pool_name(IMG_B)          # per image


def test_pool_name_format_can_match_the_fleet_layer():
  cfg = FleetConfig(pool_name_format="{template}-pool")
  assert cfg.pool_name(IMG_A) == f"{cfg.template_name(IMG_A)}-pool"


def test_pool_name_format_image_hash_placeholder():
  cfg = FleetConfig(pool_name_format="warm-{image_hash}")
  assert cfg.pool_name(IMG_A) == f"warm-{cfg.image_hash(IMG_A)}"


def test_pool_name_format_without_a_per_image_part_is_rejected():
  # The whole point: a constant name silently collapses every image onto one
  # pool, and the tasks then claim sandboxes running the wrong image.
  with pytest.raises(ValueError, match="must include"):
    FleetConfig(pool_name_format="the-pool")


def test_pool_name_format_unknown_placeholder_is_rejected():
  with pytest.raises(ValueError, match="may only reference"):
    FleetConfig(pool_name_format="{cluster}-{template}")


def test_pool_name_format_must_render_a_valid_object_name():
  with pytest.raises(ValueError, match="DNS-1123"):
    FleetConfig(pool_name_format="Pool_{template}")


def test_pool_name_format_flows_into_plan_and_on_demand(make_cluster):
  c = make_cluster("solo")
  f = SandboxFleet(FleetConfig(pool_name_format="{template}-pool",
                               max_concurrent=2), registry=ClusterRegistry([c]))
  f.load_tasks([IMG_A])
  entry = f.plan().for_image(IMG_A)
  assert entry.pool == f"{f.config.template_name(IMG_A)}-pool"
  assert entry.adopted is False
  # and the acquire()-time on-demand path uses the same scheme
  pool = f._ensure_pool(c, IMG_B, 1)
  assert pool == f"{f.config.template_name(IMG_B)}-pool"


# --- 2. discovery: match by image, not by name --------------------------- #
def _resources_with(pools, templates):
  r = Resources(MagicMock(), MagicMock(), "ns")

  def _list(group=None, version=None, namespace=None, plural=None, **kw):
    return {"items": pools if plural == constants.WARMPOOLS_PLURAL else templates}

  r.custom_api.list_namespaced_custom_object.side_effect = _list
  return r


def _tmpl(name, image, container=constants.RUNTIME_CONTAINER):
  return {"metadata": {"name": name},
          "spec": {"podTemplate": {"spec": {"containers": [
              {"name": container, "image": image}]}}}}


def _pool(name, template, replicas):
  return {"metadata": {"name": name},
          "spec": {"replicas": replicas,
                   "sandboxTemplateRef": {"name": template}}}


def test_discover_pools_finds_a_foreign_naming_scheme():
  r = _resources_with([_pool("r2e-img-aaa-pool", "r2e-img-aaa", 500)],
                      [_tmpl("r2e-img-aaa", IMG_A)])
  found = r.discover_pools()
  assert found == {IMG_A: DiscoveredPool("r2e-img-aaa-pool", "r2e-img-aaa",
                                         IMG_A, 500)}


def test_discover_pools_skips_a_pool_whose_template_is_gone():
  r = _resources_with([_pool("orphan-pool", "missing-template", 3)], [])
  assert r.discover_pools() == {}


def test_discover_pools_reads_the_first_container_when_unnamed():
  # A template written by something else need not use our container name.
  r = _resources_with([_pool("p", "t", 1)], [_tmpl("t", IMG_A, container="main")])
  assert r.discover_pools()[IMG_A].pool == "p"


def test_discover_pools_picks_deterministically_on_duplicates():
  tmpls = [_tmpl("t", IMG_A)]
  deep, shallow = _pool("z-pool", "t", 500), _pool("a-pool", "t", 2)
  assert _resources_with([deep, shallow], tmpls).discover_pools()[IMG_A].pool == "z-pool"
  # order of the listing must not change the answer
  assert _resources_with([shallow, deep], tmpls).discover_pools()[IMG_A].pool == "z-pool"


def test_discover_pools_ties_break_on_name():
  tmpls = [_tmpl("t", IMG_A)]
  pools = [_pool("z-pool", "t", 7), _pool("a-pool", "t", 7)]
  assert _resources_with(pools, tmpls).discover_pools()[IMG_A].pool == "a-pool"


# --- 3. adopt mode: plan against what is standing ------------------------ #
def test_adopt_plans_fleet_named_pools_and_creates_nothing(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]))
  _seed(c, f.config, IMG_A, IMG_B, replicas=250)
  f.load_tasks([IMG_A, IMG_B, IMG_A])
  plan = f.plan()

  assert {e.image: e.pool for e in plan.entries} == {
      IMG_A: _fleet_style(f.config, IMG_A),
      IMG_B: _fleet_style(f.config, IMG_B),
  }
  # replicas is the OBSERVED depth, not something this fleet sized
  assert all(e.replicas == 250 and e.adopted for e in plan.entries)
  c.resources.create_warmpool.assert_not_called()
  c.resources.ensure_template.assert_not_called()


def test_adopt_missing_image_raises_instead_of_provisioning(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]))
  _seed(c, f.config, IMG_A)
  f.load_tasks([IMG_A, IMG_B])
  with pytest.raises(PoolNotFoundError) as e:
    f.plan()
  msg = str(e.value)
  assert IMG_B in msg and IMG_A not in msg          # names only what is missing
  assert f.config.pool_name(IMG_B) in msg           # the name it looked for
  assert "namespace ns" in msg                      # where it looked
  c.resources.create_warmpool.assert_not_called()


def test_adopt_prefers_the_deepest_cluster(make_cluster):
  a, b = make_cluster("a"), make_cluster("b")
  f = _adopt_fleet(ClusterRegistry([a, b]))
  _seed(a, f.config, IMG_A, replicas=10)
  _seed(b, f.config, IMG_A, replicas=900)
  f.load_tasks([IMG_A])
  assert f.plan().for_image(IMG_A).cluster == "b"


def test_adopt_surfaces_a_list_failure(make_cluster):
  c = make_cluster("solo")
  c.resources.discover_pools.side_effect = RuntimeError("403 forbidden")
  f = _adopt_fleet(ClusterRegistry([c]))
  f.load_tasks([IMG_A])
  with pytest.raises(FleetError, match="could not list warm pools"):
    f.plan()


# --- 4. adopt mode: warm, scale and teardown are read-only --------------- #
def test_adopt_waits_for_one_replica_not_the_whole_pool(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]))
  _seed(c, f.config, IMG_A, replicas=5000)
  f.load_tasks([IMG_A])
  f.plan()
  f.start_warmpools(wait=True)

  pool, expected = c.resources.wait_for_pool_ready.call_args[0][:2]
  assert pool == _fleet_style(f.config, IMG_A)
  # not 5000: the provisioner owns the depth and moves it under us
  assert expected == 1
  c.resources.create_warmpool.assert_not_called()
  assert c.active_replicas == 0        # counters track what THIS fleet created


def test_adopt_raises_when_the_pool_is_not_serving(make_cluster):
  c = make_cluster("solo")
  c.resources.wait_for_pool_ready.return_value = False
  f = _adopt_fleet(ClusterRegistry([c]), ready_timeout=1)
  _seed(c, f.config, IMG_A)
  f.load_tasks([IMG_A])
  f.plan()
  with pytest.raises(FleetError, match="no ready replica"):
    f.start_warmpools(wait=True)


def test_adopt_never_scales_or_deletes_the_pool(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]))
  _seed(c, f.config, IMG_A)
  f.load_tasks([IMG_A])
  f.plan()
  f.set_pool_replicas(IMG_A, 3)        # what recycle(scale_on_hold=True) does
  f.unwarm_image(IMG_A)                # what a sliding window does
  c.resources.create_warmpool.assert_not_called()
  c.resources.delete_warmpool.assert_not_called()
  c.resources.delete_template.assert_not_called()


def test_adopt_teardown_keeps_the_pool_but_sweeps_claims(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]))
  _seed(c, f.config, IMG_A)
  f.load_tasks([IMG_A])
  f.plan()
  # the adopted objects DO carry our label here (an earlier run left them), so
  # the managed-selector sweep would otherwise delete the fleet's warm pods
  c.resources.list_warmpools.return_value = [_fleet_style(f.config, IMG_A)]
  c.resources.list_templates.return_value = [f.config.template_name(IMG_A)]
  c.resources.list_claims.return_value = ["claim-1"]

  f.teardown()
  c.resources.delete_warmpool.assert_not_called()
  c.resources.delete_template.assert_not_called()
  c.resources.delete_claim.assert_called_once_with("claim-1")


def test_adopt_teardown_still_removes_unadopted_leftovers(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]))
  _seed(c, f.config, IMG_A)
  f.load_tasks([IMG_A])
  f.plan()
  c.resources.list_warmpools.return_value = [_fleet_style(f.config, IMG_A),
                                             "stale-pool-from-a-crashed-run"]
  f.teardown()
  c.resources.delete_warmpool.assert_called_once_with("stale-pool-from-a-crashed-run")


# --- 5. the silent fallthrough ------------------------------------------- #
def test_adopt_acquire_refuses_an_unplanned_image(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]))
  _seed(c, f.config, IMG_A)
  f.load_tasks([IMG_A])
  f.plan()
  # a task added AFTER planning — plan() itself already rejects an uncovered image
  with pytest.raises(PoolNotFoundError):
    f.acquire(Task(id="t", image=IMG_B))
  c.resources.create_warmpool.assert_not_called()


def test_on_demand_fallthrough_warns_once_per_image(make_cluster, caplog):
  # Default (non-adopt) behaviour is unchanged — but no longer silent.
  c = make_cluster("solo")
  f = SandboxFleet(FleetConfig(), registry=ClusterRegistry([c]))
  with caplog.at_level(logging.WARNING, logger="agent_sandbox_rl.fleet"):
    f.acquire(Task(id="t1", image=IMG_A))
    f.acquire(Task(id="t2", image=IMG_A))
  hits = [r for r in caplog.records if "on-demand size-1 pool" in r.getMessage()]
  assert len(hits) == 1
  assert "pool_name_format" in hits[0].getMessage()
  assert c.resources.create_warmpool.call_count == 2   # behaviour unchanged


def test_adopt_end_to_end_run(make_cluster):
  c = make_cluster("solo")
  f = _adopt_fleet(ClusterRegistry([c]), max_concurrent=4)
  _seed(c, f.config, IMG_A, IMG_B, replicas=64)
  f.load_tasks([IMG_A, IMG_B])
  results = f.run(lambda t, h: t.image, strategy="naive")
  assert sorted(results) == sorted([IMG_A, IMG_B])
  # claimed from the adopted pools, by their real names
  claimed = {kw["warmpool"] for _a, kw in c.sandbox_client.create_sandbox.call_args_list}
  assert claimed == {_fleet_style(f.config, IMG_A), _fleet_style(f.config, IMG_B)}
  c.resources.create_warmpool.assert_not_called()
  c.resources.delete_warmpool.assert_not_called()
