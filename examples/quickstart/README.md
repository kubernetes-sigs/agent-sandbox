# Secure Agent Sandbox Quickstart

## Overview

This guide walks you through setting up Agent Sandbox from scratch. By default it uses a basic KIND cluster without container isolation. If you want stronger isolation, you can optionally configure **gVisor** or **Kata Containers** as a runtime — the guide will tell you when to branch off.

### What You'll Set Up

- **Sandbox**: Core isolated environment for running untrusted code
- **SandboxTemplate**: Reusable blueprint for sandbox configurations
- **SandboxClaim**: Declarative API for requesting sandboxes
- **SandboxWarmPool**: Pre-warmed sandbox pool for fast allocation
- **Python SDK**: Programmatic sandbox management
- **Router Service**: HTTP proxy for SDK communication

## Prerequisites

- Docker (20.10+)
- kubectl (1.28+)
- [KIND](https://kind.sigs.k8s.io/) (0.20+) — default; or [minikube](https://minikube.sigs.k8s.io/) if using Kata Containers or gVisor on minikube
- Python 3.9+
- Git

---

## Step 1: Clone the Agent Sandbox Repository

```bash
git clone https://github.com/kubernetes-sigs/agent-sandbox.git
cd agent-sandbox/
```

### 1.1 Choose Sandbox and Router Images

Set the image references you will use throughout this quickstart:

```bash
export ROUTER_IMAGE=sandbox-router:local
export SANDBOX_NAMESPACE=agent-sandbox-demo
export SANDBOX_TEMPLATE_NAME=python-runtime-template
```

## Step 2: Create Kubernetes Cluster

By default, create a basic KIND cluster:

```bash
kind create cluster --name agent-sandbox-demo
```

**Want container isolation?** Instead of the basic cluster above, follow one of these guides to create a cluster with a secure runtime, then return here at **Step 3**:

- **[gVisor on KIND ->](gvisor.md)** — userspace kernel isolation
- **[Kata Containers on minikube ->](kata-containers.md)** — VM-based isolation

## Step 3: Install Agent Sandbox Controller

Install the core components and extensions by following the [Installation section](../../README.md#installation) in the project root README. Make sure to install **both** the core manifest and the extensions manifest.

Verify the controller is running before continuing:

```bash
kubectl wait --for=condition=Ready pod -l app=agent-sandbox-controller -n agent-sandbox-system --timeout=120s
```

### 3.1 Create Dedicated Namespace

```bash
# Create a namespace for all Agent Sandbox resources
kubectl create namespace agent-sandbox-demo

# Set as default context to avoid repeating -n flag
kubectl config set-context --current --namespace=agent-sandbox-demo
```

## Step 4: Apply SandboxTemplate

The repository includes a ready-made SandboxTemplate at [`clients/python/agentic-sandbox-client/python-sandbox-template.yaml`](../../clients/python/agentic-sandbox-client/python-sandbox-template.yaml). Apply it to the `agent-sandbox-demo` namespace.

If you are using gVisor or Kata Containers isolation, open the file and uncomment the appropriate `runtimeClassName` line before applying.

```bash
envsubst '${SANDBOX_NAMESPACE} ${SANDBOX_TEMPLATE_NAME}' \
    < clients/python/agentic-sandbox-client/python-sandbox-template.yaml \
    | kubectl apply -f -
```

See [`extensions/examples/`](../../extensions/examples/) for additional SandboxTemplate, SandboxClaim, and SandboxWarmPool examples.

## Step 5: Create SandboxWarmPool

Create a WarmPool that references the template from Step 4 (see [`extensions/examples/sandboxwarmpool.yaml`](../../extensions/examples/sandboxwarmpool.yaml) for the base example):

```bash
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: python-warmpool
  namespace: agent-sandbox-demo
spec:
  replicas: 2
  sandboxTemplateRef:
    name: python-runtime-template
EOF
```

### 5.1 Create a SandboxClaim

With the WarmPool running, create a SandboxClaim and verify it is fulfilled immediately from the pool:

```bash
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: quickstart-test
  namespace: agent-sandbox-demo
spec:
  sandboxTemplateRef:
    name: python-runtime-template
EOF

kubectl wait --for=condition=Ready sandbox/quickstart-test --timeout=60s

POD_NAME=$(kubectl get sandbox quickstart-test -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
echo "Sandbox pod: $POD_NAME"

kubectl delete sandboxclaim quickstart-test
```

### Understanding How WarmPool Works

The WarmPool creates pre-warmed **PODS** (not Sandbox resources) that are ready to be claimed:

1. **WarmPool creates pods directly** with label `agents.x-k8s.io/pool=<hash>`
2. When you create a **SandboxClaim**, the controller claims a pod from the pool
3. The claimed pod gets:
   - Annotation: `agents.x-k8s.io/pod-name: <pod-name>`
   - Label changes from `pool=<hash>` to `sandbox-name-hash=<hash>`
4. **WarmPool automatically creates a replacement pod** to maintain replica count

Performance comparison:
- **With WarmPool**: Sub-2 second allocation (pod already running)
- **Without WarmPool**: 10-30 seconds (image pull + container/VM startup)

## Step 6: Build and Deploy Router

The Sandbox Router proxies HTTP requests from the SDK to sandbox pods. See the [Sandbox Router README](../../clients/python/agentic-sandbox-client/sandbox-router/README.md) for full details.

Build the router image locally:

```bash
cd clients/python/agentic-sandbox-client/sandbox-router
docker build -t ${ROUTER_IMAGE} .
cd ../../../../
```

The repository includes a ready-made router manifest at [`sandbox-router/sandbox_router.yaml`](../../clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml). We'll deploy it in the next step after loading the image into the cluster.

## Step 7: Load Router Image and Deploy Router

### 7.1 Load the Router Image into the Cluster

**KIND (default / gVisor):**

```bash
kind load docker-image ${ROUTER_IMAGE} --name agent-sandbox-demo
```

**minikube (Kata Containers):**

```bash
minikube image load ${ROUTER_IMAGE} -p agent-sandbox-kata
```

### 7.2 Deploy the Router

Open `clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml` and uncomment the `imagePullPolicy: Never` line, then apply:

```bash
envsubst '${ROUTER_IMAGE}' \
    < clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml \
    | kubectl apply -n agent-sandbox-demo -f -
```

### 7.3 Verify WarmPool

```bash
# Check WarmPool status
kubectl get sandboxwarmpool python-warmpool

# Check pre-warmed PODS (not Sandboxes - WarmPool creates pods directly)
kubectl get pods -l agents.x-k8s.io/pool

# Wait for pods to be ready
kubectl wait --for=condition=Ready pod -l agents.x-k8s.io/pool --timeout=60s
```

Expected output:

```text
NAME                      READY   STATUS    RESTARTS   AGE
python-warmpool-abcde     1/1     Running   0          15s
python-warmpool-fghij     1/1     Running   0          15s
```

### 7.4 Verify Router Deployment

```bash
# Check router pods
kubectl get pods -l app=sandbox-router

# Check router service
kubectl get svc sandbox-router-svc

# Test router health
kubectl port-forward svc/sandbox-router-svc 8080:8080 &
PF_PID=$!
sleep 2
curl http://localhost:8080/healthz
# Should return: {"status":"ok"}
kill $PF_PID
```

## Step 8: Install Python SDK

Follow the [installation instructions](../../clients/python/agentic-sandbox-client/README.md#installation) in the Python SDK README. For this quickstart, the editable install from source is recommended since you already cloned the repository:

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -e clients/python/agentic-sandbox-client
```

**Note:** Keep the virtual environment activated for all subsequent SDK commands.

## Step 9: Test the Setup

### 9.1 Run the SDK Test Client

The repository includes a test script that validates the full sandbox lifecycle (creation, command execution, file I/O, cleanup). See [`clients/python/agentic-sandbox-client/test_client.py`](../../clients/python/agentic-sandbox-client/test_client.py) and the [SDK Testing docs](../../clients/python/agentic-sandbox-client/README.md#testing) for details.

Run it against the quickstart namespace and template:

```bash
python clients/python/agentic-sandbox-client/test_client.py \
    --template-name python-runtime-template \
    --namespace agent-sandbox-demo
```

### 9.2 Verify WarmPool Performance

To confirm that the WarmPool is pre-warming pods, check whether the pod existed before the claim:

```bash
# Pick a claim from the warmpool
CLAIM_NAME=$(kubectl get sandboxclaim -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
POD_NAME=$(kubectl get sandbox ${CLAIM_NAME} -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}' 2>/dev/null)

if [ -n "$POD_NAME" ] && [ -n "$CLAIM_NAME" ]; then
    CLAIM_TIME=$(kubectl get sandboxclaim ${CLAIM_NAME} -o jsonpath='{.metadata.creationTimestamp}')
    POD_TIME=$(kubectl get pod ${POD_NAME} -o jsonpath='{.metadata.creationTimestamp}')
    echo "SandboxClaim created: ${CLAIM_TIME}"
    echo "Pod created:          ${POD_TIME}"
    if [[ "${POD_TIME}" < "${CLAIM_TIME}" ]]; then
        echo "Pod was PRE-WARMED from WarmPool!"
    else
        echo "Pod created on-demand (not from warmpool)"
    fi
else
    echo "No sandbox claims found. Run the test client first (Step 9.1)."
fi
```

## Step 10: Cleanup

### Quick Cleanup — Delete All Resources

```bash
# Delete the entire namespace (removes all resources at once)
kubectl delete namespace agent-sandbox-demo

# Restore default namespace context
kubectl config set-context --current --namespace=default
```

### Delete the Cluster

**KIND (default / gVisor):**

```bash
kind delete cluster --name agent-sandbox-demo
```

**minikube (Kata Containers):**

```bash
minikube delete -p agent-sandbox-kata
```

## Summary

### What You Built

- **Sandbox**: Isolated execution environments for running untrusted code
- **SandboxTemplate**: Reusable sandbox blueprints with runtime configuration
- **SandboxClaim**: Declarative sandbox provisioning API
- **SandboxWarmPool**: Pre-warmed pool for 10-15x faster allocation
- **Python SDK**: Context manager pattern for sandbox lifecycle management
- **Router Service**: HTTP proxy enabling SDK communication

## References

- [Agent Sandbox GitHub](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox Documentation](https://agent-sandbox.sigs.k8s.io/)
- [Python SDK Source](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/)
- [gVisor Documentation](https://gvisor.dev/)
- [Kata Containers Documentation](https://katacontainers.io/)
- [KIND Documentation](https://kind.sigs.k8s.io/)
- [minikube Documentation](https://minikube.sigs.k8s.io/)
