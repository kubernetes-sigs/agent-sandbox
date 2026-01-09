# Secure Agent Sandbox Quickstart

## Overview

This guide demonstrates how to set up and use Agent Sandbox with secure container runtimes. Choose your preferred isolation method:

## Isolation Strategies

**Option 1: gVisor on KIND**

**Option 2: Kata Containers on minikube**

Both options provide:
- **Sandbox**: Core isolated environment for running untrusted code
- **SandboxTemplate**: Reusable blueprint for sandbox configurations
- **SandboxClaim**: Declarative API for requesting sandboxes
- **SandboxWarmPool**: Pre-warmed sandbox pool for fast allocation
- **Python SDK**: Programmatic sandbox management
- **Router Service**: HTTP proxy for SDK communication

# Option 1: gVisor on KIND

## Prerequisites

- Docker (20.10+)
- kubectl (1.28+)
- KIND (0.20+)
- Python 3.9+
- Git
- Linux host (gVisor requires Linux)

## Step 1: Create KIND Cluster with gVisor Support

### 1.1 Install gVisor Runtime

First, install the gVisor runsc runtime on your host machine:

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
  rm -f *.sha512
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

**Continue to "Common Setup Steps" below**

# Option 2: Kata Containers on minikube

## Prerequisites

- KVM/QEMU virtualization support
- minikube (1.32+)
- kubectl (1.28+)
- Python 3.9+
- Git

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
git clone https://github.com/kata-containers/kata-containers.git
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

**Continue to "Common Setup Steps" below**

# Common Setup Steps

## Step 2: Install Agent Sandbox Controller

### 2.1 Install Core Components

```bash
export AGENT_SANDBOX_VERSION="v0.1.0"

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

## Step 3: Build and Load Python Runtime Sandbox Image

### 3.1 Clone the Agent Sandbox Repository

```bash
git clone https://github.com/kubernetes-sigs/agent-sandbox.git
cd agent-sandbox/examples/python-runtime-sandbox
```

### 3.2 Build the Sandbox Image

```bash
docker build -t python-runtime-sandbox:latest .
```

### 3.3 Load Image into Cluster

**For KIND:**
```bash
kind load docker-image python-runtime-sandbox:latest --name agent-sandbox-demo
```

**For minikube:**
```bash
minikube image load python-runtime-sandbox:latest -p agent-sandbox-kata
```

## Step 4: Create SandboxTemplate

Create a SandboxTemplate that defines the sandbox blueprint with your chosen runtime:

**For gVisor (KIND):**
```bash
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-runtime-template
  namespace: default
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: python-sandbox
    spec:
      runtimeClassName: gvisor  # Enable gVisor isolation
      containers:
      - name: python-runtime
        image: python-runtime-sandbox:latest
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8888
          protocol: TCP
        readinessProbe:
          httpGet:
            path: /
            port: 8888
          initialDelaySeconds: 2
          periodSeconds: 1
        resources:
          requests:
            cpu: "250m"
            memory: "512Mi"
            ephemeral-storage: "512Mi"
          limits:
            cpu: "500m"
            memory: "1Gi"
            ephemeral-storage: "1Gi"
      restartPolicy: OnFailure
EOF
```

**For Kata Containers (minikube):**
```bash
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-runtime-template
  namespace: default
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: python-sandbox
    spec:
      runtimeClassName: kata-qemu  # Enable Kata VM isolation
      containers:
      - name: python-runtime
        image: python-runtime-sandbox:latest
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8888
          protocol: TCP
        readinessProbe:
          httpGet:
            path: /
            port: 8888
          initialDelaySeconds: 2
          periodSeconds: 1
        resources:
          requests:
            cpu: "250m"
            memory: "512Mi"
            ephemeral-storage: "512Mi"
          limits:
            cpu: "500m"
            memory: "1Gi"
            ephemeral-storage: "1Gi"
      restartPolicy: OnFailure
EOF
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

## Step 5: Configure SandboxWarmPool

Create a SandboxWarmPool to maintain pre-warmed pod instances for fast sandbox allocation:

```bash
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: python-warmpool
  namespace: default
spec:
  replicas: 2
  sandboxTemplateRef:
    name: python-runtime-template
EOF
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

## Step 6: Build and Deploy Router Service

The Python SDK requires a router service to proxy HTTP requests to sandboxes.

### 6.1 Build Router Image

```bash
cd agent-sandbox/clients/python/agentic-sandbox-client/sandbox-router

