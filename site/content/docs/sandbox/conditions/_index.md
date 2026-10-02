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
| `False` | `DependenciesNotReady` | The Sandbox should be running, but the Pod or Service is not provisioned or not Ready yet. |
| `False` | `ReconcilerError` | Reconciling a child resource failed. The controller retries. |
| `False` | `InvalidConfiguration` | A child resource was rejected with a permanent validation error, for example a Sandbox name whose derived Service name exceeds 63 characters. Recreate the Sandbox. |
| `False` | `MultiplePods` | More than one Pod is controlled by the Sandbox and the controller cannot pick one safely. |
| `False` | `SandboxSuspended` | The Sandbox is suspending or suspended (`spec.operatingMode: Suspended`). |
| `False` | `PodSucceeded` | The Pod completed successfully. |
| `False` | `PodFailed` | The Pod failed. |
| `False` | `SandboxExpired` | `spec.shutdownTime` was reached and the resources were torn down. See [shutdown time]({{< ref "/docs/sandbox/lifecycle" >}}). |

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

Present only after the backing Pod reaches a terminal phase, and kept after expiry. It is always `True`.

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodSucceeded` | The Pod completed successfully. |
| `True` | `PodFailed` | The Pod failed. |

### PodScheduled

Mirrors the backing Pod's `PodScheduled` condition, so you can see why a Sandbox is not scheduled without reading the Pod. It is absent while the Sandbox has no backing Pod.

The status, reason and message are copied from the Pod, so reasons set by the scheduler (such as `Unschedulable` or `SchedulingGated`) pass through unchanged. The controller adds two reasons of its own:

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `PodScheduled` | The Pod is scheduled to a node. Used when the Pod condition has no reason. |
| `Unknown` | `PodSchedulingUnknown` | The Pod has not reported a `PodScheduled` condition yet, the Pod condition is not `True` and has no reason, or the Pod state could not be read. |
