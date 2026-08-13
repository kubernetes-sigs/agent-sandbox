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

"""Interactive capacity planner — consults you, then recommends a preload plan.

Walks you through picking a cluster + node pool and your batch shape, probes the pool's
CPU / disk / pod capacity, and prints the recommended strategy, ``max_concurrent``,
per-image replicas, and the binding bottleneck. **Plan-only and read-only** — it never
creates pools; to actually run, it prints the exact ``run_full_swebench_benchmark.py
--execute`` command (or pass ``--yes`` to be reminded).

    python examples/plan_capacity.py                # interactive prompts
    python examples/plan_capacity.py --context CTX --node-pool gvisor-pool-500 \
        --n-images 500 --tasks-per-image 1 --non-interactive

Under the hood it's just the package API — drop these three lines into your own code:

    from agent_sandbox_rl import Cluster, ClusterConfig, probe_capacity, plan_benchmark, render_plan
    cap  = probe_capacity(Cluster(ClusterConfig(context=ctx)).core_api, f"{label}={pool}")
    plan = plan_benchmark(cap, n_images=500, tasks_per_image=1)
"""

from __future__ import annotations

import argparse
import os
import sys

from agent_sandbox_rl import (Cluster, ClusterConfig, plan_benchmark,
                              probe_capacity, render_plan)

POOL_LABEL = "cloud.google.com/gke-nodepool"


def _ask(prompt: str, default: str) -> str:
    """Prompt with a default shown in [brackets]; Enter accepts the default."""
    try:
        ans = input(f"{prompt} [{default}]: ").strip()
    except EOFError:
        ans = ""
    return ans or default


def _ask_num(prompt: str, default, cast):
    """Like `_ask` but parses with ``cast`` (int/float), re-prompting on bad input."""
    while True:
        raw = _ask(prompt, str(default))
        try:
            return cast(raw)
        except ValueError:
            print(f"  '{raw}' is not a valid {cast.__name__}; try again.")


def _list_pools(core_api) -> list[str]:
    """Distinct node-pool labels in the cluster (best-effort)."""
    try:
        nodes = core_api.list_node().items
    except Exception:  # noqa: BLE001 — best-effort menu; selector can be typed manually
        return []
    return sorted({(n.metadata.labels or {}).get(POOL_LABEL) for n in nodes
                   if n.metadata and (n.metadata.labels or {}).get(POOL_LABEL)})


def _pick_pool(core_api, default_pool: str | None) -> str | None:
    pools = _list_pools(core_api)
    if not pools:
        return _ask("Node pool label value (blank = whole cluster)", default_pool or "") or None
    print("\nNode pools found:")
    for i, p in enumerate(pools, 1):
        print(f"  {i}) {p}")
    print("  0) whole cluster (no selector)")
    dflt = str(pools.index(default_pool) + 1) if default_pool in pools else "1"
    choice = _ask("Pick a pool by number", dflt)
    try:
        idx = int(choice)
    except ValueError:
        return choice or None        # treat as a literal pool name
    if idx == 0:
        return None
    return pools[idx - 1] if 1 <= idx <= len(pools) else None


def main(argv=None):
    args = _build_parser().parse_args(argv)
    interactive = not args.non_interactive and sys.stdin.isatty()

    context = args.context
    namespace = args.namespace
    if interactive:
        print("=== agent-sandbox-rl capacity planner ===")
        context = _ask("Kube context (blank = ambient)", context or "") or None
        namespace = _ask("Namespace", namespace)

    cluster = Cluster(ClusterConfig(name="planner", context=context, namespace=namespace))

    pool = args.node_pool
    if interactive:
        pool = _pick_pool(cluster.core_api, pool)
    selector = f"{POOL_LABEL}={pool}" if pool else None

    n_images = args.n_images
    tasks_per_image = args.tasks_per_image
    avg_image_gb = args.avg_image_gb
    cpu_request = args.cpu_request
    if interactive:
        n_images = _ask_num("Number of distinct images", n_images, int)
        tasks_per_image = _ask_num("Tasks per image (RL: rollouts/problem)", tasks_per_image, int)
        avg_image_gb = _ask_num("Avg uncompressed image size (GiB)", avg_image_gb, float)
        cpu_request = _ask_num("Per-pod CPU request (millicores)", cpu_request, int)

    print(f"\nProbing capacity ({selector or 'whole cluster'}) ...", flush=True)
    cap = probe_capacity(cluster.core_api, node_selector=selector)
    plan = plan_benchmark(cap, n_images, tasks_per_image,
                          avg_image_gb=avg_image_gb, cpu_request_milli=cpu_request,
                          max_pool=args.max_warmpool_size)

    print("\n" + render_plan(cap, plan) + "\n")

    # How to actually run it (this wizard never creates pools).
    ctx_flag = f" --context {context}" if context else ""
    sel_flag = f" --node-selector {selector}" if selector else ""
    cmd = (f"PYTHONPATH=. python tests/run_full_swebench_benchmark.py{ctx_flag}"
           f" --namespace {namespace}{sel_flag} --n-images {n_images}"
           f" --tasks-per-image {tasks_per_image} --avg-image-gb {avg_image_gb}"
           f" --cpu-request {cpu_request}"
           f" --max-warmpool-size {args.max_warmpool_size}"
           f" --execute --limit {plan.n_tasks}")
    run = args.yes
    if interactive and not run:
        run = _ask("Run it now? (this creates pools + runs the batch) [y/N]", "N").lower() == "y"
    if run:
        print("\nExecuting via the benchmark runner ...\n", flush=True)
        tests_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "tests")
        sys.path.insert(0, tests_dir)          # robust to cwd
        import run_full_swebench_benchmark as runner
        runner.main([
            *(["--context", context] if context else []),
            "--namespace", namespace,
            *(["--node-selector", selector] if selector else []),
            "--n-images", str(n_images), "--tasks-per-image", str(tasks_per_image),
            "--avg-image-gb", str(avg_image_gb), "--cpu-request", str(cpu_request),
            "--max-warmpool-size", str(args.max_warmpool_size),
            "--execute", "--limit", str(plan.n_tasks),
        ])
    else:
        print("Plan only — no pools created. To run it:\n\n  " + cmd + "\n")


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="Interactive capacity planner for agent-sandbox-rl.")
    p.add_argument("--context", default=None, help="kube context (default: ambient)")
    p.add_argument("--namespace", default="default")
    p.add_argument("--node-pool", default=None, help=f"value of the {POOL_LABEL} label")
    p.add_argument("--n-images", type=int, default=500)
    p.add_argument("--tasks-per-image", type=int, default=1)
    p.add_argument("--avg-image-gb", type=float, default=10.0)
    p.add_argument("--cpu-request", type=int, default=250)
    p.add_argument("--max-warmpool-size", type=int, default=64)
    p.add_argument("--non-interactive", action="store_true",
                   help="skip prompts; use flags/defaults (CI/scripting)")
    p.add_argument("--yes", action="store_true", help="execute the plan without prompting")
    return p


if __name__ == "__main__":
    main()
