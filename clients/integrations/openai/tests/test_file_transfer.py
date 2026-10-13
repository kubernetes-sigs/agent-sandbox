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

"""How file bytes actually move, and what happens when that channel breaks.

Two independent paths carry bytes: the bytes-clean upload/download endpoints, and base64
over the JSON ``execute`` endpoint. The exec path is bounded by how long a single command
argument may be, which is the constraint these tests pin down.
"""

from __future__ import annotations

import io
import logging
import os
import shutil
from pathlib import Path

import pytest
from agents.sandbox.errors import ExecTransportError, WorkspaceArchiveWriteError
from agents.sandbox.manifest import Manifest

from fake_k8s import FakeAsyncSandbox, FakeAsyncSandboxClient
from openai_agents_k8s_sandbox import K8sSandboxClient
from support import MAX_ARG_STRLEN, NAMESPACE, make_options


async def _sandbox_for(
    fake_client: FakeAsyncSandboxClient, session: object
) -> FakeAsyncSandbox:
    return await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)


async def test_exec_write_stays_within_the_argument_limit(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """A single execve argument is capped at 128 KiB, so base64 payloads must be chunked.

    The in-pod server ``shlex.split``s the command and execs the argv directly, so an
    unchunked base64 blob lands as one argument and fails with E2BIG well before any
    realistic file size.
    """

    payload = os.urandom(256 * 1024)
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer="exec"),
    )

    async with session:
        sandbox = await _sandbox_for(fake_client, session)
        # Applied after start() so the SDK's own probes are unaffected.
        sandbox.max_command_chars = MAX_ARG_STRLEN

        target = workspace / "big.bin"
        await session.write(target, io.BytesIO(payload))

        assert (await session.read(target)).read() == payload
        assert sandbox.longest_command_argument() <= MAX_ARG_STRLEN

    await client.delete(session)


@pytest.mark.parametrize("size", [0, 1, 32 * 1024 - 1, 128 * 1024])
async def test_exec_write_roundtrips_across_chunk_boundaries(
    client: K8sSandboxClient, workspace: Path, size: int
) -> None:
    """An empty file has no chunks to send, and the boundary sizes are easy to fencepost."""

    payload = os.urandom(size)
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer="exec"),
    )

    async with session:
        target = workspace / "payload.bin"
        await session.write(target, io.BytesIO(payload))
        assert (await session.read(target)).read() == payload

    await client.delete(session)


async def test_exec_write_cleans_up_its_staging_file(
    client: K8sSandboxClient, workspace: Path
) -> None:
    """Chunked writes stage base64 next to the target; that must not survive the write."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer="exec"),
    )

    async with session:
        await session.write(workspace / "kept.txt", io.BytesIO(os.urandom(64 * 1024)))

        assert sorted(p.name for p in workspace.iterdir()) == ["kept.txt"]

    await client.delete(session)


async def test_exec_write_reports_a_staging_cleanup_failure(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Staging that survives the write is inside the workspace the next snapshot sweeps."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer="exec"),
    )

    async with session:
        sandbox = await _sandbox_for(fake_client, session)
        # Set after the first write so the SDK's own helper installation is unaffected.
        await session.write(workspace / "warm.txt", io.BytesIO(b"warm"))
        sandbox.fail_commands_matching = f"rm -f -- {workspace}"

        with pytest.raises(WorkspaceArchiveWriteError) as excinfo:
            await session.write(workspace / "kept.txt", io.BytesIO(b"kept"))
        sandbox.fail_commands_matching = None

        assert excinfo.value.context["reason"] == "staging cleanup failed"
        # The payload landed; what failed is the sweep of the base64 staging file.
        assert (workspace / "kept.txt").read_bytes() == b"kept"
        assert list(workspace.glob("kept.txt.*.b64.part"))

    await client.delete(session)


async def test_exec_write_spares_a_file_named_like_staging(
    client: K8sSandboxClient, workspace: Path
) -> None:
    """A real file whose name looks like staging is not this write's scratch space."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer="exec"),
    )

    async with session:
        await session.write(workspace / "payload.bin.b64.part", io.BytesIO(b"not staging"))
        await session.write(workspace / "payload.bin", io.BytesIO(b"payload"))

        assert (workspace / "payload.bin").read_bytes() == b"payload"
        assert (workspace / "payload.bin.b64.part").read_bytes() == b"not staging"

    await client.delete(session)


async def test_read_fallback_can_be_turned_off(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Opting out means a broken download endpoint is visible, not silently routed."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(read_fallback=False),
    )

    async with session:
        target = workspace / "payload.txt"
        await session.write(target, io.BytesIO(b"unreadable over http"))

        sandbox = await _sandbox_for(fake_client, session)
        sandbox.fail_file_reads = RuntimeError("download endpoint exploded")

        with pytest.raises(ExecTransportError):
            await session.read(target)

    await client.delete(session)


async def test_persist_and_hydrate_over_exec_transfer(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Snapshots must work when the upload/download endpoints are unusable."""

    payload = os.urandom(96 * 1024)
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer="exec"),
    )

    async with session:
        sandbox = await _sandbox_for(fake_client, session)
        sandbox.max_command_chars = MAX_ARG_STRLEN

        await session.write(workspace / "keep" / "blob.bin", io.BytesIO(payload))
        archive = await session.persist_workspace()

        shutil.rmtree(workspace)
        workspace.mkdir(parents=True)

        await session.hydrate_workspace(archive)
        assert (await session.read(workspace / "keep" / "blob.bin")).read() == payload

        # Nothing went through the http file endpoints.
        assert sandbox.files_read == []
        assert sandbox.files_written == []

    await client.delete(session)


async def test_read_falls_back_to_exec_when_download_fails(
    client: K8sSandboxClient,
    fake_client: FakeAsyncSandboxClient,
    workspace: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    """A download endpoint that always fails must not look like a merely slow sandbox."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        target = workspace / "payload.txt"
        await session.write(target, io.BytesIO(b"still readable"))

        sandbox = await _sandbox_for(fake_client, session)
        sandbox.fail_file_reads = RuntimeError("download endpoint exploded")

        with caplog.at_level(logging.WARNING, logger="openai_agents_k8s_sandbox.session"):
            data = await session.read(target)

        assert data.read() == b"still readable"
        assert "falling back to exec" in caplog.text

    await client.delete(session)


async def test_upload_failure_surfaces_as_a_workspace_write_error(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Writes have no fallback, so the transport error must be mapped, not leaked raw."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await _sandbox_for(fake_client, session)
        sandbox.fail_file_writes = RuntimeError("upload endpoint exploded")

        with pytest.raises(WorkspaceArchiveWriteError):
            await session.write(workspace / "doomed.txt", io.BytesIO(b"x"))

    await client.delete(session)


async def test_exec_write_failure_surfaces_as_a_workspace_write_error(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer="exec"),
    )

    async with session:
        # A directory cannot be overwritten by a file redirect, so the shell exits non-zero.
        (workspace / "adirectory").mkdir(parents=True)

        with pytest.raises(WorkspaceArchiveWriteError):
            await session.write(workspace / "adirectory", io.BytesIO(b"x"))

    await client.delete(session)
