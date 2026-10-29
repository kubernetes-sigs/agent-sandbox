---
title: "Status Conditions"
linkTitle: "Status Conditions"
weight: 16
description: >
  Which conditions a Sandbox reports and how to use them.
---
A `Sandbox` reports its observed state in `status.conditions`. This page maps common questions to the condition to check, then lists every condition and reason. See the [API reference]({{< ref "/docs/api" >}}) for the full schema.

## Which condition should I check?

`Ready` is the authoritative signal. There is deliberately no `Running` condition: whether the Pod is up is part of `Ready`. `spec.operatingMode: Running` only states the desired state.

| Question | Check | Command |
| --- | --- | --- |
| Is the sandbox ready to use? | `Ready=True` | `kubectl wait sandbox/<name> --for=condition=Ready --timeout=2m` |
| Why isn't it ready? | `Ready` reason and message, plus `PodScheduled` (for example `Unschedulable`) | `kubectl get sandbox <name> -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'` |
| Has suspension finished? | `Suspended=True` | `kubectl wait sandbox/<name> --for=condition=Suspended --timeout=2m` |
| Did the workload exit? | `Finished=True`, reason `PodSucceeded` or `PodFailed` | `kubectl get sandbox <name> -o jsonpath='{.status.conditions[?(@.type=="Finished")].reason}'` |
| Has it expired? | `Ready=False` with reason `SandboxExpired` | `kubectl get sandbox <name> -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'` |
| Is this status current? | the condition's `observedGeneration` equals `metadata.generation` | `kubectl get sandbox <name> -o jsonpath='{.metadata.generation} {.status.conditions[?(@.type=="Ready")].observedGeneration}'` |

## Reference

### Ready

Summarizes whether the Sandbox can serve traffic. It is `True` only when the backing Pod is `Running` and Ready with a pod IP, and its Service exists if one is required.

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `DependenciesReady` | The Pod (and Service, if required) are provisioned and the Pod is Ready. |
| `False` | `DependenciesNotReady` | The Sandbox should be running, but the Pod or Service is not provisioned or not Ready yet. If a container cannot start, the message names it, for example `container "app" is waiting: ImagePullBackOff`. |
| `False` | `ReconcilerError` | Reconciling a child resource failed. The controller retries. |
| `False` | `InvalidConfiguration` | A child resource was rejected with a permanent validation error, for example a Sandbox name whose derived Service name exceeds 63 characters. Recreate the Sandbox. |
| `False` | `MultiplePods` | More than one Pod is controlled by the Sandbox and the controller cannot pick one safely. |
| `False` | `SandboxSuspended` | The Sandbox is suspending or suspended (`spec.operatingMode: Suspended`). |
| `False` | `PodSucceeded` | The Pod completed successfully. |
| `False` | `PodFailed` | The Pod failed. |
| `False` | `PodTerminating` | The Pod has a deletion timestamp, even if it already reached Failed or Succeeded, which `Finished` reports. With `spec.failedPodPolicy: Recreate`, the controller replaces a Failed Pod instead, so Ready is `DependenciesNotReady` and `Finished` is `False`/`NotFinished`. |
| `False` | `SandboxExpired` | `spec.shutdownTime` was reached. Resource cleanup may still be pending or in progress. See [shutdown time]({{< ref "/docs/sandbox/lifecycle" >}}). |

### Suspended

Reports progress of a suspension. It is never removed: a running Sandbox reports `False` with reason `NotSuspended`. The deprecated reason `PodNotTerminated` is no longer set.

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodTerminated` | The Pod has been terminated. The Sandbox is suspended. |
| `False` | `PodTerminating` | The Pod is still terminating. |
| `False` | `PodNotOwned` | A Pod with the Sandbox's name exists but is not owned by it, so the controller refused to delete it. |
| `False` | `NotSuspended` | The Sandbox is not suspended. |
| `Unknown` | `PodStateUnknown` | Reconciling the Pod failed, so the suspension cannot be confirmed. |

### Finished

Reports whether the backing Pod reached a terminal phase. It is never removed: it is `False` with reason `NotFinished` until a Pod owned by the Sandbox has `Succeeded` or `Failed`. While a Sandbox is suspended, `Finished` keeps its last value, so a SandboxClaim's `ttlSecondsAfterFinished` keeps running for a finished Sandbox and can delete it while suspended. `Finished` becomes `False` again when the Sandbox resumes. It also keeps its last value while its Pod is terminating, so deleting a running Pod or evicting it through the API (for example, `kubectl drain`) doesn't mark the Sandbox finished. Expiry keeps the last value when `shutdownPolicy: Retain` keeps the Sandbox. `Delete` removes the Sandbox.

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodSucceeded` | The Pod completed successfully. |
| `True` | `PodFailed` | The Pod failed. |
| `False` | `NotFinished` | The Pod has not reached a terminal phase, or the Sandbox has no Pod. With `failedPodPolicy: Recreate`, `Finished` stays `False` while the controller replaces a failed Pod. |
| `Unknown` | `PodStateUnknown` | Reconciling the Pod failed before `Finished` was first set. A later failure keeps the last value. |

### PodScheduled

Mirrors the backing Pod's `PodScheduled` condition, so you can see why a Sandbox is not scheduled without reading the Pod. It is never removed: while the Sandbox has no Pod of its own, it is `False` with reason `PodAbsent`.

The status, reason and message are copied from the Pod unless an unscheduled Pod is terminating, so reasons set by the scheduler (such as `Unschedulable` or `SchedulingGated`) pass through unchanged. The controller adds reasons of its own:

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodScheduled` | The Pod is scheduled to a node. Used when the Pod condition has no reason. |
| `False` | `PodTerminating` | An unscheduled Pod is terminating (`deletionTimestamp` is set). Scheduled Pods keep `True`. |
| `False` or `Unknown` | `PodSchedulingUnknown` | The Pod has not reported a `PodScheduled` condition yet, the Pod condition is not `True` and has no reason, or the Pod state could not be read. |
| `False` | `PodAbsent` | The Sandbox has no Pod, for example before the Pod is created, while a failed Pod is replaced, or while the Sandbox is suspended. While the Sandbox is suspended, a Pod with its name that it does not own counts as absent. |
| `False` | `SandboxExpired` | `spec.shutdownTime` was reached. The controller deletes the Pod and stops updating this condition. |
