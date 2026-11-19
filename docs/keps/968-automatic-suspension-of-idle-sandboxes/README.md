# KEP-968: Automatic Suspension of Idle Sandboxes

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [High-Level Design](#high-level-design)
    - [API Changes](#api-changes)
- [Scalability](#scalability)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

This KEP covers the idleness portion of [#968](https://github.com/kubernetes-sigs/agent-sandbox/issues/968): deciding that a Sandbox is idle, suspending it, and resuming it when traffic returns. A Sandbox opts in with an `autoSuspension` field among its lifecycle fields. `sandbox-router` tells the Sandbox controller when the Sandbox is in use. Once the Sandbox has been idle for its timeout, the controller deletes the backing Pod and keeps the volumes, as it does for `operatingMode: Suspended`. It leaves `spec.operatingMode` at `Running`. Resume is minimal: the next request through the router gets a 503 that the client must retry, and causes the controller to create the Pod again.

## Motivation

A `Running` Sandbox keeps its Pod until a user suspends it ([KEP-694](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/keps/694-kep-for-suspend-and-resume-for-beta/README.md)) or `spec.shutdownTime` passes. No component decides that a Sandbox is idle, so a Sandbox that nobody is using holds its resources. The [roadmap](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/roadmap.md) plans to "Automatically suspend inactive sandboxes and resume them upon traffic or API invocation".

The review of [#1160](https://github.com/kubernetes-sigs/agent-sandbox/pull/1160) asked for "a short KEP" that covers "the activity signal, who updates it, how often it's written, and how long-running work with no inbound traffic counts as activity". The design below marks where it answers each. The review of [#1296](https://github.com/kubernetes-sigs/agent-sandbox/pull/1296) sketched a reconciler that decides from `spec.operatingMode` and `status.lastActivityTime` "without mutating spec". This KEP builds on that sketch and on #1296's activity endpoint, and adds authentication to the endpoint.

### Goals

- Define what counts as activity and which component records it.
- Suspend an idle Sandbox without the controller writing `spec`, and define how the Sandbox resumes.
- Change no behavior in a cluster whose operator leaves the feature off.

### Non-Goals

- Holding the request that resumes a Sandbox until its Pod is Ready ([#968](https://github.com/kubernetes-sigs/agent-sandbox/issues/968)).
- Deleting a Sandbox that has stayed suspended ([#849](https://github.com/kubernetes-sigs/agent-sandbox/issues/849)).
- A default on `SandboxTemplate`.

## Proposal

### User Stories

- As a platform operator with one long-lived Sandbox per user, I want the controller to delete an unused Sandbox's Pod and keep its volume, so that idle users do not hold compute.
- As a user whose agent runs a long build after I disconnect, I want the workload to report that it is busy, so that the controller does not suspend it mid-build.

### High-Level Design

```text
router: sees activity for Sandbox S (a request, an open connection, or a busy answer)
  → reports S to the controller's activity endpoint
controller: checks the caller, then records status.lastActivityTime
  → no new activity for the timeout: records the suspension in status, deletes the Pod, keeps the volumes
router: request for suspended S → 503 with Retry-After, reports S
controller: sees the new activity → creates the Pod again
```

This KEP prefers to leave an idle Sandbox running rather than suspend one that someone is using, arguing that there is greater harm in suspending in-progress work. Where the controller's record of activity is missing, it leaves the Sandbox running.

**The controller decides from status.** `spec.operatingMode` records what the user asked for, so it stays `Running` through an automatic suspension. The Sandbox reports `Ready=False` with reason `SandboxAutoSuspended`, and the `Suspended` condition follows the same sequence as for `operatingMode: Suspended`, so `kubectl wait --for=condition=Suspended` works for both.

**The router observes activity** (#1160: "the activity signal"). `sandbox-router` counts a request that passed its authorization check, and a request or connection that is still open. The router already carries this traffic, so counting it changes no client, SDK or workload. Traffic that bypasses the router, such as a port-forward to the Pod, does not count.

**The router reports, and only the controller writes** (#1160: "who updates it"). The router sends reports to an endpoint on the controller, and the controller writes `status.lastActivityTime`. The router gets no write access to Sandboxes, and a report can only refresh that time. The router does gain a read-only watch on Sandboxes, to learn each Sandbox's `busyProbe` and whether the Sandbox is suspended. Today the router reads only Pods. The endpoint is off by default. Where the CNI does not enforce NetworkPolicies, a NetworkPolicy alone would let any Pod send reports. The router presents a ServiceAccount token that names the controller as its audience and lasts 10 minutes. The controller checks it with a TokenReview and then a SubjectAccessReview for the Sandbox's namespace. TLS on the endpoint is optional. Without TLS, someone who captures a token can keep the router's Sandboxes running or resume them until the token expires.

**The controller writes at most once per quarter of the timeout** (#1160: "how often it's written"). Updating status on every request would send one API server write per request. The controller waits the timeout plus a quarter, so that a gap between writes does not suspend a Sandbox before its timeout.

**The controller keeps the idle timer.** It counts in memory from when it sees `lastActivityTime` change, because another replica's clock may have stamped that time. The timer starts at a Sandbox's first report. A timer that started at creation would suspend Sandboxes in a cluster where reporting is off, and no request could resume them. After a restart, the controller counts each Sandbox's timeout again from zero. This can only postpone a suspension. If the router stops reporting for longer than the timeout, the controller cannot tell that from an idle Sandbox, and it suspends the Sandbox.

**A busy probe covers work with no traffic** (#1160: "long-running work with no inbound traffic"). An agent can keep working after its user disconnects, and without a signal from the workload the controller would suspend the Sandbox mid-task. A Sandbox can name a port and path. The router sends an HTTP GET there. A 200 response means the workload is busy, and any other response or a failed request means it is not. The controller does not suspend the Sandbox while the workload answers busy. This KEP sets no limit on how long a workload can report busy, so a workload that keeps reporting busy is never suspended. The router sends the busy probe because it already has a network path to the Pod. By default, only a request through the router starts the timer. `firstActivity: TrafficOrBusyProbe` lets a busy answer start it too, for a Sandbox that starts an unattended job before its first request.

**The controller records the suspension, then deletes the Pod.** A request can arrive just as the timer runs out, and a delete with no check would remove the Pod under it. The controller first copies `status.lastActivityTime` to `status.autoSuspendedActivityTime`, with a write that fails if the Sandbox has changed, and deletes the Pod only if that write succeeded. A report recorded before that write cancels the suspension, and a later one resumes the Sandbox. A `SandboxClaim` user whose client bypasses the router cannot resume an automatically suspended Sandbox. Removing `autoSuspension` from the Sandbox resumes it. If status is wiped, the Sandbox resumes, and its timer starts again at the next report. The #1296 sketch treats a wiped time as "now", but the controller cannot tell a wiped time from one that was never written, and it must not suspend a Sandbox that nothing can resume.

#### API Changes

A user writes:

```yaml
spec:
  autoSuspension:
    inactivityTimeoutSeconds: 900
    busyProbe:
      port: 8080
      path: /busy
```

A `SandboxClaim` takes the same field under `spec.lifecycle`.

```go
// AutoSuspensionFirstActivity selects what may start the idle timer.
// +kubebuilder:validation:Enum=Traffic;TrafficOrBusyProbe
type AutoSuspensionFirstActivity string

const (
	AutoSuspensionFirstActivityTraffic            AutoSuspensionFirstActivity = "Traffic"
	AutoSuspensionFirstActivityTrafficOrBusyProbe AutoSuspensionFirstActivity = "TrafficOrBusyProbe"
)

// BusyProbe names an HTTP endpoint that the workload serves to report work in progress.
type BusyProbe struct {
	// +required
	Port int32 `json:"port"`
	// +required
	Path string `json:"path"`
}

// AutoSuspension configures automatic suspension of an idle Sandbox.
type AutoSuspension struct {
	// inactivityTimeoutSeconds is how long the Sandbox may go without activity
	// before the controller suspends it.
	// +kubebuilder:validation:Minimum=60
	// +required
	InactivityTimeoutSeconds int32 `json:"inactivityTimeoutSeconds"`

	// busyProbe is the endpoint that sandbox-router polls for work in progress.
	// +optional
	BusyProbe *BusyProbe `json:"busyProbe,omitempty"`

	// +kubebuilder:default=Traffic
	// +optional
	FirstActivity AutoSuspensionFirstActivity `json:"firstActivity,omitempty"`
}

// Added to Lifecycle on Sandbox and on SandboxClaim:
// +optional
AutoSuspension *AutoSuspension `json:"autoSuspension,omitempty"`

// Added to SandboxStatus:
// +optional
LastActivityTime *metav1.Time `json:"lastActivityTime,omitempty"`
// +optional
AutoSuspendedActivityTime *metav1.Time `json:"autoSuspendedActivityTime,omitempty"`
```

`autoSuspension` sits with the other lifecycle fields, as the #1160 review asked. The SandboxClaim controller copies the claim's `autoSuspension` to the Sandbox once, and the field is immutable on the claim. The API refuses `busyProbe` on a Sandbox that uses `hostNetwork`, because the probe would reach node ports, and `TrafficOrBusyProbe` requires a `busyProbe`.

## Scalability

Each router replica sends its reports every 5 seconds as a starting value. The controller makes at most 4 `lastActivityTime` writes per active Sandbox per timeout period, plus one per resume, and none for an idle Sandbox. When reporting is on, each router replica adds one watch on Sandboxes and sends one GET to the Pod of each Sandbox that sets `busyProbe`, also every 5 seconds as a starting value.

## Alternatives

**1. A component patches `spec.operatingMode`.** [#970](https://github.com/kubernetes-sigs/agent-sandbox/pull/970), [#972](https://github.com/kubernetes-sigs/agent-sandbox/pull/972), #1160 and #1296 as first proposed did this. The patch writes a field that belongs to the user, so a user can no longer tell their own intent from the system's.

**2. A separate CRD and controller hold the policy.** The #1296 review offered this option beside the status-based sketch. It would add a second place to configure a Sandbox's lifecycle, and the #1160 review asked that new fields "consolidate with" `spec.lifecycle`.

**3. The router writes `status.lastActivityTime` itself, limited by an admission policy.** The controller would need no listener. However, the limit on the router would be a separate object that fails open wherever it is missing. The feature would also need Kubernetes 1.30, and the Sandbox would gain a second status writer.

**4. The reporter renews a `Lease`.** The #1296 review noted that a `Lease` would give "better robustness" but "will be much more complex". A status field needs no extra object per Sandbox.

**5. Only a NetworkPolicy protects the endpoint.** This is #1296 after its first review, and its reviewers accepted it as a first step. Access control then "rests entirely on the NetworkPolicy, which not all CNIs enforce", and the review asked for authentication as a follow-up.
