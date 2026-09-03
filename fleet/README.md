# Multi-cluster fleet for agent-sandbox

**Status: example.** Not part of the supported `sigs.k8s.io/agent-sandbox` API
surface. A Python package plus deployment manifests, opt-in and out of the
controller's path.

Pre-warms `SandboxWarmPool` capacity across many clusters from a single fleet
spec, so an RL rollout or batch-eval workload can reach a scale no single
cluster can hold. It is **not** a general-purpose multi-cluster scheduler and is
not trying to become one — no federation, no cross-cluster control plane. See
[What this deliberately does not do](#what-this-deliberately-does-not-do).

Two moving parts: `fleetctl`, which plans; and an in-cluster `fleet-member`,
one per cluster, which reconciles that plan locally.

## How it works

```
                    ┌─────────────────┐
   fleetctl apply ──▶  object store   ◀── capacity reports
   (spec + plan)    │  (GCS bucket)   │
                    └────────┬────────┘
                             │ assignments.json
            ┌────────────────┼────────────────┐
            ▼                ▼                ▼
      ┌───────────┐    ┌───────────┐    ┌───────────┐
      │ cluster A │    │ cluster B │    │ cluster C │
      │fleet-member│   │fleet-member│   │fleet-member│
      │     ↓      │   │     ↓      │   │     ↓      │
      │SandboxWarm │   │SandboxWarm │   │SandboxWarm │
      │   Pools    │   │   Pools    │   │   Pools    │
      └───────────┘    └───────────┘    └───────────┘
            │                │                │
            └────────────────┼────────────────┘
                             ▼  (optional)
                   ClusterProfile CRs on a hub
                   — inventory + published capacity
```

1. **Placement is object-storage-mediated.** No Karmada / KubeFleet / OCM. A
   per-cluster `fleet-member` reads its slice of `assignments.json` from a shared
   bucket and reconciles local `SandboxWarmPool` objects. `SandboxTemplate` CRs
   are operator-managed (kubectl or GitOps); the fleet-member only verifies they
   exist. Members pull; nothing reaches into a cluster from outside.
2. **Placement is capacity-aware.** Each `fleet-member` publishes warm-pool
   depth, ready count, in-flight claims and node pressure on an interval
   (`--capacity-interval`, default 30 s). The planner weights the split by what
   clusters actually report, using a Hamilton largest-remainder split so
   per-cluster totals sum exactly to the fleet target.
3. **Inventory is pluggable.** By default the planner discovers clusters from
   capacity reports in the bucket. With `--inventory=clusterprofile` it reads
   SIG-Multicluster [`ClusterProfile`](https://github.com/kubernetes-sigs/cluster-inventory-api)
   CRs from a hub instead, and each member publishes its own capacity into its
   own profile. The spec and `assignments.json` still travel through the bucket
   either way.

[ARCHITECTURE.md](ARCHITECTURE.md) has the design in full.

## Prerequisites

- `kubectl`, `python` 3.10+, `docker`, and `gcloud`
- A GCS bucket the planner and every member can read/write
- agent-sandbox installed on every member cluster, with the extension
  controllers enabled (`SandboxTemplate`, `SandboxWarmPool`)

On GKE, prefer Workload Identity over a service-account key — the
ClusterProfile path assumes it.

## Install

```bash
cd fleet
export FLEET_BUCKET=agent-sandbox-fleet-$USER

pip install -e ./python                        # provides `fleetctl`
IMAGE=us-docker.pkg.dev/my-project/fleet/fleet-member:dev ./deploy/build-push.sh
```

The image build context is the **repo root**, not `fleet/` — the Dockerfile
copies both the in-repo Python SDK and `fleet/python`.

Deploy one member per cluster:

```bash
CLUSTER_NAME=cluster-a FLEET_BUCKET="$FLEET_BUCKET" \
IMAGE=us-docker.pkg.dev/my-project/fleet/fleet-member:dev \
  ./deploy/render.sh deploy/fleet-member-deployment-wi.yaml \
  | kubectl --context cluster-a apply -f -
```

Render with `render.sh`, never bare `envsubst`: it refuses to emit a manifest
containing an unsubstituted variable. Bare `envsubst` expands an unset variable
to empty, and several of these flags are silently permissive when empty.

Apply a spec and watch it land:

```bash
fleetctl apply -f demo/fleet-spec.yaml
fleetctl status
kubectl --context cluster-a -n multi-cluster-fleet get sandboxwarmpools
```

Within about 30 s you should see `SandboxWarmPool` objects on each cluster whose
replica counts sum to each image's `target_replicas`, and `fleetctl status`
reporting per-cluster depth and claim-latency P90.

Tear the fleet down **through the fleet layer**, with the zero-model spec:

```bash
fleetctl apply -f demo/fleet-spec-drain.yaml
```

Do not just scale the members to zero. A member that restarts reads whatever
assignment is still in the bucket and begins reconciling it immediately, which
has silently re-warmed a fleet well before its intended start time.

## Using ClusterProfile for inventory

Instead of inferring the fleet from whoever wrote a capacity report, read it
from `ClusterProfile` CRs on a hub cluster. Members publish their own capacity
into their own profile via server-side apply, so the fleet describes itself.

The hub should not be a cluster under test — the planner depends on it.

```bash
# 1. install the CRD + per-member RBAC on the hub
./deploy/setup-hub.sh \
  --hub-cluster my-hub --hub-location us-central1-a \
  --project my-project \
  --members cluster-a,cluster-b,cluster-c \
  --bucket "$FLEET_BUCKET" --dry-run     # drop --dry-run when it looks right
```

`--bucket` is required: the script annotates each member's ServiceAccount with a
new per-member GSA, and that redirects *every* Google API call from the pod,
including the bucket writes the fleet already depends on.

```bash
# 2. give each member a credential-free kubeconfig for the hub
./deploy/gen-hub-kubeconfig.sh \
  --hub-cluster my-hub --hub-location us-central1-a --project my-project \
  --private-endpoint > hub-cm.yaml
kubectl --context cluster-a apply -f hub-cm.yaml
```

The ConfigMap holds only an address and a CA certificate — no token, no key.
Authentication happens at runtime via `--hub-token-source=gke-metadata`.

Use `--private-endpoint` when members and hub share a VPC. Reachability is the
usual failure here and it looks like an auth problem: if the member's bucket
writes keep working while every hub call times out, it is routing, not
credentials. A GKE control plane's public IP is not a Google API endpoint, so
Private Google Access does not cover it. Members read this ConfigMap once at
startup — `kubectl rollout restart deployment/fleet-member` after any change.

```bash
# 3. turn on publishing, per member
CLUSTER_NAME=cluster-a FLEET_BUCKET="$FLEET_BUCKET" SANDBOX_CAPACITY=50000 \
  ./deploy/render.sh deploy/fleet-member-clusterprofile-patch.yaml \
  | kubectl --context cluster-a patch deployment fleet-member \
      -n multi-cluster-fleet --patch-file /dev/stdin
```

An empty `--cluster-manager=` is falsy, which drops the label selector and
quietly inventories every `ClusterProfile` on the hub — including other fleets'.
This is the concrete reason for `render.sh`.

Verify **ownership**, not just that the value is present — a client-side apply
would land the data and own nothing:

```bash
kubectl --context my-hub get clusterprofile cluster-a -n fleet-system \
  -o jsonpath='{.metadata.managedFields[*].manager}'
# must contain agent-sandbox-fleet-member
```

```bash
# 4. plan against the hub
fleetctl apply -f demo/fleet-spec.yaml \
  --inventory=clusterprofile --hub-context=my-hub --require-heartbeat
```

Pass `--hub-context` explicitly. Without it the planner silently uses your
current context, which is rarely the hub.

Properties the members publish, all under `agents.x-k8s.io/`:
`heartbeat`, `warmpool-depth`, `warmpool-ready`, `active-claims`,
`node-pressure-score`, `max-replicas`, `sandbox-capacity`. Profiles live in
`fleet-system` at `v1alpha1` by default; override with `--hub-namespace` and
`--hub-api-version`.

### Two things that will bite you

**A cluster with no `sandbox-capacity` property does not drop out — it gets
weight 1.0.** It stays eligible, receives pools, and takes a rounding-error
share of the fleet. The plan succeeds, no error is raised, and the fleet lands
short. Check the published properties on every profile before a large apply.
Note that `fleetctl show-registry` always describes the *published* spec and
takes no `-f`, so it answers "is every member fresh?" and not "what will this
spec do?".

**Avoid `--loop` for a one-shot fill.** A heartbeat that goes stale mid-run
triggers a replan, and a replan against a fleet that is already full will reap
sandboxes.

## `fleetctl`

| command | what it does |
|---|---|
| `apply -f SPEC` | plan and write spec + `assignments.json` to the bucket |
| `status` | per-cluster table: depth, ready, claim P90 |
| `show-assignments` | dump the current `assignments.json` |
| `show-registry` | dump the live planner registry (freshness check) |
| `route --template NAME` | which cluster hosts a template, plus a ready-to-run `gcloud get-credentials` |

Inventory flags apply to every subcommand: `--inventory`, `--hub-kubeconfig`,
`--hub-context`, `--hub-namespace`, `--hub-token-source`, `--hub-api-version`,
`--cluster-manager`, `--require-heartbeat`. The bucket comes from `--bucket` or
`$FLEET_BUCKET`.

Each `apply` advances the assignment generation by one, derived from whatever
is already published — there is no `generation` field to author in the spec,
and one left over in an old spec file is warned about and ignored. `apply
--generation N` overrides that for replay and disaster recovery; it is
rejected unless `N` is strictly greater than the published generation. Two
concurrent applies do not interleave: the loser gets a `CASConflict` instead
of overwriting a plan it never read.

## What this deliberately does not do

- **CRD-ify the fleet spec.** It ships as JSON in a bucket. Promoting it to a
  `SandboxFleet` CRD on the hub is future work.
- **Route user-facing `SandboxClaim`s across clusters.** This is fleet-driven
  pre-warming only; claim routing is a separate concern.
- **Distribute model weights.** `python/agent_sandbox_fleet/trainer.py` writes
  manifests and fake deltas to the bucket for reference, but nothing consumes
  them. A real loop needs a counterpart in the inference backend.
- **Generic multi-cluster anything.** Scope is RL and batch workloads on
  agent-sandbox. This is not a Karmada/KubeFleet/OCM/MultiKueue alternative and
  will not grow into one.

## Layout

```
fleet/
├── ARCHITECTURE.md              deep design
├── python/agent_sandbox_fleet/  reuses the agent-sandbox Python SDK
│   ├── fleet_member.py          per-cluster daemon: reconcile + publish capacity
│   ├── planner.py               the plan behind `fleetctl apply`
│   ├── cli.py                   fleetctl
│   ├── inventory.py             GCS and ClusterProfile inventory sources
│   ├── publisher.py             writes capacity into a ClusterProfile (SSA)
│   ├── hubauth.py / hubcheck.py hub credentials and preflight
│   ├── placement.py             cluster selectors
│   ├── budget.py                Hamilton largest-remainder split
│   ├── sizing.py                replica sizing
│   ├── resolver.py              template → cluster routing
│   └── objectstore.py           GCS client
├── deploy/                      Dockerfile, RBAC, Deployments, hub setup scripts
└── demo/                        example specs and the claim-path benchmark
```
