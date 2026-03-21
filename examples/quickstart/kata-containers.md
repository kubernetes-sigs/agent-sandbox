# Kata Containers on minikube — Isolation Setup

This guide sets up a minikube cluster with Kata Containers VM-based isolation. After completing these steps, return to the [main quickstart guide](README.md) at **Step 3** to finish the setup.

## Additional Prerequisites

In addition to the [main prerequisites](README.md#prerequisites), you need:

- KVM/QEMU virtualization support
- [minikube](https://minikube.sigs.k8s.io/) (1.32+)
- Helm
- **Minimum 8GB RAM** (Kata VMs require significant memory overhead)
- **Minimum 4 CPU cores** recommended
- **20GB free disk space** for images and VMs

> **Note:** Kata Containers uses minikube instead of KIND. This affects a few steps in the main guide — see the callout at the bottom of this page.

## Step 1: Start minikube with Containerd

```bash
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

Follow the [official Kata Containers Helm chart README](https://github.com/kata-containers/kata-containers/blob/main/tools/packaging/kata-deploy/helm-chart/README.md) to install Kata Containers into the `kube-system` namespace, then continue below.

```bash
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

**Next:** Return to the [main quickstart guide — Step 3](README.md#step-3-install-agent-sandbox-controller) to continue setup. Remember to:

- Uncomment the `runtimeClassName: kata-qemu` line in `python-sandbox-template.yaml` before applying in Step 4
- Use `minikube image load` instead of `kind load` when loading the router image in Step 7
- Use `minikube delete -p agent-sandbox-kata` instead of `kind delete cluster` for cleanup in Step 10

## Appendix: Kata-Specific Validation

After completing the full setup from the main guide (including [Step 5 — SandboxWarmPool](README.md#step-5-create-sandboxwarmpool)), you can run these additional checks to verify Kata isolation:

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

## References

- [Kata Containers Documentation](https://katacontainers.io/)
- [minikube Documentation](https://minikube.sigs.k8s.io/)
