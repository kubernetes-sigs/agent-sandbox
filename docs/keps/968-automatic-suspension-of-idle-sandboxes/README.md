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

This KEP covers the idleness portion of [#968](https://github.com/kubernetes-sigs/agent-sandbox/issues/968): deciding that a Sandbox is idle, suspending it, and resuming it when traffic returns or a user asks. A Sandbox opts in with an `autoSuspension` field among its lifecycle fields. `sandbox-router` tells the Sandbox controller when the Sandbox is in use. Once the Sandbox has been idle for its timeout, the controller deletes the backing Pod and keeps the volumes, as it does for `operatingMode: Suspended`. It leaves `spec.operatingMode` at `Running`. Resume is minimal: the next request through the router gets a 503 that the client must retry, and causes the controller to create the Pod again.

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

**The controller decides from status.** `spec.operatingMode` records what the user asked for, so it stays `Running` through an automatic suspension. The Sandbox reports `Ready=False` with reason `SandboxAutoSuspended`, and the `Suspended` condition follows the same sequence as for `operatingMode: Suspended`, so `kubectl wait --for=condition=Suspended` works for both. For an automatic suspension the `Suspended` condition has the reason `AutoSuspended`. `spec.shutdownTime` still expires an automatically suspended Sandbox. Setting `operatingMode: Suspended` turns an automatic suspension into a manual one, and setting it back to `Running` resumes the Sandbox. The controller does not set `Finished` while a Sandbox is automatically suspended, because that would start a SandboxClaim's `ttlSecondsAfterFinished`.

**The router observes activity** (#1160: "the activity signal"). `sandbox-router` counts a request that passed its authorization check, and a request or connection that is still open. This traffic already passes through the router, so counting it changes no client, SDK or workload. Traffic that bypasses the router, such as a port-forward to the Pod, does not count. The controller suspends a Sandbox that is used only that way, unless its workload answers the busy probe. `sandboxd` answers busy while a client is connected to it. The SDKs set `busyProbe` to that endpoint on a new claim that sets `autoSuspension`.

**The router reports, and only the controller writes** (#1160: "who updates it"). The router sends reports to an endpoint on the controller, and the controller writes `status.lastActivityTime`. The router gets no write access to Sandboxes, and a report can only refresh that time. The router does gain a read-only watch on Sandboxes, to learn each Sandbox's `busyProbe` and whether the Sandbox is suspended. Today the router reads only Pods. The endpoint is off by default. Where the CNI does not enforce NetworkPolicies, a NetworkPolicy alone would let any Pod send reports. The router presents a ServiceAccount token that names the controller as its audience and lasts 10 minutes. The controller checks it with a TokenReview and then a SubjectAccessReview for `create` on `sandboxes/activity` in the Sandbox's namespace. Only the router's ServiceAccount gets that permission. The endpoint requires TLS.

**The controller writes at most once per half of the timeout** (#1160: "how often it's written"). Updating status on every request would send one API server write per request. The controller keeps the newest activity time from reports in memory and counts the deadline from it. It writes that time to status only when it is at least half a timeout newer than the stored one, so the probe rate does not change the number of writes. The controller suspends a Sandbox one timeout after that time. After a controller restart it has only the stored time, which can be up to half a timeout old. Until a new report arrives for the Sandbox, the controller waits the timeout plus a half from the stored time.

**The controller keeps the idle timer.** The timer starts when a new Pod for the Sandbox becomes Ready, or, for a Sandbox taken from a warm pool, when the SandboxClaim controller sets `autoSuspension` on it. The controller sets a new condition, `AutoSuspensionActive`, on each Sandbox that sets `autoSuspension`. It is `True` while the router reports. While no router is reporting, it is `False` and the controller suspends nothing.

**A busy probe covers work with no traffic** (#1160: "long-running work with no inbound traffic"). An agent can keep working after its user disconnects, and without a signal from the workload the controller would suspend the Sandbox mid-task. A Sandbox can name a port and path. The router sends an HTTP GET there. A 200 response means the workload is busy, and any other response or a failed request means it is not. The controller does not suspend the Sandbox while the workload answers busy. A busy answer counts as activity, so the timeout runs from the last busy answer. This KEP sets no limit on how long a workload can report busy, so a workload that keeps reporting busy is never suspended. The router sends the busy probe because it already has a network path to the Pod.

**The controller records the suspension, then deletes the Pod.** A request can arrive just as the timer runs out, and a delete with no check would remove the Pod under it. The controller first copies `status.lastActivityTime` to `status.autoSuspendedActivityTime`, with a write that fails if the Sandbox has changed, and deletes the Pod only if that write succeeded. The router reports a request at once when it has not recently reported that Sandbox. A report recorded before that write cancels the suspension, and a later one resumes the Sandbox. A user can also resume a Sandbox by raising `resumeRequests` on its `SandboxClaim`, and the SDKs' resume call does this. Removing `autoSuspension` from the Sandbox resumes it. If status is wiped, the Sandbox resumes, and the controller treats the missing time as "now".

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
	// before the controller suspends it. A Sandbox must set it, and a
	// SandboxClaim may omit it when its SandboxTemplate sets it.
	// +kubebuilder:validation:Minimum=300
	// +optional
	InactivityTimeoutSeconds *int32 `json:"inactivityTimeoutSeconds,omitempty"`

	// busyProbe is the endpoint that sandbox-router polls for work in progress.
	// +optional
	BusyProbe *BusyProbe `json:"busyProbe,omitempty"`

	// resumeRequests is a number that a user increases to resume an automatically
	// suspended Sandbox, or to restart the idle timer of a running one.
	// +optional
	ResumeRequests *int32 `json:"resumeRequests,omitempty"`
}

// Added to Lifecycle on Sandbox and on SandboxClaim:
// +optional
AutoSuspension *AutoSuspension `json:"autoSuspension,omitempty"`

// Added to SandboxSpec, so that a Sandbox that sets autoSuspension sets a timeout:
// +kubebuilder:validation:XValidation:rule="!has(self.autoSuspension) || has(self.autoSuspension.inactivityTimeoutSeconds)",message="autoSuspension requires inactivityTimeoutSeconds"

// Added to SandboxTemplateSpec. It applies to every Sandbox claimed from the
// template. It must set inactivityTimeoutSeconds and may not set resumeRequests.
// +kubebuilder:validation:XValidation:rule="has(self.inactivityTimeoutSeconds) && !has(self.resumeRequests)",message="a template sets inactivityTimeoutSeconds and not resumeRequests"
// +optional
AutoSuspension *AutoSuspension `json:"autoSuspension,omitempty"`

// Added to SandboxStatus:
// +optional
LastActivityTime *metav1.Time `json:"lastActivityTime,omitempty"`
// +optional
AutoSuspendedActivityTime *metav1.Time `json:"autoSuspendedActivityTime,omitempty"`
// +optional
ObservedResumeRequests *int32 `json:"observedResumeRequests,omitempty"`
```

`autoSuspension` sits with the other lifecycle fields, as the #1160 review asked. The SandboxClaim controller copies the claim's `autoSuspension` to the Sandbox. After creating a claim, a user can add or change only `resumeRequests`. A `SandboxTemplate` can also set the timeout and `busyProbe` for every Sandbox claimed from it. The SandboxClaim controller then refuses a claim that sets its own, so the timeout is optional on a claim. The API refuses `busyProbe` on a Sandbox that uses `hostNetwork`, because the probe would reach node ports.

## Scalability

Status writes reach the API server and etcd. Busy probes go from the router to Pods and never reach the API server. At the minimum timeout of 300 seconds:

| Sandboxes | Status writes per second | Sent to etcd per minute | GETs per second per router replica | GETs per second, 2 router replicas |
| --- | --- | --- | --- | --- |
| 10,000 | 67 | 40 MB | 133 | 267 |
| 15,000 | 100 | 60 MB | 200 | 400 |

Status writes are about 2 × Sandboxes ÷ timeout per second, and GETs about 4 × Sandboxes ÷ timeout per router replica. Both halve when the timeout doubles, so a longer timeout raises each row in proportion.

**Status writes.** The controller writes `lastActivityTime` at most twice per timeout for each Sandbox in use, plus once per resume, and never for an idle Sandbox. Each write stores the whole Sandbox, which the [tuning guide](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/performance-tuning.md#managed-kubernetes-control-plane-checklist) puts at about 10 KB. The same guide says the control plane can become the bottleneck at about 100 `Sandbox` writes per second alongside `Pod` writes. The controller writes at most once per half of the timeout, and the minimum timeout is 300 seconds, to keep this load low. The writes reach that figure at about 15,000 Sandboxes in use. Between the API server's compactions, etcd keeps the last 5 to 10 minutes of these writes, about 200 to 400 MB at 10,000 Sandboxes in use.

**Busy probes.** Each router replica probes each Sandbox that sets `busyProbe` once per quarter of the timeout. It skips a Sandbox while it has an open request or connection to that Sandbox, because that already counts as activity. A busy answer from any replica keeps the Sandbox running, so one replica that cannot reach a Pod cannot get a busy Sandbox suspended. Adding a replica does not raise any replica's rate. It adds that replica's probes to the total, and one more network path to each Pod.

**Reports and watches.** When reporting is on, each router replica sends one report to the controller every 5 seconds and holds one watch on Sandboxes.

## Alternatives

**1. A component patches `spec.operatingMode`.** [#970](https://github.com/kubernetes-sigs/agent-sandbox/pull/970), [#972](https://github.com/kubernetes-sigs/agent-sandbox/pull/972), #1160 and #1296 as first proposed did this. The patch writes a field that belongs to the user, so a user can no longer tell their own intent from the system's.

**2. A separate CRD and controller hold the policy.** The #1296 review offered this option beside the status-based sketch. It would add a second place to configure a Sandbox's lifecycle, and the #1160 review asked that new fields "consolidate with" `spec.lifecycle`.

**3. The router writes `status.lastActivityTime` itself, limited by an admission policy.** The controller would need no listener. However, the limit on the router would be a separate object that fails open wherever it is missing. The feature would also need Kubernetes 1.30, and the Sandbox would gain a second status writer.

**4. Activity lives in a `Lease` per Sandbox.** The #1296 review noted that a `Lease` would give "better robustness" but "will be much more complex". A `Lease` would make each activity write a tenth to a twentieth of the size, so etcd would keep less and the Sandbox watch would not include these writes. It would not reduce the number of writes, and it adds one object per Sandbox to create and clean up.

**5. Only a NetworkPolicy protects the endpoint.** This is #1296 after its first review, and its reviewers accepted it as a first step. Access control then "rests entirely on the NetworkPolicy, which not all CNIs enforce", and the review asked for authentication as a follow-up.

**6. The workload sends its own reports.** Each report would need its token checked against the API server, and those checks would grow with the number of busy Sandboxes. Each Pod would also need a network path to the controller, which the default NetworkPolicy blocks.
