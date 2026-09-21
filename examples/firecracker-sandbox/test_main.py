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

"""
Unit tests for the firecracker-sandbox runtime.

main.py runs inside the sandbox pod as a FastAPI server; these tests
exercise its endpoints in-process via FastAPI's TestClient, with
subprocess.Popen mocked for /exec. No kind cluster, Firecracker microVM, or
container build is needed.
"""

import os
import signal
import subprocess
import tempfile
from unittest.mock import MagicMock, patch

import pytest
from fastapi import HTTPException
from fastapi.testclient import TestClient

# main.py creates WORKSPACE (default "/workspace") at import time, which
# isn't writable outside a container. Point it at a scratch directory before
# import; the autouse `workspace` fixture below then redirects main.WORKSPACE
# to a fresh tmp_path for every individual test.
os.environ.setdefault("SANDBOX_WORKSPACE", tempfile.mkdtemp(prefix="firecracker-sandbox-test-"))

import main  # noqa: E402
from main import app  # noqa: E402

client = TestClient(app)


@pytest.fixture(autouse=True)
def workspace(tmp_path, monkeypatch):
    """Redirect WORKSPACE to a scratch directory for every test.

    main.WORKSPACE is computed once at import time from
    SANDBOX_WORKSPACE/"/workspace", which would otherwise point outside the
    test sandbox (and doesn't exist) when tests run outside a container.
    """
    monkeypatch.setattr(main, "WORKSPACE", tmp_path)
    main._user_env.clear()
    return tmp_path


def test_health_returns_204():
    response = client.get("/health")
    assert response.status_code == 204


def test_root():
    response = client.get("/")
    assert response.status_code == 200
    assert response.json() == {"runtime": "firecracker-sandbox", "version": "0.1.0"}


def test_metrics_returns_a_timestamp():
    response = client.get("/metrics")
    assert response.status_code == 200
    assert "timestamp" in response.json()


# --- _safe_path (workspace escape guard) ------------------------------------

class TestSafePath:
    def test_relative_path_inside_workspace(self, workspace):
        resolved = main._safe_path("sub/file.txt")
        assert resolved == (workspace / "sub" / "file.txt").resolve()

    def test_absolute_path_inside_workspace(self, workspace):
        target = workspace / "abs.txt"
        resolved = main._safe_path(str(target))
        assert resolved == target.resolve()

    def test_rejects_relative_traversal(self, workspace):
        with pytest.raises(HTTPException) as exc_info:
            main._safe_path("../escape.txt")
        assert exc_info.value.status_code == 403

    def test_rejects_traversal_hidden_inside_path(self, workspace):
        with pytest.raises(HTTPException) as exc_info:
            main._safe_path("sub/../../escape.txt")
        assert exc_info.value.status_code == 403

    def test_rejects_absolute_path_outside_workspace(self, workspace, tmp_path_factory):
        outside = tmp_path_factory.mktemp("outside") / "escape.txt"
        with pytest.raises(HTTPException) as exc_info:
            main._safe_path(str(outside))
        assert exc_info.value.status_code == 403

    def test_rejects_symlink_escaping_workspace(self, workspace, tmp_path_factory):
        outside_dir = tmp_path_factory.mktemp("outside")
        target_file = outside_dir / "secret.txt"
        target_file.write_text("data")
        link = workspace / "escape-link"
        link.symlink_to(target_file)

        with pytest.raises(HTTPException) as exc_info:
            main._safe_path("escape-link")
        assert exc_info.value.status_code == 403

    def test_allows_symlink_staying_inside_workspace(self, workspace):
        real = workspace / "real.txt"
        real.write_text("data")
        link = workspace / "link.txt"
        link.symlink_to(real)

        assert main._safe_path("link.txt") == real.resolve()


# --- /files (download / upload) ---------------------------------------------

def test_download_file_returns_existing_file(workspace):
    (workspace / "report.txt").write_text("contents")
    response = client.get("/files", params={"path": "report.txt"})
    assert response.status_code == 200
    assert response.content == b"contents"


