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

"""E2E coverage for the concurrency guarantees of AsyncSandboxClient.
"""

import asyncio
import time

from test.e2e.clients.python.test_e2e_python_sdk import (
    GATEWAY_NAME,
    deploy_gateway,
    deploy_router,
    sandbox_template,
    tc,
    temp_namespace,
)

import pytest
from k8s_agent_sandbox import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig


CONCURRENCY = 3
SERIAL_FRACTION_LIMIT = 0.6
# Below this serialized-time estimate, jitter dominates the fraction check.
MEASURABLE_SERIALISED_S = 3.0
TICKER_INTERVAL_S = 0.02
TICKER_GAP_LIMIT_S = 0.5
ROUTABLE_PROBE_PATH = "routability-probe"

WARMPOOL_NAME = "async-client-warmpool"


@pytest.fixture(scope="function")
def async_warmpool(tc, temp_namespace, sandbox_template):
    """A warm pool deep enough for the concurrency test to draw from.

    The shared warmpool fixture keeps two replicas, but the concurrency test
    takes ``CONCURRENCY + 1`` sandboxes: one baseline create, then a batch of
    ``CONCURRENCY``. Sizing the pool to that total stops the batch from paying
    cold starts the baseline did not, which the wall-clock ratio would read as
    serialisation.
    """
    manifest = f"""apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: {WARMPOOL_NAME}
spec:
  replicas: {CONCURRENCY + 1}
  sandboxTemplateRef:
    name: {sandbox_template}
"""
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    print("Async warmpool manifest applied.")

    tc.wait_for_warmpool_ready(WARMPOOL_NAME, namespace=temp_namespace)
    print("Async warmpool is ready.")
    return WARMPOOL_NAME


def _run_with_client(scenario, warmpool_name, namespace):

    async def _main():
        # The async client rejects the local-tunnel config (see its class
        # docstring), so the gateway mode the sync suite uses is the one under
        # test here.
        config = SandboxGatewayConnectionConfig(
            gateway_name=GATEWAY_NAME,
            gateway_namespace=namespace,
        )
        async with AsyncSandboxClient(
            connection_config=config, cleanup=True
        ) as client:
            await scenario(client, warmpool_name, namespace)

    asyncio.run(_main())


async def _assert_concurrent_creates_overlap(client, warmpool_name, namespace):
    """Concurrent creates must run in parallel, not serialise behind each other.

    Two complementary signals, because neither alone covers both failure
    modes at every speed:

    - In-flight overlap: each create records its ``(start, end)`` interval,
      and ``max(starts) < min(ends)`` requires a moment when every create was
      simultaneously in flight. This catches event-loop serialisation (sync
      IO blocking the loop) with no wall-clock threshold, so it stays
      conclusive at warm-pool speeds where timing jitter drowns any ratio.
    - Serialised-fraction wall clock: overlap alone cannot see the backend
      processing claims one at a time while all requests wait in flight, so
      the concurrent wall clock is also compared against ``CONCURRENCY``
      times a solo-create baseline — but only when that serialised estimate
      is large enough to measure (a ~0.2s warm-adopt baseline puts the
      fraction limit at ~0.36s, inside normal jitter; a presubmit flaked at
      0.39s).

    The per-create durations cannot serve as the baseline. ``gather`` starts
    every coroutine at the same instant, so each duration runs from that shared
    t0 and already contains whatever queueing that create waited through: their
    max is ``t_concurrent`` by construction, and their sum over-counts the
    shared wait rather than projecting a serial run.
    """
    print(f"\n--- Testing Concurrent Sandbox Creation (n={CONCURRENCY}) ---")

    async def _create():
        start = time.perf_counter()
        sandbox = await client.create_sandbox(
            warmpool=warmpool_name, namespace=namespace
        )
        return sandbox, start, time.perf_counter()

    baseline, baseline_start, baseline_end = await _create()
    t_baseline = baseline_end - baseline_start
    await client.delete_sandbox(baseline.claim_name, namespace=namespace)
    print(f"Baseline single create (discarded): {t_baseline:.2f}s")

    t0 = time.perf_counter()
    results = await asyncio.gather(*[_create() for _ in range(CONCURRENCY)])
    t_concurrent = time.perf_counter() - t0

    sandboxes = [sandbox for sandbox, _, _ in results]
    try:
        starts = [start - t0 for _, start, _ in results]
        ends = [end - t0 for _, _, end in results]
        t_serialised = t_baseline * CONCURRENCY

        print(
            f"Concurrent ({CONCURRENCY} sandboxes): {t_concurrent:.2f}s "
            f"(completions at {', '.join(f'{end:.2f}s' for end in sorted(ends))}; "
            f"~{t_serialised:.2f}s if serialised)"
        )

        assert len(sandboxes) == CONCURRENCY, (
            f"Expected {CONCURRENCY} sandboxes, got {len(sandboxes)}"
        )

        assert max(starts) < min(ends), (
            f"Creates never overlapped: the last one started at "
            f"{max(starts):.2f}s, after the first finished at "
            f"{min(ends):.2f}s — the event loop is serialising them."
        )

        if t_serialised >= MEASURABLE_SERIALISED_S:
            limit = t_serialised * SERIAL_FRACTION_LIMIT
            assert t_concurrent < limit, (
                f"Concurrent creation of {CONCURRENCY} sandboxes took "
                f"{t_concurrent:.2f}s against a {t_baseline:.2f}s single "
                f"create (~{t_serialised:.2f}s if fully serialised) — "
                f"expected less than {limit:.2f}s if parallel."
            )
        print("--- Concurrent Sandbox Creation Test Passed! ---")
    finally:
        await asyncio.gather(
            *[
                client.delete_sandbox(sandbox.claim_name, namespace=namespace)
                for sandbox in sandboxes
            ]
        )


