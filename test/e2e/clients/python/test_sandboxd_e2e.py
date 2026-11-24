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

"""E2E tests for the Python SDK against a real sandboxd runtime.

What needs a cluster here is the Pod tunnel that
SandboxdPodTunnelConnectionConfig opens: whether REST and gRPC calls reach
the same running Pod, whether a long-lived stream and a multi-megabyte body
survive the port-forward, and whether a fresh client can find a live Pod and
tunnel to it again. sandboxd's own behavior has Go tests in
packages/sandboxd, and the SDK's request building and response parsing have
unit tests in clients/python, so neither is re-asserted here.

Each client therefore gets one journey through the tunnel, plus one cold
start, because the sync and async readiness waits are separate code.
"""

import asyncio
import contextlib
import hashlib
import io
import os
import signal as sig

from k8s_agent_sandbox import AsyncSandboxClient, SandboxClient
from k8s_agent_sandbox.models import (
    ProcessExitEvent,
    ProcessOutputEvent,
    SandboxdPodTunnelConnectionConfig,
)


@contextlib.contextmanager
def sandboxd_sandbox(warmpool, namespace):
    """Yield a client and a sandboxd sandbox, and delete the sandbox on exit."""
    client = SandboxClient(connection_config=SandboxdPodTunnelConnectionConfig())
    try:
        yield client, client.create_sandbox(warmpool=warmpool, namespace=namespace)
    finally:
        client.delete_all()


def assert_round_trip(sandbox, path, size):
    """Stream `size` random bytes to `path` and back, then compare."""
    data = os.urandom(size)
    # Pass a file object rather than bytes so the SDK uploads in chunks, which
    # is what exercises the tunnel.
    sandbox.files.write(path, io.BytesIO(data), timeout=120)

    buffer = io.BytesIO()
    written = sandbox.files.read_to(path, buffer, timeout=120)
    # Judge correctness by content hash and sandboxd's own size accounting,
    # never by elapsed time.
    assert written == size
    assert hashlib.sha256(buffer.getvalue()).hexdigest() == (
        hashlib.sha256(data).hexdigest()
    )
    assert sandbox.files.stat(path).size == size


def assert_streaming_exec_ends_on_a_signal(sandbox):
    """Read a process's output as it streams, then stop it with a signal.

    Unary calls cannot show that the tunnel holds a server stream open, or
    that a signal sent on the same channel closes one that would otherwise
    never finish.
    """
    with sandbox.commands.start("printf ready; sleep 300") as handle:
        events = handle.events()

        first = next(events)
        assert isinstance(first, ProcessOutputEvent)
        assert first.type == "stdout"
        assert first.data == b"ready"

        handle.signal(sig.SIGTERM)
        # Draining the iterator is the assertion: the stream has to end on
        # its own after the signal, and a hang here is the failure this
        # covers.
        assert [type(event) for event in events] == [ProcessExitEvent]


def assert_reattach(sandbox, namespace):
    """Check that a fresh client finds the live Pod and tunnels to it again."""
    sandbox.files.write("reattach.txt", "written before re-attach\n")
    pod_name = sandbox.get_pod_name()

    # close_connection() drops only the local tunnel. The SandboxClaim and
    # its Pod keep running, as when an agent process restarts while its
    # sandbox stays up.
    sandbox.close_connection()

    fresh_client = SandboxClient(connection_config=SandboxdPodTunnelConnectionConfig())
    try:
        reattached = fresh_client.get_sandbox(sandbox.claim_name, namespace=namespace)
        assert reattached.sandbox_id == sandbox.sandbox_id
        assert reattached.get_pod_name() == pod_name
        assert reattached.files.read("reattach.txt") == b"written before re-attach\n"
        assert reattached.commands.run("echo 'reattached ok'").exit_code == 0
    finally:
        fresh_client.delete_all()


