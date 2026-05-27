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

"""Tests for ``KubernetesConfig`` injection into ``SandboxClient`` and the
underlying ``api_client`` injection into ``K8sHelper``.

Covers:
- ``K8sHelper(api_client=...)`` skips kubeconfig discovery (internal seam).
- ``SandboxClient(kubernetes_config=KubernetesConfig(api_client=...))`` builds
  a helper from the config rather than constructing a bare default one.
- Default discovery path (``load_incluster_config`` / ``load_kube_config``)
  is preserved when no kwargs are passed.
"""

import unittest
from unittest.mock import MagicMock, patch

from k8s_agent_sandbox.k8s_helper import K8sHelper
from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.models import KubernetesConfig, SandboxDirectConnectionConfig


class TestK8sHelperApiClientInjection(unittest.TestCase):
    """``K8sHelper(api_client=...)`` skips kubeconfig discovery."""

    @patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
    @patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
    @patch("k8s_agent_sandbox.k8s_helper.config")
    def test_injected_api_client_is_used_for_both_apis(
        self, mock_config, mock_custom_cls, mock_core_cls
    ):
        injected = MagicMock(name="injected_api_client")

        helper = K8sHelper(api_client=injected)

        mock_config.load_incluster_config.assert_not_called()
        mock_config.load_kube_config.assert_not_called()
        mock_custom_cls.assert_called_once_with(injected)
        mock_core_cls.assert_called_once_with(injected)
        self.assertIs(helper.custom_objects_api, mock_custom_cls.return_value)
        self.assertIs(helper.core_v1_api, mock_core_cls.return_value)

    @patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
    @patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
    @patch("k8s_agent_sandbox.k8s_helper.config")
    def test_default_path_preserved_when_no_injection(
        self, mock_config, mock_custom_cls, mock_core_cls
    ):
        K8sHelper()

        mock_config.load_incluster_config.assert_called_once()
        mock_custom_cls.assert_called_once_with()
        mock_core_cls.assert_called_once_with()

    @patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
    @patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
    def test_default_path_falls_back_to_kube_config_on_incluster_failure(
        self, mock_custom_cls, mock_core_cls
    ):
        from kubernetes.config import ConfigException

        with patch("k8s_agent_sandbox.k8s_helper.config") as mock_config:
            mock_config.ConfigException = ConfigException
            mock_config.load_incluster_config.side_effect = ConfigException("not in cluster")

            K8sHelper()

            mock_config.load_incluster_config.assert_called_once()
            mock_config.load_kube_config.assert_called_once()


class TestSandboxClientKubernetesConfig(unittest.TestCase):
    """``SandboxClient(kubernetes_config=...)`` builds K8sHelper from the config."""

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_kubernetes_config_api_client_is_forwarded_to_helper(self, mock_helper_cls):
        injected_api_client = MagicMock(name="injected_api_client")
        kube_cfg = KubernetesConfig(api_client=injected_api_client)

        c = SandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
            kubernetes_config=kube_cfg,
        )

        mock_helper_cls.assert_called_once_with(api_client=injected_api_client)
        self.assertIs(c.k8s_helper, mock_helper_cls.return_value)

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_default_helper_constructed_when_no_kubernetes_config(self, mock_helper_cls):
        c = SandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
        )

        mock_helper_cls.assert_called_once_with()
        self.assertIs(c.k8s_helper, mock_helper_cls.return_value)

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_kubernetes_config_with_no_api_client_still_uses_injection_path(
        self, mock_helper_cls
    ):
        # KubernetesConfig with only namespace set — api_client is None,
        # but the config object itself was passed, so we still go through
        # the K8sHelper(api_client=None) path (which behaves like default
        # discovery since K8sHelper treats None as "no injection").
        kube_cfg = KubernetesConfig(namespace="my-ns")

        SandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
            kubernetes_config=kube_cfg,
        )

        mock_helper_cls.assert_called_once_with(api_client=None)


if __name__ == "__main__":
    unittest.main()
