
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

## Accesing vscode

Port forward the vscode server port.

```
 kubectl port-forward --address 0.0.0.0 pod/sandbox-example 13337
```

Connect to the vscode-server on a browser via  http://localhost:13337 or <machine-dns>:13337

If should ask for a password.

#### Getting vscode password

In a separate terminal connect to the pod and get the password.

```
kubectl exec  sandbox-example --  cat /root/.config/code-server/config.yaml 
```

Use the password and connect to vscode.

## Use gemini-cli

Gemini cli is preinstalled. Open a teminal in vscode and use Gemini cli.
