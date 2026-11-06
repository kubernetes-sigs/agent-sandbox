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

"""Validation, Lease, and retry helpers shared by both sync and async batch handles."""

import hashlib
import json
import logging
import math
import os
import random
import re
import secrets
import socket
import string
import time
import uuid
from collections.abc import Callable, Sequence
from datetime import UTC, datetime, timedelta
from enum import Enum
from itertools import accumulate
from typing import TYPE_CHECKING, Any, NamedTuple, overload

from .constants import (
    BATCH_CREATE_RPS_ANNOTATION,
    BATCH_GROUP_LABEL,
    BATCH_ID_LABEL,
    BATCH_LEASE_DURATION_ANNOTATION,
    BATCH_LEASE_UID_LABEL,
    BATCH_NEXT_ORDINAL_ANNOTATION,
    BATCH_QUORUM_TIMEOUT_ANNOTATION,
    BATCH_WORK_BUDGET_ANNOTATION,
    CREATED_BY_LABEL,
)
from .exceptions import BatchError
from .models import BatchGroup
from .pod_metadata import validate_labels

if TYPE_CHECKING:
    from kubernetes.client import V1Lease, V1LeaseSpec, V1ObjectMeta
    from kubernetes_asyncio.client import (
        V1Lease as AsyncV1Lease,
        V1LeaseSpec as AsyncV1LeaseSpec,
        V1ObjectMeta as AsyncV1ObjectMeta,
    )

logger = logging.getLogger(__name__)

CLOCK_SKEW_MARGIN = 5
BATCH_DEFAULT_LEASE_DURATION_SECONDS = 60
BATCH_BACKOFF_BASE_SECONDS = 0.5
BATCH_BACKOFF_MAX_SECONDS = 30.0
BATCH_REQUEST_TIMEOUT_SECONDS = 30
# Set above the apiserver's 60s request timeout, its 504 response can end a long list or
# deletecollection first.
BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS = 90

_BATCH_ID_RE = re.compile(r"^[a-z]([-a-z0-9]*[a-z0-9])?$")
BATCH_ID_MAX_LENGTH = 52
_BATCH_ID_ALPHABET = string.ascii_lowercase + string.digits
_GENERATED_BATCH_ID_RANDOM_LENGTH = 11

BATCH_DEFAULT_CREATE_RPS = 50.0
BATCH_DEFAULT_MAX_IN_FLIGHT = 20
# A batch runs up to max_in_flight requests at once and needs two more API connections in the shared
# batch pool: one for the watch stream and one for Lease renewals. If the pool is too small, the
# async client (aiohttp) makes a renewal wait for a free connection, and a long enough wait lets the
# Lease expire. A full sync pool (urllib3) doesn't make requests wait; it opens one-off connections
# that close after a single request, so each pays a fresh handshake.
BATCH_POOL_HEADROOM = 2
# How long acquire() waits before re-reading a warm pool's Ready count once callers spend its credit,
# so a burst of acquires on one pool sends about one GET a second rather than one per acquire.
BATCH_WARM_CREDIT_REFRESH_SECONDS = 1.0
# Minimum cooldown after a 429 create response before warm-pool credit skips create_rps pacing
# again, so a brief apiserver burst backs off immediately without throttling later steps on a
# long-lived batch.
BATCH_WARM_CREDIT_THROTTLE_SECONDS = 5.0
BATCH_DEFAULT_QUORUM_TIMEOUT_SECONDS = 600
BATCH_DEFAULT_WORK_BUDGET_SECONDS = 3600
# Added to each claim's shutdownTime
BATCH_SHUTDOWN_MARGIN_SECONDS = 600
BATCH_CREATE_ATTEMPTS = 3
BATCH_DELETE_ATTEMPTS = 3
# Extra wait past BATCH_REQUEST_TIMEOUT_SECONDS when stopping the creators, so a create that just
# timed out has time to return and its worker to exit.
BATCH_CREATOR_STOP_GRACE_SECONDS = 1
BATCH_RELEASE_MAX_IDLE_ROUNDS = 3


