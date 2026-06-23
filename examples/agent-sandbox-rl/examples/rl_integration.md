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
