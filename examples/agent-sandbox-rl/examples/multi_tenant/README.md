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
- [Several clusters](#several-clusters)
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
| [`admin/tenant.yaml`](admin/tenant.yaml) | admin | Namespace `rl-<user>`, ServiceAccount, Role and bindings, ResourceQuota, LimitRange, and a NetworkPolicy that selects every pod in the namespace (a tenant may create a template by hand, without the SDK's pod label). Rendered by `render.py`, which refuses to leave a placeholder unfilled. |
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

**1. The admin provisions two tenants** and a kubeconfig for each. A tenant
name is a lowercase DNS-1123 label of at most 60 characters and is used as is
everywhere: the namespace is `rl-<user>`, and `run_experiment.py` refuses a
name that does not fit. The defaults give a tenant 16 pods, 4 CPU, 8 GiB and
16 claims; on kind the sandboxes request 100m and 128Mi each.

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
2. `footprint` is what the strategy holds at its peak: the warm replicas of
   the pools that are warm at once (`sliding`: the SDK's own
   `sizing.recommend_window`; `pipelined`: up to two windows; `naive`: every
   pool), plus one claimed pod and one refilling pod per concurrency slot.
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

## Several clusters

A fleet with several `ClusterConfig`s changes where things live, not the
pattern. Each `ClusterConfig` carries its own context, namespace and
`max_replicas`, and the SDK already loops per cluster for pools, pre-pull,
teardown and the report. What the example does differently with
`--contexts a,b`:

| Concern | One cluster | Several clusters |
| --- | --- | --- |
| The user's namespace | `rl-<user>` once | `rl-<user>` on **every** cluster: `provision.sh <user> --contexts a,b` applies the tenant manifest per context (GKE Fleets: Config Sync does the same from one repo) |
| RBAC and quota | one Role, one quota | one per cluster; the budget is per cluster, so each `ClusterConfig.max_replicas` is set from that cluster's quota |
| Image layers | one node cache, shared by all namespaces on it | **one cache per cluster**: cluster A's nodes do nothing for cluster B. `prepull_shared.py --contexts a,b` pulls on every cluster at once; Image Streaming is per cluster too, and Artifact Registry in the same region serves all of them |
| Placement | n/a | the default `image-affinity` maps an image to a cluster by the sorted cluster names. The runner names a cluster after the admin's context (`rl-alice@a` and `rl-bob@a` are both `a`), so every user puts an image on the same cluster and finds it in that cluster's cache. `least-loaded` and `capacity-weighted` can put one image on different clusters for different users; then only a pre-pull on every cluster keeps claims warm |
| Quota fit | one budget | one budget per cluster: the tightest cluster bounds the concurrency (placement may put a whole window there) and each budget becomes that cluster's `max_replicas`. The fleet's `max_live_sandboxes` is the sum only when every cluster has a quota; with a quota on some clusters only, the SDK's own ceiling stays, because a sum of the known budgets would also cap the quota-free clusters |
| Node pools inside one cluster | n/a | names are keyed by image and both configs share the fleet's run id, so two configs with the same context need **distinct namespaces** (package README, "Multiple clusters and multiple node pools"). Rather than a namespace per user per pool, use one config per cluster without `node_selector` and a node affinity in `TemplateSpec.extra_pod_spec` |
| Teardown and reaper | per run, per namespace | the same, per cluster; the runner prints one reaper command per context |

The commands are the single-cluster ones with a context list:

```bash
admin/provision.sh alice --contexts kind-agent-sandbox,kind-agent-sandbox-b
admin/tenant_kubeconfig.sh alice --contexts kind-agent-sandbox,kind-agent-sandbox-b -o /tmp/rl-alice.kubeconfig
python admin/prepull_shared.py --contexts kind-agent-sandbox,kind-agent-sandbox-b
python run_many.py --users alice,bob --experiments 2 \
    --kubeconfig-pattern '/tmp/rl-{user}.kubeconfig' \
    --contexts-pattern 'rl-{user}@kind-agent-sandbox,rl-{user}@kind-agent-sandbox-b' \
    -- --tasks-per-image 2 --max-concurrent 3
./verify.sh alice bob --contexts kind-agent-sandbox,kind-agent-sandbox-b
```

The tenant kubeconfig holds one context per cluster, `rl-<user>@<ctx>`, and
`run_many.py` fills `{user}` into the context pattern per user. The table
gains a "tasks per cluster" column, which shows the affinity split.

### Recorded on two kind clusters

Two kind clusters on one laptop (`kind-agent-sandbox` and `kind-agent-sandbox-b`,
one node each, the same Docker VM with 6 CPU and 12 GiB), both tenants
provisioned on both with the default quota, the same ten images and settings as
the single-cluster runs, `image-affinity` placement. The split the affinity
chose for these ten images is 4 images on cluster A and 6 on cluster B, so a
20-task run lands 8 tasks on A and 12 on B, for every user.

**The second user starts from the first user's pulls, on both clusters.**
Images removed from both nodes, no pre-pull.

| Run | Total | Pool ready, max | Tasks per cluster | Registry pulls, A / B | "already present" events, A / B |
| --- | --- | --- | --- | --- | --- |
| alice, both clusters cold | 41.7 s | 13.7 s | A=8 B=12 | 4 / 6 | 8 / 12 |
| bob, right after | 10.3 s | 1.25 s | A=8 B=12 | 0 / 0 | 12 / 18 |

The registry pulls are the ten images once, split 4 and 6 the way the affinity
split the images: a cluster pulls only the images whose pools it hosts, and bob
pulls nothing anywhere.

**Four experiments at once, two per user, on both clusters** (images cached).
Each runner fitted the quota per cluster, 8 pods each after `--share 2`, and
lowered its concurrency from 3 to 2.

```
user   experiment  namespace  claims  tasks per cluster                             claim avg s  claim max s  tasks  errors  total s  teardown clean
-----  ----------  ---------  ------  --------------------------------------------  -----------  -----------  -----  ------  -------  --------------
alice  exp1        rl-alice   20      kind-agent-sandbox=8 kind-agent-sandbox-b=12  1.49         6.81         20     0       29.24    True
alice  exp2        rl-alice   20      kind-agent-sandbox=8 kind-agent-sandbox-b=12  0.96         2.72         20     0       26.67    True
bob    exp1        rl-bob     20      kind-agent-sandbox=8 kind-agent-sandbox-b=12  1.35         8.03         20     0       30.73    True
bob    exp2        rl-bob     20      kind-agent-sandbox=8 kind-agent-sandbox-b=12  1.38         5.41         20     0       29.24    True
```

**The admin pre-pulls on both clusters at once**, after removing the images
from both nodes again: 65 s for the ten images on two clusters. Then alice and
bob start together: 15.7 s and 11.7 s, pool ready at most 1.6 s, and `rl-bob`
shows zero registry pulls on either cluster.

**A quota that differs per cluster.** With bob's quota on cluster B lowered to
8 pods by hand (A stays at 16), the runner reports budgets of 16 on A and 8 on
B, lowers the concurrency from 3 to 2 because the tightest cluster bounds it,
sets `max_replicas` 16 and 8 on the two `ClusterConfig`s, and still fills both
clusters: 20 tasks, A=8 B=12, 11.3 s, teardown verified clean on both.

`verify.sh alice bob --contexts kind-agent-sandbox,kind-agent-sandbox-b` runs the
isolation check on each cluster; all eight checks pass, and both namespaces on
both clusters are empty after every run.

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
- **Node pools inside one cluster.** Two `ClusterConfig`s with the same
  context need distinct namespaces, because names are keyed by image (see
  [Several clusters](#several-clusters)); the example does not build a
  namespace per node pool.
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
| A second kind cluster never gets past "Starting control-plane"; containerd logs `failed to create cni conf monitor ... fsnotify watcher` | the Docker VM's `fs.inotify.max_user_instances` (128 by default) is used up by the first cluster. Raise it in the VM, then restart containerd and kubelet on the new node: `docker run --rm --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sysctl -w fs.inotify.max_user_instances=8192` |
| Docker Hub `429` during the cold pull | mirror the images or pre-pull once with `admin/prepull_shared.py` |
