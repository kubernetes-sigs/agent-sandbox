# Terminal-Bench on Agent Sandbox with Harbor

This example runs [Terminal-Bench 2.0](https://github.com/laude-institute/terminal-bench-2)
through the [Harbor](https://github.com/harbor-framework/harbor) evaluation
framework with every trial inside an Agent Sandbox `SandboxClaim`, using the
[`k8s-agent-sandbox-harbor`](../../clients/integrations/harbor) integration.
It covers a smoke run on kind with the oracle agent, the full benchmark on GKE
with a model, and how to read the results.

## What runs where

```
laptop / CI runner                                   Kubernetes cluster
+---------------------------------------+            +---------------------------------------------+
| harbor run -d terminal-bench@2.0      |            | namespace harbor                            |
|   -a terminus-2 -m <model> -n 50      |            |                                             |
|   -e k8s_agent_sandbox_harbor:        |  claims,   |  SandboxTemplate  hb-3f9a...  (per image)   |
|      AgentSandboxEnvironment          |  exec,     |  SandboxWarmPool  hb-3f9a...  replicas 0|N  |
|                                       |  tar  ---> |  SandboxClaim     hb-trial-8c1d (per trial) |
|  one environment object per trial     |            |     -> Sandbox -> Pod (task image, root,    |
|  trajectories + result.json per trial |            |        sleep infinity, 1 cpu / 2 GiB)       |
+---------------------------------------+            +---------------------------------------------+
```

Harbor does not know Kubernetes here. It calls the environment's `start`,
`exec`, `upload_dir`, `download_dir` and `stop`; the integration maps them onto
agent-sandbox objects and the pod exec API. The task image is the one Terminal-Bench
publishes for each task (`alexgshaw/<task>:<tag>`); nothing is installed in it.

## How one trial works

Take the sample task `hello-world` (1 CPU, 2 GiB, image `alexgshaw/hello-world:<tag>`),
run with the oracle agent, which applies the task's reference solution:

1. **Preflight.** Harbor imports `AgentSandboxEnvironment` and calls `preflight()`:
   the kubeconfig loads and the cluster serves `extensions.agents.x-k8s.io`.
2. **start().** The environment renders a pod spec from the task (image, requests,
   limits, root, no service-account token, the egress rule below) and ensures a
   `SandboxTemplate` and a `SandboxWarmPool` named by the hash of that spec, so the
   10 sample tasks produce 10 templates and 10 pools, once. It then creates a
   `SandboxClaim` against the pool with a six-hour shutdown time and waits until the
   claim is Ready and the container accepts exec. With `replicas: 0` the controller
   cold-creates the pod; with `warm_replicas: 1` a pod is waiting and the claim binds
   in about a second.
3. **Directories.** `mkdir -p /logs/agent /logs/verifier /logs/artifacts /tests /solution`
   as root, Harbor's standard layout.
4. **Agent.** The oracle uploads `solution/` as a tar stream and runs `solve.sh`.
   Terminus 2 would instead install tmux, start a session and loop on
   `tmux send-keys` and `capture-pane`, each one an exec. Every exec is
   `bash -c <command>` with the task's working directory, Harbor's env, `su` when a
   user is set, and coreutils `timeout` for the deadline (exit 124), which is the
   same contract Harbor's own Kubernetes provider gives agents.
5. **Verifier.** Harbor uploads `tests/` to `/tests` and runs `test.sh`, which on
   Terminal-Bench installs `curl` and `uv` and runs pytest. That needs DNS and
   internet egress; see the network note. It then downloads `/logs/verifier` and reads
   `reward.txt`.
6. **stop().** The claim is deleted; the pod is gone within seconds. With
   `--no-delete` the claim stays and the log prints the `kubectl exec` to inspect it.
7. **Results.** `jobs/harbor-terminal-bench/<job>/<trial>/result.json` holds the
   reward, timings and any exception; `scripts/report.py` turns a job into a table.

**Network.** Agent Sandbox's default per-template NetworkPolicy allows egress to
public addresses only and blocks every private range, cluster DNS included. A task
could not resolve `pypi.org`. The template therefore carries its own egress rule:
kube-dns on port 53 plus public IPv4 and IPv6, and no ingress (the exec API reaches
the pod through the kubelet, not the pod network). `--ek network=managed-default`
restores the controller default for tasks that need no network.

## Run on kind

Requirements: Docker, kind, Python 3.12 or newer, about 10 GB of disk for the sample
images.

```bash
# 1. A kind cluster with the controller and the extension controllers.
EXTENSIONS=true make deploy-kind
export KUBECONFIG=$PWD/bin/KUBECONFIG

# 2. Harbor and the integration, in one Python environment.
python3.12 -m venv .venv-harbor && . .venv-harbor/bin/activate
pip install "harbor>=0.25" \
  -e clients/python/agentic-sandbox-client[async] \
  -e clients/integrations/harbor

# 3. Namespace, then the smoke run (10 tasks, oracle agent, 4 at a time).
kubectl apply -f examples/harbor-terminal-bench/deploy/namespace.yaml
examples/harbor-terminal-bench/scripts/run-sample.sh
```

`run-sample.sh` applies the namespace, runs `harbor run -c configs/sample.yaml`,
lists leftover claims (expect none) and prints the report. While it runs:

```bash
kubectl -n harbor get sandboxtemplates,sandboxwarmpools,sandboxclaims
kubectl -n harbor get pods -w
```

Expected outcome: 10 of 10 trials pass with reward 1.0. The first trial of each
task pays the image pull on the kind node (one to three minutes for the larger
images; `ready_timeout_s` is 600 in the sample config for that reason); later
attempts of the same task start in seconds.

To keep a sandbox for inspection, add `--no-delete`:

```bash
harbor run -c examples/harbor-terminal-bench/configs/sample.yaml --no-delete -n 1
kubectl -n harbor exec -it <pod> -c main -- bash
examples/harbor-terminal-bench/scripts/cleanup.sh harbor --claims
```

## Run on GKE

1. **Cluster.** Agent Sandbox with the extension controllers installed (see the
   repository README). A node pool for the sandboxes with
   [Image Streaming](https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming)
   enabled; Terminal-Bench images are 0.5 to 3 GB and streaming cuts the cold start
   to seconds. For isolation, a GKE Sandbox (gVisor) pool and `configs/gvisor.yaml`.
2. **RBAC.** `kubectl apply -f deploy/namespace.yaml -f deploy/rbac.yaml`, then bind
   the role to the identity that runs Harbor:
   `kubectl -n harbor create rolebinding harbor-runner --role=harbor-runner --user=<you>`.
3. **Images.** Docker Hub rate-limits anonymous pulls, and a 50-wide run pulls 89
   images across many nodes. Mirror them once into Artifact Registry, then point
   the run at the mirror:

   ```bash
   examples/harbor-terminal-bench/scripts/mirror-images.sh us-central1-docker.pkg.dev/<project>/hub-mirror
   ```

   Grant the node service account `roles/artifactregistry.reader` on that repository.
4. **Run.**

   ```bash
   harbor run -c examples/harbor-terminal-bench/configs/tb2.yaml \
     -m <provider/model> \
     --ek image_mirror=us-central1-docker.pkg.dev/<project>/hub-mirror
   # under gVisor:
   harbor run -c examples/harbor-terminal-bench/configs/tb2.yaml -c examples/harbor-terminal-bench/configs/gvisor.yaml -m <provider/model> --ek image_mirror=...
   ```

   `tb2.yaml` runs 5 attempts per task, 50 trials at a time, with a memory limit of
   twice the task's request. `warm_replicas: 1` keeps one pod per task image ready
   between attempts at the price of 89 idle pods; leave it at 0 for a cold run.
5. **Report.** `python examples/harbor-terminal-bench/scripts/report.py jobs/harbor-terminal-bench/<job>`.

Model credentials are Harbor's business (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and
so on, as for any Harbor run). The sandbox never sees them: Harbor passes the model
output in, not the key.

## Reading the report

```
| task | trial | reward | outcome | env setup s | agent s | verifier s |
|---|---|---|---|---|---|---|
| hello-world | hello-world__a1b2c3 | 1 | pass | 14.2 | 3.1 | 22.7 |
...
10/10 passed, 0 infrastructure errors, env setup p50 12.9 s / p95 48.0 s
```

`env setup` is Harbor's own timing of `start()`: template and pool creation (once per
image), the claim becoming Ready (image pull on a cold node dominates), and the exec
readiness probe. `outcome` is `pass` for reward 1, `fail` for a lower reward, and
`error: <type>` when Harbor recorded an exception; an `error` row is an
infrastructure or harness problem, not a model failure, and the count of those is the
number to watch when changing the cluster.

