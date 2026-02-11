# Secure Sandbox Admission Policy (VAP)

This directory contains the **Validating Admission Policy (VAP)** for the Agent Sandbox. It acts as a cluster-level guardrail to enforce a "Secure by Default" posture for all sandbox workloads.

## Enforced Security Controls

This policy **rejects** any `Sandbox` resource that attempts to bypass the following critical security controls.

| Control | Enforced Rule | Security Benefit |
| :--- | :--- | :--- |
| **Runtime Isolation** | `runtimeClassName: gvisor` | **Prevents Container Escape.** <br> Mandates the use of gVisor (a user-space kernel) to strongly isolate the untrusted workload from the underlying host kernel. |
| **Node Isolation** | `hostNetwork: false` | **Protects Node Metadata.** <br> Prevents the sandbox from accessing the host's network namespace, effectively blocking access to the Cloud Metadata Server (169.254.169.254) and other localhost services. |
| **Process Isolation** | `hostPID` & `hostIPC`: false | **Prevents Namespace Leaks.** <br> Ensures the sandbox cannot see or signal processes on the host node (`hostPID`) or use host inter-process communication mechanisms (`hostIPC`). |
| **Network Isolation** | `ports.hostPort: Forbidden` | **Prevents Port Hijacking.** <br> Blocks containers from binding directly to ports on the host node's network interface, which would bypass network policies and consume node resources. |
| **Identity Isolation** | `automountServiceAccountToken: false` | **Prevents K8s API Abuse.** <br> Ensures that the sandbox pod does not receive a Kubernetes Service Account token, preventing it from authenticating with or attacking the Kubernetes API server. |
| **Identity Isolation** | `volumes.projected: No Tokens` | **Prevents Credential Bypass.** <br> Explicitly blocks the use of "Projected Volumes" to manually mount Service Account tokens, closing a loophole that would allow attackers to bypass the `automountServiceAccountToken: false` restriction. |
| **Filesystem Isolation** | `volumes.hostPath: Forbidden` | **Prevents Host Access.** <br> Strictly blocks mounting directories from the underlying node filesystem (e.g., `/var/run/docker.sock`), which is a common vector for container breakouts. |
| **Filesystem Hardening** | `procMount: Default` | **Protects /proc Filesystem.** <br> Prevents the use of `Unmasked` proc mounts, which would expose sensitive kernel information typically hidden by the container runtime. |
| **Kernel Hardening** | `sysctls: Forbidden` | **Prevents Kernel Tuning.** <br> Prohibits the modification of kernel parameters via `sysctls`, ensuring the shared kernel state remains consistent and secure. |
| **Privilege Escalation** | `privileged: false` | **Maintains Isolation Boundary.** <br> Blocks "privileged" containers, which would otherwise allow the workload to access host devices and bypass almost all security restrictions. |
| **Hardening** | `capabilities: drop ["ALL" \| "NET_RAW"]` | **Reduces Attack Surface.** <br> Forces the removal of dangerous Linux capabilities (like raw packet manipulation), implementing defense-in-depth even within the gVisor sandbox. |
| **Hardening** | `capabilities.add: []` | **Prevents Permission Creep.** <br> Prohibits adding *any* Linux capabilities (like `NET_ADMIN` or `SYS_PTRACE`), ensuring the container remains strictly unprivileged even if other settings are loose. |
| **DoS Protection** | `resources.limits` (CPU & Memory) | **Prevents Noisy Neighbors.** <br> Requires all containers to set resource limits, preventing a single compromised or buggy sandbox from starving the underlying node of resources. |
| **User Isolation** | `runAsNonRoot: true` | **Defense in Depth.** <br> Enforces that the container process cannot run as the root user (UID 0), adding an extra layer of protection against potential container breakouts. |
## Deployment

This policy requires **Kubernetes v1.30+** (Standard on GKE Autopilot).

### Defense in Depth
1. **Apply the Policy Definition**

This policy utilizes **CEL Variables** to merge `spec.containers`, `spec.initContainers` and `spec.ephemeralContainers` into a single validation stream. 

This ensures that security controls (like `privileged: false`, `runAsNonRoot: true`, and `capabilities.drop`) are enforced on **every** container in the pod, preventing attackers from using "Side Door" attacks where malicious configuration is hidden inside an Init Container.

```bash
   kubectl apply -f secure-sandbox-policy.yaml
```

