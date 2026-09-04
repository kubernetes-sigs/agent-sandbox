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

"""Stressed end-to-end test — FleetSandboxClient rewrite.

Post 2026-07-16 sync: this used to hand-build one SandboxClient per cluster
with per-cluster semaphores. That glue moves into FleetSandboxClient in the
fleet package — the stress test now just calls
``fleet.create_sandbox(template=X)`` and the fleet layer routes.

What this proves:
- FleetSandboxClient works end-to-end at real load.
- Multi-cluster placement + GKE image streaming (when enabled on the
  fleet clusters) get exercised through the standard claim path.

Usage:
  python demo/stress-e2e.py --rate 5 --duration 60
  python demo/stress-e2e.py --rate 20 --duration 120 --keep-claims
  python demo/stress-e2e.py --rate 5 --duration 60 --strategy=hash  # cache-locality mode
"""

from __future__ import annotations

import argparse
import concurrent.futures
import os
import random
import subprocess
import sys
import threading
import time
from dataclasses import dataclass
from typing import Optional

from k8s_agent_sandbox.exceptions import SandboxNotReadyError

from agent_sandbox_fleet import (
    ClusterResolver,
    FleetSandboxClient,
    gke_context_naming,
    kind_context_naming,
)


NAMESPACE = "multi-cluster-fleet"


@dataclass
class ClaimTarget:
  """One dispatch unit: a template + how many replicas exist across the fleet
  (used for weighted rotation so bigger pools get proportionally more claims)."""
  template: str
  fleet_wide_replicas: int


@dataclass
class ClaimResult:
  template: str
  cluster: Optional[str]     # populated after resolve
  requested_at: float
  ready_at: Optional[float] = None
  claim_name: Optional[str] = None
  error: Optional[str] = None

  @property
  def latency_ms(self) -> Optional[float]:
    if self.ready_at is None:
      return None
    return (self.ready_at - self.requested_at) * 1000


# ---------------------------------------------------------------------------
# Setup helpers
# ---------------------------------------------------------------------------

def load_targets(bucket: str) -> list[ClaimTarget]:
  """Build the weighted-rotation target list from the current assignment.

  Rather than pinning to (cluster, warmpool) tuples like the old script did,
  we work at the template layer — resolver picks the cluster on dispatch.
  Weight = total replicas of that template across the fleet.
  """
  resolver = ClusterResolver(bucket)
  templates = resolver.all_templates()
  if not templates:
    sys.exit("no assignments in bucket; run `fleetctl apply` first")
  targets: list[ClaimTarget] = []
  for t in templates:
    matches = resolver.list_matches(t)
    total_replicas = sum(m.replicas for m in matches)
    if total_replicas > 0:
      targets.append(ClaimTarget(template=t, fleet_wide_replicas=total_replicas))
  if not targets:
    sys.exit("no non-empty pools in assignments; nothing to claim against")
  return targets


def prime_kubeconfig(clusters: list[str], project: str, region: str,
                     zone: str | None = None) -> None:
  """Run `gcloud container clusters get-credentials` per cluster once so the
  per-cluster contexts land in ~/.kube/config. FleetSandboxClient's lazy
  client build assumes those contexts already exist locally.
  """
  location_flag, location_value = ("--zone", zone) if zone else ("--region", region)
  for c in clusters:
    print(f"[setup] priming kubeconfig for {c} ({location_flag}={location_value})")
    subprocess.run(
        ["gcloud", "container", "clusters", "get-credentials", c,
         location_flag, location_value, "--project", project],
        check=True, capture_output=True,
    )


# ---------------------------------------------------------------------------
# Dispatch — one claim through FleetSandboxClient
# ---------------------------------------------------------------------------

