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
"""Validation for explicitly named SandboxClaims without freezing mutable spec fields."""

import re

from .exceptions import SandboxNotFoundError


_DNS1123_SUBDOMAIN_RE = re.compile(
    r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
)
_DNS1123_SUBDOMAIN_MAX_LENGTH = 253


def validate_claim_name(name: str) -> None:
    """Validates an explicit SandboxClaim name as a DNS-1123 subdomain."""
    if (
        not isinstance(name, str)
        or not name
        or len(name) > _DNS1123_SUBDOMAIN_MAX_LENGTH
        or not _DNS1123_SUBDOMAIN_RE.fullmatch(name)
    ):
        raise ValueError(
            f"Claim name '{name}' must be a valid DNS-1123 subdomain "
            "(lowercase alphanumerics, '-' and '.', starting and ending with an "
            f"alphanumeric; max {_DNS1123_SUBDOMAIN_MAX_LENGTH} characters)."
        )


def validate_claim_for_adoption(claim: dict | None, claim_name: str, warmpool: str) -> None:
    """Check the pool, deletion state and identity needed for a UID-pinned watch."""
    if claim is None:
        raise SandboxNotFoundError(
            f"SandboxClaim '{claim_name}' disappeared after the create conflict; retry the request."
        )
    metadata = claim.get("metadata") or {}
    if metadata.get("deletionTimestamp"):
        raise ValueError(f"SandboxClaim '{claim_name}' is terminating.")
    existing_pool = (claim.get("spec") or {}).get("warmPoolRef", {}).get("name")
    if existing_pool != warmpool:
        raise ValueError(
            f"SandboxClaim '{claim_name}' references warm pool '{existing_pool}', not '{warmpool}'."
        )
    for field in ("uid", "resourceVersion"):
        if not isinstance(metadata.get(field), str) or not metadata[field]:
            raise ValueError(f"SandboxClaim '{claim_name}' is missing metadata.{field}.")
