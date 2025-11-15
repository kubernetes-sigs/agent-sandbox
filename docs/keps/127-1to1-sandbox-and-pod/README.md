# KEP-127: Sandbox Warmpool Directly Create Sandboxes

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
  - [High-Level Design](#high-level-design)
    - [API Changes](#api-changes)
    - [Implementation Guidance](#implementation-guidance)
- [Scalability](#scalability)
- [Alternatives (Optional)](#alternatives-optional)
<!-- /toc -->

## Summary

To ensure 1 to 1 mapping between a sandbox and a pod, we can let sandbox warmpool create sandboxes instead of only pods.

## Motivation

https://github.com/kubernetes-sigs/agent-sandbox/pull/115 enabled sandboxes to get pods from a warmpool instead of trying to starting up pods by themselves, which drastically reduced the latency for sandboxes to get ready. However it introduced a side effect. Before warmpool, a sandbox's pod would always have the same name as the sandbox; after warmpool, a sandbox's pod name would be  diffrent from the sandbox's name if the sandbox adopted a pod from a warmpool. This makes it hard to keep track of what pods are owned by a sandbox, reducing the system's maintainability and readbility. For example, if a sandbox adopted several pods due to a bug in the sytem, we might not even notice it because the sandbox only keeps reference to one pod in its annotation. Although we already have a annotation on both the `Sandbox` and the `pod` to represent this pairing, there is no easy way to tell if another pod has been incorrectly adopted by a sandbox because the name of the pod is not fixed.

## Proposal

Instead of letting sandbox warmpool create pods to be later adopted by sandboxes, we can directly create sandboxes with pods. This way we can make the sandbox and its pod use the same name. As a result, a sandbox's pod would always have the same name as the sandbox, presenting a consistent behavior no matter if a sandbox is cold-started or adopted from a warmpool.

## New User Flow

The Admin would create a `SandboxWarmpool` along with a `SandboxTemplate`. The `SandboxWarmpool` controller would then create `Sandbox`, and the `Sandbox` controller would create `Pod`.

The End User would create a `SandboxClaim`, and the `SandboxClaim` controller would first attempt to find if there is any available `Sandbox` matching the `SandboxClaim`'s template. If yes, then the end user would get the ownership of that `Sandbox`. If not, the `SandxboxClaim` controller would create a new `Sandbox`.

## Existing Sandboxes

If an Admin has already created a `SandboxWarmpool` in the current version and has a pool of pods, then the Admin needs to recreate the `SandboxWarmpool`.

If an End User has already obtained a `Sandbox` that got a pod from the current version `SandboxWarmpool`, the End User will continue to have the `Sandbox` with its old pod.

Note: due to the need to maintain the End User's existing `Sandbox`, we need to keep the `pod-name` annotation on the `Sandbox` until we are sure that no End User still has a `Sandbox` with a pod from the current version of `SandboxWarmpool`.

### High-Level Design

Currently sandbox warmpool controller extracts the pod spec from the sandbox template. Since the sandbox template has information for the sandbox too, we can use the sandbox template to create the sandboxes.

When a sandbox claim is created, the sandbox claim controller can try to adopt a sandbox directly, instead of creating a sandbox first, then trying to adopt a pod from the warmpool.

The rest of the logic should remain unaffected, except the code in sandbox controller can be simplified since the controller reference and label setting can be done when the sandbox warmpool is creating sandboxes.

#### API Changes

No change needed.

#### Implementation Guidance

[createPoolPod](https://github.com/kubernetes-sigs/agent-sandbox/blob/2332dd153221cbb9299184432ed4a363bdd190c7/extensions/controllers/sandboxwarmpool_controller.go#L203) now should be `createPoolSandbox`, which only creats a sandbox, and let the `Sandbox Controller` create the pod.

[createSandbox](https://github.com/kubernetes-sigs/agent-sandbox/blob/2332dd153221cbb9299184432ed4a363bdd190c7/extensions/controllers/sandboxclaim_controller.go#L242) now should try to adopt a sandbox from sandbox warm pool before creating a sandbox.

## Scalability

Since a sandbox CR is light and mapped to a pod, the change doesn't affect scalability of the system.

## Alternatives (Optional)

We could keep the sandbox warm pool's current design to produce pods. In sandbox claim controller, we could also create sandboxes of the same name as the adopted pod. This would also ensure the sandbox's name is always the same as its pod's name. It requires less code change, but is also less intuitive since the name "sandboxwarmpool" seems to indicate that it's a pool for sandboxes instead of pods.