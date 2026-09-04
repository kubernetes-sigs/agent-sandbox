#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Play the FLEET-MEMBER half: publish capacity onto ClusterProfile status via
# Server-Side Apply, using the same ClusterProfilePublisher the real member
# uses. Only the capacity numbers are synthetic -- the write path is the
# production one, field manager and all.
#
#   ./demo/publish-clusterprofile.py                       # 3 clusters, healthy
#   ./demo/publish-clusterprofile.py --stale cluster-c  # 10m-old heartbeat
#   ./demo/publish-clusterprofile.py --no-pressure cluster-c
#   ./demo/publish-clusterprofile.py --field-manager rival  # SSA conflict demo
#
# Pair with demo/seed-clusterprofiles.sh (the cluster-manager half) and read
# the result back with demo/cp-registry.py.

from __future__ import annotations

import argparse
import datetime as _dt
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "python"))

from agent_sandbox_fleet import inventory  # noqa: E402
from agent_sandbox_fleet.publisher import (  # noqa: E402
    DEFAULT_FIELD_MANAGER,
    ClusterProfilePublisher,
)

# name -> (depth, ready, claims, p90_ms, pressure, sandbox_capacity)
FIXTURE = {
    "cluster-a": (1000, 900, 42, 530.0, 0.25, 199000),
    "cluster-b": (500, 500, 10, 210.0, 0.10, 199000),
    "cluster-c": (0, 0, 0, 0.0, 0.05, 99000),
}

# Used for any cluster name not in FIXTURE. An idle, healthy, unremarkable
# cluster -- so that whatever --capacity says is the only thing distinguishing
# it, which is what makes a capacity-reasoning demo legible.
DEFAULT_STATE = (0, 0, 0, 0.0, 0.05, 200)


def _iso(offset_s: float = 0.0) -> str:
  """An RFC3339 UTC timestamp `offset_s` seconds in the past.

  The offset is how --stale is built: a real heartbeat, just an old one, so
  the planner's freshness test is exercised rather than its parser.
  """
  t = _dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(seconds=offset_s)
  return t.isoformat().replace("+00:00", "Z")


def main() -> int:
  """Publish one synthetic capacity report per named cluster.

  Returns 0 only if every publish succeeded. A 409 is reported as the
  distinct, useful outcome it is -- SSA is tracking ownership -- but it still
  counts as a failure, because the values on the hub are not the ones asked
  for.
  """
  p = argparse.ArgumentParser(description=__doc__)
  p.add_argument("--kubeconfig")
  p.add_argument("--context")
  p.add_argument("--namespace", default=inventory.CLUSTERPROFILE_NAMESPACE)
  p.add_argument("--api-version", default=inventory.CLUSTERPROFILE_VERSION,
                 help="Served ClusterProfile version (upstream deprecates "
                      "v1alpha1 in favour of v1alpha2)")
  p.add_argument("--field-manager", default=DEFAULT_FIELD_MANAGER)
  p.add_argument("--force-conflicts", action="store_true")
  p.add_argument("--stale", metavar="CLUSTER", default="",
                 help="Publish a 10-minute-old heartbeat for this cluster")
  p.add_argument("--no-pressure", metavar="CLUSTER", default="",
                 help="Publish no node-pressure for this cluster, simulating a "
                      "failed measurement. Must arrive as None, never 0.0.")
  p.add_argument("--no-capacity", metavar="CLUSTER", default="",
                 help="Publish no sandbox-capacity, so the planner cannot "
                      "derive a weight and falls back to 1.0")
  p.add_argument("--clusters", nargs="*", default=sorted(FIXTURE),
                 help="Cluster names. Names outside the built-in fixture are "
                      "allowed and get DEFAULT_STATE; pair with --capacity.")
  p.add_argument("--capacity", type=int, default=None,
                 help="Override sandbox-capacity for every named cluster. "
                      "This is the knob the planner turns into a placement "
                      "weight, so it is how you demo capacity reasoning "
                      "without touching a real member.")
  args = p.parse_args()

  rc = 0
  for name in args.clusters:
    # An arbitrary name is deliberately not an error: the point of a synthetic
    # cluster is to stand in for one that does not exist, and requiring it to
    # be pre-declared in a fixture defeats that.
    depth, ready, claims, p90, pressure, capacity = FIXTURE.get(
        name, DEFAULT_STATE)
    if args.capacity is not None:
      capacity = args.capacity
    report = {
        "cluster": name,
        "updated_at": _iso(600 if name == args.stale else 0),
        "warmpool_depth": depth,
        "warmpool_ready": ready,
        "active_claims": claims,
        "claim_p90_ms": p90,
        "node_pressure_score": None if name == args.no_pressure else pressure,
    }
    pub = ClusterProfilePublisher(
        name,
        version=args.api_version,
        namespace=args.namespace,
        kubeconfig=args.kubeconfig,
        context=args.context,
        field_manager=args.field_manager,
        force=args.force_conflicts,
        sandbox_capacity=None if name == args.no_capacity else capacity,
    )
    pub.assert_ssa_configured()
    try:
      pub.publish(report)
    except Exception as e:
      # A 409 here is the useful outcome of --field-manager: it means SSA is
      # actually tracking ownership rather than letting writers clobber.
      status = getattr(e, "status", None)
      if status == 409:
        print(f"{name}: CONFLICT — another field manager owns these fields. "
              f"Re-run with --force-conflicts to take them.", file=sys.stderr)
      else:
        print(f"{name}: publish failed: {e}", file=sys.stderr)
      rc = 1
      continue
    print(f"{name}: published as {args.field_manager!r}"
          f"{' (heartbeat 10m old)' if name == args.stale else ''}"
          f"{' (no pressure)' if name == args.no_pressure else ''}")

  print("\nread it back:  ./demo/cp-registry.py")
  return rc


if __name__ == "__main__":
  sys.exit(main())
