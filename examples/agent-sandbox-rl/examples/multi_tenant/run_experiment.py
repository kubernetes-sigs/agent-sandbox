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

"""Run one experiment as one user, in that user's own namespace.

This is what a user runs. It builds a `FleetConfig` that stays inside the
namespace an admin provisioned for the user (``rl-<user>``, see
``admin/provision.sh``), names its templates and pools after the user and the
experiment, and keeps the fleet's pod count under the namespace's
`ResourceQuota`. Several experiments of one user coexist in the namespace
through ``run_isolation="names"``; other users never see this namespace.

The task images are the same for every user. The image layers live in the
node's image cache, below the namespace, so the second user on a node starts
without a pull. Nothing here pre-pulls: the admin does that once with
``admin/prepull_shared.py``.

Several clusters: ``--contexts a,b`` gives the fleet one `ClusterConfig` per
context, all in the user's namespace, which the admin provisioned on each
cluster. The quota is fitted per cluster, the tightest one sets the
concurrency, and each cluster's budget becomes its `max_replicas`.

Example:

  python run_experiment.py --user alice --experiment exp1 \\
      --kubeconfig ~/.kube/rl-alice.kubeconfig --images images.txt \\
      --tasks-per-image 2 --max-concurrent 4 --report-dir reports
"""

from __future__ import annotations

import argparse
import json
import logging
import pathlib
import re
import sys

from agent_sandbox_rl import (
    ClusterConfig,
    FleetConfig,
    ResourceSpec,
    SandboxFleet,
    TemplateSpec,
    constants,
    sizing,
)
from agent_sandbox_rl.sources import Task, load_image_list

logger = logging.getLogger("multi_tenant.run_experiment")

NAMESPACE_PREFIX = "rl-"
# Same prefix as the SDK's own pod label (agent-sandbox-rl/run-id): the
# controller passes it through to pods, and it is not a claim label domain.
TENANT_LABEL = "agent-sandbox-rl/tenant"
EXPERIMENT_LABEL = "agent-sandbox-rl/experiment"
# A released sandbox runs `sleep infinity`, which ignores SIGTERM, so without
# this its pod lingers for the default 30 s grace period and keeps counting
# against the quota and the fleet's circuit breaker.
TERMINATION_GRACE_S = 1
_NAME = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")


def slug(value: str, limit: int = 12) -> str:
  """Lower-case DNS-1123 fragment of ``value``, at most ``limit`` characters."""
  s = re.sub(r"[^a-z0-9-]+", "-", value.lower()).strip("-")
  s = s[:limit].strip("-")
  if not s:
    raise ValueError(f"{value!r} has no DNS-1123 characters")
  return s


def tenant_namespace(user: str) -> str:
  """``rl-<user>``, the namespace `admin/provision.sh` created for the user.

  The user name is used as is, never cut or lower-cased: the admin's scripts
  use it verbatim, so any transformation here would point the runner at a
  namespace nobody provisioned. A name that cannot form a namespace is refused.
  """
  if not _NAME.match(user) or len(user) > 60:
    raise ValueError(f"user {user!r} must be a DNS-1123 label (lowercase, digits, '-') "
                     "of at most 60 characters, as given to admin/provision.sh")
  return f"{NAMESPACE_PREFIX}{user}"


def cluster_name_for(context: str | None) -> str:
  """The fleet's name for a cluster, the same for every user.

  `admin/tenant_kubeconfig.sh` names a tenant's contexts ``rl-<user>@<ctx>``,
  where ``<ctx>`` is the admin's context name. The part after ``@`` is the
  cluster name, so alice's ``rl-alice@prod-a`` and bob's ``rl-bob@prod-a`` are
  one cluster ``prod-a`` to both fleets. That matters for the default
  ``image-affinity`` placement, which maps an image to a cluster by the sorted
  cluster names: with the same names, every user puts an image on the same
  cluster and finds it in that cluster's node cache.
  """
  if not context:
    return "default"
  return context.rsplit("@", 1)[-1] or context


