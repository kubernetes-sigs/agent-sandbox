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

"""Loopback interoperability tests; set SANDBOXD_BINARY to enable daemon cases."""

import asyncio
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import shlex
import socket
import subprocess
import sys
import time
from types import SimpleNamespace

import grpc
import pytest
import pytest_asyncio

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.commands.async_command_executor import AsyncCommandExecutor
from k8s_agent_sandbox.commands.command_executor import CommandExecutor
from k8s_agent_sandbox.commands._process_stubs import process_pb2 as pb
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import SandboxdInClusterConnectionConfig


@pytest.fixture(scope="module")
def daemon(tmp_path_factory):
    binary = os.environ.get("SANDBOXD_BINARY")
    if not binary:
        pytest.skip("set SANDBOXD_BINARY to run loopback daemon tests")
    root = tmp_path_factory.mktemp("bounded-daemon")
    reservations = [socket.socket(), socket.socket()]
    try:
        for sock in reservations:
            sock.bind(("127.0.0.1", 0))
        grpc_port, rest_port = [sock.getsockname()[1] for sock in reservations]
    finally:
        for sock in reservations:
            sock.close()
    args = [
        str(Path(binary).resolve()), "--listen-host=127.0.0.1",
        f"--root-dir={root}", f"--grpc-port={grpc_port}", f"--rest-port={rest_port}",
        "--shutdown-timeout=3s", "--execute-default-time-limit=1s",
        "--execute-max-time-limit=5s",
        "--execute-default-stdout-limit-bytes=4096",
        "--execute-default-stderr-limit-bytes=4096",
        "--execute-max-stdout-limit-bytes=8192",
        "--execute-max-stderr-limit-bytes=8192",
    ]
    with (root / "daemon.log").open("w") as log:
        process = subprocess.Popen(args, cwd=root, stdout=log, stderr=subprocess.STDOUT)
        try:
            with grpc.insecure_channel(f"127.0.0.1:{grpc_port}") as channel:
                grpc.channel_ready_future(channel).result(timeout=10)
            yield SimpleNamespace(root=root, grpc_port=grpc_port, rest_port=rest_port)
        finally:
            process.terminate()
            try:
                process.wait(timeout=6)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)
            assert process.returncode == 0, (root / "daemon.log").read_text()


def _connector(mode, grpc_port, rest_port):
    config = SandboxdInClusterConnectionConfig(
        mode="in-cluster-pod-ip", grpc_port=grpc_port, rest_port=rest_port,
    )
    if mode == "async":
        async def pod_ip():
            return "127.0.0.1"
        return AsyncSandboxConnector("loopback", "default", config, None, get_pod_ip=pod_ip)
    return SandboxConnector(
        "loopback", "default", config, None, get_pod_ip=lambda: "127.0.0.1",
    )


@pytest_asyncio.fixture(params=["sync", "async"])
async def client(request, daemon):
    connector = _connector(request.param, daemon.grpc_port, daemon.rest_port)
    executor = (
        AsyncCommandExecutor(connector, None, "test") if request.param == "async"
        else CommandExecutor(connector, None, "test")
    )
    # Use a short per-test name so long parametrized node IDs fit filesystem limits.
    case = daemon.root / f"case-{time.monotonic_ns()}"
    case.mkdir()

    async def run(command, **kwargs):
        if request.param == "async":
            result = await executor.run_with_limits(command, cwd=case.name, **kwargs)
        else:
            result = await asyncio.to_thread(
                executor.run_with_limits, command, cwd=case.name, **kwargs,
            )
        print(json.dumps({"case": request.node.nodeid, "result": result.model_dump()}))
        return result

    try:
        yield run, case
    finally:
        if request.param == "async":
            await connector.close()
        else:
            connector.close()


def _python(code):
    return f"exec {shlex.quote(sys.executable)} -c {shlex.quote(code)}"


