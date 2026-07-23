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

"""RL-shape recycling: reuse one sandbox across a problem's G rollouts.

Builds P problems x G rollouts, warms **shallow** (recycling holds ~1 sandbox per
problem — do NOT deep-warm), runs a **determinism gate**, then
`reuse_git_restore_sandbox` so claims scale with problems, not tasks (/G). The
recycle counterpart of `run_swebench_fleet.py`. Env-configured:

  PROBLEMS=500 ROLLOUTS=16 MAX_CONCURRENT=500 CPU=250m MEMORY=512Mi \
  DETERMINISM=5 NAMESPACE=rl \
  NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool NODE_SELECTOR_VAL=gvisor-pool \
  RUNTIME_CLASS=gvisor python run_swebench_recycle.py

Safeguards are on by default (circuit breaker + run-id label + staged warm); if a
run is killed abruptly, sweep just that run with its id (printed at startup):
`python -m agent_sandbox_rl.reaper --run-id <id> --namespace rl`. (Reaping *every*
run in the namespace needs an explicit `--all` — it also deletes healthy runs.)
"""

import json
import logging
import os
import time

from agent_sandbox_rl import (ClusterConfig, FleetConfig, ResourceSpec, SandboxFleet,
                              SweBenchSource, Task, TemplateSpec, determinism_canary,
                              reuse_git_restore_sandbox, swebench_probe)
from agent_sandbox_rl.sources import to_tasks

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")


def _env(name, default):
  return os.getenv(name, default)


def main():
  problems = int(_env("PROBLEMS", "50"))
  rollouts = int(_env("ROLLOUTS", "16"))              # G per problem
  max_concurrent = int(_env("MAX_CONCURRENT", "500"))  # concurrent problems held
  determinism = int(_env("DETERMINISM", "5"))          # canary on first N images (0 = skip)
  namespace = _env("NAMESPACE", "default")
  ready_timeout = int(_env("SANDBOX_READY_TIMEOUT", "900"))
  cpu, memory = _env("CPU", "250m"), _env("MEMORY", "512Mi")

  node_selector = None
  if _env("NODE_SELECTOR_KEY", "") and _env("NODE_SELECTOR_VAL", ""):
    node_selector = {os.environ["NODE_SELECTOR_KEY"]: os.environ["NODE_SELECTOR_VAL"]}
  template = TemplateSpec(
      runtime_class=_env("RUNTIME_CLASS", "") or None, node_selector=node_selector,
      image_pull_secret=_env("IMAGE_PULL_SECRET", "") or None,
      resources=ResourceSpec(cpu=cpu, memory=memory))

  contexts = [c for c in _env("KUBE_CONTEXTS", "").split(",") if c]
  clusters = ([ClusterConfig(name=c, context=c, namespace=namespace) for c in contexts]
              if contexts else [ClusterConfig(name="default", namespace=namespace)])

  # Shallow warm: no warm_per_task — recycle needs ~1 replica/pool, not G.
  fleet = SandboxFleet(FleetConfig(
      clusters=clusters, max_concurrent=max_concurrent,
      ready_timeout=ready_timeout, template=template))

  base = to_tasks(SweBenchSource(limit=problems))
  images = [t.image for t in base]
  tasks = [Task(id=f"p{i:04d}-r{g}", image=img)
           for i, img in enumerate(images) for g in range(rollouts)]
  fleet.load_tasks(tasks)
  print(f"recycle: {len(images)} problems x {rollouts} rollouts = {len(tasks)} tasks; "
        f"mc={max_concurrent}", flush=True)

  fleet.setup()                                        # shallow warm (staged by default)

  # Correctness gate: same task twice in one recycled sandbox must be byte-identical.
  determ = []
  for img in images[:determinism]:
    out = determinism_canary(fleet, Task(id=f"canary-{img[-12:]}", image=img), swebench_probe)
    determ.append(out["identical"] and out["reset_clean"])
    print(f"  determinism {'OK ' if determ[-1] else 'FAIL'} {img.split('/')[-1]}", flush=True)
  if determ and not all(determ):
    raise SystemExit("determinism gate FAILED — reset leaks state; recycling is unsafe")

  def rollout(task, handle):                           # no-op probe by default
    return swebench_probe(task, handle)

  t0 = time.monotonic()
  with fleet.observer.run("reuse") as rep:
    reuse_git_restore_sandbox(fleet, fleet.tasks, rollout, max_concurrent)
    rep.total_s = time.monotonic() - t0
  fleet.teardown()

  d = rep.to_dict()
  claims = d.get("claims", 0)
  print(json.dumps({
      "problems": len(images), "rollouts": rollouts, "tasks": len(tasks),
      "claims": claims,
      "claims_per_task": round(claims / max(1, len(tasks)), 4),
      "tasks_ok": d.get("tasks_ok", 0), "tasks_err": d.get("tasks_err", 0),
      "wall_s": round(d.get("total_s", 0.0), 1),
      "determinism_clean": f"{sum(determ)}/{len(determ)}",
  }, indent=2))
  print(f"\n{rep.summary()}")


if __name__ == "__main__":
  main()
