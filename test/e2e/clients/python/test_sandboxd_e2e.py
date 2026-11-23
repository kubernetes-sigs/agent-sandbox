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

These tests reach sandboxd through SandboxdPodTunnelConnectionConfig and
mirror the sandboxd scenarios in
test/e2e/clients/typescript/test-e2e-typescript-sdk.test.ts, so both SDKs are
checked against the same runtime behavior.
"""

import asyncio
import contextlib
import hashlib
import io
import os

from k8s_agent_sandbox import AsyncSandboxClient, SandboxClient
from k8s_agent_sandbox.models import SandboxdPodTunnelConnectionConfig


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
    # Pass a file object rather than bytes so the SDK uploads in chunks,
    # matching the TypeScript suite's writeStream round trip.
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


def test_sandboxd_commands(temp_namespace, sandboxd_warmpool):
    """run() reports output and exit codes from real sandboxd processes."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        # run() takes a shell string, so shell expansion applies.
        ok = sandbox.commands.run("echo 'hello from' \"$((40 + 2))\"")
        assert ok.exit_code == 0
        assert ok.stdout == "hello from 42\n"
        assert ok.stderr == ""

        both = sandbox.commands.run("printf out; printf err >&2")
        assert both.exit_code == 0
        assert both.stdout == "out"
        assert both.stderr == "err"

        assert sandbox.commands.run("exit 3").exit_code == 3

        # run() wraps every command in `sh -c`, so a missing executable is
        # the shell's exit 127 rather than a sandboxd NOT_FOUND error. The
        # TypeScript SDK, which passes argv directly, gets NOT_FOUND instead.
        missing = sandbox.commands.run("definitely-not-a-real-command-e2e")
        assert missing.exit_code == 127


def test_sandboxd_commands_env_and_cwd(temp_namespace, sandboxd_warmpool):
    """run() passes env and cwd through to the sandboxd process."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        with_env = sandbox.commands.run(
            'printf %s "$E2E_GREETING"', env={"E2E_GREETING": "hello env"}
        )
        assert with_env.exit_code == 0
        assert with_env.stdout == "hello env"

        sandbox.files.write("cwd-dir/in-cwd.txt", "found via cwd\n")
        with_cwd = sandbox.commands.run("cat in-cwd.txt", cwd="cwd-dir")
        assert with_cwd.exit_code == 0
        assert with_cwd.stdout == "found via cwd\n"


def test_sandboxd_file_operations(temp_namespace, sandboxd_warmpool):
    """The filesystem API covers the sandboxd file operations end to end."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        files = sandbox.files
        files.write("greeting.txt", "hello from the Python SDK e2e\n")
        assert files.read("greeting.txt") == b"hello from the Python SDK e2e\n"
        assert files.exists("greeting.txt")
        assert not files.exists("does-not-exist.txt")

        entry = files.stat("greeting.txt")
        assert entry.name == "greeting.txt"
        assert entry.type == "file"
        assert entry.size == len(b"hello from the Python SDK e2e\n")

        files.mkdir("nested/dir", parents=True)
        assert files.stat("nested/dir").type == "directory"

        files.copy("greeting.txt", "nested/dir/copy.txt")
        files.move("greeting.txt", "nested/moved.txt")
        assert not files.exists("greeting.txt")
        assert files.read("nested/dir/copy.txt") == files.read("nested/moved.txt")

        names = {e.name for e in files.list("nested")}
        assert names == {"dir", "moved.txt"}

        files.delete("nested", recursive=True)
        assert not files.exists("nested")


def test_sandboxd_health_and_metadata(temp_namespace, sandboxd_warmpool):
    """health() and metadata() return sandboxd's probe and filtered env."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        health = sandbox.health()
        assert health.status == "ok"
        assert health.uptime_seconds >= 0

        env = sandbox.metadata().env
        # Not an exact match: Kubernetes injects <SERVICE>_SERVICE_HOST-style
        # variables, and a Service named "sandbox-..." also carries the
        # SANDBOX_ prefix.
        assert env.get("SANDBOX_E2E_MARKER") == "python-sdk"
        assert "SANDBOX_E2E_API_TOKEN" not in env
        assert "E2E_UNPREFIXED" not in env

        # The withheld variables are set in sandboxd's environment, so their
        # absence above comes from the filter and not from a missing fixture.
        token = sandbox.commands.run('printf %s "$SANDBOX_E2E_API_TOKEN"')
        assert token.stdout == "dummy-not-a-credential"
        unprefixed = sandbox.commands.run('printf %s "$E2E_UNPREFIXED"')
        assert unprefixed.stdout == "not-exposed"


def test_sandboxd_files_and_commands_share_working_directory(
    temp_namespace, sandboxd_warmpool
):
    """The REST files API and the gRPC process API see the same directory."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        sandbox.files.write("via-rest.txt", "written via REST\n")
        cat = sandbox.commands.run("cat via-rest.txt")
        assert cat.exit_code == 0
        assert cat.stdout == "written via REST\n"

        echo = sandbox.commands.run("printf 'written via gRPC' > via-grpc.txt")
        assert echo.exit_code == 0
        assert sandbox.files.read("via-grpc.txt") == b"written via gRPC"


