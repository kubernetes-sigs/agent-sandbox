"""Snapshot archives, including the ones that are trying to escape the workspace.

``hydrate_workspace`` extracts an archive inside the pod with ``tar -x``. The archive may
come from a snapshot store the caller does not fully control, so it is validated locally
first — before any bytes are uploaded — and that guard is the security boundary here.
"""

from __future__ import annotations

import io
import shutil
from pathlib import Path

import pytest
from agents.sandbox.errors import WorkspaceArchiveReadError, WorkspaceArchiveWriteError
from agents.sandbox.manifest import Manifest

from openai_agents_k8s_sandbox import K8sSandboxClient
from support import make_options, make_tar


@pytest.fixture
async def session(client: K8sSandboxClient, workspace: Path):
    started = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )
    async with started:
        yield started
    await client.delete(started)


async def test_rejects_parent_traversal_members(session, workspace: Path) -> None:
    archive = make_tar({"../escape.txt": b"pwned"})

    with pytest.raises(WorkspaceArchiveWriteError) as excinfo:
        await session.hydrate_workspace(io.BytesIO(archive))

    assert excinfo.value.context["reason"] == "parent traversal"
    assert not (workspace.parent / "escape.txt").exists()


async def test_rejects_absolute_members(session, workspace: Path) -> None:
    archive = make_tar({"/etc/pwned.txt": b"pwned"})

    with pytest.raises(WorkspaceArchiveWriteError) as excinfo:
        await session.hydrate_workspace(io.BytesIO(archive))

    assert excinfo.value.context["reason"] == "absolute path"


async def test_rejects_symlinks_pointing_outside_the_archive(session) -> None:
    archive = make_tar({}, symlinks={"./link": "/etc/passwd"})

    with pytest.raises(WorkspaceArchiveWriteError) as excinfo:
        await session.hydrate_workspace(io.BytesIO(archive))

    assert "absolute symlink target" in excinfo.value.context["reason"]


async def test_rejects_a_corrupt_archive(session) -> None:
    with pytest.raises(WorkspaceArchiveWriteError):
        await session.hydrate_workspace(io.BytesIO(b"this is not a tar archive"))


async def test_rejects_a_non_bytes_payload(session) -> None:
    class BadStream(io.IOBase):
        def read(self, size: int = -1) -> object:  # type: ignore[override]
            return 12345

    with pytest.raises(WorkspaceArchiveWriteError) as excinfo:
        await session.hydrate_workspace(BadStream())

    assert excinfo.value.context["reason"] == "non_bytes_tar_payload"


async def test_accepts_a_clean_archive(session, workspace: Path) -> None:
    archive = make_tar({"./nested/ok.txt": b"safe"})

    await session.hydrate_workspace(io.BytesIO(archive))

    assert (workspace / "nested" / "ok.txt").read_bytes() == b"safe"


async def test_persist_maps_a_failing_tar(session, workspace: Path) -> None:
    """No workspace root means ``tar -c -C`` exits non-zero; that must be mapped."""

    shutil.rmtree(workspace)

    with pytest.raises(WorkspaceArchiveReadError) as excinfo:
        await session.persist_workspace()

    assert excinfo.value.context["exit_code"] != 0


async def test_persist_excludes_the_archive_staging_area(session, workspace: Path) -> None:
    """Staging lives outside the root precisely so it never lands in a snapshot."""

    import tarfile

    await session.write(workspace / "kept.txt", io.BytesIO(b"kept"))
    archive = await session.persist_workspace()

    with tarfile.open(fileobj=io.BytesIO(archive.read()), mode="r:*") as tar:
        names = {member.name for member in tar.getmembers()}

    assert "./kept.txt" in names
    assert not any("agents-k8s-sandbox" in name for name in names)
