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

"""Tar archives for directory and file transfer over an exec stream.

Harbor moves whole trees (tests, solutions, logs). A tar stream keeps modes,
symlinks and empty directories, and needs only ``tar`` in the image.
"""

from __future__ import annotations

import io
import logging
import tarfile
from pathlib import Path

logger = logging.getLogger(__name__)


def pack_dir(source: Path | str) -> bytes:
    """The contents of ``source`` as a tar stream rooted at ``.``."""
    source = Path(source)
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        for child in sorted(source.iterdir()):
            tar.add(child, arcname=child.name)
    return buf.getvalue()


def pack_file(source: Path | str, arcname: str) -> bytes:
    """One file as a tar stream whose single member is ``arcname``."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        tar.add(Path(source), arcname=arcname)
    return buf.getvalue()


def _skip_unsafe(member: tarfile.TarInfo, path: str) -> tarfile.TarInfo | None:
    """The data filter, except that a member it rejects is skipped with a warning.

    A sandbox directory can hold a member the filter refuses, most often a
    symlink with an absolute target. Raising there would stop the extraction
    and lose every member after it, and Harbor fails the trial when the
    verifier directory does not download.
    """
    try:
        return tarfile.data_filter(member, path)
    except tarfile.FilterError as exc:
        logger.warning("not extracting %s from the sandbox: %s", member.name, exc)
        return None


def unpack(data: bytes, target: Path | str) -> None:
    """Extract a tar stream under ``target`` with the data filter (no escapes).

    Members the filter rejects are left out; the rest are extracted.
    """
    target = Path(target)
    target.mkdir(parents=True, exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as tar:
        tar.extractall(target, filter=_skip_unsafe)