def test_download_file_missing_returns_404(workspace):
    response = client.get("/files", params={"path": "missing.txt"})
    assert response.status_code == 404


def test_download_file_rejects_path_traversal(workspace):
    response = client.get("/files", params={"path": "../escape.txt"})
    assert response.status_code == 403


def test_upload_file_writes_to_workspace(workspace):
    response = client.post(
        "/files", data={"path": "hello.txt"}, files={"file": ("hello.txt", b"hello world")})

    assert response.status_code == 200
    assert (workspace / "hello.txt").read_bytes() == b"hello world"
    assert response.json() == {"path": "hello.txt", "size": len(b"hello world")}


def test_upload_file_creates_parent_directories(workspace):
    response = client.post(
        "/files", data={"path": "sub/dir/file.txt"}, files={"file": ("file.txt", b"nested")})

    assert response.status_code == 200
    assert (workspace / "sub" / "dir" / "file.txt").read_bytes() == b"nested"


def test_upload_file_rejects_path_traversal(workspace):
    response = client.post(
        "/files", data={"path": "../escape.txt"}, files={"file": ("f.txt", b"pwned")})

    assert response.status_code == 403
    assert not (workspace.parent / "escape.txt").exists()


# --- /init and /envs ---------------------------------------------------------

def test_init_merges_envs_and_computes_skew(workspace, monkeypatch):
    monkeypatch.setattr(main.time, "time", lambda: 1000.0)
    # /init sets os.environ directly (not through monkeypatch), so clean up
    # manually rather than relying on monkeypatch's teardown to catch it.
    var = "FIRECRACKER_TEST_INIT_VAR"
    try:
        response = client.post("/init", json={"envs": {var: "bar"}, "timestamp": 990.0})
        assert response.status_code == 200
        body = response.json()
        assert body["server_time"] == 1000.0
        assert body["skew_seconds"] == 10.0
        assert os.environ[var] == "bar"
        assert main._user_env[var] == "bar"
    finally:
        os.environ.pop(var, None)


def test_init_without_timestamp_has_no_skew(workspace):
    response = client.post("/init", json={})
    assert response.status_code == 200
    assert response.json()["skew_seconds"] is None


def test_envs_includes_injected_user_env(workspace):
    main._user_env["FIRECRACKER_TEST_ENVS_VAR"] = "value"
    try:
        response = client.get("/envs")
        assert response.status_code == 200
        assert response.json()["FIRECRACKER_TEST_ENVS_VAR"] == "value"
    finally:
        main._user_env.pop("FIRECRACKER_TEST_ENVS_VAR", None)


# --- /exec --------------------------------------------------------------------

def _mock_process(mock_popen, stdout="", stderr="", returncode=0, pid=1234):
    mock_process = MagicMock()
    mock_process.communicate.return_value = (stdout, stderr)
    mock_process.returncode = returncode
    mock_process.pid = pid
    mock_popen.return_value = mock_process
    return mock_process


@patch("main.subprocess.Popen")
def test_exec_with_args_runs_without_shell(mock_popen, workspace):
    _mock_process(mock_popen, stdout="hi\n", stderr="", returncode=0)

    response = client.post("/exec", json={"cmd": "echo", "args": ["hi"]})

    assert response.status_code == 200
    assert response.json() == {"stdout": "hi\n", "stderr": "", "exit_code": 0}
    called_args, called_kwargs = mock_popen.call_args
    assert called_args[0] == ["echo", "hi"]
    assert called_kwargs["shell"] is False
    assert called_kwargs["cwd"] == str(workspace)
    assert called_kwargs["start_new_session"] is True


@patch("main.subprocess.Popen")
def test_exec_without_args_runs_via_shell(mock_popen, workspace):
    _mock_process(mock_popen, stdout="hi\nbye\n", stderr="", returncode=0)

    response = client.post("/exec", json={"cmd": "echo hi && echo bye"})

    assert response.status_code == 200
    called_args, called_kwargs = mock_popen.call_args
    assert called_args[0] == "echo hi && echo bye"
    assert called_kwargs["shell"] is True


