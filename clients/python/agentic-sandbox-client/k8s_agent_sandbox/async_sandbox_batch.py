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
import math
import ssl
import time
from collections import deque
from collections.abc import AsyncIterator, Awaitable, Callable, Iterable
from functools import partial
from typing import TYPE_CHECKING, Any, TypeVar, cast

import aiohttp
from kubernetes_asyncio import client
from kubernetes_asyncio.client import V1Lease, V1LeaseSpec, V1ObjectMeta
from kubernetes_asyncio.client.exceptions import ApiException

from . import batch_state, batch_utils
from .constants import (
    BATCH_GROUP_LABEL,
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    BATCH_ID_LABEL,
    BATCH_LEASE_NAME_PREFIX,
    BATCH_LEASE_UID_LABEL,
    BATCH_NEXT_ORDINAL_ANNOTATION,
)
from .exceptions import (
    BatchError,
    BatchExistsError,
    BatchInUseError,
    BatchLeaseExpiredError,
    BatchNotFoundError,
    BatchTimeoutError,
    SandboxNotReadyError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
    TerminalMemberError,
)
from .models import AdditionalPodMetadata, BatchEvent, BatchGroup, GroupOutcome, Member, MemberState
from .pod_metadata import build_pod_metadata
from .utils import construct_sandbox_claim_lifecycle_spec, select_pod_ip

if TYPE_CHECKING:
    from .async_k8s_helper import AsyncK8sHelper
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

# A watch that ends sooner than this without an event counts as a failed attempt, as in client-go's
# reflector. Otherwise a stream that a proxy or a broken connection ends at once would be reopened in a busy loop.
_SHORT_WATCH_SECONDS = 1

_T = TypeVar("_T")


# Network-level errors that are likely transient, so the watch, creates, and release retry them.
_TRANSPORT_ERRORS = (
    aiohttp.ClientConnectionError,
    aiohttp.ClientPayloadError,
    ConnectionError,
    asyncio.TimeoutError,
    # A peer that closes the TLS connection early (e.g. a load balancer during a control-plane upgrade)
    # is transient like a connection reset. Certificate and other TLS failures are not.
    ssl.SSLEOFError,
)


def _is_transport_error(error: BaseException) -> bool:
    # BatchTimeoutError subclasses TimeoutError, so exclude SDK errors before checking TimeoutError.
    if isinstance(error, BatchError):
        return False
    # A TLS failure is judged by the ssl error it wraps. aiohttp.ClientSSLError is itself a
    # ClientConnectionError, so it must be unwrapped before the tuple check.
    if isinstance(error, aiohttp.ClientSSLError):
        error = error.os_error
    return isinstance(error, _TRANSPORT_ERRORS)


def _is_transient_error(error: Exception) -> bool:
    if isinstance(error, ApiException):
        return batch_utils.is_retryable_status(error.status)
    return _is_transport_error(error)


