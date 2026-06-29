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

"""Run SWE-bench tasks on Agent Sandbox warm pools via agent-sandbox-rl.

The agent-sandbox-rl equivalent of the example's hand-rolled run_swebench.py:
configure cluster(s) -> load tasks -> run(strategy) -> JSON results. Multi-cluster
aware (set KUBE_CONTEXTS to spread across clusters). Env-configured:

  WARMPOOL_STRATEGY=sliding TASKS_LIMIT=4 MAX_CONCURRENT=4 \
  NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool NODE_SELECTOR_VAL=e2-pool \
  NAMESPACE=default python run_swebench_fleet.py
"""

import json
import logging
import os

from agent_sandbox_rl import (
    ClusterConfig,
    FleetConfig,
    SandboxFleet,
    SweBenchSource,
    TemplateSpec,
    swebench_probe,
)

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")


def _env(name, default):
  return os.getenv(name, default)


def main():
  strategy = _env("WARMPOOL_STRATEGY", "naive")
  # CLI convenience: TASKS_LIMIT=0 means "all" (-> None for SweBenchSource, whose
  # 0 means none). Any positive N caps to N.
  tasks_limit = int(_env("TASKS_LIMIT", "1")) or None
  offset = int(_env("OFFSET", "0"))
  max_concurrent = int(_env("MAX_CONCURRENT", "1"))
  max_pool = int(_env("MAX_WARMPOOL_SIZE", "8"))
  window = int(_env("WARMPOOL_WINDOW_SIZE", "0")) or None
  namespace = _env("NAMESPACE", "default")
  ready_timeout = int(_env("SANDBOX_READY_TIMEOUT", "900"))
  prepull = _env("PREPULL", "0") == "1"

  node_selector = None
  if _env("NODE_SELECTOR_KEY", "") and _env("NODE_SELECTOR_VAL", ""):
    node_selector = {os.environ["NODE_SELECTOR_KEY"]: os.environ["NODE_SELECTOR_VAL"]}

  template = TemplateSpec(
      runtime_class=_env("RUNTIME_CLASS", "") or None,
      node_selector=node_selector,
      image_pull_secret=_env("IMAGE_PULL_SECRET", "") or None,
  )

  # One ClusterConfig per context in KUBE_CONTEXTS (comma-separated); else the
  # ambient context.
  contexts = [c for c in _env("KUBE_CONTEXTS", "").split(",") if c]
  if contexts:
    clusters = [ClusterConfig(name=c, context=c, namespace=namespace)
                for c in contexts]
  else:
    clusters = [ClusterConfig(name="default", namespace=namespace)]

  config = FleetConfig(
      clusters=clusters, max_concurrent=max_concurrent,
      max_warmpool_size=max_pool, window_size=window,
      ready_timeout=ready_timeout, template=template)

  fleet = SandboxFleet(config)
  fleet.load_tasks(SweBenchSource(
      dataset=_env("DATASET_NAME", "R2E-Gym/SWE-Bench-Verified"),
      split=_env("DATASET_SPLIT", "test"), limit=tasks_limit, offset=offset))

  if prepull:
    fleet.preflight(); fleet.plan(); fleet.prepull(wait=True)

  results = fleet.run(_record(swebench_probe), strategy=strategy,
                      concurrency=max_concurrent)
  print(json.dumps({"strategy": strategy, "tasks": len(results),
                    "results": results}, indent=2))

  report_dir = _env("REPORT_DIR", "")
  if report_dir and fleet.report is not None:
    _write_report(report_dir, fleet.report, strategy, len(results))


def _write_report(report_dir, report, strategy, n_tasks):
  """Write the RunReport as a timestamped .txt (summary table) + .json."""
  import datetime
  import pathlib

  out = pathlib.Path(report_dir)
  out.mkdir(parents=True, exist_ok=True)
  stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
  base = out / f"{strategy}_{n_tasks}tasks_{stamp}"
  base.with_suffix(".txt").write_text(report.summary() + "\n")
  base.with_suffix(".json").write_text(json.dumps(report.to_dict(), indent=2) + "\n")
  print(f"\nwrote performance report: {base}.txt / .json")


def _record(probe):
  """Wrap the probe to emit a per-task result dict."""
  def fn(task, handle):
    out = probe(task, handle)
    return {"instance_id": task.id, "image": task.image,
            "cluster": handle.cluster_name, "hostname": handle.hostname,
            "output": out}
  return fn


if __name__ == "__main__":
  main()
