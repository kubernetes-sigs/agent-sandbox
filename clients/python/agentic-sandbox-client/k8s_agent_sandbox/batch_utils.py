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
import math
import os
import random
import re
import secrets
import socket
import string
import uuid
from collections.abc import Callable, Sequence
from datetime import UTC, datetime, timedelta
from enum import Enum
from itertools import accumulate
from typing import Any, NamedTuple

from .constants import (
    BATCH_CREATE_RPS_ANNOTATION,
    BATCH_GROUP_LABEL,
    BATCH_ID_LABEL,
    BATCH_LEASE_DURATION_ANNOTATION,
    BATCH_NEXT_ORDINAL_ANNOTATION,
    BATCH_QUORUM_TIMEOUT_ANNOTATION,
    BATCH_WORK_BUDGET_ANNOTATION,
    CREATED_BY_LABEL,
)
from .exceptions import BatchError
from .models import BatchGroup
from .pod_metadata import validate_labels

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
        if group.size < 1:
            raise ValueError(f"group '{group.warmpool}' size must be at least 1")
        if group.warmpool in pools:
            raise ValueError(f"warm pool '{group.warmpool}' appears in more than one group")
        pools.add(group.warmpool)

    labels = dict(labels or {})
    for reserved in (BATCH_ID_LABEL, BATCH_GROUP_LABEL):
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


def group_label_selector(batch_id: str, warmpool: str) -> str:
    """Returns the label selector for one group's fill claims."""
    return f"{BATCH_ID_LABEL}={batch_id},{BATCH_GROUP_LABEL}={group_label_value(warmpool)}"


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


def retry_delay(attempt: int, error: BaseException | None) -> float:
    """How long to wait before retry number ``attempt``. Uses the error's ``Retry-After`` seconds
    when it has one (capped at ``BATCH_BACKOFF_MAX_SECONDS``), or otherwise a jittered backoff.
    """
    headers = getattr(error, "headers", None)
    retry_after = headers.get("Retry-After") if headers else None
    seconds = retry_after.strip() if isinstance(retry_after, str) else ""
    # The HTTP-date form of Retry-After is ignored in favor of the backoff. isdigit() alone also
    # accepts digits such as "²" that float() can't parse.
    if seconds.isascii() and seconds.isdigit():
        return min(float(seconds), BATCH_BACKOFF_MAX_SECONDS)
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
        labels={k: v for k, v in (labels or {}).items() if k not in (BATCH_ID_LABEL, CREATED_BY_LABEL)},
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
