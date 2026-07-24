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

"""Run a batch of SWE-bench tasks against Agent Sandbox warm pools.

This is a self-contained, model-free demonstration of how the rl-tunix RL/eval
pipeline provisions pools of pre-warmed sandboxes per task image and claims one
sandbox per trajectory. For each task it:

  1. claims a pre-warmed sandbox from the image's warm pool (Python SDK), then
  2. execs a lightweight command inside it to prove the task env is live
     (router-free, via ``kubectl exec``), then
  3. terminates the sandbox.

Warm-pool provisioning follows the strategy selected by ``WARMPOOL_STRATEGY``.

Configuration is via environment variables (see README.md). Example:

    WARMPOOL_STRATEGY=naive TASKS_LIMIT=1 MAX_WARMPOOL_SIZE=1 \
    NAMESPACE=rl-tunix-swebench \
    NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool \
    NODE_SELECTOR_VAL=standard-pool \
    python run_swebench.py
"""

import json
import logging
import os
import time

from k8s_agent_sandbox import SandboxClient
from kubernetes import client, config

import strategies
import warmpool as wp

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s"
)
logger = logging.getLogger("rl-tunix-swebench")

# --- Configuration (mirrors the env knobs of eval_deepswe.py) ----------------
DATASET_NAME = os.getenv("DATASET_NAME", "R2E-Gym/SWE-Bench-Verified")
DATASET_SPLIT = os.getenv("DATASET_SPLIT", "test")
TASKS_LIMIT = int(os.getenv("TASKS_LIMIT", "1"))
WARMPOOL_STRATEGY = os.getenv("WARMPOOL_STRATEGY", "naive")  # none|naive|sliding
WARMPOOL_WINDOW_SIZE = int(os.getenv("WARMPOOL_WINDOW_SIZE", "0"))  # 0 = auto
MAX_WARMPOOL_SIZE = int(os.getenv("MAX_WARMPOOL_SIZE", "8"))
# Global concurrency budget used to size warm pools (see sizing.py). Keep at 1
# while tasks run serially; raise it together with parallel execution.
MAX_CONCURRENT = int(os.getenv("MAX_CONCURRENT", "1"))
NAMESPACE = os.getenv("NAMESPACE", "rl-tunix-swebench")
SANDBOX_READY_TIMEOUT = int(os.getenv("SANDBOX_READY_TIMEOUT", "900"))

NODE_SELECTOR_KEY = os.getenv("NODE_SELECTOR_KEY", "")
NODE_SELECTOR_VAL = os.getenv("NODE_SELECTOR_VAL", "")
IMAGE_PULL_SECRET = os.getenv("IMAGE_PULL_SECRET", "") or None
RUNTIME_CLASS = os.getenv("RUNTIME_CLASS", "") or None  # e.g. "gvisor"

# Command run inside each sandbox to prove the SWE-bench task env is live.
# SWE-bench-verified images check out the repo under /testbed.
PROBE_COMMAND = [
    "bash",
    "-lc",
    "echo READY $(hostname); git -C /testbed log -1 --oneline 2>/dev/null"
    " || ls -d /testbed 2>/dev/null || ls /",
]


def load_entries() -> list[dict]:
  """Loads SWE-bench task entries (each carries a real ``docker_image``)."""
  from datasets import load_dataset

  logger.info("Loading dataset %s [%s]", DATASET_NAME, DATASET_SPLIT)
  ds = load_dataset(DATASET_NAME, split=DATASET_SPLIT)
  entries = [
      {"instance_id": r["instance_id"], "docker_image": r["docker_image"],
       "repo": r.get("repo", "")}
      for r in ds
  ]
  if TASKS_LIMIT > 0:
    entries = entries[:TASKS_LIMIT]
  logger.info(
      "Loaded %d tasks (%d unique images)",
      len(entries), len({e["docker_image"] for e in entries}),
  )
  return entries


def main():
  config.load_kube_config()
  custom_api = client.CustomObjectsApi()
  core_api = client.CoreV1Api()
  sandbox_client = SandboxClient()

  node_selector = (
      {NODE_SELECTOR_KEY: NODE_SELECTOR_VAL}
      if NODE_SELECTOR_KEY and NODE_SELECTOR_VAL
      else None
  )
  mgr = strategies.WarmPoolManager(
      custom_api,
      NAMESPACE,
      node_selector=node_selector,
      image_pull_secret=IMAGE_PULL_SECRET,
      runtime_class=RUNTIME_CLASS,
      ready_timeout=SANDBOX_READY_TIMEOUT,
  )

  results = []

  def process(task: dict, pool: str) -> None:
    instance_id = task.get("instance_id", "?")
    started = time.time()
    sandbox = sandbox_client.create_sandbox(
        warmpool=pool,
        namespace=NAMESPACE,
        sandbox_ready_timeout=SANDBOX_READY_TIMEOUT,
    )
    try:
      pod = sandbox.get_pod_name()
      output = wp.exec_in_pod(core_api, pod, NAMESPACE, PROBE_COMMAND)
      logger.info("[%s] pod=%s output=%s", instance_id, pod, output.strip())
      results.append({
          "instance_id": instance_id,
          "docker_image": task["docker_image"],
          "pod": pod,
          "output": output.strip(),
          "elapsed_s": round(time.time() - started, 1),
      })
    finally:
      sandbox.terminate()

  entries = load_entries()
  strategy = strategies.STRATEGIES[WARMPOOL_STRATEGY]
  logger.info("Running '%s' strategy over %d tasks", WARMPOOL_STRATEGY,
              len(entries))

  if WARMPOOL_STRATEGY == "none":
    strategy(entries, mgr, process)
  elif WARMPOOL_STRATEGY == "naive":
    strategy(entries, mgr, process, max_warmpool_size=MAX_WARMPOOL_SIZE,
             max_concurrent=MAX_CONCURRENT)
  else:  # sliding
    window = WARMPOOL_WINDOW_SIZE if WARMPOOL_WINDOW_SIZE > 0 else None
    strategy(entries, mgr, process, window_size=window,
             max_concurrent=MAX_CONCURRENT,
             max_warmpool_size=MAX_WARMPOOL_SIZE)

  print(json.dumps({"strategy": WARMPOOL_STRATEGY, "results": results}, indent=2))


if __name__ == "__main__":
  main()
