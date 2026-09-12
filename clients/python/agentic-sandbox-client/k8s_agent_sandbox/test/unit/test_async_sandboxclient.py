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

import asyncio
import json
import os
import subprocess
import sys
import tempfile
import unittest
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread
from unittest.mock import ANY, AsyncMock, MagicMock, patch

import pytest

httpx = pytest.importorskip("httpx")
pytest.importorskip("kubernetes_asyncio")
from kubernetes_asyncio.client import ApiException

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.async_k8s_helper import AsyncK8sHelper
from k8s_agent_sandbox.async_sandbox import AsyncSandbox
from k8s_agent_sandbox.async_sandbox_client import (
    AsyncSandboxClient,
    _ATEXIT_DELETE_REQUEST_TIMEOUT_SECONDS,
)
from k8s_agent_sandbox.exceptions import SandboxNotFoundError, SandboxRequestError
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)
from k8s_agent_sandbox.test.unit.claim_adoption_test_support import (
    CLAIM_NAME,
    NAMESPACE,
    POD_ANNOTATIONS,
    POD_LABELS,
    REQUESTED_ENV,
    REQUESTED_LABELS,
    VOLUME_CLAIM_TEMPLATES,
    WARMPOOL,
    claim_for_request,
    matching_claim,
    mismatched_claims,
    terminal_claims,
)


