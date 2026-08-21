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

"""
Verification script for the firecracker-sandbox example.

Drives the HTTP endpoints exposed by ``main.py`` over plain HTTP. Set
``SANDBOX_BASE_URL`` to the pod's ``http://<ip>:8888`` URL (typically via
``kubectl port-forward``). No k8s SDK dependencies required.

Run::

    SANDBOX_BASE_URL=http://127.0.0.1:8888 python test_client.py

Note: this example's runtime uses a deliberately minimal endpoint contract
(``/exec``, ``/files``) that differs from the reference
``python-runtime-sandbox`` (``/execute``, ``/upload``, ``/download``). The
``k8s_agent_sandbox`` SDK targets the latter contract, so this script only
covers the direct-HTTP transport.
"""

from __future__ import annotations

import os
import sys
import time
from typing import Callable, List, Tuple


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
class TestResult:
    def __init__(self, name: str, passed: bool, detail: str = "") -> None:
        self.name = name
        self.passed = passed
        self.detail = detail


def _run_test(name: str, fn: Callable[[], str]) -> TestResult:
    start = time.time()
    try:
        detail = fn()
        elapsed = time.time() - start
        return TestResult(name, True, f"{detail} ({elapsed:.2f}s)")
    except AssertionError as exc:
        return TestResult(name, False, f"assertion: {exc}")
    except Exception as exc:  # noqa: BLE001
        return TestResult(name, False, f"{type(exc).__name__}: {exc}")


# ---------------------------------------------------------------------------
# Tests — direct HTTP
# ---------------------------------------------------------------------------
def _build_tests() -> Tuple[str, List[Callable[[], str]]]:
    base_url = os.environ.get("SANDBOX_BASE_URL")
    if not base_url:
        raise SystemExit(
            "SANDBOX_BASE_URL is not set. Point it at a running sandbox pod, "
            "e.g. via `kubectl port-forward pod/<name> 8888:8888` and export "
            "SANDBOX_BASE_URL=http://127.0.0.1:8888"
        )

    import requests

    def _health() -> str:
        r = requests.get(f"{base_url}/health", timeout=5)
        assert r.status_code == 204, r.status_code
        return "204 No Content"

    def _exec() -> str:
        r = requests.post(
            f"{base_url}/exec",
            json={"cmd": "echo 'hello from firecracker'"},
            timeout=10,
        )
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["exit_code"] == 0, body
        assert "hello from firecracker" in body["stdout"], body
        return f"stdout={body['stdout'].strip()!r}"

    def _files() -> str:
        r = requests.post(
            f"{base_url}/files",
            data={"path": "hello.txt"},
            files={"file": ("hello.txt", b"hi from direct http")},
            timeout=10,
        )
        assert r.status_code == 200, r.text
        r = requests.get(f"{base_url}/files", params={"path": "hello.txt"}, timeout=5)
        assert r.status_code == 200, r.text
        assert r.content == b"hi from direct http", r.content
        return "round-trip ok"

    def _metrics() -> str:
        r = requests.get(f"{base_url}/metrics", timeout=5)
        assert r.status_code == 200, r.text
        body = r.json()
        assert "timestamp" in body, body
        return f"keys={sorted(body.keys())}"

    def _init() -> str:
        r = requests.post(
            f"{base_url}/init",
            json={"envs": {"HELLO": "firecracker"}, "timestamp": time.time()},
            timeout=5,
        )
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["status"] == "ok", body
        r = requests.get(f"{base_url}/envs", timeout=5)
        assert r.status_code == 200, r.text
        assert r.json().get("HELLO") == "firecracker", r.json()
        return "env injected"

    def _cleanup() -> str:
        return "nothing to clean up"

    return "Direct HTTP", [
        _health, _exec, _files, _metrics, _init, _cleanup
    ]


# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------
def main() -> int:
    mode_name, tests = _build_tests()

    print(f"\n=== firecracker-sandbox verification: {mode_name} ===\n")

    results: List[TestResult] = []
    for i, fn in enumerate(tests, 1):
        name = fn.__name__.lstrip("_")
        print(f"[{i}/{len(tests)}] running {name}...")
        result = _run_test(name, fn)
        results.append(result)
        status = "PASS" if result.passed else "FAIL"
        print(f"         {status}: {result.detail}")

    print("\n=== Summary ===")
    for r in results:
        mark = "PASS" if r.passed else "FAIL"
        print(f"  [{mark}] {r.name}: {r.detail}")

    failed = [r for r in results if not r.passed]
    if failed:
        print(f"\n{len(failed)} test(s) failed.")
        return 1
    print(f"\nAll {len(results)} test(s) passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
