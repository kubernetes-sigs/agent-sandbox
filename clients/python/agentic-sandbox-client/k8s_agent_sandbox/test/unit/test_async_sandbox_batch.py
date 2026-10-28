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

"""Unit tests for the async AsyncSandboxBatch handle and AsyncSandboxClient.get_batch."""

import asyncio
import contextlib
import inspect
import logging
import ssl
import time
import unittest
from datetime import UTC, datetime, timedelta
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

pytest.importorskip("kubernetes_asyncio")

import aiohttp
from kubernetes_asyncio import client as k8s_client

from k8s_agent_sandbox import async_sandbox_batch, batch_state, batch_utils
from k8s_agent_sandbox.async_sandbox_batch import AsyncSandboxBatch
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
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
from k8s_agent_sandbox.models import BatchGroup, MemberState, SandboxDirectConnectionConfig


def _lease(
    holder_identity=None,
    renew_time=None,
    lease_duration_seconds=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
    annotations=None,
    resource_version="10",
    uid=None,
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


async def _agen(events):
    for event in events:
        yield event


async def _agen_then_raise(events, exc):
    for event in events:
        yield event
    raise exc


def _make_handle(
    client,
    batch_id="b1",
    namespace="default",
    groups=None,
    lease_duration=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
    lease_renewed_at=None,
):
    groups = groups if groups is not None else [BatchGroup(warmpool="pool-a", size=1)]
    state = batch_state.BatchState(batch_id, groups)
    return AsyncSandboxBatch(
        client=client,
        batch_id=batch_id,
        namespace=namespace,
        state=state,
        lease_name=f"batch-{batch_id}",
        holder_identity="host_1234_abcd1234",
        lease_duration=lease_duration,
        list_resource_version="5",
        lease=_lease(holder_identity="host_1234_abcd1234"),
        lease_renewed_at=time.monotonic() if lease_renewed_at is None else lease_renewed_at,
    )


class _FakeClock:
    """Stands in for the handle's monotonic clock, so renewal timing is exact."""

    def __init__(self):
        self.now = 0.0
        self._sleeps = 0

    def monotonic(self):
        return self.now

    async def sleep(self, delay):
        """Stands in for ``asyncio.sleep``: advances the clock instead of sleeping."""
        self._sleeps += 1
        if self._sleeps > 100:
            raise AssertionError("the renewal loop never stopped")
        self.now += delay


class BaseAsyncBatchClientTest(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        self.MockAsyncK8sHelper = patcher.start()
        self.addCleanup(patcher.stop)

        config = SandboxDirectConnectionConfig(api_url="http://test-router:8080")
        self.client = AsyncSandboxClient(connection_config=config, cleanup=False)
        self.mock_k8s_helper = self.client.k8s_helper
        self.mock_sandbox_class = MagicMock()
        self.client.sandbox_class = self.mock_sandbox_class


@patch.object(AsyncSandboxBatch, "_start", lambda self: None)
class TestGetBatchLeaseCases(BaseAsyncBatchClientTest):

    async def test_not_found_raises(self):
        self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=None)
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(return_value=([], "0"))
        with self.assertRaises(BatchNotFoundError):
            await self.client.get_batch("b1")

    async def test_stale_held_lease_raises_expired(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity="someone-else", renew_time=now - timedelta(seconds=120))
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        with self.assertRaises(BatchLeaseExpiredError):
            await self.client.get_batch("b1")

    async def test_missing_lease_with_claims_present_raises_expired(self):
        self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=None)
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        with self.assertRaises(BatchLeaseExpiredError):
            await self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    async def test_live_different_holder_raises_in_use(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity="other-holder", renew_time=now)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        with self.assertRaises(BatchInUseError):
            await self.client.get_batch("b1")

    async def test_empty_holder_identity_counts_as_free(self):
        # client-go releases a Lease by writing an empty holderIdentity.
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity="", renew_time=datetime.now(UTC))
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        await self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_called_once()

    async def test_takeover_write_conflict_raises_in_use(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=None, renew_time=now)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        conflict = k8s_client.ApiException(status=409)
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=conflict)
        with self.assertRaises(BatchInUseError) as ctx:
            await self.client.get_batch("b1")
        self.assertIs(ctx.exception.__cause__, conflict)

    async def test_takeover_write_other_error_propagates(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=None, renew_time=now)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(
            side_effect=k8s_client.ApiException(status=500)
        )
        with self.assertRaises(k8s_client.ApiException):
            await self.client.get_batch("b1")


@patch.object(AsyncSandboxBatch, "_start", lambda self: None)
class TestBatchStaleness(BaseAsyncBatchClientTest):

    async def test_spec_duration_boundary_adoptable_then_expired_one_second_later(self):
        now = datetime.now(UTC)
        spec_duration = 30
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()

        with patch("k8s_agent_sandbox.batch_utils.lease_now") as mock_now:
            mock_now.return_value = now
            self.mock_k8s_helper.read_batch_lease = AsyncMock(
                return_value=_lease(
                    holder_identity=None,
                    renew_time=now - timedelta(seconds=spec_duration - CLOCK_SKEW_MARGIN),
                    lease_duration_seconds=spec_duration,
                    annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
                )
            )
            self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
                return_value=([_claim("b1-0")], "5")
            )
            batch = await self.client.get_batch("b1")
            self.assertIsNotNone(batch)

        with patch("k8s_agent_sandbox.batch_utils.lease_now") as mock_now2:
            mock_now2.return_value = now
            self.mock_k8s_helper.read_batch_lease = AsyncMock(
                return_value=_lease(
                    holder_identity=None,
                    renew_time=now - timedelta(seconds=spec_duration - CLOCK_SKEW_MARGIN + 1),
                    lease_duration_seconds=spec_duration,
                    annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
                )
            )
            with self.assertRaises(BatchLeaseExpiredError):
                await self.client.get_batch("b1")

    async def test_missing_spec_duration_is_treated_as_stale(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=None, renew_time=now, lease_duration_seconds=None)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        with self.assertRaises(BatchLeaseExpiredError):
            await self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    async def test_missing_renew_time_is_treated_as_stale(self):
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(
                holder_identity=None,
                renew_time=None,
                lease_duration_seconds=BATCH_DEFAULT_LEASE_DURATION_SECONDS,
            )
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        with self.assertRaises(BatchLeaseExpiredError):
            await self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()


@patch.object(AsyncSandboxBatch, "_start", lambda self: None)
class TestLeaseTakeoverWrites(BaseAsyncBatchClientTest):

    async def test_writes_holder_identity_and_renew_time_near_now(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=None, renew_time=now)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        await self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertIsNotNone(written_lease.spec.holder_identity)
        self.assertLess(
            abs((written_lease.spec.renew_time - datetime.now(UTC)).total_seconds()), 5
        )
        self.assertEqual(written_lease.spec.acquire_time, written_lease.spec.renew_time)
        self.assertEqual(written_lease.spec.lease_transitions, 1)

    async def test_takeover_increments_existing_lease_transitions(self):
        lease = _lease(holder_identity=None, renew_time=datetime.now(UTC))
        lease.spec.lease_transitions = 3
        self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=lease)
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        await self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written_lease.spec.lease_transitions, 4)

    async def test_annotation_duration_overrides_leftover_spec_duration_on_takeover(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(
                holder_identity=None,
                renew_time=now,
                lease_duration_seconds=300,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
            )
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        batch = await self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written_lease.spec.lease_duration_seconds, 90)
        self.assertEqual(batch._lease_duration, 90)

    async def test_no_annotation_takeover_writes_default_duration(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=None, renew_time=now)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        batch = await self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written_lease.spec.lease_duration_seconds, BATCH_DEFAULT_LEASE_DURATION_SECONDS)
        self.assertEqual(batch._lease_duration, BATCH_DEFAULT_LEASE_DURATION_SECONDS)

    async def test_invalid_annotation_raises_batch_error_with_no_write(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(
                holder_identity=None,
                renew_time=now,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: "abc"},
            )
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        with self.assertRaises(BatchError):
            await self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    async def test_conflicting_group_annotations_raise_batch_error_with_no_write(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=None, renew_time=now)
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=(
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
        )
        with self.assertRaises(BatchError):
            await self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

    async def test_takeover_leaves_annotation_unchanged(self):
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(
                holder_identity=None,
                renew_time=now,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: "90"},
            )
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        await self.client.get_batch("b1")

        written_lease = self.mock_k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(
            written_lease.metadata.annotations, {BATCH_LEASE_DURATION_ANNOTATION: "90"}
        )


