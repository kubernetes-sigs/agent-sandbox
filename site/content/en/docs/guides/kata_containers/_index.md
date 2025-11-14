---
title: "Kata Containers"
linkTitle: "Kata Containers runtime"
weight: 2
description: >
  This guide shows how to run [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) with the [Kata Containers](https://katacontainers.io/) runtime using minikube as a cluster.
---

> [!NOTE]
> This guide is only tested on hardware with Linux operating system and KVM.

## Prerequisites

* Host machine that supports nested virtualization

   You can verify that by running:

   ```sh
   cat /sys/module/kvm_intel/parameters/nested
   ```

   The output must be “Y”.

* [minikube](https://minikube.sigs.k8s.io/docs/start/?arch=%2Flinux%2Fx86-64%2Fstable%2Fbinary+download)
* [kubectl](https://kubernetes.io/docs/tasks/tools/)

## Create minikube cluster

> [!NOTE]
> At this moment, we use only `containerd` runtime, since it works without additional adjustments.

```sh
minikube start --vm-driver kvm2 --memory 8192  --container-runtime=containerd --bootstrapper=kubeadm
```

## Install Kata Containers

In order to install Kata Containers we use the [kata-deploy helm chart](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy/helm-chart)

1. Install the helm chart:

   ```sh
   helm install kata-deploy \
     --namespace kube-system \
     --version  "3.22.0" \
     "oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
   ```

2. Wait until its daemonset is ready:

   ```sh
   kubectl -n kube-system rollout status daemonset/kata-deploy
   ```

3. Verify that new runtime classes are available:

   ```sh
   kubectl get runtimeClasses
   ```

   The output should be similar to this. Make sure it has `kata-qemu` runtime since it will be used in this guide:

   ```log
   $ kubectl get runtimeClasses
   NAME                       HANDLER                    AGE
   kata-clh                   kata-clh                   118s
   kata-cloud-hypervisor      kata-cloud-hypervisor      118s
   kata-dragonball            kata-dragonball            118s
   kata-fc                    kata-fc                    117s
   kata-qemu                  kata-qemu                  117s
   kata-qemu-cca              kata-qemu-cca              117s
   kata-qemu-coco-dev         kata-qemu-coco-dev         117s
   kata-qemu-nvidia-gpu       kata-qemu-nvidia-gpu       117s
   kata-qemu-nvidia-gpu-snp   kata-qemu-nvidia-gpu-snp   117s
   kata-qemu-nvidia-gpu-tdx   kata-qemu-nvidia-gpu-tdx   117s
   kata-qemu-runtime-rs       kata-qemu-runtime-rs       117s
   kata-qemu-se-runtime-rs    kata-qemu-se-runtime-rs    117s
   kata-qemu-snp              kata-qemu-snp              117s
   kata-qemu-tdx              kata-qemu-tdx              117s
   kata-stratovirt            kata-stratovirt            117s
   ```

## Install Agent Sandbox Controller

1. Apply the Agent Sandbox manifest:

   ```sh
   kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.1.0-rc.1/manifest.yaml

   ```

2. Wait until the controller’s daemonset in ready:

   ```sh
   kubectl -n agent-sandbox-system rollout status statefulset/agent-sandbox-controller
   ```

## Create a test sandbox

In order to verify that everything works, we install the simplest example from the agent-sadbox repository.

1. Clone the `agent-sandbox` repository if needed:

   ```sh
   git clone https://github.com/kubernetes-sigs/agent-sandbox.git
   ```

2. Move to the repository folder:

   ```sh
   cd agent-sandbox
   ```

3. Create a namespace for a sandbox:

   ```sh
   kubectl apply -f examples/sandbox-ns.yaml
   ```

4. Create a service account for a sandbox:

   ```sh
   kubectl apply -f examples/sandbox-sa.yaml
   ```

5. Before creating a sandbox, change the runtime class for Kata Containers in the sandbox’s manifest:

   Open `examples/sandbox.yaml` manifest file and add the field `runtimeClassName` with value `kata-qemu` in the path `spec.podTemplate.spec`.

   ```yaml
   apiVersion: agents.x-k8s.io/v1alpha1
   kind: Sandbox
   metadata:
     name: sandbox-example
     namespace: sandbox-ns
   spec:
     podTemplate:
       metadata:
       ...
       spec:
         runtimeClassName: kata-qemu # <----- Add this
         ...
     ...
   ...
   ```

6. Create a sandbox:

   ```sh
   kubectl apply -f examples/sandbox.yaml
   ```

7. Wait until sandbox is successfully created:

   ```sh
   kubectl -n sandbox-ns wait --for=condition=Ready sandbox/sandbox-example
   ```

8. Additionally, verify that example sandbox’s pod exists and running:

   ```sh
   kubectl -n sandbox-ns get pods
   ```

   The output should be similar to:

   ```
   NAME              READY   STATUS    RESTARTS   AGE
   sandbox-example   1/1     Running   0          15s
   ```

9. Describe the pod and verify that is has desired runtime class:

   ```sh
   kubectl -n sandbox-ns describe sandbox/sandbox-example
   ```

   The output should contain the `Runtime Class Name` filed

   ```yaml
   Name:         sandbox-example
   Namespace:    sandbox-ns
   ...
   Spec:
     Pod Template:
       Spec:
         ...
         Runtime Class Name:    kata-qemu <---
         ...
   ...
   ```

10. As an alternative, we can also compare kernel versions between the created sandbox container and a node:

   > [!NOTE]
   > There may be a chance that the kernel version of the Kata Containers matches the kernel version of the node, however, we assume this is usually not the case.

   1. Get kernel version of the node that hosts the sandbox’s:

      ```sh
      kubectl debug -q node/minikube --image=ubuntu -it -- uname -r
      ```

   2. Get kernel version of the example sandbox’s container itself:

      ```sh
      kubectl -n sandbox-ns exec -it sandbox-example -- uname -r
      ```

   The output of two commands should be similar to this:

   ```log
   $ kubectl debug -q node/minikube --image=ubuntu -it -- uname -r
   5.10.207

   $ kubectl -n sandbox-ns exec -it sandbox-example -- uname -r
   6.12.47
   ```

   As shown, the kernel version of the sandbox container (the latter) is different, which should indicate that the sandbox's container does not use the kernel of the host (as the “traditional” runtimes would do), but uses a completely independent kernel which is backed by an underlying virtual machine.

## Cleanup

```sh
minikube delete
```