def api_status_message(body: str | bytes | None, reason: str | None) -> str:
    """Returns the ``message`` of the Status in an apiserver error response body, or ``reason``
    when the body has none.
    """
    if body:
        try:
            message = json.loads(body).get("message")
        except (ValueError, AttributeError):
            message = None
        if message:
            return str(message)
    return reason or ""


def validate_batch_id(batch_id: str) -> None:
    """Validates a caller-supplied batch id.

    Must be a DNS-1123 label that starts with a letter and is at most 52 characters,
    so ``<id>-<ordinal>`` stays within a 63-character DNS label limit.
    """
    if (
        not isinstance(batch_id, str)
        or len(batch_id) > BATCH_ID_MAX_LENGTH
        # fullmatch, since match lets $ match before a trailing newline.
        or not _BATCH_ID_RE.fullmatch(batch_id)
    ):
        raise ValueError(
            f"batch_id must be a DNS-1123 label starting with a letter, "
            f"at most {BATCH_ID_MAX_LENGTH} characters: {batch_id!r}"
        )


def generate_holder_identity() -> str:
    """Generates a unique Lease holder identity for the current Batch handle user."""
    return f"{socket.gethostname()}_{os.getpid()}_{uuid.uuid4().hex[:8]}"


def _require_duration_exceeds_skew_margin(value: int) -> int:
    if value <= CLOCK_SKEW_MARGIN:
        raise ValueError(f"Duration must be greater than clock skew margin ({CLOCK_SKEW_MARGIN}s)")
    return value


def validate_lease_duration_value(value: object) -> int:
    """Validates that caller-supplied ``leaseDurationSeconds`` value is an int
    and is greater than ``CLOCK_SKEW_MARGIN``.
    """
    if type(value) is not int:
        raise ValueError(f"Duration must be an int, got {type(value).__name__}")
    return _require_duration_exceeds_skew_margin(value)


def parse_lease_duration_annotation(value: str | None) -> int:
    """Parses ``BATCH_LEASE_DURATION_ANNOTATION`` upon ``get_batch`` to
    validate the Lease annotation is valid. If missing, falls back to the default.
    """
    if value is None:
        return BATCH_DEFAULT_LEASE_DURATION_SECONDS
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        raise BatchError(
            f"batch lease duration annotation {value!r} is not a valid integer"
        ) from None
    try:
        return _require_duration_exceeds_skew_margin(parsed)
    except ValueError as e:
        raise BatchError(f"batch lease duration annotation: {e}") from None


def is_lease_stale(
    renew_time: datetime,
    lease_duration_seconds: int,
    now: datetime,
    skew_margin: int = CLOCK_SKEW_MARGIN,
) -> bool:
    """Computes Lease staleness to prevent ``get_batch`` operating on an inactive batch.
    Staleness is computed via ``now > renew_time + lease_duration_seconds - skew_margin``.

    Subtracting the margin makes ``get_batch`` stop adopting a Lease early, which is the safe
    side for attaching. A check that deletes a batch on staleness has to add its margin instead,
    so it never acts before the holder has given up.
    """
    return now > renew_time + timedelta(seconds=lease_duration_seconds - skew_margin)


def lease_owner_reference(lease: "V1Lease | AsyncV1Lease") -> dict[str, str]:
    """Returns the ownerReference that points a batch claim at its batch Lease, so the garbage
    collector deletes the claim once the Lease is gone.

    It must carry the UID of the Lease as the apiserver returned it, since a reference to a UID that
    doesn't exist counts as a missing owner. It sets neither ``controller`` nor
    ``blockOwnerDeletion``, so creating claims needs no permission on the Lease beyond what the
    batch already has.

    Raises:
        BatchError: If the Lease has no name or UID.
    """
    metadata = lease.metadata
    # The apiserver sets both on every Lease it returns, so only a malformed response lacks one.
    if metadata is None or not metadata.name or not metadata.uid:
        raise BatchError("batch Lease has no name or UID to own its claims")
    return {
        "apiVersion": "coordination.k8s.io/v1",
        "kind": "Lease",
        "name": metadata.name,
        "uid": metadata.uid,
    }


