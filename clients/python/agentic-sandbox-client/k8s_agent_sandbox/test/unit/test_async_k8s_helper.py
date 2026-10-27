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
import unittest
import weakref
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

pytest.importorskip("kubernetes_asyncio")

import aiohttp
from kubernetes_asyncio import client

from k8s_agent_sandbox.models import AdditionalPodMetadata
from k8s_agent_sandbox.async_k8s_helper import AsyncK8sHelper
from k8s_agent_sandbox.exceptions import SandboxClaimFailedError, SandboxMetadataError, SandboxTemplateNotFoundError
from k8s_agent_sandbox.constants import CLIENT_REQUEST_TIME_ANNOTATION


class TestAsyncK8sHelperCreateSandboxClaim(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()
        self.helper.custom_objects_api.create_namespaced_custom_object = AsyncMock()
        self.helper.core_v1_api = MagicMock()

    async def test_env_included_in_manifest(self):
        await self.helper.create_sandbox_claim(
            "test-claim",
            "test-warmpool",
            "test-namespace",
            env={"FOO": "bar", "DEBUG": "true"},
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(
            body["spec"]["env"],
            [
                {"name": "FOO", "value": "bar"},
                {"name": "DEBUG", "value": "true"},
            ],
        )

    async def test_owner_references_included_in_manifest_only_when_given(self):
        owner = [{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "name": "batch-b1", "uid": "u1"}]
        create = self.helper.custom_objects_api.create_namespaced_custom_object
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace", owner_references=owner
        )
        self.assertEqual(create.call_args.kwargs["body"]["metadata"]["ownerReferences"], owner)
        await self.helper.create_sandbox_claim("test-claim", "test-warmpool", "test-namespace")
        self.assertNotIn("ownerReferences", create.call_args.kwargs["body"]["metadata"])

    async def test_lifecycle_included_in_manifest(self):
        lifecycle = {
            "shutdownTime": "2026-12-31T23:59:59Z",
            "shutdownPolicy": "Delete",
        }
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace", lifecycle=lifecycle
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["spec"]["warmPoolRef"]["name"], "test-warmpool")

    async def test_no_lifecycle_omits_key(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace"
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertNotIn("lifecycle", body["spec"])
        self.assertEqual(body["metadata"]["labels"], {"agents.x-k8s.io/created-by": "python-client"})

    async def test_lifecycle_with_labels_and_annotations(self):
        lifecycle = {
            "shutdownTime": "2026-06-15T12:00:00Z",
            "shutdownPolicy": "Delete",
        }
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={"key": "val"},
            labels={"agent": "test"},
            lifecycle=lifecycle,
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["spec"]["lifecycle"], lifecycle)
        self.assertEqual(body["metadata"]["labels"], {"agent": "test", "agents.x-k8s.io/created-by": "python-client"})
        self.assertEqual(body["metadata"]["annotations"]["key"], "val")
        self.assertIn(CLIENT_REQUEST_TIME_ANNOTATION, body["metadata"]["annotations"])

    async def test_pod_metadata_included_in_manifest(self):
        pod_metadata = AdditionalPodMetadata(labels={"client-id": "tenant-a"})
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace", pod_metadata=pod_metadata
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(
            body["spec"]["additionalPodMetadata"]["labels"]["client-id"], "tenant-a"
        )

    async def test_pod_metadata_dict_backward_compatible(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            pod_metadata={"labels": {"client-id": "tenant-a"}, "annotations": {"a": "b"}},
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        self.assertEqual(
            call_kwargs["body"]["spec"]["additionalPodMetadata"],
            {"labels": {"client-id": "tenant-a"}, "annotations": {"a": "b"}},
        )

    async def test_no_pod_metadata_omits_key(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace"
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertNotIn("additionalPodMetadata", body["spec"])

    async def test_created_by_label_override_rejected(self):
        await self.helper.create_sandbox_claim(
            "test-claim", "test-warmpool", "test-namespace",
            labels={"agent": "test", "agents.x-k8s.io/created-by": "foo"},
        )

        call_kwargs = self.helper.custom_objects_api.create_namespaced_custom_object.call_args.kwargs
        body = call_kwargs["body"]
        self.assertEqual(body["metadata"]["labels"], {"agent": "test", "agents.x-k8s.io/created-by": "python-client"})


class TestAsyncK8sHelperResolveSandboxName(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()
        self.helper.core_v1_api = MagicMock()

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_resolve_sandbox_name_template_not_found(self, mock_watch_class):
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
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

        async def mock_stream(*args, **kwargs):
            yield mock_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        with self.assertRaises(SandboxTemplateNotFoundError) as context:
            await self.helper.resolve_sandbox_name("test-claim", "default", timeout=5)

        self.assertIn("Template 'non-existent-template' not found", str(context.exception))

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_resolve_sandbox_name_deleted_event(self, mock_watch_class):
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        mock_event = {
            "type": "DELETED",
            "object": {
                "metadata": {"name": "test-claim"}
            }
        }

        async def mock_stream(*args, **kwargs):
            yield mock_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        with self.assertRaises(SandboxMetadataError) as context:
            await self.helper.resolve_sandbox_name("test-claim", "default", timeout=5)

        self.assertIn("SandboxClaim 'test-claim' was deleted while resolving sandbox name", str(context.exception))

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_wait_for_claim_ready_single_event(self, mock_watch_class):
        """Warm-pool fast path: name + Ready arrive in one claim status update."""
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
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

        async def mock_stream(*args, **kwargs):
            yield mock_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        name = await self.helper.wait_for_claim_ready("test-claim", "default", timeout=5)
        self.assertEqual(name, "warm-sandbox-1")
        self.assertEqual(mock_watch_class.call_count, 1)

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_wait_for_claim_ready_name_before_ready(self, mock_watch_class):
        """Cold-start path: the name lands first, Ready arrives on a later event."""
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
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

        async def mock_stream(*args, **kwargs):
            yield name_only_event
            yield ready_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        name = await self.helper.wait_for_claim_ready("test-claim", "default", timeout=5)
        self.assertEqual(name, "cold-sandbox-1")

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_wait_for_claim_ready_terminal_reason_fails_fast(self, mock_watch_class):
        """A terminal Ready=False reason raises immediately instead of waiting out the timeout."""
        cases = [
            ("InvalidConfiguration", "Service 'test-claim' is invalid: must be no more than 63 characters"),
            ("PodFailed", "Pod failed"),
            ("PodSucceeded", "Pod completed successfully"),
        ]
        for reason, message in cases:
            with self.subTest(reason=reason):
                mock_watch = MagicMock()
                mock_watch.close = AsyncMock()
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

                async def mock_stream(*args, **kwargs):
                    yield mock_event

                mock_watch.stream = mock_stream
                mock_watch_class.return_value = mock_watch

                with self.assertRaises(SandboxClaimFailedError) as context:
                    await self.helper.wait_for_claim_ready("test-claim", "default", timeout=5)
                self.assertIn(reason, str(context.exception))

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_watch_resource_version_passthrough(self, mock_watch_class):
        """The ready-wait watch starts from the supplied resourceVersion
        ("0" by default) so it never forces a quorum etcd read."""
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim", "resourceVersion": "7"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "sandbox": {"name": "warm-sandbox-1"},
                },
            },
        }

        seen_kwargs = {}

        async def mock_stream(*args, **kwargs):
            seen_kwargs.update(kwargs)
            yield mock_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        name = await self.helper.wait_for_claim_ready(
            "test-claim", "default", timeout=5, resource_version="12345")
        self.assertEqual(name, "warm-sandbox-1")
        self.assertEqual(seen_kwargs["resource_version"], "12345")

        seen_kwargs.clear()
        name = await self.helper.wait_for_claim_ready("test-claim", "default", timeout=5)
        self.assertEqual(seen_kwargs["resource_version"], "0")

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_watch_410_gone_restarts_from_zero(self, mock_watch_class):
        """A compacted-away resourceVersion (410 Gone) restarts the watch
        from "0" instead of failing the wait."""
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        mock_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim", "resourceVersion": "7"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "sandbox": {"name": "warm-sandbox-1"},
                },
            },
        }

        stream_rvs = []

        async def mock_stream(*args, **kwargs):
            stream_rvs.append(kwargs.get("resource_version"))
            if len(stream_rvs) == 1:
                raise client.ApiException(status=410)
            yield mock_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        name = await self.helper.wait_for_claim_ready(
            "test-claim", "default", timeout=5, resource_version="12345")
        self.assertEqual(name, "warm-sandbox-1")
        self.assertEqual(stream_rvs, ["12345", "0"])

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_watch_transient_disconnect_resumes_from_tracked_resource_version(self, mock_watch_class):
        """A transient network disconnect (aiohttp.ClientError) resumes from the tracked
        resourceVersion instead of resetting to '0'."""
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        first_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim", "resourceVersion": "5555"},
                "status": {"conditions": []},
            },
        }
        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-claim", "resourceVersion": "7777"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "sandbox": {"name": "warm-sandbox-1"},
                },
            },
        }

        stream_rvs = []

        async def mock_stream(*args, **kwargs):
            stream_rvs.append(kwargs.get("resource_version"))
            if len(stream_rvs) == 1:
                yield first_event
                raise aiohttp.ClientConnectionError("Connection reset by peer")
            yield ready_event

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        name = await self.helper.wait_for_claim_ready(
            "test-claim", "default", timeout=5, resource_version="12345")
        self.assertEqual(name, "warm-sandbox-1")
        self.assertEqual(stream_rvs, ["12345", "5555"])

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_async_watch_non_transient_network_exception_reraises(self, mock_watch_class):
        """Non-transient exceptions (such as ClientSSLError) must not be caught and retried."""
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()

        async def mock_stream(*args, **kwargs):
            raise aiohttp.ClientSSLError(MagicMock(), OSError("certificate verify failed"))
            yield  # make this an async generator

        mock_watch.stream = mock_stream
        mock_watch_class.return_value = mock_watch

        with self.assertRaises(aiohttp.ClientSSLError):
            await self.helper.wait_for_claim_ready("test-claim", "default", timeout=5)


