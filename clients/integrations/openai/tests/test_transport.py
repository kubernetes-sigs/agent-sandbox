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

"""Transport-level behaviour: readiness reporting, timeouts, and error classification.

``ExecTimeoutError`` and ``ExecTransportError`` mean different things to a caller — one is
worth retrying with a longer budget, the other is not — so the boundary between them
matters.
"""

from __future__ import annotations

import base64
import os
import shlex
from pathlib import Path

import pytest
from agents.sandbox.errors import ExecTimeoutError, ExecTransportError
from agents.sandbox.manifest import Manifest

from fake_k8s import FakeAsyncSandboxClient
from openai_agents_k8s_sandbox import K8sHttpTransport, K8sSandboxClient
from openai_agents_k8s_sandbox.transport import _EXEC_CHUNK_CHARS
from support import NAMESPACE, make_options


@pytest.fixture
async def exec_transport(fake_client: FakeAsyncSandboxClient) -> K8sHttpTransport:
    """A transport wired straight to a sandbox, bypassing the session."""

    sandbox = await fake_client.create_sandbox("python-sandbox-pool", namespace=NAMESPACE)
    return K8sHttpTransport(sandbox, file_transfer="exec")


async def test_running_is_true_for_a_ready_sandbox(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        assert await session.running() is True

    await client.delete(session)


async def test_running_is_false_while_the_sandbox_is_not_ready(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Alive but still pending is not ready — only "SandboxReady" counts."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.status_value = "SandboxPending"

        assert await session.running() is False

    await client.delete(session)


async def test_running_is_false_when_the_status_call_fails(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.status_error = RuntimeError("status endpoint unreachable")

        assert await session.running() is False

    await client.delete(session)


async def test_unbounded_timeout_becomes_the_configured_default(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """The in-pod API has no "wait forever" mode, so None must map to a finite budget."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exec_timeout_default_s=42.0),
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.commands_timeouts.clear()

        await session.exec("true", timeout=None)

        assert sandbox.commands_timeouts == [42]

    await client.delete(session)


async def test_subsecond_timeout_floors_to_one_second(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """The endpoint takes whole seconds; rounding to 0 would mean "no time at all"."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.commands_timeouts.clear()

        await session.exec("true", timeout=0.2)

        assert sandbox.commands_timeouts == [1]

    await client.delete(session)


async def test_backend_failure_is_a_transport_error_not_a_timeout(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        await sandbox.terminate()

        with pytest.raises(ExecTransportError):
            await session.exec("true")

    await client.delete(session)


async def test_exec_write_removes_staging_on_success(
    exec_transport: K8sHttpTransport, tmp_path: Path
) -> None:
    target = tmp_path / "payload.bin"

    await exec_transport.write_file(str(target), b"x" * (64 * 1024))

    assert target.read_bytes() == b"x" * (64 * 1024)
    assert list(tmp_path.glob("*.b64.part")) == []


async def test_exec_write_passes_chunks_as_arguments(
    fake_client: FakeAsyncSandboxClient, tmp_path: Path
) -> None:
    """Chunks ride argv, not the script text: today's alphabet is not a guarantee."""

    sandbox = await fake_client.create_sandbox("python-sandbox-pool", namespace=NAMESPACE)
    transport = K8sHttpTransport(sandbox, file_transfer="exec")
    payload = os.urandom(64 * 1024)
    encoded = base64.b64encode(payload).decode("ascii")

    await transport.write_file(str(tmp_path / "payload.bin"), payload)

    # Each chunk write renders as ["sh", "-lc", <script>, "sh", <staging>, <chunk>], so
    # the tail of the argv is the chunk itself and the script never carries base64.
    writes = [shlex.split(command) for command in sandbox.commands_run if "printf" in command]
    assert [argv[5:] for argv in writes] == [
        [encoded[i : i + _EXEC_CHUNK_CHARS]]
        for i in range(0, len(encoded), _EXEC_CHUNK_CHARS)
    ]
    assert all(encoded[:64] not in argv[2] for argv in writes)


async def test_exec_write_spares_a_file_named_like_staging(
    exec_transport: K8sHttpTransport, tmp_path: Path
) -> None:
    """A fixed suffix would overwrite this file and then delete it during cleanup."""

    target = tmp_path / "payload.bin"
    lookalike = tmp_path / "payload.bin.b64.part"
    lookalike.write_bytes(b"not staging")

    await exec_transport.write_file(str(target), b"payload")

    assert target.read_bytes() == b"payload"
    assert lookalike.read_bytes() == b"not staging"


async def test_exec_write_handles_an_empty_payload(
    exec_transport: K8sHttpTransport, tmp_path: Path
) -> None:
    """No chunks to send: the staging file still has to be created and decoded."""

    target = tmp_path / "empty.bin"

    await exec_transport.write_file(str(target), b"")

    assert target.read_bytes() == b""
    assert list(tmp_path.glob("*.b64.part")) == []


async def test_exec_write_removes_staging_when_the_decode_fails(
    exec_transport: K8sHttpTransport, tmp_path: Path
) -> None:
    """The chunks land, then the decode cannot write its output — staging must not leak."""

    target = tmp_path / "target"
    target.mkdir()  # a directory cannot be overwritten by a redirect

    with pytest.raises(ExecTransportError) as excinfo:
        await exec_transport.write_file(str(target), b"payload")

    assert excinfo.value.context["exit_code"] != 0
    assert list(tmp_path.glob("*.b64.part")) == []


async def test_exec_write_preserves_the_original_error_when_staging_fails(
    exec_transport: K8sHttpTransport, tmp_path: Path
) -> None:
    """A chunk write that cannot even create staging must still report the chunk failure."""

    target = tmp_path / "no-such-dir" / "payload.bin"

    with pytest.raises(ExecTransportError) as excinfo:
        await exec_transport.write_file(str(target), b"payload")

    # The chunk-write failure, not something raised by the cleanup that follows it.
    assert excinfo.value.context["chunk"] == 0
    assert list(tmp_path.glob("*.b64.part")) == []


async def test_exec_write_reports_a_staging_cleanup_failure(
    fake_client: FakeAsyncSandboxClient, tmp_path: Path
) -> None:
    """A write nobody can see failed is worse than one that reports its leftover."""

    sandbox = await fake_client.create_sandbox("python-sandbox-pool", namespace=NAMESPACE)
    transport = K8sHttpTransport(sandbox, file_transfer="exec")
    sandbox.fail_commands_matching = "rm -f"
    target = tmp_path / "payload.bin"

    with pytest.raises(ExecTransportError) as excinfo:
        await transport.write_file(str(target), b"payload")

    assert "cleanup" in excinfo.value.message
    assert excinfo.value.context["exit_code"] == 1
    # The payload itself did land; only the staging file could not be swept.
    assert target.read_bytes() == b"payload"


async def test_exec_write_cleanup_failure_never_masks_the_write_failure(
    fake_client: FakeAsyncSandboxClient, tmp_path: Path
) -> None:
    sandbox = await fake_client.create_sandbox("python-sandbox-pool", namespace=NAMESPACE)
    transport = K8sHttpTransport(sandbox, file_transfer="exec")
    sandbox.fail_commands_matching = "rm -f"
    target = tmp_path / "target"
    target.mkdir()  # a directory cannot be overwritten by the decode's redirect

    with pytest.raises(ExecTransportError) as excinfo:
        await transport.write_file(str(target), b"payload")

    assert excinfo.value.message == "sandbox file write failed"


async def test_timeout_error_reports_the_requested_budget(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        with pytest.raises(ExecTimeoutError) as excinfo:
            await session.exec("sleep 5", timeout=1)

        assert excinfo.value.context["timeout_s"] == 1.0

    await client.delete(session)


async def test_timeout_error_marks_the_budget_as_client_side(
    client: K8sSandboxClient, workspace: Path
) -> None:
    """The error records which side enforced the budget."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        with pytest.raises(ExecTimeoutError) as excinfo:
            await session.exec("sleep 5", timeout=1)

        assert excinfo.value.context["enforced_by"] == "client"

    await client.delete(session)


async def test_timeout_error_reports_the_default_when_none_was_requested(
    client: K8sSandboxClient, workspace: Path
) -> None:
    """Reporting None for something that just timed out would read as "no timeout"."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exec_timeout_default_s=1.0),
    )

    async with session:
        with pytest.raises(ExecTimeoutError) as excinfo:
            await session.exec("sleep 5", timeout=None)

        assert excinfo.value.context["timeout_s"] == 1.0

    await client.delete(session)
