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
"""Unit tests for the NeMo Gym agent_sandbox provider (fakes only — no cluster)."""

import logging
import shlex
from collections import deque
from types import SimpleNamespace

import pytest

pytest.importorskip("nemo_gym", reason="nemo-gym is required for the provider's base types")
pytest.importorskip("k8s_agent_sandbox", reason="k8s-agent-sandbox[async] is required")
httpx = pytest.importorskip("httpx")

from nemo_gym.sandbox.providers.base import SandboxHandle, SandboxSpec, SandboxStatus  # noqa: E402

from k8s_agent_sandbox.exceptions import SandboxNotReadyError  # noqa: E402

from nemo_gym_k8s_agent_sandbox.provider import (  # noqa: E402
    MANAGED_BY_LABEL,
    MANAGED_BY_VALUE,
    SANDBOX_RUNTIME_RETURN_CODE,
    STAGING_PREFIX,
    AgentSandboxCreateError,
    AgentSandboxCreateVerificationError,
    AgentSandboxProvider,
    AgentSandboxProviderOptions,
    _AgentSandboxInstance,
)


def exec_result(stdout: str = "", stderr: str = "", exit_code: int = 0) -> SimpleNamespace:
    return SimpleNamespace(stdout=stdout, stderr=stderr, exit_code=exit_code)


class FakeCommands:
    def __init__(self) -> None:
        self.calls: list[tuple[str, int | None]] = []
        self.results: deque = deque()
        self.default = exec_result()
        self.error: BaseException | None = None

    async def run(self, command: str, timeout: int = 60):
        self.calls.append((command, timeout))
        if self.error is not None:
            raise self.error
        return self.results.popleft() if self.results else self.default

    def scripts(self) -> list[str]:
        """The inner `sh -c` script of every wrapped command sent to the runtime."""
        return [shlex.split(command)[-1] for command, _ in self.calls]


class FakeFiles:
    def __init__(self) -> None:
        self.written: list[tuple[str, bytes]] = []
        self.contents: dict[str, bytes] = {}
        self.write_error: BaseException | None = None

    async def write(self, path: str, content, timeout: int = 60, allow_unsafe_paths: bool = False):
        if self.write_error is not None:
            raise self.write_error
        data = content.encode("utf-8") if isinstance(content, str) else bytes(content)
        self.written.append((path, data))
        self.contents[path] = data

    async def read(self, path: str, timeout: int = 60, allow_unsafe_paths: bool = False) -> bytes:
        return self.contents[path]


class FakeAsyncSandbox:
    def __init__(self, claim_name: str = "sandbox-claim-test", sandbox_id: str = "sandbox-abc") -> None:
        self.claim_name = claim_name
        self.sandbox_id = sandbox_id
        self._commands = FakeCommands()
        self._files = FakeFiles()
        self.status_result: tuple[str, str] = ("SandboxReady", "")
        self.status_error: BaseException | None = None
        self.terminated = 0

    @property
    def commands(self):
        return self._commands

    @property
    def files(self):
        return self._files

    async def status(self) -> tuple[str, str]:
        if self.status_error is not None:
            raise self.status_error
        return self.status_result

    async def terminate(self) -> None:
        self.terminated += 1


class FakeK8sHelper:
    def __init__(self) -> None:
        self.claims: deque = deque()

    async def get_sandbox_claim(self, name: str, namespace: str):
        return self.claims.popleft() if self.claims else None


class FakeClient:
    def __init__(self, sandbox: FakeAsyncSandbox | None = None) -> None:
        self.sandbox = sandbox or FakeAsyncSandbox()
        self.create_error: BaseException | None = None
        self.create_calls: list[tuple[tuple, dict]] = []
        self.delete_calls: list[tuple[str, str]] = []
        self.k8s_helper = FakeK8sHelper()
        self.closed = 0

    async def create_sandbox(self, *args, **kwargs):
        self.create_calls.append((args, kwargs))
        if self.create_error is not None:
            raise self.create_error
        return self.sandbox

    async def delete_sandbox(self, claim_name: str, namespace: str = "default") -> None:
        self.delete_calls.append((claim_name, namespace))

    async def close(self) -> None:
        self.closed += 1


NO_PROBE = {"command": None}


def make_provider(client: FakeClient | None = None, **kwargs) -> AgentSandboxProvider:
    kwargs.setdefault("probe", NO_PROBE)
    provider = AgentSandboxProvider(**kwargs)
    provider._client = client or FakeClient()
    return provider


def make_handle(sandbox: FakeAsyncSandbox | None = None, **inst_kwargs) -> SandboxHandle:
    sandbox = sandbox or FakeAsyncSandbox()
    inst = _AgentSandboxInstance(
        claim_name=sandbox.claim_name,
        sandbox_name=sandbox.sandbox_id,
        namespace=inst_kwargs.pop("namespace", "default"),
        sandbox=sandbox,
        **inst_kwargs,
    )
    return SandboxHandle(sandbox_id=inst.claim_name, provider_name="agent_sandbox", raw=inst)


