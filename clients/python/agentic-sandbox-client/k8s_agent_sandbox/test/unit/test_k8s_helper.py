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

import logging
import os
import socket
import tempfile
import threading
import time
import unittest
import weakref
from unittest.mock import MagicMock, call, patch

from kubernetes import client
from kubernetes.config import incluster_config
import urllib3.exceptions
from pydantic import ValidationError
from k8s_agent_sandbox.models import AdditionalPodMetadata
from k8s_agent_sandbox.k8s_helper import K8sHelper
from k8s_agent_sandbox.exceptions import SandboxClaimFailedError, SandboxMetadataError, SandboxTemplateNotFoundError
from k8s_agent_sandbox.constants import CLIENT_REQUEST_TIME_ANNOTATION
from k8s_agent_sandbox.models import SandboxClaimEnvVar


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperCreateSandboxClaim(unittest.TestCase):

    def test_env_var_model_serializes_container_name_alias(self, mock_config, mock_api_cls, mock_core_cls):
        env_var = SandboxClaimEnvVar(
            name="FOO", value="bar", container_name="runtime"
        )

        self.assertEqual(
            env_var.model_dump(by_alias=True, exclude_none=True),
            {"name": "FOO", "value": "bar", "containerName": "runtime"},
        )

    def test_env_var_model_rejects_non_string_values(self, mock_config, mock_api_cls, mock_core_cls):
        with self.assertRaises(ValidationError):
            SandboxClaimEnvVar(name="FOO", value=1)

    def test_env_var_model_accepts_kubernetes_env_var_names(self, mock_config, mock_api_cls, mock_core_cls):
        valid_names = ["FOO", "_FOO", "my.env-name", "MY_ENV.NAME", "MyEnvName1"]

        for name in valid_names:
            self.assertEqual(SandboxClaimEnvVar(name=name, value="bar").name, name)

    def test_env_var_model_rejects_invalid_names(self, mock_config, mock_api_cls, mock_core_cls):
        invalid_names = ["", "1FOO", "BAD NAME", "BAD=NAME", ".", "..", "..FOO"]

        for name in invalid_names:
            with self.assertRaises(ValidationError):
                SandboxClaimEnvVar(name=name, value="bar")

    def test_env_included_in_manifest(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim",
            "test-warmpool",
            "test-namespace",
            env={"FOO": "bar", "DEBUG": "true"},
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(
            body["spec"]["env"],
            [
                {"name": "FOO", "value": "bar"},
                {"name": "DEBUG", "value": "true"},
            ],
        )

    def test_owner_references_included_in_manifest_only_when_given(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        owner = [{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "name": "batch-b1", "uid": "u1"}]

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace", owner_references=owner)
        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(body["metadata"]["ownerReferences"], owner)
        helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")
        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertNotIn("ownerReferences", body["metadata"])

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
        self.assertEqual(body["metadata"]["annotations"]["opentelemetry.io/trace-context"], "trace-data")
        self.assertIn(CLIENT_REQUEST_TIME_ANNOTATION, body["metadata"]["annotations"])
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
        self.assertEqual(set(body["metadata"]["annotations"].keys()), {CLIENT_REQUEST_TIME_ANNOTATION})
        self.assertEqual(body["metadata"]["labels"], {"agent": "code-agent", "agents.x-k8s.io/created-by": "python-client"})

    def test_no_labels_no_annotations(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(set(body["metadata"]["annotations"].keys()), {CLIENT_REQUEST_TIME_ANNOTATION})
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

        pod_metadata = AdditionalPodMetadata(labels={"client-id": "tenant-a"})
        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace", pod_metadata=pod_metadata
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(
            body["spec"]["additionalPodMetadata"]["labels"]["client-id"], "tenant-a"
        )

    def test_pod_metadata_dict_backward_compatible(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            pod_metadata={"labels": {"client-id": "tenant-a"}, "annotations": {"a": "b"}},
        )

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertEqual(
            body["spec"]["additionalPodMetadata"],
            {"labels": {"client-id": "tenant-a"}, "annotations": {"a": "b"}},
        )

    def test_no_pod_metadata_omits_key(self, mock_config, mock_api_cls, mock_core_cls):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api

        helper = K8sHelper()
        helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")

        body = mock_api.create_namespaced_custom_object.call_args.kwargs["body"]
        self.assertNotIn("additionalPodMetadata", body["spec"])

    def test_create_routes_batch_to_unretried_client(self, mock_config, mock_api_cls, mock_core_cls):
        default_api = MagicMock()
        batch_api = MagicMock()
        mock_api_cls.side_effect = [default_api, batch_api]

        helper = K8sHelper()
        helper.create_sandbox_claim("single-0", "pool-a", "default", _request_timeout=30)

        self.assertEqual(default_api.create_namespaced_custom_object.call_args.kwargs["_request_timeout"], 30)
        batch_api.create_namespaced_custom_object.assert_not_called()

        default_api.reset_mock()
        helper.create_sandbox_claim("b1-0", "pool-a", "default", _request_timeout=30, batch=True)

        self.assertEqual(batch_api.create_namespaced_custom_object.call_args.kwargs["_request_timeout"], 30)
        default_api.create_namespaced_custom_object.assert_not_called()


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
        cases = [
            ("VolumeClaimTemplatesError", "volumeClaimTemplates overrides are not allowed"),
            ("InvalidConfiguration", "Service 'test-claim' is invalid: must be no more than 63 characters"),
            ("PodFailed", "Pod failed"),
            ("PodSucceeded", "Pod completed successfully"),
        ]
        for reason, message in cases:
            with self.subTest(reason=reason):
                mock_watch = MagicMock()
                mock_event = {
                    "type": "MODIFIED",
                    "object": {
                        "metadata": {"name": "test-claim"},
                        "status": {
                            "conditions": [
                                {"type": "Ready", "status": "False", "reason": reason, "message": message}
                            ]
                        },
                    },
                }
                mock_watch.stream.return_value = [mock_event]
                mock_watch_class.return_value = mock_watch

                helper = K8sHelper()
                with self.assertRaises(SandboxClaimFailedError) as context:
                    helper.wait_for_claim_ready("test-claim", "default", timeout=5)
                self.assertIn(reason, str(context.exception))

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

    def test_delete_defaults_to_no_request_timeout(self, mock_config, mock_api_cls, mock_core_cls):
        default_api = MagicMock()
        batch_api = MagicMock()
        mock_api_cls.side_effect = [default_api, batch_api]

        helper = K8sHelper()
        helper.delete_sandbox_claim("claim", "default")

        self.assertIsNone(default_api.delete_namespaced_custom_object.call_args.kwargs["_request_timeout"])
        batch_api.delete_namespaced_custom_object.assert_not_called()

    def test_delete_forwards_request_timeout(self, mock_config, mock_api_cls, mock_core_cls):
        default_api = MagicMock()
        batch_api = MagicMock()
        mock_api_cls.side_effect = [default_api, batch_api]

        helper = K8sHelper()
        helper.delete_sandbox_claim("claim", "default", _request_timeout=30)

        self.assertEqual(default_api.delete_namespaced_custom_object.call_args.kwargs["_request_timeout"], 30)
        batch_api.delete_namespaced_custom_object.assert_not_called()

        default_api.reset_mock()
        helper.delete_sandbox_claim("claim", "default", _request_timeout=30, batch=True)

        self.assertEqual(batch_api.delete_namespaced_custom_object.call_args.kwargs["_request_timeout"], 30)
        default_api.delete_namespaced_custom_object.assert_not_called()


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

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_transient_disconnect_resumes_from_tracked_resource_version(
        self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls
    ):
        """A transient network disconnect resumes from the tracked resourceVersion
        instead of resetting to '0'."""
        first_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim", "resourceVersion": "5555"},
                "status": {"conditions": []},
            },
        }

        def _first_stream(*args, **kwargs):
            yield first_event
            raise urllib3.exceptions.ProtocolError("Connection broken: RemoteDisconnected")

        mock_watch = MagicMock()
        mock_watch.stream.side_effect = [
            _first_stream(),
            [self._ready_event()],
        ]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        name = helper.wait_for_claim_ready("test-claim", "default", timeout=5, resource_version="12345")

        self.assertEqual(name, "warm-sandbox-1")
        first_call, second_call = mock_watch.stream.call_args_list
        self.assertEqual(first_call.kwargs["resource_version"], "12345")
        self.assertEqual(second_call.kwargs["resource_version"], "5555")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_non_transient_network_exception_reraises(
        self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls
    ):
        """Non-transient exceptions (such as SSLError) must not be caught and retried."""
        mock_watch = MagicMock()
        mock_watch.stream.side_effect = urllib3.exceptions.SSLError("certificate verify failed")
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        with self.assertRaises(urllib3.exceptions.SSLError):
            helper.wait_for_claim_ready("test-claim", "default", timeout=5)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_transient_disconnect_reconnects(
        self, mock_watch_class, mock_config, mock_api_cls, mock_core_cls
    ):
        """wait_for_sandbox_ready reconnects upon transient network disconnect."""
        ready_sandbox_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-sandbox"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "podIPs": ["10.244.0.5"],
                },
            },
        }
        mock_watch = MagicMock()
        mock_watch.stream.side_effect = [
            urllib3.exceptions.ProtocolError("Connection broken"),
            [ready_sandbox_event],
        ]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        ip = helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=5)
        self.assertEqual(ip, "10.244.0.5")
        self.assertEqual(mock_watch.stream.call_count, 2)


