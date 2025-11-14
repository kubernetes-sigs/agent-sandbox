---
title: "gVisor on GKE"
linkTitle: "gVisor on GKE"
weight: 2
description: >
  This guide shows how to run [Agent Sangbox](https://github.com/kubernetes-sigs/agent-sandbox) with the [gVisor](https://gvisor.dev) runtime using GKE as a cluster.
---

## Prerequisites

* [gcloud CLI](https://docs.cloud.google.com/sdk/docs/install)
* [kubectl](https://kubernetes.io/docs/tasks/tools/)

## Create a GKE cluster

Specify your project:

```sh
export PROJECT_ID=$(gcloud config get project)
```

Run the following command to create a GKE cluster:

```sh
gcloud container clusters create demo-gvisor-cluster \
--location=us-central1-c \
--project=$PROJECT_ID
```

Then add a node that supports `gVisor` by running:

```sh
gcloud container node-pools create gvisor-node-pool \
--region us-central1-c \
--cluster=demo-gvisor-cluster \
--num-nodes=1 \
--sandbox type=gvisor
```

To access your cluster run this command:

```sh
gcloud container clusters get-credentials demo-gvisor-cluster \
--region us-central1-c \
--project ${PROJECT_ID}
```

## Install Sandbox CRD

Run these commands to install `Sandbox CRD`:

```sh
export VERSION=v0.1.0-rc.0
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml
```

## Testing

Run this command to create an ampty Sandbox that uses `gVisor` as a RuntimeClass:

```sh
kubectl apply -f gvisor-empty-sandbox.yaml
```

## Cleanup

```sh
gcloud container clusters delete demo-gvisor-cluster \
		--location=us-central1-c \
		--project=$PROJECT_ID
```