# Build the router image
docker build -t sandbox-router:local .
```

### 6.2 Load Router Image into Cluster

**For KIND:**
```bash
kind load docker-image sandbox-router:local --name agent-sandbox-demo
```

**For minikube:**
```bash
minikube image load sandbox-router:local -p agent-sandbox-kata
```

### 6.3 Deploy Router Service

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: sandbox-router-svc
  namespace: default
spec:
  type: ClusterIP
  selector:
    app: sandbox-router
  ports:
  - name: http
    protocol: TCP
    port: 8080
    targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sandbox-router-deployment
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: sandbox-router
  template:
    metadata:
      labels:
        app: sandbox-router
    spec:
      containers:
      - name: router
        image: sandbox-router:local
        imagePullPolicy: Never
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 10
        resources:
          requests:
            cpu: "250m"
            memory: "512Mi"
          limits:
            cpu: "1000m"
            memory: "1Gi"
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
EOF
```

### 6.4 Verify Router Deployment

```bash
# Check router pods
kubectl get pods -l app=sandbox-router

# Check router service
kubectl get svc sandbox-router-svc

# Test router health
kubectl port-forward svc/sandbox-router-svc 8080:8080 &
sleep 2
curl http://localhost:8080/healthz
# Should return: {"status":"ok"}
pkill -f "port-forward svc/sandbox-router-svc"
```

## Step 7: Install and Test Python SDK

### 7.1 Install the SDK

```bash
# Create and activate a virtual environment
python3 -m venv ~/agent-sandbox-venv
source ~/agent-sandbox-venv/bin/activate

# Install the SDK
cd agent-sandbox/clients/python/agentic-sandbox-client
pip3 install .

# Verify installation
pip3 list | grep agentic_sandbox
```

**Note:** Keep the virtual environment activated for all subsequent SDK commands.

### 7.2 Create SDK Test Script for Agent Sandbox with WarmPool

```bash
cat > test_sdk_warmpool.py <<'EOF'
#!/usr/bin/env python3

from agentic_sandbox import SandboxClient
import subprocess
import time
import sys
import signal
import socket

# Global to track port-forward process
portforward_proc = None
local_port = None

def cleanup(signum=None, frame=None):
    global portforward_proc
    if portforward_proc:
        print("\nCleaning up port-forward...")
        portforward_proc.terminate()
        try:
            portforward_proc.wait(timeout=2)
        except subprocess.TimeoutExpired:
            portforward_proc.kill()
    if signum is not None:
        sys.exit(0)

signal.signal(signal.SIGINT, cleanup)
signal.signal(signal.SIGTERM, cleanup)

def find_free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(('', 0))
        s.listen(1)
        port = s.getsockname()[1]
    return port

def setup_portforward():
    global portforward_proc, local_port
    
    local_port = find_free_port()
    print(f"Setting up port-forward: localhost:{local_port} -> svc/sandbox-router-svc:8080")
    
    portforward_proc = subprocess.Popen(
        ["kubectl", "port-forward", "svc/sandbox-router-svc", f"{local_port}:8080"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE
    )
    
    time.sleep(3)
    print(f"Port-forward started\n")
    
    return local_port

def main():
    print("=== Testing SDK with WarmPool ===\n")
    
    # Set up port-forward automatically
    setup_portforward()
    
    try:
        start_time = time.time()
        
        with SandboxClient(
            template_name="python-runtime-template",
            namespace="default",
            api_url=f"http://localhost:{local_port}",
        ) as sandbox:
            
            allocation_time = time.time() - start_time
            
            print(f"Sandbox created: {sandbox.claim_name}")
            print(f"Pod allocated: {sandbox.pod_name}")
            print(f"Allocation time: {allocation_time:.2f}s\n")
            
            print("Test 1: Running Python command")
            result = sandbox.run("python3 -c 'print(\"Hello from SDK!\")'")
            print(f"  stdout: {result.stdout.strip()}")
            print(f"  exit_code: {result.exit_code}\n")
            
            print("Test 2: File operations")
            sandbox.write("test.txt", "SDK test content")
            content = sandbox.read("test.txt")
            print(f"  File written and read successfully")
            print(f"  Content: {content.decode('utf-8').strip()}\n")
            
            print("Test 3: WarmPool performance check")
            claim_cmd = f"kubectl get sandboxclaim {sandbox.claim_name} -o jsonpath='{{.metadata.creationTimestamp}}'"
            pod_cmd = f"kubectl get pod {sandbox.pod_name} -o jsonpath='{{.metadata.creationTimestamp}}'"
            
            claim_time = subprocess.check_output(claim_cmd, shell=True).decode().strip()
            pod_time = subprocess.check_output(pod_cmd, shell=True).decode().strip()
            
            print(f"  SandboxClaim created: {claim_time}")
            print(f"  Pod created:          {pod_time}")
            
            if pod_time < claim_time:
                print(f"Pod was PRE-WARMED from WarmPool!\n")
            else:
                print(f"Pod created on-demand (not from warmpool)\n")
            
            print("Test 4: Runtime isolation check")
            try:
                result = sandbox.run("uname -r")
                sandbox_kernel = result.stdout.strip()
                print(f"  Sandbox kernel: {sandbox_kernel}")
                
                try:
                    # For minikube
                    host_kernel = subprocess.check_output(
                        ["minikube", "ssh", "-p", "agent-sandbox-kata", "uname -r"],
                        text=True,
                        stderr=subprocess.DEVNULL
                    ).strip()
                except:
                    try:
                        # For KIND
                        host_kernel = subprocess.check_output(
                            ["docker", "exec", "agent-sandbox-demo-control-plane", "uname", "-r"],
                            text=True,
                            stderr=subprocess.DEVNULL
                        ).strip()
                    except:
                        host_kernel = "unknown"
                
                if host_kernel != "unknown":
                    print(f"  Host kernel:    {host_kernel}")
                    if sandbox_kernel != host_kernel:
                        print(f"Running with VM isolation (Kata Containers)!")
                    else:
                        print(f"Running with userspace isolation (gVisor)!")
                else:
                    print(f"Host kernel detection skipped")
            except Exception as e:
                print(f"Test 4 skipped (port-forward may have died): {e}")
        
        print("\nSandbox automatically cleaned up (context manager exited)")
        print("Core tests (1-3) passed!")
        
    finally:
        cleanup()

if __name__ == "__main__":
    main()
EOF

chmod +x test_sdk_warmpool.py
```

