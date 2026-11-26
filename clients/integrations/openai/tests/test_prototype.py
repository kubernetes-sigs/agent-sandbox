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

"""End-to-end exercise of the provider against a fake pod.

These run the real ``SandboxSession`` machinery — ``start()``, manifest materialization,
the remote path probes, snapshot persist/restore — so a contract break in the SDK shows
up here rather than against a live cluster.
"""

from __future__ import annotations

import io
import shutil
from pathlib import Path

import pytest
from agents.sandbox.errors import (
    ExecTimeoutError,
    InvalidManifestPathError,
    WorkspaceReadNotFoundError,
)
from agents.sandbox.manifest import Manifest
from agents.sandbox.snapshot import LocalSnapshotSpec

from fake_k8s import FakeAsyncSandboxClient
from openai_agents_k8s_sandbox import K8sSandboxClient
from support import WARM_POOL, make_options


async def test_start_and_exec(client: K8sSandboxClient, workspace: Path) -> None:
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())

    async with session:
        result = await session.exec("echo hello")
        assert result.ok()
        assert result.stdout.strip() == b"hello"

        # The workspace root is materialized by start().
        assert workspace.is_dir()

    await client.delete(session)


async def test_create_passes_lifecycle_settings(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    options = make_options(shutdown_after_seconds=120, pod_labels={"team": "agents"})
    session = await client.create(manifest=Manifest(root=str(workspace)), options=options)

    assert fake_client.created == [
        {
            "warmpool": WARM_POOL,
            "namespace": "agents",
            "sandbox_ready_timeout": 180,
            "labels": None,
            "shutdown_after_seconds": 120,
            "pod_labels": {"team": "agents"},
        }
    ]
    await client.delete(session)


@pytest.mark.parametrize("file_transfer", ["http", "exec"])
async def test_binary_file_roundtrip(
    client: K8sSandboxClient,
    fake_client: FakeAsyncSandboxClient,
    workspace: Path,
    file_transfer: str,
) -> None:
    payload = bytes(range(256)) * 8  # NULs, high bytes, invalid UTF-8
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(file_transfer=file_transfer),
    )

    async with session:
        target = workspace / "nested" / "blob.bin"
        await session.write(target, io.BytesIO(payload))
        assert (await session.read(target)).read() == payload

        # Pin which path actually carried the bytes: `read` falls back to exec when the
        # download endpoint errors, which would otherwise let a broken http path pass.
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace="agents")
        used_http = bool(sandbox.files_read) and bool(sandbox.files_written)
        assert used_http is (file_transfer == "http")

    await client.delete(session)


async def test_read_missing_file_is_classified(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())

    async with session:
        with pytest.raises(WorkspaceReadNotFoundError):
            await session.read(workspace / "does-not-exist.txt")

    await client.delete(session)


async def test_persist_and_hydrate_workspace(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())

    async with session:
        await session.write(workspace / "keep" / "a.txt", io.BytesIO(b"kept"))
        archive = await session.persist_workspace()

        shutil.rmtree(workspace)
        workspace.mkdir(parents=True)

        await session.hydrate_workspace(archive)
        assert (await session.read(workspace / "keep" / "a.txt")).read() == b"kept"

    await client.delete(session)


async def test_resume_reattaches_to_live_claim(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())
    async with session:
        await session.write(workspace / "state.txt", io.BytesIO(b"before"))
        claim_name = session.state.claim_name
        payload = client.serialize_session_state(session.state)

    # A different process would rebuild the state from JSON.
    state = client.deserialize_session_state(payload)
    assert state.claim_name == claim_name

    resumed = await client.resume(state)
    async with resumed:
        assert (await resumed.read(workspace / "state.txt")).read() == b"before"
        assert resumed.state.claim_name == claim_name

    await client.delete(resumed)


async def test_resume_replaces_lost_claim_and_restores_snapshot(
    client: K8sSandboxClient,
    fake_client: FakeAsyncSandboxClient,
    workspace: Path,
    tmp_path: Path,
) -> None:
    snapshots = tmp_path / "snapshots"
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        snapshot=LocalSnapshotSpec(base_path=snapshots),
        options=make_options(),
    )
    async with session:
        await session.write(workspace / "durable.txt", io.BytesIO(b"survives"))
        claim_name = session.state.claim_name
        state = session.state
    # Leaving the context persisted the workspace into the local snapshot.
    assert list(snapshots.glob("*.tar"))

    # The pod and its PVC are gone: TTL expiry, node loss, namespace cleanup.
    fake_client.drop_claim(claim_name, namespace="agents")
    shutil.rmtree(workspace)

    resumed = await client.resume(state)
    async with resumed:
        assert resumed.state.claim_name != claim_name
        assert (await resumed.read(workspace / "durable.txt")).read() == b"survives"

    await client.delete(resumed)


async def test_resume_refuses_warm_pool_mismatch(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())
    async with session:
        state = session.state

    state.warm_pool = "some-other-pool"
    with pytest.raises(ValueError, match="Refusing to reattach"):
        await client.resume(state)

    await client.delete(session)


async def test_delete_terminates_the_claim(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())
    claim_name = session.state.claim_name

    await client.delete(session)

    from k8s_agent_sandbox.exceptions import SandboxNotFoundError

    with pytest.raises(SandboxNotFoundError):
        await fake_client.get_sandbox(claim_name, namespace="agents")


async def test_exec_timeout_is_mapped(client: K8sSandboxClient, workspace: Path) -> None:
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())

    async with session:
        with pytest.raises(ExecTimeoutError):
            await session.exec("sleep 5", timeout=1)

    await client.delete(session)


async def test_absolute_paths_reach_the_file_endpoints(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """The in-pod client strips a leading '/' unless allow_unsafe_paths is set."""
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())

    async with session:
        await session.write(workspace / "abs.txt", io.BytesIO(b"x"))
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace="agents")
        assert any(p.startswith("/") for p in sandbox.files_written)

    await client.delete(session)


async def test_capability_contract(client: K8sSandboxClient, workspace: Path) -> None:
    """Both are advertised as unsupported; the SDK branches on them during start()."""
    session = await client.create(manifest=Manifest(root=str(workspace)), options=make_options())

    async with session:
        assert session.supports_pty() is False
        assert session._inner.supports_docker_volume_mounts() is False

    await client.delete(session)


async def test_workspace_root_is_manifest_driven(
    client: K8sSandboxClient, tmp_path: Path
) -> None:
    """Nothing hardcodes /workspace: relative paths resolve under whatever root is set."""
    root = tmp_path / "srv" / "app"  # deliberately not named "workspace"
    session = await client.create(manifest=Manifest(root=str(root)), options=make_options())

    async with session:
        assert session._inner._workspace_root_path() == root
        assert root.is_dir()

        await session.write(Path("notes.txt"), io.BytesIO(b"relative"))
        assert (root / "notes.txt").read_bytes() == b"relative"
        assert (await session.read(Path("notes.txt"))).read() == b"relative"

        # A path outside the configured root is refused, /workspace included.
        with pytest.raises(InvalidManifestPathError):
            await session.write(Path("/workspace/escape.txt"), io.BytesIO(b"x"))

    await client.delete(session)
