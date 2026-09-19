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
import socket
import uuid
from collections.abc import Callable
from datetime import datetime, timedelta

from .exceptions import BatchError

CLOCK_SKEW_MARGIN = 5
BATCH_DEFAULT_LEASE_DURATION_SECONDS = 60
BATCH_BACKOFF_BASE_SECONDS = 0.5
BATCH_BACKOFF_MAX_SECONDS = 30.0

_BATCH_ID_RE = re.compile(r"^[a-z]([-a-z0-9]*[a-z0-9])?$")
BATCH_ID_MAX_LENGTH = 52


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
