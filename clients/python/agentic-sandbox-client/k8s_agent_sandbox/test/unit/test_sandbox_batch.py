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

import collections
import contextlib
import json
import logging
import queue
import socket
import ssl
from concurrent.futures import Future, ThreadPoolExecutor
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
    BATCH_CREATE_RPS_ANNOTATION,
    BATCH_GROUP_LABEL,
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    BATCH_ID_LABEL,
    BATCH_LEASE_DURATION_ANNOTATION,
    BATCH_LEASE_UID_LABEL,
    BATCH_NEXT_ORDINAL_ANNOTATION,
    BATCH_QUORUM_TIMEOUT_ANNOTATION,
    BATCH_WORK_BUDGET_ANNOTATION,
)
from k8s_agent_sandbox.exceptions import (
    BatchError,
    BatchExistsError,
    BatchInUseError,
    BatchLeaseExpiredError,
    BatchNotFoundError,
    BatchTimeoutError,
    QuorumUnreachableError,
    SandboxNotReadyError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
    TerminalMemberError,
)
from k8s_agent_sandbox.k8s_helper import K8sHelper
from k8s_agent_sandbox.models import BatchEventType, BatchGroup, MemberState
from k8s_agent_sandbox.sandbox_batch import SandboxBatch
from k8s_agent_sandbox.sandbox_client import SandboxClient


def _lease(
    holder_identity=None,
    renew_time=None,
    lease_duration_seconds=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
    annotations=None,
    resource_version="10",
    uid="lease-uid",
    lease_transitions=None,
):
    return k8s_client.V1Lease(
        metadata=k8s_client.V1ObjectMeta(
            name="batch-b1", resource_version=resource_version, annotations=annotations or {}, uid=uid
        ),
        spec=k8s_client.V1LeaseSpec(
            holder_identity=holder_identity,
            renew_time=renew_time,
            lease_duration_seconds=lease_duration_seconds,
            lease_transitions=lease_transitions,
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
        self.mock_k8s_helper.api_pool_size.return_value = None
        self.mock_k8s_helper.count_live_sandbox_claims.return_value = 0
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
        self.assertEqual(
            self.mock_k8s_helper.list_sandbox_claim_objects.call_args.args[1],
            f"{BATCH_ID_LABEL}=b1",
        )
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    def test_live_different_holder_raises_in_use(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity="other-holder", renew_time=now
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        with self.assertRaises(BatchInUseError):
            self.client.get_batch("b1")

    def test_empty_holder_identity_counts_as_free(self):
        # client-go releases a Lease by writing an empty holderIdentity.
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity="", renew_time=datetime.now(UTC)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        self.mock_k8s_helper.replace_batch_lease.return_value = _lease(holder_identity="h1")
        handle = self.client.get_batch("b1")
        self.assertEqual(
            self.mock_k8s_helper.list_sandbox_claim_objects.call_args.args[1],
            f"{BATCH_ID_LABEL}=b1,{BATCH_LEASE_UID_LABEL}=lease-uid",
        )
        self.assertEqual(handle._lease_uid, "lease-uid")
        self.mock_k8s_helper.replace_batch_lease.assert_called_once()

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

    def test_get_batch_checks_pool_size_and_fences_slow_lease_write(self):
        self.mock_k8s_helper.api_pool_size.return_value = 2
        with self.assertRaises(ValueError):
            self.client.get_batch("b1")
        self.mock_k8s_helper.read_batch_lease.assert_not_called()

        self.mock_k8s_helper.api_pool_size.return_value = 8
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease.side_effect = lambda *_a, **_kw: _lease(
            holder_identity=None,
            renew_time=now,
            lease_duration_seconds=6,
            annotations={BATCH_LEASE_DURATION_ANNOTATION: "6"},
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        clock = [100.0]

        def slow_replace(*_args, **_kwargs):
            clock[0] += 6.0
            return _lease(holder_identity="h1", lease_duration_seconds=6)

        self.mock_k8s_helper.replace_batch_lease.side_effect = slow_replace
        with patch("k8s_agent_sandbox.sandbox_batch.time.monotonic", side_effect=lambda: clock[0]):
            with self.assertRaises(BatchLeaseExpiredError):
                self.client.get_batch("b1")
        self.assertEqual(
            self.mock_k8s_helper.replace_batch_lease.call_args_list[0].kwargs["_request_timeout"],
            6.0,
        )

        self.mock_k8s_helper.replace_batch_lease.side_effect = None
        self.mock_k8s_helper.replace_batch_lease.return_value = _lease(
            holder_identity="h1", lease_duration_seconds=6
        )
        attached = self.client.get_batch("b1")
        self.assertEqual(attached._in_flight._value, 8 - batch_utils.BATCH_POOL_HEADROOM)

    def test_replace_batch_lease_404_is_handled_across_attach_release_and_detach(self):
        now = datetime.now(UTC)
        not_found = k8s_client.ApiException(status=404, reason="Not Found")
        self.mock_k8s_helper.read_batch_lease.side_effect = lambda *_a, **_kw: _lease(
            holder_identity=None, renew_time=now
        )
        self.mock_k8s_helper.replace_batch_lease.side_effect = not_found

        self.mock_k8s_helper.list_sandbox_claim_objects.side_effect = [([], "5"), ([], "6")]
        with self.assertRaises(BatchNotFoundError):
            self.client.get_batch("b1")

        self.mock_k8s_helper.list_sandbox_claim_objects.side_effect = [([], "5"), ([_claim("b1-0")], "6")]
        with self.assertRaises(BatchLeaseExpiredError):
            self.client.get_batch("b1")

        handle_rel = _make_handle(self.client)
        self.mock_k8s_helper.read_batch_lease.side_effect = None
        self.mock_k8s_helper.read_batch_lease.return_value = handle_rel._lease
        handle_rel.release()
        self.assertTrue(handle_rel._released)
        self.mock_k8s_helper.delete_sandbox_claims_by_label.assert_called_once()
        self.mock_k8s_helper.delete_batch_lease.assert_not_called()

        handle_det = _make_handle(self.client)
        self.mock_k8s_helper.read_batch_lease.return_value = handle_det._lease
        handle_det.detach(grace=30)
        self.assertTrue(handle_det._lease_released)


@patch.object(SandboxBatch, "_start", lambda self: None)
class TestBatchStaleness(BaseBatchClientTest):

    def test_spec_duration_boundary_adoptable_then_expired_one_second_later(self):
        now = datetime.now(UTC)
        spec_duration = 30
        with patch("k8s_agent_sandbox.batch_utils.lease_now") as mock_now:
            mock_now.return_value = now
            self.mock_k8s_helper.read_batch_lease.return_value = _lease(
                holder_identity=None,
                renew_time=now - timedelta(seconds=spec_duration - CLOCK_SKEW_MARGIN),
                lease_duration_seconds=spec_duration,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
            )
            self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
            batch = self.client.get_batch("b1")
            self.assertIsNotNone(batch)

        with patch("k8s_agent_sandbox.batch_utils.lease_now") as mock_now2:
            mock_now2.return_value = now
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
    lease_renewed_at=None,
    lease_uid="lease-uid",
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
        lease=_lease(holder_identity="host_1234_abcd1234", uid=lease_uid),
        lease_renewed_at=time.monotonic() if lease_renewed_at is None else lease_renewed_at,
    )


@patch.object(SandboxBatch, "_start", lambda self: None)
class TestRequestTimeouts(BaseBatchClientTest):

    def test_get_batch_relist_and_detach_requests_carry_request_timeouts(self):
        helper = self.mock_k8s_helper
        helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC))
        helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        handle = self.client.get_batch("b1")
        self.assertEqual(helper.read_batch_lease.call_args.kwargs["_request_timeout"], 30)
        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.replace_batch_lease.call_args.kwargs["_request_timeout"], 30)

        helper.list_sandbox_claim_objects.reset_mock()
        helper.watch_sandbox_claims.side_effect = [
            k8s_client.ApiException(status=410),
            k8s_client.ApiException(status=403),
        ]
        handle._watch_loop()
        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["_request_timeout"], 90)

        helper.read_batch_lease.reset_mock()
        helper.replace_batch_lease.reset_mock()
        helper.read_batch_lease.return_value = _lease(holder_identity=handle._holder_identity)
        handle.detach()
        self.assertEqual(helper.read_batch_lease.call_args.kwargs["_request_timeout"], 30)
        self.assertEqual(helper.replace_batch_lease.call_args.kwargs["_request_timeout"], 30)


class _FakeClock:
    """Stands in for the handle's monotonic clock, so renewal timing is exact."""

    def __init__(self):
        self.now = 0.0

    def monotonic(self):
        return self.now


class _ClockStopEvent:
    """Stands in for ``_renew_stop``: each wait advances the fake clock instead of sleeping."""

    def __init__(self, clock):
        self._clock = clock
        self._waits = 0

    def wait(self, timeout):
        self._waits += 1
        if self._waits > 100:
            raise AssertionError("the renewal loop never stopped")
        self._clock.now += timeout
        return False

    def set(self):
        pass


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
        self.assertEqual(handle._state.get_member("b1-0").state, MemberState.LOST)

    def test_connection_is_closed_once_its_members_pod_is_gone(self):
        def claim(pod_ip="10.0.0.1", ready="True", reason=None, sandbox="sbx-1"):
            return _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": ready, "reason": reason}],
                sandbox={"name": sandbox, "podIPs": [pod_ip] if pod_ip else []},
            )

        cases = {
            "same pod IP": ([{"type": "MODIFIED", "object": claim()}], False),
            "new pod IP": ([{"type": "MODIFIED", "object": claim(pod_ip="10.0.0.2")}], True),
            "new sandbox on the same pod IP": ([{"type": "MODIFIED", "object": claim(sandbox="sbx-2")}], True),
            "not Ready on the same pod IP": ([{"type": "MODIFIED", "object": claim(ready="False")}], False),
            "no pod IP": ([{"type": "MODIFIED", "object": claim(pod_ip=None, ready="False")}], True),
            "failed": (
                [{"type": "MODIFIED", "object": claim(ready="False", reason="ClaimExpired")}], True,
            ),
            "deleted": ([{"type": "DELETED", "object": claim()}], True),
            "gone on re-list": (self._terminating_error(status=410), True),
        }
        for name, (first_watch, closed) in cases.items():
            with self.subTest(name):
                self.mock_sandbox_class.side_effect = lambda **kwargs: MagicMock(sandbox_id=kwargs["sandbox_id"])
                handle = _make_handle(self.client)
                handle._state.upsert_claim(claim())
                sandbox = handle.connect(handle.members()[0])
                self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
                    first_watch, self._terminating_error(),
                ]
                self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([], "9")
                handle._watch_loop()
                self.assertEqual(sandbox.close_connection.called, closed)
                self.assertEqual("b1-0" in handle._connected, not closed)
                self.assertEqual(handle._stale_connections, [])
                # The 403 that ends the loop sets err(), which connect() would raise first.
                handle._state._error = None
                if handle._state.get_member("b1-0").state is not MemberState.READY:
                    with self.assertRaises(SandboxNotReadyError):
                        handle.connect(handle.members()[0])

    def test_a_stale_connection_that_fails_to_close_is_retried_by_the_next_client_cleanup(self):
        def claim(pod_ip):
            return _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "True"}],
                sandbox={"name": "sbx-1", "podIPs": [pod_ip]},
            )

        self.mock_sandbox_class.side_effect = lambda **kwargs: MagicMock(sandbox_id=kwargs["sandbox_id"])
        handle = _make_handle(self.client)
        handle._state.upsert_claim(claim("10.0.0.1"))
        stale = handle.connect(handle.members()[0])
        stale.close_connection.side_effect = [RuntimeError("close failed"), None]
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            [{"type": "MODIFIED", "object": claim("10.0.0.2")}], self._terminating_error(),
        ]
        handle._watch_loop()
        self.assertEqual(self.client._closing_batches, {handle})
        # The 403 that ends the loop sets err(), which connect() would raise first.
        handle._state._error = None
        self.assertIsNot(handle.connect(handle.members()[0]), stale)

        self.client.delete_all()
        self.assertEqual(stale.close_connection.call_count, 2)
        self.assertEqual(handle._stale_connections, [])

    def test_a_stale_connection_another_caller_took_is_not_closed_again(self):
        self.mock_sandbox_class.side_effect = lambda **kwargs: MagicMock(sandbox_id=kwargs["sandbox_id"])
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=2)])
        for i in range(2):
            handle._state.upsert_claim(
                _claim(
                    f"b1-{i}",
                    conditions=[{"type": "Ready", "status": "True"}],
                    sandbox={"name": f"sbx-{i}", "podIPs": [f"10.0.0.{i}"]},
                )
            )
        sandboxes = [handle.connect(member) for member in handle.members()]
        closing, proceed = threading.Event(), threading.Event()

        def close_connection():
            closing.set()
            proceed.wait(5)

        # The watch blocks closing the first stale connection while client cleanup takes the second.
        sandboxes[0].close_connection.side_effect = close_connection
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            self._terminating_error(status=410), self._terminating_error(),
        ]
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([], "9")
        self.client._closing_batches.add(handle)
        watch = threading.Thread(target=handle._watch_loop)
        watch.start()
        self.assertTrue(closing.wait(5))
        self.client.delete_all()
        proceed.set()
        watch.join(5)

        sandboxes[0].close_connection.assert_called_once()
        sandboxes[1].close_connection.assert_called_once()
        self.assertEqual(handle._stale_connections, [])

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
        self.assertEqual(handle._state.get_member("b1-0").state, MemberState.LOST)
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
        self.assertNotEqual(handle._state.get_member("b1-0").state, MemberState.LOST)

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

    def _retry_then_stop(self, handle, status, headers=None):
        """A watch attempt raising ``status`` followed by one that succeeds
        with no events, at which point the test stops the loop from inside
        the mocked call itself (there is no third attempt, so ``err()``
        never gets a chance to be set by anything after the retry)."""
        calls = {"n": 0}

        def side_effect(*args, **kwargs):
            calls["n"] += 1
            if calls["n"] == 1:
                error = self._terminating_error(status=status)
                error.headers = headers
                raise error
            handle._watch_stop.set()
            return iter(())

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = side_effect

    def test_503_waits_retry_after_then_retries_from_last_resource_version(self):
        handle = _make_handle(self.client)
        self._retry_then_stop(handle, 503, headers={"Retry-After": "7"})
        with patch.object(handle._watch_stop, "wait") as wait:
            handle._watch_loop()
        self.assertIsNone(handle.err())
        wait.assert_called_once_with(7.0)
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

    def test_connection_failure_and_tls_eof_retry_but_certificate_errors_stop(self):
        refused = urllib3.exceptions.NewConnectionError(None, "refused")
        tls_eof = urllib3.exceptions.SSLError(ssl.SSLEOFError(8, "EOF occurred in violation of protocol"))
        bad_cert = urllib3.exceptions.SSLError(ssl.SSLCertVerificationError(1, "certificate verify failed"))
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = [
            urllib3.exceptions.MaxRetryError(None, "/", reason=refused),
            urllib3.exceptions.MaxRetryError(None, "/", reason=tls_eof),
            tls_eof,
            tls_eof.args[0],
            urllib3.exceptions.MaxRetryError(None, "/", reason=bad_cert),
        ]
        with patch.object(batch_utils, "retry_delay", return_value=0):
            handle._watch_loop()
        self.assertEqual(self.mock_k8s_helper.watch_sandbox_claims.call_count, 5)
        self.assertIs(handle.err().reason, bad_cert)

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
        self.assertEqual(call_args.args[1], f"{BATCH_ID_LABEL}=b1234,{BATCH_LEASE_UID_LABEL}=lease-uid")

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
        self.assertEqual(handle._state.get_member("b1-0").state, MemberState.LOST)
        self.assertIsNone(handle.err())
        self.assertEqual(len(self.mock_k8s_helper.list_sandbox_claim_objects.call_args_list), 2)

    def test_consecutive_failures_and_empty_watches_back_off_and_an_event_resets_the_delay(self):
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
            # A watch that ends at once with no events.
            iter(()),
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
        self.assertEqual(len(delays), 5)
        self.assertTrue(0.25 <= delays[0] <= 0.5, delays)
        self.assertTrue(0.5 <= delays[1] <= 1.0, delays)
        self.assertTrue(1.0 <= delays[2] <= 2.0, delays)
        self.assertTrue(2.0 <= delays[3] <= 4.0, delays)
        self.assertTrue(0.25 <= delays[4] <= 0.5, delays)
        self.assertIsNone(handle.err())

    def test_stop_background_threads_stops_and_joins_active_watch(self):
        handle = _make_handle(self.client)
        entered = threading.Event()
        stopped = threading.Event()
        mock_watch = MagicMock()
        mock_watch.stop.side_effect = stopped.set

        def blocking_watch(*args, on_watch=None, **kwargs):
            if on_watch is not None:
                on_watch(mock_watch)
            entered.set()
            try:
                stopped.wait(5)
                return
                yield
            finally:
                if on_watch is not None:
                    on_watch(None)

        self.mock_k8s_helper.watch_sandbox_claims.side_effect = blocking_watch
        handle._watch_thread = threading.Thread(target=handle._watch_loop, daemon=True)
        handle._watch_thread.start()
        self.assertTrue(entered.wait(5))

        started = time.monotonic()
        handle._stop_background_threads()
        self.assertLess(time.monotonic() - started, 1.0)
        mock_watch.stop.assert_called_once()
        self.assertFalse(handle._watch_thread.is_alive())
        self.assertIsNone(handle._active_watch)


class TestRenewal(BaseBatchClientTest):

    def _run_renew_loop(self, handle, clock, replace):
        handle._renew_stop = _ClockStopEvent(clock)
        self.mock_k8s_helper.replace_batch_lease.side_effect = replace
        # The nth retry in a row waits n seconds, so the timing is exact.
        with patch.object(sandbox_batch, "time", clock), patch.object(
            batch_utils, "backoff_delay", lambda failures, base, cap: failures
        ):
            handle._renew_loop()

    def test_expiry_fires_a_lease_duration_after_the_last_successful_renewal_was_sent(self):
        # A 10s Lease renews every 3s. The renewal sent at 3 succeeds with a 2s response, the
        # next at 8 fails at once, and its retry at 9 hangs until 12. The next retry waits only
        # until the Lease expires at 3 + 10 = 13.
        clock = _FakeClock()
        handle = _make_handle(self.client, lease_duration=10, lease_renewed_at=0.0)
        outcomes = iter(["slow", "fail", "hang"])

        def replace(name, namespace, body, _request_timeout):
            outcome = next(outcomes)
            if outcome == "slow":
                clock.now += 2
                return body
            if outcome == "hang":
                clock.now += _request_timeout
                raise TimeoutError("timed out")
            raise k8s_client.ApiException(status=503)

        self._run_renew_loop(handle, clock, replace)
        self.assertEqual(clock.now, 13)
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)

    def test_failed_renewals_retry_on_a_backoff_and_stop_at_expiry(self):
        clock = _FakeClock()
        handle = _make_handle(self.client, lease_duration=10, lease_renewed_at=0.0)
        sent = []

        def replace(name, namespace, body, _request_timeout):
            sent.append(clock.now)
            # Renewals fail until the Lease expires, and would succeed after it.
            if clock.now < 10:
                raise k8s_client.ApiException(status=503)
            return body

        self._run_renew_loop(handle, clock, replace)
        # The first failure at the 3s interval is retried after 1s, 2s and 3s, then the Lease
        # expires at 10 before the next retry.
        self.assertEqual(sent, [3, 4, 6, 9])
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)
        self.assertTrue(handle.lease_degraded())

    @patch.object(SandboxBatch, "_start", lambda self: None)
    def test_renewals_replace_the_last_written_lease_without_reading_it(self):
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            holder_identity=None, renew_time=datetime.now(UTC)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        written = [_lease(holder_identity=None, resource_version=rv) for rv in ("11", "12", "13")]
        self.mock_k8s_helper.replace_batch_lease.side_effect = written
        handle = self.client.get_batch("b1")

        self.assertTrue(handle._renew_once())
        self.assertTrue(handle._renew_once())
        bodies = [c.args[2] for c in self.mock_k8s_helper.replace_batch_lease.call_args_list]
        self.assertIs(bodies[1], written[0])
        self.assertIs(bodies[2], written[1])
        # Only get_batch read the Lease.
        self.mock_k8s_helper.read_batch_lease.assert_called_once()

    def test_conflict_reads_the_lease_and_renews_it_while_still_held(self):
        handle = _make_handle(self.client)
        current = _lease(holder_identity=handle._holder_identity, resource_version="200")
        self.mock_k8s_helper.replace_batch_lease.side_effect = [
            k8s_client.ApiException(status=409),
            current,
        ]
        self.mock_k8s_helper.read_batch_lease.return_value = current
        self.assertTrue(handle._renew_once())
        self.assertIs(self.mock_k8s_helper.replace_batch_lease.call_args.args[2], current)
        self.assertFalse(handle.lease_degraded())
        self.assertIsNone(handle.err())

    def test_lease_degraded_from_the_first_failed_renewal_until_one_succeeds(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.replace_batch_lease.side_effect = [
            RuntimeError("api down"),
            RuntimeError("api down"),
            _lease(holder_identity=handle._holder_identity),
        ]
        self.assertFalse(handle.lease_degraded())
        with self.assertLogs(level="INFO") as ctx:
            handle._renew_once()
        self.assertTrue(any("degraded" in msg for msg in ctx.output))
        self.assertTrue(handle.lease_degraded())
        handle._renew_once()
        self.assertTrue(handle.lease_degraded())
        handle._renew_once()
        self.assertFalse(handle.lease_degraded())

    def test_holder_mismatch_stops_renewal_without_writing(self):
        for other_holder in ("someone-else", None):
            with self.subTest(other_holder=other_holder):
                self.mock_k8s_helper.reset_mock()
                handle = _make_handle(self.client)
                self.mock_k8s_helper.replace_batch_lease.side_effect = k8s_client.ApiException(
                    status=409
                )
                self.mock_k8s_helper.read_batch_lease.return_value = _lease(
                    holder_identity=other_holder
                )
                self.assertFalse(handle._renew_once())
                self.mock_k8s_helper.replace_batch_lease.assert_called_once()
                self.assertIsInstance(handle.err(), BatchInUseError)
                self.assertTrue(handle.lease_degraded())

    def test_missing_lease_stops_renewal_and_records_expired(self):
        # The Lease is gone if the replace finds nothing, or a conflict's read does.
        for replace_status in (404, 409):
            with self.subTest(replace_status=replace_status):
                self.mock_k8s_helper.reset_mock()
                handle = _make_handle(self.client)
                self.mock_k8s_helper.replace_batch_lease.side_effect = k8s_client.ApiException(
                    status=replace_status
                )
                self.mock_k8s_helper.read_batch_lease.return_value = None
                self.assertFalse(handle._renew_once())
                self.mock_k8s_helper.replace_batch_lease.assert_called_once()
                self.assertIsInstance(handle.err(), BatchLeaseExpiredError)
                # A new Lease would get a new UID, so the garbage collector would delete the claims.
                self.mock_k8s_helper.create_batch_lease.assert_not_called()

    def test_local_lease_expiry_fences_operations_before_renew_loop_wakes(self):
        operations = {
            "connect": lambda h: h.connect("b1-0"),
            "acquire known pool": lambda h: h.acquire("pool-a"),
            "acquire unseen pool": lambda h: h.acquire("pool-unseen"),
            "release_member": lambda h: h.release_member("b1-0"),
            "release_not_ready": lambda h: h.release_not_ready(),
            "release_group": lambda h: h.release_group("pool-a"),
        }
        for name, call in operations.items():
            with self.subTest(operation=name):
                self.mock_k8s_helper.reset_mock()
                self.mock_sandbox_class.reset_mock()
                handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=2)])
                handle._state.upsert_claim(
                    _claim(
                        "b1-0",
                        warmpool="pool-a",
                        conditions=[{"type": "Ready", "status": "True"}],
                        sandbox={"name": "sbx-0", "podIPs": ["10.0.0.1"]},
                    )
                )
                handle._state.upsert_claim(_claim("b1-1", warmpool="pool-a"))
                handle._lease_expires_at = time.monotonic() - 0.01
                with self.assertRaises(BatchError) as ctx:
                    call(handle)
                self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
                self.assertIs(handle.err(), ctx.exception.__cause__)
                self.assertTrue(handle.lease_degraded())
                self.assertTrue(handle._renew_stop.is_set())
                self.assertEqual(self.mock_k8s_helper.mock_calls, [])
                self.mock_sandbox_class.assert_not_called()

    def test_local_lease_expiry_mid_acquire_and_quorum_wait_fences_handle(self):
        with self.subTest("pacing_wait"):
            self.mock_k8s_helper.reset_mock()
            handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
            handle._warm_credit.record("pool-a", 0)
            handle._next_create_at = time.monotonic() + 10.0

            def expire_during_pacing(_timeout, _cancel=None):
                handle._lease_expires_at = time.monotonic() - 0.01
                return False

            with patch.object(handle._stop_creating, "wait_or", side_effect=expire_during_pacing):
                with self.assertRaises(BatchError) as ctx:
                    handle.acquire("pool-a", timeout=60)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
            self.mock_k8s_helper.create_sandbox_claim.assert_not_called()
            self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

        with self.subTest("ready_arrived_before_wait_acquired"):
            self.mock_k8s_helper.reset_mock()
            handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
            handle._warm_credit.record("pool-a", 1)

            def create_then_expire(claim_name, warmpool, *_args, **_kwargs):
                handle._state.upsert_claim(
                    _claim(
                        claim_name,
                        warmpool=warmpool,
                        conditions=[{"type": "Ready", "status": "True"}],
                        sandbox={"name": "sbx-0", "podIPs": ["10.0.0.1"]},
                    )
                )
                handle._lease_expires_at = time.monotonic() - 0.01

            self.mock_k8s_helper.create_sandbox_claim.side_effect = create_then_expire
            with self.assertRaises(BatchError) as ctx:
                handle.acquire("pool-a", timeout=60)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
            self.mock_k8s_helper.create_sandbox_claim.assert_called_once()
            self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

        with self.subTest("sleeping_wait_acquired_and_quorum"):
            self.mock_k8s_helper.reset_mock()
            self.mock_k8s_helper.create_sandbox_claim.side_effect = None
            handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=1)])
            handle._warm_credit.record("pool-a", 1)
            handle._lease_expires_at = time.monotonic() + 0.03
            started = time.monotonic()
            with self.assertRaises(BatchError) as ctx:
                handle.acquire("pool-a", timeout=60)
            self.assertLess(time.monotonic() - started, 2.0)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
            self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()

            quorum_handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=1)])
            quorum_handle._lease_expires_at = time.monotonic() + 0.03
            started = time.monotonic()
            with self.assertRaises(BatchError) as ctx:
                quorum_handle.wait_for_quorum()
            self.assertLess(time.monotonic() - started, 2.0)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)

    def test_late_renewal_after_local_expiry_keeps_lease_degraded_and_error(self):
        handle = _make_handle(self.client)
        expired_deadline: list[float] = []

        def replace_after_local_fence(*_args, **_kwargs):
            handle._lease_expires_at = time.monotonic() - 0.01
            expired_deadline.append(handle._lease_expires_at)
            with self.assertRaises(BatchError):
                handle._check_lease_held()
            return _lease(holder_identity=handle._holder_identity, resource_version="99")

        self.mock_k8s_helper.replace_batch_lease.side_effect = replace_after_local_fence
        self.assertFalse(handle._renew_once())
        self.assertTrue(handle.lease_degraded())
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)
        self.assertEqual(handle._lease_expires_at, expired_deadline[0])

    def test_remaining_lease_budget_caps_claim_and_pool_request_timeouts(self):
        handle = _make_handle(
            self.client,
            groups=[BatchGroup(warmpool="pool-a", size=0)],
            lease_duration=6,
        )
        handle._lease_expires_at = time.monotonic() + 4.0
        self.mock_k8s_helper.get_sandbox_warmpool.return_value = {
            "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
            "status": {"readyReplicas": 1},
        }
        self.mock_k8s_helper.get_sandbox_template.return_value = {"metadata": {"name": "tmpl"}}

        def create_ready(claim_name, warmpool, *_args, **_kwargs):
            with handle._lock:
                handle._state.upsert_claim(
                    _claim(
                        claim_name,
                        warmpool=warmpool,
                        conditions=[{"type": "Ready", "status": "True"}],
                        sandbox={"name": f"sbx-{claim_name}", "podIPs": ["10.0.0.1"]},
                    )
                )
                handle._wake_acquire(claim_name)

        self.mock_k8s_helper.create_sandbox_claim.side_effect = create_ready
        member = handle.acquire("pool-unseen", timeout=60)
        pool_timeout = self.mock_k8s_helper.get_sandbox_warmpool.call_args.kwargs["_request_timeout"]
        template_timeout = self.mock_k8s_helper.get_sandbox_template.call_args.kwargs[
            "_request_timeout"
        ]
        create_timeout = self.mock_k8s_helper.create_sandbox_claim.call_args.kwargs["_request_timeout"]
        self.assertGreater(pool_timeout, 0)
        self.assertLessEqual(pool_timeout, 4.0)
        self.assertGreater(template_timeout, 0)
        self.assertLessEqual(template_timeout, 4.0)
        self.assertGreater(create_timeout, 0)
        self.assertLessEqual(create_timeout, 4.0)

        handle.release_member(member)
        delete_timeout = self.mock_k8s_helper.delete_sandbox_claim.call_args.kwargs["_request_timeout"]
        self.assertGreater(delete_timeout, 0)
        self.assertLessEqual(delete_timeout, 4.0)

    def test_watch_error_keeps_lease_renewal_and_cleanup_while_lease_loss_stops_watch(self):
        handle = _make_handle(self.client)
        ready = [{"type": "Ready", "status": "True"}]
        handle._state.upsert_claim(
            _claim("b1-0", conditions=ready, sandbox={"name": "sbx-0", "podIPs": ["10.0.0.1"]}, annotations=_FILL)
        )
        handle._state.upsert_claim(
            _claim("b1-1", conditions=ready, sandbox={"name": "sbx-1", "podIPs": ["10.0.0.2"]}, annotations=_FILL)
        )
        first, second = handle.members()

        watch_error = k8s_client.ApiException(status=403)
        self.mock_k8s_helper.watch_sandbox_claims.side_effect = watch_error
        handle._watch_loop()
        self.assertIs(handle.err(), watch_error)
        self.assertFalse(handle.lease_degraded())
        self.assertFalse(handle._stop_creating.is_set())
        with self.assertRaises(BatchError) as cm:
            handle.connect(first)
        self.assertIs(cm.exception.__cause__, watch_error)

        self.mock_k8s_helper.replace_batch_lease.return_value = _lease(resource_version="25")
        self.assertTrue(handle._renew_once())
        self.assertEqual(handle._lease.metadata.resource_version, "25")

        handle.release_member(first)
        self.mock_k8s_helper.delete_sandbox_claim.assert_called_once()
        self.assertEqual(self.mock_k8s_helper.delete_sandbox_claim.call_args.args[0], "b1-0")

        mock_watch = MagicMock()
        handle._active_watch = mock_watch
        self.mock_k8s_helper.delete_sandbox_claim.reset_mock()
        self.mock_k8s_helper.replace_batch_lease.side_effect = k8s_client.ApiException(status=404)
        self.assertFalse(handle._renew_once())
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)
        self.assertTrue(handle._stop_creating.is_set())
        self.assertTrue(handle._watch_stop.is_set())
        mock_watch.stop.assert_called_once()
        with self.assertRaises(BatchError):
            handle.release_member(second)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_called()


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
        self.assertEqual(first.state, MemberState.PENDING)