@patch.object(AsyncSandboxBatch, "_start", lambda self: None)
class TestRequestTimeouts(BaseAsyncBatchClientTest):

    async def test_get_batch_relist_and_detach_requests_carry_request_timeouts(self):
        helper = self.mock_k8s_helper
        helper.read_batch_lease = AsyncMock(return_value=_lease(renew_time=datetime.now(UTC)))
        helper.list_sandbox_claim_objects = AsyncMock(return_value=([_claim("b1-0")], "5"))
        helper.replace_batch_lease = AsyncMock()
        handle = await self.client.get_batch("b1")
        self.assertEqual(helper.read_batch_lease.call_args.kwargs["_request_timeout"], 30)
        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.replace_batch_lease.call_args.kwargs["_request_timeout"], 30)

        helper.list_sandbox_claim_objects.reset_mock()
        helper.watch_sandbox_claims = MagicMock(
            side_effect=[k8s_client.ApiException(status=410), k8s_client.ApiException(status=403)]
        )
        await handle._watch_loop()
        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["_request_timeout"], 90)

        helper.read_batch_lease = AsyncMock(return_value=_lease(holder_identity=handle._holder_identity))
        helper.replace_batch_lease = AsyncMock()
        await handle.detach()
        self.assertEqual(helper.read_batch_lease.call_args.kwargs["_request_timeout"], 30)
        self.assertEqual(helper.replace_batch_lease.call_args.kwargs["_request_timeout"], 30)


class TestWatcher(BaseAsyncBatchClientTest):

    @staticmethod
    def _terminating_error(status=403):
        return k8s_client.ApiException(status=status)

    async def test_resumes_from_list_resource_version(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[self._terminating_error()]
        )
        await handle._watch_loop()
        first_call = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[0]
        self.assertEqual(first_call.args[2], "5")

    async def test_410_relists_and_diffs_vanished_claim_becomes_lost(self):
        handle = _make_handle(self.client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[self._terminating_error(status=410), self._terminating_error(status=403)]
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(return_value=([], "9"))
        await handle._watch_loop()
        self.assertEqual(handle._state.get_member("b1-0").state, MemberState.LOST)

    async def test_410_relist_retries_503_then_succeeds_reconciling_lost_claim(self):
        handle = _make_handle(self.client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[
                self._terminating_error(status=410),
                _agen_then_raise([], asyncio.CancelledError()),
            ]
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            side_effect=[self._terminating_error(status=503), ([], "9")]
        )
        with self.assertRaises(asyncio.CancelledError):
            await handle._watch_loop()
        self.assertEqual(handle._state.get_member("b1-0").state, MemberState.LOST)
        self.assertIsNone(handle.err())
        list_calls = self.mock_k8s_helper.list_sandbox_claim_objects.call_args_list
        self.assertEqual(len(list_calls), 2)

    async def test_410_relist_403_surfaces_through_err_and_stops(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[self._terminating_error(status=410)]
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            side_effect=[self._terminating_error(status=403)]
        )
        await handle._watch_loop()
        self.assertIsInstance(handle.err(), k8s_client.ApiException)
        self.assertEqual(handle.err().status, 403)

    async def test_disconnect_reconnects_from_last_resource_version(self):
        handle = _make_handle(self.client)
        event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "b1-0", "resourceVersion": "77"},
                "spec": {"warmPoolRef": {"name": "pool-a"}},
                "status": {},
            },
        }
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[
                _agen_then_raise([event], ConnectionError("boom")),
                self._terminating_error(status=403),
            ]
        )
        await handle._watch_loop()
        second_call = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[1]
        self.assertEqual(second_call.args[2], "77")

    async def test_bookmark_only_advances_resource_version(self):
        handle = _make_handle(self.client)
        bookmark_event = {"type": "BOOKMARK", "object": {"metadata": {"resourceVersion": "42"}}}
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[_agen([bookmark_event]), self._terminating_error(status=403)]
        )
        await handle._watch_loop()
        self.assertEqual(len(handle._state.members()), 0)
        second_call = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[1]
        self.assertEqual(second_call.args[2], "42")

    async def test_403_surfaces_through_err(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[self._terminating_error(status=403)]
        )
        await handle._watch_loop()
        self.assertIsInstance(handle.err(), k8s_client.ApiException)
        self.assertEqual(handle.err().status, 403)

    async def test_503_waits_retry_after_then_retries_from_last_resource_version(self):
        handle = _make_handle(self.client)
        error = self._terminating_error(status=503)
        error.headers = {"Retry-After": "7"}
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[error, _agen_then_raise([], asyncio.CancelledError())]
        )
        with patch("k8s_agent_sandbox.async_sandbox_batch.asyncio.sleep") as sleep:
            with self.assertRaises(asyncio.CancelledError):
                await handle._watch_loop()
        self.assertIsNone(handle.err())
        sleep.assert_awaited_once_with(7.0)
        calls = self.mock_k8s_helper.watch_sandbox_claims.call_args_list
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[1].args[2], "5")

    async def test_label_selector_is_exact(self):
        handle = _make_handle(self.client, batch_id="b1234")
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[self._terminating_error(status=403)]
        )
        await handle._watch_loop()
        call_args = self.mock_k8s_helper.watch_sandbox_claims.call_args_list[0]
        self.assertEqual(call_args.args[1], f"{BATCH_ID_LABEL}=b1234")

    async def test_ssl_error_surfaces_through_err_and_stops(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[aiohttp.ClientSSLError(MagicMock(), OSError("boom"))]
        )
        await handle._watch_loop()
        self.assertIsInstance(handle.err(), aiohttp.ClientSSLError)

    async def test_connection_failure_and_tls_eof_retry_but_certificate_errors_stop(self):
        refused = aiohttp.ClientConnectorError(MagicMock(), ConnectionRefusedError(111, "refused"))
        tls_eof = aiohttp.ClientConnectorSSLError(
            MagicMock(), ssl.SSLEOFError(8, "EOF occurred in violation of protocol")
        )
        bad_cert = aiohttp.ClientConnectorCertificateError(
            MagicMock(), ssl.SSLCertVerificationError(1, "certificate verify failed")
        )
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(side_effect=[refused, tls_eof, tls_eof.os_error, bad_cert])
        with patch.object(batch_utils, "retry_delay", return_value=0):
            await handle._watch_loop()
        self.assertEqual(self.mock_k8s_helper.watch_sandbox_claims.call_count, 4)
        self.assertIs(handle.err(), bad_cert)

    async def test_client_timeout_retries_from_last_resource_version(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[TimeoutError(), _agen_then_raise([], asyncio.CancelledError())]
        )
        with patch.object(batch_utils, "retry_delay", return_value=0):
            with self.assertRaises(asyncio.CancelledError):
                await handle._watch_loop()
        self.assertIsNone(handle.err())
        calls = self.mock_k8s_helper.watch_sandbox_claims.call_args_list
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[1].args[2], "5")

    async def test_unexpected_error_surfaces_through_err_and_stops(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(side_effect=[RuntimeError("boom")])
        await handle._watch_loop()
        self.assertIsInstance(handle.err(), RuntimeError)

    async def test_410_relist_transport_error_retries_then_reconciles(self):
        handle = _make_handle(self.client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[
                self._terminating_error(status=410),
                _agen_then_raise([], asyncio.CancelledError()),
            ]
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            side_effect=[aiohttp.ClientConnectionError("boom"), ([], "9")]
        )
        with self.assertRaises(asyncio.CancelledError):
            await handle._watch_loop()
        self.assertEqual(handle._state.get_member("b1-0").state, MemberState.LOST)
        self.assertIsNone(handle.err())
        self.assertEqual(len(self.mock_k8s_helper.list_sandbox_claim_objects.call_args_list), 2)

    async def test_consecutive_failures_and_empty_watches_back_off_and_an_event_resets_the_delay(self):
        handle = _make_handle(self.client)
        event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "b1-0", "resourceVersion": "6"},
                "spec": {"warmPoolRef": {"name": "pool-a"}},
                "status": {},
            },
        }
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[
                self._terminating_error(status=503),
                aiohttp.ClientConnectionError("boom"),
                self._terminating_error(status=429),
                # A watch that ends at once with no events.
                _agen([]),
                _agen_then_raise([event], self._terminating_error(status=503)),
                _agen_then_raise([], asyncio.CancelledError()),
            ]
        )
        with patch(
            "k8s_agent_sandbox.async_sandbox_batch.asyncio.sleep", new=AsyncMock()
        ) as mock_sleep:
            with self.assertRaises(asyncio.CancelledError):
                await handle._watch_loop()

        # Equal jitter keeps attempt n within [base * 2**(n-1) / 2, base * 2**(n-1)].
        delays = [c.args[0] for c in mock_sleep.await_args_list]
        self.assertEqual(len(delays), 5)
        self.assertTrue(0.25 <= delays[0] <= 0.5, delays)
        self.assertTrue(0.5 <= delays[1] <= 1.0, delays)
        self.assertTrue(1.0 <= delays[2] <= 2.0, delays)
        self.assertTrue(2.0 <= delays[3] <= 4.0, delays)
        self.assertTrue(0.25 <= delays[4] <= 0.5, delays)
        self.assertIsNone(handle.err())


