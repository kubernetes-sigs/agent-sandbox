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

"""The pod exec transport: command assembly, output handling, tar helpers."""

from __future__ import annotations

import concurrent.futures
import itertools
import shlex
import types
from pathlib import Path

import pytest
from kubernetes.client.rest import ApiException

from k8s_agent_sandbox_harbor.transport import archive
from k8s_agent_sandbox_harbor.transport.podexec import (ExecStreamClosedError, PodExecTransport,
                                                        PodGoneError, build_command)


def _unwrap(argv: list[str]) -> str:
    assert argv[:2] == ["sh", "-c"]
    return argv[2]


def test_build_command_plain():
    assert _unwrap(build_command("echo hi")) == "bash -c 'echo hi'"


def test_build_command_env_cwd_user_timeout_nesting():
    cmd = _unwrap(build_command("make test", cwd="/app", env={"A": "x y", "B": "2"}, user="agent",
                                timeout_sec=30))
    # timeout is outermost, then su, then cd, then the env prefix, then bash -c.
    assert cmd.startswith("if command -v timeout >/dev/null 2>&1; then exec timeout -k 5 30 sh -c ")
    inner = shlex.split(cmd.split("then exec timeout -k 5 30 sh -c ", 1)[1].split("; else", 1)[0])[0]
    assert inner.startswith("su agent -s /bin/bash -c ")
    innermost = shlex.split(inner.split("-c ", 1)[1])[0]
    assert innermost == "cd /app && A='x y' B=2 bash -c 'make test'"


def test_build_command_numeric_user_uses_getent():
    assert "$(getent passwd 1000 | cut -d: -f1)" in _unwrap(build_command("id", user=1000))


class FakeResp:
    """Mimics the kubernetes websocket client used by the transport."""

    def __init__(self, stdout="", stderr="", returncode=0, raise_on_status=False):
        self._chunks = [(stdout, stderr)]
        self._rc = returncode
        self._open = True
        self.closed = False
        self.raise_on_status = raise_on_status
        self.stdin = b""

    def is_open(self):
        return self._open

    def update(self, timeout=None):
        self._open = False

    def peek_stdout(self):
        return bool(self._chunks and self._chunks[0][0])

    def read_stdout(self):
        out, err = self._chunks[0]
        self._chunks[0] = ("", err)
        return out

    def peek_stderr(self):
        return bool(self._chunks and self._chunks[0][1])

    def read_stderr(self):
        out, err = self._chunks[0]
        self._chunks[0] = (out, "")
        return err

    def run_forever(self, timeout=None):
        self._open = False

    def close(self):
        self.closed = True

    def write_stdin(self, data):
        self.stdin += data

    @property
    def returncode(self):
        if self.raise_on_status:
            raise ValueError("no status")
        return self._rc


@pytest.fixture
def transport(monkeypatch):
    calls: list[dict] = []
    responses: list = []

    def fake_stream(fn, pod, namespace, **kwargs):
        resp = responses.pop(0)
        calls.append({"pod": pod, "namespace": namespace, "resp": resp, **kwargs})
        if isinstance(resp, Exception):
            raise resp
        return resp

    import k8s_agent_sandbox_harbor.transport.podexec as mod
    monkeypatch.setattr(mod, "stream", fake_stream)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=2)
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, max_output_bytes=16)
    t.calls, t.responses = calls, responses  # type: ignore[attr-defined]
    yield t
    ex.shutdown(wait=False)


async def test_exec_returns_streams_and_code_and_names_the_container(transport):
    transport.responses.append(FakeResp(stdout="out", stderr="err", returncode=3))
    r = await transport.exec("x", cwd="/app")
    assert (r.stdout, r.stderr, r.return_code) == ("out", "err", 3)
    call = transport.calls[0]
    assert call["container"] == "main" and call["tty"] is False and call["stdin"] is False
    assert call["command"][2].startswith("cd /app && ")
    assert transport.exec_count == 1


