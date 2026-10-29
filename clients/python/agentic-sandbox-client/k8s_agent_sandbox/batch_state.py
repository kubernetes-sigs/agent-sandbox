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

"""
Batch state core shared by both sync and async batch handles.

Contains methods for modifying and re-populating a batch's state that operate on already-fetched
Kubernetes objects and thus do not perform any network I/O, threading, or locking of their own.
"""

from collections import deque
from collections.abc import Sequence
from enum import Enum
from operator import itemgetter
from typing import Any

from .constants import (
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    TERMINAL_CLAIM_READY_REASONS,
)
from .exceptions import BatchError, BatchTimeoutError, QuorumUnreachableError
from .models import BatchEvent, BatchEventType, BatchGroup, GroupOutcome, Member, MemberState

CREATE_FAILED_REASON = "CreateFailed"


class ConsumerMode(Enum):
    """How a batch hands out Ready members. Set by the first consumer type called."""
    # Set upon calling events() first. Here, every Ready member is streamed.
    STREAM = "stream"
    # Set by calling group consumer (group_outcomes()) first. Here, members are handed out per group.
    GROUP = "group"
    # Set by calling wait_for_quorum() first. Here, members are held until every group has min_ready of them.
    QUORUM = "quorum"


_CONSUMER_METHODS = {
    ConsumerMode.STREAM: "events()",
    ConsumerMode.GROUP: "group_outcomes()",
    ConsumerMode.QUORUM: "wait_for_quorum()",
}


def parse_ordinal(batch_id: str, claim_name: str) -> int | None:
    """Extracts the ordinal from a ``<batch-id>-<ordinal>`` claim name."""
    prefix = f"{batch_id}-"
    if not claim_name.startswith(prefix):
        return None
    suffix = claim_name[len(prefix):]
    if not suffix.isdigit():
        return None
    return int(suffix)


def ordinal_sort_key(ordinal: int | None) -> tuple[bool, int]:
    """Sorts by ordinal, putting a claim name without an ordinal after every claim that has one."""
    return (ordinal is None, ordinal or 0)


def reconstruct_groups(batch_id: str, claim_objs: Sequence[dict[str, Any]]) -> list[BatchGroup]:
    """Rebuilds each pool's ``BatchGroup`` from its claims' group annotations.

    Groups come back in the order the batch was created with, so a handle from ``get_batch``
    lists ``groups`` in the same order as the handle that created the batch. The claims don't
    record that order directly, but each group's claims are numbered consecutively in it, so
    sorting the groups by the lowest ordinal among their remaining claims restores it. A pool with
    no group annotations on any of its claims becomes ``BatchGroup(size=0, min_ready=0)``, and
    claims that are detected with different or invalid annotation values in the same pool raise
    ``BatchError``.
    """
    by_pool: dict[str, tuple[str, str]] = {}
    # Each pool's sort key is its lowest ordinal.
    pool_order: dict[str, tuple[bool, int]] = {}
    for claim_obj in claim_objs:
        spec = claim_obj.get("spec") or {}
        warmpool = (spec.get("warmPoolRef") or {}).get("name", "")
        metadata = claim_obj.get("metadata") or {}
        key = ordinal_sort_key(parse_ordinal(batch_id, metadata.get("name", "")))
        pool_order[warmpool] = min(pool_order.get(warmpool, key), key)
        annotations = metadata.get("annotations") or {}
        size_str = annotations.get(BATCH_GROUP_SIZE_ANNOTATION)
        min_ready_str = annotations.get(BATCH_GROUP_MIN_READY_ANNOTATION)
        if size_str is None and min_ready_str is None:
            continue
        if size_str is None or min_ready_str is None:
            raise BatchError(
                f"claim {(claim_obj.get('metadata') or {}).get('name')!r} for pool "
                f"{warmpool!r} carries only one of the batch group annotations"
            )
        pair = (size_str, min_ready_str)
        if warmpool in by_pool and by_pool[warmpool] != pair:
            raise BatchError(
                f"pool {warmpool!r} has conflicting batch group annotations "
                "across its claims"
            )
        by_pool[warmpool] = pair

    groups = []
    for pool in sorted(pool_order, key=lambda p: (pool_order[p], p)):
        if pool in by_pool:
            size_str, min_ready_str = by_pool[pool]
            try:
                groups.append(
                    BatchGroup(warmpool=pool, size=int(size_str), min_ready=int(min_ready_str))
                )
            except ValueError as e:
                # pydantic's ValidationError is a ValueError too, so this covers both
                # non-integer values and a size/min_ready pair BatchGroup rejects.
                raise BatchError(
                    f"pool {pool!r} has invalid batch group annotations "
                    f"(size={size_str!r}, min_ready={min_ready_str!r})"
                ) from e
        else:
            groups.append(BatchGroup(warmpool=pool, size=0, min_ready=0))
    return groups


