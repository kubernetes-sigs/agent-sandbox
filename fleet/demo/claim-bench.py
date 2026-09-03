#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
"""Claim-path benchmark — the SDK creating sandboxes across the fleet.

WHY THIS EXISTS: the M2.5 fill (400k warm across 2 clusters, 15.3 min) only
exercised the OPERATOR path — fleet-member provisioning SandboxWarmPools via
CustomObjectsApi, because the SDK has no WarmPool/Template CRUD. That says
nothing about the path an actual user takes:

    client.create_sandbox(template="sb-m25-0042")
      -> resolver reads fleet/assignments.json  (which cluster owns it?)
      -> pinned per-cluster k8s_agent_sandbox.SandboxClient
      -> SandboxClaim -> controller binds a WARM pod -> Ready

That is FleetSandboxClient (resolver.py), and every claim below goes through
the real SDK. Run it against a standing fleet: the pools must already be
full, or you are measuring cold pod creation instead of warm claim binding.

Measures: claims/sec, claim latency distribution, per-cluster routing spread,
and failures by class. Latency here is create_sandbox() wall time, which
INCLUDES the SDK's watch until the claim reports Ready — that is the number a
user feels, not the API round-trip.

Usage:
  export KUBECONFIG=$HOME/.kube/config-a:$HOME/.kube/config-b   # REQUIRED
  export FLEET_BUCKET=agent-sandbox-fleet-my-project

  # smoke: 20 claims, 5 at a time — run this FIRST
  python3 demo/claim-bench.py --claims 20 --concurrency 5

  # throughput: 2,000 claims, 100 in flight
  python3 demo/claim-bench.py --claims 2000 --concurrency 100

  # hold them (skip cleanup) to inspect binding by hand
  python3 demo/claim-bench.py --claims 50 --concurrency 10 --no-delete
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import threading
import time
from collections import Counter
from concurrent.futures import ThreadPoolExecutor, as_completed

sys.path.insert(
    0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "python")
)

from agent_sandbox_fleet.objectstore import GCS, Paths  # noqa: E402
from agent_sandbox_fleet.resolver import FleetSandboxClient  # noqa: E402

# cluster short-name -> zone. Override for your own fleet.
ZONES = {
    "cluster-a": "us-central1-a", "cluster-b": "us-central1-b",
    "cluster-c": "us-central1-c", "cluster-d": "us-east1-b",
    "cluster-e": "us-central2-b", "cluster-f": "us-west1-b",
}


def make_context_naming(project: str, overrides: dict[str, str]):
    """Per-cluster context naming.

    gke_context_naming() takes ONE location, but this fleet spans four
    regions — A is us-central1-a and B is us-central1-b, so a single
    location cannot name both. Each cluster gets its own zone.
    """
    def _naming(cluster: str) -> str:
        if cluster in overrides:
            return overrides[cluster]
        zone = ZONES.get(cluster)
        if zone is None:
            raise KeyError(
                f"no zone known for cluster {cluster!r}; pass "
                f"--context {cluster}=<kubeconfig-context>"
            )
        return f"gke_{project}_{zone}_{cluster}"
    return _naming


def pct(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = min(len(s) - 1, max(0, int(round((p / 100.0) * (len(s) - 1)))))
    return s[k]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--bucket", default=os.environ.get("FLEET_BUCKET"))
    ap.add_argument("--project", default="my-project")
    ap.add_argument("--namespace", default="multi-cluster-fleet")
    ap.add_argument("--claims", type=int, default=20)
    ap.add_argument("--concurrency", type=int, default=5)
    ap.add_argument("--timeout", type=int, default=90,
                    help="sandbox_ready_timeout passed to the SDK")
    ap.add_argument("--no-delete", action="store_true",
                    help="leave claims standing (default: delete each after "
                         "it binds, so the pools stay full)")
    ap.add_argument("--context", action="append", default=[],
                    metavar="CLUSTER=CONTEXT",
                    help="override kubeconfig context for a cluster; repeatable")
    ap.add_argument("--pool-maxsize", type=int, default=0,
                    help="urllib3 connections per cluster "
                         "(default: 2*concurrency + 16)")
    ap.add_argument("--out", default="claim-bench-results.json")
    ap.add_argument("--dump-raw", action="store_true",
                    help="include a per-claim record (start offset, latency, "
                         "cluster) in the JSON — needed to tell a transport "
                         "retry from server saturation: retries land on a few "
                         "fixed durations, saturation spreads smoothly")
    args = ap.parse_args()

    if not args.bucket:
        ap.error("--bucket / $FLEET_BUCKET is required")
    if "KUBECONFIG" not in os.environ:
        print("WARNING: KUBECONFIG unset. This fleet keeps one kubeconfig per "
              "cluster, so contexts for BOTH clusters must be visible:\n"
              "  export KUBECONFIG=$HOME/.kube/config-a:$HOME/.kube/config-b",
              file=sys.stderr)

    overrides = dict(kv.split("=", 1) for kv in args.context)

    # Read assignments directly to build the template work-list. Sorting then
    # interleaving by cluster keeps the offered load balanced across clusters
    # regardless of how the planner ordered pools.
    gcs = GCS(args.bucket)
    assn = gcs.get_json(Paths().assignments)
    if not assn:
        print(f"no assignments in gs://{args.bucket}; run `fleetctl apply` first",
              file=sys.stderr)
        return 2

    per_cluster: dict[str, list[str]] = {}
    for cluster, ca in sorted(assn.get("clusters", {}).items()):
        per_cluster[cluster] = [p["template"] for p in ca.get("pools", [])]
    if not any(per_cluster.values()):
        print("assignments contain no pools", file=sys.stderr)
        return 2

    templates: list[str] = []
    for i in range(max(len(v) for v in per_cluster.values())):
        for cluster in sorted(per_cluster):
            if i < len(per_cluster[cluster]):
                templates.append(per_cluster[cluster][i])

    print(f"generation={assn.get('generation')} clusters="
          f"{ {c: len(v) for c, v in per_cluster.items()} } "
          f"templates={len(templates)}")
    print(f"claims={args.claims} concurrency={args.concurrency} "
          f"delete={not args.no_delete}")

    # Pool must cover 2x concurrency. create_sandbox holds a watch open for
    # its whole duration AND issues API calls alongside it, and deletes share
    # the same pool -- so N in-flight claims want ~2N connections per cluster.
    # Undersizing does NOT backpressure: urllib3 opens throwaway connections
    # instead, and the resulting TLS storm shows up as a fat latency tail, not
    # as an error. Measured at c100: pool=116 gave p99 19.8s / max 39.4s;
    # pool=400 gave p99 3.06s / max 3.13s on the same fleet.
    pool_size = args.pool_maxsize or (2 * args.concurrency + 16)
    client = FleetSandboxClient(
        args.bucket,
        context_naming=make_context_naming(args.project, overrides),
        namespace=args.namespace,
        connection_pool_maxsize=pool_size,
    )
    print(f"connection_pool_maxsize={pool_size} per cluster")

    lat: list[float] = []
    dlat: list[float] = []
    raw: list[dict] = []
    # Wall-clock offset of each create completion. The headline rate MUST come
    # from these, not from the run wall: a worker that has finished its create
    # goes straight into delete_sandbox, which is far slower than a create, so
    # the run keeps ticking long after the last claim bound. Measured at c100:
    # all 1000 creates landed by ~11.5s, deletes dragged the wall to 30.6s --
    # reporting 1000/30.6 = 32.7 claims/s understated the claim path by ~2.7x.
    create_done: list[float] = []
    routed: Counter = Counter()
    errors: Counter = Counter()
    lock = threading.Lock()

    def one(i: int) -> None:
        template = templates[i % len(templates)]
        t0 = time.perf_counter()
        try:
            sandbox = client.create_sandbox(
                template=template, sandbox_ready_timeout=args.timeout,
            )
        except Exception as e:  # noqa: BLE001 — classify, never abort the run
            with lock:
                errors[type(e).__name__] += 1
            return
        now = time.perf_counter()
        elapsed = now - t0
        # Ask the resolver where it went rather than assuming — this is the
        # routing assertion, not decoration.
        try:
            cluster = client.resolve(template).cluster
        except Exception:
            cluster = "?"
        with lock:
            lat.append(elapsed)
            create_done.append(now - started)
            routed[cluster] += 1
            raw.append({"i": i, "t0": round(t0 - started, 3),
                        "lat": round(elapsed, 3), "cluster": cluster,
                        "template": template})
        if not args.no_delete:
            name = getattr(sandbox, "claim_name", None) or getattr(sandbox, "name", None)
            if name:
                d0 = time.perf_counter()
                try:
                    client.delete_sandbox(name)
                except Exception as e:  # noqa: BLE001
                    with lock:
                        errors[f"delete:{type(e).__name__}"] += 1
                else:
                    with lock:
                        dlat.append(time.perf_counter() - d0)

    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(one, i) for i in range(args.claims)]
        done = 0
        for _ in as_completed(futures):
            done += 1
            if done % max(1, args.claims // 10) == 0:
                with lock:
                    n, e = len(lat), sum(errors.values())
                rate = n / max(1e-9, time.perf_counter() - started)
                print(f"  {done}/{args.claims} ok={n} err={e} {rate:.1f} claims/s")
    wall = time.perf_counter() - started

    ok = len(lat)
    # Span from t0 to the LAST create completion — the claim path's own clock.
    create_span = max(create_done) if create_done else wall
    result = {
        "claims_requested": args.claims,
        "claims_ok": ok,
        "claims_failed": sum(errors.values()),
        "concurrency": args.concurrency,
        "connection_pool_maxsize": pool_size,
        # The headline. Deletes are excluded by construction.
        "claims_per_s": round(ok / create_span, 2) if create_span else 0.0,
        "create_span_s": round(create_span, 2),
        # Whole run including cleanup — useful, but NOT the claim rate.
        "wall_s": round(wall, 2),
        "cycle_per_s": round(ok / wall, 2) if wall else 0.0,
        "latency_s": {
            "p50": round(pct(lat, 50), 3), "p90": round(pct(lat, 90), 3),
            "p99": round(pct(lat, 99), 3),
            "min": round(min(lat), 3) if lat else 0.0,
            "max": round(max(lat), 3) if lat else 0.0,
            "mean": round(statistics.fmean(lat), 3) if lat else 0.0,
        },
        "routed": dict(routed),
        "errors": dict(errors),
    }
    if dlat:
        result["delete_latency_s"] = {
            "p50": round(pct(dlat, 50), 3), "p90": round(pct(dlat, 90), 3),
            "p99": round(pct(dlat, 99), 3),
            "max": round(max(dlat), 3),
            "mean": round(statistics.fmean(dlat), 3),
        }
    # Stall census. A transport retry piles calls onto a few fixed durations;
    # a saturated server spreads them. Bucketing by whole seconds makes the
    # difference visible without needing the raw dump.
    slow = [r for r in raw if r["lat"] >= 5.0]
    if slow:
        buckets = Counter(int(r["lat"]) for r in slow)
        by_cluster = Counter(r["cluster"] for r in slow)
        result["stalls"] = {
            "count": len(slow),
            "pct": round(100.0 * len(slow) / max(1, ok), 2),
            "by_whole_second": dict(sorted(buckets.items())),
            "by_cluster": dict(by_cluster),
            # Templates map 1:1 to warmpools, and a warmpool's pods land on
            # whichever nodes the scheduler picked -- so a stall list that
            # repeats the same templates across runs points at bad nodes,
            # while a different set every run points at the control plane.
            "templates": sorted(r["template"] for r in slow),
        }
    if args.dump_raw:
        result["raw"] = raw

    print("\n" + json.dumps(result, indent=2))
    with open(args.out, "w") as f:
        json.dump(result, f, indent=2)
    print(f"\nwrote {args.out}")
    # Routing spread is the multi-cluster assertion: a run that lands entirely
    # on one cluster is the known per-cluster-client-collision bug resurfacing
    # (resolver.py _build_client docstring), not a fast fleet.
    if len(routed) < len([c for c, v in per_cluster.items() if v]):
        print("WARNING: claims did not reach every cluster with pools — "
              "check per-cluster client pinning", file=sys.stderr)
    return 0 if ok == args.claims else 1


if __name__ == "__main__":
    raise SystemExit(main())