class TestAsyncK8sHelperWaitForSandboxReady(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()

    async def test_returns_first_pod_ip_when_ready(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "status": {
                        "conditions": [{"type": "Ready", "status": "True"}],
                        "podIPs": ["::ffff:10.244.0.5", "fd00::5"],
                    }
                },
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            result = await self.helper.wait_for_sandbox_ready("my-sandbox", "default", timeout=10)

        self.assertEqual(result, "10.244.0.5")

    async def test_returns_none_when_no_pod_ips(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "status": {
                        "conditions": [{"type": "Ready", "status": "True"}],
                    }
                },
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            result = await self.helper.wait_for_sandbox_ready("my-sandbox", "default", timeout=10)

        self.assertIsNone(result)

    async def test_wait_for_sandbox_ready_transient_disconnect_reconnects(self):
        """wait_for_sandbox_ready reconnects upon transient aiohttp.ClientError."""
        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "my-sandbox"},
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "podIPs": ["10.244.0.5"],
                },
            },
        }

        call_count = 0

        async def _async_gen(*args, **kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                raise aiohttp.ClientConnectionError("Connection lost")
            yield ready_event

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            result = await self.helper.wait_for_sandbox_ready("my-sandbox", "default", timeout=10)

        self.assertEqual(result, "10.244.0.5")
        self.assertEqual(call_count, 2)


class TestAsyncK8sHelperDeleteSandboxClaim(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()
        self.helper.core_v1_api = MagicMock()

    async def test_delete_404_is_ignored(self):
        from kubernetes_asyncio import client as async_client
        exc = async_client.ApiException(status=404)
        self.helper.custom_objects_api.delete_namespaced_custom_object = AsyncMock(side_effect=exc)

        await self.helper.delete_sandbox_claim("missing-claim", "default")

    async def test_delete_non_404_reraises(self):
        from kubernetes_asyncio import client as async_client
        exc = async_client.ApiException(status=403)
        self.helper.custom_objects_api.delete_namespaced_custom_object = AsyncMock(side_effect=exc)

        with self.assertRaises(async_client.ApiException) as ctx:
            await self.helper.delete_sandbox_claim("claim", "default")
        self.assertEqual(ctx.exception.status, 403)


class TestAsyncK8sHelperWaitForGatewayIP(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()

    async def test_wait_for_gateway_ip_valid_ip(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_valid_hostname(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "gateway.example.com"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "gateway.example.com")

    async def test_wait_for_gateway_ip_invalid_address_special_chars(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1/path"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_invalid_hostname(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "bad_hostname"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_multiple_addresses_in_event(self):
        async def _async_gen(*args, **kwargs):
            yield {
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

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.2")

    async def test_wait_for_gateway_ip_accepts_ipv6(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "2001:db8::1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "2001:db8::1")

    async def test_wait_for_gateway_ip_disguised_ip_decimal(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "2130706433"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_disguised_ip_hex(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "0x7f000001"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_disguised_ip_dotted_hex(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "0x7f.0x0.0x0.0x1"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                }
            }
        }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_bare_hex_prefix_ip(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "0x.1"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_bare_hex_prefix_ip_dotted(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "00.0x.0x.1"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_invalid_label_length(self):
        long_label = "a" * 64
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": f"{long_label}.example.com"}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "gateway.example.com"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "gateway.example.com")

    async def test_wait_for_gateway_ip_non_dict_address(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": ["not-a-dict"]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")

    async def test_wait_for_gateway_ip_integer_value(self):
        async def _async_gen(*args, **kwargs):
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": 2130706433}]
                    }
                }
            }
            yield {
                "type": "MODIFIED",
                "object": {
                    "metadata": {"name": "test-gateway"},
                    "status": {
                        "addresses": [{"value": "192.168.1.1"}]
                    }
                }
            }

        with patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as MockWatch:
            mock_watch = MagicMock()
            mock_watch.stream = _async_gen
            mock_watch.close = AsyncMock()
            MockWatch.return_value = mock_watch

            ip = await self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=5)
            self.assertEqual(ip, "192.168.1.1")


@patch("k8s_agent_sandbox.async_k8s_helper.client.CoreV1Api")
@patch("k8s_agent_sandbox.async_k8s_helper.client.CustomObjectsApi")
@patch("k8s_agent_sandbox.async_k8s_helper.client.ApiClient")
@patch("k8s_agent_sandbox.async_k8s_helper.config")
class TestAsyncK8sHelperApiClientInjection(unittest.IsolatedAsyncioTestCase):

    async def test_injected_api_client_used_and_no_config_loaded(
        self, mock_config, mock_api_client_cls, mock_custom_cls, mock_core_cls
    ):
        injected = AsyncMock(name="ApiClient")
        helper = AsyncK8sHelper(api_client=injected)
        await helper._ensure_initialized()
        mock_config.load_incluster_config.assert_not_called()
        mock_config.load_kube_config.assert_not_called()
        mock_api_client_cls.assert_not_called()
        mock_custom_cls.assert_called_once_with(injected)
        mock_core_cls.assert_called_once_with(injected)

        # A caller-injected client must not be closed by the helper.
        await helper.close()
        injected.close.assert_not_awaited()

    async def test_injected_api_client_is_exposed_only_when_injected(
        self, mock_config, mock_api_client_cls, mock_custom_cls, mock_core_cls
    ):
        injected = AsyncMock(name="ApiClient")
        self.assertIs(AsyncK8sHelper(api_client=injected).injected_api_client, injected)

        ambient = AsyncK8sHelper()
        await ambient._ensure_initialized()
        self.assertIsNone(ambient.injected_api_client)

    async def test_injected_api_client_still_used_after_close(
        self, mock_config, mock_api_client_cls, mock_custom_cls, mock_core_cls
    ):
        injected = AsyncMock(name="ApiClient")
        helper = AsyncK8sHelper(api_client=injected)

        await helper.close()
        await helper._ensure_initialized()

        mock_config.load_incluster_config.assert_not_called()
        mock_config.load_kube_config.assert_not_called()
        mock_api_client_cls.assert_not_called()
        mock_custom_cls.assert_called_once_with(injected)
        mock_core_cls.assert_called_once_with(injected)

    async def test_internally_created_api_client_is_closed(
        self, mock_config, mock_api_client_cls, mock_custom_cls, mock_core_cls
    ):
        created = AsyncMock(name="ApiClient")
        mock_api_client_cls.return_value = created
        helper = AsyncK8sHelper()
        await helper._ensure_initialized()
        mock_api_client_cls.assert_called_once_with()

        # A client the helper created must be closed.
        await helper.close()
        created.close.assert_awaited_once()


class TestAsyncK8sHelperWatchTimeout(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()

    async def test_one_second_wait_observes_ready_resource(self):
        ready = {"type": "Ready", "status": "True"}
        cases = [
            ("resolve_sandbox_name", {"sandbox": {"name": "sandbox"}}, "sandbox"),
            ("wait_for_claim_ready", {"sandbox": {"name": "sandbox"}, "conditions": [ready]}, "sandbox"),
            ("wait_for_sandbox_ready", {"conditions": [ready], "podIPs": [{"ip": "10.0.0.1"}]}, "10.0.0.1"),
            ("wait_for_gateway_ip", {"addresses": [{"value": "10.0.0.2"}]}, "10.0.0.2"),
        ]
        for method, status, expected in cases:
            async def events():
                yield {"type": "ADDED", "object": {"status": status}}

            with self.subTest(method=method), patch(
                "k8s_agent_sandbox.async_k8s_helper.time", **{"monotonic.side_effect": [100.0, 100.1]}
            ), patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as watch_cls:
                watch_cls.return_value.stream.return_value = events()
                watch_cls.return_value.close = AsyncMock()
                self.assertEqual(await getattr(self.helper, method)("resource", "default", timeout=1), expected)
                self.assertEqual(watch_cls.return_value.stream.call_args.kwargs["timeout_seconds"], 1)

    async def test_claim_watch_restarts_during_final_fractional_second(self):
        async def empty_events():
            if False:
                yield None

        async def ready_events():
            yield {"type": "ADDED", "object": {"status": {"sandbox": {"name": "sandbox"}}}}

        with patch(
            "k8s_agent_sandbox.async_k8s_helper.time", **{"monotonic.side_effect": [100.0, 100.1, 104.5]}
        ), patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as watch_cls:
            watch_cls.return_value.stream.side_effect = [empty_events(), ready_events()]
            watch_cls.return_value.close = AsyncMock()
            self.assertEqual(await self.helper.resolve_sandbox_name("claim", "default", timeout=5), "sandbox")
            self.assertEqual(watch_cls.return_value.stream.call_count, 2)
            self.assertEqual(watch_cls.return_value.stream.call_args.kwargs["timeout_seconds"], 1)

    async def test_expired_wait_does_not_start_watch(self):
        with patch(
            "k8s_agent_sandbox.async_k8s_helper.time", **{"monotonic.side_effect": [100.0, 101.0]}
        ), patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch") as watch_cls:
            with self.assertRaises(TimeoutError):
                await self.helper.resolve_sandbox_name("claim", "default", timeout=1)
            watch_cls.assert_not_called()


@patch("k8s_agent_sandbox.async_k8s_helper.config")
class TestAsyncK8sHelperPoolSize(unittest.IsolatedAsyncioTestCase):

    async def test_pool_size_sizes_the_loaded_clients_pool(self, _):
        helper = AsyncK8sHelper(pool_size=7)
        self.assertEqual(await helper.api_pool_size(), 7)
        connector = helper.custom_objects_api.api_client.rest_client.pool_manager.connector
        self.assertEqual(connector.limit, 7)
        self.assertIsNone(helper.injected_api_client)
        await helper.close()

    async def test_a_zero_limit_reads_as_unbounded(self, _):
        configuration = client.Configuration()
        configuration.connection_pool_maxsize = 0
        injected = client.ApiClient(configuration=configuration)
        self.assertIsNone(await AsyncK8sHelper(api_client=injected).api_pool_size())
        await injected.close()

    async def test_rejects_pool_size_with_an_injected_client(self, _):
        with self.assertRaises(ValueError):
            AsyncK8sHelper(api_client=MagicMock(), pool_size=5)


class TestAsyncK8sHelperBatchMethods(unittest.IsolatedAsyncioTestCase):

    async def asyncSetUp(self):
        self.helper = AsyncK8sHelper()
        self.helper._initialized = True
        self.helper.custom_objects_api = MagicMock()
        self.helper.core_v1_api = MagicMock()
        self.helper.coordination_v1_api = MagicMock()

    async def test_list_sandbox_claim_objects_pages_and_returns_the_first_pages_resource_version(self):
        list_page = self.helper.custom_objects_api.list_namespaced_custom_object = AsyncMock(side_effect=[
            {"items": [{"metadata": {"name": "b1-0"}}], "metadata": {"resourceVersion": "42", "continue": "t1"}},
            # A page may be empty while more follow.
            {"items": [], "metadata": {"resourceVersion": "43", "continue": "t2"}},
            {"items": [{"metadata": {"name": "b1-1"}}], "metadata": {"resourceVersion": "44"}},
        ])

        items, rv = await self.helper.list_sandbox_claim_objects(
            "default", "agents.x-k8s.io/batch-id=b1"
        )

        self.assertEqual([item["metadata"]["name"] for item in items], ["b1-0", "b1-1"])
        self.assertEqual(rv, "42")
        calls = list_page.call_args_list
        self.assertEqual([c.kwargs["_continue"] for c in calls], [None, "t1", "t2"])
        for c in calls:
            self.assertEqual(c.kwargs["limit"], 500)
            self.assertEqual(c.kwargs["label_selector"], "agents.x-k8s.io/batch-id=b1")

    async def test_list_sandbox_claim_objects_restarts_once_when_a_continue_token_expires(self):
        expired = client.ApiException(status=410)

        def page(name, rv, token=None):
            return {"items": [{"metadata": {"name": name}}], "metadata": {"resourceVersion": rv, "continue": token}}

        list_page = self.helper.custom_objects_api.list_namespaced_custom_object = AsyncMock(side_effect=[
            page("old-0", "42", "t1"), expired, page("b1-0", "50", "t2"), page("b1-1", "51"),
        ])
        items, rv = await self.helper.list_sandbox_claim_objects("default", "agents.x-k8s.io/batch-id=b1")
        self.assertEqual([item["metadata"]["name"] for item in items], ["b1-0", "b1-1"])
        self.assertEqual(rv, "50")
        self.assertEqual([c.kwargs["_continue"] for c in list_page.call_args_list], [None, "t1", None, "t2"])

        for label, side_effect, expected_status in (
            ("second 410 after restart", [page("old-0", "42", "t1"), expired, page("b1-0", "50", "t2"), expired], 410),
            ("410 on first page", [expired], 410),
            ("non-410 mid-pagination", [page("old-0", "42", "t1"), client.ApiException(status=500)], 500),
        ):
            with self.subTest(label):
                list_page.side_effect = side_effect
                with self.assertRaises(client.ApiException) as raised:
                    await self.helper.list_sandbox_claim_objects("default", "agents.x-k8s.io/batch-id=b1")
                self.assertEqual(raised.exception.status, expected_status)

    async def test_count_live_sandbox_claims_counts_claims_not_being_deleted_across_pages(self):
        class _Page(dict):
            pass

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

        self.helper.custom_objects_api.list_namespaced_custom_object = AsyncMock(side_effect=next_page)

        self.assertEqual(await self.helper.count_live_sandbox_claims("default", "agents.x-k8s.io/batch-id=b1"), 3)

    async def test_delete_sandbox_claims_by_label_drains_the_response_unparsed_and_releases_it(self):
        drained = []

        async def chunks(error=None):
            for chunk in (b'{"items": [', b"]}"):
                drained.append(chunk)
                yield chunk
            if error is not None:
                raise error

        response = MagicMock(status=200)
        response.content.iter_any.side_effect = chunks
        delete = self.helper.custom_objects_api.delete_collection_namespaced_custom_object = AsyncMock(
            return_value=response
        )

        await self.helper.delete_sandbox_claims_by_label("default", "agents.x-k8s.io/batch-id=b1")

        self.assertIs(delete.call_args.kwargs["_preload_content"], False)
        self.assertEqual(len(drained), 2)
        response.release.assert_called_once()

        response.reset_mock()
        response.content.iter_any.side_effect = lambda: chunks(aiohttp.ClientPayloadError("connection broken"))
        with self.assertRaises(aiohttp.ClientPayloadError):
            await self.helper.delete_sandbox_claims_by_label("default", "agents.x-k8s.io/batch-id=b1")
        response.release.assert_called_once()

    async def test_delete_sandbox_claims_by_label_raises_for_an_error_status_and_releases_the_response(self):
        # Without preloading, kubernetes_asyncio returns an error response without raising.
        response = MagicMock(status=429, reason="Too Many Requests", headers={"Retry-After": "7"})
        response.read = AsyncMock(return_value=b'{"kind": "Status", "code": 429}')
        self.helper.custom_objects_api.delete_collection_namespaced_custom_object = AsyncMock(return_value=response)

        with self.assertRaises(client.ApiException) as raised:
            await self.helper.delete_sandbox_claims_by_label("default", "agents.x-k8s.io/batch-id=b1")

        self.assertEqual(raised.exception.status, 429)
        self.assertEqual(raised.exception.headers["Retry-After"], "7")
        self.assertEqual(raised.exception.body, '{"kind": "Status", "code": 429}')
        response.release.assert_called_once()

        binary_error = MagicMock(status=502, reason="Bad Gateway", headers={})
        binary_error.read = AsyncMock(return_value=b"\xff\xfeproxy error")
        self.helper.custom_objects_api.delete_collection_namespaced_custom_object = AsyncMock(
            return_value=binary_error
        )
        with self.assertRaises(client.ApiException) as raised:
            await self.helper.delete_sandbox_claims_by_label("default", "agents.x-k8s.io/batch-id=b1")
        self.assertEqual(raised.exception.status, 502)
        self.assertIsInstance(raised.exception.body, str)
        self.assertIn("\ufffdproxy error", raised.exception.body)
        binary_error.release.assert_called_once()

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_watch_sandbox_claims_yields_events(self, mock_watch_class):
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        event = {"type": "MODIFIED", "object": {"metadata": {"name": "b1-0"}}}
        captured_kwargs = {}

        async def _stream(*args, **kwargs):
            captured_kwargs.update(kwargs)
            yield event

        mock_watch.stream = _stream
        mock_watch_class.return_value = mock_watch

        events = [
            e async for e in self.helper.watch_sandbox_claims(
                "default", "agents.x-k8s.io/batch-id=b1", "5", 30, _request_timeout=35
            )
        ]

        self.assertEqual(events, [event])
        self.assertEqual(captured_kwargs["resource_version"], "5")
        self.assertEqual(captured_kwargs["_request_timeout"], 35)
        self.assertEqual(captured_kwargs["label_selector"], "agents.x-k8s.io/batch-id=b1")
        self.assertIs(captured_kwargs["allow_watch_bookmarks"], True)
        mock_watch.close.assert_awaited_once()

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_watch_sandbox_claims_skips_none_and_undecodable_events(self, mock_watch_class):
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()
        event = {"type": "MODIFIED", "object": {"metadata": {"name": "b1-0"}}}

        async def _stream(*args, **kwargs):
            yield None
            yield event
            # kubernetes_asyncio returns a line it can't decode as is.
            yield "not json"

        mock_watch.stream = _stream
        mock_watch_class.return_value = mock_watch

        events = [
            e async for e in self.helper.watch_sandbox_claims(
                "default", "agents.x-k8s.io/batch-id=b1", "5", 30
            )
        ]

        self.assertEqual(events, [event])

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_watch_sandbox_claims_stops_watch_on_error(self, mock_watch_class):
        mock_watch = MagicMock()
        mock_watch.close = AsyncMock()

        async def _stream(*args, **kwargs):
            raise client.ApiException(status=410)
            yield  # unreachable; used to make this an async generator function

        mock_watch.stream = _stream
        mock_watch_class.return_value = mock_watch

        with self.assertRaises(client.ApiException):
            async for _ in self.helper.watch_sandbox_claims(
                "default", "agents.x-k8s.io/batch-id=b1", "5", 30
            ):
                pass

        mock_watch.close.assert_awaited_once()

    async def test_read_batch_lease_returns_lease_on_success(self):
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))
        self.helper.coordination_v1_api.read_namespaced_lease = AsyncMock(return_value=lease)

        result = await self.helper.read_batch_lease("batch-b1", "default")

        self.assertIs(result, lease)
        self.helper.coordination_v1_api.read_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", _request_timeout=None
        )

    async def test_read_batch_lease_forwards_request_timeout(self):
        self.helper.coordination_v1_api.read_namespaced_lease = AsyncMock(return_value=None)

        await self.helper.read_batch_lease("batch-b1", "default", _request_timeout=20)

        self.helper.coordination_v1_api.read_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", _request_timeout=20
        )

    async def test_read_batch_lease_returns_none_on_404(self):
        self.helper.coordination_v1_api.read_namespaced_lease = AsyncMock(
            side_effect=client.ApiException(status=404)
        )
        self.assertIsNone(await self.helper.read_batch_lease("batch-b1", "default"))

    async def test_read_batch_lease_reraises_non_404(self):
        self.helper.coordination_v1_api.read_namespaced_lease = AsyncMock(
            side_effect=client.ApiException(status=403)
        )
        with self.assertRaises(client.ApiException):
            await self.helper.read_batch_lease("batch-b1", "default")

    async def test_replace_batch_lease_forwards_body(self):
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))
        updated_lease = client.V1Lease(
            metadata=client.V1ObjectMeta(name="batch-b1", resource_version="7")
        )
        self.helper.coordination_v1_api.replace_namespaced_lease = AsyncMock(
            return_value=updated_lease
        )

        result = await self.helper.replace_batch_lease("batch-b1", "default", lease)

        self.helper.coordination_v1_api.replace_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", lease, _request_timeout=None
        )
        self.assertIs(result, updated_lease)

    async def test_replace_batch_lease_forwards_request_timeout(self):
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))
        self.helper.coordination_v1_api.replace_namespaced_lease = AsyncMock(return_value=lease)

        await self.helper.replace_batch_lease("batch-b1", "default", lease, _request_timeout=20)

        self.helper.coordination_v1_api.replace_namespaced_lease.assert_called_once_with(
            "batch-b1", "default", lease, _request_timeout=20
        )

    async def test_claim_batch_requests_forward_resource_selector_and_timeout(self):
        api = self.helper.custom_objects_api
        coord = self.helper.coordination_v1_api
        for method in (
            "create_namespaced_custom_object", "list_namespaced_custom_object",
            "delete_collection_namespaced_custom_object", "get_namespaced_custom_object",
            "delete_namespaced_custom_object",
        ):
            setattr(api, method, AsyncMock(return_value={"items": [], "metadata": {}}))
        api.delete_collection_namespaced_custom_object.return_value = MagicMock(status=200)
        coord.create_namespaced_lease = AsyncMock()
        coord.delete_namespaced_lease = AsyncMock()
        helper = self.helper
        selector = "agents.x-k8s.io/batch-id=b1"
        lease = client.V1Lease(metadata=client.V1ObjectMeta(name="batch-b1"))
        cases = [
            ("create_sandbox_claim", lambda: helper.create_sandbox_claim(
                "b1-0", "pool-a", "default", log_level=logging.DEBUG, _request_timeout=30),
             api.create_namespaced_custom_object, {"plural": "sandboxclaims"}),
            ("list_sandbox_claim_objects", lambda: helper.list_sandbox_claim_objects(
                "default", selector, _request_timeout=30),
             api.list_namespaced_custom_object,
             {"plural": "sandboxclaims", "label_selector": selector}),
            ("count_live_sandbox_claims", lambda: helper.count_live_sandbox_claims(
                "default", selector, _request_timeout=30),
             api.list_namespaced_custom_object,
             {"plural": "sandboxclaims", "label_selector": selector}),
            ("delete_sandbox_claims_by_label", lambda: helper.delete_sandbox_claims_by_label(
                "default", selector, _request_timeout=30),
             api.delete_collection_namespaced_custom_object,
             {"group": "extensions.agents.x-k8s.io", "plural": "sandboxclaims",
              "label_selector": selector}),
            ("get_sandbox_warmpool", lambda: helper.get_sandbox_warmpool(
                "pool-a", "default", _request_timeout=30),
             api.get_namespaced_custom_object,
             {"group": "extensions.agents.x-k8s.io", "plural": "sandboxwarmpools", "name": "pool-a"}),
            ("get_sandbox_template", lambda: helper.get_sandbox_template(
                "tmpl", "default", _request_timeout=30),
             api.get_namespaced_custom_object,
             {"group": "extensions.agents.x-k8s.io", "plural": "sandboxtemplates", "name": "tmpl"}),
            ("create_batch_lease", lambda: helper.create_batch_lease(
                "default", lease, _request_timeout=30),
             coord.create_namespaced_lease, {}),
            ("delete_batch_lease", lambda: helper.delete_batch_lease(
                "batch-b1", "default", _request_timeout=30),
             coord.delete_namespaced_lease, {}),
            ("delete_sandbox_claim", lambda: helper.delete_sandbox_claim(
                "b1-0", "default", _request_timeout=30),
             api.delete_namespaced_custom_object, {"plural": "sandboxclaims", "name": "b1-0"}),
        ]
        for name, call, api_method, expected in cases:
            with self.subTest(name):
                api_method.reset_mock()
                await call()
                kwargs = api_method.call_args.kwargs
                self.assertEqual(kwargs["_request_timeout"], 30)
                for key, value in expected.items():
                    self.assertEqual(kwargs[key], value)

    async def test_claim_create_and_delete_log_at_the_given_level(self):
        self.helper.custom_objects_api.create_namespaced_custom_object = AsyncMock()
        self.helper.custom_objects_api.delete_namespaced_custom_object = AsyncMock()
        calls = {
            "create": lambda: self.helper.create_sandbox_claim(
                "b1-0", "pool-a", "default", log_level=logging.DEBUG
            ),
            "delete": lambda: self.helper.delete_sandbox_claim("b1-0", "default", log_level=logging.DEBUG),
        }
        for name, call in calls.items():
            with self.subTest(name):
                with self.assertLogs(level="DEBUG") as logs:
                    await call()
                self.assertEqual([r.levelno for r in logs.records], [logging.DEBUG])

    async def test_warmpool_and_template_gets_return_none_and_lease_delete_ignores_404_other_errors_raise(self):
        api = self.helper.custom_objects_api
        coord = self.helper.coordination_v1_api
        api.get_namespaced_custom_object = AsyncMock(side_effect=client.ApiException(status=404))
        coord.delete_namespaced_lease = AsyncMock(side_effect=client.ApiException(status=404))
        self.assertIsNone(await self.helper.get_sandbox_warmpool("pool-a", "default"))
        self.assertIsNone(await self.helper.get_sandbox_template("tmpl", "default"))
        await self.helper.delete_batch_lease("batch-b1", "default")

        api.get_namespaced_custom_object.side_effect = client.ApiException(status=403)
        coord.delete_namespaced_lease.side_effect = client.ApiException(status=403)
        with self.assertRaises(client.ApiException):
            await self.helper.get_sandbox_warmpool("pool-a", "default")
        with self.assertRaises(client.ApiException):
            await self.helper.delete_batch_lease("batch-b1", "default")

    async def test_lease_delete_at_a_resource_version_sends_a_precondition_and_counts_a_conflict_as_deleted(self):
        coord = self.helper.coordination_v1_api
        coord.delete_namespaced_lease = AsyncMock()

        await self.helper.delete_batch_lease("batch-b1", "default", resource_version="11")
        body = coord.delete_namespaced_lease.await_args.kwargs["body"]
        self.assertEqual(body.preconditions.resource_version, "11")

        coord.delete_namespaced_lease.side_effect = client.ApiException(status=409)
        await self.helper.delete_batch_lease("batch-b1", "default", resource_version="11")
        with self.assertRaises(client.ApiException):
            await self.helper.delete_batch_lease("batch-b1", "default")


if __name__ == "__main__":
    unittest.main()
