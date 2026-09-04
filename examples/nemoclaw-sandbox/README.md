# NemoClaw in Sandbox

## Overview

This tutorial walks you through setting up a complete, local testing environment for AI agents using **NemoClaw** and **OpenClaw** backed by **OpenShell**.

By the end of this guide, you will have a local Kubernetes cluster running a sandbox where your AI agent can safely generate and execute code (like Python scripts) in an isolated environment. We will use the Kubernetes `agent-sandbox` custom resources to provision these workspaces dynamically.

> **Note on Security & Isolation:** This tutorial uses KinD (Kubernetes IN Docker) for local development, which relies on standard container runtimes (`runc`). While this is perfect for testing, if you plan to deploy this in a production environment, you should configure your cluster to use secure sandboxing runtimes like gVisor or Kata Containers via Kubernetes `RuntimeClasses`.

## Prerequisites

Before starting, ensure you have the following command-line tools installed on your local machine:

* **[Docker](https://docs.docker.com/get-docker/):** Required to run our local Kubernetes cluster.
* **[KinD (Kubernetes IN Docker)](https://kind.sigs.k8s.io/docs/user/quick-start/#installation):** Used to spin up the local `agent-sandbox` cluster.
* **[kubectl](https://kubernetes.io/docs/tasks/tools/):** The Kubernetes command-line tool, used to apply manifests and interact with our sandbox pods.
* **[Helm](https://helm.sh/docs/intro/install/):** The Kubernetes package manager, required to install the OpenShell OCI chart.

## Installation

Create KinD cluster:

```bash
kind create cluster --name agent-sandbox
```

Install k8s-agent-sandbox CRDs:

```bash
export VERSION="v0.5.6"

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/sandbox-with-extensions.yaml
```

Install openshell:

```bash
helm upgrade --install openshell \
  oci://ghcr.io/nvidia/openshell/helm-chart \
  --namespace default \
  --set server.disableTls=true \
  --set server.auth.allowUnauthenticatedUsers=true

kubectl -n default rollout status statefulset/openshell
```

Apply `nemoclaw-in-sandbox.yaml` manifest by running the command below. This will create a sandbox that installs openclaw and openshell CLI.

> **Note the version:** At this moment, the current stable version of `OpenClaw` [causes a bug with sandbox name rejection](https://github.com/NVIDIA/OpenShell/issues/2651). At this moment, [the fix provided in the beta version](https://github.com/NVIDIA/OpenShell/issues/2651#issuecomment-5334730653). It should be available later in a stable version.

```bash
kubectl apply -f nemoclaw-in-sandbox.yaml
kubectl wait --for=condition=Ready pod/nemoclaw -n default --timeout=5m
```


Run this command to get into your sandbox:

```bash
kubectl exec -it nemoclaw -- bash
```

Run the following commands to setup your openclaw and configure it to use openshell with sandbox backend:

```bash
openclaw onboard --mode local

openclaw plugins install @openclaw/openshell-sandbox@2026.8.1-beta.3 # TODO: Later should be updated to a stable version to fix this issue https://github.com/NVIDIA/OpenShell/issues/2710. Currently we create openshell-wrapper.sh to fix this problem.

openclaw config set agents.defaults.sandbox.mode all
openclaw config set agents.defaults.sandbox.backend openshell
openclaw config set agents.defaults.sandbox.scope agent
openclaw config set agents.defaults.sandbox.workspaceAccess rw
```

> Since the `-- true` command is hardcoded into the OpenClaw plugin, we cannot change it via standard configuration. Instead, we can write a tiny bash wrapper to intercept OpenClaw's calls to the OpenShell CLI and swap the buggy true command with sleep infinity (which runs forever). Run the command below:

```bash
cat << 'EOF' > /root/openshell-wrapper.sh
#!/bin/bash

new_args=()
for arg in "$@"; do
  new_args+=("$arg")
done

# If the OpenClaw plugin is trying to detach with the buggy "true" command, replace it
if [ "${new_args[-1]}" = "true" ] && [ "${new_args[-2]}" = "--" ]; then
  new_args[-1]="sleep"
  new_args+=("infinity")
fi

# Find the real OpenShell binary and execute it with our corrected arguments
REAL_OPENSHELL=$(command -v openshell)
exec "$REAL_OPENSHELL" "${new_args[@]}"
EOF

chmod +x /root/openshell-wrapper.sh
```

Update openclaw config to route the wrapper:

```bash
openclaw config set plugins.entries.openshell.config.command /root/openshell-wrapper.sh
```

## Testing

To test openclaw, run the following commands:

```bash
openclaw gateway run &

# Poll status every 2 seconds until it reports "ok"
while ! openclaw gateway status 2>/dev/null | grep -q "ok"; do sleep 2; done

openclaw agent --agent main --session-key agent:main:t1 --message "Write a Python script that prints 'Hello from the sandbox' and execute it."
```

In another terminal check a new pod with `kubectl get pods`. You can exec into it and validate that the Python script was created inside the new sandbox.

## Clean up

Run the following command to delete the KinD cluster:

```bash
kind delete cluster --name agent-sandbox
```
