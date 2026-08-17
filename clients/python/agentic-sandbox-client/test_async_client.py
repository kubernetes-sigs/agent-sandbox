# Copyright 2025 The Kubernetes Authors.
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

import argparse
import asyncio
import logging
import time

from k8s_agent_sandbox import AsyncSandboxClient
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
)


logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(levelname)s - %(message)s",
    force=True,
)


CONCURRENCY = 3
SCHEDULING_ALLOWANCE_S = 0.5
TICKER_INTERVAL_S = 0.02
TICKER_GAP_LIMIT_S = 0.5


async def test_concurrent_sandbox_creation(client, warmpool_name, namespace):
    """Concurrent creates must run in parallel, not serialise behind each other.

    Warms the client with one discarded create, then runs ``CONCURRENCY``
    creates in a single ``asyncio.gather``, timing each one individually. The
    warm-up matters: the first call through a fresh client also pays lazy 
    client initialization or connection setup.

    With the client warm, a genuinely non-blocking path finishes in the time of
    the slowest single create plus scheduling overhead. If any step serialises
    (a sync k8s call blocking the loop), wall-clock instead scales with N,
    which for ``CONCURRENCY >= 2`` lands well past that bound.
    """
    print(f"\n--- Testing Concurrent Sandbox Creation (n={CONCURRENCY}) ---")

    async def _create():
        t0 = time.perf_counter()
        sandbox = await client.create_sandbox(
            warmpool=warmpool_name, namespace=namespace
        )
        return sandbox, time.perf_counter() - t0

    warmup, t_warmup = await _create()
    await client.delete_sandbox(warmup.claim_name, namespace=namespace)
    print(f"Warm-up create (discarded): {t_warmup:.2f}s")

    t0 = time.perf_counter()
    results = await asyncio.gather(*[_create() for _ in range(CONCURRENCY)])
    t_concurrent = time.perf_counter() - t0

    sandboxes = [sandbox for sandbox, _ in results]
    durations = [duration for _, duration in results]
    t_slowest = max(durations)
    t_serialised = sum(durations)

    print(
        f"Concurrent ({CONCURRENCY} sandboxes): {t_concurrent:.2f}s "
        f"(slowest single create {t_slowest:.2f}s, "
        f"~{t_serialised:.2f}s if serialised)"
    )

    assert len(sandboxes) == CONCURRENCY, (
        f"Expected {CONCURRENCY} sandboxes, got {len(sandboxes)}"
    )

    limit = t_slowest + SCHEDULING_ALLOWANCE_S
    assert t_concurrent < limit, (
        f"Concurrent creation of {CONCURRENCY} sandboxes took "
        f"{t_concurrent:.2f}s vs {t_slowest:.2f}s for the slowest single "
        f"create — expected less than {limit:.2f}s if parallel."
    )
    print("--- Concurrent Sandbox Creation Test Passed! ---")


async def test_event_loop_not_blocked(client, warmpool_name, namespace):
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


async def run_async_client_tests(connection_config, warmpool_name, namespace):
    """Runs each test with a fresh client so cleanup between tests is total."""
    async with AsyncSandboxClient(
        connection_config=connection_config, cleanup=False
    ) as client:
        await test_concurrent_sandbox_creation(client, warmpool_name, namespace)

    async with AsyncSandboxClient(
        connection_config=connection_config, cleanup=False
    ) as client:
        await test_event_loop_not_blocked(client, warmpool_name, namespace)


async def main(warmpool_name, gateway_name, api_url, namespace, server_port):
    """
    Tests AsyncSandboxClient by exercising concurrent operations and the
    event loop, then cleaning up.
    """
    print(
        f"--- Starting AsyncSandboxClient Test "
        f"(Namespace: {namespace}, Port: {server_port}) ---"
    )
    if gateway_name:
        print(f"Mode: Gateway Discovery ({gateway_name})")
        connection_config = SandboxGatewayConnectionConfig(
            gateway_name=gateway_name,
            server_port=server_port,
        )
    elif api_url:
        print(f"Mode: Direct API URL ({api_url})")
        connection_config = SandboxDirectConnectionConfig(
            api_url=api_url,
            server_port=server_port,
        )
    else:
        # Deliberately not falling back to SandboxLocalTunnelConnectionConfig —
        # AsyncSandboxClient rejects it (see its class docstring), so let the
        # caller pick a supported mode up front rather than fail deep in a run.
        raise SystemExit(
            "AsyncSandboxClient requires --gateway-name or --api-url; "
            "SandboxLocalTunnelConnectionConfig is not supported by the async "
            "client."
        )

    await run_async_client_tests(connection_config, warmpool_name, namespace)
    print("\n--- AsyncSandboxClient Test Finished ---")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Test the async Sandbox client."
    )
    parser.add_argument(
        "--warmpool-name",
        default="python-sandbox-pool",
        help="The name of the sandbox warm pool to use for the test.",
    )
    parser.add_argument(
        "--gateway-name",
        default=None,
        help=(
            "The name of the Gateway resource. Required if --api-url is not "
            "set; the async client does not support the local-tunnel fallback."
        ),
    )
    parser.add_argument(
        "--api-url",
        default=None,
        help="Direct URL to router (e.g. http://localhost:8080).",
    )
    parser.add_argument(
        "--namespace",
        default="default",
        help="Namespace to create sandbox in.",
    )
    parser.add_argument(
        "--server-port",
        type=int,
        default=8888,
        help="Port the sandbox container listens on.",
    )
    args = parser.parse_args()

    asyncio.run(
        main(
            warmpool_name=args.warmpool_name,
            gateway_name=args.gateway_name,
            api_url=args.api_url,
            namespace=args.namespace,
            server_port=args.server_port,
        )
    )
