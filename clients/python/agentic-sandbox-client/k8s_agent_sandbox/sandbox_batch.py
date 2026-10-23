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

import logging
import math
import ssl
import threading
import time
from collections import deque
from collections.abc import Callable, Iterable, Iterator
from concurrent.futures import CancelledError, Future, ThreadPoolExecutor
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
from .models import BatchEvent, BatchGroup, GroupReady, Member, MemberState
from .utils import construct_sandbox_claim_lifecycle_spec

if TYPE_CHECKING:
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
        self._connected: dict[str, "Sandbox"] = {}
        self._detached = False
        self._lease_released = False
        self._watch_stop = threading.Event()
        self._renew_stop = threading.Event()
        self._renewal_degraded = False
        # The Lease from this handle's last successful write. Renewal sends it back without reading
        # the Lease first, and only reads it if the write conflicts.
        self._lease = lease
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
        self._released = False
        self._creators: list[threading.Thread] = []
        self._stop_creating = threading.Event()
        self._create_plan: deque[tuple[str, BatchGroup]] = deque()
        self._next_create_at = 0.0
        self._create_interval = 0.0
        # The caller's labels and the batch id label, which every claim carries.
        self._labels: dict[str, str] = {BATCH_ID_LABEL: batch_id}
        # Pools acquire() doesn't check again, since claim_batch checked them or they already have claims.
        self._checked_pools: set[str] = {g.warmpool for g in state.groups}
        # The check of each pool that is running, so concurrent first acquires of a pool share it.
        self._prechecks: dict[str, Future[None]] = {}
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
        _precheck_pools(helper, [g.warmpool for g in args.groups], namespace)

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
        )
        handle._create_plan = deque(batch_utils.fill_create_plan(args.batch_id, args.groups))
        handle._create_interval = 1 / args.create_rps
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

        if lease is None and not claim_items:
            raise BatchNotFoundError(f"batch '{batch_id}' not found in namespace '{namespace}'")

        settings = batch_utils.parse_lease_metadata(
            lease.metadata.labels if lease is not None else None,
            lease.metadata.annotations if lease is not None else None,
        )
        duration = settings.lease_duration

        # Read before renewTime is stamped, so this handle's expiry is never later than the one
        # other clients see.
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

        if lease.spec.holder_identity:
            raise BatchInUseError(
                f"batch '{batch_id}' is held by '{lease.spec.holder_identity}'"
            )

        groups = batch_state.reconstruct_groups(batch_id, claim_items)
        state = batch_state.BatchState(batch_id, groups, settings.next_ordinal)
        state.seed_from_claims(claim_items)
        state.count_missing_fill_as_unable()

        holder_identity = batch_utils.generate_holder_identity()
        lease.spec.holder_identity = holder_identity
        lease.spec.renew_time = now
        lease.spec.lease_duration_seconds = duration
        lease.spec.acquire_time = now
        lease.spec.lease_transitions = (lease.spec.lease_transitions or 0) + 1
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

    def members(self, warmpool: str | None = None) -> list[Member]:
        """Returns a snapshot of the batch's members, optionally filtered to one warm pool."""
        with self._lock:
            return self._state.members(warmpool)

    def connect(self, member: Member) -> "Sandbox":
        """Returns a connected ``Sandbox`` for a ready member. If this handle is already connected
        to the Sandbox, it reuses the existing connection, otherwise it creates a new one and caches it for future calls.

        Raises ``SandboxNotReadyError`` if the member isn't ready, even while its connection is
        cached, and ``BatchError`` once ``err()`` is set, since the Lease may be lost and the cache
        can no longer vouch for the member.
        """
        with self._lock:
            self._check_active()
            if (error := self._state.error()) is not None:
                raise BatchError(f"batch '{self.batch_id}' has stopped") from error
            # No name resolution or existence check as the batch's watch cache already knows the member's sandbox_name
            current = self._state.get_member(member.claim_name)
            if current is None or current.state is not MemberState.READY:
                raise SandboxNotReadyError(
                    f"batch member '{member.claim_name}' is not ready"
                )
            cached = self._connected.get(member.claim_name)
            if cached is not None:
                return cached
            sandbox: Sandbox = self._client.sandbox_class(
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
        """Returns an iterator over the batch's events.

        ``MEMBER_READY`` is yielded once per initial claim that becomes Ready, and ``MEMBER_FAILED``
        and ``MEMBER_LOST`` once per initial claim that fails or is deleted. Called on its own, it
        streams every Ready member. Called after ``iter_ready_groups()``, it yields a group's members
        only after that group has yielded successfully there, so a group that fails yields none.
        Called after ``wait_for_quorum()``, it yields the members the quorum doesn't return once the
        quorum is reached, and none if the quorum fails. ``iter_ready_groups()`` and
        ``wait_for_quorum()`` can't be called after ``events()``, since the first consumer to be
        called fixes how members are handed out.

        The iterator ends once every initial claim is Ready or can't become Ready, once the fill
        deadline (``quorum_timeout`` after the last claim create) has passed, or once ``err()`` is
        set. A change after that, such as a handed-out member that fails, is yielded only by a later
        call of ``events()``, which continues where the previous iterator stopped. ``members()``
        always shows each claim's current state.

        Raises:
            BatchError: If this handle has been detached or released, when called or while waiting.
        """
        with self._lock:
            self._check_active()
            self._state.set_mode(batch_state.ConsumerMode.STREAM)
        return self._iterate(self._state.pop_event, self._state.events_done)

    def iter_ready_groups(self) -> Iterator[GroupReady]:
        """Returns an iterator that yields one ``GroupReady`` per group, as soon as its outcome is known.

        A group yields its first ``min_ready`` Ready members, in the order they became Ready. It
        fails with ``QuorumUnreachableError`` once too few of its members can still become Ready,
        or with ``BatchTimeoutError`` if it hasn't reached ``min_ready`` within the quorum timeout.
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

        ``QuorumUnreachableError`` and ``BatchTimeoutError`` are both ``BatchError``, so
        ``except BatchError`` handles every failure.

        Raises:
            QuorumUnreachableError: If too few members of a group can still become Ready.
            BatchTimeoutError: If the quorum isn't reached within the quorum timeout.
            BatchError: If ``events()`` or ``iter_ready_groups()`` was called first, this method
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

    def _next(
        self, pop: Callable[[], _T | None], done: Callable[[], bool], deadline: float | None = None
    ) -> _T | None:
        with self._changed:
            while True:
                self._check_active()
                # Taken before the expiry check, so a fill deadline at or before it has expired the fill.
                now = time.monotonic()
                self._expire_fill_if_due()
                item = pop()
                if item is not None:
                    return item
                if done() or self._state.error() is not None:
                    return None
                # Nothing to hand out yet, but returning None would end the iterator, so wait for
                # a change. The wait also wakes at the fill deadline so pending members can expire,
                # and at the caller's own deadline. A deadline at or before ``now`` already expired
                # the fill or ended the caller's wait, so it is skipped rather than spinning on a zero wait.
                wake_at = min((d for d in (self._fill_deadline, deadline) if d is not None and d > now), default=None)
                timeout = None if wake_at is None else max(0.0, wake_at - time.monotonic())
                self._changed.wait(timeout)

    def _expire_fill_if_due(self) -> None:
        # Called with the lock held.
        if self._fill_deadline is not None and time.monotonic() >= self._fill_deadline:
            if self._state.expire_fill():
                self._changed.notify_all()

    def acquire(self, warmpool: str, timeout: float | None = None) -> Member:
        """Adds a member from ``warmpool`` to the batch, waits until it is Ready, and returns it.

        The pool can be any warm pool in the batch's namespace. A pool that isn't one of the batch's
        groups is checked once per handle, like ``claim_batch`` checks its pools. The claim takes the
        next unused ordinal and carries its pool's group label but no group annotations, so it is
        never one of the initial claims. ``events()``, ``iter_ready_groups()``, ``wait_for_quorum()``
        and ``release_not_ready()`` leave it alone, and ``release_group()`` on its pool releases it
        with the pool's other members. Its ``shutdownTime`` is the
        time of its create plus ``quorum_timeout`` plus ``work_budget`` plus 600 seconds, and its
        create is retried and paced like the initial claims'.

        If it raises, including on a cancellation, the member is dropped from ``members()`` and its
        claim is deleted, unless the claim is already gone or its create was rejected. A failed delete is
        retried by ``release_not_ready()`` and ``release()``. It never creates a second claim, so
        call it again to try again. A member it returned that fails later shows in ``members()``
        like any other, and ``release_member`` frees it.

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
            BatchTimeoutError: If the member isn't Ready within ``timeout``. A create or create retry
                that ``create_rps`` pacing can't start within it isn't sent.
            BatchError: If this handle has been detached or released, or ``err()`` is set, when
                called or while waiting, or if ``release_member`` or ``release_group`` releases the
                member before it is Ready.
        """
        wait = batch_utils.positive_number("timeout", timeout, self._quorum_timeout)
        deadline = time.monotonic() + wait
        self._check_pool(warmpool, deadline, wait)
        with self._lock:
            self._check_active()
            if (error := self._state.error()) is not None:
                raise BatchError(f"batch '{self.batch_id}' has stopped") from error
            # A create paced past the deadline would only make a claim to delete on timeout.
            if self._next_create_at >= deadline:
                raise BatchTimeoutError(f"no create slot for warm pool '{warmpool}' within {wait:g}s")
            claim_name = self._state.start_acquire(warmpool)
            slot = self._reserve_create_slot(time.monotonic())
        # Whether a claim of the batch may exist under the name, so a failure deletes it.
        may_exist = True
        try:
            # Once creating stops, the handle is detached or released, so _next raises.
            if not self._stop_creating.wait(max(0.0, slot - time.monotonic())):
                may_exist = False
                with self._lock:
                    # Like the fill, no create starts once err() is set, since nobody could keep the
                    # claim alive.
                    if (error := self._state.error()) is not None:
                        raise BatchError(f"batch '{self.batch_id}' has stopped") from error
                    # Raises if release_group() released the claim while it waited for its slot.
                    self._state.acquired_member(claim_name)
                may_exist = True
                lifecycle = construct_sandbox_claim_lifecycle_spec(
                    self._quorum_timeout + self._work_budget + batch_utils.BATCH_SHUTDOWN_MARGIN_SECONDS
                )
                try:
                    self._create_one(
                        claim_name,
                        warmpool,
                        self._client._trace_context_annotations(),
                        # The group label lets release_group() free it with the pool's other members.
                        {**self._labels, BATCH_GROUP_LABEL: batch_utils.group_label_value(warmpool)},
                        lifecycle,
                        deadline,
                    )
                except BatchTimeoutError:
                    raise
                except Exception as e:
                    # A create that ended in a transient error, such as a timeout or a 5xx, may have
                    # been stored anyway, so its claim is still deleted.
                    may_exist = _is_transient_error(e)
                    raise TerminalMemberError(Member(
                        claim_name=claim_name,
                        warmpool=warmpool,
                        state=MemberState.FAILED,
                        reason=batch_state.CREATE_FAILED_REASON,
                        message=_create_failure_message(e),
                    )) from e
            member = self._next(
                lambda: self._state.acquired_member(claim_name),
                lambda: time.monotonic() >= deadline,
                deadline,
            )
            if member is None:
                if (error := self._state.error()) is not None:
                    raise BatchError(f"batch '{self.batch_id}' stopped while acquiring '{claim_name}'") from error
                raise BatchTimeoutError(f"acquired member '{claim_name}' wasn't Ready within {wait:g}s")
            return member
        except BaseException:
            self._drop_acquired(claim_name, may_exist)
            raise

    def _check_pool(self, warmpool: str, deadline: float, wait: float) -> None:
        """Checks a pool that isn't one of the batch's groups the first time ``acquire()`` uses it.
        Concurrent first acquires of a pool share one check and its outcome. A check that fails
        isn't remembered, so a later acquire checks the pool again.

        Raises:
            BatchTimeoutError: If another acquire's check of the pool doesn't finish by ``deadline``.
        """
        while True:
            with self._lock:
                if warmpool in self._checked_pools:
                    return
                check = self._prechecks.get(warmpool)
                if check is None:
                    check = self._prechecks[warmpool] = Future()
                    break
            try:
                check.result(timeout=max(0.0, deadline - time.monotonic()))
            except TimeoutError:
                if not check.done():
                    raise BatchTimeoutError(f"warm pool '{warmpool}' wasn't checked within {wait:g}s") from None
                # The check itself timed out, so this acquire raises the check's error.
                check.result()
            except CancelledError:
                # The acquire that ran the check was interrupted, so this one checks the pool itself.
                continue
            return
        try:
            _precheck_pools(self._client.k8s_helper, [warmpool], self.namespace)
        except BaseException as e:
            with self._lock:
                del self._prechecks[warmpool]
            if isinstance(e, Exception):
                check.set_exception(e)
            else:
                check.cancel()
            raise
        with self._lock:
            self._checked_pools.add(warmpool)
            del self._prechecks[warmpool]
        check.set_result(None)

    def _drop_acquired(self, claim_name: str, may_exist: bool) -> None:
        """Drops a claim ``acquire()`` didn't get Ready and deletes it. A failed delete is logged,
        since the error ``acquire()`` raises matters more, and left for a later release call.
        """
        with self._changed:
            needs_delete = self._state.drop_acquired(claim_name, may_exist)
            self._changed.notify_all()
        if needs_delete:
            try:
                self._delete_claim(claim_name)
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
            BatchError: If this handle has been detached or released.
            ApiException: For a non-transient error such as a 403, or the last transient error
                after ``BATCH_DELETE_ATTEMPTS`` attempts (an ``ApiException`` or a transport error).
        """
        with self._changed:
            self._check_active()
            needs_delete = self._state.release_member(member.claim_name)
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

        Each claim is deleted like ``release_member``, up to ``BATCH_DEFAULT_MAX_IN_FLIGHT`` at
        once. If a delete fails, calling ``release_not_ready`` again retries it.

        Raises:
            BatchError: If this handle has been detached or released.
            ApiException: The first error a delete ended with, once every delete has been tried
                (an ``ApiException`` or a transport error).
        """
        with self._changed:
            self._check_active()
            self._drop_planned_creates(None)
            names = self._state.release_not_ready()
            self._changed.notify_all()
        self._close_cached_connections("release_not_ready", names)
        if not names:
            return
        with ThreadPoolExecutor(min(len(names), batch_utils.BATCH_DEFAULT_MAX_IN_FLIGHT)) as pool:
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
        ``iter_ready_groups()`` yields with an error.

        Claims are deleted like ``release()`` deletes the batch's, with deletecollection rounds
        scoped to the group's label. If they fail, calling ``release_group`` again retries.

        Raises:
            ValueError: If the batch has no group, member or ``acquire()`` in flight for ``warmpool``.
            BatchError: If this handle has been detached or released, or claims are still not
                being deleted after the last round.
            ApiException: For a non-transient error such as a 403, which is raised at once.
                After the last round, the last transient error is raised (an ``ApiException``
                or a transport error).
        """
        with self._changed:
            self._check_active()
            if not self._state.has_pool(warmpool):
                raise ValueError(f"batch '{self.batch_id}' has no group or members for warm pool '{warmpool}'")
            self._drop_planned_creates(warmpool)
            names = self._state.release_group(warmpool)
            self._changed.notify_all()
        self._close_cached_connections("release_group", names)
        _delete_claims_in_rounds(
            self._client.k8s_helper,
            self.namespace,
            batch_utils.group_label_selector(self.batch_id, warmpool),
            f"batch '{self.batch_id}' group '{warmpool}'",
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
            # No worker takes the last planned create now, so the fill deadline counts from the
            # last pacing slot already taken.
            last_slot = max(time.monotonic(), self._next_create_at - self._create_interval)
            self._fill_deadline = last_slot + self._quorum_timeout

    def _delete_claim(self, claim_name: str) -> None:
        """Deletes one released claim, retrying transient errors. A claim already gone counts as deleted."""
        attempt = 1
        while True:
            try:
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
                time.sleep(batch_utils.retry_delay(attempt, e))
                attempt += 1
        with self._lock:
            self._state.note_deleted(claim_name)

    def release(self) -> None:
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
        with self._lock:
            if self._released:
                return
            if self._lease_released:
                raise BatchError(f"batch '{self.batch_id}' has been detached")
            self._detached = True
            self._changed.notify_all()

        self._stop_background_threads()
        self._close_cached_connections("release")
        _delete_batch_objects(self._client.k8s_helper, self.namespace, self.batch_id, self._lease_name)

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
        """Closes the cached connections to the given claims' sandboxes, or to every sandbox."""
        with self._lock:
            names = list(self._connected) if claim_names is None else claim_names
        for claim_name in names:
            with self._lock:
                sandbox = self._connected.pop(claim_name, None)
            if sandbox is None:
                continue
            try:
                sandbox.close_connection()
            except Exception as e:
                # A connection that fails to close is kept on the handle rather than dropped.
                with self._lock:
                    self._connected.setdefault(claim_name, sandbox)
                logger.warning(
                    f"Batch '{self.batch_id}' failed to close a cached sandbox connection "
                    f"during {action}: {e}"
                )

    def detach(self, grace: int | None = None) -> None:
        """Stops the background watch and Lease renewal, closes cached sandbox connections, and
        releases this handle's hold on the Lease. ``grace`` is the number of seconds to keep the
        Lease valid after detaching, an int greater than ``CLOCK_SKEW_MARGIN`` (5); if ``None``, we
        use the batch's original Lease duration.

        If the Lease release fails, the error propagates and this handle stays in a detached state with
        the Lease still held. Call ``detach`` again to retry; steps that already completed are skipped.
        A batch that is still creating claims stops creating first, and every claim is left in place.

        Raises:
            ValueError: If ``grace`` is not a valid Lease duration.
            BatchError: If this handle has been released.
            ApiException: If the Lease release fails (an ``ApiException`` or a transport error).
        """
        if grace is not None:
            batch_utils.validate_lease_duration_value(grace)

        # _detached prevents the caller from connecting to Sandboxes, while _lease_released signals that
        # detach fully completed (i.e. the previous steps and the Lease release write succeeded), so callers
        # can retry detach() if the release fails.
        with self._lock:
            if self._released:
                raise BatchError(f"batch '{self.batch_id}' has been released")
            if self._lease_released:
                return
            self._detached = True
            self._changed.notify_all()
            # acquire() takes no ordinal once the handle is detached, so this is final.
            next_ordinal = self._state.next_ordinal

        self._stop_background_threads()
        self._close_cached_connections("detach")

        lease = self._client.k8s_helper.read_batch_lease(
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
                lease.spec.renew_time = batch_utils.lease_now()
                lease.metadata.annotations = {
                    **(lease.metadata.annotations or {}),
                    BATCH_NEXT_ORDINAL_ANNOTATION: str(next_ordinal),
                }
                # Refresh the Lease so it doesn't expire immediately after we release it,
                # giving other clients a chance to acquire it.
                lease.spec.lease_duration_seconds = (
                    grace if grace is not None else self._lease_duration
                )
                try:
                    self._client.k8s_helper.replace_batch_lease(
                        self._lease_name,
                        self.namespace,
                        lease,
                        _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                    )
                except ApiException as e:
                    if e.status != 409:
                        raise
                    # The Lease changed since the read above, most likely because a concurrent
                    # detach() of this handle released it first. Only a Lease still held here is a failure.
                    current = self._client.k8s_helper.read_batch_lease(
                        self._lease_name, self.namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
                    )
                    if current is not None and current.spec.holder_identity == self._holder_identity:
                        raise

        with self._lock:
            self._lease_released = True
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
                        self._changed.notify_all()
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
                    with self._lock:
                        if event_type == "DELETED":
                            self._state.mark_lost((obj.get("metadata") or {}).get("name", ""))
                        elif event_type in ("ADDED", "MODIFIED"):
                            self._state.upsert_claim(obj)
                        self._changed.notify_all()
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
                    self._changed.notify_all()
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
                    self._changed.notify_all()
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
                if lease.spec.holder_identity != self._holder_identity:
                    return self._stop_renewal(
                        BatchInUseError(
                            f"batch '{self.batch_id}' Lease is no longer held by this handle "
                            f"(current holder: {lease.spec.holder_identity!r})"
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
            self._changed.notify_all()
        return False

    def _write_renewal(self, lease: Any) -> tuple[float, V1Lease]:
        """Stamps ``lease`` as renewed now and replaces it, returning the send time and the written Lease."""
        # Neither Kubernetes client has a default request timeout, so a hung apiserver would
        # otherwise stall renewal past the Lease's expiry.
        timeout = self._renew_request_timeout()
        sent_at = time.monotonic()
        lease.spec.holder_identity = self._holder_identity
        lease.spec.renew_time = batch_utils.lease_now()
        lease.spec.lease_duration_seconds = self._lease_duration
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
        so together they start at most ``create_rps`` creates per second.
        """
        while True:
            with self._lock:
                if not self._create_plan or self._stop_creating.is_set():
                    return
                claim_name, group = self._create_plan.popleft()
                slot = self._reserve_create_slot(time.monotonic())
                # This worker took the last planned create, so the fill deadline is known now
                if not self._create_plan:
                    # Counted from when the last create is sent, so groups don't time out while
                    # their claims are still waiting for a pacing slot.
                    self._fill_deadline = slot + self._quorum_timeout
                    self._changed.notify_all()
            if self._stop_creating.wait(max(0.0, slot - time.monotonic())):
                return
            with self._lock:
                # Once err() is set the handle can no longer watch or hold the batch, so further
                # claims would be ones nobody can observe or keep alive.
                if self._state.error() is not None:
                    return
                self._expire_fill_if_due()
                if self._state.fill_creates_released(group.warmpool):
                    self._state.release_claim(claim_name, group.warmpool)
                    self._changed.notify_all()
                    continue
                if self._state.group_failed(group.warmpool):
                    self._state.record_skipped_create()
                    self._changed.notify_all()
                    continue
            try:
                created = self._create_one(
                    claim_name,
                    group.warmpool,
                    self._claim_annotations[group.warmpool],
                    self._claim_labels[group.warmpool],
                    self._claim_lifecycle,
                )
            except Exception as e:
                with self._lock:
                    self._state.record_create_failure(
                        claim_name, group.warmpool, _create_failure_message(e)
                    )
                    self._changed.notify_all()
                continue
            if created:
                self._delete_released_create(claim_name, group.warmpool)

    def _reserve_create_slot(self, earliest: float) -> float:
        """Reserves the next pacing slot at or after ``earliest``, so creates and their retries
        together stay within ``create_rps``. The caller holds ``self._lock``.
        """
        slot = max(earliest, self._next_create_at)
        self._next_create_at = slot + self._create_interval
        return slot

    def _delete_released_create(self, claim_name: str, warmpool: str) -> None:
        """Deletes a fill claim whose create may have landed after the caller released its group or
        ended the fill. A failed delete leaves the claim for ``release()``.
        """
        with self._lock:
            if not self._state.fill_creates_released(warmpool):
                return
            self._state.release_claim(claim_name, warmpool)
            self._changed.notify_all()
        try:
            self._delete_claim(claim_name)
        except Exception as e:
            logger.warning(f"Batch '{self.batch_id}' failed to delete released claim '{claim_name}': {e}")

    def _create_one(
        self,
        claim_name: str,
        warmpool: str,
        annotations: dict[str, str],
        labels: dict[str, str],
        lifecycle: dict[str, str],
        deadline: float | None = None,
    ) -> bool:
        """Creates one claim, retrying transient errors, with each retry taking a pacing slot.

        Returns whether the claim may exist, which is ``False`` only if creation was stopped.

        Raises:
            BatchTimeoutError: If a retry's pacing slot is at or after ``deadline``.
            Exception: The error the create failed with, once it isn't retried.
        """
        attempt = 1
        while True:
            try:
                self._client.k8s_helper.create_sandbox_claim(
                    claim_name,
                    warmpool,
                    self.namespace,
                    annotations=annotations,
                    labels=labels,
                    lifecycle=lifecycle,
                    log_level=logging.DEBUG,
                    _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                )
                return True
            except Exception as e:
                if isinstance(e, ApiException):
                    outcome = batch_utils.create_error_outcome(e.status, attempt)
                elif _is_transport_error(e):
                    outcome = batch_utils.create_error_outcome(None, attempt)
                else:
                    outcome = batch_utils.CreateOutcome.FAIL
                if outcome == batch_utils.CreateOutcome.SUCCESS:
                    return True
                if outcome == batch_utils.CreateOutcome.RETRY:
                    with self._lock:
                        earliest = time.monotonic() + batch_utils.retry_delay(attempt, e)
                        # A retry paced past the deadline would only make a claim to delete on timeout.
                        if deadline is not None and max(earliest, self._next_create_at) >= deadline:
                            raise BatchTimeoutError(f"no create retry slot for claim '{claim_name}' before the deadline") from e
                        slot = self._reserve_create_slot(earliest)
                    if self._stop_creating.wait(max(0.0, slot - time.monotonic())):
                        return False
                    attempt += 1
                    continue
                logger.debug(f"Batch '{self.batch_id}' failed to create claim '{claim_name}': {e}")
                raise


def _precheck_pools(helper: Any, warmpools: Iterable[str], namespace: str) -> None:
    """Checks that each warm pool and the SandboxTemplate it references exist.

    Raises:
        SandboxWarmPoolNotFoundError: If a warm pool doesn't exist.
        SandboxTemplateNotFoundError: If a warm pool's template doesn't exist, or it names none.
    """
    timeout = batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
    checked_templates: set[str] = set()
    for name in warmpools:
        warmpool = helper.get_sandbox_warmpool(name, namespace, _request_timeout=timeout)
        if warmpool is None:
            raise SandboxWarmPoolNotFoundError(
                f"SandboxWarmPool '{name}' not found in namespace '{namespace}'"
            )
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


def _delete_batch_objects(helper: Any, namespace: str, batch_id: str, lease_name: str) -> None:
    """Deletes a batch's claims in rounds, then its Lease once no claim is left that isn't
    already being deleted.

    Also used by ``AsyncSandboxClient``'s atexit hook, which can't use the event loop.

    Raises:
        BatchError: If claims are still not being deleted after the last round.
        ApiException: For a non-transient error at once, or the last transient error after
            the last round.
    """
    _delete_claims_in_rounds(
        helper,
        namespace,
        f"{BATCH_ID_LABEL}={batch_id}",
        f"batch '{batch_id}'",
        finish=lambda: helper.delete_batch_lease(
            lease_name, namespace, _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS
        ),
    )


def _delete_claims_in_rounds(
    helper: Any,
    namespace: str,
    label_selector: str,
    description: str,
    finish: Callable[[], None] | None = None,
) -> None:
    """Deletes the claims matching ``label_selector`` with deletecollection rounds, re-listing
    between rounds until no claim is left that isn't already being deleted, then calls ``finish``.

    A transient error, in ``finish`` too, moves on to the next round, and it gives up after
    ``BATCH_RELEASE_MAX_IDLE_ROUNDS`` rounds in a row that delete nothing.

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
        try:
            # deletecollection deletes one claim at a time on the apiserver, so for a large batch
            # it can time out while deletion continues. For this case, we track it with the list below.
            helper.delete_sandbox_claims_by_label(
                namespace, label_selector, _request_timeout=collection_timeout
            )
        except Exception as e:
            if not _is_transient_error(e):
                raise
            last_error = e

        live: list[dict[str, Any]] | None = None
        try:
            # A consistent read (no resourceVersion), so a lagging watch cache can't report
            # that no claims are left too early.
            items, _ = helper.list_sandbox_claim_objects(
                namespace, label_selector, _request_timeout=collection_timeout
            )
            live = [c for c in items if not (c.get("metadata") or {}).get("deletionTimestamp")]
        except Exception as e:
            if not _is_transient_error(e):
                raise
            last_error = e

        if live == []:
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
        progressed = live is not None and (remaining_before is None or len(live) < remaining_before)
        idle_rounds = 0 if progressed else idle_rounds + 1
        if idle_rounds >= batch_utils.BATCH_RELEASE_MAX_IDLE_ROUNDS:
            if last_error is not None:
                raise last_error
            raise BatchError(
                f"{description}: {len(live or [])} claims are still not being deleted"
            )
        if live is not None:
            remaining_before = len(live)
        time.sleep(batch_utils.retry_delay(max(idle_rounds, 1), last_error))
