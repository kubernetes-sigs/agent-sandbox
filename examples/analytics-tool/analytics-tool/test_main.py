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

import asyncio
import json
import subprocess
from unittest.mock import patch, MagicMock

import pytest
from fastapi.testclient import TestClient

import main
from main import app

client = TestClient(app)


@pytest.fixture(autouse=True)
def working_dir(tmp_path, monkeypatch):
    """Redirect WORKING_DIR to a scratch directory for every test.

    main.WORKING_DIR is computed once at import time from os.path.isdir("/app"),
    which would otherwise point at the real repo checkout when tests run
    outside a container.
    """
    monkeypatch.setattr(main, "WORKING_DIR", str(tmp_path))
    return tmp_path


def test_health_check():
    response = client.get("/")
    assert response.status_code == 200
    assert response.json() == {"status": "ok", "message": "Sandbox Runtime is active."}


@patch("main.subprocess.run")
def test_execute_allowed_command_success(mock_run, working_dir):
    mock_run.return_value = MagicMock(stdout="hi\n", stderr="", returncode=0)

    response = client.post("/execute", json={"command": "echo hi"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "hi\n", "stderr": "", "exit_code": 0}
    mock_run.assert_called_once()
    called_args, called_kwargs = mock_run.call_args
    assert called_args[0] == ["echo", "hi"]
    assert called_kwargs["cwd"] == str(working_dir)
    assert called_kwargs["timeout"] == 30


@patch("main.subprocess.run")
def test_execute_forbidden_command_is_rejected(mock_run):
    response = client.post("/execute", json={"command": "rm -rf /"})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 1
    assert "Forbidden command: 'rm'" in body["stderr"]
    mock_run.assert_not_called()


@patch("main.subprocess.run")
def test_execute_malformed_syntax_is_rejected(mock_run):
    response = client.post("/execute", json={"command": "echo 'unterminated"})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 1
    assert "Malformed command syntax" in body["stderr"]
    mock_run.assert_not_called()


@patch("main.subprocess.run")
def test_execute_empty_command_is_rejected(mock_run):
    response = client.post("/execute", json={"command": "   "})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "No command provided", "exit_code": 1}
    mock_run.assert_not_called()


@patch("main.subprocess.run")
def test_execute_timeout_is_reported(mock_run):
    mock_run.side_effect = subprocess.TimeoutExpired(cmd=["ls"], timeout=30)

    response = client.post("/execute", json={"command": "ls"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "Command timed out", "exit_code": 124}


@patch("main.subprocess.run")
def test_execute_unexpected_error_is_reported(mock_run):
    mock_run.side_effect = OSError("boom")

    response = client.post("/execute", json={"command": "ls"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "boom", "exit_code": 1}


@patch("main.shutil.which")
@patch("main.subprocess.run")
def test_execute_python_success(mock_run, mock_which, working_dir):
    mock_which.side_effect = lambda name: "/usr/bin/python3" if name == "python3" else None
    mock_run.return_value = MagicMock(stdout="42\n", stderr="", returncode=0)

    response = client.post("/execute-python", json={"code": "print(42)"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "42\n", "stderr": "", "exit_code": 0}
    called_args, called_kwargs = mock_run.call_args
    assert called_args[0] == ["/usr/bin/python3", "-c", "print(42)"]
    assert called_kwargs["cwd"] == str(working_dir)


@patch("main.shutil.which", return_value=None)
@patch("main.subprocess.run")
def test_execute_python_no_interpreter_found(mock_run, mock_which):
    response = client.post("/execute-python", json={"code": "print(1)"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "Python interpreter not found", "exit_code": 1}
    mock_run.assert_not_called()


@patch("main.shutil.which", return_value="/usr/bin/python3")
@patch("main.subprocess.run")
def test_execute_python_timeout_is_reported(mock_run, mock_which):
    mock_run.side_effect = subprocess.TimeoutExpired(cmd=["python3"], timeout=30)

    response = client.post("/execute-python", json={"code": "while True: pass"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "Command timed out", "exit_code": 124}


def test_upload_file_writes_to_working_dir(working_dir):
    response = client.post("/upload", files={"file": ("hello.txt", b"hello world")})

    assert response.status_code == 200
    assert "uploaded successfully" in response.json()["message"]
    assert (working_dir / "hello.txt").read_bytes() == b"hello world"


def test_upload_file_creates_parent_directories(working_dir):
    response = client.post("/upload", files={"file": ("sub/dir/file.txt", b"nested")})

    assert response.status_code == 200
    assert (working_dir / "sub" / "dir" / "file.txt").read_bytes() == b"nested"


def test_upload_file_rejects_path_traversal(working_dir):
    response = client.post("/upload", files={"file": ("../escape.txt", b"pwned")})

    assert response.status_code == 400
    assert response.json() == {"message": "Invalid file path"}
    assert not (working_dir.parent / "escape.txt").exists()


def test_download_file_returns_existing_file(working_dir):
    (working_dir / "report.txt").write_text("contents")

    response = client.get("/download/report.txt")

    assert response.status_code == 200
    assert response.content == b"contents"


def test_download_file_missing_returns_404(working_dir):
    response = client.get("/download/missing.txt")
    assert response.status_code == 404


def test_download_file_rejects_path_traversal(working_dir):
    # Called directly rather than via the TestClient: the HTTP client
    # normalizes "../" out of URL paths before the request is even sent, so
    # going through client.get() here would test URL normalization instead
    # of main.download_file's own is_relative_to guard.
    result = asyncio.run(main.download_file("../etc/passwd"))

    assert result.status_code == 400
    assert json.loads(result.body) == {"message": "Invalid file path"}
