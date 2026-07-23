# APF Insulation for the Agent Sandbox Controller

An **opt-in** [API Priority and Fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/)
overlay that gives the agent-sandbox controller dedicated apiserver
concurrency, so that high-rate SandboxClaim workloads are insulated from
other tenants' API traffic — and the controller's own bulk warm-pool refill
traffic is insulated from its latency-critical claim-adoption writes.

This is a cluster-operator decision. **Nothing in the default install
applies these manifests, and they change nothing until you apply them.**

## Apply

```sh
kubectl apply -f examples/apf-insulation/apf-insulation.yaml
```

## Roll back

```sh
kubectl delete -f examples/apf-insulation/apf-insulation.yaml
```

Deleting the FlowSchemas immediately returns the controller's traffic to the
built-in `service-accounts` schema (shared `workload-low` level), i.e. the
stock behavior.

## What it creates

| object | kind | purpose |
|---|---|---|
| `agent-sandbox-critical` | PriorityLevelConfiguration + FlowSchema | the SandboxClaim → Ready hot path (claim writes, sandbox adoption update/patch + status, the adoption-time pod patch, leader-election leases) |
| `agent-sandbox-bulk` | PriorityLevelConfiguration + FlowSchema | everything else the controller does (warm-pool refill creates/deletes, informer list/watch, child objects, discovery) |
| `agent-sandbox-events` | FlowSchema | routes controller Events to the shared `workload-low` level (sacrificial) |

## Sizing

Seat counts are **fractions of the server-wide inflight limits**, so the same
manifest scales with `--max-requests-inflight` /
`--max-mutating-requests-inflight` without edits. Before relying on this
overlay at high claim rates, read
[docs/apf-insulation.md](../../docs/apf-insulation.md) for the sizing rule
(`seats(critical) >= 2x` the measured demand high watermark), the exact
PromQL to measure demand and APF wait from apiserver metrics, and the
caveats.
