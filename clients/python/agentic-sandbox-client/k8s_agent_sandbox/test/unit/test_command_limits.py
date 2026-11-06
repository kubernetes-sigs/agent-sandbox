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

"""Contracts for opt-in bounded execution on both Python command executors."""

import math
import os
from pathlib import Path
import subprocess
import sys
from unittest.mock import AsyncMock, MagicMock, patch

import grpc
import pytest
from pydantic import ValidationError

from k8s_agent_sandbox.commands.async_command_executor import AsyncCommandExecutor
from k8s_agent_sandbox.commands.command_executor import CommandExecutor
from k8s_agent_sandbox.commands._process_stubs import process_pb2 as pb
from k8s_agent_sandbox.models import BoundedExecutionResult, ExecutionLimits


class RpcFailure(grpc.RpcError):
    def __init__(self, code):
        self._code = code

    def code(self):
        return self._code

    def details(self):
        return "server diagnostic"


@pytest.fixture(params=["sync", "async"])
def bounded(request):
    connector = MagicMock()
    connector.is_sandboxd.return_value = True
    stub = MagicMock()
    if request.param == "async":
        for name in ("connect", "grpc_channel", "invalidate_sandboxd_transport"):
            setattr(connector, name, AsyncMock())
        stub.ExecuteWithLimits = AsyncMock()
        executor = AsyncCommandExecutor(connector, None, "test")
    else:
        executor = CommandExecutor(connector, None, "test")
    stub.ExecuteWithLimits.return_value = pb.ExecuteWithLimitsResponse(
        stdout=b"hello", stderr=b"diagnostic", exit_code=7,
        termination_reason=pb.EXECUTE_TERMINATION_REASON_EXITED,
        elapsed_duration={"seconds": 2, "nanos": 123456789},
    )

    async def run(command="echo hello", **kwargs):
        result = executor.run_with_limits(command, **kwargs)
        return await result if request.param == "async" else result

    with patch(
        "k8s_agent_sandbox.commands._process_stubs.process_pb2_grpc.ProcessServiceStub",
        return_value=stub,
    ):
        yield run, connector, stub


@pytest.mark.asyncio
async def test_bounded_request_and_result(bounded):
    run, connector, stub = bounded
    result = await run(
        "printf '%s' \"$A\"", timeout=12.5, time_limit=0.300000001,
        stdout_limit_bytes=2, stderr_limit_bytes=7, env={"A": "hello"}, cwd="work",
    )
    request = stub.ExecuteWithLimits.call_args.args[0]
    assert list(request.config.command) == ["/bin/sh", "-c", "printf '%s' \"$A\""]
    assert dict(request.config.env_vars) == {"A": "hello"}
    assert request.config.cwd == "work"
    assert request.time_limit.ToNanoseconds() == 300000001
    assert request.stdout_limit_bytes == 2
    assert request.stderr_limit_bytes == 7
    assert stub.ExecuteWithLimits.call_args.kwargs == {"timeout": 12.5}
    assert isinstance(result, BoundedExecutionResult)
    assert result.model_dump() == {
        "stdout": "hello", "stderr": "diagnostic", "exit_code": 7,
        "timed_out": False, "termination_reason": "exited",
        "elapsed_duration": 2.123456789,
        "stdout_truncated": False, "stderr_truncated": False,
    }
    connector.connect.assert_called_once()
    connector.grpc_channel.assert_called_once()
    stub.Execute.assert_not_called()
    connector.send_request.assert_not_called()


@pytest.mark.asyncio
async def test_omitted_limits_inherit_deployment_policy(bounded):
    run, _, stub = bounded
    await run()
    request = stub.ExecuteWithLimits.call_args.args[0]
    for field in ("time_limit", "stdout_limit_bytes", "stderr_limit_bytes"):
        assert not request.HasField(field)
    assert stub.ExecuteWithLimits.call_args.kwargs == {"timeout": 60}


@pytest.mark.asyncio
async def test_minimum_time_and_uint64_byte_limits(bounded):
    run, _, stub = bounded
    await run(time_limit=1e-9, stdout_limit_bytes=1, stderr_limit_bytes=2**64 - 1)
    request = stub.ExecuteWithLimits.call_args.args[0]
    assert request.time_limit.ToNanoseconds() == 1
    assert request.stdout_limit_bytes == 1
    assert request.stderr_limit_bytes == 2**64 - 1


