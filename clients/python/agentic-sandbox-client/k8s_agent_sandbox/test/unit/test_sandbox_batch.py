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
"""Unit tests for the sync SandboxBatch handle and SandboxClient.get_batch."""

import logging
import queue
import threading
import time
import unittest
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import urllib3.exceptions
from kubernetes import client as k8s_client

from k8s_agent_sandbox import batch_state, batch_utils, sandbox_batch
from k8s_agent_sandbox.batch_utils import (
    BATCH_DEFAULT_LEASE_DURATION_SECONDS,
    CLOCK_SKEW_MARGIN,
)
from k8s_agent_sandbox.constants import (
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    BATCH_ID_LABEL,
    BATCH_LEASE_DURATION_ANNOTATION,
    BATCH_QUORUM_TIMEOUT_ANNOTATION,
    BATCH_WORK_BUDGET_ANNOTATION,
)
from k8s_agent_sandbox.exceptions import (
    BatchError,
    BatchExistsError,
    BatchInUseError,
    BatchLeaseExpiredError,
    BatchNotFoundError,
    SandboxNotReadyError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
)
from k8s_agent_sandbox.models import BatchGroup
from k8s_agent_sandbox.sandbox_batch import SandboxBatch
from k8s_agent_sandbox.sandbox_client import SandboxClient


def _lease(
    holder_identity=None,
    renew_time=None,
    lease_duration_seconds=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
    annotations=None,
    resource_version="10",
):
    return k8s_client.V1Lease(
        metadata=k8s_client.V1ObjectMeta(
            name="batch-b1", resource_version=resource_version, annotations=annotations or {}
        ),
        spec=k8s_client.V1LeaseSpec(
            holder_identity=holder_identity,
            renew_time=renew_time,
            lease_duration_seconds=lease_duration_seconds,
        ),
    )


def _claim(name, warmpool="pool-a", conditions=None, sandbox=None, annotations=None):
    metadata: dict = {"name": name}
    if annotations:
        metadata["annotations"] = annotations
    status = {}
    if conditions is not None:
        status["conditions"] = conditions
    if sandbox is not None:
        status["sandbox"] = sandbox
    return {"metadata": metadata, "spec": {"warmPoolRef": {"name": warmpool}}, "status": status}


class BaseBatchClientTest(unittest.TestCase):
    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def setUp(self, MockK8sHelper):
        self.client = SandboxClient()
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class