@patch("k8s_agent_sandbox.k8s_helper._api_client_without_retries")
@patch("k8s_agent_sandbox.k8s_helper.client.CoordinationV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperApiClientInjection(unittest.TestCase):

    def test_injected_api_client_used_and_no_config_loaded(
        self, mock_config, mock_custom_cls, mock_core_cls, mock_coord_cls, mock_without_retries
    ):
        sentinel = MagicMock(name="ApiClient")
        default_api = MagicMock(name="DefaultCustomObjectsApi")
        batch_api = MagicMock(name="BatchCustomObjectsApi")
        mock_custom_cls.side_effect = [default_api, batch_api]
        unretried_client = MagicMock(name="UnretriedApiClient")
        mock_without_retries.return_value = unretried_client

        helper = K8sHelper(api_client=sentinel)

        self.assertIs(helper.custom_objects_api, default_api)
        self.assertIs(helper._batch_custom_objects_api, batch_api)
        self.assertEqual(mock_custom_cls.call_args_list, [call(sentinel), call(unretried_client)])
        mock_without_retries.assert_called_once_with(sentinel)
        mock_coord_cls.assert_called_once_with(unretried_client)
        self.assertIs(mock_custom_cls.call_args_list[1].args[0], mock_coord_cls.call_args.args[0])
        mock_core_cls.assert_called_once_with(sentinel)
        mock_config.load_incluster_config.assert_not_called()
        mock_config.load_kube_config.assert_not_called()

    def test_injected_api_client_is_exposed_only_when_injected(
        self, mock_config, mock_custom_cls, mock_core_cls, mock_coord_cls, _
    ):
        sentinel = MagicMock(name="ApiClient")
        self.assertIs(K8sHelper(api_client=sentinel).injected_api_client, sentinel)
        self.assertIsNone(K8sHelper().injected_api_client)


class TestK8sHelperInClusterAuth(unittest.TestCase):

    def test_incluster_service_account_token_is_sent(self):
        # 36.0.0 loaded the token but never sent it; a mocked loader can't catch that.
        original = client.Configuration.get_default_copy()
        self.addCleanup(client.Configuration.set_default, original)
        with tempfile.TemporaryDirectory() as tmp:
            token_path = os.path.join(tmp, "token")
            ca_path = os.path.join(tmp, "ca.crt")
            for path, content in ((token_path, "test-token"), (ca_path, "test-ca")):
                with open(path, "w") as f:
                    f.write(content)
            with patch.dict(os.environ, {
                "KUBERNETES_SERVICE_HOST": "10.0.0.1",
                "KUBERNETES_SERVICE_PORT": "443",
            }), patch.object(
                incluster_config, "SERVICE_TOKEN_FILENAME", token_path
            ), patch.object(incluster_config, "SERVICE_CERT_FILENAME", ca_path):
                helper = K8sHelper()

        settings = helper.core_v1_api.api_client.configuration.auth_settings()
        self.assertTrue(
            any("test-token" in str(s.get("value")) for s in settings.values()),
            f"in-cluster token missing from auth settings: {settings}",
        )


class TestK8sHelperWatchTimeout(unittest.TestCase):
    def test_one_second_wait_observes_ready_resource(self):
        ready = {"type": "Ready", "status": "True"}
        cases = [
            ("resolve_sandbox_name", {"sandbox": {"name": "sandbox"}}, "sandbox"),
            ("wait_for_claim_ready", {"sandbox": {"name": "sandbox"}, "conditions": [ready]}, "sandbox"),
            ("wait_for_sandbox_ready", {"conditions": [ready], "podIPs": [{"ip": "10.0.0.1"}]}, "10.0.0.1"),
            ("wait_for_gateway_ip", {"addresses": [{"value": "10.0.0.2"}]}, "10.0.0.2"),
        ]
        with client.ApiClient(client.Configuration()) as api_client:
            helper = K8sHelper(api_client)
            for method, status, expected in cases:
                with self.subTest(method=method), patch(
                    "k8s_agent_sandbox.k8s_helper.time", **{"monotonic.side_effect": [100.0, 100.1]}
                ), patch("k8s_agent_sandbox.k8s_helper.watch.Watch") as watch_cls:
                    watch_cls.return_value.stream.return_value = iter([
                        {"type": "ADDED", "object": {"status": status}}
                    ])
                    self.assertEqual(getattr(helper, method)("resource", "default", timeout=1), expected)
                    self.assertEqual(watch_cls.return_value.stream.call_args.kwargs["timeout_seconds"], 1)

    def test_claim_watch_restarts_during_final_fractional_second(self):
        with client.ApiClient(client.Configuration()) as api_client:
            helper = K8sHelper(api_client)
            with patch(
                "k8s_agent_sandbox.k8s_helper.time", **{"monotonic.side_effect": [100.0, 100.1, 104.5]}
            ), patch("k8s_agent_sandbox.k8s_helper.watch.Watch") as watch_cls:
                watch_cls.return_value.stream.side_effect = [iter([]), iter([
                    {"type": "ADDED", "object": {"status": {"sandbox": {"name": "sandbox"}}}}
                ])]
                self.assertEqual(helper.resolve_sandbox_name("claim", "default", timeout=5), "sandbox")
                self.assertEqual(watch_cls.return_value.stream.call_count, 2)
                self.assertEqual(watch_cls.return_value.stream.call_args.kwargs["timeout_seconds"], 1)

    def test_expired_wait_does_not_start_watch(self):
        with client.ApiClient(client.Configuration()) as api_client:
            helper = K8sHelper(api_client)
            with patch(
                "k8s_agent_sandbox.k8s_helper.time", **{"monotonic.side_effect": [100.0, 101.0]}
            ), patch("k8s_agent_sandbox.k8s_helper.watch.Watch") as watch_cls:
                with self.assertRaises(TimeoutError):
                    helper.resolve_sandbox_name("claim", "default", timeout=1)
                watch_cls.assert_not_called()


@patch("k8s_agent_sandbox.k8s_helper.config")
class TestLeaseApiClient(unittest.TestCase):

    def _assert_delegates_to_injected(self, derived_client, injected, configuration, host):
        self.assertIsNot(derived_client, injected)
        self.assertEqual(derived_client.configuration.host, host)
        self.assertEqual(derived_client.cookie, "session=1")
        self.assertEqual(derived_client.default_headers["X-Test"], "yes")
        self.assertIsNone(configuration.retries)
        configuration.api_key["authorization"] = "rotated"
        configuration.api_key_prefix["authorization"] = "Bearer"
        self.assertEqual(
            derived_client.configuration.get_api_key_with_prefix("authorization"), "Bearer rotated"
        )
        injected.set_default_header("X-Rotated", "token-2")
        injected.cookie = "session=2"
        self.assertEqual(derived_client.default_headers["X-Rotated"], "token-2")
        self.assertEqual(derived_client.cookie, "session=2")

    def test_lease_requests_are_not_retried_and_keep_the_injected_client_settings(self, _):
        configuration = client.Configuration(host="https://cluster-b:6443")
        injected = client.ApiClient(configuration=configuration, cookie="session=1")
        self.addCleanup(injected.close)
        injected.set_default_header("X-Test", "yes")
        helper = K8sHelper(api_client=injected)
        lease_api_client = helper.coordination_v1_api.api_client
        self.addCleanup(lease_api_client.close)
        self.assertIs(lease_api_client.configuration.retries, False)
        self.assertIs(helper._batch_custom_objects_api.api_client, lease_api_client)
        self._assert_delegates_to_injected(lease_api_client, injected, configuration, "https://cluster-b:6443")
        _, _, headers, _, _ = lease_api_client.param_serialize("GET", "/apis")
        self.assertEqual(headers["X-Rotated"], "token-2")
        self.assertEqual(headers["Cookie"], "session=2")
        injected.default_headers = {"X-Replaced": "1"}
        self.assertEqual(lease_api_client.default_headers, {"X-Replaced": "1"})

    def test_lease_and_batch_requests_are_not_retried_with_the_default_client(self, _):
        helper = K8sHelper()
        self.addCleanup(helper.custom_objects_api.api_client.close)
        self.addCleanup(helper.core_v1_api.api_client.close)
        self.addCleanup(helper.coordination_v1_api.api_client.close)
        self.assertIs(helper.coordination_v1_api.api_client.configuration.retries, False)
        self.assertIs(helper._batch_custom_objects_api.api_client, helper.coordination_v1_api.api_client)

    def test_batch_custom_object_requests_do_not_retry_and_keep_injected_settings(self, _):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            host = f"http://127.0.0.1:{sock.getsockname()[1]}"
            configuration = client.Configuration(host=host)
            injected = client.ApiClient(configuration=configuration, cookie="session=1")
            self.addCleanup(injected.close)
            injected.set_default_header("X-Test", "yes")
            helper = K8sHelper(api_client=injected)
            batch_api_client = helper._batch_custom_objects_api.api_client
            self.addCleanup(batch_api_client.close)

            self.assertIs(batch_api_client.configuration.retries, False)
            self.assertIs(
                batch_api_client.rest_client.pool_manager.connection_pool_kw["retries"].total, False
            )
            self.assertIs(batch_api_client, helper.coordination_v1_api.api_client)
            self._assert_delegates_to_injected(batch_api_client, injected, configuration, host)

            selector = "agents.x-k8s.io/batch-id=b1"
            batch_calls = (
                lambda: helper.delete_sandbox_claims_by_label(
                    "default", selector, _request_timeout=(0.05, 0.05)
                ),
                lambda: helper.list_sandbox_claim_objects(
                    "default", selector, _request_timeout=(0.05, 0.05)
                ),
                lambda: helper.count_live_sandbox_claims(
                    "default", selector, _request_timeout=(0.05, 0.05)
                ),
                lambda: list(
                    helper.watch_sandbox_claims(
                        "default", selector, "5", 1, _request_timeout=(0.05, 0.05)
                    )
                ),
                lambda: helper.get_sandbox_warmpool("pool-a", "default", _request_timeout=(0.05, 0.05)),
                lambda: helper.get_sandbox_template("tmpl", "default", _request_timeout=(0.05, 0.05)),
                lambda: helper.create_sandbox_claim(
                    "b1-0", "pool-a", "default", _request_timeout=(0.05, 0.05), batch=True
                ),
                lambda: helper.delete_sandbox_claim(
                    "b1-0", "default", _request_timeout=(0.05, 0.05), batch=True
                ),
            )
            # Before listen(), connect is refused: all 8 batch calls fail after one attempt with
            # NewConnectionError, while non-batch calls retry and wrap in MaxRetryError.
            for batch_call in batch_calls:
                with self.assertRaises(urllib3.exceptions.NewConnectionError) as conn_err:
                    batch_call()
                self.assertNotIsInstance(conn_err.exception, urllib3.exceptions.MaxRetryError)
            for non_batch_call in (
                lambda: helper.create_sandbox_claim(
                    "single-0", "pool-a", "default", _request_timeout=(0.05, 0.05)
                ),
                lambda: helper.delete_sandbox_claim(
                    "single-0", "default", _request_timeout=(0.05, 0.05)
                ),
            ):
                with self.assertRaises(urllib3.exceptions.MaxRetryError):
                    non_batch_call()

            # Once listening without replying, a read timeout surfaces after one attempt without retrying.
            sock.listen(16)
            sock.setblocking(False)
            for batch_call in batch_calls:
                with self.assertRaises(urllib3.exceptions.ReadTimeoutError):
                    batch_call()
                attempts = 0
                while True:
                    try:
                        conn, _ = sock.accept()
                    except BlockingIOError:
                        break
                    conn.close()
                    attempts += 1
                self.assertEqual(attempts, 1)

            # A 503 with Retry-After surfaces as ApiException after one attempt without sleeping on
            # batch calls, while non-batch delete_sandbox_claim honors urllib3 retries.
            sock.settimeout(1.0)
            throttled_attempts = 0
            queued_replies: list[bytes] = []

            def reply_throttled():
                nonlocal throttled_attempts
                while True:
                    try:
                        conn, _ = sock.accept()
                    except OSError:
                        return
                    with conn:
                        conn.recv(4096)
                        throttled_attempts += 1
                        if queued_replies:
                            conn.sendall(queued_replies.pop(0))
                        else:
                            conn.sendall(
                                b"HTTP/1.1 503 Service Unavailable\r\n"
                                b"Retry-After: 3600\r\n"
                                b"Content-Length: 2\r\n"
                                b"Connection: close\r\n\r\n{}"
                            )

            server = threading.Thread(target=reply_throttled, daemon=True)
            server.start()
            try:
                for expected, call_fn in enumerate(
                    (
                        lambda: helper.delete_sandbox_claims_by_label(
                            "default", selector, _request_timeout=(5.0, 1.0)
                        ),
                        lambda: helper.list_sandbox_claim_objects(
                            "default", selector, _request_timeout=(5.0, 1.0)
                        ),
                        lambda: helper.count_live_sandbox_claims(
                            "default", selector, _request_timeout=(5.0, 1.0)
                        ),
                        lambda: helper.get_sandbox_warmpool(
                            "pool-a", "default", _request_timeout=(5.0, 1.0)
                        ),
                        lambda: helper.get_sandbox_template(
                            "tmpl", "default", _request_timeout=(5.0, 1.0)
                        ),
                        lambda: helper.create_sandbox_claim(
                            "b1-0", "pool-a", "default", _request_timeout=(5.0, 1.0), batch=True
                        ),
                        lambda: helper.delete_sandbox_claim(
                            "b1-0", "default", _request_timeout=(5.0, 1.0), batch=True
                        ),
                    ),
                    start=1,
                ):
                    start = time.monotonic()
                    with self.assertRaises(client.ApiException) as raised:
                        call_fn()
                    elapsed = time.monotonic() - start
                    self.assertEqual(raised.exception.status, 503)
                    self.assertEqual(raised.exception.headers["Retry-After"], "3600")
                    self.assertLess(elapsed, 1.0)
                    self.assertEqual(throttled_attempts, expected)

                before_non_batch = throttled_attempts
                queued_replies.extend([
                    (
                        b"HTTP/1.1 503 Service Unavailable\r\n"
                        b"Retry-After: 0\r\n"
                        b"Content-Length: 2\r\n"
                        b"Connection: close\r\n\r\n{}"
                    ),
                    b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}",
                ])
                helper.delete_sandbox_claim("single-0", "default", _request_timeout=(5.0, 1.0))
                self.assertEqual(throttled_attempts - before_non_batch, 2)
            finally:
                sock.close()
                server.join(timeout=1.0)


