<!-- toc -->
- [KEP-0208: Resolving Mutually Exclusive Fields in SandboxClaim for Beta](#kep-0208-resolving-mutually-exclusive-fields-in-sandboxclaim-for-beta)
  - [Motivation](#motivation)
  - [User Personas](#user-personas)
  - [Preferred Solution](#preferred-solution)
      - [Pure WarmPoolRef and remove <code>TemplateRef</code> field.](#pure-warmpoolref-and-remove-templateref-field)
      - [Impact and Migration](#impact-and-migration)
      - [Controller Implementation Details](#controller-implementation-details)
  - [Alternatives Considered](#alternatives-considered)
    - [Option 1: Keep Schema as is and perform API validation](#option-1-keep-schema-as-is-and-perform-api-validation)
    - [Option 2: Pure TemplateRef and remove WarmPoolPolicy field.](#option-2-pure-templateref-and-remove-warmpoolpolicy-field)
    - [Option 3: Union Model (oneOf)](#option-3-union-model-oneof)
<!-- /toc -->
# KEP-0208: Resolving Mutually Exclusive Fields in SandboxClaim for Beta

## Motivation

We found a few Beta blockers after going through the API review: https://github.com/kubernetes-sigs/agent-sandbox/issues/740. The two issues were related to the existing `WarmPoolPolicy` spec field in `SandboxClaim`.

These issues introduce the following problems for end-users:

1. **Ambiguous Configurations (Mutually Exclusive Fields):** Currently, a user must provide a `TemplateRef` but can also optionally provide a `WarmPoolPolicy` while claiming a Sandbox. If a user specifies a targeted warm pool that was provisioned using a *different* template than the one requested in `TemplateRef`, the API allows it, but it creates a conflicting state. The user experiences unpredictable behavior because the system doesn't clearly reject or prioritize the conflicting directives.
2. **Naming Collisions (Capitalize Warm Pool Constants):** The current API for `WarmPoolPolicy` uses lowercase string constants (`none`, `default`) to dictate warm pool behavior. If a user creates a custom `SandboxWarmPool` resource and happens to name it "none" or "default", the controller cannot distinguish between the user's intent to use their custom pool versus the system's reserved policy. By capitalizing the constants (`None`, `Default`), we align with Kubernetes API conventions and prevent these routing collisions.

Before we even implement #2, I think we should decide if it is even worth having the field in the first place in the Beta API. Please note `WarmPoolPolicy` has been added very recently (April 2026). 

## User Personas

1. **Platform Administrator:** Responsible for setting up the underlying infrastructure, including defining `SandboxTemplate`s and creating `SandboxWarmPool`s. They want to control the size of warm pools, manage resource costs, and offer specific "tiers" or "environments" (e.g., `ml-workload`, `standard-dev`) for developers to consume.
2. **End User / Agentic Workflow:** The consumer of the sandbox (either a human developer or an automated AI agent). They create `SandboxClaim` resources to dynamically request execution environments. They prioritize low latency (getting a sandbox immediately) and a simple, unambiguous API contract.

## Preferred Solution

#### Pure WarmPoolRef and remove `TemplateRef` field.

The user only provides a warm pool reference in `SandboxClaim` spec. The concept of "template" is hidden from the end-user API when claiming a sandbox. The controller looks at the specified warm pool to adopt a sandbox. 

1. If the warmpool's `spec.replicas` is 0, the controller falls back to a cold start using the `spec.templateRef` configured in the warmpool. 
2. If a user provides custom environment variables, the controller will implicitly bypass the warm pool and provision a Sandbox from scratch based on the template configured in the warmpool.
3. If the warmpool has been deleted by the cluster admin, the claim controller will throw a permanent failure error.
4. If the warmpool's `spec.replicas` > 0 and no `spec.env` is provided by the user, the Sandbox is adopted from the warmpool specified in `spec.warmpoolRef`. 

```go
type SandboxClaimSpec struct {
	// WarmPoolRef targets the specific pre-warmed infrastructure pool to check out from.
	// +required
	WarmPoolRef SandboxWarmPoolRef `json:"warmPoolRef"`
}

// SandboxWarmPoolRef references a SandboxWarmPool.
type SandboxWarmPoolRef struct {
	// name of the SandboxWarmPool
	// +required
	Name string `json:"name,omitempty"`
}
```

**Pros:**
* **Clean Schema Contract**: The cleanest possible developer experience for teams using pre-warmed infrastructure. The user says, "Give me an environment out of the premium `data-science` pool," and they don't have to manage underlying templates. 
* **Predictable Infrastructure Allocation**: Simplifies allocation calculations by mapping claims strictly into finite pool groupings. 
* **Avoids Ambiguity**: Resolves the mutually exclusive fields problem elegantly.

**Cons:**
* **Platform Team Bottleneck**: If the platform team wants to share a sandbox template to dev which should only be used for cold starts, they still need to create a warm pool with size 0 (a level of indirection). *This is a design choice we make to avoid duplicating template ref*

#### Impact and Migration

Adopting the preferred solution (removing the `TemplateRef` field) simplifies the API but introduces shifts in how users and the system interact. The impact and migration paths for the primary scenarios are:

*   **Scenario A: Targeting a specific warm pool (`warmpool: "my-large-pool"`)**
    * **Impact:** Users will specify `warmPoolRef.name: "my-large-pool"`. The `TemplateRef` and `WarmPoolPolicy` fields are completely removed.
    * **Migration:** Replace `templateRef` and `warmpool` policy fields with the single `warmPoolRef` field.

*   **Scenario B: Explicitly requesting a cold start (`warmpool: "none"`) from a template**
    * **Impact:** The `warmpool: "none"` option is no longer supported directly on the claim.
    * **Migration:** If the users want a cold start of the sandbox from the template, the Platform admins must first create a `SandboxWarmPool` (with `replicas: 0`) referencing that template for devs to use. 

*   **Scenario C: Default behavior / Implicit warm pool discovery (`warmpool: "default"` or omitted)**
    *   **Impact:** Users can no longer rely on the controller to automatically discover and select an arbitrary warm pool based solely on a template reference. The implicit "default" discovery mechanism is removed.
    *   **Migration:** Users must explicitly specify the exact warm pool they want to draw from using `warmPoolRef.name`.

*   **Scenario D: Environment Variable Injection (Customizing the Sandbox)**
    *   **Impact:** Users cannot set `spec.env` with `warmpool: "none"` since the option is no longer supported directly in the claim.
    *   **Migration:** If users inject custom `Env` variables, the controller will implicitly recognize this and provision a cold start sandbox. The user does not need to specify any additional flags.

#### Controller Implementation Details

The controller logic will change as follows:

1. **Warm Pool Existence Check:**
   The controller first looks up the `SandboxWarmPool` referenced in the claim's `warmPoolRef`. If the pool doesn't exist, it errors out immediately.

2. **Implicit Cold Start Detection (Bypassing the Queue):**
   If `len(claim.Spec.Env) > 0`, the controller immediately bypasses the warm pool queue. It fetches the `SandboxTemplate` associated with the pool and routes the request directly to a cold start.

3. **Queue Evaluation and Adoption:**
   Because the claim no longer natively provides a template reference, the internal in-memory queue (`SimpleSandboxQueue`) will be refactored to use the `SandboxWarmPool`'s Name (or UID) as its primary routing key instead of the `TemplateRef` hash. When a claim is eligible:
   * The controller performs a direct O(1) lookup against the specific warm pool's queue.
   * If the queue has available Sandboxes, it pops one off the queue, assigns ownership to the claim, and updates the adoption labels (bypassing slow API list operations).
   * If the queue is empty (all warm Sandboxes are claimed, initializing, or the pool size is 0), the controller dynamically provisions a new `Sandbox` from scratch directly using the `SandboxTemplate` associated with the pool.

## Alternatives Considered 

### Option 1: Keep Schema as is and perform API validation 

In this solution, we still allow both fields to be present in the schema, but we perform a validation check against provided template name and the template name of the warmpool either at the sandbox claim controller or in admission webhook to reject conflicting user intents.

This way we retain the functionality to allow configuring specific warmpools for claim, default controller behavior to pick sandbox from available warmpool and also allow claims without warmpools. 

```go
// WarmPoolPolicy describes the policy for using warm pools.
// It can be one of the following:
type WarmPoolPolicy string

const (
	// WarmPoolPolicyNone indicates that no warm pool should be used.
	// A fresh sandbox will always be created.
	WarmPoolPolicyNone WarmPoolPolicy = "none"

	// WarmPoolPolicyDefault indicates the default behavior: select from all
	// available warm pools that match the template. This is the default behavior
	// if warmpool is not specified.
	WarmPoolPolicyDefault WarmPoolPolicy = "default"
)

type SandboxClaimSpec struct {
	// TemplateRef specifies the template to create the sandbox from.
	// +required
	TemplateRef SandboxTemplateRef `json:"sandboxTemplateRef,omitempty"`

	// warmpool specifies the warm pool policy for sandbox adoption.
	// - "none": Do not use any warm pool, always create fresh sandboxes
	// - "default": Use default behavior, select from all matching warm pools (default)
	// - A warm pool name: Select only from the specified warm pool (e.g., "large-pool", "standard-pool")
	// +optional
	// +kubebuilder:default=default
	WarmPool *WarmPoolPolicy `json:"warmpool,omitempty"`
}
```

**Pros:**
* **Explicit User Intent**: Clearly distinguishes between a user who wants a raw, custom cold start and a user who needs a sub-millisecond, pre-warmed sandbox.

**Cons:**
* **Schema Redundancy**: The end-user has to provide both the template and the pool name, which can look slightly repetitive since the warm pool technically already knows its template.
* **Active Code Overhead**: The eng team must maintain the validation logic inside the controller reconcile loop (or a validating webhook) to catch and reject mismatched specs.

### Option 2: Pure TemplateRef and remove WarmPoolPolicy field.

The user only provides a template reference in `SandboxClaim` spec. The concept of "warm pools" is entirely hidden from the end-user API. The controller automatically looks under the hood to see if a matching warm pool has an available sandbox; if it does, it grabs it, and if it doesn't, it falls back to a cold start. The concept of warm-pool is essentially an implementation detail for the sandbox claim controller. 

```go
type SandboxClaimSpec struct {
	// Warm pool routing happens entirely implicitly behind the scenes based on the template's configuration.
	// +required
	TemplateRef SandboxTemplateRef `json:"sandboxTemplateRef,omitempty"`
}

// SandboxTemplateRef references a SandboxTemplate.
type SandboxTemplateRef struct {
	// name of the SandboxTemplate
	// +required
	Name string `json:"name,omitempty"`
}
```

**Pros:**
* **Clean End-User UX**: The user-facing API is lean. End-users just ask for an application runtime and do not concern themselves with the operational mechanics. 
* **Zero Configuration Conflicts**: Impossible for a user to create a mismatch (e.g., asking for Template A but pointing to a pool running Template B).

**Cons:**
* **Loss of Priority Control:** Power users cannot explicitly guarantee their workload hits a premium, ultra-large warm pool. Everything relies on the controller's internal scheduling logic. 
* **Opaque Debugging**: If a user gets a slow "cold start," it is harder for them to diagnose why from looking at their own manifest, since the pool state is completely abstracted away.

### Option 3: Union Model (oneOf)

In this model, we allow the users to choose between a template and a warmpool but not both. 

1. If a user provides a template source, they are given the option to either adopt a sandbox from the available warmpool matching the template name or skip the warmpool.
2. If a user provides a warmpool source, the sandbox is adopted from the warmpool name specified by the user. 

This introduces complications to watch both `SandboxTemplate` and `SandboxWarmPool`. 

```go
type SandboxClaimSpec struct {
	// Source defines where to provision the sandbox from. Exactly one field must be populated.
	// +unionDiscriminator
	Source SandboxSource `json:"source"`
}

type SandboxSource struct {
	// Template gives the user the option to adopt a Sandbox from any matching warmpool.
	// +optional
	Template *TemplateSource `json:"template,omitempty"`

	// WarmPool explicitly gives the name of the pool to adopt the Sandbox from.
	// +optional
	WarmPool *WarmPoolSource `json:"warmPool,omitempty"`
}

type TemplateSource struct {
	// Name of the SandboxTemplate.
	// +required
	Name string `json:"name"`
}

type WarmPoolSource struct {
	// Name explicitly gives the name of the pool to adopt the Sandbox from.
	// +required
	Name string `json:"name"`
}
```

**Pros:**
* **Explicit User Intent**: Clearly distinguishes between a user who wants a standard warmpool or a user who needs a sub-millisecond, pre-warmed sandbox.

**Cons:**
* **Cache Scanning Overhead**: Every single time any template is touched, the controller must perform an unbound iteration through every warm pool and every claim in the cache. In a large cluster with thousands of claims, this spikes controller CPU and memory utilization, slowing down the reconciliation queue for everyone.
* **Split-Brain Code Paths**: The reconciliation loop must constantly branch its logic (if claim.Spec.Source.Template != nil vs else if claim.Spec.Source.WarmPool != nil). This doubling of execution states makes unit testing, status reporting, and state management twice as bug-prone.
