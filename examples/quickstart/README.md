# Secure Agent Sandbox Quickstart

## Overview

This guide demonstrates how to set up and use Agent Sandbox with secure container runtimes. Choose your preferred isolation method:

## Isolation Strategies

**[Option 1: gVisor on KIND](gvisor-kind-setup.md)**

**[Option 2: Kata Containers on minikube](kata-minikube-setup.md)**

Both options provide:
- **Sandbox**: Core isolated environment for running untrusted code
- **SandboxTemplate**: Reusable blueprint for sandbox configurations
- **SandboxClaim**: Declarative API for requesting sandboxes
- **SandboxWarmPool**: Pre-warmed sandbox pool for fast allocation
- **Python SDK**: Programmatic sandbox management
- **Router Service**: HTTP proxy for SDK communication

---

# Common Setup Steps

## Step 1: Clone the Agent Sandbox Repository

```bash
git clone https://github.com/kubernetes-sigs/agent-sandbox.git
cd agent-sandbox/
```

### 1.1 Choose Sandbox and Router Images

Set the image references you will use throughout this quickstart:

```bash
export PYTHON_RUNTIME_IMAGE=registry.k8s.io/agent-sandbox/python-runtime-sandbox:v0.1.1
export ROUTER_IMAGE=sandbox-router:local
```

## Step 2: Create SandboxTemplate Manifest

Create a SandboxTemplate YAML file that will define the sandbox blueprint with your chosen runtime.

Set the runtime based on your choice:

```bash
# Set the runtime based on your choice
export RUNTIME_CLASS_NAME=gvisor  # or 'kata-qemu'
```

```bash
cat > SandboxTemplate.yaml <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-runtime-template
  namespace: agent-sandbox-demo
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: python-sandbox
    spec:
      runtimeClassName: ${RUNTIME_CLASS_NAME}
      containers:
      - name: python-runtime
        image: ${PYTHON_RUNTIME_IMAGE}
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

## Step 3: Create SandboxWarmPool Manifest

Create a SandboxWarmPool YAML file that will maintain pre-warmed pod instances for fast sandbox allocation:

```bash
cat > SandboxWarmPool.yaml <<EOF
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

## Step 4: Build Router Image

Build the router image locally:

```bash
cd clients/python/agentic-sandbox-client/sandbox-router
docker build -t ${ROUTER_IMAGE} .
cd ../../../../
```

## Step 5: Create Router Service Manifest

Create a Router Service YAML file that the Python SDK will use to proxy HTTP requests to sandboxes:

```bash
cat > RouterService.yaml <<EOF
apiVersion: v1
kind: Service
metadata:
  name: sandbox-router-svc
  namespace: agent-sandbox-demo
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
  namespace: agent-sandbox-demo
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
        image: ${ROUTER_IMAGE}
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

## Step 6: Install Python SDK

### 6.1 Install the SDK

```bash
# Create and activate a virtual environment
python3 -m venv .venv
source .venv/bin/activate

# Install the SDK
cd clients/python/agentic-sandbox-client
pip3 install .

# Verify installation
pip3 list | grep k8s-agent-sandbox
# Go back to agent-sandbox directory
cd ../../../
```

**Note:** Keep the virtual environment activated for all subsequent SDK commands.

## Step 7: Create SDK Test Script

Create a test script that will validate the entire setup including WarmPool performance:

```bash
cat > test_sdk_warmpool.py <<'EOF'
#!/usr/bin/env python3

from k8s_agent_sandbox import SandboxClient
import subprocess
import time
import sys
import signal
import socket
import select

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
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1
    )

    # Wait for port-forward readiness and fail fast on startup errors.
    deadline = time.time() + 10
    while time.time() < deadline:
        if portforward_proc.poll() is not None:
            stderr_output = portforward_proc.stderr.read().strip()
            cleanup()
            raise RuntimeError(
                f"Port-forward failed to start: {stderr_output or 'process exited before readiness'}"
            )

        ready, _, _ = select.select([portforward_proc.stdout], [], [], 0.2)
        if not ready:
            continue

        line = portforward_proc.stdout.readline()
        if "Forwarding from" in line:
            print("Port-forward ready\n")
            return local_port
        if "error" in line.lower():
            stderr_output = portforward_proc.stderr.read().strip()
            cleanup()
            raise RuntimeError(
                f"Port-forward failed to start: {line.strip() or stderr_output or 'unknown error'}"
            )

    cleanup()
    stderr_output = portforward_proc.stderr.read().strip()
    raise TimeoutError(
        f"Timed out waiting for port-forward readiness: {stderr_output or 'no readiness message received'}"
    )

