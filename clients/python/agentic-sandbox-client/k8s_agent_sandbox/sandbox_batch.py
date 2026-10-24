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
from collections.abc import Callable, Iterator
from typing import TYPE_CHECKING, Any, TypeVar

import urllib3.exceptions
from kubernetes import client
from kubernetes.client import V1Lease, V1LeaseSpec, V1ObjectMeta
from kubernetes.client.exceptions import ApiException

from . import batch_state, batch_utils
from .constants import (
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    BATCH_ID_LABEL,
    BATCH_LEASE_DURATION_ANNOTATION,
    BATCH_LEASE_NAME_PREFIX,
    BATCH_QUORUM_TIMEOUT_ANNOTATION,
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
from .models import BatchEvent, BatchGroup, GroupOutcome, Member, MemberState
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
        # Held for all of release() and around detach()'s Lease writes, so whichever call starts first
        # finishes before the other acts on the Lease. It's reentrant, so a call from a signal handler on
        # the same thread doesn't hang.
        self._lifecycle_lock = threading.RLock()
        self._connected: dict[str, "Sandbox"] = {}
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
        # When this handle considers its Lease expired. It's a little earlier than the expiry other
        # clients see, so the handle never thinks it holds the batch after someone else could take it.
        self._lease_expires_at = lease_renewed_at + lease_duration
        self._watch_thread: threading.Thread | None = None
        self._renew_thread: threading.Thread | None = None

        # Notified on every change consumers wait for: watch events, create failures, errors,
        # the fill deadline being set, and detach/release.
        self._changed = threading.Condition(self._lock)
        self._quorum_timeout = quorum_timeout
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
        self._claim_labels: dict[str, str] = {}
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
        checked_templates: set[str] = set()
        for group in args.groups:
            warmpool = helper.get_sandbox_warmpool(group.warmpool, namespace, _request_timeout=timeout)
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
            if helper.get_sandbox_template(template, namespace, _request_timeout=timeout) is None:
                raise SandboxTemplateNotFoundError(
                    f"SandboxTemplate '{template}' not found in namespace '{namespace}'"
                )
            checked_templates.add(template)

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
        lease_labels, lease_annotations = batch_utils.batch_lease_metadata(
            args.batch_id, args.lease_duration, args.work_budget, args.quorum_timeout
        )
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
        )
        handle._create_plan = deque(batch_utils.fill_create_plan(args.batch_id, args.groups))
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

        annotation = None
        quorum_annotation = None
        if lease is not None:
            annotation = (lease.metadata.annotations or {}).get(BATCH_LEASE_DURATION_ANNOTATION)
            quorum_annotation = (lease.metadata.annotations or {}).get(BATCH_QUORUM_TIMEOUT_ANNOTATION)
        duration = batch_utils.parse_lease_duration_annotation(annotation)
        quorum_timeout = batch_utils.parse_quorum_timeout_annotation(quorum_annotation)

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
        state = batch_state.BatchState(batch_id, groups)
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
            quorum_timeout=quorum_timeout,
        )
        handle._fill_deadline = time.monotonic() + quorum_timeout
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
        ``group_outcomes()`` can't be called after ``events()``, since the first of the two to be
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
            BatchError: If ``events()`` was called first, or this handle has been detached or
                released, when called or while waiting.
        """
        with self._lock:
            self._check_active()
            self._state.set_mode(batch_state.ConsumerMode.GROUP)
        return self._iterate(self._state.pop_group_outcome, self._state.groups_done)

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
                if self._released:
                    return
                if self._lease_released:
                    raise BatchError(f"batch '{self.batch_id}' has been detached or is held by another handle")
                self._detached = True
                self._changed.notify_all()
                # A failed release is retried by client cleanup as a release, not as an earlier detach.
                self._detach_grace = None

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

    def _close_cached_connections(self, action: str) -> None:
        with self._lock:
            names = list(self._connected)
        for claim_name in names:
            with self._lock:
                sandbox = self._connected.pop(claim_name, None)
            if sandbox is None:
                continue
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
                        self._connected.setdefault(claim_name, sandbox)
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
            self._changed.notify_all()
            if grace is not None:
                self._detach_grace = grace
            elif self._detach_grace is None:
                self._detach_grace = self._lease_duration
            # Another detach() call can change _detach_grace while this call writes the Lease, so every write
            # uses this copy.
            grace = self._detach_grace

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
                    if lease.spec.holder_identity != self._holder_identity:
                        logger.info(
                            f"Batch '{self.batch_id}' Lease is held by "
                            f"'{lease.spec.holder_identity}', not this handle; treating as released"
                        )
                    else:
                        for attempt in range(2):
                            lease.spec.holder_identity = None
                            lease.spec.renew_time = batch_utils.lease_now()
                            # Refresh the Lease so it doesn't expire immediately after we release it,
                            # giving other clients a chance to acquire it.
                            lease.spec.lease_duration_seconds = grace
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
                                if lease is None or lease.spec.holder_identity != self._holder_identity:
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
                if self._state.group_failed(group.warmpool):
                    self._state.record_skipped_create()
                    self._changed.notify_all()
                    continue
            self._create_one(claim_name, group)

    def _reserve_create_slot(self, earliest: float) -> float:
        """Reserves the next pacing slot at or after ``earliest``, so creates and their retries
        together stay within ``create_rps``. The caller holds ``self._lock``.
        """
        slot = max(earliest, self._next_create_at)
        self._next_create_at = slot + self._create_interval
        return slot

    def _create_one(self, claim_name: str, group: BatchGroup) -> None:
        attempt = 1
        while True:
            try:
                self._client.k8s_helper.create_sandbox_claim(
                    claim_name,
                    group.warmpool,
                    self.namespace,
                    annotations=self._claim_annotations[group.warmpool],
                    labels=self._claim_labels,
                    lifecycle=self._claim_lifecycle,
                    log_level=logging.DEBUG,
                    _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                )
                return
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
                    # A backoff belongs to this create alone, so the create waits it out here and then takes a
                    # normal pacing slot. client-go retries the same way. The apiserver computes a Retry-After for a
                    # whole priority level, so a Retry-After moves the shared slot and every create waits for it.
                    retry_after = batch_utils.retry_after_seconds(e)
                    if retry_after is None and self._stop_creating.wait(batch_utils.retry_delay(attempt, None)):
                        return
                    with self._lock:
                        slot = self._reserve_create_slot(time.monotonic() + (retry_after or 0.0))
                    if self._stop_creating.wait(max(0.0, slot - time.monotonic())):
                        return
                    attempt += 1
                    continue
                logger.debug(f"Batch '{self.batch_id}' failed to create claim '{claim_name}': {e}")
                with self._lock:
                    self._state.record_create_failure(
                        claim_name, group.warmpool, _create_failure_message(e)
                    )
                    self._changed.notify_all()
                return


def _take_batch_lease(
    helper: Any,
    namespace: str,
    batch_id: str,
    lease_name: str,
    holder_identity: str,
    lease_duration: int,
    last_lease: Any,
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
        # Another handle that attached since this handle's last write raised leaseTransitions, and a
        # Lease that was deleted and created again has a new UID. Either Lease can have no holder by
        # now, so the holder check below wouldn't catch it.
        if lease.metadata.uid != last_lease.metadata.uid or (lease.spec.lease_transitions or 0) != (
            last_lease.spec.lease_transitions or 0
        ):
            raise BatchInUseError(f"batch '{batch_id}' changed hands since this handle held it")
        if lease.spec.holder_identity and lease.spec.holder_identity != holder_identity:
            raise BatchInUseError(f"batch '{batch_id}' is held by '{lease.spec.holder_identity}'")
        lease.spec.holder_identity = holder_identity
        lease.spec.renew_time = now
        lease.spec.lease_duration_seconds = lease_duration
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
        return str(written.metadata.resource_version)


def _delete_batch_objects(
    helper: Any,
    namespace: str,
    batch_id: str,
    lease_name: str,
    holder_identity: str,
    lease_duration: int,
    last_lease: Any,
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
    label_selector = f"{BATCH_ID_LABEL}={batch_id}"
    collection_timeout = batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS
    idle_rounds = 0
    remaining_before: int | None = None
    while True:
        last_error: Exception | None = None
        taken = False
        resource_version: str | None = None
        try:
            resource_version = _take_batch_lease(
                helper, namespace, batch_id, lease_name, holder_identity, lease_duration, last_lease
            )
            taken = True
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

        # Only a round whose take succeeded can end the release. After a failed take the Lease may
        # still exist, and this round has no resourceVersion to delete it at.
        if live == [] and taken:
            # This round found no Lease, so there is none to delete.
            if resource_version is None:
                return
            try:
                helper.delete_batch_lease(
                    lease_name,
                    namespace,
                    _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
                    resource_version=resource_version,
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
        time.sleep(batch_utils.retry_delay(max(idle_rounds, 1), last_error))
