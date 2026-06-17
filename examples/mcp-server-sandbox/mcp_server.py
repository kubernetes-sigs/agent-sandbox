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

"""Minimal custom MCP server that reads and writes blobs under a workspace dir.

This server runs *inside* the sandbox container. The host-side ``client.py``
spawns it via ``kubectl exec -i mcp-sandbox -- python3 -u /app/mcp_server.py``
and speaks MCP over the resulting stdio channel. It exposes three tools that
operate against the workspace directory (the PVC mount, ``/workspace`` by
default; override with the ``MCP_WORKSPACE`` env var):

  - ``list_blobs()``                  — names of all files in the workspace.
  - ``write_random_blob(name, size)`` — write ``size`` random bytes to
                                        ``<workspace>/<name>``, return sha256.
  - ``read_blob(name)``               — return size + sha256 of an existing blob.

Fork this file as a starting point for your own MCP server.
"""

import hashlib
import os
import secrets
import sys
from pathlib import Path

from mcp.server.fastmcp import FastMCP

WORKSPACE = Path(os.environ.get("MCP_WORKSPACE", "/workspace")).resolve()
WORKSPACE.mkdir(parents=True, exist_ok=True)

mcp = FastMCP("blob-store")


def _safe_path(name: str) -> Path:
    """Resolve ``name`` under WORKSPACE, rejecting traversal attempts."""
    candidate = (WORKSPACE / name).resolve()
    if WORKSPACE not in candidate.parents and candidate != WORKSPACE:
        raise ValueError(f"path escapes workspace: {name!r}")
    return candidate


@mcp.tool()
def list_blobs() -> list[str]:
    """Return the names of all files directly under the workspace."""
    return sorted(p.name for p in WORKSPACE.iterdir() if p.is_file())


@mcp.tool()
def write_random_blob(name: str, size_bytes: int) -> dict:
    """Write ``size_bytes`` cryptographically-random bytes to ``<workspace>/<name>``.

    Returns the path written, the number of bytes, and the sha256 of the
    contents so the caller can verify a later round-trip.
    """
    if size_bytes < 0 or size_bytes > 16 * 1024 * 1024:
        raise ValueError("size_bytes must be in [0, 16 MiB]")
    path = _safe_path(name)
    data = secrets.token_bytes(size_bytes)
    path.write_bytes(data)
    return {
        "path": str(path),
        "bytes_written": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
    }


@mcp.tool()
def read_blob(name: str) -> dict:
    """Return size + sha256 of ``<workspace>/<name>`` (does not return contents)."""
    path = _safe_path(name)
    data = path.read_bytes()
    return {
        "path": str(path),
        "size_bytes": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
    }


if __name__ == "__main__":
    print(f"[mcp_server] workspace={WORKSPACE}", file=sys.stderr)
    mcp.run()  # stdio transport