class TestRenewal(BaseAsyncBatchClientTest):

    async def _run_renew_loop(self, handle, clock, replace):
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=replace)
        # The nth retry in a row waits n seconds, so the timing is exact.
        with patch.object(async_sandbox_batch, "time", clock), patch.object(
            async_sandbox_batch.asyncio, "sleep", clock.sleep
        ), patch.object(batch_utils, "backoff_delay", lambda failures, base, cap: failures):
            await handle._renew_loop()

    async def test_expiry_fires_a_lease_duration_after_the_last_successful_renewal_was_sent(self):
        # A 10s Lease renews every 3s. The renewal sent at 3 succeeds with a 2s response, the
        # next at 8 fails at once, and its retry at 9 hangs until 12. The next retry waits only
        # until the Lease expires at 3 + 10 = 13.
        clock = _FakeClock()
        handle = _make_handle(self.client, lease_duration=10, lease_renewed_at=0.0)
        outcomes = iter(["slow", "fail", "hang"])

        async def replace(name, namespace, body, _request_timeout):
            outcome = next(outcomes)
            if outcome == "slow":
                clock.now += 2
                return body
            if outcome == "hang":
                clock.now += _request_timeout
                raise asyncio.TimeoutError()
            raise k8s_client.ApiException(status=503)

        await self._run_renew_loop(handle, clock, replace)
        self.assertEqual(clock.now, 13)
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)

    async def test_failed_renewals_retry_on_a_backoff_and_stop_at_expiry(self):
        clock = _FakeClock()
        handle = _make_handle(self.client, lease_duration=10, lease_renewed_at=0.0)
        sent = []

        async def replace(name, namespace, body, _request_timeout):
            sent.append(clock.now)
            # Renewals fail until the Lease expires, and would succeed after it.
            if clock.now < 10:
                raise k8s_client.ApiException(status=503)
            return body

        await self._run_renew_loop(handle, clock, replace)
        # The first failure at the 3s interval is retried after 1s, 2s and 3s, then the Lease
        # expires at 10 before the next retry.
        self.assertEqual(sent, [3, 4, 6, 9])
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)
        self.assertTrue(handle.lease_degraded())

    @patch.object(AsyncSandboxBatch, "_start", lambda self: None)
    async def test_renewals_replace_the_last_written_lease_without_reading_it(self):
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=None, renew_time=datetime.now(UTC))
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        written = [_lease(holder_identity=None, resource_version=rv) for rv in ("11", "12", "13")]
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=written)
        handle = await self.client.get_batch("b1")

        self.assertTrue(await handle._renew_once())
        self.assertTrue(await handle._renew_once())
        bodies = [c.args[2] for c in self.mock_k8s_helper.replace_batch_lease.call_args_list]
        self.assertIs(bodies[1], written[0])
        self.assertIs(bodies[2], written[1])
        # Only get_batch read the Lease.
        self.mock_k8s_helper.read_batch_lease.assert_called_once()

    async def test_conflict_reads_the_lease_and_renews_it_while_still_held(self):
        handle = _make_handle(self.client)
        current = _lease(holder_identity=handle._holder_identity, resource_version="200")
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(
            side_effect=[k8s_client.ApiException(status=409), current]
        )
        self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=current)
        self.assertTrue(await handle._renew_once())
        self.assertIs(self.mock_k8s_helper.replace_batch_lease.call_args.args[2], current)
        self.assertFalse(handle.lease_degraded())
        self.assertIsNone(handle.err())

    async def test_lease_degraded_from_the_first_failed_renewal_until_one_succeeds(self):
        handle = _make_handle(self.client)
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(
            side_effect=[
                RuntimeError("api down"),
                RuntimeError("api down"),
                _lease(holder_identity=handle._holder_identity),
            ]
        )
        self.assertFalse(handle.lease_degraded())
        with self.assertLogs("k8s_agent_sandbox.async_sandbox_batch", level="INFO") as ctx:
            await handle._renew_once()
        self.assertTrue(any("degraded" in msg for msg in ctx.output))
        self.assertTrue(handle.lease_degraded())
        await handle._renew_once()
        self.assertTrue(handle.lease_degraded())
        await handle._renew_once()
        self.assertFalse(handle.lease_degraded())

    async def test_holder_mismatch_stops_renewal_without_writing(self):
        for other_holder in ("someone-else", None):
            with self.subTest(other_holder=other_holder):
                handle = _make_handle(self.client)
                self.mock_k8s_helper.replace_batch_lease = AsyncMock(
                    side_effect=k8s_client.ApiException(status=409)
                )
                self.mock_k8s_helper.read_batch_lease = AsyncMock(
                    return_value=_lease(holder_identity=other_holder)
                )
                self.assertFalse(await handle._renew_once())
                self.mock_k8s_helper.replace_batch_lease.assert_called_once()
                self.assertIsInstance(handle.err(), BatchInUseError)
                self.assertTrue(handle.lease_degraded())

    async def test_missing_lease_stops_renewal_and_records_expired(self):
        # The Lease is gone if the replace finds nothing, or a conflict's read does.
        for replace_status in (404, 409):
            with self.subTest(replace_status=replace_status):
                handle = _make_handle(self.client)
                self.mock_k8s_helper.replace_batch_lease = AsyncMock(
                    side_effect=k8s_client.ApiException(status=replace_status)
                )
                self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=None)
                self.assertFalse(await handle._renew_once())
                self.mock_k8s_helper.replace_batch_lease.assert_called_once()
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
        self.assertEqual(first.state, MemberState.PENDING)

    def test_members_and_err_are_plain_not_coroutine_functions(self):
        self.assertFalse(inspect.iscoroutinefunction(AsyncSandboxBatch.members))
        self.assertFalse(inspect.iscoroutinefunction(AsyncSandboxBatch.err))


