# Enabling Kata Containers on AKS

## Overview

This example demonstrates how to run sandboxed agents with a stronger security boundary by using [Kata Containers](https://github.com/kata-containers/kata-containers) on an Azure Kubernetes Service (AKS) cluster.

By default, Agent Sandbox uses standard container runtimes that provide OS-level isolation where all sandboxes share the host node's kernel. AKS offers [Pod Sandboxing](https://learn.microsoft.com/azure/aks/use-pod-sandboxing) as a built-in feature: each sandboxed pod runs in its own lightweight VM with a dedicated guest kernel, on a stack composed of the [Azure Linux container host](https://learn.microsoft.com/azure/aks/use-azure-linux), the Microsoft Hyper-V hypervisor, the [Cloud Hypervisor](https://www.cloudhypervisor.org) VMM, and the Kata Containers runtime.

Unlike environments where Kata must be installed manually (see the [GKE example](../kata-gke-sandbox)), AKS provisions everything when a node pool is created with the Kata workload runtime: the nodes come with Kata pre-installed, and the cluster ships a ready-to-use `kata-vm-isolation` RuntimeClass whose scheduling selector automatically places Kata pods onto those nodes. No DaemonSet installer and no manual RuntimeClass registration are needed.

## Prerequisites

1. [Install](https://learn.microsoft.com/cli/azure/install-azure-cli) the Azure CLI and sign in with `az login`.
2. [Install kubectl](https://kubernetes.io/docs/tasks/tools/).
3. Review the [Pod Sandboxing prerequisites and limitations](https://learn.microsoft.com/azure/aks/use-pod-sandboxing) in the official AKS documentation. In short (as of July 2026): the Kata node pool must use `--os-sku AzureLinux`, a [generation 2 VM size that supports nested virtualization](https://learn.microsoft.com/azure/virtual-machines/generation-2) (for example, the Dsv5 series), and — for existing clusters — Kubernetes 1.27.0 or higher.

## Step 1: Run the Setup Script

> **Note:** this script creates billable Azure resources: a resource group, an AKS cluster with Pod Sandboxing enabled (or, with `--reuse-cluster`, a new Kata node pool on your existing cluster). It then fetches cluster credentials and verifies that the AKS-provided `kata-vm-isolation` RuntimeClass is present. See the [Cleanup](#cleanup) section to remove everything.

For details on available `[OPTIONS...]`, please see the script itself.

```shell
./setup.sh [OPTIONS...]
```

## Step 2: Install the Agent Sandbox Controller

Before you can create a `Sandbox` resource, you must install the Agent Sandbox controller on your cluster following the [Installation Guide](../../README.md#installation).

## Step 3: Deploy an Agent Sandbox

With the Kata node pool ready, deploy an Agent Sandbox that uses it.

The manifest below (`sandbox-kata-aks.yaml`) defines a `Sandbox` that requests the AKS-provided `kata-vm-isolation` runtime. The RuntimeClass carries its own scheduling `nodeSelector`, so the pod lands on the Kata node pool without any explicit selector in the manifest.

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: kata-aks-example
spec:
  podTemplate:
    spec:
      runtimeClassName: kata-vm-isolation
      containers:
      - name: hello-kata
        image: busybox:1.37
        command: ["sh", "-c", "echo 'Hello from an Agent Sandbox running in Kata on AKS!' && sleep 3600"]
```

Apply it:

```shell
kubectl apply -f sandbox-kata-aks.yaml
```

## Step 4: Verify the Isolation

Check that the sandbox is ready and its pod is running:

```shell
kubectl get sandbox kata-aks-example
kubectl get pod kata-aks-example
kubectl logs kata-aks-example
```

You should see:

```text
Hello from an Agent Sandbox running in Kata on AKS!
```

Confirm the workload runs under its own guest kernel rather than the node's kernel:

```shell
# Kernel inside the sandbox (the Kata pod VM's guest kernel)
kubectl exec kata-aks-example -- uname -r

# Kernel on the node hosting the sandbox
SANDBOX_NODE="$(kubectl get pod kata-aks-example -o jsonpath='{.spec.nodeName}')"
kubectl get node "${SANDBOX_NODE}" -o jsonpath='{.status.nodeInfo.kernelVersion}'
```

The two versions differ: the sandboxed workload is running inside its own VM with a dedicated kernel, isolated from the host by the hypervisor boundary.

Note on resources: the `kata-vm-isolation` RuntimeClass declares a fixed pod overhead for the VM and host-side components. For finer control, AKS supports [custom runtime classes with tuned overheads](https://learn.microsoft.com/azure/aks/considerations-pod-sandboxing#resource-management) on the same `kata` handler.

## Cleanup

```shell
kubectl delete -f sandbox-kata-aks.yaml
```

To remove the Kata node pool or the demo cluster entirely:

```shell
# Node pool only (keeps the cluster)
az aks nodepool delete --cluster-name <CLUSTER_NAME> --resource-group <RESOURCE_GROUP> --name <KATA_NODEPOOL_NAME>

# Whole demo cluster and its resource group
az aks delete --name <CLUSTER_NAME> --resource-group <RESOURCE_GROUP>
az group delete --name <RESOURCE_GROUP>
```
