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

"""Async handle for using a claimed or re-attached sandbox batch.

Requires the ``async`` optional dependencies::

    pip install k8s-agent-sandbox[async]
"""

import asyncio
import contextlib
import logging
import time
from typing import TYPE_CHECKING

import aiohttp
from kubernetes_asyncio import client
from kubernetes_asyncio.client import V1Lease
from kubernetes_asyncio.client.exceptions import ApiException

from . import batch_state, batch_utils
from .constants import BATCH_ID_LABEL, BATCH_LEASE_DURATION_ANNOTATION, BATCH_LEASE_NAME_PREFIX
from .exceptions import (
    BatchError,
    BatchInUseError,
    BatchLeaseExpiredError,
    BatchNotFoundError,
    SandboxNotReadyError,
)
from .models import BatchGroup, Member, MemberState

if TYPE_CHECKING:
    from .async_sandbox import AsyncSandbox
    from .async_sandbox_client import AsyncSandboxClient

logger = logging.getLogger(__name__)

# Timeout used by the apiserver to close the stream after this duration. This is used so the watcher
# task periodically has a chance to observe cancellation instead of blocking indefinitely.
_WATCH_TIMEOUT_SECONDS = 30

# Client-side timeout to handle connections that have died without a close, which timeout_seconds above would not
# be able to handle. Set longer than _WATCH_TIMEOUT_SECONDS so the apiserver ends a healthy stream first.
_WATCH_CLOSE_GRACE_SECONDS = 5
_WATCH_REQUEST_TIMEOUT = _WATCH_TIMEOUT_SECONDS + _WATCH_CLOSE_GRACE_SECONDS


def _watch_backoff_delay(failures: int) -> float:
    return batch_utils.backoff_delay(
        failures,
        batch_utils.BATCH_BACKOFF_BASE_SECONDS,
        batch_utils.BATCH_BACKOFF_MAX_SECONDS,
    )