async def test_exec_truncates_output_when_capped(transport):
    transport.responses.append(FakeResp(stdout="x" * 40))
    r = await transport.exec("x")
    assert r.stdout.startswith("x" * 16) and "truncated" in r.stdout


async def test_exec_without_exit_status_raises(transport):
    transport.responses.append(FakeResp(raise_on_status=True))
    with pytest.raises(ExecStreamClosedError):
        await transport.exec("x")


async def test_exec_maps_404_to_pod_gone(transport):
    transport.responses.append(ApiException(status=404, reason="Not Found"))
    with pytest.raises(PodGoneError):
        await transport.exec("x")


async def test_wait_ready_retries_then_succeeds(transport, monkeypatch):
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    async def no_sleep(_):
        return None

    monkeypatch.setattr(mod.asyncio, "sleep", no_sleep)
    transport.responses.extend([ApiException(status=500, reason="container not found"), FakeResp()])
    await transport.wait_ready(timeout_s=60)
    assert len(transport.calls) == 2 and transport.calls[0]["command"] == ["true"]


async def test_wait_ready_times_out_with_last_error(transport, monkeypatch):
    import k8s_agent_sandbox_harbor.transport.podexec as mod
    clock = itertools.chain([0.0, 0.0], itertools.repeat(100.0))   # start, first budget check
    monkeypatch.setattr(mod, "_now", lambda: next(clock))

    async def no_sleep(_):
        return None

    monkeypatch.setattr(mod.asyncio, "sleep", no_sleep)
    transport.responses.append(ApiException(status=500, reason="container not found"))
    with pytest.raises(TimeoutError, match="container not found"):
        await transport.wait_ready(timeout_s=60)


async def test_upload_tar_writes_the_marker_after_tar_and_confirms_it(transport, tmp_path):
    (tmp_path / "f.txt").write_text("hi")
    data = archive.pack_dir(tmp_path)
    transport.responses.extend([FakeResp(), FakeResp(returncode=0)])   # upload stream, marker check
    await transport.upload_tar(data, "/tests")
    upload, check = transport.calls
    assert upload["stdin"] is True and upload["binary"] is True
    command = upload["command"][2]
    assert command.startswith("mkdir -p /tests && tar xf - -C /tests && : > /tests/.k8s-agent-sandbox-harbor-")
    marker = command.rsplit(" ", 1)[1]
    # The archive goes through unchanged: the marker is not one of its members.
    assert upload["resp"].stdin == data
    assert check["command"][2] == f"bash -c 'test -e {marker} && rm -f {marker}'"


def test_upload_command_leaves_no_marker_when_a_member_fails(tmp_path):
    """tar extracts the members after one that fails, so a marker inside the
    archive would report a partial upload as complete."""
    import io
    import subprocess
    import tarfile

    from k8s_agent_sandbox_harbor.transport.podexec import upload_command

    def run(target: Path) -> subprocess.CompletedProcess:
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as tar:
            for name in ("conflict/earlier.txt", "later.txt"):
                info = tarfile.TarInfo(name)
                info.size = 2
                tar.addfile(info, io.BytesIO(b"hi"))
        return subprocess.run(["sh", "-c", upload_command(str(target), str(target / ".marker"))],
                              input=buf.getvalue(), capture_output=True)

    bad = tmp_path / "bad"
    bad.mkdir()
    (bad / "conflict").write_text("a file where the archive has a directory")
    assert run(bad).returncode != 0
    assert (bad / "later.txt").exists() and not (bad / ".marker").exists()

    good = tmp_path / "good"
    assert run(good).returncode == 0
    assert (good / "conflict" / "earlier.txt").exists() and (good / ".marker").exists()


