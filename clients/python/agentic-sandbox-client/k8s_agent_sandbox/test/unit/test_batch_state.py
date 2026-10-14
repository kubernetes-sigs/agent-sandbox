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

import unittest

from pydantic import ValidationError

from k8s_agent_sandbox import batch_state
from k8s_agent_sandbox.exceptions import BatchError, BatchTimeoutError, QuorumUnreachableError, TerminalMemberError
from k8s_agent_sandbox.models import BatchEventType, BatchGroup, MemberState
from k8s_agent_sandbox.constants import (
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    TERMINAL_CLAIM_READY_REASONS,
)


def _claim(
    name: str,
    warmpool: str = "pool-a",
    conditions: list[dict] | None = None,
    sandbox: dict | None = None,
    annotations: dict | None = None,
    deletion_timestamp: str | None = None,
) -> dict:
    metadata: dict = {"name": name}
    if annotations:
        metadata["annotations"] = annotations
    if deletion_timestamp:
        metadata["deletionTimestamp"] = deletion_timestamp
    status: dict = {}
    if conditions is not None:
        status["conditions"] = conditions
    if sandbox is not None:
        status["sandbox"] = sandbox
    return {
        "metadata": metadata,
        "spec": {"warmPoolRef": {"name": warmpool}},
        "status": status,
    }


class TestDeriveMember(unittest.TestCase):
    """The batch state must derive a Member correctly from a SandboxClaim."""

    def test_pending_no_sandbox_name(self):
        member = batch_state.derive_member(_claim("b1-0", conditions=[]))
        self.assertEqual(member.state, MemberState.PENDING)
        self.assertIsNone(member.sandbox_name)

    def test_bound_but_not_ready(self):
        member = batch_state.derive_member(
            _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}],
                sandbox={"name": "sbx-1"},
            )
        )
        self.assertEqual(member.state, MemberState.PENDING)
        self.assertEqual(member.sandbox_name, "sbx-1")
        self.assertEqual(member.reason, "SandboxNotReady")

    def test_ready(self):
        member = batch_state.derive_member(
            _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "True"}],
                sandbox={"name": "sbx-1", "podIPs": ["10.0.0.1"], "serviceFQDN": "sbx-1.svc"},
            )
        )
        self.assertEqual(member.state, MemberState.READY)
        self.assertEqual(member.pod_ips, ("10.0.0.1",))
        self.assertEqual(member.service_fqdn, "sbx-1.svc")

    def test_terminating_claim_counts_as_deleted(self):
        ready = {"type": "Ready", "status": "True", "reason": "Ready", "message": "x"}
        failed = {"type": "Ready", "status": "False", "reason": "ClaimExpired", "message": "x"}
        cases = (
            (ready, MemberState.LOST, None, None),
            (failed, MemberState.FAILED, "ClaimExpired", "x"),
        )
        for condition, expected, reason, message in cases:
            with self.subTest(expected=expected):
                claim = _claim(
                    "b1-0",
                    conditions=[condition],
                    sandbox={"name": "sbx-1", "podIPs": ["10.0.0.1"], "serviceFQDN": "sbx-1.svc"},
                    deletion_timestamp="2026-10-19T00:00:00Z",
                )
                member = batch_state.derive_member(claim)
                self.assertEqual(member.state, expected)
                self.assertEqual((member.reason, member.message), (reason, message))
                # The DELETED event that follows leaves the member as it is.
                state = batch_state.BatchState("b1", [])
                state.upsert_claim(claim)
                self.assertEqual(state.mark_lost("b1-0"), member)

    def test_ready_without_sandbox_name_is_not_ready(self):
        member = batch_state.derive_member(
            _claim("b1-0", conditions=[{"type": "Ready", "status": "True"}])
        )
        self.assertEqual(member.state, MemberState.PENDING)

    def test_terminal_claim_ready_reasons(self):
        for reason in TERMINAL_CLAIM_READY_REASONS:
            with self.subTest(reason=reason):
                member = batch_state.derive_member(
                    _claim(
                        "b1-0",
                        conditions=[
                            {"type": "Ready", "status": "False", "reason": reason, "message": "x"}
                        ],
                    )
                )
                self.assertEqual(member.state, MemberState.FAILED)
                self.assertEqual(member.reason, reason)

    def test_template_not_found_is_not_terminal(self):
        # The controller requeues TemplateNotFound every minute rather than failing the claim.
        member = batch_state.derive_member(
            _claim(
                "b1-0",
                conditions=[
                    {"type": "Ready", "status": "False", "reason": "TemplateNotFound", "message": "x"}
                ],
            )
        )
        self.assertEqual(member.state, MemberState.PENDING)
        self.assertEqual(member.reason, "TemplateNotFound")
        self.assertEqual(member.message, "x")

    def test_warmpool_not_found_is_not_terminal(self):
        # The controller requeues WarmPoolNotFound every minute rather than failing the claim.
        member = batch_state.derive_member(
            _claim(
                "b1-0",
                conditions=[
                    {"type": "Ready", "status": "False", "reason": "WarmPoolNotFound", "message": "no pool"}
                ],
            )
        )
        self.assertEqual(member.state, MemberState.PENDING)
        self.assertEqual(member.reason, "WarmPoolNotFound")
        self.assertEqual(member.message, "no pool")


