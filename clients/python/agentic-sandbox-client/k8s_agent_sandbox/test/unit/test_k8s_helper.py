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

import unittest
from unittest.mock import MagicMock, patch

from k8s_agent_sandbox.k8s_helper import K8sHelper


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperCreateSandboxClaim(unittest.TestCase):

    def test_labels_and_annotations_coexist_in_manifest(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels={"agent": "code-agent", "team": "platform"},
        )

        mock_api.create_namespaced_custom_object.assert_called_once()
        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["annotations"], {"opentelemetry.io/trace-context": "trace-data"})
        self.assertEqual(body["metadata"]["labels"], {"agent": "code-agent", "team": "platform"})

    def test_labels_only_no_annotations(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace",
            labels={"agent": "code-agent"},
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["annotations"], {})
        self.assertEqual(body["metadata"]["labels"], {"agent": "code-agent"})

    def test_no_labels_no_annotations(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-template", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["annotations"], {})
        self.assertNotIn("labels", body["metadata"])

    def test_lifecycle_included_in_manifest(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        lifecycle = {
            "shutdownTime": "2026-12-31T23:59:59Z",
            "shutdownPolicy": "Delete",
        }
        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace", lifecycle=lifecycle
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["spec"]["sandboxTemplateRef"]["name"], "test-template")

    def test_no_lifecycle_omits_key(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-template", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertNotIn("lifecycle", body["spec"])


if __name__ == '__main__':
    unittest.main()
