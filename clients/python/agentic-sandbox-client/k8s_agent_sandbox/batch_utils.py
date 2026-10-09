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
from typing import NamedTuple

from .constants import (
    BATCH_ID_LABEL,
    BATCH_LEASE_DURATION_ANNOTATION,
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
BATCH_REQUEST_TIMEOUT_SECONDS = 30
# Extra wait past BATCH_REQUEST_TIMEOUT_SECONDS when stopping the creators, so a create that just
# timed out has time to return and its worker to exit.
BATCH_CREATOR_STOP_GRACE_SECONDS = 1
# Set above the apiserver's 60s request timeout, its 504 response can end a long deletecollection first.
BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS = 90
BATCH_RELEASE_MAX_IDLE_ROUNDS = 3


def validate_batch_id(batch_id: str) -> None:
    """Validates a caller-supplied batch id.

    Must be a DNS-1123 label that starts with a letter and is at most 52 characters,
    so ``<id>-<ordinal>`` stays within a 63-character DNS label limit.
    """
    if (
        not isinstance(batch_id, str)
        or len(batch_id) > BATCH_ID_MAX_LENGTH
        or not _BATCH_ID_RE.match(batch_id)
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
    return wait / 2 + rand() * wait / 2


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

    if create_rps is None:
        create_rps = BATCH_DEFAULT_CREATE_RPS
    elif isinstance(create_rps, bool) or not isinstance(create_rps, (int, float)) or not create_rps > 0:
        raise ValueError(f"create_rps must be a positive number, got {create_rps!r}")

    labels = dict(labels or {})
    if BATCH_ID_LABEL in labels:
        raise ValueError(f"labels must not set {BATCH_ID_LABEL}; claim_batch sets it")
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
        create_rps=float(create_rps),
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
    # The HTTP-date form of Retry-After is ignored in favor of the backoff.
    if isinstance(retry_after, str) and retry_after.strip().isdigit():
        return min(float(retry_after.strip()), BATCH_BACKOFF_MAX_SECONDS)
    return backoff_delay(attempt, BATCH_BACKOFF_BASE_SECONDS, BATCH_BACKOFF_MAX_SECONDS)


def batch_lease_metadata(
    batch_id: str, lease_duration: int, work_budget: int, quorum_timeout: int
) -> tuple[dict[str, str], dict[str, str]]:
    """Returns the batch Lease's labels and annotations, which ``get_batch`` reads back."""
    labels = {BATCH_ID_LABEL: batch_id, CREATED_BY_LABEL: "python-client"}
    annotations = {
        BATCH_LEASE_DURATION_ANNOTATION: str(lease_duration),
        BATCH_WORK_BUDGET_ANNOTATION: str(work_budget),
        BATCH_QUORUM_TIMEOUT_ANNOTATION: str(quorum_timeout),
    }
    return labels, annotations


def warmpool_template_name(warmpool_obj: dict) -> str | None:
    """Returns the name of the SandboxTemplate a SandboxWarmPool object references, if any."""
    spec = warmpool_obj.get("spec") or {}
    return (spec.get("sandboxTemplateRef") or {}).get("name") or None