class TestLost(unittest.TestCase):

    def test_lost_member_clears_ready_and_endpoints(self):
        state = batch_state.BatchState("b1", [BatchGroup(warmpool="pool-a", size=1)])
        state.upsert_claim(
            _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "True", "reason": "Ready", "message": "x"}],
                sandbox={"name": "sbx-1", "podIPs": ["10.0.0.1"], "serviceFQDN": "sbx-1.svc"},
            )
        )
        lost = state.mark_lost("b1-0")
        self.assertEqual(lost.state, MemberState.LOST)
        self.assertEqual(lost.pod_ips, ())
        self.assertIsNone(lost.service_fqdn)
        self.assertIsNone(lost.reason)
        self.assertIsNone(lost.message)

    def test_failed_member_missing_from_a_relist_stays_failed(self):
        state = batch_state.BatchState("b1", [BatchGroup(warmpool="pool-a", size=1)])
        state.upsert_claim(
            _claim(
                "b1-0",
                conditions=[
                    {"type": "Ready", "status": "False", "reason": "ClaimExpired", "message": "x"}
                ],
            )
        )
        state.resync_from_list([])
        member = state.get_member("b1-0")
        self.assertEqual(member.state, MemberState.FAILED)
        self.assertEqual(member.reason, "ClaimExpired")


class TestBatchGroup(unittest.TestCase):

    def test_min_ready_defaults_to_size(self):
        self.assertEqual(BatchGroup(warmpool="p", size=3).min_ready, 3)

    def test_min_ready_greater_than_size_raises(self):
        with self.assertRaises(ValidationError):
            BatchGroup(warmpool="p", size=3, min_ready=4)

    def test_group_is_frozen(self):
        group = BatchGroup(warmpool="p", size=3)
        with self.assertRaises(ValidationError):
            group.min_ready = 1


class TestReconstructGroups(unittest.TestCase):

    def test_annotated_groups_come_back_with_size_and_min_ready(self):
        items = [
            _claim(
                "b1-0",
                annotations={
                    "agents.x-k8s.io/batch-group-size": "3",
                    "agents.x-k8s.io/batch-group-min-ready": "2",
                },
            ),
        ]
        groups = batch_state.reconstruct_groups("b1", items)
        self.assertEqual(groups, [BatchGroup(warmpool="pool-a", size=3, min_ready=2)])

    def test_groups_come_back_in_ordinal_order(self):
        items = [
            _claim("b1-2", warmpool="pool-a"),
            _claim("b1-1", warmpool="pool-z"),
            _claim("b1-0", warmpool="pool-z"),
        ]
        groups = batch_state.reconstruct_groups("b1", items)
        self.assertEqual([g.warmpool for g in groups], ["pool-z", "pool-a"])

    def test_claims_without_an_ordinal_sort_last_in_groups_and_members(self):
        items = [
            _claim("other", warmpool="pool-x"),
            _claim("b1-1", warmpool="pool-a"),
            _claim("b1-0", warmpool="pool-z"),
        ]
        groups = batch_state.reconstruct_groups("b1", items)
        self.assertEqual([g.warmpool for g in groups], ["pool-z", "pool-a", "pool-x"])
        state = batch_state.BatchState("b1", groups)
        state.seed_from_claims(items)
        self.assertEqual([m.claim_name for m in state.members()], ["b1-0", "b1-1", "other"])

    def test_unannotated_pool_becomes_size_zero(self):
        items = [_claim("b1-0", warmpool="pool-b")]
        groups = batch_state.reconstruct_groups("b1", items)
        self.assertEqual(groups, [BatchGroup(warmpool="pool-b", size=0, min_ready=0)])

    def test_conflicting_annotations_raise(self):
        items = [
            _claim(
                "b1-0",
                annotations={
                    "agents.x-k8s.io/batch-group-size": "3",
                    "agents.x-k8s.io/batch-group-min-ready": "2",
                },
            ),
            _claim(
                "b1-1",
                annotations={
                    "agents.x-k8s.io/batch-group-size": "5",
                    "agents.x-k8s.io/batch-group-min-ready": "2",
                },
            ),
        ]
        with self.assertRaises(BatchError):
            batch_state.reconstruct_groups("b1", items)

    def test_partial_annotations_raise(self):
        items = [_claim("b1-0", annotations={"agents.x-k8s.io/batch-group-size": "3"})]
        with self.assertRaises(BatchError):
            batch_state.reconstruct_groups("b1", items)

    def test_invalid_annotation_values_raise_batch_error(self):
        for size, min_ready in (("2", "5"), ("x", "1"), ("-1", "0")):
            with self.subTest(size=size, min_ready=min_ready):
                items = [
                    _claim(
                        "b1-0",
                        annotations={
                            "agents.x-k8s.io/batch-group-size": size,
                            "agents.x-k8s.io/batch-group-min-ready": min_ready,
                        },
                    ),
                ]
                with self.assertRaises(BatchError):
                    batch_state.reconstruct_groups("b1", items)