async def test_upload_tar_fails_at_once_when_tar_reports_an_error(transport, tmp_path):
    (tmp_path / "f").write_text("x")
    transport.responses.append(FakeResp(stderr=b"tar: conflict/earlier.txt: Cannot open", returncode=2))
    with pytest.raises(RuntimeError, match=r"failed \(exit 2\): tar: conflict/earlier.txt"):
        await transport.upload_tar(archive.pack_dir(tmp_path), "/tests")
    assert len(transport.calls) == 1                       # no marker poll after a known failure


async def test_upload_tar_fails_when_marker_never_appears(transport, tmp_path, monkeypatch):
    import k8s_agent_sandbox_harbor.transport.podexec as mod
    clock = itertools.chain([0.0], itertools.repeat(100.0))
    monkeypatch.setattr(mod, "_now", lambda: next(clock))
    (tmp_path / "f").write_text("x")
    transport.responses.extend([FakeResp(stderr=b"tar: short read"), FakeResp(returncode=1)])
    with pytest.raises(RuntimeError, match="did not complete"):
        await transport.upload_tar(archive.pack_dir(tmp_path), "/tests")


async def test_download_tar_reads_binary_and_rejects_failures(transport):
    transport.responses.append(FakeResp(stdout=b"TARDATA", returncode=0))
    assert await transport.download_tar("/logs/verifier") == b"TARDATA"
    assert transport.calls[0]["command"][2] == "cd /logs/verifier && tar cf - ."
    transport.responses.append(FakeResp(stderr=b"sh: 1: cd: can't cd to /missing", returncode=2))
    with pytest.raises(FileNotFoundError, match="does not exist"):
        await transport.download_tar("/missing")
    transport.responses.append(FakeResp(stderr=b"tar: write error", returncode=2))
    with pytest.raises(RuntimeError, match="write error"):
        await transport.download_tar("/logs")


def test_unpack_skips_a_member_the_data_filter_rejects(tmp_path: Path, caplog):
    """A symlink with an absolute target must not stop the download: the
    members after it are still extracted."""
    import io
    import tarfile

    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        link = tarfile.TarInfo("latest")
        link.type = tarfile.SYMTYPE
        link.linkname = "/logs/verifier/reward.txt"
        tar.addfile(link)
        info = tarfile.TarInfo("reward.txt")
        info.size = 1
        tar.addfile(info, io.BytesIO(b"1"))
    with caplog.at_level("WARNING"):
        archive.unpack(buf.getvalue(), tmp_path / "out")
    assert (tmp_path / "out" / "reward.txt").read_text() == "1"
    assert not (tmp_path / "out" / "latest").is_symlink()
    assert "not extracting latest" in caplog.text


def test_archive_round_trip(tmp_path: Path):
    src = tmp_path / "src"
    (src / "sub").mkdir(parents=True)
    (src / "a.sh").write_text("echo a")
    (src / "a.sh").chmod(0o755)
    (src / "sub" / "b").write_text("b")
    data = archive.pack_dir(src)
    out = tmp_path / "out"
    archive.unpack(data, out)
    assert (out / "sub" / "b").read_text() == "b" and (out / "a.sh").stat().st_mode & 0o100
    single = archive.pack_file(src / "a.sh", "renamed.sh")
    archive.unpack(single, tmp_path / "one")
    assert (tmp_path / "one" / "renamed.sh").read_text() == "echo a"


async def test_exec_waits_for_a_slot_before_its_timeout_starts(monkeypatch):
    """A queued call must not be reported as timed out: the semaphore is taken
    outside wait_for, so the timer covers only the running command."""
    import asyncio
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    slots = asyncio.Semaphore(1)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    calls: list[str] = []

    def fake_stream(fn, pod, namespace, **kwargs):
        resp = FakeResp(stdout="ok")
        calls.append(kwargs["command"][2])
        return resp

    monkeypatch.setattr(mod, "stream", fake_stream)
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, slots=slots)
    await slots.acquire()                                  # the one slot is busy
    task = asyncio.create_task(t.exec("echo hi", timeout_sec=1))
    await asyncio.sleep(1.3)                               # longer than timeout + nothing ran
    assert not task.done() and calls == []
    slots.release()
    result = await asyncio.wait_for(task, timeout=5)
    assert result.return_code == 0 and result.stdout == "ok"
    ex.shutdown(wait=False)