def dispatch(
    fleet: FleetSandboxClient,
    per_cluster_sems: dict[str, threading.Semaphore],
    sem_lock: threading.Lock,
    per_cluster_concurrency: int,
    target: ClaimTarget,
    cleanup: bool,
    timeout_s: float,
) -> ClaimResult:
  """Create a sandbox for the given template via FleetSandboxClient.

  Per-cluster semaphores are still needed even with the fleet client, because
  ONE cluster's apiserver can still saturate under too many concurrent watches
  (the 2026-07-14 lesson). We resolve first to learn which cluster we're
  targeting, then acquire that cluster's semaphore, then dispatch.
  """
  result = ClaimResult(template=target.template, cluster=None,
                       requested_at=time.time())
  try:
    resolved = fleet.resolve(target.template)
    result.cluster = resolved.cluster
  except Exception as e:  # noqa: BLE001
    result.error = f"resolve failed: {type(e).__name__}: {e}"[:200]
    return result

  # Ensure this cluster has a semaphore (lazy — clusters can appear mid-run
  # if a fleet re-plan added a new one).
  with sem_lock:
    sem = per_cluster_sems.get(result.cluster)
    if sem is None:
      sem = threading.Semaphore(per_cluster_concurrency)
      per_cluster_sems[result.cluster] = sem

  with sem:
    try:
      sandbox = fleet.create_sandbox(
          template=target.template,
          namespace=NAMESPACE,
          sandbox_ready_timeout=int(timeout_s),
          labels={"stress-test": "true"},
      )
      result.ready_at = time.time()
      result.claim_name = getattr(sandbox, "claim_name", None) or \
                          getattr(sandbox, "name", None) or \
                          getattr(sandbox, "sandbox_name", None)
    except SandboxNotReadyError as e:
      result.error = f"not ready within {timeout_s}s: {e}"[:200]
    except Exception as e:  # noqa: BLE001 — surface everything else
      result.error = f"{type(e).__name__}: {e}"[:200]
    finally:
      if cleanup and result.claim_name:
        try:
          fleet.delete_sandbox(result.claim_name, namespace=NAMESPACE)
        except Exception:  # noqa: BLE001
          pass
  return result


# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

def percentile(values: list[float], p: float) -> float:
  if not values:
    return 0.0
  s = sorted(values)
  k = max(0, min(len(s) - 1, int(round((p / 100.0) * (len(s) - 1)))))
  return s[k]


def print_summary(results: list[ClaimResult], duration_s: float) -> None:
  n = len(results)
  ok = [r for r in results if r.error is None and r.ready_at is not None]
  err = [r for r in results if r.error is not None]
  lat = [r.latency_ms for r in ok if r.latency_ms is not None]

  print()
  print("=" * 78)
  print(f"STRESS SUMMARY  ({duration_s:.1f}s wall clock)")
  print("=" * 78)
  print(f"  claims attempted   : {n}")
  print(f"  claims succeeded   : {len(ok)}  ({100*len(ok)/max(1,n):.1f}%)")
  print(f"  claims failed      : {len(err)}")
  print(f"  achieved rate      : {n/duration_s:.1f}/s dispatched")
  if lat:
    print(f"  latency P50 (ms)   : {percentile(lat, 50):.1f}")
    print(f"  latency P90 (ms)   : {percentile(lat, 90):.1f}")
    print(f"  latency P99 (ms)   : {percentile(lat, 99):.1f}")
    print(f"  latency max (ms)   : {max(lat):.1f}")

  # Per-cluster breakdown (populated by resolver at dispatch time).
  by_cluster: dict[str, list[ClaimResult]] = {}
  for r in results:
    key = r.cluster or "(unresolved)"
    by_cluster.setdefault(key, []).append(r)
  print()
  print(f"  {'cluster':<20} {'total':>6} {'ok':>4} {'err':>4} {'P50':>7} {'P90':>7}")
  print("  " + "-" * 61)
  for cluster in sorted(by_cluster):
    rs = by_cluster[cluster]
    ok_rs = [r for r in rs if r.ready_at is not None]
    ls = [r.latency_ms for r in ok_rs if r.latency_ms is not None]
    p50 = f"{percentile(ls, 50):.0f}" if ls else "-"
    p90 = f"{percentile(ls, 90):.0f}" if ls else "-"
    print(f"  {cluster:<20} {len(rs):>6} {len(ok_rs):>4} {len(rs)-len(ok_rs):>4} "
          f"{p50:>7} {p90:>7}")
  print()
  if err:
    print(f"  first {min(3,len(err))} failures:")
    for r in err[:3]:
      print(f"    - {r.template} on {r.cluster or '?'}: {r.error}")


# ---------------------------------------------------------------------------
# Entrypoint
# ---------------------------------------------------------------------------

