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

"""Bounded Kubernetes names and label values for Harbor objects.

Harbor identifies a trial by a free-form ``session_id`` and an environment by a
content hash. Kubernetes names and label values are limited to 63 characters
and a restricted alphabet, so both are reduced to a stable short hash with a
readable prefix. The full values stay in annotations for humans.
"""

from __future__ import annotations

import hashlib
import re

# Label keys. The ``harbor.io`` domain marks what this package owns, so a
# cleanup can select it and nothing else.
LABEL_MANAGED_BY = "harbor.io/managed-by"
MANAGED_BY_VALUE = "k8s-agent-sandbox-harbor"
LABEL_JOB = "harbor.io/job"
LABEL_TRIAL = "harbor.io/trial"
LABEL_ENVIRONMENT = "harbor.io/environment"
LABEL_IMAGE_HASH = "harbor.io/image-hash"

# Annotation keys carry the unbounded originals.
ANNOTATION_SESSION = "harbor.io/session-id"
ANNOTATION_IMAGE = "harbor.io/image"
ANNOTATION_ENVIRONMENT_ID = "harbor.io/environment-id"

_HASH_LEN = 12
_LABEL_MAX = 63
_UNSAFE = re.compile(r"[^A-Za-z0-9_.-]")
_NAME_UNSAFE = re.compile(r"[^a-z0-9-]")


def short_hash(*parts: str) -> str:
    """A stable 12-hex-character digest of ``parts``."""
    h = hashlib.sha256()
    for part in parts:
        h.update(part.encode("utf-8"))
        h.update(b"\0")
    return h.hexdigest()[:_HASH_LEN]


def object_name(prefix: str, *key_parts: str) -> str:
    """A DNS-1123 object name: ``prefix`` plus a short hash of ``key_parts``.

    The prefix is lower-cased and reduced to ``[a-z0-9-]`` so a caller-supplied
    prefix cannot produce an invalid name.
    """
    clean = _NAME_UNSAFE.sub("-", prefix.lower()).strip("-")
    name = f"{clean}-{short_hash(*key_parts)}" if clean else short_hash(*key_parts)
    return name[:_LABEL_MAX].rstrip("-")


def label_value(raw: str) -> str:
    """``raw`` as a valid label value, identical when it already is one.

    A value that is too long or carries characters outside ``[A-Za-z0-9_.-]``
    becomes the sanitized head plus a hash suffix, so two raw values never
    collapse into one label and a reader still recognizes the head.
    """
    if raw == "":
        return ""
    sanitized = _UNSAFE.sub("-", raw)
    if sanitized == raw and len(raw) <= _LABEL_MAX and raw[0].isalnum() and raw[-1].isalnum():
        return raw
    head_len = _LABEL_MAX - _HASH_LEN - 1
    head = sanitized[:head_len].strip("-_.")
    return f"{head}-{short_hash(raw)}" if head else short_hash(raw)
