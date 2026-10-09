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

"""Reaper (#4): delete everything a fleet run created, by label — the guaranteed
sweep for an **orphaned** run whose driver died without tearing down.

    from agent_sandbox_rl import reap
    reap(run_id="ab12cd34ef56", context="my-ctx", namespace="agent-sandbox-rl")   # one run (recommended)
    reap(all_managed=True, context="my-ctx", namespace="agent-sandbox-rl")        # EVERY run (opt-in)

    python -m agent_sandbox_rl.reaper --run-id ab12cd34ef56 --context my-ctx --namespace agent-sandbox-rl
    python -m agent_sandbox_rl.reaper --all --namespace agent-sandbox-rl          # every run (opt-in)
    reap_orphans(alive_run_ids={"ab12cd34ef56"}, namespace="agent-sandbox-rl")    # every run NOT listed
    python -m agent_sandbox_rl.reaper --orphans --alive-run-ids ab12cd34ef56 --namespace agent-sandbox-rl

Deletes claims → warmpools → sandboxes → templates (order matters: claims first so
they stop holding sandboxes; warmpools next so the controller stops replenishing),
then force-deletes the run's pods. Claims/warmpools/templates carry the run-id
label directly. Sandbox CRs do **not** (the controller does not copy it), and pods
carry it only under ``POD_RUN_ID_LABEL``: the controller drops every
template-supplied pod label under the reserved ``agents.x-k8s.io/`` prefix, and
pods created before that key existed carry no run id at all. So besides the label
passes, the sweep reaches Sandboxes and pods the way the controller links them:
each pool's ``status.selector`` (``agents.x-k8s.io/warm-pool-sandbox=<hash>``)
selects that pool's Sandboxes and pods, and each claim's ``status.sandbox.name``
names its Sandbox, whose ``status.labelSelector``
(``agents.x-k8s.io/sandbox-name-hash=<hash>``) selects its pod. Those selectors
are collected **before** the claims and pools are deleted, then applied after, so
a cascade that stalls (a finalizer, a slow controller) still ends with the pods
gone.
"""
from __future__ import annotations

import argparse
import logging
from collections.abc import Iterable
from datetime import datetime, timezone

from . import constants
from .cluster import Cluster
from .config import ClusterConfig

logger = logging.getLogger("agent_sandbox_rl.reap")


def reap(run_id: str | None = None, *, context: str | None = None,
         namespace: str = "default", kubeconfig: str | None = None,
         in_cluster: bool = False, delete_pods: bool = True,
         all_managed: bool = False) -> dict:
  """Delete resources for a single run (``run_id``), or — only when
  ``all_managed=True`` — **every** agent-sandbox-rl run in the namespace.

  The all-managed sweep is opt-in on purpose: it force-deletes (grace 0) pods of
  *healthy concurrent runs* too, so it must never be the accidental default of a
  "clean up my killed run" invocation. Returns per-kind deletion counts;
  idempotent."""
  if all_managed:
    run_id = None
  elif not run_id:
    # An empty id is not "no id": it would turn into the all-managed selector.
    raise ValueError(
        "reap requires a non-empty run_id; to sweep EVERY agent-sandbox-rl run in "
        "the namespace (including healthy concurrent ones) pass all_managed=True "
        "(CLI: --all)")
  cluster = _cluster(context=context, namespace=namespace, kubeconfig=kubeconfig,
                     in_cluster=in_cluster)
  return _reap_with_cluster(
      cluster, run_id, delete_pods=delete_pods,
      protect_claims_of=(None if run_id is None else (lambda rid: rid != run_id)))


def _cluster(*, context, namespace, kubeconfig, in_cluster) -> Cluster:
  return Cluster(
      ClusterConfig(context=context, namespace=namespace, kubeconfig=kubeconfig,
                    in_cluster=in_cluster),
      labels=constants.DEFAULT_LABELS)


