# gVisor on KIND — Isolation Setup

This guide sets up a KIND cluster with gVisor runtime isolation. After completing these steps, return to the [main quickstart guide](README.md) at **Step 3** to finish the setup.

## Additional Prerequisites

In addition to the [main prerequisites](README.md#prerequisites), you need:

- Linux host (gVisor requires Linux)

## Step 1: Install gVisor Runtime

Follow the [official gVisor installation guide](https://gvisor.dev/docs/user_guide/install/) to install the `runsc` runtime and `containerd-shim-runsc-v1` on your host machine.

## Step 2: Create KIND Cluster with gVisor

### 2.1 Create Cluster Configuration

```bash
cat <<EOF > kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
    runtime_type = "io.containerd.runsc.v1"
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /usr/local/bin/runsc
    containerPath: /usr/local/bin/runsc
  - hostPath: /usr/local/bin/containerd-shim-runsc-v1
    containerPath: /usr/local/bin/containerd-shim-runsc-v1
EOF
```

**Note:** `io.containerd.runsc.v1` implements the containerd shim v2 protocol. The "v1" refers to gVisor's shim implementation version, not the protocol version.

### 2.2 Create the Cluster

```bash
kind create cluster --name agent-sandbox-demo --config kind-config.yaml
```

### 2.3 Create RuntimeClass for gVisor

```bash
kubectl apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
```

**Next:** Return to the [main quickstart guide — Step 3](README.md#step-3-install-agent-sandbox-controller) to continue setup. When you reach Step 4 (Apply SandboxTemplate), uncomment the `runtimeClassName: gvisor` line in `python-sandbox-template.yaml` before applying.

## Appendix: gVisor-Specific Validation

After completing the full setup from the main guide (including [Step 5 — SandboxWarmPool](README.md#step-5-create-sandboxwarmpool)), you can run these additional checks to verify gVisor isolation:

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

## References

- [gVisor Documentation](https://gvisor.dev/)
- [KIND Documentation](https://kind.sigs.k8s.io/)
