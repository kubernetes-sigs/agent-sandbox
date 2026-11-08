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
import json
import logging
import socket
import ssl
import threading
import time
import unittest
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

pytest.importorskip("kubernetes_asyncio")

import aiohttp
from kubernetes_asyncio import client as k8s_client

from k8s_agent_sandbox import async_sandbox_batch, batch_state, batch_utils
from k8s_agent_sandbox.async_k8s_helper import AsyncK8sHelper
from k8s_agent_sandbox.async_sandbox_batch import AsyncSandboxBatch
from k8s_agent_sandbox.async_sandbox_client import AsyncSandboxClient
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
from k8s_agent_sandbox.models import BatchEventType, BatchGroup, MemberState, SandboxDirectConnectionConfig


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
    lease_uid="lease-uid",
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
        lease=_lease(holder_identity="host_1234_abcd1234", uid=lease_uid),
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
        self.mock_k8s_helper.api_pool_size = AsyncMock(return_value=None)
        self.mock_k8s_helper.count_live_sandbox_claims = AsyncMock(return_value=0)
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
        self.assertEqual(
            self.mock_k8s_helper.list_sandbox_claim_objects.call_args.args[1],
            f"{BATCH_ID_LABEL}=b1",
        )
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
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(return_value=_lease(holder_identity="h1"))
        handle = await self.client.get_batch("b1")
        self.assertEqual(
            self.mock_k8s_helper.list_sandbox_claim_objects.call_args.args[1],
            f"{BATCH_ID_LABEL}=b1,{BATCH_LEASE_UID_LABEL}=lease-uid",
        )
        self.assertEqual(handle._lease_uid, "lease-uid")
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

    async def test_get_batch_checks_pool_size_and_fences_slow_lease_write(self):
        self.mock_k8s_helper.api_pool_size = AsyncMock(return_value=2)
        with self.assertRaises(ValueError):
            await self.client.get_batch("b1")
        self.mock_k8s_helper.read_batch_lease.assert_not_called()

        self.mock_k8s_helper.api_pool_size = AsyncMock(return_value=8)
        now = datetime.now(UTC)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            side_effect=lambda *_a, **_kw: _lease(
                holder_identity=None,
                renew_time=now,
                lease_duration_seconds=6,
                annotations={BATCH_LEASE_DURATION_ANNOTATION: "6"},
            )
        )
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            return_value=([_claim("b1-0")], "5")
        )
        clock = [100.0]

        async def slow_replace(*_args, **_kwargs):
            clock[0] += 6.0
            return _lease(holder_identity="h1", lease_duration_seconds=6)

        self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=slow_replace)
        with patch(
            "k8s_agent_sandbox.async_sandbox_batch.time.monotonic", side_effect=lambda: clock[0]
        ):
            with self.assertRaises(BatchLeaseExpiredError):
                await self.client.get_batch("b1")
        self.assertEqual(
            self.mock_k8s_helper.replace_batch_lease.call_args_list[0].kwargs["_request_timeout"],
            6.0,
        )

        self.mock_k8s_helper.replace_batch_lease = AsyncMock(
            return_value=_lease(holder_identity="h1", lease_duration_seconds=6)
        )
        attached = await self.client.get_batch("b1")
        self.assertEqual(attached._in_flight._value, 8 - batch_utils.BATCH_POOL_HEADROOM)

    async def test_replace_batch_lease_404_is_handled_across_attach_release_and_detach(self):
        now = datetime.now(UTC)
        not_found = k8s_client.ApiException(status=404, reason="Not Found")
        self.mock_k8s_helper.read_batch_lease = AsyncMock(
            side_effect=lambda *_a, **_kw: _lease(holder_identity=None, renew_time=now)
        )
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=not_found)
        self.mock_k8s_helper.delete_sandbox_claims_by_label = AsyncMock()
        self.mock_k8s_helper.delete_batch_lease = AsyncMock()

        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            side_effect=[([], "5"), ([], "6")]
        )
        with self.assertRaises(BatchNotFoundError):
            await self.client.get_batch("b1")

        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(
            side_effect=[([], "5"), ([_claim("b1-0")], "6")]
        )
        with self.assertRaises(BatchLeaseExpiredError):
            await self.client.get_batch("b1")

        handle_rel = _make_handle(self.client)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=handle_rel._lease)
        await handle_rel.release()
        self.assertTrue(handle_rel._released)
        self.mock_k8s_helper.delete_sandbox_claims_by_label.assert_awaited_once()
        self.mock_k8s_helper.delete_batch_lease.assert_not_awaited()

        handle_det = _make_handle(self.client)
        self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=handle_det._lease)
        await handle_det.detach(grace=30)
        self.assertTrue(handle_det._lease_released)


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

    async def test_connection_is_closed_once_its_members_pod_is_gone(self):
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
            "gone on re-list": (None, True),
        }
        for name, (first_watch, closed) in cases.items():
            with self.subTest(name):
                self.mock_sandbox_class.side_effect = lambda **kwargs: MagicMock(
                    sandbox_id=kwargs["sandbox_id"], close_connection=AsyncMock()
                )
                handle = _make_handle(self.client)
                handle._state.upsert_claim(claim())
                sandbox = await handle.connect(handle.members()[0])
                self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
                    side_effect=[
                        self._terminating_error(status=410) if first_watch is None else _agen(first_watch),
                        self._terminating_error(),
                    ]
                )
                self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(return_value=([], "9"))
                await handle._watch_loop()
                self.assertEqual(sandbox.close_connection.called, closed)
                self.assertEqual("b1-0" in handle._connected, not closed)
                self.assertEqual(handle._stale_connections, [])
                # The 403 that ends the loop sets err(), which connect() would raise first.
                handle._state._error = None
                if handle._state.get_member("b1-0").state is not MemberState.READY:
                    with self.assertRaises(SandboxNotReadyError):
                        await handle.connect(handle.members()[0])

    async def test_a_stale_connection_that_fails_to_close_is_retried_by_the_next_client_cleanup(self):
        def claim(pod_ip):
            return _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "True"}],
                sandbox={"name": "sbx-1", "podIPs": [pod_ip]},
            )

        self.mock_k8s_helper.close = AsyncMock()
        self.mock_sandbox_class.side_effect = lambda **kwargs: MagicMock(
            sandbox_id=kwargs["sandbox_id"], close_connection=AsyncMock()
        )
        handle = _make_handle(self.client)
        handle._state.upsert_claim(claim("10.0.0.1"))
        stale = await handle.connect(handle.members()[0])
        stale.close_connection.side_effect = [RuntimeError("close failed"), None]
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(
            side_effect=[_agen([{"type": "MODIFIED", "object": claim("10.0.0.2")}]), self._terminating_error()]
        )
        await handle._watch_loop()
        self.assertEqual(self.client._closing_batches, {handle})
        # The 403 that ends the loop sets err(), which connect() would raise first.
        handle._state._error = None
        self.assertIsNot(await handle.connect(handle.members()[0]), stale)

        await self.client.close()
        self.assertEqual(stale.close_connection.await_count, 2)
        self.assertEqual(handle._stale_connections, [])

    async def test_a_watch_cancelled_while_closing_stale_connections_keeps_them_for_cleanup(self):
        self.mock_k8s_helper.close = AsyncMock()
        self.mock_sandbox_class.side_effect = lambda **kwargs: MagicMock(
            sandbox_id=kwargs["sandbox_id"], close_connection=AsyncMock()
        )
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=2)])
        for i in range(2):
            handle._state.upsert_claim(
                _claim(
                    f"b1-{i}",
                    conditions=[{"type": "Ready", "status": "True"}],
                    sandbox={"name": f"sbx-{i}", "podIPs": [f"10.0.0.{i}"]},
                )
            )
        sandboxes = [await handle.connect(member) for member in handle.members()]
        closing = asyncio.Event()

        async def close_connection():
            if sandboxes[0].close_connection.await_count == 1:
                closing.set()
                await asyncio.Event().wait()

        sandboxes[0].close_connection.side_effect = close_connection
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(side_effect=[self._terminating_error(status=410)])
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(return_value=([], "9"))
        task = asyncio.create_task(handle._watch_loop())
        await closing.wait()
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task
        self.assertCountEqual(handle._stale_connections, sandboxes)
        self.assertEqual(self.client._closing_batches, {handle})

        await self.client.close()
        self.assertEqual(sandboxes[0].close_connection.await_count, 2)
        sandboxes[1].close_connection.assert_awaited_once()
        self.assertEqual(handle._stale_connections, [])

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
        self.assertEqual(call_args.args[1], f"{BATCH_ID_LABEL}=b1234,{BATCH_LEASE_UID_LABEL}=lease-uid")

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

    async def test_watch_loop_closes_async_stream_on_cancel(self):
        handle = _make_handle(self.client)
        entered = asyncio.Event()
        closed = False

        async def blocking_stream(*args, **kwargs):
            nonlocal closed
            entered.set()
            try:
                await asyncio.Future()
                yield
            finally:
                closed = True

        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(side_effect=blocking_stream)
        handle._watch_task = asyncio.create_task(handle._watch_loop())
        await entered.wait()

        await handle._stop_background_tasks()
        self.assertTrue(closed)
        self.assertIsNone(handle._watch_task)


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
                # A new Lease would get a new UID, so the garbage collector would delete the claims.
                self.mock_k8s_helper.create_batch_lease.assert_not_called()

    async def test_local_lease_expiry_fences_operations_before_renew_loop_wakes(self):
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
                self.mock_k8s_helper.count_live_sandbox_claims.reset_mock()
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
                renew_task = asyncio.create_task(asyncio.sleep(60))
                handle._renew_task = renew_task
                handle._lease_expires_at = time.monotonic() - 0.01
                with self.assertRaises(BatchError) as ctx:
                    await call(handle)
                self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
                self.assertIs(handle.err(), ctx.exception.__cause__)
                self.assertTrue(handle.lease_degraded())
                with self.assertRaises(asyncio.CancelledError):
                    await renew_task
                self.assertEqual(self.mock_k8s_helper.mock_calls, [])
                self.assertEqual(self.mock_k8s_helper.count_live_sandbox_claims.await_count, 0)
                self.mock_sandbox_class.assert_not_called()

    async def test_local_lease_expiry_mid_acquire_and_quorum_wait_fences_handle(self):
        with self.subTest("pacing_wait"):
            self.mock_k8s_helper.create_sandbox_claim = AsyncMock()
            self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
            handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
            handle._warm_credit.record("pool-a", 0)
            handle._next_create_at = time.monotonic() + 10.0

            async def expire_during_pacing(_timeout):
                handle._lease_expires_at = time.monotonic() - 0.01
                return False

            with patch.object(handle, "_wait_for_stop", side_effect=expire_during_pacing):
                with self.assertRaises(BatchError) as ctx:
                    await handle.acquire("pool-a", timeout=60)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
            self.mock_k8s_helper.create_sandbox_claim.assert_not_awaited()
            self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

        with self.subTest("ready_arrived_before_wait_acquired"):
            handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=0)])
            handle._warm_credit.record("pool-a", 1)

            async def create_then_expire(claim_name, warmpool, *_args, **_kwargs):
                handle._state.upsert_claim(
                    _claim(
                        claim_name,
                        warmpool=warmpool,
                        conditions=[{"type": "Ready", "status": "True"}],
                        sandbox={"name": "sbx-0", "podIPs": ["10.0.0.1"]},
                    )
                )
                handle._lease_expires_at = time.monotonic() - 0.01

            self.mock_k8s_helper.create_sandbox_claim = AsyncMock(side_effect=create_then_expire)
            self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
            with self.assertRaises(BatchError) as ctx:
                await handle.acquire("pool-a", timeout=60)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
            self.mock_k8s_helper.create_sandbox_claim.assert_awaited_once()
            self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

        with self.subTest("sleeping_wait_acquired_and_quorum"):
            self.mock_k8s_helper.create_sandbox_claim = AsyncMock()
            self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
            handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=1)])
            handle._warm_credit.record("pool-a", 1)
            handle._lease_expires_at = time.monotonic() + 0.03
            started = time.monotonic()
            with self.assertRaises(BatchError) as ctx:
                await handle.acquire("pool-a", timeout=60)
            self.assertLess(time.monotonic() - started, 2.0)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)
            self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()

            quorum_handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=1)])
            quorum_handle._lease_expires_at = time.monotonic() + 0.03
            started = time.monotonic()
            with self.assertRaises(BatchError) as ctx:
                await quorum_handle.wait_for_quorum()
            self.assertLess(time.monotonic() - started, 2.0)
            self.assertIsInstance(ctx.exception.__cause__, BatchLeaseExpiredError)

    async def test_late_renewal_after_local_expiry_keeps_lease_degraded_and_error(self):
        handle = _make_handle(self.client)
        expired_deadline: list[float] = []

        async def replace_after_local_fence(*_args, **_kwargs):
            handle._lease_expires_at = time.monotonic() - 0.01
            expired_deadline.append(handle._lease_expires_at)
            with self.assertRaises(BatchError):
                await handle._check_lease_held()
            return _lease(holder_identity=handle._holder_identity, resource_version="99")

        self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=replace_after_local_fence)
        self.assertFalse(await handle._renew_once())
        self.assertTrue(handle.lease_degraded())
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)
        self.assertEqual(handle._lease_expires_at, expired_deadline[0])

    async def test_remaining_lease_budget_caps_claim_and_pool_request_timeouts(self):
        handle = _make_handle(
            self.client,
            groups=[BatchGroup(warmpool="pool-a", size=0)],
            lease_duration=6,
        )
        handle._lease_expires_at = time.monotonic() + 4.0
        self.mock_k8s_helper.get_sandbox_warmpool = AsyncMock(
            return_value={
                "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
                "status": {"readyReplicas": 1},
            }
        )
        self.mock_k8s_helper.get_sandbox_template = AsyncMock(
            return_value={"metadata": {"name": "tmpl"}}
        )

        async def create_ready(claim_name, warmpool, *_args, **_kwargs):
            async with handle._lock:
                handle._state.upsert_claim(
                    _claim(
                        claim_name,
                        warmpool=warmpool,
                        conditions=[{"type": "Ready", "status": "True"}],
                        sandbox={"name": f"sbx-{claim_name}", "podIPs": ["10.0.0.1"]},
                    )
                )
                handle._wake_acquire(claim_name)

        self.mock_k8s_helper.create_sandbox_claim = AsyncMock(side_effect=create_ready)
        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        member = await handle.acquire("pool-unseen", timeout=60)
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

        await handle.release_member(member)
        delete_timeout = self.mock_k8s_helper.delete_sandbox_claim.call_args.kwargs["_request_timeout"]
        self.assertGreater(delete_timeout, 0)
        self.assertLessEqual(delete_timeout, 4.0)

    async def test_watch_error_keeps_lease_renewal_and_cleanup_while_lease_loss_stops_watch(self):
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
        self.mock_k8s_helper.watch_sandbox_claims = MagicMock(side_effect=watch_error)
        await handle._watch_loop()
        self.assertIs(handle.err(), watch_error)
        self.assertFalse(handle.lease_degraded())
        self.assertFalse(handle._stop_creating.is_set())
        with self.assertRaises(BatchError) as cm:
            await handle.connect(first)
        self.assertIs(cm.exception.__cause__, watch_error)

        self.mock_k8s_helper.replace_batch_lease = AsyncMock(return_value=_lease(resource_version="25"))
        self.assertTrue(await handle._renew_once())
        self.assertEqual(handle._lease.metadata.resource_version, "25")

        self.mock_k8s_helper.delete_sandbox_claim = AsyncMock()
        await handle.release_member(first)
        self.mock_k8s_helper.delete_sandbox_claim.assert_awaited_once()
        self.assertEqual(self.mock_k8s_helper.delete_sandbox_claim.call_args.args[0], "b1-0")

        watch_task = asyncio.create_task(asyncio.Event().wait())
        handle._watch_task = watch_task
        self.mock_k8s_helper.delete_sandbox_claim.reset_mock()
        self.mock_k8s_helper.replace_batch_lease = AsyncMock(side_effect=k8s_client.ApiException(status=404))
        self.assertFalse(await handle._renew_once())
        self.assertIsInstance(handle.err(), BatchLeaseExpiredError)
        self.assertTrue(handle._stop_creating.is_set())
        with self.assertRaises(asyncio.CancelledError):
            await watch_task
        with self.assertRaises(BatchError):
            await handle.release_member(second)
        self.mock_k8s_helper.delete_sandbox_claim.assert_not_awaited()


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
            _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "True"}],
                sandbox={"name": "sbx-1", "podIPs": ["10.0.0.1"]},
            )
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
        self.assertEqual(sandbox._seeded_pod_ip, "10.0.0.1")

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
        # The batch's one claim took ordinal 0, so a re-attached handle continues from 1.
        self.assertEqual(
            written.metadata.annotations,
            {
                BATCH_LEASE_DURATION_ANNOTATION: str(BATCH_DEFAULT_LEASE_DURATION_SECONDS),
                BATCH_NEXT_ORDINAL_ANNOTATION: "1",
            },
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
        self.handle._connected["b1-0"] = (mock_sandbox, None)

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
                self.assertEqual(written.metadata.annotations[BATCH_NEXT_ORDINAL_ANNOTATION], "1")

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
        self.handle._connected["b1-0"] = (bad_sandbox, None)

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
        self.handle._connected["b1-0"] = (cached, None)

        task = asyncio.create_task(self.handle._close_cached_connections("detach"))
        await asyncio.sleep(0.01)
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

        self.assertEqual(self.handle._connected, {"b1-0": (cached, None)})
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
        handle._connected["b1-0"] = (cached, None)
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
                handle._connected["b1-0"] = (cached, None)
                self.client._active_batches[("default", "b1")] = handle

                with contextlib.suppress(asyncio.CancelledError):
                    await self.client.close()
                self.assertEqual(handle._connected, {"b1-0": (cached, None)})
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
                old._connected["b1-0"] = (cached, None)
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
        handle._connected["b1-0"] = (cached, None)
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
        handle._connected["b1-0"] = (cached, None)
        stale = MagicMock()
        handle._stale_connections.append(stale)
        self.client._active_batches[("default", "b1")] = handle
        closing = _make_handle(self.client)
        closing_cached = MagicMock()
        closing._connected["b1-0"] = (closing_cached, None)
        closing_stale = MagicMock()
        closing._stale_connections.append(closing_stale)
        self.client._closing_batches.add(closing)
        half_detached = _make_handle(self.client, batch_id="b2")
        half_detached._detach_grace = 30
        self.client._active_batches[("default", "b2")] = half_detached
        attached = _make_handle(self.client, batch_id="b3")
        attached._attached = True
        attached_cached = MagicMock()
        attached._connected["b3-0"] = (attached_cached, None)
        self.client._active_batches[("default", "b3")] = attached

        with patch("k8s_agent_sandbox.async_sandbox_client.K8sHelper"), \
             patch("k8s_agent_sandbox.async_sandbox_client._delete_batch_objects_sync") as delete:
            self.client._atexit_cleanup()

        for sandbox in (cached, stale, closing_cached, closing_stale, attached_cached):
            sandbox._close_for_atexit.assert_called_once()
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
# Fill claims carry their group's annotations. The batch state only checks that they are there.
_FILL = {BATCH_GROUP_SIZE_ANNOTATION: "1", BATCH_GROUP_MIN_READY_ANNOTATION: "1"}


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
        h.count_live_sandbox_claims = AsyncMock(return_value=0)
        h.api_pool_size = AsyncMock(return_value=None)
        h.watch_sandbox_claims = MagicMock(side_effect=self._watch)
        h.create_sandbox_claim = AsyncMock(side_effect=self._create)
        h.delete_sandbox_claims_by_label = AsyncMock(side_effect=lambda *a, **k: self.calls.append("deletecollection"))
        h.delete_batch_lease = AsyncMock(side_effect=lambda *a, **k: self.calls.append("delete_lease"))
        h.delete_sandbox_claim = AsyncMock()
        # A Lease no handle holds, so release() takes it for its handle.
        h.read_batch_lease = AsyncMock(return_value=_lease())
        h.replace_batch_lease = AsyncMock(return_value=_lease())

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
            self.push(name, warmpool, annotations=kwargs["annotations"])

    def push(self, name, warmpool="pool-a", ready=True, annotations=_FILL):
        conditions = _READY if ready else _NOT_READY
        sandbox = {"name": f"sbx-{name}"} if ready else None
        claim = _claim(name, warmpool, conditions=conditions, sandbox=sandbox, annotations=annotations)
        self.watch_queue.put_nowait({"type": "MODIFIED", "object": claim})

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


async def _drain(aiterable, timeout=5.0):
    """Collects an async iterator, failing instead of hanging if it doesn't end."""
    async def collect():
        return [item async for item in aiterable]

    try:
        return await asyncio.wait_for(collect(), timeout)
    except asyncio.TimeoutError:
        raise AssertionError("iterator did not end in time") from None


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
        helper.create_batch_lease.side_effect = lambda *a, **k: self.cluster.calls.append("lease") or _lease()
        helper.list_sandbox_claim_objects.side_effect = (
            lambda *a, **k: self.cluster.calls.append("list") or ([], "7")
        )
        batch = await self.claim(
            [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
            labels={"team": "rl"},
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

    async def test_precheck_failure_writes_nothing(self):
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
                helper.api_pool_size = AsyncMock(return_value=None)
                helper.get_sandbox_warmpool = AsyncMock(
                    return_value={"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
                )
                helper.get_sandbox_template = AsyncMock(return_value={})
                helper.create_batch_lease.reset_mock()
                if isinstance(result, Exception):
                    getattr(helper, method).side_effect = result
                else:
                    getattr(helper, method).return_value = result
                with self.assertRaises(error):
                    await self.client.claim_batch([BatchGroup(warmpool="pool-a", size=1)], **kwargs)
                if method == "api_pool_size":
                    helper.get_sandbox_warmpool.assert_not_called()
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
                helper.create_batch_lease.return_value = _lease(resource_version="77")
                if isinstance(list_result, BaseException):
                    helper.list_sandbox_claim_objects.side_effect = list_result
                else:
                    helper.list_sandbox_claim_objects.side_effect = None
                    helper.list_sandbox_claim_objects.return_value = list_result
                with self.assertRaises(error):
                    await self.claim()
                self.assertEqual(helper.delete_batch_lease.call_args.args[:2], ("batch-b1", "default"))
                self.assertEqual(helper.delete_batch_lease.call_args.kwargs["resource_version"], "77")
        self.assertEqual(self.cluster.created, [])

    async def test_setup_list_deadline_and_fenced_rollback_against_replacement_lease(self):
        from k8s_agent_sandbox.async_k8s_helper import AsyncK8sHelper

        helper = self.cluster.helper
        helper.create_batch_lease.return_value = _lease(resource_version="77")

        real_helper = AsyncK8sHelper()
        real_helper._initialized = True
        real_helper.coordination_v1_api = MagicMock()
        real_helper.coordination_v1_api.delete_namespaced_lease = AsyncMock(
            side_effect=k8s_client.ApiException(status=409)
        )
        helper.delete_batch_lease.side_effect = real_helper.delete_batch_lease

        conn_error = aiohttp.ClientConnectionError("connection dropped")
        for label, exc, ticks, expected_error in (
            ("timeout before lease expiry", TimeoutError("page timed out"), [100.0, 120.0], TimeoutError),
            ("transport error before lease expiry", conn_error, [100.0, 120.0], aiohttp.ClientConnectionError),
            (
                "deadline timeout at lease expiry",
                TimeoutError("SandboxClaim list exceeded its deadline"),
                [100.0, 145.0],
                BatchLeaseExpiredError,
            ),
            ("transport error at lease expiry", conn_error, [100.0, 145.0], BatchLeaseExpiredError),
        ):
            with self.subTest(label):
                real_helper.coordination_v1_api.delete_namespaced_lease.reset_mock()
                helper.list_sandbox_claim_objects.side_effect = exc
                with patch("k8s_agent_sandbox.async_sandbox_batch.time.monotonic", side_effect=ticks):
                    with self.assertRaises(expected_error) as raised:
                        await self.claim(lease_duration=45)
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

    async def test_setup_lease_expiry_after_list_rolls_back_and_aborts(self):
        helper = self.cluster.helper
        helper.create_batch_lease.return_value = _lease(resource_version="77")
        helper.list_sandbox_claim_objects.side_effect = None
        helper.list_sandbox_claim_objects.return_value = ([], "7")

        with patch("k8s_agent_sandbox.async_sandbox_batch.time.monotonic", side_effect=[100.0, 145.0]):
            with self.assertRaises(BatchLeaseExpiredError):
                await self.claim(lease_duration=45)

        self.assertEqual(helper.list_sandbox_claim_objects.call_args.kwargs["deadline"], 145.0)
        helper.delete_batch_lease.assert_awaited_once()
        self.assertEqual(helper.delete_batch_lease.call_args.args[:2], ("batch-b1", "default"))
        self.assertEqual(helper.delete_batch_lease.call_args.kwargs["resource_version"], "77")
        self.assertEqual(self.client._active_batches, {})
        helper.watch_sandbox_claims.assert_not_called()
        helper.replace_batch_lease.assert_not_called()
        helper.create_sandbox_claim.assert_not_called()

    async def test_setup_validates_created_lease_resource_version_and_uid(self):
        helper = self.cluster.helper
        for rv in (None, ""):
            with self.subTest(missing_rv=rv):
                helper.delete_batch_lease.reset_mock()
                helper.list_sandbox_claim_objects.reset_mock()
                helper.create_batch_lease.return_value = _lease(resource_version=rv)
                with self.assertRaisesRegex(BatchError, "no resourceVersion"):
                    await self.claim()
                helper.delete_batch_lease.assert_not_awaited()
                helper.list_sandbox_claim_objects.assert_not_awaited()

        for uid in (None, ""):
            with self.subTest(missing_uid=uid):
                helper.delete_batch_lease.reset_mock()
                helper.list_sandbox_claim_objects.reset_mock()
                helper.create_batch_lease.return_value = _lease(resource_version="77", uid=uid)
                with self.assertRaisesRegex(BatchError, "no name or UID"):
                    await self.claim()
                helper.list_sandbox_claim_objects.assert_not_awaited()
                helper.delete_batch_lease.assert_awaited_once()
                self.assertEqual(helper.delete_batch_lease.call_args.kwargs["resource_version"], "77")


class TestClaimBatchCreation(BaseClaimBatchTest):

    async def test_creates_are_bounded_by_max_in_flight_and_paced_by_create_rps(self):
        for name, batch_id, pool_size, kwargs in (
            ("explicit", "b1", None, {"max_in_flight": 2}),
            ("clamped to pool_size - 2", "b2", 4, {}),
        ):
            with self.subTest(name):
                self.cluster.created.clear()
                self.cluster.helper.create_sandbox_claim.reset_mock()
                self.cluster.helper.api_pool_size = AsyncMock(return_value=pool_size)
                gate = asyncio.Event()
                self.addCleanup(gate.set)
                self.cluster.create_hook = lambda _: gate.wait()
                log_check = self.assertNoLogs if pool_size is None else self.assertLogs
                with log_check("k8s_agent_sandbox.batch_utils", level="INFO"):
                    await self.claim(
                        [BatchGroup(warmpool="pool-a", size=5)], batch_id=batch_id, **kwargs
                    )
                await _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count >= 2)
                await asyncio.sleep(0.2)
                self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 2)
                gate.set()
                await _wait_until(lambda: len(self.cluster.created) == 5)

        self.cluster.helper.api_pool_size = AsyncMock(return_value=None)
        self.cluster.create_hook = None
        self.cluster.created.clear()
        start = time.monotonic()
        await self.claim([BatchGroup(warmpool="pool-a", size=5)], batch_id="b3", create_rps=20)
        await _wait_until(lambda: len(self.cluster.created) == 5)
        starts = sorted(t for _, t in self.cluster.created)
        for k, t in enumerate(starts):
            self.assertGreaterEqual(t - start, k * 0.05 - 0.001)

    async def test_creates_the_warm_pool_covers_skip_pacing(self):
        ready = {"pool-a": 5, "pool-b": 0}
        self.cluster.helper.get_sandbox_warmpool.side_effect = lambda name, *a, **k: {
            "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
            "status": {"readyReplicas": ready[name]},
        }
        start = time.monotonic()
        await self.claim(
            [BatchGroup(warmpool="pool-a", size=3), BatchGroup(warmpool="pool-b", size=3)],
            max_in_flight=6,
            create_rps=4,
        )
        await _wait_until(lambda: len(self.cluster.created) == 6)
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
        mixed = await self.claim(
            [BatchGroup(warmpool="pool-b", size=3, min_ready=1), BatchGroup(warmpool="pool-a", size=2, min_ready=1)],
            batch_id="b2",
            max_in_flight=5,
            create_rps=0.5,
            quorum_timeout=10,
        )
        await _wait_until(lambda: mixed._fill_deadline is not None)
        await mixed._stop_background_tasks()
        self.assertGreaterEqual(mixed._last_fill_slot - start, 3.9)
        self.assertEqual(mixed._fill_deadline, mixed._last_fill_slot + 10)

        async def noop_worker(_self):
            return

        with patch.object(AsyncSandboxBatch, "_create_worker", noop_worker):
            dropped = await self.claim(
                [BatchGroup(warmpool="pool-b", size=2, min_ready=1)],
                batch_id="b3",
                max_in_flight=1,
                create_rps=0.5,
                quorum_timeout=10,
            )
        async with dropped._lock:
            now = time.monotonic()
            dropped._last_fill_slot = now + 2.0
            dropped._next_create_at = now + 20.0
            dropped._drop_planned_creates("pool-b")
        self.assertEqual(dropped._fill_deadline, dropped._last_fill_slot + 10)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_first_429_paces_creates_during_cooldown_and_recovers(self, _):
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
                await self.claim(
                    [BatchGroup(warmpool="pool-a", size=4)], batch_id=f"b{status}", max_in_flight=1, create_rps=5
                )
                await _wait_until(lambda: len(sent_at) == 5)
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
            await self.claim(
                [BatchGroup(warmpool="pool-a", size=8)], batch_id="brecover", max_in_flight=1, create_rps=5
            )
            await _wait_until(lambda: len(sent_at) == 9)
        gaps = [later - earlier for earlier, later in zip(sent_at[1:], sent_at[2:])]
        # Claim 0 took credit 1 and its retry reserved t+0; claim 1 paced at t+0.2 during cooldown
        # (spending credit 2) and advanced offset past cooldown; claims 2..5 use the 4 remaining
        # credits unpaced; claims 6..7 pace again once credit hits 0.
        self.assertGreaterEqual(gaps[0], 0.18)
        self.assertTrue(all(gap < 0.1 for gap in gaps[1:5]), gaps)
        self.assertTrue(all(gap >= 0.18 for gap in gaps[5:]), gaps)

    async def test_cold_claim_on_watch_zeroes_pool_credit_for_remaining_fill_creates(self):
        for reason in ("SandboxCreatePending", "SandboxNotReady"):
            with self.subTest(reason):
                self.cluster = _Cluster(ready_on_create=False)
                self.client.k8s_helper = self.cluster.helper
                self.cluster.helper.get_sandbox_warmpool.side_effect = lambda name, *a, **k: {
                    "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
                    "status": {"readyReplicas": 10},
                }
                batch_ref: dict[str, AsyncSandboxBatch] = {}
                started = asyncio.Event()
                self.addCleanup(started.set)

                async def hook(name):
                    if name == "b1-0":
                        await asyncio.wait_for(started.wait(), 5)
                        cold = [{"type": "Ready", "status": "False", "reason": reason}]
                        self.cluster.watch_queue.put_nowait(
                            {"type": "ADDED", "object": _claim("b1-0", "pool-a", conditions=cold, annotations=_FILL)}
                        )
                        await _wait_until(lambda: len(batch_ref["batch"].members()) == 1)

                self.cluster.create_hook = hook
                batch = batch_ref["batch"] = await self.claim(
                    [BatchGroup(warmpool="pool-a", size=4), BatchGroup(warmpool="pool-b", size=2)],
                    max_in_flight=1,
                    create_rps=5,
                )
                self.addAsyncCleanup(batch._stop_background_tasks)
                started.set()
                await _wait_until(lambda: len(self.cluster.created) == 6)
                sent_at = dict(self.cluster.created)
                # b1-0 arrived cold on the watch, zeroing pool-a's credit so b1-1 takes the
                # first pacing slot and b1-2 and b1-3 pace at 5/s, while pool-b keeps its credit.
                self.assertGreaterEqual(sent_at["b1-2"] - sent_at["b1-1"], 0.18)
                self.assertGreaterEqual(sent_at["b1-3"] - sent_at["b1-2"], 0.18)
                self.assertLess(sent_at["b1-4"] - sent_at["b1-3"], 0.1)
                self.assertLess(sent_at["b1-5"] - sent_at["b1-4"], 0.1)
                await batch._stop_background_tasks()

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
                await handle._create_one("b1-0", "pool-a", {}, {}, {})
                self.assertEqual(self.cluster.helper.create_sandbox_claim.call_count, 2)
                self.assertTrue(earliest <= next_free_slot[0] < latest, next_free_slot)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=10)
    async def test_a_backoff_retry_is_not_paced_past_the_deadline(self, _):
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
                self.cluster.helper.create_sandbox_claim.side_effect = [aiohttp.ClientConnectionError("boom"), None]
                deadline = time.monotonic() + deadline_in
                waits = []

                async def wait(timeout):
                    waits.append(timeout)
                    if slots_taken_during_backoff:
                        handle._next_create_at = deadline
                    return False

                handle._wait_for_stop = wait
                with self.assertRaises(BatchTimeoutError):
                    await handle._create_one("b1-0", "pool-a", {}, {}, {}, deadline=deadline)
                self.assertEqual(len(waits), waits_before_raising)
                self.cluster.helper.create_sandbox_claim.assert_called_once()

    async def test_failed_group_stops_creates_only_in_group_mode(self):
        for mode in ("group", "stream"):
            with self.subTest(mode):
                self.setUp()
                gate = asyncio.Event()

                async def hook(name):
                    if name == "b1-0":
                        await gate.wait()
                        raise k8s_client.ApiException(status=403)

                self.cluster.create_hook = hook
                batch = await self.claim(
                    [BatchGroup(warmpool="pool-a", size=3), BatchGroup(warmpool="pool-b", size=2)],
                    max_in_flight=1,
                )
                consume = batch.group_outcomes if mode == "group" else batch.events
                items = consume()
                gate.set()
                await _drain(items)
                if mode == "group":
                    self.assertEqual(self.cluster.names(), ["b1-3", "b1-4"])
                else:
                    self.assertEqual(self.cluster.names(), ["b1-1", "b1-2", "b1-3", "b1-4"])


class TestConsumers(BaseClaimBatchTest):

    async def test_events_end_on_settle_and_a_second_call_continues(self):
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=2)])
        first = await anext(batch.events())
        rest = await _drain(batch.events())
        self.assertEqual(
            sorted((e.type, e.member.claim_name) for e in [first, *rest]),
            [(BatchEventType.MEMBER_READY, "b1-0"), (BatchEventType.MEMBER_READY, "b1-1")],
        )

    async def test_events_end_when_a_ready_member_stops_being_ready_after_the_fill_settles(self):
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)], quorum_timeout=600)
        events = batch.events()
        self.assertEqual((await asyncio.wait_for(anext(events), 5)).member.claim_name, "b1-0")
        self.cluster.push("b1-0", ready=False)
        await _wait_until(lambda: batch._state.get_member("b1-0").state is MemberState.PENDING)
        self.assertEqual(await _drain(events), [])

    async def test_groups_yield_when_ready_and_time_out_after_the_last_create(self):
        self.cluster.ready_on_create = False
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(async_sandbox_batch, "time", fake_time):
            batch = await self.claim(
                [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
                quorum_timeout=60,
                lease_duration=120,
            )
            await _wait_until(lambda: len(self.cluster.created) == 2)
            self.assertAlmostEqual(batch._fill_deadline - self.cluster.created[-1][1], 60, delta=0.5)
            groups = batch.group_outcomes()
            self.cluster.push("b1-0", "pool-a")
            first = await asyncio.wait_for(anext(groups), 5)
            self.assertEqual((first.warmpool, [m.claim_name for m in first.members]), ("pool-a", ["b1-0"]))

            offset[0] = 61
            self.cluster.push("b1-1", "pool-b", ready=False)
            [second] = await _drain(groups)
        self.assertEqual(second.warmpool, "pool-b")
        self.assertIsInstance(second.error, BatchTimeoutError)

    async def test_release_wakes_a_waiting_consumer_with_batch_error(self):
        self.cluster.ready_on_create = False
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        consumer = asyncio.ensure_future(_drain(batch.events()))
        await asyncio.sleep(0.1)
        await batch.release()
        with self.assertRaises(BatchError):
            await consumer

    async def test_client_close_wakes_a_waiting_consumer_with_batch_error(self):
        self.cluster.ready_on_create = False
        self.cluster.helper.close = AsyncMock()
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        consumer = asyncio.ensure_future(_drain(batch.events()))
        await asyncio.sleep(0.1)
        await self.client.close()
        with self.assertRaises(BatchError):
            await asyncio.wait_for(consumer, 5)

    async def test_iterators_end_when_the_watch_fails(self):
        self.cluster.ready_on_create = False
        self.cluster.helper.watch_sandbox_claims.side_effect = k8s_client.ApiException(status=403)
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        self.assertEqual(await _drain(batch.group_outcomes()), [])
        self.assertEqual(await _drain(batch.events()), [])
        self.assertEqual(batch.err().status, 403)


class TestWaitForQuorum(BaseClaimBatchTest):

    async def wait_in_background(self, batch, **kwargs):
        task = asyncio.ensure_future(asyncio.wait_for(batch.wait_for_quorum(**kwargs), 5))
        await _wait_until(lambda: batch._state._mode == batch_state.ConsumerMode.QUORUM)
        return task

    async def test_returns_min_ready_per_group_in_group_order_then_events_yield_the_rest(self):
        self.cluster.ready_on_create = False
        batch = await self.claim(
            [BatchGroup(warmpool="pool-a", size=2, min_ready=1), BatchGroup(warmpool="pool-b", size=2, min_ready=1)]
        )
        await _wait_until(lambda: len(self.cluster.created) == 4)
        for name, warmpool in (("b1-3", "pool-b"), ("b1-1", "pool-a"), ("b1-2", "pool-b"), ("b1-0", "pool-a")):
            self.cluster.push(name, warmpool)
        self.assertEqual([m.claim_name for m in await batch.wait_for_quorum()], ["b1-1", "b1-3"])
        self.assertEqual(
            sorted((e.type, e.member.claim_name) for e in await _drain(batch.events())),
            [(BatchEventType.MEMBER_READY, "b1-0"), (BatchEventType.MEMBER_READY, "b1-2")],
        )

    async def test_quorum_timeout_raises_batch_timeout_error_and_stops_the_remaining_creates(self):
        self.cluster.ready_on_create = False
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(async_sandbox_batch, "time", fake_time):
            # At one create per second, b1-1 still waits for its pacing slot when the deadline passes.
            batch = await self.claim(
                [BatchGroup(warmpool="pool-a", size=2)],
                create_rps=1,
                quorum_timeout=60,
                lease_duration=120,
            )
            await _wait_until(lambda: batch._fill_deadline is not None)
            task = await self.wait_in_background(batch)
            offset[0] = 62
            self.cluster.push("b1-0", ready=False)
            with self.assertRaises(BatchTimeoutError):
                await task
            await _wait_until(lambda: batch._state._never_created == 1)
        self.assertEqual(self.cluster.names(), ["b1-0"])

    async def test_an_unreachable_group_raises_and_stops_creates_for_every_group(self):
        gate = asyncio.Event()

        async def hook(name):
            if name == "b1-0":
                await gate.wait()
                raise k8s_client.ApiException(status=403)

        self.cluster.create_hook = hook
        batch = await self.claim(
            [BatchGroup(warmpool="pool-a", size=3), BatchGroup(warmpool="pool-b", size=2)], max_in_flight=1
        )
        task = await self.wait_in_background(batch)
        gate.set()
        with self.assertRaises(QuorumUnreachableError) as raised:
            await task
        self.assertEqual(raised.exception.warmpool, "pool-a")
        self.assertEqual([e.type for e in await _drain(batch.events())], [BatchEventType.MEMBER_FAILED])
        self.assertEqual(self.cluster.names(), [])

    async def test_a_cancelled_wait_fails_the_quorum(self):
        gate = asyncio.Event()

        async def hook(name):
            if name == "b1-0":
                await gate.wait()

        self.cluster.create_hook = hook
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=2)], max_in_flight=1)
        task = await self.wait_in_background(batch)
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        gate.set()
        # The quorum failed, so b1-0 becoming Ready isn't handed out and b1-1 is never created.
        self.assertEqual(await _drain(batch.events()), [])
        self.assertEqual(self.cluster.names(), ["b1-0"])

    async def test_release_or_a_failed_watch_ends_the_wait_with_batch_error(self):
        self.cluster.ready_on_create = False
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        task = await self.wait_in_background(batch)
        await batch.release()
        with self.assertRaises(BatchError):
            await task

        self.cluster.helper.watch_sandbox_claims.side_effect = k8s_client.ApiException(status=403)
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)], batch_id="b2")
        with self.assertRaises(BatchError) as raised:
            await batch.wait_for_quorum()
        self.assertEqual(raised.exception.__cause__.status, 403)

    async def test_reattached_batch_at_min_ready_returns_at_once(self):
        annotations = {BATCH_GROUP_SIZE_ANNOTATION: "2", BATCH_GROUP_MIN_READY_ANNOTATION: "1"}
        self.cluster.helper.list_sandbox_claim_objects.return_value = (
            [
                _claim("b1-0", conditions=_NOT_READY, annotations=annotations),
                _claim("b1-1", conditions=_READY, sandbox={"name": "sbx-b1-1"}, annotations=annotations),
            ],
            "5",
        )
        self.cluster.helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC))
        batch = await self.client.get_batch("b1")
        self.addAsyncCleanup(batch._stop_background_tasks)
        self.assertEqual([m.claim_name for m in await batch.wait_for_quorum()], ["b1-1"])