def _reap_with_cluster(cluster: Cluster, run_id: str | None, *,
                       delete_pods: bool,
                       protect_claims_of=None) -> dict:
  """``run_id=None`` sweeps every managed run. ``protect_claims_of(rid)`` (rid
  may be None for an unlabelled claim) says whether a claim belongs to a run
  that must keep working: pools such a claim points at, their templates, and
  their Sandboxes and pods are left alone even if they carry ``run_id``."""
  if run_id is not None and not run_id:
    raise ValueError("an empty run_id would select every managed run")
  namespace = cluster.namespace
  selector = (f"{constants.RUN_ID_LABEL}={run_id}" if run_id
              else f"{constants.MANAGED_BY_LABEL}={constants.MANAGED_BY_VALUE}")
  # Pods carry the run id under a non-reserved key (#1807); pre-upgrade pods and
  # all Sandboxes are reached through the pool/claim selectors collected below.
  pod_selector = (f"{constants.POD_RUN_ID_LABEL}={run_id}" if run_id else selector)
  r = cluster.resources
  counts: dict[str, int | str] = {}

  # A pool keeps the run id of the run that created it, so a live run that
  # adopted it (adopt_existing, or a shared namespace) would lose its pool and
  # template here. Those still referenced by a protected run's claims stay.
  protected_pools, protected_templates = (
      _in_use_elsewhere(cluster, protect_claims_of) if protect_claims_of
      else (set(), set()))
  if protected_pools:
    logger.warning("reap(%s): keeping pool(s) %s and template(s) %s, still used by "
                   "another run's claims", run_id, sorted(protected_pools),
                   sorted(protected_templates))
    counts["kept_in_use"] = len(protected_pools) + len(protected_templates)

  # Sandboxes and pods are reached through the pools and claims (see the module
  # docstring), so record how before those are deleted.
  pod_selectors, sandbox_names = _sandbox_links(cluster, selector,
                                                skip_pools=protected_pools)

  def _sweep(kind, lister, deleter, keep=frozenset()):
    names = [n for n in lister(label_selector=selector) if n not in keep]
    for n in names:
      try:
        deleter(n)
      except Exception:  # noqa: BLE001 — best-effort; keep going
        logger.warning("reap: failed to delete %s %s", kind, n, exc_info=True)
    counts[kind] = len(names)

  _sweep("claims", r.list_claims, r.delete_claim)
  _sweep("warmpools", r.list_warmpools, r.delete_warmpool, keep=protected_pools)
  # Sandboxes reachable by label (none today; kept for a controller that copies
  # the label) plus the ones the claims and pools pointed at.
  labelled = r.list_sandboxes(label_selector=selector)
  for n in sorted(set(labelled) | sandbox_names):
    try:
      r.delete_sandbox(n)
    except Exception:  # noqa: BLE001
      logger.warning("reap: failed to delete sandbox %s", n, exc_info=True)
  counts["sandboxes"] = len(set(labelled) | sandbox_names)
  _sweep("templates", r.list_templates, r.delete_template, keep=protected_templates)

  if delete_pods:
    swept = 0
    for sel in [pod_selector, *sorted(pod_selectors)]:
      try:
        cluster.core_api.delete_collection_namespaced_pod(
            namespace=namespace, label_selector=sel,
            grace_period_seconds=0, propagation_policy="Background")
        swept += 1
      except Exception:  # noqa: BLE001
        logger.warning("reap: pod delete-collection failed for %s", sel, exc_info=True)
    counts["pods"] = f"requested (force) via {swept} selector(s)"

  logger.info("reap(%s): %s", run_id or "all-managed", counts)
  return counts


def _sandbox_links(cluster: Cluster, selector: str, *,
                   skip_pools: set[str] | frozenset = frozenset()) -> tuple[set[str], set[str]]:
  """Pod label selectors and Sandbox names that belong to the objects matching
  ``selector``: every pool's ``status.selector`` (its Sandboxes and pods carry
  that label), every claim's ``status.sandbox.name``, and for those claimed
  Sandboxes their ``status.labelSelector`` (their pod carries it). Best-effort:
  a list that fails contributes nothing and the label-based sweep still runs."""
  r = cluster.resources
  pod_selectors: set[str] = set()
  sandbox_names: set[str] = set()
  pool_selectors: set[str] = set()
  try:
    for pool in r._list_objects(constants.WARMPOOLS_PLURAL, selector):  # noqa: SLF001
      if (pool.get("metadata") or {}).get("name") in skip_pools:
        continue
      sel = (pool.get("status") or {}).get("selector")
      if sel:
        pool_selectors.add(sel)
  except Exception:  # noqa: BLE001
    logger.warning("reap: could not list warm pools for pod selectors", exc_info=True)
  pod_selectors |= pool_selectors
  # Unclaimed warm Sandboxes have no claim and no run-id label; the pool's
  # selector is the only handle on them, so record their names now and delete
  # them by name later rather than trusting the pool's owner cascade.
  for sel in sorted(pool_selectors):
    try:
      for sb in r._list_objects(constants.SANDBOXES_PLURAL, sel,  # noqa: SLF001
                                group=constants.SANDBOX_GROUP,
                                version=constants.SANDBOX_VERSION):
        name = (sb.get("metadata") or {}).get("name")
        if name:
          sandbox_names.add(name)
    except Exception:  # noqa: BLE001
      logger.warning("reap: could not list sandboxes for pool selector %s", sel,
                     exc_info=True)
  claimed: set[str] = set()
  try:
    for claim in r._list_objects(constants.CLAIMS_PLURAL, selector):  # noqa: SLF001
      sb = (claim.get("status") or {}).get("sandbox") or {}
      name = sb.get("name") or sb.get("Name")
      if name:
        claimed.add(name)
  except Exception:  # noqa: BLE001
    logger.warning("reap: could not list claims for sandbox names", exc_info=True)
  sandbox_names |= claimed
  # A claimed Sandbox's pod is selected by its own hash label; pool members are
  # already covered by the pool selector, so only the claimed ones are looked up.
  if claimed:
    try:
      for sb in r._list_objects(constants.SANDBOXES_PLURAL, None,  # noqa: SLF001
                                group=constants.SANDBOX_GROUP,
                                version=constants.SANDBOX_VERSION):
        if (sb.get("metadata") or {}).get("name") in claimed:
          sel = (sb.get("status") or {}).get("labelSelector")
          if sel:
            pod_selectors.add(sel)
    except Exception:  # noqa: BLE001
      logger.warning("reap: could not list sandboxes for pod selectors", exc_info=True)
  return pod_selectors, sandbox_names


