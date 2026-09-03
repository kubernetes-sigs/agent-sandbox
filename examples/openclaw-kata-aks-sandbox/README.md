# OpenClaw on Kata Containers (AKS)

## Overview

This example runs [OpenClaw](https://github.com/openclaw/openclaw) — a headless agent gateway with a web Control UI — inside an Agent Sandbox that is isolated by [Kata Containers](https://github.com/kata-containers/kata-containers) on Azure Kubernetes Service (AKS).

It is the hardware-virtualized counterpart to the [gVisor-isolated OpenClaw example](../openclaw-gvisor-sandbox): where that one uses a syscall-interception sandbox and the template/warm-pool/claim pattern, this one uses a single `Sandbox` running in its own lightweight VM with a dedicated guest kernel, via the AKS [Pod Sandboxing](https://learn.microsoft.com/azure/aks/use-pod-sandboxing) feature.

Isolating an agent runtime this way is useful when the agent executes untrusted, model-generated code: the blast radius of a container escape is the pod VM, not the node kernel.

## Prerequisites

1. An AKS cluster with a Pod Sandboxing (Kata) node pool, and the `kata-vm-isolation` RuntimeClass available. The [kata-aks-sandbox example](../kata-aks-sandbox) provides a `setup.sh` that creates one; see also the [AKS Pod Sandboxing documentation](https://learn.microsoft.com/azure/aks/use-pod-sandboxing).
2. The Agent Sandbox controller installed on the cluster ([Installation Guide](../../README.md#installation)).
3. A default StorageClass for the workspace `PersistentVolumeClaim` (AKS provides one).

## Step 1: Deploy the Sandbox

```shell
kubectl apply -f openclaw-config.yaml
kubectl apply -f openclaw-sandbox-kata-aks.yaml
```

Wait for it to become ready:

```shell
kubectl wait --for=condition=Ready sandbox/openclaw-kata-aks-sandbox --timeout=5m
kubectl get pod openclaw-kata-aks-sandbox
```

The gateway takes a few seconds after the pod is Running before it starts listening; there is no readiness probe gating it.

## Step 2: Reach the Control UI

```shell
kubectl port-forward openclaw-kata-aks-sandbox 18789:18789
```

Then open `http://localhost:18789/#token=dummy-token-for-sandbox`. The token fragment is required — the gateway rejects a bare load. The token is set by `OPENCLAW_GATEWAY_TOKEN` in the manifest; change it for anything beyond a local demo.

## Step 3: Add a provider API key (optional)

The sandbox starts with `--allow-unconfigured`, so the gateway runs without any model provider. To let the agent actually answer, create the Secret:

```shell
kubectl create secret generic openclaw-provider-keys \
  --from-literal=ANTHROPIC_API_KEY="sk-ant-..."
```

Then uncomment the `ANTHROPIC_API_KEY` block in `openclaw-sandbox-kata-aks.yaml` and re-apply it. Run an agent turn from inside the sandbox:

```shell
kubectl exec openclaw-kata-aks-sandbox -- \
  openclaw agent --local --session-id demo -m "Say hi in three words"
```

Running the turn in-guest keeps it on the gateway's loopback interface, which avoids the device-pairing flow a remote client would need.

## Step 4: Verify the isolation

```shell
# Kernel inside the sandbox (the Kata pod VM's guest kernel)
kubectl exec openclaw-kata-aks-sandbox -- uname -r

# Kernel on the node hosting the sandbox
SANDBOX_NODE="$(kubectl get pod openclaw-kata-aks-sandbox -o jsonpath='{.spec.nodeName}')"
kubectl get node "${SANDBOX_NODE}" -o jsonpath='{.status.nodeInfo.kernelVersion}'
```

The two differ: the agent runtime is running in its own VM, not sharing the host kernel.

## Notes on sizing

The pod VM is sized from the container's resource limits. The Node.js gateway needs considerably more memory than the default pod VM size, and an embedded agent turn (`--local`) needs more still — with too little guest memory the gateway is OOM-killed inside the VM. The manifest requests 2Gi / limits 4Gi, which leaves room for both. See the AKS [Pod Sandboxing resource management notes](https://learn.microsoft.com/azure/aks/considerations-pod-sandboxing#resource-management) for how pod VM sizing and RuntimeClass overhead interact.

## Cleanup

```shell
kubectl delete -f openclaw-sandbox-kata-aks.yaml
kubectl delete -f openclaw-config.yaml
kubectl delete secret openclaw-provider-keys --ignore-not-found
```

Deleting the Sandbox also deletes the workspace `PersistentVolumeClaim` (the PVC is owned by the Sandbox), taking the agent's state with it.