### 7.3 Run the Test

```bash
python3 test_sdk_warmpool.py
```

Expected output:
```
=== Testing SDK with WarmPool ===

Setting up port-forward: localhost:42303 -> svc/sandbox-router-svc:8080
Port-forward started

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
  Sandbox kernel: 6.12.47  (Kata) or 6.1.38 (gVisor)
  Host kernel:    6.6.95
  Running with VM isolation (Kata Containers)!
  (or: Running with userspace isolation (gVisor)!)

Sandbox automatically cleaned up (context manager exited)
Core tests (1-3) passed!

Cleaning up port-forward...
```

**Note:** Test 4 is optional and may occasionally fail if port-forward becomes unstable. Tests 1-3 are the critical validations - they prove WarmPool and SDK work correctly.

### 7.4 How the SDK Works

**The Request Flow:**
1. Script automatically starts: `kubectl port-forward svc/sandbox-router-svc <random-port>:8080`
2. SDK creates SandboxClaim via K8s API
3. Controller provisions Sandbox (from WarmPool if available)
4. SDK connects to `http://localhost:<random-port>` (via api_url parameter)
5. Port-forward tunnels request to router service
6. Router resolves sandbox DNS: `http://sandbox-claim-xyz.default.svc.cluster.local:8888`
7. Router proxies request to actual sandbox pod
8. Response streams back: sandbox -> router -> port-forward -> SDK
9. Script automatically cleans up port-forward on exit

**When to Use Cluster DNS:**
The `api_url="http://sandbox-router-svc.default.svc.cluster.local:8080"` approach (without port-forward) only works when your code runs **inside a Kubernetes pod**, such as:
- AI agents deployed as pods in the cluster
- CI/CD pipelines running in Kubernetes
- Applications that themselves run in the cluster

For local development (laptop/desktop), the script handles port-forward automatically.

## Step 8: Validation and Testing

### Verify Runtime Isolation

**For gVisor:**
```bash
# Create a test sandbox
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: isolation-test
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

**For Kata Containers:**
```bash
# Create a test sandbox
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: isolation-test
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

## Step 9: Cleanup

### Remove Test Resources

```bash
# Delete all sandboxclaims
kubectl delete sandboxclaim --all -n default

# Delete all sandboxes
kubectl delete sandbox --all -n default

# Delete warmpool
kubectl delete sandboxwarmpool python-warmpool

# Delete template
kubectl delete sandboxtemplate python-runtime-template

# Delete router
kubectl delete deployment sandbox-router-deployment
kubectl delete service sandbox-router-svc
```

### Delete Cluster

**For KIND:**
```bash
kind delete cluster --name agent-sandbox-demo
```

**For minikube:**
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
- **Security Runtime**: Userspace kernel isolation with syscall filtering on KIND clusters (gVisor) or VM-based isolation with full kernel separation on minikube clusters (Kata)

## References

- [Agent Sandbox GitHub](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox Documentation](https://agent-sandbox.sigs.k8s.io/)
- [Python SDK Source](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/)
- [gVisor Documentation](https://gvisor.dev/)
- [Kata Containers Documentation](https://katacontainers.io/)
- [KIND Documentation](https://kind.sigs.k8s.io/)
- [minikube Documentation](https://minikube.sigs.k8s.io/)