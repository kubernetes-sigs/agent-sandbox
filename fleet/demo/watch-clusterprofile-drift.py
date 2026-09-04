#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Sample, once per interval, the three numbers that should agree and measure
# how far apart they drift under density.
#
# WHAT THIS IS TESTING
#   The member stamps `agents.x-k8s.io/heartbeat` with _now_ISO() every tick,
#   unconditionally (fleet_member.py:450). It derives warmpool-depth and
#   warmpool-ready from SandboxWarmPool `.status.replicas` / `.status.
#   readyReplicas` (fleet_member.py:461-462). Those two facts are fine at 40
#   sandboxes and structurally unsound at 200k, because SWP status aggregation
#   is known to stall for minutes at density -- one cluster sat frozen at
#   62,604 for 14 minutes while it was still filling.
#
#   Put together: the heartbeat proves the PUBLISHER is alive, not that its
#   NUMBERS are current. `--require-heartbeat` therefore does not do the job it
#   looks like it does. A cluster whose reported capacity is 14 minutes behind
#   reality is, to the planner, indistinguishable from one reporting live
#   numbers -- it is fresh by the only test we apply.
#
#   Leg 4 of the E2E proved we evict a member that STOPS reporting. This asks
#   the harder question: do we notice a member that keeps reporting and is
#   simply wrong? Predicted answer is no, and this measures the size of the
#   window in which it is wrong.
#
# THE THREE NUMBERS
#   published   ClusterProfile status on the hub -- what the planner acts on.
#   observed    Sum of SWP status on the member -- what the member reads.
#   actual      apiserver_storage_objects from the member's apiserver -- what
#               exists in etcd.
#
#   published lagging observed  => publish path (interval, SSA, throttling).
#   observed lagging actual     => SWP status aggregation, the known defect.
#   Both, while the heartbeat stays green, is the finding.
#
# WHY apiserver_storage_objects AND NOT A LIST
#   A full Sandbox or Pod list at 200k takes ~5 minutes and outlives the
#   apiserver's continue-token TTL -- _node_pressure() already documents dying
#   with 410 Gone doing exactly that. This metric is an etcd-derived counter:
#   one request, O(1), exact. It is refreshed on the apiserver's own poll
#   interval (~1 min), so treat it as accurate but up to a minute coarse --
#   which is still an order of magnitude finer than the drift being measured.
#
# USAGE
#   ./demo/watch-clusterprofile-drift.py \
#       --hub-context $HUB_CTX --member-context $MEMBER_CTX \
#       --cluster cluster-a --interval 30 --out drift-200k.csv
#
#   Start it BEFORE the fill and leave it running through the drain.

from __future__ import annotations

import argparse
import csv
import datetime as _dt
import pathlib
import re
import subprocess
import sys
import time

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "python"))

from agent_sandbox_fleet import inventory  # noqa: E402
from agent_sandbox_fleet.hubauth import load_hub_configuration  # noqa: E402

# IMPORTED, not restated. The first cut of this script hand-copied the label as
# "fleet.agents.x-k8s.io/managed" against the real
# "fleet.agent-sandbox.io/managed", and a label selector that matches nothing
# does not error -- it returns an empty list, which sums to 0. A monitoring
# tool that silently reports zero is worse than one that crashes, and this
# particular zero looks exactly like the freeze the script exists to detect.
from agent_sandbox_fleet.fleet_member import (  # noqa: E402
    CRD_GROUP,
    CRD_VERSION,
    MANAGED_LABEL,
    POOL_PLURAL,
)

# Resources worth counting. Sandboxes are the workload; pods are the thing that
# actually has to schedule; warmpools tell us the plan landed at all.
TRACKED = {
    "sandboxes.agents.x-k8s.io": "sbx",
    "pods": "pods",
    "sandboxwarmpools.extensions.agents.x-k8s.io": "swp",
}
# The value pattern accepts a leading '-' deliberately. Without it the -1
# sentinel does not match at all and the metric reads as absent, which is the
# same failure wearing a different hat: a blank column instead of a wrong one.
# Match it, then reject it below, so "unknown" is a decision and not an
# accident of parsing.
_METRIC = re.compile(
    r'^apiserver_storage_objects\{[^}]*resource="([^"]+)"[^}]*\}\s+(-?[0-9.e+]+)')


def _now() -> float:
  return time.time()


def _parse_iso(s: str) -> float | None:
  if not s:
    return None
  try:
    t = _dt.datetime.fromisoformat(s.replace("Z", "+00:00"))
  except ValueError:
    return None
  if t.tzinfo is None:
    t = t.replace(tzinfo=_dt.timezone.utc)
  return t.timestamp()


def _api(kubeconfig, context):
  from kubernetes import client as k8s_client
  cfg = load_hub_configuration(
      kubeconfig=kubeconfig, context=context, token_source="kubeconfig")
  return k8s_client.CustomObjectsApi(k8s_client.ApiClient(configuration=cfg))