The results table for the recorded runs on kind and GKE is added to this README
once this example has run on both.

## Files

| Path | What it is |
| --- | --- |
| `deploy/namespace.yaml` | The `harbor` namespace with Pod Security Admission `baseline` (Harbor needs root in the pod). |
| `deploy/rbac.yaml` | A Role with the verbs the runner needs: the three extension kinds, `sandboxes` read, pods read, `pods/exec`. |
| `configs/sample.yaml` | Oracle agent on `terminal-bench-sample@2.0`, 4 concurrent trials. |
| `configs/tb2.yaml` | Terminus 2 on `terminal-bench@2.0`, 5 attempts, 50 concurrent. |
| `configs/gvisor.yaml` | Overlay: gVisor runtime class, node selector and toleration for a GKE Sandbox pool. |
| `scripts/run-sample.sh` | The kind smoke run end to end. |
| `scripts/mirror-images.sh` | Copies a dataset's task images into a registry with `crane`. |
| `scripts/cleanup.sh` | Deletes leftover claims, or everything the integration created. |
| `scripts/report.py` | Summarizes a job directory; standard library only, tested in `tests/`. |

## Troubleshooting

- **Verifier fails with `Could not resolve host` or `apt-get` errors.** The template
  egress rule is missing or the cluster's DNS pods do not carry `k8s-app: kube-dns`
  in `kube-system`. Check `kubectl -n harbor get networkpolicy` and the rule's pod
  selector; on clusters with a different DNS label, use `--ek network=managed-default`
  and add a namespace-wide policy for DNS.
- **Pod rejected by Pod Security Admission.** The namespace enforces `restricted`;
  this example needs `baseline` (`deploy/namespace.yaml`) because Harbor runs as root.
- **`ImagePullBackOff` or slow first trials.** Docker Hub limits or large images.
  Mirror the images and pass `image_mirror`; raise `ready_timeout_s`.
- **Claims left behind after `Ctrl-C`.** `scripts/cleanup.sh harbor --claims`. Each
  claim also carries a shutdown time (`claim_ttl_s`, six hours by default).
- **`unknown agent-sandbox environment option(s)`.** A misspelled `--ek` key; the
  message lists the valid ones.

## Limits

Harbor's stream mode, Docker Compose tasks, GPU and TPU tasks and network allowlists
are not supported by the integration and are rejected before a claim is created.
Tasks that ship only a Dockerfile need an `image_map` with images built elsewhere;
every Terminal-Bench 2.0 task ships a prebuilt image.