class TestConnect(unittest.TestCase):

    def setUp(self):
        self.mock_client = MagicMock()
        self.mock_sandbox_instance = MagicMock()
        self.mock_client.sandbox_class.return_value = self.mock_sandbox_instance
        self.handle = _make_handle(self.mock_client)

    def test_builds_sandbox_class_without_resolve_or_get_sandbox(self):
        self.handle._state.upsert_claim(
            _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "True"}],
                sandbox={"name": "sbx-1", "podIPs": ["10.0.0.1"]},
            )
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
        self.assertEqual(sandbox._seeded_pod_ip, "10.0.0.1")

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

    def test_member_no_longer_ready_raises_even_when_cached(self):
        failed = {"type": "Ready", "status": "False", "reason": "ClaimExpired"}
        updates = {
            MemberState.PENDING: lambda: self.handle._state.upsert_claim(_claim("b1-0", conditions=[])),
            MemberState.FAILED: lambda: self.handle._state.upsert_claim(_claim("b1-0", conditions=[failed])),
            MemberState.LOST: lambda: self.handle._state.mark_lost("b1-0"),
        }
        for state, update in updates.items():
            with self.subTest(state=state):
                self.handle._state.upsert_claim(
                    _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
                )
                member = self.handle.members()[0]
                self.handle.connect(member)
                update()
                with self.assertRaises(SandboxNotReadyError):
                    self.handle.connect(member)

    def test_connect_after_err_raises_even_when_cached(self):
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        self.handle.connect(member)
        lease_lost = BatchLeaseExpiredError("lost")
        self.handle._state.note_error(lease_lost)
        with self.assertRaises(BatchError) as raised:
            self.handle.connect(member)
        self.assertIs(raised.exception.__cause__, lease_lost)

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
        # The batch's one claim took ordinal 0, so a re-attached handle continues from 1.
        self.assertEqual(
            written.metadata.annotations,
            {
                BATCH_LEASE_DURATION_ANNOTATION: str(BATCH_DEFAULT_LEASE_DURATION_SECONDS),
                BATCH_NEXT_ORDINAL_ANNOTATION: "1",
            },
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
        self.handle._connected["b1-0"] = (mock_sandbox, None)

        self.handle.detach()

        mock_sandbox.close_connection.assert_called_once()
        self.mock_client._unregister_batch.assert_called_once_with(self.handle)
        self.assertEqual(self.handle._connected, {})

        self.handle.detach()  # Second call: no exception, and does nothing.

        mock_sandbox.close_connection.assert_called_once()
        self.mock_client._unregister_batch.assert_called_once_with(self.handle)
        self.mock_client.k8s_helper.read_batch_lease.assert_called_once()
        self.mock_client.k8s_helper.replace_batch_lease.assert_called_once()

    def test_release_conflict_succeeds_only_once_the_lease_is_not_held(self):
        # A write from an earlier detach() call that timed out but landed, or a detach() that re-entered on
        # this thread, can release the Lease between this call's read and write, and a late renewal of this
        # handle can leave it held, which detach() releases with one more write.
        for after_conflict, writes, released in (
            ("released", [409], True),
            ("deleted", [409], True),
            ("another holder", [409], True),
            ("this handle", [409, 200], True),
            ("this handle", [409, 409], False),
            ("this handle", [409, 500], False),
        ):
            with self.subTest(after_conflict=after_conflict, writes=writes):
                mock_client = MagicMock()
                handle = _make_handle(mock_client)
                first = _lease(holder_identity=handle._holder_identity)
                current = {
                    "released": _lease(holder_identity=None),
                    "deleted": None,
                    "another holder": _lease(holder_identity="other"),
                    "this handle": _lease(holder_identity=handle._holder_identity),
                }[after_conflict]
                mock_client.k8s_helper.read_batch_lease.side_effect = [first, current]
                mock_client.k8s_helper.replace_batch_lease.side_effect = [
                    k8s_client.ApiException(status=status) if status != 200 else None for status in writes
                ]

                if released:
                    handle.detach(grace=17)
                else:
                    with self.assertRaises(k8s_client.ApiException):
                        handle.detach(grace=17)
                self.assertEqual(handle._lease_released, released)
                replace = mock_client.k8s_helper.replace_batch_lease
                self.assertEqual(replace.call_count, len(writes))
                written = replace.call_args.args[2]
                self.assertIs(written, current if len(writes) == 2 else first)
                self.assertIsNone(written.spec.holder_identity)
                self.assertEqual(written.spec.lease_duration_seconds, 17)
                self.assertEqual(written.metadata.annotations[BATCH_NEXT_ORDINAL_ANNOTATION], "1")

    def test_another_detach_during_the_write_keeps_this_calls_grace(self):
        # Another detach(grace=99) stores its grace while this call writes the Lease, and this call's
        # writes must keep the grace it started with.
        current = _lease(holder_identity=self.handle._holder_identity)

        def other_detach_then_conflict(*args, **kwargs):
            if self.handle._detach_grace != 99:
                self.handle._detach_grace = 99
                raise k8s_client.ApiException(status=409)

        self.mock_client.k8s_helper.read_batch_lease.side_effect = [
            _lease(holder_identity=self.handle._holder_identity), current
        ]
        self.mock_client.k8s_helper.replace_batch_lease.side_effect = other_detach_then_conflict

        self.handle.detach(grace=17)

        self.assertEqual(current.spec.lease_duration_seconds, 17)

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
        self.handle._connected["b1-0"] = (bad_sandbox, None)

        with self.assertLogs(level="WARNING"):
            self.handle.detach()  # Must not raise.

        self.mock_client.k8s_helper.replace_batch_lease.assert_called_once()

    def test_a_connection_whose_close_is_interrupted_stays_on_the_handle(self):
        cached = MagicMock()
        cached.close_connection.side_effect = KeyboardInterrupt
        self.handle._connected["b1-0"] = (cached, None)

        with self.assertRaises(KeyboardInterrupt):
            self.handle._close_cached_connections("detach")

        self.assertEqual(self.handle._connected, {"b1-0": (cached, None)})
        self.mock_client._track_closing.assert_called_once_with(self.handle)

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
# Fill claims carry their group's annotations. The batch state only checks that they are there.
_FILL = {BATCH_GROUP_SIZE_ANNOTATION: "1", BATCH_GROUP_MIN_READY_ANNOTATION: "1"}


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
        # A Lease no handle holds, so release() takes it for its handle.
        h.read_batch_lease.return_value = _lease()
        h.create_batch_lease.return_value = _lease()
        h.replace_batch_lease.return_value = _lease()
        h.count_live_sandbox_claims.return_value = 0
        h.api_pool_size.return_value = None
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
            self.push(name, warmpool, annotations=kwargs["annotations"])

    def push(self, name, warmpool="pool-a", ready=True, annotations=_FILL):
        conditions = _READY if ready else _NOT_READY
        sandbox = {"name": f"sbx-{name}"} if ready else None
        claim = _claim(name, warmpool, conditions=conditions, sandbox=sandbox, annotations=annotations)
        self.watch_queue.put({"type": "MODIFIED", "object": claim})

    def names(self):
        return [name for name, _ in self.created]


class _SteppingDatetime(datetime):
    """Each now() is an hour after the last, so claims whose shutdownTime came from separate
    now() calls would differ."""

    calls = 0

    @classmethod
    def now(cls, tz=None):
        cls.calls += 1
        return datetime(2026, 1, 1, tzinfo=UTC) + timedelta(hours=cls.calls)


def _drain(iterable, timeout=5.0):
    """Collects an iterator on another thread, failing instead of hanging if it doesn't end."""
    items: list = []
    errors: list = []

    def run():
        try:
            items.extend(iterable)
        except Exception as e:
            errors.append(e)

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    thread.join(timeout)
    if thread.is_alive():
        raise AssertionError("iterator did not end in time")
    if errors:
        raise errors[0]
    return items


def _wait_until(condition, timeout=5.0):
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        time.sleep(0.01)


class _SignalingLock:
    """Wraps a lock for a race test. ``start_racing`` runs a call on another thread and returns
    once that thread waits for this lock or finishes, so the test needs no timed wait."""

    def __init__(self, lock):
        self._lock = lock
        self._blocked_or_done = threading.Event()
        self.threads: list[threading.Thread] = []

    def __enter__(self):
        if not self._lock.acquire(blocking=False):
            self._blocked_or_done.set()
            self._lock.acquire()

    def __exit__(self, *exc_info):
        self._lock.release()

    def start_racing(self, target, *args):
        def run():
            try:
                target(*args)
            finally:
                self._blocked_or_done.set()

        thread = threading.Thread(target=run)
        self.threads.append(thread)
        thread.start()
        self._blocked_or_done.wait(5)


class BaseClaimBatchTest(BaseBatchClientTest):

    def setUp(self):
        super().setUp()
        self.cluster = _Cluster()
        self.client.k8s_helper = self.cluster.helper
        # The fake's watch ends as soon as it has no events, which an apiserver's watch never does.
        short_watch = patch.object(sandbox_batch, "_SHORT_WATCH_SECONDS", 0)
        short_watch.start()
        self.addCleanup(short_watch.stop)

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
        self.cluster.helper.create_batch_lease.side_effect = (
            lambda *a, **k: self.cluster.calls.append("lease") or _lease()
        )
        self.cluster.helper.list_sandbox_claim_objects.side_effect = (
            lambda *a, **k: self.cluster.calls.append("list") or ([], "7")
        )
        batch = self.claim(
            [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
            labels={"team": "rl"},
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
            {"team": "rl", BATCH_ID_LABEL: "b1", "agents.x-k8s.io/created-by": "python-client"},
        )
        self.assertEqual(
            lease.metadata.annotations,
            {
                BATCH_LEASE_DURATION_ANNOTATION: "45",
                BATCH_WORK_BUDGET_ANNOTATION: "120",
                BATCH_QUORUM_TIMEOUT_ANNOTATION: "90",
                BATCH_CREATE_RPS_ANNOTATION: "1000",
            },
        )
        self.assertEqual(lease.spec.holder_identity, batch._holder_identity)
        self.assertEqual(lease.spec.lease_duration_seconds, 45)
        self.assertEqual(lease.spec.lease_transitions, 0)
        self.assertLess(abs((lease.spec.renew_time - datetime.now(UTC)).total_seconds()), 5)

    def test_claim_manifests(self):
        _SteppingDatetime.calls = 0
        with patch("k8s_agent_sandbox.utils.datetime", _SteppingDatetime):
            self.claim(
                [BatchGroup(warmpool="pool-a", size=2, min_ready=1), BatchGroup(warmpool="pool-b", size=1)],
                labels={"team": "rl"},
                create_rps=2.5,
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
        # The group label is a hash of the pool name, which other SDKs must compute the same way.
        self.assertEqual(
            [c.kwargs["labels"] for c in calls],
            [
                {
                    "team": "rl",
                    BATCH_ID_LABEL: "b1",
                    BATCH_LEASE_UID_LABEL: "lease-uid",
                    BATCH_GROUP_LABEL: "8934303dc131d111",
                }
            ]
            * 2
            + [
                {
                    "team": "rl",
                    BATCH_ID_LABEL: "b1",
                    BATCH_LEASE_UID_LABEL: "lease-uid",
                    BATCH_GROUP_LABEL: "574c3c5032ce922a",
                }
            ],
        )
        self.assertEqual(
            kwargs["annotations"],
            {BATCH_GROUP_SIZE_ANNOTATION: "2", BATCH_GROUP_MIN_READY_ANNOTATION: "1"},
        )
        # One shutdownTime for the fill, counting ceil(3 / 2.5) seconds of pacing.
        lifecycle = {"shutdownTime": "2026-01-01T01:13:32Z", "shutdownPolicy": "Delete"}
        self.assertEqual([c.kwargs["lifecycle"] for c in calls], [lifecycle] * 3)
        self.assertEqual(kwargs["log_level"], logging.DEBUG)

    def test_precheck_failure_writes_nothing(self):
        helper = self.cluster.helper
        cases = {
            "missing warm pool": ("get_sandbox_warmpool", None, {}, SandboxWarmPoolNotFoundError),
            "missing template": ("get_sandbox_template", None, {}, SandboxTemplateNotFoundError),
            "warm pool names no template": (
                "get_sandbox_warmpool", {"spec": {}}, {}, SandboxTemplateNotFoundError
            ),
            "forbidden": (
                "get_sandbox_warmpool",
                k8s_client.ApiException(status=403),
                {},
                k8s_client.ApiException,
            ),
            "explicit max_in_flight above pool_size - 2": (
                "api_pool_size", 21, {"max_in_flight": 20}, ValueError
            ),
            "connection pool below 3": ("api_pool_size", 2, {}, ValueError),
        }
        for name, (method, result, kwargs, error) in cases.items():
            with self.subTest(name):
                helper.reset_mock()
                helper.api_pool_size.return_value = None
                helper.get_sandbox_warmpool.return_value = {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
                helper.get_sandbox_template.return_value = {}
                helper.get_sandbox_warmpool.side_effect = None
                if isinstance(result, Exception):
                    getattr(helper, method).side_effect = result
                else:
                    getattr(helper, method).return_value = result
                with self.assertRaises(error):
                    self.client.claim_batch([BatchGroup(warmpool="pool-a", size=1)], **kwargs)
                if method == "api_pool_size":
                    helper.get_sandbox_warmpool.assert_not_called()
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
            ("list interrupted", KeyboardInterrupt(), KeyboardInterrupt),
        ):
            with self.subTest(name):
                helper.delete_batch_lease.reset_mock()
                helper.create_batch_lease.return_value = _lease(resource_version="77")
                if isinstance(list_result, BaseException):
                    helper.list_sandbox_claim_objects.side_effect = list_result
                else:
                    helper.list_sandbox_claim_objects.side_effect = None
                    helper.list_sandbox_claim_objects.return_value = list_result
                with self.assertRaises(error):
                    self.claim()
                self.assertEqual(helper.delete_batch_lease.call_args.args[:2], ("batch-b1", "default"))
                self.assertEqual(helper.delete_batch_lease.call_args.kwargs["resource_version"], "77")
        self.assertEqual(self.cluster.created, [])

    def test_setup_list_deadline_and_fenced_rollback_against_replacement_lease(self):
        from k8s_agent_sandbox.k8s_helper import K8sHelper

        helper = self.cluster.helper
        helper.create_batch_lease.return_value = _lease(resource_version="77")

        real_helper = K8sHelper.__new__(K8sHelper)
        real_helper.coordination_v1_api = MagicMock()
        real_helper.coordination_v1_api.delete_namespaced_lease.side_effect = k8s_client.ApiException(
            status=409
        )
        helper.delete_batch_lease.side_effect = real_helper.delete_batch_lease

        read_timeout = urllib3.exceptions.ReadTimeoutError(None, "/", "read timed out")
        for label, exc, ticks, expected_error in (
            ("timeout before lease expiry", TimeoutError("page timed out"), [100.0, 120.0], TimeoutError),
            ("transport error before lease expiry", read_timeout, [100.0, 120.0], urllib3.exceptions.ReadTimeoutError),
            (
                "deadline timeout at lease expiry",
                TimeoutError("SandboxClaim list exceeded its deadline"),
                [100.0, 145.0],
                BatchLeaseExpiredError,
            ),
            ("read timeout at lease expiry", read_timeout, [100.0, 145.0], BatchLeaseExpiredError),
        ):
            with self.subTest(label):
                real_helper.coordination_v1_api.delete_namespaced_lease.reset_mock()
                helper.list_sandbox_claim_objects.side_effect = exc
                with patch("k8s_agent_sandbox.sandbox_batch.time.monotonic", side_effect=ticks):
                    with self.assertRaises(expected_error) as raised:
                        self.claim(lease_duration=45)
                if expected_error is BatchLeaseExpiredError:
                    self.assertIs(raised.exception.__cause__, exc)
                self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["deadline"], 145.0)
                delete_call = real_helper.coordination_v1_api.delete_namespaced_lease.call_args
                self.assertEqual(delete_call.args[:2], ("batch-b1", "default"))
                self.assertEqual(delete_call.kwargs["body"].preconditions.resource_version, "77")
                self.assertEqual(self.client._active_batches, {})
                helper.watch_sandbox_claims.assert_not_called()
                helper.replace_batch_lease.assert_not_called()
                helper.create_sandbox_claim.assert_not_called()

    def test_setup_lease_expiry_after_list_rolls_back_and_aborts(self):
        helper = self.cluster.helper
        helper.create_batch_lease.return_value = _lease(resource_version="77")
        helper.list_sandbox_claim_objects.side_effect = None
        helper.list_sandbox_claim_objects.return_value = ([], "7")

        with patch("k8s_agent_sandbox.sandbox_batch.time.monotonic", side_effect=[100.0, 145.0]):
            with self.assertRaises(BatchLeaseExpiredError):
                self.claim(lease_duration=45)

        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["deadline"], 145.0)
        helper.delete_batch_lease.assert_called_once()
        self.assertEqual(helper.delete_batch_lease.call_args.args[:2], ("batch-b1", "default"))
        self.assertEqual(helper.delete_batch_lease.call_args.kwargs["resource_version"], "77")
        self.assertEqual(self.client._active_batches, {})
        helper.watch_sandbox_claims.assert_not_called()
        helper.replace_batch_lease.assert_not_called()
        helper.create_sandbox_claim.assert_not_called()

    def test_setup_validates_created_lease_resource_version_and_uid(self):
        helper = self.cluster.helper
        for rv in (None, ""):
            with self.subTest(missing_rv=rv):
                helper.delete_batch_lease.reset_mock()
                helper.list_sandbox_claim_objects.reset_mock()
                helper.create_batch_lease.return_value = _lease(resource_version=rv)
                with self.assertRaisesRegex(BatchError, "no resourceVersion"):
                    self.claim()
                helper.delete_batch_lease.assert_not_called()
                helper.list_sandbox_claim_objects.assert_not_called()

        for uid in (None, ""):
            with self.subTest(missing_uid=uid):
                helper.delete_batch_lease.reset_mock()
                helper.list_sandbox_claim_objects.reset_mock()
                helper.create_batch_lease.return_value = _lease(resource_version="77", uid=uid)
                with self.assertRaisesRegex(BatchError, "no name or UID"):
                    self.claim()
                helper.list_sandbox_claim_objects.assert_not_called()
                helper.delete_batch_lease.assert_called_once()
                self.assertEqual(helper.delete_batch_lease.call_args.kwargs["resource_version"], "77")


class TestClaimBatchCreation(BaseClaimBatchTest):

    def test_creates_are_bounded_by_max_in_flight_and_paced_by_create_rps(self):
        for name, batch_id, pool_size, kwargs in (
            ("explicit", "b1", None, {"max_in_flight": 2}),
            ("clamped to pool_size - 2", "b2", 4, {}),
        ):
            with self.subTest(name):
                self.cluster.created.clear()
                self.cluster.helper.create_sandbox_claim.reset_mock()
                self.cluster.helper.api_pool_size.return_value = pool_size
                gate = threading.Event()
                self.addCleanup(gate.set)
                self.cluster.create_hook = lambda _: gate.wait(5)
                log_check = self.assertNoLogs if pool_size is None else self.assertLogs
                with log_check("k8s_agent_sandbox.batch_utils", level="INFO"):
                    self.claim(
                        [BatchGroup(warmpool="pool-a", size=5)], batch_id=batch_id, **kwargs
                    )
                _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count >= 2)
                time.sleep(0.2)
                self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 2)
                gate.set()
                _wait_until(lambda: len(self.cluster.created) == 5)

        self.cluster.helper.api_pool_size.return_value = None
        self.cluster.create_hook = None
        self.cluster.created.clear()
        start = time.monotonic()
        self.claim([BatchGroup(warmpool="pool-a", size=5)], batch_id="b3", create_rps=20)
        _wait_until(lambda: len(self.cluster.created) == 5)
        starts = sorted(t for _, t in self.cluster.created)
        for k, t in enumerate(starts):
            self.assertGreaterEqual(t - start, k * 0.05 - 0.001)

    def test_creates_the_warm_pool_covers_skip_pacing(self):
        ready = {"pool-a": 5, "pool-b": 0}
        self.cluster.helper.get_sandbox_warmpool.side_effect = lambda name, *a, **k: {
            "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
            "status": {"readyReplicas": ready[name]},
        }
        start = time.monotonic()
        self.claim(
            [BatchGroup(warmpool="pool-a", size=3), BatchGroup(warmpool="pool-b", size=3)],
            max_in_flight=6,
            create_rps=4,
        )
        _wait_until(lambda: len(self.cluster.created) == 6)
        started = {name: t - start for name, t in self.cluster.created}
        # pool-a's three claims are covered. pool-b's first takes the first pacing slot at once, and
        # pool-a's spare credit doesn't cover pool-b's others.
        for name in ("b1-0", "b1-1", "b1-2", "b1-3"):
            self.assertLess(started[name], 0.15, name)
        self.assertGreaterEqual(started["b1-4"], 0.249)
        self.assertGreaterEqual(started["b1-5"], 0.499)

        # When an earlier cold group reserves future pacing slots and the final planned claim
        # belongs to a warm-covered group, _fill_deadline counts from the latest fill slot rather
        # than now, and _drop_planned_creates ignores later slots that acquire() reserved.
        ready["pool-a"] = 5
        ready["pool-b"] = 0
        start = time.monotonic()
        mixed = self.claim(
            [BatchGroup(warmpool="pool-b", size=3, min_ready=1), BatchGroup(warmpool="pool-a", size=2, min_ready=1)],
            batch_id="b2",
            max_in_flight=5,
            create_rps=0.5,
            quorum_timeout=10,
        )
        _wait_until(lambda: mixed._fill_deadline is not None)
        mixed._stop_background_threads()
        self.assertGreaterEqual(mixed._last_fill_slot - start, 3.9)
        self.assertEqual(mixed._fill_deadline, mixed._last_fill_slot + 10)

        with patch.object(SandboxBatch, "_create_worker", lambda self: None):
            dropped = self.claim(
                [BatchGroup(warmpool="pool-b", size=2, min_ready=1)],
                batch_id="b3",
                max_in_flight=1,
                create_rps=0.5,
                quorum_timeout=10,
            )
        with dropped._lock:
            now = time.monotonic()
            dropped._last_fill_slot = now + 2.0
            dropped._next_create_at = now + 20.0
            dropped._drop_planned_creates("pool-b")
        self.assertEqual(dropped._fill_deadline, dropped._last_fill_slot + 10)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    def test_first_429_paces_creates_during_cooldown_and_recovers(self, _):
        helper = self.cluster.helper
        helper.get_sandbox_warmpool.return_value = {
            "spec": {"sandboxTemplateRef": {"name": "tmpl"}}, "status": {"readyReplicas": 6}
        }
        for status, paced in ((429, True), (503, False)):
            with self.subTest(status):
                sent_at = []
                outcomes = iter([k8s_client.ApiException(status=status)])

                def create(*args, **kwargs):
                    sent_at.append(time.monotonic())
                    if (outcome := next(outcomes, None)) is not None:
                        raise outcome

                helper.create_sandbox_claim.side_effect = create
                self.claim(
                    [BatchGroup(warmpool="pool-a", size=4)], batch_id=f"b{status}", max_in_flight=1, create_rps=5
                )
                _wait_until(lambda: len(sent_at) == 5)
                gaps = [later - earlier for earlier, later in zip(sent_at[1:], sent_at[2:])]
                if paced:
                    self.assertTrue(all(gap >= 0.18 for gap in gaps), gaps)
                else:
                    self.assertTrue(all(gap < 0.1 for gap in gaps), gaps)

        # Once the 429 cooldown expires, remaining warm credit covers later fill creates without
        # over-crediting the sandboxes already adopted by paced creates during cooldown.
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        sent_at = []
        throttled = k8s_client.ApiException(status=429)
        throttled.headers = {"Retry-After": "0"}
        outcomes = iter([throttled])

        def create_recovering(*args, **kwargs):
            sent_at.append(time.monotonic())
            if len(sent_at) == 3:
                offset[0] += batch_utils.BATCH_WARM_CREDIT_THROTTLE_SECONDS + 0.1
            if (outcome := next(outcomes, None)) is not None:
                raise outcome

        helper.create_sandbox_claim.side_effect = create_recovering
        with patch.object(batch_utils, "time", fake_time):
            self.claim([BatchGroup(warmpool="pool-a", size=8)], batch_id="brecover", max_in_flight=1, create_rps=5)
            _wait_until(lambda: len(sent_at) == 9)
        gaps = [later - earlier for earlier, later in zip(sent_at[1:], sent_at[2:])]
        # Claim 0 took credit 1 and its retry reserved t+0; claim 1 paced at t+0.2 during cooldown
        # (spending credit 2) and advanced offset past cooldown; claims 2..5 use the 4 remaining
        # credits unpaced; claims 6..7 pace again once credit hits 0.
        self.assertGreaterEqual(gaps[0], 0.18)
        self.assertTrue(all(gap < 0.1 for gap in gaps[1:5]), gaps)
        self.assertTrue(all(gap >= 0.18 for gap in gaps[5:]), gaps)

    def test_cold_claim_on_watch_zeroes_pool_credit_for_remaining_fill_creates(self):
        for reason in ("SandboxCreatePending", "SandboxNotReady"):
            with self.subTest(reason):
                self.cluster = _Cluster(ready_on_create=False)
                self.client.k8s_helper = self.cluster.helper
                self.cluster.helper.get_sandbox_warmpool.side_effect = lambda name, *a, **k: {
                    "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
                    "status": {"readyReplicas": 10},
                }
                batch_ref: dict[str, SandboxBatch] = {}
                started = threading.Event()
                self.addCleanup(started.set)

                def hook(name):
                    if name == "b1-0":
                        started.wait(5)
                        cold = [{"type": "Ready", "status": "False", "reason": reason}]
                        self.cluster.watch_queue.put(
                            {"type": "ADDED", "object": _claim("b1-0", "pool-a", conditions=cold, annotations=_FILL)}
                        )
                        _wait_until(lambda: len(batch_ref["batch"].members()) == 1)

                self.cluster.create_hook = hook
                batch = batch_ref["batch"] = self.claim(
                    [BatchGroup(warmpool="pool-a", size=4), BatchGroup(warmpool="pool-b", size=2)],
                    max_in_flight=1,
                    create_rps=5,
                )
                self.addCleanup(batch._stop_background_threads)
                started.set()
                _wait_until(lambda: len(self.cluster.created) == 6)
                sent_at = dict(self.cluster.created)
                # b1-0 arrived cold on the watch, zeroing pool-a's credit so b1-1 takes the
                # first pacing slot and b1-2 and b1-3 pace at 5/s, while pool-b keeps its credit.
                self.assertGreaterEqual(sent_at["b1-2"] - sent_at["b1-1"], 0.18)
                self.assertGreaterEqual(sent_at["b1-3"] - sent_at["b1-2"], 0.18)
                self.assertLess(sent_at["b1-4"] - sent_at["b1-3"], 0.1)
                self.assertLess(sent_at["b1-5"] - sent_at["b1-4"], 0.1)
                batch._stop_background_threads()

    def test_creates_each_groups_min_ready_claims_first_in_group_order(self):
        self.claim(
            [BatchGroup(warmpool="pool-a", size=6, min_ready=3), BatchGroup(warmpool="pool-b", size=3, min_ready=1)],
            max_in_flight=1,
        )
        _wait_until(lambda: len(self.cluster.created) == 9)
        self.assertEqual(
            [(c.args[0], c.args[1]) for c in self.cluster.helper.create_sandbox_claim.call_args_list],
            [
                ("b1-0", "pool-a"),
                ("b1-1", "pool-a"),
                ("b1-2", "pool-a"),
                ("b1-6", "pool-b"),
                ("b1-3", "pool-a"),
                ("b1-4", "pool-a"),
                ("b1-5", "pool-a"),
                ("b1-7", "pool-b"),
                ("b1-8", "pool-b"),
            ],
        )

    def test_creation_stops_once_err_is_set(self):
        attached = threading.Event()
        handle = {}

        def fail_watch_during_first_create(name):
            attached.wait(5)
            with handle["batch"]._lock:
                handle["batch"]._state.note_error(RuntimeError("watch failed"))

        self.cluster.create_hook = fail_watch_during_first_create
        handle["batch"] = batch = self.claim([BatchGroup(warmpool="pool-a", size=5)], max_in_flight=1)
        attached.set()
        _wait_until(lambda: not any(c.is_alive() for c in batch._creators))
        self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 1)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    def test_create_retries_transient_errors_and_fails_on_403(self, _):
        helper = self.cluster.helper
        sent_at = []
        outcomes = iter([k8s_client.ApiException(status=503), None])

        def create(*args, **kwargs):
            sent_at.append(time.monotonic())
            outcome = next(outcomes)
            if outcome is not None:
                raise outcome

        helper.create_sandbox_claim.side_effect = create
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)], create_rps=5)
        _wait_until(lambda: helper.create_sandbox_claim.call_count == 2)
        self.assertEqual([c.args[0] for c in helper.create_sandbox_claim.call_args_list], ["b1-0", "b1-0"])
        # The retry waits for the next pacing slot, 1/create_rps after the first send.
        self.assertGreaterEqual(sent_at[1] - sent_at[0], 0.18)
        time.sleep(0.1)
        self.assertEqual(batch.members(), [])  # No CreateFailed member.

        helper.reset_mock()
        forbidden = k8s_client.ApiException(status=403, reason="Forbidden")
        forbidden.body = '{"kind": "Status", "message": "sandboxclaims is forbidden"}'
        helper.create_sandbox_claim.side_effect = forbidden
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b2")
        _wait_until(lambda: batch.members() != [])
        [member] = batch.members()
        self.assertEqual(
            (member.state, member.reason, member.message),
            (MemberState.FAILED, "CreateFailed", "sandboxclaims is forbidden"),
        )

    def test_a_backoff_retry_leaves_the_next_pacing_slot_free_and_a_retry_after_takes_it(self):
        throttled = k8s_client.ApiException(status=429)
        throttled.headers = {"Retry-After": "7"}
        for name, error, earliest, latest in (
            # Another create takes the next slot while this one backs off.
            ("a transport error", urllib3.exceptions.ProtocolError("boom"), 0, 0.2),
            # The apiserver throttles every create, so the next slot comes after its Retry-After.
            ("a Retry-After", throttled, 7, 7.4),
        ):
            with self.subTest(name):
                handle = _make_handle(self.client)
                handle._create_interval = 0.2
                self.cluster.helper.create_sandbox_claim.reset_mock()
                self.cluster.helper.create_sandbox_claim.side_effect = [error, None]
                next_free_slot = []

                def wait(timeout, _cancel=None):
                    # Another create reserves a slot during this create's first wait.
                    if not next_free_slot:
                        now = time.monotonic()
                        with handle._lock:
                            next_free_slot.append(handle._reserve_create_slot(now) - now)
                    return False

                handle._stop_creating.wait_or = wait
                handle._create_one("b1-0", "pool-a", {}, {}, {})
                self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 2)
                self.assertTrue(earliest <= next_free_slot[0] < latest, next_free_slot)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=10)
    def test_a_backoff_retry_is_not_paced_past_the_deadline(self, _):
        for name, deadline_in, slots_taken_during_backoff, waits_before_raising in (
            # The backoff alone ends past the deadline, so it isn't waited out.
            ("a backoff past the deadline", 5, False, 0),
            # The other creates take every slot before the deadline while this create backs off.
            ("slots taken during the backoff", 20, True, 1),
        ):
            with self.subTest(name):
                handle = _make_handle(self.client)
                handle._create_interval = 0.2
                self.cluster.helper.create_sandbox_claim.reset_mock()
                self.cluster.helper.create_sandbox_claim.side_effect = [urllib3.exceptions.ProtocolError("boom"), None]
                deadline = time.monotonic() + deadline_in
                waits = []

                def wait(timeout, _cancel=None):
                    waits.append(timeout)
                    if slots_taken_during_backoff:
                        handle._next_create_at = deadline
                    return False

                handle._stop_creating.wait_or = wait
                with self.assertRaises(BatchTimeoutError):
                    handle._create_one("b1-0", "pool-a", {}, {}, {}, deadline=deadline)
                self.assertEqual(len(waits), waits_before_raising)
                self.cluster.helper.create_sandbox_claim.assert_called_once()

    def test_failed_group_stops_creates_only_in_group_mode(self):
        for mode in ("group", "stream"):
            with self.subTest(mode):
                self.setUp()
                gate = threading.Event()

                def hook(name):
                    if name == "b1-0":
                        gate.wait(5)
                        raise k8s_client.ApiException(status=403)

                self.cluster.create_hook = hook
                batch = self.claim(
                    [BatchGroup(warmpool="pool-a", size=3), BatchGroup(warmpool="pool-b", size=2)],
                    max_in_flight=1,
                )
                consume = batch.group_outcomes if mode == "group" else batch.events
                items = consume()
                gate.set()
                list(items)
                if mode == "group":
                    self.assertEqual(self.cluster.names(), ["b1-3", "b1-4"])
                else:
                    self.assertEqual(self.cluster.names(), ["b1-1", "b1-2", "b1-3", "b1-4"])