class TestConnect(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        self.mock_client = MagicMock()
        self.mock_sandbox_instance = MagicMock()
        self.mock_client.sandbox_class.return_value = self.mock_sandbox_instance
        self.handle = _make_handle(self.mock_client)

    async def test_builds_sandbox_class_without_resolve_or_get_sandbox(self):
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        sandbox = await self.handle.connect(member)

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

    async def test_repeat_call_returns_same_handle(self):
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        first = await self.handle.connect(member)
        second = await self.handle.connect(member)
        self.assertIs(first, second)
        self.mock_client.sandbox_class.assert_called_once()

    async def test_not_ready_member_raises(self):
        self.handle._state.upsert_claim(_claim("b1-0", conditions=[]))
        member = self.handle.members()[0]
        with self.assertRaises(SandboxNotReadyError):
            await self.handle.connect(member)

    async def test_member_no_longer_ready_raises_even_when_cached(self):
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
                await self.handle.connect(member)
                update()
                with self.assertRaises(SandboxNotReadyError):
                    await self.handle.connect(member)

    async def test_connect_after_err_raises_even_when_cached(self):
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        await self.handle.connect(member)
        lease_lost = BatchLeaseExpiredError("lost")
        self.handle._state.note_error(lease_lost)
        with self.assertRaises(BatchError) as raised:
            await self.handle.connect(member)
        self.assertIs(raised.exception.__cause__, lease_lost)

    async def test_connected_handle_not_registered_in_active_connection_sandboxes(self):
        patcher = patch("k8s_agent_sandbox.async_sandbox_client.AsyncK8sHelper")
        patcher.start()
        self.addCleanup(patcher.stop)
        config = SandboxDirectConnectionConfig(api_url="http://test-router:8080")
        real_client = AsyncSandboxClient(connection_config=config, cleanup=False)
        real_client.sandbox_class = MagicMock(return_value=MagicMock())
        handle = _make_handle(real_client)
        handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = handle.members()[0]
        await handle.connect(member)
        self.assertEqual(real_client._active_connection_sandboxes, {})


class TestDetach(unittest.IsolatedAsyncioTestCase):

    def setUp(self):
        self.mock_client = MagicMock()
        self.mock_client._unregister_batch = MagicMock()
        self.handle = _make_handle(self.mock_client)

    async def test_invalid_grace_raises_value_error_and_leaves_handle_running(self):
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()
        with self.assertRaises(ValueError):
            await self.handle.detach(grace=1.5)
        self.mock_client.k8s_helper.replace_batch_lease.assert_not_called()
        self.assertFalse(self.handle._detached)

    async def test_final_write_clears_holder_sets_renew_time_and_grace_duration(self):
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(
                holder_identity=self.handle._holder_identity,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: str(BATCH_DEFAULT_LEASE_DURATION_SECONDS)},
            )
        )
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()
        await self.handle.detach(grace=10)
        written = self.mock_client.k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertIsNone(written.spec.holder_identity)
        self.assertLess(abs((written.spec.renew_time - datetime.now(UTC)).total_seconds()), 5)
        self.assertEqual(written.spec.lease_duration_seconds, 10)
        self.assertEqual(
            written.metadata.annotations,
            {BATCH_LEASE_DURATION_ANNOTATION: str(BATCH_DEFAULT_LEASE_DURATION_SECONDS)},
        )

    async def test_grace_none_uses_handles_lease_duration(self):
        handle = _make_handle(self.mock_client, lease_duration=90)
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=handle._holder_identity)
        )
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()
        await handle.detach()
        written = self.mock_client.k8s_helper.replace_batch_lease.call_args.args[2]
        self.assertEqual(written.spec.lease_duration_seconds, 90)

    async def test_idempotent_detach(self):
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=self.handle._holder_identity)
        )
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()
        mock_sandbox = MagicMock()
        mock_sandbox.close_connection = AsyncMock()
        self.handle._connected["b1-0"] = mock_sandbox

        await self.handle.detach()

        mock_sandbox.close_connection.assert_called_once()
        self.mock_client._unregister_batch.assert_called_once_with(self.handle)
        self.assertEqual(self.handle._connected, {})

        await self.handle.detach()

        mock_sandbox.close_connection.assert_called_once()
        self.mock_client._unregister_batch.assert_called_once_with(self.handle)
        self.mock_client.k8s_helper.read_batch_lease.assert_called_once()
        self.mock_client.k8s_helper.replace_batch_lease.assert_called_once()

    async def test_release_conflict_succeeds_only_once_the_lease_is_not_held(self):
        # A write from an earlier detach() call that timed out but landed can release the Lease between this
        # call's read and write, and a late renewal of this handle can leave it held, which detach() releases
        # with one more write.
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
                mock_client.k8s_helper.read_batch_lease = AsyncMock(side_effect=[first, current])
                mock_client.k8s_helper.replace_batch_lease = AsyncMock(side_effect=[
                    k8s_client.ApiException(status=status) if status != 200 else None for status in writes
                ])

                if released:
                    await handle.detach(grace=17)
                else:
                    with self.assertRaises(k8s_client.ApiException):
                        await handle.detach(grace=17)
                self.assertEqual(handle._lease_released, released)
                replace = mock_client.k8s_helper.replace_batch_lease
                self.assertEqual(replace.call_count, len(writes))
                written = replace.call_args.args[2]
                self.assertIs(written, current if len(writes) == 2 else first)
                self.assertIsNone(written.spec.holder_identity)
                self.assertEqual(written.spec.lease_duration_seconds, 17)

    async def test_another_detach_during_the_write_keeps_this_calls_grace(self):
        # Another detach(grace=99) stores its grace while this call writes the Lease, and this call's
        # writes must keep the grace it started with.
        current = _lease(holder_identity=self.handle._holder_identity)

        def other_detach_then_conflict(*args, **kwargs):
            if self.handle._detach_grace != 99:
                self.handle._detach_grace = 99
                raise k8s_client.ApiException(status=409)

        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(side_effect=[
            _lease(holder_identity=self.handle._holder_identity), current
        ])
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock(side_effect=other_detach_then_conflict)

        await self.handle.detach(grace=17)

        self.assertEqual(current.spec.lease_duration_seconds, 17)

    async def test_retryable_after_release_write_failure(self):
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=self.handle._holder_identity)
        )
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock(
            side_effect=RuntimeError("api down")
        )

        with self.assertRaises(RuntimeError):
            await self.handle.detach()
        self.assertTrue(self.handle._detached)
        self.assertFalse(self.handle._lease_released)
        self.mock_client._unregister_batch.assert_not_called()

        self.mock_client.k8s_helper.replace_batch_lease.side_effect = None
        await self.handle.detach()  # Retry: completes the release.
        self.assertTrue(self.handle._lease_released)
        self.mock_client._unregister_batch.assert_called_once_with(self.handle)

    async def test_close_connection_failure_still_releases_lease(self):
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity=self.handle._holder_identity)
        )
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()
        bad_sandbox = MagicMock()
        bad_sandbox.close_connection = AsyncMock(side_effect=RuntimeError("boom"))
        self.handle._connected["b1-0"] = bad_sandbox

        with self.assertLogs("k8s_agent_sandbox.async_sandbox_batch", level="WARNING"):
            await self.handle.detach()  # Must not raise.

        self.mock_client.k8s_helper.replace_batch_lease.assert_called_once()
        self.mock_client._unregister_batch.assert_called_once_with(self.handle)

    async def test_a_connection_whose_put_back_is_cancelled_stays_on_the_handle(self):
        async def close_connection():
            # Holding the lock makes any wait for it during the put-back block until cancelled.
            await self.handle._lock.acquire()
            raise RuntimeError("close failed")

        cached = MagicMock(close_connection=AsyncMock(side_effect=close_connection))
        self.handle._connected["b1-0"] = cached

        task = asyncio.create_task(self.handle._close_cached_connections("detach"))
        await asyncio.sleep(0.01)
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

        self.assertEqual(self.handle._connected, {"b1-0": cached})
        self.mock_client._track_closing.assert_called_once_with(self.handle)

    async def test_foreign_holder_lease_is_left_untouched(self):
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(
            return_value=_lease(holder_identity="someone-else")
        )
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()

        await self.handle.detach()  # Must not raise.

        self.mock_client.k8s_helper.replace_batch_lease.assert_not_called()
        self.mock_client._unregister_batch.assert_called_once_with(self.handle)

    async def test_connect_after_detach_raises(self):
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(return_value=_lease())
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        member = self.handle.members()[0]
        await self.handle.detach()
        with self.assertRaises(BatchError):
            await self.handle.connect(member)

    async def test_members_and_err_keep_last_snapshot_after_detach(self):
        self.mock_client.k8s_helper.read_batch_lease = AsyncMock(return_value=_lease())
        self.mock_client.k8s_helper.replace_batch_lease = AsyncMock()
        self.handle._state.upsert_claim(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}], sandbox={"name": "sbx-1"})
        )
        self.handle._state.note_error(RuntimeError("boom"))

        await self.handle.detach()

        self.assertEqual([m.claim_name for m in self.handle.members()], ["b1-0"])
        self.assertIsInstance(self.handle.err(), RuntimeError)


