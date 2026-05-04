# Sandbox State Management with Conditions

<!-- toc -->
- [Motivation](#motivation)
- [Condition Hierarchy](#condition-hierarchy)
    - [1. <code>Initialized</code>](#1-initialized)
    - [2. <code>Suspended</code>](#2-suspended)
    - [3. <code>Ready</code> (Root Condition)](#3-ready-root-condition)
    - [Why "Initialized" matters](#why-initialized-matters)
    - [Terminal States: Expired & Terminating](#terminal-states-expired--terminating)
- [Usage Examples](#usage-examples)
- [Consumer Compatibility](#consumer-compatibility)
- [Alternatives Considered](#alternatives-considered)
    - [1. Retaining the Legacy <code>status.phase</code> Field](#1-retaining-the-legacy-statusphase-field)
    - [2. Utilizing a Single "Ready" Condition](#2-utilizing-a-single-ready-condition)
<!-- /toc -->

Currently, `Sandbox.Status` relies primarily on a single `Ready` condition.

* When the Sandbox is ready to take traffic, `Ready` is set to True.

* When the Sandbox is not ready to take traffic, `Ready` is set to False.

* When the Sandbox is Suspended (replicas set to 0), the underlying Pod is deleted. In this state, the `Ready` condition typically defaults to `False` or becomes stale. There is no explicit machine-readable field to distinguish between a "Suspended" state (intended) and "Pod not present" (unintended) during first time apply for example.

To align with Kubernetes API standards and address the previous limitations of using `phase` for Sandbox as discussed in https://github.com/kubernetes-sigs/agent-sandbox/pull/121, this proposal uses a `status.conditions` model instead of adding the deprecated `status.phase` field. This model establishes three conditions: `Initialized`, `Ready` and `Suspended`.

## Motivation

We currently expose a single `Ready` condition for Sandboxes. Because Sandbox acts as an "aggregation" object, a common convention is that `Ready` should be `True` when all child objects (Pod, Service, PVC) are applied to the cluster and are themselves `Ready`. 

However, relying purely on the `Ready` condition makes it harder to observe certain lifecycle transitions—specifically, when a Sandbox is in the process of scaling down (suspending). While a controller or user can observe that a Sandbox should be suspended from `spec` and verify `status.observedGeneration` to know the controller has acted on the spec, they lack a clear signal indicating whether the scale-down process is actively happening or if it has fully completed without deeply inspecting the child objects.

Adding the `Suspended` condition explicitly solves this visibility gap for scale-down. Additionally, adding `Initialized` makes the first-time setup of persistent infrastructure observable separately from the Pod.

## Condition Hierarchy

The Sandbox state is determined by three distinct layers. 

#### 1. `Initialized`
This represents the **First-Time Setup** of the sandbox. Once the persistent environment is established, this condition becomes `True` and remains `True` for the remainder of the sandbox's lifecycle.
* **Scope:** Successful creation of the **Service** and **PersistentVolumeClaim (PVC)** (if configured).
* **Persistence:** This remains `True` during suspension, confirming that the network identity and persistent storage are preserved even when the Pod is deleted.

#### 2. `Suspended`
This condition explicitly tracks the scale-down process of the Sandbox.
* **Behavior:** When `True`, the **Pod** has been successfully terminated to conserve cluster resources, meaning the scale-down is complete. When `False`, it implies the Sandbox is either fully operational or actively in the process of scaling down.
* **Ready Impact:** Similar to a Deployment of size 0, a fully suspended Sandbox is not intrinsically "broken." It just means it's not ready to take the traffic.

#### 3. `Ready` (Root Condition)
The overarching signal for whether all child objects are successfully applied to the cluster and are themselves `Ready`.

---

## Condition Dependency Matrix

The controller evaluates the hierarchy top-down. The "Gap" between `Initialized` and `Ready` represents the time taken to schedule and start the agent Pod.

| Scenario | `Initialized` | `Suspended`  | Pod | **`Ready` (Root)** | Ready Reason |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Provisioning** | `False` | `Unknown` | None | **`False`** | `SandboxInitializing` |
| **Pod Starting** | `True` | `False` | Pending | **`False`** | `SandboxPodInitializing` |
| **Operational** | `True` | `False` | Running & Ready | **`True`** | `SandboxReady` |
| **Suspended** | `True` | `True` | None | **`True`** | `SandboxSuspended` |
| **Unresponsive** | `True` | `False` | Unknown | **`Unknown`** | `SandboxUnresponsive` |
| **Expired** | `True` | `Any` | Any | **`False`** | `SandboxExpired` |
| **Terminating** | `True` | `Any` | Any | **`False`** | `SandboxDeleting` |

#### Why "Initialized" matters
By isolating the one-time setup of Service into the `Initialized` condition, we provide a convenient top-level summary of the infrastructure state. While machines typically only care if an object is "ready" or "broken", humans can rely on the `Message` field for context, and advanced client-side tooling can traverse `ownerRefs` to find specific component failures. Surfacing `Initialized` explicitly acts as an optimization, saving clients from having to build that traversal logic just to verify if the persistent environment has been successfully established.

#### Terminal States: Expired & Terminating
`Expired` and `Terminating` are treated as terminal overrides. Once a sandbox reaches its TTL or a deletion request is received, the overarching `Ready` condition transitions to `False` regardless of the sub-condition statuses. Please note `Expired` and `Terminating` aren't capabilities; they are the end of the object's life which is why they are **not** represented as **explicit conditions**.

## Usage Examples

Standard Kubernetes tooling can now interact with the sandbox state natively:

```bash
# Block a CI/CD pipeline until the sandbox is fully ready
kubectl wait --for=condition=Ready sandbox

# Verify if infrastructure is provisioned before using Sandbox
kubectl wait --for=condition=Initialized sandbox

# Determine if a sandbox is down due to a crash or a suspend
kubectl get sandbox my-env -o custom-columns=READY:.status.conditions[?(@.type=="Ready")].reason
```

## Consumer Compatibility

To prevent breaking external consumers (CLI tools, CI scripts, or monitoring):

1. **Status Contract:** The `Status` field of the `Ready` condition remains the primary API contract for functional logic. Any consumer relying on `Status: True/False` will experience zero disruption.
2. **Reason and Message Field Usage:** The `Reason` field provides machine-readable strings intended for programmatic consumption (e.g., `SandboxSuspended`, `SandboxExpired`). The `Message` field provides human-readable diagnostic details.
3. **Migration Path:** If existing automation relies on specific `Reason` strings to infer state, it is recommended to migrate that logic to observe the `Status` field or the specific sub-conditions (`Initialized`, `Suspended`) introduced in this version.

## Alternatives Considered

#### 1. Retaining the Legacy `status.phase` Field
One option considered was to continue using the single-string `status.phase` field (e.g., `Pending`, `Running`, `Suspended`).

* **Cons:**
    * **Reduced Visibility:** A single string cannot represent concurrent states. It is cumbersome to distinguish between a sandbox that is both "Successfully Initialized" and "Administratively Suspended" simultaneously.
    * **API Standards:** The Kubernetes API conventions explicitly deprecate the `Phase` pattern for new projects. Adhering to `Conditions` ensures compatibility with modern ecosystem tools like `kubectl wait`.
    * **Logic Complexity:** As the sandbox evolves, the state machine would require an exponential number of strings to represent every possible combination of infrastructure and application health.

#### 2. Utilizing a Single "Ready" Condition
We considered using only the `Ready` condition and overloading the `Reason` field to communicate the state of the infrastructure and the Pod.

* **Cons:**
    * **Brittle Client Logic:** While the `Reason` field is intended for machine consumption, forcing clients to handle a complex list of enum-like strings (e.g., `SandboxSuspended` vs `PodProvisioning`) within a single condition's `Reason` recreates the issues of the deprecated `Phase` field.
    * **Ambiguity in Suspension:** If only a `Ready` condition exists, setting it to `False` during suspension provides no programmatic signal that the persistent data (PVC) and network identity (Service) are still safely intact.