# ------------------------------------------------------------------------ create


async def test_create_uses_default_warmpool_and_maps_spec():
    client = FakeClient()
    provider = make_provider(client, create={"warmpool": "default-pool", "namespace": "rl"})
    spec = SandboxSpec(ttl_s=12.3, ready_timeout_s=90.5, metadata={"run": "rollout-7"})

    handle = await provider.create(spec)

    assert handle.provider_name == "agent_sandbox"
    assert handle.sandbox_id == client.sandbox.claim_name
    (args, kwargs) = client.create_calls[0]
    assert args == ("default-pool",)
    assert kwargs["namespace"] == "rl"
    assert kwargs["sandbox_ready_timeout"] == 91
    assert kwargs["shutdown_after_seconds"] == 13
    assert kwargs["labels"] == {"run": "rollout-7", MANAGED_BY_LABEL: MANAGED_BY_VALUE}


async def test_create_warmpool_resolution_precedence():
    client = FakeClient()
    provider = make_provider(
        client,
        create={"warmpool": "default-pool", "image_warmpools": {"img:1": "image-pool"}},
    )

    await provider.create(SandboxSpec(image="img:1"))
    assert client.create_calls[-1][0] == ("image-pool",)

    await provider.create(SandboxSpec(image="img:1", provider_options={"warmpool": "explicit-pool"}))
    assert client.create_calls[-1][0] == ("explicit-pool",)

    await provider.create(SandboxSpec(image="unmapped:latest"))
    assert client.create_calls[-1][0] == ("default-pool",)


async def test_create_without_any_warmpool_raises():
    provider = make_provider()
    with pytest.raises(AgentSandboxCreateError, match="No warm pool"):
        await provider.create(SandboxSpec(image="img:1"))


async def test_create_rejects_entrypoint():
    provider = make_provider(create={"warmpool": "pool"})
    with pytest.raises(AgentSandboxCreateError, match="entrypoint"):
        await provider.create(SandboxSpec(entrypoint=["/bin/init"]))


async def test_create_wraps_runtime_failure():
    client = FakeClient()
    client.create_error = SandboxNotReadyError("pool empty and pod never came up")
    provider = make_provider(client, create={"warmpool": "pool"})
    with pytest.raises(AgentSandboxCreateError, match="pool"):
        await provider.create(SandboxSpec())


async def test_create_does_not_seed_files():
    # NeMo Gym's sandbox API uploads spec.files through provider.upload_file()
    # after create() returns; seeding in create() too would double-write every file.
    client = FakeClient()
    provider = make_provider(client, create={"warmpool": "pool"})

    await provider.create(SandboxSpec(files={"/data/task/input.json": '{"k": 1}'}))

    assert client.sandbox._files.written == []
    assert client.sandbox._commands.calls == []


async def test_create_probe_failure_terminates_claim():
    client = FakeClient()
    client.sandbox._commands.default = exec_result(stderr="connection refused", exit_code=7)
    provider = make_provider(
        client,
        create={"warmpool": "pool"},
        probe={"command": "printf ok", "expected_stdout": "ok", "deadline_s": 0.05, "stable_delay_s": 0},
    )

    with pytest.raises(AgentSandboxCreateVerificationError, match="readiness probe"):
        await provider.create(SandboxSpec())
    assert client.sandbox.terminated == 1


async def test_create_rejects_non_positive_ttl_and_ready_timeout():
    provider = make_provider(create={"warmpool": "pool"})
    with pytest.raises(AgentSandboxCreateError, match="ttl_s"):
        await provider.create(SandboxSpec(ttl_s=0))
    with pytest.raises(AgentSandboxCreateError, match="ready_timeout_s"):
        await provider.create(SandboxSpec(ready_timeout_s=-1))


async def test_probe_runs_without_cwd_env_wrapping():
    # The probe must test runtime reachability only: a spec.workdir that the agent
    # has not created yet (or a spec.env export) must not fail a healthy sandbox.
    client = FakeClient()
    client.sandbox._commands.default = exec_result(stdout="ok")
    provider = make_provider(
        client,
        create={"warmpool": "pool"},
        probe={"command": "printf ok", "expected_stdout": "ok", "deadline_s": 5, "stable_delay_s": 0},
    )

    await provider.create(SandboxSpec(workdir="/not-created-yet", env={"FOO": "bar"}))

    probe_script = client.sandbox._commands.scripts()[0]
    assert probe_script == "printf ok"


