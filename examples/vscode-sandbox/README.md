# VSCode and Gemini CLI in a Sandbox

## Prerequisites

- A Kubernetes cluster with a default StorageClass for the workspace PVC and
  enough node storage for envbuilder's image layers and build caches. The manifest
  sets both the ephemeral storage request and limit to 8Gi.
- The Agent Sandbox controller installed. See the [installation guide](../../README.md#installation).
- `kubectl` configured to use that cluster.

Run the commands below from this example's directory:

```shell
cd examples/vscode-sandbox
```

The commands assume the sandbox is in the `default` namespace. The Router runs
in `agent-sandbox-system`.

## Create a Sandbox with VSCode and Gemini CLI

Select the runtime before creating the Sandbox. Use `base` for the standard
runtime, or apply one of the isolation overlays below instead of `base`:

```
kubectl apply -k base
```

They can then check the status of the applied resource.
Verify sandbox and pod are running:

```
kubectl get sandbox
kubectl get pod sandbox-example

kubectl wait --for=condition=Ready sandbox/sandbox-example --timeout=5m
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

If the Sandbox already exists, [recreate its Pod](#change-the-runtime-of-an-existing-sandbox)
before validating the runtime.

Validate that the `Pod` with gVisor enabled is running:

```shell
kubectl wait --for=condition=Ready sandbox sandbox-example
kubectl get pods -o jsonpath=$'{range .items[*]}{.metadata.name}: {.spec.runtimeClassName}\n{end}'
```

### Harden Agent Sandbox isolation using Kata Containers and Microsoft Hypervisor (MSHV) (Optional)

Similarly to the gVisor scenario, there are scenarios where you may want to provide
workload isolation for running untrusted workloads inside a sandbox.

[AKS Sandbox](https://learn.microsoft.com/en-us/azure/aks/use-pod-sandboxing)
provides a hardware virtualization layer between the host operating system and the
containerized workload via [CloudHypervisor](https://www.cloudhypervisor.org) and
Microsoft Hypervisor (MSHV). There is no shared kernel between the host and the guest workload.

This example demonstrates how to use `Sandbox` along with CloudHypervisor in order
to utilize the lifecycle features of `Sandbox` alongside the hypervisor
isolation features of CloudHypervisor + MSHV.

#### Create a cluster with AKS Sandboxing enabled

The scripts directory contains shell scripts for provisioning, applying, and
cleaning up Azure Kubernetes cluster resources.

From this example's directory, create the cluster and deploy the example:

```shell
./scripts/az-provision.sh
./scripts/apply.sh --sandbox-router-image registry.k8s.io/agent-sandbox/sandbox-router-go:v1.0.12
export KUBECONFIG="$PWD/scripts/kubeconfig"
```

The helper installs the Agent Sandbox controllers and the Go Sandbox Router,
then applies `overlays/kata-mshv`. It uses `scripts/kubeconfig`, which
`az-provision.sh` generates. Pass `--kubeconfig FILE` to choose a different
cluster and export that same file as `KUBECONFIG` for subsequent `kubectl`
commands. Set `VERSION` to use a different controller release.

If you already have an AKS cluster with Pod Sandboxing enabled and the controller
installed, apply the overlay directly and deploy the Router as described in
[Access through the Sandbox Router](#option-b-access-through-the-sandbox-router):

```shell
kubectl apply -k overlays/kata-mshv
```

The overlay selects the Kata RuntimeClass for MSHV isolation:

```yaml
runtimeClassName: kata-vm-isolation
```

If the Sandbox already exists, [recreate its Pod](#change-the-runtime-of-an-existing-sandbox)
after applying the overlay, whether you use the helper or `kubectl` directly.

#### Delete the AKS Kubernetes cluster and kubeconfig

Once you are done with your cluster, run the following command to deprovision
all of the resources and delete the local kubeconfig file.

```shell
./scripts/az-cleanup.sh
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

If the Sandbox already exists, [recreate its Pod](#change-the-runtime-of-an-existing-sandbox)
before validating the runtime.

Validate that the `Pod` with Kata container enabled is running:

```shell
$ kubectl wait --for=condition=Ready sandbox sandbox-example
$ kubectl get pods -o jsonpath=$'{range .items[*]}{.metadata.name}: {.spec.runtimeClassName}\n{end}'
```

## Change the runtime of an existing Sandbox

The controller uses `podTemplate` when it creates a Pod. Applying an overlay to
an existing Sandbox does not update the running Pod's spec. After applying the
chosen overlay, recreate only the Pod to use the new runtime. This stops the
Pod's processes and retains the workspace PVC:

```shell
kubectl delete pod sandbox-example
kubectl get pods -l sandbox=sandbox-example --watch
```

Stop the watch when the replacement Pod appears. Then check its runtime:

```shell
kubectl wait --for=condition=Ready pod/sandbox-example --timeout=5m
kubectl get pod sandbox-example -o jsonpath='{.spec.runtimeClassName}{"\n"}'
```

Verify that the printed RuntimeClass matches the overlay you selected, then wait
for VSCode to start as described below.

## Accessing VSCode

### 1. Wait for VSCode to Start

The `sandbox-example` pod runs an initialization process (`envbuilder`) that clones the repository and installs dependencies before starting VSCode. You must wait for this process to finish.

If you deployed an earlier version with authentication disabled, reapply the
base or isolation overlay you used to deploy it. Then [recreate the
Pod](#change-the-runtime-of-an-existing-sandbox). The new Pod uses the startup
script from the ConfigMap and keeps the checkout and edits on the workspace PVC.

Watch the logs for `HTTP server listening` and `Authentication is enabled`:

```bash
# Wait for output similar to:
$ kubectl logs -f sandbox-example

[2025-...] info  HTTP server listening on http://0.0.0.0:13337/
[2025-...] info    - Authentication is enabled
```

### 2. Get the VSCode password

`code-server` generates a password on first startup. Read it from the running Pod:

```shell
kubectl exec sandbox-example -- sed -n 's/^password: //p' /root/.config/code-server/config.yaml
```

### 3. Connect to VSCode

Enter the generated password when the browser opens the `code-server` login page.

#### Option A: Direct Pod Access

If your runtime supports direct pod port-forwarding, connect to the pod:

```bash
# Forward local port 13337 to the pod
kubectl port-forward pod/sandbox-example 13337:13337
```

Open your browser to http://localhost:13337.


#### Option B: Access through the Sandbox Router

gVisor does not support direct pod port-forwarding. Use the Go Sandbox Router
when direct access is unavailable. The Router forwards HTTP and WebSocket
traffic to the sandbox pod.

If you deployed with `scripts/apply.sh`, skip the installation commands below.
For a manual installation, run them from `examples/vscode-sandbox`. If you
started from the repository root, first run `cd examples/vscode-sandbox`.

```shell
kubectl apply -f ../../sandbox-router/deploy/serviceaccount.yaml \
  -f ../../sandbox-router/deploy/rbac.yaml \
  -f ../../sandbox-router/deploy/service.yaml

kubectl set image --local -f ../../sandbox-router/deploy/deployment.yaml \
  sandbox-router=registry.k8s.io/agent-sandbox/sandbox-router-go:v1.0.12 \
  -o yaml | kubectl apply -f -
```

Wait for the Router Deployment before connecting:

```shell
kubectl -n agent-sandbox-system rollout status deployment/sandbox-router --timeout=180s
```

##### Via Router Port-Forward Tunnel

Port-forward to the Router Service:

```shell
kubectl -n agent-sandbox-system port-forward svc/sandbox-router-svc 8080:8080
```

Check that the Router redirects you to the login page:

```bash
curl -v -H "X-Sandbox-ID: sandbox-example" \
        -H "X-Sandbox-Port: 13337" \
        http://localhost:8080
```

For browser access, use a browser extension you trust to set the same routing
headers on HTTP and WebSocket upgrade requests to `http://localhost:8080`. Open
that address and sign in.

##### Via an HTTPS Gateway

For external access, configure an [HTTPS Gateway listener](https://gateway-api.sigs.k8s.io/guides/user-guides/tls/)
with a certificate for your hostname. Before routing external traffic to
`sandbox-router-svc` on port `8080`, configure
[Router authorization](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/sandbox-router/README.md#authorization)
to limit access to the intended Sandbox and port. Scope the browser extension's
routing headers to the HTTPS address, then open that address and sign in.

#### Option C: Desktop VS Code (Optional)

The sandbox does not run `sshd`, so Remote-SSH does not apply. To use the
desktop VS Code instead of `code-server`, attach it to the pod with `kubectl`:

1. Install `kubectl` and the VS Code Dev Containers extension.
2. Run **Dev Containers: Attach to Running Kubernetes Container...** from the
   Command Palette and pick the `devcontainer-main` container in
   `sandbox-example`.
3. Open the `/workspaces` folder.

See [Attach to a container in a Kubernetes cluster](https://code.visualstudio.com/docs/devcontainers/attach-container#_attach-to-a-container-in-a-kubernetes-cluster).

## Use gemini-cli

Gemini cli is preinstalled. Open a teminal in vscode and use Gemini cli.