@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperPoolSize(unittest.TestCase):

    def test_pool_size_sizes_the_loaded_clients_pool(self, _):
        helper = K8sHelper(pool_size=7)
        self.addCleanup(helper.custom_objects_api.api_client.close)
        self.addCleanup(helper._batch_custom_objects_api.api_client.close)
        pool_manager = helper.custom_objects_api.api_client.rest_client.pool_manager
        batch_api_client = helper._batch_custom_objects_api.api_client
        batch_pool_manager = batch_api_client.rest_client.pool_manager
        self.assertEqual(pool_manager.connection_pool_kw["maxsize"], 7)
        self.assertEqual(batch_api_client.configuration.connection_pool_maxsize, 7)
        self.assertEqual(batch_pool_manager.connection_pool_kw["maxsize"], 7)
        self.assertEqual(helper.api_pool_size(), 7)
        self.assertIsNone(helper.injected_api_client)

    def test_a_zero_limit_reads_as_unbounded(self, _):
        configuration = client.Configuration()
        configuration.connection_pool_maxsize = 0
        with client.ApiClient(configuration=configuration) as injected:
            self.assertIsNone(K8sHelper(api_client=injected).api_pool_size())

    def test_rejects_an_invalid_pool_size(self, _):
        cases = {
            "zero": (0, None),
            "float": (1.5, None),
            "bool": (True, None),
            "with api_client": (5, client.ApiClient()),
        }
        for name, (pool_size, api_client) in cases.items():
            with self.subTest(name):
                with self.assertRaises(ValueError):
                    K8sHelper(api_client=api_client, pool_size=pool_size)


