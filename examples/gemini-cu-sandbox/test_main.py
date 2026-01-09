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

import pytest
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
def test_agent_command_defaults(mock_subprocess_run):
    # Configure the mock to return a successful process completion
    mock_process = MagicMock()
    mock_process.stdout = "Agent output"
    mock_process.stderr = ""
    mock_process.returncode = 0
    mock_subprocess_run.return_value = mock_process

    # Make the request with only the required query
    response = client.post("/agent", json={"query": "test query"})

    # Assertions
    assert response.status_code == 200
    assert response.json() == {
        "stdout": "Agent output",
        "stderr": "",
        "exit_code": 0
    }

    # Verify that subprocess.run was called with the correct command
    expected_command = (
        "python computer-use-preview/main.py --query 'test query' "
        "--env 'playwright' "
        "--initial_url 'https://www.google.com' "
        "--model 'gemini-2.5-computer-use-preview-10-2025'"
    )
    mock_subprocess_run.assert_called_once()
    called_args, called_kwargs = mock_subprocess_run.call_args
    assert called_args[0] == shlex.split(expected_command)


@patch('main.subprocess.run')
def test_agent_command_custom_args(mock_subprocess_run):
    # Configure the mock
    mock_process = MagicMock()
    mock_process.stdout = "Custom agent output"
    mock_process.stderr = "Custom error"
    mock_process.returncode = 1
    mock_subprocess_run.return_value = mock_process

    # Make the request with custom arguments
    request_payload = {
        "query": "custom query",
        "env": "browserbase",
        "initial_url": "https://example.com",
        "highlight_mouse": True,
        "model": "custom-model"
    }
    response = client.post("/agent", json=request_payload)

    # Assertions
    assert response.status_code == 200
    assert response.json() == {
        "stdout": "Custom agent output",
        "stderr": "Custom error",
        "exit_code": 1
    }

    # Verify the command
    expected_command = (
        "python computer-use-preview/main.py --query 'custom query' "
        "--env 'browserbase' "
        "--initial_url 'https://example.com' "
        "--model 'custom-model' --highlight_mouse"
    )
    mock_subprocess_run.assert_called_once()
    called_args, called_kwargs = mock_subprocess_run.call_args
    assert called_args[0] == shlex.split(expected_command)
