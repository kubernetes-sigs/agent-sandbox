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
"""Shared public-client scenarios for deterministic SandboxClaim adoption."""

from copy import deepcopy

from k8s_agent_sandbox.constants import (
    CREATED_BY_LABEL,
    TERMINAL_CLAIM_READY_REASONS,
)
from k8s_agent_sandbox.exceptions import (
    SandboxClaimFailedError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
)


CLAIM_NAME = "sandbox-workflow-123"
NAMESPACE = "test-namespace"
WARMPOOL = "test-warmpool"
REQUESTED_LABELS = {"workflow": "workflow-123"}
REQUESTED_ENV = {"FOO": "bar", "DEBUG": "true"}
VOLUME_CLAIM_TEMPLATES = [{"metadata": {"name": "workspace"}}]
POD_LABELS = {"workflow": "workflow-123"}
POD_ANNOTATIONS = {"owner": "workflow-123"}


def matching_claim(env: dict[str, str] | None = None) -> dict:
    """Returns a Claim matching every requested immutable field."""
    claim = {
        "apiVersion": "extensions.agents.x-k8s.io/v1beta1",
        "kind": "SandboxClaim",
        "metadata": {
            "name": CLAIM_NAME,
            "namespace": NAMESPACE,
            "resourceVersion": "existing-rv",
            "uid": "claim-uid",
            "generation": 1,
            "labels": {
                **REQUESTED_LABELS,
                CREATED_BY_LABEL: "python-client",
                "controller-added": "allowed",
            },
        },
        "spec": {
            "warmPoolRef": {"name": WARMPOOL},
            "volumeClaimTemplates": VOLUME_CLAIM_TEMPLATES,
            "additionalPodMetadata": {
                "labels": POD_LABELS,
                "annotations": POD_ANNOTATIONS,
            },
        },
    }
    if env:
        claim["spec"]["env"] = [
            {"name": name, "value": value} for name, value in env.items()
        ]
    return claim


def _replace(claim: dict, path: tuple[str, ...], value: object) -> None:
    target = claim
    for segment in path[:-1]:
        target = target[segment]
    target[path[-1]] = value


def mismatched_claims():
    """Yields every immutable mismatch with its safe diagnostic field."""
    yield None, "object representation"
    cases = (
        (("apiVersion",), "extensions.agents.x-k8s.io/v1alpha1", "apiVersion"),
        (("kind",), "OtherClaim", "kind"),
        (("metadata",), [], "metadata"),
        (("metadata", "name"), "other-claim", "metadata.name"),
        (("metadata", "namespace"), "other-namespace", "metadata.namespace"),
        (("metadata", "resourceVersion"), "", "metadata.resourceVersion"),
        (("metadata", "uid"), "", "metadata.uid"),
        (("metadata", "generation"), 0, "metadata.generation"),
        (("metadata", "labels"), [], "metadata.labels"),
        (
            ("metadata", "labels", CREATED_BY_LABEL),
            "other-client",
            CREATED_BY_LABEL,
        ),
        (
            ("metadata", "labels", "workflow"),
            "other-workflow",
            "workflow",
        ),
        (("spec",), [], "spec"),
        (("spec", "warmPoolRef"), {"name": "other-pool"}, "warmPoolRef"),
        (
            ("spec", "lifecycle"),
            {"shutdownPolicy": "Delete"},
            "lifecycle",
        ),
        (
            ("spec", "volumeClaimTemplates"),
            [{"metadata": {"name": "other-volume"}}],
            "volumeClaimTemplates",
        ),
        (
            ("spec", "additionalPodMetadata"),
            {"labels": {"workflow": "other-workflow"}},
            "additionalPodMetadata",
        ),
    )
    for path, value, error_field in cases:
        claim = deepcopy(matching_claim())
        _replace(claim, path, value)
        yield claim, error_field

    claim = matching_claim()
    claim["metadata"]["deletionTimestamp"] = "2026-08-31T23:59:00Z"
    yield claim, "metadata.deletionTimestamp"

    claim = matching_claim()
    claim["spec"]["futureBehavior"] = {"enabled": True}
    yield claim, "unsupported spec fields"


def terminal_claims():
    """Yields adopted Claims with every terminal readiness condition."""
    reasons = (
        ("TemplateNotFound", SandboxTemplateNotFoundError, "SandboxTemplate"),
        ("WarmPoolNotFound", SandboxWarmPoolNotFoundError, "SandboxWarmPool"),
        *(
            (reason, SandboxClaimFailedError, reason)
            for reason in sorted(TERMINAL_CLAIM_READY_REASONS)
        ),
    )
    for reason, error_type, error_pattern in reasons:
        claim = matching_claim()
        claim["status"] = {
            "conditions": [
                {
                    "type": "Ready",
                    "status": "False",
                    "reason": reason,
                    "message": "safe diagnostic",
                    "observedGeneration": 1,
                }
            ]
        }
        yield claim, error_type, reason, error_pattern