def _output(stdout, stderr):
    return _python(
        "import os; "
        "open('count', 'a').write('x'); "
        f"os.write(1, b'O' * {stdout}); os.write(2, b'E' * {stderr})"
    )


def _slow():
    return _python(
        "import os, time; "
        "open('pid', 'w').write(str(os.getpid())); "
        "open('count', 'a').write('x'); "
        "os.write(1, b'before'); os.write(2, b'diagnostic'); time.sleep(30)"
    )


async def _assert_reaped(case):
    pid = int((case / "pid").read_text())
    for _ in range(300):
        try:
            state = Path(f"/proc/{pid}/stat").read_text().split(") ", 1)[1].split()[0]
        except FileNotFoundError:
            return
        if state == "Z":
            return
        await asyncio.sleep(0.01)
    pytest.fail(f"command process {pid} is still running")


async def _health(run):
    result = await run("printf healthy", timeout=5, time_limit=1)
    assert result.stdout == "healthy"
    assert result.exit_code == 0
    assert result.termination_reason == "exited"


@pytest.mark.asyncio
@pytest.mark.parametrize("exit_code", [0, 7])
async def test_daemon_normal_and_nonzero_exit(client, exit_code):
    run, case = client
    result = await run(
        f"printf x >> count; printf '%s' \"$VALUE\"; printf diagnostic >&2; exit {exit_code}",
        env={"VALUE": "normal"},
    )
    assert result.stdout == "normal"
    assert result.stderr == "diagnostic"
    assert result.exit_code == exit_code
    assert result.termination_reason == "exited"
    assert not result.timed_out
    assert 0 < result.elapsed_duration < 5
    assert not result.stdout_truncated and not result.stderr_truncated
    assert (case / "count").read_text() == "x"


@pytest.mark.asyncio
@pytest.mark.parametrize("stdout,stderr", [
    (4095, 4095), (4096, 4096), (8192, 8192), (8192, 0), (0, 8192),
])
async def test_daemon_output_boundaries(client, stdout, stderr):
    run, case = client
    result = await run(_output(stdout, stderr), stdout_limit_bytes=4096, stderr_limit_bytes=4096)
    assert result.stdout == "O" * min(stdout, 4096)
    assert result.stderr == "E" * min(stderr, 4096)
    assert result.stdout_truncated is (stdout > 4096)
    assert result.stderr_truncated is (stderr > 4096)
    assert result.exit_code == 0 and result.termination_reason == "exited"
    assert (case / "count").read_text() == "x"
    await _health(run)


@pytest.mark.asyncio
async def test_daemon_independent_quotas_and_utf8(client):
    run, _ = client
    result = await run(
        "printf abcdef; printf 1234567 >&2", stdout_limit_bytes=2, stderr_limit_bytes=7,
    )
    assert result.stdout == "ab" and result.stdout_truncated
    assert result.stderr == "1234567" and not result.stderr_truncated
    result = await run(
        "printf '\\342\\202\\254\\342\\202\\254'", stdout_limit_bytes=4,
    )
    assert result.stdout == "\u20ac\ufffd"
    assert result.stdout_truncated


@pytest.mark.asyncio
async def test_daemon_execution_timeout_keeps_diagnostics(client):
    run, case = client
    result = await run(_slow(), timeout=5, time_limit=0.3)
    assert result.stdout == "before"
    assert result.stderr == "diagnostic"
    assert result.exit_code == 137
    assert result.termination_reason == "time_limit_exceeded" and result.timed_out
    assert 0.3 <= result.elapsed_duration < 5
    assert (case / "count").read_text() == "x"
    await _assert_reaped(case)
    await _health(run)


@pytest.mark.asyncio
async def test_daemon_transport_deadline_preserves_error(client):
    run, case = client
    with pytest.raises(RuntimeError) as raised:
        await run(_slow(), timeout=0.3, time_limit=3)
    error = raised.value.__cause__
    assert isinstance(error, grpc.RpcError)
    assert error.code() in (grpc.StatusCode.DEADLINE_EXCEEDED, grpc.StatusCode.CANCELLED)
    assert error.details()
    assert (case / "count").read_text() == "x"
    await _assert_reaped(case)
    await _health(run)