class TestRelease(BaseClaimBatchTest):

    async def test_release_stops_creates_then_deletes_claims_then_the_lease(self):
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=20)], create_rps=10)
        await _wait_until(lambda: len(self.cluster.created) >= 1)
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
        with self.assertRaises(BatchError):
            batch.events()

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
                helper.delete_sandbox_claims_by_label = AsyncMock(side_effect=delete_effects.get("delete_sandbox_claims_by_label"))
                helper.count_live_sandbox_claims = AsyncMock(
                    side_effect=count_effects.get("count_live_sandbox_claims", [0] * 4)
                )
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
        create_kwargs = helper.create_sandbox_claim.call_args.kwargs
        self.assertEqual((create_kwargs["_request_timeout"], create_kwargs["batch"]), (30, True))
        self.assertEqual(helper.delete_sandbox_claims_by_label.call_args.kwargs["_request_timeout"], 90)
        self.assertEqual(helper.count_live_sandbox_claims.call_args.kwargs["_request_timeout"], 90)
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
                helper.count_live_sandbox_claims.side_effect = [1, 0]
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
                helper.count_live_sandbox_claims.side_effect = [1, 0, 0]
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

    async def test_stalled_release_and_release_group_scope_deletes_to_lease_uid(self):
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

        async def delete_by_label(namespace, selector, **kwargs):
            for name in [n for n, lbl in store.items() if matches(lbl, selector)]:
                del store[name]

        async def count_live(namespace, selector, **kwargs):
            return sum(1 for lbl in store.values() if matches(lbl, selector))

        def claim_labels(uid, group_hash):
            return {BATCH_ID_LABEL: "b1", BATCH_LEASE_UID_LABEL: uid, BATCH_GROUP_LABEL: group_hash}

        with self.subTest("release after old Lease was reaped"):
            store = {
                "b1-0": claim_labels("uid-1", pool_a),
                "b1-new-0": claim_labels("uid-2", pool_a),
            }
            helper.delete_sandbox_claims_by_label = AsyncMock(side_effect=delete_by_label)
            helper.count_live_sandbox_claims = AsyncMock(side_effect=count_live)
            helper.read_batch_lease = AsyncMock(return_value=None)
            helper.delete_batch_lease = AsyncMock()
            handle = _make_handle(self.client, lease_uid="uid-1")
            await handle.release()
            self.assertEqual(set(store), {"b1-new-0"})
            helper.delete_batch_lease.assert_not_called()

        with self.subTest("round 1 deletecollection lands after uid-2 claims exist"):
            store = {"b1-0": claim_labels("uid-1", pool_a)}
            current_lease = _lease(holder_identity="host_1234_abcd1234", uid="uid-1")
            helper.read_batch_lease = AsyncMock(side_effect=lambda *a, **k: current_lease)
            helper.replace_batch_lease = AsyncMock(
                side_effect=lambda *a, **k: _lease(
                    holder_identity=a[2].spec.holder_identity, uid="uid-1", resource_version="11"
                )
            )
            helper.count_live_sandbox_claims = AsyncMock(side_effect=count_live)
            helper.delete_batch_lease = AsyncMock()

            async def delayed_round1_delete(namespace, selector, **kwargs):
                nonlocal current_lease
                # While Round 1's deletecollection is in flight, uid-1 is reaped and batch uid-2 starts.
                current_lease = _lease(
                    holder_identity="holder-2", uid="uid-2", renew_time=datetime.now(UTC)
                )
                store["b1-new-0"] = claim_labels("uid-2", pool_a)
                await delete_by_label(namespace, selector, **kwargs)
                # Simulate one uid-1 claim not yet stamped with deletionTimestamp when 504 cuts Round 1 short.
                store["b1-1"] = claim_labels("uid-1", pool_a)
                raise k8s_client.ApiException(status=504)

            helper.delete_sandbox_claims_by_label = AsyncMock(side_effect=delayed_round1_delete)
            handle = _make_handle(self.client, lease_uid="uid-1")
            with patch.object(batch_utils, "retry_delay", return_value=0):
                with self.assertRaises(BatchInUseError):
                    await handle.release()
            self.assertEqual(set(store), {"b1-1", "b1-new-0"})
            helper.delete_batch_lease.assert_not_called()

        with self.subTest("release_group scopes deletecollection to lease uid"):
            store = {
                "b1-0": claim_labels("uid-1", pool_a),
                "b1-1": claim_labels("uid-1", pool_b),
                "b1-new-0": claim_labels("uid-2", pool_a),
            }
            helper.delete_sandbox_claims_by_label = AsyncMock(side_effect=delete_by_label)
            helper.count_live_sandbox_claims = AsyncMock(side_effect=count_live)
            handle = _make_handle(
                self.client,
                groups=[BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
                lease_uid="uid-1",
            )
            await handle.release_group("pool-a")
            self.assertEqual(set(store), {"b1-1", "b1-new-0"})

        for rv in (None, ""):
            with self.subTest(missing_written_rv=rv):
                helper.read_batch_lease = AsyncMock(
                    return_value=_lease(holder_identity="host_1234_abcd1234", uid="uid-1")
                )
                helper.replace_batch_lease = AsyncMock(
                    return_value=_lease(holder_identity="host_1234_abcd1234", uid="uid-1", resource_version=rv)
                )
                helper.delete_sandbox_claims_by_label = AsyncMock()
                helper.delete_batch_lease = AsyncMock()
                handle = _make_handle(self.client, lease_uid="uid-1")
                with self.assertRaisesRegex(BatchError, "no resourceVersion"):
                    await handle.release()
                helper.delete_sandbox_claims_by_label.assert_not_awaited()
                helper.delete_batch_lease.assert_not_awaited()


class TestReleaseMembers(BaseClaimBatchTest):

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_release_member_deletes_its_claim_with_the_shared_retry_policy(self, _):
        unavailable = k8s_client.ApiException(status=503)
        cases = {
            "503 then success": ([unavailable, None], 2, None),
            "403 raises at once": ([k8s_client.ApiException(status=403)], 1, k8s_client.ApiException),
            "503 on every attempt raises": ([unavailable] * 3, 3, k8s_client.ApiException),
        }
        helper = self.cluster.helper
        for name, (effects, attempts, error) in cases.items():
            with self.subTest(name):
                helper.delete_sandbox_claim = AsyncMock(side_effect=effects)
                self.mock_sandbox_class.return_value = MagicMock(close_connection=AsyncMock())
                handle = _make_handle(self.client)
                handle._state.upsert_claim(
                    _claim("b1-0", conditions=_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                )
                sandbox = await handle.connect(handle.members()[0])
                [member] = handle.members()
                if error is None:
                    await handle.release_member(member)
                else:
                    with self.assertRaises(error):
                        await handle.release_member(member)
                sandbox.close_connection.assert_called_once()
                self.assertEqual(helper.delete_sandbox_claim.call_count, attempts)
                kwargs = helper.delete_sandbox_claim.call_args.kwargs
                self.assertEqual(
                    (kwargs["_request_timeout"], kwargs["log_level"], kwargs["batch"]),
                    (30, logging.DEBUG, True),
                )
        with self.assertRaises(ValueError):
            await handle.release_member(MagicMock(claim_name="b1-9"))

    async def test_release_not_ready_skips_unsent_creates_and_deletes_pending_members(self):
        self.cluster.ready_on_create = False
        # b1-1 waits for its pacing slot when the fill ends, and b1-2 and b1-3 are still planned.
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=4)], max_in_flight=2, create_rps=2)
        await _wait_until(lambda: self.cluster.names() == ["b1-0"])
        self.cluster.push("b1-0", ready=False)
        await _wait_until(lambda: len(batch.members()) == 1)

        await batch.release_not_ready()
        await _wait_until(lambda: all(c.done() for c in batch._creators))
        self.assertEqual(self.cluster.names(), ["b1-0"])
        self.assertEqual([c.args[0] for c in self.cluster.helper.delete_sandbox_claim.call_args_list], ["b1-0"])

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=0)
    async def test_release_not_ready_keeps_handed_out_member_when_create_retries_late(self, _):
        self.cluster.ready_on_create = False
        attempt_two, finish_retry = asyncio.Event(), asyncio.Event()
        self.addCleanup(finish_retry.set)
        attempts = 0

        async def create_claim(name, warmpool, namespace, **kwargs):
            nonlocal attempts
            attempts += 1
            if attempts == 1:
                # The first create lands on the apiserver, then fails on the client before the watch sees it.
                raise k8s_client.ApiException(status=503)
            self.cluster.push(name, warmpool, ready=True, annotations=kwargs["annotations"])
            attempt_two.set()
            await asyncio.wait_for(finish_retry.wait(), 5)
            raise k8s_client.ApiException(status=409)

        self.cluster.helper.create_sandbox_claim.side_effect = create_claim
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        [kept] = await batch.wait_for_quorum()
        await asyncio.wait_for(attempt_two.wait(), 5)

        await batch.release_not_ready()
        finish_retry.set()
        await _wait_until(lambda: all(c.done() for c in batch._creators))
        self.assertEqual(batch.members(), [kept])
        self.cluster.helper.delete_sandbox_claim.assert_not_awaited()

    async def test_release_member_stops_create_retries_and_deletes_an_in_flight_retry(self):
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
                fail_first, in_backoff = asyncio.Event(), asyncio.Event()
                entered_backoff, finish_backoff = asyncio.Event(), asyncio.Event()
                attempt_two, finish_retry = asyncio.Event(), asyncio.Event()
                in_first_delete, finish_first_delete = asyncio.Event(), asyncio.Event()
                second_delete_failed = asyncio.Event()
                for event in (fail_first, finish_backoff, finish_retry, finish_first_delete):
                    self.addCleanup(event.set)
                attempts = 0
                deletes = 0
                first_attempt: list[tuple[str, dict[str, str]]] = []

                async def create_claim(name, warmpool, namespace, **kwargs):
                    nonlocal attempts
                    attempts += 1
                    annotations = kwargs["annotations"]
                    if attempts == 1:
                        first_attempt.append((name, annotations))
                        if timing in ("seen_pending", "before_backoff"):
                            ready = origin == "fill" and timing != "seen_pending"
                            self.cluster.push(name, warmpool, ready=ready, annotations=annotations)
                        if timing in ("before_backoff", "during_backoff"):
                            await asyncio.wait_for(fail_first.wait(), 5)
                        raise k8s_client.ApiException(status=503)
                    self.cluster.push(name, warmpool, ready=(origin == "fill"), annotations=annotations)
                    attempt_two.set()
                    if timing in ("in_flight", "retry_then_fail"):
                        await asyncio.wait_for(finish_retry.wait(), 5)
                    # Simulates a retry landing after release_member deleted the first claim.
                    self.cluster.push(name, warmpool, ready=True, annotations=annotations)
                    if timing == "retry_then_fail":
                        raise k8s_client.ApiException(status=403)

                async def delete_claim(name, namespace, **kwargs):
                    nonlocal deletes
                    deletes += 1
                    call_num = deletes
                    if call_num == 1:
                        in_first_delete.set()
                        await asyncio.wait_for(finish_first_delete.wait(), 5)
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
                    batch = await self.claim([BatchGroup(warmpool="pool-a", size=1 if origin == "fill" else 0)])
                    if origin == "fill":
                        acquire_task = None
                    else:
                        acquire_timeout = 0.5 if timing == "before_backoff" else 5
                        acquire_task = asyncio.create_task(batch.acquire("pool-a", timeout=acquire_timeout))

                    if timing == "during_backoff":
                        await _wait_until(lambda: len(first_attempt) == 1)
                        orig_wait = batch._wait_for_stop

                        async def wait_gate(timeout: float) -> bool:
                            if timeout == 1.0:
                                entered_backoff.set()
                                await asyncio.wait_for(finish_backoff.wait(), 5)
                                return batch._stop_creating.is_set()
                            return await orig_wait(timeout)

                        batch._wait_for_stop = wait_gate  # type: ignore[method-assign]
                        fail_first.set()
                        await asyncio.wait_for(entered_backoff.wait(), 5)
                        claim_name, claim_annotations = first_attempt[0]
                        self.cluster.push(
                            claim_name, "pool-a", ready=(origin == "fill"), annotations=claim_annotations
                        )

                    if origin == "fill" and timing != "seen_pending":
                        [member] = await batch.wait_for_quorum()
                    else:
                        await _wait_until(lambda: len(batch.members()) == 1)
                        [member] = batch.members()

                    if timing == "seen_pending":
                        await asyncio.wait_for(in_backoff.wait(), 5)
                        self.cluster.push(
                            member.claim_name, "pool-a", ready=True, annotations=first_attempt[0][1]
                        )
                        if acquire_task is not None:
                            self.assertEqual((await asyncio.wait_for(acquire_task, 5)).claim_name, member.claim_name)
                        else:
                            self.assertEqual((await batch.wait_for_quorum())[0].claim_name, member.claim_name)
                            await _wait_until(lambda: all(c.done() for c in batch._creators))
                        self.assertEqual((attempts, deletes), (1, 0))
                        await batch._stop_background_tasks()
                        continue
                    if timing == "before_backoff":
                        release_task = asyncio.create_task(batch.release_member(member))
                        await asyncio.wait_for(in_first_delete.wait(), 5)
                        fail_first.set()
                    elif timing == "during_backoff":
                        async with batch._lock:
                            batch._next_create_at = time.monotonic() + 10
                        release_task = asyncio.create_task(batch.release_member(member))
                        await asyncio.wait_for(in_first_delete.wait(), 5)
                        finish_backoff.set()
                    else:
                        await asyncio.wait_for(attempt_two.wait(), 5)
                        release_task = asyncio.create_task(batch.release_member(member))
                        await asyncio.wait_for(in_first_delete.wait(), 5)
                        finish_retry.set()
                        await asyncio.wait_for(second_delete_failed.wait(), 5)

                    if acquire_task is not None:
                        with self.assertRaises(BatchError) as ctx:
                            await asyncio.wait_for(acquire_task, 5)
                        self.assertNotIsInstance(ctx.exception, BatchTimeoutError)
                        self.assertRegex(str(ctx.exception), "released before it was Ready")
                    else:
                        await _wait_until(lambda: all(c.done() for c in batch._creators))

                    finish_first_delete.set()
                    await asyncio.wait_for(release_task, 5)

                    self.assertEqual(batch.members(), [])
                    if timing in ("before_backoff", "during_backoff"):
                        self.assertEqual((attempts, deletes), (1, 1))
                        await batch.release_member(member)
                        self.assertEqual(deletes, 1)
                    else:
                        self.assertEqual((attempts, deletes), (2, 2))
                        # Even though the first delete finished after the retry's delete failed,
                        # the retry's delete stays pending and a later release_member retries it.
                        await batch.release_member(member)
                        self.assertEqual(deletes, 3)
                        await batch.release_member(member)
                        self.assertEqual(deletes, 3)
                    await batch._stop_background_tasks()

    async def test_release_group_deletes_by_group_label_and_skips_only_its_creates(self):
        gate = asyncio.Event()
        self.addCleanup(gate.set)

        async def hook(name):
            if name == "b1-0":
                await asyncio.wait_for(gate.wait(), 5)

        self.cluster.create_hook = hook
        # Creates go b1-0, b1-10, b1-1 to b1-4, b1-11, then the rest of pool-a.
        batch = await self.claim(
            [BatchGroup(warmpool="pool-a", size=10), BatchGroup(warmpool="pool-b", size=2)],
            max_in_flight=1,
            create_rps=5,
        )
        await _wait_until(lambda: self.cluster.helper.create_sandbox_claim.call_count == 1)
        with self.assertRaises(ValueError):
            await batch.release_group("pool-c")

        await batch.release_group("pool-a")
        gate.set()  # b1-0's create lands after its group was released.
        await _wait_until(lambda: len(batch.members()) == 2)
        helper = self.cluster.helper
        self.assertEqual(
            helper.delete_sandbox_claims_by_label.call_args.args,
            ("default", f"{BATCH_ID_LABEL}=b1,{BATCH_GROUP_LABEL}=8934303dc131d111,{BATCH_LEASE_UID_LABEL}=lease-uid"),
        )
        await _wait_until(lambda: helper.delete_sandbox_claim.call_count == 1)
        self.assertEqual(helper.delete_sandbox_claim.call_args.args[0], "b1-0")
        self.assertEqual(self.cluster.names(), ["b1-0", "b1-10", "b1-11"])
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-10", "b1-11"])
        # pool-a's skipped creates take no pacing slots, so b1-11 follows b1-10 by one slot.
        created_at = dict(self.cluster.created)
        self.assertLess(created_at["b1-11"] - created_at["b1-10"], 0.6)

    async def test_member_deletes_run_in_parallel_and_share_max_in_flight_with_creates(self):
        helper = self.cluster.helper
        in_flight_rpcs = 0
        in_flight_deletes = 0
        peak_rpcs = 0
        peak_deletes = 0
        create_entered, release_create = asyncio.Event(), asyncio.Event()
        fail_m0, finish_m1 = asyncio.Event(), asyncio.Event()
        delete_sleeping, finish_sleep = asyncio.Event(), asyncio.Event()
        m0_attempts = 0

        async def create_claim(name, warmpool, namespace, **kwargs):
            nonlocal in_flight_rpcs, peak_rpcs
            in_flight_rpcs += 1
            peak_rpcs = max(peak_rpcs, in_flight_rpcs)
            try:
                if name == "b1-3" and not create_entered.is_set():
                    create_entered.set()
                    await asyncio.wait_for(release_create.wait(), 5)
                    raise k8s_client.ApiException(status=503)
                await self.cluster._create(name, warmpool, namespace, **kwargs)
            finally:
                in_flight_rpcs -= 1

        async def delete_claim(name, namespace, **kwargs):
            nonlocal in_flight_rpcs, in_flight_deletes, peak_rpcs, peak_deletes, m0_attempts
            in_flight_rpcs += 1
            in_flight_deletes += 1
            peak_rpcs = max(peak_rpcs, in_flight_rpcs)
            peak_deletes = max(peak_deletes, in_flight_deletes)
            try:
                if name == "b1-0":
                    m0_attempts += 1
                    if m0_attempts == 1:
                        await asyncio.wait_for(fail_m0.wait(), 5)
                        raise k8s_client.ApiException(status=503)
                elif name == "b1-1":
                    await asyncio.wait_for(finish_m1.wait(), 5)
            finally:
                in_flight_rpcs -= 1
                in_flight_deletes -= 1

        helper.create_sandbox_claim.side_effect = create_claim
        helper.delete_sandbox_claim.side_effect = delete_claim
        with patch(
            "k8s_agent_sandbox.batch_utils.retry_delay",
            side_effect=lambda _a, err=None: 10 if err is None else 1,
        ):
            batch = await self.claim([BatchGroup(warmpool="pool-a", size=4)], max_in_flight=2)
            real_wait_for_stop = batch._wait_for_stop

            async def wait_on_retry(seconds):
                if asyncio.current_task() not in batch._creators:
                    delete_sleeping.set()
                    await asyncio.wait_for(finish_sleep.wait(), 10)
                    return batch._stop_creating.is_set()
                return await real_wait_for_stop(seconds)

            with patch.object(batch, "_wait_for_stop", side_effect=wait_on_retry):
                await _wait_until(lambda: len(batch.members()) == 3 and create_entered.is_set())
                m0, m1, m2 = batch.members()

                # b1-3's create holds one of the two slots, so only one release_member enters delete_sandbox_claim.
                t0 = asyncio.create_task(batch.release_member(m0))
                t1 = asyncio.create_task(batch.release_member(m1))
                await _wait_until(lambda: in_flight_deletes == 1 and len(batch.members()) == 1)
                self.assertEqual(in_flight_rpcs, 2)

                # b1-3's create fails with 503 and frees its slot before backoff, so m1's delete runs in parallel.
                release_create.set()
                await _wait_until(lambda: in_flight_deletes == 2)
                self.assertEqual(in_flight_rpcs, 2)

                # m2 waits until m0 fails with 503 and releases its slot before sleeping, then finishes while m0 sleeps.
                t2 = asyncio.create_task(batch.release_member(m2))
                await _wait_until(lambda: batch.members() == [])
                self.assertEqual(in_flight_deletes, 2)
                fail_m0.set()
                await asyncio.wait_for(delete_sleeping.wait(), 5)
                await asyncio.wait_for(t2, 2)
                finish_m1.set()
                finish_sleep.set()
                batch._stop_creating.set()
                await asyncio.gather(t0, t1)
                await _wait_until(lambda: all(c.done() for c in batch._creators))
        self.assertEqual((peak_rpcs, peak_deletes), (2, 2))

        with self.subTest("release_not_ready respects max_in_flight"):
            in_flight_not_ready = 0
            peak_not_ready = 0
            two_entered, allow_not_ready = asyncio.Event(), asyncio.Event()

            async def delete_pending(name, namespace, **kwargs):
                nonlocal in_flight_not_ready, peak_not_ready
                in_flight_not_ready += 1
                peak_not_ready = max(peak_not_ready, in_flight_not_ready)
                if in_flight_not_ready == 2:
                    two_entered.set()
                try:
                    await asyncio.wait_for(allow_not_ready.wait(), 5)
                finally:
                    in_flight_not_ready -= 1

            helper.create_sandbox_claim.side_effect = self.cluster._create
            helper.delete_sandbox_claim.side_effect = delete_pending
            handle = AsyncSandboxBatch(
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
            not_ready_task = asyncio.create_task(handle.release_not_ready())
            await asyncio.wait_for(two_entered.wait(), 5)
            await asyncio.sleep(0.02)
            self.assertEqual(peak_not_ready, 2)
            allow_not_ready.set()
            await asyncio.wait_for(not_ready_task, 5)
            self.assertEqual(peak_not_ready, 2)

    async def test_lifecycle_calls_wait_for_active_deletes_and_stop_further_deletes(self):
        helper = self.cluster.helper
        methods = (
            ("release_member", lambda h: h.release_member(h.members()[0]), "delete_sandbox_claim"),
            ("release_not_ready", lambda h: h.release_not_ready(), "delete_sandbox_claim"),
            ("release_group", lambda h: h.release_group("pool-a"), "delete_sandbox_claims_by_label"),
        )
        lifecycles = (
            ("detach", lambda h: h.detach(), "replace_lease"),
            ("release", lambda h: h.release(), "delete_lease"),
            ("client_close", lambda _h: self.client.close(), "close_helper"),
        )
        for name, call_release, delete_rpc in methods:
            for lifecycle_name, call_lifecycle, final_call in lifecycles:
                with self.subTest(method=name, lifecycle=lifecycle_name, first="release_method"):
                    helper.reset_mock()
                    helper.delete_sandbox_claim.side_effect = None
                    helper.delete_sandbox_claims_by_label.side_effect = None
                    calls: list[str] = []
                    in_delete, unblock = asyncio.Event(), asyncio.Event()
                    handle = _make_handle(
                        self.client,
                        groups=[BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=1)],
                    )
                    self.client._active_batches = {("default", "b1"): handle}
                    handle._start()
                    self.addAsyncCleanup(handle._stop_background_tasks)
                    handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
                    handle._state.upsert_claim(
                        _claim("b1-1", warmpool="pool-b", conditions=_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                    )
                    other_member = handle.members("pool-b")[0]
                    helper.read_batch_lease.side_effect = (
                        lambda *a, **k: _lease(holder_identity=handle._holder_identity)
                    )
                    helper.count_live_sandbox_claims.return_value = 0
                    helper.close = AsyncMock(side_effect=lambda: calls.append("close_helper"))

                    async def block_delete(*args, **kwargs):
                        if not in_delete.is_set():
                            calls.append("delete")
                            in_delete.set()
                            await asyncio.wait_for(unblock.wait(), 5)

                    async def record_replace(*args, **kwargs):
                        calls.append("replace_lease")
                        return _lease(holder_identity=args[2].spec.holder_identity, resource_version="11")

                    getattr(helper, delete_rpc).side_effect = block_delete
                    helper.replace_batch_lease.side_effect = record_replace
                    helper.delete_batch_lease.side_effect = lambda *a, **k: calls.append("delete_lease")

                    release_task = asyncio.create_task(call_release(handle))
                    await asyncio.wait_for(in_delete.wait(), 5)
                    lifecycle_task = asyncio.create_task(call_lifecycle(handle))
                    await _wait_until(lambda: handle._detached)
                    # Lease renewal stays alive and neither Lease writes nor helper.close run until the delete finishes.
                    self.assertFalse(handle._renew_task.done())
                    self.assertEqual(calls, ["delete"])
                    with self.assertRaises(BatchError):
                        await handle.release_member(other_member)
                    with self.assertLogs("k8s_agent_sandbox.async_sandbox_batch", level="WARNING") as logs:
                        logging.getLogger("k8s_agent_sandbox.async_sandbox_batch").warning("sentinel")
                        await handle._delete_released_create("b1-9", "pool-a")
                    self.assertEqual([r.getMessage() for r in logs.records], ["sentinel"])

                    unblock.set()
                    if name == "release_group":
                        with self.assertRaises(BatchError):
                            await release_task
                        await lifecycle_task
                        count_selectors = [
                            c.args[1] for c in helper.count_live_sandbox_claims.call_args_list
                        ]
                        self.assertNotIn(
                            batch_utils.group_label_selector("b1", "pool-a"), count_selectors
                        )
                    else:
                        await asyncio.gather(release_task, lifecycle_task)
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

                async def detach_from_another_task():
                    await asyncio.create_task(handle.detach())

                self.mock_sandbox_class.return_value = MagicMock(
                    close_connection=AsyncMock(side_effect=detach_from_another_task)
                )
                handle._state.upsert_claim(
                    _claim("b1-0", conditions=_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                )
                await handle.connect(handle.members()[0])
                handle._state.upsert_claim(
                    _claim("b1-0", conditions=_NOT_READY, sandbox={"name": "sbx"}, annotations=_FILL)
                )
                with self.assertRaises(BatchError):
                    await call_release(handle)
                self.assertTrue(handle._lease_released)
                helper.delete_sandbox_claim.assert_not_awaited()
                helper.delete_sandbox_claims_by_label.assert_not_awaited()

        with self.subTest("detach stops delete retry before backoff sleep"):
            helper.reset_mock()
            in_delete, unblock = asyncio.Event(), asyncio.Event()
            handle = _make_handle(self.client)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )

            async def fail_after_detach(*args, **kwargs):
                in_delete.set()
                await asyncio.wait_for(unblock.wait(), 5)
                raise k8s_client.ApiException(status=503)

            helper.delete_sandbox_claim.side_effect = fail_after_detach
            wait_mock = AsyncMock()
            with patch.object(handle, "_wait_for_stop", wait_mock):
                release_task = asyncio.create_task(handle.release_member(handle.members()[0]))
                await asyncio.wait_for(in_delete.wait(), 5)
                detach_task = asyncio.create_task(handle.detach())
                await _wait_until(lambda: handle._detached)
                unblock.set()
                with self.assertRaises(BatchError):
                    await release_task
                await detach_task
            wait_mock.assert_not_awaited()
            helper.delete_sandbox_claim.assert_awaited_once()
            self.assertTrue(handle._lease_released)

        with self.subTest(
            "detach stops release_group before count and sleep and does not wait for sleep"
        ):
            real_sleep = asyncio.sleep
            retry_sleeps: list[float] = []

            async def track_sleep(seconds):
                if seconds > 0.1:
                    retry_sleeps.append(seconds)
                    return
                await real_sleep(seconds)

            # Detach during deletecollection skips count and retry sleep.
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            helper.count_live_sandbox_claims.side_effect = None
            in_delete, unblock_delete = asyncio.Event(), asyncio.Event()
            handle = _make_handle(self.client)
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )
            helper.count_live_sandbox_claims.return_value = 1

            async def block_deletecollection(*args, **kwargs):
                in_delete.set()
                await asyncio.wait_for(unblock_delete.wait(), 5)

            helper.delete_sandbox_claims_by_label.side_effect = block_deletecollection
            retry_sleeps = []
            with patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5), \
                 patch("k8s_agent_sandbox.async_sandbox_batch.asyncio.sleep", side_effect=track_sleep):
                group_task = asyncio.create_task(handle.release_group("pool-a"))
                await asyncio.wait_for(in_delete.wait(), 5)
                detach_task = asyncio.create_task(handle.detach())
                await _wait_until(lambda: handle._detached)
                unblock_delete.set()
                with self.assertRaises(BatchError):
                    await group_task
                await detach_task
            helper.delete_sandbox_claims_by_label.assert_awaited_once()
            helper.count_live_sandbox_claims.assert_not_awaited()
            self.assertEqual(retry_sleeps, [])
            self.assertTrue(handle._lease_released)

            # Detach during count waits for count to finish and skips retry sleep.
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            in_count, unblock_count = asyncio.Event(), asyncio.Event()
            handle = _make_handle(self.client)
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )

            async def block_count(*args, **kwargs):
                in_count.set()
                await asyncio.wait_for(unblock_count.wait(), 5)
                return 1

            helper.count_live_sandbox_claims.side_effect = block_count
            retry_sleeps = []
            with patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5), \
                 patch("k8s_agent_sandbox.async_sandbox_batch.asyncio.sleep", side_effect=track_sleep):
                group_task = asyncio.create_task(handle.release_group("pool-a"))
                await asyncio.wait_for(in_count.wait(), 5)
                detach_task = asyncio.create_task(handle.detach())
                await _wait_until(lambda: handle._detached)
                helper.replace_batch_lease.assert_not_awaited()
                unblock_count.set()
                with self.assertRaises(BatchError):
                    await group_task
                await detach_task
            helper.delete_sandbox_claims_by_label.assert_awaited_once()
            helper.count_live_sandbox_claims.assert_awaited_once()
            self.assertEqual(retry_sleeps, [])
            self.assertTrue(handle._lease_released)

            # Detach during retry sleep finishes without waiting for the sleep, and Round 2 does not run.
            helper.reset_mock()
            helper.count_live_sandbox_claims.side_effect = None
            helper.count_live_sandbox_claims.return_value = 1
            in_sleep, unblock_sleep = asyncio.Event(), asyncio.Event()
            handle = _make_handle(self.client)
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )

            async def block_retry_sleep(seconds):
                if seconds > 0.1:
                    in_sleep.set()
                    await asyncio.wait_for(unblock_sleep.wait(), 5)
                    return
                await real_sleep(seconds)

            with patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5), \
                 patch("k8s_agent_sandbox.async_sandbox_batch.asyncio.sleep", side_effect=block_retry_sleep):
                group_task = asyncio.create_task(handle.release_group("pool-a"))
                await asyncio.wait_for(in_sleep.wait(), 5)
                await asyncio.wait_for(handle.detach(), 2)
                self.assertTrue(handle._lease_released)
                self.assertFalse(unblock_sleep.is_set())
                unblock_sleep.set()
                with self.assertRaises(BatchError):
                    await group_task
            helper.delete_sandbox_claims_by_label.assert_awaited_once()

        with self.subTest("cancelled detach leaves release clearing detach_grace before waiting"):
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            helper.count_live_sandbox_claims.side_effect = None
            in_delete, unblock = asyncio.Event(), asyncio.Event()
            handle = _make_handle(self.client)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))

            async def wait_delete(*args, **kwargs):
                in_delete.set()
                await asyncio.wait_for(unblock.wait(), 5)

            helper.delete_sandbox_claim.side_effect = wait_delete
            delete_task = asyncio.create_task(handle.release_member(handle.members()[0]))
            await asyncio.wait_for(in_delete.wait(), 5)

            detach_task = asyncio.create_task(handle.detach(grace=30))
            await _wait_until(lambda: handle._detach_grace == 30)
            detach_task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await detach_task
            self.assertTrue(handle._detach_unfinished())

            release_task = asyncio.create_task(handle.release())
            await _wait_until(handle._release_started)
            self.assertTrue(handle._release_started())
            self.assertFalse(handle._detach_unfinished())
            with self.assertRaises(BatchError):
                await asyncio.wait_for(handle.detach(), 2)

            release_task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await release_task
            self.assertFalse(handle._detach_unfinished())
            self.assertTrue(handle._release_started())

            unblock.set()
            await delete_task

        with self.subTest("cancelled delete releases active-delete barrier"):
            helper.reset_mock()
            in_delete, never = asyncio.Event(), asyncio.Event()
            handle = _make_handle(self.client)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))
            helper.read_batch_lease.side_effect = lambda *a, **k: _lease(holder_identity=handle._holder_identity)
            helper.replace_batch_lease.side_effect = (
                lambda *a, **k: _lease(holder_identity=a[2].spec.holder_identity, resource_version="11")
            )

            async def hang_delete(*args, **kwargs):
                in_delete.set()
                await never.wait()

            helper.delete_sandbox_claim.side_effect = hang_delete
            delete_task = asyncio.create_task(handle.release_member(handle.members()[0]))
            await asyncio.wait_for(in_delete.wait(), 5)
            delete_task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await delete_task
            await asyncio.wait_for(handle.detach(), 5)
            self.assertTrue(handle._lease_released)

    @patch("k8s_agent_sandbox.batch_utils.retry_delay", return_value=5)
    async def test_release_methods_delete_nothing_once_err_is_set(self, _):
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
                    await release(handle)
                self.assertIs(cm.exception.__cause__, cause)
                self.assertEqual(handle.members(), before)
                helper.delete_sandbox_claim.assert_not_awaited()
                helper.delete_sandbox_claims_by_label.assert_not_awaited()

            with self.subTest(watch_error_method=name):
                helper.reset_mock()
                helper.delete_sandbox_claim.side_effect = None
                helper.delete_sandbox_claims_by_label.side_effect = None
                helper.count_live_sandbox_claims.side_effect = None
                helper.count_live_sandbox_claims.return_value = 0
                handle = handle_with_a_pending_member()
                handle._state.note_error(watch_cause)
                self.assertIs(handle.err(), watch_cause)
                await release(handle)
                self.assertEqual(handle.members(), [])
                if name == "release_group":
                    helper.delete_sandbox_claims_by_label.assert_awaited_once()
                else:
                    helper.delete_sandbox_claim.assert_awaited_once()

        with self.subTest("err set between delete attempts"):
            helper.reset_mock()
            handle = handle_with_a_pending_member()

            def fail_and_stop(*args, **kwargs):
                handle._lease_error = cause
                raise k8s_client.ApiException(status=503)

            helper.delete_sandbox_claim.side_effect = fail_and_stop
            wait_mock = AsyncMock()
            with patch.object(handle, "_wait_for_stop", wait_mock):
                with self.assertRaises(BatchError) as cm:
                    await handle.release_member(handle.members()[0])
            self.assertIs(cm.exception.__cause__, cause)
            wait_mock.assert_not_awaited()
            helper.delete_sandbox_claim.assert_awaited_once()

        with self.subTest("err set during retry sleep"):
            helper.reset_mock()
            handle = handle_with_a_pending_member()
            helper.delete_sandbox_claim.side_effect = k8s_client.ApiException(status=503)
            with patch.object(
                handle,
                "_wait_for_stop",
                AsyncMock(side_effect=lambda _s: setattr(handle, "_lease_error", cause) or False),
            ):
                with self.assertRaises(BatchError) as cm:
                    await handle.release_member(handle.members()[0])
            self.assertIs(cm.exception.__cause__, cause)
            helper.delete_sandbox_claim.assert_awaited_once()

        with self.subTest("err set during release_group deletecollection"):
            for fail in (False, True):
                helper.reset_mock()
                helper.count_live_sandbox_claims.side_effect = None
                helper.count_live_sandbox_claims.return_value = 1
                handle = handle_with_a_pending_member()

                async def stop_during_deletecollection(*args, **kwargs):
                    handle._lease_error = cause
                    if fail:
                        raise k8s_client.ApiException(status=503)

                helper.delete_sandbox_claims_by_label.side_effect = stop_during_deletecollection
                sleep_mock = AsyncMock()
                with patch("k8s_agent_sandbox.async_sandbox_batch.asyncio.sleep", sleep_mock):
                    with self.assertRaises(BatchError) as cm:
                        await handle.release_group("pool-a")
                self.assertIs(cm.exception.__cause__, cause)
                helper.delete_sandbox_claims_by_label.assert_awaited_once()
                helper.count_live_sandbox_claims.assert_not_awaited()
                sleep_mock.assert_not_awaited()

        with self.subTest("err set during release_group count"):
            helper.reset_mock()
            helper.delete_sandbox_claims_by_label.side_effect = None
            handle = handle_with_a_pending_member()

            async def stop_during_count(*args, **kwargs):
                handle._lease_error = cause
                return 1

            helper.count_live_sandbox_claims.side_effect = stop_during_count
            sleep_mock = AsyncMock()
            with patch("k8s_agent_sandbox.async_sandbox_batch.asyncio.sleep", sleep_mock):
                with self.assertRaises(BatchError) as cm:
                    await handle.release_group("pool-a")
            self.assertIs(cm.exception.__cause__, cause)
            helper.delete_sandbox_claims_by_label.assert_awaited_once()
            helper.count_live_sandbox_claims.assert_awaited_once()
            sleep_mock.assert_not_awaited()

    async def test_watch_loop_deletes_late_fill_claims_in_released_pools_once(self):
        helper = self.cluster.helper
        handle = _make_handle(self.client, groups=[BatchGroup(warmpool="pool-a", size=3, min_ready=1)])
        handle._state.upsert_claim(
            _claim("b1-0", conditions=_READY, sandbox={"name": "sbx-0"}, annotations=_FILL)
        )
        self.assertEqual([m.claim_name for m in await handle.wait_for_quorum()], ["b1-0"])
        await handle.release_not_ready()
        helper.delete_sandbox_claim.assert_not_awaited()

        terminating = _claim("b1-2", conditions=_NOT_READY, annotations=_FILL)
        terminating["metadata"]["deletionTimestamp"] = "2026-03-01T00:00:00Z"

        async def stream(*_args, **_kwargs):
            yield {
                "type": "ADDED",
                "object": _claim("b1-1", conditions=_NOT_READY, annotations=_FILL),
            }
            yield {"type": "ADDED", "object": terminating}
            yield {
                "type": "MODIFIED",
                "object": _claim("b1-1", conditions=_READY, sandbox={"name": "sbx-1"}, annotations=_FILL),
            }
            raise asyncio.CancelledError()

        helper.watch_sandbox_claims = stream
        with contextlib.suppress(asyncio.CancelledError):
            await handle._watch_loop()
        self.assertEqual([c.args[0] for c in helper.delete_sandbox_claim.call_args_list], ["b1-1"])
        self.assertEqual([m.claim_name for m in handle.members()], ["b1-0"])

    async def test_lease_loss_wakes_delete_claim_retry_sleep(self):
        helper = self.cluster.helper
        handle = _make_handle(self.client)
        handle._state.upsert_claim(_claim("b1-0", conditions=_READY, sandbox={"name": "sbx-0"}))
        member = handle.members()[0]
        helper.delete_sandbox_claim.side_effect = k8s_client.ApiException(status=503)
        entered_retry = asyncio.Event()
        lease_error = BatchLeaseExpiredError("lease gone")

        def retry_delay(_attempt, _err=None):
            entered_retry.set()
            return 10.0

        with patch("k8s_agent_sandbox.batch_utils.retry_delay", side_effect=retry_delay):
            started = time.monotonic()
            task = asyncio.create_task(handle.release_member(member))
            await asyncio.wait_for(entered_retry.wait(), timeout=5.0)
            await asyncio.sleep(0)
            await handle._stop_renewal(lease_error)
            with self.assertRaises(BatchError) as cm:
                await asyncio.wait_for(task, timeout=3.0)
            elapsed = time.monotonic() - started

        self.assertLess(elapsed, 2.5)
        self.assertEqual(helper.delete_sandbox_claim.await_count, 1)
        self.assertIs(cm.exception.__cause__, lease_error)

    async def test_release_group_clamps_rounds_and_sleep_to_lease_deadline(self):
        helper = self.cluster.helper
        clock = [100.0]
        sleeps: list[float] = []

        async def fake_sleep(delay: float) -> None:
            sleeps.append(delay)
            clock[0] += delay

        with (
            patch.object(async_sandbox_batch, "time", SimpleNamespace(monotonic=lambda: clock[0])),
            patch.object(async_sandbox_batch.asyncio, "sleep", side_effect=fake_sleep),
        ):
            handle = _make_handle(self.client, lease_duration=60, lease_renewed_at=42.5)
            handle._state.upsert_claim(_claim("b1-0", conditions=_NOT_READY, annotations=_FILL))

            async def first_delete(*_args, **_kwargs):
                clock[0] = 101.0

            async def first_count(*_args, **_kwargs):
                clock[0] = 102.45
                return 1

            helper.delete_sandbox_claims_by_label.side_effect = first_delete
            helper.count_live_sandbox_claims.side_effect = first_count

            with self.assertRaises(BatchError) as cm:
                await handle.release_group("pool-a")

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

    async def test_creates_a_claim_outside_the_fill_from_any_pool_and_returns_it_ready(self):
        helper = self.cluster.helper
        # A size-0 group creates no claims.
        groups = [BatchGroup(warmpool="pool-a", size=2), BatchGroup(warmpool="pool-b", size=0)]
        batch = await self.claim(groups, labels={"team": "rl"}, quorum_timeout=90, work_budget=120)
        await _wait_until(lambda: len(batch.members()) == 2)
        # So a pool's Ready count isn't read again, which would add GETs to the checks counted below.
        refresh = patch.object(batch_utils, "BATCH_WARM_CREDIT_REFRESH_SECONDS", 3600)
        refresh.start()
        self.addCleanup(refresh.stop)
        _SteppingDatetime.calls = 0
        with patch("k8s_agent_sandbox.utils.datetime", _SteppingDatetime):
            member = await batch.acquire("pool-c")
        await batch.acquire("pool-c")
        await batch.acquire("pool-b")

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
        await batch.release_group("pool-c")
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0", "b1-1", "b1-4"])
        self.assertEqual(
            [e.member.claim_name for e in await _drain(batch.events())], ["b1-0", "b1-1"]
        )

    async def test_concurrent_first_acquires_check_a_pool_once_and_a_failed_check_is_not_kept(self):
        helper = self.cluster.helper
        pool = {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}
        checked: list[str] = []
        release_check = asyncio.Event()
        pool_c_results = [None, pool]

        async def get_warmpool(name, *args, **kwargs):
            checked.append(name)
            if name != "pool-a":
                await release_check.wait()
            if name == "pool-c":
                return pool_c_results.pop(0)
            if name == "pool-d":
                raise TimeoutError("request timed out")
            return pool

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        batch = await self.claim()
        await _wait_until(lambda: len(batch.members()) == 2)

        async def acquire_three(warmpool):
            release_check.clear()
            acquires = [asyncio.create_task(batch.acquire(warmpool)) for _ in range(3)]
            # One event-loop turn after the precheck starts puts the other two acquires on its
            # future.
            await _wait_until(lambda: warmpool in batch._prechecks)
            await asyncio.sleep(0)
            self.assertTrue(all(not a.done() for a in acquires))
            release_check.set()
            return await asyncio.wait_for(asyncio.gather(*acquires, return_exceptions=True), timeout=2)

        self.assertEqual(sorted(m.claim_name for m in await acquire_three("pool-b")), ["b1-2", "b1-3", "b1-4"])
        errors = await acquire_three("pool-c")
        self.assertEqual([type(e) for e in errors], [SandboxWarmPoolNotFoundError] * 3)
        self.assertEqual((await batch.acquire("pool-c")).warmpool, "pool-c")
        # A check that times out itself reaches every acquire as its own error, not as a timeout of
        # the wait for it.
        errors = await acquire_three("pool-d")
        self.assertEqual([(type(e), str(e)) for e in errors], [(TimeoutError, "request timed out")] * 3)

        self.assertEqual(checked, ["pool-a", "pool-b", "pool-c", "pool-c", "pool-d"])
        # Retrying a failed release_group() still accepts an undeclared pool after the first call
        # dropped its members.
        denied = k8s_client.ApiException(status=403)
        helper.delete_sandbox_claims_by_label.side_effect = [denied, None]
        with self.assertRaises(k8s_client.ApiException):
            await batch.release_group("pool-b")
        await batch.release_group("pool-b")
        self.assertEqual(helper.delete_sandbox_claims_by_label.await_count, 2)

    async def test_an_interrupted_pool_check_is_run_again_by_a_waiting_acquire(self):
        checked: list[str] = []

        async def get_warmpool(name, *args, **kwargs):
            checked.append(name)
            if name == "pool-b" and checked.count("pool-b") == 1:
                await asyncio.Event().wait()
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}

        self.cluster.helper.get_sandbox_warmpool.side_effect = get_warmpool
        batch = await self.claim()
        await _wait_until(lambda: len(batch.members()) == 2)
        first = asyncio.create_task(batch.acquire("pool-b"))
        await _wait_until(lambda: "pool-b" in checked)
        second = asyncio.create_task(batch.acquire("pool-b"))
        third = asyncio.create_task(batch.acquire("pool-b"))
        await asyncio.sleep(0)
        self.assertTrue(all(not t.done() for t in (first, second, third)))
        # Cancelled together, as a TaskGroup cancels its tasks, so the second stays cancelled and
        # the third checks the pool.
        first.cancel()
        second.cancel()
        self.assertEqual((await asyncio.wait_for(third, timeout=2)).warmpool, "pool-b")
        self.assertTrue(first.cancelled())
        self.assertTrue(second.cancelled())
        self.assertEqual(checked, ["pool-a", "pool-b", "pool-b"])

        # Cancelling a check while _lock is contended right after _precheck_pools (including after
        # recording warm credit) still clears _prechecks and _checking_acquires without hanging.
        in_template = asyncio.Event()
        release_template = asyncio.Event()
        release_blocker = asyncio.Event()
        blocker_held = asyncio.Event()
        self.addCleanup(release_template.set)
        self.addCleanup(release_blocker.set)

        async def get_template(name, *args, **kwargs):
            if "pool-c" in checked:
                in_template.set()
                await release_template.wait()
            return {}

        async def hold_lock():
            async with batch._lock:
                blocker_held.set()
                await release_blocker.wait()

        self.cluster.helper.get_sandbox_template.side_effect = get_template
        leader = asyncio.create_task(batch.acquire("pool-c"))
        await asyncio.wait_for(in_template.wait(), timeout=2)
        async with batch._lock:
            release_template.set()
            await asyncio.sleep(0)
            await asyncio.sleep(0)
            blocker = asyncio.create_task(hold_lock())
            await asyncio.sleep(0)
        await asyncio.wait_for(blocker_held.wait(), timeout=2)
        await asyncio.sleep(0)
        leader.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await asyncio.wait_for(leader, timeout=2)
        release_blocker.set()
        await asyncio.wait_for(blocker, timeout=2)
        self.assertEqual(batch._prechecks, {})
        self.assertEqual(batch._state._checking_acquires, {})
        self.assertEqual((await asyncio.wait_for(batch.acquire("pool-c"), timeout=2)).warmpool, "pool-c")

    async def test_an_acquire_waiting_on_another_acquires_pool_check_stops_at_its_timeout(self):
        helper = self.cluster.helper
        checked: list[str] = []
        release_check = asyncio.Event()
        real_wait_for = asyncio.wait_for

        async def get_warmpool(name, *args, **kwargs):
            checked.append(name)
            if name == "pool-d":
                return {"spec": {"sandboxTemplateRef": {"name": "tmpl-d"}}}
            if name != "pool-a":
                await release_check.wait()
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}}

        async def get_template(name, *args, **kwargs):
            if name == "tmpl-d":
                await asyncio.sleep(0.08)
            return {}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        helper.get_sandbox_template.side_effect = get_template
        batch = await self.claim()
        await _wait_until(lambda: len(batch.members()) == 2)
        first = asyncio.create_task(batch.acquire("pool-b"))
        await _wait_until(lambda: "pool-b" in batch._prechecks)
        with self.assertRaisesRegex(BatchTimeoutError, "wasn't checked within 0.05s"):
            await asyncio.wait_for(batch.acquire("pool-b", timeout=0.05), timeout=2)
        release_check.set()
        self.assertEqual((await asyncio.wait_for(first, timeout=2)).claim_name, "b1-2")

        # A check cancelled as or after a waiter's timeout expires neither leaks CancelledError
        # nor starts a second check past the deadline.
        for pool_name, race_error in (("pool-c", TimeoutError), ("pool-e", asyncio.CancelledError)):
            release_check.clear()
            intercepted = False
            first = asyncio.create_task(batch.acquire(pool_name))
            await _wait_until(lambda: pool_name in batch._prechecks)

            async def race_wait_for(fut, timeout):
                nonlocal intercepted
                if intercepted:
                    return await real_wait_for(fut, timeout)
                intercepted = True
                fut.cancel()
                await asyncio.sleep(timeout + 0.02)
                first.cancel()
                await asyncio.gather(first, return_exceptions=True)
                raise race_error

            with patch.object(async_sandbox_batch.asyncio, "wait_for", side_effect=race_wait_for):
                with self.assertRaisesRegex(BatchTimeoutError, "wasn't checked within 0.02s"):
                    await real_wait_for(batch.acquire(pool_name, timeout=0.02), timeout=2)
            self.assertEqual(checked.count(pool_name), 1)

        # A check that finishes past the deadline creates no claim and takes no ordinal.
        with self.assertRaisesRegex(BatchTimeoutError, "no create slot for warm pool 'pool-d' within 0.05s"):
            await batch.acquire("pool-d", timeout=0.05)
        self.assertEqual(helper.create_sandbox_claim.await_count, 3)
        self.assertEqual(batch._state.next_ordinal, 3)

    async def test_release_group_frees_the_acquired_members_of_a_size_zero_group(self):
        helper = self.cluster.helper
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=0)])
        await _wait_until(lambda: len(batch.members()) == 1)
        await batch.acquire("pool-b")
        self.cluster.ready_on_create = False
        task = asyncio.create_task(batch.acquire("pool-b", timeout=5))
        await _wait_until(lambda: helper.create_sandbox_claim.await_count == 3)
        await batch.release_group("pool-b")
        # An acquire still waiting on the pool raises at once.
        with self.assertRaisesRegex(BatchError, "released before it was Ready"):
            await asyncio.wait_for(task, 5)
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
        task = asyncio.create_task(batch.acquire("pool-b", timeout=5))
        await _wait_until(lambda: batch._state.next_ordinal == 4)
        await batch.release_group("pool-b")
        with self.assertRaisesRegex(BatchError, "released before it was Ready"):
            await asyncio.wait_for(task, 5)
        await batch.release_not_ready()
        self.assertEqual(helper.create_sandbox_claim.await_count, 3)
        self.assertEqual([c.args[0] for c in helper.delete_sandbox_claim.call_args_list], ["b1-2"])

    async def test_release_group_during_warm_credit_refresh_stops_acquire(self):
        helper = self.cluster.helper
        read_started = asyncio.Event()
        read_gate: asyncio.Event | None = None
        ready = {"pool-a": 0, "pool-unseen": 2, "pool-refresh": 2}

        async def get_warmpool(name, *args, **kwargs):
            gate = read_gate
            if gate is not None and name in ("pool-unseen", "pool-refresh"):
                read_started.set()
                await gate.wait()
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}, "status": {"readyReplicas": ready[name]}}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(batch_utils, "time", fake_time):
            batch = await self.claim([BatchGroup(warmpool="pool-a", size=0)])

            # Releasing an unseen pool during its first-use check stops the in-flight acquire
            # without raising ValueError or burning an ordinal.
            helper.create_sandbox_claim.reset_mock()
            helper.delete_sandbox_claim.reset_mock()
            read_gate = asyncio.Event()
            self.addCleanup(read_gate.set)
            checking = asyncio.create_task(batch.acquire("pool-unseen", timeout=5))
            await asyncio.wait_for(read_started.wait(), 5)
            await batch.release_group("pool-unseen")
            read_gate.set()
            with self.assertRaisesRegex(BatchError, "was released while acquiring"):
                await asyncio.wait_for(checking, 5)
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
                await batch.acquire("pool-a", timeout=0.2)
            self.assertEqual(batch._state.next_ordinal, 0)
            self.assertEqual(batch._state._checking_acquires, {})
            self.assertEqual(batch._state._released, set())

            batch._next_create_at = 0.0
            first = await batch.acquire("pool-refresh")
            second = await batch.acquire("pool-refresh")
            await batch.release_member(first)
            await batch.release_member(second)
            self.assertFalse(batch._state.has_pool("pool-refresh"))

            offset[0] += 1.0
            batch._next_create_at = time.monotonic() + 0.3
            helper.create_sandbox_claim.reset_mock()
            helper.delete_sandbox_claim.reset_mock()
            read_gate = asyncio.Event()
            self.addCleanup(read_gate.set)
            acquiring = asyncio.create_task(batch.acquire("pool-refresh", timeout=5))
            await asyncio.wait_for(read_started.wait(), 5)
            await batch.release_group("pool-refresh")
            read_gate.set()
            with self.assertRaisesRegex(BatchError, "was released while acquiring"):
                await asyncio.wait_for(acquiring, 5)
            helper.create_sandbox_claim.assert_not_called()
            helper.delete_sandbox_claim.assert_not_called()
            self.assertEqual(batch._state.next_ordinal, 2)
            self.assertEqual(batch._state._checking_acquires, {})
            self.assertEqual(batch._state._acquiring, {})
            self.assertEqual(batch._state._released, {"b1-0", "b1-1"})
            self.assertEqual(batch._acquire_waiters, {})

    async def test_acquires_the_warm_pool_covers_skip_pacing_and_reread_its_count_once_a_second(self):
        helper = self.cluster.helper
        ready = {"pool-a": 2}
        short_read_started = asyncio.Event()
        short_timeouts: list[float] = []

        async def get_warmpool(name, *args, **kwargs):
            timeout = kwargs.get("_request_timeout")
            if timeout is not None and timeout < 1:
                short_timeouts.append(timeout)
                short_read_started.set()
                await asyncio.sleep(5)
            # Slow enough that concurrent acquires would overlap their reads.
            await asyncio.sleep(0.05)
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}, "status": {"readyReplicas": ready[name]}}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(batch_utils, "time", fake_time):
            batch = await self.claim([BatchGroup(warmpool="pool-a", size=0)], max_in_flight=4, create_rps=2)
            start = time.monotonic()
            await asyncio.gather(*(batch.acquire("pool-a") for _ in range(4)))
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
            await asyncio.gather(*(batch.acquire("pool-a") for _ in range(3)))
            # One read covers all three, which would otherwise wait for pacing slots.
            self.assertLess(max(t - start for _, t in self.cluster.created), 0.2)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # When the next pacing slot is already free, acquire() starts at once without paying a
            # pool GET, while advancing _next_create_at for concurrent callers behind it.
            offset[0] += 1.0
            batch._next_create_at = 0.0
            self.cluster.created.clear()
            start = time.monotonic()
            await batch.acquire("pool-a")
            self.assertLess(self.cluster.created[0][1] - start, 0.1)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # A short-deadline acquire caps its pool read to its deadline and doesn't suppress
            # concurrent refreshes when its own deadline cuts the read short.
            ready["pool-a"] = 2
            self.cluster.created.clear()
            start = time.monotonic()
            short = asyncio.create_task(batch.acquire("pool-a", timeout=0.15))
            await asyncio.wait_for(short_read_started.wait(), 5)
            longer = [asyncio.create_task(batch.acquire("pool-a", timeout=5)) for _ in range(2)]
            with self.assertRaises(BatchTimeoutError):
                await asyncio.wait_for(short, 5)
            await asyncio.wait_for(asyncio.gather(*longer), 5)
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
            await batch.acquire("pool-a")
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)
            offset[0] += 5.0
            self.cluster.created.clear()
            start = time.monotonic()
            await asyncio.gather(*(batch.acquire("pool-a") for _ in range(2)))
            self.assertLess(max(t - start for _, t in self.cluster.created), 0.2)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 5)

            # A pool that isn't one of the batch's groups gets its credit from its check.
            ready["pool-c"] = 1
            self.cluster.created.clear()
            start = time.monotonic()
            await batch.acquire("pool-c")
            self.assertLess(self.cluster.created[0][1] - start, 0.2)

    async def test_cold_claim_defers_acquire_refresh_fences_in_flight_read_and_zeroes_on_relist(self):
        helper = self.cluster.helper
        ready = {"pool-a": 3, "pool-b": 4}
        in_flight_gate: asyncio.Event | None = None
        in_flight_entered = asyncio.Event()

        async def get_warmpool(name, *args, **kwargs):
            gate = in_flight_gate
            if gate is not None and name == "pool-a":
                in_flight_entered.set()
                await asyncio.wait_for(gate.wait(), 5)
            return {"spec": {"sandboxTemplateRef": {"name": "tmpl"}}, "status": {"readyReplicas": ready[name]}}

        helper.get_sandbox_warmpool.side_effect = get_warmpool
        offset = [0.0]
        fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])
        with patch.object(batch_utils, "time", fake_time):
            batch = await self.claim(
                [BatchGroup(warmpool="pool-a", size=0), BatchGroup(warmpool="pool-b", size=0)],
                max_in_flight=4,
                create_rps=4,
            )
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)
            # Age the precheck read past BATCH_WARM_CREDIT_REFRESH_SECONDS so only exhaust()
            # updating _read_at keeps acquire() from immediately re-reading stale readyReplicas.
            offset[0] += 2.0
            self.cluster.ready_on_create = False
            first = asyncio.create_task(batch.acquire("pool-a", timeout=5))
            await _wait_until(lambda: len(self.cluster.created) == 1)
            cold_pending = [{"type": "Ready", "status": "False", "reason": "SandboxCreatePending"}]
            self.cluster.watch_queue.put_nowait(
                {"type": "ADDED", "object": _claim("b1-0", "pool-a", conditions=cold_pending, annotations=None)}
            )
            await _wait_until(lambda: len(batch.members()) == 1)
            self.cluster.push("b1-0", "pool-a", ready=True, annotations=None)
            self.assertEqual((await asyncio.wait_for(first, 5)).claim_name, "b1-0")

            self.cluster.ready_on_create = True
            batch._next_create_at = time.monotonic() + 0.2
            self.cluster.created.clear()
            start = time.monotonic()
            await batch.acquire("pool-a")
            self.assertGreaterEqual(self.cluster.created[0][1] - start, 0.18)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # pool-b keeps its warm credit while pool-a is exhausted.
            self.cluster.created.clear()
            start = time.monotonic()
            await batch.acquire("pool-b")
            self.assertLess(self.cluster.created[0][1] - start, 0.1)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 2)

            # Once BATCH_WARM_CREDIT_REFRESH_SECONDS elapses, acquire() refreshes pool-a again.
            offset[0] += batch_utils.BATCH_WARM_CREDIT_REFRESH_SECONDS
            ready["pool-a"] = 2
            batch._next_create_at = time.monotonic() + 0.3
            self.cluster.created.clear()
            start = time.monotonic()
            await batch.acquire("pool-a")
            self.assertLess(self.cluster.created[0][1] - start, 0.15)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 3)

            # Spend pool-a's remaining credit and age _read_at so the next acquire starts a refresh.
            await batch.acquire("pool-a")
            offset[0] += 2.0
            ready["pool-a"] = 5
            in_flight_gate = asyncio.Event()
            self.addCleanup(in_flight_gate.set)
            batch._next_create_at = time.monotonic() + 0.05
            refreshing = asyncio.create_task(batch.acquire("pool-a", timeout=5))
            await asyncio.wait_for(in_flight_entered.wait(), 5)
            cold_not_ready = [{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}]
            self.cluster.watch_queue.put_nowait(
                {"type": "ADDED", "object": _claim("b1-90", "pool-a", conditions=cold_not_ready, annotations=None)}
            )
            await _wait_until(lambda: any(m.claim_name == "b1-90" for m in batch.members()))
            in_flight_gate.set()
            in_flight_gate = None
            await asyncio.wait_for(refreshing, 5)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)

            # The stale readyReplicas=5 read started before b1-90 arrived did not overwrite 0.
            batch._next_create_at = time.monotonic() + 0.2
            self.cluster.created.clear()
            start = time.monotonic()
            await batch.acquire("pool-a")
            self.assertGreaterEqual(self.cluster.created[0][1] - start, 0.18)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)

            # A 410 watch relist containing a cold claim also zeroes that pool's remaining credit.
            relist_cold = [{"type": "Ready", "status": "False", "reason": "SandboxCreatePending"}]
            helper.list_sandbox_claim_objects.return_value = (
                [_claim("b1-99", "pool-b", conditions=relist_cold, annotations=None)],
                "9",
            )
            raise_410 = True

            async def watch_with_410(*args, **kwargs):
                nonlocal raise_410
                if raise_410:
                    raise_410 = False
                    raise k8s_client.ApiException(status=410)
                async for ev in self.cluster._watch(*args, **kwargs):
                    yield ev

            helper.watch_sandbox_claims.side_effect = watch_with_410
            await _wait_until(lambda: any(m.claim_name == "b1-99" for m in batch.members()))
            batch._next_create_at = time.monotonic() + 0.2
            self.cluster.created.clear()
            start = time.monotonic()
            await batch.acquire("pool-b")
            self.assertGreaterEqual(self.cluster.created[0][1] - start, 0.18)
            self.assertEqual(helper.get_sandbox_warmpool.call_count, 4)

    async def test_fill_and_acquire_creates_share_max_in_flight(self):
        for outcome in ("timeout", "release_group", "err"):
            with self.subTest(outcome):
                self.cluster = _Cluster()
                self.client.k8s_helper = helper = self.cluster.helper
                helper.get_sandbox_warmpool.return_value = {
                    "spec": {"sandboxTemplateRef": {"name": "tmpl"}},
                    "status": {"readyReplicas": 3 if outcome == "timeout" else 4},
                }
                gate = asyncio.Event()
                self.addCleanup(gate.set)

                async def hook(name, g=gate):
                    if name == "b1-0":
                        await g.wait()

                self.cluster.create_hook = hook
                groups = (
                    [BatchGroup(warmpool="pool-a", size=1), BatchGroup(warmpool="pool-b", size=0)]
                    if outcome == "release_group"
                    else [BatchGroup(warmpool="pool-a", size=2 if outcome == "err" else 1)]
                )
                batch = await self.claim(groups, max_in_flight=1)
                await _wait_until(lambda: helper.create_sandbox_claim.call_count == 1)
                if outcome == "timeout":
                    with self.assertRaisesRegex(BatchTimeoutError, "no create in flight slot"):
                        await batch.acquire("pool-a", timeout=0.05)
                    self.assertEqual(helper.create_sandbox_claim.call_count, 1)
                    helper.delete_sandbox_claim.assert_not_called()

                    # First-use pool checks share max_in_flight too, and a short-deadline leader
                    # that times out waiting for a slot hands the check to a longer-deadline follower.
                    leader_waiting = asyncio.Event()
                    follower_waiting = asyncio.Event()
                    self.addCleanup(follower_waiting.set)
                    orig_shield = asyncio.shield
                    orig_in_flight_acquire = batch._in_flight.acquire
                    leader_calls = 0

                    def shield_and_signal(arg):
                        follower_waiting.set()
                        return orig_shield(arg)

                    async def leader_times_out_after_follower_waits():
                        nonlocal leader_calls
                        leader_calls += 1
                        if leader_calls == 1:
                            leader_waiting.set()
                            await asyncio.wait_for(follower_waiting.wait(), 5)
                        return await orig_in_flight_acquire()

                    with (
                        patch.object(asyncio, "shield", side_effect=shield_and_signal),
                        patch.object(batch._in_flight, "acquire", side_effect=leader_times_out_after_follower_waits),
                    ):
                        short_check = asyncio.create_task(batch.acquire("pool-b", timeout=0.25))
                        await asyncio.wait_for(leader_waiting.wait(), 5)
                        follower = asyncio.create_task(batch.acquire("pool-b", timeout=5))
                        await asyncio.wait_for(follower_waiting.wait(), 5)
                        with self.assertRaisesRegex(BatchTimeoutError, "warm pool 'pool-b' wasn't checked"):
                            await asyncio.wait_for(short_check, 5)
                        self.assertEqual(helper.get_sandbox_warmpool.call_count, 1)
                        gate.set()
                        checked_member = await asyncio.wait_for(follower, 5)
                    self.assertEqual(checked_member.warmpool, "pool-b")
                    await _wait_until(lambda: all(c.done() for c in batch._creators))
                    fill_member = next(m for m in batch.members() if m.claim_name == "b1-0")
                    self.assertEqual((await batch.acquire("pool-a")).claim_name, "b1-3")
                    batch._next_create_at = 0.0
                    offset = [0.0]
                    fake_time = SimpleNamespace(monotonic=lambda: time.monotonic() + offset[0])

                    async def expire_during_pacing(_t):
                        offset[0] += 10.0
                        return False

                    with (
                        patch.object(async_sandbox_batch, "time", fake_time),
                        patch.object(batch, "_wait_for_stop", side_effect=expire_during_pacing),
                    ):
                        with self.assertRaisesRegex(BatchTimeoutError, "no create in flight slot"):
                            await batch.acquire("pool-a", timeout=5)
                    self.assertEqual(helper.create_sandbox_claim.call_count, 3)
                    helper.delete_sandbox_claim.assert_not_called()

                    # Expiring while waiting on _lock after _in_flight.acquire succeeds skips
                    # on_send and create_sandbox_claim and releases the _in_flight slot.
                    in_flight_taken = asyncio.Event()
                    lock_held = asyncio.Event()
                    self.addCleanup(lock_held.set)
                    orig_lock = batch._lock
                    signaling_lock = _SignalingLock(orig_lock)

                    async def take_slot_then_wait_for_lock():
                        res = await orig_in_flight_acquire()
                        in_flight_taken.set()
                        await asyncio.wait_for(lock_held.wait(), 5)
                        return res

                    batch._next_create_at = 0.0
                    with (
                        patch.object(async_sandbox_batch, "time", fake_time),
                        patch.object(batch._in_flight, "acquire", side_effect=take_slot_then_wait_for_lock),
                    ):
                        acquiring = asyncio.create_task(batch.acquire("pool-a", timeout=5))
                        await asyncio.wait_for(in_flight_taken.wait(), 5)
                        batch._lock = signaling_lock  # type: ignore[assignment]
                        try:
                            async with signaling_lock:
                                lock_held.set()
                                await asyncio.wait_for(signaling_lock._blocked_or_done.wait(), 5)
                                offset[0] += 10.0
                            with self.assertRaisesRegex(BatchTimeoutError, "no create in flight slot"):
                                await asyncio.wait_for(acquiring, 5)
                        finally:
                            batch._lock = orig_lock
                    self.assertEqual(helper.create_sandbox_claim.call_count, 3)
                    helper.delete_sandbox_claim.assert_not_called()
                    self.assertFalse(batch._in_flight.locked())

                    # Warm-credit refreshes share max_in_flight with member deletes as well.
                    delete_gate = asyncio.Event()
                    delete_started = asyncio.Event()
                    self.addCleanup(delete_gate.set)

                    async def slow_delete(*args, **kwargs):
                        delete_started.set()
                        await delete_gate.wait()

                    helper.delete_sandbox_claim.side_effect = slow_delete
                    with patch.object(batch_utils, "BATCH_WARM_CREDIT_REFRESH_SECONDS", 0.0):
                        deleting = asyncio.create_task(batch.release_member(fill_member))
                        await asyncio.wait_for(delete_started.wait(), 5)
                        batch._next_create_at = time.monotonic() + 0.5
                        before_reads = helper.get_sandbox_warmpool.call_count
                        with self.assertRaisesRegex(BatchTimeoutError, "no create slot for warm pool 'pool-a'"):
                            await batch.acquire("pool-a", timeout=0.03)
                        self.assertEqual(helper.get_sandbox_warmpool.call_count, before_reads)
                        delete_gate.set()
                        await asyncio.wait_for(deleting, 5)
                    await batch._stop_background_tasks()
                    continue
                target_pool = "pool-b" if outcome == "release_group" else "pool-a"
                entered_in_flight = asyncio.Event()
                orig_in_flight_acquire = batch._in_flight.acquire

                async def record_acquire_waiting():
                    if asyncio.current_task() not in batch._creators:
                        entered_in_flight.set()
                    return await orig_in_flight_acquire()

                with patch.object(batch._in_flight, "acquire", side_effect=record_acquire_waiting):
                    task = asyncio.create_task(batch.acquire(target_pool, timeout=5))
                    await asyncio.wait_for(entered_in_flight.wait(), 5)
                    if outcome == "release_group":
                        await batch.release_group("pool-b")
                    else:
                        watch_err = k8s_client.ApiException(status=403)
                        helper.watch_sandbox_claims.side_effect = watch_err
                        await _wait_until(lambda: batch.err() is watch_err)
                    gate.set()
                    with self.assertRaises(BatchError) as ctx:
                        await asyncio.wait_for(task, 5)
                await _wait_until(lambda: all(c.done() for c in batch._creators))
                self.assertNotIsInstance(ctx.exception, BatchTimeoutError)
                if outcome == "release_group":
                    self.assertRegex(str(ctx.exception), "released before it was Ready")
                else:
                    self.assertIs(ctx.exception.__cause__, batch.err())
                self.assertEqual(helper.create_sandbox_claim.call_count, 1)
                helper.delete_sandbox_claim.assert_not_called()
                await batch._stop_background_tasks()

    async def test_failures_drop_the_member_and_delete_a_claim_that_may_exist(self):
        terminal = [{"type": "Ready", "status": "False", "reason": "InvalidConfiguration"}]
        failure = RuntimeError("watch failed")
        lease_failure = BatchLeaseExpiredError("renewal stopped")

        async def push_terminal(batch, task):
            claim = _claim("b1-1", conditions=terminal)
            self.cluster.watch_queue.put_nowait({"type": "MODIFIED", "object": claim})

        async def delete(batch, task):
            self.cluster.push("b1-1", ready=False, annotations=None)
            await _wait_until(lambda: batch.members()[-1].claim_name == "b1-1")
            self.cluster.watch_queue.put_nowait({"type": "DELETED", "object": _claim("b1-1")})

        async def release(batch, task):
            self.cluster.push("b1-1", ready=False, annotations=None)
            await _wait_until(lambda: batch.members()[-1].claim_name == "b1-1")
            await batch.release_member(batch.members()[-1])

        async def fail_watch(batch, task):
            async with batch._changed:
                batch._state.note_error(failure)
                batch._notify_all()

        async def lose_lease(batch, task):
            async with batch._changed:
                batch._lease_error = lease_failure
                batch._notify_all()

        async def cancel(batch, task):
            task.cancel()

        async def wait(batch, task):
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
            "cancelled": (None, cancel, asyncio.CancelledError, None, True),
        }
        for name, (create_error, act, error, state, deleted) in cases.items():
            with self.subTest(name):
                # A fresh cluster, so the previous batch's watch can't take this one's events.
                self.cluster = _Cluster()
                self.client.k8s_helper = helper = self.cluster.helper
                batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
                await _wait_until(lambda: len(batch.members()) == 1)
                self.cluster.ready_on_create = False
                # Past the fill deadline, like an acquire that replaces a member later, which must
                # still wait for changes rather than spin.
                batch._fill_deadline = time.monotonic()
                waits = []
                wrapped = False

                async def hook(claim_name):
                    nonlocal wrapped
                    if claim_name == "b1-1" and not wrapped:
                        wrapped = True
                        waiter = batch._acquire_waiters["b1-1"]
                        wait_for_event = waiter.wait
                        waiter.wait = lambda: waits.append(None) or wait_for_event()
                    if create_error is not None and claim_name == "b1-1":
                        raise create_error.pop(0) if isinstance(create_error, list) else create_error

                self.cluster.create_hook = hook
                task = asyncio.create_task(batch.acquire("pool-a", timeout=0.5))
                await _wait_until(lambda: helper.create_sandbox_claim.await_count >= 2)
                await act(batch, task)
                with self.assertRaises(error) as raised:
                    await task
                self.assertLess(len(waits), 20)
                if state is not None:
                    self.assertEqual(raised.exception.member.state, state)
                if create_error is not None and state is not None:
                    self.assertEqual(raised.exception.member.reason, "CreateFailed")
                if name in ("watch err set", "lease err set"):
                    expected_cause = failure if name == "watch err set" else lease_failure
                    # err() keeps the original error, and a later acquire fails before checking a
                    # new pool.
                    self.assertIs(batch.err(), expected_cause)
                    self.assertIs(raised.exception.__cause__, expected_cause)
                    helper.get_sandbox_warmpool.reset_mock()
                    with self.assertRaises(BatchError) as later:
                        await batch.acquire("pool-unseen")
                    self.assertIs(later.exception.__cause__, expected_cause)
                    helper.get_sandbox_warmpool.assert_not_awaited()
                if name == "released while pending":
                    # It fails at once rather than waiting out its timeout.
                    self.assertNotIsInstance(raised.exception, BatchTimeoutError)
                self.assertEqual([m.claim_name for m in batch.members()], ["b1-0"])
                self.assertEqual(
                    [c.args[0] for c in helper.delete_sandbox_claim.call_args_list], ["b1-1"] if deleted else []
                )
                await batch._stop_background_tasks()

        # Stopping or cancelling an acquire during pacing sends neither create nor delete.
        self.cluster = _Cluster()
        self.client.k8s_helper = helper = self.cluster.helper
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        self.addAsyncCleanup(batch._stop_background_tasks)
        await _wait_until(lambda: len(batch.members()) == 1)
        batch._next_create_at = time.monotonic() + 10
        cancelled = asyncio.create_task(batch.acquire("pool-a", timeout=20))
        await _wait_until(lambda: batch._state.next_ordinal == 2)
        cancelled.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await cancelled
        batch._next_create_at = time.monotonic() + 10
        stopped = asyncio.create_task(batch.acquire("pool-a", timeout=20))
        await _wait_until(lambda: batch._state.next_ordinal == 3)
        await batch.detach()
        with self.assertRaises(BatchError):
            await asyncio.wait_for(stopped, timeout=2)
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0"])
        self.assertEqual(helper.create_sandbox_claim.await_count, 1)
        helper.delete_sandbox_claim.assert_not_awaited()
        self.assertEqual(batch._state._pending_deletes, {})
        helper.get_sandbox_warmpool.reset_mock()
        with self.assertRaises(BatchError):
            await batch.acquire("pool-unseen")
        helper.get_sandbox_warmpool.assert_not_awaited()

    async def test_fill_and_acquired_claims_are_owned_by_the_lease_the_handle_holds(self):
        claimed = self.cluster.helper
        claimed.create_batch_lease.return_value = _lease(uid="uid-claimed")
        batch = await self.claim()
        await _wait_until(lambda: len(batch.members()) == 2)
        await batch.acquire("pool-a")
        # Stopped so its watch doesn't read the re-attached handle's events.
        await batch._stop_background_tasks()
        self.cluster = _Cluster()
        self.client.k8s_helper = helper = self.cluster.helper
        helper.list_sandbox_claim_objects.return_value = ([_claim("b1-0", annotations=_FILL)], "5")
        helper.read_batch_lease.return_value = _lease(renew_time=datetime.now(UTC), uid="uid-attached")
        helper.replace_batch_lease.return_value = _lease(renew_time=datetime.now(UTC), uid="uid-attached")
        batch = await self.client.get_batch("b1")
        self.addAsyncCleanup(batch._stop_background_tasks)
        await batch.acquire("pool-a")

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

    async def test_reattached_handle_reads_back_ordinals_labels_and_settings(self):
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
        batch = await self.client.get_batch("b1")
        self.addAsyncCleanup(batch._stop_background_tasks)
        _SteppingDatetime.calls = 0
        with patch("k8s_agent_sandbox.utils.datetime", _SteppingDatetime):
            member = await batch.acquire("pool-a")
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
        await asyncio.gather(batch.acquire("pool-a"), batch.acquire("pool-a"))
        created_at = dict(self.cluster.created)
        self.assertGreater(abs(created_at["b1-9"] - created_at["b1-8"]), 0.15)
        # A create that pacing can't start before the timeout fails without a claim or an ordinal.
        batch._next_create_at = time.monotonic() + 60
        with self.assertRaises(BatchTimeoutError):
            await batch.acquire("pool-a", timeout=1)
        self.assertEqual(helper.create_sandbox_claim.call_count, 3)
        self.assertEqual(batch._state.next_ordinal, 10)
        # Nor is a create whose slot comes after err() is set.
        batch._next_create_at = time.monotonic() + 0.3
        task = asyncio.create_task(batch.acquire("pool-a", timeout=5))
        await _wait_until(lambda: batch._state.next_ordinal == 11)
        async with batch._changed:
            batch._state.note_error(RuntimeError("watch failed"))
            batch._notify_all()
        with self.assertRaises(BatchError):
            await task
        self.assertEqual(helper.create_sandbox_claim.call_count, 3)
        self.assertEqual(helper.delete_sandbox_claim.call_count, 0)

    async def test_watch_events_for_other_claims_do_not_wake_waiting_acquire(self):
        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        await _wait_until(lambda: len(batch.members()) == 1)
        self.cluster.ready_on_create = False
        wakes = []
        entered_wait = asyncio.Event()

        async def hook(claim_name):
            if claim_name == "b1-1":
                waiter = batch._acquire_waiters["b1-1"]
                set_event = waiter.set
                wait_for_event = waiter.wait
                waiter.set = lambda: wakes.append("set") or set_event()
                waiter.wait = lambda: entered_wait.set() or wait_for_event()

        self.cluster.create_hook = hook
        task = asyncio.create_task(batch.acquire("pool-a", timeout=5))
        await asyncio.wait_for(entered_wait.wait(), timeout=2)
        # Pending events for b1-1 itself and events for other claims must not wake b1-1's waiter.
        self.cluster.push("b1-1", ready=False, annotations=None)
        self.cluster.push("b1-99", ready=False, annotations=None)
        self.cluster.push("b1-99", ready=True, annotations=None)
        self.cluster.watch_queue.put_nowait({"type": "DELETED", "object": _claim("b1-99")})
        await _wait_until(lambda: any(m.claim_name == "b1-99" and m.state == MemberState.LOST for m in batch.members()))
        self.assertEqual(wakes, [])
        self.cluster.push("b1-1", ready=True, annotations=None)
        member = await asyncio.wait_for(task, timeout=2)
        self.assertEqual(member.claim_name, "b1-1")
        self.assertEqual(wakes, ["set"])
        self.assertEqual(batch._acquire_waiters, {})

    async def test_pod_annotations_are_sent_on_create_and_retries_and_omitted_when_empty(self):
        real_helper = AsyncK8sHelper()
        bodies: list[dict] = []
        unavailable = k8s_client.ApiException(status=503)
        unavailable.headers = {"Retry-After": "0"}
        retried = False

        async def create_custom_object(**kwargs):
            nonlocal retried
            body = kwargs["body"]
            bodies.append(body)
            name = body["metadata"]["name"]
            if name == "b1-1" and not retried:
                retried = True
                raise unavailable
            await self.cluster._create(
                name,
                body["spec"]["warmPoolRef"]["name"],
                kwargs["namespace"],
                annotations=body["metadata"]["annotations"],
            )
            return body

        real_helper.custom_objects_api = MagicMock(
            create_namespaced_custom_object=AsyncMock(side_effect=create_custom_object)
        )
        real_helper._initialized = True
        self.cluster.helper.create_sandbox_claim.side_effect = real_helper.create_sandbox_claim

        batch = await self.claim([BatchGroup(warmpool="pool-a", size=1)])
        await _wait_until(lambda: len(batch.members()) == 1)
        with self.assertRaises(ValueError):
            await batch.acquire("pool-unseen", pod_annotations={"bad": None})  # type: ignore[dict-item]
        self.assertEqual([c.args[0] for c in self.cluster.helper.get_sandbox_warmpool.call_args_list], ["pool-a"])

        pin = {"podsnapshot.gke.io/ps-name": "snap-1"}
        member = await batch.acquire("pool-a", pod_annotations=pin)
        await batch.acquire("pool-a")
        await batch.acquire("pool-a", pod_annotations={})

        self.assertEqual(member.claim_name, "b1-1")
        self.assertEqual([b["metadata"]["name"] for b in bodies], ["b1-0", "b1-1", "b1-1", "b1-2", "b1-3"])
        for body in bodies[1:3]:
            self.assertEqual(body["spec"]["additionalPodMetadata"], {"annotations": pin})
            self.assertNotIn("podsnapshot.gke.io/ps-name", body["metadata"]["annotations"])
        for body in (bodies[0], bodies[3], bodies[4]):
            self.assertNotIn("additionalPodMetadata", body["spec"])

    async def test_pool_check_bounds_reads_by_deadline_and_hands_off_on_timeout(self):
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
        self.addAsyncCleanup(injected.close)
        real_helper = AsyncK8sHelper(api_client=injected)
        helper = self.cluster.helper
        helper.get_sandbox_warmpool.side_effect = real_helper.get_sandbox_warmpool
        helper.get_sandbox_template.side_effect = real_helper.get_sandbox_template

        batch = await self.claim(
            [BatchGroup(warmpool="pool-a", size=1)], max_in_flight=1, lease_duration=60
        )
        await _wait_until(lambda: len(batch.members()) == 1)

        # Stalled warm-pool GET respects timeout and releases the sole max_in_flight slot.
        start = time.monotonic()
        with self.assertRaisesRegex(
            BatchTimeoutError, "warm pool 'pool-b' wasn't checked within 0.12s"
        ):
            await batch.acquire("pool-b", timeout=0.12)
        self.assertLess(time.monotonic() - start, 0.45)
        self.assertEqual([m.claim_name for m in batch.members()], ["b1-0"])
        self.assertNotIn("pool-b", batch._checked_pools)
        self.assertEqual(batch._prechecks, {})
        self.assertFalse(batch._in_flight.locked())

        # Template GET shares the remaining deadline after the warm-pool GET consumes part of it.
        start = time.monotonic()
        with self.assertRaisesRegex(
            BatchTimeoutError, "warm pool 'pool-c' wasn't checked within 0.16s"
        ):
            await batch.acquire("pool-c", timeout=0.16)
        self.assertLess(time.monotonic() - start, 0.45)
        self.assertNotIn("pool-c", batch._checked_pools)

        # A short-deadline leader cancels check on BatchTimeoutError so a follower takes over.
        leader = asyncio.create_task(batch.acquire("pool-d", timeout=0.12))
        await _wait_until(pool_d_started.is_set)
        follower = asyncio.create_task(batch.acquire("pool-d", timeout=5.0))
        await asyncio.sleep(0)
        follower_waiting.set()
        with self.assertRaisesRegex(
            BatchTimeoutError, "warm pool 'pool-d' wasn't checked within 0.12s"
        ):
            await asyncio.wait_for(leader, timeout=2.0)
        member = await asyncio.wait_for(follower, timeout=2.0)
        self.assertEqual((member.claim_name, member.warmpool), ("b1-1", "pool-d"))

        # A follower whose short timeout expires after release_group() raises BatchError, not
        # BatchTimeoutError, even while the leader's HTTP read is still stalled.
        leader = asyncio.create_task(batch.acquire("pool-f", timeout=5.0))
        await _wait_until(pool_f_started.is_set)
        follower = asyncio.create_task(batch.acquire("pool-f", timeout=0.16))
        await asyncio.sleep(0)
        await batch.release_group("pool-f")
        with self.assertRaisesRegex(
            BatchError, "warm pool 'pool-f' was released while acquiring"
        ) as released_ctx:
            await asyncio.wait_for(follower, timeout=2.0)
        self.assertNotIsInstance(released_ctx.exception, BatchTimeoutError)
        pool_f_release.set()
        with self.assertRaisesRegex(
            BatchError, "warm pool 'pool-f' was released while acquiring"
        ):
            await asyncio.wait_for(leader, timeout=2.0)

        # A follower with a long timeout wakes at _lease_expires_at with BatchError while the
        # leader's earlier socket read is still stalled.
        leader = asyncio.create_task(batch.acquire("pool-e", timeout=5.0))
        await _wait_until(pool_e_started.is_set)
        batch._lease_expires_at = time.monotonic() + 0.10
        start = time.monotonic()
        follower = asyncio.create_task(batch.acquire("pool-e", timeout=5.0))
        await asyncio.sleep(0)
        with self.assertRaises(BatchError) as follower_expired:
            await asyncio.wait_for(follower, timeout=2.0)
        self.assertLess(time.monotonic() - start, 0.45)
        self.assertNotIsInstance(follower_expired.exception, BatchTimeoutError)
        self.assertIsInstance(follower_expired.exception.__cause__, BatchLeaseExpiredError)
        pool_e_release.set()
        with self.assertRaises(BatchError) as leader_expired:
            await asyncio.wait_for(leader, timeout=2.0)
        self.assertNotIsInstance(leader_expired.exception, BatchTimeoutError)
        self.assertIsInstance(leader_expired.exception.__cause__, BatchLeaseExpiredError)
        await batch._stop_background_tasks()

        # The template GET recomputes remaining Lease time after the pool GET spends most of it.
        b2 = await self.claim(
            [BatchGroup(warmpool="pool-a", size=0)],
            batch_id="b2",
            max_in_flight=1,
            lease_duration=60,
        )
        b2._lease_expires_at = time.monotonic() + 0.32
        start = time.monotonic()
        with self.assertRaises(BatchError) as tmpl_expired:
            await b2.acquire("pool-g", timeout=5.0)
        self.assertLess(time.monotonic() - start, 1.0)
        self.assertNotIsInstance(tmpl_expired.exception, BatchTimeoutError)
        self.assertIsInstance(tmpl_expired.exception.__cause__, BatchLeaseExpiredError)
        await b2._stop_background_tasks()

        # A leader blocked on a busy max_in_flight slot re-sleeps across an early wakeup and
        # wakes at _lease_expires_at with BatchError, not BatchTimeoutError.
        b3 = await self.claim(
            [BatchGroup(warmpool="pool-a", size=0)],
            batch_id="b3",
            max_in_flight=1,
            lease_duration=60,
        )
        orig_b3_acquire = b3._in_flight.acquire
        slot_calls = 0

        async def wake_early_once():
            nonlocal slot_calls
            slot_calls += 1
            if slot_calls == 1:
                async with asyncio.timeout(0.01):
                    return await orig_b3_acquire()
            return await orig_b3_acquire()

        await orig_b3_acquire()
        try:
            b3._lease_expires_at = time.monotonic() + 0.12
            start = time.monotonic()
            with (
                patch.object(b3._in_flight, "acquire", side_effect=wake_early_once),
                self.assertRaises(BatchError) as slot_expired,
            ):
                await b3.acquire("pool-b", timeout=5.0)
            self.assertGreaterEqual(slot_calls, 2)
            self.assertLess(time.monotonic() - start, 0.45)
            self.assertNotIsInstance(slot_expired.exception, BatchTimeoutError)
            self.assertIsInstance(slot_expired.exception.__cause__, BatchLeaseExpiredError)
        finally:
            b3._in_flight.release()
            await b3._stop_background_tasks()


