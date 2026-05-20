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

"""Tests for ``api_client`` injection into ``K8sHelper`` and ``k8s_helper``
injection into ``SandboxClient``.

These cover the explicit-injection path used by callers running outside
the cluster (Cloud Run, Lambda, peer-cluster workloads) and assert the
default discovery path (``load_incluster_config`` /
``load_kube_config``) is preserved when no kwargs are passed.
"""

import unittest
from unittest.mock import MagicMock, patch

from k8s_agent_sandbox.k8s_helper import K8sHelper
from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig


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


class TestSandboxClientK8sHelperInjection(unittest.TestCase):
    """``SandboxClient(k8s_helper=...)`` uses the injected helper."""

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_injected_helper_is_used_directly(self, mock_helper_cls):
        injected = MagicMock(name="injected_helper")

        c = SandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
            k8s_helper=injected,
        )

        self.assertIs(c.k8s_helper, injected)
        mock_helper_cls.assert_not_called()

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_default_helper_constructed_when_none_injected(self, mock_helper_cls):
        c = SandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
        )

        mock_helper_cls.assert_called_once_with()
        self.assertIs(c.k8s_helper, mock_helper_cls.return_value)


if __name__ == "__main__":
    unittest.main()
