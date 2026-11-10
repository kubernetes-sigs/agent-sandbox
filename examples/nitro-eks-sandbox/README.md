# AWS Nitro Sandbox on Amazon EKS

## Overview

This example runs VS Code in the browser ([code-server](https://github.com/coder/code-server)) inside an Agent Sandbox that is isolated by an [AWS Nitro Sandbox](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.html) on an [Amazon EKS Auto Mode](https://docs.aws.amazon.com/eks/latest/userguide/automode.html) cluster, and opens it with `kubectl port-forward`.

A pod that sets `runtimeClassName: isolated-sandbox` runs in its own Nitro Sandbox: a lightweight VM with its own kernel and hardware-level isolation of its CPU and memory, created by the Nitro Hypervisor. The pod's containers, image and spec are unchanged, and the kubelet still sees an ordinary pod, so `kubectl exec`, `logs` and `port-forward` work as they do for any other pod.

EKS Auto Mode provides everything else. A new Auto Mode cluster comes with the `isolated-sandbox` RuntimeClass, and launches a node for a sandboxed pod when one is needed. No node pool, DaemonSet installer or RuntimeClass registration is needed.

## Prerequisites

1. [Install](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) the AWS CLI and configure it for your account.
2. [Install eksctl](https://eksctl.io/installation/) and [kubectl](https://kubernetes.io/docs/tasks/tools/).
3. Review the Nitro Sandbox requirements in the [Amazon EKS documentation](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.html).

## Step 1: Create an EKS Auto Mode cluster

> **Note:** this command creates billable AWS resources: an EKS cluster, its VPC, and the EC2 instances Auto Mode launches for your pods. See [Cleanup](#cleanup) to remove them.

AWS Nitro Sandbox is supported on Amazon EKS 1.36 or higher. Check the [Amazon EKS documentation](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.html) for the supported Regions, then replace `<REGION>` and `<EKS VERSION>` below:

```shell
eksctl create cluster --name nitro-sandbox-demo --region <REGION> --version <EKS VERSION> --enable-auto-mode
```

Check that the cluster has the Nitro Sandbox runtime classes:

```shell
kubectl get runtimeclass
```

```text
NAME                   HANDLER                AGE
confidential-sandbox   confidential-sandbox   4d11h
isolated-sandbox       isolated-sandbox       4d11h
```

This example uses `isolated-sandbox`. `confidential-sandbox` is described in the [Amazon EKS documentation](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.html).

## Step 2: Install the Agent Sandbox Controller

Install the Agent Sandbox controller following the [Installation Guide](../../README.md#installation), then wait for it to be ready:

```shell
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller
```

## Step 3: Deploy VS Code in an Agent Sandbox

The manifest below (`sandbox-nitro-eks.yaml`) defines a `Sandbox` that runs code-server and requests the `isolated-sandbox` runtime. That one field is the only Nitro-specific line.

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: vscode-nitro
spec:
  podTemplate:
    spec:
      runtimeClassName: isolated-sandbox
      containers:
      - name: code-server
        image: codercom/code-server:4.145.2
        securityContext:
          runAsNonRoot: true
        ports:
        - containerPort: 8080
        resources:
          requests:
            cpu: "1"
            memory: 2Gi
```

Apply it and wait for the sandbox to be ready:

```shell
kubectl apply -f sandbox-nitro-eks.yaml
kubectl wait --for=condition=Ready sandbox/vscode-nitro --timeout=5m
kubectl get sandbox vscode-nitro
```

```text
NAME           READY   REASON              AGE
vscode-nitro   True    DependenciesReady   25s
```

## Step 4: Verify the Isolation

Compare the kernel inside the sandbox with the kernel of the node it runs on:

```shell
# Kernel inside the sandbox (the Nitro Sandbox's own kernel)
kubectl exec vscode-nitro -- uname -r

# Kernel on the node hosting the sandbox
SANDBOX_NODE="$(kubectl get pod vscode-nitro -o jsonpath='{.spec.nodeName}')"
kubectl get node "${SANDBOX_NODE}" -o jsonpath='{.status.nodeInfo.kernelVersion}'
```

```text
6.18.54
6.18.56
```

The two versions differ because the sandbox boots its own kernel. Two more signs that the pod runs in its own VM: the kernel's uptime matches the pod's age rather than the node's, and the VM reports its own product name.

```shell
kubectl exec vscode-nitro -- cat /proc/uptime
kubectl exec vscode-nitro -- cat /sys/class/dmi/id/product_name
```

```text
45.59 43.62
m8a.enclave
```

## Step 5: Open VS Code

Forward a local port to the sandbox's pod. The pod has the same name as the `Sandbox`:

```shell
kubectl port-forward pod/vscode-nitro 8080:8080
```

In another terminal, read the password that code-server generated:

```shell
kubectl exec vscode-nitro -- cat /home/coder/.config/code-server/config.yaml
```

Open http://localhost:8080 in your browser, sign in with the password, and you have VS Code with a terminal, running in the sandbox. If port 8080 is already in use on your machine, pick another local port, for example `kubectl port-forward pod/vscode-nitro 18080:8080`.

The editor is not published outside the cluster: there is no Service of type LoadBalancer, no NodePort and no Ingress. Inside the cluster, code-server listens on `0.0.0.0:8080`, so any other pod in the cluster can reach the editor by the pod's IP address; the password sign-in still applies.

## Cleanup

Stop the port-forward with `Ctrl+C`, then delete the sandbox:

```shell
kubectl delete -f sandbox-nitro-eks.yaml
```

To remove the demo cluster entirely:

```shell
eksctl delete cluster --name nitro-sandbox-demo --region <REGION>
```

## Learn more

- [Amazon EKS documentation for Nitro Sandboxes](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.html)
- [AWS Nitro Sandbox Blueprints](https://aws-samples.github.io/sample-eks-nitro-sandbox): more Agent Sandbox examples on Amazon EKS, including warm pools, coding agents, storage and EKS Pod Identity.