@patch("main.subprocess.Popen")
def test_exec_empty_args_list_still_runs_without_shell(mock_popen, workspace):
    # args=[] is falsy but not None -- the handler branches on `is not None`,
    # so an explicit empty list must still take the no-shell path.
    _mock_process(mock_popen)

    response = client.post("/exec", json={"cmd": "true", "args": []})

    assert response.status_code == 200
    called_args, called_kwargs = mock_popen.call_args
    assert called_args[0] == ["true"]
    assert called_kwargs["shell"] is False


@patch("main.subprocess.Popen")
def test_exec_merges_custom_env(mock_popen, workspace, monkeypatch):
    monkeypatch.setenv("FIRECRACKER_TEST_PREEXISTING", "1")
    _mock_process(mock_popen)

    response = client.post("/exec", json={"cmd": "true", "args": [], "env": {"CUSTOM": "42"}})

    assert response.status_code == 200
    called_env = mock_popen.call_args.kwargs["env"]
    assert called_env["CUSTOM"] == "42"
    assert called_env["FIRECRACKER_TEST_PREEXISTING"] == "1"


@patch("main.subprocess.Popen")
def test_exec_rejects_cwd_that_is_not_a_directory(mock_popen, workspace):
    (workspace / "file.txt").write_text("x")

    response = client.post("/exec", json={"cmd": "pwd", "cwd": "file.txt"})

    assert response.status_code == 400
    mock_popen.assert_not_called()


@patch("main.subprocess.Popen")
def test_exec_rejects_cwd_path_traversal(mock_popen, workspace):
    response = client.post("/exec", json={"cmd": "pwd", "cwd": "../escape"})

    assert response.status_code == 403
    mock_popen.assert_not_called()


@patch("main.os.killpg")
@patch("main.os.getpgid", return_value=4321)
@patch("main.subprocess.Popen")
def test_exec_timeout_kills_process_group(mock_popen, mock_getpgid, mock_killpg, workspace):
    mock_process = _mock_process(mock_popen, pid=4321)
    mock_process.communicate.side_effect = [
        subprocess.TimeoutExpired(cmd="sleep infinity", timeout=1),
        ("partial out", "partial err"),
    ]

    response = client.post("/exec", json={"cmd": "sleep infinity", "timeout": 1})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == -1
    assert body["stdout"] == "partial out"
    assert "[timeout after 1" in body["stderr"]
    mock_getpgid.assert_called_once_with(4321)
    mock_killpg.assert_called_once_with(4321, signal.SIGKILL)
    assert mock_process.communicate.call_count == 2


@patch("main.os.killpg", side_effect=ProcessLookupError)
@patch("main.os.getpgid", return_value=4321)
@patch("main.subprocess.Popen")
def test_exec_timeout_survives_process_group_already_gone(mock_popen, mock_getpgid, mock_killpg, workspace):
    # The process can exit between TimeoutExpired and killpg (e.g. it
    # finished right at the deadline); killpg racing a gone pgid must not
    # fail the request.
    mock_process = _mock_process(mock_popen, pid=4321)
    mock_process.communicate.side_effect = [
        subprocess.TimeoutExpired(cmd="sleep infinity", timeout=1),
        ("", ""),
    ]

    response = client.post("/exec", json={"cmd": "sleep infinity", "timeout": 1})

    assert response.status_code == 200
    assert response.json()["exit_code"] == -1


@patch("main.subprocess.Popen", side_effect=FileNotFoundError("no such file"))
def test_exec_missing_binary_returns_127(mock_popen, workspace):
    response = client.post("/exec", json={"cmd": "no-such-binary", "args": []})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 127
    assert "no such file" in body["stderr"]


@patch("main.subprocess.Popen", side_effect=OSError("boom"))
def test_exec_unexpected_error_returns_1(mock_popen, workspace):
    response = client.post("/exec", json={"cmd": "true", "args": []})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 1
    assert "boom" in body["stderr"]


def test_exec_rejects_timeout_out_of_bounds(workspace):
    assert client.post("/exec", json={"cmd": "true", "timeout": 0}).status_code == 422
    assert client.post("/exec", json={"cmd": "true", "timeout": 301}).status_code == 422
