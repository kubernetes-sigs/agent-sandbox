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
Unit tests for the n8n-mcp example.

server.py runs inside the sandbox container as a FastAPI server whose
/execute endpoint is the only thing standing between an n8n-triggered
command and the shell (shlex parsing + a command allowlist). These tests
exercise it in-process via FastAPI's TestClient, with subprocess.run
mocked. No kind cluster or container build is needed.
"""

import subprocess
from unittest.mock import MagicMock, patch

import pytest
from fastapi.testclient import TestClient

import server
from server import app

client = TestClient(app)


@pytest.fixture(autouse=True)
def working_dir(tmp_path, monkeypatch):
    """Redirect WORKING_DIR to a scratch directory for every test.

    server.WORKING_DIR is computed once at import time from
    os.path.isdir("/app"), which would otherwise point at the real repo
    checkout when tests run outside a container.
    """
    monkeypatch.setattr(server, "WORKING_DIR", str(tmp_path))
    return tmp_path


def test_health_check():
    response = client.get("/")
    assert response.status_code == 200
    assert response.json() == {"status": "ok", "message": "Sandbox Runtime is active."}


@patch("server.subprocess.run")
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


@pytest.mark.parametrize("cmd", sorted(server.ALLOWED_COMMANDS))
@patch("server.subprocess.run")
def test_execute_every_allowlisted_command_is_accepted(mock_run, cmd):
    mock_run.return_value = MagicMock(stdout="", stderr="", returncode=0)

    response = client.post("/execute", json={"command": cmd})

    assert response.status_code == 200
    assert response.json()["exit_code"] == 0
    mock_run.assert_called_once()


@patch("server.subprocess.run")
def test_execute_forbidden_command_is_rejected(mock_run):
    response = client.post("/execute", json={"command": "rm -rf /"})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 1
    assert "Forbidden command: 'rm'" in body["stderr"]
    mock_run.assert_not_called()


@patch("server.subprocess.run")
def test_execute_rejects_shell_metacharacters_via_allowlist(mock_run):
    # shlex.split treats ";" as a literal argv token (not a shell operator),
    # so "echo hi; rm -rf /" becomes argv=["echo", "hi;", "rm", "-rf", "/"] --
    # a single allowlisted "echo" call with unusual-looking arguments, not a
    # chained command. subprocess.run(args) is called with a list (shell=
    # False), so it never invokes a shell and ";" is just an inert argument
    # character either way. This test pins that behavior rather than the
    # (nonexistent, since no shell is ever invoked) injection vector.
    mock_run.return_value = MagicMock(stdout="hi;\n", stderr="", returncode=0)

    response = client.post("/execute", json={"command": "echo hi; rm -rf /"})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 0
    mock_run.assert_called_once()
    called_args, _ = mock_run.call_args
    assert called_args[0] == ["echo", "hi;", "rm", "-rf", "/"]


@patch("server.subprocess.run")
def test_execute_malformed_syntax_is_rejected(mock_run):
    response = client.post("/execute", json={"command": "echo 'unterminated"})

    assert response.status_code == 200
    body = response.json()
    assert body["exit_code"] == 1
    assert "Malformed command syntax" in body["stderr"]
    mock_run.assert_not_called()


@patch("server.subprocess.run")
def test_execute_empty_command_is_rejected(mock_run):
    response = client.post("/execute", json={"command": "   "})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "No command provided", "exit_code": 1}
    mock_run.assert_not_called()


@patch("server.subprocess.run")
def test_execute_timeout_is_reported(mock_run):
    mock_run.side_effect = subprocess.TimeoutExpired(cmd=["ls"], timeout=30)

    response = client.post("/execute", json={"command": "ls"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "Command timed out", "exit_code": 124}


@patch("server.subprocess.run")
def test_execute_unexpected_error_is_reported(mock_run):
    mock_run.side_effect = OSError("boom")

    response = client.post("/execute", json={"command": "ls"})

    assert response.status_code == 200
    assert response.json() == {"stdout": "", "stderr": "boom", "exit_code": 1}
