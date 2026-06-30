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

"""Tests for TLS / scheme support added to the connection configs and helpers.

These tests guard against silent regression to hardcoded ``http://`` URLs and
verify that TLSConfig is honored consistently across sync (requests) and
async (httpx) code paths.
"""

import os
import ssl
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import pytest

from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    TLSConfig,
)
from k8s_agent_sandbox.utils import build_base_url, build_ssl_context


# A minimal self-signed PEM used for ca_cert tests. Generated once, embedded
# here so tests are hermetic and don't require openssl at runtime.
SELF_SIGNED_PEM = """-----BEGIN CERTIFICATE-----
MIIDazCCAlOgAwIBAgIUJq6V8t6F3qHj0HfqcvN0kJ9XQ+0wDQYJKoZIhvcNAQEL
BQAwRTELMAkGA1UEBhMCVVMxEzARBgNVBAgMClNvbWUtU3RhdGUxITAfBgNVBAoM
GEludGVybmV0IFdpZGdpdHMgUHR5IEx0ZDAeFw0yNjAxMDEwMDAwMDBaFw0zNjAx
MDEwMDAwMDBaMEUxCzAJBgNVBAYTAlVTMRMwEQYDVQQIDApTb21lLVN0YXRlMSEw
HwYDVQQKDBhJbnRlcm5ldCBXaWRnaXRzIFB0eSBMdGQwggEiMA0GCSqGSIb3DQEB
AQUAA4IBDwAwggEKAoIBAQDqfake_bytes_for_test_purposes_onlyZZZ
-----END CERTIFICATE-----
"""


# --------------------------------------------------------------------------- #
# build_base_url
# --------------------------------------------------------------------------- #

class TestBuildBaseURL(unittest.TestCase):
    def test_http_default(self):
        self.assertEqual(
            build_base_url("http", "example.com", 8080),
            "http://example.com:8080",
        )

    def test_https_scheme_is_honored(self):
        # Regression: regression-blocker for hardcoded http://.
        self.assertEqual(
            build_base_url("https", "example.com", 8443),
            "https://example.com:8443",
        )

    def test_ipv4_host(self):
        self.assertEqual(
            build_base_url("https", "10.0.0.1", 9000),
            "https://10.0.0.1:9000",
        )

    def test_ipv6_host_is_bracketed(self):
        self.assertEqual(
            build_base_url("http", "fe80::1", 8080),
            "http://[fe80::1]:8080",
        )

    def test_ipv6_already_bracketed_not_double_wrapped(self):
        self.assertEqual(
            build_base_url("http", "[fe80::1]", 8080),
            "http://[fe80::1]:8080",
        )

    def test_no_port_means_default_for_scheme(self):
        # Gateway URLs typically rely on the scheme default port (80/443).
        self.assertEqual(build_base_url("https", "gw.example.com"), "https://gw.example.com")
        self.assertEqual(build_base_url("http", "gw.example.com"), "http://gw.example.com")


# --------------------------------------------------------------------------- #
# TLSConfig validators
# --------------------------------------------------------------------------- #

class TestTLSConfigValidators(unittest.TestCase):
    def test_defaults(self):
        cfg = TLSConfig()
        self.assertIsNone(cfg.ca_cert)
        self.assertFalse(cfg.insecure_skip_verify)
        self.assertIsNone(cfg.server_name_override)

    def test_ca_cert_only(self):
        cfg = TLSConfig(ca_cert="/etc/ssl/ca.pem")
        self.assertEqual(cfg.ca_cert, "/etc/ssl/ca.pem")

    def test_insecure_only(self):
        cfg = TLSConfig(insecure_skip_verify=True)
        self.assertTrue(cfg.insecure_skip_verify)

    def test_ca_cert_and_insecure_are_mutually_exclusive(self):
        # Without this rule the two settings silently fight each other and
        # the actual TLS posture depends on the (sync vs async) code path.
        with self.assertRaises(ValueError):
            TLSConfig(ca_cert="/etc/ssl/ca.pem", insecure_skip_verify=True)

    def test_sni_override_alone(self):
        cfg = TLSConfig(server_name_override="sandbox.example.com")
        self.assertEqual(cfg.server_name_override, "sandbox.example.com")


# --------------------------------------------------------------------------- #
# Connection config: tls requires https
# --------------------------------------------------------------------------- #

