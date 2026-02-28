# KEP-174: Agent Sandbox Label and Metadata propagation

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

This proposal introduces a mechanism to propagate metadata (Labels and Annotations) from a `SandboxClaim` to the created `Sandbox` and its underlying `Pod`. This is achieved by adding two new fields, `sandboxMetadata` and `podMetadata`, to the `SandboxClaim` CRD.

## Motivation

Currently, there is limited clarity and control over how labels and annotations are propagated to the underlying Sandbox and Pods from various sources like `SandboxClaim`, `SandboxTemplate`, etc.

Users often need to attach dynamic metadata to the resources created by a `SandboxClaim` for various purposes, such as:
- **Cost Allocation**: Propagating labels like `cost-center` or `team` to Pods for billing.
- **Observability**: Adding annotations for logging or monitoring sidecars.
- **Operational Control**: Using labels to trigger specific behaviors in other controllers or admission webhooks.

By explicitly defining `sandboxMetadata` and `podMetadata` in the `SandboxClaim`, we provide a clear and direct path for users to influence the metadata of the resulting resources.

## Proposal

We propose adding two optional fields to the `SandboxClaim` specification:
1.  `sandboxMetadata`: Defines labels and annotations to be applied to the `Sandbox` object.
2.  `podMetadata`: Defines labels and annotations to be applied to the `Sandbox`'s underlying `Pod`.


For both `sandboxMetadata` and `podMetadata`, the resulting labels and annotations will be a key-level merge of the metadata from the `SandboxTemplate` and the `SandboxClaim`. Both sources will be combined to form the final set of metadata.

Furthermore, the overall merging strategy for existing resources will be a union of the existing metadata on the live object and the desired metadata from the template and claim. This ensures that manually added labels or annotations, or those added by other controllers (e.g., sidecar injectors), are preserved.

### High-Level Design

The `SandboxClaim` controller will be responsible for reading these new fields and applying them during the creation or reconciliation of the `Sandbox`.

When a `Sandbox` is created or updated:
1.  **Sandbox Metadata**: The controller will merge labels and annotations from the `SandboxTemplate.metadata`, `SandboxClaim.spec.sandboxMetadata`, and the existing `Sandbox.metadata` (if it exists).
2.  **Pod Metadata**: The controller will merge `SandboxClaim.spec.podMetadata` into the `podTemplate` metadata from the `SandboxTemplate`. The `Sandbox` controller then propagates this merged metadata to the actual `Pod`. If the `Pod` already exists, any new labels/annotations from the `Sandbox`'s template should be merged into the `Pod.metadata`.

#### API Changes

We will modify the `SandboxClaimSpec` in `extensions/api/v1alpha1/sandboxclaim_types.go`.

We will introduce a shared `Metadata` struct in `extensions/api/v1alpha1` containing `Labels` and `Annotations`.

```go
// Metadata contains labels and annotations.
type Metadata struct {
	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects.
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`

	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

// SandboxClaimSpec defines the desired state of Sandbox
type SandboxClaimSpec struct {
    // ... existing fields ...

    // SandboxMetadata defines the metadata (labels and annotations) to be propagated to the Sandbox.
    // +optional
    SandboxMetadata *Metadata `json:"sandboxMetadata,omitempty"`

    // PodMetadata defines the metadata (labels and annotations) to be propagated to the Sandbox's underlying Pod.
    // +optional
    PodMetadata *Metadata `json:"podMetadata,omitempty"`
}
```

#### Implementation Guidance

1.  **Type Definition**: Define the `Metadata` struct in `extensions/api/v1alpha1/sandboxclaim_types.go` (or a shared file in that package).
2.  **Controller Logic**: Update `extensions/controllers/sandboxclaim_controller.go`.
    *   In the `Reconcile` loop, when constructing the `Sandbox` object:
        *   Initialize `Sandbox.ObjectMeta.Labels` and `Annotations` merging `SandboxTemplate` context (if any) and `SandboxClaim.Spec.SandboxMetadata`.
        *   Initialize `Sandbox.Spec.PodTemplate.ObjectMeta.Labels` and `Annotations` merging `SandboxTemplate.Spec.PodTemplate` and `SandboxClaim.Spec.PodMetadata`.
3.  **Merge Strategy**:
    *   The desired state is calculated by performing a union of keys from the `SandboxTemplate` and the `SandboxClaim`.
    *   When applying to an object (Sandbox or Pod):
        *   If the object does not exist, initialize with the calculated desired state.
        *   If the object exists, merge the desired labels and annotations into the existing metadata maps on a key-by-key basis. Do NOT replace the entire map.
    *   Standard merging order (later sources augment earlier ones): `Existing Labels` + `SandboxTemplate` + `SandboxClaim`.
    *   Ensure system-managed labels/annotations (e.g. those used by the controller for tracking) are preserved or reapplied after the merge.

#### Metadata Mutability

The metadata defined in `SandboxClaim` is mutable. When these fields are updated:
1.  The `SandboxClaim` controller will detect the change during its next reconciliation loop.
2.  It will calculate the new desired state (merging `SandboxTemplate` and the updated `SandboxClaim`).
3.  It will apply the changes to the live `Sandbox` and `Pod` resources.
4.  Keys removed from the `SandboxClaim` (and not present in the `SandboxTemplate`) will be removed from the target resources to ensure the live state reflects the desired metadata configuration.

## Scalability

This change involves only string map manipulations and metadata updates.
- **API Server Storage**: Minimal increase in object size due to additional labels/annotations.
- **Controller Performance**: Negligible impact. Map merging is cheap.
- **Watch Cache**: Standard overhead for label/annotation changes.

No significant scalability concerns.