def _create_failure_message(error: Exception) -> str:
    # str() of an ApiException holds every response header and the raw body.
    if isinstance(error, ApiException):
        return batch_utils.api_status_message(error.body, error.reason)
    return str(error)


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
        client: "AsyncSandboxClient[Any]",
        batch_id: str,
        namespace: str,
        state: batch_state.BatchState,
        lease_name: str,
        holder_identity: str,
        lease_duration: int,
        list_resource_version: str,
        lease: V1Lease,
        lease_renewed_at: float,
        quorum_timeout: int = batch_utils.BATCH_DEFAULT_QUORUM_TIMEOUT_SECONDS,
        work_budget: int = batch_utils.BATCH_DEFAULT_WORK_BUDGET_SECONDS,
        max_in_flight: int = batch_utils.BATCH_DEFAULT_MAX_IN_FLIGHT,
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
        # Serializes release()'s teardown and detach()'s Lease writes, so whichever enters first
        # finishes before the other acts on the Lease.
        self._lifecycle_lock = asyncio.Lock()
        # Each cached connection, with the pod IP it was made to.
        self._connected: dict[str, tuple["AsyncSandbox", str | None]] = {}
        # The watch moves a cached connection here when its member is gone or runs on another sandbox
        # or pod IP. connect() reads only _connected, so it never hands out these connections. Each
        # connection stays here until it closes, so the next cleanup retries a close that failed.
        self._stale_connections: list["AsyncSandbox"] = []
        # get_batch sets this, so client exit cleanup detaches the handle instead of deleting a batch that
        # another client may own.
        self._attached = False
        self._detached = False
        self._lease_released = False
        # The Lease duration detach() writes, the grace from the last detach() call that gave one or else
        # the batch's Lease duration, kept so that a retry without a grace, including the one client
        # cleanup makes, writes the same one.
        self._detach_grace: int | None = None
        self._renewal_degraded = False
        # Why this handle lost the Lease (expiry, takeover, or deletion). It stays separate from
        # watch errors, which also set err(), because a watch error leaves the Lease held, so
        # renewal and claim cleanup keep running.
        self._lease_error: BatchError | None = None
        # The Lease from this handle's last successful write. Renewal sends it back without reading
        # the Lease first, and only reads it if the write conflicts.
        self._lease = lease
        # The claims this handle creates are owned by its Lease, so the garbage collector deletes
        # them once the Lease is gone, even if no reaper or release() deletes them.
        self._owner_references = [batch_utils.lease_owner_reference(lease)]
        self._lease_uid: str = self._owner_references[0]["uid"]
        # When this handle considers its Lease expired. It's a little earlier than the expiry other
        # clients see, so the handle never thinks it holds the batch after someone else could take it.
        self._lease_expires_at = lease_renewed_at + lease_duration
        self._watch_task: asyncio.Task[None] | None = None
        self._renew_task: asyncio.Task[None] | None = None
        # Deletes the watch starts for claims that land after their pool was released, tracked so
        # detach() and release() wait for them.
        self._watch_delete_tasks: set[asyncio.Task[None]] = set()

        # Notified on every change consumers wait for: watch events, create failures, errors,
        # the fill deadline being set, and detach/release.
        self._changed = asyncio.Condition(self._lock)
        self._quorum_timeout = quorum_timeout
        self._work_budget = work_budget
        # Monotonic time at which the fill expires. After it, groups without an outcome time out
        # and events() stops waiting for fill members that aren't Ready yet. Set once the last
        # create's pacing slot is taken, or at attach for a re-attached batch.
        self._fill_deadline: float | None = None
        self._last_fill_slot: float = 0.0
        self._released = False
        self._creators: set[asyncio.Task[None]] = set()
        self._stop_creating = asyncio.Event()
        self._create_plan: deque[tuple[str, BatchGroup]] = deque()
        self._next_create_at = 0.0
        self._create_interval = 0.0
        self._warm_credit = batch_utils.WarmCredit()
        # Bounds concurrent claim creates (fill and acquire()), acquire() pool reads, and member
        # deletes to max_in_flight, matching the HTTP connection pool budget that claim_batch checks.
        self._in_flight = asyncio.BoundedSemaphore(max_in_flight)
        # Counts in-flight member and group delete requests so detach(), release(), and cleanup
        # wait for them to finish before stopping renewal, giving up the Lease, or closing the
        # API session.
        self._active_deletes = 0
        self._no_active_deletes = asyncio.Event()
        self._no_active_deletes.set()
        # One per pool, so concurrent acquires wait for one read of the pool's Ready count.
        self._credit_refresh_locks: dict[str, asyncio.Lock] = {}
        # The caller's labels, batch id label, and Lease UID label, which every claim carries.
        self._labels: dict[str, str] = {
            BATCH_ID_LABEL: batch_id,
            BATCH_LEASE_UID_LABEL: self._lease_uid,
        }
        # Pools acquire() doesn't check again, since claim_batch checked them or they already have claims.
        self._checked_pools: set[str] = {g.warmpool for g in state.groups}
        # The check of each pool that is running, so concurrent first acquires of a pool share it.
        self._prechecks: dict[str, asyncio.Future[None]] = {}
        # Set when an in-flight acquire()'s claim resolves or the batch stops, so watch events for
        # other claims don't wake every waiting acquire().
        self._acquire_waiters: dict[str, asyncio.Event] = {}
        self._claim_labels: dict[str, dict[str, str]] = {}
        self._claim_annotations: dict[str, dict[str, str]] = {}
        self._claim_lifecycle: dict[str, str] = {}

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
        client: "AsyncSandboxClient[Any]",
        args: batch_utils.ClaimBatchArgs,
        namespace: str,
        trace_annotations: dict[str, str],
    ) -> "AsyncSandboxBatch":
        """Create a batch: precheck, create its Lease, start the watch, then create claims in the background."""
        helper = client.k8s_helper
        timeout = batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        ready = await _precheck_pools(helper, [g.warmpool for g in args.groups], namespace)

        size = sum(g.size for g in args.groups)
        # One shutdownTime for the whole fill. Quorum can come as late as quorum_timeout after the
        # last paced create, so the pacing time counts too, or it would eat into work_budget.
        claim_lifecycle = construct_sandbox_claim_lifecycle_spec(
            math.ceil(size / args.create_rps)
            + args.quorum_timeout
            + args.work_budget
            + batch_utils.BATCH_SHUTDOWN_MARGIN_SECONDS
        )

        lease_name = f"{BATCH_LEASE_NAME_PREFIX}{args.batch_id}"
        label_selector = batch_utils.batch_claim_label_selector(args.batch_id)
        holder_identity = batch_utils.generate_holder_identity()
        lease_labels, lease_annotations = batch_utils.batch_lease_metadata(args)
        # Read before renewTime is stamped, so this handle's expiry is never later than the one
        # other clients see.
        renewed_at = time.monotonic()
        now = batch_utils.lease_now()
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
            created = await helper.create_batch_lease(namespace, lease, _request_timeout=timeout)
        except ApiException as e:
            if e.status == 409:
                raise BatchExistsError(
                    f"batch '{args.batch_id}' already exists in namespace '{namespace}'"
                ) from e
            raise
        created_meta, _ = batch_utils.lease_parts(created)
        created_rv = created_meta.resource_version
        if not created_rv:
            raise BatchError(
                f"batch '{args.batch_id}' Lease in namespace '{namespace}' has no resourceVersion"
            )
        lease_expires_at = renewed_at + args.lease_duration
        try:
            batch_utils.lease_owner_reference(created)
            # The list's resourceVersion is where the watch starts, so no create is missed.
            claim_items, list_rv = await helper.list_sandbox_claim_objects(
                namespace, label_selector, _request_timeout=timeout, deadline=lease_expires_at
            )
            if claim_items:
                raise BatchExistsError(
                    f"claims with batch id '{args.batch_id}' already exist in namespace '{namespace}'"
                )
            if time.monotonic() >= lease_expires_at:
                raise BatchLeaseExpiredError(
                    f"batch '{args.batch_id}' Lease expired before setup finished in namespace '{namespace}'"
                )
        except BaseException as exc:
            # BaseException too, so a cancel or Ctrl-C during the list doesn't leave the Lease behind.
            # Fence the rollback delete by created_rv so a slow list never deletes a replacement Lease.
            try:
                await helper.delete_batch_lease(
                    lease_name, namespace, _request_timeout=timeout, resource_version=created_rv
                )
            except Exception as e:
                logger.warning(f"Batch '{args.batch_id}' failed to delete its Lease: {e}")
            if (
                not isinstance(exc, BatchError)
                and (isinstance(exc, TimeoutError) or _is_transport_error(exc))
                and time.monotonic() >= lease_expires_at
            ):
                raise BatchLeaseExpiredError(
                    f"batch '{args.batch_id}' Lease expired before setup finished in namespace '{namespace}'"
                ) from exc
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
            lease=created,
            lease_renewed_at=renewed_at,
            quorum_timeout=args.quorum_timeout,
            work_budget=args.work_budget,
            max_in_flight=args.max_in_flight,
        )
        handle._create_plan = deque(batch_utils.fill_create_plan(args.batch_id, args.groups))
        handle._create_interval = 1 / args.create_rps
        for warmpool, count in ready.items():
            handle._warm_credit.record(warmpool, count)
        handle._labels = {
            **args.labels,
            BATCH_ID_LABEL: args.batch_id,
            BATCH_LEASE_UID_LABEL: handle._lease_uid,
        }
        handle._claim_labels = {
            g.warmpool: {
                **handle._labels,
                BATCH_GROUP_LABEL: batch_utils.group_label_value(g.warmpool),
            }
            for g in args.groups
        }
        handle._claim_annotations = {
            g.warmpool: {
                BATCH_GROUP_SIZE_ANNOTATION: str(g.size),
                BATCH_GROUP_MIN_READY_ANNOTATION: str(g.min_ready),
                **trace_annotations,
            }
            for g in args.groups
        }
        handle._claim_lifecycle = claim_lifecycle
        handle._start()
        for i in range(min(args.max_in_flight, len(handle._create_plan))):
            handle._creators.add(
                asyncio.create_task(handle._create_worker(), name=f"batch-{handle.batch_id}-create-{i}")
            )
        return handle

    @classmethod
    async def _attach(cls, client: "AsyncSandboxClient[Any]", batch_id: str, namespace: str) -> "AsyncSandboxBatch":
        """Attach to an existing batch and take over its Lease."""
        batch_utils.validate_batch_id(batch_id)

        lease_name = f"{BATCH_LEASE_NAME_PREFIX}{batch_id}"

        lease = await client.k8s_helper.read_batch_lease(
            lease_name, namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        )
        lease_uid = None if lease is None else batch_utils.lease_owner_reference(lease)["uid"]
        label_selector = batch_utils.batch_claim_label_selector(batch_id, lease_uid)
        claim_items, list_rv = await client.k8s_helper.list_sandbox_claim_objects(
            namespace, label_selector, _request_timeout=batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
        )

        if lease is None:
            if not claim_items:
                raise BatchNotFoundError(f"batch '{batch_id}' not found in namespace '{namespace}'")
            raise BatchLeaseExpiredError(
                f"batch '{batch_id}' Lease is missing or stale in namespace '{namespace}'"
            )

        meta, spec = batch_utils.lease_parts(lease)
        settings = batch_utils.parse_lease_metadata(meta.labels, meta.annotations)
        duration = settings.lease_duration

        # Read before renewTime is stamped, so this handle's expiry is never later than the one
        # other clients see.
        renewed_at = time.monotonic()
        now = batch_utils.lease_now()
        if (
            spec.renew_time is None
            or spec.lease_duration_seconds is None
            or batch_utils.is_lease_stale(spec.renew_time, spec.lease_duration_seconds, now)
        ):
            raise BatchLeaseExpiredError(
                f"batch '{batch_id}' Lease is missing or stale in namespace '{namespace}'"
            )

        if spec.holder_identity:
            raise BatchInUseError(
                f"batch '{batch_id}' is held by '{spec.holder_identity}'"
            )

        groups = batch_state.reconstruct_groups(batch_id, claim_items)
        state = batch_state.BatchState(batch_id, groups, settings.next_ordinal)
        state.seed_from_claims(claim_items)
        state.count_missing_fill_as_unable()

        holder_identity = batch_utils.generate_holder_identity()
        spec.holder_identity = holder_identity
        spec.renew_time = now
        spec.lease_duration_seconds = duration
        spec.acquire_time = now
        spec.lease_transitions = (spec.lease_transitions or 0) + 1
        try:
            written = await client.k8s_helper.replace_batch_lease(
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
            lease=written,
            lease_renewed_at=renewed_at,
            quorum_timeout=settings.quorum_timeout,
            work_budget=settings.work_budget,
        )
        handle._fill_deadline = time.monotonic() + settings.quorum_timeout
        handle._create_interval = 1 / settings.create_rps
        handle._labels = {
            **settings.labels,
            BATCH_ID_LABEL: batch_id,
            BATCH_LEASE_UID_LABEL: handle._lease_uid,
        }
        handle._attached = True
        handle._start()
        return handle

    def _start(self) -> None:
        self._watch_task = asyncio.create_task(self._watch_loop(), name=f"batch-{self.batch_id}-watch")
        self._renew_task = asyncio.create_task(self._renew_loop(), name=f"batch-{self.batch_id}-renew")

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
        # Cancel deletes the watch started for late claims, so none runs after this handle gives
        # up the Lease.
        for delete_task in list(self._watch_delete_tasks):
            delete_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await delete_task
        self._watch_delete_tasks.clear()
        self._watch_task = None
        self._renew_task = None

    def _batch_error_locked(self) -> Exception | None:
        """Returns the error ``err()`` reports, either a lost Lease or a fatal watch error."""
        return self._lease_error or self._state.error()

    def _check_active(self) -> None:
        if self._detached:
            raise BatchError(f"batch '{self.batch_id}' has been detached or released")

    def _expire_lease_if_due(self, now: float | None = None) -> None:
        """Marks the handle stopped once the monotonic Lease deadline passes. Requires ``_lock``."""
        if now is None:
            now = time.monotonic()
        if not self._detached and self._lease_error is None and now >= self._lease_expires_at:
            expired = self._lease_expired_error()
            self._mark_renewal_degraded(expired)
            self._lease_error = expired
            self._stop_creating.set()
            if self._renew_task is not None and self._renew_task is not asyncio.current_task():
                self._renew_task.cancel()
            if self._watch_task is not None and self._watch_task is not asyncio.current_task():
                self._watch_task.cancel()
            self._notify_all()

    def _check_healthy(self, now: float | None = None) -> None:
        # Once detach() or release() starts or err() is set, the cache can no longer vouch for
        # members or observe new claims, so the handle connects to no more sandboxes and starts no
        # more acquires.
        self._check_active()
        # Check _lease_expires_at here so caller operations fence immediately after a process pause
        # before _renew_loop wakes up.
        self._expire_lease_if_due(now)
        if (error := self._batch_error_locked()) is not None:
            raise BatchError(f"batch '{self.batch_id}' has stopped") from error

    def _check_lease_held_locked(self, now: float | None = None) -> None:
        # Once detach() or release() starts or this handle loses the Lease, a batch recreated under
        # the same id reuses the claim names and labels, so this handle deletes no more claims. A
        # watch error leaves the Lease held, so cleanup methods still delete claims.
        self._check_active()
        self._expire_lease_if_due(now)
        if self._lease_error is not None:
            raise BatchError(f"batch '{self.batch_id}' has stopped") from self._lease_error

    async def _check_lease_held(self) -> None:
        async with self._lock:
            self._check_lease_held_locked()

    @contextlib.asynccontextmanager
    async def _member_delete(self) -> AsyncIterator[None]:
        """Tracks one in-flight claim delete request without holding a lock across network I/O.

        Holding ``_lock`` or ``_lifecycle_lock`` across the HTTP call would serialize concurrent
        ``release_member``, ``release_not_ready``, and ``release_group`` deletes. Taking ``_lock``
        only to check health and update ``_active_deletes`` lets HTTP deletes run in parallel while
        still fencing against ``detach()``, ``release()``, and client cleanup, which set
        ``_detached`` under ``_lock`` to reject new deletes and call ``_wait_active_deletes()`` to
        drain active ones.
        """
        async with self._lock:
            self._check_lease_held_locked()
            self._active_deletes += 1
            self._no_active_deletes.clear()
        try:
            yield
        finally:
            # Decrements synchronously without awaiting so task cancellation cannot skip waking
            # detach(), release(), or client cleanup.
            self._active_deletes -= 1
            if self._active_deletes == 0:
                self._no_active_deletes.set()

    async def _wait_active_deletes(self) -> None:
        """Waits for in-flight claim delete requests to finish once the caller sets ``_detached``.

        ``detach()``, ``release()``, and client cleanup call this before stopping Lease renewal,
        writing the Lease, or closing the API session, so no lingering delete from this handle can
        land after another handle attaches or hit a closed HTTP session.
        """
        await self._no_active_deletes.wait()

    def members(self, warmpool: str | None = None) -> list[Member]:
        """Returns a snapshot of the batch's members, optionally filtered to one warm pool."""
        return self._state.members(warmpool)

    async def connect(self, member: Member) -> "AsyncSandbox":
        """Returns a connected ``AsyncSandbox`` for a ready member. If this handle is already connected
        to the Sandbox, it reuses the existing connection, otherwise it creates a new one and caches it for future calls.
        The connection starts from the pod IP the batch's watch reports, without reading the Sandbox.

        The batch closes a cached connection once the member's pod is gone, meaning the member is
        lost or failed or the watch reports another sandbox or pod IP for it, since the old IP may go to another
        sandbox. Call ``connect`` again once the member is Ready to get a new one. A member that is
        only not Ready keeps its connection.

        Raises ``SandboxNotReadyError`` if the member isn't ready, even while its connection is
        cached, and ``BatchError`` once ``err()`` is set, since the Lease may be lost and the cache
        can no longer vouch for the member.
        """
        async with self._lock:
            self._check_healthy()
            # No name resolution or existence check as the batch's watch cache already knows the member's sandbox_name
            current = self._state.get_member(member.claim_name)
            if current is None or current.state is not MemberState.READY:
                raise SandboxNotReadyError(
                    f"batch member '{member.claim_name}' is not ready"
                )
            cached = self._connected.get(member.claim_name)
            if cached is not None:
                return cached[0]
            sandbox: AsyncSandbox = self._client.sandbox_class(
                claim_name=member.claim_name,
                sandbox_id=current.sandbox_name,
                namespace=self.namespace,
                connection_config=self._client.connection_config,
                tracer_config=self._client.tracer_config,
                k8s_helper=self._client.k8s_helper,
            )
            # The watch already has the pod IP, so the first request needs no Sandbox GET.
            pod_ip = select_pod_ip(current.pod_ips)
            sandbox._seeded_pod_ip = pod_ip
            self._connected[member.claim_name] = (sandbox, pod_ip)
            return sandbox

    def err(self) -> Exception | None:
        """Returns the error that stopped the batch's watch or background
        Lease renewal (e.g., ``BatchLeaseExpiredError``), or ``None`` while healthy.

        A failed claim create doesn't set it. That claim shows in ``members()`` as a ``FAILED``
        member with reason ``CreateFailed``.
        """
        return self._batch_error_locked()

    def lease_degraded(self) -> bool:
        """Returns whether background Lease renewal is failing.

        ``True`` from the first failed renewal until a renewal succeeds. It stays ``True`` once
        renewal stops on an error, such as the Lease expiring, so it can be polled at any time.
        """
        return self._renewal_degraded

    def events(self) -> AsyncIterator[BatchEvent]:
        """Hands out each initial claim once, as it becomes Ready, fails, or is lost.

        ``MEMBER_READY`` is yielded once per initial claim that becomes Ready, and ``MEMBER_FAILED``
        and ``MEMBER_LOST`` once per initial claim that fails or is deleted. Called on its own, it
        streams every Ready member. Called after ``group_outcomes()``, it yields a group's members
        only after that group has yielded successfully there, so a group that fails yields none.
        Called after ``wait_for_quorum()``, it yields the members the quorum doesn't return once the
        quorum is reached, and none if the quorum fails. ``group_outcomes()`` and
        ``wait_for_quorum()`` can't be called after ``events()``, since the first consumer to be
        called fixes how members are handed out.

        The iterator ends once every initial claim has been handed out, dropped with a failed group,
        or can't become Ready, once the fill deadline (``quorum_timeout`` after the last claim create)
        has passed, or once ``err()`` is set. A Ready member that a group holds until its outcome is
        decided keeps the iterator open. A change after the iterator ends, such as a handed-out
        member that fails, is yielded only by a later call of ``events()``, which continues where
        the previous iterator stopped. ``members()`` always shows each claim's current state.

        Raises:
            BatchError: If this handle has been detached or released, when called or while waiting.
        """
        self._check_active()
        self._state.set_mode(batch_state.ConsumerMode.STREAM)
        return self._iterate(self._state.pop_event, self._state.events_done)

    def group_outcomes(self) -> AsyncIterator[GroupOutcome]:
        """Returns an async iterator that yields one ``GroupOutcome`` per group, as soon as its outcome is known.

        A group yields its first ``min_ready`` Ready members, in the order they became Ready. A group that
        fails is still yielded, with ``error`` set to ``QuorumUnreachableError`` once too few
        of its members can still become Ready, or to ``BatchTimeoutError`` if it hasn't reached
        ``min_ready`` within the quorum timeout. The handle checks that deadline only while a consumer
        waits or claims are still being created, so a group that reaches ``min_ready`` after the
        deadline, before any consumer checks it, still succeeds.
        The iterator ends once every group has yielded, or once ``err()`` is set. Calling it again
        continues with the groups that haven't yielded yet.

        Raises:
            BatchError: If ``events()`` or ``wait_for_quorum()`` was called first, or this handle has
                been detached or released, when called or while waiting.
        """
        self._check_active()
        self._state.set_mode(batch_state.ConsumerMode.GROUP)
        return self._iterate(self._state.pop_group_outcome, self._state.groups_done)

    async def wait_for_quorum(self) -> list[Member]:
        """Waits until every group has ``min_ready`` Ready members, and returns them.

        The returned list has each group's first ``min_ready`` members, in the order they became
        Ready, with the groups in batch order. Members it doesn't return are yielded by ``events()``.
        If the quorum fails, no members are handed out and no more claims are created. The quorum
        also fails if the wait ends with any other exception, such as a cancellation or
        ``KeyboardInterrupt``. Can be called only once per handle.

        The handle checks the quorum timeout only while a consumer waits or claims are still being
        created, so a quorum reached after the deadline, before any consumer checks it, still succeeds.

        ``QuorumUnreachableError`` and ``BatchTimeoutError`` are both ``BatchError``, so
        ``except BatchError`` handles every failure.

        Raises:
            QuorumUnreachableError: If too few members of a group can still become Ready.
            BatchTimeoutError: If the quorum isn't reached within the quorum timeout.
            BatchError: If ``events()`` or ``group_outcomes()`` was called first, this method
                was already called, ``err()`` is set while waiting, or this handle has been
                detached or released, when called or while waiting.
        """
        self._check_active()
        self._state.set_mode(batch_state.ConsumerMode.QUORUM)
        try:
            result = await self._next(self._state.quorum_result, lambda: False)
            if result is None:
                raise BatchError(
                    f"batch '{self.batch_id}' stopped while waiting for quorum"
                ) from self._batch_error_locked()
        except BaseException:
            # This was the only wait, so a quorum left undecided would hold its members forever.
            async with self._changed:
                self._state.fail_quorum(BatchTimeoutError("wait_for_quorum() ended before the quorum was reached"))
                self._changed.notify_all()
            raise
        members, error = result
        if error is not None:
            raise error
        return members

    async def _iterate(
        self, pop: Callable[[], _T | None], done: Callable[[], bool]
    ) -> AsyncIterator[_T]:
        while (item := await self._next(pop, done)) is not None:
            yield item

    async def _next(self, pop: Callable[[], _T | None], done: Callable[[], bool]) -> _T | None:
        async with self._changed:
            while True:
                self._check_active()
                self._expire_lease_if_due()
                if self._batch_error_locked() is None:
                    self._expire_fill_if_due()
                item = pop()
                if item is not None:
                    return item
                if done() or self._batch_error_locked() is not None:
                    return None
                # Nothing to hand out yet, but returning None would end the iterator, so wait for
                # a change. Wake at the fill or Lease deadline so pending waits end on time.
                now = time.monotonic()
                timeout = max(0.0, self._lease_expires_at - now)
                if self._fill_deadline is not None:
                    timeout = min(timeout, max(0.0, self._fill_deadline - now))
                with contextlib.suppress(asyncio.TimeoutError):
                    await asyncio.wait_for(self._changed.wait(), timeout)

    def _expire_fill_if_due(self) -> None:
        # Called with the lock held.
        if self._fill_deadline is not None and time.monotonic() >= self._fill_deadline:
            if self._state.expire_fill():
                self._changed.notify_all()

    def _wake_acquire(self, claim_name: str) -> None:
        """Wakes the ``acquire()`` waiting on ``claim_name``, if any. Requires ``_lock``."""
        if (waiter := self._acquire_waiters.get(claim_name)) is not None:
            waiter.set()

    def _notify_all(self) -> None:
        """Wakes every fill consumer and every waiting ``acquire()``. Requires ``_lock``."""
        self._changed.notify_all()
        for waiter in self._acquire_waiters.values():
            waiter.set()

    def _exhaust_warm_credit(self) -> None:
        """Zeroes credit for pools whose claims observed a cold start. Requires ``_lock``."""
        # A single watch event triggers at most one pool, while a 410 re-list can discover
        # cold claims across several pools in one resync_from_list() pass.
        for warmpool in self._state.take_exhausted_pools():
            self._warm_credit.exhaust(warmpool)

    async def acquire(
        self,
        warmpool: str,
        timeout: float | None = None,
        *,
        pod_annotations: dict[str, str] | None = None,
    ) -> Member:
        """Adds a member from ``warmpool`` to the batch, waits until it is Ready, and returns it.

        The pool can be any warm pool in the batch's namespace. A pool that isn't one of the batch's
        groups is checked once per handle, like ``claim_batch`` checks its pools. The claim takes the
        next unused ordinal and carries its pool's group label but no group annotations, so it is
        never one of the initial claims. ``events()``, ``group_outcomes()``, ``wait_for_quorum()``
        and ``release_not_ready()`` leave it alone, and ``release_group()`` on its pool releases it
        with the pool's other members. Its ``shutdownTime`` is the
        time of its create plus ``quorum_timeout`` plus ``work_budget`` plus 600 seconds, and its
        create follows the initial claims' retries and pacing. Like theirs, a create that the
        pool's Ready sandboxes cover skips ``create_rps`` pacing. Once callers spend the Ready
        count from the pool's check and ``create_rps`` pacing would delay the next create,
        ``acquire()`` reads the pool again at most once a second per pool (unless an expired
        ``timeout`` cuts a read short before recording it). When the watch observes a claim from a
        pool start cold before becoming Ready, the batch zeroes that pool's credit and defers its
        next refresh. Fill creates, ``acquire()`` creates and pool reads, and member deletes share
        ``max_in_flight``, which defaults to 20 on a handle from ``get_batch``.

        If it raises, including on a cancellation, the member is dropped from ``members()`` and its
        claim is deleted, unless the claim is already gone or its create was rejected. A failed delete is
        retried by ``release_not_ready()`` and ``release()``. Once this handle loses the batch's
        Lease it keeps the claim, which ``release()`` deletes only if no other handle has taken the
        Lease since. It never creates a second claim, so call it again to try again. A member it
        returned that fails later shows in ``members()`` like any other, and ``release_member``
        frees it.

        ``TerminalMemberError`` and ``BatchTimeoutError`` both subclass ``BatchError``, and
        ``except SandboxError`` also covers a missing pool or template. Bad arguments raise
        ``ValueError``, and checking an unchecked pool propagates Kubernetes API or network errors.

        Args:
            warmpool: The warm pool to claim the member from.
            timeout: Seconds to wait for the member to be Ready. Defaults to the batch's
                ``quorum_timeout``.
            pod_annotations: Optional annotations that ``acquire()`` writes to
                ``spec.additionalPodMetadata.annotations`` so the controller propagates them onto
                the sandbox pod. ``acquire()`` omits ``spec.additionalPodMetadata`` when
                ``pod_annotations`` is ``None`` or empty.

        Raises:
            ValueError: If ``timeout`` isn't a positive finite number, or ``pod_annotations`` isn't
                a mapping of strings.
            SandboxWarmPoolNotFoundError: If the warm pool doesn't exist.
            SandboxTemplateNotFoundError: If the warm pool's template doesn't exist, or it names none.
            TerminalMemberError: If the create fails (the member is ``FAILED`` with reason
                ``CreateFailed``), the member fails terminally, or someone else deletes its claim
                (the member is ``LOST``) before it is Ready.
            BatchTimeoutError: If checking an unchecked warm pool, creating the claim, or waiting
                for the member to be Ready doesn't finish within ``timeout``. ``acquire()`` skips
                sending any create or create retry that ``create_rps`` pacing or ``max_in_flight``
                cannot start before the deadline.
            BatchError: If this handle has been detached or released, or ``err()`` is set, when
                called or while waiting, or if ``release_member`` or ``release_group`` releases the
                member before it is Ready.
            ApiException: If checking an unchecked warm pool fails with an API error such as a
                403.
            aiohttp.ClientError, TimeoutError: If checking an unchecked warm pool fails before
                ``timeout``, such as on a connection error or request timeout.
        """
        wait = batch_utils.positive_number("timeout", timeout, self._quorum_timeout)
        pod_metadata = build_pod_metadata(None, pod_annotations)
        deadline = time.monotonic() + wait
        async with self._lock:
            self._check_healthy()
            need_check = warmpool not in self._checked_pools
            acquire_seq = self._state.enter_acquire(warmpool)
        entered_acquire = True
        refreshed = False
        claim_name: str | None = None
        waiter: asyncio.Event | None = None
        # Whether a claim of the batch may exist under the name, so a failure deletes it.
        may_exist = False
        sent = 0
        try:
            if need_check:
                await self._check_pool(warmpool, deadline, wait, acquire_seq)
            while True:
                async with self._lock:
                    self._check_healthy()
                    self._check_acquire_pool_active(warmpool, acquire_seq)
                    now = time.monotonic()
                    covered = now < deadline and self._warm_credit.take(warmpool)
                    need_refresh = (
                        not covered
                        and not refreshed
                        and now < deadline
                        and self._next_create_at > now
                        and self._warm_credit.refresh_due(warmpool)
                    )
                    if not need_refresh:
                        # A create paced past the deadline would only make a claim to delete on timeout.
                        if not covered and max(now, self._next_create_at) >= deadline:
                            raise BatchTimeoutError(f"no create slot for warm pool '{warmpool}' within {wait:g}s")
                        if not covered:
                            self._warm_credit.spend(warmpool)
                        # Hand ownership from _checking_acquires to _acquiring under one lock hold
                        # so release_group() sees the acquire throughout without burning an ordinal early.
                        claim_name = self._state.start_acquire(warmpool)
                        self._state.exit_acquire(warmpool)
                        entered_acquire = False
                        waiter = self._acquire_waiters[claim_name] = asyncio.Event()
                        slot = now if covered else self._reserve_create_slot(now)
                        break
                # Refresh only when create_rps pacing would delay this create, since a free pacing slot
                # already starts at once without an extra pool GET.
                await self._refresh_warm_credit(warmpool, deadline, acquire_seq)
                refreshed = True
            assert claim_name is not None and waiter is not None
            acquired_name = claim_name
            # Once creating stops, the handle is detached or released, so _wait_acquired raises.
            if not await self._wait_for_stop(slot - time.monotonic()):
                def can_send() -> bool:
                    # Checks again after waiting for a max_in_flight slot or before retrying, in case
                    # release_group() or release_member() freed the claim or an earlier attempt landed.
                    return (
                        self._state.acquired_member(acquired_name) is None
                        and self._state.get_member(acquired_name) is None
                    )

                def on_send() -> None:
                    nonlocal may_exist, sent
                    sent += 1
                    may_exist = True

                lifecycle = construct_sandbox_claim_lifecycle_spec(
                    self._quorum_timeout + self._work_budget + batch_utils.BATCH_SHUTDOWN_MARGIN_SECONDS
                )
                try:
                    await self._create_one(
                        acquired_name,
                        warmpool,
                        self._client._trace_context_annotations(),
                        # The group label lets release_group() free it with the pool's other members.
                        {**self._labels, BATCH_GROUP_LABEL: batch_utils.group_label_value(warmpool)},
                        lifecycle,
                        deadline,
                        can_send=can_send,
                        on_send=on_send,
                        pod_metadata=pod_metadata,
                    )
                except BatchError:
                    raise
                except Exception as e:
                    # A create that ended in a transient error, or failed after retrying one, may
                    # have stored a claim on an attempt whose response never arrived.
                    may_exist = sent > 1 or _is_transient_error(e)
                    raise TerminalMemberError(Member(
                        claim_name=acquired_name,
                        warmpool=warmpool,
                        state=MemberState.FAILED,
                        reason=batch_state.CREATE_FAILED_REASON,
                        message=_create_failure_message(e),
                    )) from e
            return await self._wait_acquired(acquired_name, waiter, deadline, wait)
        except BaseException:
            if entered_acquire:
                self._state.exit_acquire(warmpool)
            if claim_name is not None:
                await self._drop_acquired(claim_name, may_exist, retried=sent > 1)
            raise

    def _check_acquire_pool_active(self, warmpool: str, acquire_seq: int) -> None:
        """Raises if ``release_group()`` released ``warmpool`` since this ``acquire()`` started.
        Requires ``_lock``.
        """
        if self._state.pool_release_seq(warmpool) != acquire_seq:
            raise BatchError(f"warm pool '{warmpool}' was released while acquiring")

    async def _wait_acquired(
        self, claim_name: str, waiter: asyncio.Event, deadline: float, wait: float
    ) -> Member:
        while True:
            async with self._lock:
                waiter.clear()
                self._check_active()
                self._expire_lease_if_due()
                if (error := self._batch_error_locked()) is not None:
                    raise BatchError(f"batch '{self.batch_id}' stopped while acquiring '{claim_name}'") from error
                member = self._state.acquired_member(claim_name)
                if member is not None:
                    self._acquire_waiters.pop(claim_name, None)
                    return member
                now = time.monotonic()
                if deadline <= now:
                    raise BatchTimeoutError(f"acquired member '{claim_name}' wasn't Ready within {wait:g}s")
                wait_timeout = max(0.0, min(deadline, self._lease_expires_at) - now)
            with contextlib.suppress(asyncio.TimeoutError):
                await asyncio.wait_for(waiter.wait(), wait_timeout)

    async def _check_pool(self, warmpool: str, deadline: float, wait: float, acquire_seq: int) -> None:
        """Checks a pool that isn't one of the batch's groups the first time ``acquire()`` uses it,
        and records its Ready count as credit. Concurrent first acquires of a pool share one check
        and its outcome, except when a caller-local ``BatchError`` stops the leader and cancels the
        shared check so a waiting acquire can take over or surface its own batch state. A failed
        check leaves no cached entry, so a later acquire checks the pool again.

        Raises:
            BatchTimeoutError: If checking the pool or waiting for a ``max_in_flight`` slot doesn't
                finish by ``deadline``.
        """
        while True:
            async with self._lock:
                now = time.monotonic()
                self._check_healthy(now)
                self._check_acquire_pool_active(warmpool, acquire_seq)
                if warmpool in self._checked_pools:
                    return
                if now >= deadline:
                    raise BatchTimeoutError(f"warm pool '{warmpool}' wasn't checked within {wait:g}s")
                wait_timeout = min(deadline, self._lease_expires_at) - now
                check = self._prechecks.get(warmpool)
                if check is None:
                    check = self._prechecks[warmpool] = asyncio.get_running_loop().create_future()
                    # A failed check that no acquire waits for isn't logged as never retrieved.
                    check.add_done_callback(lambda done: done.cancelled() or done.exception())
                    break
            try:
                await asyncio.wait_for(asyncio.shield(check), wait_timeout)
            except TimeoutError:
                if check.done() and not check.cancelled() and check.exception() is not None:
                    check.result()
                continue
            except asyncio.CancelledError:
                task = asyncio.current_task()
                if check.cancelled() and task is not None and not task.cancelling():
                    # The leader stopped before publishing an outcome, so this acquire checks next.
                    continue
                raise
            return

        async def next_timeout() -> float:
            async with self._lock:
                now = time.monotonic()
                self._check_healthy(now)
                self._check_acquire_pool_active(warmpool, acquire_seq)
                if now >= deadline:
                    raise BatchTimeoutError(
                        f"warm pool '{warmpool}' wasn't checked within {wait:g}s"
                    )
                return min(
                    float(batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS),
                    deadline - now,
                    self._lease_expires_at - now,
                )

        acquired = False
        try:
            # Both the slot wait and each pool-check GET honor deadline and Lease expiry;
            # a caller-local BatchError cancels check so a waiting acquire can take over.
            try:
                while not acquired:
                    step_timeout = await next_timeout()
                    with contextlib.suppress(TimeoutError):
                        async with asyncio.timeout(step_timeout):
                            await self._in_flight.acquire()
                            acquired = True
                try:
                    ready = await _precheck_pools(
                        self._client.k8s_helper,
                        [warmpool],
                        self.namespace,
                        timeout=next_timeout,
                    )
                except Exception as e:
                    async with self._lock:
                        now = time.monotonic()
                        self._check_healthy(now)
                        self._check_acquire_pool_active(warmpool, acquire_seq)
                    if not isinstance(e, BatchError) and now >= deadline and (
                        isinstance(e, TimeoutError) or _is_transport_error(e)
                    ):
                        raise BatchTimeoutError(
                            f"warm pool '{warmpool}' wasn't checked within {wait:g}s"
                        ) from e
                    raise
                async with self._lock:
                    self._warm_credit.record(warmpool, ready[warmpool])
                    self._checked_pools.add(warmpool)
                    del self._prechecks[warmpool]
            finally:
                if acquired:
                    self._in_flight.release()
        except BaseException as e:
            self._prechecks.pop(warmpool, None)
            same_release = self._state.pool_release_seq(warmpool) == acquire_seq
            if (
                acquired
                and isinstance(e, Exception)
                and not isinstance(e, BatchError)
                and same_release
            ):
                check.set_exception(e)
            else:
                # Let a waiting acquire with a later deadline or after release_group() take over.
                check.cancel()
            raise
        check.set_result(None)

    async def _refresh_warm_credit(self, warmpool: str, deadline: float, acquire_seq: int) -> None:
        """Re-reads the pool's Ready count once callers spend its credit, at most once every
        ``BATCH_WARM_CREDIT_REFRESH_SECONDS``, while holding one of the ``max_in_flight`` slots, so
        a burst of acquires on a warm pool skips pacing without exceeding the connection budget.
        """
        def remaining_if_due() -> float:
            now = time.monotonic()
            self._check_healthy(now)
            self._check_acquire_pool_active(warmpool, acquire_seq)
            remaining = min(deadline, self._lease_expires_at) - now
            return remaining if remaining > 0 and self._warm_credit.refresh_due(warmpool) else 0.0

        async with self._lock:
            if (remaining := remaining_if_due()) <= 0:
                return
            refresh_lock = self._credit_refresh_locks.setdefault(warmpool, asyncio.Lock())
        acquired_refresh = False
        acquired_slot = False
        try:
            with contextlib.suppress(TimeoutError):
                async with asyncio.timeout(remaining):
                    await refresh_lock.acquire()
                    acquired_refresh = True
            if not acquired_refresh:
                # Fall back to normal pacing, which checks whether a paced slot fits the deadline.
                return
            async with self._lock:
                remaining = remaining_if_due()
            if remaining <= 0:
                return
            with contextlib.suppress(TimeoutError):
                async with asyncio.timeout(remaining):
                    await self._in_flight.acquire()
                    acquired_slot = True
            if not acquired_slot:
                return
            # Snapshot the pool sequence before GET so a refresh finishing after exhaust() cannot overwrite 0.
            async with self._lock:
                remaining = remaining_if_due()
                seq = self._warm_credit.seq(warmpool)
            if remaining <= 0:
                return
            request_timeout = min(float(batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS), remaining)
            try:
                async with asyncio.timeout(request_timeout):
                    obj = await self._client.k8s_helper.get_sandbox_warmpool(
                        warmpool, self.namespace, _request_timeout=request_timeout
                    )
                ready = 0 if obj is None else batch_utils.warmpool_ready_replicas(obj)
            except asyncio.CancelledError:
                raise
            except Exception as e:
                now = time.monotonic()
                if now >= deadline or now >= self._lease_expires_at:
                    # This acquire's deadline or Lease expiry cut the read short, so another acquire
                    # with time left can still refresh instead of waiting out the refresh interval.
                    return
                # Fall back to normal pacing so the claim create surfaces any real pool failure.
                logger.debug(f"Batch '{self.batch_id}' failed to read warm pool '{warmpool}': {e}")
                ready = 0
            async with self._lock:
                self._warm_credit.record(warmpool, ready, seq=seq)
        finally:
            if acquired_slot:
                self._in_flight.release()
            if acquired_refresh:
                refresh_lock.release()

    async def _drop_acquired(self, claim_name: str, may_exist: bool, *, retried: bool = False) -> None:
        """Drops a claim ``acquire()`` didn't get Ready and deletes it. A failed delete is logged,
        since the error ``acquire()`` raises matters more, and left for a later release call.
        """
        async with self._changed:
            self._acquire_waiters.pop(claim_name, None)
            needs_delete = self._state.drop_acquired(claim_name, may_exist, retried=retried)
            self._changed.notify_all()
        if needs_delete:
            try:
                await self._delete_claim(claim_name)
            except asyncio.CancelledError:
                raise
            except BatchError:
                # detach(), release(), or losing the Lease stopped this handle from deleting claims.
                pass
            except Exception as e:
                logger.warning(f"Batch '{self.batch_id}' failed to delete acquired claim '{claim_name}': {e}")

    async def release_member(self, member: Member) -> None:
        """Deletes one member's claim and drops the member from ``members()``.

        The member is never handed out afterwards and emits no ``MEMBER_LOST``. A member of the
        initial claims counts as unable to become Ready, so releasing one before a quorum consumer
        has decided can make its group or the quorum unreachable. A claim that is already gone
        counts as deleted, and transient errors are retried. If the delete fails, calling
        ``release_member`` again retries it.

        Raises:
            ValueError: If the member isn't in this batch.
            BatchError: If this handle has been detached or released, or once this handle loses the
                batch's Lease.
            ApiException: For a non-transient error such as a 403, or the last transient error
                after ``BATCH_DELETE_ATTEMPTS`` attempts (an ``ApiException`` or a transport error).
        """
        async with self._changed:
            self._check_lease_held_locked()
            needs_delete = self._state.release_member(member.claim_name)
            self._wake_acquire(member.claim_name)
            self._changed.notify_all()
        await self._close_cached_connections("release_member", [member.claim_name])
        if needs_delete:
            await self._delete_claim(member.claim_name)

    async def release_not_ready(self) -> None:
        """Ends the fill and deletes every initial claim that isn't Ready and was never handed out.

        Creates not yet sent are skipped, and a create that lands afterwards is deleted. A member
        that was handed out is kept, even if it has since gone not Ready. Members are picked and
        released in one step under the batch lock, so none can turn Ready and be handed out in
        between, as could happen when calling ``release_member`` on each member that ``members()``
        lists as not Ready. Released members are dropped from ``members()``, emit no
        ``MEMBER_LOST``, and count as unable to become Ready, so calling this before a quorum
        consumer has decided can make a group or the quorum unreachable. Call it once you have
        what you need.

        Deletes each claim like ``release_member``. Member deletes and claim creates share
        ``max_in_flight``, which defaults to 20 on a handle from ``get_batch``. If a delete fails,
        calling ``release_not_ready`` again retries it.

        Raises:
            BatchError: If this handle has been detached or released, or once this handle loses the
                batch's Lease.
            ApiException: The first error a delete ended with, once every delete has been tried
                (an ``ApiException`` or a transport error).
        """
        async with self._changed:
            self._check_lease_held_locked()
            self._drop_planned_creates(None)
            names = self._state.release_not_ready()
            self._changed.notify_all()
        await self._close_cached_connections("release_not_ready", names)
        if not names:
            return
        results = await asyncio.gather(*(self._delete_claim(name) for name in names), return_exceptions=True)
        errors = [r for r in results if isinstance(r, BaseException)]
        if errors:
            raise errors[0]

    async def release_group(self, warmpool: str) -> None:
        """Deletes every claim the batch holds from one pool, pending ones included, and drops their
        members from ``members()``. That covers the group's initial claims and the members
        ``acquire()`` added from the pool, and an ``acquire()`` still waiting on the pool raises
        ``BatchError``.

        The group's creates not yet sent are skipped, and a create that lands afterwards is deleted.
        Its members are never handed out afterwards and emit no ``MEMBER_LOST``. The initial ones
        count as unable to become Ready, so releasing a group before a quorum consumer has decided
        makes it, or the quorum, unreachable. Works with any consumer, typically on a group that
        ``group_outcomes()`` yields with an error.

        Claims are deleted like ``release()`` deletes the batch's, with deletecollection rounds
        scoped to the group's label. If they fail, calling ``release_group`` again retries.

        Raises:
            ValueError: If the batch has no group, member or ``acquire()`` in flight for ``warmpool``.
            BatchError: If this handle has been detached or released, once this handle loses the
                batch's Lease, or if claims are still not being deleted after the last round.
            ApiException: For a non-transient error such as a 403, which is raised at once.
                After the last round, the last transient error is raised (an ``ApiException``
                or a transport error).
        """
        async with self._changed:
            self._check_lease_held_locked()
            if not self._state.has_pool(warmpool):
                raise ValueError(f"batch '{self.batch_id}' has no group or members for warm pool '{warmpool}'")
            self._drop_planned_creates(warmpool)
            names = self._state.release_group(warmpool)
            self._notify_all()
        await self._close_cached_connections("release_group", names)
        await _delete_claims_in_rounds(
            self._client.k8s_helper,
            self.namespace,
            batch_utils.group_label_selector(self.batch_id, warmpool, self._lease_uid),
            f"batch '{self.batch_id}' group '{warmpool}'",
            guard=self._member_delete,
            deadline=lambda: self._lease_expires_at,
        )

    def _drop_planned_creates(self, warmpool: str | None) -> None:
        """Releases the planned creates not yet sent, of one pool or of every pool, so they don't
        take pacing slots. Called with the lock held.
        """
        if not self._create_plan:
            return
        kept: deque[tuple[str, BatchGroup]] = deque()
        for claim_name, group in self._create_plan:
            if warmpool is None or group.warmpool == warmpool:
                if warmpool is not None or self._state.get_member(claim_name) is None:
                    self._state.release_claim(claim_name, group.warmpool)
            else:
                kept.append((claim_name, group))
        self._create_plan = kept
        if not kept:
            # Count from _last_fill_slot rather than _next_create_at so pacing slots that
            # acquire() reserved do not push the fill deadline out.
            last_slot = max(time.monotonic(), self._last_fill_slot)
            self._fill_deadline = last_slot + self._quorum_timeout

    async def _delete_claim(self, claim_name: str) -> None:
        """Deletes one released claim, retrying transient errors. A claim already gone counts as deleted."""
        attempt = 1
        while True:
            await self._check_lease_held()
            try:
                # Scope the pool slot and active-delete guard to each HTTP attempt so a sleeping
                # retry neither holds a max_in_flight slot nor blocks detach() or release().
                async with self._in_flight, self._member_delete():
                    # Snapshot the delete sequence and remaining Lease budget before sending DELETE
                    # so finishing late cannot clear a newer cleanup delete or outlive the Lease.
                    async with self._lock:
                        now = time.monotonic()
                        self._check_lease_held_locked(now)
                        request_timeout = min(
                            batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS, self._lease_expires_at - now
                        )
                        seq = self._state.delete_seq(claim_name)
                    await self._client.k8s_helper.delete_sandbox_claim(
                        claim_name,
                        self.namespace,
                        _request_timeout=request_timeout,
                        log_level=logging.DEBUG,
                        batch=True,
                    )
                break
            except asyncio.CancelledError:
                raise
            except Exception as e:
                if attempt >= batch_utils.BATCH_DELETE_ATTEMPTS or not _is_transient_error(e):
                    raise
                await self._check_lease_held()
                # Wake as soon as the handle stops instead of sleeping out the backoff, then report why.
                if await self._wait_for_stop(batch_utils.retry_delay(attempt, e)):
                    await self._check_lease_held()
                attempt += 1
        async with self._lock:
            self._state.note_deleted(claim_name, seq)

    async def release(self) -> None:
        """Stops creating claims, stops the background watch and Lease renewal, closes cached
        sandbox connections, then deletes the batch's claims and finally its Lease. Idempotent.

        Claims are deleted with label-scoped deletecollection requests, re-listing between rounds
        until no claim is left that isn't already being deleted. A transient error moves on to the
        next round, and release gives up after ``BATCH_RELEASE_MAX_IDLE_ROUNDS`` rounds in a row
        that delete nothing. On failure the Lease is kept, and calling ``release`` again retries.
        Before each round, release writes the batch's Lease with this handle as its holder, so no
        other handle can attach while claims are deleted. If the batch is another handle's, it
        raises ``BatchInUseError`` and deletes nothing. While it runs, a ``detach()`` call raises
        ``BatchError``, and another ``release()`` call waits for it to finish.

        Raises:
            BatchInUseError: If another handle holds the batch's Lease, or the Lease changed hands
                since this handle last held it. Nothing more is deleted, and this handle is done
                with the batch.
            BatchError: If this handle has been detached, an earlier call raised
                ``BatchInUseError``, or claims are still not being deleted after the last round.
            ApiException: For a non-transient error such as a 403, which is raised at once.
                After the last round, the last transient error is raised (an ``ApiException``
                or a transport error).
        """
        async with self._lifecycle_lock:
            async with self._lock:
                if self._released:
                    return
                if self._lease_released:
                    raise BatchError(f"batch '{self.batch_id}' has been detached or is held by another handle")
                self._detached = True
                self._stop_creating.set()
                # Clearing _detach_grace marks release as started so detach() rejects new calls and
                # client cleanup retries a failed release as a release rather than a detach.
                self._detach_grace = None
                self._notify_all()

            await self._wait_active_deletes()
            await self._stop_background_tasks()
            await self._close_cached_connections("release")
            async with self._lock:
                lease = self._lease
            try:
                await _delete_batch_objects(
                    self._client.k8s_helper,
                    self.namespace,
                    self.batch_id,
                    self._lease_name,
                    self._holder_identity,
                    self._lease_duration,
                    lease,
                )
            except BatchInUseError:
                # The batch is another handle's now, so neither a retry nor client cleanup may delete anything.
                async with self._lock:
                    self._lease_released = True
                self._client._unregister_batch(self)
                raise

            async with self._lock:
                self._lease_released = True
                self._released = True
            self._client._unregister_batch(self)

    async def _close_cached_connections(self, action: str, claim_names: Iterable[str] | None = None) -> None:
        """Closes the cached connections to the given claims' sandboxes, or to every sandbox along
        with the stale connections."""
        async with self._lock:
            names = list(self._connected) if claim_names is None else claim_names
        for claim_name in names:
            async with self._lock:
                entry = self._connected.pop(claim_name, None)
            if entry is None:
                continue
            await self._close_one(entry[0], action, partial(self._connected.setdefault, claim_name, entry))
        if claim_names is None:
            async with self._lock:
                stale = list(self._stale_connections)
            await self._close_connections(stale, action)

    def _pop_stale_connections(self, claim_names: list[str]) -> list["AsyncSandbox"]:
        """Moves the cached connections that no longer reach their member to ``_stale_connections``
        and returns them, so the caller can close them outside ``_lock``. Requires ``_lock``."""
        stale = []
        for claim_name in claim_names:
            cached = self._connected.get(claim_name)
            if cached is not None and not batch_state.still_reaches(
                self._state.get_member(claim_name), cached[0].sandbox_id, cached[1]
            ):
                stale.append(self._connected.pop(claim_name)[0])
        self._stale_connections += stale
        return stale

    async def _close_connections(self, connected: list["AsyncSandbox"], action: str) -> None:
        """Closes the given stale connections that no other caller has taken from ``_stale_connections``."""
        for sandbox in connected:
            async with self._lock:
                if sandbox not in self._stale_connections:
                    continue
                self._stale_connections.remove(sandbox)
            await self._close_one(sandbox, action, partial(self._stale_connections.append, sandbox))

    async def _close_one(self, sandbox: "AsyncSandbox", action: str, keep: Callable[[], object]) -> None:
        """Closes one cached connection, or calls ``keep`` to put it back if the close fails."""
        closed = False
        try:
            await sandbox.close_connection()
            closed = True
        except Exception as e:
            logger.warning(
                f"Batch '{self.batch_id}' failed to close a cached sandbox connection "
                f"during {action}: {e}"
            )
        finally:
            if not closed:
                # A connection that fails to close is kept on the handle rather than dropped, and the
                # client keeps the handle for its next cleanup.
                keep()
                self._client._track_closing(self)

    def _detach_unfinished(self) -> bool:
        """Returns whether detach() started on this handle but hasn't released the Lease."""
        return self._detach_grace is not None and not self._lease_released

    def _release_started(self) -> bool:
        """Returns whether release() started on this handle, or its client was closed."""
        return self._detached and self._detach_grace is None

    def _exit_detaches(self) -> bool:
        """Returns whether client exit cleanup detaches this handle instead of releasing its batch."""
        # Client cleanup retries a release() that already started, even on an attached handle.
        return self._attached and not self._release_started()

    def _detach_done(self) -> bool:
        """Returns whether detach() already released the Lease, and raises ``BatchError`` once a
        release has started. Requires ``_lock``."""
        if self._release_started():
            raise BatchError(f"batch '{self.batch_id}' has been released, is being released, or its client was closed")
        return self._lease_released

    async def detach(self, grace: int | None = None) -> None:
        """Stops the background watch and Lease renewal, closes cached sandbox connections, and
        releases this handle's hold on the Lease. ``grace`` is the number of seconds to keep the
        Lease valid after detaching, an int greater than ``CLOCK_SKEW_MARGIN`` (5); if ``None``, we
        use the batch's original Lease duration.

        If the Lease release fails, the error propagates and this handle stays in a detached state with
        the Lease still held. Call ``detach`` again to retry; steps that already completed are skipped.
        A retry without ``grace`` keeps the earlier call's Lease duration, and if no retry is made,
        client cleanup makes one.
        A batch that is still creating claims stops creating first, and every claim is left in place.

        Raises:
            ValueError: If ``grace`` is not a valid Lease duration.
            BatchError: If a release has started on this handle, including one that failed, or its
                client was closed.
            ApiException: If the Lease release fails (an ``ApiException`` or a transport error).
        """
        if grace is not None:
            batch_utils.validate_lease_duration_value(grace)

        # _detached prevents the caller from connecting to Sandboxes, while _lease_released signals that
        # detach fully completed (i.e. the previous steps and the Lease release write succeeded), so callers
        # can retry detach() if the release fails.
        async with self._lock:
            if self._detach_done():
                return
            self._detached = True
            self._stop_creating.set()
            self._notify_all()
            if grace is not None:
                self._detach_grace = grace
            elif self._detach_grace is None:
                self._detach_grace = self._lease_duration
            # Another detach() call can change _detach_grace while this call writes the Lease, so every write
            # uses this copy.
            grace = self._detach_grace
            # acquire() takes no ordinal once the handle is detached, so this is final.
            next_ordinal = self._state.next_ordinal

        await self._wait_active_deletes()
        async with self._lock:
            if self._detach_done():
                return
        await self._stop_background_tasks()
        await self._close_cached_connections("detach")

        async with self._lifecycle_lock:
            async with self._lock:
                if self._detach_done():
                    return
            lease = await self._client.k8s_helper.read_batch_lease(
                self._lease_name, self.namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
            )
            if lease is not None:
                meta, spec = batch_utils.lease_parts(lease)
                if spec.holder_identity != self._holder_identity:
                    logger.info(
                        f"Batch '{self.batch_id}' Lease is held by "
                        f"'{spec.holder_identity}', not this handle; treating as released"
                    )
                else:
                    for attempt in range(2):
                        # kubernetes_asyncio 36's stub types holder_identity as str, though None clears it.
                        spec.holder_identity = cast(Any, None)
                        spec.renew_time = batch_utils.lease_now()
                        meta.annotations = {
                            **(meta.annotations or {}),
                            BATCH_NEXT_ORDINAL_ANNOTATION: str(next_ordinal),
                        }
                        # Refresh the Lease so it doesn't expire immediately after we release it,
                        # giving other clients a chance to acquire it.
                        spec.lease_duration_seconds = grace
                        try:
                            await self._client.k8s_helper.replace_batch_lease(
                                self._lease_name,
                                self.namespace,
                                lease,
                                _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                            )
                            break
                        except ApiException as e:
                            if e.status != 409 or attempt:
                                raise
                            # A 409 means the Lease changed after it was read. A write from an earlier detach() call
                            # that timed out but landed leaves it released, and a takeover leaves another holder, so
                            # both count as done. It can still be held here only if one of this handle's own renewals
                            # landed late, so the release is written once more.
                            lease = await self._client.k8s_helper.read_batch_lease(
                                self._lease_name,
                                self.namespace,
                                _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                            )
                            if lease is None:
                                break
                            meta, spec = batch_utils.lease_parts(lease)
                            if spec.holder_identity != self._holder_identity:
                                break

            async with self._lock:
                self._lease_released = True
        self._client._unregister_batch(self)

    def _take_watch_deletes_locked(self) -> list[str]:
        """Returns claims that landed after their pool was released, or none once this handle can
        no longer delete claims.
        """
        names = self._state.take_watch_deletes()
        if not names:
            return []
        self._expire_lease_if_due()
        if self._detached or self._lease_error is not None:
            return []
        return names

    def _start_watch_deletes(self, names: list[str]) -> None:
        """Deletes late claims in a separate task so the watch doesn't block on HTTP deletes."""
        task = asyncio.create_task(
            self._run_watch_deletes(names),
            name=f"batch-{self.batch_id}-watch-delete",
        )
        self._watch_delete_tasks.add(task)
        task.add_done_callback(self._watch_delete_tasks.discard)

    async def _run_watch_deletes(self, names: list[str]) -> None:
        """Deletes each late claim. ``BatchError`` means this handle detached or lost the Lease,
        which ends cleanup, so it is not logged.
        """
        for name in names:
            try:
                await self._delete_claim(name)
            except asyncio.CancelledError:
                raise
            except BatchError:
                pass
            except Exception as e:
                logger.warning(f"Batch '{self.batch_id}' failed to delete late watch claim '{name}': {e}")

    async def _drain_watch_deletes(self) -> None:
        """Waits for the late-claim deletes this watch started, cancelling them first once this
        handle can no longer delete claims.
        """
        if not self._watch_delete_tasks:
            return
        pending_tasks = list(self._watch_delete_tasks)
        if self._detached or self._lease_error is not None:
            for task in pending_tasks:
                task.cancel()
        await asyncio.gather(*pending_tasks, return_exceptions=True)

    async def _watch_loop(self) -> None:
        """Background watch loop which keeps ``self._state`` in sync with the batch's claims.

        kubernetes_asyncio's informer/reflector helpers don't support custom resources, so
        this manually implements one where the batch state is seeded from a list,
        then kept up to date with watch events using the list's resourceVersion so no events are missed.
        """
        rv = self._list_resource_version
        label_selector = batch_utils.batch_claim_label_selector(self.batch_id, self._lease_uid)
        relist = False
        # Consecutive failed attempts, which set the retry backoff.
        failures = 0
        try:
            while True:
                try:
                    if relist:
                        items, rv = await self._client.k8s_helper.list_sandbox_claim_objects(
                            self.namespace, label_selector, _request_timeout=batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
                        )
                        async with self._lock:
                            self._state.resync_from_list(items)
                            self._exhaust_warm_credit()
                            watch_deletes = self._take_watch_deletes_locked()
                            self._notify_all()
                            stale = self._pop_stale_connections(list(self._connected))
                        if watch_deletes:
                            self._start_watch_deletes(watch_deletes)
                        await self._close_connections(stale, "a re-list")
                        relist = False
                    watch_started = time.monotonic()
                    got_event = False
                    async with contextlib.aclosing(
                        self._client.k8s_helper.watch_sandbox_claims(
                            self.namespace,
                            label_selector,
                            rv,
                            _WATCH_TIMEOUT_SECONDS,
                            _request_timeout=_WATCH_REQUEST_TIMEOUT,
                        )
                    ) as stream:
                        async for event in stream:
                            failures = 0
                            got_event = True
                            event_type = event.get("type")
                            obj = event.get("object") or {}
                            seen_rv = (obj.get("metadata") or {}).get("resourceVersion")
                            if seen_rv:
                                rv = seen_rv
                            if event_type == "BOOKMARK":
                                continue
                            claim_name = (obj.get("metadata") or {}).get("name", "")
                            watch_deletes = []
                            async with self._lock:
                                if event_type == "DELETED":
                                    if self._state.mark_lost(claim_name) is not None:
                                        self._wake_acquire(claim_name)
                                elif event_type in ("ADDED", "MODIFIED"):
                                    member = self._state.upsert_claim(obj)
                                    self._exhaust_warm_credit()
                                    watch_deletes = self._take_watch_deletes_locked()
                                    if member is not None and member.state is not MemberState.PENDING:
                                        self._wake_acquire(claim_name)
                                self._changed.notify_all()
                                stale = self._pop_stale_connections([claim_name])
                            if watch_deletes:
                                self._start_watch_deletes(watch_deletes)
                            await self._close_connections(stale, "a watch event")
                    if not got_event and time.monotonic() - watch_started < _SHORT_WATCH_SECONDS:
                        failures += 1
                        await asyncio.sleep(batch_utils.retry_delay(failures, None))
                        continue
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
                        await asyncio.sleep(batch_utils.retry_delay(failures, e))
                        continue
                    # Response codes outside of this (e.g. 403, 404) are not recoverable
                    # by retrying, so we error then exit
                    async with self._lock:
                        self._state.note_error(e)
                        self._notify_all()
                    return
                except Exception as e:
                    if _is_transport_error(e):
                        # Likely transient network-level failure so retry
                        failures += 1
                        await asyncio.sleep(batch_utils.retry_delay(failures, e))
                        continue
                    # Anything else is not known to be transient, so surface it via err().
                    async with self._lock:
                        self._state.note_error(e)
                        self._notify_all()
                    return
        finally:
            # Leave no late-claim delete running once the watch loop ends.
            await self._drain_watch_deletes()

    async def _renew_loop(self) -> None:
        """Background lease renewal loop: renews at roughly 1/3 of lease duration
        so a couple of missed renewals in a row still leave margin before the Lease goes stale.
        A failed renewal is retried on a short backoff instead of a full interval, so a brief
        apiserver outage doesn't use up the time left before expiry.
        """
        # Consecutive failed renewals, which set the retry backoff.
        failures = 0
        while True:
            deadline = self._lease_expires_at
            wait = self._renew_interval if failures == 0 else batch_utils.backoff_delay(
                failures, batch_utils.BATCH_BACKOFF_BASE_SECONDS, batch_utils.BATCH_BACKOFF_MAX_SECONDS
            )
            await asyncio.sleep(max(0.0, min(wait, deadline - time.monotonic())))
            if time.monotonic() >= deadline:
                await self._stop_renewal(self._lease_expired_error())
                return
            if not await self._renew_once():
                return
            failures = failures + 1 if self._renewal_degraded else 0

    def _lease_expired_error(self) -> BatchLeaseExpiredError:
        return BatchLeaseExpiredError(
            f"batch '{self.batch_id}' lease has not renewed successfully for {self._lease_duration}s"
        )

    def _mark_renewal_degraded(self, reason: object) -> None:
        """Sets ``_renewal_degraded``, logging the first failure of each episode. Requires ``_lock``."""
        if not self._renewal_degraded:
            logger.info(f"Batch '{self.batch_id}' lease renewal degraded: {reason}")
            self._renewal_degraded = True

    def _renew_request_timeout(self) -> float:
        """Bounds a renewal request by the renew interval and by the time left before expiry."""
        left = self._lease_expires_at - time.monotonic()
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
        # Snapshot the Lease under the lock and skip renewing once this handle detached or lost the
        # Lease, so a late renewal can't revive a Lease it gave up.
        async with self._lock:
            if self._detached or self._lease_error is not None:
                return False
            lease = self._lease
        try:
            try:
                sent_at, written = await self._write_renewal(lease)
            except ApiException as e:
                if e.status == 404:
                    return await self._stop_renewal(self._lease_gone_error())
                if e.status != 409:
                    raise
                # A conflict means another holder took the Lease, or an earlier renewal whose
                # response was lost did land, so read it to tell the two apart.
                current_lease = await self._client.k8s_helper.read_batch_lease(
                    self._lease_name, self.namespace, _request_timeout=self._renew_request_timeout()
                )
                if current_lease is None:
                    return await self._stop_renewal(self._lease_gone_error())
                spec = batch_utils.lease_parts(current_lease)[1]
                if spec.holder_identity != self._holder_identity:
                    return await self._stop_renewal(
                        BatchInUseError(
                            f"batch '{self.batch_id}' Lease is no longer held by this handle "
                            f"(current holder: {spec.holder_identity!r})"
                        )
                    )
                sent_at, written = await self._write_renewal(current_lease)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            async with self._lock:
                if self._detached or self._lease_error is not None:
                    return False
                self._mark_renewal_degraded(e)
            return True
        async with self._lock:
            if self._detached or self._lease_error is not None:
                return False
            self._lease = written
            self._lease_expires_at = sent_at + self._lease_duration
            self._renewal_degraded = False
        return True

    async def _stop_renewal(self, error: BatchError) -> bool:
        """Records why this handle stops renewing the Lease. Returns ``False`` for ``_renew_once``."""
        async with self._lock:
            if self._detached:
                return False
            self._mark_renewal_degraded(error)
            if self._lease_error is None:
                self._lease_error = error
            # Losing the Lease stops creates, renewal, and the watch, since a recreated batch can
            # reuse this batch's claim names.
            self._stop_creating.set()
            if self._renew_task is not None and self._renew_task is not asyncio.current_task():
                self._renew_task.cancel()
            if self._watch_task is not None and self._watch_task is not asyncio.current_task():
                self._watch_task.cancel()
            self._notify_all()
        return False

    async def _write_renewal(self, lease: V1Lease) -> tuple[float, V1Lease]:
        """Stamps ``lease`` as renewed now and replaces it, returning the send time and the written Lease."""
        # Neither Kubernetes client has a default request timeout, so a hung apiserver would
        # otherwise stall renewal past the Lease's expiry.
        timeout = self._renew_request_timeout()
        sent_at = time.monotonic()
        spec = batch_utils.lease_parts(lease)[1]
        spec.holder_identity = self._holder_identity
        spec.renew_time = batch_utils.lease_now()
        spec.lease_duration_seconds = self._lease_duration
        written = await self._client.k8s_helper.replace_batch_lease(
            self._lease_name, self.namespace, lease, _request_timeout=timeout
        )
        return sent_at, written

    def _lease_gone_error(self) -> BatchLeaseExpiredError:
        return BatchLeaseExpiredError(
            f"batch '{self.batch_id}' Lease '{self._lease_name}' no longer exists"
        )

    async def _create_worker(self) -> None:
        """Creates planned claims one at a time. ``max_in_flight`` workers share one pacing slot,
        so together they start at most ``create_rps`` creates per second, except for the creates
        the warm pool's credit covers, which start at once.
        """
        while True:
            async with self._lock:
                self._expire_lease_if_due()
                if (
                    not self._create_plan
                    or self._stop_creating.is_set()
                    or self._batch_error_locked() is not None
                ):
                    return
                claim_name, group = self._create_plan.popleft()
                if self._warm_credit.take(group.warmpool):
                    slot = time.monotonic()
                else:
                    self._warm_credit.spend(group.warmpool)
                    slot = self._reserve_create_slot(time.monotonic())
                self._last_fill_slot = max(self._last_fill_slot, slot)
                # This worker took the last planned create, so the fill deadline is known now
                if not self._create_plan:
                    # Count from the latest fill slot so a warm-covered final group does not expire
                    # an earlier cold group whose claims still wait for their pacing slots.
                    self._fill_deadline = self._last_fill_slot + self._quorum_timeout
                    self._changed.notify_all()
            if await self._wait_for_stop(slot - time.monotonic()):
                return
            sent = 0

            def can_send() -> bool:
                self._expire_fill_if_due()
                if self._state.fill_creates_released(group.warmpool, claim_name):
                    # Keep a member the watch already saw when release_not_ready() kept Ready ones.
                    if self._state.get_member(claim_name) is None:
                        self._state.release_claim(claim_name, group.warmpool)
                        self._changed.notify_all()
                    return False
                # Once the watch observes attempt 1, retrying can only resurrect a later delete.
                if self._state.get_member(claim_name) is not None:
                    return False
                # Only an unstarted create counts as skipped; one already sent may still land.
                if sent == 0 and self._state.group_failed(group.warmpool):
                    self._state.record_skipped_create()
                    self._changed.notify_all()
                    return False
                return True

            def on_send() -> None:
                nonlocal sent
                sent += 1

            try:
                await self._create_one(
                    claim_name,
                    group.warmpool,
                    self._claim_annotations[group.warmpool],
                    self._claim_labels[group.warmpool],
                    self._claim_lifecycle,
                    can_send=can_send,
                    on_send=on_send,
                )
                may_exist = sent > 0
            except Exception as e:
                may_exist = sent > 1 or _is_transient_error(e)
                async with self._lock:
                    self._state.record_create_failure(
                        claim_name, group.warmpool, _create_failure_message(e)
                    )
                    self._changed.notify_all()
            if may_exist:
                # release_member() deletes attempt 1 itself, whereas a retry (sent > 1) in
                # flight during that delete can land afterwards and recreate the claim.
                await self._delete_released_create(claim_name, group.warmpool, retried=sent > 1)

    def _can_still_send(
        self, can_send: Callable[[], bool] | None, now: float | None = None
    ) -> bool:
        """Whether a create or its retry may still send a request. The caller holds ``self._lock``."""
        self._expire_lease_if_due(now)
        # Once err() is set the handle can no longer watch or hold the batch, so further
        # claims would be ones nobody can observe or keep alive.
        return (
            not self._stop_creating.is_set()
            and self._batch_error_locked() is None
            and (can_send is None or can_send())
        )

    async def _delete_released_create(self, claim_name: str, warmpool: str, *, retried: bool = False) -> None:
        """Deletes a fill claim whose create may have landed after the caller released it, its
        group, or ended the fill. A later release call retries a failed delete.
        """
        async with self._lock:
            self._expire_lease_if_due()
            if (
                self._detached
                or self._lease_error is not None
                or not self._state.fill_creates_released(warmpool, claim_name if retried else None)
            ):
                return
            if self._state.get_member(claim_name) is not None:
                # The watch saw the claim before release_not_ready() ended the fill, and
                # release_not_ready() kept it.
                return
            self._state.release_claim(claim_name, warmpool, needs_delete=True)
            self._changed.notify_all()
        try:
            await self._delete_claim(claim_name)
        except asyncio.CancelledError:
            raise
        except BatchError:
            # detach(), release(), or losing the Lease stopped this handle from deleting claims.
            pass
        except Exception as e:
            logger.warning(f"Batch '{self.batch_id}' failed to delete released claim '{claim_name}': {e}")

    async def _wait_for_stop(self, timeout: float) -> bool:
        """Waits up to ``timeout`` seconds for creation to be stopped, and returns whether it was."""
        if self._stop_creating.is_set() or timeout <= 0:
            return self._stop_creating.is_set()
        with contextlib.suppress(asyncio.TimeoutError):
            await asyncio.wait_for(self._stop_creating.wait(), timeout)
        return self._stop_creating.is_set()

    def _reserve_create_slot(self, earliest: float) -> float:
        """Reserves the next pacing slot at or after ``earliest``, so creates and their retries
        together stay within ``create_rps``. The caller holds ``self._lock``.
        """
        slot = max(earliest, self._next_create_at)
        self._next_create_at = slot + self._create_interval
        return slot

    def _check_retry_deadline(self, claim_name: str, deadline: float | None, earliest: float, error: Exception) -> None:
        """Raises ``BatchTimeoutError`` if a retry's pacing slot at or after ``earliest`` would be at or
        after ``deadline``. The caller holds ``self._lock``.
        """
        # A retry paced past the deadline would only make a claim to delete on timeout.
        if deadline is not None and max(earliest, self._next_create_at) >= deadline:
            raise BatchTimeoutError(f"no create retry slot for claim '{claim_name}' before the deadline") from error

    async def _send_create(
        self,
        claim_name: str,
        warmpool: str,
        annotations: dict[str, str],
        labels: dict[str, str],
        lifecycle: dict[str, str],
        deadline: float | None,
        can_send: Callable[[], bool] | None = None,
        on_send: Callable[[], None] | None = None,
        pod_metadata: AdditionalPodMetadata | None = None,
    ) -> None:
        """Sends one claim create while holding one of the ``max_in_flight`` slots, releasing the
        slot before any retry backoff sleep, unless creation stopped or ``can_send`` skipped the
        request while waiting.

        Raises:
            BatchTimeoutError: If no slot frees up before ``deadline``.
        """
        async with self._lock:
            if not self._can_still_send(can_send):
                return
        acquired = False
        wait = None if deadline is None else deadline - time.monotonic()
        if wait is None or wait > 0:
            with contextlib.suppress(TimeoutError):
                async with asyncio.timeout(wait):
                    await self._in_flight.acquire()
                    acquired = True
        if not acquired:
            async with self._lock:
                # Surface detach, release, or an already-observed claim instead of a slot timeout.
                if not self._can_still_send(can_send):
                    return
            raise BatchTimeoutError(f"no create in flight slot for claim '{claim_name}' before the deadline")
        try:
            async with self._lock:
                now = time.monotonic()
                if not self._can_still_send(can_send, now):
                    return
                # Waiting on _lock after _in_flight.acquire can cross deadline, so check again
                # before recording or sending the create.
                if deadline is not None and now >= deadline:
                    raise BatchTimeoutError(f"no create in flight slot for claim '{claim_name}' before the deadline")
                # Bound the HTTP timeout by the remaining Lease budget so an attempt started near
                # expiry cannot outlive the reaper's confirmation window.
                request_timeout = min(
                    batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS, self._lease_expires_at - now
                )
                if on_send is not None:
                    on_send()
            await self._client.k8s_helper.create_sandbox_claim(
                claim_name,
                warmpool,
                self.namespace,
                annotations=annotations,
                labels=labels,
                lifecycle=lifecycle,
                pod_metadata=pod_metadata,
                owner_references=self._owner_references,
                batch=True,
                log_level=logging.DEBUG,
                _request_timeout=request_timeout,
            )
        finally:
            self._in_flight.release()

    async def _create_one(
        self,
        claim_name: str,
        warmpool: str,
        annotations: dict[str, str],
        labels: dict[str, str],
        lifecycle: dict[str, str],
        deadline: float | None = None,
        can_send: Callable[[], bool] | None = None,
        on_send: Callable[[], None] | None = None,
        pod_metadata: AdditionalPodMetadata | None = None,
    ) -> None:
        """Creates one claim, retrying transient errors, with each retry taking a pacing slot.

        Checks ``can_send`` under the batch lock before each attempt and retry wait, skipping the
        create when it returns ``False``, and calls ``on_send`` right before each attempt sends
        ``create_sandbox_claim``.

        Raises:
            BatchTimeoutError: If a retry's pacing slot is at or after ``deadline``, or no
                ``max_in_flight`` slot frees up before it.
            Exception: The error the create failed with, once it isn't retried.
        """
        attempt = 1
        while True:
            try:
                await self._send_create(
                    claim_name,
                    warmpool,
                    annotations,
                    labels,
                    lifecycle,
                    deadline,
                    can_send=can_send,
                    on_send=on_send,
                    pod_metadata=pod_metadata,
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
                retry_after = batch_utils.retry_after_seconds(e)
                if isinstance(e, ApiException) and e.status == 429:
                    async with self._lock:
                        first_throttle = self._warm_credit.throttle(retry_after)
                    if first_throttle:
                        logger.info(
                            f"Apiserver throttled batch '{self.batch_id}', temporarily pacing creates at create_rps"
                        )
                if outcome == batch_utils.CreateOutcome.SUCCESS:
                    return
                if outcome == batch_utils.CreateOutcome.RETRY:
                    # A backoff belongs to this create alone, so the create waits it out here and then takes a
                    # normal pacing slot. client-go retries the same way. The apiserver computes a Retry-After for a
                    # whole priority level, so a Retry-After moves the shared slot and every create waits for it.
                    if retry_after is None:
                        backoff = batch_utils.retry_delay(attempt, None)
                        async with self._lock:
                            if not self._can_still_send(can_send):
                                return
                            self._check_retry_deadline(claim_name, deadline, time.monotonic() + backoff, e)
                        if await self._wait_for_stop(backoff):
                            return
                    async with self._lock:
                        if not self._can_still_send(can_send):
                            return
                        earliest = time.monotonic() + (retry_after or 0.0)
                        # Other creates keep taking slots during a backoff, so the create checks the deadline again.
                        self._check_retry_deadline(claim_name, deadline, earliest, e)
                        slot = self._reserve_create_slot(earliest)
                    if await self._wait_for_stop(slot - time.monotonic()):
                        return
                    attempt += 1
                    continue
                logger.debug(f"Batch '{self.batch_id}' failed to create claim '{claim_name}': {e}")
                raise


async def _precheck_pools(
    helper: "AsyncK8sHelper",
    warmpools: Iterable[str],
    namespace: str,
    timeout: int | float | Callable[[], Awaitable[float]] = batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
) -> dict[str, int]:
    """Checks that each warm pool and the SandboxTemplate it references exist, and returns each
    pool's Ready replica count.

    Raises:
        SandboxWarmPoolNotFoundError: If a warm pool doesn't exist.
        SandboxTemplateNotFoundError: If a warm pool's template doesn't exist, or it names none.
    """
    checked_templates: set[str] = set()
    ready: dict[str, int] = {}
    for name in warmpools:
        step_timeout = await timeout() if callable(timeout) else timeout
        warmpool = await helper.get_sandbox_warmpool(
            name, namespace, _request_timeout=step_timeout
        )
        if warmpool is None:
            raise SandboxWarmPoolNotFoundError(
                f"SandboxWarmPool '{name}' not found in namespace '{namespace}'"
            )
        ready[name] = batch_utils.warmpool_ready_replicas(warmpool)
        template = batch_utils.warmpool_template_name(warmpool)
        if template is None:
            raise SandboxTemplateNotFoundError(
                f"SandboxWarmPool '{name}' does not reference a SandboxTemplate"
            )
        if template in checked_templates:
            continue
        step_timeout = await timeout() if callable(timeout) else timeout
        if await helper.get_sandbox_template(
            template, namespace, _request_timeout=step_timeout
        ) is None:
            raise SandboxTemplateNotFoundError(
                f"SandboxTemplate '{template}' not found in namespace '{namespace}'"
            )
        checked_templates.add(template)
    return ready


async def _take_batch_lease(
    helper: "AsyncK8sHelper",
    namespace: str,
    batch_id: str,
    lease_name: str,
    holder_identity: str,
    lease_duration: int,
    last_lease: V1Lease,
) -> str | None:
    """Writes the batch's Lease with ``holder_identity`` as its holder and returns the
    resourceVersion written, or returns ``None`` if the Lease is missing. The write is
    conditional, so it doesn't replace a Lease another client wrote after it was read.
    ``last_lease`` is the Lease from that handle's last successful write.
    ``release()`` calls this before each delete round. By then renewal has stopped, and the
    handle may have detached or let its Lease expire, so this checks that the batch is still the
    one the handle owned and makes the handle its holder again. That keeps ``get_batch`` from
    attaching another handle while claims are deleted. The other Lease writes check different
    things, so they don't use it.

    Raises:
        BatchInUseError: If another handle holds the Lease or writes it first, or the Lease
            changed hands since ``last_lease`` was written.
    """
    timeout = batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
    retried = False
    # This runs at most twice, because a second conflict raises.
    while True:
        lease = await helper.read_batch_lease(lease_name, namespace, _request_timeout=timeout)
        now = batch_utils.lease_now()
        if lease is None:
            return None
        meta, spec = batch_utils.lease_parts(lease)
        last_meta, last_spec = batch_utils.lease_parts(last_lease)
        # Another handle that attached since this handle's last write raised leaseTransitions, and a
        # Lease that was deleted and created again has a new UID. Either Lease can have no holder by
        # now, so the holder check below wouldn't catch it.
        if meta.uid != last_meta.uid or (spec.lease_transitions or 0) != (
            last_spec.lease_transitions or 0
        ):
            raise BatchInUseError(f"batch '{batch_id}' changed hands since this handle held it")
        if spec.holder_identity and spec.holder_identity != holder_identity:
            raise BatchInUseError(f"batch '{batch_id}' is held by '{spec.holder_identity}'")
        spec.holder_identity = holder_identity
        spec.renew_time = now
        spec.lease_duration_seconds = lease_duration
        try:
            written = await helper.replace_batch_lease(lease_name, namespace, lease, _request_timeout=timeout)
        except ApiException as e:
            if e.status != 409:
                raise
            if retried:
                raise BatchInUseError(f"batch '{batch_id}' Lease was written by another client") from e
            # A 409 means the Lease changed after it was read. It can still be held by this handle
            # only if one of its own renewals landed late, so it is read and written once more.
            retried = True
            continue
        written_meta, _ = batch_utils.lease_parts(written)
        if not written_meta.resource_version:
            raise BatchError(
                f"batch '{batch_id}' Lease in namespace '{namespace}' has no resourceVersion"
            )
        return str(written_meta.resource_version)


async def _delete_batch_objects(
    helper: "AsyncK8sHelper",
    namespace: str,
    batch_id: str,
    lease_name: str,
    holder_identity: str,
    lease_duration: int,
    last_lease: V1Lease,
) -> None:
    """Deletes a batch's claims in rounds, then its Lease once no claim is left that isn't
    already being deleted.

    Each round first takes the Lease for ``holder_identity``, so every delete is sent while that
    handle holds it or no Lease exists, and the Lease is deleted only at the resourceVersion that
    handle wrote. ``last_lease`` is the Lease from that handle's last successful write.

    Raises:
        BatchInUseError: If another handle holds the Lease or writes it first, or the Lease
            changed hands since ``last_lease`` was written.
        BatchError: If claims are still not being deleted after the last round.
        ApiException: For a non-transient error at once, or the last transient error after
            the last round.
    """
    resource_version: str | None = None

    async def take() -> None:
        nonlocal resource_version
        resource_version = await _take_batch_lease(
            helper, namespace, batch_id, lease_name, holder_identity, lease_duration, last_lease
        )

    async def finish() -> None:
        # This round found no Lease, so there is none to delete.
        if resource_version is not None:
            await helper.delete_batch_lease(
                lease_name,
                namespace,
                _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                resource_version=resource_version,
            )

    lease_uid = batch_utils.lease_owner_reference(last_lease)["uid"]
    await _delete_claims_in_rounds(
        helper,
        namespace,
        batch_utils.batch_claim_label_selector(batch_id, lease_uid),
        f"batch '{batch_id}'",
        hold=take,
        finish=finish,
    )


async def _delete_claims_in_rounds(
    helper: "AsyncK8sHelper",
    namespace: str,
    label_selector: str,
    description: str,
    hold: Callable[[], Awaitable[None]] | None = None,
    finish: Callable[[], Awaitable[None]] | None = None,
    guard: Callable[[], contextlib.AbstractAsyncContextManager[None]] = contextlib.nullcontext,
    deadline: Callable[[], float] | None = None,
) -> None:
    """Deletes the claims matching ``label_selector`` with deletecollection rounds, re-listing
    between rounds until no claim is left that isn't already being deleted, then calls ``finish``.
    Each round first calls ``hold``, and ``guard`` wraps each request and runs before retry sleep,
    raising when the caller can no longer delete claims.

    A transient error, in ``hold`` and ``finish`` too, moves on to the next round, and it gives up
    after ``BATCH_RELEASE_MAX_IDLE_ROUNDS`` rounds in a row that delete nothing.

    Raises:
        BatchError: If claims are still not being deleted after the last round.
        ApiException: For a non-transient error at once, or the last transient error after
            the last round.
    """
    collection_timeout = batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
    idle_rounds = 0
    remaining_before: int | None = None

    def step_budget() -> tuple[int | float, float | None]:
        """Returns this step's request timeout and the current Lease deadline.

        The timeout is the collection timeout capped by the time left on the Lease, read fresh each
        step because renewal can extend it. Raises ``TimeoutError`` once the Lease time is used up,
        and the caller's next ``guard()`` then raises the Lease error.
        """
        if deadline is None:
            return collection_timeout, None
        round_deadline = deadline()
        remaining = round_deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError(f"{description} delete deadline elapsed")
        return min(float(collection_timeout), remaining), round_deadline

    def is_round_retryable(error: Exception) -> bool:
        """Treats a Lease deadline ``TimeoutError`` like a transient error, so the next ``guard()``
        reports the lost Lease instead of the timeout.
        """
        if _is_transient_error(error):
            return True
        return deadline is not None and not isinstance(error, BatchError) and isinstance(error, TimeoutError)

    while True:
        last_error: Exception | None = None
        held = False
        try:
            if hold is not None:
                await hold()
            held = True
            # deletecollection deletes one claim at a time on the apiserver, so for a large batch
            # it can time out while deletion continues. For this case, we track it with the list below.
            async with guard():
                step_timeout, _ = step_budget()
                await helper.delete_sandbox_claims_by_label(
                    namespace, label_selector, _request_timeout=step_timeout
                )
        except Exception as e:
            if not is_round_retryable(e):
                raise
            last_error = e

        live: int | None = None
        try:
            # A consistent read (no resourceVersion), so a lagging watch cache can't report
            # that no claims are left too early.
            async with guard():
                step_timeout, round_deadline = step_budget()
                live = await helper.count_live_sandbox_claims(
                    namespace,
                    label_selector,
                    _request_timeout=step_timeout,
                    deadline=round_deadline,
                )
        except Exception as e:
            if not is_round_retryable(e):
                raise
            last_error = e

        # Only a round whose hold succeeded can finish. After a failed hold the Lease may still exist,
        # and this round has no resourceVersion to delete it at.
        if live == 0 and held:
            try:
                if finish is not None:
                    await finish()
                return
            except Exception as e:
                if not is_round_retryable(e):
                    raise
                last_error = e

        # A round that shrinks the number of live claims doesn't count toward giving up, so a large
        # batch that takes many rounds to delete isn't abandoned while it is still making progress.
        progressed = live is not None and (remaining_before is None or live < remaining_before)
        idle_rounds = 0 if progressed else idle_rounds + 1
        if idle_rounds >= batch_utils.BATCH_RELEASE_MAX_IDLE_ROUNDS:
            # If the Lease ran out, report that rather than the last transient error.
            if deadline is not None:
                async with guard():
                    pass
            if last_error is not None:
                raise last_error
            raise BatchError(
                f"{description}: {live or 0} claims are still not being deleted"
            )
        if live is not None:
            remaining_before = live
        # Fail fast if the handle detached or stopped instead of sleeping through retry backoff,
        # without counting the sleep itself as an active delete.
        async with guard():
            delay = batch_utils.retry_delay(max(idle_rounds, 1), last_error)
            if deadline is not None:
                delay = min(delay, max(0.0, deadline() - time.monotonic()))
        await asyncio.sleep(delay)
