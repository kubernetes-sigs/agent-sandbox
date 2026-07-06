# User Guide: `sandboxd` Portable Backend gRPC Runtime Interface

`sandboxd` is a lightweight, high-performance gRPC daemon running inside Kubernetes sandbox environments managed by `agent-sandbox`. It standardizes the execution and filesystem interface between AI Agent client SDKs and isolated sandbox containers, replacing ad-hoc HTTP endpoints with a vendor-neutral, CNCF-aligned gRPC protocol.

---

## 1. Value Proposition & Key Capabilities

| Capability | Legacy Ad-hoc HTTP / REST | `sandboxd` gRPC Daemon |
| :--- | :--- | :--- |
| **Protocol** | Text JSON over HTTP/1.1 | Binary Protocol Buffers over gRPC (HTTP/2 / Unix Socket) |
| **Latency** | High (JSON parsing & HTTP handshake overhead) | Ultra-Low (Binary serialization & connection multiplexing) |
| **Output Delivery** | Polling or buffered monolithic responses | Real-time bi-directional streaming (`stdout`, `stderr`, `stdin`) |
| **Terminal Allocator** | Basic pipe redirection (no PTY) | Pseudo-terminal allocation (`creack/pty`) with window resizing |
| **Security** | Ad-hoc path handling | Built-in path sandboxing (`pathutil`) & symlink resolution |
| **Language Support** | Custom API per container | Universal Protobuf stubs for Python, Go, Node.js, Rust, etc. |

---

## 2. Pod Architecture: Sidecar Deployment Model (Default)

The **Sidecar Deployment Model** is the primary, recommended architecture for `agent-sandbox`. `sandboxd` runs as a platform-managed sidecar container alongside the user's workload container inside the sandbox Pod.

```
+---------------------------------------------------------------------------------------+
| Kubernetes Sandbox Pod                                                                |
|                                                                                       |
|   +---------------------------------------------------------------+                   |
|   | User Workload Container (Untouched BYO Image: PyTorch, Node)  |                   |
|   |   Mounts: /workspace (shared-workspace), /var/run/sandboxd    |                   |
|   +-------------------------------+-------------------------------+                   |
|                                   ^                                                   |
|                                   | Unix Domain Socket (/var/run/sandboxd/sandboxd.sock)
|                                   v                                                   |
|   +-------------------------------+-------------------------------+                   |
|   | sandboxd Daemon Sidecar (Platform-Managed Image)              |                   |
|   |   Executes /bin/sandboxd --socket-path=/var/run/sandboxd/...  |                   |
|   |   Mounts: /workspace (shared-workspace), /var/run/sandboxd    |                   |
|   +---------------------------------------------------------------+                   |
+---------------------------------------------------------------------------------------+
```

### Advantages of the Sidecar Model:
1. **Zero Friction Bring-Your-Own-Image (BYO)**: Users bring any stock OCI image (`pytorch/pytorch`, `python:3.11`, distroless) without rebuilding or embedding daemons.
2. **Independent Versioning & Security**: The platform manages and patches `sandboxd` independently of user container lifecycles.
3. **Clean Workspace Isolation**: Sharing a dedicated `/workspace` volume ensures `sandboxd` operates on intended files without needing unrestricted rootfs access.

---

## 3. SDK Socket Discovery Contract (Part 3 Contract)

To ensure seamless interoperability between `sandboxd` and Client SDKs across languages:

* **Well-Known Default Socket Path**: `/var/run/sandboxd/sandboxd.sock`
* **Environment Variable Override**: `SANDBOXD_SOCKET` (e.g. `export SANDBOXD_SOCKET=/var/run/sandboxd/sandboxd.sock`)

Client SDKs will automatically check `os.Getenv("SANDBOXD_SOCKET")` first, falling back to `/var/run/sandboxd/sandboxd.sock`.

---

## 4. Native `agent-sandbox` Kubernetes Custom Resources

