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
"""Helpers for validating labels and assembling pod metadata."""

import re


# Kubernetes label validation: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#syntax-and-character-set
_LABEL_NAME_RE = re.compile(r"^[A-Za-z0-9][-A-Za-z0-9_.]*[A-Za-z0-9]$|^[A-Za-z0-9]$")
_LABEL_PREFIX_RE = re.compile(r"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")
LABEL_NAME_MAX_LENGTH = 63
LABEL_PREFIX_MAX_LENGTH = 253


def validate_label_name(name: str, context: str):
    """Validates a label name segment (key or value) against k8s constraints."""
    if len(name) > LABEL_NAME_MAX_LENGTH:
        raise ValueError(
            f"Label {context} '{name}' exceeds max length of {LABEL_NAME_MAX_LENGTH} characters."
        )
    if not _LABEL_NAME_RE.match(name):
        raise ValueError(
            f"Label {context} '{name}' contains invalid characters. "
            f"Must start and end with alphanumeric, and contain only [-A-Za-z0-9_.]."
        )


def validate_labels(labels: dict[str, str]):
    """Validates label keys and values against Kubernetes constraints."""
    for key, value in labels.items():
        if not key:
            raise ValueError("Label key cannot be empty.")

        # Keys can have an optional prefix: "prefix/name"
        if "/" in key:
            prefix, name = key.split("/", 1)
            if not prefix or len(prefix) > LABEL_PREFIX_MAX_LENGTH:
                raise ValueError(
                    f"Label key prefix '{prefix}' is invalid or exceeds {LABEL_PREFIX_MAX_LENGTH} characters."
                )
            if not _LABEL_PREFIX_RE.match(prefix):
                raise ValueError(
                    f"Label key prefix '{prefix}' must be a valid DNS subdomain."
                )
            if not name:
                raise ValueError(f"Label key '{key}' has an empty name after prefix.")
            validate_label_name(name, f"key name in '{key}'")
        else:
            validate_label_name(key, f"key '{key}'")

        # Values can be empty, but if non-empty must match the same name constraints
        if value:
            validate_label_name(value, f"value '{value}' for key '{key}'")


def build_pod_metadata(
    pod_labels: dict[str, str] | None,
    pod_annotations: dict[str, str] | None,
) -> dict | None:
    """Assembles the ``spec.additionalPodMetadata`` payload.

    ``pod_labels`` are validated with the same RFC-1123 rules as claim
    labels so callers fail fast client-side. Client-side validation only
    checks label syntax (RFC-1123) and does not replicate the controller's domain
    allow-list or system-label restrictions, so some keys accepted here may
    still be rejected server-side. Empty sections are omitted, and ``None`` is
    returned when neither labels nor annotations are supplied so no empty
    ``additionalPodMetadata`` is written to the manifest.
    """
    if pod_labels:
        validate_labels(pod_labels)

    pod_metadata: dict = {}
    if pod_labels:
        pod_metadata["labels"] = pod_labels
    if pod_annotations:
        pod_metadata["annotations"] = pod_annotations
    return pod_metadata or None
