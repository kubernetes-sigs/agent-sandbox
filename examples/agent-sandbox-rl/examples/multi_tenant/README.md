# Many users, one cluster: a namespace per user, one image set

Several people run their own experiments on one cluster, against the same task
images, each in a namespace of their own. A user sees, resizes and deletes only
their own templates, pools and claims; a user cannot hold more of the cluster
than the admin gave them; the image bytes are pulled once per node and serve
every user. Nothing here changes the SDK. It composes `ClusterConfig.namespace`,
`run_isolation="names"`, `FleetConfig.labels`, `fleet.prepull()`,
`max_live_sandboxes` and `teardown(verify=True)` with the Kubernetes objects an
admin provisions once per user.

- [What is cached, and what is shared across namespaces](#what-is-cached-and-what-is-shared-across-namespaces)
- [Layouts compared](#layouts-compared)
- [Files](#files)
- [Walk-through on kind](#walk-through-on-kind)
- [What a tenant may do](#what-a-tenant-may-do)
- [Sizing under the quota](#sizing-under-the-quota)
- [On GKE](#on-gke)
- [Recorded runs](#recorded-runs)
- [Limits and follow-ups](#limits-and-follow-ups)
- [Troubleshooting](#troubleshooting)

## What is cached, and what is shared across namespaces

Image bytes live on the node, below the namespace. Running pods live in a
namespace. That one line decides the design: when two namespaces share nodes,
the second user's pod finds the image already on the node and starts without a
pull.

| Layer | Where it lives | Shared across user namespaces |
| --- | --- | --- |
| Image layers | the node's image cache (kubelet and containerd), `imagePullPolicy: IfNotPresent` | **yes**. The kubelet event reads `Container image "..." already present on machine`; `verify.sh` counts those |
| `fleet.prepull()` DaemonSet | the DaemonSet is namespaced; the layers it pulls land in the node cache | **yes**, the layers stay after the DaemonSet is gone |
| GKE Image Streaming | per node, lazy loads from Artifact Registry | **yes** |
| GKE secondary boot disk (image preloading) | baked into the node pool's disk image | **yes**, new nodes start warm |
| Registry mirror, pull-through cache, peer-to-peer distribution | regional, or node to node | **yes** |
| `SandboxTemplate`, `SandboxWarmPool`, warm pods | the user's namespace; a claim binds only to a pool in its own namespace | **no**: every user holds their own pools |
| `SandboxClaim`, `Sandbox`, pods | the user's namespace | **no**: this is the isolation |
| `ResourceQuota`, `LimitRange`, RBAC, `NetworkPolicy` | the user's namespace | **no**: this is the budget and the boundary |
| Pod Snapshots (gVisor) | a checkpoint per sandbox | **no** |

Three things to know on a shared cluster:

- **Private images.** User B can start user A's private image from the node
  cache without credentials. Clusters that close that with the
  `AlwaysPullImages` admission plugin pay a registry round trip per start.
  Newer kubelets close it with pull-credential verification (beta in Kubernetes
  1.34): a second pod re-pulls unless it presents the same credentials. Images
  pulled with the node's own identity, such as the GKE node service account
  reading Artifact Registry, are not affected, so that is what this example
  assumes.
- **Image garbage collection.** The node cache is not pinned. The kubelet
  evicts images past the disk high threshold or the maximum unused age, so a
  large shared set on small boot disks gets evicted between users. Bigger boot
  disks, Image Streaming, a secondary boot disk, or
  `prepull_shared.py --keep` (its pods keep the images referenced) prevent that.
- **Pools are per user.** Two users warming the same image hold two sets of
  idle pods. The layers are shared, the pods' CPU and memory are not. The
  namespace quota bounds that.

## Layouts compared

The SDK keys template and pool names by image (`r2e-img-<hash>`,
`pool-<template>`), so two fleets in one namespace that warm the same image
meet on one name. The package README's "Concurrent runs on one cluster" covers
the ways to live with that. Side by side with this example:

| Layout | Knobs | Warm pods per image | Image layers | Who sees or deletes whose objects | Quota and RBAC boundary | Teardown unit | Use when |
| --- | --- | --- | --- | --- | --- | --- | --- |
| One namespace, hash names | `run_isolation="none"` (default) | one set; a second run on the image is refused with a `FleetError` naming the owner | shared | everyone in the namespace | the namespace, shared by all | this run's objects by run-id label | one run at a time per namespace |
| One namespace, shared on purpose | `run_id="<job>"` on every process, or `adopt_existing=True` | **one set, shared**: the only layout that shares warm pods | shared | all consumers claim from the same pools | the namespace | `reap(run_id=...)` once per job | one job with several processes, or a platform-owned warm fleet |
| One namespace, names per run | `run_isolation="names"` | one set per run | shared | everyone still sees everything | the namespace | this run's names | several runs of one team |
| Namespace per run | `run_isolation="namespace"` | one set per run | shared | per run | per run, but the fleet identity creates namespaces and `run_namespace_setup` must add the quota every run | the namespace | a pipeline that wants a throwaway namespace per run |
| **Namespace per user, names per experiment** (this example) | admin-provisioned `rl-<user>` + `run_isolation="names"` | one set per user and experiment | **shared** when the namespaces share nodes | a user sees only their namespace (403 elsewhere) | per user, provisioned once, no cluster rights for the user | the experiment (`teardown`), the user (`reap(all_managed=True)`) | several users on one cluster with their own budgets |

Moving from one namespace to a namespace per user costs the sharing of warm
pods, and only that. The image layers are on the same nodes either way, so the
second user's claims start as fast as the first user's second pass.

## Files

| File | Who runs it | What it does |
| --- | --- | --- |
| [`admin/tenant.yaml`](admin/tenant.yaml) | admin | Namespace `rl-<user>`, ServiceAccount, Role and bindings, ResourceQuota, LimitRange, NetworkPolicy. Rendered by `render.py`, which refuses to leave a placeholder unfilled. |
| [`admin/cluster-read.yaml`](admin/cluster-read.yaml) | admin | the one ClusterRole tenants need: `get` on the four CRDs (preflight) and on RuntimeClass. |
| [`admin/provision.sh`](admin/provision.sh) | admin | `provision.sh <user> [--pods 16 --cpu 4 --memory 8Gi --claims 16]` applies both; `--delete` removes the tenant. |
| [`admin/tenant_kubeconfig.sh`](admin/tenant_kubeconfig.sh) | admin | writes a kubeconfig with a short-lived token for the tenant's ServiceAccount. |
| [`admin/prepull_shared.py`](admin/prepull_shared.py) | admin | pulls [`images.txt`](images.txt) onto the sandbox nodes once, from the `rl-shared` namespace. |
| [`run_experiment.py`](run_experiment.py) | user | one experiment in the user's namespace: fits the quota, runs the tasks, verifies the teardown, writes `report.json`. |
| [`run_many.py`](run_many.py) | demo | N users × M experiments at once, one table at the end. |
| [`verify.sh`](verify.sh) | admin | objects and quota use per namespace, the cached-pull events, and the cross-namespace 403. |

## Walk-through on kind

A kind cluster with the extension controllers, from the repository root:

```bash
EXTENSIONS=true make deploy-kind
export KUBECONFIG=$PWD/bin/KUBECONFIG
pip install -e clients/python/agentic-sandbox-client -e examples/agent-sandbox-rl
cd examples/agent-sandbox-rl/examples/multi_tenant
```

**1. The admin provisions two tenants** and a kubeconfig for each. The
defaults give a tenant 16 pods, 4 CPU, 8 GiB and 16 claims; on kind the
sandboxes request 100m and 128Mi each.

```bash
admin/provision.sh alice
admin/provision.sh bob
admin/tenant_kubeconfig.sh alice -o /tmp/rl-alice.kubeconfig
admin/tenant_kubeconfig.sh bob   -o /tmp/rl-bob.kubeconfig
KUBECONFIG=/tmp/rl-alice.kubeconfig kubectl get sandboxclaims            # own namespace: empty list
KUBECONFIG=/tmp/rl-alice.kubeconfig kubectl get sandboxclaims -n rl-bob  # Forbidden
```

**2. The admin pre-pulls the image list once.** The DaemonSet runs in
`rl-shared`; the layers land on every node. `--keep` leaves it so nodes the
autoscaler adds pull the list on arrival.

```bash
python admin/prepull_shared.py --images images.txt
```

**3. Each user runs experiments in their namespace.** Two experiments per user,
all four at once, with the user's own kubeconfig:

```bash
python run_many.py --users alice,bob --experiments 2 \
    --kubeconfig-pattern '/tmp/rl-{user}.kubeconfig' --report-dir reports \
    -- --tasks-per-image 2 --max-concurrent 3
```

Each runner reads the namespace's `ResourceQuota` first and lowers its
concurrency to what fits (see [Sizing under the quota](#sizing-under-the-quota)),
runs its tasks with the `sliding` strategy, tears down and runs the run-scoped
reaper to verify that nothing is left. Reports go to
`reports/<user>/<experiment>/report.json`; the driver prints one table.

**4. Verify**, as the admin, during or after the runs:

```bash
./verify.sh alice bob
```

Per namespace it prints the fleet objects (grouped by run id while runs are
live), the quota's used/hard columns, the NetworkPolicy, and the kubelet's
`Pulled` events split into pulls from the registry and "already present on
machine". Then it impersonates each tenant's ServiceAccount: reading its own
claims passes, reading the neighbor's is refused.

**5. Clean up.** A run that ends cleanly leaves nothing. For a crashed run,
the runner printed the reaper command for its run id; an admin sweeps a whole
tenant with the reaper or removes the tenant:

```bash
python -m agent_sandbox_rl.reaper --all --namespace rl-alice
admin/provision.sh alice --delete
```

## What a tenant may do

The Role in `tenant.yaml` is the list of API calls the SDK, its reaper and the
runner make inside a namespace, and nothing else:

| Objects | Verbs | Why |
| --- | --- | --- |
| `sandboxtemplates`, `sandboxwarmpools`, `sandboxclaims` | get, list, watch, create, update, patch, delete | the fleet's own objects |
| `sandboxes` | get, list, watch, delete | liveness checks and the reaper |
| `pods` | get, list, watch, delete, deletecollection | the breaker's pod count; the reaper force-deletes a run's pods |
| `pods/exec`, `pods/status`, `pods/log` | create, get | the handle runs commands over the exec API and checks the pod |
| `namespaces` (own), `resourcequotas`, `limitranges` | get, list | preflight; the runner's quota fit |
| `networkpolicies` | get, list | preflight checks that one covers the fleet's pods |
| `events` | get, list, watch | the user's own debugging |

Cluster-wide, one ClusterRole: `get` on the four CRDs and on RuntimeClass,
because preflight reads them. A small Role in the controller's namespace lets
the tenant `get` the controller Deployment, which preflight reports as a
warning otherwise. No DaemonSets (pre-pull is the admin's) and no `nodes`
(the report's node count is simply absent for a tenant).

## Sizing under the quota

The quota rejects a pod at admission. No pod exists then, so the fleet's
fail-fast, which watches pod conditions, sees nothing and a warm pool that
cannot grow waits out `ready_timeout`. The runner therefore fits itself
before it starts:

1. `pod_budget` converts the quota's free `pods`, `requests.cpu` and
   `requests.memory` into pods at the per-sandbox requests; the tightest wins.
   With `--share N` (set by `run_many.py` to the experiments a user starts at
   once) each experiment takes 1/N of that.
2. `footprint` is what the `sliding` strategy holds at its peak: the warm
   replicas of the pools the window keeps warm (the SDK's own
   `sizing.recommend_window`), plus one claimed pod and one refilling pod per
   concurrency slot.
3. `fit_to_quota` lowers `--max-concurrent` until the footprint fits, and the
   budget becomes the fleet's `max_live_sandboxes`, so the fleet's own circuit
   breaker and the quota agree.

The sandboxes run `sleep infinity`, which ignores SIGTERM, so the template
sets `terminationGracePeriodSeconds: 1`; otherwise every released sandbox
lingers 30 s and keeps counting against the quota and the breaker.

## On GKE

The same steps, with three differences:

- Point `--images` at a mirrored dataset slice in Artifact Registry (see the
  package README's "Image registry rewrite") and pull with the node service
  account, so the cross-namespace cache reuse is not affected by pull-credential
  verification.
- Pass `--node-selector` and, on GKE Sandbox pools, `--runtime-class gvisor`
  to `prepull_shared.py`, so the DaemonSet lands on the nodes the sandboxes use.
- Turn on Image Streaming for sets larger than the boot disk, or bake a
  secondary boot disk; `examples/openclaw-fleet-gke` documents both.

Bind a Google identity instead of, or next to, the ServiceAccount:
`provision.sh alice --user-subject alice@example.com`.

## Recorded runs

All runs on one kind node (6 CPU, 12 GiB, Kubernetes 1.35, containerd 2.2) with
the ten images of `images.txt`, two tasks per image, `--max-concurrent 3`,
`sliding`, sandboxes at 100m and 128Mi, the default tenant quota (16 pods,
4 CPU, 8 GiB, 16 claims). The node's copies of the ten images were removed
before each cold case (`crictl rmi` on the node). Every run ended with
`teardown_clean=True`: the run-scoped reaper found nothing remaining in the
namespace. Times are the `RunReport` phases; "pool ready" is the longest wait
for one warm pool, which is where an image pull shows up.

**The second user starts from the first user's pulls.** No pre-pull. alice runs
first on a node without the images, bob runs the same images right after, in
his own namespace.

| Run | Total | Pool ready, max | Claim, avg / max | Registry pulls in the namespace | "already present on machine" events |
| --- | --- | --- | --- | --- | --- |
| alice, cold node | 46.3 s | 13.8 s | 0.51 s / 1.16 s | 26 | 44 |
| bob, right after, same node | 8.5 s | 0.84 s | 0.52 s / 1.17 s | 0 | 46 |

**The admin pre-pulls once, then nobody is first.** Images removed again,
`admin/prepull_shared.py` run from `rl-shared` (one DaemonSet, deleted when
done), then alice and bob start at the same time.

| Step | Time | Note |
| --- | --- | --- |
| `prepull_shared.py`, ten images onto the node | 49 s | the same pulls alice paid above, paid once by the admin |
| alice, first run after the pre-pull | 9.9 s | pool ready max 1.0 s, claim avg 0.60 s |
| bob, at the same time | 8.8 s | pool ready max 1.0 s, claim avg 0.49 s, 0 registry pulls in `rl-bob` |

**Four experiments at once, two per user**, on the warmed node, through
`run_many.py` (which passes `--share 2`). Each runner saw 16 free pods, took a
budget of 8 and lowered its concurrency from 3 to 2, because the footprint of
three slots (three warm pools plus two pods per slot, 9 pods) does not fit 8.

```
user   experiment  namespace  claims  claim avg s  claim max s  tasks  errors  total s  teardown clean
-----  ----------  ---------  ------  -----------  -----------  -----  ------  -------  --------------
alice  exp1        rl-alice   20      0.8          2.94         20     0       19.89    True
alice  exp2        rl-alice   20      0.93         3.03         20     0       22.23    True
bob    exp1        rl-bob     20      0.86         3.27         20     0       21.27    True
bob    exp2        rl-bob     20      0.79         3.35         20     0       21.61    True
```

**`verify.sh alice bob` while two runs were live**, then after:

```
== rl-alice
   templates=3 warmpools=3 claims=2 sandboxes=0 pods=7 (+11 terminating)
     run 4870b63ff5a3: 3 pool(s), 1 ready
   quota: count/sandboxclaims.extensions.agents.x-k8s.io=2/16  pods=7/16  requests.cpu=700m/4  requests.memory=896Mi/8Gi
   networkpolicy: agent-sandbox-rl-fleet
   image pulls seen by kubelet: from registry=26  already present on machine=129
== rl-bob
   templates=1 warmpools=1 claims=0 sandboxes=0 pods=10 (+10 terminating)
     run 98d22eeb22a7: 1 pool(s), 0 ready
   quota: count/sandboxclaims.extensions.agents.x-k8s.io=0/16  pods=10/16  requests.cpu=1/4  requests.memory=1280Mi/8Gi
   networkpolicy: agent-sandbox-rl-fleet
   image pulls seen by kubelet: from registry=0  already present on machine=134
== isolation
   PASS alice reads rl-alice
   PASS alice is refused on rl-bob
   PASS bob reads rl-bob
   PASS bob is refused on rl-alice
```

After the runs both namespaces list zero templates, pools, claims, sandboxes
and pods. The registry pull count in `rl-alice` is the 26 pulls of its cold run
(ten images plus the pool refills that raced the first pull); `rl-bob` never
pulled from the registry in any run.

What was learned while recording, and is now in the code: a caller-set
`run_id` means a shared job to the SDK and turns off both the circuit breaker
and `verify_teardown`, so the runner names its objects with
`template_name_prefix` instead; `max_live_sandboxes` is a pod ceiling, not a
claim ceiling, so it is set to the quota's pod budget, not to `max_concurrent`;
and without `terminationGracePeriodSeconds: 1` the released `sleep infinity`
pods lingered 30 s and pushed bob's namespace to its 16-pod quota.

## Limits and follow-ups

- **Warm pools are per user.** A cross-namespace pool would be a controller
  feature; the example states the cost and points at `max_warmpool_size`.
- **One cluster.** With several `ClusterConfig`s the namespace, the quota and
  the pre-pull are needed on every cluster, and two node pools inside one
  cluster need distinct namespaces because names are keyed by image. A
  follow-up example covers it.
- **Quota per namespace, not per experiment.** Experiments of one user share
  the budget; `--share` is a convention, not an enforcement. A Kueue
  `LocalQueue` per tenant is the next step when fairness between users'
  experiments matters.
- **kind does not enforce NetworkPolicy** (kindnet). The policy is applied and
  verified as present; enforcement needs Calico, Cilium or GKE Dataplane V2.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `preflight failed — crd:sandboxclaims: 403` | the tenant lacks `cluster-read.yaml`; rerun `provision.sh` |
| `controller: ... 403` warning in preflight | the binding in the controller's namespace is missing; harmless, claims still work |
| `circuit breaker TRIPPED: N live pods > ceiling` | `max_live_sandboxes` below the footprint; let the runner fit the quota (do not pass `--no-quota-fit` with a small `--max-live`) |
| `SandboxPodCreateFailed ... exceeded quota` events | another experiment of the same user took the budget; pass `--share`, or raise the quota |
| `teardown_clean=False` | the reaper found leftovers; the runner prints the `reaper --run-id` command |
| Docker Hub `429` during the cold pull | mirror the images or pre-pull once with `admin/prepull_shared.py` |