class TestSnapshots(unittest.TestCase):

    def test_members_pod_ips_cannot_be_mutated(self):
        state = batch_state.BatchState("b1", [BatchGroup(warmpool="pool-a", size=1)])
        state.upsert_claim(
            _claim(
                "b1-0",
                conditions=[{"type": "Ready", "status": "True"}],
                sandbox={"name": "sbx-1", "podIPs": ["10.0.0.1"]},
            )
        )
        member = state.members()[0]
        self.assertEqual(member.pod_ips, ("10.0.0.1",))
        with self.assertRaises(AttributeError):
            member.pod_ips.append("10.0.0.99")


class TestErrorSticky(unittest.TestCase):

    def test_first_error_wins(self):
        state = batch_state.BatchState("b1", [])
        state.note_error(ValueError("first"))
        state.note_error(ValueError("second"))
        self.assertEqual(str(state.error()), "first")


_READY = [{"type": "Ready", "status": "True"}]
_NOT_READY = [{"type": "Ready", "status": "False", "reason": "SandboxNotReady"}]
_TERMINAL = [{"type": "Ready", "status": "False", "reason": "InvalidConfiguration"}]


# Fill claims carry their group's annotations. BatchState only checks that they are there.
_FILL = {BATCH_GROUP_SIZE_ANNOTATION: "1", BATCH_GROUP_MIN_READY_ANNOTATION: "1"}


def _ready(name, warmpool="pool-a", fill=True):
    annotations = _FILL if fill else None
    return _claim(name, warmpool, conditions=_READY, sandbox={"name": f"sbx-{name}"}, annotations=annotations)


def _pending(name, warmpool="pool-a"):
    return _claim(name, warmpool, conditions=_NOT_READY, annotations=_FILL)


def _state(*groups, mode=None):
    state = batch_state.BatchState("b1", list(groups))
    if mode is not None:
        state.set_mode(mode)
    return state


def _drain_events(state):
    events = []
    while (event := state.pop_event()) is not None:
        events.append((event.type, event.member.claim_name))
    return events


def _quorum(state):
    """The quorum's member names, its error, or ``None`` while it is undecided."""
    result = state.quorum_result()
    if result is None:
        return None
    members, error = result
    return error if error is not None else [m.claim_name for m in members]


def _drain_groups(state):
    results = []
    while (result := state.pop_group_outcome()) is not None:
        results.append(result)
    return results


