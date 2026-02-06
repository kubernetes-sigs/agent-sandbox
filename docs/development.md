# Development Guide

This guide provides instructions for building, running, and debugging the agent-sandbox controller.

## Prerequisites

Before you begin, ensure you have the following tools installed:

* [Go](https://golang.org/doc/install)
* [Docker](https://docs.docker.com/get-docker/)
  * [Docker buildx plugin](https://github.com/docker/buildx?tab=readme-ov-file#installing) On Debian-based systems, `apt install docker-buildx-plugin`
* [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) (optional, a convenient tool for running local Kubernetes clusters using Docker container)
* [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/)

## Building the Controller

#### Local binary
To build the controller binary, run the following command:

```sh
make build
```

This will compile the controller and create a binary at `bin/manager`.

#### Pushing to a container registry

For working with a remote cluster you can build and push the image to a container registry.

```sh
./dev/tools/push-images --image-prefix=<registry-url-with-trailing-slash>
```

### Regenerate CRD and RBAC

Whenever any changes are made to the `api/` folder or the `controllers/` folder (kubebuilder tags), you may have to regenerate the CRDs and the RBAC manifests.

```sh
make all 
# which runs dev/tools/fix-go-generate
```

## Deploying to cluster

#### Deploying to local `kind` cluster

To run the controller on a local `kind` cluster, use the following command:

```sh
make deploy-kind
```

This command will:

1.  Create a `kind` cluster named `agent-sandbox` if it doesn't already exist.
2.  Build the controller's container image.
3.  Push the image to the `kind` cluster.
4.  Deploy the controller to the `kind` cluster.

You can verify that the controller is running by checking the pods in the `agent-sandbox-system` namespace:

```sh
kubectl get pods -n agent-sandbox-system
```

#### Deploying to a remote cluster

Make sure your kubectl context is set to the cluster you want to deploy to.

```sh
./dev/tools/deploy-to-kube --image-prefix=<registry-url-with-trailing-slash>
```

## Debugging the Controller

There are several ways to debug the controller while it's running in the cluster.

### View Controller Logs

To view the controller's logs, use the `kubectl logs` command. First, find the name of the controller's pod:

```sh
kubectl get pods -n agent-sandbox-system
```

Then, view the logs for that pod:

```sh
kubectl logs -n agent-sandbox-system <pod-name>
```

### Accessing the Controller Pod

You can get a shell into the controller's pod to inspect its filesystem and environment:

```sh
kubectl exec -it -n agent-sandbox-system <pod-name> -- /bin/bash
```

### Running the Controller on the Host

For a faster feedback loop, you can run the controller directly on your host machine and have it connect to the `kind` cluster.

1.  **Get the `kubeconfig` for the `kind` cluster:**

    ```sh
    kind get kubeconfig --name agent-sandbox > /tmp/kubeconfig
    ```

2.  **Set the `KUBECONFIG` environment variable:**

    ```sh
    export KUBECONFIG=/tmp/kubeconfig
    ```

3.  **Apply the CRD:**

    ```sh
    kubectl apply -f ./k8s/crds/
    ```
4.  **Run the controller:**

    ```sh
    go run ./cmd/agent-sandbox-controller/main.go
    ```

The controller will now be running on your host machine and will be connected to the `kind` cluster. You can now use a debugger like Delve to debug the controller.
