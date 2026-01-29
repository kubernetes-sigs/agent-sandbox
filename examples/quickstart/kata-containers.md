# Option 2: Kata Containers on minikube

[← Back to Common Setup Steps](README.md)

## Prerequisites

- KVM/QEMU virtualization support
- minikube (1.32+)
- kubectl (1.28+)
- Python 3.9+
- Git
- Helm
- **Minimum 8GB RAM** (Kata VMs require significant memory overhead)
- **Minimum 4 CPU cores** recommended
- **20GB free disk space** for images and VMs

## Step 1: Start minikube with Containerd

```bash
# Start minikube with containerd runtime
minikube start \
  --driver=kvm2 \
  --container-runtime=containerd \
  --cpus=4 \
  --memory=8192 \
  --profile=agent-sandbox-kata

# Verify cluster is ready
kubectl wait --for=condition=Ready nodes --all --timeout=120s
```

## Step 2: Install Kata Containers using Helm

Kata Containers now uses Helm as the official installation method.

```bash
# Clone Kata Containers repository
git clone --depth 1 https://github.com/kata-containers/kata-containers.git
cd kata-containers/tools/packaging/kata-deploy/helm-chart/kata-deploy

# Update Helm chart dependencies
helm dependency update

# Install Kata Containers using local Helm chart
helm install kata-deploy . \
  --namespace kube-system \
  --create-namespace \
  --wait

# Wait for kata-deploy pods to be ready
kubectl -n kube-system wait --for=condition=Ready pod -l name=kata-deploy --timeout=300s

# Label the minikube node
kubectl label nodes agent-sandbox-kata kata-containers=enabled
```

### Verify Installation

```bash
# Check available RuntimeClasses
kubectl get runtimeclass

# Test Kata with a simple pod
kubectl run kata-test --image=busybox:latest --restart=Never --overrides='
{
  "spec": {
    "runtimeClassName": "kata-qemu",
    "containers": [{
      "name": "kata-test",
      "image": "busybox:latest",
      "command": ["sh", "-c", "uname -r && sleep 3600"]
    }]
  }
}'

# Check it's running
kubectl wait --for=condition=Ready pod/kata-test --timeout=60s

# Verify it's using Kata (should show different kernel version)
kubectl exec kata-test -- uname -r

# Cleanup test pod
kubectl delete pod kata-test
```

## Step 3: Install Agent Sandbox Controller

### 3.1 Install Core Components

```bash
# Fetch latest version (or use specific version like "v0.1.0")
export AGENT_SANDBOX_VERSION=$(curl -s https://api.github.com/repos/kubernetes-sigs/agent-sandbox/releases/latest | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
echo "Using Agent Sandbox version: ${AGENT_SANDBOX_VERSION}"

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/manifest.yaml
```

### 3.2 Install Extensions (SandboxTemplate, SandboxClaim, SandboxWarmPool)

```bash
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/extensions.yaml
```

### 3.3 Verify Installation

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

### 3.4 Create Dedicated Namespace

```bash
# Create a namespace for all Agent Sandbox resources
kubectl create namespace agent-sandbox-demo

# Set as default context to avoid repeating -n flag
kubectl config set-context --current --namespace=agent-sandbox-demo
```

## Step 4: Load Python Runtime Sandbox Image

Load the Python runtime sandbox image we built in the common setup steps into the minikube cluster:

```bash
minikube image load python-runtime-sandbox:latest -p agent-sandbox-kata
```

## Step 5: Apply SandboxTemplate

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

## Step 6: Apply and Verify SandboxWarmPool

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

## Step 7: Load Router Image and Deploy Router Service

Load the router image we built in the common setup steps into the minikube cluster:

```bash
minikube image load sandbox-router:local -p agent-sandbox-kata
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

## Step 8: Run SDK Tests

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
  Sandbox kernel: 6.12.47 (Kata)
  Host kernel:    6.6.95
  Running with VM isolation (Kata Containers)!

Sandbox automatically cleaned up (context manager exited)
Core tests (1-3) passed!

Cleaning up port-forward...
```

**Note:** Test 4 is optional and may occasionally fail if port-forward becomes unstable. Tests 1-3 are the critical validations - they prove WarmPool and SDK work correctly.

## Step 9: Validation and Testing

### Verify Kata Runtime Isolation

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

# Verify Kata runtime
kubectl get pod $POD_NAME -o jsonpath='{.spec.runtimeClassName}'; echo
# Should output: kata-qemu

# Check VM kernel (different from host)
kubectl exec $POD_NAME -- uname -r
# Should show Kata's VM kernel version (different from host)

# Verify it's running in a VM
kubectl exec $POD_NAME -- cat /proc/cpuinfo | grep hypervisor
# Should show hypervisor flag

kubectl delete sandboxclaim isolation-test
```

## Step 10: Cleanup

### Quick Cleanup - Delete Everything

```bash
# Delete the entire namespace (removes all resources at once)
kubectl delete namespace agent-sandbox-demo

# Restore default namespace context
kubectl config set-context --current --namespace=default
```

### Delete minikube Cluster

```bash
minikube delete -p agent-sandbox-kata
```

## Summary

### What You Built

- **Sandbox**: Isolated execution environments with secure runtime isolation
- **SandboxTemplate**: Reusable sandbox blueprints with runtime configuration
- **SandboxClaim**: Declarative sandbox provisioning API
- **SandboxWarmPool**: Pre-warmed pool for 10-15x faster allocation
- **Python SDK**: Context manager pattern for sandbox lifecycle management
- **Router Service**: HTTP proxy enabling SDK communication
- **Security Runtime**: VM-based isolation with full kernel separation on minikube clusters (Kata)

## References

- [Agent Sandbox GitHub](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox Documentation](https://agent-sandbox.sigs.k8s.io/)
- [Python SDK Source](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/)
- [Kata Containers Documentation](https://katacontainers.io/)
- [minikube Documentation](https://minikube.sigs.k8s.io/)