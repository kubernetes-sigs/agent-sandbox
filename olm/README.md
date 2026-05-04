# agent-sandbox-operator
Operator Lifecycle Manager (OLM) packaging for [Agent Sandbox](https://agent-sandbox.sigs.k8s.io): it installs the controller, CRDs (`Sandbox` and extension APIs), and RBAC so you can deploy Agent Sandbox from OperatorHub, OpenShift, or any cluster that consumes OLM bundles—without applying the upstream `k8s/` manifests by hand.

## Description

[Agent Sandbox](https://agent-sandbox.sigs.k8s.io) is a Kubernetes SIG Apps project that provides a declarative API for long-running, stateful, singleton workloads—think isolated dev environments, notebooks, or AI agent runtimes backed by a single pod with stable identity and optional persistent storage. The core `Sandbox` CRD manages that lifecycle; the extension APIs (`SandboxTemplate`, `SandboxClaim`, and `SandboxWarmPool`) add templating, claim-based provisioning, and warm pools for faster startup.

The operator packages the upstream Agent Sandbox controller (including extension reconcilers) for **Operator Lifecycle Manager**: CRDs, RBAC, metrics Service, and Deployment are kept in sync with the main project’s `k8s/` manifests via `hack/sync-k8s-manifests`, then published as an OLM bundle (`make bundle`) for installation on OpenShift, OperatorHub, or any OLM-enabled cluster. After install, you create and manage `Sandbox` and extension resources the same way as with a plain manifest deploy—see the [project docs](https://agent-sandbox.sigs.k8s.io/docs/) and [examples](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples) for API usage and samples.

## Getting Started

### Prerequisites
- go version v1.24.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/agent-sandbox-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/agent-sandbox-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/agent-sandbox-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/agent-sandbox-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
operator-sdk edit --plugins=helm/v1-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
Please read our [Contributing Guidelines](CONTRIBUTING.md) for our full code review and PR policies.

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

