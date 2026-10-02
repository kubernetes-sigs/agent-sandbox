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
from collections import deque
from datetime import UTC, datetime
from typing import TYPE_CHECKING

import aiohttp
from kubernetes_asyncio import client
from kubernetes_asyncio.client import V1Lease, V1LeaseSpec, V1ObjectMeta
from kubernetes_asyncio.client.exceptions import ApiException

from . import batch_state, batch_utils
from .constants import (
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    BATCH_ID_LABEL,
    BATCH_LEASE_DURATION_ANNOTATION,
    BATCH_LEASE_NAME_PREFIX,
)
from .exceptions import (
    BatchError,
    BatchExistsError,
    BatchInUseError,
    BatchLeaseExpiredError,
    BatchNotFoundError,
    SandboxNotReadyError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
)
from .models import BatchGroup, Member
from .utils import construct_sandbox_claim_lifecycle_spec

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

# Network-level errors that are likely transient, so the watch, creates, and release retry them.
_TRANSPORT_ERRORS = (
    aiohttp.ClientConnectionError,
    aiohttp.ClientPayloadError,
    ConnectionError,
    asyncio.TimeoutError,
)


def _is_transport_error(error: BaseException) -> bool:
    # aiohttp.ClientSSLError subclasses ClientConnectionError but isn't transient
    return isinstance(error, _TRANSPORT_ERRORS) and not isinstance(error, aiohttp.ClientSSLError)


def _is_transient_error(error: Exception) -> bool:
    if isinstance(error, ApiException):
        return batch_utils.is_retryable_status(error.status)
    return _is_transport_error(error)


def _watch_backoff_delay(failures: int) -> float:
    return batch_utils.backoff_delay(
        failures,
        batch_utils.BATCH_BACKOFF_BASE_SECONDS,
        batch_utils.BATCH_BACKOFF_MAX_SECONDS,
    )