Below are production CRDs demonstrating the sidecar pattern (compliant with GKE admission & hardening policies):

### 4.1 Direct `Sandbox` Manifest (`agents.x-k8s.io/v1beta1`)

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: python-calculator-sandbox
  namespace: default
spec:
  podTemplate:
    spec:
      automountServiceAccountToken: false
      runtimeClassName: gvisor
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      nodeSelector:
        sandbox.gke.io/runtime: gvisor
      tolerations:
        - key: sandbox.gke.io/runtime
          operator: Equal
          value: gvisor
          effect: NoSchedule
      containers:
        # 1. User Workload Container (Stock Image, Untouched)
        - name: user-workload
          image: registry.k8s.io/agent-sandbox/python-runtime-sandbox:v0.1.0
          command: ["sleep", "infinity"]
          env:
            - name: SANDBOXD_SOCKET
              value: /var/run/sandboxd/sandboxd.sock
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: "1"
              memory: 1Gi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: shared-workspace
              mountPath: /workspace
            - name: sandboxd-socket
              mountPath: /var/run/sandboxd

        # 2. Platform-Managed sandboxd Sidecar
        - name: sandboxd-daemon
          image: <YOUR_REGISTRY>/sandboxd-sidecar:latest
          args:
            - --socket-path=/var/run/sandboxd/sandboxd.sock
            - --root-dir=/workspace
            - -v=2
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: "1"
              memory: 1Gi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: shared-workspace
              mountPath: /workspace
            - name: sandboxd-socket
              mountPath: /var/run/sandboxd
      restartPolicy: Never
      volumes:
        - name: shared-workspace
          emptyDir: {}
        - name: sandboxd-socket
          emptyDir: {}
```

---

### 4.2 Pre-Warmed Pool Workflow: `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim` (`extensions.agents.x-k8s.io/v1beta1`)

```yaml
# 1. Define Sidecar Template
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata:
  name: python-sandbox-template
  namespace: default
spec:
  podTemplate:
    spec:
      automountServiceAccountToken: false
      runtimeClassName: gvisor
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      nodeSelector:
        sandbox.gke.io/runtime: gvisor
      tolerations:
        - key: sandbox.gke.io/runtime
          operator: Equal
          value: gvisor
          effect: NoSchedule
      containers:
        # User Container
        - name: user-workload
          image: registry.k8s.io/agent-sandbox/python-runtime-sandbox:v0.1.0
          command: ["sleep", "infinity"]
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: "1"
              memory: 1Gi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: shared-workspace
              mountPath: /workspace
            - name: sandboxd-socket
              mountPath: /var/run/sandboxd

        # Sidecar Daemon
        - name: sandboxd-daemon
          image: <YOUR_REGISTRY>/sandboxd-sidecar:latest
          args:
            - --socket-path=/var/run/sandboxd/sandboxd.sock
            - --root-dir=/workspace
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: "1"
              memory: 1Gi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: shared-workspace
              mountPath: /workspace
            - name: sandboxd-socket
              mountPath: /var/run/sandboxd
      volumes:
        - name: shared-workspace
          emptyDir: {}
        - name: sandboxd-socket
          emptyDir: {}
---
# 2. Maintain Pre-Warmed Pool
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: python-calculator-warmpool
  namespace: default
spec:
  replicas: 2
  sandboxTemplateRef:
    name: python-sandbox-template
---
# 3. Claim Sandbox
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: python-calculator-claim
  namespace: default
spec:
  warmPoolRef:
    name: python-calculator-warmpool
  lifecycle:
    shutdownPolicy: Delete
```

---

## 5. Multi-Language Developer Guide & Real-Time Output Streaming

### A. Python Client Example with Real-Time Streaming

```python
import os
import grpc
import filesystem_pb2 as fs_pb
import filesystem_pb2_grpc as fs_grpc
import process_pb2 as proc_pb
import process_pb2_grpc as proc_grpc