@patch("k8s_agent_sandbox.k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CoordinationV1Api")
@patch("k8s_agent_sandbox.k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.k8s_helper.config")
class TestK8sHelperBatchMethods(unittest.TestCase):

    def test_list_sandbox_claim_objects_pages_and_returns_the_first_pages_resource_version(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        mock_api.list_namespaced_custom_object.side_effect = [
            {
                "items": [{"metadata": {"name": "b1-0", "managedFields": [{"manager": "sdk"}]}, "spec": {}}],
                "metadata": {"resourceVersion": "42", "continue": "t1"},
            },
            # A page may be empty while more follow.
            {"items": [], "metadata": {"resourceVersion": "43", "continue": "t2"}},
            {
                "items": [{"metadata": {"name": "b1-1", "managedFields": [{"manager": "sdk"}]}, "spec": {}}],
                "metadata": {"resourceVersion": "44"},
            },
        ]

        helper = K8sHelper()
        items, rv = helper.list_sandbox_claim_objects("default", "agents.x-k8s.io/batch-id=b1")

        self.assertEqual(
            items,
            [{"metadata": {"name": "b1-0"}, "spec": {}}, {"metadata": {"name": "b1-1"}, "spec": {}}],
        )
        self.assertEqual(rv, "42")
        calls = mock_api.list_namespaced_custom_object.call_args_list
        self.assertEqual([c.kwargs["_continue"] for c in calls], [None, "t1", "t2"])
        for c in calls:
            self.assertEqual(c.kwargs["limit"], 500)
            self.assertEqual(c.kwargs["label_selector"], "agents.x-k8s.io/batch-id=b1")

    def test_list_sandbox_claim_objects_restarts_once_when_a_continue_token_expires(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        expired = client.ApiException(status=410)

        def page(name, rv, token=None):
            return {"items": [{"metadata": {"name": name}}], "metadata": {"resourceVersion": rv, "continue": token}}

        helper = K8sHelper()
        mock_api.list_namespaced_custom_object.side_effect = [
            page("old-0", "42", "t1"), expired, page("b1-0", "50", "t2"), page("b1-1", "51"),
        ]
        items, rv = helper.list_sandbox_claim_objects("default", "agents.x-k8s.io/batch-id=b1")
        self.assertEqual([item["metadata"]["name"] for item in items], ["b1-0", "b1-1"])
        self.assertEqual(rv, "50")
        self.assertEqual(
            [c.kwargs["_continue"] for c in mock_api.list_namespaced_custom_object.call_args_list],
            [None, "t1", None, "t2"],
        )

        for label, side_effect, expected_status in (
            ("second 410 after restart", [page("old-0", "42", "t1"), expired, page("b1-0", "50", "t2"), expired], 410),
            ("410 on first page", [expired], 410),
            ("non-410 mid-pagination", [page("old-0", "42", "t1"), client.ApiException(status=500)], 500),
        ):
            with self.subTest(label):
                mock_api.list_namespaced_custom_object.side_effect = side_effect
                with self.assertRaises(client.ApiException) as raised:
                    helper.list_sandbox_claim_objects("default", "agents.x-k8s.io/batch-id=b1")
                self.assertEqual(raised.exception.status, expected_status)

    def test_list_sandbox_claim_objects_deadline_caps_page_timeouts_and_stops_once_expired(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        expired = client.ApiException(status=410)

        def page(name, rv, token=None):
            return {"items": [{"metadata": {"name": name}}], "metadata": {"resourceVersion": rv, "continue": token}}

        helper = K8sHelper()
        for label, req_timeout, expected_timeouts in (
            ("scalar timeout", 10.0, [10.0, 8.0, 4.0, 1.5]),
            ("tuple timeout", (5.0, 10.0), [(5.0, 10.0), (5.0, 8.0), (4.0, 4.0), (1.5, 1.5)]),
            ("unset timeout", None, [15.0, 8.0, 4.0, 1.5]),
        ):
            with self.subTest(label), patch(
                "k8s_agent_sandbox.k8s_helper.time.monotonic", side_effect=[95.0, 102.0, 106.0, 108.5]
            ):
                mock_api.list_namespaced_custom_object.reset_mock()
                mock_api.list_namespaced_custom_object.side_effect = [
                    page("old-0", "42", "t1"),
                    expired,
                    page("b1-0", "50", "t2"),
                    page("b1-1", "51"),
                ]
                items, rv = helper.list_sandbox_claim_objects(
                    "default",
                    "agents.x-k8s.io/batch-id=b1",
                    _request_timeout=req_timeout,
                    deadline=110.0,
                )
                self.assertEqual([item["metadata"]["name"] for item in items], ["b1-0", "b1-1"])
                self.assertEqual(rv, "50")
                self.assertEqual(
                    [c.kwargs["_request_timeout"] for c in mock_api.list_namespaced_custom_object.call_args_list],
                    expected_timeouts,
                )

        for label, ticks, pages, expected_calls in (
            ("expired before first page", [110.0], [page("b1-0", "50")], 0),
            ("expired before next page", [105.0, 110.0], [page("b1-0", "50", "t1"), page("b1-1", "51")], 1),
        ):
            with self.subTest(label), patch(
                "k8s_agent_sandbox.k8s_helper.time.monotonic", side_effect=ticks
            ):
                mock_api.list_namespaced_custom_object.reset_mock()
                mock_api.list_namespaced_custom_object.side_effect = pages
                with self.assertRaises(TimeoutError):
                    helper.list_sandbox_claim_objects(
                        "default",
                        "agents.x-k8s.io/batch-id=b1",
                        _request_timeout=10.0,
                        deadline=110.0,
                    )
                self.assertEqual(mock_api.list_namespaced_custom_object.call_count, expected_calls)

    def test_count_live_sandbox_claims_counts_claims_not_being_deleted_across_pages(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        class _Page(dict):
            pass

        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        live = {"metadata": {"name": "live"}}
        deleting = {"metadata": {"name": "deleting", "deletionTimestamp": "2026-01-01T00:00:00Z"}}
        pages = [
            # A restart after an expired continue token drops the earlier pages from the count.
            _Page({"items": [live, live], "metadata": {"continue": "t0"}}),
            client.ApiException(status=410),
            _Page({"items": [live, deleting], "metadata": {"continue": "t1"}}),
            _Page({"items": [deleting, live, live], "metadata": {}}),
        ]
        prev_page_ref = None

        def next_page(**_):
            nonlocal prev_page_ref
            if prev_page_ref is not None:
                self.assertIsNone(prev_page_ref())
            item = pages.pop(0)
            if isinstance(item, Exception):
                raise item
            prev_page_ref = weakref.ref(item)
            return item

        mock_api.list_namespaced_custom_object.side_effect = next_page

        helper = K8sHelper()

        self.assertEqual(helper.count_live_sandbox_claims("default", "agents.x-k8s.io/batch-id=b1"), 3)

    def test_delete_sandbox_claims_by_label_drains_the_response_unparsed_and_releases_it(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        delete = mock_api.delete_collection_namespaced_custom_object
        response = delete.return_value
        response.closed = True
        chunks = iter([b'{"items": [', b"]}"])
        response.stream.return_value = chunks
        helper = K8sHelper()

        helper.delete_sandbox_claims_by_label("default", "agents.x-k8s.io/batch-id=b1")

        self.assertIs(delete.call_args.kwargs["_preload_content"], False)
        response.stream.assert_called_once_with(decode_content=False)
        self.assertEqual(list(chunks), [])
        response.close.assert_not_called()
        response.release_conn.assert_called_once()

        response.reset_mock()
        response.closed = False
        cleanup_order: list[str] = []
        response.close.side_effect = lambda: cleanup_order.append("close")
        response.release_conn.side_effect = lambda: cleanup_order.append("release_conn")
        response.stream.side_effect = urllib3.exceptions.ProtocolError("connection broken")
        with self.assertRaises(urllib3.exceptions.ProtocolError):
            helper.delete_sandbox_claims_by_label("default", "agents.x-k8s.io/batch-id=b1")
        self.assertEqual(cleanup_order, ["close", "release_conn"])

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_sandbox_claims_yields_events(
        self, mock_watch_class, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_watch = MagicMock()
        event = {"type": "MODIFIED", "object": {"metadata": {"name": "b1-0"}}}
        mock_watch.stream.return_value = [event]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        events = list(
            helper.watch_sandbox_claims(
                "default", "agents.x-k8s.io/batch-id=b1", "5", 30, _request_timeout=35
            )
        )

        self.assertEqual(events, [event])
        self.assertEqual(mock_watch.stream.call_args.kwargs["resource_version"], "5")
        self.assertEqual(mock_watch.stream.call_args.kwargs["_request_timeout"], 35)
        self.assertEqual(mock_watch.stream.call_args.kwargs["label_selector"], "agents.x-k8s.io/batch-id=b1")
        self.assertIs(mock_watch.stream.call_args.kwargs["allow_watch_bookmarks"], True)
        mock_watch.stop.assert_called_once()

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_sandbox_claims_skips_none_events(
        self, mock_watch_class, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_watch = MagicMock()
        event = {"type": "MODIFIED", "object": {"metadata": {"name": "b1-0"}}}
        mock_watch.stream.return_value = [None, event, None]
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        events = list(
            helper.watch_sandbox_claims("default", "agents.x-k8s.io/batch-id=b1", "5", 30)
        )

        self.assertEqual(events, [event])

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_watch_sandbox_claims_stops_watch_on_error(
        self, mock_watch_class, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_watch = MagicMock()

        def _stream(*args, **kwargs):
            raise client.ApiException(status=410)
            yield  # unreachable; used to make this a generator function

        mock_watch.stream = _stream
        mock_watch_class.return_value = mock_watch

        helper = K8sHelper()
        with self.assertRaises(client.ApiException):
            list(helper.watch_sandbox_claims("default", "agents.x-k8s.io/batch-id=b1", "5", 30))

        mock_watch.stop.assert_called_once()

    def test_read_batch_lease_returns_lease_on_success(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))
        mock_coord.read_namespaced_lease.return_value = lease

        helper = K8sHelper()
        result = helper.read_batch_lease("batch-b1", "default")

        self.assertIs(result, lease)
        mock_coord.read_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", _request_timeout=None
        )

    def test_read_batch_lease_forwards_request_timeout(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord

        helper = K8sHelper()
        helper.read_batch_lease("batch-b1", "default", _request_timeout=20)

        mock_coord.read_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", _request_timeout=20
        )

    def test_read_batch_lease_returns_none_on_404(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        mock_coord.read_namespaced_lease.side_effect = client.ApiException(status=404)

        helper = K8sHelper()
        self.assertIsNone(helper.read_batch_lease("batch-b1", "default"))

    def test_read_batch_lease_reraises_non_404(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        mock_coord.read_namespaced_lease.side_effect = client.ApiException(status=403)

        helper = K8sHelper()
        with self.assertRaises(client.ApiException):
            helper.read_batch_lease("batch-b1", "default")

    def test_replace_batch_lease_forwards_body(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))
        updated_lease = client.V1Lease(
            metadata=client.V1ObjectMeta(name="batch-b1", resource_version="7")
        )
        mock_coord.replace_namespaced_lease.return_value = updated_lease

        helper = K8sHelper()
        result = helper.replace_batch_lease("batch-b1", "default", lease)

        mock_coord.replace_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", lease, _request_timeout=None
        )
        self.assertIs(result, updated_lease)

    def test_replace_batch_lease_forwards_request_timeout(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))

        helper = K8sHelper()
        helper.replace_batch_lease("batch-b1", "default", lease, _request_timeout=20)

        mock_coord.replace_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", lease, _request_timeout=20
        )

    def test_claim_batch_requests_forward_resource_selector_and_timeout(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        mock_api.list_namespaced_custom_object.return_value = {"items": [], "metadata": {}}
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        helper = K8sHelper()
        selector = "agents.x-k8s.io/batch-id=b1"
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))
        cases = [
            ("create_sandbox_claim", lambda: helper.create_sandbox_claim(
                "b1-0", "pool-a", "default", log_level=logging.DEBUG, _request_timeout=30),
             mock_api.create_namespaced_custom_object, {"plural": "sandboxclaims"}),
            ("list_sandbox_claim_objects", lambda: helper.list_sandbox_claim_objects(
                "default", selector, _request_timeout=30),
             mock_api.list_namespaced_custom_object,
             {"plural": "sandboxclaims", "label_selector": selector}),
            ("count_live_sandbox_claims", lambda: helper.count_live_sandbox_claims(
                "default", selector, _request_timeout=30),
             mock_api.list_namespaced_custom_object,
             {"plural": "sandboxclaims", "label_selector": selector}),
            ("delete_sandbox_claims_by_label", lambda: helper.delete_sandbox_claims_by_label(
                "default", selector, _request_timeout=30),
             mock_api.delete_collection_namespaced_custom_object,
             {"group": "extensions.agents.x-k8s.io", "plural": "sandboxclaims",
              "label_selector": selector}),
            ("get_sandbox_warmpool", lambda: helper.get_sandbox_warmpool(
                "pool-a", "default", _request_timeout=30),
             mock_api.get_namespaced_custom_object,
             {"group": "extensions.agents.x-k8s.io", "plural": "sandboxwarmpools", "name": "pool-a"}),
            ("get_sandbox_template", lambda: helper.get_sandbox_template(
                "tmpl", "default", _request_timeout=30),
             mock_api.get_namespaced_custom_object,
             {"group": "extensions.agents.x-k8s.io", "plural": "sandboxtemplates", "name": "tmpl"}),
            ("create_batch_lease", lambda: helper.create_batch_lease(
                "default", lease, _request_timeout=30),
             mock_coord.create_namespaced_lease, {}),
            ("delete_batch_lease", lambda: helper.delete_batch_lease(
                "batch-b1", "default", _request_timeout=30),
             mock_coord.delete_namespaced_lease, {}),
        ]
        for name, call, api_method, expected in cases:
            with self.subTest(name):
                api_method.reset_mock()
                call()
                kwargs = api_method.call_args.kwargs
                self.assertEqual(kwargs["_request_timeout"], 30)
                for key, value in expected.items():
                    self.assertEqual(kwargs[key], value)

    def test_claim_create_and_delete_log_at_the_given_level(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        helper = K8sHelper()
        calls = {
            "create": lambda: helper.create_sandbox_claim("b1-0", "pool-a", "default", log_level=logging.DEBUG),
            "delete": lambda: helper.delete_sandbox_claim("b1-0", "default", log_level=logging.DEBUG),
        }
        for name, call in calls.items():
            with self.subTest(name):
                with self.assertLogs(level="DEBUG") as logs:
                    call()
                self.assertEqual([r.levelno for r in logs.records], [logging.DEBUG])

    def test_warmpool_and_template_gets_return_none_and_lease_delete_ignores_404_other_errors_raise(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_api = MagicMock()
        mock_api_cls.return_value = mock_api
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        helper = K8sHelper()

        mock_api.get_namespaced_custom_object.side_effect = client.ApiException(status=404)
        mock_coord.delete_namespaced_lease.side_effect = client.ApiException(status=404)
        self.assertIsNone(helper.get_sandbox_warmpool("pool-a", "default"))
        self.assertIsNone(helper.get_sandbox_template("tmpl", "default"))
        helper.delete_batch_lease("batch-b1", "default")

        mock_api.get_namespaced_custom_object.side_effect = client.ApiException(status=403)
        mock_coord.delete_namespaced_lease.side_effect = client.ApiException(status=403)
        with self.assertRaises(client.ApiException):
            helper.get_sandbox_warmpool("pool-a", "default")
        with self.assertRaises(client.ApiException):
            helper.delete_batch_lease("batch-b1", "default")

    def test_lease_delete_at_a_resource_version_sends_a_precondition_and_counts_a_conflict_as_deleted(
        self, mock_config, mock_api_cls, mock_coord_cls, mock_core_cls
    ):
        mock_coord = MagicMock()
        mock_coord_cls.return_value = mock_coord
        helper = K8sHelper()

        with self.assertRaises(ValueError):
            helper.delete_batch_lease("batch-b1", "default", resource_version="")
        mock_coord.delete_namespaced_lease.assert_not_called()

        helper.delete_batch_lease("batch-b1", "default", resource_version="11")
        body = mock_coord.delete_namespaced_lease.call_args.kwargs["body"]
        self.assertEqual(body.preconditions.resource_version, "11")

        mock_coord.delete_namespaced_lease.side_effect = client.ApiException(status=409)
        helper.delete_batch_lease("batch-b1", "default", resource_version="11")
        with self.assertRaises(client.ApiException):
            helper.delete_batch_lease("batch-b1", "default")


if __name__ == '__main__':
    unittest.main()