class TestStreamMode(unittest.TestCase):

    def test_each_fill_member_is_ready_once_and_non_fill_claims_emit_nothing(self):
        state = _state(BatchGroup(warmpool="pool-a", size=2), mode=batch_state.ConsumerMode.STREAM)
        state.upsert_claim(_ready("b1-0"))
        state.upsert_claim(_pending("b1-0"))
        state.upsert_claim(_ready("b1-0"))
        state.upsert_claim(_ready("b1-2", fill=False))
        state.upsert_claim(_ready("b1-1", warmpool="pool-z"))
        self.assertEqual(_drain_events(state), [(BatchEventType.MEMBER_READY, "b1-0")])

    def test_status_events(self):
        failed, lost = BatchEventType.MEMBER_FAILED, BatchEventType.MEMBER_LOST
        cases = {
            "terminal": ([lambda s: s.upsert_claim(_claim("b1-0", conditions=_TERMINAL, annotations=_FILL))], [(failed, "b1-0")]),
            "lost": ([lambda s: s.upsert_claim(_pending("b1-0")), lambda s: s.mark_lost("b1-0")], [(lost, "b1-0")]),
            "create failed": ([lambda s: s.record_create_failure("b1-0", "pool-a", "forbidden")], [(failed, "b1-0")]),
            "terminal then deleted": ([
                lambda s: s.upsert_claim(_claim("b1-0", conditions=_TERMINAL, annotations=_FILL)),
                lambda s: s.mark_lost("b1-0"),
            ], [(failed, "b1-0")]),
        }
        for name, (steps, want) in cases.items():
            with self.subTest(name):
                state = _state(BatchGroup(warmpool="pool-a", size=1), mode=batch_state.ConsumerMode.STREAM)
                for step in steps:
                    step(state)
                self.assertEqual(_drain_events(state), want)

    def test_ready_members_seen_before_a_mode_are_delivered_when_stream_mode_is_set(self):
        state = _state(BatchGroup(warmpool="pool-a", size=2))
        state.upsert_claim(_ready("b1-1"))
        state.upsert_claim(_ready("b1-0"))
        self.assertEqual(_drain_events(state), [])
        state.set_mode(batch_state.ConsumerMode.STREAM)
        self.assertEqual(
            _drain_events(state),
            [(BatchEventType.MEMBER_READY, "b1-1"), (BatchEventType.MEMBER_READY, "b1-0")],
        )


class TestGroupMode(unittest.TestCase):

    def test_success_yields_first_min_ready_by_ready_order_then_streams_the_rest_once(self):
        state = _state(BatchGroup(warmpool="pool-a", size=4, min_ready=2))
        for name in ("b1-3", "b1-0", "b1-1"):
            state.upsert_claim(_ready(name))
        state.set_mode(batch_state.ConsumerMode.GROUP)
        [result] = _drain_groups(state)
        self.assertIsNone(result.error)
        self.assertEqual([m.claim_name for m in result.members], ["b1-3", "b1-0"])
        self.assertEqual(_drain_events(state), [(BatchEventType.MEMBER_READY, "b1-1")])

        state.upsert_claim(_ready("b1-2"))
        self.assertEqual(_drain_events(state), [(BatchEventType.MEMBER_READY, "b1-2")])

        # A cohort member flapping, and events() joining later, hand out nothing again.
        state.upsert_claim(_pending("b1-3"))
        state.upsert_claim(_ready("b1-3"))
        state.set_mode(batch_state.ConsumerMode.STREAM)
        self.assertEqual(_drain_events(state), [])
        self.assertEqual(_drain_groups(state), [])

    def test_a_held_member_that_goes_not_ready_leaves_the_cohort_until_ready_again(self):
        for mode in (batch_state.ConsumerMode.GROUP, batch_state.ConsumerMode.QUORUM):
            with self.subTest(mode.name):
                state = _state(BatchGroup(warmpool="pool-a", size=3, min_ready=2), mode=mode)
                state.upsert_claim(_ready("b1-0"))
                state.upsert_claim(_pending("b1-0"))
                state.upsert_claim(_ready("b1-1"))
                self.assertEqual((_drain_groups(state), _quorum(state)), ([], None))
                state.upsert_claim(_ready("b1-2"))
                if mode == batch_state.ConsumerMode.GROUP:
                    [outcome] = _drain_groups(state)
                    names = [m.claim_name for m in outcome.members]
                else:
                    names = _quorum(state)
                self.assertEqual(names, ["b1-1", "b1-2"])

    def test_unreachable_group_is_finished_and_never_hands_out_members(self):
        state = _state(BatchGroup(warmpool="pool-a", size=3, min_ready=2), mode=batch_state.ConsumerMode.GROUP)
        # b1-2 is past the first min_ready ordinals, and is still held while the group has no verdict.
        state.upsert_claim(_ready("b1-2"))
        state.upsert_claim(_claim("b1-0", conditions=_TERMINAL, annotations=_FILL))
        self.assertEqual(_drain_groups(state), [])
        state.upsert_claim(_pending("b1-1"))
        state.mark_lost("b1-1")

        [result] = _drain_groups(state)
        self.assertIsInstance(result.error, QuorumUnreachableError)
        self.assertEqual(
            (result.error.size, result.error.min_ready, result.error.failed, result.error.lost),
            (3, 2, 1, 1),
        )
        self.assertEqual(result.members, [])
        self.assertTrue(state.group_failed("pool-a"))

        state.upsert_claim(_pending("b1-2"))
        state.upsert_claim(_ready("b1-2"))
        self.assertEqual(
            _drain_events(state),
            [(BatchEventType.MEMBER_FAILED, "b1-0"), (BatchEventType.MEMBER_LOST, "b1-1")],
        )
        self.assertEqual([m.claim_name for m in state.members()], ["b1-0", "b1-1", "b1-2"])

    def test_expire_fill_times_out_undecided_groups_only(self):
        state = _state(
            BatchGroup(warmpool="pool-a", size=1),
            BatchGroup(warmpool="pool-b", size=1),
            mode=batch_state.ConsumerMode.GROUP,
        )
        state.upsert_claim(_ready("b1-0", warmpool="pool-a"))
        state.upsert_claim(_pending("b1-1", warmpool="pool-b"))
        self.assertFalse(state.fill_settled())
        state.expire_fill()

        results = {r.warmpool: r for r in _drain_groups(state)}
        self.assertIsNone(results["pool-a"].error)
        # A caller catching either BatchError or TimeoutError catches it.
        self.assertIsInstance(results["pool-b"].error, BatchTimeoutError)
        self.assertIsInstance(results["pool-b"].error, BatchError)
        self.assertIsInstance(results["pool-b"].error, TimeoutError)
        self.assertTrue(state.fill_settled())
        self.assertTrue(state.groups_done())

    def test_mode_rules(self):
        stream, group, quorum = (
            batch_state.ConsumerMode.STREAM,
            batch_state.ConsumerMode.GROUP,
            batch_state.ConsumerMode.QUORUM,
        )
        allowed = {(stream, stream), (group, stream), (group, group), (quorum, stream)}
        for first in (stream, group, quorum):
            for then in (stream, group, quorum):
                with self.subTest(first=first.name, then=then.name):
                    state = _state(BatchGroup(warmpool="pool-a", size=1), mode=first)
                    if (first, then) in allowed:
                        state.set_mode(then)
                    else:
                        with self.assertRaises(BatchError):
                            state.set_mode(then)

    def test_group_with_min_ready_zero_yields_an_empty_success_at_once(self):
        state = _state(BatchGroup(warmpool="pool-a", size=2, min_ready=0), mode=batch_state.ConsumerMode.GROUP)
        [result] = _drain_groups(state)
        self.assertEqual((result.members, result.error), ([], None))


