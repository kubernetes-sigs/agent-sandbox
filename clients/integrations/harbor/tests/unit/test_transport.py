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
    clock = itertools.chain([0.0], itertools.repeat(100.0))
    monkeypatch.setattr(mod, "_now", lambda: next(clock))

    async def no_sleep(_):
        return None

    monkeypatch.setattr(mod.asyncio, "sleep", no_sleep)
    transport.responses.append(ApiException(status=500, reason="container not found"))
    with pytest.raises(TimeoutError, match="container not found"):
        await transport.wait_ready(timeout_s=60)


async def test_upload_tar_appends_a_marker_and_confirms_it(transport, tmp_path):
    import io
    import tarfile

    (tmp_path / "f.txt").write_text("hi")
    data = archive.pack_dir(tmp_path)
    transport.responses.extend([FakeResp(), FakeResp(returncode=0)])   # upload stream, marker check
    await transport.upload_tar(data, "/tests")
    upload, check = transport.calls
    assert upload["stdin"] is True and upload["binary"] is True
    assert upload["command"][2] == "mkdir -p /tests && tar xf - -C /tests"
    with tarfile.open(fileobj=io.BytesIO(upload["resp"].stdin), mode="r:*") as tar:
        names = tar.getnames()
    assert "f.txt" in names and any(n.startswith(".k8s-agent-sandbox-harbor-") for n in names)
    assert "test -e /tests/.k8s-agent-sandbox-harbor-" in check["command"][2]


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
    transport.responses.append(FakeResp(stderr=b"tar: cannot cd", returncode=2))
    with pytest.raises(RuntimeError, match="cannot cd"):
        await transport.download_tar("/missing")


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
