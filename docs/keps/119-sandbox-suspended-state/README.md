# Sandbox Suspended State with Status Condition

<!-- toc -->
- [Motivation](#motivation)
- [Condition Hierarchy](#condition-hierarchy)
    - [1. <code>Suspended</code>](#1-suspended)
    - [2. <code>Ready</code> (Root Condition)](#2-ready-root-condition)
- [Controller Implementation](#controller-implementation)
- [Usage Examples](#usage-examples)
- [Alternatives Considered](#alternatives-considered)
    - [1. Retaining the Legacy <code>status.phase</code> Field](#1-retaining-the-legacy-statusphase-field)
    - [2. Utilizing a Single &quot;Ready&quot; Condition](#2-utilizing-a-single-ready-condition)
<!-- /toc -->

## Motivation

We currently expose a single `Ready` condition for Sandboxes. Because Sandbox acts as an "aggregation" object, a common convention is that `Ready` should be `True` when all child objects (Pod, Service, PVC) are applied to the cluster and are themselves `Ready`. However, relying purely on the `Ready` condition makes it harder to observe certain lifecycle transitions—specifically, when a Sandbox is in the process of suspending or resuming. While a controller or user can observe that a Sandbox should be suspended from `spec.operatingMode` and verify `status.observedGeneration` to know the controller has acted on the spec, they lack a clear signal indicating whether the suspension process is actively happening or if it has fully completed without deeply inspecting the child objects.

Adding the `Suspended` condition explicitly solves this visibility gap for both suspend and resume cycles. This KEP proposes keeping the condition present and toggling it between `True` and `False`, so clients can reliably observe the most recent `lastTransitionTime` for suspend/resume. Additionally, it lays the API groundwork for future enhancements like "soft pause". In a future soft pause scenario, a Sandbox's execution might be paused (e.g., freezing processes via the container runtime) without terminating the underlying Pod. An explicit `Suspended` condition provides a stable, consistent API abstraction to represent this halted state to users and automation, regardless of the underlying technical implementation.

Furthermore, there is a growing need to represent a more diverse and granular status in the Agent Sandbox UI. A richer set of conditions allows the UI to provide clear, user-friendly feedback about the exact stage of a Sandbox's lifecycle, rather than just a simple binary Ready or Not Ready state.

## Condition Hierarchy

The Sandbox state is determined by multiple distinct layers. 

#### 1. `Suspended`
This condition explicitly tracks whether the sandbox environment is currently paused or hibernated. 
* **Status: True** – The Sandbox is no longer actively executing workloads due to a suspension request. The `Reason` field specifies how it is suspended (`PodTerminated`).
* **Status: False** – The Sandbox is active, running, or in a transient lifecycle phase (`PodTerminating`, `PodResuming`, `PodRunning`). `lastTransitionTime` is updated only when the status flips between `True` and `False`, and is retained while the status remains `False` even if the reason/message changes.
* **Ready Impact:** The moment a suspension is requested (e.g., `spec.operatingMode: Suspended`), the `Ready` condition immediately transitions to `False` with the reason `SandboxSuspended`. It holds this reason through the entire suspending phase and remains that way once fully suspended.

#### 2. `Ready` (Root Condition)
The overarching signal for whether all child objects are successfully applied to the cluster and are themselves `Ready`.

---

## Condition Dependency Matrix

The controller evaluates the hierarchy top-down. The `Suspended` condition remains present across all operational states after its initial transition, utilizing explicit, state-driven reason strings to drive UI and automation clarity.

| Scenario | Suspended Status | Suspended Reason | Pod Object State | **Ready Status** | Ready Reason | Description / UI Visual Context |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Provisioning** | `False` | `Provisioning` | nil (Initial build) | **`False`** | `DependenciesNotReady` | Transient State: Sandbox is provisioning for the first time. |
| **Suspending** | `False` | `PodTerminating` | non-nil (Deleting) | **`False`** | `SandboxSuspended` | Transient State: Sandbox suspension requested. Active workloads are being de-provisioned. |
| **Suspended** | `True` | `PodTerminated` | nil (Fully deleted) | **`False`** | `SandboxSuspended` | Stable State: Pod has been successfully terminated. Sandbox is fully suspended. |
| **Resuming** | `False` | `PodResuming` | nil (Recreating) | **`False`** | `DependenciesNotReady` | Transient State: Sandbox resumption requested. Recreation of underlying workload is in progress. |
| **Operational** | `False` | `PodRunning` | non-nil (Active) | **`True`** | `DependenciesReady` | Stable State: Sandbox is active and the underlying Pod is running. |

## Controller Implementation

The state transitions map cleanly to a level-based evaluation of desired state (`OperatingMode`) against physical state (Pod existence):

```go
func (r *SandboxReconciler) computeSuspendedCondition(
    sandbox *sandboxv1beta1.Sandbox, 
    pod *corev1.Pod,
) *metav1.Condition {
    
    desiresSuspension := sandbox.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended
    existingSuspendedCond := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionSuspended))

    // Initialize the condition container (keep the condition present once initialized)
    suspended := &metav1.Condition{
        Type:               string(sandboxv1beta1.SandboxConditionSuspended),
        ObservedGeneration: sandbox.Generation,
    }

    if desiresSuspension {
        if pod == nil {
            // Stable State: Fully Suspended
            suspended.Status = metav1.ConditionTrue
            suspended.Reason = sandboxv1beta1.SandboxReasonSuspendedPodTerminated
            suspended.Message = "Pod has been successfully terminated. Sandbox is fully suspended."
        } else {
            // Transient State: In the process of scaling down
            suspended.Status = metav1.ConditionFalse
            suspended.Reason = "PodTerminating"
            suspended.Message = "Sandbox suspension requested. Active workloads are being de-provisioned."
        }
    } else {
        // The spec wants it RUNNING
        if pod == nil {
            // Differentiate between a brand-new Sandbox and one waking up from suspension
            suspended.Status = metav1.ConditionFalse
            if existingSuspendedCond != nil && existingSuspendedCond.Status == metav1.ConditionTrue {
                suspended.Reason = "PodResuming"
                suspended.Message = "Sandbox resumption requested. Recreation of underlying workload is in progress."
            } else {
                suspended.Reason = "Provisioning"
                suspended.Message = "Sandbox is provisioning."
            }
        } else {
            // Stable State: The pod exists and is active.
            suspended.Status = metav1.ConditionFalse
            suspended.Reason = "PodRunning"
            suspended.Message = "Sandbox is active and the underlying Pod is running."
        }
    }

    return suspended
}
```

## Usage Examples

Standard Kubernetes tooling can now interact with the sandbox state natively and reliably using both states of the condition:

```bash
# Block a CI/CD pipeline until the sandbox is fully operational and ready to take traffic
kubectl wait --for=condition=Ready sandbox/my-env

# Wait until the sandbox has successfully suspended / hibernated
kubectl wait --for=condition=Suspended=True sandbox/my-env --timeout=60s

# Wait until the sandbox has successfully resumed and is no longer suspended
kubectl wait --for=condition=Suspended=False sandbox/my-env --timeout=60s

# Determine why a sandbox is not ready (e.g. DependenciesNotReady, SandboxSuspended)
kubectl get sandbox my-env -o custom-columns=READY_REASON:.status.conditions[?(@.type=="Ready")].reason

# View the status of the Suspended condition explicitly
kubectl get sandbox my-env -o custom-columns=SUSPENDED:.status.conditions[?(@.type=="Suspended")].status

# Check exactly when the sandbox was last resumed or suspended
kubectl get sandbox my-env -o jsonpath='{.status.conditions[?(@.type=="Suspended")].lastTransitionTime}'
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
