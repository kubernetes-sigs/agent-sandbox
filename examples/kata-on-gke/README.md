# Enabling Kata Containers on GKE

## Overview

This guide details how to deploy [Kata Containers](https://github.com/kata-containers/kata-containers) on a GKE cluster using a custom DaemonSet.

## Prerequisites

Official doc: [https://cloud.google.com/kubernetes-engine/docs/how-to/nested-virtualization#before_you_begin](https://cloud.google.com/kubernetes-engine/docs/how-to/nested-virtualization#before_you_begin)

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

## Running the Setup Script

```shell
./setup.sh [OPTIONS...]
```

## Validation (The "Truth Test")

To verify isolation, we deploy a pod and confirm it is running on a different kernel than the GKE host.

**1. Check the Host Kernel:**

```shell
kubectl get nodes -o wide
# Note the KERNEL-VERSION (e.g., 5.15.0-1040-gcp)
```

**2. Deploy a Test Pod:**

Create `test-kata.yaml`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: kata-test-pod
spec:
  runtimeClassName: kata-qemu
  containers:
    - name: nginx
      image: nginx:latest
  nodeSelector:
    # Force this pod onto the Ubuntu/Intel pool
    cloud.google.com/gke-os-distribution: ubuntu
```

Apply it:

```shell
kubectl apply -f test-kata.yaml
```

**3. Verify Isolation:**

Run this command to check the kernel inside the pod:

```shell
kubectl exec -it kata-test-pod -- uname -r
```

* **Success**: The output is a different kernel version (often older or containing kata).
* **Failure**: The output is identical to the host node kernel.

# Troubleshooting

| Error                                           | Cause                                                                                  | Solution                                                                                       |
| :---------------------------------------------- | :------------------------------------------------------------------------------------- | :--------------------------------------------------------------------------------------------- |
| **Pod Stuck in ContainerCreating or CrashLoopBackOff** | The node does not support Nested Virtualization.                                         | Check machine type. Ensure you are using **N2 (Intel)**. Do not use E2 or AMD.                 |
| **404 Not Found during kubectl apply**          | The GitHub raw URLs in the main branch changed.                                        | Use the pinned 3.2.0 URLs provided in Step 2.                                                  |
| **no handler found error in Pod events**        | The RuntimeClass is missing or the node hasn't finished installing Kata.                 | Verify Step 3 was applied. Check kube-system pods are running.                               |