def report_path(report_dir: pathlib.Path | str, user: str, experiment: str) -> pathlib.Path:
  """Where one experiment's ``report.json`` goes; `run_many.py` reads the same."""
  return pathlib.Path(report_dir) / user / slug(experiment, 63) / "report.json"


def name_prefix_for(user: str, experiment: str) -> str:
  """The template name prefix of one experiment: ``<user>-<experiment>-``.

  With ``run_isolation="names"`` the SDK puts its run id after the prefix, so
  templates read ``alice-exp1-<run id>-<image hash>`` and pools ``pool-`` plus
  that. The run id itself stays the fleet's own: a caller-set ``run_id`` means a
  job shared by several processes, which turns off the pod-count circuit
  breaker and the teardown verification, and this runner wants both.
  """
  prefix = f"{slug(user)}-{slug(experiment)}-"
  if len(prefix) > 40:                      # leaves room for run id, hash, "pool-"
    raise ValueError(f"name prefix {prefix!r} is too long")
  return prefix


def footprint(counts: dict[str, int], max_concurrent: int, max_pool: int) -> tuple[int, int]:
  """What the sliding strategy holds in the namespace at its peak.

  Returns ``(window, pods)``: the number of image pools the strategy keeps warm
  at once (the SDK's own `sizing.recommend_window`) and the pods that costs:
  those pools' warm replicas, one claimed pod per concurrency slot, and one
  replacement per slot while a pool refills behind a claim. ``counts`` maps
  each image to its task count, in task order, as `SandboxFleet.image_counts`.
  """
  total = sum(counts.values()) or 1
  window = sizing.recommend_window(counts, max_concurrent, max_pool)
  warm = sum(sizing.compute_replicas(n, total, max_concurrent, max_pool)
             for n in list(counts.values())[:window])
  return window, warm + 2 * max_concurrent