@overload
def lease_parts(lease: "V1Lease") -> "tuple[V1ObjectMeta, V1LeaseSpec]": ...


@overload
def lease_parts(lease: "AsyncV1Lease") -> "tuple[AsyncV1ObjectMeta, AsyncV1LeaseSpec]": ...


def lease_parts(
    lease: "V1Lease | AsyncV1Lease",
) -> "tuple[V1ObjectMeta | AsyncV1ObjectMeta, V1LeaseSpec | AsyncV1LeaseSpec]":
    """Returns a batch Lease's ``(metadata, spec)``.

    Raises:
        BatchError: If the Lease has no ``metadata`` or ``spec``.
    """
    if lease.metadata is None or lease.spec is None:
        raise BatchError("batch Lease has no metadata or spec")
    return lease.metadata, lease.spec


def lease_now() -> datetime:
    """Returns the current time for a Lease's ``renewTime`` or ``acquireTime``.

    Both are MicroTime fields, which the apiserver only accepts with six fractional digits, but the
    client serializes with ``isoformat()``, which drops the fraction when the microsecond is 0. So a
    zero microsecond is bumped to 1.
    """
    now = datetime.now(UTC)
    return now if now.microsecond else now.replace(microsecond=1)


def is_retryable_status(status: int | None) -> bool:
    """Whether an apiserver response status is likely transient: no status (a transport-level
    failure), 429, or 5xx.
    """
    return status is None or status == 429 or 500 <= status < 600


def backoff_delay(
    attempt: int, base: float, cap: float, rand: Callable[[], float] = random.random
) -> float:
    """Returns how long to wait before retry number ``attempt`` (1 for the first retry).

    The wait doubles with each attempt, from ``base`` up to ``cap``, and the returned delay is
    jittered to a random value between wait/2 and wait.
    """
    # Cap the exponent early to avoid computing massive 2**n values
    doublings = min(attempt - 1, math.ceil(math.log2(cap / base)))
    wait = min(cap, base * 2 ** doublings)
    return float(wait / 2 + rand() * wait / 2)


def generate_batch_id() -> str:
    """Generates a batch id: ``"b"`` followed by 11 random lowercase letters and digits."""
    return "b" + "".join(
        secrets.choice(_BATCH_ID_ALPHABET) for _ in range(_GENERATED_BATCH_ID_RANDOM_LENGTH)
    )


class ClaimBatchArgs(NamedTuple):
    """``claim_batch`` arguments after validation, with defaults filled in."""
    groups: list[BatchGroup]
    labels: dict[str, str]
    batch_id: str
    create_rps: float
    max_in_flight: int
    work_budget: int
    quorum_timeout: int
    lease_duration: int


def _positive_int(name: str, value: object, default: int) -> int:
    if value is None:
        return default
    if type(value) is not int or value <= 0:
        raise ValueError(f"{name} must be a positive int, got {value!r}")
    return value


def positive_number(name: str, value: object, default: float) -> float:
    """Validates an optional positive int or float argument, filling in ``default`` for ``None``.

    Raises:
        ValueError: If ``value`` isn't a positive finite number.
    """
    if value is None:
        return float(default)
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not 0 < value < math.inf:
        raise ValueError(f"{name} must be a positive finite number, got {value!r}")
    return float(value)


