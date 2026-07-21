# Plugging `agent-sandbox-rl` into an RL framework

The fleet is framework-agnostic. The contract is simple:

1. build a `FleetConfig` (one or many `ClusterConfig`s),
2. `load_tasks(...)`,
3. either drive the **primitives** yourself (`setup` → `acquire` → use
   `handle.hostname`/`handle.endpoint()`/`handle.exec(...)` → `release` →
   `teardown`), or call the **managed runner** `fleet.run(process_fn, strategy,
   concurrency)`.

A `SandboxHandle` is the integration point: `hostname` (stable in-cluster DNS),
`pod_name`, `pod_ip`, `endpoint(port)`, `exec(cmd)` (router-free), `release()`.

## Sizing for rollouts (instant claims)

RL typically samples **G rollouts per problem** (e.g. GRPO group size), so the *same*
problem image is claimed G times at once — unlike a 1:1 eval sweep. For that, turn on
the instant-claim levers so no rollout queues behind another:

```python
from agent_sandbox_rl import SandboxFleet, FleetConfig, ClusterConfig, TemplateSpec

fleet = SandboxFleet(FleetConfig(
    clusters=[ClusterConfig(name="c1", namespace="rl")],
    max_concurrent=64,
    max_warmpool_size=16,                 # >= rollouts per problem (G)
    warm_per_task=True,                   # one warm replica per rollout
    template=TemplateSpec(colocate_replicas=True)))  # G replicas, one image pull/node
fleet.run(rollout_fn, strategy="naive", keep_warm=True)   # pools persist across steps
```