@pytest.mark.asyncio
async def test_largest_representable_time_limit(bounded):
    run, _, stub = bounded
    limit = math.nextafter((2**63 - 1) / 1e9, 0)
    await run(time_limit=limit)
    request = stub.ExecuteWithLimits.call_args.args[0]
    assert 0 < request.time_limit.ToNanoseconds() <= 2**63 - 1
    assert request.time_limit.ToNanoseconds() == round(limit * 1e9)


@pytest.mark.asyncio
@pytest.mark.parametrize("field", ["stdout_limit_bytes", "stderr_limit_bytes"])
async def test_one_explicit_cap_preserves_other_defaults(bounded, field):
    run, _, stub = bounded
    await run(**{field: 7})
    request = stub.ExecuteWithLimits.call_args.args[0]
    assert request.HasField(field)
    other = "stderr_limit_bytes" if field == "stdout_limit_bytes" else "stdout_limit_bytes"
    assert not request.HasField(other)
    assert not request.HasField("time_limit")


@pytest.mark.asyncio
async def test_time_limit_keeps_output_exit_code_and_flags(bounded):
    run, _, stub = bounded
    stub.ExecuteWithLimits.return_value = pb.ExecuteWithLimitsResponse(
        stdout=b"partial", stderr=b"warning", exit_code=137,
        termination_reason=pb.EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED,
        elapsed_duration={"nanos": 300000000},
        stdout_truncated=True, stderr_truncated=False,
    )
    result = await run(time_limit=0.3, timeout=10)
    assert result.exit_code == 137
    assert result.stdout == "partial"
    assert result.stderr == "warning"
    assert result.timed_out is True
    assert result.termination_reason == "time_limit_exceeded"
    assert result.elapsed_duration == 0.3
    assert result.stdout_truncated is True
    assert result.stderr_truncated is False


@pytest.mark.asyncio
@pytest.mark.parametrize("stdout_flag,stderr_flag", [(True, False), (False, True), (True, True)])
async def test_stream_flags_and_utf8_decoding(bounded, stdout_flag, stderr_flag):
    run, _, stub = bounded
    stub.ExecuteWithLimits.return_value = pb.ExecuteWithLimitsResponse(
        stdout=b"\xe2\x82\xac\xe2", stderr=b"\xff",
        termination_reason=pb.EXECUTE_TERMINATION_REASON_EXITED,
        stdout_truncated=stdout_flag, stderr_truncated=stderr_flag,
    )
    result = await run(stdout_limit_bytes=4, stderr_limit_bytes=1)
    assert result.stdout == "\u20ac\ufffd"
    assert result.stderr == "\ufffd"
    assert result.stdout_truncated is stdout_flag
    assert result.stderr_truncated is stderr_flag
    assert not result.timed_out


@pytest.mark.asyncio
@pytest.mark.parametrize("reason,expected", [(0, "unspecified"), (99, "99")])
async def test_unknown_termination_reason_is_preserved(bounded, reason, expected):
    run, _, stub = bounded
    stub.ExecuteWithLimits.return_value = pb.ExecuteWithLimitsResponse(
        termination_reason=reason,
    )
    result = await run()
    assert result.termination_reason == expected
    assert not result.timed_out


@pytest.mark.asyncio
@pytest.mark.parametrize("code", [
    grpc.StatusCode.DEADLINE_EXCEEDED, grpc.StatusCode.CANCELLED,
    grpc.StatusCode.INVALID_ARGUMENT, grpc.StatusCode.RESOURCE_EXHAUSTED,
    grpc.StatusCode.UNAVAILABLE,
])
async def test_transport_errors_keep_diagnostics_without_replay(bounded, code):
    run, connector, stub = bounded
    error = RpcFailure(code)
    stub.ExecuteWithLimits.side_effect = error
    with pytest.raises(RuntimeError, match=f"{code.name}.*server diagnostic") as raised:
        await run(timeout=0.1, time_limit=5)
    assert raised.value.__cause__ is error
    stub.ExecuteWithLimits.assert_called_once()
    stub.Execute.assert_not_called()
    connector.send_request.assert_not_called()
    if code == grpc.StatusCode.UNAVAILABLE:
        connector.invalidate_sandboxd_transport.assert_called_once_with(
            connector.grpc_channel.return_value
        )
    else:
        connector.invalidate_sandboxd_transport.assert_not_called()


