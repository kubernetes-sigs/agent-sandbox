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
"""Shared interpretation of SandboxClaim status."""

from .constants import TERMINAL_CLAIM_READY_REASONS
from .exceptions import (
    SandboxClaimFailedError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
)


def _is_current_condition(
    condition: dict,
    generation: object,
    require_current_generation: bool,
) -> bool:
    if not require_current_generation:
        return True
    observed_generation = condition.get("observedGeneration")
    return (
        generation is None
        or observed_generation is None
        or observed_generation == generation
    )


def _raise_ready_failure(condition: dict, claim_name: str) -> None:
    if condition.get("type") != "Ready" or condition.get("status") != "False":
        return
    reason = condition.get("reason")
    if reason == "TemplateNotFound":
        raise SandboxTemplateNotFoundError(
            "SandboxTemplate requested does not exist: "
            f"{condition.get('message', 'Template not found')}"
        )
    if reason == "WarmPoolNotFound":
        raise SandboxWarmPoolNotFoundError(
            "SandboxWarmPool requested does not exist: "
            f"{condition.get('message', 'WarmPool not found')}"
        )
    if reason in TERMINAL_CLAIM_READY_REASONS:
        raise SandboxClaimFailedError(
            f"SandboxClaim '{claim_name}' failed with terminal reason "
            f"{reason}: {condition.get('message', '')}"
        )


def get_claim_sandbox_name(
    claim: dict,
    claim_name: str,
    *,
    require_ready: bool,
    require_current_generation: bool,
) -> str | None:
    """Return the bound Sandbox name once the requested status is observed."""
    metadata = claim.get("metadata") or {}
    generation = metadata.get("generation") if isinstance(metadata, dict) else None
    status = claim.get("status") or {}
    if not isinstance(status, dict):
        return None

    ready = False
    conditions = status.get("conditions") or []
    if isinstance(conditions, list):
        for condition in conditions:
            if not isinstance(condition, dict):
                continue
            if not _is_current_condition(
                condition, generation, require_current_generation
            ):
                continue
            _raise_ready_failure(condition, claim_name)
            if (
                condition.get("type") == "Ready"
                and condition.get("status") == "True"
            ):
                ready = True

    sandbox_status = status.get("sandbox") or {}
    if not isinstance(sandbox_status, dict):
        return None
    sandbox_name = sandbox_status.get("name") or sandbox_status.get("Name")
    if (
        isinstance(sandbox_name, str)
        and sandbox_name
        and (ready or not require_ready)
    ):
        return sandbox_name
    return None
