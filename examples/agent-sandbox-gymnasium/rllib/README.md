# RLlib training with SandboxEnv

This example trains a small RLlib PPO policy on a CPU-only file task executed
inside real Kubernetes Agent Sandboxes. It connects the existing text-native
`SandboxEnv` integration to RLlib without using an LLM or a pretrained model.

The policy must learn the following two-step sequence:

```text
create-directory → write-file
```

Each RLlib EnvRunner constructs its own `SandboxClient` and `SandboxEnv`. A new
SandboxClaim is created on episode reset and released when the episode is
replaced or the environment closes.

## Space adaptation

`SandboxEnv` accepts shell-command strings and returns stdout/stderr strings.
This example maps that interface to a `Discrete` action space and a numeric
`Box` observation before passing it to PPO:

| RLlib action | Command executed in the Sandbox |
| --- | --- |
| `0` | Create `/tmp/agent-sandbox-rllib/output` |
| `1` | Write the expected `answer.txt` if the directory exists |
| `2` | Remove the task output |

After every action, the fixed command reports these four observation values:

```text
[directory_exists, file_exists, content_is_correct, remaining_steps]
```

The adapter gives `+1.0` when the expected file exists, `-0.05` for an
unfinished step, and `-1.0` for a Sandbox connection or observation error.

## Prerequisites

- Python 3.11 or 3.12.
- A Kubernetes cluster with the Agent Sandbox controller and extensions.
- The Sandbox Router when using the default local tunnel mode.
- The runtime and two-replica warm pool from the parent
  [Gymnasium example](../README.md), named `simple-sandbox-warmpool` in the
  `gymnasium` namespace.

The two replicas match the default EnvRunner concurrency and make the initial
episode resets warm. Every claimed Sandbox must be replenished, so sustained
training can still fall back to cold creation when episodes finish faster than
replacement Sandboxes become ready. Size the pool for both EnvRunner
concurrency and the expected reset rate relative to Sandbox startup latency.

## Install

From the repository root, create an isolated environment and install the local
SDK, local Gymnasium integration, and RLlib dependencies:

```bash
python3 -m venv bin/python-venv-rllib
bin/python-venv-rllib/bin/pip install \
  -e clients/python/agentic-sandbox-client \
  -e clients/integrations/gymnasium
bin/python-venv-rllib/bin/pip install \
  -r examples/agent-sandbox-gymnasium/rllib/requirements.txt
```

RLlib 2.58.0 pins Gymnasium 1.2.2. The in-repository Gymnasium integration
therefore supports Gymnasium 1.2.2 and later 1.x releases.

## Verify the Sandbox task

Before training, run the deterministic two-action path once against the
cluster:

```bash
bin/python-venv-rllib/bin/python \
  examples/agent-sandbox-gymnasium/rllib/verify_sandbox_task.py
```

The verification script claims one Sandbox, creates the directory, writes the
expected file, verifies the terminal observation, and releases the claim in
`finally`.

## Train locally

The default tunnel connection lets local Ray worker processes reach the
Sandbox Router through `kubectl port-forward`:

```bash
bin/python-venv-rllib/bin/python \
  examples/agent-sandbox-gymnasium/rllib/train.py
```

The program performs a deterministic evaluation before training, trains PPO
for five iterations by default, evaluates again, saves a checkpoint below
`bin/`, stops the original Algorithm, and verifies the checkpoint with a
separately restored Algorithm. Use `--iterations` to change the training
duration.

Training is stochastic. A successful learned episode should look like:

```text
create-directory  reward=-0.05
write-file        reward=+1.00
return=+0.95 success=True
```

## Multiple EnvRunners and KubeRay

`--num-env-runners` controls the number of independent rollout processes. Each
process creates its Kubernetes client locally rather than serializing a client
from the driver:

```bash
bin/python-venv-rllib/bin/python \
  examples/agent-sandbox-gymnasium/rllib/train.py \
  --num-env-runners 4
```

When the driver and EnvRunners execute inside a RayCluster or RayJob, use an
in-cluster connection and the cluster's Ray address:

```bash
python train.py \
  --ray-address auto \
  --connection-mode in-cluster \
  --num-env-runners 4
```

The Ray pods' service account must be allowed to create, get, list, watch, and
delete `SandboxClaim` objects and to get, list, and watch `Sandbox` objects in
the selected namespace.

## Cleanup behavior

- `SandboxEnv.reset()` terminates the previous episode's SandboxClaim before
  claiming a fresh Sandbox.
- `DiscreteFileTaskWrapper.close()` closes the current environment and calls
  `SandboxClient.delete_all()` as a second cleanup boundary.
- The training and verification entrypoints use `finally` blocks so handled
  failures still stop Algorithms, release claims, and shut down Ray.
- `SandboxClient(cleanup=True)` supplies best-effort process-exit cleanup for
  abrupt worker termination.

The example never asserts a fixed training score in CI. Unit tests cover only
the deterministic action/observation adaptation and cleanup behavior;
cluster-backed verification is explicit.
