# CI Runner Sandbox

Run each CI job in its own Agent Sandbox. Instead of executing untrusted
pull-request code on a shared, long-lived runner, the CI system claims a
pre-warmed sandbox per job, runs the build and test commands inside it,
streams the output back to the CI log, and destroys the sandbox when the job
ends. Every job gets a clean environment, a hard resource ceiling, and
isolation from other jobs — and a crashed CI job cannot leak a sandbox,
because the claim carries a shutdown time the controller enforces.

This example is CI-agnostic. The driver script works from any system that can
run Python and reach the cluster's API server; a reference GitHub Actions
workflow is included.

## Files

* `Dockerfile`: CI tooling image — the Agent Sandbox Python runtime base plus
  `git`, `make`, `jq`, and `curl`. Extend it with your project's toolchain.
* `sandboxtemplate.yaml`: `ci-runner-template` — non-root, no service account
  token, an emptyDir `/workspace`, and probes on the runtime port (8888).
* `sandboxwarmpool.yaml`: `ci-runner-pool` — keeps 2 ready sandboxes so jobs
  claim in seconds instead of waiting for a cold pod start.
* `run_ci_job.py`: the driver. Claims a sandbox, runs commands in order,
  echoes their output, exits with the first failing command's exit code, and
  always terminates the sandbox.
* `github-actions-workflow.yaml`: reference workflow. Inert here — copy it to
  your repository's `.github/workflows/` to activate it.
* `requirements.txt`: driver dependency (`k8s-agent-sandbox`).
* `test_run_ci_job.py`: driver unit tests (mocked SDK, no cluster needed).

## Prerequisites

* A Kubernetes cluster with agent-sandbox **and the extension controllers**
  installed (`EXTENSIONS=true make deploy-kind` creates a local one).
* Python >= 3.11 with `pip install -r requirements.txt`.
* A kubeconfig whose credentials may create `SandboxClaim` objects in the CI
  namespace. Grant the least privilege you can — see
  [sandbox-ksa](../sandbox-ksa) for scoping sandbox-side credentials and your
  cluster's RBAC tooling for the CI side.

## Quickstart (kind)

1. Create a local cluster with the extension controllers and load the CI
   image into it:

   ```bash
   EXTENSIONS=true make deploy-kind
   docker build -t ci-runner-sandbox:v1 examples/ci-runner-sandbox
   kind load docker-image ci-runner-sandbox:v1 --name agent-sandbox
   ```

2. Apply the template and warm pool, then wait for it to be ready:

   ```bash
   kubectl --kubeconfig bin/KUBECONFIG apply -f examples/ci-runner-sandbox/sandboxtemplate.yaml \
                                             -f examples/ci-runner-sandbox/sandboxwarmpool.yaml
   kubectl --kubeconfig bin/KUBECONFIG wait --for=jsonpath='{.status.readyReplicas}'=2 \
     sandboxwarmpool/ci-runner-pool --timeout=120s
   ```

3. Run a job. Commands after `--` execute in order inside the sandbox's
   `/workspace`:

   ```bash
   export KUBECONFIG=bin/KUBECONFIG
   python examples/ci-runner-sandbox/run_ci_job.py -- \
     "echo hello from the sandbox" \
     "python --version && git --version" \
     "exit 3"
   ```

   The driver prints each command's output, stops at the failing one, and
   exits with code `3` — the same contract a CI system expects from a build
   step. Verify the sandbox is gone:

   ```bash
   kubectl get sandboxclaims,sandboxes   # no ci-* objects remain
   ```

## Using it from CI

Point the driver at your cluster and pass your real build commands:

```bash
python run_ci_job.py \
  --namespace ci \
  --claim-name "${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" \
  --timeout 1800 \
  -- \
  "git clone https://github.com/myorg/myrepo /workspace/repo" \
  "cd /workspace/repo && make test"
```

| Flag / env | Meaning |
| --- | --- |
| `--namespace` / `SANDBOX_NAMESPACE` | Namespace of the warm pool (default `default`). |
| `--warmpool` / `SANDBOX_WARMPOOL_NAME` | Pool to claim from (default `ci-runner-pool`). |
| `--timeout` / `CI_COMMAND_TIMEOUT` | Per-command wall-clock limit in seconds (default 1800). A killed command exits with code 124. |
| `--ttl` | Backstop claim lifetime in seconds (default 3600). The controller deletes the claim and sandbox after it even if the driver dies. |
| `--claim-name` / `GITHUB_RUN_ID` | Run id embedded in the claim name (`ci-<sanitized-id>`) so a leaked sandbox traces back to its job. |
| `SANDBOX_KUBE_CONTEXT` | Optional kubeconfig context pinning both the claim and the data-plane tunnel to one cluster. |

See `github-actions-workflow.yaml` for a complete job definition, including
kubeconfig handling from a repository secret.

## Cleanup semantics

Three layers make sure no sandbox outlives its job:

1. The driver calls `terminate()` in a `finally` block — the normal path.
2. The claim's `shutdownTime` (set from `--ttl`) — the controller deletes the
   claim and sandbox even if the CI job is canceled or the runner crashes.
3. `shutdownPolicy: Delete` on SDK-created claims — nothing lingers in an
   expired state.

## Production notes

* **Real clusters:** push the image to a registry and change
  `imagePullPolicy` to `IfNotPresent` in the template.
* **Sizing the pool:** set warm pool `replicas` to your peak number of
  concurrent jobs; claims beyond the pool wait for a cold start. Autoscale
  with [hpa-swp-scaling](../hpa-swp-scaling) or
  [keda-scale-to-zero](../keda-scale-to-zero).
* **Egress control:** CI jobs often only need your git host, package
  registries, and artifact storage. Restrict egress with a NetworkPolicy —
  see [composing-sandbox-nw-policies](../composing-sandbox-nw-policies) and
  [demo-cilium-egress](../demo-cilium-egress).
* **Dependency caches:** this example uses an emptyDir workspace, so every
  job downloads dependencies from scratch. For caches that survive across
  jobs, add `volumeClaimTemplates` to the template (see
  [warmpool-quickstart](../warmpool-quickstart)).

## Related examples

* [warmpool-quickstart](../warmpool-quickstart): the template/pool/claim
  lifecycle this example builds on.
* [claude-code-sandbox](../claude-code-sandbox): the same Python SDK driver
  pattern, driving a coding agent instead of CI commands.
* [kueue-agent-sandbox](../kueue-agent-sandbox): admission-control sandbox
  workloads when CI demand exceeds cluster capacity.
