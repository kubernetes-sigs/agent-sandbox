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
    reap(run_id="ab12cd34ef56", context="my-ctx", namespace="rl")   # one run
    reap(context="my-ctx", namespace="rl")                          # all asrl-managed

    python -m agent_sandbox_rl.reaper --run-id ab12cd34ef56 --context my-ctx --namespace rl

Deletes claims → warmpools → sandboxes → templates (order matters: claims first so
they stop holding sandboxes; warmpools next so the controller stops replenishing),
then force-deletes any pods carrying the run-id label. Claims/warmpools/templates
carry the run-id label directly. Deleting claims + warmpools **cascades the Sandbox
CRs** via owner refs — Sandbox CRs themselves don't carry the run-id label (the
controller doesn't copy it), so the sandbox pass is a best-effort no-op unless a
future controller propagates it. Sandbox **pods** do carry the label (via the pod
template), so the pod delete-collection is a real, direct sweep.
"""
from __future__ import annotations

import argparse
import logging

from . import constants
from .cluster import Cluster
from .config import ClusterConfig

logger = logging.getLogger("agent_sandbox_rl.reap")


def reap(run_id: str | None = None, *, context: str | None = None,
         namespace: str = "default", kubeconfig: str | None = None,
         in_cluster: bool = False, delete_pods: bool = True) -> dict:
  """Delete resources matching a run-id (or all asrl-managed if ``run_id`` is None).

  Returns a dict of per-kind deletion counts. Safe to run repeatedly (idempotent)."""
  selector = (f"{constants.RUN_ID_LABEL}={run_id}" if run_id
              else f"{constants.MANAGED_BY_LABEL}={constants.MANAGED_BY_VALUE}")
  cluster = Cluster(
      ClusterConfig(context=context, namespace=namespace, kubeconfig=kubeconfig,
                    in_cluster=in_cluster),
      labels=constants.DEFAULT_LABELS)
  r = cluster.resources
  counts: dict[str, int | str] = {}

  def _sweep(kind, lister, deleter):
    names = lister(label_selector=selector)
    for n in names:
      try:
        deleter(n)
      except Exception:  # noqa: BLE001 — best-effort; keep going
        logger.warning("reap: failed to delete %s %s", kind, n, exc_info=True)
    counts[kind] = len(names)

  _sweep("claims", r.list_claims, r.delete_claim)
  _sweep("warmpools", r.list_warmpools, r.delete_warmpool)
  _sweep("sandboxes", r.list_sandboxes, r.delete_sandbox)
  _sweep("templates", r.list_templates, r.delete_template)

  if delete_pods:
    try:
      cluster.core_api.delete_collection_namespaced_pod(
          namespace=namespace, label_selector=selector,
          grace_period_seconds=0, propagation_policy="Background")
      counts["pods"] = "requested (force)"
    except Exception:  # noqa: BLE001
      logger.warning("reap: pod delete-collection failed", exc_info=True)

  logger.info("reap(%s): %s", run_id or "all-managed", counts)
  return counts


def main(argv=None) -> None:
  p = argparse.ArgumentParser(description="Reap agent-sandbox-rl resources by label.")
  p.add_argument("--run-id", default=None, help="reap one run; omit to reap all asrl-managed")
  p.add_argument("--context", default=None)
  p.add_argument("--namespace", default="default")
  p.add_argument("--kubeconfig", default=None)
  p.add_argument("--in-cluster", action="store_true")
  p.add_argument("--keep-pods", action="store_true", help="don't force-delete pods")
  a = p.parse_args(argv)
  logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
  counts = reap(a.run_id, context=a.context, namespace=a.namespace,
                kubeconfig=a.kubeconfig, in_cluster=a.in_cluster,
                delete_pods=not a.keep_pods)
  print(counts)


if __name__ == "__main__":
  main()
