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

"""Sync handle for using a claimed or re-attached sandbox batch."""

import contextlib
import logging
import math
import ssl
import threading
import time
from collections import Counter, deque
from collections.abc import Callable, Iterable, Iterator
from concurrent.futures import CancelledError, Future, ThreadPoolExecutor
from functools import partial
from typing import TYPE_CHECKING, Any, TypeVar

import urllib3.exceptions
from kubernetes import client
from kubernetes.client import V1Lease, V1LeaseSpec, V1ObjectMeta
from kubernetes.client.exceptions import ApiException

from . import batch_state, batch_utils
from .constants import (
    BATCH_GROUP_LABEL,
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    BATCH_ID_LABEL,
    BATCH_LEASE_NAME_PREFIX,
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
from .models import BatchEvent, BatchGroup, GroupOutcome, Member, MemberState
from .utils import construct_sandbox_claim_lifecycle_spec, select_pod_ip

if TYPE_CHECKING:
    from kubernetes_asyncio.client import V1Lease as AsyncV1Lease

    from .k8s_helper import K8sHelper
    from .sandbox import Sandbox
    from .sandbox_client import SandboxClient

logger = logging.getLogger(__name__)

# Timeout used by the apiserver to close the stream after this duration. This is used so the watcher
# thread periodically rechecks for a stop request instead of blocking on the connection indefinitely.
_WATCH_TIMEOUT_SECONDS = 30

# Client-side timeout to handle connections that have died without a close, which timeout_seconds above would not
# be able to handle. Set longer than _WATCH_TIMEOUT_SECONDS so the apiserver ends a healthy stream first.
_WATCH_CLOSE_GRACE_SECONDS = 5
_WATCH_REQUEST_TIMEOUT = _WATCH_TIMEOUT_SECONDS + _WATCH_CLOSE_GRACE_SECONDS

# A watch that ends sooner than this without an event counts as a failed attempt, as in client-go's
# reflector. Otherwise a stream that a proxy or a broken connection ends at once would be reopened in a busy loop.
_SHORT_WATCH_SECONDS = 1

# Network-level errors that are likely transient, so the watch, creates, and release retry them.
_TRANSPORT_ERRORS = (
    urllib3.exceptions.ProtocolError,
    urllib3.exceptions.ReadTimeoutError,
    urllib3.exceptions.MaxRetryError,
    # urllib3 doesn't retry Lease requests, so a refused or timed-out connect raises this error instead
    # of a MaxRetryError. NewConnectionError subclasses it.
    urllib3.exceptions.ConnectTimeoutError,
    ConnectionError,
    # A peer that closes the TLS connection early (e.g. a load balancer during a control-plane upgrade)
    # is transient like a connection reset. Certificate and other TLS failures are not.
    ssl.SSLEOFError,
)


def _is_transport_error(error: BaseException) -> bool:
    # A TLS failure is judged by the ssl error urllib3 wrapped, so a MaxRetryError caused by one is
    # not retried just because MaxRetryError is in the tuple.
    cause = error.reason if isinstance(error, urllib3.exceptions.MaxRetryError) else error
    if isinstance(cause, urllib3.exceptions.SSLError):
        error = cause.args[0] if cause.args else cause
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


_T = TypeVar("_T")


class SandboxBatch:
    """A handle to a batch's claims, obtained via ``SandboxClient.claim_batch`` or
    ``SandboxClient.get_batch``.

    Keeps a live cache of the batch's claims through one label-scoped watch
    (a background daemon thread), renews the batch Lease on another daemon
    thread, and exposes methods for connecting to ready sandboxes and detaching from the batch.
    A claimed batch also creates its claims on background worker threads.
    """

    def __init__(
        self,
        client: "SandboxClient[Any]",
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

        self._lock = threading.Lock()
        # Serializes release()'s teardown and detach()'s Lease writes, so whichever enters first
        # finishes before the other acts on the Lease. It's reentrant, so a call from a signal handler
        # on the same thread doesn't hang.
        self._lifecycle_lock = threading.RLock()
        # Each cached connection, with the pod IP it was made to.
        self._connected: dict[str, tuple["Sandbox", str | None]] = {}
        # The watch moves a cached connection here when its member is gone or runs on another sandbox
        # or pod IP. connect() reads only _connected, so it never hands out these connections. Each
        # connection stays here until it closes, so the next cleanup retries a close that failed.
        self._stale_connections: list["Sandbox"] = []
        # get_batch sets this, so client exit cleanup detaches the handle instead of deleting a batch that
        # another client may own.
        self._attached = False
        self._detached = False
        self._lease_released = False
        # The Lease duration detach() writes, the grace from the last detach() call that gave one or else
        # the batch's Lease duration, kept so that a retry without a grace, including the one client
        # cleanup makes, writes the same one.
        self._detach_grace: int | None = None
        self._watch_stop = threading.Event()
        self._renew_stop = threading.Event()
        self._renewal_degraded = False
        # The Lease from this handle's last successful write. Renewal sends it back without reading
        # the Lease first, and only reads it if the write conflicts.
        self._lease = lease
        # The claims this handle creates are owned by its Lease, so the garbage collector deletes
        # them once the Lease is gone, even if no reaper or release() deletes them.
        self._owner_references = [batch_utils.lease_owner_reference(lease)]
        # When this handle considers its Lease expired. It's a little earlier than the expiry other
        # clients see, so the handle never thinks it holds the batch after someone else could take it.
        self._lease_expires_at = lease_renewed_at + lease_duration
        self._watch_thread: threading.Thread | None = None
        self._renew_thread: threading.Thread | None = None

        # Notified on every change consumers wait for: watch events, create failures, errors,
        # the fill deadline being set, and detach/release.
        self._changed = threading.Condition(self._lock)
        self._quorum_timeout = quorum_timeout
        self._work_budget = work_budget
        # Monotonic time at which the fill expires. After it, groups without an outcome time out
        # and events() stops waiting for fill members that aren't Ready yet. Set once the last
        # create's pacing slot is taken, or at attach for a re-attached batch.
        self._fill_deadline: float | None = None
        self._last_fill_slot: float = 0.0
        self._released = False
        self._creators: list[threading.Thread] = []
        self._stop_creating = threading.Event()
        self._create_plan: deque[tuple[str, BatchGroup]] = deque()
        self._next_create_at = 0.0
        self._create_interval = 0.0
        self._warm_credit = batch_utils.WarmCredit()
        # Bounds concurrent claim creates (fill and acquire()), acquire() pool reads, and member
        # deletes to max_in_flight, matching the HTTP connection pool budget that claim_batch checks.
        self._in_flight = threading.BoundedSemaphore(max_in_flight)
        self._max_in_flight = max_in_flight
        # Counts in-flight member and group delete requests per thread so detach() and release()
        # wait for other threads' deletes before giving up the Lease, without hanging a signal
        # handler that calls release() or detach() on a deleting thread.
        self._active_deletes: Counter[int] = Counter()
        # One per pool, so concurrent acquires wait for one read of the pool's Ready count.
        self._credit_refresh_locks: dict[str, threading.Lock] = {}
        # The caller's labels and the batch id label, which every claim carries.
        self._labels: dict[str, str] = {BATCH_ID_LABEL: batch_id}
        # Pools acquire() doesn't check again, since claim_batch checked them or they already have claims.
        self._checked_pools: set[str] = {g.warmpool for g in state.groups}
        # The check of each pool that is running, so concurrent first acquires of a pool share it.
        self._prechecks: dict[str, Future[None]] = {}
        # Set when an in-flight acquire()'s claim resolves or the batch stops, so watch events for
        # other claims don't wake every waiting acquire().
        self._acquire_waiters: dict[str, threading.Event] = {}
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
    def _claim(
        cls,
        client: "SandboxClient[Any]",
        args: batch_utils.ClaimBatchArgs,
        namespace: str,
        trace_annotations: dict[str, str],
    ) -> "SandboxBatch":
        """Create a batch: precheck, create its Lease, start the watch, then create claims in the background."""
        helper = client.k8s_helper
        timeout = batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        ready = _precheck_pools(helper, [g.warmpool for g in args.groups], namespace)

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
        label_selector = f"{BATCH_ID_LABEL}={args.batch_id}"
        holder_identity = batch_utils.generate_holder_identity()
        lease_labels, lease_annotations = batch_utils.batch_lease_metadata(args)
        # Read before renewTime is stamped, so this handle's expiry is never later than the one
        # other clients see.
        renewed_at = time.monotonic()
        now = batch_utils.lease_now()
        lease = V1Lease(
            metadata=V1ObjectMeta(
                name=lease_name, labels=lease_labels, annotations=lease_annotations
            ),
            spec=V1LeaseSpec(
                holder_identity=holder_identity,
                lease_duration_seconds=args.lease_duration,
                acquire_time=now,
                renew_time=now,
                lease_transitions=0,
            ),
        )
        try:
            created = helper.create_batch_lease(namespace, lease, _request_timeout=timeout)
        except ApiException as e:
            if e.status == 409:
                raise BatchExistsError(
                    f"batch '{args.batch_id}' already exists in namespace '{namespace}'"
                ) from e
            raise
        try:
            # The list's resourceVersion is where the watch starts, so no create is missed.
            claim_items, list_rv = helper.list_sandbox_claim_objects(
                namespace, label_selector, _request_timeout=timeout
            )
            if claim_items:
                raise BatchExistsError(
                    f"claims with batch id '{args.batch_id}' already exist in namespace '{namespace}'"
                )
        except BaseException:
            # BaseException too, so a cancel or Ctrl-C during the list doesn't leave the Lease behind.
            try:
                helper.delete_batch_lease(lease_name, namespace, _request_timeout=timeout)
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
        handle._labels = {**args.labels, BATCH_ID_LABEL: args.batch_id}
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
        for _ in range(min(args.max_in_flight, len(handle._create_plan))):
            creator = threading.Thread(target=handle._create_worker, daemon=True)
            handle._creators.append(creator)
            creator.start()
        return handle

    @classmethod
    def _attach(cls, client: "SandboxClient[Any]", batch_id: str, namespace: str) -> "SandboxBatch":
        """Attach to an existing batch and take over its Lease."""
        batch_utils.validate_batch_id(batch_id)

        lease_name = f"{BATCH_LEASE_NAME_PREFIX}{batch_id}"
        label_selector = f"{BATCH_ID_LABEL}={batch_id}"

        lease = client.k8s_helper.read_batch_lease(
            lease_name, namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        )
        claim_items, list_rv = client.k8s_helper.list_sandbox_claim_objects(
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
            written = client.k8s_helper.replace_batch_lease(
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
        handle._labels = {**settings.labels, BATCH_ID_LABEL: batch_id}
        handle._attached = True
        handle._start()
        return handle

    def _start(self) -> None:
        self._watch_thread = threading.Thread(target=self._watch_loop, daemon=True)
        self._renew_thread = threading.Thread(target=self._renew_loop, daemon=True)
        self._watch_thread.start()
        self._renew_thread.start()

    def _check_active(self) -> None:
        if self._detached:
            raise BatchError(f"batch '{self.batch_id}' has been detached or released")

    def _check_healthy(self) -> None:
        # Once detach() or release() starts or err() is set, this handle may no longer hold the
        # batch's Lease, and a batch recreated under the same id reuses the claim names and labels,
        # so the handle connects to no more sandboxes and deletes no more claims.
        self._check_active()
        if (error := self._state.error()) is not None:
            raise BatchError(f"batch '{self.batch_id}' has stopped") from error

    def _check_lease_held(self) -> None:
        with self._lock:
            self._check_healthy()

    @contextlib.contextmanager
    def _member_delete(self) -> Iterator[None]:
        """Tracks one in-flight claim delete request without holding a lock across network I/O.

        Holding ``_lock`` or ``_lifecycle_lock`` across the HTTP call would serialize concurrent
        ``release_member``, ``release_not_ready``, and ``release_group`` deletes. Taking ``_lock``
        only to check health and update ``_active_deletes`` lets HTTP deletes run in parallel while
        still fencing against ``detach()`` and ``release()``, which set ``_detached`` under
        ``_lock`` to reject new deletes and call ``_wait_active_deletes()`` to drain active ones.
        """
        tid = threading.get_ident()
        with self._lock:
            self._check_healthy()
            self._active_deletes[tid] += 1
        try:
            yield
        finally:
            with self._lock:
                self._active_deletes[tid] -= 1
                if not self._active_deletes[tid]:
                    del self._active_deletes[tid]
                if self._detached:
                    self._changed.notify_all()

    def _wait_active_deletes(self, for_detach: bool = False) -> None:
        """Waits for other threads' in-flight claim deletes once the caller sets ``_detached``.

        ``detach()`` and ``release()`` call this before stopping Lease renewal or writing the Lease,
        so no lingering delete from this handle lands after another handle attaches or a recreated
        batch reuses the same claim names. Subtracting the current thread's own count avoids a
        deadlock when a signal handler calls ``release()`` or ``detach()`` on a thread mid-delete,
        and ``for_detach`` exits early if ``release()`` takes over or the Lease is already released.
        """
        tid = threading.get_ident()
        with self._changed:
            while self._active_deletes.total() > self._active_deletes[tid]:
                if for_detach and (self._release_started() or self._lease_released):
                    return
                self._changed.wait()

    def members(self, warmpool: str | None = None) -> list[Member]:
        """Returns a snapshot of the batch's members, optionally filtered to one warm pool."""
        with self._lock:
            return self._state.members(warmpool)

    def connect(self, member: Member) -> "Sandbox":
        """Returns a connected ``Sandbox`` for a ready member. If this handle is already connected
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
        with self._lock:
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
            sandbox: Sandbox = self._client.sandbox_class(
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
        with self._lock:
            return self._state.error()

    def lease_degraded(self) -> bool:
        """Returns whether background Lease renewal is failing.

        ``True`` from the first failed renewal until a renewal succeeds. It stays ``True`` once
        renewal stops on an error, such as the Lease expiring, so it can be polled at any time.
        """
        with self._lock:
            return self._renewal_degraded

    def events(self) -> Iterator[BatchEvent]:
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
        with self._lock:
            self._check_active()
            self._state.set_mode(batch_state.ConsumerMode.STREAM)
        return self._iterate(self._state.pop_event, self._state.events_done)

    def group_outcomes(self) -> Iterator[GroupOutcome]:
        """Returns an iterator that yields one ``GroupOutcome`` per group, as soon as its outcome is known.

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
        with self._lock:
            self._check_active()
            self._state.set_mode(batch_state.ConsumerMode.GROUP)
        return self._iterate(self._state.pop_group_outcome, self._state.groups_done)

    def wait_for_quorum(self) -> list[Member]:
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
        with self._lock:
            self._check_active()
            self._state.set_mode(batch_state.ConsumerMode.QUORUM)
        try:
            result = self._next(self._state.quorum_result, lambda: False)
            if result is None:
                raise BatchError(f"batch '{self.batch_id}' stopped while waiting for quorum") from self._state.error()
        except BaseException:
            # This was the only wait, so a quorum left undecided would hold its members forever.
            with self._changed:
                self._state.fail_quorum(BatchTimeoutError("wait_for_quorum() ended before the quorum was reached"))
                self._changed.notify_all()
            raise
        members, error = result
        if error is not None:
            raise error
        return members

    def _iterate(self, pop: Callable[[], _T | None], done: Callable[[], bool]) -> Iterator[_T]:
        while (item := self._next(pop, done)) is not None:
            yield item

    def _next(self, pop: Callable[[], _T | None], done: Callable[[], bool]) -> _T | None:
        with self._changed:
            while True:
                self._check_active()
                self._expire_fill_if_due()
                item = pop()
                if item is not None:
                    return item
                if done() or self._state.error() is not None:
                    return None
                # Nothing to hand out yet, but returning None would end the iterator, so wait for
                # a change. The wait also wakes at the fill deadline so pending members can expire.
                timeout = None
                if self._fill_deadline is not None:
                    timeout = max(0.0, self._fill_deadline - time.monotonic())
                self._changed.wait(timeout)

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

    def acquire(self, warmpool: str, timeout: float | None = None) -> Member:
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
        retried by ``release_not_ready()`` and ``release()``. Once ``err()`` is set the claim is kept,
        since the handle may no longer hold the batch's Lease, and ``release()`` deletes it. It never
        creates a second claim, so call it again to try again. A member it returned that fails later
        shows in ``members()`` like any other, and ``release_member`` frees it.

        ``TerminalMemberError`` and ``BatchTimeoutError`` are both ``BatchError``, so
        ``except BatchError`` handles every runtime failure. The other errors mean a bad argument
        or a missing pool or template.

        Args:
            warmpool: The warm pool to claim the member from.
            timeout: Seconds to wait for the member to be Ready. Defaults to the batch's
                ``quorum_timeout``.

        Raises:
            ValueError: If ``timeout`` isn't a positive finite number.
            SandboxWarmPoolNotFoundError: If the warm pool doesn't exist.
            SandboxTemplateNotFoundError: If the warm pool's template doesn't exist, or it names none.
            TerminalMemberError: If the create fails (the member is ``FAILED`` with reason
                ``CreateFailed``), the member fails terminally, or someone else deletes its claim
                (the member is ``LOST``) before it is Ready.
            BatchTimeoutError: If the member isn't Ready within ``timeout``. ``acquire()`` skips
                sending any create or create retry that ``create_rps`` pacing or ``max_in_flight``
                cannot start before the deadline.
            BatchError: If this handle has been detached or released, or ``err()`` is set, when
                called or while waiting, or if ``release_member`` or ``release_group`` releases the
                member before it is Ready.
        """
        wait = batch_utils.positive_number("timeout", timeout, self._quorum_timeout)
        deadline = time.monotonic() + wait
        with self._lock:
            self._check_healthy()
            need_check = warmpool not in self._checked_pools
            acquire_seq = self._state.enter_acquire(warmpool)
        entered_acquire = True
        refreshed = False
        claim_name: str | None = None
        waiter: threading.Event | None = None
        # Whether a claim of the batch may exist under the name, so a failure deletes it.
        may_exist = False
        sent = 0
        try:
            if need_check:
                self._check_pool(warmpool, deadline, wait, acquire_seq)
            while True:
                with self._lock:
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
                        waiter = self._acquire_waiters[claim_name] = threading.Event()
                        slot = now if covered else self._reserve_create_slot(now)
                        break
                # Refresh only when create_rps pacing would delay this create, since a free pacing slot
                # already starts at once without an extra pool GET.
                self._refresh_warm_credit(warmpool, deadline, acquire_seq)
                refreshed = True
            assert claim_name is not None and waiter is not None
            acquired_name = claim_name
            # Once creating stops, the handle is detached or released, so _wait_acquired raises.
            if not self._stop_creating.wait(max(0.0, slot - time.monotonic())):
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
                    self._create_one(
                        acquired_name,
                        warmpool,
                        self._client._trace_context_annotations(),
                        # The group label lets release_group() free it with the pool's other members.
                        {**self._labels, BATCH_GROUP_LABEL: batch_utils.group_label_value(warmpool)},
                        lifecycle,
                        deadline,
                        can_send=can_send,
                        on_send=on_send,
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
            return self._wait_acquired(acquired_name, waiter, deadline, wait)
        except BaseException:
            if entered_acquire:
                with self._lock:
                    self._state.exit_acquire(warmpool)
            if claim_name is not None:
                self._drop_acquired(claim_name, may_exist, retried=sent > 1)
            raise

    def _check_acquire_pool_active(self, warmpool: str, acquire_seq: int) -> None:
        """Raises if ``release_group()`` released ``warmpool`` since this ``acquire()`` started.
        Requires ``_lock``.
        """
        if self._state.pool_release_seq(warmpool) != acquire_seq:
            raise BatchError(f"warm pool '{warmpool}' was released while acquiring")

    def _wait_acquired(
        self, claim_name: str, waiter: threading.Event, deadline: float, wait: float
    ) -> Member:
        while True:
            with self._lock:
                waiter.clear()
                self._check_active()
                member = self._state.acquired_member(claim_name)
                if member is not None:
                    self._acquire_waiters.pop(claim_name, None)
                    return member
                if (error := self._state.error()) is not None:
                    raise BatchError(f"batch '{self.batch_id}' stopped while acquiring '{claim_name}'") from error
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise BatchTimeoutError(f"acquired member '{claim_name}' wasn't Ready within {wait:g}s")
            waiter.wait(remaining)

    def _check_pool(self, warmpool: str, deadline: float, wait: float, acquire_seq: int) -> None:
        """Checks a pool that isn't one of the batch's groups the first time ``acquire()`` uses it,
        and records its Ready count as credit. Concurrent first acquires of a pool share one check
        and its outcome. A failed check leaves no cached entry, so a later acquire checks the pool
        again.

        Raises:
            BatchTimeoutError: If another acquire's check of the pool doesn't finish by ``deadline``,
                or no ``max_in_flight`` slot frees up before it.
        """
        while True:
            with self._lock:
                self._check_healthy()
                self._check_acquire_pool_active(warmpool, acquire_seq)
                if warmpool in self._checked_pools:
                    return
                if time.monotonic() >= deadline:
                    raise BatchTimeoutError(f"warm pool '{warmpool}' wasn't checked within {wait:g}s")
                check = self._prechecks.get(warmpool)
                if check is None:
                    check = self._prechecks[warmpool] = Future()
                    break
            try:
                check.result(timeout=max(0.0, deadline - time.monotonic()))
            except TimeoutError:
                if not check.done() or check.cancelled():
                    raise BatchTimeoutError(f"warm pool '{warmpool}' wasn't checked within {wait:g}s") from None
                # The check itself timed out, so this acquire raises the check's error.
                check.result()
            except CancelledError:
                # The acquire that ran the check was interrupted, so this one checks the pool itself.
                continue
            return
        acquired = False
        try:
            remaining = deadline - time.monotonic()
            if remaining > 0:
                acquired = self._in_flight.acquire(timeout=remaining)
            try:
                with self._lock:
                    self._check_healthy()
                    self._check_acquire_pool_active(warmpool, acquire_seq)
                if not acquired:
                    raise BatchTimeoutError(f"warm pool '{warmpool}' wasn't checked within {wait:g}s")
                ready = _precheck_pools(self._client.k8s_helper, [warmpool], self.namespace)
                with self._lock:
                    self._warm_credit.record(warmpool, ready[warmpool])
                    self._checked_pools.add(warmpool)
                    del self._prechecks[warmpool]
            finally:
                if acquired:
                    self._in_flight.release()
        except BaseException as e:
            with self._lock:
                self._prechecks.pop(warmpool, None)
                same_release = self._state.pool_release_seq(warmpool) == acquire_seq
            if acquired and isinstance(e, Exception) and same_release:
                check.set_exception(e)
            else:
                # Let a waiting acquire with a later deadline or after release_group() take over.
                check.cancel()
            raise
        check.set_result(None)

    def _refresh_warm_credit(self, warmpool: str, deadline: float, acquire_seq: int) -> None:
        """Re-reads the pool's Ready count once callers spend its credit, at most once every
        ``BATCH_WARM_CREDIT_REFRESH_SECONDS``, while holding one of the ``max_in_flight`` slots, so
        a burst of acquires on a warm pool skips pacing without exceeding the connection budget.
        """
        def remaining_if_due() -> float:
            self._check_healthy()
            self._check_acquire_pool_active(warmpool, acquire_seq)
            remaining = deadline - time.monotonic()
            return remaining if remaining > 0 and self._warm_credit.refresh_due(warmpool) else 0.0

        with self._lock:
            if (remaining := remaining_if_due()) <= 0:
                return
            refresh_lock = self._credit_refresh_locks.setdefault(warmpool, threading.Lock())
        if not refresh_lock.acquire(timeout=remaining):
            # Fall back to normal pacing, which checks whether a paced slot fits the deadline.
            return
        try:
            with self._lock:
                remaining = remaining_if_due()
            if remaining <= 0 or not self._in_flight.acquire(timeout=remaining):
                return
            try:
                # Snapshot the pool sequence before GET so a refresh finishing after exhaust() cannot overwrite 0.
                with self._lock:
                    remaining = remaining_if_due()
                    seq = self._warm_credit.seq(warmpool)
                if remaining <= 0:
                    return
                request_timeout = min(float(batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS), remaining)
                try:
                    obj = self._client.k8s_helper.get_sandbox_warmpool(
                        warmpool, self.namespace, _request_timeout=request_timeout
                    )
                    ready = 0 if obj is None else batch_utils.warmpool_ready_replicas(obj)
                except Exception as e:
                    if time.monotonic() >= deadline:
                        # This acquire's deadline cut the read short, so another acquire with more time
                        # left can still refresh the pool instead of waiting out the refresh interval.
                        return
                    # Fall back to normal pacing so the claim create surfaces any real pool failure.
                    logger.debug(f"Batch '{self.batch_id}' failed to read warm pool '{warmpool}': {e}")
                    ready = 0
                with self._lock:
                    self._warm_credit.record(warmpool, ready, seq=seq)
            finally:
                self._in_flight.release()
        finally:
            refresh_lock.release()

    def _drop_acquired(self, claim_name: str, may_exist: bool, *, retried: bool = False) -> None:
        """Drops a claim ``acquire()`` didn't get Ready and deletes it. A failed delete is logged,
        since the error ``acquire()`` raises matters more, and left for a later release call.
        """
        with self._changed:
            self._acquire_waiters.pop(claim_name, None)
            needs_delete = self._state.drop_acquired(claim_name, may_exist, retried=retried)
            self._changed.notify_all()
        if needs_delete:
            try:
                self._delete_claim(claim_name)
            except BatchError:
                # detach(), release(), or err() stopped this handle from deleting claims.
                pass
            except Exception as e:
                logger.warning(f"Batch '{self.batch_id}' failed to delete acquired claim '{claim_name}': {e}")

    def release_member(self, member: Member) -> None:
        """Deletes one member's claim and drops the member from ``members()``.

        The member is never handed out afterwards and emits no ``MEMBER_LOST``. A member of the
        initial claims counts as unable to become Ready, so releasing one before a quorum consumer
        has decided can make its group or the quorum unreachable. A claim that is already gone
        counts as deleted, and transient errors are retried. If the delete fails, calling
        ``release_member`` again retries it.

        Raises:
            ValueError: If the member isn't in this batch.
            BatchError: If this handle has been detached or released, or once ``err()`` is set,
                since the handle may no longer hold the batch's Lease.
            ApiException: For a non-transient error such as a 403, or the last transient error
                after ``BATCH_DELETE_ATTEMPTS`` attempts (an ``ApiException`` or a transport error).
        """
        with self._changed:
            self._check_healthy()
            needs_delete = self._state.release_member(member.claim_name)
            self._wake_acquire(member.claim_name)
            self._changed.notify_all()
        self._close_cached_connections("release_member", [member.claim_name])
        if needs_delete:
            self._delete_claim(member.claim_name)

    def release_not_ready(self) -> None:
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
            BatchError: If this handle has been detached or released, or once ``err()`` is set,
                since the handle may no longer hold the batch's Lease.
            ApiException: The first error a delete ended with, once every delete has been tried
                (an ``ApiException`` or a transport error).
        """
        with self._changed:
            self._check_healthy()
            self._drop_planned_creates(None)
            names = self._state.release_not_ready()
            self._changed.notify_all()
        self._close_cached_connections("release_not_ready", names)
        if not names:
            return
        with ThreadPoolExecutor(min(len(names), self._max_in_flight)) as pool:
            futures = [pool.submit(self._delete_claim, name) for name in names]
        errors = [e for f in futures if (e := f.exception()) is not None]
        if errors:
            raise errors[0]

    def release_group(self, warmpool: str) -> None:
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
            BatchError: If this handle has been detached or released, once ``err()`` is set,
                since the handle may no longer hold the batch's Lease, or if claims are still not
                being deleted after the last round.
            ApiException: For a non-transient error such as a 403, which is raised at once.
                After the last round, the last transient error is raised (an ``ApiException``
                or a transport error).
        """
        with self._changed:
            self._check_healthy()
            if not self._state.has_pool(warmpool):
                raise ValueError(f"batch '{self.batch_id}' has no group or members for warm pool '{warmpool}'")
            self._drop_planned_creates(warmpool)
            names = self._state.release_group(warmpool)
            self._notify_all()
        self._close_cached_connections("release_group", names)
        _delete_claims_in_rounds(
            self._client.k8s_helper,
            self.namespace,
            batch_utils.group_label_selector(self.batch_id, warmpool),
            f"batch '{self.batch_id}' group '{warmpool}'",
            guard=self._member_delete,
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
                self._state.release_claim(claim_name, group.warmpool)
            else:
                kept.append((claim_name, group))
        self._create_plan = kept
        if not kept:
            # Count from _last_fill_slot rather than _next_create_at so pacing slots that
            # acquire() reserved do not push the fill deadline out.
            last_slot = max(time.monotonic(), self._last_fill_slot)
            self._fill_deadline = last_slot + self._quorum_timeout

    def _delete_claim(self, claim_name: str) -> None:
        """Deletes one released claim, retrying transient errors. A claim already gone counts as deleted."""
        attempt = 1
        while True:
            try:
                # Scope the pool slot and active-delete guard to each HTTP attempt so a sleeping
                # retry neither holds a max_in_flight slot nor blocks detach() or release().
                with self._in_flight, self._member_delete():
                    # Snapshot the delete sequence before sending DELETE so finishing late
                    # cannot clear a newer cleanup delete queued after a create retry.
                    with self._lock:
                        seq = self._state.delete_seq(claim_name)
                    self._client.k8s_helper.delete_sandbox_claim(
                        claim_name,
                        self.namespace,
                        _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                        log_level=logging.DEBUG,
                    )
                break
            except Exception as e:
                if attempt >= batch_utils.BATCH_DELETE_ATTEMPTS or not _is_transient_error(e):
                    raise
                self._check_lease_held()
                time.sleep(batch_utils.retry_delay(attempt, e))
                attempt += 1
        with self._lock:
            self._state.note_deleted(claim_name, seq)

    def _check_releasable_locked(self) -> bool:
        """Returns ``False`` if ``release()`` already finished, and raises ``BatchError`` if
        ``detach()`` finished or another handle holds the Lease. Requires ``_lock``."""
        if self._released:
            return False
        if self._lease_released:
            raise BatchError(f"batch '{self.batch_id}' has been detached or is held by another handle")
        return True

    def release(self) -> None:
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
        with self._lifecycle_lock:
            with self._lock:
                if not self._check_releasable_locked():
                    return
                self._detached = True
                self._stop_creating.set()
                # Clearing _detach_grace marks release as started so detach() rejects new calls and
                # client cleanup retries a failed release as a release rather than a detach.
                self._detach_grace = None
                self._notify_all()

        # Wait outside _lifecycle_lock so a signal handler calling release() or detach() on a thread
        # mid-delete does not block on _lifecycle_lock while this call waits for that delete.
        self._wait_active_deletes()

        with self._lifecycle_lock:
            with self._lock:
                if not self._check_releasable_locked():
                    return

            self._stop_background_threads()
            self._close_cached_connections("release")
            with self._lock:
                lease = self._lease
            try:
                _delete_batch_objects(
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
                with self._lock:
                    self._lease_released = True
                    self._changed.notify_all()
                self._client._unregister_batch(self)
                raise

            with self._lock:
                self._lease_released = True
                self._released = True
            self._client._unregister_batch(self)

    def _stop_background_threads(self) -> None:
        """Stops claim creators and stops the watcher and lease renewal loop."""
        self._stop_creating.set()
        # Pause to ensure each in-progress create request has been responded to
        deadline = time.monotonic() + batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS + batch_utils.BATCH_CREATOR_STOP_GRACE_SECONDS
        for creator in self._creators:
            creator.join(max(0.0, deadline - time.monotonic()))
        self._watch_stop.set()
        self._renew_stop.set()
        if self._renew_thread is not None:
            self._renew_thread.join()

    def _close_cached_connections(self, action: str, claim_names: Iterable[str] | None = None) -> None:
        """Closes the cached connections to the given claims' sandboxes, or to every sandbox along
        with the stale connections."""
        with self._lock:
            names = list(self._connected) if claim_names is None else claim_names
        for claim_name in names:
            with self._lock:
                entry = self._connected.pop(claim_name, None)
            if entry is None:
                continue
            self._close_one(entry[0], action, partial(self._connected.setdefault, claim_name, entry))
        if claim_names is None:
            with self._lock:
                stale = list(self._stale_connections)
            self._close_connections(stale, action)

    def _pop_stale_connections(self, claim_names: list[str]) -> list["Sandbox"]:
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

    def _close_connections(self, connected: list["Sandbox"], action: str) -> None:
        """Closes the given stale connections that no other caller has taken from ``_stale_connections``."""
        for sandbox in connected:
            with self._lock:
                if sandbox not in self._stale_connections:
                    continue
                self._stale_connections.remove(sandbox)
            self._close_one(sandbox, action, partial(self._stale_connections.append, sandbox))

    def _close_one(self, sandbox: "Sandbox", action: str, keep: Callable[[], object]) -> None:
        """Closes one cached connection, or calls ``keep`` to put it back if the close fails."""
        closed = False
        try:
            sandbox.close_connection()
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
                with self._lock:
                    keep()
                self._client._track_closing(self)

    def _detach_unfinished(self) -> bool:
        """Returns whether detach() started on this handle but hasn't released the Lease."""
        return self._detach_grace is not None and not self._lease_released

    def _release_started(self) -> bool:
        """Returns whether release() started on this handle."""
        return self._detached and self._detach_grace is None

    def _exit_detaches(self) -> bool:
        """Returns whether client exit cleanup detaches this handle instead of releasing its batch."""
        # Client cleanup retries a release() that already started, even on an attached handle.
        return self._attached and not self._release_started()

    def _detach_done(self) -> bool:
        """Returns whether detach() already released the Lease, and raises ``BatchError`` once a
        release has started. Requires ``_lock``."""
        if self._release_started():
            raise BatchError(f"batch '{self.batch_id}' has been released or is being released")
        return self._lease_released

    def detach(self, grace: int | None = None) -> None:
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
            BatchError: If a release has started on this handle, including one that failed.
            ApiException: If the Lease release fails (an ``ApiException`` or a transport error).
        """
        if grace is not None:
            batch_utils.validate_lease_duration_value(grace)

        # _detached prevents the caller from connecting to Sandboxes, while _lease_released signals that
        # detach fully completed (i.e. the previous steps and the Lease release write succeeded), so callers
        # can retry detach() if the release fails.
        with self._lock:
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

        self._wait_active_deletes(for_detach=True)
        with self._lock:
            if self._detach_done():
                return
        self._stop_background_threads()
        self._close_cached_connections("detach")

        with self._lifecycle_lock:
            with self._lock:
                if self._detach_done():
                    return
            try:
                lease = self._client.k8s_helper.read_batch_lease(
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
                            spec.holder_identity = None
                            spec.renew_time = batch_utils.lease_now()
                            meta.annotations = {
                                **(meta.annotations or {}),
                                BATCH_NEXT_ORDINAL_ANNOTATION: str(next_ordinal),
                            }
                            # Refresh the Lease so it doesn't expire immediately after we release it,
                            # giving other clients a chance to acquire it.
                            spec.lease_duration_seconds = grace
                            # A signal handler on this thread can re-enter _lifecycle_lock and run
                            # release() or detach() during any request above or below.
                            with self._lock:
                                if self._detach_done():
                                    return
                            try:
                                self._client.k8s_helper.replace_batch_lease(
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
                                # that timed out but landed, or a detach() that re-entered on this thread, leaves it
                                # released, and a takeover leaves another holder, so all of these count as done. It can
                                # still be held here only if one of this handle's own renewals landed late, so the
                                # release is written once more.
                                lease = self._client.k8s_helper.read_batch_lease(
                                    self._lease_name,
                                    self.namespace,
                                    _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                                )
                                if lease is None:
                                    break
                                meta, spec = batch_utils.lease_parts(lease)
                                if spec.holder_identity != self._holder_identity:
                                    break
            except Exception:
                with self._lock:
                    if self._detach_done():
                        return
                raise

            with self._lock:
                if self._detach_done():
                    return
                self._lease_released = True
                self._changed.notify_all()
        self._client._unregister_batch(self)

    def _watch_loop(self) -> None:
        """Background watch loop which keeps ``self._state`` in sync with the batch's claims.

        The kubernetes client has no informer or reflector for custom resources, so
        this manually implements one where the batch state is seeded from a list,
        then kept up to date with watch events using the list's resourceVersion so no events are missed.
        """
        rv = self._list_resource_version
        label_selector = f"{BATCH_ID_LABEL}={self.batch_id}"
        relist = False
        # Consecutive failed attempts, which set the retry backoff.
        failures = 0
        while not self._watch_stop.is_set():
            try:
                if relist:
                    items, rv = self._client.k8s_helper.list_sandbox_claim_objects(
                        self.namespace, label_selector, _request_timeout=batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
                    )
                    if self._watch_stop.is_set():
                        return
                    with self._lock:
                        self._state.resync_from_list(items)
                        self._exhaust_warm_credit()
                        self._notify_all()
                        stale = self._pop_stale_connections(list(self._connected))
                    self._close_connections(stale, "a re-list")
                    relist = False
                watch_started = time.monotonic()
                got_event = False
                for event in self._client.k8s_helper.watch_sandbox_claims(
                    self.namespace, label_selector, rv, _WATCH_TIMEOUT_SECONDS, _request_timeout=_WATCH_REQUEST_TIMEOUT
                ):
                    if self._watch_stop.is_set():
                        break
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
                    with self._lock:
                        if event_type == "DELETED":
                            if self._state.mark_lost(claim_name) is not None:
                                self._wake_acquire(claim_name)
                        elif event_type in ("ADDED", "MODIFIED"):
                            member = self._state.upsert_claim(obj)
                            self._exhaust_warm_credit()
                            if member is not None and member.state is not MemberState.PENDING:
                                self._wake_acquire(claim_name)
                        self._changed.notify_all()
                        stale = self._pop_stale_connections([claim_name])
                    self._close_connections(stale, "a watch event")
                if (
                    not got_event
                    and time.monotonic() - watch_started < _SHORT_WATCH_SECONDS
                    and not self._watch_stop.is_set()
                ):
                    failures += 1
                    self._watch_stop.wait(batch_utils.retry_delay(failures, None))
                    continue
                # The watch ended at its timeout, which is not a failure.
                failures = 0
            except client.ApiException as e:
                if e.status == 410:
                    # Indicates etcd RV compaction aged our resourceVersion out of the apiserver's watch cache,
                    # so the watch can't continue. Re-list, reconcile the cache (catches events that occurred while there
                    # was no watch), and continue watching from the new list's resourceVersion.
                    relist = True
                    continue
                if batch_utils.is_retryable_status(e.status):
                    # Likely transient: retry the re-list, or the watch from the last-seen resourceVersion.
                    failures += 1
                    self._watch_stop.wait(batch_utils.retry_delay(failures, e))
                    continue
                # Response codes outside of this (e.g. 403, 404) are not recoverable
                # by retrying, so we error then exit
                if self._watch_stop.is_set():
                    return
                with self._lock:
                    self._state.note_error(e)
                    self._notify_all()
                return
            except Exception as e:
                if _is_transport_error(e):
                    # Likely transient network-level failure so retry
                    failures += 1
                    self._watch_stop.wait(batch_utils.retry_delay(failures, e))
                    continue
                # Anything else (e.g. a certificate error) is not known to be transient, so surface it
                # via err() instead of leaving the thread dead with err() still None.
                if self._watch_stop.is_set():
                    return
                with self._lock:
                    self._state.note_error(e)
                    self._notify_all()
                return

    def _renew_loop(self) -> None:
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
            if self._renew_stop.wait(max(0.0, min(wait, deadline - time.monotonic()))):
                return
            if time.monotonic() >= deadline:
                self._stop_renewal(
                    BatchLeaseExpiredError(
                        f"batch '{self.batch_id}' lease has not renewed successfully "
                        f"for {self._lease_duration}s"
                    )
                )
                return
            if not self._renew_once():
                return
            with self._lock:
                failures = failures + 1 if self._renewal_degraded else 0

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

    def _renew_once(self) -> bool:
        """Does a single attempt to renew a Lease, and sets ``_renewal_degraded`` if it fails.

        Replaces the Lease this handle last wrote without reading it first. Only a conflict means
        the Lease changed since, so only then is it read to find out whether this handle still
        holds it.

        Returns ``False`` if another holder has taken the Lease or the Lease no longer exists,
        meaning this handle must stop renewing rather than overwrite it.
        """
        try:
            try:
                sent_at, written = self._write_renewal(self._lease)
            except ApiException as e:
                if e.status == 404:
                    return self._stop_renewal(self._lease_gone_error())
                if e.status != 409:
                    raise
                # A conflict means another holder took the Lease, or an earlier renewal whose
                # response was lost did land, so read it to tell the two apart.
                lease = self._client.k8s_helper.read_batch_lease(
                    self._lease_name, self.namespace, _request_timeout=self._renew_request_timeout()
                )
                if lease is None:
                    return self._stop_renewal(self._lease_gone_error())
                spec = batch_utils.lease_parts(lease)[1]
                if spec.holder_identity != self._holder_identity:
                    return self._stop_renewal(
                        BatchInUseError(
                            f"batch '{self.batch_id}' Lease is no longer held by this handle "
                            f"(current holder: {spec.holder_identity!r})"
                        )
                    )
                sent_at, written = self._write_renewal(lease)
        except Exception as e:
            with self._lock:
                self._mark_renewal_degraded(e)
            return True
        with self._lock:
            self._lease = written
            self._lease_expires_at = sent_at + self._lease_duration
            self._renewal_degraded = False
        return True

    def _stop_renewal(self, error: BatchError) -> bool:
        """Records why this handle stops renewing the Lease. Returns ``False`` for ``_renew_once``."""
        with self._lock:
            self._mark_renewal_degraded(error)
            self._state.note_error(error)
            self._notify_all()
        return False

    def _write_renewal(self, lease: V1Lease) -> tuple[float, V1Lease]:
        """Stamps ``lease`` as renewed now and replaces it, returning the send time and the written Lease."""
        # Neither Kubernetes client has a default request timeout, so a hung apiserver would
        # otherwise stall renewal past the Lease's expiry.
        timeout = self._renew_request_timeout()
        sent_at = time.monotonic()
        spec = batch_utils.lease_parts(lease)[1]
        spec.holder_identity = self._holder_identity
        spec.renew_time = batch_utils.lease_now()
        spec.lease_duration_seconds = self._lease_duration
        written = self._client.k8s_helper.replace_batch_lease(
            self._lease_name, self.namespace, lease, _request_timeout=timeout
        )
        return sent_at, written

    def _lease_gone_error(self) -> BatchLeaseExpiredError:
        return BatchLeaseExpiredError(
            f"batch '{self.batch_id}' Lease '{self._lease_name}' no longer exists"
        )

    def _create_worker(self) -> None:
        """Creates planned claims one at a time. ``max_in_flight`` workers share one pacing slot,
        so together they start at most ``create_rps`` creates per second, except for the creates
        the warm pool's credit covers, which start at once.
        """
        while True:
            with self._lock:
                if (
                    not self._create_plan
                    or self._stop_creating.is_set()
                    or self._state.error() is not None
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
            if self._stop_creating.wait(max(0.0, slot - time.monotonic())):
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
                self._create_one(
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
                with self._lock:
                    self._state.record_create_failure(
                        claim_name, group.warmpool, _create_failure_message(e)
                    )
                    self._changed.notify_all()
            if may_exist:
                # release_member() deletes attempt 1 itself, whereas a retry (sent > 1) in
                # flight during that delete can land afterwards and recreate the claim.
                self._delete_released_create(claim_name, group.warmpool, retried=sent > 1)

    def _can_still_send(self, can_send: Callable[[], bool] | None) -> bool:
        """Whether a create or its retry may still send a request. The caller holds ``self._lock``."""
        # Once err() is set the handle can no longer watch or hold the batch, so further
        # claims would be ones nobody can observe or keep alive.
        return (
            not self._stop_creating.is_set()
            and self._state.error() is None
            and (can_send is None or can_send())
        )

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

    def _delete_released_create(self, claim_name: str, warmpool: str, *, retried: bool = False) -> None:
        """Deletes a fill claim whose create may have landed after the caller released it, its
        group, or ended the fill. A later release call retries a failed delete.
        """
        with self._lock:
            if (
                self._stop_creating.is_set()
                or self._state.error() is not None
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
            self._delete_claim(claim_name)
        except BatchError:
            # detach(), release(), or err() stopped this handle from deleting claims.
            pass
        except Exception as e:
            logger.warning(f"Batch '{self.batch_id}' failed to delete released claim '{claim_name}': {e}")

    def _send_create(
        self,
        claim_name: str,
        warmpool: str,
        annotations: dict[str, str],
        labels: dict[str, str],
        lifecycle: dict[str, str],
        deadline: float | None,
        can_send: Callable[[], bool] | None = None,
        on_send: Callable[[], None] | None = None,
    ) -> None:
        """Sends one claim create while holding one of the ``max_in_flight`` slots, releasing the
        slot before any retry backoff sleep, unless creation stopped or ``can_send`` skipped the
        request while waiting.

        Raises:
            BatchTimeoutError: If no slot frees up before ``deadline``.
        """
        with self._lock:
            if not self._can_still_send(can_send):
                return
        wait = None if deadline is None else deadline - time.monotonic()
        if (wait is not None and wait <= 0) or not self._in_flight.acquire(timeout=wait):
            with self._lock:
                # Surface detach, release, or an already-observed claim instead of a slot timeout.
                if not self._can_still_send(can_send):
                    return
            raise BatchTimeoutError(f"no create in flight slot for claim '{claim_name}' before the deadline")
        try:
            with self._lock:
                if not self._can_still_send(can_send):
                    return
                # Waiting on _lock after _in_flight.acquire can cross deadline, so check again
                # before recording or sending the create.
                if deadline is not None and time.monotonic() >= deadline:
                    raise BatchTimeoutError(f"no create in flight slot for claim '{claim_name}' before the deadline")
                if on_send is not None:
                    on_send()
            self._client.k8s_helper.create_sandbox_claim(
                claim_name,
                warmpool,
                self.namespace,
                annotations=annotations,
                labels=labels,
                lifecycle=lifecycle,
                owner_references=self._owner_references,
                log_level=logging.DEBUG,
                _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
            )
        finally:
            self._in_flight.release()

    def _create_one(
        self,
        claim_name: str,
        warmpool: str,
        annotations: dict[str, str],
        labels: dict[str, str],
        lifecycle: dict[str, str],
        deadline: float | None = None,
        can_send: Callable[[], bool] | None = None,
        on_send: Callable[[], None] | None = None,
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
                self._send_create(
                    claim_name, warmpool, annotations, labels, lifecycle, deadline, can_send, on_send
                )
                return
            except Exception as e:
                if isinstance(e, ApiException):
                    outcome = batch_utils.create_error_outcome(e.status, attempt)
                elif _is_transport_error(e):
                    outcome = batch_utils.create_error_outcome(None, attempt)
                else:
                    outcome = batch_utils.CreateOutcome.FAIL
                retry_after = batch_utils.retry_after_seconds(e)
                if isinstance(e, ApiException) and e.status == 429:
                    with self._lock:
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
                        with self._lock:
                            if not self._can_still_send(can_send):
                                return
                            self._check_retry_deadline(claim_name, deadline, time.monotonic() + backoff, e)
                        if self._stop_creating.wait(backoff):
                            return
                    with self._lock:
                        if not self._can_still_send(can_send):
                            return
                        earliest = time.monotonic() + (retry_after or 0.0)
                        # Other creates keep taking slots during a backoff, so the create checks the deadline again.
                        self._check_retry_deadline(claim_name, deadline, earliest, e)
                        slot = self._reserve_create_slot(earliest)
                    if self._stop_creating.wait(max(0.0, slot - time.monotonic())):
                        return
                    attempt += 1
                    continue
                logger.debug(f"Batch '{self.batch_id}' failed to create claim '{claim_name}': {e}")
                raise


def _precheck_pools(helper: "K8sHelper", warmpools: Iterable[str], namespace: str) -> dict[str, int]:
    """Checks that each warm pool and the SandboxTemplate it references exist, and returns each
    pool's Ready replica count.

    Raises:
        SandboxWarmPoolNotFoundError: If a warm pool doesn't exist.
        SandboxTemplateNotFoundError: If a warm pool's template doesn't exist, or it names none.
    """
    timeout = batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
    checked_templates: set[str] = set()
    ready: dict[str, int] = {}
    for name in warmpools:
        warmpool = helper.get_sandbox_warmpool(name, namespace, _request_timeout=timeout)
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
        if helper.get_sandbox_template(template, namespace, _request_timeout=timeout) is None:
            raise SandboxTemplateNotFoundError(
                f"SandboxTemplate '{template}' not found in namespace '{namespace}'"
            )
        checked_templates.add(template)
    return ready


def _take_batch_lease(
    helper: "K8sHelper",
    namespace: str,
    batch_id: str,
    lease_name: str,
    holder_identity: str,
    lease_duration: int,
    last_lease: "V1Lease | AsyncV1Lease",
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
        lease = helper.read_batch_lease(lease_name, namespace, _request_timeout=timeout)
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
            written = helper.replace_batch_lease(lease_name, namespace, lease, _request_timeout=timeout)
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
        return str(written_meta.resource_version)


def _delete_batch_objects(
    helper: "K8sHelper",
    namespace: str,
    batch_id: str,
    lease_name: str,
    holder_identity: str,
    lease_duration: int,
    last_lease: "V1Lease | AsyncV1Lease",
) -> None:
    """Deletes a batch's claims in rounds, then its Lease once no claim is left that isn't
    already being deleted.

    Each round first takes the Lease for ``holder_identity``, so every delete is sent while that
    handle holds it or no Lease exists, and the Lease is deleted only at the resourceVersion that
    handle wrote. ``last_lease`` is the Lease from that handle's last successful write.
    Also used by ``AsyncSandboxClient``'s atexit hook, which can't use the event loop.

    Raises:
        BatchInUseError: If another handle holds the Lease or writes it first, or the Lease
            changed hands since ``last_lease`` was written.
        BatchError: If claims are still not being deleted after the last round.
        ApiException: For a non-transient error at once, or the last transient error after
            the last round.
    """
    resource_version: str | None = None

    def take() -> None:
        nonlocal resource_version
        resource_version = _take_batch_lease(
            helper, namespace, batch_id, lease_name, holder_identity, lease_duration, last_lease
        )

    def finish() -> None:
        # This round found no Lease, so there is none to delete.
        if resource_version is not None:
            helper.delete_batch_lease(
                lease_name,
                namespace,
                _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                resource_version=resource_version,
            )

    _delete_claims_in_rounds(
        helper,
        namespace,
        f"{BATCH_ID_LABEL}={batch_id}",
        f"batch '{batch_id}'",
        hold=take,
        finish=finish,
    )


def _delete_claims_in_rounds(
    helper: "K8sHelper",
    namespace: str,
    label_selector: str,
    description: str,
    hold: Callable[[], None] | None = None,
    finish: Callable[[], None] | None = None,
    guard: Callable[[], contextlib.AbstractContextManager[None]] = contextlib.nullcontext,
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
    while True:
        last_error: Exception | None = None
        held = False
        try:
            if hold is not None:
                hold()
            held = True
            # deletecollection deletes one claim at a time on the apiserver, so for a large batch
            # it can time out while deletion continues. For this case, we track it with the list below.
            with guard():
                helper.delete_sandbox_claims_by_label(
                    namespace, label_selector, _request_timeout=collection_timeout
                )
        except Exception as e:
            if not _is_transient_error(e):
                raise
            last_error = e

        live: int | None = None
        try:
            # A consistent read (no resourceVersion), so a lagging watch cache can't report
            # that no claims are left too early.
            with guard():
                live = helper.count_live_sandbox_claims(
                    namespace, label_selector, _request_timeout=collection_timeout
                )
        except Exception as e:
            if not _is_transient_error(e):
                raise
            last_error = e

        # Only a round whose hold succeeded can finish. After a failed hold the Lease may still exist,
        # and this round has no resourceVersion to delete it at.
        if live == 0 and held:
            try:
                if finish is not None:
                    finish()
                return
            except Exception as e:
                if not _is_transient_error(e):
                    raise
                last_error = e

        # A round that shrinks the number of live claims doesn't count toward giving up, so a large
        # batch that takes many rounds to delete isn't abandoned while it is still making progress.
        progressed = live is not None and (remaining_before is None or live < remaining_before)
        idle_rounds = 0 if progressed else idle_rounds + 1
        if idle_rounds >= batch_utils.BATCH_RELEASE_MAX_IDLE_ROUNDS:
            if last_error is not None:
                raise last_error
            raise BatchError(
                f"{description}: {live or 0} claims are still not being deleted"
            )
        if live is not None:
            remaining_before = live
        # Fail fast if the handle detached or stopped instead of sleeping through retry backoff,
        # without counting the sleep itself as an active delete.
        with guard():
            pass
        time.sleep(batch_utils.retry_delay(max(idle_rounds, 1), last_error))
