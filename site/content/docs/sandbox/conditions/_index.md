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

`Ready` and `Serving` answer different questions. Read `Ready` to decide whether to send new work to a Sandbox. SDK ready-waits, SandboxClaim and the warm pool all read `Ready`. Read `Serving` only to keep reaching work that is already bound to the Pod, for example a router that reconnects a client to its running session while the Pod drains. The pair `Ready=False`, `Serving=True` has the same meaning as an EndpointSlice endpoint with `ready: false` and `serving: true`.

| Question | Check | Command |
| --- | --- | --- |
| Is the sandbox ready to use? | `Ready=True` | `kubectl wait sandbox/<name> --for=condition=Ready --timeout=2m` |
| Why isn't it ready? | `Ready` reason and message, plus `PodScheduled` (for example `Unschedulable`) | `kubectl get sandbox <name> -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'` |
| Can the Pod still serve sessions it holds while it terminates? | `Serving=True` (with `Ready=False`, reason `PodTerminating`) | `kubectl get sandbox <name> -o jsonpath='{.status.conditions[?(@.type=="Serving")].status}'` |
| Has suspension finished? | `Suspended=True` | `kubectl wait sandbox/<name> --for=condition=Suspended --timeout=2m` |
| Did the workload exit? | `Finished=True`, reason `PodSucceeded` or `PodFailed` | `kubectl get sandbox <name> -o jsonpath='{.status.conditions[?(@.type=="Finished")].reason}'` |
| Has it expired? | `Ready=False` with reason `SandboxExpired` | `kubectl get sandbox <name> -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'` |
| Is this status current? | the condition's `observedGeneration` equals `metadata.generation` | `kubectl get sandbox <name> -o jsonpath='{.metadata.generation} {.status.conditions[?(@.type=="Ready")].observedGeneration}'` |

## Reference

### Ready

Summarizes whether the Sandbox can take new work. It is `True` only when the backing Pod is `Running` and Ready with a pod IP, and its Service exists if one is required.

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
| `False` | `PodTerminating` | The Pod has a deletion timestamp, even if it already reached Failed or Succeeded, which `Finished` reports. With `spec.failedPodPolicy: Recreate`, the controller replaces a Failed Pod instead, so Ready is `DependenciesNotReady` and `Finished` is absent. |
| `False` | `SandboxExpired` | `spec.shutdownTime` was reached. Resource cleanup may still be pending or in progress. See [shutdown time]({{< ref "/docs/sandbox/lifecycle" >}}). |

### Serving

Reports whether the backing Pod can serve the work it already holds. It mirrors the Pod's readiness and ignores termination, like the `serving` condition of an EndpointSlice endpoint. `Ready` turns `False` with reason `PodTerminating` as soon as the Pod has a deletion timestamp. A Pod with a graceful shutdown (a `preStop` hook and a long `terminationGracePeriodSeconds`) can stay Ready for much longer, and during that window `Serving` stays `True`. The Sandbox creates its replacement Pod only after the old one is gone, so the terminating Pod is the only one that can serve during the window. A `Ready` Sandbox is always `Serving`.

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodReady` | The Pod is `Running` and Ready with a pod IP. The message says whether it is terminating. |
| `False` | `PodNotReady` | The Pod exists but is not `Running` and Ready with a pod IP. |
| `False` | `PodNotFound` | The Sandbox has no Pod, for example while it is suspended or before the Pod is created. |
| `False` | `PodNotOwned` | The Sandbox is suspended and a Pod it does not own holds its name. |
| `False` | `SandboxExpired` | `spec.shutdownTime` was reached and the Pod no longer serves. Until then, `Serving` follows the deleted Pod while it drains. |
| `Unknown` | `PodStateUnknown` | Reconciling the Pod failed, so its state cannot be determined. A running Sandbox reports this when a Pod it does not own holds its name, or when it controls more than one Pod. |

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

Present only while the controller reports an owned Pod in a terminal phase. It is always `True`. With `spec.failedPodPolicy: Recreate`, a Failed Pod is left out of status while a replacement is pending, so this condition is absent then. Expiry does not create it, but `shutdownPolicy: Retain` keeps an existing one. `Delete` removes the Sandbox.

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodSucceeded` | The Pod completed successfully. |
| `True` | `PodFailed` | The Pod failed. |

### PodScheduled

Mirrors the backing Pod's `PodScheduled` condition, so you can see why a Sandbox is not scheduled without reading the Pod. It is absent when the Pod is confirmed absent or not owned by the Sandbox. A failed Pod lookup does not remove it, see `PodSchedulingUnknown`.

The status, reason and message are copied from the Pod unless an unscheduled Pod is terminating, so reasons set by the scheduler (such as `Unschedulable` or `SchedulingGated`) pass through unchanged. The controller adds three reasons of its own:

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodScheduled` | The Pod is scheduled to a node. Used when the Pod condition has no reason. |
| `False` | `PodTerminating` | An unscheduled Pod is terminating (`deletionTimestamp` is set). Scheduled Pods keep `True`. |
| `False` or `Unknown` | `PodSchedulingUnknown` | The Pod has not reported a `PodScheduled` condition yet, the Pod condition is not `True` and has no reason, or the Pod state could not be read. |
