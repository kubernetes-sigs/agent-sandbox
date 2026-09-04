#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Drive the `clusterprofile` inventory provider directly against a hub, with no
# GCS in the loop.
#
# `fleetctl show-registry --inventory=clusterprofile` also works, but it builds
# a GCS client first (that is where cluster_weights lives), so it needs a
# bucket. This script isolates the Kubernetes read path, which is the part
# every unit test fakes out.
#
#   ./demo/cp-registry.py
#   ./demo/cp-registry.py --namespace fleet-system --cluster-manager gke-fleet
#   ./demo/cp-registry.py --require-heartbeat
#   ./demo/cp-registry.py --plan demo/fleet-spec-clusterprofile-e2e.yaml

from __future__ import annotations

import argparse
import json
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "python"))

from agent_sandbox_fleet import inventory, planner  # noqa: E402


def main() -> int:
  p = argparse.ArgumentParser(description=__doc__)
  p.add_argument("--kubeconfig")
  p.add_argument("--context")
  p.add_argument("--namespace", default=inventory.CLUSTERPROFILE_NAMESPACE)
  p.add_argument("--api-version", default=inventory.CLUSTERPROFILE_VERSION,
                 help="Served ClusterProfile version (upstream deprecates "
                      "v1alpha1 in favour of v1alpha2)")
  p.add_argument("--cluster-manager")
  p.add_argument("--require-heartbeat", action="store_true")
  p.add_argument("--weights", help='JSON dict of operator weight overrides, '
                                   'e.g. \'{"cluster-a": 2.0}\'')
  p.add_argument("--plan", help="Also run the planner against this FleetSpec "
                                "YAML and print the resulting assignment")
  p.add_argument("--raw", action="store_true",
                 help="Dump the raw ClusterProfile objects and exit")
  args = p.parse_args()

  prov = inventory.ClusterProfileInventory(
      version=args.api_version,
      namespace=args.namespace,
      kubeconfig=args.kubeconfig,
      context=args.context,
      cluster_manager=args.cluster_manager,
      require_heartbeat=args.require_heartbeat,
  )

  if args.raw:
    print(json.dumps(prov.list_profiles(), indent=2, default=str))
    return 0

  # Resolve weights BEFORE loading, so the table shows the weights that are
  # actually in force. A spec's cluster_weights override the capacity-derived
  # ones, and printing the derived set next to a plan built from the spec set
  # is just a lie with extra steps.
  spec = None
  if args.plan:
    import yaml
    spec = planner.FleetSpec.model_validate(
        yaml.safe_load(pathlib.Path(args.plan).read_text()))

  if args.weights:
    weights = json.loads(args.weights)
    origin = "--weights override"
  elif spec is not None and spec.cluster_weights:
    weights = spec.cluster_weights
    origin = f"cluster_weights from {args.plan}"
  else:
    weights = {}
    origin = "derived from agents.x-k8s.io/sandbox-capacity"

  reg = prov.load(weights)
  print(f"weights: {origin}\n")

  if not reg.clusters:
    print("no ClusterProfiles found — check --namespace / --cluster-manager",
          file=sys.stderr)
    return 1

  fmt = "{:<16} {:>8} {:>7} {:>9} {:>9} {:>8} {:>9} {:>6}"
  print(fmt.format("cluster", "weight", "age_s", "wp_depth", "wp_ready",
                   "claims", "pressure", "fresh"))
  print(fmt.format(*["-" * n for n in (16, 8, 7, 9, 9, 8, 9, 6)]))
  no_hb = prov.clusters_without_heartbeat
  for name in sorted(reg.clusters):
    c = reg.clusters[name]
    if c.report_age_s >= 1e8:
      age = "STALE"
    elif name in no_hb:
      # Not "0 seconds old" — no age was measurable at all.
      age = "cond?"
    else:
      age = f"{c.report_age_s:.0f}"
    pressure = "-" if c.node_pressure_score is None else f"{c.node_pressure_score:.2f}"
    print(fmt.format(name, f"{c.weight:g}", age, c.warmpool_depth,
                     c.warmpool_ready, c.active_claims, pressure,
                     "yes" if c.report_age_s <= reg.max_report_age_s else "NO"))

  fresh = [c.name for c in reg.fresh()]
  print(f"\nplacement-eligible: {fresh or 'NONE'}")
  if no_hb:
    print(f"age 'cond?' = liveness inferred from ControlPlaneHealthy, no "
          f"heartbeat published: {sorted(no_hb)}")

  if spec is not None:
    assn = planner.plan(spec, reg)
    print("\nassignment:")
    print(json.dumps(assn.model_dump()["clusters"], indent=2))

  return 0


if __name__ == "__main__":
  sys.exit(main())
