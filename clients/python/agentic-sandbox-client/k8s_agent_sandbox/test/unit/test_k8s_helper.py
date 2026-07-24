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

from kubernetes import client
from k8s_agent_sandbox.k8s_helper import K8sHelper
from k8s_agent_sandbox.exceptions import SandboxClaimFailedError, SandboxMetadataError, SandboxTemplateNotFoundError


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperCreateSandboxClaim(unittest.TestCase):

    def test_labels_and_annotations_coexist_in_manifest(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels={"agent": "code-agent", "team": "platform"},
        )

        mock_api.create_namespaced_custom_object.assert_called_once()
        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["annotations"], {"opentelemetry.io/trace-context": "trace-data"})
        self.assertEqual(body["metadata"]["labels"], {"agent": "code-agent", "team": "platform", "agents.x-k8s.io/created-by": "python-client"})

    def test_labels_only_no_annotations(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            labels={"agent": "code-agent"},
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["annotations"], {})
        self.assertEqual(body["metadata"]["labels"], {"agent": "code-agent", "agents.x-k8s.io/created-by": "python-client"})

    def test_no_labels_no_annotations(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["annotations"], {})
        self.assertEqual(body["metadata"]["labels"], {"agents.x-k8s.io/created-by": "python-client"})

    def test_created_by_label_override_rejected(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            labels={"agent": "code-agent", "agents.x-k8s.io/created-by": "foo"},
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["labels"], {"agent": "code-agent", "agents.x-k8s.io/created-by": "python-client"})

    def test_lifecycle_included_in_manifest(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        lifecycle = {
            "shutdownTime": "2026-12-31T23:59:59Z",
            "shutdownPolicy": "Delete",
        }
        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace", lifecycle=lifecycle
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["spec"]["warmPoolRef"]["name"], "test-warmpool")

    def test_no_lifecycle_omits_key(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertNotIn("lifecycle", body["spec"])

    def test_pod_metadata_included_in_manifest(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        pod_metadata = {"labels": {"client-id": "tenant-a"}}
        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace", pod_metadata=pod_metadata
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(
            body["spec"]["additionalPodMetadata"]["labels"]["client-id"], "tenant-a"
        )

    def test_no_pod_metadata_omits_key(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertNotIn("additionalPodMetadata", body["spec"])


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

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_claim_ready_single_event(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        """Warm-pool fast path: name + Ready arrive in one claim status update."""
        mock_watch = MagicMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "sandbox": {"name": "warm-sandbox-1", "podIPs": ["10.0.0.5"]},
                },
            },
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.wait_for_claim_ready("test-claim", "default", timeout=5)

        self.assertEqual(name, "warm-sandbox-1")
        # Only the claim is watched; no call against the Sandbox resource.
        self.assertEqual(mock_watch_class.call_count, 1)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_claim_ready_name_before_ready(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        """Cold-start path: the name lands first, Ready arrives on a later event."""
        mock_watch = MagicMock()
        name_only_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}],
                    "sandbox": {"name": "cold-sandbox-1"},
                },
            },
        }
        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "sandbox": {"name": "cold-sandbox-1", "podIPs": ["10.0.0.9"]},
                },
            },
        }
        mock_watch.stream.return_value = [name_only_event, ready_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.wait_for_claim_ready("test-claim", "default", timeout=5)
        self.assertEqual(name, "cold-sandbox-1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_claim_ready_template_not_found(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
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
                            "message": "Template 'non-existent-template' not found",
                        }
                    ]
                },
            },
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        with self.assertRaises(SandboxTemplateNotFoundError):
            helper.wait_for_claim_ready("test-claim", "default", timeout=5)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_claim_ready_terminal_reason_fails_fast(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        """A terminal Ready=False reason raises immediately instead of waiting out the timeout."""
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
                            "reason": "VolumeClaimTemplatesError",
                            "message": "volumeClaimTemplates overrides are not allowed",
                        }
                    ]
                },
            },
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        with self.assertRaises(SandboxClaimFailedError) as context:
            helper.wait_for_claim_ready("test-claim", "default", timeout=5)
        self.assertIn("VolumeClaimTemplatesError", str(context.exception))

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_claim_ready_transient_reason_keeps_waiting(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        """Transient Ready=False reasons (controller retries) do not abort the wait."""
        mock_watch = MagicMock()
        transient_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "False", "reason": "ReconcilerError",
                                    "message": "Error seen: transient conflict"}],
                },
            },
        }
        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "sandbox": {"name": "recovered-sandbox-1"},
                },
            },
        }
        mock_watch.stream.return_value = [transient_event, ready_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.wait_for_claim_ready("test-claim", "default", timeout=5)
        self.assertEqual(name, "recovered-sandbox-1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_resolve_sandbox_name_returns_before_ready(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        """resolve_sandbox_name keeps its legacy semantics: name alone suffices."""
        mock_watch = MagicMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}],
                    "sandbox": {"name": "pending-sandbox"},
                },
            },
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.resolve_sandbox_name("test-claim", "default", timeout=5)
        self.assertEqual(name, "pending-sandbox")


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperDeleteSandboxClaim(unittest.TestCase):

    def test_delete_404_is_ignored(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        exc = client.ApiException(status=404)
        mock_api.delete_namespaced_custom_object.side_effect = exc

        helper = K8sHelper()
        helper.delete_sandbox_claim("missing-claim", "default")

    def test_delete_non_404_reraises(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        exc = client.ApiException(status=403)
        mock_api.delete_namespaced_custom_object.side_effect = exc

        helper = K8sHelper()
        with self.assertRaises(client.ApiException) as ctx:
            helper.delete_sandbox_claim("claim", "default")
        self.assertEqual(ctx.exception.status, 403)


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperWaitForGatewayIP(unittest.TestCase):

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_valid_ip(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_valid_hostname(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "gateway.example.com"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "gateway.example.com")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_invalid_address_special_chars(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1/path"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_invalid_hostname(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "bad_hostname"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_multiple_addresses_in_event(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [
                        {"value": "bad_hostname"},
                        {"value": "192.168.1.2"},
                    ]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.2")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_accepts_ipv6(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_ipv6 = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "2001:db8::1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_ipv6]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "2001:db8::1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_disguised_ip_decimal(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "2130706433"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_disguised_ip_hex(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "0x7f000001"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_disguised_ip_dotted_hex(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "0x7f.0x0.0x0.0x1"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_bare_hex_prefix_ip(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "0x.1"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_bare_hex_prefix_ip_dotted(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "00.0x.0x.1"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_invalid_label_length(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        long_label = "a" * 64
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": f"{long_label}.example.com"}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "gateway.example.com"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "gateway.example.com")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_non_dict_address(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": ["not-a-dict"]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_integer_value(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_event_invalid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": 2130706433}]
                }
            }
        }
        mock_event_valid = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-gateway"},
                "status": {
                    "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }
        mock_watch.stream.return_value = [mock_event_invalid, mock_event_valid]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
        self.assertEqual(ip, "192.168.1.1")


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperWatchResourceVersion(unittest.TestCase):
    """The claim ready-wait watch must always carry an explicit
    resourceVersion (the created claim's, or "0") so the apiserver serves it
    from the watch cache — an unset resourceVersion forces a quorum etcd read
    to establish initial state on every wait."""

    @staticmethod
    def _ready_event(rv="7"):
        return {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim", "resourceVersion": rv},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "sandbox": {"name": "warm-sandbox-1"},
                },
            },
        }

    def test_create_sandbox_claim_returns_api_response(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        mock_api.create_namespaced_custom_object.return_value = {
            "metadata": {"name": "test-claim", "resourceVersion": "42"}
        }

        helper = K8sHelper()
        resp = helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")
        self.assertEqual(resp["metadata"]["resourceVersion"], "42")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_defaults_to_resource_version_zero(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_watch.stream.return_value = [self._ready_event()]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.wait_for_claim_ready("test-claim", "default", timeout=5)

        self.assertEqual(name, "warm-sandbox-1")
        self.assertEqual(mock_watch.stream.call_args.kwargs["resource_version"], "0")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_starts_from_created_claim_resource_version(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_watch.stream.return_value = [self._ready_event()]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.wait_for_claim_ready("test-claim", "default", timeout=5, resource_version="12345")

        self.assertEqual(name, "warm-sandbox-1")
        self.assertEqual(mock_watch.stream.call_args.kwargs["resource_version"], "12345")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_410_gone_restarts_from_zero(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        """A compacted-away resourceVersion (410 Gone) restarts the watch
        from "0" instead of failing the wait."""
        mock_watch = MagicMock()
        mock_watch.stream.side_effect = [
            client.ApiException(status=410),
            [self._ready_event()],
        ]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.wait_for_claim_ready("test-claim", "default", timeout=5, resource_version="12345")

        self.assertEqual(name, "warm-sandbox-1")
        first, second = mock_watch.stream.call_args_list
        self.assertEqual(first.kwargs["resource_version"], "12345")
        self.assertEqual(second.kwargs["resource_version"], "0")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_non_410_api_exception_reraises(self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls):
        mock_watch = MagicMock()
        mock_watch.stream.side_effect = client.ApiException(status=403)
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        with self.assertRaises(client.ApiException) as ctx:
            helper.wait_for_claim_ready("test-claim", "default", timeout=5)
        self.assertEqual(ctx.exception.status, 403)


if __name__ == '__main__':
    unittest.main()