class TestQuorumMode(unittest.TestCase):

    def test_reached_only_when_every_group_has_min_ready_then_the_rest_stream_once(self):
        state = _state(
            BatchGroup(warmpool="pool-a", size=3, min_ready=2),
            BatchGroup(warmpool="pool-b", size=2, min_ready=1),
            BatchGroup(warmpool="pool-c", size=1, min_ready=0),
            mode=batch_state.ConsumerMode.QUORUM,
        )
        state.upsert_claim(_ready("b1-4", warmpool="pool-b"))
        state.upsert_claim(_ready("b1-1", warmpool="pool-a"))
        state.upsert_claim(_ready("b1-3", warmpool="pool-b"))
        self.assertIsNone(_quorum(state))
        self.assertEqual(_drain_events(state), [])

        state.upsert_claim(_ready("b1-0", warmpool="pool-a"))
        self.assertEqual(_quorum(state), ["b1-1", "b1-0", "b1-4"])
        self.assertEqual(_drain_events(state), [(BatchEventType.MEMBER_READY, "b1-3")])

        state.upsert_claim(_ready("b1-2", warmpool="pool-a"))
        # b1-0 was returned by the quorum, so going not Ready and back doesn't hand it out again.
        state.upsert_claim(_pending("b1-0", warmpool="pool-a"))
        state.upsert_claim(_ready("b1-0", warmpool="pool-a"))
        self.assertEqual(_drain_events(state), [(BatchEventType.MEMBER_READY, "b1-2")])

    def test_an_unreachable_group_fails_the_quorum_for_every_group(self):
        state = _state(
            BatchGroup(warmpool="pool-a", size=2),
            BatchGroup(warmpool="pool-b", size=1),
            mode=batch_state.ConsumerMode.QUORUM,
        )
        state.upsert_claim(_ready("b1-2", warmpool="pool-b"))
        state.upsert_claim(_claim("b1-0", warmpool="pool-a", conditions=_TERMINAL, annotations=_FILL))

        error = _quorum(state)
        self.assertIsInstance(error, QuorumUnreachableError)
        self.assertEqual(error.warmpool, "pool-a")
        self.assertTrue(state.group_failed("pool-a"))
        self.assertTrue(state.group_failed("pool-b"))

        # No member of any group is handed out after the quorum failed, neither pool-b's held b1-2
        # nor pool-a's b1-1 becoming Ready later. MEMBER_FAILED is a status event rather than a
        # hand-out, so it still reaches events(), as in group mode.
        state.upsert_claim(_ready("b1-1", warmpool="pool-a"))
        state.upsert_claim(_pending("b1-2", warmpool="pool-b"))
        state.upsert_claim(_ready("b1-2", warmpool="pool-b"))
        self.assertEqual(_drain_events(state), [(BatchEventType.MEMBER_FAILED, "b1-0")])

    def test_a_deadline_or_an_ended_wait_fails_only_an_undecided_quorum(self):
        deadlines = {
            "fill expired": lambda s: s.expire_fill(),
            "wait ended": lambda s: s.fail_quorum(BatchTimeoutError("wait_for_quorum() ended before the quorum was reached")),
        }
        for name, deadline in deadlines.items():
            with self.subTest(name):
                state = _state(BatchGroup(warmpool="pool-a", size=2), mode=batch_state.ConsumerMode.QUORUM)
                state.upsert_claim(_ready("b1-0"))
                deadline(state)
                self.assertIsInstance(_quorum(state), BatchTimeoutError)
                self.assertTrue(state.group_failed("pool-a"))
                state.upsert_claim(_ready("b1-1"))
                self.assertEqual(_drain_events(state), [])

                state = _state(BatchGroup(warmpool="pool-a", size=1), mode=batch_state.ConsumerMode.QUORUM)
                state.upsert_claim(_ready("b1-0"))
                deadline(state)
                self.assertEqual(_quorum(state), ["b1-0"])
                self.assertFalse(state.group_failed("pool-a"))

    def test_members_ready_before_the_mode_is_set_count(self):
        for expired in (False, True):
            with self.subTest(fill_expired=expired):
                state = _state(BatchGroup(warmpool="pool-a", size=3, min_ready=2))
                state.upsert_claim(_ready("b1-1"))
                state.upsert_claim(_ready("b1-0"))
                if expired:
                    state.expire_fill()
                state.set_mode(batch_state.ConsumerMode.QUORUM)
                self.assertEqual(_quorum(state), ["b1-1", "b1-0"])

        state = _state(BatchGroup(warmpool="pool-a", size=3, min_ready=2))
        state.upsert_claim(_ready("b1-0"))
        state.expire_fill()
        state.set_mode(batch_state.ConsumerMode.QUORUM)
        self.assertIsInstance(_quorum(state), BatchTimeoutError)


