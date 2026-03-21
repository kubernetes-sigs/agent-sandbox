# Enabling Kata Containers on GKE

## Overview

This example demonstrates how to run sandboxed agents with a stronger security boundary by using [Kata Containers](https://github.com/kata-containers/kata-containers) on a Google Kubernetes Engine (GKE) cluster.

By default, Agent Sandbox uses standard container runtimes that provide OS-level isolation where all sandboxes share the host node's kernel. This guide shows how to configure and use the Kata runtime to give each sandbox its own dedicated kernel, providing stronger, hardware-virtualized isolation. This is a common requirement for running highly sensitive or untrusted workloads.

## Prerequisites

1.  [Install](https://cloud.google.com/sdk/docs/install) and then [initialize](https://cloud.google.com/sdk/docs/initializing) the gcloud CLI
2.  Enable GKE API:
    ```shell
    gcloud services enable container.googleapis.com
    ```
3.  [Ensure that your organization policy supports creating nested VMs](https://cloud.google.com/compute/docs/instances/nested-virtualization/managing-constraint#check_whether_nested_virtualization_is_allowed).
4.  Review the nested VM [restrictions](https://cloud.google.com/compute/docs/instances/nested-virtualization/overview#restrictions) (as of Dec 2025). Kata requires specific hardware support that is not available on default GKE nodes.
    *   **Machine Type:** Must be **Intel N2** series (e.g., n2-standard-4).
        *   *Prohibited:* E2 (no nested virt), AMD (N2D - nested virt not supported by GKE yet), ARM (T2A).
    *   **OS Image:** Must be **Ubuntu** (UBUNTU_CONTAINERD).
        *   *Prohibited:* Container-Optimized OS (COS) is read-only and blocks the installer.
    *   **Region/Zone:** Must use a zone where N2 hardware is available (e.g., us-central1-a, us-west1-b).
5.  [Install kubectl](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_kubectl).

More details can be found in the [official doc](https://cloud.google.com/kubernetes-engine/docs/how-to/nested-virtualization#before_you_begin).

## Step 1: Run the Setup Script

For details on available `[OPTIONS...]`, please see the script itself.
```shell
./setup.sh [OPTIONS...]
```

## Step 2: Install the Agent Sandbox Controller

Before you can create a `Sandbox` resource, you must install the Agent Sandbox controller on your cluster following the [Installation Guide](../../README.md#installation).

## Step 3: Deploy an Agent Sandbox

With Kata installed on your GKE nodes, you can now deploy an `Agent Sandbox` resource that uses it.

The manifest below (`sandbox-kata-gke.yaml`) defines a `Sandbox` that requests the `kata-qemu` runtime and includes a `nodeSelector` to ensure it is scheduled onto a compatible Ubuntu node.

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: kata-gke-example
spec:
  podTemplate:
    spec:
      runtimeClassName: kata-qemu
      containers:
      - name: hello-kata
        image: busybox
        command: ["sh", "-c", "echo 'Hello from an Agent Sandbox running in Kata!' && sleep 3600"]
      nodeSelector:
        # Force this pod onto the Ubuntu/Intel pool where Kata is installed
        cloud.google.com/gke-os-distribution: ubuntu
```

Apply the manifest to create the Sandbox:
```shell
kubectl apply -f sandbox-kata-gke.yaml
```

## Step 4: Verify the Sandbox is Running

After applying the manifest, verify that the sandbox becomes `Ready` and that its controller successfully creates a pod.

1.  Wait for the `Sandbox` resource to report a `Ready` condition:
    ```shell
    kubectl wait --for=condition=Ready sandbox/kata-gke-example --timeout=5m
    ```

2.  Get the label selector from the sandbox's status and use it to find the pod:
    ```shell
    SELECTOR=$(kubectl get sandbox kata-gke-example -o jsonpath='{.status.selector}')
    kubectl describe pod -l $SELECTOR
    ```
    You should see the pod `STATUS` as `Running`.

## Step 5: Verify the Isolation

This test verifies the sandbox is using Kata's stronger, hardware-virtualized isolation. Unlike standard containers which share the host's kernel, Kata provides a dedicated kernel for the sandbox. A difference between the host and pod kernel versions proves this stronger isolation is active.

**1. Check the Host Node Kernel:**

```shell
kubectl get nodes -o wide
# Note the KERNEL-VERSION of your Ubuntu node (e.g., 6.8.0-1041-gke)
```

**2. Check the Sandbox Pod Kernel:**

First, get the selector from the sandbox status to find the pod name:
```shell
POD_NAME=$(kubectl get pod -l $SELECTOR -o jsonpath='{.items[0].metadata.name}')
```

Now, execute `uname -r` inside the sandbox pod:
```shell
kubectl exec -it $POD_NAME -- uname -r
```

* **Success**: The output is a different kernel version (e.g., `6.1.38`). This proves the agent is running inside its own VM with its own kernel, isolated from the host.
* **Failure**: The output is identical to the host node's kernel. This indicates the sandbox is not using the Kata runtime correctly.

# Troubleshooting

| Error                                           | Cause                                                                                  | Solution                                                                                       |
| :---------------------------------------------- | :------------------------------------------------------------------------------------- | :--------------------------------------------------------------------------------------------- |
| **Pod Stuck in ContainerCreating or CrashLoopBackOff** | The node does not support Nested Virtualization.                                         | Check machine type. Ensure you are using **N2 (Intel)**. Do not use E2 or AMD.                 |
| **404 Not Found during kubectl apply**          | The GitHub raw URLs in the main branch changed.                                        | Use the pinned 3.2.0 URLs provided in Step 2.                                                  |
| **no handler found error in Pod events**        | The RuntimeClass is missing or the node hasn't finished installing Kata.                 | Verify Step 3 was applied. Check kube-system pods are running.                               |

### Further Troubleshooting

For issues not covered in the table, the following resources may be helpful:

*   **Agent Sandbox Controller:** Check the controller's logs for errors related to the sandbox resource:
    ```shell
    kubectl logs statefulset/agent-sandbox-controller -n agent-sandbox-system
    ```
*   **General Pod Issues:** For problems with the pod itself (e.g., `ImagePullBackOff`, `CrashLoopBackOff`), use `kubectl describe pod <pod-name>`. See the official [Kubernetes guide to debugging pods](https://kubernetes.io/docs/tasks/debug/debug-pod-replication-controller/).
*   **Kata Containers:** For issues related to the Kata runtime itself, refer to the [Kata Containers troubleshooting guide](https://github.com/kata-containers/kata-containers/blob/main/docs/troubleshooting.md).
*   **GKE:** For cluster-level problems, consult the [GKE troubleshooting documentation](https://cloud.google.com/kubernetes-engine/docs/troubleshooting).