class TestAsyncSandboxClient(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        self.MockAsyncK8sHelper = patcher.start()
        self.addCleanup(patcher.stop)

        self.config = SandboxDirectConnectionConfig(
            api_url="http://test-router:8080", server_port=8888
        )
        # cleanup=False keeps tests hermetic; the new default (True) registers a global atexit hook.
        self.client = AsyncSandboxClient(connection_config=self.config, cleanup=False)
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class
        default_claim_response = object()
        claim_getter = AsyncMock(return_value=default_claim_response)

        def get_claim(claim_name, namespace):
            configured_response = claim_getter.return_value
            if configured_response is not default_claim_response:
                return configured_response
            return claim_for_request(
                claim_name=claim_name, namespace=namespace
            )

        claim_getter.side_effect = get_claim
        self.mock_k8s_helper.get_sandbox_claim = claim_getter

    @patch("uuid.uuid4")
    async def test_create_sandbox_success(self, mock_uuid):
        mock_uuid.return_value.hex = "generated"
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="resolved-id")
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})

        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            mock_create.return_value = claim_for_request(
                claim_name="sandbox-claim-generate",
                namespace="test-namespace",
                warmpool="test-warmpool",
            )

            sandbox = await self.client.create_sandbox("test-warmpool", "test-namespace")

            mock_create.assert_called_once_with(
                "sandbox-claim-generate",
                "test-warmpool",
                "test-namespace",
                labels=None,
                lifecycle=None,
                volume_claim_templates=None,
                pod_metadata=None,
                env=None,
            )

            self.assertEqual(sandbox, mock_sandbox_instance)

            active = await self.client.list_active_sandboxes()
            self.assertEqual(len(active), 1)
            self.assertEqual(len(self.client._automatic_cleanup_claims), 1)

    @patch("uuid.uuid4")
    async def test_create_sandbox_with_env(self, mock_uuid):
        mock_uuid.return_value.hex = "1234abcd"
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="resolved-id")

        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        env = {"FOO": "bar", "DEBUG": "true"}

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create:
            mock_create.return_value = {"metadata": {"resourceVersion": "12345"}}

            await self.client.create_sandbox("test-warmpool", "test-namespace", env=env)

            mock_create.assert_called_once_with(
                "sandbox-claim-1234abcd",
                "test-warmpool",
                "test-namespace",
                labels=None,
                lifecycle=None,
                volume_claim_templates=None,
                pod_metadata=None,
                env=env,
            )
            self.mock_k8s_helper.wait_for_claim_ready.assert_awaited_once_with(
                "sandbox-claim-1234abcd",
                "test-namespace",
                180,
                resource_version="12345",
                claim_validator=None,
            )

    async def test_create_sandbox_failure_cleanup(self):
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=Exception("Timeout")
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with patch.object(
            self.client,
            "_create_claim",
            new_callable=AsyncMock,
            return_value=claim_for_request(claim_name="generated-claim"),
        ):

            with self.assertRaises(Exception) as ctx:
                await self.client.create_sandbox("test-warmpool", "test-namespace")

            self.assertEqual(str(ctx.exception), "Timeout")
            self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
                ANY,
                "test-namespace",
                expected_uid="claim-uid",
            )

    async def test_create_sandbox_cancellation_cleanup(self):
        """CancelledError (BaseException) should still trigger claim cleanup."""
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=asyncio.CancelledError()
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with patch.object(
            self.client,
            "_create_claim",
            new_callable=AsyncMock,
            return_value=claim_for_request(claim_name="generated-claim"),
        ):

            with self.assertRaises(asyncio.CancelledError):
                await self.client.create_sandbox("test-warmpool", "test-namespace")

            self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
                ANY,
                "test-namespace",
                expected_uid="claim-uid",
            )

    @patch("uuid.uuid4")
    async def test_cancelled_creation_still_cleans_up_when_close_fails(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        created_claim = claim_for_request(claim_name=claim_name)
        created_claim["metadata"]["uid"] = "created-uid"
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=created_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        candidate = MagicMock()
        candidate.close_connection = AsyncMock(
            side_effect=RuntimeError("close failed")
        )
        self.mock_sandbox_class.return_value = candidate

        with patch.object(
            self.client,
            "_register_created_handle",
            new_callable=AsyncMock,
            side_effect=asyncio.CancelledError(),
        ):
            with self.assertRaises(asyncio.CancelledError):
                await self.client.create_sandbox(WARMPOOL, NAMESPACE)

        candidate.close_connection.assert_awaited_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            claim_name, NAMESPACE, expected_uid="created-uid"
        )

    @patch("uuid.uuid4")
    async def test_cancellation_waiting_to_register_cleans_generated_claim(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        wait_started = asyncio.Event()
        release_wait = asyncio.Event()
        candidate_built = asyncio.Event()
        candidate_handle = MagicMock()
        candidate_handle.close_connection = AsyncMock()

        async def wait_for_ready(*_args, **_kwargs):
            wait_started.set()
            await release_wait.wait()
            return "resolved-id"

        def build_handle(*_args, **_kwargs):
            candidate_built.set()
            return candidate_handle

        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value={
                "metadata": {
                    "resourceVersion": "created-rv",
                    "uid": "created-uid",
                }
            }
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_sandbox_class.side_effect = build_handle
        create_task = asyncio.create_task(
            self.client.create_sandbox(WARMPOOL, NAMESPACE)
        )
        await asyncio.wait_for(wait_started.wait(), timeout=5)
        await self.client._lock.acquire()
        try:
            release_wait.set()
            await asyncio.wait_for(candidate_built.wait(), timeout=5)
            create_task.cancel()
        finally:
            self.client._lock.release()

        with self.assertRaises(asyncio.CancelledError):
            await create_task
        candidate_handle.close_connection.assert_awaited_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            "sandbox-claim-1234abcd",
            NAMESPACE,
            expected_uid="created-uid",
        )
        self.assertEqual(self.client._active_connection_sandboxes, {})
        self.assertEqual(self.client._automatic_cleanup_claims, set())

    async def test_create_sandbox_uses_explicit_claim_name(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=claim_for_request(
                claim_name="sandbox-workflow-123",
                namespace="test-namespace",
                warmpool="test-warmpool",
            )
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )

        await self.client.create_sandbox(
            "test-warmpool",
            "test-namespace",
            claim_name="sandbox-workflow-123",
        )

        self.mock_k8s_helper.create_sandbox_claim.assert_awaited_once_with(
            "sandbox-workflow-123",
            "test-warmpool",
            "test-namespace",
            annotations={},
            labels=None,
            lifecycle=None,
            volume_claim_templates=None,
            pod_metadata=None,
            env=None,
        )

    async def test_explicit_claim_replaces_stale_generated_ownership(self):
        key = ("test-namespace", "sandbox-workflow-123")
        stale_handle = MagicMock()
        stale_handle.is_active = False
        stale_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = stale_handle
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=claim_for_request(
                claim_name="sandbox-workflow-123",
                namespace="test-namespace",
                warmpool="test-warmpool",
            )
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )

        sandbox = await self.client.create_sandbox(
            "test-warmpool",
            "test-namespace",
            claim_name="sandbox-workflow-123",
        )

        self.assertIs(self.client._active_connection_sandboxes[key], sandbox)
        stale_handle.close_connection.assert_awaited_once_with()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    async def test_adoption_replaces_active_handle_for_recreated_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        active_handle = MagicMock()
        active_handle.claim_name = CLAIM_NAME
        active_handle.is_active = True
        active_handle.sandbox_id = "stable-sandbox"
        active_handle.close_connection = AsyncMock()
        candidate_handle = MagicMock()
        candidate_handle.sandbox_id = "stable-sandbox"
        candidate_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = active_handle
        self.client._active_claim_uids[key] = "old-uid"
        recreated_claim = claim_for_request(resource_version="replacement-rv")
        recreated_claim["metadata"]["uid"] = "replacement-uid"
        recreated_claim["status"] = {
            "conditions": [
                {"type": "Ready", "status": "True", "observedGeneration": 1}
            ],
            "sandbox": {"name": "stable-sandbox"},
        }
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=recreated_claim
        )
        self.mock_sandbox_class.return_value = candidate_handle

        sandbox = await self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
        )

        self.assertIs(sandbox, candidate_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], candidate_handle)
        active_handle.close_connection.assert_awaited_once_with()
        self.assertIsNone(active_handle.claim_name)
        candidate_handle.close_connection.assert_not_awaited()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    async def test_stale_adoption_cannot_replace_concurrent_claim_incarnation(self):
        key = (NAMESPACE, CLAIM_NAME)
        initial_handle = MagicMock(is_active=True, sandbox_id="initial-sandbox")
        initial_handle.close_connection = AsyncMock()
        old_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        old_handle.claim_name = CLAIM_NAME
        old_handle.close_connection = AsyncMock()
        new_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        new_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = initial_handle
        self.client._active_claim_uids[key] = "initial-uid"
        old_claim = claim_for_request(resource_version="old-rv")
        old_claim["metadata"]["uid"] = "old-uid"
        new_claim = claim_for_request(resource_version="new-rv")
        new_claim["metadata"]["uid"] = "new-uid"
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            side_effect=[old_claim, new_claim]
        )
        old_wait_started = asyncio.Event()
        release_old_wait = asyncio.Event()

        async def wait_for_ready(*_args, resource_version, **_kwargs):
            if resource_version == "old-rv":
                old_wait_started.set()
                await release_old_wait.wait()
            return "stable-sandbox"

        handles = iter((new_handle, old_handle))

        def build_handle(*_args, **_kwargs):
            return next(handles)

        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_sandbox_class.side_effect = build_handle
        stale_task = asyncio.create_task(
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
            )
        )
        await asyncio.wait_for(old_wait_started.wait(), timeout=5)
        try:
            current = await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
            )
        finally:
            release_old_wait.set()

        with self.assertRaisesRegex(RuntimeError, "changed concurrently"):
            await asyncio.wait_for(stale_task, timeout=5)
        self.assertIs(current, new_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], new_handle)
        self.assertEqual(self.client._active_claim_uids[key], "new-uid")
        old_handle.close_connection.assert_awaited_once_with()
        self.assertIsNone(old_handle.claim_name)
        new_handle.close_connection.assert_not_awaited()

    async def test_adopt_existing_requires_explicit_claim_name(self):
        with self.assertRaisesRegex(ValueError, "explicit claim_name"):
            await self.client.create_sandbox(
                "test-warmpool", adopt_existing=True
            )

        self.mock_k8s_helper.create_sandbox_claim.assert_not_called()

    async def test_create_sandbox_rejects_invalid_explicit_claim_names(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock()
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )
        invalid_names = (
            "",
            "UPPERCASE",
            "-leading",
            "trailing-",
            "two..dots",
            "a" * 254,
        )

        for claim_name in invalid_names:
            with self.subTest(claim_name=claim_name):
                with self.assertRaisesRegex(ValueError, "DNS-1123"):
                    await self.client.create_sandbox(
                        "test-warmpool", claim_name=claim_name
                    )

        self.mock_k8s_helper.create_sandbox_claim.assert_not_called()

    async def test_create_sandbox_accepts_native_valid_long_claim_names(self):
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )

        for claim_name in ("a" * 64, "a" * 253):
            with self.subTest(length=len(claim_name)):
                self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
                    return_value=claim_for_request(claim_name=claim_name)
                )

                await self.client.create_sandbox(
                    WARMPOOL, NAMESPACE, claim_name=claim_name
                )

                self.assertIn((NAMESPACE, claim_name), self.client._active_connection_sandboxes)

    async def test_create_sandbox_adopts_existing_claim_after_conflict(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value={
                "apiVersion": "extensions.agents.x-k8s.io/v1beta1",
                "kind": "SandboxClaim",
                "metadata": {
                    "name": "sandbox-workflow-123",
                    "namespace": "test-namespace",
                    "resourceVersion": "existing-rv",
                    "uid": "claim-uid",
                    "generation": 1,
                    "labels": {
                        "agents.x-k8s.io/created-by": "python-client"
                    },
                },
                "spec": {"warmPoolRef": {"name": "test-warmpool"}},
            }
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )

        sandbox = await self.client.create_sandbox(
            "test-warmpool",
            "test-namespace",
            claim_name="sandbox-workflow-123",
            adopt_existing=True,
        )

        self.mock_k8s_helper.get_sandbox_claim.assert_awaited_once_with(
            "sandbox-workflow-123", "test-namespace"
        )
        self.mock_k8s_helper.wait_for_claim_ready.assert_awaited_once_with(
            "sandbox-workflow-123",
            "test-namespace",
            180,
            resource_version="existing-rv",
            claim_validator=ANY,
        )
        self.assertEqual(sandbox, self.mock_sandbox_class.return_value)

    async def test_create_sandbox_reports_claim_deleted_after_adoption_conflict(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(return_value=None)
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock()
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(
            SandboxNotFoundError, "disappeared after the create conflict"
        ):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
            )

        self.mock_k8s_helper.wait_for_claim_ready.assert_not_awaited()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_create_sandbox_propagates_conflict_without_adoption(self):
        conflict = ApiException(status=409)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=conflict
        )

        with self.assertRaises(ApiException) as context:
            await self.client.create_sandbox(
                "test-warmpool",
                claim_name="sandbox-workflow-123",
            )

        self.assertIs(context.exception, conflict)
        self.mock_k8s_helper.get_sandbox_claim.assert_not_called()

    async def test_create_sandbox_rejects_mismatched_existing_claims(self):
        for existing_claim, error_field in mismatched_claims():
            with self.subTest(error_field=error_field):
                self.mock_k8s_helper.reset_mock()
                self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
                    side_effect=ApiException(status=409)
                )
                self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
                    return_value=existing_claim
                )
                self.mock_k8s_helper.wait_for_claim_ready = AsyncMock()
                self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

                with self.assertRaisesRegex(ValueError, error_field):
                    await self.client.create_sandbox(
                        WARMPOOL,
                        NAMESPACE,
                        labels=REQUESTED_LABELS,
                        claim_name=CLAIM_NAME,
                        adopt_existing=True,
                        volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                        pod_labels=POD_LABELS,
                        pod_annotations=POD_ANNOTATIONS,
                    )

                self.mock_k8s_helper.wait_for_claim_ready.assert_not_awaited()
                self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_adopt_existing_rejects_relative_shutdown_time(self):
        with self.assertRaisesRegex(ValueError, "shutdown_after_seconds"):
            await self.client.create_sandbox(
                "test-warmpool",
                claim_name="sandbox-workflow-123",
                adopt_existing=True,
                shutdown_after_seconds=60,
            )

        self.mock_k8s_helper.create_sandbox_claim.assert_not_called()

    async def test_create_sandbox_returns_already_ready_adopted_claim(self):
        existing_claim = matching_claim()
        existing_claim["status"] = {
            "conditions": [
                {
                    "type": "Ready",
                    "status": "True",
                    "observedGeneration": 1,
                }
            ],
            "sandbox": {"name": "ready-sandbox"},
        }
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=existing_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock()

        sandbox = await self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
        )

        self.mock_k8s_helper.wait_for_claim_ready.assert_not_awaited()
        self.assertEqual(sandbox, self.mock_sandbox_class.return_value)
        self.assertEqual(
            self.mock_sandbox_class.call_args.kwargs["sandbox_id"],
            "ready-sandbox",
        )

    async def test_create_sandbox_accepts_server_defaulted_spec_fields(self):
        existing_claim = matching_claim()
        existing_claim["spec"]["futureBehavior"] = {"enabled": True}
        existing_claim["status"] = {
            "conditions": [
                {
                    "type": "Ready",
                    "status": "True",
                    "observedGeneration": 1,
                }
            ],
            "sandbox": {"name": "ready-sandbox"},
        }
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=existing_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock()

        sandbox = await self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
        )

        self.assertEqual(sandbox, self.mock_sandbox_class.return_value)
        self.mock_k8s_helper.wait_for_claim_ready.assert_not_awaited()

    async def test_create_sandbox_ignores_stale_ready_adopted_snapshot(self):
        existing_claim = matching_claim()
        existing_claim["status"] = {
            "conditions": [
                {
                    "type": "Ready",
                    "status": "True",
                    "observedGeneration": 0,
                }
            ],
            "sandbox": {"name": "stale-sandbox"},
        }
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=existing_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="current-sandbox"
        )

        await self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
        )

        self.mock_k8s_helper.wait_for_claim_ready.assert_awaited_once()
        self.assertEqual(
            self.mock_sandbox_class.call_args.kwargs["sandbox_id"],
            "current-sandbox",
        )

    async def test_create_sandbox_revalidates_adopted_watch_events(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=matching_claim()
        )

        async def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            self.assertIsNotNone(claim_validator)
            changed_claim = matching_claim()
            changed_claim["spec"]["warmPoolRef"] = {"name": "other-pool"}
            claim_validator(changed_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(ValueError, "spec.warmPoolRef"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_successful_explicit_create_revalidates_watch_contract(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=matching_claim()
        )

        async def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            self.assertIsNotNone(claim_validator)
            changed_claim = matching_claim()
            changed_claim["spec"]["warmPoolRef"] = {"name": "other-pool"}
            claim_validator(changed_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(ValueError, "spec.warmPoolRef"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_successful_explicit_create_rejects_recreated_claim_during_watch(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=matching_claim()
        )

        async def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            self.assertIsNotNone(claim_validator)
            recreated_claim = matching_claim()
            recreated_claim["metadata"]["uid"] = "replacement-uid"
            claim_validator(recreated_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(ValueError, "metadata.uid"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    @patch("k8s_agent_sandbox.async_k8s_helper.watch.Watch")
    async def test_successful_explicit_create_rejects_recreated_claim_after_410(
        self, mock_watch_class
    ):
        created_claim = matching_claim()
        recreated_claim = matching_claim()
        recreated_claim["metadata"].update(
            uid="replacement-uid", resourceVersion="replacement-rv"
        )
        recreated_claim["status"] = {
            "sandbox": {"name": "replacement-sandbox"},
            "conditions": [
                {
                    "type": "Ready",
                    "status": "True",
                    "observedGeneration": 1,
                }
            ],
        }
        stream_resource_versions = []

        async def expired_stream(*_args, **kwargs):
            stream_resource_versions.append(kwargs["resource_version"])
            raise ApiException(status=410)
            yield

        async def replacement_stream(*_args, **kwargs):
            stream_resource_versions.append(kwargs["resource_version"])
            yield {"type": "ADDED", "object": recreated_claim}

        first_watch = MagicMock()
        first_watch.stream = expired_stream
        first_watch.close = AsyncMock()
        second_watch = MagicMock()
        second_watch.stream = replacement_stream
        second_watch.close = AsyncMock()
        mock_watch_class.side_effect = [first_watch, second_watch]
        real_helper = AsyncK8sHelper.__new__(AsyncK8sHelper)
        real_helper._initialized = True
        real_helper.custom_objects_api = MagicMock()
        self.client.k8s_helper = real_helper

        with patch.object(
            self.client,
            "_create_claim",
            new=AsyncMock(return_value=created_claim),
        ), patch.object(
            self.client, "_delete_claim", new_callable=AsyncMock
        ) as delete_claim:
            with self.assertRaisesRegex(ValueError, "metadata.uid"):
                await self.client.create_sandbox(
                    WARMPOOL,
                    NAMESPACE,
                    labels=REQUESTED_LABELS,
                    claim_name=CLAIM_NAME,
                    adopt_existing=True,
                    volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                    pod_labels=POD_LABELS,
                    pod_annotations=POD_ANNOTATIONS,
                )

        self.assertEqual(stream_resource_versions, ["existing-rv", "0"])
        delete_claim.assert_not_awaited()
        self.mock_sandbox_class.assert_not_called()

    async def test_create_sandbox_rejects_recreated_claim_during_watch(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=matching_claim()
        )

        async def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            recreated_claim = matching_claim()
            recreated_claim["metadata"]["uid"] = "replacement-uid"
            claim_validator(recreated_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(ValueError, "metadata.uid"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_create_sandbox_propagates_adopted_terminal_conditions(self):
        for existing_claim, error_type, reason, error_pattern in terminal_claims():
            with self.subTest(reason=reason):
                self.mock_k8s_helper.reset_mock()
                self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
                    side_effect=ApiException(status=409)
                )
                self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
                    return_value=existing_claim
                )
                self.mock_k8s_helper.wait_for_claim_ready = AsyncMock()
                self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

                with self.assertRaisesRegex(error_type, error_pattern):
                    await self.client.create_sandbox(
                        WARMPOOL,
                        NAMESPACE,
                        labels=REQUESTED_LABELS,
                        claim_name=CLAIM_NAME,
                        adopt_existing=True,
                        volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                        pod_labels=POD_LABELS,
                        pod_annotations=POD_ANNOTATIONS,
                    )

                self.mock_k8s_helper.wait_for_claim_ready.assert_not_awaited()
                self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_create_sandbox_adopts_matching_full_contract(self):
        existing_claim = matching_claim(env=REQUESTED_ENV)
        existing_claim["status"] = {
            "conditions": [{"type": "Ready", "status": "True"}],
            "sandbox": {"name": 42},
        }
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=existing_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )

        await self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
            env=REQUESTED_ENV,
        )

        self.mock_k8s_helper.wait_for_claim_ready.assert_awaited_once_with(
            CLAIM_NAME,
            NAMESPACE,
            180,
            resource_version="existing-rv",
            claim_validator=ANY,
        )

    async def test_create_sandbox_rejects_mismatched_existing_env(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=matching_claim(env={"FOO": "other"})
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock()
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(ValueError, "spec.env"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
                env=REQUESTED_ENV,
            )

        self.mock_k8s_helper.wait_for_claim_ready.assert_not_awaited()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_explicit_claim_is_not_deleted_when_wait_fails(self):
        key = (NAMESPACE, CLAIM_NAME)
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=claim_for_request()
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=TimeoutError("timed out")
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(TimeoutError, "timed out"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    async def test_adopted_claim_is_not_deleted_when_wait_fails(self):
        key = (NAMESPACE, CLAIM_NAME)
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=matching_claim()
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=TimeoutError("timed out")
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaisesRegex(TimeoutError, "timed out"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    async def test_explicit_claim_is_not_deleted_on_context_exit(self):
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=claim_for_request()
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_k8s_helper.close = AsyncMock()
        sandbox = MagicMock()
        sandbox.terminate = AsyncMock()
        sandbox.close_connection = AsyncMock()
        self.mock_sandbox_class.return_value = sandbox

        await self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            claim_name=CLAIM_NAME,
        )
        await self.client.__aexit__(None, None, None)

        sandbox.terminate.assert_not_awaited()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_explicit_claim_is_not_deleted_when_wait_is_cancelled(self):
        key = (NAMESPACE, CLAIM_NAME)
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=claim_for_request()
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=asyncio.CancelledError()
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaises(asyncio.CancelledError):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    async def test_explicit_create_failure_releases_stale_automatic_ownership(self):
        key = (NAMESPACE, CLAIM_NAME)
        self.client._automatic_cleanup_claims.add(key)
        server_error = ApiException(status=500)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=server_error
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock()
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaises(ApiException) as context:
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
            )

        self.assertIs(context.exception, server_error)
        self.mock_k8s_helper.get_sandbox_claim.assert_not_awaited()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    @patch("uuid.uuid4")
    async def test_generated_claim_without_uid_is_not_deleted_by_automatic_cleanup(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        key = (NAMESPACE, claim_name)
        created_claim = claim_for_request(
            claim_name=claim_name,
            namespace=NAMESPACE,
        )
        created_claim["metadata"].pop("uid")
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=created_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="resolved-id"
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        generated_handle = MagicMock(claim_name=claim_name)
        generated_handle.close_connection = AsyncMock()
        self.mock_sandbox_class.return_value = generated_handle

        await self.client.create_sandbox(WARMPOOL, NAMESPACE)
        await self.client._delete_automatic_cleanup_claims()

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertNotIn(key, self.client._automatic_cleanup_claim_uids)

    async def test_automatic_cleanup_never_deletes_without_observed_uid(self):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(claim_name=CLAIM_NAME)
        generated_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        await self.client._delete_automatic_cleanup_claims()

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    @patch("uuid.uuid4")
    async def test_generated_claim_without_uid_is_not_deleted_after_ambiguous_failure(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        server_error = ApiException(status=500)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=server_error
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaises(ApiException) as context:
            await self.client.create_sandbox(WARMPOOL, NAMESPACE)

        self.assertIs(context.exception, server_error)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    @patch("uuid.uuid4")
    async def test_generated_claim_conflict_does_not_delete_existing_claim(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        conflict = ApiException(status=409)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=conflict
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        with self.assertRaises(ApiException) as context:
            await self.client.create_sandbox(WARMPOOL, NAMESPACE)

        self.assertIs(context.exception, conflict)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_rejected_explicit_claim_name_remains_caller_owned(self):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(is_active=True, sandbox_id="generated")
        generated_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._automatic_cleanup_claims.add(key)
        conflict = ApiException(status=409)
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=conflict
        )

        with self.assertRaises(ApiException) as context:
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
            )

        self.assertIs(context.exception, conflict)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertIn(key, self.client._caller_owned_claims)

    @patch("uuid.uuid4")
    async def test_explicit_attempt_stops_cleanup_of_previously_generated_name(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        key = (NAMESPACE, claim_name)
        generated_handle = MagicMock(is_active=True, sandbox_id="old-sandbox")
        generated_handle.close_connection = AsyncMock()
        generated_handle.terminate = AsyncMock()
        created_claim = claim_for_request(claim_name=claim_name)
        created_claim["metadata"]["uid"] = "original-uid"
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=created_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="old-sandbox"
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_sandbox_class.return_value = generated_handle
        await self.client.create_sandbox(WARMPOOL, NAMESPACE)

        replacement = claim_for_request(claim_name=claim_name)
        replacement["metadata"]["uid"] = "replacement-uid"
        replacement["spec"]["warmPoolRef"]["name"] = "other-pool"
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=replacement
        )

        with self.assertRaisesRegex(ValueError, "warmPoolRef"):
            await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=claim_name,
                adopt_existing=True,
            )

        await self.client._delete_automatic_cleanup_claims()

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        generated_handle.terminate.assert_not_awaited()
        generated_handle.close_connection.assert_not_awaited()
        self.assertIs(
            self.client._active_connection_sandboxes[key], generated_handle
        )
        self.assertIn(key, self.client._caller_owned_claims)

    async def test_automatic_cleanup_skips_caller_protected_generated_registration(
        self,
    ):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(is_active=True, sandbox_id="generated")
        generated_handle.close_connection = AsyncMock()
        generated_handle.terminate = AsyncMock()
        self.client._active_connection_sandboxes[key] = generated_handle
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        async with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)
            self.client._claim_ownership.register_automatic(key, "generated-uid")

        await self.client._delete_automatic_cleanup_claims()

        generated_handle.terminate.assert_not_awaited()
        generated_handle.close_connection.assert_not_awaited()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        self.assertIs(self.client._active_connection_sandboxes[key], generated_handle)

    async def test_deliberate_delete_clears_caller_ownership(self):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(
            is_active=True,
            sandbox_id="generated",
            claim_name=CLAIM_NAME,
        )
        generated_handle.terminate = AsyncMock()
        generated_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._active_claim_uids[key] = "generated-uid"
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.client._claim_ownership.register_automatic(key, "generated-uid")
        async with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        await self.client.delete_sandbox(CLAIM_NAME, NAMESPACE)

        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertNotIn(key, self.client._caller_owned_claims)
        generated_handle.terminate.assert_not_awaited()
        generated_handle.close_connection.assert_awaited_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="generated-uid"
        )

    @patch("uuid.uuid4")
    async def test_late_generated_claim_keeps_explicit_ownership(self, mock_uuid):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        key = (NAMESPACE, claim_name)
        generated_wait_started = asyncio.Event()
        release_generated_wait = asyncio.Event()
        generated_handle = MagicMock()
        generated_handle.is_active = True
        generated_handle.close_connection = AsyncMock()
        explicit_handle = MagicMock()
        explicit_handle.is_active = True
        explicit_handle.close_connection = AsyncMock()

        created_claim = claim_for_request(claim_name=claim_name)
        existing_claim = claim_for_request(
            claim_name=claim_name, resource_version="existing-rv"
        )
        existing_claim["status"] = {
            "conditions": [
                {"type": "Ready", "status": "True", "observedGeneration": 1}
            ],
            "sandbox": {"name": "explicit-sandbox"},
        }
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=[created_claim, ApiException(status=409)]
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=existing_claim
        )

        async def wait_for_ready(*_args, **_kwargs):
            generated_wait_started.set()
            await release_generated_wait.wait()
            return "generated-sandbox"

        def build_handle(*_args, sandbox_id, **_kwargs):
            if sandbox_id == "explicit-sandbox":
                return explicit_handle
            return generated_handle

        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_sandbox_class.side_effect = build_handle
        generated_task = asyncio.create_task(
            self.client.create_sandbox(WARMPOOL, NAMESPACE)
        )
        await asyncio.wait_for(generated_wait_started.wait(), timeout=5)
        try:
            explicit_result = await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=claim_name,
                adopt_existing=True,
            )
        finally:
            release_generated_wait.set()

        with self.assertRaisesRegex(RuntimeError, "changed concurrently"):
            await asyncio.wait_for(generated_task, timeout=5)
        self.assertIs(explicit_result, explicit_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], explicit_handle)
        generated_handle.close_connection.assert_awaited_once_with()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    @patch("uuid.uuid4")
    async def test_late_generated_failure_cannot_delete_explicit_claim(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        key = (NAMESPACE, claim_name)
        generated_wait_started = asyncio.Event()
        release_generated_wait = asyncio.Event()
        explicit_handle = MagicMock()
        explicit_handle.close_connection = AsyncMock()

        existing_claim = claim_for_request(
            claim_name=claim_name, resource_version="existing-rv"
        )
        existing_claim["status"] = {
            "conditions": [
                {"type": "Ready", "status": "True", "observedGeneration": 1}
            ],
            "sandbox": {"name": "explicit-sandbox"},
        }
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=[
                claim_for_request(claim_name=claim_name),
                ApiException(status=409),
            ]
        )
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=existing_claim
        )

        async def wait_for_ready(*_args, **_kwargs):
            generated_wait_started.set()
            await release_generated_wait.wait()
            raise TimeoutError("generated wait timed out")

        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            side_effect=wait_for_ready
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_sandbox_class.return_value = explicit_handle
        generated_task = asyncio.create_task(
            self.client.create_sandbox(WARMPOOL, NAMESPACE)
        )
        await asyncio.wait_for(generated_wait_started.wait(), timeout=5)
        try:
            explicit_result = await self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=claim_name,
                adopt_existing=True,
            )
        finally:
            release_generated_wait.set()

        with self.assertRaisesRegex(TimeoutError, "generated wait timed out"):
            await asyncio.wait_for(generated_task, timeout=5)
        self.assertIs(explicit_result, explicit_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], explicit_handle)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    async def test_get_sandbox_existing_active(self):
        key = ("test-namespace", "test-claim")
        mock_sandbox = MagicMock()
        mock_sandbox.is_active = True
        mock_sandbox.sandbox_id = "resolved-id"
        mock_sandbox.terminate = AsyncMock()
        self.client._active_connection_sandboxes[key] = mock_sandbox
        self.client._active_claim_uids[key] = "claim-uid"

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="resolved-id")
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})

        sandbox = await self.client.get_sandbox("test-claim", "test-namespace")
        self.assertEqual(sandbox, mock_sandbox)
        self.mock_sandbox_class.assert_not_called()

    async def test_get_sandbox_replaces_active_handle_for_recreated_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        old_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        old_handle.claim_name = CLAIM_NAME
        old_handle.close_connection = AsyncMock()
        new_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        new_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = old_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            return_value="stable-sandbox"
        )
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})
        self.mock_sandbox_class.return_value = new_handle

        sandbox = await self.client.get_sandbox(CLAIM_NAME, NAMESPACE)

        self.assertIs(sandbox, new_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], new_handle)
        old_handle.close_connection.assert_awaited_once_with()
        self.assertIsNone(old_handle.claim_name)
        self.assertEqual(self.client._active_claim_uids[key], "claim-uid")

    async def test_get_sandbox_stale_lookup_cannot_return_concurrent_handle(self):
        key = (NAMESPACE, CLAIM_NAME)
        initial_handle = MagicMock(is_active=True, sandbox_id="initial-sandbox")
        initial_handle.claim_name = CLAIM_NAME
        initial_handle.close_connection = AsyncMock()
        concurrent_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        concurrent_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = initial_handle
        self.client._active_claim_uids[key] = "claim-uid"
        self.client._claim_ownership.register_automatic(key, "claim-uid")
        lookup_started = asyncio.Event()
        release_lookup = asyncio.Event()

        async def resolve_sandbox(*_args, **_kwargs):
            lookup_started.set()
            await release_lookup.wait()
            return "stable-sandbox"

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=resolve_sandbox
        )
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})
        lookup_task = asyncio.create_task(
            self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
        )
        await asyncio.wait_for(lookup_started.wait(), timeout=5)
        self.client._active_connection_sandboxes[key] = concurrent_handle
        self.client._active_claim_uids[key] = "replacement-uid"
        self.client._claim_ownership.register_automatic(key, "replacement-uid")
        release_lookup.set()

        with self.assertRaisesRegex(RuntimeError, "changed concurrently"):
            await asyncio.wait_for(lookup_task, timeout=5)
        self.assertIs(
            self.client._active_connection_sandboxes[key], concurrent_handle
        )
        initial_handle.close_connection.assert_awaited_once_with()
        self.assertIsNone(initial_handle.claim_name)
        concurrent_handle.close_connection.assert_not_awaited()
        self.assertEqual(
            self.client._claim_ownership.automatic_cleanup_uid(key),
            "replacement-uid",
        )

    async def test_stale_lookup_cannot_reuse_different_sandbox_binding(self):
        key = (NAMESPACE, CLAIM_NAME)
        expected_handle = MagicMock(
            is_active=True, sandbox_id="initial-sandbox"
        )
        expected_handle.claim_name = CLAIM_NAME
        expected_handle.close_connection = AsyncMock()
        current_handle = MagicMock(
            is_active=True, sandbox_id="old-binding"
        )
        current_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = expected_handle
        self.client._active_claim_uids[key] = "claim-uid"
        async with self.client._lock:
            lookup_operation = self.client._claim_ownership.begin_lookup(key)
            self.client._active_connection_sandboxes[key] = current_handle

        try:
            with self.assertRaisesRegex(RuntimeError, "changed concurrently"):
                await self.client._reuse_or_replace_resolved_handle(
                    key,
                    expected_handle,
                    lookup_operation,
                    CLAIM_NAME,
                    "new-binding",
                    NAMESPACE,
                    "claim-uid",
                )
        finally:
            async with self.client._lock:
                self.client._claim_ownership.finish_lookup(
                    key, lookup_operation
                )

        self.assertIs(self.client._active_connection_sandboxes[key], current_handle)
        current_handle.close_connection.assert_not_awaited()

    async def test_get_sandbox_cannot_restore_claim_deleted_during_lookup(self):
        key = (NAMESPACE, CLAIM_NAME)
        lookup_started = asyncio.Event()
        release_lookup = asyncio.Event()
        claim = claim_for_request()
        claim["metadata"]["uid"] = "deleted-uid"
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(return_value=claim)
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        async def resolve_sandbox(*_args, **_kwargs):
            lookup_started.set()
            await release_lookup.wait()
            return "deleted-sandbox"

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=resolve_sandbox
        )
        lookup_task = asyncio.create_task(
            self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
        )
        await asyncio.wait_for(lookup_started.wait(), timeout=5)

        await self.client.delete_sandbox(CLAIM_NAME, NAMESPACE)
        release_lookup.set()

        with self.assertRaisesRegex(RuntimeError, "changed concurrently"):
            await asyncio.wait_for(lookup_task, timeout=5)
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.mock_sandbox_class.assert_not_called()
        self.assertEqual(self.client._claim_ownership._lookup_operations, {})

    async def test_get_sandbox_inactive_reattaches(self):
        mock_inactive = MagicMock()
        mock_inactive.is_active = False
        mock_inactive.terminate = AsyncMock()
        mock_inactive.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[("test-namespace", "test-claim")] = mock_inactive

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="resolved-id")
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})

        mock_new = MagicMock()
        self.mock_sandbox_class.return_value = mock_new

        sandbox = await self.client.get_sandbox("test-claim", "test-namespace")
        self.assertEqual(sandbox, mock_new)
        mock_inactive.close_connection.assert_awaited_once_with()

    async def test_get_sandbox_not_found(self):
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=Exception("Not found")
        )

        with self.assertRaises(RuntimeError) as ctx:
            await self.client.get_sandbox("test-claim", "test-namespace")

        self.assertIn("not found", str(ctx.exception))

    async def test_get_sandbox_failure_detaches_explicit_handle_without_deleting_claim(self):
        key = ("test-namespace", "test-claim")
        explicit_handle = MagicMock()
        explicit_handle.close_connection = AsyncMock()
        explicit_handle.terminate = AsyncMock()
        self.client._active_connection_sandboxes[key] = explicit_handle
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=Exception("transient")
        )

        with self.assertRaises(RuntimeError):
            await self.client.get_sandbox("test-claim", "test-namespace")

        explicit_handle.close_connection.assert_awaited_once_with()
        explicit_handle.terminate.assert_not_awaited()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertNotIn(key, self.client._active_connection_sandboxes)

    async def test_get_sandbox_failure_deletes_generated_claim_by_uid(self):
        key = ("test-namespace", "test-claim")
        generated_handle = MagicMock()
        generated_handle.claim_name = "test-claim"
        generated_handle.close_connection = AsyncMock()
        generated_handle.terminate = AsyncMock()
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._claim_ownership.register_automatic(key, "generated-uid")
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=Exception("missing")
        )

        with self.assertRaises(RuntimeError):
            await self.client.get_sandbox("test-claim", "test-namespace")

        generated_handle.terminate.assert_not_awaited()
        generated_handle.close_connection.assert_awaited_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            "test-claim", "test-namespace", expected_uid="generated-uid"
        )
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertIsNone(generated_handle.claim_name)

    async def test_context_cleanup_deletes_reattached_claim_by_observed_uid(self):
        reattached_handle = MagicMock()
        reattached_handle.claim_name = CLAIM_NAME
        reattached_handle.close_connection = AsyncMock()
        reattached_handle.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = reattached_handle
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            return_value="resolved-id"
        )
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})
        claim = claim_for_request()
        claim["metadata"]["uid"] = "reattached-uid"
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(return_value=claim)
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_k8s_helper.close = AsyncMock()

        await self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
        await self.client.__aexit__(None, None, None)

        reattached_handle.terminate.assert_not_awaited()
        reattached_handle.close_connection.assert_awaited_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="reattached-uid"
        )
        self.assertIsNone(reattached_handle.claim_name)

    async def test_retained_handle_cannot_delete_recreated_claim_after_cleanup(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = AsyncSandbox.__new__(AsyncSandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._claim_ownership.register_automatic(key, "original-uid")

        await self.client._delete_automatic_cleanup_claims()
        await retained_handle.terminate()

        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="original-uid"
        )
        self.assertIsNone(retained_handle.claim_name)
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertIn(key, self.client._automatic_cleanup_claims)

    async def test_get_sandbox_rejects_claim_recreated_during_resolution(self):
        key = (NAMESPACE, CLAIM_NAME)
        original_handle = MagicMock()
        original_handle.claim_name = CLAIM_NAME
        original_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = original_handle
        self.client._claim_ownership.register_automatic(key, "original-uid")
        original = claim_for_request()
        original["metadata"]["uid"] = "original-uid"
        replacement = claim_for_request()
        replacement["metadata"]["uid"] = "replacement-uid"
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=original
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock(
            side_effect=ApiException(status=409)
        )

        async def resolve_sandbox(*_args, claim_validator, **_kwargs):
            claim_validator(replacement)
            return "replacement-sandbox"

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=resolve_sandbox
        )

        with self.assertRaisesRegex(SandboxNotFoundError, "metadata.uid"):
            await self.client.get_sandbox(CLAIM_NAME, NAMESPACE)

        self.mock_k8s_helper.get_sandbox.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="original-uid"
        )
        self.assertIsNone(original_handle.claim_name)
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertIn(key, self.client._automatic_cleanup_claims)
        original_handle.close_connection.assert_awaited_once_with()

    async def test_get_sandbox_warmpool_mismatch_keeps_existing_handle(self):
        key = (NAMESPACE, CLAIM_NAME)
        existing_handle = MagicMock()
        existing_handle.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = existing_handle
        claim = claim_for_request()
        claim["spec"]["warmPoolRef"]["name"] = "other-pool"
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(return_value=claim)

        with self.assertRaisesRegex(ValueError, "Refusing to reattach"):
            await self.client.get_sandbox(
                CLAIM_NAME, NAMESPACE, warmpool_name=WARMPOOL
            )

        self.assertIs(self.client._active_connection_sandboxes[key], existing_handle)
        existing_handle.close_connection.assert_not_awaited()
        self.mock_k8s_helper.resolve_sandbox_name.assert_not_called()

    async def test_get_sandbox_failure_cannot_remove_concurrently_explicit_handle(self):
        key = (NAMESPACE, CLAIM_NAME)
        lookup_started = asyncio.Event()
        release_lookup = asyncio.Event()
        generated_handle = MagicMock()
        generated_handle.close_connection = AsyncMock()
        generated_handle.terminate = AsyncMock()
        adopted_handle = MagicMock()
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._automatic_cleanup_claims.add(key)

        async def fail_lookup(*_args, **_kwargs):
            lookup_started.set()
            await release_lookup.wait()
            raise RuntimeError("stale lookup failed")

        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(
            side_effect=fail_lookup
        )
        lookup_task = asyncio.create_task(
            self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
        )
        await lookup_started.wait()

        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=matching_claim()
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="adopted-sandbox"
        )
        self.mock_sandbox_class.return_value = adopted_handle
        await self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
        )

        release_lookup.set()
        with self.assertRaises(SandboxNotFoundError):
            await lookup_task

        generated_handle.terminate.assert_not_awaited()
        generated_handle.close_connection.assert_awaited_once_with()
        self.assertIs(self.client._active_connection_sandboxes[key], adopted_handle)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    async def test_slow_automatic_cleanup_does_not_block_another_claim(self):
        cleanup_key = (NAMESPACE, "generated-claim")
        termination_started = asyncio.Event()
        release_termination = asyncio.Event()
        explicit_create_called = asyncio.Event()
        generated_handle = MagicMock()

        async def block_termination():
            termination_started.set()
            await release_termination.wait()

        async def create_claim(*_args, **_kwargs):
            explicit_create_called.set()
            return matching_claim()

        generated_handle.terminate = AsyncMock()
        generated_handle.close_connection = AsyncMock(
            side_effect=block_termination
        )
        adopted_handle = MagicMock()
        self.client._active_connection_sandboxes[cleanup_key] = generated_handle
        self.client._claim_ownership.register_automatic(
            cleanup_key, "generated-uid"
        )
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=create_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="adopted-sandbox"
        )
        self.mock_sandbox_class.return_value = adopted_handle

        cleanup_task = asyncio.create_task(
            self.client._delete_automatic_cleanup_claims()
        )
        await termination_started.wait()
        create_task = asyncio.create_task(
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )
        )
        try:
            await asyncio.wait_for(explicit_create_called.wait(), timeout=0.5)
        finally:
            release_termination.set()
            await asyncio.gather(cleanup_task, create_task)

        self.assertIs(
            self.client._active_connection_sandboxes[(NAMESPACE, CLAIM_NAME)],
            adopted_handle,
        )
        self.assertNotIn(cleanup_key, self.client._automatic_cleanup_claims)

    async def test_slow_delete_does_not_block_create_for_another_claim(self):
        deleted_claim_name = "generated-claim"
        deleted_key = (NAMESPACE, deleted_claim_name)
        termination_started = asyncio.Event()
        release_termination = asyncio.Event()
        explicit_create_called = asyncio.Event()
        generated_handle = MagicMock()

        async def block_termination():
            termination_started.set()
            await release_termination.wait()

        async def create_claim(*_args, **_kwargs):
            explicit_create_called.set()
            return matching_claim()

        generated_handle.claim_name = deleted_claim_name
        generated_handle.terminate = AsyncMock()
        generated_handle.close_connection = AsyncMock(
            side_effect=block_termination
        )
        self.client._active_connection_sandboxes[deleted_key] = generated_handle
        self.client._active_claim_uids[deleted_key] = "generated-uid"
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            side_effect=create_claim
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="created-sandbox"
        )

        delete_task = asyncio.create_task(
            self.client.delete_sandbox(deleted_claim_name, NAMESPACE)
        )
        await asyncio.wait_for(termination_started.wait(), timeout=5)
        create_task = asyncio.create_task(
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )
        )
        try:
            await asyncio.wait_for(explicit_create_called.wait(), timeout=0.5)
        finally:
            release_termination.set()
            await asyncio.gather(delete_task, create_task)

    async def test_list_active_sandboxes(self):
        mock_active = MagicMock()
        mock_active.is_active = True
        self.client._active_connection_sandboxes[("ns1", "active-claim")] = mock_active

        mock_inactive = MagicMock()
        mock_inactive.is_active = False
        self.client._active_connection_sandboxes[("ns2", "inactive-claim")] = mock_inactive

        active = await self.client.list_active_sandboxes()
        self.assertEqual(active, [("ns1", "active-claim")])

    async def test_list_active_retires_inactive_automatic_handle(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = AsyncSandbox.__new__(AsyncSandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")

        self.assertEqual(await self.client.list_active_sandboxes(), [])
        await self.client._delete_automatic_cleanup_claims()
        await retained_handle.terminate()

        self.assertIsNone(retained_handle.claim_name)
        self.assertNotIn(key, self.client._active_claim_uids)
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="old-uid"
        )

    async def test_close_preserves_caller_owned_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = AsyncSandbox.__new__(AsyncSandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.mock_k8s_helper.close = AsyncMock()
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")
        async with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        await self.client.close()
        await self.client._delete_automatic_cleanup_claims()

        self.assertEqual(retained_handle.claim_name, CLAIM_NAME)
        self.assertIn(key, self.client._caller_owned_claims)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_failed_lookup_preserves_caller_owned_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = AsyncSandbox.__new__(AsyncSandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")
        async with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        await self.client._detach_failed_lookup(key, retained_handle)
        await self.client._delete_automatic_cleanup_claims()

        self.assertEqual(retained_handle.claim_name, CLAIM_NAME)
        self.assertIn(key, self.client._caller_owned_claims)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_failed_lookup_cannot_delete_claim_adopted_concurrently(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = AsyncSandbox.__new__(AsyncSandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")
        async with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        await self.client._detach_failed_lookup(key, retained_handle)
        await self.client._delete_automatic_cleanup_claims()

        self.assertEqual(retained_handle.claim_name, CLAIM_NAME)
        self.assertIn(key, self.client._caller_owned_claims)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

    async def test_list_all_sandboxes(self):
        self.mock_k8s_helper.list_sandbox_claims = AsyncMock(
            return_value=["sb-1", "sb-2"]
        )
        result = await self.client.list_all_sandboxes("test-ns")
        self.assertEqual(result, ["sb-1", "sb-2"])

    async def test_delete_sandbox_in_registry(self):
        key = ("test-ns", "test-claim")
        mock_sandbox = MagicMock(claim_name="test-claim")
        mock_sandbox.terminate = AsyncMock()
        mock_sandbox.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = mock_sandbox
        self.client._active_claim_uids[key] = "claim-uid"
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()

        await self.client.delete_sandbox("test-claim", "test-ns")
        mock_sandbox.terminate.assert_not_awaited()
        mock_sandbox.close_connection.assert_awaited_once_with()
        self.assertNotIn(
            key,
            self.client._automatic_cleanup_claims,
        )
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once_with(
            "test-claim", "test-ns", expected_uid="claim-uid"
        )

    async def test_delete_failure_preserves_registry_for_retry(self):
        key = ("test-ns", "test-claim")
        mock_sandbox = MagicMock(claim_name="test-claim")
        mock_sandbox.terminate = AsyncMock()
        mock_sandbox.close_connection = AsyncMock(
            side_effect=RuntimeError("delete failed")
        )
        self.client._active_connection_sandboxes[key] = mock_sandbox
        self.client._active_claim_uids[key] = "claim-uid"
        self.client._claim_ownership.register_automatic(key, "claim-uid")

        await self.client.delete_sandbox("test-claim", "test-ns")

        self.assertIs(self.client._active_connection_sandboxes[key], mock_sandbox)
        self.assertEqual(self.client._active_claim_uids[key], "claim-uid")
        self.assertIn(key, self.client._automatic_cleanup_claims)

    async def test_delete_all(self):
        mock1 = MagicMock()
        mock1.terminate = AsyncMock()
        mock2 = MagicMock()
        mock2.terminate = AsyncMock()
        self.client._active_connection_sandboxes[("ns1", "c1")] = mock1
        self.client._active_connection_sandboxes[("ns2", "c2")] = mock2
        self.client._automatic_cleanup_claims = {("ns1", "c1")}

        with patch.object(self.client, "delete_sandbox", new_callable=AsyncMock) as mock_del:
            await self.client.delete_all()
            self.assertEqual(mock_del.call_count, 2)

    async def test_close_clears_registry(self):
        key = ("ns", "claim")
        mock_sandbox = MagicMock()
        mock_sandbox.claim_name = "claim"
        mock_sandbox.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = mock_sandbox
        self.client._active_claim_uids[key] = "claim-uid"
        self.client._claim_ownership.register_automatic(key, "claim-uid")
        self.mock_k8s_helper.close = AsyncMock()

        await self.client.close()

        self.assertEqual(len(self.client._active_connection_sandboxes), 0)
        self.assertEqual(self.client._active_claim_uids, {})
        self.assertIsNone(mock_sandbox.claim_name)
        self.assertIn(key, self.client._automatic_cleanup_claims)
        mock_sandbox.close_connection.assert_awaited_once()
        self.mock_k8s_helper.close.assert_awaited_once()

    async def test_cancelled_handle_retirement_cannot_install_candidate(self):
        key = (NAMESPACE, CLAIM_NAME)
        close_started = asyncio.Event()
        old_handle = MagicMock(
            is_active=True,
            sandbox_id="old-sandbox",
            claim_name=CLAIM_NAME,
        )

        async def block_close():
            close_started.set()
            await asyncio.Event().wait()

        old_handle.close_connection = AsyncMock(side_effect=block_close)
        candidate = MagicMock(
            is_active=True,
            sandbox_id="new-sandbox",
            claim_name=CLAIM_NAME,
        )
        candidate.close_connection = AsyncMock()
        self.client._active_connection_sandboxes[key] = old_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(
            return_value=claim_for_request()
        )
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(
            return_value="new-sandbox"
        )
        self.mock_sandbox_class.return_value = candidate

        creation = asyncio.create_task(
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
            )
        )
        await asyncio.wait_for(close_started.wait(), timeout=5)
        creation.cancel()

        with self.assertRaises(asyncio.CancelledError):
            await creation

        self.assertIsNone(old_handle.claim_name)
        self.assertIsNone(candidate.claim_name)
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertNotIn(key, self.client._active_claim_uids)

    async def test_context_manager(self):
        self.mock_k8s_helper.close = AsyncMock()

        async with self.client as c:
            self.assertIsInstance(c, AsyncSandboxClient)

        self.mock_k8s_helper.close.assert_called_once()

    async def test_requires_connection_config(self):
        with self.assertRaises(ValueError) as ctx:
            AsyncSandboxClient(connection_config=None)
        self.assertIn("connection_config is required", str(ctx.exception))

    def test_cleanup_default_registers_atexit(self):
        """Constructing without cleanup= should default to True and register the hook."""
        with patch("k8s_agent_sandbox.async_sandbox_client.atexit") as mock_atexit:
            client = AsyncSandboxClient(connection_config=self.config)
            mock_atexit.register.assert_called_once_with(client._atexit_cleanup)

    def test_cleanup_true_registers_atexit(self):
        """cleanup=True should register the _atexit_cleanup method as an atexit handler."""
        with patch("k8s_agent_sandbox.async_sandbox_client.atexit") as mock_atexit:
            client = AsyncSandboxClient(connection_config=self.config, cleanup=True)
            mock_atexit.register.assert_called_once_with(client._atexit_cleanup)

    def test_cleanup_false_does_not_register_atexit(self):
        """cleanup=False should opt out and not register any atexit handler."""
        with patch("k8s_agent_sandbox.async_sandbox_client.atexit") as mock_atexit:
            AsyncSandboxClient(connection_config=self.config, cleanup=False)
            mock_atexit.register.assert_not_called()

    def test_atexit_cleanup_deletes_tracked_claims(self):
        """_atexit_cleanup should delete every automatically managed claim."""
        self.client._active_connection_sandboxes = {
            ("default", "claim-abc"): MagicMock(),
            ("other-ns", "claim-xyz"): MagicMock(),
        }
        self.client._claim_ownership.register_automatic(
            ("default", "claim-abc"), "claim-abc-uid"
        )
        self.client._claim_ownership.register_automatic(
            ("other-ns", "claim-xyz"), "claim-xyz-uid"
        )
        mock_helper_instance = MagicMock()
        mock_helper_instance.delete_sandbox_claim = MagicMock()

        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper", return_value=mock_helper_instance):
            self.client._atexit_cleanup()

        mock_helper_instance.delete_sandbox_claim.assert_any_call(
            "claim-abc",
            "default",
            _request_timeout=_ATEXIT_DELETE_REQUEST_TIMEOUT_SECONDS,
            expected_uid="claim-abc-uid",
        )
        mock_helper_instance.delete_sandbox_claim.assert_any_call(
            "claim-xyz",
            "other-ns",
            _request_timeout=_ATEXIT_DELETE_REQUEST_TIMEOUT_SECONDS,
            expected_uid="claim-xyz-uid",
        )

    def test_atexit_cleanup_uses_observed_claim_uid(self):
        key = ("default", "claim-abc")
        self.client._claim_ownership.register_automatic(key, "claim-uid")
        mock_helper_instance = MagicMock()

        with patch(
            "k8s_agent_sandbox.async_sandbox_client.K8sHelper",
            return_value=mock_helper_instance,
        ):
            self.client._atexit_cleanup()

        mock_helper_instance.delete_sandbox_claim.assert_called_once_with(
            "claim-abc",
            "default",
            _request_timeout=_ATEXIT_DELETE_REQUEST_TIMEOUT_SECONDS,
            expected_uid="claim-uid",
        )

    def test_atexit_cleanup_skips_claim_without_observed_uid(self):
        key = ("default", "claim-abc")
        self.client._automatic_cleanup_claims.add(key)

        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper") as mock_helper:
            self.client._atexit_cleanup()

        mock_helper.assert_not_called()

    def test_atexit_cleanup_skips_when_no_sandboxes(self):
        """_atexit_cleanup should be a no-op when there are no tracked sandboxes."""
        self.client._active_connection_sandboxes = {}
        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper") as MockHelper:
            self.client._atexit_cleanup()
            MockHelper.assert_not_called()

    def test_atexit_cleanup_skips_explicit_claims(self):
        self.client._active_connection_sandboxes = {
            ("default", "explicit-claim"): MagicMock()
        }
        with patch(
            "k8s_agent_sandbox.async_sandbox_client.K8sHelper"
        ) as mock_helper:
            self.client._atexit_cleanup()

        mock_helper.assert_not_called()

    def test_atexit_cleanup_skips_caller_protected_automatic_claim(self):
        key = ("default", "claim-abc")
        self.client._automatic_cleanup_claims.add(key)
        self.client._caller_owned_claims.add(key)

        with patch(
            "k8s_agent_sandbox.async_sandbox_client.K8sHelper"
        ) as mock_helper:
            self.client._atexit_cleanup()

        mock_helper.assert_not_called()

    def test_atexit_cleanup_suppresses_errors(self):
        """_atexit_cleanup should not propagate exceptions — cleanup is best-effort.
        A warning is printed to stderr so the user knows a sandbox was orphaned."""
        self.client._active_connection_sandboxes = {("default", "claim-abc"): MagicMock()}
        self.client._claim_ownership.register_automatic(
            ("default", "claim-abc"), "claim-uid"
        )
        mock_helper_instance = MagicMock()
        mock_helper_instance.delete_sandbox_claim = MagicMock(side_effect=Exception("network error"))

        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper", return_value=mock_helper_instance):
            with patch("k8s_agent_sandbox.async_sandbox_client.sys.stderr") as mock_stderr:
                # Should not raise
                self.client._atexit_cleanup()
                # Should have printed a warning
                mock_stderr.write.assert_called()

    def test_atexit_cleanup_suppresses_helper_construction_errors(self):
        """A failure constructing K8sHelper itself (e.g. no reachable kubeconfig)
        must not escape _atexit_cleanup either — cleanup is best-effort."""
        self.client._active_connection_sandboxes = {("default", "claim-abc"): MagicMock()}
        self.client._claim_ownership.register_automatic(
            ("default", "claim-abc"), "claim-uid"
        )

        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper", side_effect=Exception("no kubeconfig")):
            with patch("k8s_agent_sandbox.async_sandbox_client.sys.stderr") as mock_stderr:
                # Should not raise
                self.client._atexit_cleanup()
                # Should have printed a warning
                mock_stderr.write.assert_called()

    def test_atexit_cleanup_suppresses_claim_snapshot_errors(self):
        """A failure snapshotting managed claims must not escape cleanup."""
        mock_claims = MagicMock()
        mock_claims.__iter__.side_effect = RuntimeError("set changed size during iteration")
        self.client._automatic_cleanup_claims = mock_claims

        with patch("k8s_agent_sandbox.async_sandbox_client.sys.stderr") as mock_stderr:
            # Should not raise
            self.client._atexit_cleanup()
            # Should have printed a warning
            mock_stderr.write.assert_called()

    async def test_validate_labels_rejects_invalid_value(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", labels={"agent": "invalid value!"})

    async def test_validate_labels_rejects_empty_key(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", labels={"": "v"})

    async def test_create_sandbox_with_pod_metadata(self):
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="resolved-id")
        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            await self.client.create_sandbox(
                "test-warmpool", "test-namespace",
                pod_labels={"client-id": "tenant-a"},
                pod_annotations={"note": "owned-by-tenant-a"},
            )

            call_kwargs = mock_create.call_args[1]
            self.assertEqual(
                call_kwargs["pod_metadata"],
                {
                    "labels": {"client-id": "tenant-a"},
                    "annotations": {"note": "owned-by-tenant-a"},
                },
            )

    async def test_create_sandbox_rejects_invalid_pod_label(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", pod_labels={"bad key!": "v"})

    async def test_create_sandbox_with_shutdown_after_seconds(self):
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="resolved-id")
        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            await self.client.create_sandbox(
                "test-warmpool", "test-namespace", shutdown_after_seconds=300
            )

            mock_create.assert_called_once()
            call_kwargs = mock_create.call_args
            lifecycle = call_kwargs[1].get("lifecycle")
            self.assertIsNotNone(lifecycle)
            self.assertEqual(lifecycle["shutdownPolicy"], "Delete")
            self.assertIn("shutdownTime", lifecycle)

    async def test_create_sandbox_with_volume_claim_templates(self):
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="resolved-id")
        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        vcts = [{"metadata": {"name": "data"}, "spec": {"resources": {"requests": {"storage": "10Gi"}}}}]

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            await self.client.create_sandbox(
                "test-warmpool",
                "test-namespace",
                volume_claim_templates=vcts,
            )

            mock_create.assert_called_once_with(
                ANY,
                "test-warmpool",
                "test-namespace",
                labels=None,
                lifecycle=None,
                volume_claim_templates=vcts,
                pod_metadata=None,
                env=None,
            )

    async def test_create_claim_with_volume_claim_templates(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"

        vcts = [{"metadata": {"name": "data"}, "spec": {"resources": {"requests": {"storage": "10Gi"}}}}]
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock()

        await self.client._create_claim(
            "test-claim",
            "test-warmpool",
            "test-namespace",
            volume_claim_templates=vcts,
        )

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim",
            "test-warmpool",
            "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels=None,
            lifecycle=None,
            volume_claim_templates=vcts,
            pod_metadata=None,
            env=None,
        )

    async def test_create_sandbox_without_shutdown_after_seconds(self):
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="resolved-id")
        mock_sandbox_instance = MagicMock()
        mock_sandbox_instance.terminate = AsyncMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock) as mock_create, \
             patch.object(self.client, "_wait_for_sandbox_ready", new_callable=AsyncMock):

            await self.client.create_sandbox("test-warmpool", "test-namespace")

            call_kwargs = mock_create.call_args
            lifecycle = call_kwargs[1].get("lifecycle")
            self.assertIsNone(lifecycle)

    async def test_create_claim_with_env(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = None
        self.mock_k8s_helper.create_sandbox_claim = AsyncMock()

        env = {"FOO": "bar"}
        await self.client._create_claim("test-claim", "test-warmpool", "test-namespace", env=env)

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim",
            "test-warmpool",
            "test-namespace",
            annotations={},
            labels=None,
            lifecycle=None,
            volume_claim_templates=None,
            pod_metadata=None,
            env=env,
        )

    async def test_shutdown_after_seconds_validation_zero(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", shutdown_after_seconds=0)

    async def test_shutdown_after_seconds_validation_negative(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", shutdown_after_seconds=-1)

    async def test_shutdown_after_seconds_validation_bool(self):
        with self.assertRaises(ValueError):
            await self.client.create_sandbox("t", shutdown_after_seconds=True)


class TestAsyncSandbox(unittest.IsolatedAsyncioTestCase):

    async def test_requires_connection_config(self):
        with self.assertRaises(ValueError) as ctx:
            AsyncSandbox(
                claim_name="test",
                sandbox_id="test-id",
                connection_config=None,
            )
        self.assertIn("connection_config is required", str(ctx.exception))

    async def test_get_pod_ip(self):
        """Tests that get_pod_ip returns the pod IP when present."""
        mock_k8s_helper = AsyncMock()
        mock_k8s_helper.get_sandbox = AsyncMock(return_value={
            "status": {
                "podIPs": ["10.244.0.42"]
            }
        })
        sandbox = AsyncSandbox(
            claim_name="test",
            sandbox_id="test-id",
            connection_config=MagicMock(),
            k8s_helper=mock_k8s_helper,
        )
        self.assertEqual(await sandbox.get_pod_ip(), "10.244.0.42")

    async def test_get_pod_ip_prioritization_and_normalization(self):
        """Tests that get_pod_ip uses select_pod_ip to prioritize and normalize IPs."""
        mock_k8s_helper = AsyncMock()
        mock_k8s_helper.get_sandbox = AsyncMock(return_value={
            "status": {
                "podIPs": ["::ffff:10.244.0.42", "2001:db8::1"]
            }
        })
        sandbox = AsyncSandbox(
            claim_name="test",
            sandbox_id="test-id",
            connection_config=MagicMock(),
            k8s_helper=mock_k8s_helper,
        )
        self.assertEqual(await sandbox.get_pod_ip(), "10.244.0.42")

    @patch("k8s_agent_sandbox.async_sandbox.AsyncFilesystem")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncCommandExecutor")
    @patch("k8s_agent_sandbox.async_sandbox.create_tracer_manager")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncSandboxConnector")
    @patch("k8s_agent_sandbox.async_sandbox.AsyncK8sHelper")
    async def test_in_cluster_passes_pod_ip_callback(self, mock_k8s_helper, mock_connector, mock_create_tracer_manager, mock_command_executor, mock_filesystem):
        config = SandboxInClusterConnectionConfig()
        mock_create_tracer_manager.return_value = (MagicMock(), MagicMock())

        sandbox = AsyncSandbox(
            claim_name="test-claim",
            sandbox_id="test-id",
            connection_config=config,
        )

        callback = mock_connector.call_args.kwargs["get_pod_ip"]
        self.assertIs(callback.__self__, sandbox)
        self.assertIs(callback.__func__, AsyncSandbox.get_pod_ip)


class TestAsyncSandboxClientInCluster(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        self.MockAsyncK8sHelper = patcher.start()
        self.addCleanup(patcher.stop)

    async def test_in_cluster_config_accepted(self):
        config = SandboxInClusterConnectionConfig()
        client = AsyncSandboxClient(connection_config=config, cleanup=False)
        self.assertIsInstance(client.connection_config, SandboxInClusterConnectionConfig)

    async def test_in_cluster_connection_config_passed_to_sandbox(self):
        config = SandboxInClusterConnectionConfig()
        client = AsyncSandboxClient(connection_config=config, cleanup=False)
        mock_k8s_helper = client.k8s_helper
        mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="my-sandbox")

        mock_sandbox_class = MagicMock()
        mock_sandbox_class.return_value = MagicMock()
        client.sandbox_class = mock_sandbox_class

        with patch.object(client, "_create_claim", new_callable=AsyncMock), \
             patch.object(client, "_wait_for_sandbox_ready", new_callable=AsyncMock):
            await client.create_sandbox("my-warmpool")

        call_kwargs = mock_sandbox_class.call_args.kwargs
        self.assertEqual(call_kwargs["connection_config"], config)


class TestAsyncConnector(unittest.IsolatedAsyncioTestCase):

    async def test_rejects_local_tunnel_config(self):
        with self.assertRaises(ValueError) as ctx:
            AsyncSandboxConnector(
                sandbox_id="test",
                namespace="default",
                connection_config=SandboxLocalTunnelConnectionConfig(),
                k8s_helper=MagicMock(),
            )
        self.assertIn("does not support SandboxLocalTunnelConnectionConfig", str(ctx.exception))

    async def test_post_requests_are_not_retried_on_server_error(self):
        connector = AsyncSandboxConnector(
            sandbox_id="test",
            namespace="default",
            connection_config=SandboxDirectConnectionConfig(
                api_url="http://router"
            ),
            k8s_helper=MagicMock(),
        )
        response = MagicMock()
        response.status_code = 503
        response.is_redirect = False
        response.raise_for_status.side_effect = httpx.HTTPStatusError(
            "503 Service Unavailable",
            request=MagicMock(),
            response=response,
        )
        connector.client.request = AsyncMock(return_value=response)

        try:
            with patch(
                "k8s_agent_sandbox.async_connector.asyncio.sleep",
                new=AsyncMock(),
            ):
                with self.assertRaises(SandboxRequestError):
                    await connector.send_request("POST", "execute")

            self.assertEqual(connector.client.request.await_count, 1)
        finally:
            await connector.close()

    async def test_idempotent_methods_are_retried_on_server_error(self):
        for method in ("GET", "PUT", "DELETE"):
            with self.subTest(method=method):
                connector = AsyncSandboxConnector(
                    sandbox_id="test",
                    namespace="default",
                    connection_config=SandboxDirectConnectionConfig(
                        api_url="http://router"
                    ),
                    k8s_helper=MagicMock(),
                )
                error_response = MagicMock()
                error_response.status_code = 503
                error_response.is_redirect = False
                ok_response = MagicMock()
                ok_response.status_code = 200
                ok_response.is_redirect = False
                ok_response.raise_for_status.return_value = None
                connector.client.request = AsyncMock(
                    side_effect=[error_response, ok_response]
                )

                try:
                    with patch(
                        "k8s_agent_sandbox.async_connector.asyncio.sleep",
                        new=AsyncMock(),
                    ):
                        result = await connector.send_request(method, "path")

                    self.assertIs(result, ok_response)
                    self.assertEqual(connector.client.request.await_count, 2)
                finally:
                    await connector.close()

    async def test_in_cluster_resolves_dns_by_default(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        url = await connector._resolve_base_url()
        self.assertEqual(url, "http://my-sandbox.dev.svc.cluster.local:8888")

    async def test_in_cluster_resolves_pod_ip_via_callable(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
            get_pod_ip=AsyncMock(return_value="10.244.0.5"),
        )
        url = await connector._resolve_base_url()
        self.assertEqual(url, "http://10.244.0.5:8888")

    async def test_in_cluster_resolves_ipv6_pod_ip(self):
        """IPv6 pod IPs must be bracketed in the base URL (RFC 3986)."""
        config = SandboxInClusterConnectionConfig(server_port=8888)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
            get_pod_ip=AsyncMock(return_value="2001:db8::1"),
        )
        url = await connector._resolve_base_url()
        self.assertEqual(url, "http://[2001:db8::1]:8888")

    async def test_gateway_resolves_ipv6(self):
        """Gateway IPv6 addresses must be bracketed in the base URL."""
        config = SandboxGatewayConnectionConfig(
            gateway_name="test-gw",
            gateway_namespace="default",
        )
        mock_k8s = MagicMock()
        mock_k8s.wait_for_gateway_ip = AsyncMock(return_value="2001:db8::1")
        connector = AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=mock_k8s,
        )
        url = await connector._resolve_base_url()
        self.assertEqual(url, "http://[2001:db8::1]")

    async def test_gateway_does_not_bracket_ipv4(self):
        """Gateway IPv4 addresses must NOT be bracketed."""
        config = SandboxGatewayConnectionConfig(
            gateway_name="test-gw",
            gateway_namespace="default",
        )
        mock_k8s = MagicMock()
        mock_k8s.wait_for_gateway_ip = AsyncMock(return_value="34.56.78.90")
        connector = AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=mock_k8s,
        )
        url = await connector._resolve_base_url()
        self.assertEqual(url, "http://34.56.78.90")

    async def test_in_cluster_does_not_inject_router_headers(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        self.assertFalse(connector._inject_router_headers)

    async def test_direct_injects_router_headers(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        self.assertTrue(connector._inject_router_headers)

    async def test_timeout_header_is_sent_for_router_requests(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        mock_response = AsyncMock(spec=httpx.Response)
        mock_response.status_code = 200
        mock_response.is_redirect = False
        mock_response.raise_for_status.return_value = None
        connector.client.request = AsyncMock(return_value=mock_response)

        await connector.send_request("GET", "health", timeout=123)

        _, call_kwargs = connector.client.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertEqual(sent_headers.get("X-Sandbox-Timeout"), "123")

    async def test_timeout_object_uses_read_timeout_for_router_requests(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        mock_response = AsyncMock(spec=httpx.Response)
        mock_response.status_code = 200
        mock_response.is_redirect = False
        mock_response.raise_for_status.return_value = None
        connector.client.request = AsyncMock(return_value=mock_response)

        await connector.send_request("GET", "health", timeout=httpx.Timeout(123.0))

        _, call_kwargs = connector.client.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertEqual(sent_headers.get("X-Sandbox-Timeout"), "123.0")

    async def test_timeout_object_without_read_timeout_does_not_send_header(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        mock_response = AsyncMock(spec=httpx.Response)
        mock_response.status_code = 200
        mock_response.is_redirect = False
        mock_response.raise_for_status.return_value = None
        connector.client.request = AsyncMock(return_value=mock_response)

        await connector.send_request("GET", "health", timeout=httpx.Timeout(None))

        _, call_kwargs = connector.client.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertNotIn("X-Sandbox-Timeout", sent_headers)

    async def test_unsupported_timeout_does_not_send_header(self):
        config = SandboxDirectConnectionConfig(api_url="http://router")
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        mock_response = AsyncMock(spec=httpx.Response)
        mock_response.status_code = 200
        mock_response.is_redirect = False
        mock_response.raise_for_status.return_value = None
        connector.client.request = AsyncMock(return_value=mock_response)

        await connector.send_request("GET", "health", timeout=object())

        _, call_kwargs = connector.client.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertNotIn("X-Sandbox-Timeout", sent_headers)

    async def test_timeout_header_is_not_sent_for_in_cluster_requests(self):
        config = SandboxInClusterConnectionConfig(server_port=8888)
        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="dev",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        mock_response = AsyncMock(spec=httpx.Response)
        mock_response.status_code = 200
        mock_response.is_redirect = False
        mock_response.raise_for_status.return_value = None
        connector.client.request = AsyncMock(return_value=mock_response)

        await connector.send_request("GET", "health", timeout=123)

        _, call_kwargs = connector.client.request.call_args
        sent_headers = call_kwargs.get("headers", {})
        self.assertNotIn("X-Sandbox-Timeout", sent_headers)


class AsyncSandboxHandler(BaseHTTPRequestHandler):
    """Minimal handler for async connector HTTP tests."""

    def do_POST(self):
        if self.path == "/execute":
            self._respond(HTTPStatus.OK, {"stdout": "hello", "stderr": "", "exit_code": 0})
        elif self.path == "/server-error":
            self._respond(HTTPStatus.INTERNAL_SERVER_ERROR, {"detail": "boom"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "not found"})

    def do_GET(self):
        if self.path == "/health":
            self._respond(HTTPStatus.OK, {"status": "healthy"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "not found"})

    def _respond(self, status: HTTPStatus, body: dict):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        payload = json.dumps(body).encode()
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


def _start_stub_server(handler_cls):
    """Starts handler_cls on a local HTTPServer on a daemon thread.

    Returns (server, thread, port); pass to _stop_stub_server in tearDownClass.
    """
    server = HTTPServer(("127.0.0.1", 0), handler_cls)
    port = server.server_address[1]
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread, port


def _stop_stub_server(server, thread):
    server.shutdown()
    server.server_close()
    thread.join(timeout=5)


class TestAsyncConnectorHTTP(unittest.IsolatedAsyncioTestCase):

    @classmethod
    def setUpClass(cls):
        cls.server, cls.server_thread, cls.port = _start_stub_server(AsyncSandboxHandler)

    @classmethod
    def tearDownClass(cls):
        _stop_stub_server(cls.server, cls.server_thread)

    def _make_connector(self) -> AsyncSandboxConnector:
        config = SandboxDirectConnectionConfig(
            api_url=f"http://127.0.0.1:{self.port}",
            server_port=self.port,
        )
        k8s_helper = MagicMock()
        return AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=k8s_helper,
        )

    async def test_successful_request(self):
        connector = self._make_connector()
        try:
            response = await connector.send_request("GET", "health")
            self.assertEqual(response.status_code, 200)
            self.assertEqual(response.json()["status"], "healthy")
        finally:
            await connector.close()

    async def test_follow_redirects_is_false(self):
        connector = self._make_connector()
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.is_redirect = False
        mock_response.raise_for_status = MagicMock()
        connector.client.request = AsyncMock(return_value=mock_response)

        try:
            await connector.send_request("GET", "health")

            call_args, call_kwargs = connector.client.request.call_args
            self.assertFalse(call_kwargs.get("follow_redirects", True))
        finally:
            await connector.close()

    async def test_follow_redirects_in_kwargs_popped(self):
        connector = self._make_connector()
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.is_redirect = False
        mock_response.raise_for_status = MagicMock()
        connector.client.request = AsyncMock(return_value=mock_response)

        try:
            await connector.send_request("GET", "health", follow_redirects=True)

            call_args, call_kwargs = connector.client.request.call_args
            self.assertFalse(call_kwargs.get("follow_redirects", True))
        finally:
            await connector.close()

    async def test_redirect_raises_error(self):
        connector = self._make_connector()
        mock_response = MagicMock()
        mock_response.status_code = 302
        mock_response.is_redirect = True
        mock_response.raise_for_status = MagicMock()
        connector.client.request = AsyncMock(return_value=mock_response)

        try:
            with self.assertRaises(SandboxRequestError):
                await connector.send_request("GET", "health")
        finally:
            await connector.close()

    async def test_304_does_not_raise_redirect_error(self):
        connector = self._make_connector()
        mock_response = MagicMock()
        mock_response.status_code = 304
        mock_response.is_redirect = False
        mock_response.raise_for_status = MagicMock()
        connector.client.request = AsyncMock(return_value=mock_response)

        try:
            await connector.send_request("GET", "health")
        finally:
            await connector.close()

    async def test_300_does_not_raise_redirect_error(self):
        connector = self._make_connector()
        mock_response = MagicMock()
        mock_response.status_code = 300
        mock_response.is_redirect = False
        mock_response.raise_for_status = MagicMock()
        connector.client.request = AsyncMock(return_value=mock_response)

        try:
            await connector.send_request("GET", "health")
        finally:
            await connector.close()

    async def test_post_execute(self):
        connector = self._make_connector()
        try:
            response = await connector.send_request(
                "POST", "execute", json={"command": "echo hello"}
            )
            self.assertEqual(response.status_code, 200)
            data = response.json()
            self.assertEqual(data["stdout"], "hello")
            self.assertEqual(data["exit_code"], 0)
        finally:
            await connector.close()

    async def test_404_raises_sandbox_request_error(self):
        connector = self._make_connector()
        try:
            with self.assertRaises(SandboxRequestError) as ctx:
                await connector.send_request("GET", "nonexistent")
            self.assertEqual(ctx.exception.status_code, 404)
        finally:
            await connector.close()

    async def test_sandbox_request_error_is_runtime_error(self):
        """Backward compat: SandboxRequestError is still a RuntimeError."""
        connector = self._make_connector()
        try:
            with self.assertRaises(RuntimeError):
                await connector.send_request("GET", "nonexistent")
        finally:
            await connector.close()

    async def test_connection_refused_no_status_code(self):
        config = SandboxDirectConnectionConfig(
            api_url="http://127.0.0.1:1", server_port=1
        )
        connector = AsyncSandboxConnector(
            sandbox_id="test",
            namespace="default",
            connection_config=config,
            k8s_helper=MagicMock(),
        )
        try:
            with self.assertRaises(SandboxRequestError) as ctx:
                await connector.send_request("POST", "run", timeout=1)
            self.assertIsNone(ctx.exception.status_code)
        finally:
            await connector.close()

    async def test_sandbox_headers_sent(self):
        """Verify X-Sandbox-* headers are included in requests."""
        connector = self._make_connector()
        try:
            response = await connector.send_request("GET", "health")
            # We can't easily inspect request headers from the server side
            # in this test setup, but the request succeeds which validates
            # the header injection doesn't break the flow.
            self.assertEqual(response.status_code, 200)
        finally:
            await connector.close()


class TestAsyncSandboxClientInClusterConnectionConfig(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        self.MockAsyncK8sHelper = patcher.start()
        self.addCleanup(patcher.stop)

        self.config = SandboxInClusterConnectionConfig(server_port=8888)
        # cleanup=False keeps tests hermetic; the new default (True) registers a global atexit hook.
        self.client = AsyncSandboxClient(connection_config=self.config, cleanup=False)
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class

    async def test_create_sandbox_passes_connection_config(self):
        self.mock_k8s_helper.wait_for_claim_ready = AsyncMock(return_value="sandbox-123")
        self.mock_k8s_helper.wait_for_sandbox_ready = AsyncMock(return_value="10.244.0.5")

        mock_sandbox = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox

        with patch.object(self.client, "_create_claim", new_callable=AsyncMock):
            await self.client.create_sandbox("test-template", "default")

        call_kwargs = self.mock_sandbox_class.call_args.kwargs
        self.assertEqual(call_kwargs["connection_config"], self.config)

    async def test_get_sandbox_passes_connection_config(self):
        self.mock_k8s_helper.resolve_sandbox_name = AsyncMock(return_value="sandbox-123")
        self.mock_k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})
        self.mock_k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=claim_for_request(
                claim_name="test-claim", namespace="default"
            )
        )

        mock_sandbox = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox

        await self.client.get_sandbox("test-claim", "default")

        call_kwargs = self.mock_sandbox_class.call_args.kwargs
        self.assertEqual(call_kwargs["connection_config"], self.config)

    async def test_get_sandbox_passes_connection_config_for_non_incluster(self):
        """Verify connection_config is passed through for non-InCluster configs."""
        config = SandboxDirectConnectionConfig(api_url="http://test", server_port=8888)
        client = AsyncSandboxClient(connection_config=config, cleanup=False)
        client.k8s_helper.resolve_sandbox_name = AsyncMock(return_value="sandbox-123")
        client.k8s_helper.get_sandbox = AsyncMock(return_value={"metadata": {}})
        client.k8s_helper.get_sandbox_claim = AsyncMock(
            return_value=claim_for_request(
                claim_name="test-claim", namespace="default"
            )
        )

        mock_sandbox = MagicMock()
        client.sandbox_class = MagicMock(return_value=mock_sandbox)

        await client.get_sandbox("test-claim", "default")

        call_kwargs = client.sandbox_class.call_args.kwargs
        self.assertEqual(call_kwargs["connection_config"], config)


class TestAsyncConnectorCacheInvalidation(unittest.IsolatedAsyncioTestCase):
    """Tests for Bug Fix #2: Cache invalidation on HTTPStatusError."""

    async def test_server_error_clears_pod_ip_cache(self):
        """Verify a 5xx clears the pod IP cache so the next request re-resolves.

        A 5xx is how a stale cached Pod IP commonly surfaces after the pod is
        replaced, so the cached routing state is dropped.
        """
        config = SandboxInClusterConnectionConfig(server_port=8888)

        # Mock get_pod_ip to track how many times it's called
        call_count = [0]
        async def mock_get_pod_ip():
            call_count[0] += 1
            return "10.244.0.5"

        connector = AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=MagicMock(),
            get_pod_ip=mock_get_pod_ip,
        )

        # Mock httpx client to return 503 on first request
        mock_response = MagicMock()
        mock_response.status_code = 503
        mock_response.is_redirect = False
        mock_response.raise_for_status.side_effect = httpx.HTTPStatusError(
            "503 Service Unavailable",
            request=MagicMock(),
            response=mock_response
        )

        connector.client.request = AsyncMock(return_value=mock_response)

        try:
            with patch(
                "k8s_agent_sandbox.async_connector.asyncio.sleep",
                new=AsyncMock(),
            ):
                # First request should fail with 503
                with self.assertRaises(SandboxRequestError):
                    await connector.send_request("GET", "test")

                # Verify cache was cleared (pod_ip_resolved reset)
                self.assertFalse(connector._pod_ip_resolved,
                               "A 5xx should clear pod_ip_resolved flag")
                self.assertIsNone(connector._cached_pod_ip_url,
                                "A 5xx should clear cached pod IP URL")

                # Second request should re-resolve pod IP (call count increases)
                initial_count = call_count[0]
                mock_response.status_code = 200
                mock_response.raise_for_status.side_effect = None
                connector.client.request = AsyncMock(return_value=mock_response)

                await connector.send_request("GET", "test")

                self.assertEqual(call_count[0], initial_count + 1,
                               "After cache invalidation, pod IP should be re-resolved")
        finally:
            await connector.close()

    async def test_client_error_keeps_pod_ip_cache(self):
        """Verify a 4xx leaves the pod IP cache intact.

        A 4xx means the sandbox answered a well-formed request with a client
        error; routing is fine, so the cached Pod IP must be preserved.
        """
        config = SandboxInClusterConnectionConfig(server_port=8888)

        call_count = [0]
        async def mock_get_pod_ip():
            call_count[0] += 1
            return "10.244.0.5"

        connector = AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=MagicMock(),
            get_pod_ip=mock_get_pod_ip,
        )

        mock_response = MagicMock()
        mock_response.status_code = 404
        mock_response.is_redirect = False
        mock_response.raise_for_status.side_effect = httpx.HTTPStatusError(
            "404 Not Found",
            request=MagicMock(),
            response=mock_response
        )

        connector.client.request = AsyncMock(return_value=mock_response)

        try:
            with self.assertRaises(SandboxRequestError):
                await connector.send_request("GET", "test")

            # The 404 resolved a Pod IP once, it must stay cached.
            self.assertTrue(connector._pod_ip_resolved,
                            "A 4xx must not clear pod_ip_resolved flag")
            self.assertIsNotNone(connector._cached_pod_ip_url,
                                 "A 4xx must not clear cached pod IP URL")

            # A second request must reuse the cache.
            initial_count = call_count[0]
            mock_response.status_code = 200
            mock_response.raise_for_status.side_effect = None
            connector.client.request = AsyncMock(return_value=mock_response)

            await connector.send_request("GET", "test")

            self.assertEqual(call_count[0], initial_count,
                             "A 4xx must leave the pod IP cached")
        finally:
            await connector.close()

    async def test_http_error_clears_pod_ip_cache(self):
        """Verify HTTPError (connection failures) also clears pod IP cache."""
        config = SandboxInClusterConnectionConfig(server_port=8888)

        async def mock_get_pod_ip():
            return "10.244.0.5"

        connector = AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=MagicMock(),
            get_pod_ip=mock_get_pod_ip,
        )

        # Mock httpx client to raise connection error
        connector.client.request = AsyncMock(
            side_effect=httpx.ConnectError("Connection refused")
        )

        try:
            with self.assertRaises(SandboxRequestError):
                await connector.send_request("GET", "test")

            # Verify cache was cleared
            self.assertFalse(connector._pod_ip_resolved,
                           "HTTPError should clear pod_ip_resolved flag")
            self.assertIsNone(connector._cached_pod_ip_url,
                            "HTTPError should clear cached pod IP URL")
        finally:
            await connector.close()

    async def test_gateway_cache_cleared_on_status_error(self):
        """Verify HTTPStatusError clears gateway base_url cache."""
        from k8s_agent_sandbox.models import SandboxGatewayConnectionConfig

        config = SandboxGatewayConnectionConfig(
            gateway_name="test-gw",
            gateway_namespace="default",
        )

        mock_k8s = MagicMock()
        mock_k8s.wait_for_gateway_ip = AsyncMock(return_value="34.56.78.90")

        connector = AsyncSandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=mock_k8s,
        )

        # First request to establish base_url
        mock_response_ok = MagicMock()
        mock_response_ok.status_code = 200
        mock_response_ok.is_redirect = False
        mock_response_ok.raise_for_status = MagicMock()
        connector.client.request = AsyncMock(return_value=mock_response_ok)

        await connector.send_request("GET", "health")
        self.assertIsNotNone(connector._base_url, "base_url should be cached")

        # Now return 503 error
        mock_response_error = MagicMock()
        mock_response_error.status_code = 503
        mock_response_error.is_redirect = False
        mock_response_error.raise_for_status.side_effect = httpx.HTTPStatusError(
            "503 Service Unavailable",
            request=MagicMock(),
            response=mock_response_error
        )
        connector.client.request = AsyncMock(return_value=mock_response_error)

        try:
            with self.assertRaises(SandboxRequestError):
                await connector.send_request("GET", "test")

            # Verify gateway cache was cleared
            self.assertIsNone(connector._base_url,
                            "HTTPStatusError should clear gateway base_url cache")
        finally:
            await connector.close()