# Discover socket path via environment variable or default
socket_path = f"unix://{os.getenv('SANDBOXD_SOCKET', '/var/run/sandboxd/sandboxd.sock')}"
channel = grpc.insecure_channel(socket_path)
fs_client = fs_grpc.FilesystemServiceStub(channel)
proc_client = proc_grpc.ProcessServiceStub(channel)

# 1. Upload calculate script via FilesystemService.Write
calc_code = b"""
import math
import time

print("--> Starting calculation pipeline...", flush=True)
time.sleep(1)
val = 144
res = math.sqrt(val) * 42
print(f"CALCULATION_RESULT: sqrt({val}) * 42 = {res}", flush=True)
"""

def generate_chunks():
    yield fs_pb.WriteRequest(metadata=fs_pb.FileMetadata(path="calc.py", mode=0o644))
    yield fs_pb.WriteRequest(chunk=calc_code)

fs_client.Write(generate_chunks())
print("1. Uploaded calc.py successfully.")

# 2. Execute and stream output in real-time via ProcessService.Start
start_req = proc_pb.StartRequest(
    config=proc_pb.ProcessConfig(
        command=["python3", "calc.py"],
        cwd="/workspace"
    )
)

print("2. Executing script and streaming frames...")
for response in proc_client.Start(start_req):
    event_type = response.WhichOneof("event")
    if event_type == "init":
        print(f"   -> Process Started (Virtual PID: {response.init.process_id})")
    elif event_type == "stdout":
        print(f"   -> STDOUT Stream Chunk: {response.stdout.decode('utf-8')}", end="")
    elif event_type == "exit":
        print(f"   -> Process Finished (Exit Code: {response.exit.exit_code})")
```

---

### B. Real-Time Execution Timeline Output

When executed against `sandboxd`, output frames are delivered incrementally as they occur:

```text
Connecting to sandboxd daemon at unix:///var/run/sandboxd/sandboxd.sock...
1. Uploaded calc.py successfully.
2. Executing script and streaming frames...
[2ms]    FRAME 1: InitEvent (Virtual PID=5)
[4ms]    FRAME 2: StdoutChunk -> --> Starting calculation pipeline...
[1.01s]  FRAME 3: StdoutChunk -> CALCULATION_RESULT: sqrt(144) * 42 = 504.0
[1.01s]  FRAME 4: ExitEvent (Exit Code=0)
```

---

### C. Go Client Example

```go
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	filesystemv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/filesystem/v1"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

func getSocketPath() string {
	if s := os.Getenv("SANDBOXD_SOCKET"); s != "" {
		return s
	}
	return "/var/run/sandboxd/sandboxd.sock"
}

func main() {
	socketPath := getSocketPath()
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}

	conn, err := grpc.NewClient("passthrough:///unix",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	procClient := processv1.NewProcessServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Execute one-shot calculation via ProcessService.Execute
	res, err := procClient.Execute(ctx, &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"python3", "-c", "import math; print(f'RESULT: {math.sqrt(144) * 42}')"},
			Cwd:     proto.String("/workspace"),
		},
	})
	if err != nil {
		panic(err)
	}

	fmt.Printf("Exit Code: %d\n", res.GetExitCode())
	fmt.Printf("Output: %s\n", string(res.GetStdout()))
}
```

---

### D. CLI Inspection using `grpcurl`

```bash
SOCKET_PATH=${SANDBOXD_SOCKET:-/var/run/sandboxd/sandboxd.sock}

# Stat the uploaded script
grpcurl -plaintext -unix "$SOCKET_PATH" \
  -d '{"path": "calc.py"}' \
  filesystem.v1.FilesystemService/Stat

# Stream execution output
grpcurl -plaintext -unix "$SOCKET_PATH" \
  -d '{"config": {"command": ["python3", "calc.py"], "cwd": "/workspace"}}' \
  process.v1.ProcessService/Start
```
