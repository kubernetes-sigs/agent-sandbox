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

import os
import shlex
import signal
import subprocess
from unittest.mock import patch, MagicMock

import pytest
from fastapi.testclient import TestClient

from main import app, get_safe_path

client = TestClient(app)


def test_health_check():
    response = client.get("/")
    assert response.status_code == 200
    assert response.json() == {"status": "ok", "message": "Sandbox Runtime is active."}


class TestGetSafePath:
    def test_allows_relative_path_within_base(self):
        base_dir = os.path.realpath("/app")
        assert get_safe_path("foo/bar.txt") == os.path.join(base_dir, "foo", "bar.txt")

    def test_strips_leading_slashes(self):
        base_dir = os.path.realpath("/app")
        assert get_safe_path("///foo.txt") == os.path.join(base_dir, "foo.txt")

    def test_rejects_parent_directory_traversal(self):
        with pytest.raises(ValueError):
            get_safe_path("../../etc/passwd")

    def test_rejects_traversal_hidden_inside_path(self):
        with pytest.raises(ValueError):
            get_safe_path("foo/../../bar")


def _mock_process(mock_popen, stdout="", stderr="", returncode=0, pid=1234):
    """Configures mock_popen (a patched main.subprocess.Popen) to behave like
    a Popen used as a context manager, and returns the inner process mock.
    """
    mock_process = MagicMock()
    mock_process.communicate.return_value = (stdout, stderr)
    mock_process.returncode = returncode
    mock_process.pid = pid
    mock_popen.return_value.__enter__.return_value = mock_process
    return mock_process


@patch('main.subprocess.Popen')
def test_execute_command_success(mock_popen):
    _mock_process(mock_popen, stdout="hello\n", stderr="", returncode=0)

    with patch.dict(os.environ):
        os.environ.pop("SANDBOX_EXEC_TIMEOUT_SECONDS", None)
        response = client.post("/execute", json={"command": "echo hello"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "hello\n", "stderr": "", "exit_code": 0}

    mock_popen.assert_called_once()
    called_args, called_kwargs = mock_popen.call_args
    assert called_args[0] == shlex.split("echo hello")
    assert called_kwargs["cwd"] == "/app"
    assert called_kwargs["start_new_session"] is True
    mock_popen.return_value.__enter__.return_value.communicate.assert_called_once_with(timeout=300.0)


@patch('main.subprocess.Popen')
def test_execute_command_uses_configured_timeout(mock_popen):
    mock_process = _mock_process(mock_popen)

    with patch.dict(os.environ, {"SANDBOX_EXEC_TIMEOUT_SECONDS": "5"}):
        response = client.post("/execute", json={"command": "echo hello"})

    assert response.status_code == 200
    mock_process.communicate.assert_called_once_with(timeout=5.0)


@patch('main.subprocess.Popen')
def test_execute_command_falls_back_to_default_on_invalid_timeout(mock_popen):
    mock_process = _mock_process(mock_popen)

    with patch.dict(os.environ, {"SANDBOX_EXEC_TIMEOUT_SECONDS": "not-a-number"}):
        response = client.post("/execute", json={"command": "echo hello"})

    assert response.status_code == 200
    mock_process.communicate.assert_called_once_with(timeout=300.0)


@patch('main.os.killpg')
@patch('main.os.getpgid', return_value=4321)
@patch('main.subprocess.Popen')
def test_execute_command_timeout_returns_failed_execution(mock_popen, mock_getpgid, mock_killpg):
    mock_process = _mock_process(mock_popen, pid=4321)
    mock_process.communicate.side_effect = [
        subprocess.TimeoutExpired(cmd="sleep infinity", timeout=300),
        ("", ""),
    ]

    response = client.post("/execute", json={"command": "sleep infinity"})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 1
    assert "Failed to execute command" in body["stderr"]
    assert "timed out" in body["stderr"]
    mock_getpgid.assert_called_once_with(4321)
    mock_killpg.assert_called_once_with(4321, signal.SIGKILL)


def test_execute_command_invalid_syntax_returns_error():
    # An unterminated quote makes shlex.split raise, which the handler
    # catches and reports as a failed execution rather than a 500.
    response = client.post("/execute", json={"command": "echo 'unterminated"})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 1
    assert "Failed to execute command" in body["stderr"]


def test_upload_file_writes_to_safe_path(tmp_path):
    target = tmp_path / "uploaded.txt"

    with patch('main.get_safe_path', return_value=str(target)):
        response = client.post("/upload", files={"file": ("uploaded.txt", b"hello world")})

    assert response.status_code == 200
    assert target.read_bytes() == b"hello world"


def test_upload_file_rejects_path_traversal():
    with patch('main.get_safe_path', side_effect=ValueError("Access denied")):
        response = client.post("/upload", files={"file": ("../evil.txt", b"pwned")})

    assert response.status_code == 403
    assert response.json() == {"message": "Access denied"}


def test_download_file_returns_existing_file(tmp_path):
    target = tmp_path / "report.txt"
    target.write_text("contents")

    with patch('main.get_safe_path', return_value=str(target)):
        response = client.get("/download/report.txt")

    assert response.status_code == 200
    assert response.content == b"contents"


def test_download_file_missing_returns_404(tmp_path):
    target = tmp_path / "missing.txt"

    with patch('main.get_safe_path', return_value=str(target)):
        response = client.get("/download/missing.txt")

    assert response.status_code == 404


def test_download_file_rejects_path_traversal():
    with patch('main.get_safe_path', side_effect=ValueError("Access denied")):
        response = client.get("/download/etc/passwd")

    assert response.status_code == 403


def test_list_files_returns_directory_entries(tmp_path):
    (tmp_path / "a.txt").write_text("a")
    (tmp_path / "sub").mkdir()

    with patch('main.get_safe_path', return_value=str(tmp_path)):
        response = client.get("/list/somedir")

    assert response.status_code == 200
    names = {entry["name"] for entry in response.json()}
    assert names == {"a.txt", "sub"}


def test_list_files_rejects_non_directory(tmp_path):
    target = tmp_path / "file.txt"
    target.write_text("x")

    with patch('main.get_safe_path', return_value=str(target)):
        response = client.get("/list/file.txt")

    assert response.status_code == 404


def test_list_files_rejects_path_traversal():
    with patch('main.get_safe_path', side_effect=ValueError("Access denied")):
        response = client.get("/list/etc")

    assert response.status_code == 403


def test_exists_true_for_present_file(tmp_path):
    target = tmp_path / "present.txt"
    target.write_text("x")

    with patch('main.get_safe_path', return_value=str(target)):
        response = client.get("/exists/present.txt")

    assert response.status_code == 200
    assert response.json() == {"path": "present.txt", "exists": True}


def test_exists_false_for_missing_file(tmp_path):
    missing = tmp_path / "absent.txt"

    with patch('main.get_safe_path', return_value=str(missing)):
        response = client.get("/exists/absent.txt")

    assert response.status_code == 200
    assert response.json() == {"path": "absent.txt", "exists": False}


def test_exists_rejects_path_traversal():
    with patch('main.get_safe_path', side_effect=ValueError("Access denied")):
        response = client.get("/exists/etc/passwd")

    assert response.status_code == 403
