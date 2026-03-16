# Sandbox State Management with Conditions

We need to expose `status` method for Sandboxes in the Python SDK: https://github.com/kubernetes-sigs/agent-sandbox/pull/280. The current outstanding implementation checks the `Pod` status instead of `Sandbox` and transforms the Pod status into the Sandbox status on the client side. This is not an ideal implementation. We should expose `status` of the Sandbox on the controller side as a first class field. 

To align with Kubernetes API standards and address the previous limitations of using `phase` for Sandbox as discussed in https://github.com/kubernetes-sigs/agent-sandbox/pull/121, this proposal uses a `status.conditions` model instead of adding the deprecated `status.phase` field. This model establishes three conditions: `Initialized`, `Ready` and `Suspended`.

## Motivation

The pattern of using `status.phase` is deprecated in Kubernetes. Phase was essentially a state-machine enumeration field, that contradicted system-design principles and hampered evolution, since adding new enum values breaks backward compatibility: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties. 

## Condition Hierarchy

The Sandbox state is determined by three distinct layers. 

#### 1. `Initialized`
This represents the **First-Time Setup** of the sandbox. Once the persistent environment is established, this condition becomes `True` and remains `True` for the remainder of the sandbox's lifecycle.
* **Scope:** Successful creation of the **Service** and **PersistentVolumeClaim (PVC)** (if configured).
* **Persistence:** This remains `True` during suspension, confirming that the network identity and persistent storage are preserved even when the Pod is deleted.

#### 2. `Suspended`
This represents the user's desired operational state.
* **Behavior:** When `True`, the **Pod** is terminated to conserve cluster resources.
* **Ready Impact:** Acts as a logical circuit breaker; if `Suspended` is `True`, `Ready` must be `False`.

#### 3. `Ready` (Root Condition)
The overarching signal for whether the sandbox is currently usable. It is derived from the layers below it.

---

## Condition Dependency Matrix

The controller evaluates the hierarchy top-down. The "Gap" between `Initialized` and `Ready` represents the time taken to schedule and start the agent Pod.

| Scenario | `Initialized` | `Suspended`  | Pod | **`Ready` (Root)** | Ready Reason |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Provisioning** | `False` | `Unknown` | None | **`False`** | `SandboxInitializing` |
| **Pod Starting** | `True` | `False` | Pending | **`False`** | `PodProvisioning` |
| **Operational** | `True` | `False` | Running & Ready | **`True`** | `SandboxReady` |
| **Suspended** | `True` | `True` | None/Terminating | **`False`** | `SandboxSuspended` |
| **Unresponsive** | `True` | `False` | Unknown | **`Unknown`** | `SandboxUnresponsive` |
| **Expired** | `True` | `Any` | Any | **`False`** | `SandboxExpired` |
| **Terminating** | `True` | `Any` | Any | **`False`** | `SandboxDeleting` |

#### Why "Initialized" matters
By isolating the one-time setup of Service into the `Initialized` condition, we provide users with high-fidelity feedback:
* If **`Initialized` is False**, the issue is likely a platform or cloud provider error (e.g., failed to provide stable identity).
* If **`Initialized` is True but `Ready` is False**, the issue is likely an application error (e.g., ImagePullBackOff or CrashLoopBackOff).

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
2. **Reason Field Usage:** The `Reason` field is strictly diagnostic. We will be updating these strings to provide higher fidelity (e.g., `Suspended`, `Expired`). 
3. **Migration Path:** If existing automation relies on specific `Reason` strings, it is recommended to migrate that logic to observe the `Status` field or the specific sub-conditions (`Provisioned`, `Suspended`) introduced in this version.

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
    * **Brittle Client Logic:** The `Reason` field is intended for human consumption, not machine logic. Forcing clients to parse strings like `Suspended` vs `Provisioning` within the `Reason` field makes automation scripts fragile.
    * **Ambiguity in Suspension:** If only a `Ready` condition exists, setting it to `False` during suspension provides no programmatic signal that the persistent data (PVC) and network identity (Service) are still safely intact.
    * **Diagnostic Difficulty:** Without the `Initialized` condition, a user or machine cannot distinguish between a "Platform/Infrastructure Failure" (e.g., failed to create a Service) and an "Application Failure" (e.g., the Agent Pod is crashing).