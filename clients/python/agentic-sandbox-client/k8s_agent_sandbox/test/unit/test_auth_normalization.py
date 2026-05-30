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
from unittest.mock import MagicMock

from k8s_agent_sandbox.utils import normalize_kubernetes_auth_config


def _make_client(api_key, api_key_prefix=None):
    mock_client = MagicMock()
    mock_config = MagicMock()
    mock_config.api_key = api_key
    mock_config.api_key_prefix = api_key_prefix
    mock_client.Configuration.get_default_copy.return_value = mock_config
    return mock_client, mock_config


class TestNormalizeKubernetesAuthConfig(unittest.TestCase):

    def test_normalize_with_only_authorization_key(self):
        """Test normalization when only 'authorization' key exists (k8s <36)."""
        mock_client, mock_config = _make_client({'authorization': 'token-123'})

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key['authorization'], 'token-123')
        self.assertEqual(mock_config.api_key['BearerToken'], 'token-123')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    def test_normalize_with_only_bearer_token_key(self):
        """Test normalization when only 'BearerToken' key exists (k8s 36+)."""
        mock_client, mock_config = _make_client({'BearerToken': 'token-456'})

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key['BearerToken'], 'token-456')
        self.assertEqual(mock_config.api_key['authorization'], 'token-456')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    def test_normalize_with_both_keys_different_values(self):
        """Test normalization leaves both keys intact when both exist with different values."""
        mock_client, mock_config = _make_client({'BearerToken': 'new-token', 'authorization': 'old-token'})

        normalize_kubernetes_auth_config(client_module=mock_client)

        # Both keys left as-is to avoid silently switching credentials
        self.assertEqual(mock_config.api_key['BearerToken'], 'new-token')
        self.assertEqual(mock_config.api_key['authorization'], 'old-token')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    def test_normalize_with_both_keys_same_value(self):
        """Test normalization is a no-op when both keys already exist with same value."""
        mock_client, mock_config = _make_client({'BearerToken': 'same-token', 'authorization': 'same-token'})

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key['BearerToken'], 'same-token')
        self.assertEqual(mock_config.api_key['authorization'], 'same-token')
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    def test_normalize_with_no_api_key(self):
        """Test normalization is a no-op when api_key is None."""
        mock_client, mock_config = _make_client(None)

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertIsNone(mock_config.api_key)
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    def test_normalize_with_empty_api_key(self):
        """Test normalization is a no-op when api_key is empty dict."""
        mock_client, mock_config = _make_client({})

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key, {})
        mock_client.Configuration.set_default.assert_called_once_with(mock_config)

    def test_normalize_copies_prefix_with_authorization_key(self):
        """Test that api_key_prefix is mirrored when copying authorization to BearerToken."""
        mock_client, mock_config = _make_client(
            api_key={'authorization': 'token-123'},
            api_key_prefix={'authorization': 'Bearer'},
        )

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key['BearerToken'], 'token-123')
        self.assertEqual(mock_config.api_key_prefix['BearerToken'], 'Bearer')

    def test_normalize_copies_prefix_with_bearer_token_key(self):
        """Test that api_key_prefix is mirrored when copying BearerToken to authorization."""
        mock_client, mock_config = _make_client(
            api_key={'BearerToken': 'token-456'},
            api_key_prefix={'BearerToken': 'Bearer'},
        )

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key['authorization'], 'token-456')
        self.assertEqual(mock_config.api_key_prefix['authorization'], 'Bearer')

    def test_normalize_does_not_overwrite_existing_prefix(self):
        """Test that an existing api_key_prefix entry is not overwritten."""
        mock_client, mock_config = _make_client(
            api_key={'BearerToken': 'token-456'},
            api_key_prefix={'BearerToken': 'Bearer', 'authorization': 'Token'},
        )

        normalize_kubernetes_auth_config(client_module=mock_client)

        # authorization prefix already set - must not be overwritten
        self.assertEqual(mock_config.api_key_prefix['authorization'], 'Token')


if __name__ == '__main__':
    unittest.main()
