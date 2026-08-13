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

"""`recycle=` flag on fleet.run / async_fleet.run — an orthogonal modifier that
swaps the task→sandbox binding (reset-and-reuse) while the chosen *strategy* still
governs warming. These tests verify the routing (executor injection), opt
plumbing, and that recycle composes with every strategy — the reset/quarantine
mechanics themselves live in test_recycle.py."""

import unittest.mock as m

import pytest

import agent_sandbox_rl.recycle as recycle_mod
from agent_sandbox_rl import (AsyncSandboxFleet, ClusterRegistry, FleetConfig,
                              SandboxFleet)
from agent_sandbox_rl.handles import SandboxHandle
from agent_sandbox_rl.preflight import PreflightReport

_CLEAN = "PRISTINE=abc\nHEAD=abc\nDIRTY=0\nENV=e1\nCFG=g1\nPROCS=5"


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _fleet(registry, **cfg):
  return SandboxFleet(FleetConfig(**cfg), registry=registry)


def _patch_clean_exec(monkeypatch):
  monkeypatch.setattr(SandboxHandle, "exec", lambda self, cmd: _CLEAN)


def _spy_reuse(monkeypatch):
  """Replace the sync recycle executor with a spy that returns one result per
  task (so run() bookkeeping stays happy) and records how it was called."""
  spy = m.MagicMock(side_effect=lambda fleet, tasks, pf, conc, **kw: [pf(t, None) for t in tasks])
  monkeypatch.setattr(recycle_mod, "reuse_git_restore_sandbox", spy)
  return spy


def _spy_reuse_async(monkeypatch):
  async def _fake(afleet, tasks, pf, conc, **kw):
    return [await afleet._call(pf, t, None) for t in tasks]
  spy = m.MagicMock(side_effect=_fake)
  monkeypatch.setattr(recycle_mod, "reuse_git_restore_sandbox_async", spy)
  return spy


# --- routing: recycle flag selects the executor --------------------------- #
def test_run_recycle_true_routes_to_reuse_executor(make_cluster, monkeypatch):
  spy = _spy_reuse(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["img", "img", "img"])
  res = f.run(lambda t, h: "ok", strategy="naive", recycle=True)
  assert res == ["ok", "ok", "ok"]
  assert spy.call_count == 1                    # naive → one executor call over all tasks


def test_run_recycle_false_does_not_call_reuse(make_cluster, monkeypatch):
  spy = _spy_reuse(monkeypatch)
  _patch_clean_exec(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["img", "img", "img"])
  f.run(lambda t, h: "ok", strategy="naive", recycle=False)
  spy.assert_not_called()                        # default path = process_parallel


def test_run_recycle_forwards_opts(make_cluster, monkeypatch):
  spy = _spy_reuse(monkeypatch)
  reset = recycle_mod.GitRestoreReset(testbed="/repo")
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=2)
  f.load_tasks(["img", "img"])
  f.run(lambda t, h: 1, strategy="naive", recycle=True, reset=reset,
        max_reuses=7, reset_timeout=9.0, use_session=False, scale_on_hold=False)
  _fleet_arg, _tasks, _pf, _conc = spy.call_args.args
  kw = spy.call_args.kwargs
  assert kw["reset"] is reset and kw["max_reuses"] == 7
  assert kw["reset_timeout"] == 9.0 and kw["use_session"] is False
  assert kw["scale_on_hold"] is False


def test_run_recycle_composes_with_windowed_strategy(make_cluster, monkeypatch):
  # recycle must layer under a warming strategy, not only naive: the executor is
  # invoked once per window (here 'none' = window of 1 image).
  spy = _spy_reuse(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=2)
  f.load_tasks(["a", "a", "b", "b"])             # 2 images
  f.run(lambda t, h: t.image, strategy="none", recycle=True)
  assert spy.call_count == 2                      # one executor call per window/image


# --- end-to-end claim economics (real executor, fake exec) ---------------- #
def test_run_recycle_reuses_one_claim_per_image(make_cluster, monkeypatch):
  _patch_clean_exec(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["img"] * 4)                       # 1 image, 4 tasks
  res = f.run(lambda t, h: h.pod_name, strategy="naive", recycle=True,
              use_session=False)
  assert len(res) == 4 and all(isinstance(x, str) for x in res)
  assert c.sandbox_client.create_sandbox.call_count == 1   # reused across all 4 (÷G)


def test_run_without_recycle_claims_per_task(make_cluster, monkeypatch):
  # contrast: the default path re-claims, so it creates strictly more sandboxes
  # than the recycled run for the same multi-task-per-image workload.
  _patch_clean_exec(monkeypatch)
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["img"] * 4)
  f.run(lambda t, h: h.pod_name, strategy="naive", recycle=False)
  assert c.sandbox_client.create_sandbox.call_count > 1


# --- async twin ----------------------------------------------------------- #
async def test_async_run_recycle_true_routes_to_async_reuse(make_cluster, monkeypatch):
  spy = _spy_reuse_async(monkeypatch)
  c = make_cluster("solo")
  af = AsyncSandboxFleet(FleetConfig(max_concurrent=4), registry=ClusterRegistry([c]))
  af.load_tasks(["img", "img", "img"])
  res = await af.run(lambda t, h: "ok", strategy="naive", recycle=True)
  assert res == ["ok", "ok", "ok"]
  assert spy.call_count == 1
  af.close()


async def test_async_run_recycle_forwards_async_opts(make_cluster, monkeypatch):
  spy = _spy_reuse_async(monkeypatch)
  c = make_cluster("solo")
  af = AsyncSandboxFleet(FleetConfig(max_concurrent=4), registry=ClusterRegistry([c]))
  af.load_tasks(["img", "img"])
  await af.run(lambda t, h: 1, strategy="naive", recycle=True,
               max_reuses=5, shards_per_image=3, claim_concurrency=2)
  kw = spy.call_args.kwargs
  assert kw["max_reuses"] == 5 and kw["shards_per_image"] == 3
  assert kw["claim_concurrency"] == 2
  af.close()


async def test_async_run_recycle_false_does_not_call_reuse(make_cluster, monkeypatch):
  spy = _spy_reuse_async(monkeypatch)
  _patch_clean_exec(monkeypatch)
  c = make_cluster("solo")
  af = AsyncSandboxFleet(FleetConfig(max_concurrent=4), registry=ClusterRegistry([c]))
  af.load_tasks(["img", "img"])
  await af.run(lambda t, h: "ok", strategy="naive", recycle=False)
  spy.assert_not_called()
  af.close()


async def test_async_run_recycle_reuses_one_claim_per_image(make_cluster, monkeypatch):
  _patch_clean_exec(monkeypatch)
  c = make_cluster("solo")
  af = AsyncSandboxFleet(FleetConfig(max_concurrent=4), registry=ClusterRegistry([c]))
  af.load_tasks(["img"] * 4)
  res = await af.run(lambda t, h: h.pod_name, strategy="naive", recycle=True,
                     use_session=False)
  assert len(res) == 4 and all(isinstance(x, str) for x in res)
  assert c.sandbox_client.create_sandbox.call_count == 1
  await af.teardown()
  af.close()