class AsyncSandboxBatch:
    """An async handle to an existing batch's claims, obtained via ``AsyncSandboxClient.get_batch``.

    Keeps a live cache of the batch's claims through one label-scoped watch
    (a background asyncio task), renews the batch Lease on another task, and
    exposes methods for connecting to ready sandboxes and detaching from the batch.
    """

    def __init__(
        self,
        client: "AsyncSandboxClient",
        batch_id: str,
        namespace: str,
        state: batch_state.BatchState,
        lease_name: str,
        holder_identity: str,
        lease_duration: int,
        list_resource_version: str,
        lease: V1Lease,
        lease_renewed_at: float,
    ) -> None:
        self._client = client
        self.batch_id = batch_id
        self.namespace = namespace
        self._state = state
        self._lease_name = lease_name
        self._holder_identity = holder_identity
        self._lease_duration = lease_duration
        self._renew_interval = max(1, lease_duration // 3)
        self._list_resource_version = list_resource_version

        self._lock = asyncio.Lock()
        self._connected: dict[str, "AsyncSandbox"] = {}
        self._detached = False
        self._lease_released = False
        self._renewal_degraded = False
        # The Lease as this handle last wrote it, so renewal can replace it without reading it first.
        self._lease = lease
        # Monotonic time taken before the last successful write stamped renewTime. The Lease
        # expires a full Lease duration after it, which is no later than the stored renewTime.
        self._last_renew_sent = lease_renewed_at
        self._watch_task: asyncio.Task | None = None
        self._renew_task: asyncio.Task | None = None

    @property
    def groups(self) -> list[BatchGroup]:
        """The batch's ``BatchGroup``\\ s; one per warm pool."""
        return list(self._state.groups)

    @property
    def size(self) -> int:
        """The total number of claims across all groups."""
        return self._state.size

    @classmethod
    async def _attach(cls, client: "AsyncSandboxClient", batch_id: str, namespace: str) -> "AsyncSandboxBatch":
        """Attach to an existing batch and take over its Lease."""
        batch_utils.validate_batch_id(batch_id)

        lease_name = f"{BATCH_LEASE_NAME_PREFIX}{batch_id}"
        label_selector = f"{BATCH_ID_LABEL}={batch_id}"

        lease = await client.k8s_helper.read_batch_lease(lease_name, namespace)
        claim_items, list_rv = await client.k8s_helper.list_sandbox_claim_objects(namespace, label_selector)

        if lease is None and not claim_items:
            raise BatchNotFoundError(f"batch '{batch_id}' not found in namespace '{namespace}'")

        annotation = None
        if lease is not None:
            annotation = (lease.metadata.annotations or {}).get(BATCH_LEASE_DURATION_ANNOTATION)
        duration = batch_utils.parse_lease_duration_annotation(annotation)

        # Taken before now, which the takeover writes as renewTime, so this handle's expiry is
        # never later than the one the stored renewTime implies.
        renewed_at = time.monotonic()
        now = batch_utils.lease_now()
        spec_duration = lease.spec.lease_duration_seconds if lease is not None else None
        renew_time = lease.spec.renew_time if lease is not None else None
        if (
            lease is None
            or renew_time is None
            or spec_duration is None
            or batch_utils.is_lease_stale(renew_time, spec_duration, now)
        ):
            raise BatchLeaseExpiredError(
                f"batch '{batch_id}' Lease is missing or stale in namespace '{namespace}'"
            )

        if lease.spec.holder_identity is not None:
            raise BatchInUseError(
                f"batch '{batch_id}' is held by '{lease.spec.holder_identity}'"
            )

        groups = batch_state.reconstruct_groups(batch_id, claim_items)
        state = batch_state.BatchState(batch_id, groups)
        state.seed_from_claims(claim_items)

        holder_identity = batch_utils.generate_holder_identity()
        lease.spec.holder_identity = holder_identity
        lease.spec.renew_time = now
        lease.spec.lease_duration_seconds = duration
        lease.spec.acquire_time = now
        lease.spec.lease_transitions = (lease.spec.lease_transitions or 0) + 1
        try:
            written = await client.k8s_helper.replace_batch_lease(lease_name, namespace, lease)
        except ApiException as e:
            if e.status == 409:
                raise BatchInUseError(
                    f"batch '{batch_id}' Lease was taken over by another client while attaching"
                ) from e
            raise

        handle = cls(
            client=client,
            batch_id=batch_id,
            namespace=namespace,
            state=state,
            lease_name=lease_name,
            holder_identity=holder_identity,
            lease_duration=duration,
            list_resource_version=list_rv,
            lease=written,
            lease_renewed_at=renewed_at,
        )
        handle._start()
        return handle

    def _start(self) -> None:
        self._watch_task = asyncio.ensure_future(self._watch_loop())
        self._renew_task = asyncio.ensure_future(self._renew_loop())

    async def _stop_background_tasks(self) -> None:
        """Cancels the watcher and lease renewal loop."""
        for task in (self._watch_task, self._renew_task):
            if task is None:
                continue
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task
        self._watch_task = None
        self._renew_task = None

    def _check_active(self) -> None:
        if self._detached:
            raise BatchError(f"batch '{self.batch_id}' has been detached")

    def members(self, warmpool: str | None = None) -> list[Member]:
        """Returns a snapshot of the batch's members, optionally filtered to one warm pool."""
        return self._state.members(warmpool)

    async def connect(self, member: Member) -> "AsyncSandbox":
        """Returns a connected ``AsyncSandbox`` for a ready member. If this handle is already connected
        to the Sandbox, it reuses the existing connection, otherwise it creates a new one and caches it for future calls.
        
        Raises ``SandboxNotReadyError`` if the member isn't ready.
        """
        async with self._lock:
            self._check_active()
            cached = self._connected.get(member.claim_name)
            if cached is not None:
                return cached
            # No name resolution or existence check as the batch's watch cache already knows the member's sandbox_name
            current = self._state.get_member(member.claim_name)
            if current is None or current.state is not MemberState.READY:
                raise SandboxNotReadyError(
                    f"batch member '{member.claim_name}' is not ready"
                )
            sandbox = self._client.sandbox_class(
                claim_name=member.claim_name,
                sandbox_id=current.sandbox_name,
                namespace=self.namespace,
                connection_config=self._client.connection_config,
                tracer_config=self._client.tracer_config,
                k8s_helper=self._client.k8s_helper,
            )
            self._connected[member.claim_name] = sandbox
            return sandbox

    def err(self) -> Exception | None:
        """Returns the error that stopped the batch's watch or background
        Lease renewal (e.g., ``BatchLeaseExpiredError``), or ``None`` while healthy.
        """
        return self._state.error()

    def lease_degraded(self) -> bool:
        """Returns whether background Lease renewal is failing.

        ``True`` from the first failed renewal until a renewal succeeds. It stays ``True`` once
        renewal stops on an error, such as the Lease expiring, so it can be polled at any time.
        """
        return self._renewal_degraded

    async def detach(self, grace: int | None = None) -> None:
        """Stops the background watch and Lease renewal, closes cached sandbox connections, and
        releases this handle's hold on the Lease. ``grace`` is the number of seconds to keep the
        Lease valid after detaching, an int greater than ``CLOCK_SKEW_MARGIN`` (5); if ``None``, we
        use the batch's original Lease duration.

        If the Lease release fails, the error propagates and this handle stays in a detached state with
        the Lease still held. Call ``detach`` again to retry; steps that already completed are skipped.

        Raises:
            ValueError: If ``grace`` is not a valid Lease duration.
        """
        if grace is not None:
            batch_utils.validate_lease_duration_value(grace)

        # _detached prevents the caller from connecting to Sandboxes, while _lease_released signals that
        # detach fully completed (i.e. the previous steps and the Lease release write succeeded), so callers
        # can retry detach() if the release fails.
        async with self._lock:
            if self._lease_released:
                return
            self._detached = True

        await self._stop_background_tasks()

        async with self._lock:
            connected = list(self._connected.values())
            self._connected.clear()
        for sandbox in connected:
            try:
                await sandbox.close_connection()
            except Exception as e:
                logger.warning(
                    f"Batch '{self.batch_id}' failed to close a cached sandbox connection "
                    f"during detach: {e}"
                )

        lease = await self._client.k8s_helper.read_batch_lease(self._lease_name, self.namespace)
        if lease is not None:
            if lease.spec.holder_identity != self._holder_identity:
                logger.info(
                    f"Batch '{self.batch_id}' Lease is held by "
                    f"'{lease.spec.holder_identity}', not this handle; treating as released"
                )
            else:
                lease.spec.holder_identity = None
                lease.spec.renew_time = batch_utils.lease_now()
                # Refresh the Lease so it doesn't expire immediately after we release it,
                # giving other clients a chance to acquire it.
                lease.spec.lease_duration_seconds = (
                    grace if grace is not None else self._lease_duration
                )
                await self._client.k8s_helper.replace_batch_lease(
                    self._lease_name, self.namespace, lease
                )

        async with self._lock:
            self._lease_released = True

        self._client._unregister_batch(self.namespace, self.batch_id)

    async def _watch_loop(self) -> None:
        """Background watch loop which keeps ``self._state`` in sync with the batch's claims.

        kubernetes_asyncio's informer/reflector helpers don't support custom resources, so
        this manually implements one where the batch state is seeded from a list,
        then kept up to date with watch events using the list's resourceVersion so no events are missed.
        """
        rv = self._list_resource_version
        label_selector = f"{BATCH_ID_LABEL}={self.batch_id}"
        relist = False
        # Consecutive failed attempts, which set the retry backoff.
        failures = 0
        while True:
            try:
                if relist:
                    items, rv = await self._client.k8s_helper.list_sandbox_claim_objects(
                        self.namespace, label_selector
                    )
                    async with self._lock:
                        self._state.resync_from_list(items)
                    relist = False
                async for event in self._client.k8s_helper.watch_sandbox_claims(
                    self.namespace, label_selector, rv, _WATCH_TIMEOUT_SECONDS, _request_timeout=_WATCH_REQUEST_TIMEOUT
                ):
                    failures = 0
                    event_type = event.get("type")
                    obj = event.get("object") or {}
                    seen_rv = (obj.get("metadata") or {}).get("resourceVersion")
                    if seen_rv:
                        rv = seen_rv
                    if event_type == "BOOKMARK":
                        continue
                    async with self._lock:
                        if event_type == "DELETED":
                            self._state.mark_lost((obj.get("metadata") or {}).get("name", ""))
                        elif event_type in ("ADDED", "MODIFIED"):
                            self._state.upsert_claim(obj)
                # The watch ended at its timeout, which is not a failure.
                failures = 0
            except asyncio.CancelledError:
                raise
            except client.ApiException as e:
                # Indicates etcd RV compaction aged our resourceVersion out of the apiserver's watch cache,
                # so the watch can't continue. Re-list, reconcile the cache (catches events that occurred while there
                # was no watch), and continue watching from the new list's resourceVersion.
                if e.status == 410:
                    relist = True
                    continue
                if batch_utils.is_retryable_status(e.status):
                    # Likely transient: retry the re-list, or the watch from the last-seen resourceVersion.
                    failures += 1
                    await asyncio.sleep(_watch_backoff_delay(failures))
                    continue
                # Response codes outside of this (e.g. 403, 404) are not recoverable
                # by retrying, so we error then exit
                async with self._lock:
                    self._state.note_error(e)
                return
            except aiohttp.ClientSSLError as e:
                # aiohttp.ClientSSLError is a subclass of aiohttp.ClientConnectionError, so it
                # must be caught ahead of the tuple below. It is not known to be transient
                # so surface it via err() instead of retrying.
                async with self._lock:
                    self._state.note_error(e)
                return
            except (
                aiohttp.ClientConnectionError,
                aiohttp.ClientPayloadError,
                ConnectionError,
            ):
                # Likely transient network-level failure so retry
                failures += 1
                await asyncio.sleep(_watch_backoff_delay(failures))
                continue
            except Exception as e:
                # Anything else is not known to be transient either, so surface it via err().
                async with self._lock:
                    self._state.note_error(e)
                return

    async def _renew_loop(self) -> None:
        """Background lease renewal loop: renews at roughly 1/3 of lease duration
        so a couple of missed renewals in a row still leave margin before the Lease goes stale.

        The Lease expires a full Lease duration after the last successful renewal was sent. The
        loop wakes at that deadline even when the next renewal isn't due yet, so ``err()`` reports
        the expiry on time, and then stops renewing. A renewal that succeeded after the expiry
        would keep the Lease fresh for a handle that has already reported it lost.
        """
        while True:
            deadline = self._last_renew_sent + self._lease_duration
            await asyncio.sleep(max(0.0, min(self._renew_interval, deadline - time.monotonic())))
            if time.monotonic() >= deadline:
                await self._stop_renewal(
                    BatchLeaseExpiredError(
                        f"batch '{self.batch_id}' lease has not renewed successfully "
                        f"for {self._lease_duration}s"
                    )
                )
                return
            if not await self._renew_once():
                return

    def _mark_renewal_degraded(self, reason: object) -> None:
        """Sets ``_renewal_degraded``, logging the first failure of each episode. Requires ``_lock``."""
        if not self._renewal_degraded:
            logger.info(f"Batch '{self.batch_id}' lease renewal degraded: {reason}")
            self._renewal_degraded = True

    def _renew_request_timeout(self) -> float:
        """Bounds a renewal request by the renew interval and by the time left before expiry."""
        left = self._last_renew_sent + self._lease_duration - time.monotonic()
        if left <= 0:
            raise TimeoutError("the Lease expired before the renewal request was sent")
        return min(self._renew_interval, left)

    async def _renew_once(self) -> bool:
        """Does a single attempt to renew a Lease, and sets ``_renewal_degraded`` if it fails.

        Replaces the Lease this handle last wrote without reading it first. Only a conflict means
        the Lease changed since, so only then is it read to find out whether this handle still
        holds it.

        Returns ``False`` if another holder has taken the Lease or the Lease no longer exists,
        meaning this handle must stop renewing rather than overwrite it.
        """
        try:
            try:
                sent_at, written = await self._write_renewal(self._lease)
            except ApiException as e:
                if e.status == 404:
                    return await self._stop_renewal(self._lease_gone_error())
                if e.status != 409:
                    raise
                # A conflict means another holder took the Lease, or an earlier renewal whose
                # response was lost did land, so read it to tell the two apart.
                lease = await self._client.k8s_helper.read_batch_lease(
                    self._lease_name, self.namespace, _request_timeout=self._renew_request_timeout()
                )
                if lease is None:
                    return await self._stop_renewal(self._lease_gone_error())
                if lease.spec.holder_identity != self._holder_identity:
                    return await self._stop_renewal(
                        BatchInUseError(
                            f"batch '{self.batch_id}' Lease is no longer held by this handle "
                            f"(current holder: {lease.spec.holder_identity!r})"
                        )
                    )
                sent_at, written = await self._write_renewal(lease)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            async with self._lock:
                self._mark_renewal_degraded(e)
            return True
        async with self._lock:
            self._lease = written
            self._last_renew_sent = sent_at
            self._renewal_degraded = False
        return True

    async def _stop_renewal(self, error: BatchError) -> bool:
        """Records why this handle stops renewing the Lease. Returns ``False`` for ``_renew_once``."""
        async with self._lock:
            self._mark_renewal_degraded(error)
            self._state.note_error(error)
        return False

    async def _write_renewal(self, lease) -> tuple[float, V1Lease]:
        """Stamps ``lease`` as renewed now and replaces it, returning the send time and the written Lease."""
        # Neither Kubernetes client has a default request timeout, so a hung apiserver would
        # otherwise stall renewal past the Lease's expiry.
        timeout = self._renew_request_timeout()
        sent_at = time.monotonic()
        lease.spec.holder_identity = self._holder_identity
        lease.spec.renew_time = batch_utils.lease_now()
        lease.spec.lease_duration_seconds = self._lease_duration
        written = await self._client.k8s_helper.replace_batch_lease(
            self._lease_name, self.namespace, lease, _request_timeout=timeout
        )
        return sent_at, written

    def _lease_gone_error(self) -> BatchLeaseExpiredError:
        return BatchLeaseExpiredError(
            f"batch '{self.batch_id}' Lease '{self._lease_name}' no longer exists"
        )