async def _assert_event_loop_not_blocked(client, warmpool_name, namespace):
    """The async client must not block the event loop during any operation.

    Runs a fine-grained ticker throughout a create + command run and checks
    that no single scheduling gap grows past ``TICKER_GAP_LIMIT_S``. This
    catches sync IO in the async path regardless of how long the underlying
    operation legitimately takes — a blocking call produces one huge gap
    whether the create takes 2 seconds or 30.
    """
    print("\n--- Testing Event Loop Not Blocked ---")

    stop = asyncio.Event()
    ticker_ready = asyncio.Event()
    gaps: list[float] = []

    async def ticker():
        last = time.perf_counter()
        ticker_ready.set()
        while not stop.is_set():
            await asyncio.sleep(TICKER_INTERVAL_S)
            now = time.perf_counter()
            gaps.append(now - last)
            last = now

    ticker_task = asyncio.create_task(ticker())
    await ticker_ready.wait()
    try:
        sandbox = await client.create_sandbox(
            warmpool=warmpool_name, namespace=namespace
        )
        await sandbox.files.exists(ROUTABLE_PROBE_PATH)
        result = await sandbox.commands.run("echo hello")
        assert result.exit_code == 0, (
            f"Command failed with exit code {result.exit_code}: {result.stderr}"
        )
    finally:
        stop.set()
        await ticker_task

    assert gaps, "Ticker never fired — test is invalid."
    max_gap = max(gaps)
    print(
        f"Ticker samples: {len(gaps)}, max gap: {max_gap * 1000:.1f}ms "
        f"(threshold {TICKER_GAP_LIMIT_S * 1000:.0f}ms)"
    )

    assert max_gap < TICKER_GAP_LIMIT_S, (
        f"Event loop blocked for {max_gap * 1000:.1f}ms — expected less than "
        f"{TICKER_GAP_LIMIT_S * 1000:.0f}ms."
    )
    print("--- Event Loop Not Blocked Test Passed! ---")


def test_async_client_concurrent_sandbox_creation(
        temp_namespace, async_warmpool):
    """AsyncSandboxClient creates sandboxes in parallel, not one at a time."""
    _run_with_client(
        _assert_concurrent_creates_overlap, async_warmpool, temp_namespace
    )


def test_async_client_event_loop_not_blocked(
    temp_namespace, deploy_router, deploy_gateway, async_warmpool
):
    """AsyncSandboxClient keeps the event loop responsive, data path included."""
    _run_with_client(
        _assert_event_loop_not_blocked, async_warmpool, temp_namespace
    )
