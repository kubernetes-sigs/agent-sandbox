---
title: "Agent Sandbox Installation"
linkTitle: "Agent Sandbox Installation"
weight: 2
description: >
  This guide shows how to install Agent Sandbox resources on [Kubernetes in Docker (KinD)](https://kind.sigs.k8s.io/) and on [GKE](https://cloud.google.com/kubernetes-engine).
---

## Prerequisites

* [kubectl](https://kubernetes.io/docs/tasks/tools/#kubectl) CLI tool.

* For KinD cluster you need:
    * [docker](https://docs.docker.com/engine/install/) or [podman](https://podman.io/docs/installation) installed.
    * [kind](https://kubernetes.io/docs/tasks/tools/#kind) CLI tool.

* For GKE cluster you need:
    * [gcloud CLI](https://cloud.google.com/cli)

1. Run command to create a cluster using KinD:
   ```sh
   kind create cluster --wait 90s --name agent-sandbox-test
   ```
   Or run these commands to create a cluster in GKE and get credentials to your cluster:
   ```sh
   gcloud container clusters create-auto agent-sandbox-test --region=us-central1
   gcloud container clusters get-credentials agent-sandbox-test --location us-central1
   ```

2. Install the agent-sandbox controller and its CRDs with the following command:
   ```sh
   # Get the latest version of the release:
   VERSION=$(basename $(curl -sSL -o /dev/null -w "%{url_effective}" https://github.com/kubernetes-sigs/agent-sandbox/releases/latest))

   # To install only the core components:
   kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/sandbox.yaml
   
   # To install the extensions components:
   kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml

   # Wait until all agent-sandbox-controller pods are running:
   kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=90s
   ```

3. Before using the client, you must deploy the `sandbox-router`. Follow these steps:

   > [!WARNING]
   > The default deployment operates with unauthenticated routing (`--authz-mode=allow-all`) for local testing. See [sandbox-router](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router) for production authentication and TLS options.
   > Remote deployment via kustomize (`?ref=${VERSION}`) requires `v1.0.3` or later; for earlier releases, deploy from a local clone (`kubectl apply -k sandbox-router/deploy/`).

   ```sh
   kubectl apply -k "github.com/kubernetes-sigs/agent-sandbox//sandbox-router/deploy?ref=${VERSION}"

   # Or from a local clone:
   # kubectl apply -k sandbox-router/deploy/

   # Wait until all router pods are running:
   kubectl -n agent-sandbox-system rollout status deployment/sandbox-router --timeout=90s
   ```

4. Create a Sandbox Template. For example the `python-runtime-sandbox`. More information about this runtime can be found [here](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/examples/python-runtime-sandbox/).
   ```bash
   curl -sSL https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/refs/tags/${VERSION}/clients/python/agentic-sandbox-client/python-sandbox-template.yaml | sed -e 's|${SANDBOX_NAMESPACE}|default|g' -e 's|${SANDBOX_TEMPLATE_NAME}|test-sandbox-template|g' | kubectl apply -f -
   ```