def main() -> int:
  ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
  ap.add_argument("--bucket", default=os.environ.get("FLEET_BUCKET"),
                  help="GCS bucket (default: $FLEET_BUCKET)")
  ap.add_argument("--project", default=os.environ.get("PROJECT"),
                  help="GCP project (default: $PROJECT). Omit + set --kind for kind mode.")
  ap.add_argument("--region", default=os.environ.get("REGION", "us-central1"),
                  help="GKE region for regional (Autopilot) clusters")
  ap.add_argument("--zone", default=os.environ.get("ZONE"),
                  help="GKE zone for zonal (Standard) clusters — overrides --region")
  ap.add_argument("--kind", action="store_true",
                  help="Use kind context naming (kind-<cluster>) instead of GKE")
  ap.add_argument("--rate", type=float, default=5.0)
  ap.add_argument("--duration", type=float, default=60.0)
  ap.add_argument("--concurrency", type=int, default=128,
                  help="Total ThreadPoolExecutor workers (soft ceiling)")
  ap.add_argument("--per-cluster-concurrency", type=int, default=20,
                  help="In-flight claim ceiling per cluster (prevents apiserver saturation)")
  ap.add_argument("--claim-timeout", type=float, default=90.0,
                  help="Seconds to wait for a claim to become Ready")
  ap.add_argument("--keep-claims", action="store_true",
                  help="Don't delete claims after Ready")
  ap.add_argument("--strategy", default="round-robin",
                  choices=["first", "round-robin", "hash"],
                  help="Multi-cluster resolution strategy (default: round-robin)")
  args = ap.parse_args()

  if not args.bucket:
    ap.error("--bucket or $FLEET_BUCKET is required")
  if not args.kind and not args.project:
    ap.error("--project or $PROJECT is required (or pass --kind for local dev)")

  # 1) Read assignment → target list.
  print(f"[setup] loading assignment targets from gs://{args.bucket}/fleet/assignments.json")
  targets = load_targets(args.bucket)
  # Get the current cluster set so we can prime kubeconfig up front.
  resolver = ClusterResolver(args.bucket)
  all_clusters = sorted({m.cluster for t in targets
                         for m in resolver.list_matches(t.template)})
  print(f"[setup] targets: {len(targets)} templates across {len(all_clusters)} clusters")
  for t in targets:
    matches = resolver.list_matches(t.template)
    hosts = ",".join(sorted({m.cluster for m in matches}))
    print(f"        - {t.template:<28} fleet-replicas={t.fleet_wide_replicas:<5} hosts={hosts}")

  # 2) Ensure kubeconfig has contexts for every target cluster (GKE only).
  if not args.kind:
    prime_kubeconfig(all_clusters, args.project, args.region, args.zone)

  # 3) Build a single FleetSandboxClient — the whole point of this rewrite.
  if args.kind:
    ctx_naming = kind_context_naming()
  else:
    location = args.zone or args.region
    ctx_naming = gke_context_naming(args.project, location)
  fleet = FleetSandboxClient(
      bucket=args.bucket,
      context_naming=ctx_naming,
      namespace=NAMESPACE,
      resolve_strategy=args.strategy,
  )

  # 4) Weighted rotation — templates with more fleet-wide replicas get more traffic.
  per_cluster_sems: dict[str, threading.Semaphore] = {
      c: threading.Semaphore(args.per_cluster_concurrency) for c in all_clusters
  }
  sem_lock = threading.Lock()

  weighted: list[ClaimTarget] = []
  for t in targets:
    weighted.extend([t] * t.fleet_wide_replicas)
  random.shuffle(weighted)

  print(f"[run] rate={args.rate}/s duration={args.duration:.0f}s "
        f"concurrency={args.concurrency} "
        f"per_cluster_concurrency={args.per_cluster_concurrency} "
        f"strategy={args.strategy} cleanup={not args.keep_claims}")

  # 5) Dispatch loop — one claim per `1/rate` seconds.
  interval = 1.0 / max(0.1, args.rate)
  start = time.time()
  deadline = start + args.duration
  results: list[ClaimResult] = []
  idx = 0

  with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as ex:
    futures = []
    next_fire = start
    last_progress = 0
    while time.time() < deadline:
      now = time.time()
      if now < next_fire:
        time.sleep(min(next_fire - now, 0.05))
        continue
      target = weighted[idx % len(weighted)]
      idx += 1
      futures.append(ex.submit(
          dispatch, fleet, per_cluster_sems, sem_lock,
          args.per_cluster_concurrency, target,
          not args.keep_claims, args.claim_timeout,
      ))
      next_fire += interval
      elapsed = int(now - start)
      if elapsed >= last_progress + 10:
        done = sum(1 for f in futures if f.done())
        print(f"[run] t+{elapsed:>3}s  dispatched={idx}  completed={done}  "
              f"in-flight={len(futures)-done}")
        last_progress = elapsed

    print(f"[run] dispatch done at t+{time.time() - start:.1f}s; waiting for "
          f"{sum(1 for f in futures if not f.done())} in-flight "
          f"(up to --claim-timeout={args.claim_timeout:.0f}s)")
    for f in concurrent.futures.as_completed(futures):
      results.append(f.result())

  print_summary(results, time.time() - start)
  return 0


if __name__ == "__main__":
  sys.exit(main())
