# envd Sandbox

This example demonstrates running [envd](https://github.com/e2b-dev/infra/tree/main/packages/envd) — E2B's in-sandbox daemon — as the container entrypoint inside an agent-sandbox. envd exposes an E2B-compatible REST and gRPC API on port 49983, providing filesystem operations, process execution, environment management, and metrics.

## Overview

**envd** is the data-plane daemon that runs inside every E2B sandbox. It provides:

- **Filesystem API** — upload, download, list, stat, watch files
- **Process API** — start, connect, stream I/O (with PTY support)
- **REST endpoints** — `/health`, `/metrics`, `/init`, `/envs`, `/freeze`, `/unfreeze`
- **Port forwarding** — auto-discovers and forwards localhost ports

This example builds envd from the upstream [e2b-dev/infra](https://github.com/e2b-dev/infra) source and runs it in a standard Linux container with the `--isnotfc` flag (skipping Firecracker MMDS polling). No KVM or kata-deploy is required.

The [kvcache-ai/AgentENV](https://github.com/kvcache-ai/AgentENV) project uses envd in a similar way, packaging it into an ext4 tools drive attached to Firecracker microVMs for their AI agent RL training platform.

## Architecture

```
  Client SDK (Python / Go / TS / curl)
        │
        │  HTTP / gRPC (port 49983)
        │  ─── kubectl port-forward ───┐
        ▼                               │
  ┌─────────────────────────────────────┐
  │  envd Pod (standard Linux container)│
  │                                     │
  │  REST API:                          │
  │    GET  /health                     │
  │    GET  /metrics                    │
  │    POST /init                       │
  │    GET  /envs                       │
  │    GET  /files   (download)         │
  │    POST /files   (upload)           │
  │                                     │
  │  gRPC (ConnectRPC):                 │
  │    filesystem.{Stat,ListDir,...}    │
  │    process.{Start,Connect,...}      │
  │                                     │
  │  Port Scanner + Forwarder           │
  └─────────────────────────────────────┘
        ▲
        │
  agent-sandbox controller
```

## Prerequisites

1. Kubernetes cluster with agent-sandbox controller installed ([installation guide](../../README.md#installation))
2. `kubectl` configured to access the cluster
3. Docker (or other OCI builder) to build the envd image

## Step 1 — Build and push the envd image

```bash
cd examples/envd-sandbox

# Build (default uses a pinned envd commit; override with --build-arg ENVD_VERSION=<ref>)
export IMAGE=<your-registry>/envd-sandbox:latest
docker build -t ${IMAGE} .

# Push (for Kind, use: kind load docker-image ${IMAGE})
docker push ${IMAGE}
```

## Step 2 — Create the Sandbox

```bash
envsubst < sandbox-envd.yaml | kubectl apply -f -
kubectl get sandbox envd-example
```

## Step 3 — Verify the pod is running

```bash
kubectl get pod -l sandbox=envd
kubectl logs -l sandbox=envd
# Should show envd startup banner and "listening on :49983"
```

## Step 4 — Run the verification scripts

First, set up port-forwarding:

```bash
POD=$(kubectl get pod -l sandbox=envd -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward pod/$POD 49983:49983 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null' EXIT

export SANDBOX_BASE_URL=http://127.0.0.1:49983

# Wait for readiness
for i in $(seq 1 30); do
  curl -sf ${SANDBOX_BASE_URL}/health >/dev/null 2>&1 && break
  sleep 0.2
done
```

Then run any of the four client scripts:

### Python

```bash
pip install -r requirements.txt
python test_client.py
```

### Shell (curl + jq)

```bash
chmod +x test_client.sh
./test_client.sh
```

### Go

```bash
go run test_client.go
```

### TypeScript

```bash
npx tsx test_client.ts
```

All four scripts test the same operations:
1. **health** — `GET /health` → 204
2. **init** — `POST /init` → 204 (initializes sandbox with `defaultUser: user` for subprocess execution)
3. **files** — upload + download round-trip via `POST/GET /files`
4. **metrics** — `GET /metrics` → JSON with system stats (`ts`, `cpu_count`, `mem_total`, etc.)

## Security Considerations

> **WARNING**: envd in `--isnotfc` mode runs **without authentication**. Do NOT expose port 49983 to a public network.

**Security model** (following [e2b-dev design](https://github.com/e2b-dev/infra/blob/main/packages/envd/debug.Dockerfile)):
- **envd runs as root**: Required for PTY allocation, process management, and cgroup operations.
- **User code runs as non-root**: The `/init` request sets `defaultUser: "user"` (uid 1000), so all user-spawned processes execute as an unprivileged user. This limits blast radius if user code is malicious.

The included `sandbox-envd.yaml` applies a **default-deny ingress NetworkPolicy** that restricts access to port 49983 to pods labeled `access: envd-client`. This prevents unauthorized workloads in the cluster from reaching the envd API.

### Production hardening checklist

- **NetworkPolicy**: The default manifest includes a deny-all ingress policy. Adjust the `podSelector` to match your cluster's access control model (e.g., allow only the sandbox-router pod).
- **Pin envd version**: The Dockerfile pins `ENVD_VERSION` to a specific upstream commit. To update, override at build time:
  ```bash
  docker build --build-arg ENVD_VERSION=<commit-or-tag> -t ${IMAGE} .
  ```
- **Token authentication**: To enable authenticated access, send an initial `POST /init` request with an `accessToken` field to bootstrap the daemon's token. Subsequent requests must include the `X-Access-Token` header. Do **not** set `E2B_ACCESS_TOKEN` as an environment variable — the token is configured via the `/init` bootstrap flow.
- **Non-root subprocesses**: The Dockerfile creates a `user` (uid 1000) and the test clients set `defaultUser: "user"` in `/init`. All user code runs as this unprivileged user by default.
- **Ephemeral storage**: envd writes to the container's rootfs; pod restart wipes all state.

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| Pod stuck in `CrashLoopBackOff` | envd cannot reach MMDS (missing `--isnotfc`) | Verify the Dockerfile CMD includes `--isnotfc` |
| `failed to create cgroup` in logs | cgroup v2 not available in container | Ensure `--no-cgroups` flag is set in CMD |
| `connection refused` on port 49983 | envd not yet ready | Wait for readiness probe; check `kubectl logs` |
| File upload returns 500 | Path resolution issue | File paths resolve relative to `/home/user`; use relative paths or absolute paths under `/home/user/` |
| Port-forward drops | Pod restarted | Re-run `kubectl port-forward` |

## Cleanup

```bash
kubectl delete -f <(envsubst < sandbox-envd.yaml)
# Or individually:
# kubectl delete sandbox envd-example
# kubectl delete networkpolicy envd-deny-ingress
```

## Related

- [envd source](https://github.com/e2b-dev/infra/tree/main/packages/envd) — upstream E2B daemon
- [kvcache-ai/AgentENV](https://github.com/kvcache-ai/AgentENV/tree/main/thirdparty/envd) — envd in Firecracker microVMs
- [E2B docs](https://e2b.dev/docs) — E2B sandbox platform documentation
- [firecracker-sandbox](../firecracker-sandbox/) — VM-isolated sandbox with a similar API contract
