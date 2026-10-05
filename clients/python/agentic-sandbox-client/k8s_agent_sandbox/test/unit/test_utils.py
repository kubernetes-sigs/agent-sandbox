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

import json
import os
import stat
import unittest
from types import SimpleNamespace

from kubernetes import client as sync_client
from kubernetes import config as sync_config
from kubernetes_asyncio import client as async_client
from kubernetes_asyncio import config as async_config

from k8s_agent_sandbox.utils import (
    async_kubectl_kubeconfig_args,
    extract_sandbox_name_hash,
    kubectl_kubeconfig_args,
    merge_headers,
)


class TestExtractSandboxNameHash(unittest.TestCase):
    def test_extract_sandbox_name_hash_empty(self):
        sandbox_object = {
            "status": {
                "selector": ""
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertIsNone(sandbox_name_hash)

    def test_extract_sandbox_name_hash_single(self):
        sandbox_object = {
            "status": {
                "selector": "agents.x-k8s.io/sandbox-name-hash=abc12345"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertEqual(sandbox_name_hash, "abc12345")

    def test_extract_sandbox_name_hash_multi(self):
        sandbox_object = {
            "status": {
                "selector": "app=example-sandbox,agents.x-k8s.io/sandbox-name-hash=abc12345"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertEqual(sandbox_name_hash, "abc12345")

    def test_extract_sandbox_name_hash_extra_whitespace(self):
        sandbox_object = {
            "status": {
                "selector": "agents.x-k8s.io/sandbox-name-hash = abc12345"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertEqual(sandbox_name_hash, "abc12345")

    def test_extract_sandbox_name_hash_not_included(self):
        sandbox_object = {
            "status": {
                "selector": "app=example-sandbox"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertIsNone(sandbox_name_hash)

    def test_extract_sandbox_name_hash_no_status(self):
        sandbox_object = {}
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertIsNone(sandbox_name_hash)


def _read_kubeconfig(args):
    flag, path = args
    assert flag == "--kubeconfig"
    with open(path) as f:
        return json.load(f)


def _cluster_and_user(kubeconfig):
    return kubeconfig["clusters"][0]["cluster"], kubeconfig["users"][0]["user"]


# What kubectl writes for a token user. load_kube_config stores the token under
# api_key["BearerToken"], not "authorization".
_TOKEN_KUBECONFIG = {
    "apiVersion": "v1",
    "kind": "Config",
    "clusters": [{"name": "c", "cluster": {"server": "https://cluster-b:6443"}}],
    "users": [{"name": "u", "user": {"token": "loaded-secret"}}],
    "contexts": [{"name": "x", "context": {"cluster": "c", "user": "u"}}],
    "current-context": "x",
}


class TestKubectlKubeconfigArgs(unittest.TestCase):
    def _configuration(self):
        cfg = sync_client.Configuration()
        cfg.host = "https://cluster-b:6443"
        cfg.ssl_ca_cert = "/certs/ca.crt"
        cfg.cert_file = "/certs/client.crt"
        cfg.key_file = "/certs/client.key"
        cfg.api_key = {"authorization": "secret"}
        cfg.api_key_prefix = {"authorization": "Bearer"}
        return cfg

    def test_no_flags_without_api_client(self):
        with kubectl_kubeconfig_args(None) as args:
            self.assertEqual(args, [])

    def test_kubeconfig_targets_the_injected_cluster(self):
        api_client = sync_client.ApiClient(configuration=self._configuration())

        with kubectl_kubeconfig_args(api_client) as args:
            kubeconfig = _read_kubeconfig(args)
            self.assertEqual(
                stat.S_IMODE(os.stat(args[1]).st_mode), 0o600,
                "the file can hold a bearer token",
            )

        cluster, user = _cluster_and_user(kubeconfig)
        self.assertEqual(
            cluster,
            {"server": "https://cluster-b:6443", "certificate-authority": "/certs/ca.crt"},
        )
        self.assertEqual(
            user,
            {
                "client-certificate": "/certs/client.crt",
                "client-key": "/certs/client.key",
                "token": "secret",
            },
        )
        self.assertEqual(kubeconfig["current-context"], "sandbox")
        self.assertFalse(os.path.exists(args[1]))

    def test_optional_cluster_settings(self):
        cfg = self._configuration()
        cfg.verify_ssl = False
        cfg.tls_server_name = "api.internal"
        cfg.proxy = "http://proxy:3128"

        with kubectl_kubeconfig_args(sync_client.ApiClient(configuration=cfg)) as args:
            cluster, _ = _cluster_and_user(_read_kubeconfig(args))

        self.assertTrue(cluster["insecure-skip-tls-verify"])
        # kubectl rejects a CA together with insecure-skip-tls-verify.
        self.assertNotIn("certificate-authority", cluster)
        self.assertEqual(cluster["tls-server-name"], "api.internal")
        self.assertEqual(cluster["proxy-url"], "http://proxy:3128")

    def test_impersonation_headers_are_carried_over(self):
        api_client = sync_client.ApiClient(configuration=self._configuration())
        api_client.set_default_header("impersonate-user", "alice")
        api_client.set_default_header("Impersonate-Group", "devs")

        with kubectl_kubeconfig_args(api_client) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["as"], "alice")
        self.assertEqual(user["as-groups"], ["devs"])

    def test_no_impersonation_without_headers(self):
        api_client = sync_client.ApiClient(configuration=self._configuration())

        with kubectl_kubeconfig_args(api_client) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertNotIn("as", user)
        self.assertNotIn("as-groups", user)

    def test_refreshed_token_is_used(self):
        cfg = self._configuration()
        cfg.refresh_api_key_hook = lambda c: c.api_key.update(authorization="fresh")

        with kubectl_kubeconfig_args(sync_client.ApiClient(configuration=cfg)) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["token"], "fresh")

    def test_token_from_load_kube_config_is_carried_over(self):
        cfg = sync_client.Configuration()
        sync_config.load_kube_config_from_dict(_TOKEN_KUBECONFIG, client_configuration=cfg)

        with kubectl_kubeconfig_args(sync_client.ApiClient(configuration=cfg)) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["token"], "loaded-secret")

    def test_token_from_default_headers_is_carried_over(self):
        api_client = sync_client.ApiClient(
            configuration=sync_client.Configuration(host="https://cluster-b:6443"),
            header_name="Authorization",
            header_value="Bearer header-secret",
        )

        with kubectl_kubeconfig_args(api_client) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["token"], "header-secret")

    def test_configuration_token_wins_over_default_headers(self):
        api_client = sync_client.ApiClient(
            configuration=self._configuration(),
            header_name="Authorization",
            header_value="Bearer header-secret",
        )

        with kubectl_kubeconfig_args(api_client) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["token"], "secret")

    def test_basic_auth_is_not_carried_over(self):
        cfg = self._configuration()
        cfg.api_key = {"authorization": "dXNlcjpwYXNz"}
        cfg.api_key_prefix = {"authorization": "Basic"}

        with kubectl_kubeconfig_args(sync_client.ApiClient(configuration=cfg)) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertNotIn("token", user)

    def test_file_is_removed_when_the_body_raises(self):
        api_client = sync_client.ApiClient(configuration=self._configuration())

        with self.assertRaises(RuntimeError):
            with kubectl_kubeconfig_args(api_client) as args:
                raise RuntimeError("boom")

        self.assertFalse(os.path.exists(args[1]))


class TestAsyncKubectlKubeconfigArgs(unittest.IsolatedAsyncioTestCase):
    def _configuration(self):
        cfg = async_client.Configuration()
        cfg.host = "https://cluster-b:6443"
        cfg.ssl_ca_cert = "/certs/ca.crt"
        cfg.api_key = {"authorization": "secret"}
        cfg.api_key_prefix = {"authorization": "Bearer"}
        return cfg

    async def test_no_flags_without_api_client(self):
        async with async_kubectl_kubeconfig_args(None) as args:
            self.assertEqual(args, [])

    async def test_kubeconfig_targets_the_injected_cluster(self):
        # Only ``.configuration`` is read, and a real async ApiClient would
        # try to load the fake CA file.
        api_client = SimpleNamespace(
            configuration=self._configuration(), default_headers={"Impersonate-User": "alice"}
        )

        async with async_kubectl_kubeconfig_args(api_client) as args:
            kubeconfig = _read_kubeconfig(args)

        cluster, user = _cluster_and_user(kubeconfig)
        self.assertEqual(cluster["server"], "https://cluster-b:6443")
        self.assertEqual(cluster["certificate-authority"], "/certs/ca.crt")
        self.assertEqual(user, {"token": "secret", "as": "alice"})
        self.assertFalse(os.path.exists(args[1]))

    async def test_token_from_load_kube_config_is_carried_over(self):
        cfg = async_client.Configuration()
        await async_config.load_kube_config_from_dict(_TOKEN_KUBECONFIG, client_configuration=cfg)

        async with async_kubectl_kubeconfig_args(SimpleNamespace(configuration=cfg, default_headers={})) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["token"], "loaded-secret")

    async def test_token_from_default_headers_is_carried_over(self):
        api_client = SimpleNamespace(
            configuration=async_client.Configuration(),
            default_headers={"Authorization": "Bearer header-secret"},
        )

        async with async_kubectl_kubeconfig_args(api_client) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["token"], "header-secret")

    async def test_async_refresh_hook_is_awaited(self):
        cfg = self._configuration()

        async def refresh(c):
            c.api_key["authorization"] = "fresh"

        cfg.refresh_api_key_hook = refresh

        async with async_kubectl_kubeconfig_args(SimpleNamespace(configuration=cfg, default_headers={})) as args:
            _, user = _cluster_and_user(_read_kubeconfig(args))

        self.assertEqual(user["token"], "fresh")


class TestMergeHeaders(unittest.TestCase):
    def test_later_layer_wins_case_insensitively(self):
        merged = merge_headers(
            {"Authorization": "Bearer a", "X-Keep": "1"}, {"authorization": "Bearer b"}
        )
        self.assertEqual(merged, {"X-Keep": "1", "authorization": "Bearer b"})

    def test_none_layers_are_skipped(self):
        self.assertEqual(merge_headers({"A": "1"}, None), {"A": "1"})
        self.assertEqual(merge_headers(None, None), {})

    def test_inputs_are_not_mutated(self):
        base = {"A": "1"}
        merge_headers(base, {"a": "2"}).clear()
        self.assertEqual(base, {"A": "1"})


if __name__ == "__main__":
    unittest.main()
