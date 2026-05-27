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

"""Async sibling of ``test_k8s_helper_injection.py``.

Exercises ``AsyncK8sHelper(api_client=...)`` and the ``kubernetes_config``
kwarg on ``AsyncSandboxClient``. Mirrors the sync tests so the two paths
cannot drift.
"""

import unittest
from unittest.mock import MagicMock, patch

import pytest

pytest.importorskip("kubernetes_asyncio")

from k8s_agent_sandbox.async_k8s_helper import AsyncK8sHelper
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
from k8s_agent_sandbox.models import KubernetesConfig, SandboxDirectConnectionConfig


class TestAsyncK8sHelperApiClientInjection(unittest.IsolatedAsyncioTestCase):
    """``AsyncK8sHelper(api_client=...)`` skips kubeconfig discovery."""

    @patch("k8s_agent_sandbox.async_k8s_helper.client.CoreV1Api")
    @patch("k8s_agent_sandbox.async_k8s_helper.client.CustomObjectsApi")
    @patch("k8s_agent_sandbox.async_k8s_helper.client.ApiClient")
    @patch("k8s_agent_sandbox.async_k8s_helper.config")
    async def test_injected_api_client_is_used_for_both_apis(
        self, mock_config, mock_api_cls, mock_custom_cls, mock_core_cls
    ):
        injected = MagicMock(name="injected_api_client")

        helper = AsyncK8sHelper(api_client=injected)
        await helper._ensure_initialized()

        mock_config.load_incluster_config.assert_not_called()
        mock_config.load_kube_config.assert_not_called()
        mock_api_cls.assert_not_called()
        mock_custom_cls.assert_called_once_with(injected)
        mock_core_cls.assert_called_once_with(injected)
        self.assertIs(helper.custom_objects_api, mock_custom_cls.return_value)
        self.assertIs(helper.core_v1_api, mock_core_cls.return_value)

    @patch("k8s_agent_sandbox.async_k8s_helper.client.CoreV1Api")
    @patch("k8s_agent_sandbox.async_k8s_helper.client.CustomObjectsApi")
    @patch("k8s_agent_sandbox.async_k8s_helper.client.ApiClient")
    @patch("k8s_agent_sandbox.async_k8s_helper.config")
    async def test_default_path_preserved_when_no_injection(
        self, mock_config, mock_api_cls, mock_custom_cls, mock_core_cls
    ):
        helper = AsyncK8sHelper()
        await helper._ensure_initialized()

        mock_config.load_incluster_config.assert_called_once()
        mock_api_cls.assert_called_once()


class TestAsyncSandboxClientKubernetesConfig(unittest.TestCase):
    """``AsyncSandboxClient(kubernetes_config=...)`` builds AsyncK8sHelper from the config."""

    @patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
    def test_kubernetes_config_api_client_is_forwarded_to_helper(self, mock_helper_cls):
        injected_api_client = MagicMock(name="injected_api_client")
        kube_cfg = KubernetesConfig(api_client=injected_api_client)

        c = AsyncSandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
            kubernetes_config=kube_cfg,
        )

        mock_helper_cls.assert_called_once_with(api_client=injected_api_client)
        self.assertIs(c.k8s_helper, mock_helper_cls.return_value)

    @patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
    def test_default_helper_constructed_when_no_kubernetes_config(self, mock_helper_cls):
        c = AsyncSandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
        )

        mock_helper_cls.assert_called_once_with()
        self.assertIs(c.k8s_helper, mock_helper_cls.return_value)

    @patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
    def test_kubernetes_config_with_no_api_client_still_uses_injection_path(
        self, mock_helper_cls
    ):
        kube_cfg = KubernetesConfig(namespace="my-ns")

        AsyncSandboxClient(
            connection_config=SandboxDirectConnectionConfig(api_url="http://example"),
            kubernetes_config=kube_cfg,
        )

        mock_helper_cls.assert_called_once_with(api_client=None)


if __name__ == "__main__":
    unittest.main()