def derive_member(claim_obj: dict[str, Any]) -> Member:
    """Builds a ``Member`` from a raw SandboxClaim object."""
    metadata = claim_obj.get("metadata") or {}
    spec = claim_obj.get("spec") or {}
    status = claim_obj.get("status") or {}
    conditions = status.get("conditions") or []

    sandbox_status = status.get("sandbox") or {}
    sandbox_name = sandbox_status.get("name") or None

    state = MemberState.PENDING
    reason: str | None = None
    message: str | None = None
    ready_condition = next((c for c in conditions if c.get("type") == "Ready"), None)
    if ready_condition is not None:
        reason = ready_condition.get("reason")
        message = ready_condition.get("message")
        if ready_condition.get("status") == "True":
            if sandbox_name is not None:
                state = MemberState.READY
        # We do not treat WarmPoolNotFound or TemplateNotFound as terminal, because the controller
        # requeues those every minute rather than failing the claim.
        elif ready_condition.get("status") == "False" and reason in TERMINAL_CLAIM_READY_REASONS:
            state = MemberState.FAILED

    # The controller stops reconciling a terminating claim, so its Ready condition goes stale.
    # Treat it as already deleted, the same way mark_lost does, so the DELETED event changes nothing.
    terminating = metadata.get("deletionTimestamp") is not None
    if terminating and state is not MemberState.FAILED:
        state = MemberState.LOST
        reason = message = None

    return Member(
        claim_name=metadata.get("name", ""),
        sandbox_name=sandbox_name,
        warmpool=(spec.get("warmPoolRef") or {}).get("name", ""),
        pod_ips=() if terminating else tuple(sandbox_status.get("podIPs") or []),
        service_fqdn=None if terminating else sandbox_status.get("serviceFQDN"),
        state=state,
        reason=reason,
        message=message,
    )