def _published(api, args) -> dict:
  """What the planner sees. Missing properties are reported as None, never 0 --
  a zero here would read as 'the cluster is empty' rather than 'we did not
  measure it', which is the distinction the whole run is about."""
  obj = api.get_namespaced_custom_object(
      group=inventory.CLUSTERPROFILE_GROUP, version=args.api_version,
      namespace=args.hub_namespace, plural=inventory.CLUSTERPROFILE_PLURAL,
      name=args.cluster)
  props = {p["name"]: p.get("value")
           for p in ((obj.get("status") or {}).get("properties") or [])}

  def num(key, cast=int):
    v = props.get(key)
    try:
      return cast(v)
    except (TypeError, ValueError):
      return None

  hb = _parse_iso(props.get(inventory.PROP_HEARTBEAT, "") or "")
  return {
      "hb_age": None if hb is None else _now() - hb,
      "pub_depth": num(inventory.PROP_WARMPOOL_DEPTH),
      "pub_ready": num(inventory.PROP_WARMPOOL_READY),
      "pub_capacity": num(inventory.PROP_SANDBOX_CAPACITY),
  }


def _observed(api, args) -> dict:
  """What the member reads: the same label-scoped SWP list fleet_member.py
  does, summed the same way. O(pools), safe at any density."""
  resp = api.list_namespaced_custom_object(
      group=CRD_GROUP, version=CRD_VERSION, namespace=args.namespace,
      plural=POOL_PLURAL, label_selector=f"{MANAGED_LABEL}=true")
  items = resp.get("items", [])
  depth = ready = 0
  for pool in items:
    st = pool.get("status") or {}
    depth += int(st.get("replicas", 0) or 0)
    ready += int(st.get("readyReplicas", 0) or 0)
  return {"pools": len(items), "obs_depth": depth, "obs_ready": ready}


def _actual(args) -> dict:
  """Ground truth from etcd. Shells out to kubectl on purpose: `get --raw` is
  stable across every client version, whereas ApiClient.call_api's signature
  is not, and this is the one call that must not break mid-run."""
  out = {v: None for v in TRACKED.values()}
  cmd = ["kubectl", "get", "--raw", "/metrics"]
  if args.member_context:
    cmd[1:1] = ["--context", args.member_context]
  if args.kubeconfig:
    cmd[1:1] = ["--kubeconfig", args.kubeconfig]
  try:
    txt = subprocess.run(cmd, capture_output=True, text=True, timeout=120,
                         check=True).stdout
  except Exception as e:
    print(f"  (metrics scrape failed: {e})", file=sys.stderr)
    return out
  for line in txt.splitlines():
    m = _METRIC.match(line)
    if m and m.group(1) in TRACKED:
      v = int(float(m.group(2)))
      # -1 is the apiserver's sentinel for "I do not know this count", not a
      # count. Observed live: `sandboxes.agents.x-k8s.io` went to -1 partway
      # through a 180k fill while `pods` stayed accurate. Recording it as a
      # number is worse than recording nothing, because a constant -1 makes
      # the movement delta zero, which makes every frozen counter look
      # justified and suppresses the exact finding this script exists for.
      out[TRACKED[m.group(1)]] = None if v < 0 else v
  return out


class _Freeze:
  """Tracks how long a counter has been pinned at one value."""

  def __init__(self):
    self.value = None
    self.since = None
    self.worst = 0.0

  def update(self, value, now: float) -> float:
    if value is None:
      return 0.0
    if value != self.value:
      self.value, self.since = value, now
      return 0.0
    held = now - (self.since or now)
    self.worst = max(self.worst, held)
    return held


