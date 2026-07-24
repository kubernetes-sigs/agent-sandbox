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

from k8s_agent_sandbox import SandboxNotFoundError
import pytest

from deepagents_k8s_agent_sandbox.lifecycle_manager import (
    ExistingSandboxInstanceLifecycleManager,
    ExistingSandboxClaimLifecycleManager,
    LabelScopedLifecycleManager,
)


class TestExistingSandbox:
    def test_existing_instance(self, mock_sandbox):
        manager = ExistingSandboxInstanceLifecycleManager(mock_sandbox)

        assert manager.get_sandbox() == mock_sandbox
    
    def test_existing_claim(self, mock_sandbox, mock_sandbox_client):
        manager = ExistingSandboxClaimLifecycleManager(
            mock_sandbox_client,
            "my-claim",
            "my-namespace",
        )

        sandbox = manager.get_sandbox()

        assert sandbox == mock_sandbox

        mock_sandbox_client.get_sandbox.assert_called_once_with(
            "my-claim", namespace="my-namespace"
        )
        assert mock_sandbox_client.create_sandbox.call_count == 0


class TestLabelScopedManager:
    @pytest.fixture
    def manager(self, mock_sandbox_client, sample_sandbox_settings):
        return LabelScopedLifecycleManager(
            mock_sandbox_client,
            sample_sandbox_settings,
            scope={
                "thread-id": "1",
                "assistant-id": "2",
            },
            scope_labels_prefix="my-prefix"
        )
    def test_with_new_sandbox(self, mock_sandbox_client, mock_sandbox, sample_sandbox_settings, manager):

        mock_sandbox_client.list_all_sandboxes.return_value = ["my-claim"]
        mock_sandbox_client.get_sandbox.side_effect = SandboxNotFoundError

        sandbox = manager.get_sandbox()

        assert sandbox == mock_sandbox

        mock_sandbox_client.list_all_sandboxes.assert_called_once_with(
            namespace=sample_sandbox_settings.namespace,
            label_selector="my-prefix/thread-id=1,my-prefix/assistant-id=2"
        )

        mock_sandbox_client.get_sandbox.assert_called_once_with(
            "my-claim", namespace=sample_sandbox_settings.namespace
        )

        mock_sandbox_client.create_sandbox.assert_called_once_with(
            sample_sandbox_settings.warmpool,
            namespace=sample_sandbox_settings.namespace,
            sandbox_ready_timeout=sample_sandbox_settings.sandbox_ready_timeout,
            labels={'my-prefix/thread-id': '1', 'my-prefix/assistant-id': '2'},
            shutdown_after_seconds=sample_sandbox_settings.shutdown_after_seconds,
            volume_claim_templates=sample_sandbox_settings.volume_claim_templates,
            pod_labels=sample_sandbox_settings.pod_labels,
            pod_annotations=sample_sandbox_settings.pod_annotations,
        )
    
    def test_with_existing_sandbox(self, mock_sandbox_client, mock_sandbox, sample_sandbox_settings, manager):
        mock_sandbox_client.list_all_sandboxes.return_value = ["my-claim"]

        sandbox = manager.get_sandbox()

        assert sandbox == mock_sandbox
        
        mock_sandbox_client.list_all_sandboxes.assert_called_once_with(
            namespace=sample_sandbox_settings.namespace,
            label_selector="my-prefix/thread-id=1,my-prefix/assistant-id=2"
        )

        mock_sandbox_client.get_sandbox.assert_called_once_with(
            "my-claim", namespace=sample_sandbox_settings.namespace
        )

        assert mock_sandbox_client.create_sandbox.call_count == 0




