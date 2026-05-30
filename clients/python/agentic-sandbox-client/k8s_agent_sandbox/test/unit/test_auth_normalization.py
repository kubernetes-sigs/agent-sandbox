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

from k8s_agent_sandbox.utils import normalize_kubernetes_auth_config


class TestNormalizeKubernetesAuthConfig(unittest.TestCase):

    @patch("k8s_agent_sandbox.utils.client")
    def test_normalize_with_only_authorization_key(self, mock_client):
        """Test normalization when only 'authorization' key exists (k8s <36)."""
        # Setup mock config with only 'authorization' key
        mock_config = MagicMock()
        mock_config.api_key = {'authorization': 'token-123'}
        mock_client.Configuration.get_default_copy.return_value = mock_config

        # Run normalization
        normalize_kubernetes_auth_config()

        # Verify both keys exist with same value
        self.assertEqual(mock_config.api_key['authorization'], 'token-123')
        self.assertEqual(mock_config.api_key['BearerToken'], 'token-123')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    @patch("k8s_agent_sandbox.utils.client")
    def test_normalize_with_only_bearer_token_key(self, mock_client):
        """Test normalization when only 'BearerToken' key exists (k8s 36+)."""
        # Setup mock config with only 'BearerToken' key
        mock_config = MagicMock()
        mock_config.api_key = {'BearerToken': 'token-456'}
        mock_client.Configuration.get_default_copy.return_value = mock_config

        # Run normalization
        normalize_kubernetes_auth_config()

        # Verify both keys exist with same value
        self.assertEqual(mock_config.api_key['BearerToken'], 'token-456')
        self.assertEqual(mock_config.api_key['authorization'], 'token-456')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    @patch("k8s_agent_sandbox.utils.client")
    def test_normalize_with_both_keys_different_values(self, mock_client):
        """Test normalization prioritizes BearerToken when both keys exist with different values."""
        # Setup mock config with both keys having different values
        mock_config = MagicMock()
        mock_config.api_key = {'BearerToken': 'new-token', 'authorization': 'old-token'}
        mock_client.Configuration.get_default_copy.return_value = mock_config

        # Run normalization
        normalize_kubernetes_auth_config()

        # Verify BearerToken wins - both keys should have 'new-token'
        self.assertEqual(mock_config.api_key['BearerToken'], 'new-token')
        self.assertEqual(mock_config.api_key['authorization'], 'new-token')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    @patch("k8s_agent_sandbox.utils.client")
    def test_normalize_with_both_keys_same_value(self, mock_client):
        """Test normalization when both keys already exist with same value."""
        # Setup mock config with both keys having same value
        mock_config = MagicMock()
        mock_config.api_key = {'BearerToken': 'same-token', 'authorization': 'same-token'}
        mock_client.Configuration.get_default_copy.return_value = mock_config

        # Run normalization
        normalize_kubernetes_auth_config()

        # Verify values are preserved
        self.assertEqual(mock_config.api_key['BearerToken'], 'same-token')
        self.assertEqual(mock_config.api_key['authorization'], 'same-token')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    @patch("k8s_agent_sandbox.utils.client")
    def test_normalize_with_no_api_key(self, mock_client):
        """Test normalization is a no-op when api_key is None."""
        # Setup mock config with no api_key
        mock_config = MagicMock()
        mock_config.api_key = None
        mock_client.Configuration.get_default_copy.return_value = mock_config

        # Run normalization
        normalize_kubernetes_auth_config()

        # Verify api_key is still None
        self.assertIsNone(mock_config.api_key)
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    @patch("k8s_agent_sandbox.utils.client")
    def test_normalize_with_empty_api_key(self, mock_client):
        """Test normalization is a no-op when api_key is empty dict."""
        # Setup mock config with empty api_key dict
        mock_config = MagicMock()
        mock_config.api_key = {}
        mock_client.Configuration.get_default_copy.return_value = mock_config

        # Run normalization
        normalize_kubernetes_auth_config()

        # Verify api_key is still empty
        self.assertEqual(mock_config.api_key, {})
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)


if __name__ == '__main__':
    unittest.main()