class TestConsumers(BaseClaimBatchTest):

    def test_events_end_on_settle_and_a_second_call_continues(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=2)])
        first = next(batch.events())
        rest = list(batch.events())
        self.assertEqual(
            sorted((e.type, e.member.claim_name) for e in [first, *rest]),
            [(BatchEventType.MEMBER_READY, "b1-0"), (BatchEventType.MEMBER_READY, "b1-1")],
        )

    def test_events_end_when_a_ready_member_stops_being_ready_after_the_fill_settles(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)], quorum_timeout=600)
        events = batch.events()
        self.assertEqual(next(events).member.claim_name, "b1-0")
        self.cluster.push("b1-0", ready=False)
        _wait_until(lambda: batch._state.get_member("b1-0").state is MemberState.PENDING)
        self.assertEqual(_drain(events), [])

    def test_groups_yield_when_ready_and_time_out_after_the_last_create(self):
        self.cluster.ready_on_create = False
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0], sleep=time.sleep)
        with patch.object(sandbox_batch, "time", fake_time):
            batch = self.claim(
                [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
                quorum_timeout=60,
                lease_duration=120,
            )
            _wait_until(lambda: len(self.cluster.created) == 2)
            self.assertAlmostEqual(batch._fill_deadline - self.cluster.created[-1][1], 60, delta=0.5)
            groups = batch.group_outcomes()
            self.cluster.push("b1-0", "pool-a")
            first = next(groups)
            self.assertEqual((first.warmpool, [m.claim_name for m in first.members]), ("pool-a", ["b1-0"]))

            offset[0] = 61
            self.cluster.push("b1-1", "pool-b", ready=False)
            [second] = list(groups)
        self.assertEqual(second.warmpool, "pool-b")
        self.assertIsInstance(second.error, BatchTimeoutError)

    def test_release_wakes_a_waiting_consumer_with_batch_error(self):
        self.cluster.ready_on_create = False
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        errors: list[Exception] = []

        def consume():
            try:
                list(batch.events())
            except BatchError as e:
                errors.append(e)

        consumer = threading.Thread(target=consume)
        consumer.start()
        time.sleep(0.1)
        batch.release()
        consumer.join(5)
        self.assertEqual(len(errors), 1)

    def test_client_exit_cleanup_wakes_a_waiting_consumer_with_batch_error(self):
        self.cluster.ready_on_create = False
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        errors: list[Exception] = []

        def consume():
            try:
                list(batch.events())
            except BatchError as e:
                errors.append(e)

        consumer = threading.Thread(target=consume)
        consumer.start()
        time.sleep(0.1)
        self.client._delete_automatic_sandboxes()
        consumer.join(5)
        self.assertEqual(len(errors), 1)

    def test_iterators_end_when_the_watch_fails(self):
        self.cluster.ready_on_create = False
        self.cluster.helper.watch_sandbox_claims.side_effect = k8s_client.ApiException(status=403)
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        self.assertEqual(_drain(batch.group_outcomes()), [])
        self.assertEqual(_drain(batch.events()), [])
        self.assertEqual(batch.err().status, 403)


class TestWaitForQuorum(BaseClaimBatchTest):

    def wait_in_background(self, pool, batch, **kwargs):
        future = pool.submit(batch.wait_for_quorum, **kwargs)
        _wait_until(lambda: batch._state._mode == batch_state.ConsumerMode.QUORUM)
        return future

    def test_returns_min_ready_per_group_in_group_order_then_events_yield_the_rest(self):
        self.cluster.ready_on_create = False
        batch = self.claim(
            [BatchGroup(warmpool="pool-a", size=2, min_ready=1), BatchGroup(warmpool="pool-b", size=2, min_ready=1)]
        )
        _wait_until(lambda: len(self.cluster.created) == 4)
        for name, warmpool in (("b1-3", "pool-b"), ("b1-1", "pool-a"), ("b1-2", "pool-b"), ("b1-0", "pool-a")):
            self.cluster.push(name, warmpool)
        self.assertEqual([m.claim_name for m in batch.wait_for_quorum()], ["b1-1", "b1-3"])
        self.assertEqual(
            sorted((e.type, e.member.claim_name) for e in _drain(batch.events())),
            [(BatchEventType.MEMBER_READY, "b1-0"), (BatchEventType.MEMBER_READY, "b1-2")],
        )

    def test_quorum_timeout_raises_batch_timeout_error_and_stops_the_remaining_creates(self):
        self.cluster.ready_on_create = False
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0], sleep=time.sleep)
        with patch.object(sandbox_batch, "time", fake_time), ThreadPoolExecutor() as pool:
            # At one create per second, b1-1 still waits for its pacing slot when the deadline passes.
            batch = self.claim(
                [BatchGroup(warmpool="pool-a", size=2)],
                create_rps=1,
                quorum_timeout=60,
                lease_duration=120,
            )
            _wait_until(lambda: batch._fill_deadline is not None)
            future = self.wait_in_background(pool, batch)
            offset[0] = 62
            self.cluster.push("b1-0", ready=False)
            with self.assertRaises(BatchTimeoutError):
                future.result(5)
            _wait_until(lambda: batch._state._never_created == 1)
        self.assertEqual(self.cluster.names(), ["b1-0"])

    def test_an_unreachable_group_raises_and_stops_creates_for_every_group(self):
        gate = threading.Event()

        def hook(name):
            if name == "b1-0":
                gate.wait(5)
                raise k8s_client.ApiException(status=403)

        self.cluster.create_hook = hook
        batch = self.claim(
            [BatchGroup(warmpool="pool-a", size=3), BatchGroup(warmpool="pool-b", size=2)], max_in_flight=1
        )
        with ThreadPoolExecutor() as pool:
            future = self.wait_in_background(pool, batch)
            gate.set()
            with self.assertRaises(QuorumUnreachableError) as raised:
                future.result(5)
        self.assertEqual(raised.exception.warmpool, "pool-a")
        self.assertEqual([e.type for e in _drain(batch.events())], [BatchEventType.MEMBER_FAILED])
        self.assertEqual(self.cluster.names(), [])

    def test_any_exception_that_ends_the_wait_fails_the_quorum(self):
        gate = threading.Event()
        self.cluster.create_hook = lambda name: gate.wait(5) if name == "b1-0" else None
        batch = self.claim([BatchGroup(warmpool="pool-a", size=2)], max_in_flight=1)
        with patch.object(batch._changed, "wait", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                batch.wait_for_quorum()
        gate.set()
        # The quorum failed, so b1-0 becoming Ready isn't handed out and b1-1 is never created.
        self.assertEqual(_drain(batch.events()), [])
        self.assertEqual(self.cluster.names(), ["b1-0"])

    def test_release_or_a_failed_watch_ends_the_wait_with_batch_error(self):
        self.cluster.ready_on_create = False
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        with ThreadPoolExecutor() as pool:
            future = self.wait_in_background(pool, batch)
            batch.release()
            with self.assertRaises(BatchError):
                future.result(5)

        self.cluster.helper.watch_sandbox_claims.side_effect = k8s_client.ApiException(status=403)
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b2")
        with self.assertRaises(BatchError) as raised:
            batch.wait_for_quorum()
        self.assertEqual(raised.exception.__cause__.status, 403)

    def test_reattached_batch_at_min_ready_returns_at_once(self):
        annotations = {BATCH_GROUP_SIZE_ANNOTATION: "2", BATCH_GROUP_MIN_READY_ANNOTATION: "1"}
        self.cluster.helper.list_sandbox_claim_objects.return_value = (
            [
                _claim("b1-0", conditions=_NOT_READY, annotations=annotations),
                _claim("b1-1", conditions=_READY, sandbox={"name": "sbx-b1-1"}, annotations=annotations),
            ],
            "5",
        )
        self.cluster.helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC))
        batch = self.client.get_batch("b1")
        self.addCleanup(batch._stop_background_threads)
        self.assertEqual([m.claim_name for m in batch.wait_for_quorum()], ["b1-1"])


