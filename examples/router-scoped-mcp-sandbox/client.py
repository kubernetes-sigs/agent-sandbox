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
    python3 client.py --token TOKEN --target-id box-a [--expect-forbidden]

The router URL and routing headers default to values matching sandbox.yaml
run through run-test-kind.sh; override with --router-url / --namespace /
--port for other setups.
"""

import argparse
import asyncio
import hashlib
import json
import sys

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


async def run(args: argparse.Namespace) -> int:
    headers = {
        "X-Sandbox-ID": args.target_id,
        "X-Sandbox-Namespace": args.namespace,
        "X-Sandbox-Port": str(args.port),
        "Authorization": f"Bearer {args.token}",
    }
    url = f"{args.router_url.rstrip('/')}/mcp"

    try:
        async with streamablehttp_client(url, headers=headers) as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()

                if args.expect_forbidden:
                    print(f"[host] FAIL: expected the router to reject this token against "
                          f"{args.target_id!r}, but the MCP session initialized")
                    return 1

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
        if args.expect_forbidden:
            print(f"[host] OK — request against {args.target_id!r} was rejected as expected: {exc}")
            return 0
        print(f"[host] FAIL: unexpected error talking to {args.target_id!r}: {exc}")
        return 1


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--router-url", default="http://sandbox-router-svc.default.svc.cluster.local:8080")
    p.add_argument("--namespace", default="default")
    p.add_argument("--port", type=int, default=8000)
    p.add_argument("--token", required=True, help="scoped token from ../mint-token")
    p.add_argument("--target-id", required=True, help="X-Sandbox-ID to send — the sandbox being addressed")
    p.add_argument("--expect-forbidden", action="store_true",
                    help="invert the pass/fail check: success means the router rejected the request")
    return p.parse_args()


if __name__ == "__main__":
    sys.exit(asyncio.run(run(parse_args())))
