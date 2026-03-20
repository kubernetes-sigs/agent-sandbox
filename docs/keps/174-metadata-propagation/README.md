# KEP-0174: Label and Annotation Propagation to Sandbox Pods

---

## Summary

This KEP proposes a standardized mechanism to clarify how labels and annotations propagate from top-level user requests (`SandboxClaim`) down to the final compute resources (`Pod`). This ensures consistency for observability, billing, and workload management while maintaining the homogeneity and performance of `SandboxWarmPools`.

## Motivation

Currently, metadata fields exist across several disconnected layers (Sandbox, SandboxTemplate, SandboxWarmpool, and SandboxClaim), with no dedicated fields to pass user-specific metadata directly to the Pod in the base design. There is a critical need to allow users to "personalize" homogeneous instances (e.g., for cost attribution or session management) without triggering resource re-creations or "template explosion."

## Proposal

The proposal introduces new fields to `SandboxClaim` and `Sandbox` to allow unique metadata to pass down the stack while keeping `SandboxTemplate` and `Warmpool` unchanged.

### User Stories (Optional)

#### Use Case 1: Custom Metadata Propagation (Identification)
Allows users to add unique, user-defined labels and annotations to a Sandbox and its Pod to distinguish specific workloads for observability and cost attribution (billing).

#### Use Case 2: Stateful Session Management (Pod Snapshots)
Managed programmatically via the Agent Sandbox Python SDK to save and restore the exact execution state of an agent. This enables "save game" functionality and rapid pause/resume cycles for cost optimization.

**Note**: In a Warmpool scenario, for the case involving the restoration of dedicated snapshots within a Warmpool, refer to [Issue 208](https://github.com/kubernetes-sigs/agent-sandbox/issues/208). The scope of the current implementation is strictly limited to updating Sandbox and Pod metadata (labels and annotations) without triggering resource re-creation. Any metadata that imposes functional control necessitating a Pod restart will be addressed separately under [Issue 208](https://github.com/kubernetes-sigs/agent-sandbox/issues/208).

### High-Level Design

The model allows `SandboxClaim` to pass unique metadata down the stack:
1.  **SandboxClaim**: Introduce `additionalSandboxMetadata` and `additionalPodMetadata`.
2.  **Sandbox**: Introduce `additionalPodMetadata` to store values from the Claim.
3.  **Sandbox**: Add `SandboxClaim`’s `additionalSandboxMetadata` into its own labels and annotations.

#### Safety Principle: No Overrides
To ensure predictability, the controller will not allow overrides. If a key exists in both the Template and the Claim with different values, the request will be rejected with an error.

#### API Changes

```go
// sandbox_types.go:
// PodMetadata exists. Other fields are new.
type PodMetadata struct {
    Labels      map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`
    Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

type SandboxSpec struct {
    // ...
    AdditionalPodMetadata PodMetadata `json:"additionalPodMetadata,omitempty"`
}

// sandboxclaim_types.go:
type AdditionalSandboxMetadata struct {
    Labels      map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`
    Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

type SandboxClaimSpec struct {
    // ...
    AdditionalSandboxMetadata AdditionalSandboxMetadata `json:"additionalSandboxMetadata,omitempty"`
    // Another option is to create AdditionalPodMetadata instead of depending on sandboxv1alpha1.
    AdditionalPodMetadata     sandboxv1alpha1.PodMetadata `json:"additionalPodMetadata,omitempty"`
}
```

#### Implementation Guidance

1. The SandboxClaim controller propagates additional metadata to the assigned Sandbox resource.
2. The Sandbox controller merges these values with the base template and injects them into the final Pod.

##### Scenario A: Cold Start (No Warmpool)
This scenario occurs when a user requests a sandbox environment without utilizing a pre-provisioned pool.

1.  **New Pod (Creation)**: If the `Sandbox` is newly created and no Pod exists, the `Sandbox` controller merges the base `podTemplate` from the `SandboxTemplate` with the `additionalPodMetadata` from the `Sandbox` spec. The Pod is created with these labels and annotations already present.
2.  **Pod Exists (Metadata Update)**: If a Pod was already created via a Cold Start and the `SandboxClaim` is subsequently updated with new metadata, the `Sandbox` controller performs an **in-place update** of the Pod's `metadata.labels` and `metadata.annotations`. This update does not trigger a Pod restart.

##### Scenario B: Warmpool
This scenario involves the use of a `SandboxWarmPool` to provide rapid resource assignment.

1.  **New Pod Claim (Adoption/Injection)**: When a `SandboxClaim` is first assigned that "adopts" a Sandbox from the Warmpool, the `SandboxClaim` controller performs an **in-place update** to inject the `additionalSandboxMetadata` into the Sandbox's labels and annotations and add the `additionalPodMetadata`. This achieves sub-millisecond dispatch latency without restarting the container or re-creating the resource.
2.  **After Pod Claimed (Metadata Update)**: If the `SandboxClaim` is updated after the Sandbox has already been adopted from the Warmpool, the `SandboxClaim` controller watches the changes and performs an **in-place update** to inject the `additionalSandboxMetadata` into the Sandbox's labels and annotations and add the `additionalPodMetadata` to reflect the changes, ensuring continuous consistency without resource re-creation.

## Scalability

The design prioritizes system performance and scalability by keeping `SandboxWarmPool` and `SandboxTemplate` static:
*   **Maintaining Homogeneity**: Keeps pool resources interchangeable for random assignment.
*   **Preventing Template Explosion**: Avoids creating thousands of nearly identical templates for unique user sessions.
*   **Performance & Latency**: Achieves sub-millisecond dispatch latency by injecting metadata during the "adoption" phase instead of re-creating resources.

## Alternatives (Optional)

The alternative of modifying the `SandboxTemplate` for every request was considered but rejected because it would trigger resource re-creations and lead to template management overhead. Instead, for consistent labels across all Pods in a pool, users should modify the `SandboxTemplate` directly as introduced in [Issue 347](https://github.com/kubernetes-sigs/agent-sandbox/issues/347).
