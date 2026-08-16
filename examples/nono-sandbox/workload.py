#!/usr/bin/env python3
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
"""Tiny demo workload for the nono-sandbox example.

It makes the boundaries visible in `kubectl logs`:
  1. a permitted write to /workspace
  2. a denied read of a mounted-but-ungranted secret (Landlock)
  3. a provider credential replaced with a session-scoped phantom token
  4. a real LogCLI running in a narrower, query-only child sandbox:
       - one exact incident-query argv succeeds against the demo Loki
       - altered flags and the delete command are denied before LogCLI starts
       - an argv-approved labels call is independently denied at L7
  5. SCOPED outer-agent egress, not "network off":
       - a host that is NOT allow-listed         -> blocked by nono's proxy
       - the allow-listed host at a NON-allowed path -> blocked at L7
       - the allow-listed host + method + path       -> permitted by policy
         (this is the agent's real traffic; it still needs an upstream + key)

The network probes go through nono's proxy via the standard *_PROXY env vars that
nono sets for the child, so the deny decisions are deterministic even offline. The
workload stays alive so the Sandbox holds Ready until the test writes a finish
sentinel; it then exits cleanly so nono can finalize and sign the audit session.
"""
import os
import string
import subprocess
import time
import urllib.error
import urllib.request


def fs_attempt(label, fn):
    """Run an allowed filesystem operation and report whether it succeeded."""
    try:
        fn()
        print(f"[ok]    {label}", flush=True)
    except Exception as exc:  # noqa: BLE001 - an allowed operation really failed
        print(f"[fail]  {label}: {exc}", flush=True)


def expect_blocked(label, fn):
    """Run an operation that policy is expected to block and report the result."""
    try:
        fn()
        print(f"[policy-fail] unexpectedly allowed {label}", flush=True)
    except Exception:  # noqa: BLE001 - the policy boundary is the assertion
        print(f"[policy-ok] blocked {label}", flush=True)


def write_workspace():
    """Write the permitted demo file into the workspace."""
    with open("/workspace/hello.txt", "w", encoding="utf-8") as fh:
        fh.write("written by the sandboxed agent\n")


def read_secret():
    """Attempt to read mounted secret data that is outside the filesystem grant."""
    with open("/etc/secret-config/token", "r", encoding="utf-8") as fh:
        fh.read()


def read_audit_state():
    """Attempt to list supervisor-owned audit state that the agent cannot access."""
    os.listdir("/var/lib/nono-state/nono/audit")


def credential_probe():
    """Prove the child received nono's random proxy token, not the provider key."""
    token = os.environ.get("OPENAI_API_KEY", "")
    is_session_token = len(token) == 64 and all(char in string.hexdigits for char in token)
    if is_session_token:
        print(
            "[credential-ok] agent received a session-scoped phantom, not the provider key",
            flush=True,
        )
    else:
        print("[credential-fail] expected a nono phantom token", flush=True)


def expect_http_blocked(label, method, url):
    """Send an HTTP request that network policy is expected to reject."""
    req = urllib.request.Request(url, method=method)
    try:
        with urllib.request.urlopen(req, timeout=8) as resp:
            print(
                f"[policy-fail] unexpectedly allowed {label} -> HTTP {resp.status}",
                flush=True,
            )
    except urllib.error.HTTPError as exc:
        if exc.code == 403:
            print(f"[policy-ok] blocked {label}", flush=True)
        else:
            print(f"[policy-fail] {label} returned HTTP {exc.code}", flush=True)
    except Exception as exc:  # noqa: BLE001 - connection refused / blocked / offline
        # CONNECT denials surface through urllib as a URL error containing 403.
        if "403" in str(exc):
            print(f"[policy-ok] blocked {label}", flush=True)
        else:
            print(f"[policy-fail] could not verify {label}: {exc}", flush=True)


INCIDENT_QUERY = '{service="payments"} |= "p99 latency"'
QUERY_ARGS: tuple[str, ...] = (
    "--quiet",
    "--output=raw",
    "query",
    "--since=15m",
    "--limit=10",
    INCIDENT_QUERY,
)