This cuts the per-rollout **claim tail** — in a synchronous step you wait for the
slowest of G rollouts, so a stray queued claim delays the whole step. Use `naive` or
`sliding` (not `pipelined`, whose window shrinks under deep replicas), and
`keep_warm=True` to reuse pools across training steps. Full rationale and measurements:
[eval vs RL](../README.md#eval-vs-rl--recommended-recipes).

## Recycling — reuse one sandbox across a problem's G rollouts

Instant-claim (above) gives each rollout its *own* fresh sandbox. **Recycling** instead
holds **one** sandbox per problem and git-restore-resets it between rollouts, so **claims
scale with problems, not tasks** (÷G) — cutting claim latency and control-plane load at
RL scale. It's the recommended path for high G, and it's a wall-clock + reliability win
when a shallow warm pool would otherwise saturate under same-image claims.

```python
from agent_sandbox_rl import (SandboxFleet, FleetConfig, ClusterConfig, TemplateSpec,
                              ResourceSpec, SweBenchSource, Task, swebench_probe,
                              reuse_git_restore_sandbox, determinism_canary)
from agent_sandbox_rl.sources import to_tasks

# SHALLOW warm: recycle holds ~1 sandbox per problem, so 1 replica/pool is enough.
# Do NOT deep-warm (warm_per_task) for recycling — that stresses the warm-pool controller.
fleet = SandboxFleet(FleetConfig(
    clusters=[ClusterConfig(name="c1", namespace="rl")],
    max_concurrent=500,                                    # concurrent problems held
    template=TemplateSpec(resources=ResourceSpec(cpu="250m", memory="512Mi"))))

G = 16
problems = to_tasks(SweBenchSource(limit=500))
tasks = [Task(id=f"p{i:04d}-r{g}", image=t.image)         # P problems × G rollouts
         for i, t in enumerate(problems) for g in range(G)]
fleet.load_tasks(tasks)
fleet.setup()                                             # shallow warm (staged by default)

# Correctness gate FIRST — same task twice in one recycled sandbox must be byte-identical:
c = determinism_canary(fleet, problems[0], swebench_probe)
assert c["identical"] and c["reset_clean"], "reset leaks state — recycling is unsafe here"

# One claim per problem; git-restore reset between its G rollouts:
results = reuse_git_restore_sandbox(fleet, fleet.tasks, rollout_fn, concurrency=500)
fleet.teardown()
```

- **Claims ≈ P, not P·G** — the headline. Reset is git-only by default (`git reset --hard`
  + `clean -xdff` + verify the pristine SHA); a dirty reset **quarantines** the sandbox
  (fresh claim) so contamination can never silently bias rewards. A non-git `/testbed`
  transparently falls back to fresh-claim-per-task.
- **`determinism_canary`** is the ground-truth check — run it before trusting recycling
  for training.
- **Async** (Ray / SkyRL / tunix loops): `reuse_git_restore_sandbox_async(afleet, …)` —
  a coroutine per group. `shards_per_image=K` runs K sandboxes/image in parallel (saturate
  a small image set); `claim_concurrency=N` staged-claims to stay under the apiserver.

### Safeguards at scale (on by default)
Large/deep warm pools can stress the warm-pool controller
([#1215](https://github.com/kubernetes-sigs/agent-sandbox/issues/1215)); the SDK is
fail-safe by default:
- **Circuit breaker** — `FleetConfig.overcommit_factor` (1.5) / `max_live_sandboxes`: if
  live sandboxes exceed the ceiling, the fleet tears down and raises `FleetOvercommitError`
  (catches accidental over-creation — runaway / orphan).
- **Guaranteed teardown + reaper** — every resource is labelled with `fleet.run_id`;
  `atexit`/SIGINT/SIGTERM tear down on graceful exit, and **`reap(run_id=…)`** /
  `python -m agent_sandbox_rl.reaper` sweeps an **orphaned** run (SIGKILL / OOM / node loss).
- **Staged fill/claims** — `warm_create_budget` (staged warm) + `claim_concurrency` bound
  concurrent apiserver ops; for large deep warms also keep the controller's
  `--sandbox-warm-pool-concurrent-workers` low (≤10).

## Generic env wrapper (primitives)

```python
from agent_sandbox_rl import SandboxFleet, FleetConfig, ClusterConfig, SweBenchSource

fleet = SandboxFleet(FleetConfig(
    clusters=[ClusterConfig(name="c1", namespace="rl")],
    max_concurrent=16, max_warmpool_size=32, placement="image-affinity"))
fleet.load_tasks(SweBenchSource(limit=500))
fleet.setup()                         # preflight + plan + warm pools

class SweEnv:
    def reset(self, task):
        self.h = fleet.acquire(task)  # a live, isolated sandbox
        return self.h.endpoint()      # connect your agent here (or self.h.exec)
    def step(self, action):
        return self.h.exec(action)    # router-free command exec
    def close(self):
        fleet.release(self.h)

# ... run rollouts ...
fleet.teardown()
```

## R2E-Gym + tunix deepswe (the real path)

tunix deepswe doesn't provision sandboxes itself — it goes through R2E-Gym:

```
tunix SWEEnv (swe_env.py)  →  R2E-Gym RepoEnv(backend="kubernetes-sandbox")  →  DockerRuntime  →  a sandbox
```

R2E-Gym's `kubernetes-sandbox` backend **cold-creates** a sandbox per env, and
`eval_deepswe.py` reimplements warm pools inline (against the old `v1alpha1` CRDs:
`TEMPLATE_STR`, `create_warmpool`/`delete_warmpool`, the `active_warmpools` sliding
loop in `run_evaluation`). The `agent-sandbox-rl` **R2E-Gym adapter** replaces all
of that: it binds a fleet-pre-warmed pod (v1beta1, sized, observed) into R2E-Gym's
`RepoEnv`, so the same `RepoEnv`/reward path runs unchanged on warm pools.

```python
from agent_sandbox_rl import SandboxFleet, FleetConfig, ClusterConfig
from agent_sandbox_rl.adapters.swebench import SweBenchSource
from agent_sandbox_rl.adapters.r2egym import make_fleet_repo_env, r2egym_command_files

fleet = SandboxFleet(FleetConfig(clusters=[ClusterConfig(namespace=NS)],
                                 max_concurrent=MAX_CONCURRENT))
fleet.load_tasks(SweBenchSource(limit=500, keep_row=True))   # keep_row REQUIRED

def rollout(task, handle):                    # one warm pod per task
    env = make_fleet_repo_env(handle, command_files=r2egym_command_files())
    try:
        instruction = env.get_task_instruction()
        # ... your agent loop: obs, _, done, _ = env.step(action) ...
        return env.compute_reward()           # real R2E-Gym grading
    finally:
        env.close()                           # no-op teardown; fleet owns the pod

results = fleet.run(rollout, strategy="sliding", concurrency=MAX_CONCURRENT)
```

Contracts:
- **`keep_row=True`** stores the full dataset row under `task.metadata["ds"]`,
  which R2E-Gym's env + reward grading require.
- **Namespace flows from the handle** (`ClusterConfig.namespace`) into R2E-Gym's
  exec/file-copy automatically — no need to match R2E-Gym's hardcoded `default`.
- **The fleet owns the pod.** `env.close()` never deletes it; `fleet.run` /
  `fleet.release(handle)` does. One episode per acquire (for a fresh pod,
  release + acquire).

To wire it into tunix's `SWEEnv` directly, subclass it to build a `FleetRepoEnv`
instead of `RepoEnv` (acquire in `_initial_observation`, release in `close`); the
fleet then replaces `eval_deepswe.py`'s inline warm-pool management.
[`examples/deepswe_eval_nb.ipynb`](deepswe_eval_nb.ipynb) is a runnable,
**no-model** demo of this path (stub policy) — it falls back to a router-free
`exec` probe when R2E-Gym isn't installed.

Requires R2E-Gym, which isn't on PyPI — install it from its checkout
(`pip install -e path/to/R2E-Gym`). There is no `r2egym` extra.

## TorchRL / SkyRL

Wrap `acquire`/`release` around an episode in your `EnvBase`/env:

```python
class SandboxEnv(EnvBase):
    def _reset(self, td):
        self._h = fleet.acquire(self._task)
        ...
    def _step(self, td):
        obs = self._h.exec(td["action"])
        ...
    def close(self):
        fleet.release(self._h)
```

For async frameworks, use `AsyncSandboxFleet` (awaitable `acquire`/`release`/
`run`; `process_fn` may be a coroutine):

```python
from agent_sandbox_rl import AsyncSandboxFleet
fleet = AsyncSandboxFleet(cfg); fleet.load_tasks(src)
results = await fleet.run(async_rollout, strategy="sliding", concurrency=64)
```

## Multi-cluster

Give several `ClusterConfig`s (different `context`/`kubeconfig`) and a
`placement` policy; the fleet spreads pools/claims across clusters and each
`SandboxHandle` carries its owning cluster's connection info:

```python
FleetConfig(clusters=[
    ClusterConfig(name="us-central2", context="ctx-a", namespace="rl"),
    ClusterConfig(name="us-east1",   context="ctx-b", namespace="rl", weight=2.0),
], placement="image-affinity", max_concurrent=128)
```

Cross-cluster reachability is the caller's concern: in-cluster learners use the
sandbox DNS hostname; out-of-cluster learners need per-cluster routable endpoints
(Gateway/LoadBalancer) or co-located workers.