def check_pool_size(pool_size: int | None, max_in_flight: int, *, explicit: bool) -> int:
    """Checks that the Kubernetes API connection pool fits ``max_in_flight`` creates and
    ``BATCH_POOL_HEADROOM`` more, clamping a default ``max_in_flight`` down when at least one
    create slot remains. ``None`` means the pool is unbounded.

    Raises:
        ValueError: If an explicit ``max_in_flight`` doesn't fit, or the pool holds no create
            slots.
    """
    if pool_size is None:
        return max_in_flight
    available = pool_size - BATCH_POOL_HEADROOM
    if max_in_flight <= available:
        return max_in_flight
    if not explicit:
        if available >= 1:
            logger.info(
                "Clamping default max_in_flight from %d to %d to fit Kubernetes API connection "
                "pool size %d (pass pool_size=%d or more for the full default)",
                max_in_flight,
                available,
                pool_size,
                max_in_flight + BATCH_POOL_HEADROOM,
            )
            return available
        max_in_flight = 1
    needed = max_in_flight + BATCH_POOL_HEADROOM
    remedy = "Lower max_in_flight, or construct" if max_in_flight > 1 else "Construct"
    raise ValueError(
        f"max_in_flight={max_in_flight} needs a Kubernetes API connection pool of at least "
        f"{needed}, but the client's pool holds {pool_size}. {remedy} "
        f"the client with pool_size={needed} or more (for an injected api_client, set "
        f"connection_pool_maxsize on its configuration)"
    )


def validate_claim_batch_args(
    groups: Sequence[BatchGroup],
    labels: dict[str, str] | None,
    batch_id: str | None,
    create_rps: float | None,
    max_in_flight: int | None,
    work_budget: int | None,
    quorum_timeout: int | None,
    lease_duration: int | None,
) -> ClaimBatchArgs:
    """Validates ``claim_batch`` arguments and fills in defaults.

    Raises:
        ValueError: If any argument is invalid.
    """
    groups = list(groups)
    if not groups:
        raise ValueError("groups must not be empty")
    pools: set[str] = set()
    for group in groups:
        if not isinstance(group, BatchGroup):
            raise ValueError(f"groups must contain BatchGroup items, got {type(group).__name__}")
        if group.warmpool in pools:
            raise ValueError(f"warm pool '{group.warmpool}' appears in more than one group")
        pools.add(group.warmpool)

    labels = dict(labels or {})
    for reserved in (BATCH_ID_LABEL, BATCH_LEASE_UID_LABEL, BATCH_GROUP_LABEL):
        if reserved in labels:
            raise ValueError(f"labels must not set {reserved}; claim_batch sets it")
    if labels:
        validate_labels(labels)

    if batch_id is None:
        batch_id = generate_batch_id()
    else:
        validate_batch_id(batch_id)

    return ClaimBatchArgs(
        groups=groups,
        labels=labels,
        batch_id=batch_id,
        create_rps=positive_number("create_rps", create_rps, BATCH_DEFAULT_CREATE_RPS),
        max_in_flight=_positive_int("max_in_flight", max_in_flight, BATCH_DEFAULT_MAX_IN_FLIGHT),
        work_budget=_positive_int("work_budget", work_budget, BATCH_DEFAULT_WORK_BUDGET_SECONDS),
        quorum_timeout=_positive_int(
            "quorum_timeout", quorum_timeout, BATCH_DEFAULT_QUORUM_TIMEOUT_SECONDS
        ),
        lease_duration=(
            BATCH_DEFAULT_LEASE_DURATION_SECONDS
            if lease_duration is None
            else validate_lease_duration_value(lease_duration)
        ),
    )


def group_label_value(warmpool: str) -> str:
    """Returns the ``BATCH_GROUP_LABEL`` value for a pool, a hash of its name that fits a label value.

    64 bits of SHA-256, so two pools of one batch practically never share a value, which would make
    ``release_group`` delete the other pool's claims too.
    """
    return hashlib.sha256(warmpool.encode()).hexdigest()[:16]


