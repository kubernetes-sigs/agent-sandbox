# Harbor environment for Agent Sandbox

`k8s-agent-sandbox-harbor` runs [Harbor](https://github.com/harbor-framework/harbor)
evaluation trials in Agent Sandbox `SandboxClaim`s instead of local Docker
containers. Harbor loads it by import path, so no change to Harbor is needed:

```bash
harbor run -d terminal-bench@2.0 -a terminus-2 -m <model> -k 5 -n 50 \
  -e k8s_agent_sandbox_harbor:AgentSandboxEnvironment --ek namespace=harbor
```

Each trial gets one claim against a warm pool that holds the task's image, and
every `exec` and file transfer goes through the Kubernetes pod exec API, so the
task image needs nothing installed. Terminal-Bench 2.0's 89 tasks all ship a
prebuilt image and run unchanged.

## Quick install

```bash
pip install "git+https://github.com/kubernetes-sigs/agent-sandbox.git@<tag>#subdirectory=clients/integrations/harbor"
```

Requirements: Python >= 3.12 (Harbor's floor), a kubeconfig for a cluster with
Agent Sandbox and its extension controllers (`SandboxTemplate`, `SandboxWarmPool`,
`SandboxClaim`), and RBAC for the user running `harbor`: create, get, list and
delete on those three kinds, get and list on pods, and create on `pods/exec`.

## How a trial runs

1. `start()` resolves the task image (`[environment].docker_image`, optionally
   through a registry mirror), creates a `SandboxTemplate` and a `SandboxWarmPool`
   for that image and its resources (once per image per process; a 409 means
   another trial got there first), then creates a `SandboxClaim` with a shutdown
   time and waits for it to be Ready and to accept exec.
2. `exec()` runs `bash -c <command>` through the pod exec API with Harbor's cwd,
   env, `user` (`su`) and `timeout_sec` (coreutils `timeout`, exit 124), the
   same contract as Harbor's own Kubernetes provider.
3. `upload_dir()` and `download_dir()` stream tar archives through exec;
   `upload_file()` and `download_file()` use the same path with one member.
4. `stop(delete=True)` deletes the claim; `--no-delete` keeps it and logs the
   `kubectl exec` command to inspect the pod.

The pod runs as root (Harbor installs agents and uses `su`), with
`automountServiceAccountToken: false`, the task's CPU, memory and storage as
requests, limits derived from them, and a NetworkPolicy that allows cluster DNS
and the public internet only. The controller's default policy blocks DNS, and
Harbor verifiers install packages with `apt` and `uv`, so the template carries its
own egress rule (`--ek network=managed-default` keeps the controller default).

## Options (`--ek key=value`)

| Key | Default | Meaning |
| --- | --- | --- |
| `namespace` | `default` | Namespace for templates, pools and claims. |
| `kube_context` | current | kubeconfig context; ignored in-cluster. |
| `ready_timeout_s` | `300` | Wait for the claim to be Ready and the container to accept exec. |
| `runtime_class` | none | `runtimeClassName` for the sandbox pod, for example `gvisor`. |
| `node_selector` | none | `k=v,k2=v2`. |
| `tolerations` | none | JSON list of tolerations. |
| `image_pull_secret` | none | Pull secret name in `namespace`. |
| `image_mirror` | none | Registry prefix that replaces the image's registry (Docker Hub shorthand expands to `library/`). |
| `image_map` | none | JSON file `{environment_id or task name: image}` for tasks without `docker_image`. |
| `warm_replicas` | `0` | Pods kept ready per task image; `0` cold-starts each claim. |
| `cpu_limit_multiplier` | `1.0` | CPU limit as a multiple of the request; `0` sets none. |
| `memory_limit_multiplier` | `1.0` | Memory limit as a multiple of the request; `0` sets none. |
| `network` | `dns-public` | `dns-public` or `managed-default`. |
| `claim_ttl_s` | `21600` | Shutdown time of each claim, a safety net when `harbor` dies; `0` sets none. |
| `max_output_bytes` | `0` | Truncate exec output beyond this size; `0` keeps everything. |
| `exec_threads` | `64` | Threads for the blocking exec websockets per process. |
| `template_prefix` | `hb` | Name prefix of templates, pools and claims. |
| `extra_labels` | none | `k=v,...` labels on every object created. |
| `run_as_root` | `true` | Run the pod as uid 0. |

Every option also reads a `K8S_SANDBOX_HARBOR_<KEY>` environment variable; an
`--ek` value wins. A misspelled key is an error that lists the valid ones.

Harbor's own flags apply as usual: `-n` concurrent trials, `-k` attempts,
`--no-delete`, `--override-memory-mb`, and the resource enforcement policies.

## Cleanup

Trials delete their claims. After a killed run:

```bash
python -m k8s_agent_sandbox_harbor cleanup --namespace harbor --claims   # claims only, pools stay
python -m k8s_agent_sandbox_harbor cleanup --namespace harbor --all      # claims, pools, templates
```

Both select only objects labelled `harbor.io/managed-by=k8s-agent-sandbox-harbor`.

## Limits

- Harbor's stream mode, Docker Compose tasks, GPU and TPU tasks and network
  allowlists are not supported; `_validate_definition` rejects them before a
  claim is created.
- Images are not built. A task that ships only a Dockerfile needs an
  `image_map` entry built elsewhere.
- Harbor's pod labels are not forwarded to the pod: the controller admits pod
  labels from an allowlisted domain only. Claims, templates and pools carry the
  `harbor.io/*` labels.
- Pod Security Admission `restricted` rejects the root pod; use `baseline`, or a
  gVisor or Kata runtime class for isolation.

## Development

```bash
cd clients/integrations/harbor
python3.12 -m venv .venv && . .venv/bin/activate
pip install -e ../../python/agentic-sandbox-client[async] -e '.[test]'
pytest
```

The unit tests run the environment against Harbor's real `BaseEnvironment`
with fake Kubernetes clients and a local stand-in for the pod, so a Harbor
signature change fails here first. `make test-unit` at the repo root runs the
same suite (`integrations-harbor`).