def test_sandboxd_journey(temp_namespace, sandboxd_warmpool):
    """One pass through everything the Pod tunnel has to carry."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        # One unary gRPC call, to show the channel carries a call and its
        # result. Exit codes and stream splitting belong to sandboxd.
        result = sandbox.commands.run("printf out; printf err >&2")
        assert result.exit_code == 0
        assert result.stdout == "out"
        assert result.stderr == "err"

        # REST and gRPC are different ports on the same Pod, so write through
        # one and read back through the other.
        sandbox.files.write("via-rest.txt", "written via REST\n")
        cat = sandbox.commands.run("cat via-rest.txt")
        assert cat.exit_code == 0
        assert cat.stdout == "written via REST\n"

        assert_streaming_exec_ends_on_a_signal(sandbox)
        assert_round_trip(sandbox, "stream-roundtrip.bin", 4 * 1024 * 1024)
        assert_reattach(sandbox, temp_namespace)


def test_sandboxd_cold_start(temp_namespace, sandboxd_coldpool):
    """A claim with no pre-warmed slot becomes usable.

    This is the only scenario that needs the pool at zero replicas. The
    client waits out Pod scheduling and the readiness probe instead of
    adopting a slot that is already running.
    """
    with sandboxd_sandbox(sandboxd_coldpool, temp_namespace) as (_, sandbox):
        result = sandbox.commands.run("echo 'cold start ok'")
        assert result.exit_code == 0
        assert result.stdout == "cold start ok\n"


# AsyncSandboxClient has its own connector, port-forward handling, readiness
# wait and get_sandbox(), so the tests above do not cover any of it.


class _AsyncBuffer:
    """Collect bytes from the async read_to(), which awaits each write."""

    def __init__(self):
        self.buffer = io.BytesIO()

    async def write(self, content):
        return self.buffer.write(content)


def async_sandboxd_client():
    return AsyncSandboxClient(connection_config=SandboxdPodTunnelConnectionConfig())


async def assert_async_round_trip(sandbox, path, size):
    """Async counterpart of assert_round_trip."""
    data = os.urandom(size)
    await sandbox.files.write(path, io.BytesIO(data), timeout=120)

    destination = _AsyncBuffer()
    written = await sandbox.files.read_to(path, destination, timeout=120)
    assert written == size
    assert hashlib.sha256(destination.buffer.getvalue()).hexdigest() == (
        hashlib.sha256(data).hexdigest()
    )
    assert (await sandbox.files.stat(path)).size == size


async def assert_async_streaming_exec_ends_on_a_signal(sandbox):
    """Async counterpart of assert_streaming_exec_ends_on_a_signal."""
    async with await sandbox.commands.start("printf ready; sleep 300") as handle:
        events = handle.events()

        first = await anext(events)
        assert isinstance(first, ProcessOutputEvent)
        assert first.type == "stdout"
        assert first.data == b"ready"

        await handle.signal(sig.SIGTERM)
        assert [type(event) async for event in events] == [ProcessExitEvent]


async def assert_async_reattach(sandbox, namespace):
    """Async counterpart of assert_reattach."""
    await sandbox.files.write("reattach.txt", "written before re-attach\n")
    pod_name = await sandbox.get_pod_name()
    await sandbox.close_connection()

    async with async_sandboxd_client() as fresh_client:
        reattached = await fresh_client.get_sandbox(
            sandbox.claim_name, namespace=namespace
        )
        assert reattached.sandbox_id == sandbox.sandbox_id
        assert await reattached.get_pod_name() == pod_name
        assert await reattached.files.read("reattach.txt") == (
            b"written before re-attach\n"
        )
        result = await reattached.commands.run("echo 'reattached ok'")
        assert result.exit_code == 0


def test_sandboxd_async_journey(temp_namespace, sandboxd_warmpool):
    """The async client's own pass through the Pod tunnel."""

    async def scenario():
        async with async_sandboxd_client() as client:
            sandbox = await client.create_sandbox(
                warmpool=sandboxd_warmpool, namespace=temp_namespace
            )

            result = await sandbox.commands.run("printf out; printf err >&2")
            assert result.exit_code == 0
            assert result.stdout == "out"
            assert result.stderr == "err"

            await sandbox.files.write("via-rest.txt", "written via REST\n")
            cat = await sandbox.commands.run("cat via-rest.txt")
            assert cat.exit_code == 0
            assert cat.stdout == "written via REST\n"

            await assert_async_streaming_exec_ends_on_a_signal(sandbox)
            await assert_async_round_trip(
                sandbox, "async-roundtrip.bin", 4 * 1024 * 1024
            )
            await assert_async_reattach(sandbox, temp_namespace)

    asyncio.run(scenario())


def test_sandboxd_async_cold_start(temp_namespace, sandboxd_coldpool):
    """The async readiness wait brings up a sandbox with no warm slot."""

    async def scenario():
        async with async_sandboxd_client() as client:
            sandbox = await client.create_sandbox(
                warmpool=sandboxd_coldpool, namespace=temp_namespace
            )
            result = await sandbox.commands.run("echo 'cold start ok'")
            assert result.exit_code == 0
            assert result.stdout == "cold start ok\n"

    asyncio.run(scenario())
