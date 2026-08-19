# Copyright 2025 The Kubernetes Authors.
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

import shlex
from fastapi.testclient import TestClient
from unittest.mock import patch, MagicMock
from main import app

client = TestClient(app)


def test_health_check():
    response = client.get("/")
    assert response.status_code == 200
    assert response.json() == {"status": "ok", "message": "Sandbox Runtime is active."}


@patch('main.subprocess.run')
def test_agent_command_with_env_api_key(mock_subprocess_run, monkeypatch):
    monkeypatch.setenv("GEMINI_API_KEY", "env-key")

    mock_process = MagicMock()
    mock_process.stdout = "Agent output"
    mock_process.stderr = ""
    mock_process.returncode = 0
    mock_subprocess_run.return_value = mock_process

    response = client.post("/agent", json={"query": "test query"})

    assert response.status_code == 200
    assert response.json() == {
        "stdout": "Agent output",
        "stderr": "",
        "exit_code": 0,
    }

    mock_subprocess_run.assert_called_once()
    called_args, called_kwargs = mock_subprocess_run.call_args
    expected_command = "python computer-use-preview/main.py --query 'test query'"
    assert called_args[0] == shlex.split(expected_command)
    assert called_kwargs["cwd"] == "/app"
    assert called_kwargs["env"]["GEMINI_API_KEY"] == "env-key"


@patch('main.subprocess.run')
def test_agent_command_request_api_key_overrides_env(mock_subprocess_run, monkeypatch):
    monkeypatch.setenv("GEMINI_API_KEY", "env-key")

    mock_process = MagicMock()
    mock_process.stdout = "Custom agent output"
    mock_process.stderr = "Custom error"
    mock_process.returncode = 1
    mock_subprocess_run.return_value = mock_process

    response = client.post("/agent", json={"query": "custom query", "api_key": "request-key"})

    assert response.status_code == 200
    assert response.json() == {
        "stdout": "Custom agent output",
        "stderr": "Custom error",
        "exit_code": 1,
    }

    mock_subprocess_run.assert_called_once()
    called_args, called_kwargs = mock_subprocess_run.call_args
    expected_command = "python computer-use-preview/main.py --query 'custom query'"
    assert called_args[0] == shlex.split(expected_command)
    assert called_kwargs["env"]["GEMINI_API_KEY"] == "request-key"


@patch('main.subprocess.run')
def test_agent_command_without_api_key(mock_subprocess_run, monkeypatch):
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)

    response = client.post("/agent", json={"query": "test query"})

    assert response.status_code == 200
    assert response.json() == {
        "stdout": "",
        "stderr": (
            "GEMINI_API_KEY not found in request or environment variables. "
            "Please set it via request or environment variable (e.g., K8s secret)."
        ),
        "exit_code": 1,
    }
    mock_subprocess_run.assert_not_called()
