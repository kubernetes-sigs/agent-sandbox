# agent-sandbox-operator
Operator Lifecycle Manager (OLM) packaging for [Agent Sandbox](https://agent-sandbox.sigs.k8s.io): it installs the controller, CRDs (`Sandbox` and extension APIs), and RBAC so you can deploy Agent Sandbox from OperatorHub, OpenShift, or any cluster that consumes OLM bundles—without applying the upstream `k8s/` manifests by hand.

## Description

[Agent Sandbox](https://agent-sandbox.sigs.k8s.io) is a Kubernetes SIG Apps project that provides a declarative API for long-running, stateful, singleton workloads—think isolated dev environments, notebooks, or AI agent runtimes backed by a single pod with stable identity and optional persistent storage. The core `Sandbox` CRD manages that lifecycle; the extension APIs (`SandboxTemplate`, `SandboxClaim`, and `SandboxWarmPool`) add templating, claim-based provisioning, and warm pools for faster startup.

The operator packages the upstream Agent Sandbox controller (including extension reconcilers) for **Operator Lifecycle Manager**: CRDs, RBAC, metrics Service, and Deployment are kept in sync with the main project’s `k8s/` manifests via `hack/sync-k8s-manifests`, then published as an OLM bundle (`make bundle`) for installation on OpenShift, OperatorHub, or any OLM-enabled cluster. After install, you create and manage `Sandbox` and extension resources the same way as with a plain manifest deploy—see the [project docs](https://agent-sandbox.sigs.k8s.io/docs/) and [examples](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples) for API usage and samples.

## Installation

This directory packages Agent Sandbox for **OLM** (OperatorHub, OpenShift, and other catalog-driven installs). It is not the primary install path for most clusters.

| Method | Where to start |
| --- | --- |
| Release manifests (`kubectl apply`) | [Installation](../README.md#installation) in the repo root README (generated from [`k8s/`](../k8s/)) |
| Helm | [`helm/README.md`](../helm/README.md) |
| OLM bundle | Published operator catalog, or [Releasing a new operator version](#releasing-a-new-operator-version) / [Testing a bundle locally](#testing-a-bundle-locally) below |

For controller development (local kind cluster, image build), see [docs/development.md](../docs/development.md).

## Maintaining operator manifests

CRDs, ClusterRoles, and the controller Deployment in this module are **copies** of the main Agent Sandbox tree. They are not authored separately under `olm/config/`.

### Single source of truth

| Layer | Location | How it changes |
| --- | --- | --- |
| API types and kubebuilder markers | [`api/`](../api/), [`extensions/api/`](../extensions/api/) | Edit Go types and controller RBAC markers |
| Generated install YAML | [`k8s/`](../k8s/) (`crds/`, `rbac.generated.yaml`, `controller.yaml`, `extensions.controller.yaml`, …) | From the **repo root**: `make fix-go-generate` (or `make all`) |
| Operator SDK / OLM config | `olm/config/` | From the **repo root**: `make fix-go-generate` (or `make fix-olm-manifests`); equivalent to `make manifests` in this directory |

Contributors should **not** hand-edit the synced paths below. Change the upstream API or manifests, regenerate `k8s/`, then refresh the operator config.

### Synced paths (do not edit by hand)

`make copy-k8s-config` (also run as the `manifests` target) copies from `../k8s` (`K8S_ROOT`, default one level above this module):

- `k8s/crds/*.yaml` → `config/crd/bases/` and `config/crd/kustomization.yaml` (resource list derived from copied CRDs)
- `k8s/rbac.generated.yaml` → `config/rbac/role.yaml`
- `k8s/extensions-rbac.generated.yaml` → `config/rbac/extensions_role.yaml`
- `k8s/extensions.yaml` → `config/rbac/extensions_role_binding.yaml`
- `k8s/controller.yaml` and `k8s/extensions.controller.yaml` → `config/rbac/support.yaml` and `config/manager/manager.yaml` via [`hack/sync-k8s-manifests`](hack/sync-k8s-manifests/) (Namespace, ServiceAccount, bindings, Service, extensions Deployment; image placeholder rewritten for the operator image)

Run from `olm/`:

```sh
make manifests
# equivalent:
make copy-k8s-config
```

Other `make` targets (`test`, `deploy`, `bundle`, …) depend on `manifests` and will run the sync when needed.

### Typical workflow

1. Change API or controller code in the parent repo; run `make fix-go-generate` at the repo root. This regenerates `k8s/` and syncs `olm/config/` (via [`dev/tools/sync-olm-manifests`](../dev/tools/sync-olm-manifests) in [`codegen.go`](../codegen.go)).
2. Commit the updated `k8s/` output and synced `config/crd/bases/`, `config/rbac/`, and `config/manager/manager.yaml` together with any OLM bundle changes (`make bundle` when publishing).

### Operator-only config (safe to edit)

OLM and kubebuilder scaffolding that are **not** overwritten by `copy-k8s-config` include, for example: `config/manifests/` (ClusterServiceVersion), `config/default/`, `config/prometheus/`, `config/network-policy/`, `config/scorecard/`, and `config/samples/`. Adjust those when changing catalog metadata, metrics wiring, or install UX—not when updating CRD schemas or controller RBAC.

### Releasing a new operator version

From `olm/`, after syncing manifests and ensuring the controller image you want is published, set the release version and controller image, generate the OLM bundle, then build and push the bundle image:

```sh
export VERSION=0.4.6
export IMG=registry.k8s.io/agent-sandbox/agent-sandbox-controller:v${VERSION}
export IMAGE_TAG_BASE=quay.io/you/agent-sandbox-operator   # required for bundle-build; or set BUNDLE_IMG instead

make bundle
make bundle-build
make bundle-push
```

`make bundle` refreshes `config/` from `../k8s`, stamps the CSV with `VERSION`, and sets the related image to `IMG`. Set `IMAGE_TAG_BASE` to your OCI registry prefix before `bundle-build` / `bundle-push`; `BUNDLE_IMG` is then `${IMAGE_TAG_BASE}-bundle:v${VERSION}`. Override the full tag with `BUNDLE_IMG` if needed (e.g. `make bundle-push BUNDLE_IMG=quay.io/you/agent-sandbox-operator-bundle:v0.4.6`). You need registry credentials and a container runtime (`docker` or `podman`) for `bundle-build` / `bundle-push`.

### Testing a bundle locally

Log in to a Kubernetes cluster that can run OLM (for example OpenShift, or a kind cluster with OLM installed). From `olm/`:

```sh
export VERSION=0.4.6
export IMG=registry.k8s.io/agent-sandbox/agent-sandbox-controller:v${VERSION}
export IMAGE_TAG_BASE=your-registry/agent-sandbox-operator   # required for bundle-build
export BUNDLE_IMG=${IMAGE_TAG_BASE}-bundle:v${VERSION}         # or set BUNDLE_IMG directly

make bundle
make bundle-build

operator-sdk run bundle ${BUNDLE_IMG}
```

`operator-sdk run bundle` installs the bundle into the cluster’s OLM namespace so you can subscribe and verify the operator before publishing. Use the same `VERSION`, `IMG`, and `BUNDLE_IMG` you intend to ship.

> **Note:** The CRD conversion webhook `clientConfig` hardcodes `namespace: agent-sandbox-system`. Because `webhookdefinitions` is stripped from the CSV (preventing OLM from rewriting the `clientConfig`), the operator **must** be installed in the `agent-sandbox-system` namespace for conversion webhooks to function. If you use `operator-sdk run bundle`, pass `--namespace agent-sandbox-system`; the `operatorframework.io/suggested-namespace` annotation is only a hint and is not enforced.

## Contributing
Please read our [Contributing Guidelines](../CONTRIBUTING.md) for our full code review and PR policies.

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