class TestClientClose(BaseAsyncBatchClientTest):

    async def test_close_stops_tracked_batches_and_closes_their_connections(self):
        self.mock_k8s_helper.close = AsyncMock()
        handle = _make_handle(self.client)
        # A failed stop must still leave the cached connection closed.
        handle._stop_background_tasks = AsyncMock(side_effect=RuntimeError("stop failed"))
        cached = MagicMock(close_connection=AsyncMock())
        handle._connected["b1-0"] = cached
        self.client._active_batches[("default", "b1")] = handle

        await self.client.close()

        handle._stop_background_tasks.assert_awaited_once()
        cached.close_connection.assert_awaited_once()
        self.assertEqual(self.client._active_batches, {})
        with self.assertRaises(BatchError):
            await handle.connect(MagicMock())
        with self.assertRaises(BatchError):
            await handle.detach()

    async def test_a_connection_that_fails_to_close_is_retried_by_the_next_cleanup(self):
        self.mock_k8s_helper.close = AsyncMock()
        for error in (RuntimeError("close failed"), asyncio.CancelledError()):
            with self.subTest(type(error).__name__):
                handle = _make_handle(self.client)
                cached = MagicMock(close_connection=AsyncMock(side_effect=[error, None]))
                handle._connected["b1-0"] = cached
                self.client._active_batches[("default", "b1")] = handle

                with contextlib.suppress(asyncio.CancelledError):
                    await self.client.close()
                self.assertEqual(handle._connected, {"b1-0": cached})
                self.assertIn(handle, [*self.client._active_batches.values(), *self.client._closing_batches])

                await self.client.close()
                self.assertEqual(cached.close_connection.await_count, 2)
                self.assertEqual(handle._connected, {})
                self.assertEqual(self.client._active_batches, {})
                self.assertEqual(self.client._closing_batches, set())

    async def test_cleanup_closes_a_reattached_batchs_old_connection_without_stopping_the_old_handle(self):
        self.mock_k8s_helper.close = AsyncMock()
        for lease_write_error in (None, RuntimeError("timed out after the write applied")):
            with self.subTest(lease_write_error=lease_write_error):
                old = _make_handle(self.client)
                cached = MagicMock(close_connection=AsyncMock(side_effect=[RuntimeError("close failed"), None]))
                old._connected["b1-0"] = cached
                self.client._active_batches[("default", "b1")] = old
                self.mock_k8s_helper.read_batch_lease = AsyncMock(
                    return_value=_lease(holder_identity=old._holder_identity)
                )
                self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=lease_write_error)

                with self.assertLogs(level="WARNING"), contextlib.suppress(RuntimeError):
                    await old.detach()
                new = _make_handle(self.client)
                new._stop_background_tasks = AsyncMock()
                with patch.object(AsyncSandboxBatch, "_attach", AsyncMock(return_value=new)):
                    await self.client.get_batch("b1")
                old._stop_background_tasks = AsyncMock()

                await self.client.close()
                self.assertEqual(cached.close_connection.await_count, 2)
                old._stop_background_tasks.assert_not_called()
                new._stop_background_tasks.assert_awaited_once()
                self.assertEqual(self.client._closing_batches, set())

    async def test_a_close_that_fails_after_an_overlapping_cleanup_and_reattach_is_retried(self):
        self.mock_k8s_helper.close = AsyncMock()
        handle = _make_handle(self.client)
        newer = _make_handle(self.client)

        async def close_connection():
            if cached.close_connection.await_count == 1:
                # An overlapping cleanup finds no cached connection and unregisters the handle, then
                # get_batch registers a newer handle for the same batch.
                await self.client.close()
                with patch.object(AsyncSandboxBatch, "_attach", AsyncMock(return_value=newer)):
                    await self.client.get_batch("b1")
                raise RuntimeError("close failed")

        cached = MagicMock(close_connection=AsyncMock(side_effect=close_connection))
        handle._connected["b1-0"] = cached
        self.client._active_batches[("default", "b1")] = handle
        # detach() fails after its close, so only the put-back can keep the connection tracked.
        self.mock_k8s_helper.read_batch_lease = AsyncMock(side_effect=RuntimeError("api down"))

        with self.assertRaises(RuntimeError):
            await handle.detach()
        self.assertEqual(self.client._closing_batches, {handle})

        await self.client.close()
        self.assertEqual(cached.close_connection.await_count, 2)
        self.assertEqual(self.client._closing_batches, set())

    async def test_cleanup_finishes_an_unfinished_detach_with_its_grace(self):
        self.mock_k8s_helper.close = AsyncMock()
        for grace, written_duration in ((42, 42), (None, 90)):
            with self.subTest(grace=grace):
                handle = _make_handle(self.client, lease_duration=90)
                self.client._active_batches[("default", "b1")] = handle
                helper = self.client.k8s_helper
                helper.read_batch_lease = AsyncMock(
                    side_effect=lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                )
                helper.replace_batch_lease = AsyncMock(side_effect=[RuntimeError("api down"), None])
                with self.assertRaises(RuntimeError):
                    await handle.detach(grace=grace)

                await self.client.close()

                written = helper.replace_batch_lease.call_args.args[2]
                self.assertEqual(helper.replace_batch_lease.await_count, 2)
                self.assertIsNone(written.spec.holder_identity)
                self.assertEqual(written.spec.lease_duration_seconds, written_duration)
                self.assertEqual(self.client._active_batches, {})

    async def test_a_failed_detach_retry_is_logged_and_keeps_the_handle_registered(self):
        self.mock_k8s_helper.close = AsyncMock()
        handle = _make_handle(self.client)
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease = AsyncMock(side_effect=lambda *a, **k: _lease(holder_identity=handle._holder_identity))
        helper.replace_batch_lease = AsyncMock(side_effect=RuntimeError("api down"))
        with self.assertRaises(RuntimeError):
            await handle.detach(grace=42)

        with self.assertLogs(level="ERROR"):
            await self.client.close()

        self.assertEqual(helper.replace_batch_lease.await_count, 2)
        self.assertEqual(self.client._active_batches, {("default", "b1"): handle})

    def test_atexit_closes_batch_connections_and_releases_only_claimed_batches(self):
        handle = _make_handle(self.client)
        cached = MagicMock()
        handle._connected["b1-0"] = cached
        self.client._active_batches[("default", "b1")] = handle
        closing = _make_handle(self.client)
        closing_cached = MagicMock()
        closing._connected["b1-0"] = closing_cached
        self.client._closing_batches.add(closing)
        half_detached = _make_handle(self.client, batch_id="b2")
        half_detached._detach_grace = 30
        self.client._active_batches[("default", "b2")] = half_detached
        attached = _make_handle(self.client, batch_id="b3")
        attached._attached = True
        attached_cached = MagicMock()
        attached._connected["b3-0"] = attached_cached
        self.client._active_batches[("default", "b3")] = attached

        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper"), \
             patch("k8s_agent_sandbox.async_sandbox_client._delete_batch_objects_sync") as delete:
            self.client._atexit_cleanup()

        cached._close_for_atexit.assert_called_once()
        closing_cached._close_for_atexit.assert_called_once()
        attached_cached._close_for_atexit.assert_called_once()
        delete.assert_called_once()
        self.assertEqual(delete.call_args.args[2], "b1")

    def test_unregistering_an_old_handle_keeps_a_newer_one_registered(self):
        old_handle = MagicMock(namespace="default", batch_id="b1")
        new_handle = MagicMock(namespace="default", batch_id="b1")
        self.client._active_batches[("default", "b1")] = new_handle

        self.client._unregister_batch(old_handle)

        self.assertEqual(self.client._active_batches, {("default", "b1"): new_handle})


_READY = [{"type": "Ready", "status": "True"}]
_NOT_READY = [{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}]


class _Cluster:
    """A fake AsyncK8sHelper for claim_batch: records requests and feeds created claims to the watch."""

    def __init__(self, ready_on_create=True):
        self.ready_on_create = ready_on_create
        self.calls: list[str] = []
        self.created: list[tuple[str, float]] = []
        self.watch_queue: asyncio.Queue = asyncio.Queue()
        self.create_hook = None
        self.helper = MagicMock()
        h = self.helper
        h.get_sandbox_warmpool = AsyncMock(return_value={"spec": {"sandboxTemplateRef": {"name": "tmpl"}}})
        h.get_sandbox_template = AsyncMock(return_value={"metadata": {"name": "tmpl"}})
        h.create_batch_lease = AsyncMock(return_value=_lease())
        h.list_sandbox_claim_objects = AsyncMock(return_value=([], "7"))
        h.watch_sandbox_claims = MagicMock(side_effect=self._watch)
        h.create_sandbox_claim = AsyncMock(side_effect=self._create)
        h.delete_sandbox_claims_by_label = AsyncMock(side_effect=lambda *a, **k: self.calls.append("deletecollection"))
        h.delete_batch_lease = AsyncMock(side_effect=lambda *a, **k: self.calls.append("delete_lease"))
        # A Lease no handle holds, so release() takes it for its handle.
        h.read_batch_lease = AsyncMock(return_value=_lease())
        h.replace_batch_lease = AsyncMock()

    async def _watch(self, *args, **kwargs):
        try:
            yield await asyncio.wait_for(self.watch_queue.get(), 0.05)
        except asyncio.TimeoutError:
            return
        while not self.watch_queue.empty():
            yield self.watch_queue.get_nowait()

    async def _create(self, name, warmpool, namespace, **kwargs):
        if self.create_hook is not None:
            await self.create_hook(name)
        self.calls.append("create")
        self.created.append((name, time.monotonic()))
        if self.ready_on_create:
            self.push(name, warmpool)

    def push(self, name, warmpool="pool-a", ready=True):
        conditions = _READY if ready else _NOT_READY
        sandbox = {"name": f"sbx-{name}"} if ready else None
        self.watch_queue.put_nowait(
            {"type": "MODIFIED", "object": _claim(name, warmpool, conditions=conditions, sandbox=sandbox)}
        )

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


