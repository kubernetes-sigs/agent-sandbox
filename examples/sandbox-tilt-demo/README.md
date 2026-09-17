# Agent Sandbox Tilt Demo

A minimal [Tilt](https://tilt.dev) configuration for developing against Agent
Sandbox locally. It builds the controller from source, loads it into a `kind`
cluster, installs the `Sandbox` CRD and controller, and runs a demo `Sandbox`
so you can watch a backing pod get created — then torn down with `tilt down`.

This covers the core `Sandbox` API only. The extension CRDs
(`SandboxTemplate`, `SandboxClaim`, `SandboxWarmPool`) are out of scope by
design; see [`../warmpool-quickstart/`](../warmpool-quickstart/) for those.

## Files

| Path | Purpose |
| --- | --- |
| `Tiltfile` | Builds the controller + demo images, deploys the controller, runs the demo Sandbox. |
| `sandbox.yaml` | The demo `Sandbox` custom resource. |
| `demo/Dockerfile` | A tiny image that echoes a message and sleeps, so the pod stays up for inspection. |

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/) (or another container engine) running
- [Tilt](https://tilt.dev/install)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick#installing-with-go) — Tilt connects to an existing `kind` cluster; it does not create one

The demo image pulls `alpine` from the registry on first start, so the cluster
nodes need registry access.

## Usage

**1. Create a `kind` cluster.** Tilt drives a cluster named `agent-sandbox`
(the same name `make deploy-kind` uses); create it first:

```bash
kind create cluster --name agent-sandbox
```

**2. Start the demo:**

```bash
tilt up
```

Tilt builds the controller image, loads it into `kind`, installs the CRD and
controller, and creates the demo `Sandbox`. Watch a pod come up in the Tilt UI
or the log:

```bash
kubectl get pods -w
```

The demo `Sandbox` becomes `Ready` once its pod is Running and Ready.

**3. Inspect the Sandbox and its pod:**

```bash
kubectl get sandbox demo
kubectl describe sandbox demo
# The backing pod shares the Sandbox name:
kubectl logs demo -c my-container
```

**4. Tear it down:**

```bash
tilt down
```

`tilt down` deletes the controller, CRD, and the demo `Sandbox`; the controller
terminates the backing pod as part of that cleanup. The `kind` cluster itself is
left in place — delete it separately when done:

```bash
kind delete cluster --name agent-sandbox
```

## Live update

Editing the controller source rebuilds and reloads the controller automatically
(Tilt tracks the Dockerfile's `COPY` layers). Editing `sandbox.yaml` or the
demo image triggers a reload of just the demo `Sandbox`.
