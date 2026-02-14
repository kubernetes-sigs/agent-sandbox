# Option 1: gVisor on KIND

[← Back to Common Setup Steps](README.md)

## Prerequisites

- Docker (20.10+)
- kubectl (1.28+)
- KIND (0.20+)
- Python 3.9+
- Git
- Linux host (gVisor requires Linux)
- wget

## Step 1: Create KIND Cluster with gVisor Support

### 1.1 Install gVisor Runtime

First, install the gVisor runsc runtime on your host machine (root privileges are required):

```bash
# Download and install runsc
(
  set -e
  ARCH=$(uname -m)
  URL=https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}
  wget ${URL}/runsc ${URL}/runsc.sha512 \
    ${URL}/containerd-shim-runsc-v1 ${URL}/containerd-shim-runsc-v1.sha512
  sha512sum -c runsc.sha512 \
    -c containerd-shim-runsc-v1.sha512
  rm -f runsc.sha512 containerd-shim-runsc-v1.sha512
  chmod a+rx runsc containerd-shim-runsc-v1
  sudo mv runsc containerd-shim-runsc-v1 /usr/local/bin
)
```

### 1.2 Create KIND Cluster Configuration

Create a KIND cluster configuration file that enables gVisor:

```bash
cat <<EOF > kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /usr/local/bin/runsc
    containerPath: /usr/local/bin/runsc
  - hostPath: /usr/local/bin/containerd-shim-runsc-v1
    containerPath: /usr/local/bin/containerd-shim-runsc-v1
EOF
```

### 1.3 Create the Cluster

```bash
kind create cluster --name agent-sandbox-demo --config kind-config.yaml
```

### 1.4 Configure Containerd for gVisor

Configure the KIND cluster's containerd to support the gVisor runtime:

```bash
# WARNING: This overwrites the entire containerd config.
# If you have existing KIND configurations (e.g., registry mirrors),
# consider backing up /etc/containerd/config.toml first.
docker exec -it agent-sandbox-demo-control-plane bash -c 'cat <<EOF > /etc/containerd/config.toml
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF'

# Restart containerd
docker exec -it agent-sandbox-demo-control-plane systemctl restart containerd

# Wait for cluster to stabilize
kubectl wait --for=condition=Ready nodes --all --timeout=60s
```

### 1.5 Create RuntimeClass for gVisor

```bash
kubectl apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
```

## Step 2: Install Agent Sandbox Controller

### 2.1 Install Core Components

```bash
# Fetch latest version (or use specific version like "v0.1.0")
export AGENT_SANDBOX_VERSION=$(curl -s https://api.github.com/repos/kubernetes-sigs/agent-sandbox/releases/latest | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
echo "Using Agent Sandbox version: ${AGENT_SANDBOX_VERSION}"

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/manifest.yaml
```

### 2.2 Install Extensions (SandboxTemplate, SandboxClaim, SandboxWarmPool)

```bash
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/extensions.yaml
```

### 2.3 Verify Installation

```bash
# Check that the controller is running
kubectl get pods -n agent-sandbox-system

# Wait for the controller to be ready
kubectl wait --for=condition=Ready pod -l app=agent-sandbox-controller -n agent-sandbox-system --timeout=120s
```

Expected output:
```
NAME                          READY   STATUS    RESTARTS   AGE
agent-sandbox-controller-0    1/1     Running   0          30s
```

### 2.4 Create Dedicated Namespace

```bash
# Create a namespace for all Agent Sandbox resources
kubectl create namespace agent-sandbox-demo

# Set as default context to avoid repeating -n flag
kubectl config set-context --current --namespace=agent-sandbox-demo
```

## Step 3: Load Python Runtime Sandbox Image

Load the Python runtime sandbox image we built in the common setup steps into the KIND cluster:

```bash
kind load docker-image python-runtime-sandbox:latest --name agent-sandbox-demo
```

## Step 4: Apply SandboxTemplate

Apply the SandboxTemplate manifest we created in the common setup steps:

```bash
kubectl apply -f SandboxTemplate.yaml
```

### Verify SandboxTemplate

```bash
kubectl get sandboxtemplate
```

Expected output:
```
NAME                       AGE
python-runtime-template    5s
```

## Step 5: Apply and Verify SandboxWarmPool

Apply the SandboxWarmPool manifest we created in the common setup steps:

```bash
kubectl apply -f SandboxWarmPool.yaml
```

