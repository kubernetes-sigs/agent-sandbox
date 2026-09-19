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

from collections.abc import Sequence
from operator import itemgetter

from .constants import (
    BATCH_GROUP_MIN_READY_ANNOTATION,
    BATCH_GROUP_SIZE_ANNOTATION,
    TERMINAL_CLAIM_READY_REASONS,
)
from .exceptions import BatchError
from .models import BatchGroup, Member


def parse_ordinal(batch_id: str, claim_name: str) -> int | None:
    """Extracts the ordinal from a ``<batch-id>-<ordinal>`` claim name."""
    prefix = f"{batch_id}-"
    if not claim_name.startswith(prefix):
        return None
    suffix = claim_name[len(prefix):]
    if not suffix.isdigit():
        return None
    return int(suffix)


def reconstruct_groups(claim_objs: Sequence[dict]) -> list[BatchGroup]:
    """Rebuilds each pool's ``BatchGroup`` from its claims' group annotations.

    A pool with no group annotations on any of its claims becomes ``BatchGroup(size=0, min_ready=0)``,
    and claims that are detected with different or invalid annotation values in the same pool raise ``BatchError``.
    """
    by_pool: dict[str, tuple[str, str]] = {}
    pools_seen: set[str] = set()
    for claim_obj in claim_objs:
        spec = claim_obj.get("spec") or {}
        warmpool = (spec.get("warmPoolRef") or {}).get("name", "")
        pools_seen.add(warmpool)
        annotations = (claim_obj.get("metadata") or {}).get("annotations") or {}
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
    for pool in sorted(pools_seen):
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


def derive_member(claim_obj: dict) -> Member:
    """Builds a ``Member`` from a raw SandboxClaim object."""
    metadata = claim_obj.get("metadata") or {}
    spec = claim_obj.get("spec") or {}
    status = claim_obj.get("status") or {}
    conditions = status.get("conditions") or []

    ready = False
    terminal = False
    reason: str | None = None
    message: str | None = None
    ready_condition = next((c for c in conditions if c.get("type") == "Ready"), None)
    if ready_condition is not None:
        reason = ready_condition.get("reason")
        message = ready_condition.get("message")
        if ready_condition.get("status") == "True":
            ready = True
        # We do not treat WarmPoolNotFound or TemplateNotFound as terminal, because the controller
        # requeues those every minute rather than failing the claim.
        elif ready_condition.get("status") == "False" and reason in TERMINAL_CLAIM_READY_REASONS:
            terminal = True

    sandbox_status = status.get("sandbox") or {}
    sandbox_name = sandbox_status.get("name") or None
    ready = ready and sandbox_name is not None

    return Member(
        claim_name=metadata.get("name", ""),
        sandbox_name=sandbox_name,
        warmpool=(spec.get("warmPoolRef") or {}).get("name", ""),
        pod_ips=tuple(sandbox_status.get("podIPs") or []),
        service_fqdn=sandbox_status.get("serviceFQDN"),
        ready=ready,
        terminal=terminal,
        lost=False,
        reason=reason,
        message=message,
    )


class BatchState:
    """The internal state of a batch handle, containing its members and metadata."""

    def __init__(self, batch_id: str, groups: Sequence[BatchGroup]) -> None:
        self.batch_id = batch_id
        self.groups: list[BatchGroup] = list(groups)
        self.size = sum(g.size for g in self.groups)

        self._members: dict[str, Member] = {}
        self._ordinals: dict[str, int] = {}
        self._error: Exception | None = None

    def seed_from_claims(self, claim_objs: Sequence[dict]) -> None:
        """Reconstructs state from the batch's existing claims."""
        for claim_obj in claim_objs:
            self.upsert_claim(claim_obj)

    def upsert_claim(self, claim_obj: dict) -> Member | None:
        """Derives a ``Member`` from a claim and inserts or updates it."""
        metadata = claim_obj.get("metadata") or {}
        claim_name = metadata.get("name", "")
        ordinal = parse_ordinal(self.batch_id, claim_name)
        if ordinal is not None:
            self._ordinals[claim_name] = ordinal
        member = derive_member(claim_obj)
        self._members[claim_name] = member
        return member

    def mark_lost(self, claim_name: str) -> Member | None:
        """Handles a lost claim (watch DELETED or missing on re-list)."""
        existing = self._members.get(claim_name)
        if existing is None:
            # Deleted before this handle ever saw an ADDED/MODIFIED event for it.
            return None
        lost_member = existing.model_copy(
            update={"lost": True, "ready": False, "pod_ips": (), "service_fqdn": None}
        )
        self._members[claim_name] = lost_member
        return lost_member

    def resync_from_list(self, claim_objs: Sequence[dict]) -> None:
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
            (self._ordinals.get(name, 0), member)
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