def _in_use_elsewhere(cluster: Cluster, protect_claims_of) -> tuple[set[str], set[str]]:
  """Pools referenced by a claim for which ``protect_claims_of(claim_run_id)``
  is true, and the templates of those pools. Any claim in the namespace counts,
  managed or not. A failed list protects nothing and is logged; the run-scoped
  label sweep still applies."""
  r = cluster.resources
  pools: set[str] = set()
  try:
    for claim in r._list_objects(constants.CLAIMS_PLURAL, None):  # noqa: SLF001
      rid = ((claim.get("metadata") or {}).get("labels") or {}).get(constants.RUN_ID_LABEL)
      if not protect_claims_of(rid):
        continue
      ref = ((claim.get("spec") or {}).get("warmPoolRef") or {}).get("name")
      if ref:
        pools.add(ref)
  except Exception:  # noqa: BLE001
    logger.warning("reap: could not list claims to find pools still in use",
                   exc_info=True)
    return set(), set()
  templates: set[str] = set()
  if pools:
    try:
      for pool in r._list_objects(constants.WARMPOOLS_PLURAL, None):  # noqa: SLF001
        if (pool.get("metadata") or {}).get("name") in pools:
          ref = ((pool.get("spec") or {}).get("sandboxTemplateRef") or {}).get("name")
          if ref:
            templates.add(ref)
    except Exception:  # noqa: BLE001
      logger.warning("reap: could not list warm pools to find templates still in "
                     "use", exc_info=True)
  return pools, templates


def _created_at(obj: dict) -> datetime | None:
  ts = (obj.get("metadata") or {}).get("creationTimestamp")
  if not ts:
    return None
  try:
    return datetime.fromisoformat(ts.replace("Z", "+00:00"))
  except ValueError:
    return None


def find_orphan_runs(cluster: Cluster, alive_run_ids: Iterable[str], *,
                     min_age_s: float = 300.0,
                     now: datetime | None = None) -> dict[str, dict]:
  """Run ids present in the namespace (on claims, pools or templates carrying
  the run-id label) that are not in ``alive_run_ids`` and whose newest object
  is older than ``min_age_s``. The age floor protects a run that has just
  started and is not yet in the caller's alive list. Returns
  ``{run_id: {"objects": n, "newest_age_s": float}}``."""
  alive = set(alive_run_ids)
  now = now or datetime.now(timezone.utc)
  managed = f"{constants.MANAGED_BY_LABEL}={constants.MANAGED_BY_VALUE}"
  seen: dict[str, dict] = {}
  for plural in (constants.CLAIMS_PLURAL, constants.WARMPOOLS_PLURAL,
                 constants.TEMPLATES_PLURAL):
    for obj in cluster.resources._list_objects(plural, managed):  # noqa: SLF001
      rid = ((obj.get("metadata") or {}).get("labels") or {}).get(constants.RUN_ID_LABEL)
      if not rid:
        continue
      created = _created_at(obj)
      age = (now - created).total_seconds() if created else float("inf")
      rec = seen.setdefault(rid, {"objects": 0, "newest_age_s": float("inf")})
      rec["objects"] += 1
      rec["newest_age_s"] = min(rec["newest_age_s"], age)
  return {rid: rec for rid, rec in seen.items()
          if rid not in alive and rec["newest_age_s"] >= min_age_s}


