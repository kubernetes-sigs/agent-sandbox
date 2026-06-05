# MCP Sandbox

A self-contained example of running a Model Context Protocol (MCP) server
inside an Agent Sandbox, using nothing but `kubectl`. The host acts as the
MCP client, the sandbox hosts the MCP server, and `kubectl exec` is the
stdio transport between them.

## How it works

`kubectl exec -i POD -- python3 -u /app/mcp_server.py` is a long-lived
bidirectional stdio channel — exactly what MCP's stdio transport wants.
The host-side MCP client uses `kubectl exec` as its "subprocess," and
kubectl pipes JSON-RPC frames into and out of the pod:

```text
┌────────── host ──────────┐                  ┌──────── sandbox pod ────────┐
│                          │                  │                             │
│  client.py               │                  │   mcp_server.py             │
│    └─ MCP ClientSession  │ ◄──── stdio ───► │     └─ MCP FastMCP server   │
│                          │  via kubectl exec│                             │
│                          │                  │      /workspace  ──► PVC    │
│                          │                  │                             │
└──────────────────────────┘                  └─────────────────────────────┘
```

No agent-sandbox Python SDK, no warm pool, no sandbox-router, no in-sandbox
driver. The MCP server runs inside the sandbox, owns the PVC mount, and is
the only thing in the pod that touches files. The host's only job is to be
the MCP client.

The example exercises the full create → execute → I/O → return → terminate
loop, and also proves the PVC is doing real work — not just sitting in the
manifest unused:

1. `envsubst < sandbox.yaml | kubectl apply -f -` creates a `Sandbox` with a 1Gi PVC mounted at `/workspace`.
2. **Session 1**: `client.py` opens an MCP session and calls `list_blobs` (expects empty) → `write_random_blob` (writes random bytes to the PVC, returns sha256).
3. **Suspend → Resume**: `client.py` patches `sandbox/mcp-sandbox` with `spec.operatingMode: Suspended` — the controller deletes the pod but keeps the Sandbox object and the PVC. Then it patches back to `Running` and the controller creates a fresh pod with the same PVC reattached.
4. **Session 2**: `client.py` opens a *second* MCP session against the new pod, calls `list_blobs` (expects `['random.bin']`) and `read_blob` (expects the same sha256). If the sha256 doesn't match, the data didn't persist — i.e. the PVC isn't working. On `emptyDir` or the container overlay this step would fail.
5. `client.py` then `kubectl cp`s the file out and re-hashes locally, confirming the bytes round-trip back to the host.
6. `kubectl delete -f sandbox.yaml` tears everything down.

## Files

| File | Role |
|---|---|
| [`sandbox.yaml`](sandbox.yaml) | Bare `Sandbox` CRD. One container running `sleep infinity`, one 1Gi PVC mounted at `/workspace`. Uses `${IMAGE}` so you can `envsubst` your registry path. |
| [`Dockerfile`](Dockerfile) | `python:3.11-slim` + `pip install mcp` + copy `mcp_server.py`. Nothing else. |
| [`mcp_server.py`](mcp_server.py) | Custom MCP server (FastMCP). Exposes `list_blobs`, `write_random_blob(name, size_bytes)`, `read_blob(name)`. All operate on `/workspace` (override with `MCP_WORKSPACE`). |
| [`client.py`](client.py) | Host-side MCP client. Uses `kubectl exec -i` as the stdio transport, runs the Suspend→Resume cycle, then `kubectl cp`s the result file out and verifies its sha256. |
| [`requirements.txt`](requirements.txt) | `mcp` (Python MCP SDK) for the host. |

## Prerequisites

