# Sandbox Suspended State with Status Condition

<!-- toc -->
- [Motivation](#motivation)
- [Condition Hierarchy](#condition-hierarchy)
    - [1. <code>Suspended</code>](#1-suspended)
    - [2. <code>Ready</code> (Root Condition)](#2-ready-root-condition)
- [Usage Examples](#usage-examples)
- [Alternatives Considered](#alternatives-considered)
    - [1. Retaining the Legacy <code>status.phase</code> Field](#1-retaining-the-legacy-statusphase-field)
    - [2. Utilizing a Single &quot;Ready&quot; Condition](#2-utilizing-a-single-ready-condition)
<!-- /toc -->

## Motivation

We currently expose a single `Ready` condition for Sandboxes. Because Sandbox acts as an "aggregation" object, a common convention is that `Ready` should be `True` when all child objects (Pod, Service, PVC) are applied to the cluster and are themselves `Ready`. However, relying purely on the `Ready` condition makes it harder to observe certain lifecycle transitions—specifically, when a Sandbox is in the process of scaling down (suspending). While a controller or user can observe that a Sandbox should be suspended from `spec` and verify `status.observedGeneration` to know the controller has acted on the spec, they lack a clear signal indicating whether the scale-down process is actively happening or if it has fully completed without deeply inspecting the child objects.

Adding the `Suspended` condition explicitly solves this visibility gap for scale-down. Additionally, it lays the API groundwork for future enhancements like "soft pause". In a future soft pause scenario, a Sandbox's execution might be paused (e.g., freezing processes via the container runtime) without terminating the underlying Pod. An explicit `Suspended` condition provides a stable, consistent API abstraction to represent this halted state to users and automation, regardless of the underlying technical implementation.

Furthermore, there is a growing need to represent a more diverse and granular status in the Agent Sandbox UI. A richer set of conditions allows the UI to provide clear, user-friendly feedback about the exact stage of a Sandbox's lifecycle, rather than just a simple binary Ready or Not Ready state.

## Condition Hierarchy

The Sandbox state is determined by multiple distinct layers. 

#### 1. `Suspended`
This condition explicitly tracks the suspension process of the Sandbox.
* **Behavior:** When `True`, the Sandbox is no longer actively executing workloads due to a suspension request. The `Reason` field specifies *how* it is suspended (e.g., `PodTerminated`, `ProcessesFrozen`). When `False`, it implies the Sandbox is actively in the process of suspending. If not suspending, this condition is omitted.
* **Ready Impact:** The moment a suspension is requested (e.g. `replicas: 0`), the `Ready` condition immediately transitions to `False` with the reason `SandboxSuspended`. It holds this reason through the entire suspending phase and remains that way once fully suspended.

#### 2. `Ready` (Root Condition)
The overarching signal for whether all child objects are successfully applied to the cluster and are themselves `Ready`.

---

## Condition Dependency Matrix

The controller evaluates the hierarchy top-down.

| Scenario | `Suspended` | Suspended Reason | Pod Phase | **`Ready` (Root)** | Ready Reason |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Provisioning** | None | None | None | **`False`** | `DependenciesNotReady` |
| **Pod Starting** | None | None | Pending | **`False`** | `DependenciesNotReady` |
| **Operational** | None | None | Running & Ready | **`True`** | `DependenciesReady` |
| **Suspending** | `False` | `PodNotTerminated` | Running / Terminating | **`False`** | `SandboxSuspended` |
| **Suspended** | `True` | `PodTerminated` | None | **`False`** | `SandboxSuspended` |

## Usage Examples

Standard Kubernetes tooling can now interact with the sandbox state natively:

```bash
# Block a CI/CD pipeline until the sandbox is fully operational and ready to take traffic
kubectl wait --for=condition=Ready sandbox/my-env

# Wait until the sandbox has successfully scaled down
kubectl wait --for=condition=Suspended=True sandbox/my-env

# Determine why a sandbox is not ready (e.g. DependenciesNotReady, SandboxExpired)
kubectl get sandbox my-env -o custom-columns=READY_REASON:.status.conditions[?(@.type=="Ready")].reason

# View the status of the Suspended condition explicitly
kubectl get sandbox my-env -o custom-columns=SUSPENDED:.status.conditions[?(@.type=="Suspended")].status
```

## Alternatives Considered

#### 1. Retaining the Legacy `status.phase` Field
One option considered was to continue using the single-string `status.phase` field (e.g., `Pending`, `Running`, `Suspended`).

* **Cons:**
    * **Inability to Represent Concurrent States:** A single string cannot represent multiple facets of the lifecycle simultaneously. For example, if we wanted to represent a Sandbox that is "Hibernated" but also "Updating Network", we would end up with a combinatorial explosion of compound phase strings.
    * **API Standards:** The Kubernetes API conventions explicitly deprecate the `Phase` pattern for new projects. Adhering to `Conditions` ensures compatibility with modern ecosystem tools like `kubectl wait`.
    * **Logic Complexity:** As the sandbox evolves, the state machine would require an exponential number of strings to represent every possible combination of infrastructure and application health.

#### 2. Utilizing a Single "Ready" Condition
We considered using only the `Ready` condition and overloading the `Reason` field to communicate the state of the infrastructure and the Pod.

* **Cons:**
    * **Lack of Granularity for Future Suspend States:** A single `Ready: False` status cannot granularly distinguish between different types of suspension. Future features like "freeze" (pausing container processes), "hibernate" (snapshotting memory to disk), or traditional "scale-to-zero" would all appear identically as "not ready". Extracting `Suspended` into its own explicitly tracked condition gives us the design space to represent these nuanced operational states in the future.
    * **Ambiguity in Suspension:** If only a `Ready` condition exists, setting it to `False` during suspension provides no programmatic signal that the network identity (Service) are still safely intact.
