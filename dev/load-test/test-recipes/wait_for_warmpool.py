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

"""Check WarmPool capacity and template convergence for ClusterLoader2 Exec."""

import argparse
from collections import defaultdict
import json
import os
from pathlib import Path
import subprocess
import sys
import time

REVISION = "BENCHMARK_REVISION"


def convergence_errors(pools, sandboxes, pool_count, replicas, revision):
    """Reject stale counters and old/terminating members after template fan-out."""
    errors = []
    if len(pools) != pool_count:
        errors.append(f"expected {pool_count} pools, found {len(pools)}")
    by_owner = defaultdict(list)
    for sandbox in sandboxes:
        for owner in sandbox["metadata"].get("ownerReferences", []):
            if owner.get("controller") and owner.get("kind") == "SandboxWarmPool":
                by_owner[owner["uid"]].append(sandbox)
    for pool in pools:
        metadata = pool["metadata"]
        status = pool.get("status", {})
        name = metadata["name"]
        if (metadata.get("deletionTimestamp")
                or pool["spec"].get("replicas", 1) != replicas
                or status.get("observedGeneration") != metadata["generation"]
                or status.get("replicas", 0) != replicas
                or status.get("readyReplicas", 0) != replicas):
            errors.append(f"{name}: pool spec/status has not converged")
        members = by_owner[metadata["uid"]]
        if len(members) != replicas:
            errors.append(f"{name}: expected {replicas} members, found {len(members)}")
        for member in members:
            meta = member["metadata"]
            containers = member["spec"]["podTemplate"]["spec"]["containers"]
            current_revision = next((env.get("value") for container in containers
                                     if container["name"] == "sandbox"
                                     for env in container.get("env", [])
                                     if env["name"] == REVISION), None)
            ready = any(c.get("type") == "Ready" and c.get("status") == "True"
                        and c.get("observedGeneration") == meta["generation"]
                        for c in member.get("status", {}).get("conditions", []))
            if meta.get("deletionTimestamp") or current_revision != revision or not ready:
                errors.append(f"{name}/{meta['name']}: not Ready at revision {revision}")
    return errors


def kubectl(*args, timeout):
    """Bound a request and its subprocess by the caller's remaining budget."""
    if timeout <= 0:
        raise subprocess.TimeoutExpired("kubectl", timeout)
    return subprocess.check_output(["kubectl", f"--request-timeout={timeout:.3f}s", *args],
                                   text=True, stderr=subprocess.PIPE, timeout=timeout)


def error_message(exc):
    """Preserve kubectl stderr so API failures are actionable."""
    if isinstance(exc, subprocess.CalledProcessError) and exc.stderr:
        return f"{exc}: {exc.stderr.strip()}"
    return str(exc)


def snapshot(output, pools, sandboxes, namespaces, status):
    """Save metrics each stage, but full objects only at the first failure."""
    deadline = time.monotonic() + 120
    commands = {"controller-metrics.txt": ["get", "--raw", os.environ.get(
        "CONTROLLER_METRICS_PATH",
        "/api/v1/namespaces/agent-sandbox-system/services/agent-sandbox-controller:metrics/proxy/metrics")]}
    first_failure = output.parent / "first-failure.txt"
    if status != "converged" and not first_failure.exists():
        # CL2 continues after a failed measurement; avoid repeating large dumps.
        first_failure.write_text(output.name + "\n")
        (output / "pools.json").write_text(json.dumps(pools) + "\n")
        (output / "sandboxes.json").write_text(json.dumps(sandboxes) + "\n")
        for namespace in namespaces:
            commands[f"{namespace}-pods.json"] = ["get", "pods", "-n", namespace, "-o", "json"]
            commands[f"{namespace}-events.json"] = ["get", "events", "-n", namespace, "-o", "json"]
    for filename, command in commands.items():
        try:
            (output / filename).write_text(kubectl(*command, timeout=deadline - time.monotonic()))
        except (subprocess.SubprocessError, OSError) as exc:
            # Metrics/RBAC availability must not turn convergence into a failure.
            (output / (filename + ".error")).write_text(error_message(exc) + "\n")


def main():
    """Poll within one stage deadline and report convergence separately from API errors."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pools", type=int, required=True)
    parser.add_argument("--replicas", type=int, required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--stage", required=True)
    parser.add_argument("--timeout", type=int, default=1800, help="seconds")
    parser.add_argument("--interval", type=float, default=10, help="seconds")
    args = parser.parse_args()
    if args.pools < 1 or args.replicas < 1 or args.timeout <= 0 or args.interval <= 0:
        parser.error("pool/replica counts, timeout and interval must be positive")
    # Explicit paths keep CL2 and kubectl on the same cluster and scope the run.
    for key in ("KUBECONFIG", "WARMPOOL_RUN_ID", "WARMPOOL_REPORT_DIR"):
        if not os.environ.get(key):
            parser.error(f"{key} is required")
    output = Path(os.environ["WARMPOOL_REPORT_DIR"]) / args.stage
    output.mkdir(parents=True, exist_ok=True)
    start = time.monotonic()
    deadline = start + args.timeout
    pools, sandboxes, namespaces = [], [], []
    status, errors = "error", []
    try:
        while True:
            if time.monotonic() >= deadline:
                status = "timeout"
                break
            # Large LISTs share the stage budget instead of an independent 20s cap.
            pools = json.loads(kubectl("get", "sandboxwarmpools", "-A", "-l",
                f"benchmark-run={os.environ['WARMPOOL_RUN_ID']}", "-o", "json",
                timeout=deadline - time.monotonic()))["items"]
            namespaces = sorted({p["metadata"]["namespace"] for p in pools})
            sandboxes = []
            # Custom run labels are not copied to members; use the controller's
            # membership label to exclude standalone Sandboxes, then match owner UIDs.
            for namespace in namespaces:
                sandboxes.extend(json.loads(kubectl("get", "sandboxes", "-n", namespace,
                                                    "-l", "agents.x-k8s.io/warm-pool-sandbox",
                                                    "-o", "json", timeout=deadline - time.monotonic()))["items"])
            # These separate LISTs are not an atomic snapshot. Transient mismatches
            # during reconciliation are expected and are checked again on the next poll.
            errors = convergence_errors(pools, sandboxes, args.pools, args.replicas, args.revision)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                status = "timeout"
                errors.append("stage deadline expired while observing convergence")
                break
            if not errors:
                status = "converged"
                break
            time.sleep(min(args.interval, remaining))
    except (subprocess.SubprocessError, OSError, ValueError) as exc:
        errors = [error_message(exc)]
    elapsed = time.monotonic() - start
    snapshot(output, pools, sandboxes, namespaces, status)
    result = dict(status=status, stage=args.stage, pools=args.pools, replicas=args.replicas,
                  revision=args.revision, elapsed_seconds=elapsed, errors=errors)
    (output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({**result, "errors": errors[:10]}))
    return 0 if status == "converged" else 1


if __name__ == "__main__":
    sys.exit(main())