def pod_budget(hard: dict, used: dict, *, cpu: str, memory: str) -> int | None:
  """How many more sandbox pods the quota admits, or None without a quota.

  ``hard`` and ``used`` are a `ResourceQuota` status; ``pods``, ``requests.cpu``
  and ``requests.memory`` are the keys that bound a fleet, each converted to
  pods at the given per-pod requests.
  """
  from kubernetes.utils import parse_quantity

  per_pod = {"pods": 1, "requests.cpu": parse_quantity(cpu),
             "requests.memory": parse_quantity(memory)}
  budgets = []
  for key, cost in per_pod.items():
    if key in hard and cost > 0:
      left = parse_quantity(hard[key]) - parse_quantity(used.get(key, "0"))
      budgets.append(max(0, int(left // cost)))
  return min(budgets) if budgets else None


def fit_to_quota(hard: dict, used: dict, *, cpu: str, memory: str, counts: dict[str, int],
                 wanted: int, max_pool: int, share: int = 1) -> tuple[int, int | None]:
  """The largest concurrency up to ``wanted`` whose footprint fits the quota.

  Returns ``(max_concurrent, budget)``. ``budget`` is this experiment's share of
  the pods the quota still admits (``share`` experiments start at once in the
  namespace), or None without a quota; ``max_concurrent`` is 0 when not even one
  slot fits. The quota rejects pods at admission, where the fleet's fail-fast
  cannot see them, so fitting first is what keeps a run from waiting out
  ``ready_timeout``.
  """
  budget = pod_budget(hard, used, cpu=cpu, memory=memory)
  if budget is None:
    return wanted, None
  budget //= max(1, share)
  for mc in range(wanted, 0, -1):
    if footprint(counts, mc, max_pool)[1] <= budget:
      return mc, budget
  return 0, budget


def read_quota(core_api, namespace: str) -> tuple[dict, dict]:
  """(hard, used) summed over the namespace's ResourceQuotas; empty if none."""
  hard: dict = {}
  used: dict = {}
  for q in core_api.list_namespaced_resource_quota(namespace).items:
    hard.update(q.status.hard or {})
    used.update(q.status.used or {})
  return hard, used


def build_config(user: str, experiment: str, *, contexts: list[str] | None = None,
                 kubeconfig: str | None = None, max_concurrent: int = 4,
                 max_warmpool_size: int = 4, cpu: str = "100m", memory: str = "128Mi",
                 ready_timeout: int = 600, pod_budget: int | None = None,
                 cluster_budgets: dict[str, int] | None = None) -> FleetConfig:
  """The fleet of one experiment: the user's namespace on every cluster, names
  per user and experiment and run, labels that say whose run it is, the
  quota's pod budget as the fleet's own hard ceiling (`max_live_sandboxes`),
  and, with several clusters, each cluster's budget as its `max_replicas`."""
  namespace = tenant_namespace(user)              # refuses a name no admin provisioned
  contexts = list(contexts or [None])
  clusters = []
  for ctx in contexts:
    name = cluster_name_for(ctx)
    clusters.append(ClusterConfig(
        name=name, context=ctx, kubeconfig=kubeconfig, namespace=namespace,
        max_replicas=(cluster_budgets or {}).get(name)))
  if len({c.name for c in clusters}) != len(clusters):
    raise ValueError(f"contexts {contexts!r} name the same cluster twice")
  return FleetConfig(
      clusters=clusters,
      run_isolation="names",
      template_name_prefix=name_prefix_for(user, experiment),
      labels={**constants.DEFAULT_LABELS,
              TENANT_LABEL: user, EXPERIMENT_LABEL: slug(experiment, 63)},
      max_concurrent=max_concurrent,
      max_warmpool_size=max_warmpool_size,
      max_live_sandboxes=pod_budget,
      ready_timeout=ready_timeout,
      template=TemplateSpec(
          resources=ResourceSpec(cpu=cpu, memory=memory),
          # The admin's NetworkPolicy in the namespace covers every fleet pod;
          # the controller then creates none per template.
          network_policy_management="Unmanaged",
          extra_pod_spec={"terminationGracePeriodSeconds": TERMINATION_GRACE_S}),
  )


def probe(task, handle) -> dict:
  """The per-task work: one short command, enough to prove the sandbox runs."""
  out = handle.exec(["sh", "-c", "head -1 /etc/os-release; hostname"]).strip()
  return {"task": task.id, "image": task.image,
          "cluster": getattr(handle, "cluster_name", None), "output": out}


def drive(fleet, images: list[str], *, tasks_per_image: int, strategy: str = "sliding",
          process_fn=probe) -> dict:
  """Load ``tasks_per_image`` tasks per image, run them, and verify the teardown.

  Works on `SandboxFleet` and on the package's `FakeFleet`. Returns the fleet's
  run id, the task results, the `RunReport` and the per-cluster
  ``verify_teardown`` counts (empty for fakes, which have no reaper).
  """
  tasks = [Task(id=f"img{n}-{slug(img.rsplit('/', 1)[-1], 30)}-{i}", image=img)
           for n, img in enumerate(images) for i in range(tasks_per_image)]
  fleet.load_tasks(tasks)
  results = fleet.run(process_fn, strategy=strategy)
  verify = fleet.verify_teardown() if hasattr(fleet, "verify_teardown") else {}
  report = fleet.report.to_dict() if fleet.report is not None else {}
  return {"run_id": getattr(fleet, "run_id", None), "results": results,
          "report": report, "verify_teardown": verify}


def teardown_clean(verify: dict) -> bool:
  """True when nothing of the run remained on any cluster after the reaper.

  `verify_teardown` reports two things per cluster: what the run-scoped reaper
  found right after teardown (pods the controller was still removing count
  there) and ``remaining``, the same kinds re-listed after the sweep and a short
  settle. Clean means the second is all zero and no cluster was skipped.
  """
  if not verify:
    return True
  for counts in verify.values():
    if "skipped" in counts or "error" in counts:
      return False
    if any((counts.get("remaining") or {}).values()):
      return False
  return True


def reaper_found(verify: dict) -> dict[str, int]:
  """What the reaper swept after teardown, summed over clusters, by kind."""
  out: dict[str, int] = {}
  for counts in verify.values():
    for k, v in counts.items():
      if isinstance(v, int):
        out[k] = out.get(k, 0) + v
  return out


def claims_by_cluster(results: list) -> dict[str, int]:
  """How many tasks ran on each cluster, from the probe's ``cluster`` field."""
  out: dict[str, int] = {}
  for r in results:
    if isinstance(r, dict) and r.get("cluster"):
      out[r["cluster"]] = out.get(r["cluster"], 0) + 1
  return dict(sorted(out.items()))


def write_report(path: pathlib.Path, *, user: str, experiment: str,
                 outcome: dict, max_concurrent: int) -> pathlib.Path:
  path.parent.mkdir(parents=True, exist_ok=True)
  results = outcome["results"]
  errors = [r for r in results if isinstance(r, BaseException)]
  doc = {
      "user": user, "experiment": experiment, "run_id": outcome.get("run_id"),
      "namespace": tenant_namespace(user), "max_concurrent": max_concurrent,
      "tasks": len(results), "task_errors": len(errors),
      "claims_by_cluster": claims_by_cluster(results),
      "report": outcome["report"], "verify_teardown": outcome["verify_teardown"],
      "teardown_clean": teardown_clean(outcome["verify_teardown"]),
      "reaper_found": reaper_found(outcome["verify_teardown"]),
      "results": [r if not isinstance(r, BaseException) else {"error": str(r)}
                  for r in results],
  }
  path.write_text(json.dumps(doc, indent=2, default=str) + "\n")
  return path


def main(argv=None) -> int:
  p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
  p.add_argument("--user", required=True,
                 help="tenant name as given to admin/provision.sh; the namespace is rl-<user>")
  p.add_argument("--experiment", required=True)
  p.add_argument("--images", default=str(pathlib.Path(__file__).with_name("images.txt")),
                 help="one image per line (default: images.txt next to this script)")
  p.add_argument("--tasks-per-image", type=int, default=2)
  p.add_argument("--max-concurrent", type=int, default=4)
  p.add_argument("--max-warmpool-size", type=int, default=4)
  p.add_argument("--cpu", default="100m", help="per-sandbox CPU request")
  p.add_argument("--memory", default="128Mi", help="per-sandbox memory request")
  p.add_argument("--strategy", default="sliding")
  p.add_argument("--ready-timeout", type=int, default=600)
  p.add_argument("--contexts", default=None,
                 help="comma-separated kube contexts, one cluster each (default: the "
                      "current context); the cluster name is the part after '@'")
  p.add_argument("--kubeconfig", default=None,
                 help="kubeconfig file, e.g. the one admin/tenant_kubeconfig.sh wrote")
  p.add_argument("--report-dir", default="reports",
                 help="report.json goes to <report-dir>/<user>/<experiment>/")
  p.add_argument("--share", type=int, default=1,
                 help="experiments of this user that start at once; each gets 1/N of the "
                      "quota's free pods (run_many.py sets it)")
  p.add_argument("--no-quota-fit", action="store_true",
                 help="do not lower --max-concurrent to what the namespace quota allows")
  p.add_argument("-v", "--verbose", action="store_true")
  args = p.parse_args(argv)
  logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                      format="%(asctime)s %(levelname)s %(name)s %(message)s")

  images = load_image_list(args.images)
  if not images:
    p.error(f"no images in {args.images}")
  try:
    tenant_namespace(args.user)
  except ValueError as e:
    p.error(str(e))
  contexts = [c for c in (args.contexts or "").split(",") if c] or None
  kwargs = dict(contexts=contexts, kubeconfig=args.kubeconfig,
                max_concurrent=args.max_concurrent, max_warmpool_size=args.max_warmpool_size,
                cpu=args.cpu, memory=args.memory, ready_timeout=args.ready_timeout)
  try:
    fleet = SandboxFleet(build_config(args.user, args.experiment, **kwargs))
  except ValueError as e:
    p.error(str(e))
  namespace = fleet.config.clusters[0].namespace

  max_concurrent = args.max_concurrent
  if not args.no_quota_fit:
    counts = {img: args.tasks_per_image for img in images}
    # Per cluster: what its quota admits for this experiment. The tightest
    # cluster bounds the concurrency, because placement may put a whole window
    # there; each cluster's budget caps the warm replicas it takes (max_replicas)
    # and their sum is the fleet's pod ceiling.
    budgets: dict[str, int] = {}
    fits = max_concurrent
    for cluster in fleet.registry:
      hard, used = read_quota(cluster.core_api, namespace)
      fit, budget = fit_to_quota(hard, used, cpu=args.cpu, memory=args.memory, counts=counts,
                                 wanted=max_concurrent, max_pool=args.max_warmpool_size,
                                 share=args.share)
      if budget is None:
        continue
      budgets[cluster.name] = budget
      if fit < 1:
        logger.error("quota in %s on cluster %s leaves no room for one slot (budget %d "
                     "pods for this experiment): hard=%s used=%s", namespace, cluster.name,
                     budget, hard, used)
        return 2
      fits = min(fits, fit)
    if budgets:
      if fits < max_concurrent:
        logger.warning("quota in %s admits %s pods for this experiment; lowering "
                       "--max-concurrent from %d to %d", namespace,
                       ", ".join(f"{b} on {c}" for c, b in budgets.items()),
                       max_concurrent, fits)
      max_concurrent = fits
      fleet = SandboxFleet(build_config(
          args.user, args.experiment, **{**kwargs, "max_concurrent": fits},
          pod_budget=sum(budgets.values()),
          cluster_budgets=budgets if len(budgets) > 1 else None))
    window, peak = footprint(counts, max_concurrent, args.max_warmpool_size)
    logger.info("footprint: %d pools warm at once, up to %d pods at the peak%s",
                window, peak, f" (quota budgets {budgets})" if budgets else "")
  logger.info("user=%s experiment=%s namespace=%s run_id=%s names=%s*", args.user,
              args.experiment, namespace, fleet.run_id, fleet.config.template_name_prefix)

  outcome = drive(fleet, images, tasks_per_image=args.tasks_per_image,
                  strategy=args.strategy)
  path = write_report(report_path(args.report_dir, args.user, args.experiment),
                      user=args.user, experiment=args.experiment, outcome=outcome,
                      max_concurrent=max_concurrent)
  run_id = outcome["run_id"]
  errors = sum(1 for r in outcome["results"] if isinstance(r, BaseException))
  clean = teardown_clean(outcome["verify_teardown"])
  print(f"{args.user}/{args.experiment}: run_id={run_id} tasks={len(outcome['results'])} "
        f"errors={errors} teardown_clean={clean} report={path}")
  by_cluster = claims_by_cluster(outcome["results"])
  if len(by_cluster) > 1:
    print("  tasks per cluster: " + "  ".join(f"{c}={n}" for c, n in by_cluster.items()))
  if not clean:
    for ctx in (contexts or [None]):
      print(f"  leftovers: python -m agent_sandbox_rl.reaper --run-id {run_id} "
            f"--namespace {namespace}" + (f" --context {ctx}" if ctx else ""))
  return 0 if errors == 0 and clean else 1


if __name__ == "__main__":
  sys.exit(main())
