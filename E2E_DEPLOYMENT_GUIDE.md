# Comprehensive E2E Deployment and Persona Integration Guide

This document provides a complete guide to deploying the gRPC Sandbox Daemon (`agent-sandbox-agent`) on a GKE cluster. It demonstrates how to bridge the workflows of **Persona A (Platform Engineer)** and **Persona B (AI Developer)** using Go and Python.

---

## Persona A: The Platform / Infrastructure Engineer
**Role:** Creates secure container images, registers sandbox templates, and builds programmatic orchestrators.

### Step 1: Build the GKE-Optimized Sandbox Agent
Run from the root of your workspace:

```bash
# 1. Compile the Go binaries
make build

# 2. Build and Push the Image
# The Dockerfile includes ipykernel for Jupyter support and internal health checks
docker build --target agent -t us-central1-docker.pkg.dev/<project>/<repo>/agent-sandbox-agent:latest .
docker push us-central1-docker.pkg.dev/<project>/<repo>/agent-sandbox-agent:latest
```

### Step 2: Register the SandboxTemplate
This template is fully compliant with GKE Hardening Policies. Note that **one template can serve multiple claims** simultaneously; Kubernetes provides unique IP isolation for every pod.

For a ready-made example, you can use the repo-provided template at `extensions/examples/gprc/python-sandbox-template.yaml`.

**Create `persona_a_template.yaml`:**
```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-sandbox-template
  namespace: default
spec:
  podTemplate:
    spec:
      runtimeClassName: gvisor
      securityContext:
        runAsUser: 1000
        runAsNonRoot: true
      nodeSelector:
        sandbox.gke.io/runtime: gvisor
      containers:
        - name: runtime
          image: registry.k8s.io/agent-sandbox/python-runtime-sandbox:v0.1.0
          ports:
            - containerPort: 8888
          resources:
            requests:
              cpu: "250m"
              memory: "512Mi"
            limits:
              cpu: "500m"
              memory: "1Gi"
          securityContext:
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: shared-workspace
              mountPath: /workspace

        - name: sandbox-daemon
          image: us-central1-docker.pkg.dev/<project>/<repo>/agent-sandbox-agent:latest
          command: ["/bin/agent-sandbox-agent"]
          args: ["--port=50051"]
          ports:
            - containerPort: 50051
              name: grpc-port
          resources:
            requests:
              cpu: "100m"
              memory: "128Mi"
            limits:
              cpu: "200m"
              memory: "256Mi"
          env:
            - name: HOME
              value: /tmp
          securityContext:
            runAsUser: 1000
            runAsNonRoot: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: shared-workspace
              mountPath: /workspace

      restartPolicy: OnFailure
      volumes:
        - name: shared-workspace
          emptyDir: {}
```

**Apply it:**
```bash
kubectl apply -f persona_a_template.yaml
```

> Alternatively, use the provided example template in the repo:
> `kubectl apply -f extensions/examples/gprc/python-sandbox-template.yaml`

### Step 3: Programmatic Orchestration (Optional)
Persona A can dynamically trigger sandboxes using the Kubernetes API.

**Golang Example (`examples/manifest_builder/main.go`):**
```go
func BuildSandboxClaim(name, namespace, templateName string) *unstructured.Unstructured {
    return &unstructured.Unstructured{
        Object: map[string]interface{}{
            "apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
            "kind":       "SandboxClaim",
            "metadata": map[string]interface{}{"name": name, "namespace": namespace},
            "spec": map[string]interface{}{
                "sandboxTemplateRef": map[string]interface{}{"name": templateName},
            },
        },
    }
}
```

### Step 4: Trigger a Sandbox (Claim)
**Create `persona_a_claim.yaml`:**
```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: e2e-verification-claim
  namespace: default
spec:
  sandboxTemplateRef:
    name: python-sandbox-template
```

**Apply and wait for readiness:**
```bash
kubectl apply -f persona_a_claim.yaml
kubectl wait --for=condition=Ready pod/e2e-verification-claim --timeout=300s
```

---

## Persona B: The AI / Agent Developer
**Role:** Interacts with the active sandbox via gRPC SDKs to execute cognitive loops.

### Step 1: Establish Connectivity
Find your pod name and establish a tunnel. 

**Pro Tip:** If testing multiple sandboxes locally, map different local ports to the same remote port:
```bash
# To talk to Sandbox A
kubectl port-forward pod/e2e-verification-claim 50051:50051
```

### Step 2: Execute Code via gRPC

**Python E2E Example:**
```python
import grpc
import agent_sandbox_pb2 as pb
import agent_sandbox_pb2_grpc as pb_grpc

channel = grpc.insecure_channel("localhost:50051")
proc_stub = pb_grpc.ProcessServiceStub(channel)

# Execute code inside the secure GKE container
resp = proc_stub.Execute(pb.ExecuteRequest(command=["python3", "-c", "print('Hello GKE')"]))
print(resp.stdout)
```

**Go E2E Example:**
```go
conn, _ := grpc.Dial("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
jupyterClient := pb.NewJupyterServiceClient(conn)

// Create a stateful session (Agent blocks internally until Jupyter is ready)
session, _ := jupyterClient.CreateSession(ctx, &pb.CreateJupyterSessionRequest{KernelName: "python3"})

// Execute code with persistent variables
jupyterClient.ExecuteCode(ctx, &pb.ExecuteJupyterCodeRequest{
    SessionId: session.SessionId,
    Code:      "x = 42",
})
```

### Step 3: Interactive Verification
The Sandbox Agent handles its own internal "warm-up". You can run the full verification suite in one go:

**Option 1: Run Go POC (from workspace root)**
```bash
go run examples/client_poc/main.go
```

**Option 2: Run Python POC**
```bash
# Run the verification script
python3 examples/client_poc.py
```

---

## Troubleshooting & Success Metrics

### Success Metrics
- **One-Go Reliability:** The very first gRPC call succeeds (no manual sleep needed).
- **Persistence:** Variables defined in one `ExecuteCode` call are available in the next.
- **Hardening:** Pod is running as non-root user `1000` on a `gvisor` node.

### Common Issues
- **`connection refused` on 50051:** Ensure no other local process is using 50051. Use `lsof -i :50051` to check.
- **`404 Not Found` on Session Create:** This is fixed in the latest image by including `ipykernel`. Ensure you are using the latest built image.
- **`Pending` Pod:** This is normal; GKE Autopilot is provisioning a gVisor node pool. Wait 3-5 minutes.
