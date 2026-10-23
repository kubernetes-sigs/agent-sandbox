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

"""Cluster-free parsing helpers for the DeerFlow agent-sandbox provider.

Kept free of deerflow / k8s_agent_sandbox imports so they are unit-testable
anywhere (see test_parsing.py).
"""

from __future__ import annotations

import posixpath


def to_root_relative(path: str, root: str) -> str:
    """Translate a sandbox-absolute path to a sandboxd root-dir-relative path.

    DeerFlow tools pass absolute paths (/mnt/user-data/workspace/fib.py); the
    SDK filesystem API addresses files relative to sandboxd's --root-dir.
    """
    root = root.rstrip("/")
    # Normalize BEFORE the prefix check so traversal segments cannot smuggle
    # the path out of the root ("/mnt/user-data/../../etc" -> "/etc").
    path = posixpath.normpath(path)
    if path == root:
        return "."
    prefix = root + "/"
    if not path.startswith(prefix):
        raise PermissionError(f"path {path!r} is outside the sandbox root {root!r}")
    return path[len(prefix):]


def slice_lines(content: str, start_line: int | None, end_line: int | None) -> str:
    """Return the 1-indexed, inclusive [start_line, end_line] window of content.

    Mirrors the harness read-file contract
    (``deerflow.sandbox.read_file_contract.split_file_lines``): lines end only
    at ``"\\n"``, a trailing ``"\\r"`` is dropped, and the window is re-joined
    with ``"\\n"``. ``str.splitlines()`` is deliberately NOT used — it also
    ends lines at ``"\\r"``, ``\\f``, ``\\v``, ``\\x85``, ``\\u2028``/
    ``\\u2029``, which makes remote windows disagree with ``count_file_lines``
    and corrupts CRLF round-trips.
    """
    if start_line is not None and start_line < 1:
        raise ValueError(f"start_line must be >= 1, got {start_line}")
    if end_line is not None and end_line < 1:
        raise ValueError(f"end_line must be >= 1, got {end_line}")
    lines = content.split("\n")
    if lines and lines[-1] == "":
        # A trailing newline terminates the last line; it is not an empty
        # extra line (this keeps len(lines) == count_file_lines(content)).
        lines.pop()
    lines = [line.rstrip("\r") for line in lines]
    start = (start_line - 1) if start_line is not None else 0
    end = end_line if end_line is not None else len(lines)
    return "\n".join(lines[start:max(start, end)])


def parse_grep_line(line: str) -> tuple[str, int, str] | None:
    """Parse one `grep -n` output line `path:lineno:content`.

    Returns None for lines that are not matches (e.g. grep's
    'Binary file ... matches' notice), which callers skip.
    """
    first = line.find(":")
    if first < 0:
        return None
    second = line.find(":", first + 1)
    if second < 0:
        return None
    try:
        lineno = int(line[first + 1:second])
    except ValueError:
        return None
    return line[:first], lineno, line[second + 1:]
