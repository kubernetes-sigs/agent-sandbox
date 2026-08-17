# Pi Code Agent in Kubernetes Sandbox Example

This example demonstrates how to run [Pi](https://github.com/earendil-works/pi) — a terminal-based coding agent from Earendil Works — inside a Kubernetes cluster using the `Sandbox` CRD from [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox).

It mirrors Pi's [Plain Docker](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/containerization.md#plain-docker) containerization pattern, adapted to the Sandbox CRD.

## Prerequisites

1. A Kubernetes cluster (Kind, Minikube, GKE, etc.).
2. `agent-sandbox` controller and CRDs installed in the cluster.
3. `kubectl` configured to access your cluster.
4. Docker (for building the Pi image locally).
5. (If using Kind) [Kind](https://kind.sigs.k8s.io/) installed.
6. An Anthropic API key.

## Files

- `Dockerfile`: Builds `pi-sandbox:local` from `node:24-bookworm-slim`, installing `@earendil-works/pi-coding-agent@0.84.1`.
- `sandbox.yaml`: The `Sandbox` manifest. Two PVCs (`/workspace` for code, `/root/.pi/agent` for Pi's own settings/sessions), `ANTHROPIC_API_KEY` injected from a Secret, `stdin`/`tty` enabled so you can attach to the Pi TUI.
- `run-test-kind.sh`: Build, load into Kind, apply, and wait — one command end-to-end.

## How to Use

### 1. Create the API key Secret

```bash
kubectl create secret generic pi-code-agent-api-keys \
  --from-literal=ANTHROPIC_API_KEY="your_anthropic_key"
```

### 2. Deploy the Sandbox

#### Option A: Quick path (Kind)

```bash
./run-test-kind.sh
```

This builds the image, loads it into the Kind cluster (default name `agent-sandbox`, override with `KIND_CLUSTER_NAME=...`), applies `sandbox.yaml`, and waits for the pod to become Ready.

#### Option B: Manual path

```bash
# Build the image
docker build -t pi-sandbox:local .

# Apply the Sandbox manifest
kubectl apply -f sandbox.yaml
```

**Kind users** — load the image into the cluster before applying the manifest (or use `./run-test-kind.sh`, which does this for you):

```bash
kind load docker-image pi-sandbox:local --name "${KIND_CLUSTER_NAME:-agent-sandbox}"
```

**Non-Kind clusters (Minikube, GKE, EKS, ...)** — `sandbox.yaml` references `pi-sandbox:local`, which is a node-local image. For clusters that cannot see your local Docker daemon, push the image to a registry and update the `image:` field in `sandbox.yaml` before applying:

```bash
# Example: push to a registry you control
docker tag pi-sandbox:local REGISTRY/pi-sandbox:0.84.1
docker push REGISTRY/pi-sandbox:0.84.1

# Edit sandbox.yaml: replace `image: pi-sandbox:local` with `image: REGISTRY/pi-sandbox:0.84.1`
kubectl apply -f sandbox.yaml
```

### 3. Verify the Deployment

```bash
kubectl get sandboxes
kubectl get pods -l sandbox=pi-code-agent
```

You should see a pod named `pi-code-agent` (matching `Sandbox.metadata.name`).

### 4. Interact with Pi

Pi is a terminal TUI; attach to the running container:

```bash
POD_NAME="$(kubectl get pods -l sandbox=pi-code-agent -o jsonpath='{.items[0].metadata.name}')"
kubectl attach -it "${POD_NAME}"
```

Detach without killing Pi: press `Ctrl+P`, then `Ctrl+Q`.

## Cleanup

```bash
kubectl delete -f sandbox.yaml
kubectl delete secret pi-code-agent-api-keys
```

The PVCs are retained by default. To delete them as well:

```bash
kubectl delete pvc -l sandbox=pi-code-agent
```

## Configuration

- **Different API provider:** Pi supports multiple providers. Add additional env vars (e.g., `OPENAI_API_KEY`) to `sandbox.yaml` and create corresponding entries in the Secret.
- **Different PVC sizes:** edit `volumeClaimTemplates` in `sandbox.yaml`.
- **Tighter security:** add a `securityContext` to the container spec (see `examples/nullclaw-sandbox/nullclaw-sandbox.yaml` for an example).
- **Newer Pi version:** bump the version in `Dockerfile` (`@earendil-works/pi-coding-agent@<new-version>`) and rebuild.