def batch_claim_label_selector(batch_id: str, lease_uid: str | None = None) -> str:
    """Returns the label selector for a batch's claims, scoped to ``lease_uid`` when given."""
    if lease_uid is None:
        return f"{BATCH_ID_LABEL}={batch_id}"
    return f"{BATCH_ID_LABEL}={batch_id},{BATCH_LEASE_UID_LABEL}={lease_uid}"


def group_label_selector(batch_id: str, warmpool: str, lease_uid: str | None = None) -> str:
    """Returns the label selector for one group's claims, scoped to ``lease_uid`` when given."""
    base = f"{BATCH_ID_LABEL}={batch_id},{BATCH_GROUP_LABEL}={group_label_value(warmpool)}"
    if lease_uid is None:
        return base
    return f"{base},{BATCH_LEASE_UID_LABEL}={lease_uid}"


def fill_create_plan(batch_id: str, groups: Sequence[BatchGroup]) -> list[tuple[str, BatchGroup]]:
    """Returns the fill's claim names and their groups, in the order the claims are created.

    Each group's claims get consecutive ordinals, in group order, so a re-attached handle can
    restore the group order from them. The create order is different. Each group's first
    ``min_ready`` claims go first, so quorum waits only on their pacing, and the rest follow. Within
    each part the groups go one after another in group order, so the first groups are complete and
    can be consumed while later groups are still being created.
    """
    first_ordinals = list(accumulate((group.size for group in groups), initial=0))
    slots = sorted(
        (k >= (group.min_ready or 0), index, k)
        for index, group in enumerate(groups)
        for k in range(group.size)
    )
    return [(f"{batch_id}-{first_ordinals[index] + k}", groups[index]) for _, index, k in slots]


class CreateOutcome(Enum):
    """What a failed claim create means."""
    SUCCESS = "success"
    RETRY = "retry"
    FAIL = "fail"


def create_error_outcome(status: int | None, attempt: int) -> CreateOutcome:
    """Decides what a failed claim create means, given its status and attempt number.

    Receiving a 409 on the first attempt means a name collision, and we assume that on a later attempt,
    it almost always means an earlier attempt did create the claim but its response was lost (e.g. the
    request timed out or the connection dropped after the apiserver stored the claim).
    """
    if status == 409:
        return CreateOutcome.FAIL if attempt == 1 else CreateOutcome.SUCCESS
    if is_retryable_status(status) and attempt < BATCH_CREATE_ATTEMPTS:
        return CreateOutcome.RETRY
    return CreateOutcome.FAIL


def retry_after_seconds(error: BaseException | None) -> float | None:
    """Returns the error's ``Retry-After`` seconds, capped at ``BATCH_BACKOFF_MAX_SECONDS``, or
    ``None`` if it has none.
    """
    headers = getattr(error, "headers", None)
    retry_after = headers.get("Retry-After") if headers else None
    seconds = retry_after.strip() if isinstance(retry_after, str) else ""
    # The HTTP-date form of Retry-After is ignored in favor of the backoff. isdigit() alone also
    # accepts digits such as "²" that float() can't parse.
    if seconds.isascii() and seconds.isdigit():
        return min(float(seconds), BATCH_BACKOFF_MAX_SECONDS)
    return None


def retry_delay(attempt: int, error: BaseException | None) -> float:
    """How long to wait before retry number ``attempt``. Uses ``retry_after_seconds(error)`` when
    the error has a ``Retry-After``, or otherwise a jittered backoff.
    """
    retry_after = retry_after_seconds(error)
    if retry_after is not None:
        return retry_after
    return backoff_delay(attempt, BATCH_BACKOFF_BASE_SECONDS, BATCH_BACKOFF_MAX_SECONDS)


def _parse_int_annotation(annotations: dict[str, str], key: str, default: int, minimum: int) -> int:
    value = annotations.get(key)
    if value is None:
        return default
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        parsed = minimum - 1
    if parsed < minimum:
        raise BatchError(f"batch annotation {key} value {value!r} is not an integer of at least {minimum}")
    return parsed


