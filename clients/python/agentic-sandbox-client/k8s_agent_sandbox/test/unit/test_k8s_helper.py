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
from k8s_agent_sandbox.exceptions import SandboxMetadataError, SandboxTemplateNotFoundError


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

    def test_create_claim_with_warmpool_none(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace", warmpool="none"
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["spec"]["warmpool"], "none")

    def test_create_claim_with_specific_warmpool(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-template", "test-namespace", warmpool="custom-pool"
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["spec"]["warmpool"], "custom-pool")

    def test_create_claim_warmpool_omitted(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-template", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertNotIn("warmpool", body["spec"])


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


class _K8sHelperPatchedBase(unittest.TestCase):
    """Base class that patches K8sHelper's external dependencies via setUp/tearDown."""

    def setUp(self):
        def start(target, **kwargs):
            p = patch(target, **kwargs)
            self.addCleanup(p.stop)
            return p.start()

        # config module: no autospec — we override ConfigException after patching
        self.mock_config = start("k8s_agent_sandbox.k8s_helper.config")
        self.mock_configuration_cls = start("k8s_agent_sandbox.k8s_helper.client.Configuration", autospec=True)
        self.mock_custom_objects_api_cls = start("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi", autospec=True)
        self.mock_core_cls = start("k8s_agent_sandbox.k8s_helper.client.CoreV1Api", autospec=True)
        self.mock_api_client_cls = start("k8s_agent_sandbox.k8s_helper.client.ApiClient", autospec=True)
        self.mock_normalize = start("k8s_agent_sandbox.k8s_helper.normalize_kubernetes_auth_config", autospec=True)
        self.mock_config.ConfigException = Exception


class TestK8sHelperNormalization(_K8sHelperPatchedBase):

    def test_k8s_helper_init_calls_normalization(self):
        """Test that K8sHelper.__init__ loads into an explicit Configuration and passes it to normalize and ApiClient."""
        helper = K8sHelper()

        expected_cfg = self.mock_configuration_cls.return_value
        self.mock_normalize.assert_called_once_with(configuration=expected_cfg)
        self.mock_api_client_cls.assert_called_once_with(configuration=self.mock_normalize.return_value)


class TestK8sHelperClose(_K8sHelperPatchedBase):

    def test_close_calls_api_client_close_and_nulls_state(self):
        """Test that close() releases the ApiClient and nulls out API attributes."""
        mock_api_client_instance = MagicMock()
        self.mock_api_client_cls.return_value = mock_api_client_instance

        helper = K8sHelper()
        helper.close()

        mock_api_client_instance.close.assert_called_once()
        self.assertIsNone(helper._api_client)
        self.assertIsNone(helper.custom_objects_api)
        self.assertIsNone(helper.core_v1_api)

    def test_close_is_idempotent(self):
        """Test that calling close() twice only closes the ApiClient once and leaves state as None."""
        mock_api_client_instance = MagicMock()
        self.mock_api_client_cls.return_value = mock_api_client_instance

        helper = K8sHelper()
        helper.close()
        helper.close()

        mock_api_client_instance.close.assert_called_once()
        self.assertIsNone(helper._api_client)
        self.assertIsNone(helper.custom_objects_api)
        self.assertIsNone(helper.core_v1_api)

    def test_context_manager_closes_on_exit(self):
        """Test that K8sHelper can be used as a context manager and closes on exit."""
        mock_api_client_instance = MagicMock()
        self.mock_api_client_cls.return_value = mock_api_client_instance

        with K8sHelper() as helper:
            self.assertIsNotNone(helper._api_client)

        mock_api_client_instance.close.assert_called_once()
        self.assertIsNone(helper._api_client)

    def test_use_after_close_raises_runtime_error(self):
        """Test that calling a public method after close() raises RuntimeError."""
        helper = K8sHelper()
        helper.close()

        with self.assertRaisesRegex(RuntimeError, 'closed'):
            helper.create_sandbox_claim('name', 'template', 'namespace')


if __name__ == '__main__':
    unittest.main()
