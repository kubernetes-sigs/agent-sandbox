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
"""Validation for safely adopting an existing SandboxClaim."""

import re
from dataclasses import dataclass

from .constants import (
    CLAIM_API_GROUP,
    CLAIM_API_VERSION,
    CREATED_BY_LABEL,
    TERMINAL_CLAIM_READY_REASONS,
)
from .exceptions import (
    SandboxClaimFailedError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
)
from .utils import construct_sandbox_claim_env_spec


_SUPPORTED_SPEC_FIELDS = frozenset(
    {
        "warmPoolRef",
        "lifecycle",
        "volumeClaimTemplates",
        "additionalPodMetadata",
        "env",
    }
)
_DNS1123_SUBDOMAIN_RE = re.compile(
    r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
)
_DNS1123_SUBDOMAIN_MAX_LENGTH = 253


@dataclass(frozen=True)
class ValidatedClaimIdentity:
    """Stable identity needed to continue watching a validated Claim."""

    resource_version: str
    uid: str


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


def _reject(claim_name: str, field: str) -> None:
    raise ValueError(
        f"SandboxClaim '{claim_name}' has a different {field}; refusing to adopt."
    )


def _normalized_optional(value):
    return value or None


def _serialized_env(env: dict[str, str] | None) -> list[dict]:
    return [
        env_var.model_dump(by_alias=True, exclude_none=True)
        for env_var in construct_sandbox_claim_env_spec(env)
    ]


def get_ready_sandbox_name(claim: dict, claim_name: str) -> str | None:
    """Returns the Sandbox already observed as Ready on an adopted Claim."""
    metadata = claim.get("metadata") or {}
    generation = metadata.get("generation")
    status = claim.get("status") or {}
    if not isinstance(status, dict):
        return None

    ready = False
    for condition in status.get("conditions") or []:
        if not isinstance(condition, dict):
            continue
        if condition.get("observedGeneration") != generation:
            continue
        reason = condition.get("reason")
        ready_is_false = (
            condition.get("type") == "Ready"
            and condition.get("status") == "False"
        )
        if ready_is_false and reason == "TemplateNotFound":
            raise SandboxTemplateNotFoundError(
                "SandboxTemplate requested does not exist: "
                f"{condition.get('message', 'Template not found')}"
            )
        if reason == "WarmPoolNotFound":
            raise SandboxWarmPoolNotFoundError(
                "SandboxWarmPool requested does not exist: "
                f"{condition.get('message', 'WarmPool not found')}"
            )
        if ready_is_false and reason in TERMINAL_CLAIM_READY_REASONS:
            raise SandboxClaimFailedError(
                f"SandboxClaim '{claim_name}' failed with terminal reason "
                f"{reason}: {condition.get('message', '')}"
            )
        if condition.get("type") == "Ready" and condition.get("status") == "True":
            ready = True

    sandbox_status = status.get("sandbox") or {}
    if not isinstance(sandbox_status, dict):
        return None
    sandbox_name = sandbox_status.get("name") or sandbox_status.get("Name")
    if ready and isinstance(sandbox_name, str) and sandbox_name:
        return sandbox_name
    return None


def validate_claim_for_adoption(
    claim: dict,
    *,
    claim_name: str,
    namespace: str,
    warmpool: str,
    labels: dict[str, str] | None,
    lifecycle: dict | None,
    volume_claim_templates: list[dict] | None,
    pod_metadata: dict | None,
    env: dict[str, str] | None,
    expected_uid: str | None = None,
) -> ValidatedClaimIdentity:
    """Validates that an existing Claim is the exact requested allocation.

    Additional labels are tolerated because controllers and admission may add
    labels. Spec fields fail closed: attaching to behavior the caller did not
    request would make a deterministic name unsafe as an idempotency key.

    Returns the existing object's identity for a watch that cannot miss a
    readiness transition or silently switch to a recreated object.
    """
    if not isinstance(claim, dict):
        _reject(claim_name, "object representation")

    expected_api_version = f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}"
    if claim.get("apiVersion") != expected_api_version:
        _reject(claim_name, "apiVersion")
    if claim.get("kind") != "SandboxClaim":
        _reject(claim_name, "kind")

    metadata = claim.get("metadata")
    if not isinstance(metadata, dict):
        _reject(claim_name, "metadata")
    if metadata.get("name") != claim_name:
        _reject(claim_name, "metadata.name")
    if metadata.get("namespace") != namespace:
        _reject(claim_name, "metadata.namespace")
    if metadata.get("deletionTimestamp"):
        _reject(claim_name, "metadata.deletionTimestamp")

    resource_version = metadata.get("resourceVersion")
    if not isinstance(resource_version, str) or not resource_version:
        _reject(claim_name, "metadata.resourceVersion")
    uid = metadata.get("uid")
    if not isinstance(uid, str) or not uid:
        _reject(claim_name, "metadata.uid")
    if expected_uid is not None and uid != expected_uid:
        _reject(claim_name, "metadata.uid")
    generation = metadata.get("generation")
    if (
        not isinstance(generation, int)
        or isinstance(generation, bool)
        or generation < 1
    ):
        _reject(claim_name, "metadata.generation")

    existing_labels = metadata.get("labels") or {}
    if not isinstance(existing_labels, dict):
        _reject(claim_name, "metadata.labels")
    expected_labels = {**(labels or {}), CREATED_BY_LABEL: "python-client"}
    for key, value in expected_labels.items():
        if existing_labels.get(key) != value:
            _reject(claim_name, f"metadata.labels[{key}]")

    spec = claim.get("spec")
    if not isinstance(spec, dict):
        _reject(claim_name, "spec")
    unsupported_fields = sorted(set(spec) - _SUPPORTED_SPEC_FIELDS)
    if unsupported_fields:
        raise ValueError(
            f"SandboxClaim '{claim_name}' has unsupported spec fields; "
            "refusing to adopt."
        )
    if spec.get("warmPoolRef") != {"name": warmpool}:
        _reject(claim_name, "spec.warmPoolRef")

    optional_fields = {
        "lifecycle": lifecycle,
        "volumeClaimTemplates": volume_claim_templates,
        "additionalPodMetadata": pod_metadata,
        "env": _serialized_env(env),
    }
    for field, expected in optional_fields.items():
        if _normalized_optional(spec.get(field)) != _normalized_optional(expected):
            _reject(claim_name, f"spec.{field}")

    return ValidatedClaimIdentity(resource_version=resource_version, uid=uid)