def run_logcli(*args):
    """Invoke by command name so nono's tool-sandbox shim mediates the call."""
    return subprocess.run(
        ["logcli", *args],
        check=False,
        capture_output=True,
        text=True,
        timeout=15,
    )


def tool_sandbox_probes():
    """Exercise the allowed and denied LogCLI tool-sandbox boundaries."""
    # The broker source token and the tool-only destination variables must not
    # be present in the outer agent process.
    tool_only_env = ("LOKI_TOKEN", "LOKI_ADDR", "LOKI_BEARER_TOKEN")
    if not any(os.environ.get(name) for name in tool_only_env):
        print("[tool-ok] Loki identity and address are absent from the agent loop", flush=True)
    else:
        print(
            "[tool-fail] a Loki credential or destination leaked into the agent loop",
            flush=True,
        )

    allowed = run_logcli(*QUERY_ARGS)
    if (
        allowed.returncode == 0
        and "payments p99 latency exceeded 900ms" in allowed.stdout
    ):
        print("[tool-ok] exact LogCLI incident query returned the seeded Loki log", flush=True)
    else:
        print(
            f"[tool-fail] allowed LogCLI query: rc={allowed.returncode} "
            f"stdout={allowed.stdout.strip()} stderr={allowed.stderr.strip()}",
            flush=True,
        )

    # This argv is explicitly permitted so LogCLI launches, but labels uses a
    # different Loki API path and the proxy independently rejects it.
    l7_denied = run_logcli("--quiet", "labels")
    l7_output = l7_denied.stdout + l7_denied.stderr
    if l7_denied.returncode != 0 and (
        "403" in l7_output or "forbidden" in l7_output.lower()
    ):
        print("[tool-ok] L7 policy blocked LogCLI from the labels endpoint", flush=True)
    else:
        print(
            f"[tool-fail] LogCLI L7 policy: rc={l7_denied.returncode} "
            f"stdout={l7_denied.stdout.strip()} stderr={l7_denied.stderr.strip()}",
            flush=True,
        )

    # Altering even one argument is rejected before the binary starts. This
    # prevents LogCLI's many output, file, auth, address, and parallel-query
    # flags from becoming ambient authority.
    changed_args = list(QUERY_ARGS)
    changed_args[4] = "--limit=5000"
    argv_denied = run_logcli(*changed_args)
    if argv_denied.returncode != 0 and "tool-sandbox denied logcli" in argv_denied.stderr:
        print("[tool-ok] invocation policy blocked altered LogCLI arguments", flush=True)
    else:
        print(
            f"[tool-fail] LogCLI argv policy: rc={argv_denied.returncode} "
            f"stderr={argv_denied.stderr.strip()}",
            flush=True,
        )

    delete_denied = run_logcli("delete", "list")
    if delete_denied.returncode != 0 and "tool-sandbox denied logcli" in delete_denied.stderr:
        print("[tool-ok] invocation policy blocked LogCLI deletion management", flush=True)
    else:
        print(
            f"[tool-fail] LogCLI delete policy: rc={delete_denied.returncode} "
            f"stderr={delete_denied.stderr.strip()}",
            flush=True,
        )


fs_attempt("wrote /workspace/hello.txt", write_workspace)
expect_blocked("read of mounted /etc/secret-config", read_secret)
expect_blocked("read of protected audit state", read_audit_state)
credential_probe()
tool_sandbox_probes()

# Not allow-listed at all -> proxy refuses the CONNECT.
expect_http_blocked(
    "egress to non-allow-listed host (example.com)", "GET", "https://example.com/"
)
# Allow-listed host, but only POST /v1/chat/completions is permitted -> L7 deny.
expect_http_blocked(
    "allow-listed host at disallowed path (/v1/models)",
    "GET",
    "https://api.openai.com/v1/models",
)

finish_sentinel = "/workspace/.finish-demo"
print("demo complete; holding open so the Sandbox stays Ready", flush=True)
while not os.path.exists(finish_sentinel):
    time.sleep(1)
print("[audit-ok] workload exiting so the supervisor can finalize audit", flush=True)
