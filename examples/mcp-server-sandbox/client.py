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

"""Host-side MCP client that talks to mcp_server.py running inside the sandbox.

The transport is plain stdio — but instead of spawning the MCP server as a
local subprocess, we spawn ``kubectl exec -i mcp-sandbox -- python3 -u
/app/mcp_server.py``. kubectl forwards stdin/stdout between our process
and the server in the pod, so from the MCP client's perspective it's an
ordinary stdio session. No SDK, no router, no warm pool — just kubectl
exec.

To prove the PVC actually does something, the demo runs in two MCP
sessions with a Suspend → Resume cycle in between:

  Session 1: write a random blob to /workspace and remember its sha256.
  ──── patch the Sandbox to spec.operatingMode=Suspended; the controller
       deletes the pod (but keeps the Sandbox object and the PVC). Then
       patch back to Running; the controller creates a fresh pod with
       the same PVC reattached and a fresh container fs. ────
  Session 2: read the blob back. If sha256 still matches, the bytes
             survived because /workspace is on the PVC; on an emptyDir
             or container overlay they would have been wiped.

Finally we ``kubectl cp`` the file out of the pod and re-hash it locally
to confirm the bytes round-trip back to the host.
"""

import asyncio
import hashlib
import json
import subprocess
import sys
import time
from pathlib import Path

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

POD = "mcp-sandbox"
WORKSPACE = "/workspace"
BLOB_NAME = "random.bin"
BLOB_SIZE = 256
OUT_PATH = Path("./returned-random.bin")
POD_READY_TIMEOUT_SEC = 180

# Per-command timeouts on every kubectl call. 
KUBECTL_TIMEOUT_SEC = 30        # patch, get
KUBECTL_CP_TIMEOUT_SEC = 120    # cp may take longer for larger files


def _result_payload(call_result):
    """Return the tool result as a Python value.

    Prefer FastMCP's ``structuredContent`` when present. Otherwise the
    SDK exposes the result as a text block — FastMCP serialises dict /
    list return values to JSON in that text block, so try to parse JSON
    first and fall back to the raw text only if it isn't valid JSON.
    """
    structured = getattr(call_result, "structuredContent", None)
    if structured is not None:
        return structured
    for item in call_result.content or []:
        text = getattr(item, "text", None)
        if text is None:
            continue
        try:
            return json.loads(text)
        except (ValueError, TypeError):
            return text
    return None


def _mcp_server_params() -> StdioServerParameters:
    # `python3 -u` disables stdout buffering so MCP JSON-RPC frames are
    # delivered immediately rather than held in libc buffers.
    return StdioServerParameters(
        command="kubectl",
        args=[
            "exec", "-i", POD,
            "--",
            "python3", "-u", "/app/mcp_server.py",
        ],
    )


async def session_write() -> dict:
    """First MCP session: list tools, list empty workspace, write a blob."""
    async with stdio_client(_mcp_server_params()) as (stdio_read, stdio_write):
        async with ClientSession(stdio_read, stdio_write) as session:
            await session.initialize()

            tools = await session.list_tools()
            print(f"[host] tools advertised by server: {[t.name for t in tools.tools]}")

            before = _result_payload(await session.call_tool("list_blobs", {}))
            print(f"[host] list_blobs (before write) -> {before}")

            written = _result_payload(await session.call_tool(
                "write_random_blob",
                {"name": BLOB_NAME, "size_bytes": BLOB_SIZE},
            ))
            print(f"[host] write_random_blob({BLOB_NAME!r}, {BLOB_SIZE}) -> {written}")
            return written


async def session_read() -> dict:
    """Second MCP session against the freshly-restarted pod."""
    async with stdio_client(_mcp_server_params()) as (stdio_read, stdio_write):
        async with ClientSession(stdio_read, stdio_write) as session:
            await session.initialize()

            after = _result_payload(await session.call_tool("list_blobs", {}))
            print(f"[host] list_blobs (after restart) -> {after}")

            read_back = _result_payload(await session.call_tool(
                "read_blob", {"name": BLOB_NAME},
            ))
            print(f"[host] read_blob({BLOB_NAME!r}) -> {read_back}")
            return read_back


