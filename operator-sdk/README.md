# Agent Sandbox Operator SDK Project

This directory contains a Helm-based Operator SDK project for `agent-sandbox`.

It is intentionally wired to use the existing Helm assets from the repo root:

- Chart source: `../helm` (see `watches.yaml`)
- CRDs source: `helm/crds/*` copied into `config/crd/bases/*`
- RBAC source: Helm-generated RBAC rules only (see `config/rbac/role.yaml`)

No new API/controller scaffolding is used as the source of truth for CRDs/RBAC.

## Prerequisites

- `kubectl` configured to your target cluster
- `docker` or `podman` (Makefile defaults to `docker`)
- `operator-sdk` (optional; Makefile downloads tools when needed)

## Build and Push Operator Image

Run from this directory:

```bash
cd operator-sdk
make docker-build IMG=<registry>/<repo>/agent-sandbox-operator:<tag>
make docker-push IMG=<registry>/<repo>/agent-sandbox-operator:<tag>
```

## Install CRDs and Deploy

```bash
cd operator-sdk
make install
make deploy IMG=<registry>/<repo>/agent-sandbox-operator:<tag>
```

Remove deployment and CRDs:

```bash
cd operator-sdk
make undeploy
make uninstall
```

## Run Locally Without Container Build

```bash
cd operator-sdk
make run
```

## Generate and Validate OLM Bundle

```bash
cd operator-sdk
make bundle VERSION=0.1.0 CHANNELS=alpha DEFAULT_CHANNEL=alpha
```

Build and push bundle image:

```bash
cd operator-sdk
make bundle-build BUNDLE_IMG=<registry>/<repo>/agent-sandbox-operator-bundle:v0.1.0
make bundle-push BUNDLE_IMG=<registry>/<repo>/agent-sandbox-operator-bundle:v0.1.0
```

## Important Notes

- `watches.yaml` points to `../helm`, so chart changes in `helm/` are consumed directly.
- CRDs and RBAC are auto-synced from `helm/` before `make install`, `make deploy`, and `make bundle`.
- You can run sync explicitly anytime:

```bash
cd operator-sdk
make sync-helm-assets
```