class TestSettle(unittest.TestCase):

    def test_fill_settles_once_every_member_is_ready_failed_or_never_created(self):
        state = _state(BatchGroup(warmpool="pool-a", size=3), mode=batch_state.ConsumerMode.STREAM)
        state.upsert_claim(_ready("b1-0"))
        state.record_create_failure("b1-1", "pool-a", "forbidden")
        self.assertFalse(state.fill_settled())
        state.record_skipped_create()
        self.assertTrue(state.fill_settled())
        self.assertFalse(state.events_done())
        _drain_events(state)
        self.assertTrue(state.events_done())


class TestReattachedFill(unittest.TestCase):

    def test_missing_fill_claims_count_as_unable(self):
        state = _state(BatchGroup(warmpool="pool-a", size=3, min_ready=2))
        state.seed_from_claims([_ready("b1-0")])
        state.count_missing_fill_as_unable()
        self.assertTrue(state.fill_settled())
        state.set_mode(batch_state.ConsumerMode.GROUP)
        [result] = _drain_groups(state)
        self.assertIsInstance(result.error, QuorumUnreachableError)

    def test_a_group_with_no_claims_left_still_counts_the_other_groups_claims_as_fill(self):
        # Group pool-a had b1-0 and b1-1, both gone, so the new handle only knows pool-b's group.
        group_b = {BATCH_GROUP_SIZE_ANNOTATION: "2", BATCH_GROUP_MIN_READY_ANNOTATION: "2"}
        claims = [
            _claim(name, "pool-b", conditions=_READY, sandbox={"name": f"sbx-{name}"}, annotations=group_b)
            for name in ("b1-2", "b1-3")
        ]
        state = batch_state.BatchState("b1", batch_state.reconstruct_groups("b1", claims))
        state.seed_from_claims(claims)
        state.count_missing_fill_as_unable()
        state.set_mode(batch_state.ConsumerMode.GROUP)
        [result] = _drain_groups(state)
        self.assertIsNone(result.error)
        self.assertEqual([m.claim_name for m in result.members], ["b1-2", "b1-3"])


