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

"""Host-side MCP client, reached entirely through sandbox-router.

Unlike mcp-server-sandbox's client.py, this script never shells out to
``kubectl`` or ``ssh``. Its only inputs are the router's URL, the four
X-Sandbox-* routing headers, and a scoped token minted by ../mint-token
(standing in for the Sandbox controller). There is no kubeconfig on this
machine at all in the intended deployment — the credential in hand is the
token, nothing else.

Usage:
    python3 client.py --token TOKEN --target-id box-a [--expect-status 403]

The default --router-url is the localhost port-forward that
run-test-kind.sh establishes (kubectl port-forward svc/sandbox-router-svc
18080:8080) — the in-cluster Service DNS name is not reachable from the
host on kind. Running in-cluster instead, pass
--router-url http://sandbox-router-svc.default.svc.cluster.local:8080.
The routing headers default to values matching sandbox.yaml; override
with --namespace / --port for other setups.

With --expect-status, the script sends one plain HTTP request and passes
only if the router answers with exactly that status code — asserting the
documented 403 (valid token, wrong sandbox) vs 401 (forged/expired
token) distinction rather than accepting any failure.
"""

import argparse
import asyncio
import hashlib
import json
import sys
import urllib.error
import urllib.request

from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

BLOB_NAME = "random.bin"
BLOB_SIZE = 256


def _result_payload(call_result):
    """Return the tool result as a Python value (see mcp-server-sandbox's
    client.py for why this dance is necessary)."""
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


def check_rejection(url: str, headers: dict, args: argparse.Namespace) -> int:
    """Assert the router rejects this request with exactly --expect-status.

    The authz decision happens in the router before any proxying, so a
    single plain HTTP request is enough to observe the status code the
    MCP client library would otherwise swallow into a generic error.
    """
    req = urllib.request.Request(
        url,
        data=b"{}",
        method="POST",
        headers={
            **headers,
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            print(f"[host] FAIL: expected HTTP {args.expect_status} against "
                  f"{args.target_id!r}, but got {resp.status}")
            return 1
    except urllib.error.HTTPError as exc:
        if exc.code == args.expect_status:
            print(f"[host] OK — router returned {exc.code} for {args.target_id!r} as expected")
            return 0
        print(f"[host] FAIL: expected HTTP {args.expect_status} against "
              f"{args.target_id!r}, got {exc.code}")
        return 1


async def run(args: argparse.Namespace) -> int:
    headers = {
        "X-Sandbox-ID": args.target_id,
        "X-Sandbox-Namespace": args.namespace,
        "X-Sandbox-Port": str(args.port),
        "Authorization": f"Bearer {args.token}",
    }
    url = f"{args.router_url.rstrip('/')}/mcp"

    if args.expect_status is not None:
        return check_rejection(url, headers, args)

    try:
        async with streamablehttp_client(url, headers=headers) as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()

                written = _result_payload(await session.call_tool(
                    "write_random_blob", {"name": BLOB_NAME, "size_bytes": BLOB_SIZE},
                ))
                print(f"[host] write_random_blob({BLOB_NAME!r}, {BLOB_SIZE}) -> {written}")

                read_back = _result_payload(await session.call_tool(
                    "read_blob", {"name": BLOB_NAME},
                ))
                print(f"[host] read_blob({BLOB_NAME!r}) -> {read_back}")

                if read_back.get("sha256") != written.get("sha256"):
                    print("[host] FAIL: sha256 mismatch between write and read")
                    return 1
                print(f"[host] OK — round-trip sha256 matches: {written['sha256']}")
                return 0
    except Exception as exc:  # noqa: BLE001 - report and let run-test-kind.sh classify it
        print(f"[host] FAIL: unexpected error talking to {args.target_id!r}: {exc}")
        return 1


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--router-url", default="http://localhost:18080",
                   help="matches run-test-kind.sh's port-forward; use the "
                        "sandbox-router-svc Service DNS name when running in-cluster")
    p.add_argument("--namespace", default="default")
    p.add_argument("--port", type=int, default=8000)
    p.add_argument("--token", required=True, help="scoped token from ../mint-token")
    p.add_argument("--target-id", required=True, help="X-Sandbox-ID to send — the sandbox being addressed")
    p.add_argument("--expect-status", type=int, default=None, metavar="CODE",
                   help="invert the check: pass only if the router rejects the "
                        "request with exactly this HTTP status (403 wrong sandbox, "
                        "401 forged/expired)")
    return p.parse_args()


if __name__ == "__main__":
    sys.exit(asyncio.run(run(parse_args())))