1. A Kubernetes cluster with the [Agent Sandbox controller](../../README.md#installation) installed. **Only the core CRDs** are needed — no extensions bundle, no sandbox-router.
2. `kubectl` configured against the cluster, plus `docker`, `envsubst`, and Python 3.10+ on the host.

## Run it (local cluster, e.g. Kind)

```bash
# 1. Build the image and load it into Kind.
export IMAGE=mcp-sandbox:latest
docker build -t "${IMAGE}" .
kind load docker-image "${IMAGE}" --name agent-sandbox

# 2. Create the sandbox.
envsubst < sandbox.yaml | kubectl apply -f -
kubectl wait --for=condition=Ready sandbox/mcp-sandbox --timeout=120s

# 3. Run the host-side MCP client.
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
python3 client.py

# 4. Tear down.
kubectl delete -f sandbox.yaml
rm returned-random.bin
```

Expected `client.py` output, in order:

```text
============================================================
Session 1 — write a random blob to the PVC
============================================================
[host] tools advertised by server: ['list_blobs', 'write_random_blob', 'read_blob']
[host] list_blobs (before write) -> []
[host] write_random_blob('random.bin', 256) -> {'path': '/workspace/random.bin', 'bytes_written': 256, 'sha256': '...'}

============================================================
Suspend → Resume — the PVC persists, the container fs does not
============================================================
[host] patching sandbox/mcp-sandbox operatingMode=Suspended (controller will delete the pod)...
[host] pod mcp-sandbox is gone (Sandbox is Suspended)
[host] patching sandbox/mcp-sandbox operatingMode=Running (controller will recreate the pod)...
[host] waiting up to 180s for pod mcp-sandbox to be Ready again...
[host] pod mcp-sandbox is Ready again

============================================================
Session 2 — read the blob back from the PVC
============================================================
[host] list_blobs (after restart) -> ['random.bin']
[host] read_blob('random.bin') -> {'path': '/workspace/random.bin', 'size_bytes': 256, 'sha256': '...'}
[host] OK — sha256 matches across pod restart: ...

============================================================
Return the file to the host and re-hash locally
============================================================
[host] kubectl cp mcp-sandbox:/workspace/random.bin -> returned-random.bin
[host] returned 256 bytes; host sha256=...
[host] OK — PVC contents survived pod restart and round-trip back to the host
```

## Run it against a remote cluster (e.g. GKE)

### GKE

```bash
# 1. One-time: create the cluster and configure auth.
gcloud container clusters create-auto mcp-demo --region=us-central1
gcloud container clusters get-credentials mcp-demo --region=us-central1
gcloud auth configure-docker us-central1-docker.pkg.dev

# 2. Build and push the image to Artifact Registry.
export IMAGE=us-central1-docker.pkg.dev/${PROJECT}/${REPO}/mcp-sandbox:latest
docker build -t "${IMAGE}" .
docker push "${IMAGE}"

# 3. From here the flow is identical to local.
envsubst < sandbox.yaml | kubectl apply -f -
kubectl wait --for=condition=Ready sandbox/mcp-sandbox --timeout=120s
python3 client.py
kubectl delete -f sandbox.yaml
```

### EKS / AKS / other

Same flow — only the registry push and credential commands differ:

- **EKS:** `aws eks update-kubeconfig --name <cluster>` + push to ECR (`aws ecr get-login-password ...`).
- **AKS:** `az aks get-credentials -g <rg> -n <cluster>` + push to ACR (`az acr login -n <registry>`).
- **Any cluster:** push to Docker Hub (`docker push docker.io/<user>/mcp-sandbox`).

Once `${IMAGE}` resolves to something the cluster's nodes can pull, the
`envsubst` / `kubectl apply` / `python3 client.py` / `kubectl delete`
sequence is unchanged.

### Latency note

Every MCP request from the host hops through `kubectl exec` → kube-apiserver
→ kubelet → pod, so round-trip latency is noticeably higher than a local
stdio session. Fine for demos, interactive tooling, and CI; if you need
production-grade throughput for many concurrent MCP sessions, graduate to
the [agentic-sandbox-client](../../clients/python/agentic-sandbox-client/)
SDK, which uses a router/gateway path instead of per-call apiserver hops.

## References

- [Model Context Protocol](https://modelcontextprotocol.io/) — the protocol spec.
- [Python MCP SDK](https://github.com/modelcontextprotocol/python-sdk) — what `mcp_server.py` and `client.py` are built on.
- [Official MCP servers](https://github.com/modelcontextprotocol/servers) — drop-in replacements (filesystem, git, time, etc.). To use one, swap the `command`/`args` in `client.py`'s `StdioServerParameters` and install the server in the `Dockerfile`.
- [agentic-sandbox-client](../../clients/python/agentic-sandbox-client/) — when you outgrow `kubectl exec` (multi-tenant agent platforms, warm pools, gateway-routed traffic), this is the SDK to graduate to.
