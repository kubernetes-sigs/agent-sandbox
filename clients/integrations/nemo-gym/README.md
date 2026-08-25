# Agent Sandbox provider for NVIDIA NeMo Gym

This package makes [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) a
sandbox backend for [NVIDIA NeMo Gym](https://github.com/NVIDIA-NeMo/Gym) RL training
environments, alongside NeMo Gym's built-in providers (Docker, Daytona, ECS Fargate,
Enroot, OpenShell, OpenSandbox, Apptainer).

NeMo Gym environments get their isolated execution sandboxes as **SandboxClaims against
SandboxWarmPools**, so per-rollout sandbox acquisition is claim-bind latency (typically
sub-second on a warm pool hit) instead of pod scheduling + image pull. Claim TTLs
(`ttl_s`) map to the claim lifecycle's `shutdownTime`, so the controller garbage-collects
expired sandboxes even if the training process dies.

## How it plugs in

NeMo Gym discovers providers through the `nemo_gym.sandbox_providers` entry-point group
(see NeMo Gym's `nemo_gym/sandbox/providers/registry.py`). This package publishes
`agent_sandbox` in that group, so:

```bash
pip install nemo-gym-k8s-agent-sandbox   # alongside nemo-gym
```

is all it takes for `agent_sandbox` to become a valid provider name — no registration
code, no NeMo Gym fork.

> **Python version**: this package declares `requires-python >=3.13.14`, matching the
> floor that nemo-gym itself publishes on PyPI. pip/uv enforce that floor at resolve
> time, so installs on earlier interpreters (including 3.13.x patch releases below
> 3.13.14) are rejected with a nemo-gym resolution error. If nemo-gym relaxes its
> floor, this package's floor (and the `min_python` gate in `dev/tools/test-unit`)
> should be relaxed to match.

## Cluster prerequisites

- The agent-sandbox controller **and extensions** (SandboxClaim / SandboxTemplate /
  SandboxWarmPool CRDs and controllers) installed.
- At least one `SandboxWarmPool` whose `SandboxTemplate` runs an image serving the
  sandbox runtime REST API (`/execute`, `/upload`, `/download`, ...) on
  `connection.server_port` (default 8888). See
  [`examples/python-runtime-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/python-runtime-sandbox) for the
  reference runtime image.
- For clients running **outside** the cluster: the sandbox router plus either a Gateway
  (`connection.mode: gateway`) or a directly reachable router URL
  (`connection.mode: direct`). Inside the cluster, `in_cluster` mode talks straight to
  sandbox pod IPs and needs neither.

## Usage

Add the provider config to a NeMo Gym run (the shipped config binds the standard
instance name `sandbox`, so it drops in wherever an openshell/opensandbox config would):

```bash
AGENT=responses_api_agents/mini_swe_agent_2/configs/mini_swe_agent_2.yaml
MODEL=responses_api_models/vllm_model/configs/vllm_model.yaml
ng_run "+config_paths=[$AGENT, /path/to/configs/agent_sandbox.yaml, $MODEL]"
```

See [`configs/agent_sandbox.yaml`](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/integrations/nemo-gym/configs/agent_sandbox.yaml) for the annotated config.
Kubernetes API access uses standard kubeconfig / in-cluster service account resolution.

### Image -> warm pool routing

Warm pools pre-bake their image in the pool's SandboxTemplate, so `sandbox_spec.image`
cannot be pulled per-create. Instead, images route to pools:

```yaml
create:
  warmpool: nemo-gym-pool          # default pool
  image_warmpools:                 # exact image string -> pool name
    my-registry/swebench-runtime:latest: swebench-pool
```

Resolution order per sandbox: `provider_options.warmpool` >
`image_warmpools[spec.image]` > `create.warmpool`. An image that only resolves via the
default pool logs a warning (the pool's template decides the real image).

### Per-sandbox `provider_options`

| Key | Meaning |
| --- | --- |
| `warmpool` | Claim from this pool (overrides all routing) |
| `namespace` | Claim namespace (overrides `create.namespace`) |
| `pod_labels` / `pod_annotations` | Stamped on the sandbox Pod via `additionalPodMetadata` |
| `volume_claim_templates` | Volume claim templates merged into the sandbox |

## Semantics and limitations

| NeMo Gym spec field | Mapping |
| --- | --- |
| `image` | Routed to a warm pool via `image_warmpools` (not pulled per-create) |
| `ttl_s` | Claim lifecycle `shutdownTime` + `Delete` policy (enforced server-side) |
| `env` | Exported inside every wrapped exec script |
| `workdir` | Default `cd` for every exec (falls back to the runtime's own cwd) |
| `files` | Uploaded by NeMo Gym's sandbox API via `upload_file` after `create` returns |
| `metadata` | SandboxClaim labels (must satisfy Kubernetes label syntax) |
| `resources` | **Ignored with a warning** — resources come from the SandboxTemplate |
| `entrypoint` | **Unsupported** (create error) — the template owns the pod command |
| `exec(user=...)` | **Ignored with a warning** — the runtime API runs as the pod user |

Implementation notes:

- The runtime's `/execute` tokenizes with `shlex.split` and runs **without a shell**, so
  every command is sent as `<exec_shell> -c '<script>'` with `cwd`/`env` folded in.
- The file API only accepts `..`-free **relative** paths rooted at the runtime server's
  working directory. Absolute `upload_file`/`download_file` paths are honored by staging
  through a relative temp name and copying via exec — this assumes the file-API root and
  the exec working directory are the same directory, which holds for the reference
  runtime image (both come from its `SANDBOX_BASE_DIR`, default `/app`).
- An exec that exceeds its timeout abandons the HTTP request, but the process may keep
  running inside the pod (the runtime API has no server-side kill); the result carries
  `error_type="timeout"` and return code 125.

## Development

```bash
pip install -e '../../python/agentic-sandbox-client[async]'
pip install -e '.[test]'
pytest tests/unit
```

Tests are pure unit tests against fakes — no cluster, no NeMo Gym run required (only the
`nemo-gym` package for its provider base types).
