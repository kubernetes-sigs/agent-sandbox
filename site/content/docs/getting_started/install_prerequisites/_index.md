---
title: "Agent Sandbox Installation"
linkTitle: "Agent Sandbox Installation"
weight: 2
description: >
  This guide shows how install Agent Sandbox resources in [Kubernetes in Docker (KinD)](https://kind.sigs.k8s.io/) and in [GKE](https://cloud.google.com/kubernetes-engine).
---

## Prerequisites

* [kubectl](https://kubernetes.io/docs/tasks/tools/#kubectl) CLI tool.

* For KinD cluster you need:
    * [docker](https://docs.docker.com/engine/install/) or [podman](https://podman.io/docs/installation) installed.
    * [kind](https://kubernetes.io/docs/tasks/tools/#kind) CLI tool.

* For GKE cluster you need:
    * [gcloud CLI](https://cloud.google.com/cli)

1. Run command to create a cluster in KinD:
   ```sh
   kind create cluster --name agent-sandbox-test
   ```
   Or run these commands to create a cluster in GKE and get credentials to your cluster:
   ```sh
   gcloud container clusters create-auto agent-sandbox-test --region=us-central1
   gcloud container clusters get-credentials agent-sandbox-test --location us-central1
   ```

2. Install the agent-sandbox controller and its CRDs with the following command:
   ```sh
   # Get the latest version of the release:
   VERSION=$(curl https://api.github.com/repos/kubernetes-sigs/agent-sandbox/releases/latest | jq -r '.tag_name')

   # To install only the core components:
   kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml
   
   # To install the extensions components:
   kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml

   # Make sure the controller pods are running:
   kubectl -n agent-sandbox-system get pods
   ```

3. Before using the client, you must deploy the `sandbox-router`. Follow these there steps:
   ```sh
   curl -sSL https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/refs/tags/${VERSION}/clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml | sed 's|${ROUTER_IMAGE}|us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandbox-router:latest-main|g' > sandbox_router.yaml
   kubectl apply -f sandbox_router.yaml

   # Make sure the router pods are running:
   kubectl get pods
   ```

4. Create a Sandbox Template. For example the `python-runtime-sandbox`. More information about this runtime can be found [here](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/examples/python-runtime-sandbox/).
   ```bash
   curl -sSLO http://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/refs/tags/${VERSION}/clients/python/agentic-sandbox-client/python-sandbox-template.yaml
   kubectl apply -f python-sandbox-template.yaml
   ```