class TestRelease(BaseClaimBatchTest):

    def test_release_stops_creates_then_deletes_claims_then_the_lease(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=20)], create_rps=10)
        _wait_until(lambda: len(self.cluster.created) >= 1)
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
        with self.assertRaises(BatchError):
            batch.events()

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

        unavailable = k8s_client.ApiException(status=503)
        cases = {
            "503 then success": (
                {"delete_sandbox_claims_by_label": [unavailable, None]},
                {"count_live_sandbox_claims": [1, 0]},
                None,
            ),
            "403 raises at once": ({"delete_sandbox_claims_by_label": [k8s_client.ApiException(status=403)]}, {}, k8s_client.ApiException),
            "no progress raises BatchError": ({}, {"count_live_sandbox_claims": [2] * 4}, BatchError),
            "no progress raises the last transient error": (
                {"delete_sandbox_claims_by_label": [unavailable] * 4},
                {"count_live_sandbox_claims": [unavailable] * 4},
                k8s_client.ApiException,
            ),
            "progress resets the idle rounds": ({}, {"count_live_sandbox_claims": [5, 5, 4, 4, 4, 0]}, None),
        }
        for name, (delete_effects, count_effects, error) in cases.items():
            with self.subTest(name):
                helper.reset_mock()
                helper.delete_sandbox_claims_by_label.side_effect = delete_effects.get("delete_sandbox_claims_by_label")
                helper.count_live_sandbox_claims.side_effect = count_effects.get("count_live_sandbox_claims", [0] * 4)
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

    def test_a_second_release_retries_a_connection_that_failed_to_close(self):
        self.cluster.helper.delete_sandbox_claims_by_label.side_effect = [k8s_client.ApiException(status=403), None]
        handle = _make_handle(self.client)
        cached = MagicMock()
        cached.close_connection.side_effect = [RuntimeError("close failed"), None]
        handle._connected["b1-0"] = (cached, None)

        with self.assertRaises(k8s_client.ApiException):
            handle.release()
        self.assertEqual(handle._connected, {"b1-0": (cached, None)})

        handle.release()
        self.assertEqual(cached.close_connection.call_count, 2)
        self.assertEqual(handle._connected, {})

    def test_requests_carry_request_timeouts(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        _wait_until(lambda: len(self.cluster.created) == 1)
        batch.release()
        helper = self.cluster.helper
        create_kwargs = helper.create_sandbox_claim.call_args.kwargs
        self.assertEqual((create_kwargs["_request_timeout"], create_kwargs["batch"]), (30, True))
        self.assertEqual(helper.delete_sandbox_claims_by_label.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.count_live_sandbox_claims.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.delete_batch_lease.call_args.kwargs["_request_timeout"], 30)

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

    @patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=lambda s: None))
    def test_release_deletes_nothing_while_another_handle_holds_the_lease(self):
        helper = self.cluster.helper
        for name, reads, deletes in (
            ("another holder", [_lease(holder_identity="other-handle")] * 2, []),
            # Another handle attached and then detached, so the Lease has no holder.
            ("an attach since this handle's last write", [_lease(lease_transitions=1)] * 2, []),
            # The first round found no Lease. The Lease created after it belongs to another batch.
            ("a Lease created again after a round", [None, _lease(uid="another-uid")], ["deletecollection"]),
        ):
            with self.subTest(name):
                helper.reset_mock()
                self.cluster.calls.clear()
                helper.read_batch_lease.side_effect = reads
                helper.count_live_sandbox_claims.side_effect = [1, 0]
                handle = _make_handle(self.client)
                self.client._active_batches[("default", "b1")] = handle

                with self.assertRaises(BatchInUseError):
                    handle.release()
                helper.replace_batch_lease.assert_not_called()
                self.assertEqual(self.cluster.calls, deletes)
                self.assertEqual(self.client._active_batches, {})

                helper.reset_mock()
                with self.assertRaises(BatchError):
                    handle.release()
                helper.read_batch_lease.assert_not_called()

    def test_release_retries_one_lease_conflict_then_raises_in_use(self):
        helper = self.cluster.helper
        conflict = k8s_client.ApiException(status=409)
        for name, replace_effects, error in (
            ("a late renewal of this handle", [conflict, _lease(resource_version="11")], None),
            ("another writer", [conflict, conflict], BatchInUseError),
        ):
            with self.subTest(name):
                helper.reset_mock()
                handle = _make_handle(self.client)
                helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                helper.replace_batch_lease.side_effect = replace_effects
                if error is None:
                    handle.release()
                    helper.delete_sandbox_claims_by_label.assert_called_once()
                else:
                    with self.assertRaises(error):
                        handle.release()
                    helper.delete_sandbox_claims_by_label.assert_not_called()
                    helper.delete_batch_lease.assert_not_called()

    @patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=lambda s: None))
    def test_release_with_no_lease_deletes_the_claims_and_creates_nothing(self):
        helper = self.cluster.helper
        for name, take_error, calls in (
            ("no Lease", None, ["deletecollection"]),
            # The Lease may still exist, so a failed take doesn't count as finding none.
            ("the take fails", k8s_client.ApiException(status=503), []),
        ):
            with self.subTest(name):
                helper.reset_mock()
                self.cluster.calls.clear()
                helper.read_batch_lease.return_value = None
                helper.read_batch_lease.side_effect = take_error
                handle = _make_handle(self.client)
                if take_error is None:
                    handle.release()
                else:
                    with self.assertRaises(k8s_client.ApiException):
                        handle.release()
                self.assertEqual(self.cluster.calls, calls)
                helper.create_batch_lease.assert_not_called()
                self.assertEqual(handle._released, take_error is None)

    @patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=lambda s: None))
    def test_release_takes_the_lease_before_each_round_and_deletes_it_at_the_version_written(self):
        helper = self.cluster.helper
        for name, holder, resource_version in (
            ("held by this handle", "host_1234_abcd1234", "10"),
            # This handle's own detach landed, so no handle holds the Lease and no other handle attached.
            ("after this handle's own detach", None, "11"),
        ):
            with self.subTest(name):
                self.cluster.calls.clear()
                handle = _make_handle(self.client)
                helper.read_batch_lease.side_effect = (
                    lambda *a, **k: _lease(holder_identity=holder, resource_version=resource_version)
                )
                # urllib3 doesn't retry Lease requests, so release() sees the raw connect error.
                refused = urllib3.exceptions.NewConnectionError(None, "refused")
                written = iter([_lease(resource_version="11"), refused, _lease(resource_version="13")])

                def replace(*args, **kwargs):
                    self.cluster.calls.append("take")
                    result = next(written)
                    if isinstance(result, Exception):
                        raise result
                    return result

                helper.replace_batch_lease.side_effect = replace
                # The second round's take fails, so only the third round deletes the Lease.
                helper.count_live_sandbox_claims.side_effect = [1, 0, 0]
                handle.release()
                self.assertEqual(
                    self.cluster.calls, ["take", "deletecollection", "take", "take", "deletecollection", "delete_lease"]
                )
                self.assertEqual(helper.delete_batch_lease.call_args.kwargs["resource_version"], "13")

    def test_the_first_lifecycle_call_wins(self):
        helper = self.cluster.helper
        released = ["write", "deletecollection", "delete_lease"]
        for first, second, second_waits, calls_while_blocked, second_error, calls in (
            ("release", "detach", False, ["write", "deletecollection"], BatchError, released),
            ("detach", "release", True, ["write"], BatchError, ["write"]),
            ("release", "release", True, ["write", "deletecollection"], None, released),
        ):
            with self.subTest(first=first, second=second):
                self.cluster.calls.clear()
                handle = _make_handle(self.client)
                lock = handle._lifecycle_lock = _SignalingLock(handle._lifecycle_lock)
                blocked, resume = threading.Event(), threading.Event()
                self.addCleanup(resume.set)
                outcomes = {}

                def record(call, block):
                    self.cluster.calls.append(call)
                    if block and not blocked.is_set():
                        blocked.set()
                        resume.wait(5)
                    return _lease(resource_version="11")

                helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                helper.replace_batch_lease.side_effect = lambda *a, **k: record("write", first == "detach")
                helper.delete_sandbox_claims_by_label.side_effect = lambda *a, **k: record("deletecollection", True)

                def run(key, method):
                    try:
                        getattr(handle, method)()
                        outcomes[key] = None
                    except Exception as e:
                        outcomes[key] = e

                first_thread = threading.Thread(target=run, args=("first", first))
                first_thread.start()
                blocked.wait(5)
                lock.start_racing(run, "second", second)
                self.assertEqual("second" in outcomes, not second_waits)
                self.assertEqual(self.cluster.calls, calls_while_blocked)

                resume.set()
                for thread in (first_thread, *lock.threads):
                    thread.join()
                self.assertIsNone(outcomes["first"])
                self.assertIs(type(outcomes["second"]), second_error or type(None))
                self.assertEqual(self.cluster.calls, calls)

    def test_a_detach_during_detach_on_the_same_thread_does_not_hang(self):
        helper = self.cluster.helper
        handle = _make_handle(self.client)
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)

        error: BaseException | None = None

        def write(*args, **kwargs):
            # A signal handler that runs during this write calls detach() on the same thread, and
            # the outer write then fails after the inner call already released the Lease.
            if helper.replace_batch_lease.call_count == 1:
                handle.detach()
                raise RuntimeError("connection reset")

        def run():
            nonlocal error
            try:
                handle.detach()
            except BaseException as e:
                error = e

        helper.replace_batch_lease.side_effect = write
        thread = threading.Thread(target=run, daemon=True)
        thread.start()
        thread.join(timeout=5)
        self.assertFalse(thread.is_alive())
        self.assertIsNone(error)
        self.assertTrue(handle._lease_released)

    @patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=lambda s: None))
    def test_a_release_during_detach_on_the_same_thread_makes_detach_raise(self):
        helper = self.cluster.helper
        self.addCleanup(self.client._active_batches.clear)

        with self.subTest("during read_batch_lease with a failed release"):
            helper.reset_mock()
            handle = _make_handle(self.client)
            self.client._active_batches = {("default", "b1"): handle}
            helper.count_live_sandbox_claims.return_value = 1
            helper.delete_sandbox_claims_by_label.side_effect = k8s_client.ApiException(status=503)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )
            release_writes = 0

            def read_during_failed_release(*args, **kwargs):
                nonlocal release_writes
                if helper.read_batch_lease.call_count == 1:
                    with contextlib.suppress(k8s_client.ApiException):
                        handle.release()
                    release_writes = helper.replace_batch_lease.call_count
                return _lease(holder_identity=handle._holder_identity)

            helper.read_batch_lease.side_effect = read_during_failed_release
            with self.assertRaises(BatchError):
                handle.detach()
            self.assertGreater(release_writes, 0)
            self.assertEqual(helper.replace_batch_lease.call_count, release_writes)
            self.assertEqual(
                [c.args[2].spec.holder_identity for c in helper.replace_batch_lease.call_args_list],
                [handle._holder_identity] * release_writes,
            )
            self.assertFalse(handle._lease_released)
            self.assertEqual(self.client._active_batches, {("default", "b1"): handle})

        with self.subTest("during replace_batch_lease before the write lands"):
            helper.reset_mock()
            handle = _make_handle(self.client)
            self.client._active_batches = {("default", "b1"): handle}
            helper.count_live_sandbox_claims.return_value = 0
            helper.delete_sandbox_claims_by_label.side_effect = None
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)

            def write_before_landing(*args, **kwargs):
                if helper.replace_batch_lease.call_count == 1:
                    handle.release()
                    raise k8s_client.ApiException(status=404)
                return _lease(holder_identity=args[2].spec.holder_identity, resource_version="11")

            helper.replace_batch_lease.side_effect = write_before_landing
            with self.assertRaises(BatchError):
                handle.detach()
            self.assertTrue(handle._released)

        with self.subTest("during replace_batch_lease after the write lands"):
            helper.reset_mock()
            handle = _make_handle(self.client)
            self.client._active_batches = {("default", "b1"): handle}
            helper.count_live_sandbox_claims.return_value = 0
            helper.delete_sandbox_claims_by_label.side_effect = None
            current_holder: str | None = handle._holder_identity
            helper.read_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=current_holder, resource_version="11")
            )

            def write_after_landing(*args, **kwargs):
                nonlocal current_holder
                current_holder = args[2].spec.holder_identity
                if helper.replace_batch_lease.call_count == 1:
                    handle.release()
                return _lease(holder_identity=args[2].spec.holder_identity, resource_version="12")

            helper.replace_batch_lease.side_effect = write_after_landing
            with self.assertRaises(BatchError):
                handle.detach()
            self.assertTrue(handle._released)

    def test_stalled_release_and_release_group_scope_deletes_to_lease_uid(self):
        helper = self.cluster.helper
        pool_a = batch_utils.group_label_value("pool-a")
        pool_b = batch_utils.group_label_value("pool-b")

        def matches(labels, selector):
            return all(
                labels.get(key) == value
                for term in selector.split(",")
                for key, value in [term.split("=", 1)]
            )

        store: dict[str, dict[str, str]] = {}

        def delete_by_label(namespace, selector, **kwargs):
            for name in [n for n, lbl in store.items() if matches(lbl, selector)]:
                del store[name]

        def count_live(namespace, selector, **kwargs):
            return sum(1 for lbl in store.values() if matches(lbl, selector))

        def claim_labels(uid, group_hash):
            return {BATCH_ID_LABEL: "b1", BATCH_LEASE_UID_LABEL: uid, BATCH_GROUP_LABEL: group_hash}

        with self.subTest("release after old Lease was reaped"):
            helper.reset_mock()
            store = {
                "b1-0": claim_labels("uid-1", pool_a),
                "b1-new-0": claim_labels("uid-2", pool_a),
            }
            helper.delete_sandbox_claims_by_label.side_effect = delete_by_label
            helper.count_live_sandbox_claims.side_effect = count_live
            helper.read_batch_lease.return_value = None
            handle = _make_handle(self.client, lease_uid="uid-1")
            handle.release()
            self.assertEqual(set(store), {"b1-new-0"})
            helper.delete_batch_lease.assert_not_called()

        with self.subTest("round 1 deletecollection lands after uid-2 claims exist"):
            helper.reset_mock()
            store = {"b1-0": claim_labels("uid-1", pool_a)}
            current_lease = _lease(holder_identity="host_1234_abcd1234", uid="uid-1")
            helper.read_batch_lease.side_effect = lambda *a, **k: current_lease
            helper.replace_batch_lease.side_effect = lambda *a, **k: _lease(
                holder_identity=a[2].spec.holder_identity, uid="uid-1", resource_version="11"
            )
            helper.count_live_sandbox_claims.side_effect = count_live

            def delayed_round1_delete(namespace, selector, **kwargs):
                nonlocal current_lease
                # While Round 1's deletecollection is in flight, uid-1 is reaped and batch uid-2 starts.
                current_lease = _lease(
                    holder_identity="holder-2", uid="uid-2", renew_time=datetime.now(UTC)
                )
                store["b1-new-0"] = claim_labels("uid-2", pool_a)
                delete_by_label(namespace, selector, **kwargs)
                # Simulate one uid-1 claim not yet stamped with deletionTimestamp when 504 cuts Round 1 short.
                store["b1-1"] = claim_labels("uid-1", pool_a)
                raise k8s_client.ApiException(status=504)

            helper.delete_sandbox_claims_by_label.side_effect = delayed_round1_delete
            handle = _make_handle(self.client, lease_uid="uid-1")
            with patch.object(batch_utils, "retry_delay", return_value=0):
                with self.assertRaises(BatchInUseError):
                    handle.release()
            self.assertEqual(set(store), {"b1-1", "b1-new-0"})
            helper.delete_batch_lease.assert_not_called()

        with self.subTest("release_group scopes deletecollection to lease uid"):
            helper.reset_mock()
            store = {
                "b1-0": claim_labels("uid-1", pool_a),
                "b1-1": claim_labels("uid-1", pool_b),
                "b1-new-0": claim_labels("uid-2", pool_a),
            }
            helper.delete_sandbox_claims_by_label.side_effect = delete_by_label
            helper.count_live_sandbox_claims.side_effect = count_live
            handle = _make_handle(
                self.client,
                groups=[BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
                lease_uid="uid-1",
            )
            handle.release_group("pool-a")
            self.assertEqual(set(store), {"b1-1", "b1-new-0"})

        for rv in (None, ""):
            with self.subTest(missing_written_rv=rv):
                helper.reset_mock(return_value=True, side_effect=True)
                helper.read_batch_lease.return_value = _lease(holder_identity="host_1234_abcd1234", uid="uid-1")
                helper.replace_batch_lease.return_value = _lease(
                    holder_identity="host_1234_abcd1234", uid="uid-1", resource_version=rv
                )
                handle = _make_handle(self.client, lease_uid="uid-1")
                with self.assertRaisesRegex(BatchError, "no resourceVersion"):
                    handle.release()
                helper.delete_sandbox_claims_by_label.assert_not_called()
                helper.delete_batch_lease.assert_not_called()


class TestReleaseMembers(BaseClaimBatchTest):

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    def test_release_member_deletes_its_claim_with_the_shared_retry_policy(self, _):
        unavailable = k8s_client.ApiException(status=503)
        cases = {
            "503 then success": ([unavailable, None], 2, None),
            "403 raises at once": ([k8s_client.ApiException(status=403)], 1, k8s_client.ApiException),
            "503 on every attempt raises": ([unavailable] * 3, 3, k8s_client.ApiException),
        }
        helper = self.cluster.helper
        for name, (effects, attempts, error) in cases.items():
            with self.subTest(name):
                helper.reset_mock()
                self.mock_sandbox_class.reset_mock()
                helper.delete_sandbox_claim.side_effect = effects
                handle = _make_handle(self.client)
                handle._state.upsert_claim(
                    _claim("b1-0", conditions=_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                )
                sandbox = handle.connect(handle.members()[0])
                [member] = handle.members()
                if error is None:
                    handle.release_member(member)
                else:
                    with self.assertRaises(error):
                        handle.release_member(member)
                sandbox.close_connection.assert_called_once()
                self.assertEqual(helper.delete_sandbox_claim.call_count, attempts)
                kwargs = helper.delete_sandbox_claim.call_args.kwargs
                self.assertEqual(
                    (kwargs["_request_timeout"], kwargs["log_level"], kwargs["batch"]),
                    (30, logging.DEBUG, True),
                )
        with self.assertRaises(ValueError):
            handle.release_member(MagicMock(claim_name="b1-9"))

    def test_release_not_ready_skips_unsent_creates_and_deletes_pending_members(self):
        self.cluster.ready_on_create = False
        # b1-1 waits for its pacing slot when the fill ends, and b1-2 and b1-3 are still planned.
        batch = self.claim([BatchGroup(warmpool="pool-a", size=4)], max_in_flight=2, create_rps=2)
        _wait_until(lambda: self.cluster.names() == ["b1-0"])
        self.cluster.push("b1-0", ready=False)
        _wait_until(lambda: len(batch.members()) == 1)

        batch.release_not_ready()
        _wait_until(lambda: not any(c.is_alive() for c in batch._creators))
        self.assertEqual(self.cluster.names(), ["b1-0"])
        self.assertEqual([c.args[0] for c in self.cluster.helper.delete_sandbox_claim.call_args_list], ["b1-0"])

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    def test_release_not_ready_keeps_handed_out_member_when_create_retries_late(self, _):
        self.cluster.ready_on_create = False
        attempt_two, finish_retry = threading.Event(), threading.Event()
        self.addCleanup(finish_retry.set)
        attempts = 0

        def create_claim(name, warmpool, namespace, **kwargs):
            nonlocal attempts
            attempts += 1
            if attempts == 1:
                # The first create lands on the apiserver, then fails on the client before the watch sees it.
                raise k8s_client.ApiException(status=503)
            self.cluster.push(name, warmpool, ready=True, annotations=kwargs["annotations"])
            attempt_two.set()
            finish_retry.wait(5)
            raise k8s_client.ApiException(status=409)

        self.cluster.helper.create_sandbox_claim.side_effect = create_claim
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        [kept] = batch.wait_for_quorum()
        self.assertTrue(attempt_two.wait(5))

        batch.release_not_ready()
        finish_retry.set()
        _wait_until(lambda: not any(c.is_alive() for c in batch._creators))
        self.assertEqual(batch.members(), [kept])
        self.cluster.helper.delete_sandbox_claim.assert_not_called()

    def test_release_member_stops_create_retries_and_deletes_an_in_flight_retry(self):
        cases = (
            ("fill", "seen_pending"),
            ("fill", "before_backoff"),
            ("fill", "during_backoff"),
            ("fill", "in_flight"),
            ("fill", "retry_then_fail"),
            ("acquire", "seen_pending"),
            ("acquire", "before_backoff"),
            ("acquire", "during_backoff"),
            ("acquire", "in_flight"),
        )
        for origin, timing in cases:
            with self.subTest(origin=origin, timing=timing):
                self.cluster = _Cluster()
                self.client.k8s_helper = helper = self.cluster.helper
                self.cluster.ready_on_create = False
                fail_first, in_backoff = threading.Event(), threading.Event()
                entered_backoff, finish_backoff = threading.Event(), threading.Event()
                attempt_two, finish_retry = threading.Event(), threading.Event()
                in_first_delete, finish_first_delete = threading.Event(), threading.Event()
                second_delete_failed = threading.Event()
                for event in (fail_first, finish_backoff, finish_retry, finish_first_delete):
                    self.addCleanup(event.set)
                attempts = 0
                deletes = 0
                delete_lock = threading.Lock()
                first_attempt: list[tuple[str, dict[str, str]]] = []

                def create_claim(name, warmpool, namespace, **kwargs):
                    nonlocal attempts
                    attempts += 1
                    annotations = kwargs["annotations"]
                    if attempts == 1:
                        first_attempt.append((name, annotations))
                        if timing in ("seen_pending", "before_backoff"):
                            ready = origin == "fill" and timing != "seen_pending"
                            self.cluster.push(name, warmpool, ready=ready, annotations=annotations)
                        if timing in ("before_backoff", "during_backoff"):
                            fail_first.wait(5)
                        raise k8s_client.ApiException(status=503)
                    self.cluster.push(name, warmpool, ready=(origin == "fill"), annotations=annotations)
                    attempt_two.set()
                    if timing in ("in_flight", "retry_then_fail"):
                        finish_retry.wait(5)
                    # Simulates a retry landing after release_member deleted the first claim.
                    self.cluster.push(name, warmpool, ready=True, annotations=annotations)
                    if timing == "retry_then_fail":
                        raise k8s_client.ApiException(status=403)

                def delete_claim(name, namespace, **kwargs):
                    nonlocal deletes
                    with delete_lock:
                        deletes += 1
                        call_num = deletes
                    if call_num == 1:
                        in_first_delete.set()
                        finish_first_delete.wait(5)
                        return
                    if call_num == 2 and timing in ("in_flight", "retry_then_fail"):
                        second_delete_failed.set()
                        raise k8s_client.ApiException(status=403)

                def delay(_attempt, _err=None):
                    in_backoff.set()
                    return 1.0 if timing == "during_backoff" else (10.0 if timing == "before_backoff" else 0.0)

                helper.create_sandbox_claim = create_claim
                helper.delete_sandbox_claim = delete_claim
                with patch("k8s_agent_sandbox.batch_utils.retry_delay", new=delay):
                    batch = self.claim([BatchGroup(warmpool="pool-a", size=1 if origin == "fill" else 0)])
                    with ThreadPoolExecutor(2) as pool:
                        if origin == "fill":
                            acquire_fut = None
                        else:
                            acquire_timeout = 0.5 if timing == "before_backoff" else 5
                            acquire_fut = pool.submit(batch.acquire, "pool-a", timeout=acquire_timeout)

                        if timing == "during_backoff":
                            _wait_until(lambda: len(first_attempt) == 1)
                            orig_wait = batch._stop_creating.wait_or

                            def wait_gate(timeout, cancel=None):
                                # The retry backoff is 1.0 seconds, computed from a deadline.
                                if 0.5 < timeout <= 1.0:
                                    entered_backoff.set()
                                    finish_backoff.wait(5)
                                    return batch._stop_creating.is_set()
                                return orig_wait(timeout, cancel)

                            batch._stop_creating.wait_or = wait_gate  # type: ignore[method-assign]
                            fail_first.set()
                            self.assertTrue(entered_backoff.wait(5))
                            claim_name, claim_annotations = first_attempt[0]
                            self.cluster.push(
                                claim_name, "pool-a", ready=(origin == "fill"), annotations=claim_annotations
                            )

                        if origin == "fill" and timing != "seen_pending":
                            [member] = batch.wait_for_quorum()
                        else:
                            _wait_until(lambda: len(batch.members()) == 1)
                            [member] = batch.members()

                        if timing == "seen_pending":
                            self.assertTrue(in_backoff.wait(5))
                            self.cluster.push(
                                member.claim_name, "pool-a", ready=True, annotations=first_attempt[0][1]
                            )
                            if acquire_fut is not None:
                                self.assertEqual(acquire_fut.result(timeout=5).claim_name, member.claim_name)
                            else:
                                self.assertEqual(batch.wait_for_quorum()[0].claim_name, member.claim_name)
                                _wait_until(lambda: not any(c.is_alive() for c in batch._creators))
                            self.assertEqual((attempts, deletes), (1, 0))
                            batch._stop_background_threads()
                            continue
                        if timing == "before_backoff":
                            release_fut = pool.submit(batch.release_member, member)
                            self.assertTrue(in_first_delete.wait(5))
                            fail_first.set()
                        elif timing == "during_backoff":
                            with batch._lock:
                                batch._next_create_at = time.monotonic() + 10
                            release_fut = pool.submit(batch.release_member, member)
                            self.assertTrue(in_first_delete.wait(5))
                            finish_backoff.set()
                        else:
                            self.assertTrue(attempt_two.wait(5))
                            release_fut = pool.submit(batch.release_member, member)
                            self.assertTrue(in_first_delete.wait(5))
                            finish_retry.set()
                            self.assertTrue(second_delete_failed.wait(5))

                        if acquire_fut is not None:
                            with self.assertRaises(BatchError) as ctx:
                                acquire_fut.result(timeout=5)
                            self.assertNotIsInstance(ctx.exception, BatchTimeoutError)
                            self.assertRegex(str(ctx.exception), "released before it was Ready")
                        else:
                            _wait_until(lambda: not any(c.is_alive() for c in batch._creators))

                        finish_first_delete.set()
                        release_fut.result(timeout=5)

                    self.assertEqual(batch.members(), [])
                    if timing in ("before_backoff", "during_backoff"):
                        self.assertEqual((attempts, deletes), (1, 1))
                        batch.release_member(member)
                        self.assertEqual(deletes, 1)
                    else:
                        self.assertEqual((attempts, deletes), (2, 2))
                        # Even though the first delete finished after the retry's delete failed,
                        # the retry's delete stays pending and a later release_member retries it.
                        batch.release_member(member)
                        self.assertEqual(deletes, 3)
                        batch.release_member(member)
                        self.assertEqual(deletes, 3)
                    batch._stop_background_threads()

    def test_release_group_deletes_by_group_label_and_skips_only_its_creates(self):
        gate = threading.Event()
        self.addCleanup(gate.set)
        self.cluster.create_hook = lambda name: gate.wait(5) if name == "b1-0" else None
        # Creates go b1-0, b1-10, b1-1 to b1-4, b1-11, then the rest of pool-a.
        batch = self.claim(
            [BatchGroup(warmpool="pool-a", size=10), BatchGroup(warmpool="pool-b", size=2)],
            max_in_flight=1,
            create_rps=5,
        )
        _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count == 1)
        with self.assertRaises(ValueError):
            batch.release_group("pool-c")

        batch.release_group("pool-a")
        gate.set()  # b1-0's create lands after its group was released.
        _wait_until(lambda: len(batch.members()) == 2)
        helper = self.cluster.helper
        self.assertEqual(
            helper.delete_sandbox_claims_by_label.call_args.args,
            ("default", f"{BATCH_ID_LABEL}=b1,{BATCH_GROUP_LABEL}=8934303dc131d111,{BATCH_LEASE_UID_LABEL}=lease-uid"),
        )
        _wait_until(lambda: helper.delete_sandbox_claim.call_count == 1)
        self.assertEqual(helper.delete_sandbox_claim.call_args.args[0], "b1-0")
        self.assertEqual(self.cluster.names(), ["b1-0", "b1-10", "b1-11"])
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-10", "b1-11"])
        # pool-a's skipped creates take no pacing slots, so b1-11 follows b1-10 by one slot.
        created_at = dict(self.cluster.created)
        self.assertLess(created_at["b1-11"] - created_at["b1-10"], 0.6)

    def test_member_deletes_run_in_parallel_and_share_max_in_flight_with_creates(self):
        helper = self.cluster.helper
        counter_lock = threading.Lock()
        in_flight_rpcs = 0
        in_flight_deletes = 0
        peak_rpcs = 0
        peak_deletes = 0
        create_entered, release_create = threading.Event(), threading.Event()
        fail_m0, finish_m1 = threading.Event(), threading.Event()
        delete_sleeping, finish_sleep = threading.Event(), threading.Event()
        m0_attempts = 0
        for event in (release_create, fail_m0, finish_m1, finish_sleep):
            self.addCleanup(event.set)

        def create_claim(name, warmpool, namespace, **kwargs):
            nonlocal in_flight_rpcs, peak_rpcs
            with counter_lock:
                in_flight_rpcs += 1
                peak_rpcs = max(peak_rpcs, in_flight_rpcs)
            try:
                if name == "b1-3" and not create_entered.is_set():
                    create_entered.set()
                    release_create.wait(5)
                    raise k8s_client.ApiException(status=503)
                self.cluster._create(name, warmpool, namespace, **kwargs)
            finally:
                with counter_lock:
                    in_flight_rpcs -= 1

        def delete_claim(name, namespace, **kwargs):
            nonlocal in_flight_rpcs, in_flight_deletes, peak_rpcs, peak_deletes, m0_attempts
            with counter_lock:
                in_flight_rpcs += 1
                in_flight_deletes += 1
                peak_rpcs = max(peak_rpcs, in_flight_rpcs)
                peak_deletes = max(peak_deletes, in_flight_deletes)
            try:
                if name == "b1-0":
                    m0_attempts += 1
                    if m0_attempts == 1:
                        fail_m0.wait(5)
                        raise k8s_client.ApiException(status=503)
                elif name == "b1-1":
                    finish_m1.wait(5)
            finally:
                with counter_lock:
                    in_flight_rpcs -= 1
                    in_flight_deletes -= 1

        helper.create_sandbox_claim.side_effect = create_claim
        helper.delete_sandbox_claim.side_effect = delete_claim
        with patch(
            "k8s_agent_sandbox.batch_utils.retry_delay",
            side_effect=lambda _a, err=None: 10 if err is None else 1,
        ):
            batch = self.claim([BatchGroup(warmpool="pool-a", size=4)], max_in_flight=2)
            real_wait = batch._stop_creating.wait

            def wait_on_retry(seconds=None):
                if threading.current_thread() not in batch._creators:
                    delete_sleeping.set()
                    finish_sleep.wait(10)
                    return batch._stop_creating.is_set()
                return real_wait(seconds)

            with patch.object(batch._stop_creating, "wait", side_effect=wait_on_retry):
                _wait_until(lambda: len(batch.members()) == 3 and create_entered.is_set())
                m0, m1, m2 = batch.members()

                # b1-3's create holds one of the two slots, so only one release_member enters delete_sandbox_claim.
                t0 = threading.Thread(target=batch.release_member, args=(m0,))
                t1 = threading.Thread(target=batch.release_member, args=(m1,))
                t0.start()
                t1.start()
                _wait_until(lambda: in_flight_deletes == 1 and len(batch.members()) == 1)
                self.assertEqual(in_flight_rpcs, 2)

                # b1-3's create fails with 503 and frees its slot before backoff, so m1's delete runs in parallel.
                release_create.set()
                _wait_until(lambda: in_flight_deletes == 2)
                self.assertEqual(in_flight_rpcs, 2)

                # m2 waits until m0 fails with 503 and releases its slot before sleeping, then finishes while m0 sleeps.
                t2 = threading.Thread(target=batch.release_member, args=(m2,))
                t2.start()
                _wait_until(lambda: batch.members() == [])
                self.assertEqual(in_flight_deletes, 2)
                fail_m0.set()
                self.assertTrue(delete_sleeping.wait(5))
                t2.join(timeout=2)
                self.assertFalse(t2.is_alive())
                finish_m1.set()
                finish_sleep.set()
                batch._stop_creating.set()
                for thread in (t0, t1):
                    thread.join(timeout=5)
                _wait_until(lambda: not any(c.is_alive() for c in batch._creators))
        self.assertEqual((peak_rpcs, peak_deletes), (2, 2))

        with self.subTest("release_not_ready respects max_in_flight"):
            in_flight_not_ready = 0
            peak_not_ready = 0
            two_entered, allow_not_ready = threading.Event(), threading.Event()
            self.addCleanup(allow_not_ready.set)

            def delete_pending(name, namespace, **kwargs):
                nonlocal in_flight_not_ready, peak_not_ready
                with counter_lock:
                    in_flight_not_ready += 1
                    peak_not_ready = max(peak_not_ready, in_flight_not_ready)
                    if in_flight_not_ready == 2:
                        two_entered.set()
                try:
                    allow_not_ready.wait(5)
                finally:
                    with counter_lock:
                        in_flight_not_ready -= 1

            helper.create_sandbox_claim.side_effect = self.cluster._create
            helper.delete_sandbox_claim.side_effect = delete_pending
            handle = SandboxBatch(
                client=self.client,
                batch_id="b2",
                namespace="default",
                state=batch_state.BatchState("b2", [BatchGroup(warmpool="pool-a", size=3)]),
                lease_name="batch-b2",
                holder_identity="host_1234_abcd1234",
                lease_duration=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
                list_resource_version="5",
                lease=_lease(holder_identity="host_1234_abcd1234"),
                lease_renewed_at=time.monotonic(),
                max_in_flight=2,
            )
            for idx in range(3):
                handle._state.upsert_claim(_claim(f"b2-{idx}", conditions=_NOT_READY, annotations=_FILL))
            not_ready_thread = threading.Thread(target=handle.release_not_ready)
            not_ready_thread.start()
            self.assertTrue(two_entered.wait(5))
            time.sleep(0.02)
            self.assertEqual(peak_not_ready, 2)
            allow_not_ready.set()
            not_ready_thread.join(timeout=5)
            self.assertEqual(peak_not_ready, 2)

    def test_lifecycle_calls_wait_for_active_deletes_and_stop_further_deletes(self):
        helper = self.cluster.helper
        methods = (
            ("release_member", lambda h: h.release_member(h.members()[0]), "delete_sandbox_claim"),
            ("release_not_ready", lambda h: h.release_not_ready(), "delete_sandbox_claim"),
            ("release_group", lambda h: h.release_group("pool-a"), "delete_sandbox_claims_by_label"),
        )
        for name, call_release, delete_rpc in methods:
            for lifecycle, final_call in (("detach", "replace_lease"), ("release", "delete_lease")):
                with self.subTest(method=name, lifecycle=lifecycle, first="release_method"):
                    helper.reset_mock()
                    helper.delete_sandbox_claim.side_effect = None
                    helper.delete_sandbox_claims_by_label.side_effect = None
                    calls: list[str] = []
                    in_delete, unblock = threading.Event(), threading.Event()
                    self.addCleanup(unblock.set)
                    handle = _make_handle(
                        self.client,
                        groups=[BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
                    )
                    handle._start()
                    self.addCleanup(handle._stop_background_threads)
                    handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
                    handle._state.upsert_claim(
                        _claim("b1-1", warmpool="pool-b", conditions=_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                    )
                    other_member = handle.members("pool-b")[0]
                    helper.read_batch_lease.side_effect = (
                        lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                    )
                    helper.count_live_sandbox_claims.return_value = 0

                    def block_delete(*args, **kwargs):
                        if not in_delete.is_set():
                            calls.append("delete")
                            in_delete.set()
                            unblock.wait(5)

                    def record_replace(*args, **kwargs):
                        calls.append("replace_lease")
                        return _lease(holder_identity=args[2].spec.holder_identity, resource_version="11")

                    getattr(helper, delete_rpc).side_effect = block_delete
                    helper.replace_batch_lease.side_effect = record_replace
                    helper.delete_batch_lease.side_effect = lambda *a, **k: calls.append("delete_lease")
                    outcomes: dict[str, BaseException | None] = {}

                    def run(key, fn):
                        try:
                            fn()
                            outcomes[key] = None
                        except BaseException as e:
                            outcomes[key] = e

                    release_thread = threading.Thread(target=run, args=("release_op", lambda: call_release(handle)))
                    release_thread.start()
                    self.assertTrue(in_delete.wait(5))
                    lifecycle_thread = threading.Thread(target=run, args=("lifecycle", getattr(handle, lifecycle)))
                    lifecycle_thread.start()
                    _wait_until(lambda: handle._detached)
                    # Lease renewal stays alive and no Lease write or delete runs until the active delete finishes.
                    self.assertTrue(handle._renew_thread.is_alive())
                    self.assertEqual(calls, ["delete"])
                    with self.assertRaises(BatchError):
                        handle.release_member(other_member)
                    with self.assertLogs("k8s_agent_sandbox.sandbox_batch", level="WARNING") as logs:
                        logging.getLogger("k8s_agent_sandbox.sandbox_batch").warning("sentinel")
                        handle._delete_released_create("b1-9", "pool-a")
                    self.assertEqual([r.getMessage() for r in logs.records], ["sentinel"])

                    unblock.set()
                    for thread in (release_thread, lifecycle_thread):
                        thread.join(timeout=5)
                    self.assertIsNone(outcomes["lifecycle"])
                    if name == "release_group":
                        self.assertIsInstance(outcomes["release_op"], BatchError)
                        count_selectors = [
                            c.args[1] for c in helper.count_live_sandbox_claims.call_args_list
                        ]
                        self.assertNotIn(
                            batch_utils.group_label_selector("b1", "pool-a"), count_selectors
                        )
                    else:
                        self.assertIsNone(outcomes["release_op"])
                    self.assertEqual(calls[0], "delete")
                    self.assertIn(final_call, calls[1:])

            with self.subTest(method=name, first="detach"):
                helper.reset_mock()
                helper.delete_sandbox_claim.side_effect = None
                helper.delete_sandbox_claims_by_label.side_effect = None
                handle = _make_handle(self.client)
                helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                helper.replace_batch_lease.side_effect = (
                    lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
                )

                def detach_from_another_thread():
                    thread = threading.Thread(target=handle.detach)
                    thread.start()
                    thread.join(timeout=5)

                self.mock_sandbox_class.return_value = MagicMock(close_connection=detach_from_another_thread)
                handle._state.upsert_claim(
                    _claim("b1-0", conditions=_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                )
                handle.connect(handle.members()[0])
                handle._state.upsert_claim(
                    _claim("b1-0", conditions=_NOT_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                )
                with self.assertRaises(BatchError):
                    call_release(handle)
                self.assertTrue(handle._lease_released)
                helper.delete_sandbox_claim.assert_not_called()
                helper.delete_sandbox_claims_by_label.assert_not_called()

        with self.subTest("detach stops delete retry before backoff sleep"):
            helper.reset_mock()
            in_delete, unblock = threading.Event(), threading.Event()
            self.addCleanup(unblock.set)
            handle = _make_handle(self.client)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )

            def fail_after_detach(*args, **kwargs):
                in_delete.set()
                unblock.wait(5)
                raise k8s_client.ApiException(status=503)

            helper.delete_sandbox_claim.side_effect = fail_after_detach
            wait_mock = MagicMock()
            release_error: BaseException | None = None

            def run_release():
                nonlocal release_error
                try:
                    handle.release_member(handle.members()[0])
                except BaseException as e:
                    release_error = e

            with patch.object(handle._stop_creating, "wait", wait_mock):
                release_thread = threading.Thread(target=run_release)
                release_thread.start()
                self.assertTrue(in_delete.wait(5))
                detach_thread = threading.Thread(target=handle.detach)
                detach_thread.start()
                _wait_until(lambda: handle._detached)
                unblock.set()
                release_thread.join(timeout=5)
                detach_thread.join(timeout=5)
            self.assertIsInstance(release_error, BatchError)
            wait_mock.assert_not_called()
            helper.delete_sandbox_claim.assert_called_once()
            self.assertTrue(handle._lease_released)

        with self.subTest(
            "detach stops release_group before count and sleep and does not wait for sleep"
        ):
            # Detach during deletecollection skips count and retry sleep.
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            helper.count_live_sandbox_claims.side_effect = None
            in_delete, unblock_delete = threading.Event(), threading.Event()
            self.addCleanup(unblock_delete.set)
            handle = _make_handle(self.client)
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )
            helper.count_live_sandbox_claims.return_value = 1

            def block_deletecollection(*args, **kwargs):
                in_delete.set()
                unblock_delete.wait(5)

            helper.delete_sandbox_claims_by_label.side_effect = block_deletecollection
            sleep_mock = MagicMock()
            group_error: BaseException | None = None

            def run_group():
                nonlocal group_error
                try:
                    handle.release_group("pool-a")
                except BaseException as e:
                    group_error = e

            with patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5), \
                 patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=sleep_mock)):
                group_thread = threading.Thread(target=run_group)
                group_thread.start()
                self.assertTrue(in_delete.wait(5))
                detach_thread = threading.Thread(target=handle.detach)
                detach_thread.start()
                _wait_until(lambda: handle._detached)
                unblock_delete.set()
                group_thread.join(timeout=5)
                detach_thread.join(timeout=5)
            self.assertIsInstance(group_error, BatchError)
            helper.delete_sandbox_claims_by_label.assert_called_once()
            helper.count_live_sandbox_claims.assert_not_called()
            sleep_mock.assert_not_called()
            self.assertTrue(handle._lease_released)

            # Detach during count waits for count to finish and skips retry sleep.
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            in_count, unblock_count = threading.Event(), threading.Event()
            self.addCleanup(unblock_count.set)
            handle = _make_handle(self.client)
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )

            def block_count(*args, **kwargs):
                in_count.set()
                unblock_count.wait(5)
                return 1

            helper.count_live_sandbox_claims.side_effect = block_count
            sleep_mock = MagicMock()
            group_error = None
            with patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5), \
                 patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=sleep_mock)):
                group_thread = threading.Thread(target=run_group)
                group_thread.start()
                self.assertTrue(in_count.wait(5))
                detach_thread = threading.Thread(target=handle.detach)
                detach_thread.start()
                _wait_until(lambda: handle._detached)
                helper.replace_batch_lease.assert_not_called()
                unblock_count.set()
                group_thread.join(timeout=5)
                detach_thread.join(timeout=5)
            self.assertIsInstance(group_error, BatchError)
            helper.delete_sandbox_claims_by_label.assert_called_once()
            helper.count_live_sandbox_claims.assert_called_once()
            sleep_mock.assert_not_called()
            self.assertTrue(handle._lease_released)

            # Detach during retry sleep finishes without waiting for the sleep, and Round 2 does not run.
            helper.reset_mock()
            helper.count_live_sandbox_claims.side_effect = None
            helper.count_live_sandbox_claims.return_value = 1
            in_sleep, unblock_sleep = threading.Event(), threading.Event()
            self.addCleanup(unblock_sleep.set)
            handle = _make_handle(self.client)
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )

            def block_sleep(_seconds):
                in_sleep.set()
                unblock_sleep.wait(5)

            group_error = None
            with patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5), \
                 patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=block_sleep)):
                group_thread = threading.Thread(target=run_group)
                group_thread.start()
                self.assertTrue(in_sleep.wait(5))
                handle.detach()
                self.assertTrue(handle._lease_released)
                self.assertFalse(unblock_sleep.is_set())
                unblock_sleep.set()
                group_thread.join(timeout=5)
            self.assertIsInstance(group_error, BatchError)
            helper.delete_sandbox_claims_by_label.assert_called_once()

        with self.subTest("interrupted detach leaves release clearing detach_grace before waiting"):
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            helper.count_live_sandbox_claims.side_effect = None
            in_delete, unblock = threading.Event(), threading.Event()
            self.addCleanup(unblock.set)
            handle = _make_handle(self.client)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))

            def hang_delete(*args, **kwargs):
                in_delete.set()
                unblock.wait(5)

            helper.delete_sandbox_claim.side_effect = hang_delete
            delete_thread = threading.Thread(target=handle.release_member, args=(handle.members()[0],))
            delete_thread.start()
            self.assertTrue(in_delete.wait(5))

            with patch.object(handle._changed, "wait", side_effect=KeyboardInterrupt):
                with self.assertRaises(KeyboardInterrupt):
                    handle.detach(grace=30)
            self.assertTrue(handle._detach_unfinished())

            orig_wait = handle._changed.wait
            in_release_wait = threading.Event()
            should_interrupt = False

            def wait_then_interrupt(timeout=None):
                in_release_wait.set()
                orig_wait(timeout=5)
                if should_interrupt:
                    raise KeyboardInterrupt

            release_err: BaseException | None = None

            def run_interrupted_release():
                nonlocal release_err
                try:
                    handle.release()
                except BaseException as e:
                    release_err = e

            with patch.object(handle._changed, "wait", side_effect=wait_then_interrupt):
                release_thread = threading.Thread(target=run_interrupted_release)
                release_thread.start()
                self.assertTrue(in_release_wait.wait(5))
                self.assertTrue(handle._release_started())
                self.assertFalse(handle._detach_unfinished())
                with self.assertRaises(BatchError):
                    handle.detach()

                should_interrupt = True
                with handle._changed:
                    handle._changed.notify_all()
                release_thread.join(timeout=5)
            self.assertIsInstance(release_err, KeyboardInterrupt)
            self.assertFalse(handle._detach_unfinished())
            self.assertTrue(handle._release_started())

            unblock.set()
            delete_thread.join(timeout=5)

        with self.subTest("same-thread signal-handler reentry does not deadlock"):
            for lifecycle in ("detach", "release"):
                helper.reset_mock()
                helper.delete_sandbox_claims_by_label.side_effect = None
                helper.delete_batch_lease.side_effect = None
                helper.count_live_sandbox_claims.return_value = 0
                handle = _make_handle(self.client)
                handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
                helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                helper.replace_batch_lease.side_effect = (
                    lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
                )

                def reenter_lifecycle(*args, **kwargs):
                    getattr(handle, lifecycle)()
                    raise k8s_client.ApiException(status=503)

                helper.delete_sandbox_claim.side_effect = reenter_lifecycle
                sleep_mock = MagicMock()
                with patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=sleep_mock)):
                    with self.assertRaises(BatchError):
                        handle.release_member(handle.members()[0])
                sleep_mock.assert_not_called()
                helper.delete_sandbox_claim.assert_called_once()
                self.assertTrue(handle._lease_released)

            # A same-thread detach() or release() inside delete_sandbox_claim wakes a waiting detach().
            for reenter, expect_error in (("detach", False), ("release", True)):
                helper.reset_mock()
                in_delete, unblock = threading.Event(), threading.Event()
                self.addCleanup(unblock.set)
                handle = _make_handle(self.client)
                handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
                helper.count_live_sandbox_claims.return_value = 0
                helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                helper.replace_batch_lease.side_effect = (
                    lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
                )
                detach_error: BaseException | None = None
                detach_woke_before_delete_returned = False
                wait_calls = 0
                orig_wait = handle._changed.wait

                def count_wait(*args, **kwargs):
                    nonlocal wait_calls
                    wait_calls += 1
                    return orig_wait(*args, **kwargs)

                if reenter == "detach":
                    helper.read_batch_lease.side_effect = lambda *a, **k: (
                        _wait_until(lambda: wait_calls >= 2),
                        _lease(holder_identity=handle._holder_identity),
                    )[1]

                def run_detach():
                    nonlocal detach_error
                    try:
                        handle.detach()
                    except BaseException as e:
                        detach_error = e

                detach_thread = threading.Thread(target=run_detach)

                def reenter_while_detach_waits(*args, **kwargs):
                    nonlocal detach_woke_before_delete_returned
                    in_delete.set()
                    _wait_until(lambda: wait_calls >= 1)
                    getattr(handle, reenter)()
                    detach_thread.join(timeout=1)
                    detach_woke_before_delete_returned = not detach_thread.is_alive()
                    unblock.wait(5)

                helper.delete_sandbox_claim.side_effect = reenter_while_detach_waits
                with patch.object(handle._changed, "wait", side_effect=count_wait):
                    delete_thread = threading.Thread(target=handle.release_member, args=(handle.members()[0],))
                    delete_thread.start()
                    self.assertTrue(in_delete.wait(5))
                    detach_thread.start()
                    detach_thread.join(timeout=5)
                    unblock.set()
                    delete_thread.join(timeout=5)
                self.assertTrue(detach_woke_before_delete_returned)
                if expect_error:
                    self.assertIsInstance(detach_error, BatchError)
                    self.assertTrue(handle._released)
                else:
                    self.assertIsNone(detach_error)
                    self.assertTrue(handle._lease_released)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5)
    def test_release_methods_delete_nothing_once_err_is_set(self, _):
        helper = self.cluster.helper
        cause = BatchLeaseExpiredError("renewal stopped")
        watch_cause = RuntimeError("watch failed")

        def handle_with_a_pending_member():
            handle = _make_handle(self.client)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
            return handle

        for name, release in (
            ("release_member", lambda h: h.release_member(h.members()[0])),
            ("release_not_ready", lambda h: h.release_not_ready()),
            ("release_group", lambda h: h.release_group("pool-a")),
        ):
            with self.subTest(method=name):
                helper.reset_mock()
                handle = handle_with_a_pending_member()
                before = handle.members()
                handle._lease_error = cause
                with self.assertRaises(BatchError) as cm:
                    release(handle)
                self.assertIs(cm.exception.__cause__, cause)
                self.assertEqual(handle.members(), before)
                helper.delete_sandbox_claim.assert_not_called()
                helper.delete_sandbox_claims_by_label.assert_not_called()

            with self.subTest(watch_error_method=name):
                helper.reset_mock()
                helper.delete_sandbox_claim.side_effect = None
                helper.delete_sandbox_claims_by_label.side_effect = None
                helper.count_live_sandbox_claims.side_effect = None
                helper.count_live_sandbox_claims.return_value = 0
                handle = handle_with_a_pending_member()
                handle._state.note_error(watch_cause)
                self.assertIs(handle.err(), watch_cause)
                release(handle)
                self.assertEqual(handle.members(), [])
                if name == "release_group":
                    helper.delete_sandbox_claims_by_label.assert_called_once()
                else:
                    helper.delete_sandbox_claim.assert_called_once()

        with self.subTest("err set between delete attempts"):
            helper.reset_mock()
            handle = handle_with_a_pending_member()

            def fail_and_stop(*args, **kwargs):
                handle._lease_error = cause
                raise k8s_client.ApiException(status=503)

            helper.delete_sandbox_claim.side_effect = fail_and_stop
            wait_mock = MagicMock()
            with patch.object(handle._stop_creating, "wait", wait_mock):
                with self.assertRaises(BatchError) as cm:
                    handle.release_member(handle.members()[0])
            self.assertIs(cm.exception.__cause__, cause)
            wait_mock.assert_not_called()
            helper.delete_sandbox_claim.assert_called_once()

        with self.subTest("err set during retry sleep"):
            helper.reset_mock()
            handle = handle_with_a_pending_member()
            helper.delete_sandbox_claim.side_effect = k8s_client.ApiException(status=503)
            with patch.object(
                handle._stop_creating,
                "wait",
                side_effect=lambda _s=None: setattr(handle, "_lease_error", cause) or False,
            ):
                with self.assertRaises(BatchError) as cm:
                    handle.release_member(handle.members()[0])
            self.assertIs(cm.exception.__cause__, cause)
            helper.delete_sandbox_claim.assert_called_once()

        with self.subTest("err set during release_group deletecollection"):
            for fail in (False, True):
                helper.reset_mock()
                helper.count_live_sandbox_claims.side_effect = None
                helper.count_live_sandbox_claims.return_value = 1
                handle = handle_with_a_pending_member()

                def stop_during_deletecollection(*args, **kwargs):
                    handle._lease_error = cause
                    if fail:
                        raise k8s_client.ApiException(status=503)

                helper.delete_sandbox_claims_by_label.side_effect = stop_during_deletecollection
                sleep_mock = MagicMock()
                with patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=sleep_mock)):
                    with self.assertRaises(BatchError) as cm:
                        handle.release_group("pool-a")
                self.assertIs(cm.exception.__cause__, cause)
                helper.delete_sandbox_claims_by_label.assert_called_once()
                helper.count_live_sandbox_claims.assert_not_called()
                sleep_mock.assert_not_called()

        with self.subTest("err set during release_group count"):
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            handle = handle_with_a_pending_member()

            def stop_during_count(*args, **kwargs):
                handle._lease_error = cause
                return 1

            helper.count_live_sandbox_claims.side_effect = stop_during_count
            sleep_mock = MagicMock()
            with patch.object(sandbox_batch, "time", SimpleNamespace(monotonic=time.monotonic, sleep=sleep_mock)):
                with self.assertRaises(BatchError) as cm:
                    handle.release_group("pool-a")
            self.assertIs(cm.exception.__cause__, cause)
            helper.delete_sandbox_claims_by_label.assert_called_once()
            helper.count_live_sandbox_claims.assert_called_once()
            sleep_mock.assert_not_called()

    def test_watch_loop_deletes_late_fill_claims_in_released_pools_once(self):
        helper = self.cluster.helper
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=3, min_ready=1)])
        handle._state.upsert_claim(
            _claim("b1-0", conditions=_READY, sandbox={"name": "sbx-0"}, annotations=_FILL)
        )
        self.assertEqual([m.claim_name for m in handle.wait_for_quorum()], ["b1-0"])
        handle.release_not_ready()
        helper.delete_sandbox_claim.assert_not_called()

        terminating = _claim("b1-2", conditions=_NOT_READY, annotations=_FILL)
        terminating["metadata"]["deletionTimestamp"] = "2026-03-01T00:00:00Z"

        def stream(*_args, **_kwargs):
            yield {
                "type": "ADDED",
                "object": _claim("b1-1", conditions=_NOT_READY, annotations=_FILL),
            }
            yield {"type": "ADDED", "object": terminating}
            yield {
                "type": "MODIFIED",
                "object": _claim("b1-1", conditions=_READY, sandbox={"name": "sbx-1"}, annotations=_FILL),
            }
            handle._watch_stop.set()

        helper.watch_sandbox_claims.side_effect = stream
        handle._watch_loop()
        self.assertEqual([c.args[0] for c in helper.delete_sandbox_claim.call_args_list], ["b1-1"])
        self.assertEqual([m.claim_name for m in handle.members()], ["b1-0"])

    def test_watch_deletes_start_before_they_are_tracked(self):
        # _stop_background_threads joins every tracked delete thread, and joining a thread that
        # hasn't started raises, so a thread must start before it enters _watch_delete_threads.
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=1)])
        handle._state._watch_deletes.append("b1-0")
        with handle._lock:
            handle._start_watch_deletes_locked()
            tracked = list(handle._watch_delete_threads)
            self.assertEqual(len(tracked), 1)
            # join() raises RuntimeError on a thread that hasn't started.
            tracked[0].join(timeout=0)
        tracked[0].join(timeout=3.0)
        self.assertFalse(tracked[0].is_alive())

    def test_lease_loss_wakes_delete_claim_retry_sleep(self):
        helper = self.cluster.helper
        handle = _make_handle(self.client)
        handle._state.upsert_claim(_claim("b1-0", conditions=_READY, sandbox={"name": "sbx-0"}))
        member = handle.members()[0]
        helper.delete_sandbox_claim.side_effect = k8s_client.ApiException(status=503)
        entered_retry = threading.Event()
        lease_error = BatchLeaseExpiredError("lease gone")
        errors: list[BaseException] = []

        def retry_delay(_attempt, _err=None):
            entered_retry.set()
            return 10.0

        def run_release():
            try:
                handle.release_member(member)
            except BaseException as exc:
                errors.append(exc)

        with patch("k8s_agent_sandbox.batch_utils.retry_delay", side_effect=retry_delay):
            started = time.monotonic()
            worker = threading.Thread(target=run_release)
            worker.start()
            self.assertTrue(entered_retry.wait(5))
            handle._stop_renewal(lease_error)
            worker.join(timeout=3.0)
            elapsed = time.monotonic() - started

        self.assertFalse(worker.is_alive())
        self.assertLess(elapsed, 2.5)
        self.assertEqual(helper.delete_sandbox_claim.call_count, 1)
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], BatchError)
        self.assertIs(errors[0].__cause__, lease_error)

    def test_release_group_clamps_rounds_and_sleep_to_lease_deadline(self):
        helper = self.cluster.helper
        clock = [100.0]
        sleeps: list[float] = []

        def fake_sleep(delay: float) -> None:
            sleeps.append(delay)
            clock[0] += delay

        with patch.object(
            sandbox_batch, "time", SimpleNamespace(monotonic=lambda: clock[0], sleep=fake_sleep)
        ):
            handle = _make_handle(self.client, lease_duration=60, lease_renewed_at=42.5)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))

            def first_delete(*_args, **_kwargs):
                clock[0] = 101.0

            def first_count(*_args, **_kwargs):
                clock[0] = 102.45
                return 1

            helper.delete_sandbox_claims_by_label.side_effect = first_delete
            helper.count_live_sandbox_claims.side_effect = first_count

            with self.assertRaises(BatchError) as cm:
                handle.release_group("pool-a")

        self.assertIsInstance(cm.exception.__cause__, BatchLeaseExpiredError)
        self.assertAlmostEqual(
            helper.delete_sandbox_claims_by_label.call_args.kwargs["_request_timeout"], 2.5
        )
        self.assertAlmostEqual(
            helper.count_live_sandbox_claims.call_args.kwargs["_request_timeout"], 1.5
        )
        self.assertEqual(helper.count_live_sandbox_claims.call_args.kwargs["deadline"], 102.5)
        self.assertEqual(len(sleeps), 1)
        self.assertAlmostEqual(sleeps[0], 0.05)