async def test_timed_out_exec_closes_the_stream_off_the_exec_pool(monkeypatch):
    """With the single exec thread hung, the close must still run (on the
    default executor) and a failing close must not hide the timeout result."""
    import asyncio
    import threading
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    release = threading.Event()

    class HangingResp(FakeResp):
        def is_open(self):
            return not release.is_set()

        def update(self, timeout=None):
            release.wait(0.05)

        def close(self):
            release.set()
            raise RuntimeError("close failed")

    monkeypatch.setattr(mod, "stream", lambda fn, pod, ns, **kw: HangingResp())
    monkeypatch.setattr(mod, "_LOCAL_TIMEOUT_GRACE_S", 0)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, slots=asyncio.Semaphore(1))
    try:
        result = await asyncio.wait_for(t.exec("sleep 100", timeout_sec=1), timeout=5)
        assert result.return_code == 124 and release.is_set()
    finally:
        release.set()                                      # never leave the worker hung
        ex.shutdown(wait=True)


async def test_cancelled_exec_closes_its_stream_and_frees_the_thread(monkeypatch):
    """A cancelled call must give its thread back: with one exec thread, the
    next call runs instead of timing out behind the abandoned stream."""
    import asyncio
    import threading
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    class OpenResp(FakeResp):
        def update(self, timeout=None):                    # the command never ends
            import time
            time.sleep(0.02)

    hung = OpenResp()
    responses = [hung, FakeResp(stdout="next")]
    opened = threading.Event()

    def fake_stream(fn, pod, ns, **kw):
        resp = responses.pop(0)
        opened.set()
        return resp

    monkeypatch.setattr(mod, "stream", fake_stream)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, slots=asyncio.Semaphore(1))
    try:
        task = asyncio.create_task(t.exec("sleep 300"))
        # Cancel only once the first call holds the hung stream, so the second
        # call cannot take it whatever the scheduling.
        assert await asyncio.get_running_loop().run_in_executor(None, opened.wait, 5)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        result = await asyncio.wait_for(t.exec("echo next", timeout_sec=1), timeout=5)
        assert (result.return_code, result.stdout) == (0, "next")
        assert hung.closed
    finally:
        hung._open = False                                 # never leave the worker hung
        ex.shutdown(wait=True)


def test_connect_gets_a_default_timeout_only_when_none_is_set(monkeypatch):
    import websocket
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    seen: list[dict] = []

    def fake_connect(self, url, **options):
        seen.append(options)

    monkeypatch.setattr(websocket.WebSocket, "connect", fake_connect)
    monkeypatch.setattr(mod, "_connect_wrapped", False)
    mod.ensure_connect_timeout(12)
    mod.ensure_connect_timeout(12)                          # idempotent: wraps once

    class Sock:
        def __init__(self, timeout=None):
            self.sock_opt = types.SimpleNamespace(timeout=timeout)

    websocket.WebSocket.connect(Sock(), "wss://x")          # another library's socket: untouched
    mod._connect_scope.active = True                        # as while the transport opens a stream
    try:
        websocket.WebSocket.connect(Sock(), "wss://x")          # no timeout anywhere -> default
        websocket.WebSocket.connect(Sock(), "wss://x", timeout=3)   # caller's wins
        websocket.WebSocket.connect(Sock(timeout=5), "wss://x")     # socket's wins
    finally:
        mod._connect_scope.active = False
    assert [o.get("timeout") for o in seen] == [None, 12, 3, None]
    assert websocket.WebSocket.connect.__wrapped__ is fake_connect


