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


@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_create_sandbox_tool_with_default_args(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
    mcp_server_settings,
):
    result = await mcp_client.call_tool(
        "create_sandbox", 
        {
            "warmpool": "my-warmpool", 
            "namespace": "my-namespace",
        },
    )

    assert result.structured_content == {"sandbox_claim_name": mock_sandbox.claim_name}
    assert result.is_error is False

    create_sandbox_labels = mock_sandbox_client.create_sandbox.call_args.kwargs["labels"]
    
    assert mcp_server_settings.session_id_label_key in create_sandbox_labels

    mock_sandbox_client.create_sandbox.assert_called_once_with(
        'my-warmpool',
        namespace='my-namespace',
        sandbox_ready_timeout=180,
        labels=create_sandbox_labels,
        shutdown_after_seconds=300,
        pod_labels=None,
        pod_annotations=None,
    )

@pytest.mark.anyio
@pytest.mark.usefixtures("mocked_servers_sandbox_client_class")
async def test_call_create_sandbox_tool_with_non_default_args(
    mcp_client,
    mock_sandbox_client,
    mock_sandbox,
    mcp_server_settings,
):
    result = await mcp_client.call_tool(
        "create_sandbox", 
        {
            "warmpool": "my-warmpool", 
            "namespace": "my-namespace",
            "sandbox_ready_timeout": 30,
            "labels": {"my-label-key": "my-label-value"},
            "shutdown_after_seconds": 500,
            "pod_labels": {"my-pod-label-key": "my-pod-label-value"},
            "pod_annotations": {"my-pod-annotation-key": "my-pod-annotation-value"},
        },
    )

    assert result.structured_content == {"sandbox_claim_name": mock_sandbox.claim_name}
    assert result.is_error is False
    
    create_sandbox_labels = mock_sandbox_client.create_sandbox.call_args.kwargs["labels"]

    extra_label = create_sandbox_labels.get("my-label-key")
    assert extra_label == "my-label-value"
    
    assert mcp_server_settings.session_id_label_key in create_sandbox_labels
    
    mock_sandbox_client.create_sandbox.assert_called_once_with(
        'my-warmpool',
        namespace='my-namespace',
        sandbox_ready_timeout=30,
        labels=create_sandbox_labels,
        shutdown_after_seconds=500,
        pod_labels={'my-pod-label-key': 'my-pod-label-value'},
        pod_annotations={'my-pod-annotation-key': 'my-pod-annotation-value'},
    )
