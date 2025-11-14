---
title: "Concepts"
linkTitle: "Concepts"
weight: 2
description: >
  This section of the documentation helps you learn about the components, APIs and abstractions that Agent Sandbox uses to represent your cluster and workloads Core Agent Sandbox Concepts
---

## Sandbox

The `Sandbox` CRD is designed to solve the problem of managing **isolated, stateful, singleton workloads** in Kubernetes.

While Kubernetes is exceptionally good at running stateless, replicated applications (like web servers managed by a `Deployment`), it lacks a simple, dedicated abstraction for applications that need to be:

  * **Singleton:** only one instance of the application should ever be running.
  * **Stateful:** the application needs to retain its state (e.g., files on a disk, configuration, user data) across restarts or updates.
  * **Isolated:** it requires a secure, self-contained environment, often for running user-provided code or interactive sessions.

Common examples include AI agent runtimes, remote development environments (like a VS Code server), or data science tools (like a Jupyter Notebook). Today, users often resort to using a `StatefulSet` with `replicas: 1` as a workaround, but this is a cumbersome approach that doesn't offer specialized features for this use case.

The `Sandbox` resource provides a **first-class API** for these workloads, making their management declarative, simple, and Kubernetes-native.

## Benefits

Using the `Sandbox` CRD provides several key benefits:

1.  **Declarative Simplicity:** instead of manually creating and managing a `StatefulSet`, `PersistentVolumeClaim`, and `Service`, we can define a single `Sandbox` resource. The controller handles the complexity of creating and wiring together the underlying components.

2.  **Stateful by Design:** persistence is a core feature. The `Sandbox` automatically manages a persistent volume, ensuring that any data saved within the environment survives pod restarts, crashes, or node failures.

3.  **Strong Isolation:** by design, it encourages running workloads in an isolated manner, which is critical for security, especially in multi-tenant environments where you might be running untrusted code.

## Simple Example

Here is a basic example of a `Sandbox` manifest that runs a simple container with a persistent home directory.
{{% snippet file="additional/examples/sandbox-ksa/sandbox.yaml" type="yaml" %}}


## Glossary

* **Controller**
    The cluster-level component that reconciles the state of `Sandbox` resources, managing their underlying `StatefulSets` and `PersistentVolumeClaims`.

* **Custom Resource Definition (CRD)**
    An extension of the Kubernetes API that introduces the `Sandbox` resource, making it a native object in the cluster.

* **PersistentVolumeClaim (PVC)**
    A request for storage used by a `Sandbox` to provide a persistent, stateful filesystem for the workload.

* **Sandbox**
    The unit of deployment in `agent-sandbox`. It represents an isolated, stateful, and singleton workload, often used for development environments or AI agent runtimes.

* **Singleton Workload**
    An application designed to have only one running instance, which is the core management pattern for the `Sandbox` resource.

* **Stateful Workload**
    An application that requires its data to be preserved across restarts and hibernations.

* **StatefulSet**
    The underlying Kubernetes API object used by the controller to manage a `Sandbox`'s pod and ensure it has a stable identity and persistent storage.
