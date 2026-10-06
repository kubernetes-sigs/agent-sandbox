# One NetworkPolicy per namespace for agent-sandbox-rl fleets

The agent-sandbox controller creates one Kubernetes `NetworkPolicy` per
`SandboxTemplate`, selecting pods by the template's hash label. That is the right
shape for a handful of long-lived templates. An RL fleet is a different shape:
[`agent-sandbox-rl`](../agent-sandbox-rl) creates a template per task image, so a
namespace holds hundreds of templates, and recipes that warm the next step's pools
create and delete dozens of templates per training step. Each one brings a policy
create, a policy delete, and a per-template label that the CNI has to keep in its
policy identity.

This example runs the fleet under **one `NetworkPolicy` per namespace**. The policy
selects the label the SDK already puts on every sandbox pod, `app=agent-sandbox-rl`,
and the fleet creates its templates with `networkPolicyManagement: Unmanaged`, so the
controller creates no per-template policy at all. Nothing selects on a per-template
or per-pod label any more.

Measured on one 400-node sandbox pool during a four-job RL run: 480 per-template
policies in a single namespace, 32 of them created and 32 retired at every step
dispatch, and, on Dataplane V2 (Cilium), one security identity per sandbox pod,
13,508 at the peak, because every pod carried unique labels. The identity allocator
fell behind at about 2,000 pod creates per minute and took 232 nodes NotReady. The
policy count is the part this example removes; the identity part needs one more
cluster-side change, described [below](#cilium-and-dataplane-v2-identities).

## What changes and what stays the same

| | Controller default (`Managed`) | This example |
| --- | --- | --- |
| Policies in the namespace | one per `SandboxTemplate`, created and deleted with it | one, created once by the operator |
| Pod selector | `agents.x-k8s.io/sandbox-template-ref-hash=<hash>` | `app=agent-sandbox-rl` |
| Ingress | sandbox-router only | sandbox-router, plus pods in the namespace that are not sandboxes (the trainer) |
| Egress | public IPs only; private, link-local and cluster ranges denied | same, plus UDP/TCP 53 to kube-dns |
| Pod DNS | controller sets `dnsPolicy: None` with public resolvers | pod default (`ClusterFirst`), hence the kube-dns rule |
| Sandbox-to-sandbox traffic | denied | denied |
| FQDN or L7 rules | not expressible | not expressible; layer [`ClusterNetworkPolicy`](../network-policy-api-sandbox) on top |

The controller only rewrites pod DNS in secure-by-default mode (Managed with no
custom rules). Unmanaged pods keep cluster DNS, which is why the policy allows
kube-dns. If your pods must not see cluster DNS, set `dnsPolicy: None` and
`dnsConfig` in `TemplateSpec.extra_pod_spec` and drop that rule.

## Prerequisites

- A cluster with the agent-sandbox controller and extensions installed, and a CNI
  that enforces Kubernetes `NetworkPolicy` (GKE Dataplane V2, Cilium, Calico, or
  kube-network-policies).
- The RL SDK installed from this checkout, as in the
  [agent-sandbox-rl setup](../agent-sandbox-rl/README.md#3-install-the-python-packages-client-side).
- `TemplateSpec.network_policy_management`, added together with this example.

## Steps

### 1. Create the namespace and the policy

Edit `metadata.namespace` in the manifest if your fleet does not use
`agent-sandbox-rl`, then:

```bash
kubectl create namespace agent-sandbox-rl
kubectl apply -f manifests/fleet-network-policy.yaml
```

Apply the policy before the first warm. A Kubernetes `NetworkPolicy` is additive:
until one selects a pod, that pod is open; with the per-template policy gone, this
policy is the only thing isolating the sandboxes.

### 2. Create templates as Unmanaged

Set the mode on the fleet's `TemplateSpec`:

```python
from agent_sandbox_rl import ClusterConfig, FleetConfig, SandboxFleet, TemplateSpec

fleet = SandboxFleet(FleetConfig(
    clusters=[ClusterConfig(name="c1", context="<kube-context>", namespace="agent-sandbox-rl")],
    max_concurrent=16, max_warmpool_size=16, warm_per_task=True,
    template=TemplateSpec(network_policy_management="Unmanaged")))
```

Every template the fleet creates carries `spec.networkPolicyManagement: Unmanaged`.
For a template that already exists and belongs to this run (or to no run), the SDK
patches the field; the controller then deletes the policy it owned. Templates
labelled with another run's id are never touched. The field sits outside the pod
template, so the patch does not roll existing pools. Leaving
`network_policy_management` unset (the default) writes nothing and never flips a
template back, so existing fleets are unaffected.

### 3. Warm a pool and verify

`warm_unmanaged.py` warms one image under this configuration, waits for it, and
reports the policies and template mode it finds; then it tears the pool down.

```bash
KUBE_CONTEXT=<kube-context> NAMESPACE=agent-sandbox-rl \
NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool NODE_SELECTOR_VAL=<sandbox-pool> \
python warm_unmanaged.py
```

`scripts/verify.sh` checks the same things against any namespace, at any time:

```bash
NAMESPACE=agent-sandbox-rl KUBE_CONTEXT=<kube-context> scripts/verify.sh
```

It passes when the namespace-wide policy selects `app=agent-sandbox-rl`, every
agent-sandbox-rl template is Unmanaged, and no template-owned policy remains. On a
Cilium cluster it also prints how many identities still carry per-pod keys.

## Reading the policy

[`manifests/fleet-network-policy.yaml`](manifests/fleet-network-policy.yaml), rule by rule:

- **Ingress from the trainer.** Any pod in the namespace whose `app` label is not
  `agent-sandbox-rl`. For a tunix-style run that is the orchestrator and the rollout
  workers, which talk to the OpenHands agent-server on port 8000 by pod IP. Sandboxes
  do not match, so they cannot reach each other. If the trainer runs elsewhere, add a
  `namespaceSelector` peer. `kubectl exec` and the Python SDK's exec path go through
  the API server and the kubelet, not the pod network, so they need no rule.
- **Ingress from the sandbox-router**, as in the controller default.
- **Egress to kube-dns** on 53/UDP and 53/TCP. With NodeLocal DNSCache, add the cache
  address too (GKE: `169.254.20.10/32`); it falls inside the link-local range the next
  rule excludes.
- **Egress to public IPs**, with the same carve-outs as the controller default:
  `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16` (the metadata
  server), and the IPv6 unique-local and link-local ranges. In-cluster services,
  the VPC and the metadata server stay unreachable.

Policies are additive, so per-tenant allows (a model gateway, an artifact mirror) go
in a second policy in the same namespace with the same `podSelector`, and
cluster-wide guardrails and FQDN allowlists go in `ClusterNetworkPolicy` objects as
in [network-policy-api-sandbox](../network-policy-api-sandbox).

## Cilium and Dataplane V2 identities

Cilium keys a pod's security identity on its full label set. Every sandbox pod
carries `agents.x-k8s.io/sandbox-name-hash` (unique per sandbox), and claimed pods
carry `agents.x-k8s.io/claim-uid`; pool pods carry
`agents.x-k8s.io/warm-pool-sandbox`, and the SDK adds `agent-sandbox-rl/run-id`.
Unless the agent is told otherwise, each pod therefore gets its own identity, and
identities are allocated through a single operator.

With this example in place no policy selects on any of those keys, so they can be
dropped from identity computation with Cilium's `labels` option (exclusion form,
space separated, leading `!` excludes a prefix):

```yaml
# cilium-config, key "labels"
labels: "!k8s:agents.x-k8s.io/sandbox-name-hash !k8s:agents.x-k8s.io/claim-uid !k8s:agents.x-k8s.io/warm-pool-sandbox !k8s:agent-sandbox-rl/run-id !k8s:agents.x-k8s.io/sandbox-template-ref-hash"
```

The agent reads this at startup, so it needs a rolling restart in a quiet window.
Afterwards the sandboxes in a namespace collapse to one identity (the labels that
remain are `app`, `sandbox=<template>` and the namespace label; exclude
`k8s:sandbox` as well if you do not need per-template identities). On GKE
Dataplane V2 the ConfigMap is managed and reverts on edit; the change has to be
requested from GKE. Keep `sandbox-template-ref-hash` identity-relevant on any cluster
that still runs Managed templates, because their policies select on it.

Verify with:

```bash
kubectl get ciliumidentities --no-headers | wc -l
kubectl get ciliumidentities -o json | jq '[.items[]["security-labels"] | keys[]] | unique'
```

Before this change the second command lists the per-pod keys; after it, it does not.

## Rolling back

Set `TemplateSpec(network_policy_management="Managed")` and run the fleet once, or
patch the templates by hand; the controller recreates its per-template policies on
the next reconcile. Then delete the namespace-wide policy. Do it in that order, so no
sandbox is left without a policy in between.

## Limitations

- Kubernetes `NetworkPolicy` is L3/L4. Domain-name egress and L7 need
  `ClusterNetworkPolicy` or a CNI-specific policy on top.
- The ingress rule trusts every non-sandbox pod in the namespace. Give the trainer a
  label of its own and select on it if other workloads share the namespace.
- The `sandbox=<template>` pod label stays per template; the SDK uses it for the
  replica co-location affinity. No policy selects on it.
