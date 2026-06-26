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

import asyncio
import time

import pytest

from agent_sandbox_rl import AsyncSandboxFleet, FleetConfig
from agent_sandbox_rl.preflight import PreflightReport


@pytest.fixture(autouse=True)
def _stub_preflight(monkeypatch):
  def ok(cluster, **kw):
    r = PreflightReport(cluster.name)
    r.add("stub", True)
    return r
  monkeypatch.setattr("agent_sandbox_rl.preflight.preflight_cluster", ok)


def _fleet(registry, **cfg):
  return AsyncSandboxFleet(FleetConfig(**cfg), registry=registry)


def test_async_thread_pool_scales_with_concurrency(make_cluster):
  # Must NOT use asyncio's default ~16-thread pool: pipelined overlaps
  # prefetch+process+unwarm and wait_for_pool_ready holds a thread, so a
  # CPU-count-sized pool starves teardown and deadlocks. Pool must scale with
  # max_concurrent (covers ~process + 2 windows).
  from agent_sandbox_rl import ClusterRegistry
  f = _fleet(ClusterRegistry([make_cluster("solo")]), max_concurrent=40)
  pool = f._thread_pool()
  try:
    assert pool._max_workers >= 2 * 40       # scales with concurrency
    assert pool._max_workers > 32            # well above asyncio's default ~16
  finally:
    f.close()


async def test_async_naive_sync_processfn(two_cluster_registry):
  f = _fleet(two_cluster_registry, placement="round-robin")
  f.load_tasks(["imgA", "imgB", "imgA"])
  res = await f.run(lambda t, h: t.image.upper(), strategy="naive")
  assert sorted(res) == ["IMGA", "IMGA", "IMGB"]
  assert f.handles() == []


async def test_async_naive_async_processfn(two_cluster_registry):
  f = _fleet(two_cluster_registry)
  f.load_tasks(["imgA", "imgB"])

  async def pf(task, handle):
    await asyncio.sleep(0)
    return handle.cluster_name

  res = await f.run(pf, strategy="naive")
  assert len(res) == 2
  assert f.handles() == []


async def test_async_call_awaits_returned_awaitable(two_cluster_registry):
  # A callable that is NOT a coroutine function but RETURNS an awaitable (e.g.
  # functools.partial of an async fn, or a sync fn returning a coroutine) must
  # still be awaited — not handed back as an un-awaited coroutine.
  f = _fleet(two_cluster_registry)
  f.load_tasks(["imgA", "imgB"])

  async def inner(task, handle):
    await asyncio.sleep(0)
    return handle.cluster_name

  def returns_coro(task, handle):       # sync callable, returns a coroutine
    return inner(task, handle)

  res = await f.run(returns_coro, strategy="naive")
  assert len(res) == 2
  assert all(isinstance(r, str) for r in res)   # awaited (not coroutine objects)
  assert f.handles() == []


async def test_async_epochs_reuse_pools(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["i1", "i2"])
  res = await f.run(lambda t, h: t.image, strategy="naive", epochs=2)
  assert len(res) == 2 and all(sorted(r) == ["i1", "i2"] for r in res)
  assert c.resources.create_warmpool.call_count == 2   # reused across epochs
  assert f.handles() == []
  assert c.active_replicas == 0


async def test_async_pipelined_order_and_peak(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), placement="image-affinity",
             max_concurrent=1, window_size=1)
  f.load_tasks(["imgB", "imgA", "imgB", "imgC"])

  async def pf(task, handle):
    await asyncio.sleep(0.03)               # let the next window's prefetch overlap
    return task.image

  res = await f.run(pf, strategy="pipelined")
  assert res == ["imgB", "imgA", "imgB", "imgC"]   # original task order preserved
  assert 1 <= f.report.peak_warm <= 2              # never >2 windows resident
  assert f.handles() == []


async def test_async_pipelined_teardown_on_prefetch_failure(make_cluster):
  from agent_sandbox_rl import ClusterRegistry, FleetError
  c = make_cluster("solo")
  c.resources.wait_for_pool_ready.side_effect = [True, False, False, False]
  f = _fleet(ClusterRegistry([c]), placement="image-affinity",
             max_concurrent=1, window_size=1)
  f.load_tasks(["i1", "i2", "i3"])
  with pytest.raises(FleetError):
    await f.run(lambda t, h: t.id, strategy="pipelined")
  assert f.handles() == []
  assert c.active_claims == 0 and c.active_replicas == 0


async def test_async_concurrency_overlaps(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=4)
  f.load_tasks(["img"] * 4)

  async def pf(task, handle):
    await asyncio.sleep(0.1)

  start = time.monotonic()
  await f.run(pf, strategy="naive", concurrency=4)
  assert time.monotonic() - start < 0.35      # ~0.1s, not ~0.4s


async def test_async_sliding_and_none(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), window_size=1, max_concurrent=2)
  # interleaved image order so a grouped-by-image regression would reorder results
  f.load_tasks(["iB", "iA", "iB"])
  res = await f.run(lambda t, h: t.image, strategy="sliding")
  assert res == ["iB", "iA", "iB"]            # exact original task order
  assert f.handles() == []

  c2 = make_cluster("solo2")
  f2 = _fleet(ClusterRegistry([c2]), max_concurrent=4, max_warmpool_size=8)
  f2.load_tasks(["i1", "i2"])
  await f2.run(lambda t, h: t.id, strategy="none")
  for call in c2.resources.create_warmpool.call_args_list:
    assert call.args[2] == 1                   # none forces replicas=1


async def test_async_context_manager(two_cluster_registry):
  f = _fleet(two_cluster_registry)
  f.load_tasks(["imgA"])
  async with f:                                # setup() on enter, teardown() on exit
    hs = await f.acquire_batch(f.tasks)
    assert len(hs) == 1 and f.hostnames()
  assert f.handles() == []


async def test_async_per_task_error_captured(make_cluster):
  from agent_sandbox_rl import ClusterRegistry
  c = make_cluster("solo")
  f = _fleet(ClusterRegistry([c]), max_concurrent=2)
  f.load_tasks(["a", "b", "c"])

  def pf(t, h):
    if t.image == "b":
      raise RuntimeError("boom")
    return t.image

  res = await f.run(pf, strategy="naive", concurrency=2)
  assert res[0] == "a" and res[2] == "c"
  assert isinstance(res[1], RuntimeError)
  assert f.handles() == []