async def _wait_until(condition, timeout=5.0):
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        await asyncio.sleep(0.01)


class _SignalingLock:
    """Wraps a lock for a race test. ``start_racing`` runs a call in another task and returns
    once that task waits for this lock or finishes, so the test needs no timed wait."""

    def __init__(self, lock):
        self._lock = lock
        self._blocked_or_done = asyncio.Event()
        self.tasks: list[asyncio.Task] = []

    async def __aenter__(self):
        if self._lock.locked():
            self._blocked_or_done.set()
        await self._lock.acquire()

    async def __aexit__(self, *exc_info):
        self._lock.release()

    async def start_racing(self, target, *args):
        async def run():
            try:
                await target(*args)
            finally:
                self._blocked_or_done.set()

        self.tasks.append(asyncio.create_task(run()))
        await asyncio.wait_for(self._blocked_or_done.wait(), 5)


class BaseClaimBatchTest(BaseAsyncBatchClientTest):

    def setUp(self):
        super().setUp()
        self.cluster = _Cluster()
        self.client.k8s_helper = self.cluster.helper
        # The fake's watch ends as soon as it has no events, which an apiserver's watch never does.
        short_watch = patch.object(async_sandbox_batch, "_SHORT_WATCH_SECONDS", 0)
        short_watch.start()
        self.addCleanup(short_watch.stop)

    async def claim(self, groups=None, **kwargs):
        kwargs.setdefault("batch_id", "b1")
        kwargs.setdefault("create_rps", 1000)
        batch = await self.client.claim_batch(
            groups or [BatchGroup(warmpool="pool-a", size=2)], **kwargs
        )
        self.addAsyncCleanup(batch._stop_background_tasks)
        return batch


class TestClaimBatchSetup(BaseClaimBatchTest):

    async def test_setup_order_and_lease(self):
        helper = self.cluster.helper
        helper.get_sandbox_warmpool.side_effect = lambda *a, **k: (
            self.cluster.calls.append("precheck") or {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
        )
        helper.create_batch_lease.side_effect = lambda *a, **k: self.cluster.calls.append("lease")
        helper.list_sandbox_claim_objects.side_effect = (
            lambda *a, **k: self.cluster.calls.append("list") or ([], "7")
        )
        batch = await self.claim(
            [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
            quorum_timeout=90,
            work_budget=120,
            lease_duration=45,
        )
        await _wait_until(lambda: len(self.cluster.created) == 2)

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

    async def test_claim_manifests(self):
        _SteppingDatetime.calls = 0
        with patch("k8s_agent_sandbox.utils.datetime", _SteppingDatetime):
            await self.claim(
                [BatchGroup(warmpool="pool-a", size=2, min_ready=1), BatchGroup(warmpool="pool-b", size=1)],
                labels={"team": "rl"},
                create_rps=2.5,
                quorum_timeout=90,
                work_budget=120,
            )
            await _wait_until(lambda: len(self.cluster.created) == 3)

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
        # One shutdownTime for the fill, counting ceil(3 / 2.5) seconds of pacing.
        lifecycle = {"shutdownTime": "2026-01-01T01:13:32Z", "shutdownPolicy": "Delete"}
        self.assertEqual([c.kwargs["lifecycle"] for c in calls], [lifecycle] * 3)
        self.assertEqual(kwargs["log_level"], logging.DEBUG)

    async def test_precheck_failure_writes_nothing(self):
        helper = self.cluster.helper
        cases = {
            "missing warm pool": ("get_sandbox_warmpool", None, SandboxWarmPoolNotFoundError),
            "missing template": ("get_sandbox_template", None, SandboxTemplateNotFoundError),
            "warm pool names no template": ("get_sandbox_warmpool", {"spec": {}}, SandboxTemplateNotFoundError),
            "forbidden": ("get_sandbox_warmpool", k8s_client.ApiException(status=403), k8s_client.ApiException),
        }
        for name, (method, result, error) in cases.items():
            with self.subTest(name):
                helper.get_sandbox_warmpool = AsyncMock(return_value={"spec": {"sandboxTemplateRef": {"name": "tmpl"}}})
                helper.get_sandbox_template = AsyncMock(return_value={})
                helper.create_batch_lease.reset_mock()
                if isinstance(result, Exception):
                    getattr(helper, method).side_effect = result
                else:
                    getattr(helper, method).return_value = result
                with self.assertRaises(error):
                    await self.client.claim_batch([BatchGroup(warmpool="pool-a", size=1)])
                helper.create_batch_lease.assert_not_called()

    async def test_setup_failure_deletes_only_a_lease_it_created(self):
        helper = self.cluster.helper
        with self.subTest("lease exists"):
            helper.create_batch_lease.side_effect = k8s_client.ApiException(status=409)
            with self.assertRaises(BatchExistsError):
                await self.claim()
            helper.delete_batch_lease.assert_not_called()
        helper.create_batch_lease.side_effect = None
        for name, list_result, error in (
            ("claims exist", ([_claim("b1-0")], "7"), BatchExistsError),
            ("list fails", k8s_client.ApiException(status=500), k8s_client.ApiException),
            ("list interrupted", asyncio.CancelledError(), asyncio.CancelledError),
        ):
            with self.subTest(name):
                helper.delete_batch_lease.reset_mock()
                if isinstance(list_result, BaseException):
                    helper.list_sandbox_claim_objects.side_effect = list_result
                else:
                    helper.list_sandbox_claim_objects.side_effect = None
                    helper.list_sandbox_claim_objects.return_value = list_result
                with self.assertRaises(error):
                    await self.claim()
                self.assertEqual(helper.delete_batch_lease.call_args.args[:2], ("batch-b1", "default"))
        self.assertEqual(self.cluster.created, [])


class TestClaimBatchCreation(BaseClaimBatchTest):

    async def test_creates_are_bounded_by_max_in_flight_and_paced_by_create_rps(self):
        gate = asyncio.Event()
        self.cluster.create_hook = lambda name: gate.wait()
        await self.claim([BatchGroup(warmpool="pool-a", size=5)], max_in_flight=2)
        await _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count == 2)
        await asyncio.sleep(0.2)
        self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 2)
        gate.set()
        await _wait_until(lambda: len(self.cluster.created) == 5)

        self.cluster.create_hook = None
        self.cluster.created.clear()
        start = time.monotonic()
        await self.claim([BatchGroup(warmpool="pool-a", size=5)], batch_id="b2", create_rps=20)
        await _wait_until(lambda: len(self.cluster.created) == 5)
        starts = sorted(t for _, t in self.cluster.created)
        for k, t in enumerate(starts):
            self.assertGreaterEqual(t - start, k * 0.05 - 0.001)

    async def test_creates_each_groups_min_ready_claims_first_in_group_order(self):
        await self.claim(
            [BatchGroup(warmpool="pool-a", size=6, min_ready=3), BatchGroup(warmpool="pool-b", size=3, min_ready=1)],
            max_in_flight=1,
        )
        await _wait_until(lambda: len(self.cluster.created) == 9)
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

    async def test_creation_stops_once_err_is_set(self):
        attached = asyncio.Event()
        handle = {}

        async def fail_watch_during_first_create(name):
            await attached.wait()
            async with handle["batch"]._lock:
                handle["batch"]._state.note_error(RuntimeError("watch failed"))

        self.cluster.create_hook = fail_watch_during_first_create
        handle["batch"] = batch = await self.claim([BatchGroup(warmpool="pool-a", size=5)], max_in_flight=1)
        attached.set()
        await _wait_until(lambda: all(c.done() for c in batch._creators))
        self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 1)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_create_retries_transient_errors_and_fails_on_403(self, _):
        helper = self.cluster.helper
        sent_at = []
        outcomes = iter([k8s_client.ApiException(status=503), None])

        async def create(*args, **kwargs):
            sent_at.append(time.monotonic())
            outcome = next(outcomes)
            if outcome is not None:
                raise outcome

        helper.create_sandbox_claim = AsyncMock(side_effect=create)
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)], create_rps=5)
        await _wait_until(lambda: helper.create_sandbox_claim.call_count == 2)
        self.assertEqual([c.args[0] for c in helper.create_sandbox_claim.call_args_list], ["b1-0", "b1-0"])
        # The retry waits for the next pacing slot, 1/create_rps after the first send.
        self.assertGreaterEqual(sent_at[1] - sent_at[0], 0.18)
        await asyncio.sleep(0.1)
        self.assertEqual(batch.members(), [])  # No CreateFailed member.

        forbidden = k8s_client.ApiException(status=403, reason="Forbidden")
        forbidden.body = '{"kind": "Status", "message": "sandboxclaims is forbidden"}'
        helper.create_sandbox_claim = AsyncMock(side_effect=forbidden)
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b2")
        await _wait_until(lambda: batch.members() != [])
        [member] = batch.members()
        self.assertEqual(
            (member.state, member.reason, member.message),
            (MemberState.FAILED, "CreateFailed", "sandboxclaims is forbidden"),
        )

    async def test_a_backoff_retry_leaves_the_next_pacing_slot_free_and_a_retry_after_takes_it(self):
        throttled = k8s_client.ApiException(status=429)
        throttled.headers = {"Retry-After": "7"}
        for name, error, earliest, latest in (
            # Another create takes the next slot while this one backs off.
            ("a transport error", aiohttp.ClientConnectionError("boom"), 0, 0.2),
            # The apiserver throttles every create, so the next slot comes after its Retry-After.
            ("a Retry-After", throttled, 7, 7.4),
        ):
            with self.subTest(name):
                handle = _make_handle(self.client)
                handle._claim_annotations = {"pool-a": {}}
                handle._create_interval = 0.2
                self.cluster.helper.create_sandbox_claim.reset_mock()
                self.cluster.helper.create_sandbox_claim.side_effect = [error, None]
                next_free_slot = []

                async def wait(timeout):
                    # Another create reserves a slot during this create's first wait.
                    if not next_free_slot:
                        now = time.monotonic()
                        async with handle._lock:
                            next_free_slot.append(handle._reserve_create_slot(now) - now)
                    return False

                handle._wait_for_stop = wait
                await handle._create_one("b1-0", BatchGroup(warmpool="pool-a", size=1))
                self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 2)
                self.assertTrue(earliest <= next_free_slot[0] < latest, next_free_slot)


