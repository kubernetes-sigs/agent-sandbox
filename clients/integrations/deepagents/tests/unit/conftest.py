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

from unittest.mock import MagicMock

import pytest


from deepagents_k8s_agent_sandbox.lifecycle_manager import (
    ExistingSandboxClaimLifecycleManager, 
    ExistingSandboxInstanceLifecycleManager, 
    LabelScopedLifecycleManager,
)
from deepagents_k8s_agent_sandbox.settings import (
    K8sAgentSandboxSettings,
)


@pytest.fixture
def mock_sandbox():
    return MagicMock()

@pytest.fixture
def mock_sandbox_client(mock_sandbox):
    client = MagicMock()
    client.create_sandbox.return_value = mock_sandbox
    client.get_sandbox.return_value = mock_sandbox
    return client


@pytest.fixture
def sample_sandbox_settings():
    return K8sAgentSandboxSettings(
        "my-warmpool",
        "my-namespace",
    )

@pytest.fixture(params=[
    "existing-sandbox-instance", 
    "existing-sandbox-claim", 
    "label-scoped"
])
def lifecycle_manager(request, mock_sandbox, mock_sandbox_client, sample_sandbox_settings):
    if request.param == "existing-sandbox-instance":
        return ExistingSandboxInstanceLifecycleManager(mock_sandbox)
    elif request.param == "existing-sandbox-claim":
        return ExistingSandboxClaimLifecycleManager(mock_sandbox_client, "my-claim", "my-namespace")
    elif request.param == "label-scoped":
        return LabelScopedLifecycleManager(
            mock_sandbox_client,
            sample_sandbox_settings,
            scope={
                "thread-id": "1",
                "assistant-id": "2",
            },
            scope_labels_prefix="my-prefix"
        )
    else:
        raise ValueError(f"Unknown lifecycle manager type: {request.param}")

