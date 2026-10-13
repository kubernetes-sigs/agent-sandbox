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
in-cluster connection and the cluster's Ray address. Run this command from
the directory containing `train.py` inside your Ray application environment:

```bash
python train.py \
  --ray-address auto \
  --connection-mode in-cluster \
  --num-env-runners 4
```

Here `in-cluster` is the legacy Python runtime transport, not sandboxd. For a
complete direct-sandboxd deployment, use the RayJob below. The Ray pods need
namespace-scoped Claim create/get/watch/delete and Sandbox get/watch; they do
not need list, Pod, Service, Secret or PVC permissions.

## Run a sandboxd RayJob on Kubernetes

The manifests in [kuberay/](kuberay/) use one namespace, a CPU-only Ray head,
one worker with two independent EnvRunner actors, a two-replica warm pool and
a head-only checkpoint PVC. Commands use sandboxd gRPC on port 9090; the
preflight also tests REST file write/read on port 8080. Each Sandbox has its
own Service DNS address. Neither the Sandbox Router nor port-forwarding is used.

Prerequisites:

- Agent Sandbox core **and extensions**, with the SandboxTemplate `service`
  and `networkPolicy` fields supported. Use the project's
  [installation/development instructions](../../../docs/development.md).
- KubeRay **1.7.0**, a default dynamic StorageClass and enough allocatable
  capacity. The initial Ray head/worker budget is 2 CPU / 4Gi **each**; reserve
  additional capacity for the operators, Sandbox Pods and transient overlap.
  A warm pool of two means two *unclaimed* spares, not a two-Pod total limit.
- A CNI implementing NetworkPolicy for isolation. Default kind networking can
  test functionality but does not prove the policy blocks other callers.
- A selected Kubernetes context and an image registry reachable from every
  node, or locally loaded images when using kind.

The shared image uses Ray/RLlib 2.58.0, Python 3.11, Gymnasium 1.2.2 and CPU
Torch 2.9.0. The SDK, Gymnasium integration and example code are installed from
the same local checkout. Builds do not need `.git`: setuptools-scm receives
the local build version `1.0.0+local`, not a published SDK release version.
The Ray CPU base is pinned to a multi-architecture image index supporting
Linux AMD64 and ARM64; the external application dependencies are pinned in
`requirements.lock`. Docker selects the base for the target platform. Build
the example image for your cluster's node architecture, using `--platform
linux/amd64` or `--platform linux/arm64` when it differs from the Docker host.
A normal `docker build` produces one target-platform image, not a multi-platform
example image. The earlier full RayJob validation ran on Linux ARM64; AMD64
training has not been validated. To override the base, pass
`--build-arg RAY_BASE_IMAGE=...` with an immutable Ray 2.58.0 / Python 3.11 CPU
image for the target platform.

### Build and choose images

Run from this repository's root. The example source is copied from this
checkout, including the SDK and Gymnasium integration. Dependencies are
installed at build time, never in running Pods. This example does not publish
an official RLlib image.

```bash
export RLLIB_IMAGE=kind.local/rllib-sandbox:local
export SANDBOXD_IMAGE=kind.local/sandboxd:local
docker build -f examples/agent-sandbox-gymnasium/rllib/kuberay/Dockerfile.rllib \
  -t "$RLLIB_IMAGE" .
docker run --rm "$RLLIB_IMAGE" python -m pip check
```

Build sandboxd from the same checkout:

```bash
docker build -f packages/sandboxd/Dockerfile -t "$SANDBOXD_IMAGE" .
```

For kind, load both images into **your selected test cluster**:

```bash
kind load docker-image "$RLLIB_IMAGE" "$SANDBOXD_IMAGE" --name YOUR_KIND_CLUSTER
```

For another cluster, instead build/tag/push to your own registry and set the
two image variables to those immutable references. Push is an explicit user
operation. All three Ray containers (head, worker and submitter) must use the
same example image; do not mix interpreter or Ray versions. Avoid `latest-main`
for sandboxd.

### Deploy in order

Check your context before writing resources. The namespace is intentionally
dedicated; do not mix the example with unrelated workloads.

```bash
kubectl config current-context
export KUBE_CONTEXT=YOUR_VERIFIED_CONTEXT
export EXAMPLE=examples/agent-sandbox-gymnasium/rllib/kuberay
kubectl --context "$KUBE_CONTEXT" get storageclass
kubectl --context "$KUBE_CONTEXT" apply -f "$EXAMPLE/namespace.yaml"
kubectl --context "$KUBE_CONTEXT" apply -f "$EXAMPLE/rbac.yaml"
sed "s|REPLACE_WITH_YOUR_SANDBOXD_IMAGE|$SANDBOXD_IMAGE|g" "$EXAMPLE/sandbox.yaml" \
  | kubectl --context "$KUBE_CONTEXT" apply -f -
kubectl --context "$KUBE_CONTEXT" apply -f "$EXAMPLE/checkpoint-pvc.yaml"
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example wait \
  --for=jsonpath='{.status.readyReplicas}'=2 \
  sandboxwarmpool/rllib-sandboxd-warmpool --timeout=180s
