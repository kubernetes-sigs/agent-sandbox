#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
"""Prove a hub-driven FleetSpec plans to the totals you intended — offline.

WHY THIS EXISTS: a spec with `cluster_weights: {}` moves the budget split out
of the file and into whatever the members publish as
`agents.x-k8s.io/sandbox-capacity`. That is the point of the ClusterProfile
integration, and it is also the failure mode: the spec no longer states the
answer, so nothing catches a wrong capacity until a six-cluster run has already
been spent. A spec generator can assert published == target at generation time;
this checks the same invariant against the live hub.

It runs the REAL `ClusterProfileInventory.load()` and the REAL `planner.plan()`
— only `list_profiles()` is substituted, so hamilton_split, compute_replicas,
the spread-first pre-pass and the min_clusters round-robin are all exercised as
they will be on the day. No cluster, no hub, no credentials.

Usage:
  # capacities default to "whatever makes the spec exact", i.e. the check is
  # "does the intended publication reproduce the intended plan"
  ./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml

  # or state what is actually published, and see what it would really do
  ./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml \
    --capacities cluster-a=199000,cluster-b=199000,...

  # what happens if F never publishes?
  ./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml --omit cluster-f

Exit 0 = the plan matches the spec's own arithmetic. Exit 1 = it does not, and
the diff is printed per cluster.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent / "python"))

import yaml  # noqa: E402

from agent_sandbox_fleet import inventory, planner  # noqa: E402


def _profile(name: str, capacity: int | None, now: str) -> dict:
  """A ClusterProfile shaped the way fleet_member's publisher emits one."""
  props = [
      {"name": inventory.PROP_HEARTBEAT, "value": now},
      # depth/ready are read by the scored selectors. Zero is the honest value
      # for a fleet that is empty at apply time, which is the precondition the
      # runbook enforces in Phase 0.
      {"name": inventory.PROP_WARMPOOL_DEPTH, "value": "0"},
      {"name": inventory.PROP_WARMPOOL_READY, "value": "0"},
      {"name": inventory.PROP_ACTIVE_CLAIMS, "value": "0"},
  ]
  if capacity is not None:
    props.append(
        {"name": inventory.PROP_SANDBOX_CAPACITY, "value": str(capacity)})
  return {
      "metadata": {"name": name},
      "status": {
          "properties": props,
          "conditions": [
              {"type": inventory.COND_CONTROL_PLANE_HEALTHY, "status": "True"},
              {"type": inventory.COND_JOINED, "status": "True"},
          ],
      },
  }


class _FakeHub(inventory.ClusterProfileInventory):
  """Real load()/_to_planner_cluster()/_weight_for(); canned list_profiles()."""

  def __init__(self, profiles: list[dict]):
    self._profiles = profiles
    self._version = inventory.CLUSTERPROFILE_VERSION
    self._namespace = inventory.CLUSTERPROFILE_NAMESPACE
    self._cluster_manager = None
    self._api = None
    self._no_heartbeat: set[str] = set()

  def list_profiles(self) -> list[dict]:
    return self._profiles


