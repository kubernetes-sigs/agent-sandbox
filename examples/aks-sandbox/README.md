# Agent Sandbox on Azure Kubernetes Service (AKS)

## Overview

This example demonstrates how to deploy Agent Sandbox on [Azure Kubernetes Service (AKS)](https://learn.microsoft.com/azure/aks/). It includes an automated setup script to create an AKS cluster (with an optional spot node pool for cost savings) and several Sandbox manifests showcasing AKS-specific features.

## Prerequisites

1. **Azure CLI:** [Install](https://learn.microsoft.com/cli/azure/install-azure-cli) and sign in with `az login`.
2. **kubectl:** [Install kubectl](https://kubernetes.io/docs/tasks/tools/).
3. **An Azure subscription** with permissions to create resource groups and AKS clusters. See the [AKS quickstart prerequisites](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-cli#prerequisites) for details.

## Files

| File | Description |
|------|-------------|
| `setup.sh` | Creates an AKS cluster and optional spot node pool |
| `sandbox.yaml` | Basic Sandbox on on-demand nodes |
| `sandbox-spot.yaml` | Sandbox scheduled onto an AKS spot node pool |
| `sandbox-with-storage.yaml` | Sandbox with persistent storage via Azure Disk |

## Step 1: Run the Setup Script

The script creates a resource group, an AKS cluster, and optionally a spot node pool. Run `./setup.sh --help` to see all available options.

```shell
./setup.sh [OPTIONS...]
```

Default configuration:
- **Resource group:** `agent-sandbox-rg`
- **Location:** `eastus`
- **Cluster name:** `agent-sandbox-cluster`
- **VM size:** `Standard_D4s_v3`
- **Spot pool:** disabled (pass `--enable-spot` to create one)

## Step 2: Install the Agent Sandbox Controller

Before creating Sandbox resources, install the Agent Sandbox controller on your cluster following the [Installation Guide](../../README.md#installation).

## Step 3: Deploy a Sandbox

**Basic sandbox (on-demand nodes):**

The `sandbox.yaml` manifest creates a minimal Sandbox:

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: aks-sandbox
spec:
  podTemplate:
    spec:
      containers:
      - name: my-sandbox
        image: busybox:1.37
        command: ["sh", "-c", "echo 'Hello from Agent Sandbox on AKS!' && sleep 3600"]
```

```shell
kubectl apply -f sandbox.yaml
```

**Sandbox on a spot node pool** (requires `--enable-spot` during setup):

```shell
kubectl apply -f sandbox-spot.yaml
```

**Sandbox with persistent storage** (Azure Managed Disk):

```shell
kubectl apply -f sandbox-with-storage.yaml
```

## Step 4: Verify the Sandbox is Running

1. Wait for the Sandbox to become `Ready`:
   ```shell
   kubectl wait --for=condition=Ready sandbox/aks-sandbox --timeout=5m
   ```

2. Get the pod created by the Sandbox and check its status:
   ```shell
   SELECTOR=$(kubectl get sandbox aks-sandbox -o jsonpath='{.status.selector}')
   kubectl get pod -l $SELECTOR
   ```

3. Check the container logs:
   ```shell
   POD_NAME=$(kubectl get pod -l $SELECTOR -o jsonpath='{.items[0].metadata.name}')
   kubectl logs $POD_NAME -c my-sandbox
   ```
   You should see: `Hello from Agent Sandbox on AKS!`

## Cleanup

```shell
# Delete sandbox resources
kubectl delete sandbox --all

# Delete the AKS cluster and resource group
az group delete --name agent-sandbox-rg --yes --no-wait
```

## Notes

- **VM sizes:** The default VM size (`Standard_D4s_v3`) may not be available in all subscriptions or regions. If cluster creation fails with a `BadRequest` error, the Azure CLI will list the available sizes. Use `--vm-size` to override, e.g. `./setup.sh --vm-size Standard_D2s_v3`.
- **Storage:** The `sandbox-with-storage.yaml` example uses the `managed-csi` StorageClass, which provisions [Azure Managed Disks](https://learn.microsoft.com/azure/aks/azure-disk-csi) and is the default on AKS. For shared storage across sandboxes, consider [Azure Files](https://learn.microsoft.com/azure/aks/azure-files-csi).
- **Spot eviction:** Azure spot VMs can be evicted when capacity is needed. Use persistent storage to preserve state across evictions. For more details, see the [AKS spot node pool documentation](https://learn.microsoft.com/azure/aks/spot-node-pool).
- **Networking:** AKS supports both Azure CNI and kubenet. No special networking configuration is required for Agent Sandbox.

## Troubleshooting

| Error | Cause | Solution |
|:------|:------|:---------|
| `BadRequest: The VM size of ... is not allowed` | The default VM size is not available in your subscription or region | Use `--vm-size` to specify an available size from the error message |
| Pod stuck in `Pending` with spot YAML | No spot capacity available or spot pool not created | Verify the spot pool exists with `az aks nodepool list -g agent-sandbox-rg --cluster-name agent-sandbox-cluster -o table` |
| PVC stuck in `Pending` | StorageClass `managed-csi` not available | Check available StorageClasses with `kubectl get sc` and update the YAML |

For general Agent Sandbox issues, check the controller logs:
```shell
kubectl logs deployment/agent-sandbox-controller -n agent-sandbox-system
```