class TestAcquire(BaseClaimBatchTest):

    def test_creates_a_claim_outside_the_fill_from_any_pool_and_returns_it_ready(self):
        helper = self.cluster.helper
        # A size-0 group creates no claims.
        groups = [BatchGroup(warmpool="pool-a", size=2), BatchGroup(warmpool="pool-b", size=0)]
        batch = self.claim(groups, labels={"team": "rl"}, quorum_timeout=90, work_budget=120)
        _wait_until(lambda: len(batch.members()) == 2)
        # So a pool's Ready count isn't read again, which would add GETs to the checks counted below.
        refresh = patch.object(batch_utils, "BATCH_WARM_CREDIT_REFRESH_SECONDS", 3600)
        refresh.start()
        self.addCleanup(refresh.stop)
        _SteppingDatetime.calls = 0
        with patch("k8s_agent_sandbox.utils.datetime", _SteppingDatetime):
            member = batch.acquire("pool-c")
        batch.acquire("pool-c")
        batch.acquire("pool-b")

        self.assertEqual((member.claim_name, member.warmpool, member.state), ("b1-2", "pool-c", MemberState.READY))
        # A pool that isn't one of the batch's groups is checked, once.
        self.assertEqual(
            [c.args[0] for c in helper.get_sandbox_warmpool.call_args_list], ["pool-a", "pool-b", "pool-c"]
        )
        create = helper.create_sandbox_claim.call_args_list[2]
        self.assertEqual(create.args[:2], ("b1-2", "pool-c"))
        # The group label but no group annotations.
        self.assertEqual(
            create.kwargs["labels"],
            {
                "team": "rl",
                BATCH_ID_LABEL: "b1",
                BATCH_LEASE_UID_LABEL: "lease-uid",
                BATCH_GROUP_LABEL: batch_utils.group_label_value("pool-c"),
            },
        )
        self.assertEqual(create.kwargs["annotations"], {})
        # Its own shutdownTime, counted from its create.
        self.assertEqual(
            create.kwargs["lifecycle"], {"shutdownTime": "2026-01-01T01:13:30Z", "shutdownPolicy": "Delete"}
        )
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0", "b1-1", "b1-2", "b1-3", "b1-4"])
        # release_group() also takes a pool that only acquire() used.
        batch.release_group("pool-c")
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0", "b1-1", "b1-4"])
        self.assertEqual([e.member.claim_name for e in _drain(batch.events())], ["b1-0", "b1-1"])

    def test_concurrent_first_acquires_check_a_pool_once_and_a_failed_check_is_not_kept(self):
        helper = self.cluster.helper
        pool = {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
        checked: list[str] = []
        release_check = threading.Event()
        pool_c_results = [None, pool]

        def get_warmpool(name, *args, **kwargs):
            checked.append(name)
            if name != "pool-a":
                release_check.wait(timeout=2)
            if name == "pool-c":
                return pool_c_results.pop(0)
            if name == "pool-d":
                raise TimeoutError("request timed out")
            return pool

        waiting: list[Future] = []

        class _WaitedFuture(Future):
            def result(self, timeout=None):
                waiting.append(self)
                return super().result(timeout)

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        batch = self.claim()
        _wait_until(lambda: len(batch.members()) == 2)

        def acquire_three(warmpool):
            waiting.clear()
            release_check.clear()
            with ThreadPoolExecutor(max_workers=3) as executor, \
                 patch.object(sandbox_batch, "Future", _WaitedFuture):
                acquires = [executor.submit(batch.acquire, warmpool) for _ in range(3)]
                # The first check finishes only once the other two acquires wait on it.
                _wait_until(lambda: len(waiting) == 2)
                release_check.set()
                return [a.exception(timeout=2) or a.result() for a in acquires]

        self.assertEqual(sorted(m.claim_name for m in acquire_three("pool-b")), ["b1-2", "b1-3", "b1-4"])
        errors = acquire_three("pool-c")
        self.assertEqual([type(e) for e in errors], [SandboxWarmPoolNotFoundError] * 3)
        self.assertEqual(batch.acquire("pool-c").warmpool, "pool-c")
        # A check that times out itself reaches every acquire as its own error, not as a timeout of
        # the wait for it.
        errors = acquire_three("pool-d")
        self.assertEqual([(type(e), str(e)) for e in errors], [(TimeoutError, "request timed out")] * 3)

        self.assertEqual(checked, ["pool-a", "pool-b", "pool-c", "pool-c", "pool-d"])
        # Retrying a failed release_group() still accepts an undeclared pool after the first call
        # dropped its members.
        denied = k8s_client.ApiException(status=403)
        helper.delete_sandbox_claims_by_label.side_effect = [denied, None]
        with self.assertRaises(k8s_client.ApiException):
            batch.release_group("pool-b")
        batch.release_group("pool-b")
        self.assertEqual(helper.delete_sandbox_claims_by_label.call_count, 2)

    def test_an_interrupted_pool_check_is_run_again_by_a_waiting_acquire(self):
        checked: list[str] = []
        release_check = threading.Event()
        waiting: list[Future] = []

        class _Interrupt(BaseException):
            pass

        class _WaitedFuture(Future):
            def result(self, timeout=None):
                waiting.append(self)
                return super().result(timeout)

        def get_warmpool(name, *args, **kwargs):
            checked.append(name)
            if name == "pool-b" and checked.count("pool-b") == 1:
                release_check.wait(timeout=2)
                raise _Interrupt()
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}

        self.cluster.helper.get_sandbox_warmpool.side_effect = get_warmpool
        batch = self.claim()
        _wait_until(lambda: len(batch.members()) == 2)
        with ThreadPoolExecutor(max_workers=2) as executor, patch.object(sandbox_batch, "Future", _WaitedFuture):
            first = executor.submit(batch.acquire, "pool-b")
            _wait_until(lambda: "pool-b" in checked)
            second = executor.submit(batch.acquire, "pool-b")
            _wait_until(lambda: len(waiting) == 1)
            release_check.set()
            self.assertIsInstance(first.exception(timeout=2), _Interrupt)
            self.assertEqual(second.result(timeout=2).warmpool, "pool-b")
        self.assertEqual(checked, ["pool-a", "pool-b", "pool-b"])

    def test_an_acquire_waiting_on_another_acquires_pool_check_stops_at_its_timeout(self):
        helper = self.cluster.helper
        checked: list[str] = []
        release_check = threading.Event()
        interrupt = False
        race_error: type[BaseException] | None = None

        class _Interrupt(BaseException):
            pass

        class _RaceFuture(Future):
            def result(self, timeout=None):
                if timeout is not None and race_error is not None:
                    time.sleep(timeout + 0.02)
                    release_check.set()
                    _wait_until(self.cancelled)
                    raise race_error
                return super().result(timeout)

        def get_warmpool(name, *args, **kwargs):
            checked.append(name)
            if name == "pool-d":
                return {"spec": {"sandboxTemplateRef": {"name": "tmpl-d"}}}
            if name != "pool-a":
                release_check.wait(timeout=2)
                if interrupt:
                    raise _Interrupt()
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        helper.get_sandbox_template.side_effect = lambda name, *a, **k: (
            time.sleep(0.08) if name == "tmpl-d" else None
        ) or {}
        batch = self.claim()
        _wait_until(lambda: len(batch.members()) == 2)
        with ThreadPoolExecutor(max_workers=1) as executor, patch.object(sandbox_batch, "Future", _RaceFuture):
            first = executor.submit(batch.acquire, "pool-b")
            _wait_until(lambda: "pool-b" in batch._prechecks)
            with self.assertRaisesRegex(BatchTimeoutError, "wasn't checked within 0.05s"):
                batch.acquire("pool-b", timeout=0.05)
            release_check.set()
            self.assertEqual(first.result(timeout=2).claim_name, "b1-2")

            # A check cancelled as or after a waiter's timeout expires neither leaks CancelledError
            # nor starts a second check past the deadline.
            interrupt = True
            for pool_name, race_error in (("pool-c", TimeoutError), ("pool-e", sandbox_batch.CancelledError)):
                release_check.clear()
                first = executor.submit(batch.acquire, pool_name)
                _wait_until(lambda: pool_name in batch._prechecks)
                with self.assertRaisesRegex(BatchTimeoutError, "wasn't checked within 0.02s"):
                    batch.acquire(pool_name, timeout=0.02)
                self.assertIsInstance(first.exception(timeout=2), _Interrupt)
                self.assertEqual(checked.count(pool_name), 1)

        # A check that finishes past the deadline creates no claim and takes no ordinal.
        with self.assertRaisesRegex(BatchTimeoutError, "no create slot for warm pool 'pool-d' within 0.05s"):
            batch.acquire("pool-d", timeout=0.05)
        self.assertEqual(helper.create_sandbox_claim.call_count, 3)
        self.assertEqual(batch._state.next_ordinal, 3)

    def test_release_group_frees_the_acquired_members_of_a_size_zero_group(self):
        helper = self.cluster.helper
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=0)])
        _wait_until(lambda: len(batch.members()) == 1)
        batch.acquire("pool-b")
        self.cluster.ready_on_create = False
        with ThreadPoolExecutor(1) as pool:
            future = pool.submit(batch.acquire, "pool-b", timeout=5)
            _wait_until(lambda: helper.create_sandbox_claim.call_count == 3)
            batch.release_group("pool-b")
            # An acquire still waiting on the pool raises at once.
            self.assertRegex(str(future.exception(timeout=5)), "released before it was Ready")
        self.assertEqual(
            helper.delete_sandbox_claims_by_label.call_args.args,
            (
                "default",
                (
                    f"{BATCH_ID_LABEL}=b1,"
                    f"{BATCH_GROUP_LABEL}={batch_utils.group_label_value('pool-b')},"
                    f"{BATCH_LEASE_UID_LABEL}=lease-uid"
                ),
            ),
        )
        # Its create may have landed after the label delete, so it is also deleted by name.
        self.assertEqual([c.args[0] for c in helper.delete_sandbox_claim.call_args_list], ["b1-2"])
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0"])

        # An acquire whose pacing slot comes after the release sends no create and leaves nothing to delete.
        batch._next_create_at = time.monotonic() + 0.3
        with ThreadPoolExecutor(1) as pool:
            future = pool.submit(batch.acquire, "pool-b", timeout=5)
            _wait_until(lambda: batch._state.next_ordinal == 4)
            batch.release_group("pool-b")
            self.assertRegex(str(future.exception(timeout=5)), "released before it was Ready")
        batch.release_not_ready()
        self.assertEqual(helper.create_sandbox_claim.call_count, 3)
        self.assertEqual([c.args[0] for c in helper.delete_sandbox_claim.call_args_list], ["b1-2"])

    def test_release_group_during_warm_credit_refresh_stops_acquire(self):
        helper = self.cluster.helper
        read_started = threading.Event()
        read_gate: threading.Event | None = None
        ready = {"pool-a": 0, "pool-unseen": 2, "pool-refresh": 2}

        def get_warmpool(name, *args, **kwargs):
            gate = read_gate
            if gate is not None and name in ("pool-unseen", "pool-refresh"):
                read_started.set()
                gate.wait(5)
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}, "status": {"readyReplicas": ready[name]}}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(batch_utils, "time", fake_time):
            batch = self.claim([BatchGroup(warmpool="pool-a", size=0)])

            # Releasing an unseen pool during its first-use check stops the in-flight acquire
            # without raising ValueError or burning an ordinal.
            helper.create_sandbox_claim.reset_mock()
            helper.delete_sandbox_claim.reset_mock()
            read_gate = threading.Event()
            self.addCleanup(read_gate.set)
            with ThreadPoolExecutor(1) as pool:
                checking = pool.submit(batch.acquire, "pool-unseen", timeout=5)
                self.assertTrue(read_started.wait(5))
                batch.release_group("pool-unseen")
                read_gate.set()
                with self.assertRaisesRegex(BatchError, "was released while acquiring"):
                    checking.result(timeout=5)
            read_gate = None
            read_started.clear()
            helper.create_sandbox_claim.assert_not_called()
            helper.delete_sandbox_claim.assert_not_called()
            self.assertEqual(batch._state.next_ordinal, 0)
            self.assertEqual(batch._state._checking_acquires, {})
            self.assertEqual(batch._state._released, set())

            # Timing out with no create slot after a warm-credit refresh finds 0 Ready replicas
            # neither advances next_ordinal nor records a released claim.
            offset[0] += 1.0
            batch._next_create_at = time.monotonic() + 10.0
            with self.assertRaisesRegex(BatchTimeoutError, "no create slot for warm pool 'pool-a'"):
                batch.acquire("pool-a", timeout=0.2)
            self.assertEqual(batch._state.next_ordinal, 0)
            self.assertEqual(batch._state._checking_acquires, {})
            self.assertEqual(batch._state._released, set())

            batch._next_create_at = 0.0
            first = batch.acquire("pool-refresh")
            second = batch.acquire("pool-refresh")
            batch.release_member(first)
            batch.release_member(second)
            self.assertFalse(batch._state.has_pool("pool-refresh"))

            offset[0] += 1.0
            batch._next_create_at = time.monotonic() + 0.3
            helper.create_sandbox_claim.reset_mock()
            helper.delete_sandbox_claim.reset_mock()
            read_gate = threading.Event()
            self.addCleanup(read_gate.set)
            with ThreadPoolExecutor(1) as pool:
                acquiring = pool.submit(batch.acquire, "pool-refresh", timeout=5)
                self.assertTrue(read_started.wait(5))
                batch.release_group("pool-refresh")
                read_gate.set()
                with self.assertRaisesRegex(BatchError, "was released while acquiring"):
                    acquiring.result(timeout=5)
            helper.create_sandbox_claim.assert_not_called()
            helper.delete_sandbox_claim.assert_not_called()
            self.assertEqual(batch._state.next_ordinal, 2)
            self.assertEqual(batch._state._checking_acquires, {})
            self.assertEqual(batch._state._acquiring, {})
            self.assertEqual(batch._state._released, {"b1-0", "b1-1"})
            self.assertEqual(batch._acquire_waiters, {})

    def test_acquires_the_warm_pool_covers_skip_pacing_and_reread_its_count_once_a_second(self):
        helper = self.cluster.helper
        ready = {"pool-a": 2}
        short_read_started = threading.Event()
        short_timeouts: list[float] = []

        def get_warmpool(name, *args, **kwargs):
            timeout = kwargs.get("_request_timeout")
            if timeout is not None and timeout < 1:
                short_timeouts.append(timeout)
                short_read_started.set()
                time.sleep(timeout)
                raise urllib3.exceptions.ReadTimeoutError(None, None, "timed out")
            # Slow enough that concurrent acquires would overlap their reads.
            time.sleep(0.05)
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}, "status": {"readyReplicas": ready[name]}}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(batch_utils, "time", fake_time):
            batch = self.claim([BatchGroup(warmpool="pool-a", size=0)], max_in_flight=4, create_rps=2)
            start = time.monotonic()
            with ThreadPoolExecutor(4) as pool:
                list(pool.map(lambda _: batch.acquire("pool-a"), range(4)))
            started = sorted(t - start for _, t in self.cluster.created)
            # Two are covered and the first paced one takes its slot at once. The count was just
            # read, so it isn't read again.
            self.assertLess(started[2], 0.15)
            self.assertGreaterEqual(started[3], 0.499)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 1)

            ready["pool-a"] = 3
            offset[0] += 1.0
            self.cluster.created.clear()
            start = time.monotonic()
            with ThreadPoolExecutor(3) as pool:
                list(pool.map(lambda _: batch.acquire("pool-a"), range(3)))
            # One read covers all three, which would otherwise wait for pacing slots.
            self.assertLess(max(t - start for _, t in self.cluster.created), 0.2)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # When the next pacing slot is already free, acquire() starts at once without paying a
            # pool GET, while advancing _next_create_at for concurrent callers behind it.
            offset[0] += 1.0
            batch._next_create_at = 0.0
            self.cluster.created.clear()
            start = time.monotonic()
            batch.acquire("pool-a")
            self.assertLess(self.cluster.created[0][1] - start, 0.1)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # A short-deadline acquire caps its pool read to its deadline and doesn't suppress
            # concurrent refreshes when its own deadline cuts the read short.
            ready["pool-a"] = 2
            self.cluster.created.clear()
            start = time.monotonic()
            with ThreadPoolExecutor(3) as pool:
                short = pool.submit(batch.acquire, "pool-a", timeout=0.15)
                self.assertTrue(short_read_started.wait(5))
                longer = [pool.submit(batch.acquire, "pool-a", timeout=5) for _ in range(2)]
                with self.assertRaises(BatchTimeoutError):
                    short.result(timeout=5)
                for fut in longer:
                    fut.result(timeout=5)
            self.assertEqual(len(short_timeouts), 1)
            self.assertLessEqual(short_timeouts[0], 0.15)
            self.assertLess(max(t - start for _, t in self.cluster.created), 0.35)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)

            # During a 429 cooldown, acquire() paces without re-reading the pool, then refreshes
            # again once the cooldown expires.
            ready["pool-a"] = 2
            offset[0] += 1.0
            batch._warm_credit.throttle()
            batch._next_create_at = time.monotonic() + 0.05
            batch.acquire("pool-a")
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)
            offset[0] += 5.0
            self.cluster.created.clear()
            start = time.monotonic()
            with ThreadPoolExecutor(2) as pool:
                list(pool.map(lambda _: batch.acquire("pool-a"), range(2)))
            self.assertLess(max(t - start for _, t in self.cluster.created), 0.2)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 5)

            # A pool that isn't one of the batch's groups gets its credit from its check.
            ready["pool-c"] = 1
            self.cluster.created.clear()
            start = time.monotonic()
            batch.acquire("pool-c")
            self.assertLess(self.cluster.created[0][1] - start, 0.2)

    def test_cold_claim_defers_acquire_refresh_fences_in_flight_read_and_zeroes_on_relist(self):
        helper = self.cluster.helper
        ready = {"pool-a": 3, "pool-b": 4}
        in_flight_gate: threading.Event | None = None
        in_flight_entered = threading.Event()

        def get_warmpool(name, *args, **kwargs):
            gate = in_flight_gate
            if gate is not None and name == "pool-a":
                in_flight_entered.set()
                gate.wait(5)
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}, "status": {"readyReplicas": ready[name]}}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(batch_utils, "time", fake_time):
            batch = self.claim(
                [BatchGroup(warmpool="pool-a", size=0), BatchGroup(warmpool="pool-b", size=0)],
                max_in_flight=4,
                create_rps=4,
            )
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)
            # Age the precheck read past BATCH_WARM_CREDIT_REFRESH_SECONDS so only exhaust()
            # updating _read_at keeps acquire() from immediately re-reading stale readyReplicas.
            offset[0] += 2.0
            self.cluster.ready_on_create = False
            with ThreadPoolExecutor(1) as pool:
                first = pool.submit(batch.acquire, "pool-a", timeout=5)
                _wait_until(lambda: len(self.cluster.created) == 1)
                cold_pending = [{"type": "Ready", "status": "False", "reason": "SandboxCreatePending"}]
                self.cluster.watch_queue.put(
                    {"type": "ADDED", "object": _claim("b1-0", "pool-a", conditions=cold_pending, annotations=None)}
                )
                _wait_until(lambda: len(batch.members()) == 1)
                self.cluster.push("b1-0", "pool-a", ready=True, annotations=None)
                self.assertEqual(first.result(timeout=5).claim_name, "b1-0")

            self.cluster.ready_on_create = True
            batch._next_create_at = time.monotonic() + 0.2
            self.cluster.created.clear()
            start = time.monotonic()
            batch.acquire("pool-a")
            self.assertGreaterEqual(self.cluster.created[0][1] - start, 0.18)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # pool-b keeps its warm credit while pool-a is exhausted.
            self.cluster.created.clear()
            start = time.monotonic()
            batch.acquire("pool-b")
            self.assertLess(self.cluster.created[0][1] - start, 0.1)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # Once BATCH_WARM_CREDIT_REFRESH_SECONDS elapses, acquire() refreshes pool-a again.
            offset[0] += batch_utils.BATCH_WARM_CREDIT_REFRESH_SECONDS
            ready["pool-a"] = 2
            batch._next_create_at = time.monotonic() + 0.3
            self.cluster.created.clear()
            start = time.monotonic()
            batch.acquire("pool-a")
            self.assertLess(self.cluster.created[0][1] - start, 0.15)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 3)

            # Spend pool-a's remaining credit and age _read_at so the next acquire starts a refresh.
            batch.acquire("pool-a")
            offset[0] += 2.0
            ready["pool-a"] = 5
            in_flight_gate = threading.Event()
            self.addCleanup(in_flight_gate.set)
            batch._next_create_at = time.monotonic() + 0.05
            with ThreadPoolExecutor(1) as pool:
                refreshing = pool.submit(batch.acquire, "pool-a", timeout=5)
                self.assertTrue(in_flight_entered.wait(5))
                cold_not_ready = [{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}]
                self.cluster.watch_queue.put(
                    {"type": "ADDED", "object": _claim("b1-90", "pool-a", conditions=cold_not_ready, annotations=None)}
                )
                _wait_until(lambda: any(m.claim_name == "b1-90" for m in batch.members()))
                in_flight_gate.set()
                in_flight_gate = None
                refreshing.result(timeout=5)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)

            # The stale readyReplicas=5 read started before b1-90 arrived did not overwrite 0.
            batch._next_create_at = time.monotonic() + 0.2
            self.cluster.created.clear()
            start = time.monotonic()
            batch.acquire("pool-a")
            self.assertGreaterEqual(self.cluster.created[0][1] - start, 0.18)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)

            # A 410 watch relist containing a cold claim also zeroes that pool's remaining credit.
            relist_cold = [{"type": "Ready", "status": "False", "reason": "SandboxCreatePending"}]
            helper.list_sandbox_claim_objects.return_value = (
                [_claim("b1-99", "pool-b", conditions=relist_cold, annotations=None)],
                "9",
            )
            raise_410 = True

            def watch_with_410(*args, **kwargs):
                nonlocal raise_410
                if raise_410:
                    raise_410 = False
                    raise k8s_client.ApiException(status=410)
                yield from self.cluster._watch(*args, **kwargs)

            helper.watch_sandbox_claims.side_effect = watch_with_410
            _wait_until(lambda: any(m.claim_name == "b1-99" for m in batch.members()))
            batch._next_create_at = time.monotonic() + 0.2
            self.cluster.created.clear()
            start = time.monotonic()
            batch.acquire("pool-b")
            self.assertGreaterEqual(self.cluster.created[0][1] - start, 0.18)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)

    def test_fill_and_acquire_creates_share_max_in_flight(self):
        for outcome in ("timeout", "release_group", "err"):
            with self.subTest(outcome):
                self.cluster = _Cluster()
                self.client.k8s_helper = helper = self.cluster.helper
                helper.get_sandbox_warmpool.return_value = {
                    "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
                    "status": {"readyReplicas": 3 if outcome == "timeout" else 4},
                }
                gate = threading.Event()
                self.addCleanup(gate.set)
                self.cluster.create_hook = lambda name, g=gate: name == "b1-0" and g.wait(5)
                groups = (
                    [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=0)]
                    if outcome == "release_group"
                    else [BatchGroup(warmpool="pool-a", size=2 if outcome == "err" else 1)]
                )
                batch = self.claim(groups, max_in_flight=1)
                self.addCleanup(batch._stop_background_threads)
                _wait_until(lambda: helper.create_sandbox_claim.call_count == 1)
                if outcome == "timeout":
                    with self.assertRaisesRegex(BatchTimeoutError, "no create in flight slot"):
                        batch.acquire("pool-a", timeout=0.05)
                    self.assertEqual(helper.create_sandbox_claim.call_count, 1)
                    helper.delete_sandbox_claim.assert_not_called()

                    # First-use pool checks share max_in_flight too, and a short-deadline leader
                    # that times out waiting for a slot hands the check to a longer-deadline follower.
                    leader_waiting = threading.Event()
                    follower_waiting = threading.Event()
                    self.addCleanup(follower_waiting.set)

                    class _WaitedFuture(Future):
                        def result(self, timeout=None):
                            follower_waiting.set()
                            return super().result(timeout)

                    orig_in_flight_acquire = batch._in_flight.acquire
                    leader_calls = 0

                    def leader_times_out_after_follower_waits(*args, **kwargs):
                        nonlocal leader_calls
                        leader_calls += 1
                        if leader_calls == 1:
                            leader_waiting.set()
                            self.assertTrue(follower_waiting.wait(5))
                        return orig_in_flight_acquire(*args, **kwargs)

                    with (
                        ThreadPoolExecutor(2) as pool,
                        patch.object(sandbox_batch, "Future", _WaitedFuture),
                        patch.object(batch._in_flight, "acquire", side_effect=leader_times_out_after_follower_waits),
                    ):
                        short_check = pool.submit(batch.acquire, "pool-b", timeout=0.25)
                        self.assertTrue(leader_waiting.wait(5))
                        follower = pool.submit(batch.acquire, "pool-b", timeout=5)
                        self.assertTrue(follower_waiting.wait(5))
                        with self.assertRaisesRegex(BatchTimeoutError, "warm pool 'pool-b' wasn't checked"):
                            short_check.result(timeout=5)
                        self.assertEqual(helper.get_sandbox_warmpool.call_count, 1)
                        gate.set()
                        checked_member = follower.result(timeout=5)
                    self.assertEqual(checked_member.warmpool, "pool-b")
                    _wait_until(lambda: not any(c.is_alive() for c in batch._creators))
                    fill_member = next(m for m in batch.members() if m.claim_name == "b1-0")
                    self.assertEqual(batch.acquire("pool-a").claim_name, "b1-3")
                    batch._next_create_at = 0.0
                    offset = [0.0]
                    fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0], sleep=time.sleep)

                    def expire_during_pacing(_timeout, _cancel=None):
                        offset[0] += 10.0
                        return False

                    with (
                        patch.object(sandbox_batch, "time", fake_time),
                        patch.object(batch._stop_creating, "wait_or", side_effect=expire_during_pacing),
                    ):
                        with self.assertRaisesRegex(BatchTimeoutError, "no create in flight slot"):
                            batch.acquire("pool-a", timeout=5)
                    self.assertEqual(helper.create_sandbox_claim.call_count, 3)
                    helper.delete_sandbox_claim.assert_not_called()

                    # Expiring while waiting on _lock after _in_flight.acquire succeeds skips
                    # on_send and create_sandbox_claim and releases the _in_flight slot.
                    in_flight_taken = threading.Event()
                    lock_held = threading.Event()
                    self.addCleanup(lock_held.set)
                    orig_lock = batch._lock
                    signaling_lock = _SignalingLock(orig_lock)

                    def take_slot_then_wait_for_lock(*args, **kwargs):
                        res = orig_in_flight_acquire(*args, **kwargs)
                        self.assertTrue(res)
                        in_flight_taken.set()
                        self.assertTrue(lock_held.wait(5))
                        return res

                    batch._next_create_at = 0.0
                    with (
                        patch.object(sandbox_batch, "time", fake_time),
                        patch.object(batch._in_flight, "acquire", side_effect=take_slot_then_wait_for_lock),
                        ThreadPoolExecutor(1) as pool,
                    ):
                        acquiring = pool.submit(batch.acquire, "pool-a", timeout=5)
                        self.assertTrue(in_flight_taken.wait(5))
                        batch._lock = signaling_lock  # type: ignore[assignment]
                        try:
                            with signaling_lock:
                                lock_held.set()
                                self.assertTrue(signaling_lock._blocked_or_done.wait(5))
                                offset[0] += 10.0
                            with self.assertRaisesRegex(BatchTimeoutError, "no create in flight slot"):
                                acquiring.result(timeout=5)
                        finally:
                            batch._lock = orig_lock
                    self.assertEqual(helper.create_sandbox_claim.call_count, 3)
                    helper.delete_sandbox_claim.assert_not_called()
                    self.assertTrue(batch._in_flight.acquire(blocking=False))
                    batch._in_flight.release()

                    # Warm-credit refreshes share max_in_flight with member deletes as well.
                    delete_gate = threading.Event()
                    delete_started = threading.Event()
                    self.addCleanup(delete_gate.set)

                    def slow_delete(*args, **kwargs):
                        delete_started.set()
                        delete_gate.wait(5)

                    helper.delete_sandbox_claim.side_effect = slow_delete
                    with patch.object(batch_utils, "BATCH_WARM_CREDIT_REFRESH_SECONDS", 0.0), ThreadPoolExecutor(1) as pool:
                        deleting = pool.submit(batch.release_member, fill_member)
                        self.assertTrue(delete_started.wait(5))
                        batch._next_create_at = time.monotonic() + 0.5
                        before_reads = helper.get_sandbox_warmpool.call_count
                        with self.assertRaisesRegex(BatchTimeoutError, "no create slot for warm pool 'pool-a'"):
                            batch.acquire("pool-a", timeout=0.03)
                        self.assertEqual(helper.get_sandbox_warmpool.call_count, before_reads)
                        delete_gate.set()
                        deleting.result(timeout=5)
                    batch._stop_background_threads()
                    continue
                target_pool = "pool-b" if outcome == "release_group" else "pool-a"
                entered_in_flight = threading.Event()
                orig_in_flight_acquire = batch._in_flight.acquire

                def record_acquire_waiting(*args, **kwargs):
                    if threading.current_thread() not in batch._creators:
                        entered_in_flight.set()
                    return orig_in_flight_acquire(*args, **kwargs)

                with (
                    patch.object(batch._in_flight, "acquire", side_effect=record_acquire_waiting),
                    ThreadPoolExecutor(1) as pool,
                ):
                    future = pool.submit(batch.acquire, target_pool, timeout=5)
                    self.assertTrue(entered_in_flight.wait(5))
                    if outcome == "release_group":
                        batch.release_group("pool-b")
                    else:
                        watch_err = k8s_client.ApiException(status=403)
                        helper.watch_sandbox_claims.side_effect = watch_err
                        _wait_until(lambda: batch.err() is watch_err)
                    gate.set()
                    raised = future.exception(timeout=5)
                _wait_until(lambda: not any(c.is_alive() for c in batch._creators))
                self.assertIsInstance(raised, BatchError)
                self.assertNotIsInstance(raised, BatchTimeoutError)
                if outcome == "release_group":
                    self.assertRegex(str(raised), "released before it was Ready")
                else:
                    self.assertIs(raised.__cause__, batch.err())
                self.assertEqual(helper.create_sandbox_claim.call_count, 1)
                helper.delete_sandbox_claim.assert_not_called()
                batch._stop_background_threads()

    def test_failures_drop_the_member_and_delete_a_claim_that_may_exist(self):
        terminal = [{"type": "Ready", "status": "False", "reason": "InvalidConfiguration"}]
        failure = RuntimeError("watch failed")
        lease_failure = BatchLeaseExpiredError("renewal stopped")

        def push_terminal(batch):
            self.cluster.watch_queue.put({"type": "MODIFIED", "object": _claim("b1-1", conditions=terminal)})

        def delete(batch):
            self.cluster.push("b1-1", ready=False, annotations=None)
            _wait_until(lambda: batch.members()[-1].claim_name == "b1-1")
            self.cluster.watch_queue.put({"type": "DELETED", "object": _claim("b1-1")})

        def release(batch):
            self.cluster.push("b1-1", ready=False, annotations=None)
            _wait_until(lambda: batch.members()[-1].claim_name == "b1-1")
            batch.release_member(batch.members()[-1])

        def fail_watch(batch):
            with batch._changed:
                batch._state.note_error(failure)
                batch._notify_all()

        def lose_lease(batch):
            with batch._changed:
                batch._lease_error = lease_failure
                batch._notify_all()

        def wait(batch):
            pass

        unavailable = k8s_client.ApiException(status=503)
        unavailable.headers = {"Retry-After": "5"}
        # Retried at once, so every attempt fails within the timeout.
        always_unavailable = k8s_client.ApiException(status=503)
        always_unavailable.headers = {"Retry-After": "0"}
        cases = {
            "create fails": (k8s_client.ApiException(status=403), wait, TerminalMemberError, MemberState.FAILED, False),
            "rejected after retry": (
                [always_unavailable, k8s_client.ApiException(status=403)],
                wait,
                TerminalMemberError,
                MemberState.FAILED,
                True,
            ),
            "last attempt unavailable": (always_unavailable, wait, TerminalMemberError, MemberState.FAILED, True),
            "retry slot past the timeout": (unavailable, wait, BatchTimeoutError, None, True),
            "terminal": (None, push_terminal, TerminalMemberError, MemberState.FAILED, True),
            "deleted by someone else": (None, delete, TerminalMemberError, MemberState.LOST, False),
            "timeout": (None, wait, BatchTimeoutError, None, True),
            # A watch error leaves the Lease held so acquire() still deletes the pending claim.
            "watch err set": (None, fail_watch, BatchError, None, True),
            # Losing the Lease fences claim deletes so release() cleans up the claim later.
            "lease err set": (None, lose_lease, BatchError, None, False),
            "released while pending": (None, release, BatchError, None, True),
            "interrupted": (KeyboardInterrupt(), wait, KeyboardInterrupt, None, True),
        }
        for name, (create_error, act, error, state, deleted) in cases.items():
            with self.subTest(name):
                # A fresh cluster, so the previous batch's watch can't take this one's events.
                self.cluster = _Cluster()
                self.client.k8s_helper = helper = self.cluster.helper
                batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
                _wait_until(lambda: len(batch.members()) == 1)
                self.cluster.ready_on_create = False
                # Past the fill deadline, like an acquire that replaces a member later, which must
                # still wait for changes rather than spin.
                batch._fill_deadline = time.monotonic()
                waits = []
                wrapped = False

                def hook(claim_name):
                    nonlocal wrapped
                    if claim_name == "b1-1" and not wrapped:
                        wrapped = True
                        waiter = batch._acquire_waiters["b1-1"]
                        wait_for_event = waiter.wait
                        waiter.wait = lambda timeout=None: waits.append(timeout) or wait_for_event(timeout)
                    if create_error is not None and claim_name == "b1-1":
                        raise create_error.pop(0) if isinstance(create_error, list) else create_error

                self.cluster.create_hook = hook
                with ThreadPoolExecutor(1) as pool:
                    future = pool.submit(batch.acquire, "pool-a", timeout=0.5)
                    _wait_until(lambda: helper.create_sandbox_claim.call_count >= 2)
                    act(batch)
                    raised = future.exception(timeout=5)
                self.assertIsInstance(raised, error)
                self.assertLess(len(waits), 20)
                if state is not None:
                    self.assertEqual(raised.member.state, state)
                if create_error is not None and state is not None:
                    self.assertEqual(raised.member.reason, "CreateFailed")
                if name in ("watch err set", "lease err set"):
                    expected_cause = failure if name == "watch err set" else lease_failure
                    # err() keeps the original error, and a later acquire fails before checking a
                    # new pool.
                    self.assertIs(batch.err(), expected_cause)
                    self.assertIs(raised.__cause__, expected_cause)
                    helper.get_sandbox_warmpool.reset_mock()
                    with self.assertRaises(BatchError) as later:
                        batch.acquire("pool-unseen")
                    self.assertIs(later.exception.__cause__, expected_cause)
                    helper.get_sandbox_warmpool.assert_not_called()
                if name == "released while pending":
                    # It fails at once rather than waiting out its timeout.
                    self.assertNotIsInstance(raised, BatchTimeoutError)
                self.assertEqual([m.claim_name for m in batch.members()], ["b1-0"])
                self.assertEqual(
                    [c.args[0] for c in helper.delete_sandbox_claim.call_args_list], ["b1-1"] if deleted else []
                )
                batch._stop_background_threads()

        # Stopping or interrupting an acquire during pacing sends neither create nor delete.
        self.cluster = _Cluster()
        self.client.k8s_helper = helper = self.cluster.helper
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        self.addCleanup(batch._stop_background_threads)
        _wait_until(lambda: len(batch.members()) == 1)
        batch._next_create_at = time.monotonic() + 10
        with patch.object(batch._stop_creating, "wait_or", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                batch.acquire("pool-a", timeout=20)
        batch._next_create_at = time.monotonic() + 10
        with ThreadPoolExecutor(1) as pool:
            future = pool.submit(batch.acquire, "pool-a", timeout=20)
            _wait_until(lambda: batch._state.next_ordinal == 3)
            batch.detach()
            self.assertIsInstance(future.exception(timeout=2), BatchError)
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0"])
        self.assertEqual(helper.create_sandbox_claim.call_count, 1)
        helper.delete_sandbox_claim.assert_not_called()
        self.assertEqual(batch._state._pending_deletes, {})
        helper.get_sandbox_warmpool.reset_mock()
        with self.assertRaises(BatchError):
            batch.acquire("pool-unseen")
        helper.get_sandbox_warmpool.assert_not_called()

    def test_fill_and_acquired_claims_are_owned_by_the_lease_the_handle_holds(self):
        claimed = self.cluster.helper
        claimed.create_batch_lease.return_value = _lease(uid="uid-claimed")
        batch = self.claim()
        _wait_until(lambda: len(batch.members()) == 2)
        batch.acquire("pool-a")
        # Stopped so its watch doesn't read the re-attached handle's events.
        batch._stop_background_threads()
        self.cluster = _Cluster()
        self.client.k8s_helper = helper = self.cluster.helper
        helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0", annotations=_FILL)], "5")
        helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC), uid="uid-attached")
        helper.replace_batch_lease.return_value = _lease(renew_time=datetime.now(UTC), uid="uid-attached")
        batch = self.client.get_batch("b1")
        self.addCleanup(batch._stop_background_threads)
        batch.acquire("pool-a")

        def owned_by(uid):
            return [{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "name": "batch-b1", "uid": uid}]

        self.assertCountEqual(
            [
                (c.args[0], c.kwargs["owner_references"])
                for c in claimed.create_sandbox_claim.call_args_list + helper.create_sandbox_claim.call_args_list
            ],
            [
                ("b1-0", owned_by("uid-claimed")),
                ("b1-1", owned_by("uid-claimed")),
                ("b1-2", owned_by("uid-claimed")),
                ("b1-1", owned_by("uid-attached")),
            ],
        )

    def test_reattached_handle_reads_back_ordinals_labels_and_settings(self):
        helper = self.cluster.helper
        helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0", annotations=_FILL)], "5")
        lease = _lease(renew_time=datetime.now(UTC), annotations={
            BATCH_NEXT_ORDINAL_ANNOTATION: "7",
            BATCH_QUORUM_TIMEOUT_ANNOTATION: "90",
            BATCH_WORK_BUDGET_ANNOTATION: "120",
            BATCH_CREATE_RPS_ANNOTATION: "5",
        })
        lease.metadata.labels = {"team": "rl", BATCH_ID_LABEL: "b1", "agents.x-k8s.io/created-by": "python-client"}
        helper.read_batch_lease.return_value = lease
        helper.replace_batch_lease.return_value = lease
        batch = self.client.get_batch("b1")
        self.addCleanup(batch._stop_background_threads)
        _SteppingDatetime.calls = 0
        with patch("k8s_agent_sandbox.utils.datetime", _SteppingDatetime):
            member = batch.acquire("pool-a")
        self.assertEqual(member.claim_name, "b1-7")
        create = helper.create_sandbox_claim.call_args
        self.assertEqual(create.kwargs["lifecycle"]["shutdownTime"], "2026-01-01T01:13:30Z")
        self.assertEqual(
            create.kwargs["labels"],
            {
                "team": "rl",
                BATCH_ID_LABEL: "b1",
                BATCH_LEASE_UID_LABEL: "lease-uid",
                BATCH_GROUP_LABEL: batch_utils.group_label_value("pool-a"),
            },
        )
        # Paced at the batch's create_rps of 5, so concurrent creates start 0.2s apart.
        with ThreadPoolExecutor(2) as pool:
            list(pool.map(lambda _: batch.acquire("pool-a"), range(2)))
        created_at = dict(self.cluster.created)
        self.assertGreater(abs(created_at["b1-9"] - created_at["b1-8"]), 0.15)
        # A create that pacing can't start before the timeout fails without a claim or an ordinal.
        batch._next_create_at = time.monotonic() + 60
        with self.assertRaises(BatchTimeoutError):
            batch.acquire("pool-a", timeout=1)
        self.assertEqual(helper.create_sandbox_claim.call_count, 3)
        self.assertEqual(batch._state.next_ordinal, 10)
        # Nor is a create whose slot comes after err() is set.
        batch._next_create_at = time.monotonic() + 0.3
        with ThreadPoolExecutor(1) as pool:
            future = pool.submit(batch.acquire, "pool-a", timeout=5)
            _wait_until(lambda: batch._state.next_ordinal == 11)
            with batch._changed:
                batch._state.note_error(RuntimeError("watch failed"))
                batch._notify_all()
            self.assertIsInstance(future.exception(timeout=5), BatchError)
        self.assertEqual(helper.create_sandbox_claim.call_count, 3)
        self.assertEqual(helper.delete_sandbox_claim.call_count, 0)

    def test_watch_events_for_other_claims_do_not_wake_waiting_acquire(self):
        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        _wait_until(lambda: len(batch.members()) == 1)
        self.cluster.ready_on_create = False
        wakes = []
        entered_wait = threading.Event()

        def hook(claim_name):
            if claim_name == "b1-1":
                waiter = batch._acquire_waiters["b1-1"]
                set_event = waiter.set
                wait_for_event = waiter.wait
                waiter.set = lambda: wakes.append("set") or set_event()
                waiter.wait = lambda timeout=None: entered_wait.set() or wait_for_event(timeout)

        self.cluster.create_hook = hook
        with ThreadPoolExecutor(1) as pool:
            future = pool.submit(batch.acquire, "pool-a", timeout=5)
            self.assertTrue(entered_wait.wait(timeout=2))
            # Pending events for b1-1 itself and events for other claims must not wake b1-1's waiter.
            self.cluster.push("b1-1", ready=False, annotations=None)
            self.cluster.push("b1-99", ready=False, annotations=None)
            self.cluster.push("b1-99", ready=True, annotations=None)
            self.cluster.watch_queue.put({"type": "DELETED", "object": _claim("b1-99")})
            _wait_until(lambda: any(m.claim_name == "b1-99" and m.state == MemberState.LOST for m in batch.members()))
            self.assertEqual(wakes, [])
            self.cluster.push("b1-1", ready=True, annotations=None)
            member = future.result(timeout=2)
        self.assertEqual(member.claim_name, "b1-1")
        self.assertEqual(wakes, ["set"])
        self.assertEqual(batch._acquire_waiters, {})

    def test_pod_annotations_are_sent_on_create_and_retries_and_omitted_when_empty(self):
        with patch("k8s_agent_sandbox.k8s_helper.config"), \
             patch("k8s_agent_sandbox.k8s_helper.client.ApiClient"):
            real_helper = K8sHelper()
        bodies: list[dict] = []
        unavailable = k8s_client.ApiException(status=503)
        unavailable.headers = {"Retry-After": "0"}
        retried = False

        def create_custom_object(**kwargs):
            nonlocal retried
            body = kwargs["body"]
            bodies.append(body)
            name = body["metadata"]["name"]
            if name == "b1-1" and not retried:
                retried = True
                raise unavailable
            self.cluster._create(
                name,
                body["spec"]["warmPoolRef"]["name"],
                kwargs["namespace"],
                annotations=body["metadata"]["annotations"],
            )
            return body

        real_helper._batch_custom_objects_api = MagicMock(
            create_namespaced_custom_object=MagicMock(side_effect=create_custom_object)
        )
        self.cluster.helper.create_sandbox_claim.side_effect = real_helper.create_sandbox_claim

        batch = self.claim([BatchGroup(warmpool="pool-a", size=1)])
        _wait_until(lambda: len(batch.members()) == 1)
        with self.assertRaises(ValueError):
            batch.acquire("pool-unseen", pod_annotations={"bad": None})  # type: ignore[dict-item]
        self.assertEqual([c.args[0] for c in self.cluster.helper.get_sandbox_warmpool.call_args_list], ["pool-a"])

        pin = {"podsnapshot.gke.io/ps-name": "snap-1"}
        member = batch.acquire("pool-a", pod_annotations=pin)
        batch.acquire("pool-a")
        batch.acquire("pool-a", pod_annotations={})

        self.assertEqual(member.claim_name, "b1-1")
        self.assertEqual([b["metadata"]["name"] for b in bodies], ["b1-0", "b1-1", "b1-1", "b1-2", "b1-3"])
        for body in bodies[1:3]:
            self.assertEqual(body["spec"]["additionalPodMetadata"], {"annotations": pin})
            self.assertNotIn("podsnapshot.gke.io/ps-name", body["metadata"]["annotations"])
        for body in (bodies[0], bodies[3], bodies[4]):
            self.assertNotIn("additionalPodMetadata", body["spec"])

    def test_pool_check_bounds_reads_by_deadline_and_hands_off_on_timeout(self):
        stop_server = threading.Event()
        pool_f_release = threading.Event()
        pool_e_release = threading.Event()
        self.addCleanup(stop_server.set)
        self.addCleanup(pool_f_release.set)
        self.addCleanup(pool_e_release.set)
        pool_d_started = threading.Event()
        pool_f_started = threading.Event()
        pool_e_started = threading.Event()
        follower_waiting = threading.Event()

        def http_json(payload: dict) -> bytes:
            raw = json.dumps(payload).encode()
            return (
                b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
                + f"Content-Length: {len(raw)}\r\nConnection: close\r\n\r\n".encode()
                + raw
            )

        def send_pool(conn: socket.socket, template: str) -> None:
            try:
                conn.sendall(
                    http_json(
                        {
                            "spec": {"sandboxTemplateRef": {"name": template}},
                            "status": {"readyReplicas": 1},
                        }
                    )
                )
            except OSError:
                pass

        def handle_conn(conn: socket.socket) -> None:
            with conn:
                try:
                    req = conn.recv(4096).decode("latin1")
                except OSError:
                    return
                first_line = req.splitlines()[0] if req else ""
                path = first_line.split(" ")[1] if " " in first_line else ""
                name = path.rsplit("/", 1)[-1]
                if "/sandboxwarmpools/" in path:
                    if name == "pool-a":
                        send_pool(conn, "tmpl-a")
                        return
                    if name == "pool-c":
                        stop_server.wait(0.08)
                        send_pool(conn, "tmpl-c")
                        return
                    if name == "pool-d":
                        pool_d_started.set()
                        follower_waiting.wait(2.0)
                        stop_server.wait(0.18)
                        send_pool(conn, "tmpl-d")
                        return
                    if name == "pool-f":
                        pool_f_started.set()
                        pool_f_release.wait(1.2)
                        send_pool(conn, "tmpl-a")
                        return
                    if name == "pool-e":
                        pool_e_started.set()
                        pool_e_release.wait(1.2)
                        send_pool(conn, "tmpl-a")
                        return
                    if name == "pool-g":
                        stop_server.wait(0.25)
                        send_pool(conn, "tmpl-g")
                        return
                    stop_server.wait(1.2)
                    return
                if name in ("tmpl-a", "tmpl-d"):
                    conn.sendall(http_json({"metadata": {"name": name}}))
                    return
                stop_server.wait(1.2)

        sock = socket.socket()
        self.addCleanup(sock.close)
        sock.bind(("127.0.0.1", 0))
        sock.listen(16)
        host = f"http://127.0.0.1:{sock.getsockname()[1]}"

        def serve() -> None:
            while not stop_server.is_set():
                try:
                    conn, _ = sock.accept()
                except OSError:
                    return
                threading.Thread(target=handle_conn, args=(conn,), daemon=True).start()

        threading.Thread(target=serve, daemon=True).start()

        configuration = k8s_client.Configuration(host=host)
        injected = k8s_client.ApiClient(configuration=configuration)
        self.addCleanup(injected.close)
        with patch("k8s_agent_sandbox.k8s_helper.config"):
            real_helper = K8sHelper(api_client=injected)
        self.addCleanup(real_helper._batch_custom_objects_api.api_client.close)
        helper = self.cluster.helper
        helper.get_sandbox_warmpool.side_effect = real_helper.get_sandbox_warmpool
        helper.get_sandbox_template.side_effect = real_helper.get_sandbox_template

        batch = self.claim(
            [BatchGroup(warmpool="pool-a", size=1)], max_in_flight=1, lease_duration=60
        )
        _wait_until(lambda: len(batch.members()) == 1)

        # Stalled warm-pool GET respects timeout and releases the sole max_in_flight slot.
        start = time.monotonic()
        with self.assertRaisesRegex(
            BatchTimeoutError, "warm pool 'pool-b' wasn't checked within 0.12s"
        ):
            batch.acquire("pool-b", timeout=0.12)
        self.assertLess(time.monotonic() - start, 0.45)
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0"])
        self.assertNotIn("pool-b", batch._checked_pools)
        self.assertEqual(batch._prechecks, {})
        self.assertTrue(batch._in_flight.acquire(blocking=False))
        batch._in_flight.release()

        # Template GET shares the remaining deadline after the warm-pool GET consumes part of it.
        start = time.monotonic()
        with self.assertRaisesRegex(
            BatchTimeoutError, "warm pool 'pool-c' wasn't checked within 0.16s"
        ):
            batch.acquire("pool-c", timeout=0.16)
        self.assertLess(time.monotonic() - start, 0.45)
        self.assertNotIn("pool-c", batch._checked_pools)

        class _SignalingFuture(Future):
            def result(self, timeout=None):
                follower_waiting.set()
                return super().result(timeout)

        # A short-deadline leader cancels check on BatchTimeoutError so a follower takes over.
        with (
            ThreadPoolExecutor(max_workers=2) as executor,
            patch.object(sandbox_batch, "Future", _SignalingFuture),
        ):
            leader = executor.submit(batch.acquire, "pool-d", timeout=0.12)
            self.assertTrue(pool_d_started.wait(2.0))
            follower = executor.submit(batch.acquire, "pool-d", timeout=5.0)
            self.assertTrue(follower_waiting.wait(2.0))
            with self.assertRaisesRegex(
                BatchTimeoutError, "warm pool 'pool-d' wasn't checked within 0.12s"
            ):
                leader.result(timeout=2.0)
            member = follower.result(timeout=2.0)
        self.assertEqual((member.claim_name, member.warmpool), ("b1-1", "pool-d"))

        # A follower whose short timeout expires after release_group() raises BatchError, not
        # BatchTimeoutError, even while the leader's HTTP read is still stalled.
        follower_waiting.clear()
        with (
            ThreadPoolExecutor(max_workers=2) as executor,
            patch.object(sandbox_batch, "Future", _SignalingFuture),
        ):
            leader = executor.submit(batch.acquire, "pool-f", timeout=5.0)
            self.assertTrue(pool_f_started.wait(2.0))
            follower = executor.submit(batch.acquire, "pool-f", timeout=0.16)
            self.assertTrue(follower_waiting.wait(2.0))
            batch.release_group("pool-f")
            with self.assertRaisesRegex(
                BatchError, "warm pool 'pool-f' was released while acquiring"
            ) as released_ctx:
                follower.result(timeout=2.0)
            self.assertNotIsInstance(released_ctx.exception, BatchTimeoutError)
            pool_f_release.set()
            with self.assertRaisesRegex(
                BatchError, "warm pool 'pool-f' was released while acquiring"
            ):
                leader.result(timeout=2.0)

        # A follower with a long timeout wakes at _lease_expires_at with BatchError while the
        # leader's earlier socket read is still stalled.
        follower_waiting.clear()
        with (
            ThreadPoolExecutor(max_workers=2) as executor,
            patch.object(sandbox_batch, "Future", _SignalingFuture),
        ):
            leader = executor.submit(batch.acquire, "pool-e", timeout=5.0)
            self.assertTrue(pool_e_started.wait(2.0))
            batch._lease_expires_at = time.monotonic() + 0.10
            start = time.monotonic()
            follower = executor.submit(batch.acquire, "pool-e", timeout=5.0)
            self.assertTrue(follower_waiting.wait(2.0))
            with self.assertRaises(BatchError) as follower_expired:
                follower.result(timeout=2.0)
            self.assertLess(time.monotonic() - start, 0.45)
            self.assertNotIsInstance(follower_expired.exception, BatchTimeoutError)
            self.assertIsInstance(follower_expired.exception.__cause__, BatchLeaseExpiredError)
            pool_e_release.set()
            with self.assertRaises(BatchError) as leader_expired:
                leader.result(timeout=2.0)
            self.assertNotIsInstance(leader_expired.exception, BatchTimeoutError)
            self.assertIsInstance(leader_expired.exception.__cause__, BatchLeaseExpiredError)
        batch._stop_background_threads()

        # The template GET recomputes remaining Lease time after the pool GET spends most of it.
        b2 = self.claim(
            [BatchGroup(warmpool="pool-a", size=0)],
            batch_id="b2",
            max_in_flight=1,
            lease_duration=60,
        )
        b2._lease_expires_at = time.monotonic() + 0.32
        start = time.monotonic()
        with self.assertRaises(BatchError) as tmpl_expired:
            b2.acquire("pool-g", timeout=5.0)
        self.assertLess(time.monotonic() - start, 1.0)
        self.assertNotIsInstance(tmpl_expired.exception, BatchTimeoutError)
        self.assertIsInstance(tmpl_expired.exception.__cause__, BatchLeaseExpiredError)
        b2._stop_background_threads()

        # A leader blocked on a busy max_in_flight slot re-sleeps across an early wakeup and
        # wakes at _lease_expires_at with BatchError, not BatchTimeoutError.
        b3 = self.claim(
            [BatchGroup(warmpool="pool-a", size=0)],
            batch_id="b3",
            max_in_flight=1,
            lease_duration=60,
        )
        orig_b3_acquire = b3._in_flight.acquire
        slot_calls = 0

        def wake_early_once(*args, **kwargs):
            nonlocal slot_calls
            slot_calls += 1
            if slot_calls == 1:
                return orig_b3_acquire(timeout=0.01)
            return orig_b3_acquire(*args, **kwargs)

        self.assertTrue(orig_b3_acquire(blocking=False))
        try:
            b3._lease_expires_at = time.monotonic() + 0.12
            start = time.monotonic()
            with (
                patch.object(b3._in_flight, "acquire", side_effect=wake_early_once),
                self.assertRaises(BatchError) as slot_expired,
            ):
                b3.acquire("pool-b", timeout=5.0)
            self.assertGreaterEqual(slot_calls, 2)
            self.assertLess(time.monotonic() - start, 0.45)
            self.assertNotIsInstance(slot_expired.exception, BatchTimeoutError)
            self.assertIsInstance(slot_expired.exception.__cause__, BatchLeaseExpiredError)
        finally:
            b3._in_flight.release()
            b3._stop_background_threads()