@pytest.mark.asyncio
async def test_old_server_is_explicitly_unsupported(bounded):
    run, connector, stub = bounded
    error = RpcFailure(grpc.StatusCode.UNIMPLEMENTED)
    stub.ExecuteWithLimits.side_effect = error
    with pytest.raises(NotImplementedError, match="ExecuteWithLimits") as raised:
        await run()
    assert raised.value.__cause__ is error
    stub.ExecuteWithLimits.assert_called_once()
    stub.Execute.assert_not_called()
    connector.send_request.assert_not_called()
    connector.invalidate_sandboxd_transport.assert_not_called()


@pytest.mark.asyncio
async def test_legacy_runtime_fails_before_dispatch(bounded):
    run, connector, stub = bounded
    connector.is_sandboxd.return_value = False
    with pytest.raises(NotImplementedError, match="sandboxd"):
        await run()
    connector.connect.assert_not_called()
    connector.send_request.assert_not_called()
    stub.ExecuteWithLimits.assert_not_called()


INVALID_LIMITS = [
    *[{"time_limit": v} for v in (
        0, -1, 1e-10, math.inf, -math.inf, math.nan, 1e100, True, "1",
        (2**63 - 1) / 1e9,
    )],
    *[{field: v}
      for field in ("stdout_limit_bytes", "stderr_limit_bytes")
      for v in (0, -1, 1.5, 1.0, math.inf, math.nan, True, "1", 2**64)],
]


@pytest.mark.parametrize("kwargs", INVALID_LIMITS)
def test_invalid_limit_models(kwargs):
    with pytest.raises(ValidationError):
        ExecutionLimits(**kwargs)


@pytest.mark.asyncio
@pytest.mark.parametrize("kwargs", [
    *INVALID_LIMITS,
    *[{"timeout": v} for v in (0, -1, math.inf, math.nan, True, "1")],
    {"env": {"A=B": "value"}},
    {"env": {"": "value"}},
])
async def test_invalid_options_fail_before_connect(bounded, kwargs):
    run, connector, stub = bounded
    with pytest.raises(ValueError):
        await run(**kwargs)
    connector.connect.assert_not_called()
    stub.ExecuteWithLimits.assert_not_called()


def test_limits_validate_assignments():
    limits = ExecutionLimits(time_limit=1, stdout_limit_bytes=10)
    with pytest.raises(ValidationError):
        limits.stdout_limit_bytes = 0


@pytest.mark.asyncio
async def test_missing_grpc_extra_is_actionable(bounded):
    run, connector, _ = bounded
    with patch.dict(sys.modules, {"grpc": None}):
        with pytest.raises(ImportError, match=r"k8s-agent-sandbox\[grpc\]"):
            await run()
    connector.connect.assert_not_called()


def test_imports_work_without_grpc_or_protobuf():
    package_root = str(Path(__file__).resolve().parents[3])
    script = """
import importlib.abc
import sys

class BlockGrpc(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path, target=None):
        if fullname == "grpc" or fullname.startswith(("grpc.", "google.protobuf")):
            raise ImportError("optional dependency blocked")

sys.meta_path.insert(0, BlockGrpc())
from k8s_agent_sandbox.commands.command_executor import CommandExecutor
from k8s_agent_sandbox.commands.async_command_executor import AsyncCommandExecutor
from k8s_agent_sandbox.models import BoundedExecutionResult, ExecutionLimits
assert ExecutionLimits(time_limit=1).time_limit == 1
assert hasattr(CommandExecutor, "run_with_limits")
assert hasattr(AsyncCommandExecutor, "run_with_limits")
"""
    completed = subprocess.run(
        [sys.executable, "-c", script], capture_output=True, text=True,
        env={**os.environ, "PYTHONPATH": package_root}, timeout=20,
    )
    assert completed.returncode == 0, completed.stderr
