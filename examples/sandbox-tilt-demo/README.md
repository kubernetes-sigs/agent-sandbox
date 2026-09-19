# Agent Sandbox Tilt Demo

A [Tilt](https://tilt.dev) local development setup for building applications that use Agent
Sandbox. Deploys the controller on a Kind cluster along with a simple web
app running, so you can edit application code and have it redeployed
in seconds while a real controller reconciles the `Sandbox` resources it creates.

The app is a FastAPI service that has a `POST /create` route that creates a `Sandbox` 
and waits for it to go Ready, and a `POST /delete`  that removes it. 

This covers the core `Sandbox` API only. The extension CRDs (`SandboxTemplate`, 
`SandboxClaim`, `SandboxWarmPool`) have examples available at 
[`../warmpool-quickstart/`](../warmpool-quickstart/).

## Files

| Path | Purpose |
| --- | --- |
| `demo/app.py` | A simple FastAPI app. `POST /create` creates the `Sandbox`, `POST /delete` removes it. |
| `manifests/app.yaml` | Deployment + ServiceAccount + Role + RoleBinding for the app. The `Role` is the RBAC an app needs to manage sandboxes. |
| `demo/Dockerfile` | Builds the app image. |
| `Tiltfile` | Installs the controller, builds and deploys the app, forwards it to `localhost:8080`. |

The `Sandbox` the app creates runs `alpine:latest` executing
`echo 'Hello Sandbox!' && sleep 86400`. It stays up until deleted, so the
`Sandbox` holds `Ready`. The image and sleep come from the `SANDBOX_IMAGE` and
`SANDBOX_DURATION` env vars, which default in `demo/app.py`. They can be 
updated in `manifests/app.yaml` to override.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/) (or another container engine) running
- [Tilt](https://tilt.dev/install)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick#installing-with-go)

The Sandbox workload pulls `alpine` from the registry on first start, so the
cluster nodes need registry access (set `SANDBOX_IMAGE` to a locally available
image to avoid this).

## Usage

1. Create a Kind cluster:

```bash
kind create cluster --name agent-sandbox
```

The Tiltfile hardcodes a `kind-agent-sandbox` context to prevent it from installing on your current context, so make sure this name is exact, or update the Tiltfile to match the cluster name. 

2. Start the cluster:

```bash
tilt up
```

Tilt installs the CRD and controller, then builds and runs the app.

3. Create a Sandbox:

Tilt forwards the app to `localhost:8080`, so it is reachable as soon as the
`agent-sandbox-demo-app` resource is green (no `kubectl port-forward` needed):

```bash
curl -s -X POST http://localhost:8080/create
```

The call returns once the `Sandbox` reports `Ready`, which happens when the
controller's backing pod is Running and Ready. The pod shares the Sandbox name:

```bash
kubectl get sandbox demo
kubectl logs demo  # should print: Hello Sandbox!
```

Delete it again with:

```bash
curl -s -X POST http://localhost:8080/delete
```

4. Tear it down:

```bash
tilt down
```

`tilt down` deletes the controller, CRD and app. Removing the CRD takes any
`Sandbox` still present with it. When finished, delete the Kind cluster when done:

```bash
kind delete cluster --name agent-sandbox
```

## The app development loop

Edit `demo/app.py` or `manifests/app.yaml` and Tilt rebuilds/ redeploys 
the app, typically in seconds. This enables rapid development of
applications that use Agent Sandbox in a way that allows the controller
to be part of the local development loop. No need to deploy to a remote
cluster to see how the application changes behave. 

Because this example lives in the agent-sandbox repository, the Tiltfile also
builds the controller from local source, so editing `api/`, `cmd/`,
`controllers/`, `extensions/` or `internal/` triggers a controller rebuild. In
your own project you would drop that `docker_build` and apply a released
controller instead (see [Installation](../../README.md#installation)).

## Adapting it to your app

- **Replace `demo/app.py`.** The only part that has to survive is the `Sandbox`
  body in `build_sandbox_body()`, which is the API surface apps integrate
  with.
- **Keep the `Role` in `manifests/app.yaml` in step with what your app calls.**
  It currently grants `create`, `get` and `delete` on `sandboxes`.
- **Keep `resource_deps=['agent-sandbox-controller']`** on your app's
  `k8s_resource` so it never starts before the `Sandbox` CRD is served.