```

The PVC uses the default StorageClass; set `storageClassName` explicitly if
your cluster has none. With `WaitForFirstConsumer`, Pending is expected until
the head Pod is scheduled: **do not wait for Bound before submitting the job**.
Storage must support the head's UID 1000 / GID 100 and `fsGroup: 100`. The
driver probes actual writability before training; it does not assume fsGroup
works with every CSI driver.

```bash
sed "s|REPLACE_WITH_YOUR_RLLIB_IMAGE|$RLLIB_IMAGE|g" "$EXAMPLE/rayjob.yaml" \
  | kubectl --context "$KUBE_CONTEXT" apply -f -
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example wait \
  --for=jsonpath='{.status.rayClusterName}' \
  rayjob/rllib-sandbox-training --timeout=180s
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example wait \
  --for=jsonpath='{.status.phase}'=Bound pvc/rllib-checkpoints --timeout=180s
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example get rayjob,pods,jobs
```

The Ray ServiceAccount has only namespace-scoped Sandbox permissions. The
submitter has no Sandbox RoleBinding and no mounted API token; the Sandbox
Pods also have no token. Template ingress admits same-namespace Ray Pods with
the example label, only on 8080/9090; Sandbox egress is denied. Existing policies
that restrict Ray's DNS/API/internal traffic must be handled by the deployer,
not by granting cluster-wide access to this application.

### Observe and retrieve results

Replace `SUBMITTER_JOB_NAME` below with the exact Job name from the preceding
listing. KubeRay 1.7 follows the Ray driver logs:

```bash
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example logs \
  job/SUBMITTER_JOB_NAME -f
```

Before the 300-second cleanup TTL, also capture Ray Pod logs and namespace
events for troubleshooting:

```bash
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example logs \
  -l app.kubernetes.io/part-of=rllib-sandbox-example --all-containers --prefix
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example get events \
  --sort-by=.metadata.creationTimestamp
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example wait \
  --for=jsonpath='{.status.jobStatus}'=SUCCEEDED \
  rayjob/rllib-sandbox-training --timeout=900s
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example wait \
  --for=jsonpath='{.status.jobDeploymentStatus}'=Complete \
  rayjob/rllib-sandbox-training --timeout=180s
```

`--verify-run` performs deterministic command/REST preflight, five real PPO
iterations, complete per-EnvRunner evidence collection, checkpoint save,
original Algorithm stop and named Claim deletion checks, then a **new**
Algorithm restore/evaluation and final cleanup. It fails for missing remote
actors, shared Claim/Sandbox identities, task execution errors, missing positive
sample/train counters, bad checkpoints or Claim GET/cleanup failures. It does
not assert a training score: a valid policy episode with `success=False` is
not a transport failure. Automatic EnvRunner/environment restarts are disabled
because they would lose this example's in-memory evidence.

Look for JSON events `PREFLIGHT_OK`, `PPO_ITERATION`, `ENV_RUNNER_EVIDENCE`,
`CHECKPOINT_SAVED`, `CHECKPOINT_RESTORED`, `CLAIMS_DELETED`, then
`RUN_SUCCEEDED`. Before TTL, use the head Pod's `ray-head` container to inspect
or copy `/checkpoints/<run-id>/verification.json` and `algorithm/`. Each run has
its own directory. The checkpoint preserves RLlib state, **not** Sandbox files
or processes; workers do not mount the PVC.

After completion, RayCluster is removed after about 300 seconds; the independent
PVC remains. RayJob and submitter logs remain with the normal operator setting;
`DELETE_RAYJOB_CR_AFTER_JOB_FINISHES` changes that retention. Failed jobs may
also lose Ray Pods at TTL, so collect logs promptly. Re-run by deleting the old
RayJob and applying it again; the retained PVC uses a new run directory.

### Cleanup and failures

`RUN_FAILED` reports the root error, known Claims and cleanup errors. Named
GET checks have a shared 120-second deadline and bounded request timeouts;
only 404 proves deletion. For leftover Claims, inspect the exact namespace/name
from the report and delete those names normally. Do not use `--all`, force
deletion or a namespace sweep. The example does not guarantee cleanup after
SIGKILL, driver/Pod crashes or actor replacement, nor wait for background Pod GC.

Remove computation/configuration without deleting retained data:

```bash
kubectl --context "$KUBE_CONTEXT" delete -f "$EXAMPLE/rayjob.yaml" --ignore-not-found
# Inspect any remaining RayCluster and delete only this example's cluster.
kubectl --context "$KUBE_CONTEXT" delete -f "$EXAMPLE/sandbox.yaml" --ignore-not-found
kubectl --context "$KUBE_CONTEXT" delete -f "$EXAMPLE/rbac.yaml" --ignore-not-found
```

Do **not** delete the namespace while retaining the PVC. After backing up the
checkpoint, an optional separate data-deletion step is:

```bash
kubectl --context "$KUBE_CONTEXT" -n rllib-sandbox-example delete pvc/rllib-checkpoints
```

Depending on the PV reclaim policy, this can irreversibly delete the underlying
storage. Delete the namespace only after deciding to discard all its data.

## Cleanup behavior

- `SandboxEnv.reset()` terminates the previous episode's SandboxClaim before
  claiming a fresh Sandbox.
- `DiscreteFileTaskWrapper.close()` closes the current environment and calls
  `SandboxClient.delete_all()` as a second cleanup boundary.
- The training and verification entrypoints use `finally` blocks so handled
  failures still stop Algorithms, release claims, and shut down Ray.
- `SandboxClient(cleanup=True)` supplies best-effort process-exit cleanup for
  abrupt worker termination.

The example never asserts a fixed training score in CI. Lightweight tests cover
the deterministic adaptation, evidence/cleanup validators and cross-manifest
contracts without importing Ray or Torch; cluster-backed verification is explicit.
