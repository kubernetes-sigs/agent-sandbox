# Agent Sandbox

**agent-sandbox enables easy management of isolated, stateful, singleton workloads, ideal for use cases like AI agent runtimes.**

This project is developing a `Sandbox` Custom Resource Definition (CRD) and controller for Kubernetes, under the umbrella of [SIG Apps](https://github.com/kubernetes/community/tree/master/sig-apps). The goal is to provide a declarative, standardized API for managing workloads that require the characteristics of a long-running, stateful, singleton container with a stable identity, much like a lightweight, single-container VM experience built on Kubernetes primitives.

## Installation

You can install the agent-sandbox controller and its CRDs with the following command.

```sh
export VERSION="vX.Y.Z" # Replace with your desired version from https://github.com/kubernetes-sigs/agent-sandbox/releases

# To install only the core components:
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/manifest.yaml
# To install extensions components:
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/extensions.yaml
```

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
*   **Memory sharing across sandboxes:** Exploring possibilities to share memory across Sandboxes on the same host, even if they are primarily non-homogenous. This capability is a feature of the specific runtime, and users should select a runtime that aligns with their security and performance requirements.
*   **Rich identity & connectivity:** Exploring dual user/sandbox identities and efficient traffic routing without per-sandbox Services.
*   **Programmable:** Encouraging applications and agents to programmatically consume the Sandbox API.

## Community, Discussion, Contribution, and Support

This is a community-driven effort, and we welcome collaboration!

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-apps)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-apps)

Please feel free to open issues, suggest features, and contribute code!

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k8s.io/community/contributors/guide/owners.md
[Creative Commons 4.0]: https://git.k8s.io/website/LICENSE
