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
from k8s_agent_sandbox.exceptions import SandboxMetadataError, SandboxTemplateNotFoundError, SandboxReconcilerError


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


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperResolveSandboxName(unittest.TestCase):

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_resolve_sandbox_name_template_not_found(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [
                        {
                            "type": "Ready",
                            "status": "False",
                            "reason": "TemplateNotFound",
                            "message": "Template 'non-existent-template' not found"
                        }
                    ]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()

        with self.assertRaises(SandboxTemplateNotFoundError) as context:
            helper.resolve_sandbox_name("test-claim", "default", timeout=5)

        self.assertIn("Template 'non-existent-template' not found", str(context.exception))

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_resolve_sandbox_name_deleted_event(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event = {
            "type": "DELETED",
            "object": {
                "metadata": {"name": "test-claim"}
            }
        }
        
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch
        
        helper = K8sHelper()
        
        with self.assertRaises(SandboxMetadataError) as context:
            helper.resolve_sandbox_name("test-claim", "default", timeout=5)
            
        self.assertIn("SandboxClaim 'test-claim' was deleted while resolving sandbox name", str(context.exception))

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_resolve_sandbox_name_fast_fail(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [
                        {
                            "type": "Ready",
                            "status": "False",
                            "reason": "ReconcilerError",
                            "message": "Missing warm pods"
                        }
                    ]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()

        with self.assertRaises(SandboxReconcilerError) as context:
            helper.resolve_sandbox_name("test-claim", "default", timeout=5)

        self.assertIn("SandboxClaim failed with reason 'ReconcilerError': Missing warm pods", str(context.exception))

if __name__ == '__main__':
    unittest.main()