class TestRelease(BaseClaimBatchTest):

    async def test_release_stops_creates_then_deletes_claims_then_the_lease(self):
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=20)], create_rps=10)
        await _wait_until(lambda: len(self.cluster.created) >= 1)
        self.cluster.helper.list_sandbox_claim_objects.return_value = (
            [{"metadata": {"name": "b1-0", "deletionTimestamp": "2026-01-01T00:00:00Z"}}],
            "9",
        )
        await batch.release()
        await asyncio.sleep(0.3)  # Several pacing slots, in case a creator is still running.
        calls = self.cluster.calls
        self.assertLess(len(self.cluster.created), 20)
        self.assertEqual(calls[calls.index("deletecollection"):], ["deletecollection", "delete_lease"])
        self.assertNotIn(("default", "b1"), self.client._active_batches)

        self.cluster.helper.delete_sandbox_claims_by_label.reset_mock()
        await batch.release()  # Idempotent.
        self.cluster.helper.delete_sandbox_claims_by_label.assert_not_called()
        with self.assertRaises(BatchError):
            await batch.connect(MagicMock(claim_name="b1-0"))
        with self.assertRaises(BatchError):
            await batch.detach()

    async def test_hung_create_delays_release_only_until_the_request_timeout(self):
        hung = asyncio.Event()
        self.cluster.create_hook = lambda name: hung.wait()
        with patch.object(batch_utils, "BATCH_REQUEST_TIMEOUT_SECONDS", 0):
            batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
            await _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count == 1)
            start = time.monotonic()
            await batch.release()
        self.assertLess(time.monotonic() - start, 3)

    async def test_release_after_detach_raises(self):
        handle = _make_handle(self.client)
        self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=handle._holder_identity)
        await handle.detach()
        with self.assertRaises(BatchError):
            await handle.release()
        self.cluster.helper.delete_sandbox_claims_by_label.assert_not_called()

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_release_retries_rounds_until_no_progress(self, _):
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
                helper.delete_sandbox_claims_by_label = AsyncMock(side_effect=delete_effects.get("delete_sandbox_claims_by_label"))
                helper.list_sandbox_claim_objects = AsyncMock(side_effect=list_effects.get("list_sandbox_claim_objects", [live(0)] * 4))
                helper.delete_batch_lease = AsyncMock()
                handle = _make_handle(self.client)
                if error is None:
                    await handle.release()
                    helper.delete_batch_lease.assert_called_once()
                else:
                    with self.assertRaises(error):
                        await handle.release()
                    helper.delete_batch_lease.assert_not_called()
                    self.assertFalse(handle._released)

    async def test_requests_carry_request_timeouts(self):
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        await _wait_until(lambda: len(self.cluster.created) == 1)
        await batch.release()
        helper = self.cluster.helper
        self.assertEqual(helper.create_sandbox_claim.call_args.kwargs["_request_timeout"], 30)
        self.assertEqual(helper.delete_sandbox_claims_by_label.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.delete_batch_lease.call_args.kwargs["_request_timeout"], 30)

    async def test_detach_stops_creates_and_deletes_nothing(self):
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=20)], create_rps=10)
        await _wait_until(lambda: len(self.cluster.created) >= 1)
        self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=batch._holder_identity)
        await batch.detach()
        created = len(self.cluster.created)
        await asyncio.sleep(0.3)
        self.assertEqual(len(self.cluster.created), created)
        self.assertLess(created, 20)
        self.cluster.helper.delete_sandbox_claims_by_label.assert_not_called()
        self.cluster.helper.delete_batch_lease.assert_not_called()

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_release_deletes_nothing_while_another_handle_holds_the_lease(self, _):
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
                helper.list_sandbox_claim_objects.side_effect = [([_claim("b1-0")], "9"), ([], "9")]
                handle = _make_handle(self.client)
                self.client._active_batches[("default", "b1")] = handle

                with self.assertRaises(BatchInUseError):
                    await handle.release()
                helper.replace_batch_lease.assert_not_awaited()
                self.assertEqual(self.cluster.calls, deletes)
                self.assertEqual(self.client._active_batches, {})

                helper.reset_mock()
                with self.assertRaises(BatchError):
                    await handle.release()
                helper.read_batch_lease.assert_not_awaited()

    async def test_release_retries_one_lease_conflict_then_raises_in_use(self):
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
                    await handle.release()
                    helper.delete_sandbox_claims_by_label.assert_awaited_once()
                else:
                    with self.assertRaises(error):
                        await handle.release()
                    helper.delete_sandbox_claims_by_label.assert_not_awaited()
                    helper.delete_batch_lease.assert_not_awaited()

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_release_with_no_lease_deletes_the_claims_and_creates_nothing(self, _):
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
                    await handle.release()
                else:
                    with self.assertRaises(k8s_client.ApiException):
                        await handle.release()
                self.assertEqual(self.cluster.calls, calls)
                helper.create_batch_lease.assert_not_awaited()
                self.assertEqual(handle._released, take_error is None)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_release_takes_the_lease_before_each_round_and_deletes_it_at_the_version_written(self, _):
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
                written = iter(
                    [_lease(resource_version="11"), k8s_client.ApiException(status=503), _lease(resource_version="13")]
                )

                def replace(*args, **kwargs):
                    self.cluster.calls.append("take")
                    result = next(written)
                    if isinstance(result, Exception):
                        raise result
                    return result

                helper.replace_batch_lease.side_effect = replace
                # The second round's take fails, so only the third round deletes the Lease.
                helper.list_sandbox_claim_objects.side_effect = [([_claim("b1-0")], "9"), ([], "9"), ([], "9")]
                await handle.release()
                self.assertEqual(
                    self.cluster.calls, ["take", "deletecollection", "take", "take", "deletecollection", "delete_lease"]
                )
                self.assertEqual(helper.delete_batch_lease.await_args.kwargs["resource_version"], "13")

    async def test_the_first_lifecycle_call_wins(self):
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
                blocked, resume = asyncio.Event(), asyncio.Event()
                outcomes = {}

                async def record(call, block):
                    self.cluster.calls.append(call)
                    if block and not blocked.is_set():
                        blocked.set()
                        await resume.wait()
                    return _lease(resource_version="11")

                helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)

                async def write(*args, **kwargs):
                    return await record("write", first == "detach")

                async def deletecollection(*args, **kwargs):
                    return await record("deletecollection", True)

                helper.replace_batch_lease.side_effect = write
                helper.delete_sandbox_claims_by_label.side_effect = deletecollection

                async def run(key, method):
                    try:
                        await getattr(handle, method)()
                        outcomes[key] = None
                    except Exception as e:
                        outcomes[key] = e

                first_task = asyncio.create_task(run("first", first))
                await asyncio.wait_for(blocked.wait(), 5)
                await lock.start_racing(run, "second", second)
                self.assertEqual("second" in outcomes, not second_waits)
                self.assertEqual(self.cluster.calls, calls_while_blocked)

                resume.set()
                await asyncio.gather(first_task, *lock.tasks)
                self.assertIsNone(outcomes["first"])
                self.assertIs(type(outcomes["second"]), second_error or type(None))
                self.assertEqual(self.cluster.calls, calls)


