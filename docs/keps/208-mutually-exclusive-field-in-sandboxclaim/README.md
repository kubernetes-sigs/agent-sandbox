<!-- toc -->
- [KEP-0208: Resolving Mutually Exclusive Fields in SandboxClaim for Beta](#kep-0208-resolving-mutually-exclusive-fields-in-sandboxclaim-for-beta)
  - [Motivation](#motivation)
  - [Preferred Solution](#preferred-solution)
      - [Pure TemplateRef and remove <code>WarmPoolPolicy</code> field. <em>Preferred</em>](#pure-templateref-and-remove-warmpoolpolicy-field-preferred)
      - [Impact and Migration](#impact-and-migration)
      - [Controller Implementation Details (Implicit Warm Pool Adoption)](#controller-implementation-details-implicit-warm-pool-adoption)
  - [Alternatives Considered](#alternatives-considered)
    - [Option 1: Keep Schema as is and perform API validation](#option-1-keep-schema-as-is-and-perform-api-validation)
    - [Option 2: Pure WarmPoolRef and remove <code>TemplateRef</code> field.](#option-2-pure-warmpoolref-and-remove-templateref-field)
    - [Option 3: Union Model (oneOf)](#option-3-union-model-oneof)
<!-- /toc -->
# KEP-0208: Resolving Mutually Exclusive Fields in SandboxClaim for Beta

## Motivation

We found a few Beta blockers after going through the API review: https://github.com/kubernetes-sigs/agent-sandbox/issues/740. The two issues were related to the existing `WarmPoolPolicy` spec field in `SandboxClaim`.

These issues introduce the following problems for end-users:

1. **Ambiguous Configurations (Mutually Exclusive Fields):** Currently, a user must provide a `TemplateRef` but can also optionally provide a `WarmPoolPolicy` while claiming a Sandbox. If a user specifies a targeted warm pool that was provisioned using a *different* template than the one requested in `TemplateRef`, the API allows it, but it creates a conflicting state. The user experiences unpredictable behavior because the system doesn't clearly reject or prioritize the conflicting directives.
2. **Naming Collisions (Capitalize Warm Pool Constants):** The current API for `WarmPoolPolicy` uses lowercase string constants (`none`, `default`) to dictate warm pool behavior. If a user creates a custom `SandboxWarmPool` resource and happens to name it "none" or "default", the controller cannot distinguish between the user's intent to use their custom pool versus the system's reserved policy. By capitalizing the constants (`None`, `Default`), we align with Kubernetes API conventions and prevent these routing collisions.

Before we even implement #2, I think we should decide if it is even worth having the field in the first place in the Beta API. Please note `WarmPoolPolicy` has been added very recently (April 2026). 

## Preferred Solution

#### Pure TemplateRef and remove `WarmPoolPolicy` field. *Preferred*

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
* **Cleanest End-User UX:** The user-facing API is incredibly lean. End-users just ask for an application runtime and do not concern themselves with the operational mechanics.
* **Zero Configuration Conflicts**: Impossible for a user to create a mismatch (e.g., asking for Template A but pointing to a pool running Template B).

**Cons:**
* **Loss of Priority Control**: Power users cannot explicitly guarantee their workload hits a premium, ultra-fast warm pool. Everything relies on the controller's internal scheduling logic. *Although, if the users need this feature today they can do it by creating a different template name per warmpool*.
* **Opaque Debugging**: If a user gets a slow "cold start," it is harder for them to diagnose why from looking at their own manifest, since the pool state is completely abstracted away. 

#### Impact and Migration

Adopting the preferred solution (removing the `WarmPoolPolicy` field) simplifies the API but introduces shifts in how users and the system interact. The impact and migration paths for the three primary scenarios are:

*   **Scenario A: Targeting a specific warm pool (`warmpool: "my-fast-pool"`)**
    *   **Impact:** Users can no longer explicitly target a specific warm pool from the claim if multiple pools use the same template. The controller's selection becomes non-deterministic to the user.
    *   **Migration:** Users must adopt a 1:1 mapping between `SandboxTemplate` and `SandboxWarmPool` (e.g., creating `SandboxTemplate-fast` and `SandboxTemplate-slow`). The user dictates the pool implicitly by requesting the specific template.

*   **Scenario B: Explicitly requesting a cold start (`warmpool: "none"`)**
    *   **Impact:** Users testing initialization logic or requiring a strictly fresh environment can no longer bypass warm pools using a `SandboxClaim` if a matching pool exists.
    *   **Migration:** The user must bypass the `SandboxClaim` API entirely and directly provision a `Sandbox` resource to guarantee a cold start. If a customer really need to provide a support for disabling a warmpool, we can provide an optional `spec.disableWarmpool` option to support this usecase later.

*   **Scenario C: Environment Variable Injection (Customizing the Sandbox)**
    *   **Impact:** Custom environment variables cannot be injected into an already-running warm pool Sandbox. Currently, the controller enforces this by rejecting claims with `Env` vars unless they explicitly opt out of warm pools.
    *   **Migration:** The presence of custom environment variables (`len(claim.Spec.Env) > 0`) will now act as an *implicit* cold-start signal. The controller will automatically bypass the warm pool queue and route the request to provision a fresh Sandbox, removing a leaky abstraction and improving the user experience.


#### Controller Implementation Details (Implicit Warm Pool Adoption)

The controller logic will change as follows:

1. **Implicit Cold Start Detection (Bypassing the Queue):**
   Before touching any queues, the controller first inspects the `SandboxClaim` spec. If the claim contains custom environment variables (`len(claim.Spec.Env) > 0`), it knows it cannot inject these into an already-running pod. The controller immediately bypasses the warm pool queue and routes the request to a cold start.

2. **Querying the Cache by Template:**
   If the claim is eligible for adoption, the controller no longer looks up a specific pool name. Instead, it queries its local cache (using an indexer on the `.spec.sandboxTemplateRef.name` field) for *all* `SandboxWarmPool` resources that match the claim's `TemplateRef`.

3. **Queue Evaluation and Adoption:**
   Based on the cache query results, the controller executes the following logic:
   * **No matching pool found:** The controller proceeds to a cold start, creating a new `Sandbox` directly from the `SandboxTemplate`.
   * **One matching pool found:** The controller checks the pool's queue of available, unadopted Sandboxes.
     * If the queue has available Sandboxes, it pops one off the queue, adds the adoption labels/owner references linking it to the claim, and updates the pool's status.
     * If the queue is empty (all warm Sandboxes are claimed or initializing), the controller falls back to a cold start to satisfy the claim immediately rather than waiting.
   * **Multiple matching pools found (Edge Case):** Because users can no longer explicitly specify a pool name, multiple warm pools might reference the same template. The controller will use a deterministic tie-breaker (e.g., picking the pool with the highest number of ready replicas) to decide which queue to pop from first.

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
	// - A warm pool name: Select only from the specified warm pool (e.g., "fast-pool", "secure-pool")
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

### Option 2: Pure WarmPoolRef and remove `TemplateRef` field. 

Strictly speaking the existence of `SandboxClaim` is very closely tied to `SandboxWarmPool`. Claiming a sandbox without a warmpool is the same as creating a new Sandbox from scratch. We also currently cannot claim a sandbox from a warmpool if it is in a suspended state. `Suspend` and `Resume` can only work in `Sandbox` and has no meaning in the context of a `SandboxClaim` as well. So it is worth thinking if it actually makes sense to tie `SandboxClaim` and `SandboxWarmPool` together.

To do this, we can have the `SandboxClaim` watch against the `SandboxWarmPool` instead of the `SandboxTemplate` and we remove the `SandboxTemplateRef` field entirely from the claim. This means the `SandboxClaim` now entirely depends on `SandboxWarmPool`. A sandbox can now only be claimed from a warmpool. To retain the ability for a fallback cold-start, we have to go through two hoops to figure out the template ref associated with the warmpool to create a sandbox from scratch.

To support the other scenario, i.e create a sandbox easily from a template which isn't configured to any warmpool, we can create a new CRD like `SandboxTemplateClaim` which allows the user for easy creation of Sandboxes. Although this comes with it's own overhead to manage the controller and API surface area expansion.

```go
type SandboxClaimSpec struct {
	// WarmPoolRef targets the specific pre-warmed infrastructure pool to check out from.
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
* **Clean Schema Contract**: The cleanest possible developer experience for teams using pre-warmed infrastructure. The user says, "Give me an environment out of the premium data-science pool," and they don't have to manage underlying templates.
* **Predictable Infrastructure Allocation:** Simplifies allocation calculations by mapping claims strictly into finite pool groupings.

**Cons:**
* **The "Cold Start" Dependency Risk**: If `premium-pool` is accidentally hard-deleted by an administrator, the SandboxClaims are blinded. Because the template isn't written directly on the claim, the controller cannot fall back to a dynamic cold start out of empty air if the pool resource itself ceases to exist in the cluster state.

* **Cascade Latency**: Reconciliations are delayed by an extra step. A template update must finish updating the warm pool before the claim controller even gets notified that its sandbox needs to be rolled over.

### Option 3: Union Model (oneOf)

In this model, we allow the users to choose between a template and a warmpool but not both. 

1. If a user provides a template source, they are given the option to either adopt a sandbox from the available warmpool matching the template name or skip the warmpool.
2. If a user provides a warmpool source, the sandbox is adopted from the warmpool name specified by the user. 

This introduces complications to watch both `SandboxClaim` and `SandboxWarmPool`. 

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