@patch.object(SandboxBatch, "_start", lambda self: None)
class TestGetBatchQuorumTimeout(BaseBatchClientTest):

    def test_quorum_timeout_annotation_is_validated_and_counts_from_attach(self):
        self.mock_k8s_helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0")], "5")
        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            renew_time=datetime.now(UTC), annotations={BATCH_QUORUM_TIMEOUT_ANNOTATION: "abc"}
        )
        with self.assertRaises(BatchError):
            self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            renew_time=datetime.now(UTC), annotations={BATCH_QUORUM_TIMEOUT_ANNOTATION: "90"}
        )
        batch = self.client.get_batch("b1")
        self.assertAlmostEqual(batch._fill_deadline - time.monotonic(), 90, delta=5)


class TestClientBatchCleanup(BaseClaimBatchTest):

    @patch.object(SandboxBatch, "_start", lambda self: None)
    def test_delete_all_releases_every_tracked_batch_and_exit_cleanup_detaches_attached_ones(self):
        for cleanup, exiting in (("delete_all", False), ("_delete_automatic_sandboxes", True)):
            with self.subTest(cleanup):
                self.client._active_batches.clear()
                claimed = self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b1")
                self.cluster.helper.list_sandbox_claim_objects.return_value = ([_claim("b2-0")], "5")
                self.cluster.helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC))
                attached = self.client.get_batch("b2")
                self.cluster.helper.list_sandbox_claim_objects.return_value = ([], "7")
                self.assertEqual(set(self.client._active_batches.values()), {claimed, attached})

                self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=attached._holder_identity)
                with patch.object(SandboxBatch, "release", autospec=True) as release:
                    getattr(self.client, cleanup)()
                released = {claimed} if exiting else {claimed, attached}
                self.assertEqual({c.args[0] for c in release.call_args_list}, released)
                self.assertEqual(attached._lease_released, exiting)
                self.assertEqual(attached in self.client._active_batches.values(), not exiting)

                self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=claimed._holder_identity)
                claimed.release()
                self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=attached._holder_identity)
                attached.detach()
                self.assertEqual(self.client._active_batches, {})

    def test_cleanup_closes_a_reattached_batchs_old_connection_without_stopping_the_old_handle(self):
        for lease_write_error in (None, RuntimeError("timed out after the write applied")):
            with self.subTest(lease_write_error=lease_write_error):
                old = _make_handle(self.client)
                cached = MagicMock()
                cached.close_connection.side_effect = [RuntimeError("close failed"), None]
                old._connected["b1-0"] = (cached, None)
                self.client._active_batches[("default", "b1")] = old
                self.client.k8s_helper.read_batch_lease.return_value = _lease(holder_identity=old._holder_identity)
                self.client.k8s_helper.replace_batch_lease.side_effect = lease_write_error

                with self.assertLogs(level="WARNING"), contextlib.suppress(RuntimeError):
                    old.detach()
                new = _make_handle(self.client)
                new._stop_background_threads = MagicMock()
                with patch.object(SandboxBatch, "_attach", return_value=new):
                    self.client.get_batch("b1")
                old._stop_background_threads = MagicMock()

                self.client.delete_all()
                self.assertEqual(cached.close_connection.call_count, 2)
                old._stop_background_threads.assert_not_called()
                new._stop_background_threads.assert_called_once()
                self.assertEqual(self.client._closing_batches, set())

    def test_a_close_that_fails_after_an_overlapping_cleanup_and_reattach_is_retried(self):
        handle = _make_handle(self.client)
        newer = _make_handle(self.client)
        cached = MagicMock()

        def close_connection():
            if cached.close_connection.call_count == 1:
                # An overlapping cleanup finds no cached connection and unregisters the handle, then
                # get_batch registers a newer handle for the same batch.
                self.client.delete_all()
                with patch.object(SandboxBatch, "_attach", return_value=newer):
                    self.client.get_batch("b1")
                raise RuntimeError("close failed")

        cached.close_connection.side_effect = close_connection
        handle._connected["b1-0"] = (cached, None)
        self.client._active_batches[("default", "b1")] = handle
        # detach() fails after its close, so only the put-back can keep the connection tracked.
        self.client.k8s_helper.read_batch_lease.side_effect = RuntimeError("api down")

        with self.assertRaises(RuntimeError):
            handle.detach()
        self.assertEqual(self.client._closing_batches, {handle})

        self.client.delete_all()
        self.assertEqual(cached.close_connection.call_count, 2)
        self.assertEqual(self.client._closing_batches, set())

    def test_cleanup_finishes_an_unfinished_detach_with_its_grace(self):
        for grace, written_duration in ((42, 42), (None, 90)):
            with self.subTest(grace=grace):
                handle = _make_handle(self.client, lease_duration=90)
                self.client._active_batches[("default", "b1")] = handle
                helper = self.client.k8s_helper
                helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                helper.replace_batch_lease.reset_mock()
                helper.replace_batch_lease.side_effect = [RuntimeError("api down"), None]
                with self.assertRaises(RuntimeError):
                    handle.detach(grace=grace)

                self.client.delete_all()

                written = helper.replace_batch_lease.call_args.args[2]
                self.assertEqual(helper.replace_batch_lease.call_count, 2)
                self.assertIsNone(written.spec.holder_identity)
                self.assertEqual(written.spec.lease_duration_seconds, written_duration)
                self.assertEqual(self.client._active_batches, {})
                helper.delete_sandbox_claims_by_label.assert_not_called()
                helper.delete_batch_lease.assert_not_called()

    def test_cleanup_releases_a_handle_whose_release_failed_after_a_failed_detach(self):
        handle = _make_handle(self.client)
        handle._attached = True
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
        helper.replace_batch_lease.side_effect = RuntimeError("api down")
        with self.assertRaises(RuntimeError):
            handle.detach(grace=42)
        helper.replace_batch_lease.side_effect = None
        helper.delete_sandbox_claims_by_label.side_effect = [k8s_client.ApiException(status=403), None]
        with self.assertRaises(k8s_client.ApiException):
            handle.release()

        # Exit cleanup still retries a release that started on an attached handle.
        self.client._delete_automatic_sandboxes()

        # After the failed detach write, every Lease write is a release taking the Lease, not a detach.
        written = [c.args[2] for c in helper.replace_batch_lease.call_args_list[1:]]
        self.assertEqual([lease.spec.holder_identity for lease in written], [handle._holder_identity] * 2)
        helper.delete_batch_lease.assert_called_once()
        self.assertEqual(self.client._active_batches, {})

    def test_a_failed_detach_in_exit_cleanup_is_logged_and_releases_nothing(self):
        handle = _make_handle(self.client)
        handle._attached = True
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
        helper.replace_batch_lease.side_effect = RuntimeError("api down")

        with self.assertLogs(level="ERROR"):
            self.client._delete_automatic_sandboxes()

        # The detach makes the only Lease write. The handle stays registered so the next cleanup finishes it.
        self.assertEqual([c.args[2].spec.holder_identity for c in helper.replace_batch_lease.call_args_list], [None])
        self.assertEqual(self.client._active_batches, {("default", "b1"): handle})
        helper.delete_sandbox_claims_by_label.assert_not_called()

    def test_exit_cleanup_during_detach_detaches_an_attached_handle(self):
        handle = _make_handle(self.client)
        handle._attached = True
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
        lock = handle._lock = _SignalingLock(handle._lock)

        class CleanupBetweenDetachWrites(SandboxBatch):
            def __setattr__(self, name, value):
                super().__setattr__(name, value)
                # Another thread's exit cleanup runs once detach() has set _detached but not yet _detach_grace.
                if name == "_detached" and value and not lock.threads:
                    lock.start_racing(self._client._delete_automatic_sandboxes)

        handle.__class__ = CleanupBetweenDetachWrites
        handle.detach()
        lock.threads.pop().join()

        helper.delete_sandbox_claims_by_label.assert_not_called()
        self.assertEqual(self.client._active_batches, {})

    def test_a_failed_detach_retry_is_logged_and_keeps_the_handle_registered(self):
        handle = _make_handle(self.client)
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
        helper.replace_batch_lease.side_effect = RuntimeError("api down")
        with self.assertRaises(RuntimeError):
            handle.detach(grace=42)

        with self.assertLogs(level="ERROR"):
            self.client.delete_all()

        self.assertEqual(helper.replace_batch_lease.call_count, 2)
        self.assertEqual(self.client._active_batches, {("default", "b1"): handle})
        helper.delete_sandbox_claims_by_label.assert_not_called()

    def test_unregistering_an_old_handle_keeps_a_newer_one_registered(self):
        old_handle = MagicMock(namespace="default", batch_id="b1")
        new_handle = MagicMock(namespace="default", batch_id="b1")
        self.client._active_batches[("default", "b1")] = new_handle

        self.client._unregister_batch(old_handle)

        self.assertEqual(self.client._active_batches, {("default", "b1"): new_handle})

    def test_a_handle_registered_during_unregister_stays_registered(self):
        client = self.client
        old_handle = MagicMock(namespace="default", batch_id="b1")
        new_handle = MagicMock(namespace="default", batch_id="b1")
        lock = client._batches_lock = _SignalingLock(threading.Lock())

        class RacingDict(dict):
            raced = False

            def get(self, *args):
                value = super().get(*args)
                # Only the first lookup races, since get_batch's own lookups would start more threads.
                if not self.raced:
                    self.raced = True
                    # Without the registry lock, get_batch registers new_handle before this call returns.
                    lock.start_racing(client.get_batch, "b1")
                return value

        client._active_batches = RacingDict({("default", "b1"): old_handle})
        with patch.object(SandboxBatch, "_attach", return_value=new_handle):
            client._unregister_batch(old_handle)
            lock.threads.pop().join()
        self.assertEqual(client._active_batches, {("default", "b1"): new_handle})

    def test_a_handle_registered_during_cleanups_snapshot_is_not_released(self):
        client = self.client
        old_handle = _make_handle(client)
        new_handle = _make_handle(client, batch_id="b2")
        lock = client._batches_lock = _SignalingLock(threading.Lock())

        class RacingDict(dict):
            def items(self):
                # Without the registry lock, get_batch registers new_handle before the snapshot is taken.
                lock.start_racing(client.get_batch, "b2")
                return super().items()

        client._active_batches = RacingDict({("default", "b1"): old_handle})
        with patch.object(SandboxBatch, "_attach", return_value=new_handle), \
             patch.object(SandboxBatch, "release", autospec=True) as release:
            client.delete_all()
            lock.threads.pop().join()
        self.assertEqual([c.args[0] for c in release.call_args_list], [old_handle])
        self.assertIs(client._active_batches[("default", "b2")], new_handle)

    def test_cleanup_unregisters_a_handle_whose_batch_another_handle_holds(self):
        handle = _make_handle(self.client)
        cached = MagicMock()
        cached.close_connection.side_effect = [RuntimeError("close failed"), None]
        handle._connected["b1-0"] = (cached, None)
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.return_value = _lease(holder_identity="other-handle")

        with self.assertLogs(level="WARNING") as logs:
            self.client.delete_all()
        self.assertEqual(len([r for r in logs.records if "Not releasing batch" in r.getMessage()]), 1)
        self.assertEqual(self.client._active_batches, {})
        self.assertEqual(self.client._closing_batches, {handle})

        self.client.delete_all()
        # The next cleanup only closes the leftover connection and never retries the release.
        helper.read_batch_lease.assert_called_once()
        self.assertEqual(cached.close_connection.call_count, 2)
        self.assertEqual(self.client._closing_batches, set())
        helper.delete_sandbox_claims_by_label.assert_not_called()