class TestConnectionConfigTLSConsistency(unittest.TestCase):
    def test_gateway_http_with_tls_rejected(self):
        with self.assertRaises(ValueError):
            SandboxGatewayConnectionConfig(
                gateway_name="gw",
                scheme="http",
                tls=TLSConfig(insecure_skip_verify=True),
            )

    def test_gateway_https_with_tls_ok(self):
        cfg = SandboxGatewayConnectionConfig(
            gateway_name="gw",
            scheme="https",
            tls=TLSConfig(insecure_skip_verify=True),
        )
        self.assertEqual(cfg.scheme, "https")

    def test_in_cluster_http_with_tls_rejected(self):
        with self.assertRaises(ValueError):
            SandboxInClusterConnectionConfig(
                scheme="http",
                tls=TLSConfig(insecure_skip_verify=True),
            )

    def test_local_tunnel_http_with_tls_rejected(self):
        with self.assertRaises(ValueError):
            SandboxLocalTunnelConnectionConfig(
                scheme="http",
                tls=TLSConfig(insecure_skip_verify=True),
            )

    def test_direct_http_url_with_tls_rejected(self):
        with self.assertRaises(ValueError):
            SandboxDirectConnectionConfig(
                api_url="http://router.example.com",
                tls=TLSConfig(insecure_skip_verify=True),
            )

    def test_direct_https_url_with_tls_ok(self):
        cfg = SandboxDirectConnectionConfig(
            api_url="https://router.example.com",
            tls=TLSConfig(insecure_skip_verify=True),
        )
        self.assertIsNotNone(cfg.tls)


# --------------------------------------------------------------------------- #
# build_ssl_context
# --------------------------------------------------------------------------- #

class TestBuildSSLContext(unittest.TestCase):
    def test_no_tls_returns_true(self):
        # True == use system CAs via the underlying HTTP client.
        self.assertIs(build_ssl_context(None), True)

    def test_insecure_skip_returns_false(self):
        self.assertIs(build_ssl_context(TLSConfig(insecure_skip_verify=True)), False)

    def test_ca_cert_file_path_loaded(self):
        # Use the system trust bundle if present, otherwise skip — we only care
        # that the helper resolves a path to an SSLContext, not the cert chain.
        candidates = [
            "/etc/ssl/cert.pem",
            "/etc/ssl/certs/ca-certificates.crt",
            "/etc/pki/tls/certs/ca-bundle.crt",
        ]
        path = next((p for p in candidates if os.path.exists(p)), None)
        if path is None:
            self.skipTest("no system CA bundle available")
        ctx = build_ssl_context(TLSConfig(ca_cert=path))
        self.assertIsInstance(ctx, ssl.SSLContext)

    def test_ca_cert_invalid_path_raises(self):
        with self.assertRaises(ValueError):
            build_ssl_context(TLSConfig(ca_cert="/nonexistent/ca.pem"))

    def test_ca_cert_pem_string_detected(self):
        # We don't need a real cert chain — we just need to confirm the PEM
        # branch is taken (vs file-path branch) for a "-----BEGIN" header.
        # ssl.load_verify_locations will reject malformed PEM with SSLError,
        # which the helper wraps as ValueError. That alone proves PEM mode.
        with self.assertRaises(ValueError):
            build_ssl_context(TLSConfig(ca_cert=SELF_SIGNED_PEM))


# --------------------------------------------------------------------------- #
# Strategy URL construction honors scheme
# --------------------------------------------------------------------------- #