async def test_wait_ready_probe_takes_a_slot(monkeypatch):
    import asyncio
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    slots = asyncio.Semaphore(1)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    probes: list[int] = []
    monkeypatch.setattr(mod, "stream", lambda fn, pod, ns, **kw: (probes.append(1), FakeResp())[1])
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, slots=slots)
    await slots.acquire()                                  # the one slot is busy
    task = asyncio.create_task(t.wait_ready(timeout_s=60))
    await asyncio.sleep(0.3)
    assert probes == [] and not task.done()                # no probe without a slot
    slots.release()
    await asyncio.wait_for(task, timeout=5)
    assert probes == [1]
    ex.shutdown(wait=True)


async def test_wait_ready_counts_the_slot_wait_against_its_deadline(monkeypatch):
    """A probe that cannot get a slot must fail at the deadline, not wait for
    as long as another call holds the slot."""
    import asyncio
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    slots = asyncio.Semaphore(1)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    probes: list[int] = []
    monkeypatch.setattr(mod, "stream", lambda fn, pod, ns, **kw: (probes.append(1), FakeResp())[1])
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, slots=slots)
    await slots.acquire()                                  # held for the whole test
    with pytest.raises(TimeoutError, match="no exec slot was free"):
        await asyncio.wait_for(t.wait_ready(timeout_s=0.2), timeout=5)
    assert probes == []
    slots.release()
    await asyncio.wait_for(t.wait_ready(timeout_s=5), timeout=5)   # the slot was not leaked
    assert probes == [1]
    ex.shutdown(wait=True)


async def test_exec_stops_keeping_output_past_the_cap(monkeypatch):
    """The cap bounds memory while the stream runs, not only the result."""
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    class ChattyResp(FakeResp):
        def __init__(self):
            super().__init__()
            self.left = 500
            self._all = object()                           # the client's duplicate capture

        def _frames_immediately_available(self):           # newer clients: drain all in one update
            return True

        def is_open(self):
            return self.left > 0

        def update(self, timeout=None):
            self.left -= 1

        def peek_stdout(self):
            return True

        def read_stdout(self):
            return "x" * 10

    resp = ChattyResp()
    monkeypatch.setattr(mod, "stream", lambda fn, pod, ns, **kw: resp)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, max_output_bytes=16)
    outputs: list = []
    real = mod._Output
    monkeypatch.setattr(mod, "_Output", lambda cap: (outputs.append(real(cap)), outputs[-1])[1])
    r = await t.exec("yes")
    assert r.stdout == "x" * 16 + mod._TRUNCATED_MARK
    assert resp.left == 0                                  # drained to the end
    assert outputs[0]._size <= 16 + 10                     # kept near the cap, not all 5000
    assert isinstance(resp._all, mod._IgnoredIO)           # no second copy of each frame
    assert resp._frames_immediately_available() is False   # one frame per update
    ex.shutdown(wait=True)


async def test_wait_ready_does_not_probe_when_the_slot_comes_free_past_the_deadline(monkeypatch):
    import asyncio
    import k8s_agent_sandbox_harbor.transport.podexec as mod

    slots = asyncio.Semaphore(1)
    ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    probes: list[int] = []
    monkeypatch.setattr(mod, "stream", lambda fn, pod, ns, **kw: (probes.append(1), FakeResp())[1])
    # Start, the budget check before the slot wait, then the check after it.
    clock = itertools.chain([0.0, 0.0], itertools.repeat(100.0))
    monkeypatch.setattr(mod, "_now", lambda: next(clock))
    api = types.SimpleNamespace(connect_get_namespaced_pod_exec=object())
    t = PodExecTransport(api_factory=lambda: api, pod="p", namespace="ns", container="main",
                         executor=ex, slots=slots)
    with pytest.raises(TimeoutError, match="did not accept exec within 60s"):
        await t.wait_ready(timeout_s=60)
    assert probes == [] and not slots.locked()             # no probe, and the slot went back
    ex.shutdown(wait=True)