def main() -> int:
  p = argparse.ArgumentParser()
  p.add_argument("spec", type=pathlib.Path)
  p.add_argument("--capacities", default="",
                 help="comma-separated name=value. Default: the per-cluster "
                      "totals the spec itself implies, i.e. the exact-A/B case.")
  p.add_argument("--omit", default="",
                 help="comma-separated clusters that publish NO capacity "
                      "property (weight falls back to 1.0) -- use to see the "
                      "blast radius of a half-configured member")
  p.add_argument("--drop", default="",
                 help="comma-separated clusters that do not appear on the hub "
                      "at all")
  args = p.parse_args()

  spec = planner.FleetSpec(**yaml.safe_load(args.spec.read_text()))
  n = spec.min_clusters
  if not n:
    print("error: this preflight is for min_clusters>0 hub-driven specs; "
          "with 0 the cluster set is scored, not round-robin, and there is no "
          "spec-implied per-cluster total to check against", file=sys.stderr)
    return 2

  # The spec's OWN arithmetic: min_clusters pins model i to sorted-by-name
  # fresh cluster i % n, so the intended total for slot j is the sum of
  # target_tasks over models j, j+n, j+2n, ... Derived, never restated by the
  # operator -- restating it would only prove the operator can add.
  slot_totals = [
      sum(m.target_tasks for m in spec.models[j::n]) for j in range(n)
  ]

  if args.capacities:
    caps = {}
    for kv in args.capacities.split(","):
      k, _, v = kv.strip().partition("=")
      caps[k] = int(v)
    names = sorted(caps)
  else:
    print("error: --capacities is required (the spec names no clusters when "
          "cluster_weights is empty -- that is the whole point of it)",
          file=sys.stderr)
    return 2

  if len(names) != n:
    print(f"error: {len(names)} capacities but min_clusters={n}",
          file=sys.stderr)
    return 2

  omit = {c for c in args.omit.split(",") if c}
  drop = {c for c in args.drop.split(",") if c}
  now = _dt.datetime.now(_dt.timezone.utc).isoformat()

  profiles = [
      _profile(nm, None if nm in omit else caps[nm], now)
      for nm in names if nm not in drop
  ]

  registry = _FakeHub(profiles).load(spec.cluster_weights)
  assignments = planner.plan(spec, registry)

  # ------------------------------------------------------------------ report
  print(f"spec        {args.spec}")
  print(f"            max_concurrent={spec.max_concurrent:,} max_pool={spec.max_pool} "
        f"min_clusters={n} policy={spec.placement_policy}")
  print(f"            cluster_weights={'EMPTY (hub-driven)' if not spec.cluster_weights else spec.cluster_weights}")
  print(f"hub         {len(profiles)} profiles, "
        f"{len(registry.fresh())} fresh"
        + (f", omitting capacity on {sorted(omit)}" if omit else "")
        + (f", absent: {sorted(drop)}" if drop else ""))
  print()

  fresh = sorted(c.name for c in registry.fresh())
  intended = dict(zip(fresh, slot_totals)) if len(fresh) == n else {}

  print(f"  {'cluster':<16}{'weight':>12}{'pools':>8}{'planned':>12}"
        f"{'intended':>12}{'delta':>10}")
  ok = True
  total = 0
  for name in sorted(assignments.clusters):
    ca = assignments.clusters[name]
    got = sum(pool.replicas for pool in ca.pools)
    total += got
    want = intended.get(name)
    delta = "" if want is None else f"{got - want:+,}"
    if want is not None and got != want:
      ok = False
    print(f"  {name:<16}{registry.get(name).weight:>12,.0f}"
          f"{len(ca.pools):>8}{got:>12,}"
          f"{(f'{want:,}' if want is not None else '?'):>12}{delta:>10}")

  missing = [c for c in fresh if c not in assignments.clusters]
  for name in missing:
    ok = False
    print(f"  {name:<16}{'-':>12}{0:>8}{0:>12}{'-':>12}{'NOT PLACED':>10}")

  print(f"  {'FLEET':<16}{'':>12}{'':>8}{total:>12,}"
        f"{spec.max_concurrent:>12,}{total - spec.max_concurrent:>+10,}")
  print()

  if not intended:
    print(f"INCONCLUSIVE: {len(fresh)} fresh clusters but min_clusters={n}. "
          "The round-robin lands on a different set than the spec was "
          "generated for, so every per-model target_tasks is on the wrong "
          "cluster. This is the failure the runbook's fresh=true check "
          "prevents -- it is not a rounding issue.")
    return 1
  if ok and total == spec.max_concurrent:
    print("OK: published capacity reproduces the spec's own arithmetic "
          "exactly. Safe to apply.")
    return 0
  print("MISMATCH: the plan is not what the spec's target_tasks describe. "
        "Applying this places a different number of sandboxes than intended.")
  return 1


if __name__ == "__main__":
  raise SystemExit(main())
