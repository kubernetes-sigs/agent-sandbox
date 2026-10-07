# One NetworkPolicy per namespace for agent-sandbox-rl fleets on GKE

The agent-sandbox controller creates one Kubernetes `NetworkPolicy` per
`SandboxTemplate`, selecting pods by the template's hash label. That is the right
shape for a handful of long-lived templates. A fleet is a different shape:
[`agent-sandbox-rl`](../agent-sandbox-rl) creates a template per task image, so a
namespace that runs a large task set holds hundreds of templates, and a fleet that
warms and retires pools as its task set changes creates and deletes templates
continuously. Each one brings a policy create, a policy delete, and a per-template
label that the CNI has to keep in its policy identity.

This example runs the fleet under **one `NetworkPolicy` per namespace**. The policy
selects the label the SDK already puts on every sandbox pod, `app=agent-sandbox-rl`,
and the fleet creates its templates with `networkPolicyManagement: Unmanaged`, so the
controller creates no per-template policy at all. Nothing selects on a per-template
or per-pod label any more.

On GKE Dataplane V2 the cost goes beyond the policy count. Dataplane V2 is built on
Cilium, which gives every distinct pod label set its own security identity,
allocated through a single managed operator. Every sandbox pod carries labels unique
to it, so every sandbox pod gets its own identity, and a fleet that creates pods
quickly creates identities just as quickly. The policy count is the part this
example removes; the identity part needs one more cluster-side change, described
[below](#gke-dataplane-v2-identities).

## What changes and what stays the same

| | Controller default (`Managed`) | This example |
| --- | --- | --- |
| Policies in the namespace | one per `SandboxTemplate`, created and deleted with it | one, created once by the operator |
| Pod selector | `agents.x-k8s.io/sandbox-template-ref-hash=<hash>` | `app=agent-sandbox-rl` |
| Ingress | sandbox-router only | sandbox-router, plus pods in the namespace labelled `agent-sandbox-rl/client=true` (the fleet's clients) |
| Egress | public IPs only; private, link-local and cluster ranges denied | same, plus UDP/TCP 53 to kube-dns |
| Pod DNS | controller sets `dnsPolicy: None` with public resolvers | pod default (`ClusterFirst`), hence the kube-dns rule |
| Sandbox-to-sandbox traffic | denied | denied |
| FQDN or L7 rules | not expressible | not expressible; layer [`ClusterNetworkPolicy`](../network-policy-api-sandbox) on top |

The controller only rewrites pod DNS in secure-by-default mode (Managed with no
custom rules). Unmanaged pods keep cluster DNS, which is why the policy allows
kube-dns. If your pods must not see cluster DNS, set `dnsPolicy: None` and
`dnsConfig` in `TemplateSpec.extra_pod_spec` and drop that rule.

## Prerequisites

- A GKE cluster with the agent-sandbox controller and extensions installed, and
  `NetworkPolicy` enforcement: Dataplane V2 enforces it natively; on the legacy
  datapath, enable network policy enforcement (`--enable-network-policy`). The
  manifest is plain Kubernetes `NetworkPolicy`, so any conformant implementation
  works as well.
- The RL SDK installed from this checkout, as in the
  [agent-sandbox-rl setup](../agent-sandbox-rl/README.md#3-install-the-python-packages-client-side).
- `TemplateSpec.network_policy_management`, added together with this example.

## Steps

### 1. Create the namespace and the policy

```bash
NAMESPACE=agent-sandbox-rl     # the fleet's namespace
kubectl create namespace "$NAMESPACE"
kubectl apply -n "$NAMESPACE" -f manifests/fleet-network-policy.yaml
```

Label the pods that create claims and connect to the sandboxes
`agent-sandbox-rl/client=true` in their pod template; the policy admits ingress only
from those and from the sandbox-router.

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
labelled with another run's id are never touched. Leaving
`network_policy_management` unset (the default) writes nothing and never flips a
template back, so existing fleets are unaffected.

`fleet.preflight()` (and `setup()`, which calls it) logs a `networkpolicy` warning
when the templates are Unmanaged and no NetworkPolicy in the namespace selects the
fleet's pods, or when the fleet's identity cannot list policies.

The mode sits outside the part of the template the warm pool hashes, so switching
it does not replace existing pool members. They keep the pod spec they were created
with: members created under Managed keep the controller's public DNS resolvers,
which the policy's public egress rule still allows, and new members use cluster DNS.

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

- **Ingress from the fleet's clients.** Pods in the namespace labelled
  `agent-sandbox-rl/client=true`: the processes that create claims and connect to
  the sandboxes over the pod network. Other pods in the namespace, sandboxes
  included, get nothing from this rule, so sandboxes cannot reach each other.
  The label is a trust boundary only if untrusted users cannot create pods in the
  namespace, because anyone who can create a pod can set it. If they can, run the
  clients in their own namespace and replace the `podSelector` with a
  `namespaceSelector` for it. `kubectl exec` and the Python SDK's exec path go
  through the API server and the kubelet, not the pod network, so they need no rule.
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

## GKE Dataplane V2 identities

Dataplane V2 is built on Cilium, which keys a pod's security identity on its full
label set. Every sandbox pod carries `agents.x-k8s.io/sandbox-name-hash` (unique per
sandbox); claimed pods carry `agents.x-k8s.io/claim-uid`, pool pods carry
`agents.x-k8s.io/warm-pool-sandbox`, and the SDK adds `agent-sandbox-rl/run-id`.
Unless the agent is told otherwise, each pod therefore gets its own identity.

Cilium's `labels` option drops keys from identity computation (exclusion form, space
separated, a leading `!` excludes a prefix). The option is **cluster-wide**, and
Cilium resolves policy selectors against identity labels, so a NetworkPolicy that
selects on an excluded key stops selecting its pods, in every namespace, and those
pods lose the isolation it gave them. Work through the steps in order.

1. **List the label keys on the sandbox pods and how many values each has.** Keys
   with one value per pod, claim, pool or run are the ones that mint identities.
   Other integrations, such as a queueing system that admits sandbox pods, can add
   keys of their own; exclude any that are per pod as well.

   ```bash
   kubectl get pods -A -l app=agent-sandbox-rl -o json \
     | jq -r '[.items[].metadata.labels | to_entries[]] | group_by(.key)[]
              | "\(.[0].key) \(map(.value) | unique | length)"'
   ```

2. **Confirm no policy, in any namespace, selects on a key you plan to exclude.**
   Check the pods a policy applies to and the peers its rules allow, since a peer
   selector on an excluded key stops matching too. The output must not contain any
   of the keys:

   ```bash
   kubectl get networkpolicies -A -o json \
     | jq -r '.items[].spec
              | ([.podSelector] + [.ingress[]?.from[]?.podSelector] + [.egress[]?.to[]?.podSelector])[]
              | select(. != null) | (.matchLabels // {} | keys[]), (.matchExpressions // [] | .[].key)' \
     | sort | uniq -c
   kubectl get ciliumnetworkpolicies,ciliumclusterwidenetworkpolicies -A
   ```

   If the Network Policy API is installed, check its `ClusterNetworkPolicy` objects
   the same way: their subjects and peers select pods by label as well.

3. **Request the exclusion list.** It keeps `agents.x-k8s.io/sandbox-template-ref-hash`
   identity-relevant, because every Managed template's policy selects on it:

   ```yaml
   # cilium-config, key "labels"
   labels: "!k8s:agents.x-k8s.io/sandbox-name-hash !k8s:agents.x-k8s.io/claim-uid !k8s:agents.x-k8s.io/warm-pool-sandbox !k8s:agent-sandbox-rl/run-id"
   ```

   Add `!k8s:agents.x-k8s.io/sandbox-template-ref-hash` only if step 2 shows no
   policy uses it, which means no namespace on the cluster runs Managed templates.
   The same goes for `!k8s:sandbox`: other workloads, including several examples in
   this repository, select single sandboxes by that key.

4. **Roll it out.** On GKE Dataplane V2 the Cilium ConfigMap is managed and reverts
   on edit, so request the change from GKE. Elsewhere, restart the agents, and the
   operators as well when the operator manages identities. Existing identity
   objects are not rewritten: pods get the filtered identity as their endpoints are
   regenerated, and unused identities are garbage-collected.

With the list from step 3, sandbox pods share identities per template: the keys that
remain per template are `sandbox` and `agents.x-k8s.io/sandbox-template-ref-hash`,
and `agents.x-k8s.io/created-by` takes one of a few values. Identities grow with the
number of templates, not with the number of pods.

Verify with:

```bash
kubectl get ciliumidentities --no-headers | wc -l
kubectl get ciliumidentities -o json | jq '[.items[]["security-labels"] | keys[]] | unique'
```

Before the change the second command lists the per-pod keys; after it, it does not.

## Rolling back

1. Set `TemplateSpec(network_policy_management="Managed")` and run the fleet once,
   or patch the templates by hand. The controller recreates its per-template
   policies on the next reconcile.
2. Keep the namespace-wide policy until every pod created while the templates were
   Unmanaged is gone. Switching the mode does not replace pool members, and those
   pods use cluster DNS, which the controller's default policy blocks; they never
   got the public resolvers the controller sets for Managed templates. While both
   policies select them, the allows add up and DNS keeps working. Recycle the pools
   (scale them to zero and back, or let the fleet retire them), then check that no
   such pod is left; the command prints nothing when none is:

   ```bash
   kubectl get pods -n "$NAMESPACE" -l app=agent-sandbox-rl \
     -o jsonpath='{range .items[*]}{.metadata.name} {.spec.dnsPolicy}{"\n"}{end}' | grep -v ' None$'
   ```

   Pods from templates with custom `networkPolicy` rules or an explicit `dnsPolicy`
   also show up here; they are not affected by the switch.
3. Delete the namespace-wide policy.

## Limitations

- Kubernetes `NetworkPolicy` is L3/L4. Domain-name egress and L7 need
  `ClusterNetworkPolicy` or a CNI-specific policy on top.
- Client ingress rests on the `agent-sandbox-rl/client` label, which anyone who can
  create pods in the namespace can set. Where that includes untrusted users, use a
  separate client namespace and a `namespaceSelector`.
- The `sandbox=<template>` pod label stays per template; the SDK uses it for the
  replica co-location affinity. No policy selects on it.
