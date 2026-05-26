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

"""Unit tests for the bearer token synchronization and patching utility logic.

This module tests that K8s client config modifications correctly propagate changes between
`authorization` and `BearerToken` key definitions to support correct auth in the client SDK.
"""

import unittest
from unittest.mock import MagicMock

from k8s_agent_sandbox.utils import _sync_k8s_bearer_token, patch_k8s_config


class FakeConfiguration:
    _default = None

    def __init__(self):
        self.api_key = {}
        self.api_key_prefix = {}
        self.refresh_api_key_hook = None

    @classmethod
    def get_default_copy(cls):
        if cls._default is None:
            return FakeConfiguration()
        copy_cfg = FakeConfiguration()
        copy_cfg.api_key = dict(cls._default.api_key)
        copy_cfg.api_key_prefix = dict(cls._default.api_key_prefix)
        copy_cfg.refresh_api_key_hook = cls._default.refresh_api_key_hook
        return copy_cfg

    @classmethod
    def set_default(cls, c):
        cls._default = c


class FakeClientModule:
    def __init__(self):
        self.Configuration = FakeConfiguration

    def set_default(self, c):
        self.Configuration.set_default(c)

    def get_default_copy(self):
        return self.Configuration.get_default_copy()


class TestBearerTokenPatch(unittest.TestCase):

    def setUp(self):
        FakeConfiguration._default = None


    def test_sync_bearer_token_from_authorization(self):
        config = FakeConfiguration()
        config.api_key["authorization"] = "bearer-token-123"
        config.api_key_prefix["authorization"] = "Bearer"

        _sync_k8s_bearer_token(config)

        self.assertEqual(config.api_key["BearerToken"], "bearer-token-123")
        self.assertEqual(config.api_key_prefix["BearerToken"], "Bearer")
        self.assertEqual(config.api_key["authorization"], "bearer-token-123")
        self.assertEqual(config.api_key_prefix["authorization"], "Bearer")

    def test_sync_bearer_token_from_bearer_token(self):
        config = FakeConfiguration()
        config.api_key["BearerToken"] = "bearer-token-456"
        config.api_key_prefix["BearerToken"] = "Bearer"

        _sync_k8s_bearer_token(config)

        self.assertEqual(config.api_key["authorization"], "bearer-token-456")
        self.assertEqual(config.api_key_prefix["authorization"], "Bearer")
        self.assertEqual(config.api_key["BearerToken"], "bearer-token-456")
        self.assertEqual(config.api_key_prefix["BearerToken"], "Bearer")

    def test_patch_k8s_config_wraps_refresh_hook_and_avoids_nesting(self):
        client_module = FakeClientModule()
        config = FakeConfiguration()
        
        # Original refresh hook
        mock_orig_hook = MagicMock()
        def orig_hook(cfg):
            cfg.api_key["authorization"] = "refreshed-token"
            mock_orig_hook(cfg)
        
        config.refresh_api_key_hook = orig_hook
        client_module.set_default(config)

        # First patch
        patch_k8s_config(client_module)
        patched_config = client_module.get_default_copy()
        
        self.assertIsNotNone(patched_config.refresh_api_key_hook)
        self.assertNotEqual(patched_config.refresh_api_key_hook, orig_hook)

        # Trigger refresh hook and verify it syncs BearerToken
        test_cfg = FakeConfiguration()
        test_cfg.api_key = {"authorization": "old-token"}
        patched_config.refresh_api_key_hook(test_cfg)
        
        mock_orig_hook.assert_called_once_with(test_cfg)
        self.assertEqual(test_cfg.api_key["authorization"], "refreshed-token")
        self.assertEqual(test_cfg.api_key["BearerToken"], "refreshed-token")

        # Second patch - should NOT wrap again
        second_hook = patched_config.refresh_api_key_hook
        client_module.set_default(patched_config)
        patch_k8s_config(client_module)
        
        third_config = client_module.get_default_copy()
        self.assertEqual(third_config.refresh_api_key_hook, second_hook)

    def test_sync_bearer_token_empty_config(self):
        config = FakeConfiguration()
        _sync_k8s_bearer_token(config)
        self.assertEqual(config.api_key, {})
        self.assertEqual(config.api_key_prefix, {})

    def test_patch_k8s_config_with_none_hook(self):
        client_module = FakeClientModule()
        config = FakeConfiguration()
        config.refresh_api_key_hook = None
        client_module.set_default(config)

        patch_k8s_config(client_module)
        patched_config = client_module.get_default_copy()
        self.assertIsNone(patched_config.refresh_api_key_hook)

    def test_sync_bearer_token_when_both_keys_exist_but_differ_after_refresh(self):
        config = FakeConfiguration()
        # 1. Initial state (only authorization set)
        config.api_key["authorization"] = "initial-token"
        
        # 2. First sync - populates both keys and sets state
        _sync_k8s_bearer_token(config)
        self.assertEqual(config.api_key["authorization"], "initial-token")
        self.assertEqual(config.api_key["BearerToken"], "initial-token")

        # 3. Simulate a token refresh hook that updates only 'authorization'
        config.api_key["authorization"] = "refreshed-token"

        # 4. Run sync again - it must detect the update and propagate it to 'BearerToken'!
        _sync_k8s_bearer_token(config)
        self.assertEqual(config.api_key["authorization"], "refreshed-token")
        self.assertEqual(config.api_key["BearerToken"], "refreshed-token")

    def test_sync_bearer_token_when_both_keys_exist_but_differ_after_refresh_bearer_primary(self):
        config = FakeConfiguration()
        # 1. Initial state (only BearerToken set)
        config.api_key["BearerToken"] = "initial-token"
        
        # 2. First sync - populates both keys and sets state
        _sync_k8s_bearer_token(config)
        self.assertEqual(config.api_key["authorization"], "initial-token")
        self.assertEqual(config.api_key["BearerToken"], "initial-token")

        # 3. Simulate a token refresh hook that updates only 'BearerToken'
        config.api_key["BearerToken"] = "refreshed-token"

        # 4. Run sync again - it must detect the update and propagate it to 'authorization'!
        _sync_k8s_bearer_token(config)
        self.assertEqual(config.api_key["authorization"], "refreshed-token")
        self.assertEqual(config.api_key["BearerToken"], "refreshed-token")

    def test_sync_both_keys_exist_but_differ_no_last_known_prefers_bearer_token(self):
        config = FakeConfiguration()
        config.api_key["authorization"] = "stale-token"
        config.api_key["BearerToken"] = "new-token"

        _sync_k8s_bearer_token(config)

        self.assertEqual(config.api_key["authorization"], "new-token")
        self.assertEqual(config.api_key["BearerToken"], "new-token")
        self.assertEqual(getattr(config, "_last_known_bearer_token"), "new-token")

    def test_patch_k8s_config_pre_synchronizes_before_refresh_hook_execution(self):
        client_module = FakeClientModule()
        config = FakeConfiguration()
        config.api_key["authorization"] = "pre-hook-token"

        hook_called = False
        def orig_hook(cfg):
            nonlocal hook_called
            hook_called = True
            # Verify that BearerToken has been pre-synchronized BEFORE the hook runs!
            self.assertEqual(cfg.api_key.get("BearerToken"), "pre-hook-token")
            cfg.api_key["authorization"] = "post-hook-token"

        config.refresh_api_key_hook = orig_hook
        client_module.set_default(config)

        patch_k8s_config(client_module)
        patched_config = client_module.get_default_copy()

        test_cfg = FakeConfiguration()
        test_cfg.api_key = {"authorization": "pre-hook-token"}
        patched_config.refresh_api_key_hook(test_cfg)

        self.assertTrue(hook_called)
        self.assertEqual(test_cfg.api_key["authorization"], "post-hook-token")
        self.assertEqual(test_cfg.api_key["BearerToken"], "post-hook-token")




if __name__ == '__main__':
    unittest.main()
