# KEP-0694: Suspend and Resume for Agent Sandboxes Beta

> **Note:** This KEP is a subset of the [main KEP](https://github.com/kubernetes-sigs/agent-sandbox/pull/762/changes), which goes over the broader details of the Suspend and Resume API along with the Snapshot Provider. The motivation, use cases, and user personas for this feature are discussed in the main KEP.

<!-- toc -->
- [Current State (Alpha)](#current-state-alpha)
- [Goals for Beta](#goals-for-beta)
- [API for Triggering Suspend/Resume in Beta](#api-for-triggering-suspendresume-in-beta)
    - [1. API Aggregation Server (True Custom Subresources) - <em>Rejected</em>](#1-api-aggregation-server-true-custom-subresources---rejected)
    - [2. Ephemeral &quot;Action&quot; Custom Resources - <em>Rejected</em>](#2-ephemeral-action-custom-resources---rejected)
    - [3. The <code>spec.suspend</code> Boolean - <em>Rejected</em>](#3-the-specsuspend-boolean---rejected)
    - [4. The <code>spec.replicas</code> (The /scale Pivot) - <em>Rejected</em>](#4-the-specreplicas-the-scale-pivot---rejected)
    - [5. Suspension Gates (<code>spec.suspensionGates</code>) - <em>Rejected for Beta</em>](#5-suspension-gates-specsuspensiongates---rejected-for-beta)
    - [6. The <code>spec.mode</code> Enum - <em>Selected for Beta</em>](#6-the-specmode-enum---selected-for-beta)
- [Implementation Plan](#implementation-plan)
- [Migration Plan (Alpha to Beta)](#migration-plan-alpha-to-beta)
  - [Alternatives Considered for Migration](#alternatives-considered-for-migration)
<!-- /toc -->

## Current State (Alpha)

The current Sandbox API has `spec.replicas` field that controls the creation and deletion of Pods 
associated with the Sandbox. The allowed values are 0 and 1. `spec.replicas` is part of the `/scale` subresource which can be used by other systems like HPA or KEDA to auto scale the sandbox.

When the user patches `spec.replicas` to 0, the pod associated with the Sandbox is terminated. The
Sandbox CR still exists in the cluster.
When the user patches `spec.replicas` to 1, the pod is created and attached back to the Sandbox and. 

The `spec.replicas` field is being shoehorned for Suspend and Resume use-case. This is unintuitive for the users who explicitly want to take Suspend and Resume action on the Sandbox. 

## Goals for Beta

Implementing suspend and resume is complex. The full range of use cases is discussed in the 
primary KEP: https://github.com/kubernetes-sigs/agent-sandbox/pull/762/changes. To ensure a safe and manageable delivery, we are limiting the features to be implemented in Beta.

There is only one goal for Beta.

1. Provide a clean and fluent API for users to suspend and resume the Sandbox. 

## API for Triggering Suspend/Resume in Beta

To implement Suspend and Resume, the following architectural paths were evaluated:

#### 1. API Aggregation Server (True Custom Subresources) - *Rejected*
Instead of using standard CRDs, we could build and deploy a custom API Aggregation Server (using the `apiregistration.k8s.io` API) to support native HTTP endpoints like `POST /apis/agents.x-k8s.io/v1alpha1/namespaces/default/sandboxes/dev-42/suspend`.
* **Pros:** Offers a completely native, secure, and clean API. Enables fine-grained RBAC specifically for the `/suspend` action.
* **Cons:** Introduces massive operational complexity. Requires maintaining an active API Aggregator Pod, managing custom TLS certificates, and handling etcd storage manually.

#### 2. Ephemeral "Action" Custom Resources - *Rejected*
Simulating imperative actions by creating a lightweight, temporary CR (e.g., `SandboxAction`) whose sole purpose is to trigger the suspend/resume event.
* **Pros:** 100% standard CRD-compatible and leaves a historical audit trail.
* **Cons:** Creates resource churn in etcd and requires writing a garbage collector to delete these action objects after completion.

#### 3. The `spec.suspend` Boolean - *Rejected*
Using a simple boolean flag (`spec.suspend: true`) to control the lifecycle.
* **Pros:** Very simple and intuitive.
* **Cons:** Violates Kubernetes API conventions. As per API guidelines, booleans are discouraged for fields that might evolve to have more states in the future. As sandbox lifecycles grow, we may need states like `Archived` or `Hibernating`, which a simple boolean cannot support without causing schema conflicts.

#### 4. The `spec.replicas` (The /scale Pivot) - *Rejected*
Keeping the standard `spec.replicas` as is and overloading its behavior (e.g., `replicas: 0`) to trigger stateful hibernation instead of standard deletion.
* **Pros:** Retains native Kubernetes `/scale` subresource integration out of the box.
* **Cons:** 
  * Ecosystem Compatibility Risks: Native tools (HPA, KEDA) expect `replicas: 0` to result in clean-slate deletions.
  * Data-Loss Risk: Users routinely run `replicas=0` as a quick way to clean up resources. Silently saving memory dumps for "deleted" pods can quickly fill cloud storage buckets.

#### 5. Suspension Gates (`spec.suspensionGates`) - *Rejected for Beta*
Inspired by Pod Scheduling Gates, this approach introduces an array of gates (e.g., `suspensionGates: [{name: "user-intent"}]`). The Sandbox is forced to suspend if any gate is present, and resumes only when the list is empty.
* **Pros:**
  * **Multi-Entity Orchestration:** Works well when multiple actors (an end-user, a cluster scaler like Kueue, etc.) need to independently pause the workspace without conflicting or overwriting each other's intent.
* **Cons:**
  * **Unintuitive UX:** Requiring an end-user to add a specific string to an array (like `user-intent`) just to pause their workspace is awkward compared to a simple mode switch.
  * **Weak Validation:** Since it relies on arbitrary strings rather than enums, a typo from the user could cause the suspension to silently fail.
  * **Upstream Uncertainty:** This pattern is still being debated in upstream Kubernetes (e.g., kubernetes/kubernetes#121681) and hasn't been accepted yet, meaning it needs more time to be designed well before we rely on it as a core mechanic.

#### 6. The `spec.mode` Enum - *Selected for Beta*
This approach introduces a strongly-typed Enum (`spec.mode: Running | Suspended`) block to dictate how the suspension is physically handled. 
* **Pros:**
  * **API Compliant:** Aligns perfectly with Kubernetes API conventions by avoiding rigid booleans and preventing mutually exclusive field conflicts.
  * **Semantic Meaning:** Clearly states the sandbox's execution state while leaving room for future expansion (e.g., `Stopped`).
  * **Scale Constraints:** Matches the singleton nature of 1-to-1 interactive workspaces perfectly.

**Alternative Enum Naming Considered (and Rejected):**
* **"State" Named Fields (`spec.lifecycleState`, `spec.powerState`, `spec.desiredState`):** Rejected because it blurs the semantic line between `spec` and `status`. In Kubernetes conventions, `spec` is strictly where users declare desired intent, while `status` is where the system reports the actual observed state. Placing a field named "State" inside the `spec` block creates semantic ambiguity.
* **Nested Suspension Object (`spec.suspension.mode`):** Rejected because it results in a highly unintuitive UX. Reading `suspension: { mode: Running }` is semantically contradictory and confusing for developers to parse (i.e., "how can the suspension mode be running?").

**API Specification (Go Types):**

```go
// SandboxMode defines the desired operational state of the Sandbox.
// +kubebuilder:validation:Enum=Running;Suspended
type SandboxMode string

const (
	// SandboxModeRunning indicates the sandbox should be actively running.
	SandboxModeRunning SandboxMode = "Running"
	// SandboxModeSuspended indicates the sandbox should be suspended.
	SandboxModeSuspended SandboxMode = "Suspended"
)

type SandboxSpec struct {
	// ... existing fields ...

	// mode specifies the desired operational state of the Sandbox.
	// Defaults to Running if not specified.
	// +kubebuilder:default=Running
	// +optional
	Mode SandboxMode `json:"mode,omitempty"`
}
```

## Implementation Plan

The implementation focuses on updating the Sandbox controller's reconciliation loop to respect `spec.mode`:

1. **Reconciliation of `Suspended` Mode:**
   * When the controller observes `spec.mode: Suspended`, it explicitly triggers a graceful deletion of the underlying Pod.
   * The controller will explicitly *not* delete any attached PersistentVolumeClaims (PVCs) or stable network identities (Services).
   
2. **Reconciliation of `Running` Mode:**
   * When the controller observes `spec.mode: Running` (which is the default), it checks if an active Pod exists.
   * If no Pod exists, it constructs a new Pod using the Sandbox's original `spec.podTemplate`.
   * The new Pod is seamlessly bound to the existing PVCs, ensuring state is retained.

## Migration Plan (Alpha to Beta)

Upgrading from Alpha to Beta is designed to be seamless for end-users, relying heavily on native Kubernetes API defaulting mechanisms to prevent disruption.

1. **CRD Update:** The cluster administrator applies the updated `Sandbox` CRD containing the new `spec.mode` Enum field.
2. **Defaulting Behavior:** Because the `spec.mode` field is defined with `// +kubebuilder:default=Running`, all existing Sandbox resources in the cluster will automatically be treated as `Running` by the API server.
3. **Controller Upgrade:** When the Sandbox controller is updated to the Beta version, it reads existing Sandbox resources as `Running`. As long as their Pods exist, the controller will take no destructive action.

### Alternatives Considered for Migration

* **Manual Migration Script:** We considered providing a script to manually patch all existing Sandbox objects to explicitly include `spec.mode: Running` before starting the Beta controller. 
  * *Rejected:* This introduces unnecessary operational friction for cluster operators. Relying on Kubernetes' built-in CRD defaulting logic (`+kubebuilder:default=Running`) guarantees a zero-touch migration for existing resources.
* **Intercepting `spec.replicas=0` via Mutating Webhook:** We explored adding a webhook that intercepts updates setting `spec.replicas: 0` and transparently translates them into `spec.mode: Suspended`. 
  * *Rejected:* This violates explicit user intent. In the Kubernetes ecosystem, scaling to `0` implies a full teardown of compute and its associated ephemeral components. Suspending, however, implies explicit state and identity retention. Conflating the two would confuse users and could lead to accidental, orphaned storage leaks.
```