class LeaseSettings(NamedTuple):
    """The batch settings ``get_batch`` reads back from the Lease's labels and annotations."""
    lease_duration: int
    quorum_timeout: int
    work_budget: int
    next_ordinal: int
    create_rps: float
    # The labels the caller passed to claim_batch.
    labels: dict[str, str]


def parse_lease_metadata(labels: dict[str, str] | None, annotations: dict[str, str] | None) -> LeaseSettings:
    """Parses the batch Lease's labels and annotations upon ``get_batch``. A missing annotation
    falls back to its default.

    Raises:
        BatchError: If an annotation's value is invalid.
    """
    annotations = annotations or {}
    create_rps_str = annotations.get(BATCH_CREATE_RPS_ANNOTATION)
    create_rps = BATCH_DEFAULT_CREATE_RPS
    if create_rps_str is not None:
        try:
            create_rps = float(create_rps_str)
        except ValueError:
            create_rps = 0.0
        if not 0 < create_rps < math.inf:
            raise BatchError(
                f"batch annotation {BATCH_CREATE_RPS_ANNOTATION} value {create_rps_str!r} is not a positive number"
            )
    return LeaseSettings(
        lease_duration=parse_lease_duration_annotation(annotations.get(BATCH_LEASE_DURATION_ANNOTATION)),
        quorum_timeout=_parse_int_annotation(
            annotations, BATCH_QUORUM_TIMEOUT_ANNOTATION, BATCH_DEFAULT_QUORUM_TIMEOUT_SECONDS, 1
        ),
        work_budget=_parse_int_annotation(
            annotations, BATCH_WORK_BUDGET_ANNOTATION, BATCH_DEFAULT_WORK_BUDGET_SECONDS, 1
        ),
        next_ordinal=_parse_int_annotation(annotations, BATCH_NEXT_ORDINAL_ANNOTATION, 0, 0),
        create_rps=create_rps,
        labels={
            k: v
            for k, v in (labels or {}).items()
            if k not in (BATCH_ID_LABEL, BATCH_LEASE_UID_LABEL, CREATED_BY_LABEL)
        },
    )


def batch_lease_metadata(args: ClaimBatchArgs) -> tuple[dict[str, str], dict[str, str]]:
    """Returns the batch Lease's labels and annotations, which ``get_batch`` reads back."""
    labels = {**args.labels, BATCH_ID_LABEL: args.batch_id, CREATED_BY_LABEL: "python-client"}
    annotations = {
        BATCH_LEASE_DURATION_ANNOTATION: str(args.lease_duration),
        BATCH_WORK_BUDGET_ANNOTATION: str(args.work_budget),
        BATCH_QUORUM_TIMEOUT_ANNOTATION: str(args.quorum_timeout),
        BATCH_CREATE_RPS_ANNOTATION: f"{args.create_rps:g}",
    }
    return labels, annotations


def warmpool_template_name(warmpool_obj: dict[str, Any]) -> str | None:
    """Returns the name of the SandboxTemplate a SandboxWarmPool object references, if any."""
    spec = warmpool_obj.get("spec") or {}
    return (spec.get("sandboxTemplateRef") or {}).get("name") or None


def warmpool_ready_replicas(warmpool_obj: dict[str, Any]) -> int:
    """Returns a SandboxWarmPool object's ``status.readyReplicas``, or 0 if it has none."""
    ready = (warmpool_obj.get("status") or {}).get("readyReplicas")
    return ready if isinstance(ready, int) and ready > 0 else 0


