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
from agents.run_config import SandboxArchiveLimits
from agents.sandbox.errors import WorkspaceArchiveReadError, WorkspaceArchiveWriteError
from agents.sandbox.manifest import Manifest

from fake_k8s import FakeAsyncSandboxClient
from openai_agents_k8s_sandbox import K8sSandboxClient
from support import NAMESPACE, make_options, make_tar


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


class _CountingStream(io.IOBase):
    """Hands out a fixed chunk on demand and records how much has been consumed."""

    def __init__(self, chunk: bytes, chunks: int) -> None:
        self.chunk = chunk
        self.remaining = chunks
        self.served = 0

    def read(self, size: int = -1) -> bytes:  # type: ignore[override]
        if self.remaining <= 0:
            return b""
        self.remaining -= 1
        self.served += len(self.chunk)
        return self.chunk


async def test_rejects_an_oversized_archive_before_buffering_it_all(session) -> None:
    """The archive is held in memory, so the ceiling has to bite mid-stream."""

    limit = 64 * 1024
    chunk = b"\0" * io.DEFAULT_BUFFER_SIZE
    # Far more than the limit: draining all of it is exactly the failure mode.
    stream = _CountingStream(chunk, chunks=4096)
    session._set_archive_limits(SandboxArchiveLimits(max_input_bytes=limit))

    with pytest.raises(WorkspaceArchiveWriteError) as excinfo:
        await session.hydrate_workspace(stream)

    assert excinfo.value.context["reason"] == "archive input size exceeds limit"
    assert excinfo.value.context["limit"] == limit

    # It stopped as soon as the limit was crossed rather than reading the whole stream.
    assert stream.served <= limit + len(chunk)
    assert stream.remaining > 0


async def test_persist_rejects_an_oversized_workspace(session, workspace: Path) -> None:
    """The snapshot is buffered whole, and hydrate would refuse it on the way back in."""

    limit = 4 * 1024
    await session.write(workspace / "big.bin", io.BytesIO(b"x" * (64 * 1024)))
    session._set_archive_limits(SandboxArchiveLimits(max_input_bytes=limit))

    with pytest.raises(WorkspaceArchiveReadError) as excinfo:
        await session.persist_workspace()

    assert excinfo.value.context["reason"] == "archive size exceeds limit"
    assert excinfo.value.context["limit"] == limit
    assert excinfo.value.context["actual"] > limit


async def test_persist_stays_within_a_generous_limit(session, workspace: Path) -> None:
    session._set_archive_limits(SandboxArchiveLimits(max_input_bytes=8 * 1024 * 1024))
    await session.write(workspace / "kept.txt", io.BytesIO(b"kept"))

    assert (await session.persist_workspace()).read()


async def test_unlimited_by_default(session, workspace: Path) -> None:
    """Limits are opt-in; without them hydration behaves as it always did."""

    assert session._inner._max_archive_input_bytes() is None

    await session.hydrate_workspace(io.BytesIO(make_tar({"./ok.txt": b"safe"})))
    assert (workspace / "ok.txt").read_bytes() == b"safe"


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


async def test_hydrate_reports_a_staging_cleanup_failure(
    session, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Staging is shared state in the pod's /tmp, so a leak there just accumulates."""

    sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
    sandbox.fail_commands_matching = f"rm -f -- {session._inner._ARCHIVE_STAGING_DIR}"

    with pytest.raises(WorkspaceArchiveWriteError) as excinfo:
        await session.hydrate_workspace(io.BytesIO(make_tar({"./ok.txt": b"safe"})))
    sandbox.fail_commands_matching = None

    assert excinfo.value.context["reason"] == "staging cleanup failed"
    # The extraction itself went through; only its staging tar could not be swept.
    assert (workspace / "ok.txt").read_bytes() == b"safe"


async def test_persist_reports_a_staging_cleanup_failure(
    session, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
    sandbox.fail_commands_matching = f"rm -f -- {session._inner._ARCHIVE_STAGING_DIR}"

    with pytest.raises(WorkspaceArchiveReadError) as excinfo:
        await session.persist_workspace()
    sandbox.fail_commands_matching = None

    assert excinfo.value.context["reason"] == "staging cleanup failed"


async def test_persist_excludes_the_archive_staging_area(session, workspace: Path) -> None:
    """Staging lives outside the root precisely so it never lands in a snapshot."""

    import tarfile

    await session.write(workspace / "kept.txt", io.BytesIO(b"kept"))
    archive = await session.persist_workspace()

    with tarfile.open(fileobj=io.BytesIO(archive.read()), mode="r:*") as tar:
        names = {member.name for member in tar.getmembers()}

    assert "./kept.txt" in names
    assert not any("agents-k8s-sandbox" in name for name in names)