async def test_create_probe_waits_for_stable_count():
    client = FakeClient()
    commands = client.sandbox._commands
    commands.results.extend([exec_result(exit_code=1), exec_result(stdout="ok"), exec_result(stdout="ok")])
    provider = make_provider(
        client,
        create={"warmpool": "pool"},
        probe={
            "command": "printf ok",
            "expected_stdout": "ok",
            "deadline_s": 5,
            "stable_count": 2,
            "stable_delay_s": 0,
        },
    )

    await provider.create(SandboxSpec())
    assert len(commands.calls) == 3


# -------------------------------------------------------------------------- exec


async def test_exec_wraps_env_cwd_and_maps_result():
    sandbox = FakeAsyncSandbox()
    sandbox._commands.default = exec_result(stdout="out", stderr="err", exit_code=3)
    provider = make_provider()
    handle = make_handle(sandbox, env={"BASE": "1"}, workdir="/work")

    result = await provider.exec(handle, "echo hi; false", env={"EXTRA": "a b"}, timeout_s=9.2)

    command, timeout = sandbox._commands.calls[0]
    shell, dash_c, script = shlex.split(command)
    assert (shell, dash_c) == ("/bin/sh", "-c")
    assert script.splitlines() == [
        "export BASE=1",
        "export EXTRA='a b'",
        f"cd /work || exit {SANDBOX_RUNTIME_RETURN_CODE}",
        "echo hi; false",
    ]
    assert timeout == 10
    assert (result.stdout, result.stderr, result.return_code) == ("out", "err", 3)
    assert result.error_type is None


async def test_exec_cwd_overrides_spec_workdir():
    sandbox = FakeAsyncSandbox()
    provider = make_provider()
    handle = make_handle(sandbox, workdir="/work")

    await provider.exec(handle, "pwd", cwd="/elsewhere")
    assert "cd /elsewhere" in sandbox._commands.scripts()[0]


async def test_exec_rejects_invalid_env_name():
    provider = make_provider()
    handle = make_handle()
    with pytest.raises(ValueError, match="environment variable name"):
        await provider.exec(handle, "true", env={"BAD-NAME": "x"})


async def test_exec_timeout_and_runtime_failures_return_sentinel():
    sandbox = FakeAsyncSandbox()
    provider = make_provider()
    handle = make_handle(sandbox)

    sandbox._commands.error = httpx.ConnectTimeout("deadline")
    result = await provider.exec(handle, "sleep 999")
    assert (result.return_code, result.error_type) == (SANDBOX_RUNTIME_RETURN_CODE, "timeout")

    sandbox._commands.error = httpx.ConnectError("boom")
    result = await provider.exec(handle, "true")
    assert (result.return_code, result.error_type) == (SANDBOX_RUNTIME_RETURN_CODE, "sandbox")

    sandbox._commands.error = ZeroDivisionError("bug in caller land")
    with pytest.raises(ZeroDivisionError):
        await provider.exec(handle, "true")


async def test_exec_runtime_error_scoping():
    # Bare RuntimeError from the command executor (malformed runtime response)
    # is a sandbox failure at the exec call site only...
    sandbox = FakeAsyncSandbox()
    provider = make_provider()
    handle = make_handle(sandbox)

    sandbox._commands.error = RuntimeError("Failed to decode JSON response from sandbox")
    result = await provider.exec(handle, "true")
    assert (result.return_code, result.error_type) == (SANDBOX_RUNTIME_RETURN_CODE, "sandbox")

    # ...but RuntimeError *subclasses* from unrelated code are not swallowed.
    class NotASandboxProblem(RuntimeError):
        pass

    sandbox._commands.error = NotASandboxProblem("programming bug")
    with pytest.raises(NotASandboxProblem):
        await provider.exec(handle, "true")


async def test_exec_warns_and_ignores_user(caplog):
    sandbox = FakeAsyncSandbox()
    provider = make_provider()
    handle = make_handle(sandbox)

    with caplog.at_level(logging.WARNING, logger="nemo_gym_k8s_agent_sandbox.provider"):
        result = await provider.exec(handle, "id", user="root")

    assert result.return_code == 0
    assert any("user" in record.message for record in caplog.records)


# ------------------------------------------------------------------------- files


async def test_upload_file_stages_then_copies(tmp_path):
    sandbox = FakeAsyncSandbox()
    provider = make_provider()
    # workdir must NOT leak into staging execs: staging names resolve against the
    # runtime server's own cwd, where the file API drops uploads.
    handle = make_handle(sandbox, workdir="/work")
    source = tmp_path / "artifact.bin"
    source.write_bytes(b"\x00\x01binary")

    await provider.upload_file(handle, source, "/data/out/artifact.bin")

    staging, data = sandbox._files.written[0]
    assert staging.startswith(STAGING_PREFIX) and "/" not in staging
    assert data == b"\x00\x01binary"
    script = sandbox._commands.scripts()[0]
    assert script == f"mkdir -p /data/out && cp {staging} /data/out/artifact.bin && rm -f {staging}"
    assert "cd " not in script


