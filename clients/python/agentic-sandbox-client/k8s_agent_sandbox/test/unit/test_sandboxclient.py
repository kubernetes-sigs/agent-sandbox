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
import threading
import unittest
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import HTTPServer, BaseHTTPRequestHandler
from unittest.mock import MagicMock, patch, ANY

from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry

from kubernetes import config as k8s_config
from kubernetes.client import ApiException
from k8s_agent_sandbox.sandbox import Sandbox
from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.pod_metadata import validate_labels
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxInClusterConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
)
from k8s_agent_sandbox.exceptions import (
    SandboxPortForwardError,
    SandboxRequestError,
    SandboxNotFoundError,
)
from k8s_agent_sandbox.k8s_helper import K8sHelper
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


class TestSandboxClient(unittest.TestCase):

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def setUp(self, MockK8sHelper):
        self.client = SandboxClient()
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class
        default_claim_response = object()
        claim_getter = self.mock_k8s_helper.get_sandbox_claim
        claim_getter.return_value = default_claim_response

        def get_claim(claim_name, namespace):
            configured_response = claim_getter.return_value
            if configured_response is not default_claim_response:
                return configured_response
            return claim_for_request(
                claim_name=claim_name, namespace=namespace
            )

        claim_getter.side_effect = get_claim

    @patch('uuid.uuid4')
    def test_create_sandbox_success(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, '_create_claim') as mock_create_claim:
            # The create response's resourceVersion seeds the ready-wait
            # watch so it is served from the apiserver watch cache.
            mock_create_claim.return_value = {
                "metadata": {
                    "resourceVersion": "12345",
                    "uid": "claim-uid",
                }
            }

            sandbox = self.client.create_sandbox("test-warmpool", "test-namespace")

            mock_create_claim.assert_called_once_with(
                "sandbox-claim-1234abcd",
                "test-warmpool",
                "test-namespace",
                labels=None,
                lifecycle=None,
                volume_claim_templates=None,
                pod_metadata=None,
                env=None,
            )
            # A single claim watch resolves the sandbox name AND readiness;
            # no separate wait on the Sandbox resource. The watch starts at
            # the created claim's resourceVersion.
            self.mock_k8s_helper.wait_for_claim_ready.assert_called_once_with(
                "sandbox-claim-1234abcd",
                "test-namespace",
                180,
                resource_version="12345",
                claim_validator=None,
            )
            self.mock_k8s_helper.wait_for_sandbox_ready.assert_not_called()
            self.assertEqual(sandbox, mock_sandbox_instance)
            
            # Verify the new sandbox is tracked in the registry
            self.assertEqual(len(self.client._active_connection_sandboxes), 1)
            self.assertEqual(self.client._active_connection_sandboxes[("test-namespace", "sandbox-claim-1234abcd")], mock_sandbox_instance)
            self.assertEqual(
                self.client._automatic_cleanup_claims,
                {("test-namespace", "sandbox-claim-1234abcd")},
            )

    @patch('uuid.uuid4')
    def test_create_sandbox_failure_cleanup(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.wait_for_claim_ready.side_effect = Exception("Timeout Error")

        with patch.object(self.client, '_create_claim') as mock_create_claim:
            mock_create_claim.return_value = claim_for_request(
                claim_name="sandbox-claim-1234abcd"
            )
            with self.assertRaises(Exception) as context:
                self.client.create_sandbox("test-warmpool", "test-namespace")
                
            self.assertEqual(str(context.exception), "Timeout Error")
            # Ensure delete_sandbox_claim is called to cleanup orphan claim on failure
            self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
                "sandbox-claim-1234abcd",
                "test-namespace",
                expected_uid="claim-uid",
            )

    @patch("uuid.uuid4")
    def test_failed_creation_preserves_original_error_when_close_fails(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        created_claim = claim_for_request(claim_name=claim_name)
        created_claim["metadata"]["uid"] = "created-uid"
        self.mock_k8s_helper.create_sandbox_claim.return_value = created_claim
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"
        candidate = MagicMock()
        candidate.close_connection.side_effect = RuntimeError("close failed")
        self.mock_sandbox_class.return_value = candidate

        with patch.object(
            self.client,
            "_register_created_handle",
            side_effect=RuntimeError("registration failed"),
        ):
            with self.assertRaisesRegex(RuntimeError, "registration failed"):
                self.client.create_sandbox(WARMPOOL, NAMESPACE)

        candidate.close_connection.assert_called_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            claim_name, NAMESPACE, expected_uid="created-uid"
        )

    def test_create_sandbox_uses_explicit_claim_name(self):
        self.mock_k8s_helper.create_sandbox_claim.return_value = claim_for_request(
            claim_name="sandbox-workflow-123",
            namespace="test-namespace",
            warmpool="test-warmpool",
        )
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"

        self.client.create_sandbox(
            "test-warmpool",
            "test-namespace",
            claim_name="sandbox-workflow-123",
        )

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
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

    def test_explicit_claim_replaces_stale_generated_ownership(self):
        key = ("test-namespace", "sandbox-workflow-123")
        stale_handle = MagicMock()
        stale_handle.is_active = False
        self.client._active_connection_sandboxes[key] = stale_handle
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.create_sandbox_claim.return_value = claim_for_request(
            claim_name="sandbox-workflow-123",
            namespace="test-namespace",
            warmpool="test-warmpool",
        )
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"

        sandbox = self.client.create_sandbox(
            "test-warmpool",
            "test-namespace",
            claim_name="sandbox-workflow-123",
        )

        self.assertIs(self.client._active_connection_sandboxes[key], sandbox)
        stale_handle.close_connection.assert_called_once_with()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    def test_adoption_replaces_active_handle_for_recreated_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        active_handle = MagicMock()
        active_handle.claim_name = CLAIM_NAME
        active_handle.is_active = True
        active_handle.sandbox_id = "stable-sandbox"
        candidate_handle = MagicMock()
        candidate_handle.sandbox_id = "stable-sandbox"
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
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = recreated_claim
        self.mock_sandbox_class.return_value = candidate_handle

        sandbox = self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
        )

        self.assertIs(sandbox, candidate_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], candidate_handle)
        active_handle.close_connection.assert_called_once_with()
        self.assertIsNone(active_handle.claim_name)
        candidate_handle.close_connection.assert_not_called()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    def test_stale_adoption_cannot_replace_concurrent_claim_incarnation(self):
        key = (NAMESPACE, CLAIM_NAME)
        initial_handle = MagicMock(is_active=True, sandbox_id="initial-sandbox")
        old_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        old_handle.claim_name = CLAIM_NAME
        new_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        self.client._active_connection_sandboxes[key] = initial_handle
        self.client._active_claim_uids[key] = "initial-uid"
        old_claim = claim_for_request(resource_version="old-rv")
        old_claim["metadata"]["uid"] = "old-uid"
        new_claim = claim_for_request(resource_version="new-rv")
        new_claim["metadata"]["uid"] = "new-uid"
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.side_effect = [old_claim, new_claim]
        old_wait_started = threading.Event()
        release_old_wait = threading.Event()

        def wait_for_ready(*_args, resource_version, **_kwargs):
            if resource_version == "old-rv":
                old_wait_started.set()
                release_old_wait.wait(timeout=5)
            return "stable-sandbox"

        handles = iter((new_handle, old_handle))

        def build_handle(*_args, **_kwargs):
            return next(handles)

        stale_errors = []

        def adopt_old_claim():
            try:
                self.client.create_sandbox(
                    WARMPOOL,
                    NAMESPACE,
                    claim_name=CLAIM_NAME,
                    adopt_existing=True,
                )
            except Exception as error:
                stale_errors.append(error)

        self.mock_k8s_helper.wait_for_claim_ready.side_effect = wait_for_ready
        self.mock_sandbox_class.side_effect = build_handle
        stale_thread = threading.Thread(target=adopt_old_claim)
        stale_thread.start()
        self.assertTrue(old_wait_started.wait(timeout=5))
        try:
            current = self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
            )
        finally:
            release_old_wait.set()
            stale_thread.join(timeout=5)

        self.assertFalse(stale_thread.is_alive())
        self.assertIs(current, new_handle)
        self.assertEqual(len(stale_errors), 1)
        self.assertIsInstance(stale_errors[0], RuntimeError)
        self.assertIn("changed concurrently", str(stale_errors[0]))
        self.assertIs(self.client._active_connection_sandboxes[key], new_handle)
        self.assertEqual(self.client._active_claim_uids[key], "new-uid")
        old_handle.close_connection.assert_called_once_with()
        self.assertIsNone(old_handle.claim_name)
        new_handle.close_connection.assert_not_called()

    def test_adopt_existing_requires_explicit_claim_name(self):
        with self.assertRaisesRegex(ValueError, "explicit claim_name"):
            self.client.create_sandbox("test-warmpool", adopt_existing=True)

        self.mock_k8s_helper.create_sandbox_claim.assert_not_called()

    def test_create_sandbox_rejects_invalid_explicit_claim_names(self):
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
                    self.client.create_sandbox(
                        "test-warmpool", claim_name=claim_name
                    )

        self.mock_k8s_helper.create_sandbox_claim.assert_not_called()

    def test_create_sandbox_accepts_native_valid_long_claim_names(self):
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"

        for claim_name in ("a" * 64, "a" * 253):
            with self.subTest(length=len(claim_name)):
                self.mock_k8s_helper.create_sandbox_claim.return_value = (
                    claim_for_request(claim_name=claim_name)
                )

                self.client.create_sandbox(
                    WARMPOOL, NAMESPACE, claim_name=claim_name
                )

                self.assertIn((NAMESPACE, claim_name), self.client._active_connection_sandboxes)

    def test_create_sandbox_adopts_existing_claim_after_conflict(self):
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = {
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
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"

        sandbox = self.client.create_sandbox(
            "test-warmpool",
            "test-namespace",
            claim_name="sandbox-workflow-123",
            adopt_existing=True,
        )

        self.mock_k8s_helper.get_sandbox_claim.assert_called_once_with(
            "sandbox-workflow-123", "test-namespace"
        )
        self.mock_k8s_helper.wait_for_claim_ready.assert_called_once_with(
            "sandbox-workflow-123",
            "test-namespace",
            180,
            resource_version="existing-rv",
            claim_validator=ANY,
        )
        self.assertEqual(sandbox, self.mock_sandbox_class.return_value)

    def test_create_sandbox_reports_claim_deleted_after_adoption_conflict(self):
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = None

        with self.assertRaisesRegex(
            SandboxNotFoundError, "disappeared after the create conflict"
        ):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
            )

        self.mock_k8s_helper.wait_for_claim_ready.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_create_sandbox_propagates_conflict_without_adoption(self):
        conflict = ApiException(status=409)
        self.mock_k8s_helper.create_sandbox_claim.side_effect = conflict

        with self.assertRaises(ApiException) as context:
            self.client.create_sandbox(
                "test-warmpool",
                claim_name="sandbox-workflow-123",
            )

        self.assertIs(context.exception, conflict)
        self.mock_k8s_helper.get_sandbox_claim.assert_not_called()

    def test_create_sandbox_rejects_mismatched_existing_claims(self):
        for existing_claim, error_field in mismatched_claims():
            with self.subTest(error_field=error_field):
                self.mock_k8s_helper.reset_mock()
                self.mock_k8s_helper.create_sandbox_claim.side_effect = (
                    ApiException(status=409)
                )
                self.mock_k8s_helper.get_sandbox_claim.return_value = (
                    existing_claim
                )

                with self.assertRaisesRegex(ValueError, error_field):
                    self.client.create_sandbox(
                        WARMPOOL,
                        NAMESPACE,
                        labels=REQUESTED_LABELS,
                        claim_name=CLAIM_NAME,
                        adopt_existing=True,
                        volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                        pod_labels=POD_LABELS,
                        pod_annotations=POD_ANNOTATIONS,
                    )

                self.mock_k8s_helper.wait_for_claim_ready.assert_not_called()
                self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_adopt_existing_rejects_relative_shutdown_time(self):
        with self.assertRaisesRegex(ValueError, "shutdown_after_seconds"):
            self.client.create_sandbox(
                "test-warmpool",
                claim_name="sandbox-workflow-123",
                adopt_existing=True,
                shutdown_after_seconds=60,
            )

        self.mock_k8s_helper.create_sandbox_claim.assert_not_called()

    def test_create_sandbox_returns_already_ready_adopted_claim(self):
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
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = existing_claim

        sandbox = self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
        )

        self.mock_k8s_helper.wait_for_claim_ready.assert_not_called()
        self.assertEqual(sandbox, self.mock_sandbox_class.return_value)
        self.assertEqual(
            self.mock_sandbox_class.call_args.kwargs["sandbox_id"],
            "ready-sandbox",
        )

    def test_create_sandbox_accepts_server_defaulted_spec_fields(self):
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
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = existing_claim

        sandbox = self.client.create_sandbox(
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
        self.mock_k8s_helper.wait_for_claim_ready.assert_not_called()

    def test_create_sandbox_ignores_stale_ready_adopted_snapshot(self):
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
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = existing_claim
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "current-sandbox"

        self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            adopt_existing=True,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
        )

        self.mock_k8s_helper.wait_for_claim_ready.assert_called_once()
        self.assertEqual(
            self.mock_sandbox_class.call_args.kwargs["sandbox_id"],
            "current-sandbox",
        )

    def test_create_sandbox_revalidates_adopted_watch_events(self):
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = matching_claim()

        def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            self.assertIsNotNone(claim_validator)
            changed_claim = matching_claim()
            changed_claim["spec"]["warmPoolRef"] = {"name": "other-pool"}
            claim_validator(changed_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready.side_effect = wait_for_ready

        with self.assertRaisesRegex(ValueError, "spec.warmPoolRef"):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_successful_explicit_create_revalidates_watch_contract(self):
        self.mock_k8s_helper.create_sandbox_claim.return_value = matching_claim()

        def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            self.assertIsNotNone(claim_validator)
            changed_claim = matching_claim()
            changed_claim["spec"]["warmPoolRef"] = {"name": "other-pool"}
            claim_validator(changed_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready.side_effect = wait_for_ready

        with self.assertRaisesRegex(ValueError, "spec.warmPoolRef"):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_successful_explicit_create_rejects_recreated_claim_during_watch(self):
        self.mock_k8s_helper.create_sandbox_claim.return_value = matching_claim()

        def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            self.assertIsNotNone(claim_validator)
            recreated_claim = matching_claim()
            recreated_claim["metadata"]["uid"] = "replacement-uid"
            claim_validator(recreated_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready.side_effect = wait_for_ready

        with self.assertRaisesRegex(ValueError, "metadata.uid"):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_successful_explicit_create_rejects_recreated_claim_after_410(
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
        real_helper = K8sHelper.__new__(K8sHelper)
        real_helper.custom_objects_api = MagicMock()
        first_watch = MagicMock()
        first_watch.stream.side_effect = ApiException(status=410)
        second_watch = MagicMock()
        second_watch.stream.return_value = iter(
            [{"type": "ADDED", "object": recreated_claim}]
        )
        mock_watch_class.side_effect = [first_watch, second_watch]
        self.client.k8s_helper = real_helper

        with patch.object(
            self.client, "_create_claim", return_value=created_claim
        ), patch.object(self.client, "_delete_claim") as delete_claim:
            with self.assertRaisesRegex(ValueError, "metadata.uid"):
                self.client.create_sandbox(
                    WARMPOOL,
                    NAMESPACE,
                    labels=REQUESTED_LABELS,
                    claim_name=CLAIM_NAME,
                    adopt_existing=True,
                    volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                    pod_labels=POD_LABELS,
                    pod_annotations=POD_ANNOTATIONS,
                )

        first_call, second_call = (
            first_watch.stream.call_args,
            second_watch.stream.call_args,
        )
        self.assertEqual(first_call.kwargs["resource_version"], "existing-rv")
        self.assertEqual(second_call.kwargs["resource_version"], "0")
        delete_claim.assert_not_called()
        self.mock_sandbox_class.assert_not_called()

    def test_create_sandbox_rejects_recreated_claim_during_watch(self):
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = matching_claim()

        def wait_for_ready(*_args, claim_validator=None, **_kwargs):
            recreated_claim = matching_claim()
            recreated_claim["metadata"]["uid"] = "replacement-uid"
            claim_validator(recreated_claim)
            return "unreachable"

        self.mock_k8s_helper.wait_for_claim_ready.side_effect = wait_for_ready

        with self.assertRaisesRegex(ValueError, "metadata.uid"):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_create_sandbox_propagates_adopted_terminal_conditions(self):
        for existing_claim, error_type, reason, error_pattern in terminal_claims():
            with self.subTest(reason=reason):
                self.mock_k8s_helper.reset_mock()
                self.mock_k8s_helper.create_sandbox_claim.side_effect = (
                    ApiException(status=409)
                )
                self.mock_k8s_helper.get_sandbox_claim.return_value = (
                    existing_claim
                )

                with self.assertRaisesRegex(error_type, error_pattern):
                    self.client.create_sandbox(
                        WARMPOOL,
                        NAMESPACE,
                        labels=REQUESTED_LABELS,
                        claim_name=CLAIM_NAME,
                        adopt_existing=True,
                        volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                        pod_labels=POD_LABELS,
                        pod_annotations=POD_ANNOTATIONS,
                    )

                self.mock_k8s_helper.wait_for_claim_ready.assert_not_called()
                self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_create_sandbox_adopts_matching_full_contract(self):
        existing_claim = matching_claim(env=REQUESTED_ENV)
        existing_claim["status"] = {
            "conditions": [{"type": "Ready", "status": "True"}],
            "sandbox": {"name": 42},
        }
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = existing_claim
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"

        self.client.create_sandbox(
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

        self.mock_k8s_helper.wait_for_claim_ready.assert_called_once_with(
            CLAIM_NAME,
            NAMESPACE,
            180,
            resource_version="existing-rv",
            claim_validator=ANY,
        )

    def test_create_sandbox_rejects_mismatched_existing_env(self):
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = matching_claim(
            env={"FOO": "other"}
        )

        with self.assertRaisesRegex(ValueError, "spec.env"):
            self.client.create_sandbox(
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

        self.mock_k8s_helper.wait_for_claim_ready.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_explicit_claim_is_not_deleted_when_wait_fails(self):
        key = (NAMESPACE, CLAIM_NAME)
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.create_sandbox_claim.return_value = claim_for_request()
        self.mock_k8s_helper.wait_for_claim_ready.side_effect = TimeoutError(
            "timed out"
        )

        with self.assertRaisesRegex(TimeoutError, "timed out"):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    def test_adopted_claim_is_not_deleted_when_wait_fails(self):
        key = (NAMESPACE, CLAIM_NAME)
        self.client._automatic_cleanup_claims.add(key)
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = matching_claim()
        self.mock_k8s_helper.wait_for_claim_ready.side_effect = TimeoutError(
            "timed out"
        )

        with self.assertRaisesRegex(TimeoutError, "timed out"):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                labels=REQUESTED_LABELS,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
                volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
                pod_labels=POD_LABELS,
                pod_annotations=POD_ANNOTATIONS,
            )

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    def test_explicit_claim_is_not_deleted_by_automatic_cleanup(self):
        self.mock_k8s_helper.create_sandbox_claim.return_value = claim_for_request()
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"

        sandbox = self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            claim_name=CLAIM_NAME,
        )
        self.client._delete_automatic_cleanup_claims()

        sandbox.terminate.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_explicit_create_failure_releases_stale_automatic_ownership(self):
        key = (NAMESPACE, CLAIM_NAME)
        self.client._automatic_cleanup_claims.add(key)
        server_error = ApiException(status=500)
        self.mock_k8s_helper.create_sandbox_claim.side_effect = server_error

        with self.assertRaises(ApiException) as context:
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
                adopt_existing=True,
            )

        self.assertIs(context.exception, server_error)
        self.mock_k8s_helper.get_sandbox_claim.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    @patch("uuid.uuid4")
    def test_generated_claim_without_uid_is_not_deleted_by_automatic_cleanup(
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
        self.mock_k8s_helper.create_sandbox_claim.return_value = created_claim
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "resolved-id"
        generated_handle = MagicMock(claim_name=claim_name)
        self.mock_sandbox_class.return_value = generated_handle

        self.client.create_sandbox(WARMPOOL, NAMESPACE)
        self.client._delete_automatic_cleanup_claims()

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertNotIn(key, self.client._automatic_cleanup_claim_uids)

    def test_automatic_cleanup_never_deletes_without_observed_uid(self):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(claim_name=CLAIM_NAME)
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._automatic_cleanup_claims.add(key)

        self.client._delete_automatic_cleanup_claims()

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    @patch("uuid.uuid4")
    def test_generated_claim_without_uid_is_not_deleted_after_ambiguous_failure(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        server_error = ApiException(status=500)
        self.mock_k8s_helper.create_sandbox_claim.side_effect = server_error

        with self.assertRaises(ApiException) as context:
            self.client.create_sandbox(WARMPOOL, NAMESPACE)

        self.assertIs(context.exception, server_error)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    @patch("uuid.uuid4")
    def test_generated_claim_conflict_does_not_delete_existing_claim(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        conflict = ApiException(status=409)
        self.mock_k8s_helper.create_sandbox_claim.side_effect = conflict

        with self.assertRaises(ApiException) as context:
            self.client.create_sandbox(WARMPOOL, NAMESPACE)

        self.assertIs(context.exception, conflict)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_rejected_explicit_claim_name_remains_caller_owned(self):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(is_active=True, sandbox_id="generated")
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._automatic_cleanup_claims.add(key)
        conflict = ApiException(status=409)
        self.mock_k8s_helper.create_sandbox_claim.side_effect = conflict

        with self.assertRaises(ApiException) as context:
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=CLAIM_NAME,
            )

        self.assertIs(context.exception, conflict)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertIn(key, self.client._caller_owned_claims)

    @patch("uuid.uuid4")
    def test_explicit_attempt_stops_cleanup_of_previously_generated_name(
        self, mock_uuid
    ):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        key = (NAMESPACE, claim_name)
        generated_handle = MagicMock(is_active=True, sandbox_id="old-sandbox")
        created_claim = claim_for_request(claim_name=claim_name)
        created_claim["metadata"]["uid"] = "original-uid"
        self.mock_k8s_helper.create_sandbox_claim.return_value = created_claim
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "old-sandbox"
        self.mock_sandbox_class.return_value = generated_handle
        self.client.create_sandbox(WARMPOOL, NAMESPACE)

        replacement = claim_for_request(claim_name=claim_name)
        replacement["metadata"]["uid"] = "replacement-uid"
        replacement["spec"]["warmPoolRef"]["name"] = "other-pool"
        self.mock_k8s_helper.create_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.mock_k8s_helper.get_sandbox_claim.return_value = replacement

        with self.assertRaisesRegex(ValueError, "warmPoolRef"):
            self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=claim_name,
                adopt_existing=True,
            )

        self.client._delete_automatic_cleanup_claims()

        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        generated_handle.terminate.assert_not_called()
        generated_handle.close_connection.assert_not_called()
        self.assertIs(
            self.client._active_connection_sandboxes[key], generated_handle
        )
        self.assertIn(key, self.client._caller_owned_claims)

    def test_automatic_cleanup_skips_caller_protected_generated_registration(self):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(is_active=True, sandbox_id="generated")
        self.client._active_connection_sandboxes[key] = generated_handle
        with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)
            self.client._claim_ownership.register_automatic(key, "generated-uid")

        self.client._delete_automatic_cleanup_claims()

        generated_handle.terminate.assert_not_called()
        generated_handle.close_connection.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertIs(self.client._active_connection_sandboxes[key], generated_handle)

    def test_deliberate_delete_clears_caller_ownership(self):
        key = (NAMESPACE, CLAIM_NAME)
        generated_handle = MagicMock(
            is_active=True,
            sandbox_id="generated",
            claim_name=CLAIM_NAME,
        )
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._active_claim_uids[key] = "generated-uid"
        self.client._claim_ownership.register_automatic(key, "generated-uid")
        with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        self.client.delete_sandbox(CLAIM_NAME, NAMESPACE)

        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertNotIn(key, self.client._caller_owned_claims)
        generated_handle.terminate.assert_not_called()
        generated_handle.close_connection.assert_called_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="generated-uid"
        )

    @patch("uuid.uuid4")
    def test_late_generated_claim_keeps_explicit_ownership(self, mock_uuid):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        key = (NAMESPACE, claim_name)
        generated_wait_started = threading.Event()
        release_generated_wait = threading.Event()
        generated_handle = MagicMock()
        generated_handle.is_active = True
        explicit_handle = MagicMock()
        explicit_handle.is_active = True
        generated_results = []
        generated_errors = []

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
        self.mock_k8s_helper.create_sandbox_claim.side_effect = [
            created_claim,
            ApiException(status=409),
        ]
        self.mock_k8s_helper.get_sandbox_claim.return_value = existing_claim

        def wait_for_ready(*_args, **_kwargs):
            generated_wait_started.set()
            release_generated_wait.wait(timeout=5)
            return "generated-sandbox"

        def build_handle(*_args, sandbox_id, **_kwargs):
            if sandbox_id == "explicit-sandbox":
                return explicit_handle
            return generated_handle

        def create_generated_claim():
            try:
                generated_results.append(
                    self.client.create_sandbox(WARMPOOL, NAMESPACE)
                )
            except Exception as error:
                generated_errors.append(error)

        self.mock_k8s_helper.wait_for_claim_ready.side_effect = wait_for_ready
        self.mock_sandbox_class.side_effect = build_handle
        generated_thread = threading.Thread(target=create_generated_claim)
        generated_thread.start()
        self.assertTrue(generated_wait_started.wait(timeout=5))
        try:
            explicit_result = self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=claim_name,
                adopt_existing=True,
            )
        finally:
            release_generated_wait.set()
            generated_thread.join(timeout=5)

        self.assertFalse(generated_thread.is_alive())
        self.assertEqual(len(generated_errors), 1)
        self.assertIsInstance(generated_errors[0], RuntimeError)
        self.assertIn("changed concurrently", str(generated_errors[0]))
        self.assertIs(explicit_result, explicit_handle)
        self.assertEqual(generated_results, [])
        self.assertIs(self.client._active_connection_sandboxes[key], explicit_handle)
        generated_handle.close_connection.assert_called_once_with()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    @patch("uuid.uuid4")
    def test_late_generated_failure_cannot_delete_explicit_claim(self, mock_uuid):
        mock_uuid.return_value.hex = "1234abcd"
        claim_name = "sandbox-claim-1234abcd"
        key = (NAMESPACE, claim_name)
        generated_wait_started = threading.Event()
        release_generated_wait = threading.Event()
        explicit_handle = MagicMock()
        generated_errors = []

        existing_claim = claim_for_request(
            claim_name=claim_name, resource_version="existing-rv"
        )
        existing_claim["status"] = {
            "conditions": [
                {"type": "Ready", "status": "True", "observedGeneration": 1}
            ],
            "sandbox": {"name": "explicit-sandbox"},
        }
        self.mock_k8s_helper.create_sandbox_claim.side_effect = [
            claim_for_request(claim_name=claim_name),
            ApiException(status=409),
        ]
        self.mock_k8s_helper.get_sandbox_claim.return_value = existing_claim

        def wait_for_ready(*_args, **_kwargs):
            generated_wait_started.set()
            release_generated_wait.wait(timeout=5)
            raise TimeoutError("generated wait timed out")

        def create_generated_claim():
            try:
                self.client.create_sandbox(WARMPOOL, NAMESPACE)
            except Exception as error:
                generated_errors.append(error)

        self.mock_k8s_helper.wait_for_claim_ready.side_effect = wait_for_ready
        self.mock_sandbox_class.return_value = explicit_handle
        generated_thread = threading.Thread(target=create_generated_claim)
        generated_thread.start()
        self.assertTrue(generated_wait_started.wait(timeout=5))
        try:
            explicit_result = self.client.create_sandbox(
                WARMPOOL,
                NAMESPACE,
                claim_name=claim_name,
                adopt_existing=True,
            )
        finally:
            release_generated_wait.set()
            generated_thread.join(timeout=5)

        self.assertFalse(generated_thread.is_alive())
        self.assertEqual(len(generated_errors), 1)
        self.assertIsInstance(generated_errors[0], TimeoutError)
        self.assertIs(explicit_result, explicit_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], explicit_handle)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    def test_get_sandbox_existing_active(self):
        key = ("test-namespace", "test-claim")
        mock_sandbox = MagicMock()
        mock_sandbox.is_active = True
        mock_sandbox.sandbox_id = "resolved-id"
        self.client._active_connection_sandboxes[key] = mock_sandbox
        self.client._active_claim_uids[key] = "claim-uid"
        
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}

        sandbox = self.client.get_sandbox("test-claim", "test-namespace")
        
        self.assertEqual(sandbox, mock_sandbox)
        self.mock_sandbox_class.assert_not_called()

    def test_get_sandbox_replaces_active_handle_for_recreated_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        old_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        old_handle.claim_name = CLAIM_NAME
        new_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        self.client._active_connection_sandboxes[key] = old_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "stable-sandbox"
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}
        self.mock_sandbox_class.return_value = new_handle

        sandbox = self.client.get_sandbox(CLAIM_NAME, NAMESPACE)

        self.assertIs(sandbox, new_handle)
        self.assertIs(self.client._active_connection_sandboxes[key], new_handle)
        old_handle.close_connection.assert_called_once_with()
        self.assertIsNone(old_handle.claim_name)
        self.assertEqual(self.client._active_claim_uids[key], "claim-uid")

    def test_get_sandbox_stale_lookup_cannot_return_concurrent_handle(self):
        key = (NAMESPACE, CLAIM_NAME)
        initial_handle = MagicMock(is_active=True, sandbox_id="initial-sandbox")
        initial_handle.claim_name = CLAIM_NAME
        concurrent_handle = MagicMock(is_active=True, sandbox_id="stable-sandbox")
        self.client._active_connection_sandboxes[key] = initial_handle
        self.client._active_claim_uids[key] = "claim-uid"
        self.client._claim_ownership.register_automatic(key, "claim-uid")
        lookup_started = threading.Event()
        release_lookup = threading.Event()
        lookup_errors = []

        def resolve_sandbox(*_args, **_kwargs):
            lookup_started.set()
            release_lookup.wait(timeout=5)
            return "stable-sandbox"

        def run_lookup():
            try:
                self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
            except RuntimeError as error:
                lookup_errors.append(error)

        self.mock_k8s_helper.resolve_sandbox_name.side_effect = resolve_sandbox
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}
        lookup_thread = threading.Thread(target=run_lookup)
        lookup_thread.start()
        self.assertTrue(lookup_started.wait(timeout=5))
        self.client._active_connection_sandboxes[key] = concurrent_handle
        self.client._active_claim_uids[key] = "replacement-uid"
        self.client._claim_ownership.register_automatic(key, "replacement-uid")
        release_lookup.set()
        lookup_thread.join(timeout=5)

        self.assertFalse(lookup_thread.is_alive())
        self.assertEqual(len(lookup_errors), 1)
        self.assertIn("changed concurrently", str(lookup_errors[0]))
        self.assertIs(
            self.client._active_connection_sandboxes[key], concurrent_handle
        )
        initial_handle.close_connection.assert_called_once_with()
        self.assertIsNone(initial_handle.claim_name)
        concurrent_handle.close_connection.assert_not_called()
        self.assertEqual(
            self.client._claim_ownership.automatic_cleanup_uid(key),
            "replacement-uid",
        )

    def test_stale_lookup_cannot_reuse_different_sandbox_binding(self):
        key = (NAMESPACE, CLAIM_NAME)
        expected_handle = MagicMock(
            is_active=True, sandbox_id="initial-sandbox"
        )
        expected_handle.claim_name = CLAIM_NAME
        current_handle = MagicMock(
            is_active=True, sandbox_id="old-binding"
        )
        self.client._active_connection_sandboxes[key] = expected_handle
        self.client._active_claim_uids[key] = "claim-uid"
        with self.client._lock:
            lookup_operation = self.client._claim_ownership.begin_lookup(key)
            self.client._active_connection_sandboxes[key] = current_handle

        try:
            with self.assertRaisesRegex(RuntimeError, "changed concurrently"):
                self.client._reuse_or_replace_resolved_handle(
                    key,
                    expected_handle,
                    lookup_operation,
                    CLAIM_NAME,
                    "new-binding",
                    NAMESPACE,
                    "claim-uid",
                )
        finally:
            with self.client._lock:
                self.client._claim_ownership.finish_lookup(
                    key, lookup_operation
                )

        self.assertIs(self.client._active_connection_sandboxes[key], current_handle)
        current_handle.close_connection.assert_not_called()

    def test_get_sandbox_cannot_restore_claim_deleted_during_lookup(self):
        key = (NAMESPACE, CLAIM_NAME)
        lookup_started = threading.Event()
        release_lookup = threading.Event()
        lookup_errors = []
        claim = claim_for_request()
        claim["metadata"]["uid"] = "deleted-uid"
        self.mock_k8s_helper.get_sandbox_claim.return_value = claim
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}

        def resolve_sandbox(*_args, **_kwargs):
            lookup_started.set()
            release_lookup.wait(timeout=5)
            return "deleted-sandbox"

        def run_lookup():
            try:
                self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
            except RuntimeError as error:
                lookup_errors.append(error)

        self.mock_k8s_helper.resolve_sandbox_name.side_effect = resolve_sandbox
        lookup_thread = threading.Thread(target=run_lookup)
        lookup_thread.start()
        self.assertTrue(lookup_started.wait(timeout=5))

        self.client.delete_sandbox(CLAIM_NAME, NAMESPACE)
        release_lookup.set()
        lookup_thread.join(timeout=5)

        self.assertFalse(lookup_thread.is_alive())
        self.assertEqual(len(lookup_errors), 1)
        self.assertIn("changed concurrently", str(lookup_errors[0]))
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.mock_sandbox_class.assert_not_called()
        self.assertEqual(self.client._claim_ownership._lookup_operations, {})

    def test_get_sandbox_inactive_reattaches(self):
        mock_inactive_sandbox = MagicMock()
        mock_inactive_sandbox.is_active = False
        self.client._active_connection_sandboxes[("test-namespace", "test-claim")] = mock_inactive_sandbox
        
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}
        
        mock_new_sandbox = MagicMock()
        self.mock_sandbox_class.return_value = mock_new_sandbox
        
        sandbox = self.client.get_sandbox("test-claim", "test-namespace")
        
        self.assertEqual(sandbox, mock_new_sandbox)
        self.assertEqual(self.client._active_connection_sandboxes[("test-namespace", "test-claim")], mock_new_sandbox)
        self.mock_k8s_helper.get_sandbox.assert_called_with("resolved-id", "test-namespace")
        self.mock_sandbox_class.assert_called_once()

    def test_get_sandbox_not_found_k8s_error(self):
        self.mock_k8s_helper.resolve_sandbox_name.side_effect = Exception("Not found")
        
        with self.assertRaises(RuntimeError) as context:
            self.client.get_sandbox("test-claim", "test-namespace")
            
        self.assertIn("Sandbox claim 'test-claim' not found or resolution failed in namespace 'test-namespace'", str(context.exception))

    def test_get_sandbox_failure_detaches_explicit_handle_without_deleting_claim(self):
        key = ("test-namespace", "test-claim")
        explicit_handle = MagicMock()
        self.client._active_connection_sandboxes[key] = explicit_handle
        self.mock_k8s_helper.resolve_sandbox_name.side_effect = Exception("transient")

        with self.assertRaises(SandboxNotFoundError):
            self.client.get_sandbox("test-claim", "test-namespace")

        explicit_handle.close_connection.assert_called_once_with()
        explicit_handle.terminate.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()
        self.assertNotIn(key, self.client._active_connection_sandboxes)

    def test_get_sandbox_failure_deletes_generated_claim_by_uid(self):
        key = ("test-namespace", "test-claim")
        generated_handle = MagicMock()
        generated_handle.claim_name = "test-claim"
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._claim_ownership.register_automatic(key, "generated-uid")
        self.mock_k8s_helper.resolve_sandbox_name.side_effect = Exception("missing")

        with self.assertRaises(SandboxNotFoundError):
            self.client.get_sandbox("test-claim", "test-namespace")

        generated_handle.terminate.assert_not_called()
        generated_handle.close_connection.assert_called_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            "test-claim", "test-namespace", expected_uid="generated-uid"
        )
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertIsNone(generated_handle.claim_name)

    def test_automatic_cleanup_deletes_reattached_claim_by_observed_uid(self):
        key = (NAMESPACE, CLAIM_NAME)
        reattached_handle = MagicMock()
        reattached_handle.claim_name = CLAIM_NAME
        self.mock_sandbox_class.return_value = reattached_handle
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = {"metadata": {}}
        claim = claim_for_request()
        claim["metadata"]["uid"] = "reattached-uid"
        self.mock_k8s_helper.get_sandbox_claim.return_value = claim

        self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
        self.client._delete_automatic_cleanup_claims()

        reattached_handle.terminate.assert_not_called()
        reattached_handle.close_connection.assert_called_once_with()
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="reattached-uid"
        )
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)
        self.assertIsNone(reattached_handle.claim_name)

    def test_retained_handle_cannot_delete_recreated_claim_after_cleanup(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = Sandbox.__new__(Sandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.mock_k8s_helper.delete_sandbox_claim.side_effect = ApiException(
            status=409
        )
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._claim_ownership.register_automatic(key, "original-uid")

        self.client._delete_automatic_cleanup_claims()
        retained_handle.terminate()

        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="original-uid"
        )
        self.assertIsNone(retained_handle.claim_name)
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertIn(key, self.client._automatic_cleanup_claims)

    def test_get_sandbox_rejects_claim_recreated_during_resolution(self):
        key = (NAMESPACE, CLAIM_NAME)
        original_handle = MagicMock()
        original_handle.claim_name = CLAIM_NAME
        self.client._active_connection_sandboxes[key] = original_handle
        self.client._claim_ownership.register_automatic(key, "original-uid")
        original = claim_for_request()
        original["metadata"]["uid"] = "original-uid"
        replacement = claim_for_request()
        replacement["metadata"]["uid"] = "replacement-uid"
        self.mock_k8s_helper.get_sandbox_claim.return_value = original
        self.mock_k8s_helper.delete_sandbox_claim.side_effect = ApiException(
            status=409
        )

        def resolve_sandbox(*_args, claim_validator, **_kwargs):
            claim_validator(replacement)
            return "replacement-sandbox"

        self.mock_k8s_helper.resolve_sandbox_name.side_effect = resolve_sandbox

        with self.assertRaisesRegex(SandboxNotFoundError, "metadata.uid"):
            self.client.get_sandbox(CLAIM_NAME, NAMESPACE)

        self.mock_k8s_helper.get_sandbox.assert_not_called()
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="original-uid"
        )
        self.assertIsNone(original_handle.claim_name)
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertIn(key, self.client._automatic_cleanup_claims)

    def test_get_sandbox_failure_cannot_remove_concurrently_explicit_handle(self):
        key = (NAMESPACE, CLAIM_NAME)
        lookup_started = threading.Event()
        release_lookup = threading.Event()
        lookup_errors = []
        generated_handle = MagicMock()
        adopted_handle = MagicMock()
        self.client._active_connection_sandboxes[key] = generated_handle
        self.client._automatic_cleanup_claims.add(key)

        def fail_lookup(*_args, **_kwargs):
            lookup_started.set()
            release_lookup.wait(timeout=5)
            raise RuntimeError("stale lookup failed")

        def run_lookup():
            try:
                self.client.get_sandbox(CLAIM_NAME, NAMESPACE)
            except SandboxNotFoundError as error:
                lookup_errors.append(error)

        self.mock_k8s_helper.resolve_sandbox_name.side_effect = fail_lookup
        lookup_thread = threading.Thread(target=run_lookup)
        lookup_thread.start()
        self.assertTrue(lookup_started.wait(timeout=5))

        self.mock_k8s_helper.create_sandbox_claim.return_value = matching_claim()
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "adopted-sandbox"
        self.mock_sandbox_class.return_value = adopted_handle
        self.client.create_sandbox(
            WARMPOOL,
            NAMESPACE,
            labels=REQUESTED_LABELS,
            claim_name=CLAIM_NAME,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_labels=POD_LABELS,
            pod_annotations=POD_ANNOTATIONS,
        )

        release_lookup.set()
        lookup_thread.join(timeout=5)

        self.assertFalse(lookup_thread.is_alive())
        self.assertEqual(len(lookup_errors), 1)
        generated_handle.terminate.assert_not_called()
        generated_handle.close_connection.assert_called_once_with()
        self.assertIs(self.client._active_connection_sandboxes[key], adopted_handle)
        self.assertNotIn(key, self.client._automatic_cleanup_claims)

    def test_slow_delete_does_not_block_create_for_another_claim(self):
        deleted_claim_name = "generated-claim"
        deleted_key = (NAMESPACE, deleted_claim_name)
        termination_started = threading.Event()
        release_termination = threading.Event()
        explicit_create_called = threading.Event()
        generated_handle = MagicMock()
        created_results = []

        def block_termination():
            termination_started.set()
            release_termination.wait(timeout=5)

        def create_claim(*_args, **_kwargs):
            explicit_create_called.set()
            return matching_claim()

        def create_explicit_claim():
            created_results.append(
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

        generated_handle.claim_name = deleted_claim_name
        generated_handle.close_connection.side_effect = block_termination
        self.client._active_connection_sandboxes[deleted_key] = generated_handle
        self.client._active_claim_uids[deleted_key] = "generated-uid"
        self.mock_k8s_helper.create_sandbox_claim.side_effect = create_claim
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "created-sandbox"

        delete_thread = threading.Thread(
            target=self.client.delete_sandbox,
            args=(deleted_claim_name, NAMESPACE),
        )
        create_thread = threading.Thread(target=create_explicit_claim)
        delete_thread.start()
        self.assertTrue(termination_started.wait(timeout=5))
        create_thread.start()
        try:
            create_started_without_waiting = explicit_create_called.wait(timeout=0.5)
        finally:
            release_termination.set()
            delete_thread.join(timeout=5)
            create_thread.join(timeout=5)

        self.assertTrue(create_started_without_waiting)
        self.assertFalse(delete_thread.is_alive())
        self.assertFalse(create_thread.is_alive())
        self.assertEqual(created_results, [self.mock_sandbox_class.return_value])

    def test_slow_automatic_cleanup_does_not_block_another_claim(self):
        cleanup_key = (NAMESPACE, "generated-claim")
        termination_started = threading.Event()
        release_termination = threading.Event()
        explicit_create_called = threading.Event()
        generated_handle = MagicMock()

        def block_close():
            termination_started.set()
            release_termination.wait(timeout=5)

        def create_claim(*_args, **_kwargs):
            explicit_create_called.set()
            return matching_claim()

        generated_handle.close_connection.side_effect = block_close
        self.client._active_connection_sandboxes[cleanup_key] = generated_handle
        self.client._claim_ownership.register_automatic(
            cleanup_key, "generated-uid"
        )
        self.mock_k8s_helper.create_sandbox_claim.side_effect = create_claim
        self.mock_k8s_helper.wait_for_claim_ready.return_value = "created-sandbox"

        cleanup_thread = threading.Thread(
            target=self.client._delete_automatic_cleanup_claims
        )
        cleanup_thread.start()
        self.assertTrue(termination_started.wait(timeout=5))
        create_thread = threading.Thread(
            target=self.client.create_sandbox,
            args=(WARMPOOL, NAMESPACE),
            kwargs={
                "labels": REQUESTED_LABELS,
                "claim_name": CLAIM_NAME,
                "volume_claim_templates": VOLUME_CLAIM_TEMPLATES,
                "pod_labels": POD_LABELS,
                "pod_annotations": POD_ANNOTATIONS,
            },
        )
        create_thread.start()
        try:
            create_started_without_waiting = explicit_create_called.wait(timeout=0.5)
        finally:
            release_termination.set()
            cleanup_thread.join(timeout=5)
            create_thread.join(timeout=5)

        self.assertTrue(create_started_without_waiting)
        self.assertFalse(cleanup_thread.is_alive())
        self.assertFalse(create_thread.is_alive())

    def test_get_sandbox_underlying_sandbox_not_found(self):
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"
        self.mock_k8s_helper.get_sandbox.return_value = None
        
        with self.assertRaises(RuntimeError) as context:
            self.client.get_sandbox("test-claim", "test-namespace")
            
        self.assertIn("Sandbox claim 'test-claim' not found or resolution failed in namespace 'test-namespace'", str(context.exception))
        self.assertIn("Underlying Sandbox 'resolved-id' not found.", str(context.exception))

    def test_list_active_sandboxes(self):
        mock_active = MagicMock()
        mock_active.is_active = True
        self.client._active_connection_sandboxes[("ns1", "active-claim")] = mock_active
        
        mock_inactive = MagicMock()
        mock_inactive.is_active = False
        self.client._active_connection_sandboxes[("ns2", "inactive-claim")] = mock_inactive
        
        active_list = self.client.list_active_sandboxes()
        
        self.assertEqual(active_list, [("ns1", "active-claim")])
        self.assertNotIn(("ns2", "inactive-claim"), self.client._active_connection_sandboxes)

    def test_list_active_retires_inactive_automatic_handle(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = Sandbox.__new__(Sandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")

        self.assertEqual(self.client.list_active_sandboxes(), [])
        self.client._delete_automatic_cleanup_claims()
        retained_handle.terminate()

        self.assertIsNone(retained_handle.claim_name)
        self.assertNotIn(key, self.client._active_claim_uids)
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            CLAIM_NAME, NAMESPACE, expected_uid="old-uid"
        )

    def test_list_active_preserves_caller_owned_inactive_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = Sandbox.__new__(Sandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")
        with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        self.assertEqual(self.client.list_active_sandboxes(), [])
        self.client._delete_automatic_cleanup_claims()

        self.assertEqual(retained_handle.claim_name, CLAIM_NAME)
        self.assertIn(key, self.client._caller_owned_claims)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_failed_lookup_preserves_caller_owned_claim(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = Sandbox.__new__(Sandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")
        with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        self.client._detach_failed_lookup(key, retained_handle)
        self.client._delete_automatic_cleanup_claims()

        self.assertEqual(retained_handle.claim_name, CLAIM_NAME)
        self.assertIn(key, self.client._caller_owned_claims)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_failed_lookup_cannot_delete_claim_adopted_concurrently(self):
        key = (NAMESPACE, CLAIM_NAME)
        retained_handle = Sandbox.__new__(Sandbox)
        retained_handle.claim_name = CLAIM_NAME
        retained_handle.namespace = NAMESPACE
        retained_handle.k8s_helper = self.mock_k8s_helper
        retained_handle._is_closed = True
        self.client._active_connection_sandboxes[key] = retained_handle
        self.client._active_claim_uids[key] = "old-uid"
        self.client._claim_ownership.register_automatic(key, "old-uid")
        with self.client._lock:
            self.client._claim_ownership.mark_caller_owned(key)

        self.client._detach_failed_lookup(key, retained_handle)
        self.client._delete_automatic_cleanup_claims()

        self.assertEqual(retained_handle.claim_name, CLAIM_NAME)
        self.assertIn(key, self.client._caller_owned_claims)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

    def test_list_all_sandboxes(self):
        self.mock_k8s_helper.list_sandbox_claims.return_value = ["sandbox-1", "sandbox-2"]
        
        result = self.client.list_all_sandboxes("test-namespace")
        
        self.mock_k8s_helper.list_sandbox_claims.assert_called_once_with("test-namespace", label_selector=None)
        self.assertEqual(result, ["sandbox-1", "sandbox-2"])

    def test_delete_sandbox_in_registry(self):
        key = ("test-namespace", "test-claim")
        mock_sandbox = MagicMock(claim_name="test-claim")
        self.client._active_connection_sandboxes[key] = mock_sandbox
        self.client._active_claim_uids[key] = "claim-uid"
        self.client._automatic_cleanup_claims.add(
            key
        )
        
        self.client.delete_sandbox("test-claim", "test-namespace")
        
        mock_sandbox.terminate.assert_not_called()
        mock_sandbox.close_connection.assert_called_once_with()
        self.assertNotIn(key, self.client._active_connection_sandboxes)
        self.assertNotIn(
            ("test-namespace", "test-claim"),
            self.client._automatic_cleanup_claims,
        )
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once_with(
            "test-claim", "test-namespace", expected_uid="claim-uid"
        )

    def test_delete_failure_preserves_registry_for_retry(self):
        key = ("test-namespace", "test-claim")
        mock_sandbox = MagicMock(claim_name="test-claim")
        mock_sandbox.close_connection.side_effect = RuntimeError(
            "delete failed"
        )
        self.client._active_connection_sandboxes[key] = mock_sandbox
        self.client._active_claim_uids[key] = "claim-uid"
        self.client._claim_ownership.register_automatic(key, "claim-uid")

        self.client.delete_sandbox("test-claim", "test-namespace")

        self.assertIs(self.client._active_connection_sandboxes[key], mock_sandbox)
        self.assertEqual(self.client._active_claim_uids[key], "claim-uid")
        self.assertIn(key, self.client._automatic_cleanup_claims)

    def test_delete_sandbox_not_in_registry_success(self):
        with patch.object(self.client, '_delete_claim') as mock_delete_claim:
            self.client.delete_sandbox("test-claim", "test-namespace")
            
        mock_delete_claim.assert_called_once_with("test-claim", "test-namespace")

    def test_delete_all(self):
        mock_sandbox1 = MagicMock()
        mock_sandbox1.namespace = "ns1"
        self.client._active_connection_sandboxes[("ns1", "claim1")] = mock_sandbox1
        
        mock_sandbox2 = MagicMock()
        mock_sandbox2.namespace = "ns2"
        self.client._active_connection_sandboxes[("ns2", "claim2")] = mock_sandbox2
        self.client._automatic_cleanup_claims = {
            ("ns1", "claim1"),
        }
        
        with patch.object(self.client, 'delete_sandbox') as mock_delete:
            self.client.delete_all()
            self.assertEqual(mock_delete.call_count, 2)
            mock_delete.assert_any_call("claim1", namespace="ns1")
            mock_delete.assert_any_call("claim2", namespace="ns2")

    def test_cleanup_true_registers_generated_only_atexit_handler(self):
        with patch(
            "k8s_agent_sandbox.sandbox_client.atexit"
        ) as mock_atexit, patch(
            "k8s_agent_sandbox.sandbox_client.K8sHelper"
        ):
            client = SandboxClient(cleanup=True)

        mock_atexit.register.assert_called_once_with(
            client._delete_automatic_cleanup_claims
        )

    @patch('uuid.uuid4')
    def test_create_sandbox_with_labels(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        labels = {"agent": "code-agent", "team": "platform"}

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox("test-warmpool", "test-namespace", labels=labels)

            mock_create_claim.assert_called_once_with(
                "sandbox-claim-1234abcd", "test-warmpool", "test-namespace",
                labels={"agent": "code-agent", "team": "platform"},
                lifecycle=None,
                volume_claim_templates=None,
                pod_metadata=None,
                env=None,
            )

    @patch('uuid.uuid4')
    def test_create_sandbox_with_pod_metadata(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox(
                "test-warmpool", "test-namespace",
                pod_labels={"client-id": "tenant-a"},
                pod_annotations={"note": "owned-by-tenant-a"},
            )

            mock_create_claim.assert_called_once_with(
                "sandbox-claim-1234abcd", "test-warmpool", "test-namespace",
                labels=None,
                lifecycle=None,
                volume_claim_templates=None,
                pod_metadata={
                    "labels": {"client-id": "tenant-a"},
                    "annotations": {"note": "owned-by-tenant-a"},
                },
                env=None,
            )

    def test_create_sandbox_rejects_invalid_pod_label(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", pod_labels={"bad key!": "value"})
        self.assertIn("invalid characters", str(ctx.exception))

    @patch('uuid.uuid4')
    def test_create_sandbox_with_volume_claim_templates(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        vcts = [{"metadata": {"name": "data"}, "spec": {"resources": {"requests": {"storage": "10Gi"}}}}]

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox(
                "test-warmpool",
                "test-namespace",
                volume_claim_templates=vcts,
            )

            mock_create_claim.assert_called_once_with(
                "sandbox-claim-1234abcd", "test-warmpool", "test-namespace",
                labels=None,
                lifecycle=None,
                volume_claim_templates=vcts,
                pod_metadata=None,
                env=None,
            )

    def test_create_claim_with_volume_claim_templates(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"

        vcts = [{"metadata": {"name": "data"}, "spec": {"resources": {"requests": {"storage": "10Gi"}}}}]
        self.client._create_claim(
            "test-claim",
            "test-warmpool",
            "test-namespace",
            volume_claim_templates=vcts,
        )

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels=None,
            lifecycle=None,
            volume_claim_templates=vcts,
            pod_metadata=None,
            env=None,
        )

    def test_create_claim_with_labels(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"

        labels = {"agent": "code-agent"}
        self.client._create_claim("test-claim", "test-warmpool", "test-namespace", labels=labels)

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels={"agent": "code-agent"},
            lifecycle=None,
            volume_claim_templates=None,
            pod_metadata=None,
            env=None,
        )

    def test_create_claim_with_pod_metadata(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"

        pod_metadata = {"labels": {"client-id": "tenant-a"}}
        self.client._create_claim(
            "test-claim", "test-warmpool", "test-namespace", pod_metadata=pod_metadata
        )

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels=None,
            lifecycle=None,
            volume_claim_templates=None,
            pod_metadata={"labels": {"client-id": "tenant-a"}},
            env=None,
        )

    def test_create_claim(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = "trace-data"

        self.client._create_claim("test-claim", "test-warmpool", "test-namespace")

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={"opentelemetry.io/trace-context": "trace-data"},
            labels=None,
            lifecycle=None,
            volume_claim_templates=None,
            pod_metadata=None,
            env=None,
        )

    def test_validate_labels_rejects_invalid_value(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", labels={"agent": "invalid value!"})
        self.assertIn("invalid characters", str(ctx.exception))

    def test_validate_labels_rejects_too_long_value(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", labels={"agent": "a" * 64})
        self.assertIn("exceeds max length", str(ctx.exception))

    def test_validate_labels_rejects_empty_key(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", labels={"": "value"})
        self.assertIn("Label key cannot be empty", str(ctx.exception))

    def test_validate_labels_rejects_invalid_key(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", labels={"bad key!": "value"})
        self.assertIn("invalid characters", str(ctx.exception))

    def test_validate_labels_rejects_too_long_key(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", labels={"a" * 64: "value"})
        self.assertIn("exceeds max length", str(ctx.exception))

    def test_validate_labels_accepts_prefixed_key(self):
        validate_labels({"app.kubernetes.io/name": "my-app"})

    def test_validate_labels_rejects_invalid_prefix(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-template", labels={"BAD PREFIX/name": "value"})
        self.assertIn("valid DNS subdomain", str(ctx.exception))

    def test_validate_labels_accepts_single_char_prefix(self):
        validate_labels({"a/name": "value"})

    def test_validate_labels_accepts_empty_value(self):
        validate_labels({"key": ""})

    def test_validate_labels_accepts_valid(self):
        validate_labels({"agent": "code-agent", "team": "platform-123"})

    def test_wait_for_sandbox_ready(self):
        self.client._wait_for_sandbox_ready("sandbox-id", "test-namespace", 45)
        
        self.mock_k8s_helper.wait_for_sandbox_ready.assert_called_once_with(
            "sandbox-id", "test-namespace", 45
        )

    @patch('uuid.uuid4')
    def test_create_sandbox_with_shutdown_after_seconds(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox(
                "test-warmpool", "test-namespace", shutdown_after_seconds=300
            )

            mock_create_claim.assert_called_once()
            lifecycle = mock_create_claim.call_args[1].get("lifecycle")
            self.assertIsNotNone(lifecycle)
            self.assertEqual(lifecycle["shutdownPolicy"], "Delete")
            self.assertIn("shutdownTime", lifecycle)

    @patch('uuid.uuid4')
    def test_create_sandbox_with_env(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        env = {"FOO": "bar", "DEBUG": "true"}

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox("test-warmpool", "test-namespace", env=env)

            mock_create_claim.assert_called_once_with(
                "sandbox-claim-1234abcd",
                "test-warmpool",
                "test-namespace",
                labels=None,
                lifecycle=None,
                volume_claim_templates=None,
                pod_metadata=None,
                env=env,
            )

    @patch('uuid.uuid4')
    def test_create_sandbox_without_shutdown_after_seconds(self, mock_uuid):
        mock_uuid.return_value.hex = '1234abcd'
        self.mock_k8s_helper.resolve_sandbox_name.return_value = "resolved-id"

        mock_sandbox_instance = MagicMock()
        self.mock_sandbox_class.return_value = mock_sandbox_instance

        with patch.object(self.client, '_create_claim') as mock_create_claim, \
             patch.object(self.client, '_wait_for_sandbox_ready'):

            self.client.create_sandbox("test-warmpool", "test-namespace")

            call_kwargs = mock_create_claim.call_args
            lifecycle = call_kwargs[1].get("lifecycle")
            self.assertIsNone(lifecycle)

    @patch("k8s_agent_sandbox.utils.datetime")
    def test_create_claim_with_lifecycle(self, mock_datetime):
        frozen_now = datetime(2026, 6, 15, 12, 0, 0, tzinfo=timezone.utc)
        mock_datetime.now.return_value = frozen_now
        mock_datetime.side_effect = lambda *a, **kw: datetime(*a, **kw)

        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = None

        lifecycle = {
            "shutdownTime": "2026-06-15T12:05:00Z",
            "shutdownPolicy": "Delete",
        }
        self.client._create_claim(
            "test-claim", "test-warmpool", "test-namespace", lifecycle=lifecycle
        )

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={},
            labels=None,
            lifecycle=lifecycle,
            volume_claim_templates=None,
            pod_metadata=None,
            env=None,
        )

    def test_create_claim_without_lifecycle(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = None

        self.client._create_claim("test-claim", "test-warmpool", "test-namespace")

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={},
            labels=None,
            lifecycle=None,
            volume_claim_templates=None,
            pod_metadata=None,
            env=None,
        )

    def test_create_claim_with_env(self):
        self.client.tracing_manager = MagicMock()
        self.client.tracing_manager.get_trace_context_json.return_value = None

        env = {"FOO": "bar"}
        self.client._create_claim("test-claim", "test-warmpool", "test-namespace", env=env)

        self.mock_k8s_helper.create_sandbox_claim.assert_called_once_with(
            "test-claim", "test-warmpool", "test-namespace",
            annotations={},
            labels=None,
            lifecycle=None,
            volume_claim_templates=None,
            pod_metadata=None,
            env=env,
        )

    def test_shutdown_after_seconds_validation_zero(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", shutdown_after_seconds=0)
        self.assertIn("positive", str(ctx.exception))

    def test_shutdown_after_seconds_validation_negative(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", shutdown_after_seconds=-1)
        self.assertIn("positive", str(ctx.exception))

    def test_shutdown_after_seconds_validation_bool(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", shutdown_after_seconds=True)
        self.assertIn("integer", str(ctx.exception))

    def test_shutdown_after_seconds_validation_float(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", shutdown_after_seconds=1.5)
        self.assertIn("integer", str(ctx.exception))

    def test_shutdown_after_seconds_validation_string(self):
        with self.assertRaises(ValueError) as ctx:
            self.client.create_sandbox("test-warmpool", shutdown_after_seconds="10")
        self.assertIn("integer", str(ctx.exception))

    def test_get_sandbox_claim_warmpool_name(self):
        self.mock_k8s_helper.get_sandbox_claim.return_value = {
            "spec": {"warmPoolRef": {"name": "my-warmpool"}},
        }
        warmpool_name = self.client.get_sandbox_claim_warmpool_name("my-claim", "my-namespace")
        self.assertEqual(warmpool_name, "my-warmpool")

    def test_get_sandbox_claim_warmpool_name_claim_not_found(self):
        self.mock_k8s_helper.get_sandbox_claim.return_value = None
        with self.assertRaises(SandboxNotFoundError):
            self.client.get_sandbox_claim_warmpool_name("my-claim", "my-namespace")


class SandboxHandler(BaseHTTPRequestHandler):
    """Minimal api handler with basic routing to exercise error paths."""

    def do_POST(self):
        if self.path == "/run":
            self._respond(HTTPStatus.ACCEPTED, {"status": "accepted", "message": "Trajectory execution started"})
        elif self.path == "/run-busy":
            self._respond(HTTPStatus.CONFLICT, {"detail": "A task is already running. Each sandbox can only execute one task at a time."})
        elif self.path == "/run-shutdown":
            self._respond(HTTPStatus.SERVICE_UNAVAILABLE, {"detail": "Service is shutting down, cannot accept new jobs"})
        elif self.path == "/run-500":
            self._respond(HTTPStatus.INTERNAL_SERVER_ERROR, {"detail": "Internal server error"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "Not found"})

    def do_GET(self):
        if self.path == "/health":
            self._respond(HTTPStatus.OK, {"status": "healthy", "message": "Sandbox service is running"})
        else:
            self._respond(HTTPStatus.NOT_FOUND, {"detail": "Not found"})

    def _respond(self, status: HTTPStatus, body: dict):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        payload = json.dumps(body).encode()
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


class TestRequestExceptions(unittest.TestCase):
    """Integration tests: real HTTP server + SandboxConnector.send_request()."""

    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), SandboxHandler)
        cls.port = cls.server.server_address[1]
        cls.server_thread = threading.Thread(target=cls.server.serve_forever)
        cls.server_thread.daemon = True
        cls.server_thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.server_thread.join(timeout=5)

    def _make_connector(self) -> SandboxConnector:
        """Creates a SandboxConnector pointing at the local test server."""
        config = SandboxDirectConnectionConfig(
            api_url=f"http://127.0.0.1:{self.port}",
            server_port=self.port,
        )
        k8s_helper = MagicMock()
        connector = SandboxConnector(
            sandbox_id="test-sandbox",
            namespace="default",
            connection_config=config,
            k8s_helper=k8s_helper,
        )
        adapter = HTTPAdapter(max_retries=Retry(total=0))
        connector.session.mount("http://", adapter)
        return connector

    def test_run_accepted(self):
        """POST /run returns 202."""
        connector = self._make_connector()
        response = connector.send_request("POST", "run", json={"query": "test"})
        self.assertEqual(response.status_code, 202)
        self.assertEqual(response.json()["status"], "accepted")

    def test_health_ok(self):
        """GET /health returns 200."""
        connector = self._make_connector()
        response = connector.send_request("GET", "health")
        self.assertEqual(response.status_code, 200)

    def test_409_raises_sandbox_request_error(self):
        """Validates 409 SandboxRequestError."""
        connector = self._make_connector()
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run-busy")
        self.assertEqual(ctx.exception.status_code, 409)
        body = ctx.exception.response.json()
        self.assertIn("already running", body["detail"])

    def test_409_is_catchable_as_runtime_error(self):
        """Backwards compatibility: SandboxRequestError is still a RuntimeError."""
        connector = self._make_connector()
        with self.assertRaises(RuntimeError):
            connector.send_request("POST", "run-busy")

    def test_503_raises_sandbox_request_error(self):
        """Validates 503 SandboxRequestError."""
        connector = self._make_connector()
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run-shutdown")
        self.assertEqual(ctx.exception.status_code, 503)
        body = ctx.exception.response.json()
        self.assertIn("shutting down", body["detail"])

    def test_500_raises_sandbox_request_error(self):
        """Validates 500 SandboxRequestError."""
        connector = self._make_connector()
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run-500")
        self.assertEqual(ctx.exception.status_code, 500)

    def test_connection_refused_has_no_status_code(self):
        """Validates no status_code when server is unreachable."""
        config = SandboxDirectConnectionConfig(api_url="http://192.0.2.0", server_port=8888)
        k8s_helper = MagicMock()
        connector = SandboxConnector(
            sandbox_id="test-sandbox", namespace="default",
            connection_config=config, k8s_helper=k8s_helper,
        )
        adapter = HTTPAdapter(max_retries=Retry(total=0))
        connector.session.mount("http://", adapter)
        with self.assertRaises(SandboxRequestError) as ctx:
            connector.send_request("POST", "run", timeout=1)
        self.assertIsNone(ctx.exception.status_code)

    def test_port_forward_crash_raises_sandbox_port_forward_error(self):
        """Validates SandboxPortForwardError when verify_connection detects a crash."""
        connector = self._make_connector()
        with patch.object(connector.strategy, "verify_connection",
                          side_effect=SandboxPortForwardError("Kubectl Port-Forward crashed!")):
            with self.assertRaises(SandboxPortForwardError):
                connector.send_request("GET", "health")

    def test_port_forward_error_is_catchable_as_runtime_error(self):
        """Backwards compatibility: SandboxPortForwardError is still a RuntimeError."""
        connector = self._make_connector()
        with patch.object(connector.strategy, "verify_connection",
                          side_effect=SandboxPortForwardError("Kubectl Port-Forward crashed!")):
            with self.assertRaises(RuntimeError):
                connector.send_request("GET", "health")


class TestK8sHelperWatchNoneEvents(unittest.TestCase):
    """Tests that watch streams gracefully handle None events.

    The `watch` api can yield `None` when the underlying
    connection times out/drops/etc. These tests verify that the watch
    loop can handle this gracefully.
    """

    def setUp(self):
        with patch("kubernetes.config.load_incluster_config", side_effect=k8s_config.ConfigException("not in cluster")), \
             patch("kubernetes.config.load_kube_config"):
            self.helper = K8sHelper()
        self.helper.custom_objects_api = MagicMock()

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_returns_pod_ip(self, mock_watch_cls):
        """wait_for_sandbox_ready returns the prioritized and normalized pod IP when present."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch
        mock_watch.stream.return_value = [{
            "type": "MODIFIED",
            "object": {
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                    "podIPs": ["::ffff:10.244.0.5", "fd00::5"],
                },
            },
        }]
        result = self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)
        self.assertEqual(result, "10.244.0.5")

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_returns_none_when_no_pod_ips(self, mock_watch_cls):
        """wait_for_sandbox_ready returns None when podIPs is absent."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch
        mock_watch.stream.return_value = [{
            "type": "MODIFIED",
            "object": {
                "status": {
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            },
        }]
        result = self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)
        self.assertIsNone(result)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_skips_none_events(self, mock_watch_cls):
        """None events from the watch stream should be skipped, not crash."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-sandbox"},
                "status": {
                    "conditions": [
                        {"type": "Ready", "status": "True"},
                    ],
                },
            },
        }
        mock_watch.stream.return_value = [None, None, ready_event]
        self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_all_none_times_out(self, mock_watch_cls):
        """A stream of only None events should exhaust the watch and raise TimeoutError."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        mock_watch.stream.return_value = [None, None, None]

        with self.assertRaises(TimeoutError):
            self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_skips_none_events(self, mock_watch_cls):
        """None events in the gateway watch should be skipped."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        gateway_ready_event = {
            "type": "MODIFIED",
            "object": {
                "status": {
                    "addresses": [{"value": "10.0.0.1"}],
                },
            },
        }
        mock_watch.stream.return_value = [None, gateway_ready_event]

        ip = self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=10)
        self.assertEqual(ip, "10.0.0.1")


class TestSandboxClientInClusterConfig(unittest.TestCase):
    """Tests that SandboxClient stores and propagates SandboxInClusterConnectionConfig."""

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_in_cluster_config_stored(self, _):
        config = SandboxInClusterConnectionConfig()
        sc = SandboxClient(connection_config=config)
        self.assertIsInstance(sc.connection_config, SandboxInClusterConnectionConfig)

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_default_config_is_local_tunnel(self, _):
        sc = SandboxClient()
        self.assertIsInstance(sc.connection_config, SandboxLocalTunnelConnectionConfig)

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_in_cluster_config_custom_port(self, _):
        config = SandboxInClusterConnectionConfig(server_port=9000)
        sc = SandboxClient(connection_config=config)
        self.assertEqual(sc.connection_config.server_port, 9000)

    @patch('k8s_agent_sandbox.sandbox_client.K8sHelper')
    def test_in_cluster_config_default_port(self, _):
        config = SandboxInClusterConnectionConfig()
        sc = SandboxClient(connection_config=config)
        self.assertEqual(sc.connection_config.server_port, 8888)

    def _create_sandbox_with_in_cluster_config(self, namespace='default'):
        with patch('k8s_agent_sandbox.sandbox_client.K8sHelper'), \
             patch('uuid.uuid4') as mock_uuid:
            mock_uuid.return_value.hex = 'aabbccdd'
            client = SandboxClient(connection_config=SandboxInClusterConnectionConfig())
            client.k8s_helper.resolve_sandbox_name.return_value = 'my-sandbox'
            mock_sandbox_class = MagicMock()
            mock_sandbox_class.return_value = MagicMock()
            client.sandbox_class = mock_sandbox_class
            with patch.object(client, '_create_claim'), \
                 patch.object(client, '_wait_for_sandbox_ready'):
                client.create_sandbox('my-template', namespace=namespace)
            return mock_sandbox_class.call_args.kwargs

    def test_sandbox_created_with_in_cluster_config(self):
        call_kwargs = self._create_sandbox_with_in_cluster_config()
        self.assertIsInstance(call_kwargs['connection_config'], SandboxInClusterConnectionConfig)

    def test_sandbox_namespace_passed_correctly(self):
        call_kwargs = self._create_sandbox_with_in_cluster_config(namespace='prod')
        self.assertEqual(call_kwargs['namespace'], 'prod')

    def test_client_does_not_pass_pod_ip(self):
        """SandboxClient no longer threads pod_ip — Sandbox resolves it internally."""
        call_kwargs = self._create_sandbox_with_in_cluster_config()
        self.assertNotIn('pod_ip', call_kwargs)


from pydantic import ValidationError

class TestConnectionConfigValidation(unittest.TestCase):
    def test_valid_router_namespace(self):
        valid_namespaces = ["default", "agent-sandbox-system", "my-namespace-123", "kube-system"]
        for ns in valid_namespaces:
            config = SandboxLocalTunnelConnectionConfig(router_namespace=ns)
            self.assertEqual(config.router_namespace, ns)

    def test_invalid_router_namespace(self):
        invalid_namespaces = [
            "-invalid",        # starts with hyphen
            "invalid-",        # ends with hyphen
            "Uppercase",       # contains uppercase letters
            "ns_with_under",   # contains underscore
            "ns;echo",         # command injection attempt
            "ns|sh",           # pipe attempt
            "ns&rm",           # ampersand attempt
            "",                # empty namespace
        ]
        for ns in invalid_namespaces:
            with self.assertRaises(ValidationError):
                SandboxLocalTunnelConnectionConfig(router_namespace=ns)


if __name__ == '__main__':
    unittest.main()