class AsyncSandboxBatch:
    """An async handle to a batch's claims, obtained via ``AsyncSandboxClient.claim_batch`` or
    ``AsyncSandboxClient.get_batch``.

    Keeps a live cache of the batch's claims through one label-scoped watch
    (a background asyncio task), renews the batch Lease on another task, and
    exposes methods for connecting to ready sandboxes and detaching from the batch.
    A claimed batch also creates its claims on background worker tasks.
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
        self._last_renew_success = time.monotonic()
        self._watch_task: asyncio.Task | None = None
        self._renew_task: asyncio.Task | None = None

        self._released = False
        self._creators: set[asyncio.Task] = set()
        self._stop_creating = asyncio.Event()
        self._create_plan: deque[tuple[str, BatchGroup]] = deque()
        self._next_create_at = 0.0
        self._create_interval = 0.0
        self._claim_labels: dict[str, str] = {}
        self._claim_annotations: dict[str, dict[str, str]] = {}
        self._claim_shutdown_after_seconds = 0

    @property
    def groups(self) -> list[BatchGroup]:
        """The batch's ``BatchGroup``\\ s; one per warm pool."""
        return list(self._state.groups)

    @property
    def size(self) -> int:
        """The total number of claims across all groups."""
        return self._state.size

    @classmethod
    async def _claim(
        cls,
        client: "AsyncSandboxClient",
        args: batch_utils.ClaimBatchArgs,
        namespace: str,
        trace_annotations: dict[str, str],
    ) -> "AsyncSandboxBatch":
        """Create a batch: precheck, create its Lease, start the watch, then create claims in the background."""
        helper = client.k8s_helper
        timeout = batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        checked_templates: set[str] = set()
        for group in args.groups:
            warmpool = await helper.get_sandbox_warmpool(group.warmpool, namespace, _request_timeout=timeout)
            if warmpool is None:
                raise SandboxWarmPoolNotFoundError(
                    f"SandboxWarmPool '{group.warmpool}' not found in namespace '{namespace}'"
                )
            template = batch_utils.warmpool_template_name(warmpool)
            if template is None:
                raise SandboxTemplateNotFoundError(
                    f"SandboxWarmPool '{group.warmpool}' does not reference a SandboxTemplate"
                )
            if template in checked_templates:
                continue
            if await helper.get_sandbox_template(template, namespace, _request_timeout=timeout) is None:
                raise SandboxTemplateNotFoundError(
                    f"SandboxTemplate '{template}' not found in namespace '{namespace}'"
                )
            checked_templates.add(template)

        lease_name = f"{BATCH_LEASE_NAME_PREFIX}{args.batch_id}"
        label_selector = f"{BATCH_ID_LABEL}={args.batch_id}"
        holder_identity = batch_utils.generate_holder_identity()
        lease_labels, lease_annotations = batch_utils.batch_lease_metadata(
            args.batch_id, args.lease_duration, args.work_budget, args.quorum_timeout
        )
        now = datetime.now(UTC)
        lease = V1Lease(
            metadata=V1ObjectMeta(name=lease_name, labels=lease_labels, annotations=lease_annotations),
            spec=V1LeaseSpec(
                holder_identity=holder_identity,
                lease_duration_seconds=args.lease_duration,
                acquire_time=now,
                renew_time=now,
                lease_transitions=0,
            ),
        )
        try:
            await helper.create_batch_lease(namespace, lease, _request_timeout=timeout)
        except ApiException as e:
            if e.status == 409:
                raise BatchExistsError(
                    f"batch '{args.batch_id}' already exists in namespace '{namespace}'"
                ) from e
            raise
        try:
            # The list's resourceVersion is where the watch starts, so no create is missed.
            claim_items, list_rv = await helper.list_sandbox_claim_objects(
                namespace, label_selector, _request_timeout=timeout
            )
            if claim_items:
                raise BatchExistsError(
                    f"claims with batch id '{args.batch_id}' already exist in namespace '{namespace}'"
                )
        except Exception:
            try:
                await helper.delete_batch_lease(lease_name, namespace, _request_timeout=timeout)
            except Exception as e:
                logger.warning(f"Batch '{args.batch_id}' failed to delete its Lease: {e}")
            raise

        handle = cls(
            client=client,
            batch_id=args.batch_id,
            namespace=namespace,
            state=batch_state.BatchState(args.batch_id, args.groups),
            lease_name=lease_name,
            holder_identity=holder_identity,
            lease_duration=args.lease_duration,
            list_resource_version=list_rv,
        )
        handle._create_plan = deque(
            (f"{args.batch_id}-{ordinal}", group)
            for ordinal, group in enumerate(g for g in args.groups for _ in range(g.size))
        )
        handle._create_interval = 1 / args.create_rps
        handle._claim_labels = {**args.labels, BATCH_ID_LABEL: args.batch_id}
        handle._claim_annotations = {
            g.warmpool: {
                BATCH_GROUP_SIZE_ANNOTATION: str(g.size),
                BATCH_GROUP_MIN_READY_ANNOTATION: str(g.min_ready),
                **trace_annotations,
            }
            for g in args.groups
        }
        handle._claim_shutdown_after_seconds = (
            args.quorum_timeout + args.work_budget + batch_utils.BATCH_SHUTDOWN_MARGIN_SECONDS
        )
        handle._start()
        for _ in range(min(args.max_in_flight, len(handle._create_plan))):
            handle._creators.add(asyncio.ensure_future(handle._create_worker()))
        return handle

    @classmethod
    async def _attach(cls, client: "AsyncSandboxClient", batch_id: str, namespace: str) -> "AsyncSandboxBatch":
        """Attach to an existing batch and take over its Lease."""
        batch_utils.validate_batch_id(batch_id)

        lease_name = f"{BATCH_LEASE_NAME_PREFIX}{batch_id}"
        label_selector = f"{BATCH_ID_LABEL}={batch_id}"

        lease = await client.k8s_helper.read_batch_lease(
            lease_name, namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        )
        claim_items, list_rv = await client.k8s_helper.list_sandbox_claim_objects(
            namespace, label_selector, _request_timeout=batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
        )

        if lease is None and not claim_items:
            raise BatchNotFoundError(f"batch '{batch_id}' not found in namespace '{namespace}'")

        annotation = None
        if lease is not None:
            annotation = (lease.metadata.annotations or {}).get(BATCH_LEASE_DURATION_ANNOTATION)
        duration = batch_utils.parse_lease_duration_annotation(annotation)

        now = datetime.now(UTC)
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

        groups = batch_state.reconstruct_groups(claim_items)
        state = batch_state.BatchState(batch_id, groups)
        state.seed_from_claims(claim_items)

        holder_identity = batch_utils.generate_holder_identity()
        lease.spec.holder_identity = holder_identity
        lease.spec.renew_time = now
        lease.spec.lease_duration_seconds = duration
        lease.spec.acquire_time = now
        lease.spec.lease_transitions = (lease.spec.lease_transitions or 0) + 1
        try:
            await client.k8s_helper.replace_batch_lease(
                lease_name, namespace, lease, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
            )
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
        )
        handle._start()
        return handle

    def _start(self) -> None:
        self._watch_task = asyncio.ensure_future(self._watch_loop())
        self._renew_task = asyncio.ensure_future(self._renew_loop())

    async def _stop_background_tasks(self) -> None:
        """Stops claim creators and cancels the watcher and lease renewal loop."""
        self._stop_creating.set()
        if self._creators:
            # Pause to ensure each in-progress create request has been responded to
            _, pending = await asyncio.wait(
                self._creators, timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS + batch_utils.BATCH_CREATOR_STOP_GRACE_SECONDS
            )
            for creator in pending:
                creator.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await creator
            self._creators.clear()
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
            raise BatchError(f"batch '{self.batch_id}' has been detached or released")

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
            if current is None or not current.ready:
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

    async def release(self) -> None:
        """Stops creating claims, stops the background watch and Lease renewal, closes cached
        sandbox connections, then deletes the batch's claims and finally its Lease. Idempotent.

        Claims are deleted with label-scoped deletecollection requests, re-listing between rounds
        until no claim is left that isn't already being deleted. A transient error moves on to the
        next round, and release gives up after ``BATCH_RELEASE_MAX_IDLE_ROUNDS`` rounds in a row
        that delete nothing. On failure the Lease is kept, and calling ``release`` again retries.

        Raises:
            BatchError: If this handle has been detached, or claims are still not being deleted
                after the last round.
            ApiException: For a non-transient error such as a 403, which is raised at once.
                After the last round, the last transient error is raised (an ``ApiException``
                or a transport error).
        """
        async with self._lock:
            if self._released:
                return
            if self._lease_released:
                raise BatchError(f"batch '{self.batch_id}' has been detached")
            self._detached = True

        await self._stop_background_tasks()
        await self._close_cached_connections("release")
        await _delete_batch_objects(
            self._client.k8s_helper, self.namespace, self.batch_id, self._lease_name
        )

        async with self._lock:
            self._lease_released = True
            self._released = True
        self._client._unregister_batch(self.namespace, self.batch_id)

    async def _close_cached_connections(self, action: str) -> None:
        async with self._lock:
            connected = list(self._connected.values())
            self._connected.clear()
        for sandbox in connected:
            try:
                await sandbox.close_connection()
            except Exception as e:
                logger.warning(
                    f"Batch '{self.batch_id}' failed to close a cached sandbox connection "
                    f"during {action}: {e}"
                )

    async def detach(self, grace: int | None = None) -> None:
        """Stops the background watch and Lease renewal, closes cached sandbox connections, and
        releases this handle's hold on the Lease. ``grace`` is the number of seconds to keep the
        Lease valid after detaching; if ``None``, we use the batch's original Lease duration.

        If the Lease release fails, the error propagates and this handle stays in a detached state with
        the Lease still held. Call ``detach`` again to retry; steps that already completed are skipped.
        A batch that is still creating claims stops creating first, and every claim is left in place.

        Raises:
            ValueError: If ``grace`` is not a valid Lease duration.
            BatchError: If this handle has been released.
        """
        if grace is not None:
            batch_utils.validate_lease_duration_value(grace)

        # _detached prevents the caller from connecting to Sandboxes, while _lease_released signals that
        # detach fully completed (i.e. the previous steps and the Lease release write succeeded), so callers
        # can retry detach() if the release fails.
        async with self._lock:
            if self._released:
                raise BatchError(f"batch '{self.batch_id}' has been released")
            if self._lease_released:
                return
            self._detached = True

        await self._stop_background_tasks()
        await self._close_cached_connections("detach")

        lease = await self._client.k8s_helper.read_batch_lease(
            self._lease_name, self.namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        )
        if lease is not None:
            if lease.spec.holder_identity != self._holder_identity:
                logger.info(
                    f"Batch '{self.batch_id}' Lease is held by "
                    f"'{lease.spec.holder_identity}', not this handle; treating as released"
                )
            else:
                lease.spec.holder_identity = None
                lease.spec.renew_time = datetime.now(UTC)
                # Refresh the Lease so it doesn't expire immediately after we release it,
                # giving other clients a chance to acquire it.
                lease.spec.lease_duration_seconds = (
                    grace if grace is not None else self._lease_duration
                )
                await self._client.k8s_helper.replace_batch_lease(
                    self._lease_name,
                    self.namespace,
                    lease,
                    _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
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
                        self.namespace, label_selector, _request_timeout=batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
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
            except _TRANSPORT_ERRORS:
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
        """
        while True:
            await asyncio.sleep(self._renew_interval)
            if not await self._renew_once():
                return

    async def _renew_once(self) -> bool:
        """Does a single attempt to renew a Lease. A transient failure sets ``_renewal_degraded``
        and logs, and if the Lease has not been successfully renewed for a full Lease duration,
        sets a batch-level error.

        Returns ``False`` if another holder has taken the Lease or the Lease no longer exists,
        meaning this handle must stop renewing rather than overwrite it.
        """
        try:
            # Neither Kubernetes client has a default request timeout. Bounding each request by the
            # renewal interval makes a hung apiserver count as a failed renewal.
            lease = await self._client.k8s_helper.read_batch_lease(
                self._lease_name, self.namespace, _request_timeout=self._renew_interval
            )
            if lease is None:
                async with self._lock:
                    self._state.note_error(
                        BatchLeaseExpiredError(
                            f"batch '{self.batch_id}' Lease '{self._lease_name}' no longer exists"
                        )
                    )
                return False
            if lease.spec.holder_identity != self._holder_identity:
                async with self._lock:
                    self._state.note_error(
                        BatchInUseError(
                            f"batch '{self.batch_id}' Lease is no longer held by this handle "
                            f"(current holder: {lease.spec.holder_identity!r})"
                        )
                    )
                return False
            lease.spec.holder_identity = self._holder_identity
            lease.spec.renew_time = datetime.now(UTC)
            lease.spec.lease_duration_seconds = self._lease_duration
            await self._client.k8s_helper.replace_batch_lease(
                self._lease_name, self.namespace, lease, _request_timeout=self._renew_interval
            )
        except asyncio.CancelledError:
            raise
        except Exception as e:
            async with self._lock:
                if not self._renewal_degraded:
                    logger.info(f"Batch '{self.batch_id}' lease renewal degraded: {e}")
                    self._renewal_degraded = True
                # time.monotonic() is a clock that only ever moves forward, so unlike datetime.now(),
                # it can't jump backward from an NTP correction or a system clock change.
                if time.monotonic() - self._last_renew_success >= self._lease_duration:
                    self._state.note_error(
                        BatchLeaseExpiredError(
                            f"batch '{self.batch_id}' lease has not renewed successfully "
                            f"for {self._lease_duration}s"
                        )
                    )
            return True
        async with self._lock:
            self._last_renew_success = time.monotonic()
            self._renewal_degraded = False
        return True

    async def _create_worker(self) -> None:
        """Creates planned claims one at a time. ``max_in_flight`` workers share one pacing slot,
        so together they start at most ``create_rps`` creates per second.
        """
        while True:
            async with self._lock:
                if not self._create_plan or self._stop_creating.is_set():
                    return
                claim_name, group = self._create_plan.popleft()
                slot = max(time.monotonic(), self._next_create_at)
                self._next_create_at = slot + self._create_interval
            if await self._wait_for_stop(slot - time.monotonic()):
                return
            await self._create_one(claim_name, group)

    async def _wait_for_stop(self, timeout: float) -> bool:
        """Waits up to ``timeout`` seconds for creation to be stopped, and returns whether it was."""
        if self._stop_creating.is_set() or timeout <= 0:
            return self._stop_creating.is_set()
        with contextlib.suppress(asyncio.TimeoutError):
            await asyncio.wait_for(self._stop_creating.wait(), timeout)
        return self._stop_creating.is_set()

    async def _create_one(self, claim_name: str, group: BatchGroup) -> None:
        attempt = 1
        while True:
            try:
                await self._client.k8s_helper.create_sandbox_claim(
                    claim_name,
                    group.warmpool,
                    self.namespace,
                    annotations=self._claim_annotations[group.warmpool],
                    labels=self._claim_labels,
                    lifecycle=construct_sandbox_claim_lifecycle_spec(self._claim_shutdown_after_seconds),
                    log_level=logging.DEBUG,
                    _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                )
                return
            except asyncio.CancelledError:
                raise
            except Exception as e:
                if isinstance(e, ApiException):
                    outcome = batch_utils.create_error_outcome(e.status, attempt)
                elif _is_transport_error(e):
                    outcome = batch_utils.create_error_outcome(None, attempt)
                else:
                    outcome = batch_utils.CreateOutcome.FAIL
                if outcome == batch_utils.CreateOutcome.SUCCESS:
                    return
                if outcome == batch_utils.CreateOutcome.RETRY:
                    if await self._wait_for_stop(batch_utils.retry_delay(attempt, e)):
                        return
                    attempt += 1
                    continue
                logger.debug(f"Batch '{self.batch_id}' failed to create claim '{claim_name}': {e}")
                async with self._lock:
                    self._state.record_create_failure(claim_name, group.warmpool, str(e))
                return


async def _delete_batch_objects(helper, namespace: str, batch_id: str, lease_name: str) -> None:
    """Deletes a batch's claims in rounds, then its Lease once no claim is left that isn't
    already being deleted.

    Raises:
        BatchError: If claims are still not being deleted after the last round.
        ApiException: For a non-transient error at once, or the last transient error after
            the last round.
    """
    label_selector = f"{BATCH_ID_LABEL}={batch_id}"
    collection_timeout = batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
    idle_rounds = 0
    remaining_before: int | None = None
    while True:
        last_error: Exception | None = None
        try:
            # deletecollection deletes one claim at a time on the apiserver, so for a large batch
            # it can time out while deletion continues. For this case, we track it with the list below.
            await helper.delete_sandbox_claims_by_label(
                namespace, label_selector, _request_timeout=collection_timeout
            )
        except Exception as e:
            if not _is_transient_error(e):
                raise
            last_error = e

        live: list[dict] | None = None
        try:
            # A consistent read (no resourceVersion), so a lagging watch cache can't report
            # that no claims are left too early.
            items, _ = await helper.list_sandbox_claim_objects(
                namespace, label_selector, _request_timeout=collection_timeout
            )
            live = [c for c in items if not (c.get("metadata") or {}).get("deletionTimestamp")]
        except Exception as e:
            if not _is_transient_error(e):
                raise
            last_error = e

        if live == []:
            try:
                await helper.delete_batch_lease(
                    lease_name, namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
                )
                return
            except Exception as e:
                if not _is_transient_error(e):
                    raise
                last_error = e

        # A round that shrinks the number of live claims doesn't count toward giving up, so a large
        # batch that takes many rounds to delete isn't abandoned while it is still making progress.
        progressed = live is not None and (remaining_before is None or len(live) < remaining_before)
        idle_rounds = 0 if progressed else idle_rounds + 1
        if idle_rounds >= batch_utils.BATCH_RELEASE_MAX_IDLE_ROUNDS:
            if last_error is not None:
                raise last_error
            raise BatchError(
                f"batch '{batch_id}': {len(live or [])} claims are still not being deleted"
            )
        if live is not None:
            remaining_before = len(live)
        await asyncio.sleep(batch_utils.retry_delay(max(idle_rounds, 1), last_error))