def main():
    print("=== Testing SDK with WarmPool ===\n")

    try:
        # Set up port-forward automatically
        setup_portforward()

        start_time = time.time()
        
        with SandboxClient(
            template_name="python-runtime-template",
            namespace="agent-sandbox-demo",
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
                print(f"  Pod was PRE-WARMED from WarmPool!\n")
            else:
                print(f"  Pod created on-demand (not from warmpool)\n")
            
            print("Test 4: Runtime isolation check")
            try:
                # Use Python to count /dev entries - works in any Python container
                count_script = "import os; print(len(os.listdir('/dev')))"
                result = sandbox.run(f"python3 -c \"{count_script}\"")
                dev_count = int(result.stdout.strip())
                print(f"  /dev entries: {dev_count}")
                
                # Also check kernel version
                result_uname = sandbox.run("uname -r")
                sandbox_kernel = result_uname.stdout.strip()
                print(f"  Sandbox kernel: {sandbox_kernel}")
                
                try:
                    host_kernel = subprocess.check_output(
                        ["docker", "exec", "agent-sandbox-demo-control-plane", "uname", "-r"],
                        text=True,
                        stderr=subprocess.DEVNULL
                    ).strip()
                except:
                    try:
                        host_kernel = subprocess.check_output(
                            ["minikube", "ssh", "-p", "agent-sandbox-kata", "uname -r"],
                            text=True,
                            stderr=subprocess.DEVNULL
                        ).strip()
                    except:
                        host_kernel = "unknown"
                
                if host_kernel != "unknown":
                    print(f"  Host kernel:    {host_kernel}")
                
                # Determine isolation type
                if dev_count < 20:
                    print(f"\n  Running with userspace isolation (gVisor)")
                    print(f"    - Minimal /dev filesystem ({dev_count} entries)")
                    print(f"    - Emulated kernel {sandbox_kernel}")
                elif sandbox_kernel != host_kernel:
                    print(f"\n  Running with VM isolation (Kata Containers)")
                    print(f"    - Full /dev filesystem ({dev_count} entries)")
                    print(f"    - Different kernel (VM: {sandbox_kernel} vs Host: {host_kernel})")
                else:
                    print(f"\n  Running with standard container runtime")
                    print(f"    - /dev entries: {dev_count}")
                        
            except Exception as e:
                print(f"  Runtime detection failed: {e}")
        
        print("\nSandbox automatically cleaned up (context manager exited)")
        print("\n=== All tests passed! ===")
        
    finally:
        cleanup()

if __name__ == "__main__":
    main()
EOF
```

### How the SDK Works

**The Request Flow:**
1. Script automatically starts: `kubectl port-forward svc/sandbox-router-svc <random-port>:8080`
2. SDK creates SandboxClaim via K8s API
3. Controller provisions Sandbox (from WarmPool if available)
4. SDK connects to `http://localhost:<random-port>` (via api_url parameter)
5. Port-forward tunnels request to router service
6. Router resolves sandbox DNS: `http://sandbox-claim-xyz.agent-sandbox-demo.svc.cluster.local:8888`
7. Router proxies request to actual sandbox pod
8. Response streams back: sandbox → router → port-forward → SDK
9. Script automatically cleans up port-forward on exit

**When to Use Cluster DNS:**
The `api_url="http://sandbox-router-svc.agent-sandbox-demo.svc.cluster.local:8080"` approach (without port-forward) only works when your code runs **inside a Kubernetes pod**, such as:
- AI agents deployed as pods in the cluster
- CI/CD pipelines running in Kubernetes
- Applications that themselves run in the cluster

For local development (laptop/desktop), the script handles port-forward automatically.

---

## Next Steps

Now continue to **one of the platform-specific guides** to complete the setup:

- **[gVisor on KIND Setup →](gvisor-kind-setup.md)**
- **[Kata Containers on minikube Setup →](kata-minikube-setup.md)**

Each guide will walk you through:
1. Setting up your chosen runtime (gVisor or Kata)
2. Installing Agent Sandbox Controller
3. Loading the locally built router image
4. Applying the manifests you created
5. Running tests and validating isolation

## References

- [Agent Sandbox GitHub](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox Documentation](https://agent-sandbox.sigs.k8s.io/)
- [Python SDK Source](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/)
- [gVisor Documentation](https://gvisor.dev/)
- [Kata Containers Documentation](https://katacontainers.io/)
- [KIND Documentation](https://kind.sigs.k8s.io/)
- [minikube Documentation](https://minikube.sigs.k8s.io/)