class TestClientBatchCleanup(BaseClaimBatchTest):

    @patch.object(AsyncSandboxBatch, "_start", lambda self: None)
    async def test_delete_all_releases_every_tracked_batch_and_exit_cleanup_detaches_attached_ones(self):
        for cleanup, exiting in (("delete_all", False), ("_delete_automatic_sandboxes", True)):
            with self.subTest(cleanup):
                self.client._active_batches.clear()
                claimed = await self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b1")
                self.cluster.helper.list_sandbox_claim_objects.return_value = ([_claim("b2-0")], "5")
                self.cluster.helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC))
                attached = await self.client.get_batch("b2")
                self.cluster.helper.list_sandbox_claim_objects.return_value = ([], "7")
                self.assertEqual(set(self.client._active_batches.values()), {claimed, attached})

                self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=attached._holder_identity)
                with patch.object(AsyncSandboxBatch, "release", autospec=True) as release:
                    await getattr(self.client, cleanup)()
                released = {claimed} if exiting else {claimed, attached}
                self.assertEqual({c.args[0] for c in release.call_args_list}, released)
                self.assertEqual(attached._lease_released, exiting)
                self.assertEqual(attached in self.client._active_batches.values(), not exiting)

                self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=claimed._holder_identity)
                await claimed.release()
                self.cluster.helper.read_batch_lease.return_value = _lease(holder_identity=attached._holder_identity)
                await attached.detach()
                self.assertEqual(self.client._active_batches, {})

    async def test_delete_all_finishes_an_unfinished_detach_and_deletes_nothing(self):
        handle = _make_handle(self.client)
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
        helper.replace_batch_lease.side_effect = [RuntimeError("api down"), None]
        with self.assertRaises(RuntimeError):
            await handle.detach(grace=42)

        await self.client.delete_all()

        self.assertEqual(helper.replace_batch_lease.call_args.args[2].spec.lease_duration_seconds, 42)
        self.assertEqual(self.client._active_batches, {})
        helper.delete_sandbox_claims_by_label.assert_not_called()
        helper.delete_batch_lease.assert_not_called()

    async def test_cleanup_releases_a_handle_whose_release_failed_after_a_failed_detach(self):
        handle = _make_handle(self.client)
        handle._attached = True
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
        helper.replace_batch_lease.side_effect = RuntimeError("api down")
        with self.assertRaises(RuntimeError):
            await handle.detach(grace=42)
        helper.replace_batch_lease.side_effect = None
        helper.delete_sandbox_claims_by_label.side_effect = [k8s_client.ApiException(status=403), None]
        with self.assertRaises(k8s_client.ApiException):
            await handle.release()

        # Exit cleanup still retries a release that started on an attached handle.
        await self.client._delete_automatic_sandboxes()

        # After the failed detach write, every Lease write is a release taking the Lease, not a detach.
        written = [c.args[2] for c in helper.replace_batch_lease.await_args_list[1:]]
        self.assertEqual([lease.spec.holder_identity for lease in written], [handle._holder_identity] * 2)
        helper.delete_batch_lease.assert_awaited_once()
        self.assertEqual(self.client._active_batches, {})

    async def test_a_failed_detach_in_exit_cleanup_is_logged_and_releases_nothing(self):
        handle = _make_handle(self.client)
        handle._attached = True
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
        helper.replace_batch_lease.side_effect = RuntimeError("api down")

        with self.assertLogs(level="ERROR"):
            await self.client._delete_automatic_sandboxes()

        # The detach makes the only Lease write. The handle stays registered so the next cleanup finishes it.
        self.assertEqual([c.args[2].spec.holder_identity for c in helper.replace_batch_lease.call_args_list], [None])
        self.assertEqual(self.client._active_batches, {("default", "b1"): handle})
        helper.delete_sandbox_claims_by_label.assert_not_awaited()

    async def test_delete_all_retries_the_connections_of_a_handle_that_left_the_registry(self):
        handle = _make_handle(self.client)
        cached = MagicMock(close_connection=AsyncMock())
        handle._connected["b1-0"] = cached
        self.client._closing_batches.add(handle)

        await self.client.delete_all()

        cached.close_connection.assert_awaited_once()
        self.assertEqual(self.client._closing_batches, set())

    async def test_cleanup_unregisters_a_handle_whose_batch_another_handle_holds(self):
        handle = _make_handle(self.client)
        cached = MagicMock(close_connection=AsyncMock(side_effect=[RuntimeError("close failed"), None]))
        handle._connected["b1-0"] = cached
        self.client._active_batches[("default", "b1")] = handle
        helper = self.client.k8s_helper
        helper.read_batch_lease.return_value = _lease(holder_identity="other-handle")

        with self.assertLogs(level="WARNING") as logs:
            await self.client.delete_all()
        self.assertEqual(len([r for r in logs.records if "Not releasing batch" in r.getMessage()]), 1)
        self.assertEqual(self.client._active_batches, {})
        self.assertEqual(self.client._closing_batches, {handle})

        await self.client.delete_all()
        # The next cleanup only closes the leftover connection and never retries the release.
        helper.read_batch_lease.assert_awaited_once()
        self.assertEqual(cached.close_connection.await_count, 2)
        self.assertEqual(self.client._closing_batches, set())
        helper.delete_sandbox_claims_by_label.assert_not_awaited()

    async def test_atexit_cleanup_uses_the_sync_helper_and_continues_past_errors(self):
        self.client._active_batches[("ns-a", "b1")] = _make_handle(self.client, batch_id="b1", namespace="ns-a")
        self.client._active_batches[("ns-b", "b2")] = _make_handle(self.client, batch_id="b2", namespace="ns-b")
        lease_a, lease_b = (batch._lease for batch in self.client._active_batches.values())
        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper") as sync_helper, \
             patch("k8s_agent_sandbox.async_sandbox_client._delete_batch_objects_sync") as delete:
            delete.side_effect = [RuntimeError("down"), None]
            self.client._atexit_cleanup()
        holder, duration = "host_1234_abcd1234", BATCH_DEFAULT_LEASE_DURATION_SECONDS
        self.assertEqual(
            [c.args for c in delete.call_args_list],
            [
                (sync_helper.return_value, "ns-a", "b1", "batch-b1", holder, duration, lease_a),
                (sync_helper.return_value, "ns-b", "b2", "batch-b2", holder, duration, lease_b),
            ],
        )

    async def test_close_stops_creator_tasks(self):
        self.cluster.helper.close = AsyncMock()
        await self.claim([BatchGroup(warmpool="pool-a", size=20)], create_rps=10)
        await _wait_until(lambda: len(self.cluster.created) >= 1)
        await self.client.close()
        created = len(self.cluster.created)
        await asyncio.sleep(0.3)
        self.assertEqual(len(self.cluster.created), created)


if __name__ == "__main__":
    unittest.main()