class TestCreateDrainPacingAndAcquireWakeup(BaseBatchClientTest):

    def test_release_drains_in_flight_acquire_create(self):
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
        entered_post = threading.Event()
        allow_post_return = threading.Event()
        live_lock = threading.Lock()
        live_claims: set[str] = set()

        def slow_create(claim_name, warmpool, *_args, **_kwargs):
            entered_post.set()
            allow_post_return.wait(timeout=5.0)
            with live_lock:
                live_claims.add(claim_name)
            return _claim(claim_name, warmpool=warmpool)

        def delete_by_label(*_args, **_kwargs):
            with live_lock:
                live_claims.clear()

        self.mock_k8s_helper.create_sandbox_claim.side_effect = slow_create
        self.mock_k8s_helper.delete_sandbox_claims_by_label.side_effect = delete_by_label
        self.mock_k8s_helper.read_batch_lease.return_value = handle._lease

        acq_errors: list[BaseException] = []
        acq_thread = threading.Thread(
            target=lambda: self._capture_exc(
                lambda: handle.acquire("pool-a", timeout=5.0), acq_errors
            ),
            daemon=True,
        )
        rel_errors: list[BaseException] = []
        rel_thread = threading.Thread(
            target=lambda: self._capture_exc(handle.release, rel_errors), daemon=True
        )
        acq_thread.start()
        self.assertTrue(entered_post.wait(timeout=3.0))
        rel_thread.start()
        _wait_until(lambda: handle._detached)
        self.mock_k8s_helper.delete_sandbox_claims_by_label.assert_not_called()
        allow_post_return.set()
        acq_thread.join(timeout=3.0)
        rel_thread.join(timeout=3.0)
        self.assertFalse(acq_thread.is_alive())
        self.assertFalse(rel_thread.is_alive())
        handle._wait_active_requests()
        self.assertEqual(len(acq_errors), 1)
        self.assertEqual(rel_errors, [])
        self.assertEqual(live_claims, set())

    @staticmethod
    def _capture_exc(fn, out):
        try:
            fn()
        except BaseException as exc:
            out.append(exc)

    def test_release_group_cancels_popped_pacing_slot_creates(self):
        groups = [
            BatchGroup(warmpool="pool-fast", size=1),
            BatchGroup(warmpool="pool-slow", size=2),
        ]
        handle = _make_handle(self.client, groups=groups)
        plan = batch_utils.fill_create_plan("b1", groups)
        handle._create_plan = collections.OrderedDict.fromkeys(plan)
        for entry in plan:
            handle._plan_by_pool.setdefault(entry[1].warmpool, {})[entry] = None
        handle._claim_labels = {g.warmpool: {} for g in groups}
        handle._claim_annotations = {g.warmpool: {} for g in groups}
        handle._warm_credit.record("pool-fast", 1)
        handle._warm_credit.record("pool-slow", 0)
        handle._create_interval = 5.0
        handle._next_create_at = time.monotonic() + 5.0

        def create_and_upsert(claim_name, warmpool, *_args, **_kwargs):
            obj = _claim(
                claim_name,
                warmpool=warmpool,
                conditions=[{"type": "Ready", "status": "True"}],
                sandbox={"name": f"sbx-{claim_name}"},
                annotations=_FILL,
            )
            with handle._changed:
                handle._state.upsert_claim(obj)
                handle._changed.notify_all()
            return obj

        self.mock_k8s_helper.create_sandbox_claim.side_effect = create_and_upsert
        workers = [threading.Thread(target=handle._create_worker, daemon=True) for _ in range(3)]
        handle._creators = workers
        for w in workers:
            w.start()
        try:
            ev_iter = handle.events()
            self.assertEqual(next(ev_iter).member.warmpool, "pool-fast")
            _wait_until(lambda: not handle._create_plan)
            self.assertEqual(len(handle._create_plan), 0)
            handle.release_group("pool-slow")
            done = threading.Event()
            rest: list[object] = []
            reader = threading.Thread(
                target=lambda: (rest.extend(ev_iter), done.set()), daemon=True
            )
            reader.start()
            self.assertTrue(done.wait(timeout=2.5))
            reader.join(timeout=2.5)
            self.assertFalse(reader.is_alive())
            self.assertEqual(rest, [])
        finally:
            handle._stop_creating.set()
            for w in workers:
                w.join(timeout=3.0)
                self.assertFalse(w.is_alive())

    def test_acquire_wakes_on_release_group_during_retry_and_clamps_in_flight_to_lease(self):
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
        entered_retry = threading.Event()
        api_429 = k8s_client.ApiException(status=429, reason="Too Many Requests")
        api_429.headers = {"Retry-After": "15"}
        orig_wait_or = handle._stop_creating.wait_or

        def wait_or(timeout, cancel=None):
            if timeout > 0:
                entered_retry.set()
            return orig_wait_or(timeout, cancel)

        handle._stop_creating.wait_or = wait_or  # type: ignore[method-assign]
        self.mock_k8s_helper.create_sandbox_claim.side_effect = api_429
        acq_errors: list[BaseException] = []
        acq_thread = threading.Thread(
            target=lambda: self._capture_exc(
                lambda: handle.acquire("pool-a", timeout=30.0), acq_errors
            ),
            daemon=True,
        )
        acq_thread.start()
        self.assertTrue(entered_retry.wait(timeout=3.0))
        t0 = time.monotonic()
        handle.release_group("pool-a")
        acq_thread.join(timeout=2.5)
        self.assertFalse(acq_thread.is_alive())
        self.assertLess(time.monotonic() - t0, 2.5)
        self.assertIsInstance(acq_errors[0], BatchError)
        self.mock_k8s_helper.create_sandbox_claim.assert_called_once()

        handle2 = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
        handle2._in_flight = threading.BoundedSemaphore(1)
        self.assertTrue(handle2._in_flight.acquire(blocking=False))
        try:
            handle2._lease_expires_at = time.monotonic() + 0.10
            t1 = time.monotonic()
            with self.assertRaises(BatchError) as ctx:
                handle2.acquire("pool-a", timeout=10.0)
            self.assertLess(time.monotonic() - t1, 2.5)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
        finally:
            handle2._in_flight.release()

    def test_acquire_keeps_pacing_and_retrying_when_notify_all_wakes_its_waiter(self):
        # _notify_all() also sets acquire waiters (for example after a healthy 410 re-list), so a
        # wake during pacing or a retry wait must not skip the create.
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
        # One event per create count, so the test can tell the pacing sleep from the retry sleep.
        sleeping = {0: threading.Event(), 1: threading.Event()}
        calls: list[str] = []
        orig_wait_or = handle._stop_creating.wait_or

        def wait_or(timeout, cancel=None):
            if timeout > 0.2 and cancel is not None and len(calls) in sleeping:
                sleeping[len(calls)].set()
            return orig_wait_or(timeout, cancel)

        handle._stop_creating.wait_or = wait_or  # type: ignore[method-assign]
        api_429 = k8s_client.ApiException(status=429, reason="Too Many Requests")
        api_429.headers = {"Retry-After": "1"}
        sent = threading.Event()

        def create(name, *args, **kwargs):
            calls.append(name)
            if len(calls) == 1:
                raise api_429
            sent.set()

        self.mock_k8s_helper.create_sandbox_claim.side_effect = create
        self.mock_k8s_helper.count_live_sandbox_claims.return_value = 0
        with handle._lock:
            handle._next_create_at = time.monotonic() + 1.0
        acq_errors: list[BaseException] = []
        acq_thread = threading.Thread(
            target=lambda: self._capture_exc(lambda: handle.acquire("pool-a", timeout=10.0), acq_errors),
            daemon=True,
        )
        acq_thread.start()
        for creates_so_far in (0, 1):
            self.assertTrue(sleeping[creates_so_far].wait(timeout=3.0))
            self.assertEqual(len(calls), creates_so_far)
            with handle._lock:
                handle._notify_all()
        self.assertTrue(sent.wait(timeout=3.0))
        self.assertEqual(len(calls), 2)
        handle.release_group("pool-a")
        acq_thread.join(timeout=3.0)
        self.assertFalse(acq_thread.is_alive())
        self.assertIsInstance(acq_errors[0], BatchError)


if __name__ == "__main__":
    unittest.main()
