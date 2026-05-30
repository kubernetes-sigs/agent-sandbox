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


class _ConfigStub:
    """Minimal stub matching the _Configuration Protocol surface."""
    def __init__(self, api_key=None, api_key_prefix=None):
        self.api_key = api_key
        self.api_key_prefix = api_key_prefix


def _make_client(api_key, api_key_prefix=None):
    mock_client = MagicMock()
    stub_config = _ConfigStub(api_key=api_key, api_key_prefix=api_key_prefix)
    mock_client.Configuration.get_default_copy.return_value = stub_config
    return mock_client, stub_config


class TestNormalizeKubernetesAuthConfig(unittest.TestCase):

    def test_normalize_with_only_authorization_key(self):
        """Test normalization when only 'authorization' key exists (kubernetes package <36)."""
        mock_client, mock_config = _make_client({'authorization': 'token-123'})

        result = normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertIs(result, mock_config)
        self.assertEqual(mock_config.api_key['authorization'], 'token-123')
        self.assertEqual(mock_config.api_key['BearerToken'], 'token-123')

    def test_normalize_with_only_bearer_token_key(self):
        """Test normalization when only 'BearerToken' key exists (kubernetes package >=36)."""
        mock_client, mock_config = _make_client({'BearerToken': 'token-456'})

        result = normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertIs(result, mock_config)
        self.assertEqual(mock_config.api_key['BearerToken'], 'token-456')
        self.assertEqual(mock_config.api_key['authorization'], 'token-456')

    def test_normalize_with_both_keys_different_values(self):
        """Test normalization raises ValueError when both api_key entries exist with different values."""
        mock_client, mock_config = _make_client({'BearerToken': 'new-token', 'authorization': 'old-token'})

        with self.assertRaisesRegex(ValueError, 'different values'):
            normalize_kubernetes_auth_config(client_module=mock_client)

    def test_normalize_raises_on_prefix_mismatch(self):
        """Test normalization raises ValueError when both api_key_prefix entries differ."""
        mock_client, mock_config = _make_client(
            api_key={'BearerToken': 'token', 'authorization': 'token'},
            api_key_prefix={'BearerToken': 'Bearer', 'authorization': 'Token'},
        )

        with self.assertRaisesRegex(ValueError, 'api_key_prefix'):
            normalize_kubernetes_auth_config(client_module=mock_client)

    def test_normalize_with_both_keys_same_value(self):
        """Test normalization returns config with prefix initialized when both keys already exist with same value."""
        mock_client, mock_config = _make_client({'BearerToken': 'same-token', 'authorization': 'same-token'})

        result = normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertIs(result, mock_config)
        self.assertEqual(mock_config.api_key['BearerToken'], 'same-token')
        self.assertEqual(mock_config.api_key['authorization'], 'same-token')
        self.assertIsNotNone(result.api_key_prefix)
        self.assertEqual(result.api_key_prefix.get('BearerToken'), 'Bearer')
        self.assertEqual(result.api_key_prefix.get('authorization'), 'Bearer')

    def test_normalize_with_no_api_key(self):
        """Test normalization returns config unchanged when api_key is None."""
        mock_client, mock_config = _make_client(None)

        result = normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertIs(result, mock_config)
        self.assertIsNone(mock_config.api_key)
        self.assertIsNone(result.api_key_prefix)

    def test_normalize_with_empty_api_key(self):
        """Test normalization returns config unchanged when api_key is empty dict."""
        mock_client, mock_config = _make_client({})

        result = normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertIs(result, mock_config)
        self.assertEqual(mock_config.api_key, {})

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

    def test_normalize_does_not_overwrite_existing_prefix_when_same(self):
        """Test that an existing api_key_prefix entry is not overwritten when values match."""
        mock_client, mock_config = _make_client(
            api_key={'BearerToken': 'token-456'},
            api_key_prefix={'BearerToken': 'Bearer', 'authorization': 'Bearer'},
        )

        result = normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key_prefix['authorization'], 'Bearer')

    def test_normalize_mirrors_prefix_when_both_tokens_set_but_one_prefix_missing(self):
        """Test prefix is mirrored even when both tokens are already set with the same value."""
        mock_client, mock_config = _make_client(
            api_key={'BearerToken': 'token', 'authorization': 'token'},
            api_key_prefix={'BearerToken': 'Bearer'},
        )

        normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertEqual(mock_config.api_key_prefix['authorization'], 'Bearer')

    def test_normalize_initializes_prefix_and_sets_bearer_default_when_none(self):
        """Test that api_key_prefix is initialized and 'Bearer' is set for all token keys when prefix was None."""
        mock_client, mock_config = _make_client(
            api_key={'authorization': 'token-123'},
            api_key_prefix=None,
        )

        result = normalize_kubernetes_auth_config(client_module=mock_client)

        self.assertIsNotNone(result.api_key_prefix)
        self.assertEqual(result.api_key_prefix.get('authorization'), 'Bearer')
        self.assertEqual(result.api_key_prefix.get('BearerToken'), 'Bearer')


if __name__ == '__main__':
    unittest.main()