### Verify WarmPool Creation

```bash
# Check WarmPool status
kubectl get sandboxwarmpool python-warmpool

# Check pre-warmed PODS (not Sandboxes - WarmPool creates pods directly)
kubectl get pods -l agents.x-k8s.io/pool

# Wait for pods to be ready
kubectl wait --for=condition=Ready pod -l agents.x-k8s.io/pool --timeout=60s
```

Expected output:
```
NAME                      READY   STATUS    RESTARTS   AGE
python-warmpool-abcde     1/1     Running   0          15s
python-warmpool-fghij     1/1     Running   0          15s
```

## Step 6: Load Router Image and Deploy Router Service

Load the router image we built in the common setup steps into the KIND cluster:

```bash
kind load docker-image sandbox-router:local --name agent-sandbox-demo
```

Apply the Router Service manifest we created in the common setup steps:

```bash
kubectl apply -f RouterService.yaml
```

### Verify Router Deployment

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

## Step 7: Run SDK Tests

Run the test script we created in the common setup steps. The SDK was already installed in the common setup.

```bash
python3 test_sdk_warmpool.py
```

Expected output:
```
=== Testing SDK with WarmPool ===

Setting up port-forward: localhost:42303 -> svc/sandbox-router-svc:8080
Port-forward ready

Sandbox created: sandbox-claim-298019a0
Pod allocated: python-warmpool-t8g22
Allocation time: 0.80s

Test 1: Running Python command
  stdout: Hello from SDK!
  exit_code: 0

Test 2: File operations
  File written and read successfully
  Content: SDK test content

Test 3: WarmPool performance check
  SandboxClaim created: 2025-12-29T10:32:48Z
  Pod created:          2025-12-29T10:15:53Z
  Pod was PRE-WARMED from WarmPool!

Test 4: Runtime isolation check
  Sandbox kernel: 6.1.38 (gVisor)
  Host kernel:    6.6.95
  Running with userspace isolation (gVisor)!

Sandbox automatically cleaned up (context manager exited)
Core tests (1-3) passed!

Cleaning up port-forward...
```

**Note:** Test 4 is optional and may occasionally fail if port-forward becomes unstable. Tests 1-3 are the critical validations - they prove WarmPool and SDK work correctly.

## Step 8: Validation and Testing

### Verify gVisor Runtime Isolation

```bash
# Create a test sandbox
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: isolation-test
  namespace: agent-sandbox-demo
spec:
  sandboxTemplateRef:
    name: python-runtime-template
EOF

kubectl wait --for=condition=Ready sandbox/isolation-test --timeout=60s

POD_NAME=$(kubectl get sandbox isolation-test -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')

# Verify gVisor runtime
kubectl get pod $POD_NAME -o jsonpath='{.spec.runtimeClassName}'
# Should output: gvisor

# Check gVisor kernel virtualization
kubectl exec $POD_NAME -- dmesg | head -5
# Should show gVisor's boot messages 

# Check restricted device access (gVisor limits /dev)
kubectl exec $POD_NAME -- ls /dev | wc -l
# Should show ~16 devices (vs ~150+ in normal containers)

kubectl delete sandboxclaim isolation-test
```

## Step 9: Cleanup

### Quick Cleanup - Delete Everything

```bash
# Delete the entire namespace (removes all resources at once)
kubectl delete namespace agent-sandbox-demo

# Restore default namespace context
kubectl config set-context --current --namespace=default
```

### Delete KIND Cluster

```bash
kind delete cluster --name agent-sandbox-demo
```

## Summary

### What You Built

- **Sandbox**: Isolated execution environments with secure runtime isolation
- **SandboxTemplate**: Reusable sandbox blueprints with runtime configuration
- **SandboxClaim**: Declarative sandbox provisioning API
- **SandboxWarmPool**: Pre-warmed pool for 10-15x faster allocation
- **Python SDK**: Context manager pattern for sandbox lifecycle management
- **Router Service**: HTTP proxy enabling SDK communication
- **Security Runtime**: Userspace kernel isolation with syscall filtering on KIND clusters (gVisor)

## References

- [Agent Sandbox GitHub](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox Documentation](https://agent-sandbox.sigs.k8s.io/)
- [Python SDK Source](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/)
- [gVisor Documentation](https://gvisor.dev/)
- [KIND Documentation](https://kind.sigs.k8s.io/)