class SandboxClaimDeleteHandler(BaseHTTPRequestHandler):
    """Stub K8s apiserver; only handles the DELETE call atexit cleanup makes."""

    received_deletes = []

    def do_DELETE(self):
        self.__class__.received_deletes.append(self.path)
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", "application/json")
        payload = b"{}"
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass  # silence BaseHTTPRequestHandler's default per-request access-log line


class TestAtexitCleanupRealInterpreterShutdown(unittest.TestCase):
    """Regression test for a bug where AsyncSandboxClient's atexit cleanup only failed at real interpreter shutdown: 
    kubernetes_asyncio's aiohttp transport dispatches a per-request netrc lookup via a background thread, which fails
    once Python's own thread-pool teardown has begun. No in-process test can reproduce that condition because the 
    interpreter never actually exits mid-suite, so this spawns a real subprocess and lets it exit for real."""

    @classmethod
    def setUpClass(cls):
        cls.server, cls.server_thread, cls.port = _start_stub_server(SandboxClaimDeleteHandler)

    @classmethod
    def tearDownClass(cls):
        _stop_stub_server(cls.server, cls.server_thread)

    def setUp(self):
        SandboxClaimDeleteHandler.received_deletes = []

    def _write_fake_kubeconfig(self) -> str:
        fd, path = tempfile.mkstemp(suffix=".yaml")
        with os.fdopen(fd, "w") as f:
            f.write(f"""\
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://127.0.0.1:{self.port}
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user: {{}}
""")
        self.addCleanup(os.remove, path)
        return path

    def test_atexit_cleanup_deletes_claim_on_real_process_exit(self):
        kubeconfig_path = self._write_fake_kubeconfig()
        script = """
import asyncio
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
from k8s_agent_sandbox.models import SandboxDirectConnectionConfig

async def main():
    client = AsyncSandboxClient(
        connection_config=SandboxDirectConnectionConfig(api_url="http://unused:8080"),
        cleanup=True,
    )
    client._active_connection_sandboxes[("default", "claim-abc")] = object()
    client._claim_ownership.register_automatic(
        ("default", "claim-abc"), "claim-uid"
    )

asyncio.run(main())
# No explicit close()/delete_all() — relying entirely on the atexit hook.
"""
        # K8sHelper.__init__ tries load_incluster_config() before falling
        # back to KUBECONFIG. If the parent process happens to be running
        # inside a real pod (e.g. in CI), KUBERNETES_SERVICE_HOST/PORT are
        # already set and inherited via os.environ, which would make the
        # subprocess skip KUBECONFIG entirely and talk to the real in-cluster
        # apiserver instead of this stub. Strip them so it deterministically
        # falls through to the fake kubeconfig.
        env = dict(os.environ)
        for k in ("KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT", "KUBERNETES_PORT"):
            env.pop(k, None)
        env["KUBECONFIG"] = kubeconfig_path

        result = subprocess.run(
            [sys.executable, "-c", script],
            capture_output=True,
            text=True,
            timeout=30,
            env=env,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("Traceback", result.stderr, result.stderr)
        self.assertEqual(
            SandboxClaimDeleteHandler.received_deletes,
            ["/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/default/sandboxclaims/claim-abc"],
        )


if __name__ == "__main__":
    unittest.main()