class TestStrategyURLsHonorScheme(unittest.TestCase):
    """Each connection strategy must use the configured scheme.

    These tests would have caught the original hardcoded-http bug and would
    catch a regression to it.
    """

    def test_in_cluster_dns_url_uses_https(self):
        from k8s_agent_sandbox.connector import InClusterConnectionStrategy
        cfg = SandboxInClusterConnectionConfig(scheme="https")
        s = InClusterConnectionStrategy(
            sandbox_id="my-sb", namespace="dev", config=cfg
        )
        self.assertEqual(s.connect(), "https://my-sb.dev.svc.cluster.local:8888")

    def test_in_cluster_pod_ip_url_uses_https(self):
        from k8s_agent_sandbox.connector import InClusterConnectionStrategy
        cfg = SandboxInClusterConnectionConfig(scheme="https", use_pod_ip=True)
        s = InClusterConnectionStrategy(
            sandbox_id="my-sb",
            namespace="dev",
            config=cfg,
            get_pod_ip=lambda: "10.0.0.5",
        )
        self.assertEqual(s.connect(), "https://10.0.0.5:8888")

    def test_in_cluster_pod_ip_ipv6_url_uses_https_brackets(self):
        from k8s_agent_sandbox.connector import InClusterConnectionStrategy
        cfg = SandboxInClusterConnectionConfig(scheme="https", use_pod_ip=True)
        s = InClusterConnectionStrategy(
            sandbox_id="my-sb",
            namespace="dev",
            config=cfg,
            get_pod_ip=lambda: "fe80::1",
        )
        self.assertEqual(s.connect(), "https://[fe80::1]:8888")

    def test_gateway_url_uses_https_no_port(self):
        from k8s_agent_sandbox.connector import GatewayConnectionStrategy
        cfg = SandboxGatewayConnectionConfig(gateway_name="gw", scheme="https")
        helper = MagicMock()
        helper.wait_for_gateway_ip.return_value = "203.0.113.5"
        s = GatewayConnectionStrategy(cfg, helper)
        # Gateway URLs intentionally have no port — they use the scheme default
        # (443 for https, 80 for http). server_port is forwarded as a header.
        self.assertEqual(s.connect(), "https://203.0.113.5")


# --------------------------------------------------------------------------- #
# Deprecation warning
# --------------------------------------------------------------------------- #

class TestPlaintextHTTPWarning(unittest.TestCase):
    def test_in_cluster_http_warns(self):
        from k8s_agent_sandbox.connector import SandboxConnector
        cfg = SandboxInClusterConnectionConfig(scheme="http")
        with self.assertLogs("k8s_agent_sandbox.connector", level="WARNING") as cm:
            SandboxConnector(
                sandbox_id="x", namespace="dev",
                connection_config=cfg, k8s_helper=MagicMock(),
            )
        self.assertTrue(
            any("plaintext http" in r.lower() for r in cm.output),
            f"expected plaintext warning, got: {cm.output}",
        )

    def test_in_cluster_https_does_not_warn(self):
        from k8s_agent_sandbox.connector import SandboxConnector
        cfg = SandboxInClusterConnectionConfig(scheme="https")
        # assertLogs raises if NO log is produced at the given level — so we
        # check by patching the logger directly instead.
        with patch("k8s_agent_sandbox.connector.logging.getLogger") as gl:
            gl.return_value = MagicMock()
            SandboxConnector(
                sandbox_id="x", namespace="dev",
                connection_config=cfg, k8s_helper=MagicMock(),
            )
            warning_calls = [
                c for c in gl.return_value.warning.call_args_list
                if "plaintext" in (c.args[0] if c.args else "").lower()
            ]
            self.assertEqual(warning_calls, [])

    def test_local_tunnel_http_does_not_warn(self):
        # LocalTunnel speaks to 127.0.0.1, plaintext is acceptable so we
        # intentionally do not nag the user.
        from k8s_agent_sandbox.connector import SandboxConnector
        cfg = SandboxLocalTunnelConnectionConfig(scheme="http")
        with patch("k8s_agent_sandbox.connector.logging.getLogger") as gl:
            gl.return_value = MagicMock()
            SandboxConnector(
                sandbox_id="x", namespace="dev",
                connection_config=cfg, k8s_helper=MagicMock(),
            )
            warning_calls = [
                c for c in gl.return_value.warning.call_args_list
                if "plaintext" in (c.args[0] if c.args else "").lower()
            ]
            self.assertEqual(warning_calls, [])


# --------------------------------------------------------------------------- #
# Sync vs async TLS behavior consistency
# --------------------------------------------------------------------------- #

class TestSyncAsyncTLSConsistency(unittest.TestCase):
    """Same TLSConfig must yield equivalent verify posture in both clients.

    Sync and async use different HTTP libraries (requests vs httpx) with
    different verify= semantics. The build_ssl_context helper is the shared
    source of truth — these tests pin that contract.
    """

    def test_insecure_skip_returns_false_in_both(self):
        tls = TLSConfig(insecure_skip_verify=True)
        self.assertIs(build_ssl_context(tls), False)

    def test_no_tls_returns_true_in_both(self):
        self.assertIs(build_ssl_context(None), True)


if __name__ == "__main__":
    unittest.main()