2. **Bind the Policy to the Cluster:**
```bash
    kubectl apply -f secure-sandbox-binding.yaml
```

## Testing & Verification
To verify the policy is active, try creating a non-compliant sandbox.

1. Compliant Sandbox (Should Succeed):

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: secure-sandbox
spec:
  podTemplate:
    spec:
      runtimeClassName: gvisor
      hostNetwork: false
      automountServiceAccountToken: false

      nodeSelector:
        sandbox.gke.io/runtime: gvisor
      tolerations:
      - key: sandbox.gke.io/runtime
        operator: Equal
        value: gvisor
        effect: NoSchedule
      
      # 1. Init Containers (Must also be secure!)
      initContainers:
      - name: setup-data
        image: alpine:3.18
        command: ["/bin/sh", "-c", "echo 'initializing...'"]
        securityContext:
          runAsNonRoot: true
          runAsUser: 1000
          capabilities:
            drop: ["ALL"]
        resources:
          limits:
            cpu: "100m"
            memory: "64Mi"

      # 2. Main Containers
      containers:
      - name: main-agent
        image: python:3.11-slim
        command: ["python3", "-c", "import time; time.sleep(3600)"]
        securityContext:
          runAsNonRoot: true
          runAsUser: 1000
          # privileged: false       # Implied default, but policy enforces non-privileged
          capabilities:
            drop: ["ALL"]
        resources:
          limits:
            cpu: "500m"
            memory: "512Mi"

      # 3. Sidecar Containers (e.g., Log Shipper)
      - name: log-sidecar
        image: busybox:1.36
        command: ["/bin/sh", "-c", "tail -f /dev/null"]
        securityContext:
          runAsNonRoot: true
          runAsUser: 1000
          capabilities:
            drop: ["ALL"]
        resources:
          limits:
            cpu: "100m"
            memory: "128Mi"
```

2. Non-Compliant Sandbox (Should Fail because of missing `runtimeClassName`):

Since there's no `runtimeClassName` specified, the VAP will reject the creation of the Sandbox resource. 

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: insecure-sandbox
spec:
  podTemplate:
    spec:
      hostNetwork: false
      containers:
      - name: agent
        image: python:3.11-slim
```


Expected Error:

If you created a `Sandbox` resource directly: 

```
kubectl apply -f insecure-sandbox.yaml

The sandboxes "insecure-sandbox" is invalid: : ValidatingAdmissionPolicy 'secure-sandbox-policy' with binding 'secure-sandbox-binding' denied request: Security Violation: All Sandboxes must use 'runtimeClassName: gvisor'
```

Or if you created a `SandboxTemplate` + `SandboxClaim` you should see the error in the controller logs. 
```
2026-02-11T01:32:35Z    ERROR   Error creating sandbox for claim        {"controller": "sandboxclaim", "controllerGroup": "extensions.agents.x-k8s.io", "controllerKind": "SandboxClaim", "SandboxClaim": {"name":"egress-test-claim","namespace":"default"}, "namespace": "default", "name": "egress-test-claim", "reconcileID": "c46a1f97-1286-4b73-9de0-364d01dda8a6", "claimName": "egress-test-claim", "error": "sandbox create error: sandboxes.agents.x-k8s.io \"egress-test-claim\" is forbidden: ValidatingAdmissionPolicy 'secure-sandbox-policy' with binding 'secure-sandbox-binding' denied request: Security Violation: All Sandboxes must use 'runtimeClassName: gvisor'"}
```


3. Verify Init Container Protection
Attempt to create a Sandbox with a secure main container but a privileged init container.

**Manifest (`bad-init.yaml`):**
```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: side-door-attack
spec:
  podTemplate:
    spec:
      runtimeClassName: gvisor
      automountServiceAccountToken: false
      hostNetwork: false
      containers:
      - name: innocent-worker
        image: alpine
        securityContext: { runAsNonRoot: true, capabilities: { drop: ["ALL"] } }
        resources: { limits: { cpu: "100m", memory: "128Mi" } }
      initContainers:
      - name: evil-setup
        image: alpine
        securityContext: { privileged: true } # <--- MALICIOUS
  ```
Expected error: 

```
The sandboxes "side-door-attack" is invalid: 
* <nil>: Security Violation: Privileged containers are not allowed (checked in containers, initContainers, and ephemeralContainers).
```