@pytest.mark.asyncio
async def test_daemon_deployment_defaults(client):
    run, case = client
    result = await run(_output(6000, 6000))
    assert result.stdout == "O" * 4096 and result.stdout_truncated
    assert result.stderr == "E" * 4096 and result.stderr_truncated
    assert (case / "count").read_text() == "x"
    result = await run(_slow(), timeout=5)
    assert result.termination_reason == "time_limit_exceeded"
    assert result.timed_out and result.exit_code == 137
    assert result.stdout == "before" and result.stderr == "diagnostic"
    assert 1 <= result.elapsed_duration < 5
    assert (case / "count").read_text() == "xx"
    await _assert_reaped(case)
    await _health(run)


@pytest.mark.asyncio
async def test_daemon_accepts_explicit_deployment_maxima(client):
    run, case = client
    result = await run(
        _output(8192, 8192), time_limit=5,
        stdout_limit_bytes=8192, stderr_limit_bytes=8192,
    )
    assert result.stdout == "O" * 8192 and not result.stdout_truncated
    assert result.stderr == "E" * 8192 and not result.stderr_truncated
    assert result.exit_code == 0
    assert (case / "count").read_text() == "x"


@pytest.mark.asyncio
@pytest.mark.parametrize("limits", [
    {"time_limit": 6}, {"stdout_limit_bytes": 8193}, {"stderr_limit_bytes": 8193},
])
async def test_daemon_deployment_maxima_reject_before_launch(client, limits):
    run, case = client
    with pytest.raises(RuntimeError) as raised:
        await run("printf x >> count", **limits)
    error = raised.value.__cause__
    assert error.code() == grpc.StatusCode.INVALID_ARGUMENT
    assert "deployment maximum" in error.details()
    assert not (case / "count").exists()
    await _health(run)


@pytest.mark.asyncio
@pytest.mark.parametrize("mode", ["sync", "async"])
async def test_old_server_has_zero_legacy_dispatch(mode, tmp_path):
    count = tmp_path / "legacy-count"

    def legacy_execute(request, context):
        with count.open("a") as stream:
            stream.write("x")
        return pb.ExecuteResponse(stdout=b"legacy", exit_code=0)

    with ThreadPoolExecutor(max_workers=2) as pool:
        server = grpc.server(pool)
        server.add_generic_rpc_handlers((grpc.method_handlers_generic_handler(
            "process.v1.ProcessService", {
                "Execute": grpc.unary_unary_rpc_method_handler(
                    legacy_execute, request_deserializer=pb.ExecuteRequest.FromString,
                    response_serializer=pb.ExecuteResponse.SerializeToString,
                ),
            },
        ),))
        port = server.add_insecure_port("127.0.0.1:0")
        server.start()
        connector = _connector(mode, port, 8080 if port != 8080 else 8081)
        executor = (
            AsyncCommandExecutor(connector, None, "test") if mode == "async"
            else CommandExecutor(connector, None, "test")
        )
        try:
            with pytest.raises(NotImplementedError, match="ExecuteWithLimits") as raised:
                if mode == "async":
                    await executor.run_with_limits("printf x", timeout=5)
                else:
                    await asyncio.to_thread(executor.run_with_limits, "printf x", timeout=5)
            assert raised.value.__cause__.code() == grpc.StatusCode.UNIMPLEMENTED
            assert not count.exists()
            if mode == "async":
                result = await executor.run("true", timeout=5)
            else:
                result = await asyncio.to_thread(executor.run, "true", timeout=5)
            assert result.stdout == "legacy"
            assert count.read_text() == "x"
        finally:
            if mode == "async":
                await connector.close()
            else:
                connector.close()
            server.stop(0).wait(timeout=5)
