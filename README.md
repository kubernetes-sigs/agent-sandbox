<div align="center">
  <img src="site/assets/icons/color_logo.svg" alt="Agent Sandbox logo" width="150" />

  <h1>Agent Sandbox</h1>
</div>


<p>
  <a href="https://github.com/kubernetes-sigs/agent-sandbox/releases"><img src="https://img.shields.io/github/v/release/kubernetes-sigs/agent-sandbox" alt="GitHub release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/Apache-2-blue.svg" alt="Apache-2.0 license"></a>
  <a href="https://goreportcard.com/report/sigs.k8s.io/agent-sandbox"><img src="https://goreportcard.com/badge/sigs.k8s.io/agent-sandbox" alt="Go Report Card"></a>
</p>

[Website](https://agent-sandbox.sigs.k8s.io) · [Docs](https://agent-sandbox.sigs.k8s.io/docs/) · [DeepWiki](https://deepwiki.com/kubernetes-sigs/agent-sandbox) · [Getting Started](https://agent-sandbox.sigs.k8s.io/docs/getting_started/) · [Examples](examples/) · [Roadmap](roadmap.md)

**agent-sandbox enables easy management of isolated, stateful, singleton workloads, ideal for use cases like AI agent runtimes.**

This project is developing a `Sandbox` Custom Resource Definition (CRD) and controller for Kubernetes, under the umbrella of [SIG Apps](https://github.com/kubernetes/community/tree/master/sig-apps). The goal is to provide a declarative, standardized API for managing workloads that require the characteristics of a long-running, stateful, singleton container with a stable identity, much like a lightweight, single-container VM experience built on Kubernetes primitives.

## Overview

### Core: Sandbox

The `Sandbox` CRD is the core of agent-sandbox. It provides a declarative API for managing a single, stateful pod with a stable identity and persistent storage. This is useful for workloads that don't fit well into the stateless, replicated model of Deployments or the numbered, stable model of StatefulSets.

Key features of the `Sandbox` CRD include:

*   **Stable Identity:** Each Sandbox has a stable hostname and network identity.
*   **Persistent Storage:** Sandboxes can be configured with persistent storage that survives restarts.
*   **Lifecycle Management:** The Sandbox controller manages the lifecycle of the pod, including creation, scheduled deletion, pausing and resuming.

### Extensions

The `extensions` module provides additional CRDs and controllers that build on the core `Sandbox` API to provide more advanced features.

*   `SandboxTemplate`: Provides a way to define reusable templates for creating Sandboxes, making it easier to manage large numbers of similar Sandboxes.
*   `SandboxClaim`: Allows users to create Sandboxes from a template, abstracting away the details of the underlying Sandbox configuration.
*   `SandboxWarmPool`: Manages a pool of pre-warmed Sandboxes that can be quickly allocated to users, reducing the time it takes to get a new Sandbox up and running.

## Architecture

agent-sandbox follows the Kubernetes controller pattern. Users create a Sandbox custom resource, and the controller manages the underlying runtime resources.

### Architecture Diagram

```mermaid
flowchart LR

    User[User]

    Claim[SandboxClaim]
    Template[SandboxTemplate]
    Sandbox[Sandbox]

    Pod[Pod]
    Runtime[Sandbox Runtime]

    WarmPool[SandboxWarmPool]

    subgraph Extensions[Extensions]
      Claim
      Template
      WarmPool
    end

    %% User paths
    User -->|creates| Sandbox
    User -->|creates| Claim

    %% Claim workflow
    Claim -->|references| Template
    Claim -->|adopts| Sandbox

    %% Pod handling
    Claim -->|adopts sandboxes from| WarmPool
    Sandbox -->|creates Pod| Pod

    %% Runtime
    Pod --> Runtime

    %% Warm pool
    WarmPool -->|pre-warms sandboxes| Sandbox
```

## Installation

### Core Components & Extensions

You can install the agent-sandbox controller and its CRDs with the following command.

```sh
# Replace "vX.Y.Z" with a specific version tag (e.g., "v0.1.0") from
# https://github.com/kubernetes-sigs/agent-sandbox/releases
export VERSION="vX.Y.Z"

# To install only the core components:
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml

# To install the extensions components:
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml
```

### Python SDK

To interact with the agent-sandbox programmatically, you can use the Python SDK. This client library provides a high-level interface for creating and managing sandboxes.

For detailed installation and usage instructions, please refer to the [Python SDK README](clients/python/agentic-sandbox-client/README.md).

## Configuration

For advanced scale and concurrency tuning (e.g., API QPS and worker counts), please see the [Configuration Guide](docs/configuration.md).

## Getting Started

Once you have installed the controller, you can create a simple Sandbox by applying the following YAML to your cluster:

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: my-sandbox
spec:
  podTemplate:
    spec:
      containers:
      - name: my-container
        image: <IMAGE>
```

This will create a new Sandbox named `my-sandbox` running the image you specify. You can then access the Sandbox using its stable hostname, `my-sandbox`.

For more complex examples, including how to use the extensions, please see the [examples/](examples/) and [extensions/examples/](extensions/examples/) directories.

## Motivation

Kubernetes excels at managing stateless, replicated applications (Deployments) and stable, numbered sets of stateful pods (StatefulSets). However, there's a growing need for an abstraction to handle use cases such as:

*   **Development Environments:** Isolated, persistent, network-accessible cloud environments for developers.
*   **AI Agent Runtimes:** Isolated environments for executing untrusted, LLM-generated code.
*   **Notebooks and Research Tools:** Persistent, single-container sessions for tools like Jupyter Notebooks.
*   **Stateful Single-Pod Services:** Hosting single-instance applications (e.g., build agents, small databases) needing a stable identity without StatefulSet overhead.

While these can be approximated by combining StatefulSets (size 1), Services, and PersistentVolumeClaims, this approach is cumbersome and lacks specialized lifecycle management like hibernation.

## Desired Sandbox Characteristics

We aim for the Sandbox to be vendor-neutral, supporting various runtimes. Key characteristics include:

*   **Strong Isolation:** Supporting different runtimes like gVisor or Kata Containers to provide enhanced security and isolation between the sandbox and the host, including both kernel and network isolation. This is crucial for running untrusted code or multi-tenant scenarios.
*   **Deep hibernation:** Saving state to persistent storage and potentially archiving the Sandbox object.
*   **Automatic resume:** Resuming a sandbox on network connection.
*   **Efficient persistence:** Elastic and rapidly provisioned storage.
*   **Memory sharing across sandboxes:** Exploring possibilities to share memory across Sandboxes on the same host, even if they are primarily non-homogeneous. This capability is a feature of the specific runtime, and users should select a runtime that aligns with their security and performance requirements.
*   **Rich identity & connectivity:** Exploring dual user/sandbox identities and efficient traffic routing without per-sandbox Services.
*   **Programmable:** Encouraging applications and agents to programmatically consume the Sandbox API.

## Roadmap

The current Roadmap can be found at [roadmap.md](roadmap.md).

## Community, Discussion, Contribution, and Support

This is a community-driven effort, and we welcome collaboration!

**Note on PR Velocity:** To maintain high velocity and keep our queues clean, this project uses stale PR management (30-day auto-stale and 15-day auto-close for inactive PRs) and allows maintainers to fast-track or take over approved community PRs. Please read our [Contributing Guidelines](CONTRIBUTING.md) for our full code review and PR policies.

### AI-Assisted Code Reviews (Experimental)

To help improve our review velocity, we are currently experimenting with AI-assisted code reviews, starting with GitHub Copilot as our automated first-pass reviewer. Here is the workflow:

1. Copilot will be assigned as the first reviewer of all open PRs (skipping PRs without a signed CLA)
1. After Copilot reviews are posted, the PR will be labeled `action-required: resolve-copilot-comments`
   * **⚠️ Important Contribution Note:** If you receive a code suggestion from Copilot in your PR, please don't directly apply suggestions via the GitHub UI. It will set Copilot as co-author and break the Kubernetes CLA requirements. For more information, read our [Contributing Guidelines](CONTRIBUTING.md). 
1. After all of Copilot reviews are marked resolved, the PR will be labeled `ready-for-review`
1. Maintainers will review `ready-for-review` PRs and provide final approval 

We actively welcome your feedback on the quality, relevance, and helpfulness of these automated reviews! As we iterate on this process, we also plan to evaluate and test different AI review tools to find the best fit for our project's workflow.

### Contact Us

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-apps)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-apps)

Please feel free to open issues, suggest features, and contribute code!

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k8s.io/community/contributors/guide/owners.md
[Creative Commons 4.0]: https://git.k8s.io/website/LICENSE