class WarmCredit:
    """Counts, per warm pool, the creates that the pool's Ready sandboxes can cover.

    A covered create skips ``create_rps`` pacing, so only ``max_in_flight`` bounds it. Pacing
    exists so that a burst of cold claims doesn't start many pods on a node at once, and a claim
    a Ready sandbox takes starts no pod. Prechecks read the initial count, and ``acquire()`` reads
    the pool again once callers spend that credit and ``create_rps`` pacing would otherwise delay
    a create. When the batch watch sees a claim from a pool report a cold-start ``Ready=False``
    reason before becoming Ready, ``exhaust()`` zeroes that pool's credit and defers its next
    refresh so shared-pool bursts fall back to ``create_rps`` once Ready sandboxes run out, and
    ``seq()`` prevents an already in-flight refresh from overwriting a newer update. A 429
    response pauses the credit for numeric ``Retry-After`` values up to
    ``BATCH_BACKOFF_MAX_SECONDS`` (with a ``BATCH_WARM_CREDIT_THROTTLE_SECONDS`` minimum; missing
    or date-form headers use ``BATCH_WARM_CREDIT_THROTTLE_SECONDS``) so newly scheduled creates
    back off to ``create_rps`` while a long-lived batch recovers warm speed once the apiserver
    clears. The handles call this class with their lock held.
    """

    def __init__(self) -> None:
        self._credit: dict[str, int] = {}
        self._read_at: dict[str, float] = {}
        self._seq: dict[str, int] = {}
        self._throttled_until = -math.inf

    def _is_throttled(self) -> bool:
        """Returns whether a 429 cooldown is still active."""
        return time.monotonic() < self._throttled_until

    def seq(self, warmpool: str) -> int:
        """Returns the pool's update sequence so an in-flight refresh skips overwriting a newer update."""
        return self._seq.get(warmpool, 0)

    def record(self, warmpool: str, ready: int, *, seq: int | None = None) -> None:
        """Sets the pool's credit to its Ready replica count unless ``seq`` is stale."""
        if seq is not None and self._seq.get(warmpool, 0) != seq:
            return
        self._credit[warmpool] = ready
        self._read_at[warmpool] = time.monotonic()
        self._seq[warmpool] = self._seq.get(warmpool, 0) + 1

    def exhaust(self, warmpool: str) -> None:
        """Zeroes the pool's credit and defers its next refresh after a claim cold-starts."""
        self._credit[warmpool] = 0
        self._read_at[warmpool] = time.monotonic()
        self._seq[warmpool] = self._seq.get(warmpool, 0) + 1

    def refresh_due(self, warmpool: str) -> bool:
        """Whether callers spent the pool's credit, no 429 cooldown is active, and the last read
        happened at least ``BATCH_WARM_CREDIT_REFRESH_SECONDS`` ago, making a new read worthwhile.
        """
        if self._is_throttled() or self._credit.get(warmpool, 0) > 0:
            return False
        return time.monotonic() - self._read_at.get(warmpool, -math.inf) >= BATCH_WARM_CREDIT_REFRESH_SECONDS

    def take(self, warmpool: str) -> bool:
        """Spends one of the pool's credit when unthrottled, returning whether it covered the create."""
        if self._is_throttled() or self._credit.get(warmpool, 0) <= 0:
            return False
        self._credit[warmpool] -= 1
        return True

    def spend(self, warmpool: str) -> None:
        """Deducts one credit for a paced create so creates sent during a 429 cooldown still account
        for the Ready sandboxes they adopt instead of over-crediting cold claims once cooldown ends.
        """
        if self._credit.get(warmpool, 0) > 0:
            self._credit[warmpool] -= 1

    def throttle(self, retry_after: float | None = None) -> bool:
        """Pauses warm credit for at least ``BATCH_WARM_CREDIT_THROTTLE_SECONDS`` (or ``retry_after``
        when longer), returning whether this call started a new cooldown window.
        """
        now = time.monotonic()
        first = now >= self._throttled_until
        cooldown = max(retry_after or 0.0, BATCH_WARM_CREDIT_THROTTLE_SECONDS)
        self._throttled_until = max(self._throttled_until, now + cooldown)
        return first