async def test_upload_file_failure_raises_and_cleans_staging(tmp_path):
    sandbox = FakeAsyncSandbox()
    sandbox._commands.results.append(exec_result(stderr="cp: no space", exit_code=1))
    provider = make_provider()
    handle = make_handle(sandbox)
    source = tmp_path / "f"
    source.write_text("x")

    with pytest.raises(RuntimeError, match="no space"):
        await provider.upload_file(handle, source, "/data/f")

    # Second exec is the best-effort staging cleanup.
    assert sandbox._commands.scripts()[1].startswith("rm -f ")


async def test_download_file_stages_reads_and_cleans(tmp_path):
    sandbox = FakeAsyncSandbox()
    provider = make_provider()
    handle = make_handle(sandbox, workdir="/work")
    target = tmp_path / "nested" / "result.tar"

    async def fake_read(path, timeout=60, allow_unsafe_paths=False):
        return b"tarball-bytes"

    sandbox._files.read = fake_read

    await provider.download_file(handle, "/data/result.tar", target)

    scripts = sandbox._commands.scripts()
    assert scripts[0].startswith("cp /data/result.tar " + STAGING_PREFIX)
    assert scripts[1].startswith("rm -f " + STAGING_PREFIX)
    assert target.read_bytes() == b"tarball-bytes"


async def test_download_file_missing_source_raises(tmp_path):
    sandbox = FakeAsyncSandbox()
    sandbox._commands.results.append(exec_result(stderr="cp: not found", exit_code=1))
    provider = make_provider()
    handle = make_handle(sandbox)

    with pytest.raises(RuntimeError, match="not found"):
        await provider.download_file(handle, "/data/missing", tmp_path / "out")


# --------------------------------------------------------------------- lifecycle


async def test_status_mapping():
    sandbox = FakeAsyncSandbox()
    provider = make_provider()
    handle = make_handle(sandbox)

    sandbox.status_result = ("SandboxReady", "")
    assert await provider.status(handle) == SandboxStatus.RUNNING

    sandbox.status_result = ("SandboxNotReady", "warming up")
    assert await provider.status(handle) == SandboxStatus.STARTING

    sandbox.status_result = ("SandboxNotFound", "")
    assert await provider.status(handle) == SandboxStatus.STOPPED

    sandbox.status_error = httpx.ConnectError("apiserver flake")
    assert await provider.status(handle) == SandboxStatus.UNKNOWN

    # A bare RuntimeError outside the exec path is a programming bug: propagate,
    # don't misreport the sandbox as UNKNOWN.
    sandbox.status_error = RuntimeError("bug in status handling")
    with pytest.raises(RuntimeError, match="bug in status handling"):
        await provider.status(handle)


async def test_close_terminates_claim_and_untracks_from_client():
    client = FakeClient()
    provider = make_provider(client)
    handle = make_handle(client.sandbox, namespace="rl")

    await provider.close(handle)
    assert client.sandbox.terminated == 1
    # The client's active-sandbox registry entry must be released too, or a long
    # RL run accumulates one dead handle per rollout.
    assert client.delete_calls == [(client.sandbox.claim_name, "rl")]


async def test_close_wait_deleted_polls_until_gone():
    client = FakeClient()
    client.k8s_helper.claims.append({"metadata": {"name": "still-there"}})
    provider = make_provider(
        client,
        operations={"close_wait_deleted": True, "close_timeout_s": 5, "poll_interval_s": 0.01},
    )
    handle = make_handle(client.sandbox)

    await provider.close(handle)
    assert client.sandbox.terminated == 1
    assert not client.k8s_helper.claims


async def test_aclose_closes_client_and_blocks_reuse():
    client = FakeClient()
    provider = make_provider(client, create={"warmpool": "pool"})

    await provider.aclose()
    await provider.aclose()  # idempotent

    assert client.closed == 1
    with pytest.raises(RuntimeError, match="closed"):
        await provider.create(SandboxSpec())


# ----------------------------------------------------------------------- options


def test_provider_options_reject_unknown_keys():
    with pytest.raises(ValueError, match="Unknown agent_sandbox provider_options"):
        AgentSandboxProviderOptions.from_mapping({"warmpool": "p", "policy": {}})


def test_provider_name_matches_entry_point():
    assert AgentSandboxProvider.name == "agent_sandbox"


def test_connection_config_validation():
    with pytest.raises(ValueError, match=r"connection\.mode"):
        make_provider(connection={"mode": "carrier-pigeon"})
    with pytest.raises(ValueError, match="api_url"):
        make_provider(connection={"mode": "direct"})
    with pytest.raises(ValueError, match="gateway_name"):
        make_provider(connection={"mode": "gateway"})
