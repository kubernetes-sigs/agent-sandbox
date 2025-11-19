
## Create a Sandbox with VSCode and Gemini CLI

Apply the sandbox manifest with PVC

```
kubectl apply -k base
```

They can then check the status of the applied resource.
Verify sandbox and pod are running:

```
kubectl get sandbox
kubectl get pod sandbox-example

kubectl wait --for=condition=Ready sandbox sandbox-example
```

### Harden Agent Sandbox isolation using gVisor (Optional)

The `Sandbox` API provides lifecycle features that are useful for managing long running
sandbox workloads on kubernetes. In real world scenarios, you may want to also
provide workload isolation for running untrusted workloads inside a sandbox.

[gVisor](https://gvisor.dev/docs/) provides a virtualization layer between
applications and the host operating system that creates a strong layer of
isolation. It implements the kernel in userspace and minimizes the risk of a
workload gaining access to the host machine.

This example demonstrates how to use `Sandbox` along with gVisor in order
to utilize the lifecycle features of `Sandbox` in addition with the workload
isolation features of gVisor.

#### Create a cluster with gVisor enabled

First, enable gVisor on your Kubernetes cluster. For examples of how to enable
gVisor, see the [gVisor documentation](https://gvisor.dev/docs/user_guide/quick_start/kubernetes/).

#### Create a Sandbox using the gVisor runtimeClassName

Apply the kustomize overlay to inject `runtimeClassName: gvisor` to the
`vscode-sandbox` example and apply it to the cluster:

```shell
kubectl apply -k overlays/gvisor
```

Validate that the `Pod` with gVisor enabled is running:

```shell
$ kubectl wait --for=condition=Ready sandbox sandbox-example
$ kubectl get pods -o jsonpath=$'{range .items[*]}{.metadata.name}: {.spec.runtimeClassName}\n{end}'
```
### Harden Agent Sandbox isolation using Kata Containers (Optional)

#### Prerequisites

* Host machine that supports nested virtualization

   You can verify that by running:

   ```sh
   cat /sys/module/kvm_intel/parameters/nested
   ```
   In case of AMD platform replace `kvm_intel` with `kvm_amd`
   The output must be “Y” or 1.

* [minikube](https://minikube.sigs.k8s.io/docs/start/?arch=%2Flinux%2Fx86-64%2Fstable%2Fbinary+download)
* [kubectl](https://kubernetes.io/docs/tasks/tools/)

#### Create minikube cluster

> Note:
> At this moment, we use only `containerd` runtime, since it works without additional adjustments.

```sh
minikube start --vm-driver kvm2 --memory 8192  --container-runtime=containerd --bootstrapper=kubeadm
```

#### Install Kata Containers

Follow the instructions provided at [Kata Containers Installation Guide](https://github.com/kata-containers/kata-containers/tree/main/docs/install)

#### Create a Sandbox using the kata-qemu runtimeClassName

Apply the kustomize overlay to inject `runtimeClassName: kata-qemu` to the
`vscode-sandbox` example and apply it to the cluster:

```shell
kubectl apply -k overlays/kata
```

Validate that the `Pod` with Kata container enabled is running:

```shell
$ kubectl wait --for=condition=Ready sandbox sandbox-example
$ kubectl get pods -o jsonpath=$'{range .items[*]}{.metadata.name}: {.spec.runtimeClassName}\n{end}'
```

## Accessing VSCode

To access the VSCode instance securely and reliably, use the Sandbox Router.

### Option 1: Production (via Gateway)

If you have deployed the Gateway **and** Router (see `clients/python/agentic-sandbox-client/sandbox_router`), access VSCode via the external Gateway IP.

1. Get the Gateway IP:
```bash
export GATEWAY_IP=$(kubectl get gateway external-http-gateway -n default -o jsonpath='{.status.addresses[0].value}')
echo "Gateway IP: $GATEWAY_IP"
```

2. **Connect:** You must inject the routing headers. Use `curl` to test:
```bash
# Replace <GATEWAY_IP> with the actual IP
curl -v -H "X-Sandbox-ID: sandbox-example" \
        -H "X-Sandbox-Port: 13337" \
        http://$GATEWAY_IP
```

### Option 2: Development (via Router Tunnel)

For local development, port-forward to the **Router Service** (do not port-forward to the pod directly, as it may fail with secure runtimes like gVisor).

1. Start the Tunnel: 
```bash 
# Forward local 8080 to the Router Service
kubectl port-forward svc/sandbox-router-svc 8080:8080 -n default
```

2. Access via Curl: You need to send the correct headers to route traffic to your specific sandbox. Via curl, set:
```bash
curl -v -H "X-Sandbox-ID: sandbox-example" \
        -H "X-Sandbox-Port: 13337" \
        http://localhost:8080
```


#### Getting VSCode password

In a separate terminal connect to the pod and get the password.

```
kubectl exec  sandbox-example --  cat /root/.config/code-server/config.yaml 
```

Use the password and connect to vscode.

## Use gemini-cli

Gemini cli is preinstalled. Open a teminal in vscode and use Gemini cli.