def reap_orphans(alive_run_ids: Iterable[str], *, min_age_s: float = 300.0,
                 context: str | None = None, namespace: str = "default",
                 kubeconfig: str | None = None, in_cluster: bool = False,
                 delete_pods: bool = True, dry_run: bool = False) -> dict[str, dict]:
  """Reap every run in the namespace that is not in ``alive_run_ids``.

  This is the sweep an external reaper (a CronJob that knows which drivers are
  still running) should call instead of matching names or ages itself. Runs are
  discovered through the run-id label on their claims, pools and templates;
  Sandbox CRs never carry it and pods created before `POD_RUN_ID_LABEL` existed
  do not either, so a run with only pods or Sandboxes left is not found here.
  For each run found, the sweep reaches its Sandboxes and pods through its pools
  and claims, as `reap(run_id=...)` does. Pools (and their templates) that a
  claim of a run not being reaped still points at are kept, so a live run that
  adopted an older run's pool keeps it. ``alive_run_ids`` is the caller's list
  of runs whose driver is still up; passing an empty list reaps everything older
  than ``min_age_s``. ``dry_run`` reports without deleting. Returns
  ``{run_id: counts}`` (or the discovery record under ``dry_run``)."""
  cluster = _cluster(context=context, namespace=namespace, kubeconfig=kubeconfig,
                     in_cluster=in_cluster)
  orphans = find_orphan_runs(cluster, alive_run_ids, min_age_s=min_age_s)
  if not orphans:
    logger.info("reap_orphans: nothing to reap in namespace %s", namespace)
    return {}
  if dry_run:
    logger.info("reap_orphans (dry run): would reap %s", sorted(orphans))
    return orphans
  results: dict[str, dict] = {}
  for rid in sorted(orphans):
    results[rid] = _reap_with_cluster(
        cluster, rid, delete_pods=delete_pods,
        protect_claims_of=lambda claim_rid: claim_rid not in orphans)
  return results


def main(argv=None) -> None:
  p = argparse.ArgumentParser(description="Reap agent-sandbox-rl resources by label.")
  p.add_argument("--run-id", default=None, help="reap one run by id (recommended)")
  p.add_argument("--all", action="store_true",
                 help="sweep ALL agent-sandbox-rl runs in the namespace, incl. healthy "
                      "concurrent ones (required when --run-id is omitted)")
  p.add_argument("--context", default=None)
  p.add_argument("--namespace", default="default")
  p.add_argument("--kubeconfig", default=None)
  p.add_argument("--in-cluster", action="store_true")
  p.add_argument("--keep-pods", action="store_true", help="don't force-delete pods")
  p.add_argument("--orphans", action="store_true",
                 help="reap every run NOT listed in --alive-run-ids (requires it)")
  p.add_argument("--alive-run-ids", default=None,
                 help="comma-separated run ids whose drivers are still running; "
                      "an empty value means none are")
  p.add_argument("--min-age-s", type=float, default=300.0,
                 help="with --orphans: skip runs whose newest object is younger than this")
  p.add_argument("--dry-run", action="store_true",
                 help="with --orphans: report the runs that would be reaped, delete nothing")
  a = p.parse_args(argv)
  if a.dry_run and not a.orphans:
    p.error("--dry-run is only supported with --orphans; --run-id and --all delete")
  logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
  try:
    if a.orphans:
      if a.alive_run_ids is None:
        p.error("--orphans requires --alive-run-ids (pass an empty value if none are alive)")
      alive = [x.strip() for x in a.alive_run_ids.split(",") if x.strip()]
      counts = reap_orphans(alive, min_age_s=a.min_age_s, context=a.context,
                            namespace=a.namespace, kubeconfig=a.kubeconfig,
                            in_cluster=a.in_cluster, delete_pods=not a.keep_pods,
                            dry_run=a.dry_run)
    else:
      counts = reap(a.run_id, context=a.context, namespace=a.namespace,
                    kubeconfig=a.kubeconfig, in_cluster=a.in_cluster,
                    delete_pods=not a.keep_pods, all_managed=a.all)
  except ValueError as e:
    p.error(str(e))
  print(counts)


if __name__ == "__main__":
  main()