def main() -> int:
  p = argparse.ArgumentParser(description=__doc__)
  p.add_argument("--kubeconfig")
  p.add_argument("--hub-context", required=True)
  p.add_argument("--member-context", required=True)
  p.add_argument("--cluster", default="cluster-a",
                 help="ClusterProfile name == member cluster name.")
  p.add_argument("--namespace", default="multi-cluster-fleet",
                 help="Namespace the warm pools live in on the member.")
  p.add_argument("--hub-namespace", default=inventory.CLUSTERPROFILE_NAMESPACE)
  p.add_argument("--api-version", default=inventory.CLUSTERPROFILE_VERSION)
  p.add_argument("--interval", type=float, default=30.0)
  p.add_argument("--duration", type=float, default=0.0,
                 help="Seconds; 0 runs until interrupted.")
  p.add_argument("--max-report-age", type=float, default=90.0,
                 help="Must match PlannerRegistry.max_report_age_s. This is "
                      "the line the planner draws between eligible and not.")
  p.add_argument("--drift-threshold", type=int, default=500,
                 help="Sandbox delta below which a frozen counter is just "
                      "quiet rather than wrong.")
  p.add_argument("--out", help="Append samples to this CSV.")
  args = p.parse_args()

  hub = _api(args.kubeconfig, args.hub_context)
  member = _api(args.kubeconfig, args.member_context)

  writer = fh = None
  if args.out:
    fh = open(args.out, "a", newline="")
    writer = csv.writer(fh)
    if fh.tell() == 0:
      writer.writerow(["ts", "elapsed_s", "hb_age_s", "eligible", "pub_depth",
                       "pub_ready", "pools", "obs_depth", "obs_ready", "sbx",
                       "pods", "swp", "pub_frozen_s", "obs_frozen_s",
                       "verdict"])

  pub_freeze, obs_freeze = _Freeze(), _Freeze()
  last_seen = {"sbx": None, "pods": None}
  worst_lie = 0  # largest sandbox count the published number failed to reflect
  t0 = _now()

  print(f"watching {args.cluster}: hub={args.hub_context} "
        f"member={args.member_context}, every {args.interval:.0f}s")
  print(f"{'elapsed':>8} {'hb_age':>7} {'pub_ready':>10} {'obs_ready':>10} "
        f"{'actual_sbx':>11} {'pods':>8} {'pub_frz':>8} {'obs_frz':>8}  verdict")

  try:
    while True:
      now = _now()
      try:
        pub = _published(hub, args)
      except Exception as e:
        print(f"  (hub read failed: {e})", file=sys.stderr)
        pub = {"hb_age": None, "pub_depth": None, "pub_ready": None,
               "pub_capacity": None}
      try:
        obs = _observed(member, args)
      except Exception as e:
        print(f"  (member read failed: {e})", file=sys.stderr)
        obs = {"pools": None, "obs_depth": None, "obs_ready": None}
      act = _actual(args)

      pub_frz = pub_freeze.update(pub["pub_ready"], now)
      obs_frz = obs_freeze.update(obs["obs_ready"], now)

      hb = pub["hb_age"]
      eligible = hb is not None and hb <= args.max_report_age

      # Movement signal, sandboxes preferred, pods as the fallback. Each series
      # keeps its OWN previous value, and a delta is only ever taken within one
      # series. The first cut shared a single `last` across both, so every time
      # the -1 sentinel toggled the sandbox counter off and on, the next delta
      # compared pods against sandboxes -- a constant offset (the ~6,888
      # system pods) that read as sudden movement and fired two false
      # FRESH-BUT-WRONG verdicts on an idle, fully-settled cluster.
      truth = act["sbx"] if act["sbx"] is not None else act["pods"]
      key = "sbx" if act["sbx"] is not None else (
          "pods" if act["pods"] is not None else None)
      moved = 0
      if key is not None and last_seen[key] is not None:
        moved = abs(act[key] - last_seen[key])
      for k in ("sbx", "pods"):
        if act[k] is not None:
          last_seen[k] = act[k]

      # The finding, stated as a predicate: the planner considers this cluster
      # fresh, its published number has not moved, and the cluster underneath
      # it demonstrably has.
      verdict = "ok"
      # Tooling sanity, checked before anything else. The member found pools to
      # report on and this script found none, so the two are not looking at the
      # same objects -- wrong namespace, wrong label, wrong context. Say so
      # instead of charting a flat zero as though it were a measurement.
      if obs["pools"] == 0 and (pub["pub_ready"] or 0) > 0:
        verdict = "SELECTOR-MISMATCH(this script is wrong, not the fleet)"
      elif not eligible:
        verdict = "not-eligible"
      if verdict == "ok" and pub_frz >= args.interval * 1.5 and moved >= args.drift_threshold:
        if obs_frz >= args.interval * 1.5:
          verdict = "FRESH-BUT-WRONG(swp)"   # SWP aggregation stalled
        else:
          verdict = "FRESH-BUT-WRONG(pub)"   # member saw it, hub did not
      if verdict.startswith("FRESH-BUT-WRONG") and truth is not None \
          and pub["pub_ready"] is not None:
        worst_lie = max(worst_lie, abs(truth - pub["pub_ready"]))

      def s(v, w, fmt="{:,}"):
        return f"{'-':>{w}}" if v is None else f"{fmt.format(v):>{w}}"

      print(f"{now - t0:>7.0f}s {s(hb, 6, '{:.0f}')}s {s(pub['pub_ready'], 10)} "
            f"{s(obs['obs_ready'], 10)} {s(act['sbx'], 11)} {s(act['pods'], 8)} "
            f"{pub_frz:>7.0f}s {obs_frz:>7.0f}s  {verdict}", flush=True)

      if writer:
        writer.writerow([
            _dt.datetime.now(_dt.timezone.utc).isoformat(), round(now - t0),
            None if hb is None else round(hb), int(eligible), pub["pub_depth"],
            pub["pub_ready"], obs["pools"], obs["obs_depth"], obs["obs_ready"],
            act["sbx"], act["pods"], act["swp"], round(pub_frz),
            round(obs_frz), verdict])
        fh.flush()

      if args.duration and (now - t0) >= args.duration:
        break
      time.sleep(max(0.0, args.interval - (_now() - now)))
  except KeyboardInterrupt:
    print("\ninterrupted")
  finally:
    if fh:
      fh.close()

  print(f"\nworst published-counter freeze: {pub_freeze.worst:.0f}s")
  print(f"worst SWP-status freeze:        {obs_freeze.worst:.0f}s")
  print(f"largest gap between the planner's number and etcd, while the "
        f"planner considered this cluster fresh: {worst_lie:,} sandboxes")
  return 0


if __name__ == "__main__":
  sys.exit(main())