class TestRelease(unittest.TestCase):

    def test_released_members_leave_members_count_as_unable_and_ignore_later_events(self):
        state = _state(BatchGroup(warmpool="pool-a", size=4, min_ready=2), mode=batch_state.ConsumerMode.GROUP)
        state.upsert_claim(_pending("b1-0"))
        state.upsert_claim(_claim("b1-1", conditions=_TERMINAL, annotations=_FILL))
        state.upsert_claim(_pending("b1-2"))
        state.upsert_claim(_ready("b1-9", fill=False))
        _drain_events(state)
        for name in ("b1-0", "b1-1", "b1-9"):
            state.release_member(name)
        self.assertEqual([m.claim_name for m in state.members()], ["b1-2"])
        # b1-2 and b1-3 can still reach min_ready. A non-fill member doesn't count against the group.
        self.assertEqual(_drain_groups(state), [])

        state.release_member("b1-2")
        [result] = _drain_groups(state)
        self.assertEqual(
            (result.error.failed, result.error.lost, result.error.released), (1, 0, 2)
        )
        # The claims' later updates and deletes change nothing and emit no MEMBER_LOST.
        state.upsert_claim(_ready("b1-0"))
        state.mark_lost("b1-2")
        self.assertEqual(state.members(), [])
        self.assertEqual(_drain_events(state), [])

    def test_release_member_deletes_until_noted_and_raises_for_unknown_claims(self):
        state = _state(BatchGroup(warmpool="pool-a", size=3))
        state.upsert_claim(_pending("b1-0"))
        state.upsert_claim(_pending("b1-1"))
        state.mark_lost("b1-1")
        state.record_create_failure("b1-2", "pool-a", "conflict")
        self.assertTrue(state.release_member("b1-0"))
        self.assertTrue(state.release_member("b1-0"))  # Its delete hasn't succeeded yet.
        state.note_deleted("b1-0")
        self.assertFalse(state.release_member("b1-0"))
        # A lost member's claim is gone, and a failed create's name may be another client's claim.
        self.assertFalse(state.release_member("b1-1"))
        self.assertFalse(state.release_member("b1-2"))
        with self.assertRaises(ValueError):
            state.release_member("b1-9")

    def test_a_ready_event_queued_before_release_is_not_handed_out(self):
        state = _state(BatchGroup(warmpool="pool-a", size=2), mode=batch_state.ConsumerMode.STREAM)
        state.upsert_claim(_ready("b1-0"))
        state.upsert_claim(_ready("b1-1"))
        state.release_member("b1-0")
        self.assertEqual(_drain_events(state), [(BatchEventType.MEMBER_READY, "b1-1")])

    def test_release_not_ready_keeps_ready_handed_out_and_non_fill_members_and_ends_the_fill(self):
        state = _state(BatchGroup(warmpool="pool-a", size=5), mode=batch_state.ConsumerMode.STREAM)
        state.upsert_claim(_ready("b1-0"))
        state.upsert_claim(_ready("b1-1"))
        _drain_events(state)
        state.upsert_claim(_pending("b1-1"))  # Handed out, then not Ready.
        state.upsert_claim(_pending("b1-2"))
        state.upsert_claim(_claim("b1-3", conditions=_TERMINAL, annotations=_FILL))
        state.upsert_claim(_claim("b1-9", conditions=_NOT_READY))  # No group annotations.
        _drain_events(state)

        self.assertEqual(sorted(state.release_not_ready()), ["b1-2", "b1-3"])
        self.assertEqual([m.claim_name for m in state.members()], ["b1-0", "b1-1", "b1-9"])
        # A released member that turns Ready, and a fill claim created before the fill ended, are
        # never handed out.
        state.upsert_claim(_ready("b1-2"))
        state.upsert_claim(_ready("b1-4"))
        self.assertEqual(_drain_events(state), [])
        self.assertTrue(state.fill_creates_released("pool-a"))

    def test_release_group_releases_its_pool_and_any_fill_claim_that_lands_later(self):
        state = _state(
            BatchGroup(warmpool="pool-a", size=3), BatchGroup(warmpool="pool-b", size=1),
            mode=batch_state.ConsumerMode.GROUP,
        )
        state.upsert_claim(_ready("b1-0"))
        state.upsert_claim(_ready("b1-3", "pool-b"))
        # acquire() claims from the pool go too, Ready or still in flight, but aren't counted.
        state.upsert_claim(_ready("b1-9", fill=False))
        in_flight = state.start_acquire("pool-a")
        self.assertEqual(state.release_group("pool-a"), ["b1-0", "b1-9"])
        with self.assertRaisesRegex(BatchError, "released before it was Ready"):
            state.acquired_member(in_flight)
        # Its create may land after the label delete, so acquire() deletes it by name.
        self.assertTrue(state.drop_acquired(in_flight, created=True))
        state.upsert_claim(_ready("b1-1"))
        state.record_create_failure("b1-2", "pool-a", "forbidden")

        self.assertEqual([m.claim_name for m in state.members()], ["b1-3"])
        self.assertFalse(state.fill_creates_released("pool-b"))
        outcomes = {r.warmpool: r for r in _drain_groups(state)}
        self.assertEqual(outcomes["pool-a"].error.released, 1)  # Decided at the first release.
        self.assertEqual([m.claim_name for m in outcomes["pool-b"].members], ["b1-3"])
        self.assertTrue(state.fill_settled())