class BatchState:
    """The internal state of a batch handle, containing its members and metadata.

    Exposes methods for tracking the batch's fill (its initial claims, which carry the group annotations).
    Each change to a fill member queues the events and group outcomes that ``events()`` and
    ``group_outcomes()`` yield, and decides the quorum ``wait_for_quorum()`` returns, in O(1).
    """

    def __init__(self, batch_id: str, groups: Sequence[BatchGroup]) -> None:
        self.batch_id = batch_id
        self.groups: list[BatchGroup] = list(groups)
        self.size = sum(g.size for g in self.groups)

        self._members: dict[str, Member] = {}
        # Claims of the initial fill. Ordinals can't tell them apart from later claims, since a
        # re-attached handle only knows the sizes of the groups that still have claims.
        self._fill_claims: set[str] = set()
        self._error: Exception | None = None

        self._group_by_pool: dict[str, BatchGroup] = {g.warmpool: g for g in self.groups}
        # BatchGroup sets a min_ready of None to size, so we use ``or 0`` only to satisfy the type checker.
        self._min_ready: dict[str, int] = {g.warmpool: g.min_ready or 0 for g in self.groups}
        self._mode: ConsumerMode | None = None
        # The Ready fill members not yet handed out, per pool. A dict is used as an ordered set to return
        # members in the order they became ready.
        self._waiting: dict[str, dict[str, None]] = {pool: {} for pool in self._group_by_pool}
        # Set of claims already handed to the handle
        self._dispatched: set[str] = set()
        # Fill members that can no longer become Ready (terminal or lost). Each is counted once and never
        # removed, even if its claim comes back, so group outcomes and the settle check never contradict
        # what callers were already told.
        self._unable: set[str] = set()
        self._failed: dict[str, int] = dict.fromkeys(self._group_by_pool, 0)
        self._lost: dict[str, int] = dict.fromkeys(self._group_by_pool, 0)
        # Fill claims that will never exist (e.g., creates skipped because their group failed, and
        # claims missing when a batch is re-attached). These have no name, so they are not added to _unable.
        self._never_created = 0
        # Fill claims each group lacked when the batch was re-attached, also counted in _lost and
        # _never_created. Those two also count real losses and skipped creates, so they can't tell how
        # many counts a late claim may take back.
        self._missing: dict[str, int] = dict.fromkeys(self._group_by_pool, 0)
        # Fill members whose outcome is decided. A member is added once it is unable, handed out, or
        # dropped while Ready because its group or the quorum failed. It stays here if it later stops
        # being Ready, so a settled fill stays settled. A held member isn't added, since it may stop
        # being Ready and be handed out later. _unable and _dispatched overlap, so one set keeps the
        # count O(1).
        self._resolved: set[str] = set()
        self._group_outcomes: dict[str, GroupOutcome] = {}   # Each group's final outcome
        self._events_to_yield: deque[BatchEvent] = deque()  # Events events() hasn't returned yet; oldest first
        self._group_outcomes_to_yield: deque[GroupOutcome] = deque()  # Outcomes group_outcomes() hasn't returned yet; oldest first
        self._fill_expired = False
        # The quorum's result once it is decided, either the members it returns or the error it failed with.
        self._quorum_members: list[Member] | None = None
        self._quorum_error: Exception | None = None
        # Groups with fewer than min_ready held members, so the quorum is reached when this is empty.
        self._groups_short_of_quorum: set[str] = set(self._group_by_pool)

    def seed_from_claims(self, claim_objs: Sequence[dict[str, Any]]) -> None:
        """Reconstructs state from the batch's existing claims."""
        for claim_obj in claim_objs:
            self.upsert_claim(claim_obj)

    def upsert_claim(self, claim_obj: dict[str, Any]) -> Member | None:
        """Derives a ``Member`` from a claim and inserts or updates it."""
        metadata = claim_obj.get("metadata") or {}
        claim_name = metadata.get("name", "")
        annotations = metadata.get("annotations") or {}
        if BATCH_GROUP_SIZE_ANNOTATION in annotations and BATCH_GROUP_MIN_READY_ANNOTATION in annotations:
            self._fill_claims.add(claim_name)
        member = derive_member(claim_obj)
        if claim_name not in self._members and self._is_fill(claim_name, member) and self._missing[member.warmpool]:
            # A create that timed out before the batch was re-attached landed later. Its claim takes back
            # one missing count, so the slot counts once, by its real state.
            self._missing[member.warmpool] -= 1
            self._lost[member.warmpool] -= 1
            self._never_created -= 1
        self._members[claim_name] = member
        self._on_fill_change(claim_name, member)
        return member

    def mark_lost(self, claim_name: str) -> Member | None:
        """Handles a lost claim (watch DELETED or missing on re-list).

        A member that already failed stays ``FAILED``, so its state keeps saying why it can't
        become Ready. A lost member drops its reason and message, which described the deleted
        claim's last Ready condition.
        """
        existing = self._members.get(claim_name)
        if existing is None:
            # Deleted before this handle ever saw an ADDED/MODIFIED event for it.
            return None
        update: dict[str, Any] = {"pod_ips": (), "service_fqdn": None}
        if existing.state is not MemberState.FAILED:
            update.update(state=MemberState.LOST, reason=None, message=None)
        lost_member = existing.model_copy(update=update)
        self._members[claim_name] = lost_member
        self._on_fill_change(claim_name, lost_member)
        return lost_member

    def resync_from_list(self, claim_objs: Sequence[dict[str, Any]]) -> None:
        """Replaces the cache with a fresh list after a watch 410."""
        seen_names = set()
        for claim_obj in claim_objs:
            name = (claim_obj.get("metadata") or {}).get("name", "")
            seen_names.add(name)
            self.upsert_claim(claim_obj)
        missing = set(self._members.keys()) - seen_names
        for name in missing:
            self.mark_lost(name)

    def members(self, warmpool: str | None = None) -> list[Member]:
        """Returns a snapshot of the batch's members, optionally filtered to one pool."""
        items = [
            (ordinal_sort_key(parse_ordinal(self.batch_id, name)), member)
            for name, member in self._members.items()
            if warmpool is None or member.warmpool == warmpool
        ]
        items.sort(key=itemgetter(0))
        return [m for _, m in items]

    def get_member(self, claim_name: str) -> Member | None:
        """Returns a single member."""
        return self._members.get(claim_name)

    def note_error(self, error: Exception) -> None:
        """Records a sticky error for ``err()``."""
        if self._error is None:
            self._error = error

    def error(self) -> Exception | None:
        return self._error

    def _is_fill(self, claim_name: str, member: Member) -> bool:
        """Whether a claim is one of the batch's initial claims (i.e. it carries the group annotations and its pool is known)."""
        return claim_name in self._fill_claims and member.warmpool in self._group_by_pool

    def _on_fill_change(self, claim_name: str, member: Member) -> None:
        """Updates the fill accounting for one changed member, queuing any events and group outcome."""
        if not self._is_fill(claim_name, member):
            return
        pool = member.warmpool
        if claim_name not in self._unable and member.state in (MemberState.FAILED, MemberState.LOST):
            self._unable.add(claim_name)
            self._resolved.add(claim_name)
            if member.state is MemberState.LOST:
                self._lost[pool] += 1
            else:
                self._failed[pool] += 1
            self._waiting[pool].pop(claim_name, None)
            event_type = (
                BatchEventType.MEMBER_LOST
                if member.state is MemberState.LOST
                else BatchEventType.MEMBER_FAILED
            )
            self._events_to_yield.append(BatchEvent(type=event_type, member=member))
        elif claim_name not in self._unable and member.state is MemberState.READY:
            if claim_name not in self._dispatched:
                self._route_ready_member(claim_name, pool)
        elif member.state is not MemberState.READY:
            # A member that handed out earlier stays handed out, while one that still waiting
            # is dropped until it is Ready again.
            self._waiting[pool].pop(claim_name, None)
        if self._mode == ConsumerMode.GROUP:
            self._decide_group_outcome(pool)
        elif self._mode == ConsumerMode.QUORUM:
            self._check_quorum(pool)

    def _route_ready_member(self, claim_name: str, pool: str) -> None:
        """Decides, then either hands a newly Ready member out, holds it until its group
        yields or the quorum is reached, or drops it if its group or the quorum failed.
        """
        match self._mode:
            case ConsumerMode.STREAM:
                self._dispatch(claim_name)
            case ConsumerMode.QUORUM:
                if self._quorum_members is not None:
                    self._dispatch(claim_name)
                elif self._quorum_error is None:
                    self._waiting[pool][claim_name] = None
                else:
                    # The quorum failed, so the member is dropped.
                    self._resolved.add(claim_name)
            case _:
                # In group mode, a group's Ready members are held until the group yields, so a
                # group that fails never hands out any of its members. Before any consumer is
                # called, every Ready member is held so it counts once the mode is set.
                outcome = self._group_outcomes.get(pool)
                if outcome is None:
                    self._waiting[pool][claim_name] = None
                elif outcome.error is None:
                    self._dispatch(claim_name)
                else:
                    # The group failed, so the member is dropped.
                    self._resolved.add(claim_name)

    def _dispatch(self, claim_name: str) -> None:
        """Hands a member out to ``events()`` as ``MEMBER_READY``."""
        self._dispatched.add(claim_name)
        self._resolved.add(claim_name)
        self._events_to_yield.append(
            BatchEvent(type=BatchEventType.MEMBER_READY, member=self._members[claim_name])
        )

    def _decide_group_outcome(self, pool: str) -> None:
        """Sets a group's outcome once it has ``min_ready`` held members, or can no longer get them."""
        if pool in self._group_outcomes:
            return
        min_ready = self._min_ready[pool]
        waiting = self._waiting[pool]
        if len(waiting) >= min_ready:
            names = list(waiting)
            waiting.clear()
            cohort = names[:min_ready]
            self._dispatched.update(cohort)
            self._resolved.update(cohort)
            self._set_group_outcome(GroupOutcome(warmpool=pool, members=[self._members[n] for n in cohort]))
            for name in names[min_ready:]:
                self._dispatch(name)
        elif (error := self._unreachable_error(pool)) is not None:
            self._resolved.update(waiting)
            waiting.clear()
            self._set_group_outcome(GroupOutcome(warmpool=pool, error=error))

    def _unreachable_error(self, pool: str) -> QuorumUnreachableError | None:
        """Returns the error for a group that can no longer get ``min_ready`` Ready members, or ``None``."""
        group = self._group_by_pool[pool]
        min_ready = self._min_ready[pool]
        if group.size - self._failed[pool] - self._lost[pool] >= min_ready:
            return None
        return QuorumUnreachableError(
            warmpool=pool,
            size=group.size,
            min_ready=min_ready,
            failed=self._failed[pool],
            lost=self._lost[pool],
        )

    def _set_group_outcome(self, outcome: GroupOutcome) -> None:
        """Records a group's outcome and queues it for ``group_outcomes()``."""
        self._group_outcomes[outcome.warmpool] = outcome
        self._group_outcomes_to_yield.append(outcome)

    def _time_out_undecided_groups(self) -> None:
        """Gives every group without an outcome a ``BatchTimeoutError``."""
        for pool in self._group_by_pool:
            if pool not in self._group_outcomes:
                self._waiting[pool].clear()
                self._set_group_outcome(GroupOutcome(warmpool=pool, error=BatchTimeoutError("Group quorum timed out")))

    def _check_quorum(self, pool: str) -> None:
        """Reaches the quorum once every group has ``min_ready`` held members, or fails it once
        ``pool``'s group can no longer get them. Only ``pool``'s group changed, so only it is checked.
        """
        if self._quorum_members is not None or self._quorum_error is not None:
            return
        if len(self._waiting[pool]) < self._min_ready[pool]:
            self._groups_short_of_quorum.add(pool)
            if (error := self._unreachable_error(pool)) is not None:
                self.fail_quorum(error)
            return
        self._groups_short_of_quorum.discard(pool)
        if self._groups_short_of_quorum:
            return
        # The quorum returns exactly min_ready members per group. The rest are queued on events()
        # at the same moment, so they aren't delayed.
        quorum: list[str] = []
        extras: list[str] = []
        for pool_name, waiting in self._waiting.items():
            names = list(waiting)
            waiting.clear()
            min_ready = self._min_ready[pool_name]
            quorum.extend(names[:min_ready])
            extras.extend(names[min_ready:])
        self._dispatched.update(quorum)
        self._resolved.update(quorum)
        self._quorum_members = [self._members[n] for n in quorum]
        for name in extras:
            self._dispatch(name)

    def fail_quorum(self, error: Exception) -> tuple[list[Member], Exception | None]:
        """Fails the quorum if it isn't decided yet, and returns its result. No member is handed out
        afterwards, and every group counts as failed so that its remaining claims aren't created.
        """
        if self._quorum_members is None and self._quorum_error is None:
            self._quorum_error = error
            for waiting in self._waiting.values():
                self._resolved.update(waiting)
                waiting.clear()
        # The handle takes the lock again after its wait ends, so the watch may have reached the
        # quorum in between. That quorum stays reached, and the caller gets its members.
        return self._quorum_members or [], self._quorum_error

    def quorum_result(self) -> tuple[list[Member], Exception | None] | None:
        """Returns the quorum's members and the error it failed with, or ``None`` while it is undecided."""
        if self._quorum_members is None and self._quorum_error is None:
            return None
        return self._quorum_members or [], self._quorum_error

    def set_mode(self, mode: ConsumerMode) -> None:
        """Fixes how Ready members are handed out, on the first call of ``events()``
        (``ConsumerMode.STREAM``), ``group_outcomes()`` (``ConsumerMode.GROUP``), or
        ``wait_for_quorum()`` (``ConsumerMode.QUORUM``).

        Raises:
            BatchError: If ``group_outcomes()`` or ``wait_for_quorum()`` is called after a different
                consumer fixed the mode, or ``wait_for_quorum()`` is called a second time.
        """
        if self._mode is not None:
            # events() can join any mode, and group_outcomes() can be called again to continue.
            if mode == ConsumerMode.STREAM or mode == self._mode == ConsumerMode.GROUP:
                return
            if mode == self._mode == ConsumerMode.QUORUM:
                raise BatchError("wait_for_quorum() can only be called once")
            raise BatchError(f"{_CONSUMER_METHODS[mode]} can't be used after {_CONSUMER_METHODS[self._mode]}")
        self._mode = mode
        match mode:
            case ConsumerMode.STREAM:
                for waiting in self._waiting.values():
                    for name in waiting:
                        self._dispatch(name)
                    waiting.clear()
            case ConsumerMode.GROUP:
                for pool in self._group_by_pool:
                    self._decide_group_outcome(pool)
                if self._fill_expired:
                    self._time_out_undecided_groups()
            case ConsumerMode.QUORUM:
                for pool in self._group_by_pool:
                    self._check_quorum(pool)
                if self._fill_expired:
                    self.fail_quorum(BatchTimeoutError("Batch quorum timed out"))

    def record_create_failure(self, claim_name: str, warmpool: str, message: str) -> None:
        """Records a claim whose create failed as a terminal member with reason ``CreateFailed``."""
        if claim_name in self._members:
            # The watch has already seen the claim, so an earlier attempt did create it.
            return
        # Only the fill's creates are recorded here, and their claims would have carried the group
        # annotations.
        self._fill_claims.add(claim_name)
        member = Member(
            claim_name=claim_name,
            warmpool=warmpool,
            state=MemberState.FAILED,
            reason=CREATE_FAILED_REASON,
            message=message,
        )
        self._members[claim_name] = member
        self._on_fill_change(claim_name, member)

    def record_skipped_create(self) -> None:
        """Counts a fill claim that won't be created because its group failed, so the fill can settle."""
        self._never_created += 1

    def group_failed(self, warmpool: str) -> bool:
        """Whether the group's quorum, or the batch's quorum, failed, so its remaining claims shouldn't be created."""
        if self._mode == ConsumerMode.QUORUM:
            return self._quorum_error is not None
        outcome = self._group_outcomes.get(warmpool)
        return self._mode == ConsumerMode.GROUP and outcome is not None and outcome.error is not None

    def expire_fill(self) -> bool:
        """Ends the fill at its deadline. In group mode, every group without an outcome gets a
        ``BatchTimeoutError``, and in quorum mode, an undecided quorum fails with one.
        Members are left as they are.

        Returns ``False`` if the fill had already expired.
        """
        if self._fill_expired:
            return False
        self._fill_expired = True
        if self._mode == ConsumerMode.GROUP:
            self._time_out_undecided_groups()
        elif self._mode == ConsumerMode.QUORUM:
            self.fail_quorum(BatchTimeoutError("Batch quorum timed out"))
        return True

    def count_missing_fill_as_unable(self) -> None:
        """For a re-attached batch, counts each group's fill claims that don't exist as unable.
        The previous handle stopped creating before it detached, so such a claim can still appear only
        if a create that timed out landed anyway. ``upsert_claim`` then takes its count back.
        """
        seen: dict[str, int] = dict.fromkeys(self._group_by_pool, 0)
        for name, member in self._members.items():
            if self._is_fill(name, member):
                seen[member.warmpool] += 1
        for pool, group in self._group_by_pool.items():
            missing = max(0, group.size - seen[pool])
            self._missing[pool] += missing
            self._lost[pool] += missing
            self._never_created += missing

    def pop_event(self) -> BatchEvent | None:
        return self._events_to_yield.popleft() if self._events_to_yield else None

    def pop_group_outcome(self) -> GroupOutcome | None:
        return self._group_outcomes_to_yield.popleft() if self._group_outcomes_to_yield else None

    def fill_settled(self) -> bool:
        """Whether every fill member's outcome is decided, or if the fill deadline has passed."""
        return self._fill_expired or len(self._resolved) + self._never_created >= self.size

    def events_done(self) -> bool:
        return self.fill_settled() and not self._events_to_yield

    def groups_done(self) -> bool:
        return (
            len(self._group_outcomes) == len(self._group_by_pool)
            and not self._group_outcomes_to_yield
        )