def _patch_operating_mode(mode: str) -> None:
    patch = json.dumps({"spec": {"operatingMode": mode}})
    subprocess.run(
        ["kubectl", "patch", "sandbox", POD, "--type=merge", "-p", patch],
        check=True,
        timeout=KUBECTL_TIMEOUT_SEC,
    )


def _pod_ready_status() -> str:
    """Return the pod's Ready condition status: 'True', 'False', or '' if absent.
    """
    try:
        result = subprocess.run(
            [
                "kubectl", "get", "pod", POD,
                "--ignore-not-found",
                "-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}",
            ],
            capture_output=True, text=True,
            timeout=KUBECTL_TIMEOUT_SEC,
        )
    except subprocess.TimeoutExpired:
        return ""
    return result.stdout.strip() if result.returncode == 0 else ""


def suspend_then_resume() -> None:
    """Cycle the pod via spec.operatingMode to prove the PVC outlives it.

    Suspended → the controller deletes the pod (Sandbox + PVC remain).
    Running   → the controller creates a fresh pod with the same PVC
                reattached and a fresh container filesystem.
    """
    print(f"[host] patching sandbox/{POD} operatingMode=Suspended (controller will delete the pod)...")
    _patch_operating_mode("Suspended")

    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            result = subprocess.run(
                ["kubectl", "get", "pod", POD, "--ignore-not-found", "-o", "name"],
                capture_output=True, text=True,
                timeout=KUBECTL_TIMEOUT_SEC,
            )
        except subprocess.TimeoutExpired:
            time.sleep(2)
            continue
        if not result.stdout.strip():
            print(f"[host] pod {POD} is gone (Sandbox is Suspended)")
            break
        time.sleep(2)
    else:
        raise RuntimeError(f"pod {POD} did not terminate within 120s of Suspend")

    print(f"[host] patching sandbox/{POD} operatingMode=Running (controller will recreate the pod)...")
    _patch_operating_mode("Running")

    print(f"[host] waiting up to {POD_READY_TIMEOUT_SEC}s for pod {POD} to be Ready again...")
    deadline = time.monotonic() + POD_READY_TIMEOUT_SEC
    while time.monotonic() < deadline:
        if _pod_ready_status() == "True":
            # Ready=True races slightly with kubectl exec being usable;
            # give the container a beat to finish startup.
            time.sleep(2)
            print(f"[host] pod {POD} is Ready again")
            return
        time.sleep(2)
    raise RuntimeError(f"pod {POD} did not become Ready within {POD_READY_TIMEOUT_SEC}s")


async def run() -> int:
    print("=" * 60)
    print("Session 1 — write a random blob to the PVC")
    print("=" * 60)
    written = await session_write()
    server_sha = written["sha256"]

    print()
    print("=" * 60)
    print("Suspend → Resume — the PVC persists, the container fs does not")
    print("=" * 60)
    suspend_then_resume()

    print()
    print("=" * 60)
    print("Session 2 — read the blob back from the PVC")
    print("=" * 60)
    read_back = await session_read()
    if read_back.get("sha256") != server_sha:
        print(
            f"[host] FAIL: sha256 differs across restart "
            f"(expected {server_sha}, got {read_back.get('sha256')})"
        )
        return 1
    print(f"[host] OK — sha256 matches across pod restart: {server_sha}")

    print()
    print("=" * 60)
    print("Return the file to the host and re-hash locally")
    print("=" * 60)
    print(f"[host] kubectl cp {POD}:{WORKSPACE}/{BLOB_NAME} -> {OUT_PATH}")
    subprocess.run(
        ["kubectl", "cp", f"{POD}:{WORKSPACE}/{BLOB_NAME}", str(OUT_PATH)],
        check=True,
        timeout=KUBECTL_CP_TIMEOUT_SEC,
    )
    host_sha = hashlib.sha256(OUT_PATH.read_bytes()).hexdigest()
    print(f"[host] returned {OUT_PATH.stat().st_size} bytes; host sha256={host_sha}")
    if host_sha != server_sha:
        print("[host] FAIL: returned file sha256 doesn't match server's sha256")
        return 1

    print("[host] OK — PVC contents survived pod restart and round-trip back to the host")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(run()))
