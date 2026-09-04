#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
"""Prove a FleetSpec plans to the totals you intended — offline.

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

TWO MODES, picked from the spec rather than from a flag:

  hub-driven  (`min_clusters: N > 0`)
      The spec names no clusters, so `--capacities` is REQUIRED — it stands in
      for what the members would publish. `min_clusters` pins model i to
      sorted-fresh cluster i % N, which gives a per-cluster total the spec
      implies on its own; the check is planned == that total, per cluster.

  spec-driven (`min_clusters: 0`, `cluster_weights` non-empty)
      The spec names its clusters and the placement policy picks where each
      pool lands, so there is no per-cluster figure to check against — an
      `image-affinity` spec is deliberately letting the image hash decide.
      `--capacities` is optional (it defaults to the named clusters); the check
      is the fleet total and that no named cluster is left empty. The
      per-cluster distribution is reported, not asserted.

Usage:
  # spec-driven: demo/fleet-spec-xs.yaml names three clusters and uses
  # image-affinity, so this runs as-is
  ./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml

  # state what is actually published instead, and see what it would really do
  ./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml \
    --capacities std-multi-1=199000,std-multi-2=199000,std-multi-3=199000

  # either mode: what happens if a member never publishes capacity?
  ./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml --omit std-multi-3

  # hub-driven spec (cluster_weights: {}, min_clusters: 6) — --capacities is
  # not optional there, because the spec names nobody
  ./demo/preflight-cp-plan.py my-hub-spec.yaml \
    --capacities cluster-a=199000,cluster-b=199000,cluster-c=199000,\
cluster-d=199000,cluster-e=199000,cluster-f=199000

Exit 0 = the plan matches what the spec describes. Exit 1 = it does not, and
the diff is printed per cluster. Exit 2 = the invocation itself is wrong.
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
                 help="comma-separated name=value standing in for what the "
                      "members publish. REQUIRED for a hub-driven spec "
                      "(min_clusters>0), which names no clusters. Optional for "
                      "a spec that names its own cluster_weights, where it "
                      "defaults to those clusters.")
  p.add_argument("--omit", default="",
                 help="comma-separated clusters that publish NO capacity "
                      "property (weight falls back to 1.0) -- use to see the "
                      "blast radius of a half-configured member")
  p.add_argument("--drop", default="",
                 help="comma-separated clusters that do not appear on the hub "
                      "at all. Always exits 1 -- a fleet with a member missing "
                      "is not the fleet the spec names, whatever the totals "
                      "come out to.")
  args = p.parse_args()

  # Same reasoning as the --capacities parse below: a bad spec is a wrong
  # invocation (exit 2), not a plan that came out wrong (exit 1), and a raw
  # pydantic/yaml traceback out of a preflight tool reads as the tool being
  # broken. The validation error itself is the useful part, so it is printed
  # rather than summarised.
  try:
    spec = planner.FleetSpec(**yaml.safe_load(args.spec.read_text()))
  except (OSError, yaml.YAMLError, TypeError, ValueError) as e:
    print(f"error: {args.spec} is not a loadable FleetSpec:\n{e}",
          file=sys.stderr)
    return 2

  n = spec.min_clusters
  # min_clusters>0 is what makes a per-cluster total derivable; see the two
  # modes in the module docstring.
  hub_driven = bool(n)

  # The spec's OWN arithmetic, hub-driven only: min_clusters pins model i to
  # sorted-by-name fresh cluster i % n, so the intended total for slot j is the
  # sum of target_tasks over models j, j+n, j+2n, ... Derived, never restated
  # by the operator -- restating it would only prove the operator can add.
  slot_totals = [
      sum(m.target_tasks for m in spec.models[j::n]) for j in range(n)
  ] if hub_driven else []

  if args.capacities:
    caps = {}
    for kv in args.capacities.split(","):
      k, _, v = kv.strip().partition("=")
      if not k or not v:
        print(f"error: --capacities entry {kv.strip()!r} is not NAME=VALUE",
              file=sys.stderr)
        return 2
      # int() raises on anything non-numeric, and the operator's most likely
      # slip here is a units suffix -- 199k, 199000i, 2e5 -- copied out of a
      # sizing note. An uncaught ValueError reports it as a traceback from
      # inside a preflight tool, which reads as the tool being broken rather
      # than the flag being wrong.
      try:
        caps[k] = int(v)
      except ValueError:
        print(f"error: --capacities entry {kv.strip()!r} has a non-integer "
              f"value {v!r}. This stands in for the published "
              f"{inventory.PROP_SANDBOX_CAPACITY} property, which is a plain "
              "sandbox count -- no units, no suffix.", file=sys.stderr)
        return 2
      if caps[k] < 0:
        print(f"error: --capacities entry {kv.strip()!r} is negative. A "
              "capacity is a count; 0 is allowed and means a cluster that "
              "publishes no headroom.", file=sys.stderr)
        return 2
    names = sorted(caps)
  elif spec.cluster_weights:
    # The spec names its clusters, so there is nothing to stand in for. Publish
    # the same capacity everywhere and let cluster_weights carry the
    # distribution -- that is what the spec is asking for. max_concurrent is
    # used as the value because it is by construction large enough never to be
    # the binding constraint, so the report shows the policy's choice rather
    # than an artificial capacity ceiling.
    names = sorted(spec.cluster_weights)
    caps = {nm: spec.max_concurrent for nm in names}
  else:
    print("error: --capacities is required for this spec (min_clusters="
          f"{n} with cluster_weights empty, so the spec names no clusters at "
          "all -- that is the whole point of a hub-driven spec)",
          file=sys.stderr)
    return 2

  if hub_driven and len(names) != n:
    print(f"error: {len(names)} capacities but min_clusters={n}",
          file=sys.stderr)
    return 2

  omit = {c for c in args.omit.split(",") if c}
  drop = {c for c in args.drop.split(",") if c}

  # A name in --omit/--drop that is not in the cluster set is a typo, and a
  # silent one: the tool would model the fleet the operator did NOT ask about
  # and then print OK. Both flags exist to make a failure visible, so failing
  # to apply one has to be louder than the failure it was simulating.
  unknown = sorted((omit | drop) - set(names))
  if unknown:
    print(f"error: --omit/--drop names {unknown} are not in the cluster set "
          f"{names}. Nothing would have been omitted or dropped, and the run "
          "would have reported on a healthy fleet.", file=sys.stderr)
    return 2

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
  intended = (dict(zip(fresh, slot_totals))
              if hub_driven and len(fresh) == n else {})

  # One row per cluster ANY of the three sources knows about, not just the ones
  # the planner emitted an assignment for. A --drop'ped cluster is absent from
  # `profiles`, so it is absent from the registry and from `fresh`; iterating
  # `assignments.clusters` alone would drop it from the report entirely under a
  # hub-driven spec -- the opposite of what --drop was asked to demonstrate.
  row = "  {:<16}{:>12}{:>8}{:>12}{:>12}{:>10}  {}"
  print(row.format("cluster", "weight", "pools", "planned", "intended",
                   "delta", "note"))
  ok = True
  total = 0
  unplaced: list[str] = []
  absent: list[str] = []
  for name in sorted(set(assignments.clusters) | set(fresh) | set(names)):
    ca = assignments.clusters.get(name)
    got = sum(pool.replicas for pool in ca.pools) if ca else 0
    total += got
    want = intended.get(name)
    delta = "" if want is None else f"{got - want:+,}"
    if want is not None and got != want:
      ok = False
    notes = []
    if name in drop:
      # Not idle. Not there. Worth its own word, because the two are fixed by
      # completely different things.
      notes.append("NOT ON HUB")
      absent.append(name)
    if got == 0:
      notes.append("no pools")
      unplaced.append(name)
    entry = registry.clusters.get(name)
    print(row.format(name, f"{entry.weight:,.0f}" if entry else "-",
                     len(ca.pools) if ca else 0, f"{got:,}",
                     f"{want:,}" if want is not None else "?", delta,
                     ", ".join(notes)))

  # A cluster that received no pools. Under a hub-driven spec that is an error
  # -- min_clusters says every slot is filled. Under a spec-driven one it can be
  # a legitimate outcome (image-affinity hashes six templates onto three
  # clusters; nothing promises a cluster wins one), so it is reported and left
  # to the operator rather than failing the run.
  #
  # "Received no pools" has two shapes and this used to catch only one: a fresh
  # cluster missing from assignments entirely. The other is an assignment that
  # is present but EMPTY, which is exactly what a named-but-not-fresh cluster
  # gets -- and because its name was in the dict, it counted as placed. The run
  # then printed "every named cluster is placed" over a table showing it with
  # zero. A cluster holding nothing is not placed, however it got there.
  if hub_driven and unplaced:
    ok = False

  print(row.format("FLEET", "", "", f"{total:,}", f"{spec.max_concurrent:,}",
                   f"{total - spec.max_concurrent:+,}", ""))
  print()

  # Checked in BOTH modes, and before the totals. Hub-driven already fails a
  # dropped cluster, but only via len(fresh) != n -> INCONCLUSIVE, which blames
  # the round-robin for something much simpler. Spec-driven did not fail it at
  # all: the surviving clusters absorb the budget, the fleet total comes out at
  # exactly max_concurrent, and the run reports OK for a fleet that is a member
  # short of the one being asked about. The total is the wrong thing to check
  # this with, so it is checked separately from it.
  if absent:
    print(f"MISSING FROM HUB: {', '.join(absent)} published no ClusterProfile "
          "at all, so everything above is a plan for a smaller fleet than the "
          "spec names. The fleet total may still come out exactly right — the "
          "remaining clusters absorb the budget — and that is precisely why it "
          "cannot be the check.")
    return 1

  if hub_driven:
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

  # Spec-driven. The per-cluster split is the policy's to choose, so the only
  # things assertable are that the fleet total is what was asked for and that
  # the plan is non-degenerate.
  if total != spec.max_concurrent:
    print("MISMATCH: the fleet total is not max_concurrent. With "
          f"max_pool={spec.max_pool} and {sum(len(ca.pools) for ca in assignments.clusters.values())} "
          "pools the per-pool cap may be binding before the budget is spent — "
          "raise max_pool, add clusters, or lower max_concurrent.")
    return 1
  if unplaced:
    print(f"OK (with a caveat): the fleet total is exactly "
          f"{spec.max_concurrent:,}, but {', '.join(unplaced)} received no "
          f"pools. Under placement_policy={spec.placement_policy} that can be "
          "correct — the distribution is chosen per pool, not per cluster — "
          "but an idle member in a fleet you are paying for is worth a look.")
    return 0
  print(f"OK: every named cluster is placed and the fleet total is exactly "
        f"{spec.max_concurrent:,}. The per-cluster split above is "
        f"placement_policy={spec.placement_policy}'s to choose; it is reported, "
        "not asserted.")
  return 0


if __name__ == "__main__":
  raise SystemExit(main())