@patch.object(AsyncSandboxBatch, "_start", lambda self: None)
class TestGetBatchQuorumTimeout(BaseAsyncBatchClientTest):

    async def test_quorum_timeout_annotation_is_validated_and_counts_from_attach(self):
        self.mock_k8s_helper.list_sandbox_claim_objects = AsyncMock(return_value=([_claim("b1-0")], "5"))
        self.mock_k8s_helper.replace_batch_lease = AsyncMock()
        self.mock_k8s_helper.read_batch_lease = AsyncMock(return_value=_lease(
            renew_time=datetime.now(UTC), annotations={BATCH_QUORUM_TIMEOUT_ANNOTATION: "abc"}
        ))
        with self.assertRaises(BatchError):
            await self.client.get_batch("b1")
        self.mock_k8s_helper.replace_batch_lease.assert_not_called()

        self.mock_k8s_helper.read_batch_lease.return_value = _lease(
            renew_time=datetime.now(UTC), annotations={BATCH_QUORUM_TIMEOUT_ANNOTATION: "90"}
        )
        batch = await self.client.get_batch("b1")
        self.assertAlmostEqual(batch._fill_deadline - time.monotonic(), 90, delta=5)


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
        handle._connected["b1-0"] = (cached, None)
        self.client._closing_batches.add(handle)

        await self.client.delete_all()

        cached.close_connection.assert_awaited_once()
        self.assertEqual(self.client._closing_batches, set())

    async def test_cleanup_unregisters_a_handle_whose_batch_another_handle_holds(self):
        handle = _make_handle(self.client)
        cached = MagicMock(close_connection=AsyncMock(side_effect=[RuntimeError("close failed"), None]))
        handle._connected["b1-0"] = (cached, None)
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