@patch.object(SandboxBatch, "_start", lambda self: None)
class TestGetBatchLeaseCases(BaseBatchClientTest):

    def test_not_found_raises(self):
        self.mock_k8s_helper.read_batch_lease.return_value = None
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([], "0")
        with self.assertRaises(BatchNotFoundError):
            self.client.get_batch("b1")

    def test_stale_held_lease_raises_expired(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity="someone-else", renew_time=now - timedelta(seconds=120)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        with self.assertRaises(BatchLeaseExpiredError):
            self.client.get_batch("b1")

    def test_missing_lease_with_claims_present_raises_expired(self):
        self.mock_k8s_helper.read_batch_lease.return_value = None
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        with self.assertRaises(BatchLeaseExpiredError):
            self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    def test_live_different_holder_raises_in_use(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity="other-holder", renew_time=now
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        with self.assertRaises(BatchInUseError):
            self.client.get_batch("b1")

    def test_takeover_write_conflict_raises_in_use(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None, renew_time=now
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        conflict = k8s_client.ApiException(status=409)
        self.mock_k8s_helper.replace_batch_lease.side_effect = conflict
        with self.assertRaises(BatchInUseError) as ctx:
            self.client.get_batch("b1")
        self.assertIs(ctx.exception.__cause__, conflict)

    def test_takeover_write_other_error_propagates(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None, renew_time=now
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        self.mock_k8s_helper.replace_batch_lease.side_effect = k8s_client.ApiException(status=500)
        with self.assertRaises(k8s_client.ApiException):
            self.client.get_batch("b1")


@patch.object(SandboxBatch, "_start", lambda self: None)
class TestBatchStaleness(BaseBatchClientTest):

    def test_spec_duration_boundary_adoptable_then_expired_one_second_later(self):
        now = datetime.now(UTC)
        spec_duration = 30
        with patch("k8s_agent_sandbox.sandbox_batch.datetime") as mock_dt:
            mock_dt.now.return_value = now
            self.mock_k8s_helper.read_batch_lease.return_value = _lease(
                holder_identity=None,
                renew_time=now - timedelta(seconds=spec_duration - CLOCK_SKEW_MARGIN),
                lease_duration_seconds=spec_duration,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
            )
            self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
            batch = self.client.get_batch("b1")
            self.assertIsNotNone(batch)

        with patch("k8s_agent_sandbox.sandbox_batch.datetime") as mock_dt2:
            mock_dt2.now.return_value = now
            self.mock_k8s_helper.read_batch_lease.return_value = _lease(
                holder_identity=None,
                renew_time=now - timedelta(seconds=spec_duration - CLOCK_SKEW_MARGIN + 1),
                lease_duration_seconds=spec_duration,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
            )
            with self.assertRaises(BatchLeaseExpiredError):
                self.client.get_batch("b1")

    def test_missing_spec_duration_is_treated_as_stale(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None, renew_time=now, lease_duration_seconds=None
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        with self.assertRaises(BatchLeaseExpiredError):
            self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    def test_missing_renew_time_is_treated_as_stale(self):
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None,
            renew_time=None,
            lease_duration_seconds=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        with self.assertRaises(BatchLeaseExpiredError):
            self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()


@patch.object(SandboxBatch, "_start", lambda self: None)
class TestLeaseTakeoverWrites(BaseBatchClientTest):

    def test_writes_holder_identity_and_renew_time_near_now(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None, renew_time=now
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertIsNotNone(written_lease.spec.holder_identity)
        self.assertLess(
            abs((written_lease.spec.renew_time - datetime.now(UTC)).total_seconds()), 5
        )
        self.assertEqual(written_lease.spec.acquire_time, written_lease.spec.renew_time)
        self.assertEqual(written_lease.spec.lease_transitions, 1)

    def test_takeover_increments_existing_lease_transitions(self):
        lease = _lease(holder_identity=None, renew_time=datetime.now(UTC))
        lease.spec.lease_transitions = 3
        self.mock_k8s_helper.read_batch_lease.return_value = lease
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written_lease.spec.lease_transitions, 4)

    def test_annotation_duration_overrides_leftover_spec_duration_on_takeover(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None,
            renew_time=now,
            lease_duration_seconds=300,  # leftover detach grace
            annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        batch = self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written_lease.spec.lease_duration_seconds, 90)
        self.assertEqual(batch._lease_duration, 90)

    def test_no_annotation_takeover_writes_default_duration(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None, renew_time=now
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        batch = self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written_lease.spec.lease_duration_seconds, BATCH_DEFAULT_LEASE_DURATION_SECONDS)
        self.assertEqual(batch._lease_duration, BATCH_DEFAULT_LEASE_DURATION_SECONDS)

    def test_invalid_annotation_raises_batch_error_with_no_write(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None,
            renew_time=now,
            annotations={BATCH_LEASE_DURATION_ANNOTATION: "abc"},
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        with self.assertRaises(BatchError):
            self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    def test_conflicting_group_annotations_raise_batch_error_with_no_write(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None, renew_time=now
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = (
            [
                _claim(
                    "b1-0",
                    annotations={
                        BATCH_GROUP_SIZE_ANNOTATION: "3",
                        BATCH_GROUP_MIN_READY_ANNOTATION: "2",
                    },
                ),
                _claim(
                    "b1-1",
                    annotations={
                        BATCH_GROUP_SIZE_ANNOTATION: "5",
                        BATCH_GROUP_MIN_READY_ANNOTATION: "2",
                    },
                ),
            ],
            "5",
        )
        with self.assertRaises(BatchError):
            self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    def test_takeover_leaves_annotation_unchanged(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None,
            renew_time=now,
            annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(
            written_lease.metadata.annotations, {BATCH_LEASE_DURATION_ANNOTATION: "90"}
        )


def _make_handle(
    client,
    batch_id="b1",
    namespace="default",
    groups=None,
    lease_duration=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
):
    groups = groups if groups is not None else [BatchGroup(warmpool="pool-a", size=1)]
    state = batch_state.BatchState(batch_id, groups)
    return SandboxBatch(
        client=client,
        batch_id=batch_id,
        namespace=namespace,
        state=state,
        lease_name=f"batch-{batch_id}",
        holder_identity="host_1234_abcd1234",
        lease_duration=lease_duration,
        list_resource_version="5",
    )


class TestWatcher(BaseBatchClientTest):

    def _terminating_error(self, status=403):
        return k8s_client.ApiException(status=status)

    def test_resumes_from_list_resource_version(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [self._terminating_error()]
        handle._watch_loop()
        first_call = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[0]
        self.assertEqual(first_call.args[2], "5")

    def test_410_relists_and_diffs_vanished_claim_becomes_lost(self):
        handle = _make_handle(self.client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            self._terminating_error(status=410),
            self._terminating_error(status=403),
        ]
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([], "9")
        handle._watch_loop()
        self.assertTrue(handle._state.get_member("b1-0").lost)

    def test_410_relist_retries_503_then_succeeds_reconciling_lost_claim(self):
        handle = _make_handle(self.client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        calls = {"n": 0}

        def watch_side_effect(*args, **kwargs):
            calls["n"] += 1
            if calls["n"] == 1:
                raise self._terminating_error(status=410)
            handle._watch_stop.set()
            return iter(())

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = watch_side_effect
        self.mock_k8s_helper.list_sandbox_claim_objects.side_effect = [
            self._terminating_error(status=503),
            ([], "9"),
        ]
        handle._watch_loop()
        self.assertTrue(handle._state.get_member("b1-0").lost)
        self.assertIsNone(handle.err())
        list_calls = self.mock_k8s_helper.list_sandbox_claim_objects.call_args_list
        self.assertEqual(len(list_calls), 2)

    def test_410_relist_403_surfaces_through_err_and_stops(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [self._terminating_error(status=410)]
        self.mock_k8s_helper.list_sandbox_claim_objects.side_effect = [
            self._terminating_error(status=403)
        ]
        handle._watch_loop()
        self.assertIsInstance(handle.err(), k8s_client.ApiException)
        self.assertEqual(handle.err().status, 403)

    def test_410_relist_after_stop_set_does_not_touch_state(self):
        handle = _make_handle(self.client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [self._terminating_error(status=410)]

        def list_side_effect(*args, **kwargs):
            handle._watch_stop.set()
            return ([], "9")

        self.mock_k8s_helper.list_sandbox_claim_objects.side_effect = list_side_effect
        handle._watch_loop()
        self.assertFalse(handle._state.get_member("b1-0").lost)

    def test_disconnect_reconnects_from_last_resource_version(self):
        handle = _make_handle(self.client)

        def first_stream(*args, **kwargs):
            yield {"type": "MODIFIED", "object": {"metadata": {"name": "b1-0", "resourceVersion": "77"}, "spec": {"warmPoolRef": {"name": "pool-a"}}, "status": {}}}
            raise urllib3.exceptions.ProtocolError("boom")

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            first_stream(),
            self._terminating_error(status=403),
        ]
        handle._watch_loop()
        second_call = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[1]
        self.assertEqual(second_call.args[2], "77")

    def test_bookmark_only_advances_resource_version(self):
        handle = _make_handle(self.client)
        bookmark_event = {"type": "BOOKMARK", "object": {"metadata": {"resourceVersion": "42"}}}
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            [bookmark_event],
            self._terminating_error(status=403),
        ]
        handle._watch_loop()
        self.assertEqual(len(handle._state.members()), 0)
        second_call = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[1]
        self.assertEqual(second_call.args[2], "42")

    def test_403_surfaces_through_err(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [self._terminating_error(status=403)]
        handle._watch_loop()
        self.assertIsInstance(handle.err(), k8s_client.ApiException)
        self.assertEqual(handle.err().status, 403)

    def test_error_after_stop_set_leaves_err_none(self):
        handle = _make_handle(self.client)

        def watch_side_effect(*args, **kwargs):
            handle._watch_stop.set()
            raise self._terminating_error(status=403)

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = watch_side_effect
        handle._watch_loop()
        self.assertIsNone(handle.err())

    def _retry_then_stop(self, handle, status):
        """A watch attempt raising ``status`` followed by one that succeeds
        with no events, at which point the test stops the loop from inside
        the mocked call itself (there is no third attempt, so ``err()``
        never gets a chance to be set by anything after the retry)."""
        calls = {"n": 0}

        def side_effect(*args, **kwargs):
            calls["n"] += 1
            if calls["n"] == 1:
                raise self._terminating_error(status=status)
            handle._watch_stop.set()
            return iter(())

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = side_effect

    def test_503_retries_from_last_resource_version_and_leaves_err_none(self):
        handle = _make_handle(self.client)
        self._retry_then_stop(handle, 503)
        handle._watch_loop()
        self.assertIsNone(handle.err())
        calls = self.mock_k8s_helper.watch_sandbox_claims.call_args_list
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[1].args[2], "5")

    def test_ssl_error_surfaces_through_err_and_stops(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            urllib3.exceptions.SSLError("boom")
        ]
        handle._watch_loop()
        self.assertIsInstance(handle.err(), urllib3.exceptions.SSLError)

    def test_connection_failure_retries_unless_caused_by_ssl(self):
        refused = urllib3.exceptions.NewConnectionError(None, "refused")
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            urllib3.exceptions.MaxRetryError(None, "/", reason=refused),
            urllib3.exceptions.MaxRetryError(None, "/", reason=urllib3.exceptions.SSLError("boom")),
        ]
        with patch("k8s_agent_sandbox.sandbox_batch._watch_backoff_delay", return_value=0):
            handle._watch_loop()
        self.assertEqual(self.mock_k8s_helper.watch_sandbox_claims.call_count, 2)
        self.assertIsInstance(handle.err().reason, urllib3.exceptions.SSLError)

    def test_unexpected_error_surfaces_through_err_and_stops(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [RuntimeError("boom")]
        handle._watch_loop()
        self.assertIsInstance(handle.err(), RuntimeError)

    def test_label_selector_is_exact(self):
        handle = _make_handle(self.client, batch_id="b1234")
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [self._terminating_error(status=403)]
        handle._watch_loop()
        call_args = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[0]
        self.assertEqual(call_args.args[1], f"{BATCH_ID_LABEL}=b1234")

    def test_410_relist_transport_error_retries_then_reconciles(self):
        handle = _make_handle(self.client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        calls = {"n": 0}

        def watch_side_effect(*args, **kwargs):
            calls["n"] += 1
            if calls["n"] == 1:
                raise self._terminating_error(status=410)
            handle._watch_stop.set()
            return iter(())

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = watch_side_effect
        self.mock_k8s_helper.list_sandbox_claim_objects.side_effect = [
            urllib3.exceptions.ProtocolError("boom"),
            ([], "9"),
        ]
        handle._watch_loop()
        self.assertTrue(handle._state.get_member("b1-0").lost)
        self.assertIsNone(handle.err())
        self.assertEqual(len(self.mock_k8s_helper.list_sandbox_claim_objects.call_args_list), 2)

    def test_consecutive_failures_back_off_and_an_event_resets_the_delay(self):
        handle = _make_handle(self.client)
        delays = []
        handle._watch_stop.wait = lambda timeout=None: delays.append(timeout) or False

        def event_then_503(*args, **kwargs):
            yield {"type": "MODIFIED", "object": {"metadata": {"name": "b1-0", "resourceVersion": "6"}, "spec": {"warmPoolRef": {"name": "pool-a"}}, "status": {}}}
            raise self._terminating_error(status=503)

        def stop(*args, **kwargs):
            handle._watch_stop.set()
            return iter(())

        responses = iter([
            self._terminating_error(status=503),
            urllib3.exceptions.ProtocolError("boom"),
            self._terminating_error(status=429),
            event_then_503(),
        ])

        def watch_side_effect(*args, **kwargs):
            response = next(responses, None)
            if response is None:
                return stop()
            if isinstance(response, Exception):
                raise response
            return response

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = watch_side_effect
        handle._watch_loop()

        # Equal jitter keeps attempt n within [base * 2**(n-1) / 2, base * 2**(n-1)].
        self.assertEqual(len(delays), 4)
        self.assertTrue(0.25 <= delays[0] <= 0.5, delays)
        self.assertTrue(0.5 <= delays[1] <= 1.0, delays)
        self.assertTrue(1.0 <= delays[2] <= 2.0, delays)
        self.assertTrue(0.25 <= delays[3] <= 0.5, delays)
        self.assertIsNone(handle.err())


class TestRenewal(BaseBatchClientTest):

    def test_renewal_uses_read_resource_version(self):
        handle = _make_handle(self.client)
        lease = _lease(holder_identity=handle._holder_identity, resource_version="123")
        self.mock_k8s_helper.read_batch_lease.return_value = lease
        self.assertTrue(handle._renew_once())
        self.mock_k8s_helper.replace_batch_lease.assert_called_once_with(
            handle._lease_name, handle.namespace, lease, _request_timeout=handle._renew_interval
        )
        self.assertEqual(lease.metadata.resource_version, "123")

    def test_renewal_requests_are_bounded_by_the_renew_interval(self):
        handle = _make_handle(self.client, lease_duration=60)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=handle._holder_identity
        )
        self.assertTrue(handle._renew_once())
        self.assertEqual(
            self.mock_k8s_helper.read_batch_lease.call_args.kwargs["_request_timeout"], 20
        )
        self.assertEqual(
            self.mock_k8s_helper.replace_batch_lease.call_args.kwargs["_request_timeout"], 20
        )

    def test_first_failure_logs_degraded(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.read_batch_lease.side_effect = RuntimeError("api down")
        with self.assertLogs(level="INFO") as ctx:
            handle._renew_once()
        self.assertTrue(any("degraded" in msg for msg in ctx.output))
        self.assertTrue(handle._renewal_degraded)

    def test_holder_mismatch_stops_renewal_without_writing(self):
        for other_holder in ("someone-else", None):
            with self.subTest(other_holder=other_holder):
                self.mock_k8s_helper.reset_mock()
                handle = _make_handle(self.client)
                self.mock_k8s_helper.read_batch_lease.return_value = _lease(
                    holder_identity=other_holder
                )
                self.assertFalse(handle._renew_once())
                self.mock_k8s_helper.replace_batch_lease.assert_not_called()
                self.assertIsInstance(handle.err(), BatchInUseError)

    def test_missing_lease_stops_renewal_and_records_expired(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.read_batch_lease.return_value = None
        self.assertFalse(handle._renew_once())
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)

    def test_successful_renewal_clears_degraded(self):
        handle = _make_handle(self.client)
        handle._renewal_degraded = True
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=handle._holder_identity
        )
        handle._renew_once()
        self.assertFalse(handle._renewal_degraded)

    def test_no_success_for_lease_duration_returns_expired(self):
        handle = _make_handle(self.client, lease_duration=BATCH_DEFAULT_LEASE_DURATION_SECONDS)
        handle._last_renew_success = time.monotonic() - (BATCH_DEFAULT_LEASE_DURATION_SECONDS + 1)
        self.mock_k8s_helper.read_batch_lease.side_effect = RuntimeError("api down")
        handle._renew_once()
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)


class TestMembers(unittest.TestCase):

    def test_warmpool_filter_and_snapshot_immutability(self):
        client_mock = MagicMock()
        handle = _make_handle(client_mock)
        handle._state.upsert_claim(_claim("b1-0", warmpool="pool-a"))
        handle._state.upsert_claim(_claim("b1-1", warmpool="pool-b"))

        pool_a = handle.members(warmpool="pool-a")
        self.assertEqual([m.claim_name for m in pool_a], ["b1-0"])

        first = handle.members()[0]
        handle._state.upsert_claim(
            _claim("b1-0", warmpool="pool-a", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.assertFalse(first.ready)


class TestConnect(unittest.TestCase):

    def setUp(self):
        self.mock_client = MagicMock()
        self.mock_sandbox_instance = MagicMock()
        self.mock_client.sandbox_class.return_value = self.mock_sandbox_instance
        self.handle = _make_handle(self.mock_client)

    def test_builds_sandbox_class_without_resolve_or_get_sandbox(self):
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        sandbox = self.handle.connect(member)

        self.mock_client.sandbox_class.assert_called_once_with(
            claim_name="b1-0",
            sandbox_id="sbx-1",
            namespace="default",
            connection_config=self.mock_client.connection_config,
            tracer_config=self.mock_client.tracer_config,
            k8s_helper=self.mock_client.k8s_helper,
        )
        self.mock_client.k8s_helper.resolve_sandbox_name.assert_not_called()
        self.mock_client.k8s_helper.get_sandbox.assert_not_called()
        self.assertIs(sandbox, self.mock_sandbox_instance)

    def test_repeat_call_returns_same_handle(self):
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        first = self.handle.connect(member)
        second = self.handle.connect(member)
        self.assertIs(first, second)
        self.mock_client.sandbox_class.assert_called_once()

    def test_not_ready_member_raises(self):
        self.handle._state.upsert_claim(_claim("b1-0", conditions=[]))
        member = self.handle.members()[0]
        with self.assertRaises(SandboxNotReadyError):
            self.handle.connect(member)

    def test_lost_member_raises(self):
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        self.handle._state.mark_lost("b1-0")
        with self.assertRaises(SandboxNotReadyError):
            self.handle.connect(member)

    @patch("k8s_agent_sandbox.sandbox_client.K8sHelper")
    def test_connected_handle_not_registered_in_active_connection_sandboxes(self, MockK8sHelper):
        real_client = SandboxClient()
        real_client.sandbox_class = MagicMock(return_value=MagicMock())
        handle = _make_handle(real_client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = handle.members()[0]
        handle.connect(member)
        self.assertEqual(real_client._active_connection_sandboxes, {})


class TestDetach(unittest.TestCase):

    def setUp(self):
        self.mock_client = MagicMock()
        self.handle = _make_handle(self.mock_client)

    def test_invalid_grace_raises_value_error_and_leaves_handle_running(self):
        with self.assertRaises(ValueError):
            self.handle.detach(grace=1.5)
        self.mock_client.k8s_helper.replace_batch_lease.assert_not_called()
        self.assertFalse(self.handle._detached)

    def test_final_write_clears_holder_sets_renew_time_and_grace_duration(self):
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=self.handle._holder_identity,
            annotations={BATCH_LEASE_DURATION_ANNOTATION: str(BATCH_DEFAULT_LEASE_DURATION_SECONDS)},
        )
        self.handle.detach(grace=10)
        written = self.mock_client.k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertIsNone(written.spec.holder_identity)
        self.assertLess(abs((written.spec.renew_time - datetime.now(UTC)).total_seconds()), 5)
        self.assertEqual(written.spec.lease_duration_seconds, 10)
        self.assertEqual(
            written.metadata.annotations,
            {BATCH_LEASE_DURATION_ANNOTATION: str(BATCH_DEFAULT_LEASE_DURATION_SECONDS)},
        )

    def test_grace_none_uses_handles_lease_duration(self):
        handle = _make_handle(self.mock_client, lease_duration=90)
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=handle._holder_identity
        )
        handle.detach(grace=None)
        written = self.mock_client.k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written.spec.lease_duration_seconds, 90)

    def test_idempotent_detach(self):
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=self.handle._holder_identity
        )
        mock_sandbox = MagicMock()
        self.handle._connected["b1-0"] = mock_sandbox

        self.handle.detach()

        mock_sandbox.close_connection.assert_called_once()
        self.assertEqual(self.handle._connected, {})

        self.handle.detach()  # Second call: no exception, and does nothing.

        mock_sandbox.close_connection.assert_called_once()
        self.mock_client.k8s_helper.read_batch_lease.assert_called_once()
        self.mock_client.k8s_helper.replace_batch_lease.assert_called_once()

    def test_retryable_after_release_write_failure(self):
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=self.handle._holder_identity
        )
        self.mock_client.k8s_helper.replace_batch_lease.side_effect = RuntimeError("api down")

        with self.assertRaises(RuntimeError):
            self.handle.detach()
        self.assertTrue(self.handle._detached)
        self.assertFalse(self.handle._lease_released)

        self.mock_client.k8s_helper.replace_batch_lease.side_effect = None
        self.handle.detach()  # Retry: completes the release.
        self.assertTrue(self.handle._lease_released)

    def test_close_connection_failure_still_releases_lease(self):
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=self.handle._holder_identity
        )
        bad_sandbox = MagicMock()
        bad_sandbox.close_connection.side_effect = RuntimeError("boom")
        self.handle._connected["b1-0"] = bad_sandbox

        with self.assertLogs(level="WARNING"):
            self.handle.detach()  # Must not raise.

        self.mock_client.k8s_helper.replace_batch_lease.assert_called_once()

    def test_foreign_holder_lease_is_left_untouched(self):
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity="someone-else"
        )

        self.handle.detach()  # Must not raise.

        self.mock_client.k8s_helper.replace_batch_lease.assert_not_called()

    def test_connect_after_detach_raises(self):
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease()
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        self.handle.detach()
        with self.assertRaises(BatchError):
            self.handle.connect(member)

    def test_members_and_err_keep_last_snapshot_after_detach(self):
        self.mock_client.k8s_helper.read_batch_lease.return_value = _lease()
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.handle._state.note_error(RuntimeError("boom"))

        self.handle.detach()

        self.assertEqual([m.claim_name for m in self.handle.members()], ["b1-0"])
        self.assertIsInstance(self.handle.err(), RuntimeError)


class TestStart(unittest.TestCase):

    def test_start_launches_two_daemon_threads(self):
        mock_client = MagicMock()
        handle = _make_handle(mock_client)
        with patch.object(SandboxBatch, "_watch_loop", lambda self: None), \
             patch.object(SandboxBatch, "_renew_loop", lambda self: None):
            handle._start()
            handle._watch_thread.join(timeout=2)
            handle._renew_thread.join(timeout=2)
        self.assertTrue(handle._watch_thread.daemon)
        self.assertTrue(handle._renew_thread.daemon)


_READY = [{"type": "Ready", "status": "True"}]
_NOT_READY = [{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}]


class _Cluster:
    """A fake K8sHelper for claim_batch: records requests and feeds created claims to the watch."""

    def __init__(self, ready_on_create=True):
        self.ready_on_create = ready_on_create
        self.calls: list[str] = []
        self.created: list[tuple[str, float]] = []
        self.watch_queue: queue.Queue = queue.Queue()
        self.create_hook = None
        self.helper = MagicMock()
        h = self.helper
        h.get_sandbox_warmpool.return_value = {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
        h.get_sandbox_template.return_value = {"metadata": {"name": "tmpl"}}
        h.list_sandbox_claim_objects.return_value = ([], "7")
        h.watch_sandbox_claims.side_effect = self._watch
        h.create_sandbox_claim.side_effect = self._create
        h.delete_sandbox_claims_by_label.side_effect = lambda *a, **k: self.calls.append("deletecollection")
        h.delete_batch_lease.side_effect = lambda *a, **k: self.calls.append("delete_lease")

    def _watch(self, *args, **kwargs):
        try:
            yield self.watch_queue.get(timeout=0.05)
        except queue.Empty:
            return
        while True:
            try:
                yield self.watch_queue.get_nowait()
            except queue.Empty:
                return

    def _create(self, name, warmpool, namespace, **kwargs):
        if self.create_hook is not None:
            self.create_hook(name)
        self.calls.append("create")
        self.created.append((name, time.monotonic()))
        if self.ready_on_create:
            self.push(name, warmpool)

    def push(self, name, warmpool="pool-a", ready=True):
        conditions = _READY if ready else _NOT_READY
        sandbox = {"name": f"sbx-{name}"} if ready else None
        self.watch_queue.put(
            {"type": "MODIFIED", "object": _claim(name, warmpool, conditions=conditions, sandbox=sandbox)}
        )

    def names(self):
        return [name for name, _ in self.created]


def _wait_until(condition, timeout=5.0):
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        time.sleep(0.01)


class BaseClaimBatchTest(BaseBatchClientTest):

    def setUp(self):
        super().setUp()
        self.cluster = _Cluster()
        self.client.k8s_helper = self.cluster.helper

    def claim(self, groups=None, **kwargs):
        kwargs.setdefault("batch_id", "b1")
        kwargs.setdefault("create_rps", 1000)
        batch = self.client.claim_batch(
            groups or [BatchGroup(warmpool="pool-a", size=2)], **kwargs
        )
        self.addCleanup(batch._stop_background_threads)
        return batch


class TestClaimBatchSetup(BaseClaimBatchTest):

    def test_setup_order_and_lease(self):
        self.cluster.helper.get_sandbox_warmpool.side_effect = lambda *a, **k: (
            self.cluster.calls.append("precheck") or {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
        )
        self.cluster.helper.create_batch_lease.side_effect = lambda *a, **k: self.cluster.calls.append("lease")
        self.cluster.helper.list_sandbox_claim_objects.side_effect = (
            lambda *a, **k: self.cluster.calls.append("list") or ([], "7")
        )
        batch = self.claim(
            [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
            quorum_timeout=90,
            work_budget=120,
            lease_duration=45,
        )
        _wait_until(lambda: len(self.cluster.created) == 2)

        helper = self.cluster.helper
        self.assertEqual(
            [c.args[0] for c in helper.get_sandbox_warmpool.call_args_list], ["pool-a", "pool-b"]
        )
        helper.get_sandbox_template.assert_called_once()  # Both pools share one template.
        self.assertEqual(self.cluster.calls, ["precheck", "precheck", "lease", "list", "create", "create"])
        self.assertEqual(helper.watch_sandbox_claims.call_args_list[0].args[2], "7")

        lease = helper.create_batch_lease.call_args.args[1]
        self.assertEqual(lease.metadata.name, "batch-b1")
        self.assertEqual(
            lease.metadata.labels,
            {BATCH_ID_LABEL: "b1", "agents.x-k8s.io/created-by": "python-client"},
        )
        self.assertEqual(
            lease.metadata.annotations,
            {
                BATCH_LEASE_DURATION_ANNOTATION: "45",
                BATCH_WORK_BUDGET_ANNOTATION: "120",
                BATCH_QUORUM_TIMEOUT_ANNOTATION: "90",
            },
        )
        self.assertEqual(lease.spec.holder_identity, batch._holder_identity)
        self.assertEqual(lease.spec.lease_duration_seconds, 45)
        self.assertEqual(lease.spec.lease_transitions, 0)
        self.assertLess(abs((lease.spec.renew_time - datetime.now(UTC)).total_seconds()), 5)

    def test_claim_manifests(self):
        self.claim(
            [BatchGroup(warmpool="pool-a", size=2, min_ready=1), BatchGroup(warmpool="pool-b", size=1)],
            labels={"team": "rl"},
            quorum_timeout=90,
            work_budget=120,
        )
        _wait_until(lambda: len(self.cluster.created) == 3)

        calls = sorted(self.cluster.helper.create_sandbox_claim.call_args_list, key=lambda c: c.args[0])
        self.assertEqual(
            [(c.args[0], c.args[1]) for c in calls],
            [("b1-0", "pool-a"), ("b1-1", "pool-a"), ("b1-2", "pool-b")],
        )
        kwargs = calls[0].kwargs
        self.assertEqual(kwargs["labels"], {"team": "rl", BATCH_ID_LABEL: "b1"})
        self.assertEqual(
            kwargs["annotations"],
            {BATCH_GROUP_SIZE_ANNOTATION: "2", BATCH_GROUP_MIN_READY_ANNOTATION: "1"},
        )
        self.assertEqual(kwargs["lifecycle"]["shutdownPolicy"], "Delete")
        shutdown = datetime.strptime(kwargs["lifecycle"]["shutdownTime"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=UTC)
        expected = datetime.now(UTC) + timedelta(seconds=90 + 120 + 600)
        self.assertLess(abs((shutdown - expected).total_seconds()), 5)
        self.assertEqual(kwargs["log_level"], logging.DEBUG)

    def test_precheck_failure_writes_nothing(self):
        helper = self.cluster.helper
        cases = {
            "missing warm pool": ("get_sandbox_warmpool", None, SandboxWarmPoolNotFoundError),
            "missing template": ("get_sandbox_template", None, SandboxTemplateNotFoundError),
            "warm pool names no template": ("get_sandbox_warmpool", {"spec": {}}, SandboxTemplateNotFoundError),
            "forbidden": ("get_sandbox_warmpool", k8s_client.ApiException(status=403), k8s_client.ApiException),
        }
        for name, (method, result, error) in cases.items():
            with self.subTest(name):
                helper.reset_mock()
                helper.get_sandbox_warmpool.return_value = {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
                helper.get_sandbox_template.return_value = {}
                helper.get_sandbox_warmpool.side_effect = None
                if isinstance(result, Exception):
                    getattr(helper, method).side_effect = result
                else:
                    getattr(helper, method).return_value = result
                with self.assertRaises(error):
                    self.client.claim_batch([BatchGroup(warmpool="pool-a", size=1)])
                helper.create_batch_lease.assert_not_called()

    def test_setup_failure_deletes_only_a_lease_it_created(self):
        helper = self.cluster.helper
        with self.subTest("lease exists"):
            helper.create_batch_lease.side_effect = k8s_client.ApiException(status=409)
            with self.assertRaises(BatchExistsError):
                self.claim()
            helper.delete_batch_lease.assert_not_called()
        helper.create_batch_lease.side_effect = None
        for name, list_result, error in (
            ("claims exist", ([_claim("b1-0")], "7"), BatchExistsError),
            ("list fails", k8s_client.ApiException(status=500), k8s_client.ApiException),
        ):
            with self.subTest(name):
                helper.delete_batch_lease.reset_mock()
                if isinstance(list_result, Exception):
                    helper.list_sandbox_claim_objects.side_effect = list_result
                else:
                    helper.list_sandbox_claim_objects.side_effect = None
                    helper.list_sandbox_claim_objects.return_value = list_result
                with self.assertRaises(error):
                    self.claim()
                self.assertEqual(helper.delete_batch_lease.call_args.args[:2], ("batch-b1", "default"))
        self.assertEqual(self.cluster.created, [])


class TestClaimBatchCreation(BaseClaimBatchTest):

    def test_creates_are_bounded_by_max_in_flight_and_paced_by_create_rps(self):
        gate = threading.Event()
        self.cluster.create_hook = lambda name: gate.wait(5)
        self.claim([BatchGroup(warmpool="pool-a", size=5)], max_in_flight=2)
        _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count == 2)
        time.sleep(0.2)
        self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 2)
        gate.set()
        _wait_until(lambda: len(self.cluster.created) == 5)

        self.cluster.create_hook = None
        self.cluster.created.clear()
        start = time.monotonic()
        self.claim([BatchGroup(warmpool="pool-a", size=5)], batch_id="b2", create_rps=20)
        _wait_until(lambda: len(self.cluster.created) == 5)
        starts = sorted(t for _, t in self.cluster.created)
        for k, t in enumerate(starts):
            self.assertGreaterEqual(t - start, k * 0.05 - 0.001)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    def test_create_retries_transient_errors_and_fails_on_403(self, _):
        helper = self.cluster.helper
        helper.create_sandbox_claim.side_effect = [k8s_client.ApiException(status=503), None]
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        _wait_until(lambda: helper.create_sandbox_claim.call_count == 2)
        self.assertEqual([c.args[0] for c in helper.create_sandbox_claim.call_args_list], ["b1-0", "b1-0"])
        time.sleep(0.1)
        self.assertEqual(batch.members(), [])  # No CreateFailed member.

        helper.reset_mock()
        helper.create_sandbox_claim.side_effect = k8s_client.ApiException(status=403, reason="Forbidden")
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b2")
        _wait_until(lambda: batch.members() != [])
        [member] = batch.members()
        self.assertEqual((member.terminal, member.reason), (True, "CreateFailed"))

class TestRelease(BaseClaimBatchTest):

    def test_release_stops_creates_then_deletes_claims_then_the_lease(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=20)], create_rps=10)
        _wait_until(lambda: len(self.cluster.created) >= 1)
        self.cluster.helper.list_sandbox_claim_objects.return_value = (
            [_claim("b1-0") | {"metadata": {"name": "b1-0", "deletionTimestamp": "2026-01-01T00:00:00Z"}}],
            "9",
        )
        batch.release()
        time.sleep(0.3)  # Several pacing slots, in case a creator is still running.
        calls = self.cluster.calls
        self.assertLess(len(self.cluster.created), 20)
        self.assertEqual(calls[calls.index("deletecollection"):], ["deletecollection", "delete_lease"])
        self.assertNotIn(("default", "b1"), self.client._active_batches)

        self.cluster.helper.reset_mock()
        batch.release()  # Idempotent.
        self.cluster.helper.delete_sandbox_claims_by_label.assert_not_called()
        with self.assertRaises(BatchError):
            batch.connect(MagicMock(claim_name="b1-0"))
        with self.assertRaises(BatchError):
            batch.detach()

    def test_hung_create_delays_release_only_until_the_request_timeout(self):
        hung = threading.Event()
        self.cluster.create_hook = lambda name: hung.wait(10)
        with patch.object(batch_utils, "BATCH_REQUEST_TIMEOUT_SECONDS", 0):
            batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
            self.addCleanup(hung.set)
            _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count == 1)
            start = time.monotonic()
            batch.release()
        self.assertLess(time.monotonic() - start, 3)

    def test_release_after_detach_raises(self):
        handle = _make_handle(self.client)
        self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=handle._holder_identity)
        handle.detach()
        with self.assertRaises(BatchError):
            handle.release()
        self.cluster.helper.delete_sandbox_claims_by_label.assert_not_called()

    @patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=lambda s: None))
    def test_release_retries_rounds_until_no_progress(self):
        helper = self.cluster.helper

        def live(n):
            return ([_claim(f"b1-{i}") for i in range(n)], "9")

        unavailable = k8s_client.ApiException(status=503)
        cases = {
            "503 then success": (
                {"delete_sandbox_claims_by_label": [unavailable, None]},
                {"list_sandbox_claim_objects": [live(1), live(0)]},
                None,
            ),
            "403 raises at once": ({"delete_sandbox_claims_by_label": [k8s_client.ApiException(status=403)]}, {}, k8s_client.ApiException),
            "no progress raises BatchError": ({}, {"list_sandbox_claim_objects": [live(2)] * 4}, BatchError),
            "no progress raises the last transient error": (
                {"delete_sandbox_claims_by_label": [unavailable] * 4},
                {"list_sandbox_claim_objects": [unavailable] * 4},
                k8s_client.ApiException,
            ),
            "progress resets the idle rounds": ({}, {"list_sandbox_claim_objects": [live(5), live(5), live(4), live(4), live(4), live(0)]}, None),
        }
        for name, (delete_effects, list_effects, error) in cases.items():
            with self.subTest(name):
                helper.reset_mock()
                helper.delete_sandbox_claims_by_label.side_effect = delete_effects.get("delete_sandbox_claims_by_label")
                helper.list_sandbox_claim_objects.side_effect = list_effects.get("list_sandbox_claim_objects", [live(0)] * 4)
                helper.delete_batch_lease.side_effect = None
                handle = _make_handle(self.client)
                if error is None:
                    handle.release()
                    helper.delete_batch_lease.assert_called_once()
                else:
                    with self.assertRaises(error):
                        handle.release()
                    helper.delete_batch_lease.assert_not_called()
                    self.assertFalse(handle._released)

    def test_requests_carry_request_timeouts(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        _wait_until(lambda: len(self.cluster.created) == 1)
        batch.release()
        helper = self.cluster.helper
        self.assertEqual(helper.create_sandbox_claim.call_args.kwargs["_request_timeout"], 30)
        self.assertEqual(helper.delete_sandbox_claims_by_label.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.delete_batch_lease.call_args.kwargs["_request_timeout"], 30)

        handle = _make_handle(self.client)
        helper.read_batch_lease.return_value = _lease(holder_identity=handle._holder_identity)
        handle.detach()
        self.assertEqual(helper.read_batch_lease.call_args.kwargs["_request_timeout"], 30)
        self.assertEqual(helper.replace_batch_lease.call_args.kwargs["_request_timeout"], 30)

    def test_detach_stops_creates_and_deletes_nothing(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=20)], create_rps=10)
        _wait_until(lambda: len(self.cluster.created) >= 1)
        self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=batch._holder_identity)
        batch.detach()
        created = len(self.cluster.created)
        time.sleep(0.3)
        self.assertEqual(len(self.cluster.created), created)
        self.assertLess(created, 20)
        self.cluster.helper.delete_sandbox_claims_by_label.assert_not_called()
        self.cluster.helper.delete_batch_lease.assert_not_called()


class TestClientBatchCleanup(BaseClaimBatchTest):

    @patch.object(SandboxBatch, "_start", lambda self: None)
    def test_cleanup_releases_every_tracked_batch(self):
        for cleanup in ("delete_all", "_delete_automatic_sandboxes"):
            with self.subTest(cleanup):
                self.client._active_batches.clear()
                claimed = self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b1")
                self.cluster.helper.list_sandbox_claim_objects.return_value = ([_claim("b2-0")], "5")
                self.cluster.helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC))
                attached = self.client.get_batch("b2")
                self.cluster.helper.list_sandbox_claim_objects.return_value = ([], "7")
                self.assertEqual(set(self.client._active_batches.values()), {claimed, attached})

                with patch.object(SandboxBatch, "release", autospec=True) as release:
                    getattr(self.client, cleanup)()
                self.assertEqual({c.args[0] for c in release.call_args_list}, {claimed, attached})

                claimed.release()
                self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=attached._holder_identity)
                attached.detach()
                self.assertEqual(self.client._active_batches, {})


if __name__ == "__main__":
    unittest.main()
