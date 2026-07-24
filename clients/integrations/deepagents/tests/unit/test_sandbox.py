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

import pytest

from k8s_agent_sandbox.models import ExecutionResult
from deepagents.backends.protocol import (
    ExecuteResponse,
    FileUploadResponse,
    FileDownloadResponse,
    FILE_NOT_FOUND,
    PERMISSION_DENIED,
    IS_DIRECTORY,
)
from deepagents_k8s_agent_sandbox import (
    K8sAgentSandbox,
)


def test_execute(lifecycle_manager, mock_sandbox):
    backend = K8sAgentSandbox(
        lifecycle_manager,
    )

    mock_sandbox.commands.run.return_value = ExecutionResult(
        exit_code=0,
        stdout="some output",
        stderr="some logs",
    )
    
    result = backend.execute("some-command", timeout=180)

    assert result == ExecuteResponse(
        output='some output\n<stderr>\nsome logs\n</stderr>', 
        exit_code=0, 
        truncated=False
    )


@pytest.mark.parametrize("state,expected_error", [
    ("missing", None),
    ("file", None),
    ("directory", IS_DIRECTORY),
    ("denied", PERMISSION_DENIED),
])
def test_upload_files(lifecycle_manager, mock_sandbox, state, expected_error):
    backend = K8sAgentSandbox(lifecycle_manager)

    def run_side_effect(cmd, *args, **kwargs):
        if "mkdir" in cmd:
            return ExecutionResult(exit_code=0, stdout="", stderr="")
        else:
            assert "if [ -w" in cmd
        return ExecutionResult(exit_code=0, stdout=state, stderr="")

    mock_sandbox.commands.run.side_effect = run_side_effect
    mock_sandbox.files.write.return_value = None

    result = backend.upload_files([("some/path.txt", b"content")])

    assert len(result) == 1
    assert result[0] == FileUploadResponse(path="some/path.txt", error=expected_error)


@pytest.mark.parametrize("state,expected_error,expected_content", [
    ("file", None, b"file content"),
    ("missing", FILE_NOT_FOUND, None),
    ("directory", IS_DIRECTORY, None),
    ("denied", PERMISSION_DENIED, None),
])
def test_download_files(lifecycle_manager, mock_sandbox, state, expected_error, expected_content):
    backend = K8sAgentSandbox(lifecycle_manager)

    mock_sandbox.commands.run.return_value = ExecutionResult(
        exit_code=0,
        stdout=state, 
        stderr=""
    )

    mock_sandbox.files.read.return_value = b"file content"

    result = backend.download_files(["some/path.txt"])

    assert "if [ -r" in mock_sandbox.commands.run.call_args.args[0]

    assert len(result) == 1
    assert result[0] == FileDownloadResponse(
        path="some/path.txt",
        content=expected_content,
        error=expected_error,
    )