def test_sandboxd_get_sandbox_reattaches(temp_namespace, sandboxd_warmpool):
    """A fresh SandboxClient re-attaches to a live sandboxd sandbox."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        sandbox.files.write("reattach.txt", "written before re-attach\n")
        pod_name = sandbox.get_pod_name()

        # close_connection() drops only the local connection. The
        # SandboxClaim and its Pod keep running, as when an agent process
        # restarts while its sandbox stays up.
        sandbox.close_connection()

        fresh_client = SandboxClient(
            connection_config=SandboxdPodTunnelConnectionConfig()
        )
        try:
            reattached = fresh_client.get_sandbox(
                sandbox.claim_name, namespace=temp_namespace
            )
            assert reattached.claim_name == sandbox.claim_name
            assert reattached.sandbox_id == sandbox.sandbox_id
            assert reattached.get_pod_name() == pod_name
            assert reattached.files.read("reattach.txt") == (
                b"written before re-attach\n"
            )
            result = reattached.commands.run("echo 'reattached ok'")
            assert result.exit_code == 0
            assert result.stdout == "reattached ok\n"
        finally:
            fresh_client.delete_all()


def test_sandboxd_large_file_round_trip(temp_namespace, sandboxd_warmpool):
    """A multi-megabyte file survives a streamed upload and download."""
    with sandboxd_sandbox(sandboxd_warmpool, temp_namespace) as (_, sandbox):
        assert_round_trip(sandbox, "stream-roundtrip.bin", 4 * 1024 * 1024)


def test_sandboxd_cold_pool(temp_namespace, sandboxd_coldpool):
    """A sandbox provisioned without a pre-warmed slot serves runtime calls."""
    with sandboxd_sandbox(sandboxd_coldpool, temp_namespace) as (_, sandbox):
        result = sandbox.commands.run("echo 'cold start ok'")
        assert result.exit_code == 0
        assert result.stdout == "cold start ok\n"
        # Smaller than the warm-pool round trip: this checks that streaming
        # works after cold connection setup, not large-payload throughput.
        assert_round_trip(sandbox, "cold-stream-roundtrip.bin", 2 * 1024 * 1024)


# The async SDK has its own sandboxd connector, port-forward handling, and
# get_sandbox() implementation, so the sync tests above do not cover them.
# Sandboxd's per-feature behavior does not depend on the client, and the
# async unit tests cover the request building, so these tests exercise only
# the async-specific paths.


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


def test_sandboxd_async_client(temp_namespace, sandboxd_warmpool):
    """AsyncSandboxClient reaches sandboxd over REST, gRPC, and streams."""

    async def scenario():
        async with async_sandboxd_client() as client:
            sandbox = await client.create_sandbox(
                warmpool=sandboxd_warmpool, namespace=temp_namespace
            )

            health = await sandbox.health()
            assert health.status == "ok"

            result = await sandbox.commands.run("printf out; printf err >&2")
            assert result.exit_code == 0
            assert result.stdout == "out"
            assert result.stderr == "err"

            await sandbox.files.write("async.txt", "written by the async SDK\n")
            cat = await sandbox.commands.run("cat async.txt")
            assert cat.stdout == "written by the async SDK\n"

            await assert_async_round_trip(
                sandbox, "async-roundtrip.bin", 4 * 1024 * 1024
            )

    asyncio.run(scenario())


def test_sandboxd_async_get_sandbox_reattaches(temp_namespace, sandboxd_warmpool):
    """A fresh AsyncSandboxClient re-attaches to a live sandboxd sandbox."""

    async def scenario():
        async with async_sandboxd_client() as client:
            sandbox = await client.create_sandbox(
                warmpool=sandboxd_warmpool, namespace=temp_namespace
            )
            await sandbox.files.write("reattach.txt", "written before re-attach\n")
            pod_name = await sandbox.get_pod_name()

            # As in the sync test, close_connection() leaves the SandboxClaim
            # and its Pod running.
            await sandbox.close_connection()

            async with async_sandboxd_client() as fresh_client:
                reattached = await fresh_client.get_sandbox(
                    sandbox.claim_name, namespace=temp_namespace
                )
                assert reattached.claim_name == sandbox.claim_name
                assert reattached.sandbox_id == sandbox.sandbox_id
                assert await reattached.get_pod_name() == pod_name
                assert await reattached.files.read("reattach.txt") == (
                    b"written before re-attach\n"
                )
                result = await reattached.commands.run("echo 'reattached ok'")
                assert result.exit_code == 0
                assert result.stdout == "reattached ok\n"

    asyncio.run(scenario())


def test_sandboxd_async_cold_pool(temp_namespace, sandboxd_coldpool):
    """AsyncSandboxClient waits for a cold sandbox and then serves calls."""

    async def scenario():
        async with async_sandboxd_client() as client:
            sandbox = await client.create_sandbox(
                warmpool=sandboxd_coldpool, namespace=temp_namespace
            )
            result = await sandbox.commands.run("echo 'cold start ok'")
            assert result.exit_code == 0
            assert result.stdout == "cold start ok\n"
            await assert_async_round_trip(
                sandbox, "cold-stream-roundtrip.bin", 2 * 1024 * 1024
            )

    asyncio.run(scenario())
