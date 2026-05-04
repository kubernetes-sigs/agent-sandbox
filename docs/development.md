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

### Regenerate API documentation

Whenever any changes are made to the `api/` folder or the `extensions/`, you may need to regenerate API documentation to keep it up-to-date.

```sh
make generate-api-docs
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

## CI/CD with Prow

The project uses [Prow](https://prow.k8s.io) for CI/CD. 

The configuration for these jobs is managed in the [kubernetes/test-infra](https://github.com/kubernetes/test-infra) repository, see [`config/jobs/kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes/test-infra/tree/master/config/jobs/kubernetes-sigs/agent-sandbox).

The CI scripts are located in the [`dev/ci/`](../dev/ci/) directory.

Note that presubmits are triggered on every push to the `main` branch, and postsubmits are triggered on every merge to the `main` branch.

### Pull Requests

[@k8s-ci-robot](https://github.com/k8s-ci-robot) will automatically merge PRs that have both `lgtm` and `approve` labels, and have passed all the Prow presubmits, see the [code review process](https://github.com/kubernetes/community/blob/master/contributors/guide/owners.md#the-code-review-process). 

You can add commands in PRs/issues to trigger certain actions, see [Prow command help](https://prow.k8s.io/command-help?repo=kubernetes-sigs%2Fagent-sandbox) for more information.

This repo uses squash merge by default. If you need to retain separate commits in a PR when it merges, use the `/label tide/merge-method-rebase` command to change the merge method.

@k8s-ci-robot doesn't handle GitHub Actions in presubmits, so [GitHub Workflows](../.github/workflows) should contain only non-presubmit jobs.

### Image Registries and Promotion

The project uses Google Artifact Registry (GAR) for container image storage and distribution.

#### Registries

-   **Staging Registry**: `us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox`.
    This is where all intermediate and development images are pushed as postsubmits. See [post-agent-sandbox-push-images job history](https://prow.k8s.io/job-history/gs/kubernetes-ci-logs/logs/post-agent-sandbox-push-images).
-   **Production Registry**: `registry.k8s.io/agent-sandbox`.
    Official releases are served from this registry.

#### Promotion Process

To move an image from staging to the production registry, a **promotion process** is required:

1.  **Staging**: Images are built and pushed to the staging registry.
2.  **Promotion PR**: A PR is submitted to the [kubernetes/k8s.io](https://github.com/kubernetes/k8s.io) repository. This PR updates the registry configuration (e.g., [`registry.k8s.io/images/k8s-staging-agent-sandbox/images.yaml`](https://github.com/kubernetes/k8s.io/blob/main/registry.k8s.io/images/k8s-staging-agent-sandbox/images.yaml)) with the image digest and its associated tag. See [example PR](https://github.com/kubernetes/k8s.io/pull/9230).
3.  **Promotion**: Once the PR is merged, the image is automatically promoted to `registry.k8s.io`.

This step can be automated by running `make release-promote TAG=vX.Y.Z`. This calls `dev/tools/tag-promote-images` script which handles the promotion process. `IMAGES_TO_PROMOTE` variable in the script can be updated to include more images.