class TestOrdinals(unittest.TestCase):

    def test_claim_names_continue_past_the_fill_listed_claims_and_a_recorded_ordinal(self):
        state = _state(BatchGroup(warmpool="pool-a", size=2))
        self.assertEqual(state.start_acquire("pool-a"), "b1-2")
        state.upsert_claim(_ready("b1-5", fill=False))
        self.assertEqual(state.start_acquire("pool-a"), "b1-6")
        for recorded, want in ((9, ("b1-9", 10)), (1, ("b1-2", 3))):
            reattached = batch_state.BatchState("b1", [BatchGroup(warmpool="pool-a", size=2)], recorded)
            self.assertEqual((reattached.start_acquire("pool-a"), reattached.next_ordinal), want)


class TestAcquired(unittest.TestCase):

    def test_outcomes_drop_and_deletes(self):
        cases = {
            "ready": (_ready("b1-1", fill=False), "ready", ["b1-1"]),
            "pending": (_claim("b1-1", conditions=_NOT_READY), None, ["b1-1"]),
            "terminal": (_claim("b1-1", conditions=_TERMINAL), MemberState.FAILED, ["b1-1"]),
            "lost": (_claim("b1-1", conditions=_NOT_READY), MemberState.LOST, []),
            "not seen yet": (None, None, ["b1-1"]),
        }
        for name, (claim, outcome, deletes) in cases.items():
            with self.subTest(name):
                state = _state(BatchGroup(warmpool="pool-a", size=1, min_ready=1), mode=batch_state.ConsumerMode.GROUP)
                if claim is not None:
                    state.upsert_claim(claim)
                if outcome is MemberState.LOST:
                    state.mark_lost("b1-1")
                if outcome in (MemberState.FAILED, MemberState.LOST):
                    with self.assertRaises(TerminalMemberError) as raised:
                        state.acquired_member("b1-1")
                    self.assertEqual(raised.exception.member.state, outcome)
                elif outcome == "ready":
                    self.assertEqual(state.acquired_member("b1-1").state, MemberState.READY)
                else:
                    self.assertIsNone(state.acquired_member("b1-1"))
                # An acquired claim in a fill pool is never part of the fill.
                self.assertEqual((_drain_events(state), _drain_groups(state)), ([], []))

                self.assertEqual(state.drop_acquired("b1-1", created=True), bool(deletes))
                state.upsert_claim(_ready("b1-1", fill=False))
                self.assertEqual(state.members(), [])
                # A failed delete is retried by release_not_ready().
                self.assertEqual(state.release_not_ready(), deletes)

        state = _state(BatchGroup(warmpool="pool-a", size=1))
        self.assertFalse(state.drop_acquired("b1-1", created=False))
        state.upsert_claim(_ready("b1-1", fill=False))
        self.assertEqual(state.members(), [])

    def test_has_pool_covers_groups_members_and_acquires_in_flight(self):
        state = _state(BatchGroup(warmpool="pool-a", size=1))
        state.upsert_claim(_ready("b1-5", "pool-b", fill=False))
        state.start_acquire("pool-c")
        self.assertEqual([state.has_pool(p) for p in ("pool-a", "pool-b", "pool-c", "pool-d")], [True, True, True, False])

    def test_releasing_a_pending_acquired_member_ends_its_wait(self):
        state = _state(BatchGroup(warmpool="pool-a", size=1))
        state.upsert_claim(_claim("b1-1", conditions=_NOT_READY))
        self.assertTrue(state.release_member("b1-1"))
        with self.assertRaisesRegex(BatchError, "released before it was Ready"):
            state.acquired_member("b1-1")
        # release_member owns the delete, so acquire's cleanup doesn't add another.
        state.note_deleted("b1-1")
        self.assertFalse(state.drop_acquired("b1-1", created=True))
        self.assertEqual(state.release_not_ready(), [])


if __name__ == "__main__":
    unittest.main()
