# Testing guide

Three ways to exercise the fleet layer, ordered by cost:

1. **Unit tests** — no infra, seconds.
2. **Offline planner preflight** — no infra, proves a spec plans to the totals
   you intended before you spend a fleet on finding out it doesn't.
3. **Real GKE** — N clusters + a GCS bucket + Workload Identity. Either driven
   by hand (§3) or by the one-command driver (§4).

There is no kind path. The fleet layer's whole subject is per-cluster capacity
reporting and cross-cluster placement, and neither is meaningfully exercised by
two single-node clusters on one laptop.

## Prereqs

```bash
gcloud auth login
gcloud auth application-default login
gcloud config set project <YOUR_PROJECT>
gcloud services enable container.googleapis.com storage.googleapis.com iam.googleapis.com
```

Local: `python` 3.10+, `kubectl` 1.31+, `docker` 26+, gcloud SDK 500+.

## 1. Unit tests — no infra

```bash
cd fleet/python
python3 -m venv .venv && . .venv/bin/activate
pip install -e '.[test]'
pytest -v
```

Covers the placement selectors, the Hamilton largest-remainder budget split,
replica sizing, the planner end to end, both inventory sources, the
`ClusterProfile` publisher, hub auth, the object store, and the fleet-member's
reconcile loop against a fake API server.

The budget parity assertions in `tests/test_budget.py` are pinned to reference
values, not recomputed — the split is the one thing here that has to be exact,
because a rounding error becomes a fleet that lands short.

## 2. Offline planner preflight — no infra

A spec with `cluster_weights: {}` moves the budget split out of the file and
into whatever the members publish as `agents.x-k8s.io/sandbox-capacity`. That
is the point of the `ClusterProfile` integration, and it is also the failure
mode: the spec no longer states the answer, so nothing catches a wrong capacity
until a six-cluster run has already been spent.

`preflight-cp-plan.py` runs the real `ClusterProfileInventory.load()` and the
real `planner.plan()` against synthetic profiles — only `list_profiles()` is
substituted, so the Hamilton split, `compute_replicas`, the spread-first
pre-pass and the `min_clusters` round-robin all execute as they will on the day.

```bash
# does the intended publication reproduce the intended plan?
./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml

# what is actually published — what would it really do?
./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml \
  --capacities cluster-a=199000,cluster-b=199000,cluster-c=199000

# what happens if one member never publishes?
./demo/preflight-cp-plan.py demo/fleet-spec-xs.yaml --omit cluster-c
```

Exit 0 = the plan matches the spec's own arithmetic. Exit 1 = it does not, and
the per-cluster diff is printed.

Run the `--omit` case at least once. A cluster with no `sandbox-capacity`
property does **not** drop out — it gets weight 1.0, stays eligible, and takes
a rounding-error share. Nothing errors; the fleet simply lands short.

## 3. Real GKE — by hand

### 3a. Bucket and service account

```bash
export PROJECT=<YOUR_PROJECT>
export REGION=us-central1
export FLEET_BUCKET=agent-sandbox-fleet-$USER
export GSA_EMAIL=fleet-agent@$PROJECT.iam.gserviceaccount.com

gsutil mb -p $PROJECT -l $REGION gs://$FLEET_BUCKET
gcloud iam service-accounts create fleet-agent --display-name="Fleet member GCS access"
gsutil iam ch serviceAccount:$GSA_EMAIL:objectAdmin gs://$FLEET_BUCKET
```

### 3b. Clusters

Two is the minimum useful test. Each cluster costs roughly $70/mo idle.

```bash
export CLUSTERS=(fleet-a fleet-b fleet-c)

for c in "${CLUSTERS[@]}"; do
  gcloud container clusters create-auto $c \
      --project=$PROJECT --region=$REGION --release-channel=rapid
done
```

Autopilot is recommended here because it removes node-pool sizing from the
test. `deploy/create-gke-standard-fleet.sh` builds Standard clusters with image
streaming enabled instead, which is what you want once you start caring about
warm-pool fill time rather than correctness.

### 3c. agent-sandbox, RBAC, and Workload Identity, per cluster

```bash
for c in "${CLUSTERS[@]}"; do
  gcloud container clusters get-credentials $c --region=$REGION --project=$PROJECT

  # agent-sandbox itself, including the extension controllers that own
  # SandboxTemplate and SandboxWarmPool.
  kubectl apply -f ../k8s/crds/

  kubectl apply -f deploy/rbac.yaml

  gcloud iam service-accounts add-iam-policy-binding $GSA_EMAIL \
      --role=roles/iam.workloadIdentityUser \
      --member="serviceAccount:$PROJECT.svc.id.goog[multi-cluster-fleet/fleet-member]"
  kubectl annotate serviceaccount fleet-member -n multi-cluster-fleet \
      iam.gke.io/gcp-service-account=$GSA_EMAIL --overwrite
done
```

### 3d. Build and push the member image

```bash
gcloud artifacts repositories create fleet-images \
    --repository-format=docker --location=$REGION 2>/dev/null || true
gcloud auth configure-docker $REGION-docker.pkg.dev

export IMAGE=$REGION-docker.pkg.dev/$PROJECT/fleet-images/fleet-member:v0.1.0
./deploy/build-push.sh
```

The build context is the **repo root**, not `fleet/` — the Dockerfile copies
both `clients/python/agentic-sandbox-client` (the SDK) and `fleet/python`.

### 3e. Deploy one member per cluster

```bash
for c in "${CLUSTERS[@]}"; do
  gcloud container clusters get-credentials $c --region=$REGION --project=$PROJECT
  CLUSTER_NAME=$c FLEET_BUCKET=$FLEET_BUCKET IMAGE=$IMAGE \
    ./deploy/render.sh deploy/fleet-member-deployment-wi.yaml | kubectl apply -f -
  kubectl -n multi-cluster-fleet rollout status deployment/fleet-member --timeout=120s
done
```

Use `render.sh`, not bare `envsubst`. `envsubst` expands an unset variable to
empty and emits the manifest anyway; `render.sh` refuses. This matters most for
`--cluster-manager`, where empty is falsy and drops the label selector, so the
member inventories every `ClusterProfile` on the hub — including other fleets'.

### 3f. Verify the members are reporting

```bash
gsutil ls gs://$FLEET_BUCKET/fleet/capacity/
gsutil cat gs://$FLEET_BUCKET/fleet/capacity/fleet-a.json | jq
```

Expected: one `fleet/capacity/<cluster>.json` per cluster, `updated_at` within
the last 60 s, `warmpool_depth: 0` initially.

### 3g. Apply a spec and watch placement

Edit `demo/fleet-spec-xs.yaml` so `cluster_weights` names your clusters, then:

```bash
pip install -e ./python
fleetctl apply -f demo/fleet-spec-xs.yaml
fleetctl status
gsutil cat gs://$FLEET_BUCKET/fleet/assignments.json | jq

CLUSTERS="${CLUSTERS[*]}" PROJECT=$PROJECT REGION=$REGION ./demo/observe.sh
```

`observe.sh` prints a per-cluster snapshot plus the GCS side. Note that bash
does not export arrays — pass `CLUSTERS="${CLUSTERS[*]}"` as shown, or the
script silently falls back to its two-cluster default.

Expected: `assignments.json` in the bucket, members reconcile within 30 s, and
per-image replica counts across clusters sum to each image's `target_replicas`.

### 3h. Failure injection

```bash
./demo/observe.sh --fail-modes
```

Runs all three: kill a member (its capacity report ages out, the next `apply`
shifts assignments away), hand-delete a warm pool (the member recreates it on
the next reconcile), and corrupt `assignments.json` (members log a parse error
and keep serving last-known-good).

Each should recover without operator intervention. The corrupt-JSON case is the
one worth watching closely — a member that *stops* serving on a bad read is a
regression, because the bucket is a shared blast radius.

### 3i. Teardown

Drain **through** the fleet layer first:

```bash
fleetctl apply -f demo/fleet-spec-drain.yaml
```

Do not just scale the members to zero. A member that restarts reads whatever
assignment is still in the bucket and begins reconciling it immediately — that
has silently re-warmed a fleet well before its intended start time and made the
measurement that followed unusable.

Then:

```bash
for c in "${CLUSTERS[@]}"; do
  gcloud container clusters delete $c --region=$REGION --project=$PROJECT --quiet
done
gsutil rm -r gs://$FLEET_BUCKET/**
gsutil rb gs://$FLEET_BUCKET
gcloud iam service-accounts delete $GSA_EMAIL --quiet
```

## 4. Real GKE — the XS driver

`demo/run-xs-e2e.sh` is §3 in one command, across three XS Standard clusters
with image streaming on. Every phase is idempotent, so re-running after a
failure resumes rather than duplicates.

```bash
PROJECT=my-proj FLEET_BUCKET=my-bucket ./demo/run-xs-e2e.sh
```

| phase | what it does |
|---|---|
| 1 | verify prereqs — env vars, gcloud auth, quota estimate |
| 2 | create N clusters via `deploy/create-gke-standard-fleet.sh` |
| 3 | install agent-sandbox + fleet-member on each; bucket + WI binding |
| 4 | apply `demo/fleet-spec-xs.yaml`, measure cold wall-clock to fully warm |
| 5 | claim stress test, print summary |
| 6 | optional `--spindown` — node pools to zero, control planes left up |

Overridable: `ZONE`, `CLUSTER_PREFIX`, `N_CLUSTERS`, `FLEET_MEMBER_IMAGE`,
`STRESS_RATE`, `STRESS_DURATION`, `SKIP_STRESS`, `SKIP_TEARDOWN`, and `PHASE`
to run exactly one phase.

Phase 6 does not delete anything — it scales node pools to zero and leaves the
control planes billing at roughly $0.30/hr. For a real teardown use
`./deploy/create-gke-standard-fleet.sh delete`.

`deploy/example-templates-xs.yaml` holds the `SandboxTemplate` CRs the XS spec
expects. Templates are operator-managed; the fleet-member only verifies they
exist and will not create them for you.

## 5. Claim load test

§3 and §4 exercise the WarmPool → Sandbox path. `stress-e2e.py` exercises the
full lifecycle — WarmPool → Sandbox → **SandboxClaim adoption** → Ready →
delete — and is what makes the capacity-aware selector respond to real load.

```bash
cd python && . .venv/bin/activate
export FLEET_BUCKET=<your-bucket> PROJECT=<your-project> REGION=us-central1

python ../demo/stress-e2e.py --rate 5 --duration 60
```

It reads the current `assignments.json`, rotates through every pool weighted by
size, refreshes kubeconfig contexts itself, and cleans up after itself unless
told otherwise. It prints per-cluster counts, latency percentiles, and a
warm-hit rate.

- `--rate` claims/sec. Ramp to 20+ to stress warm-pool replenishment.
- `--duration` seconds. `--rate 20 --duration 300` ≈ 6k claims.
- `--keep-claims` leaves the `SandboxClaim`s in place for inspection.
- `--concurrency` in-flight operations, default 32.

Watch `fleetctl status` in another terminal: `wp_depth` should stay roughly
constant, `wp_ready` may dip and recover, and `active_claims` should climb to a
plateau and return to 0 afterwards.

**Read the warm-hit rate, not the latency.** Above 90% means claims are being
served from the pre-warmed pool. Below 50% means the pool is draining faster
than the `SandboxWarmPool` controller replenishes it, and every latency number
in the summary is then measuring cold sandbox creation instead of the fleet.

## 6. Acceptance checklist

- [ ] `pytest -v` in `fleet/python` — 100% pass
- [ ] `python -m agent_sandbox_fleet.fleet_member --help` — entrypoint works
- [ ] `IMAGE=... ./deploy/build-push.sh` — image builds from the repo root
- [ ] `./demo/preflight-cp-plan.py <spec>` — exit 0; and `--omit <cluster>`
      shows the short landing rather than an error
- [ ] 3+ clusters, one member each, all publishing capacity within 60 s
- [ ] `fleetctl apply` on a 3-image spec spreads pools per the selected policy,
      and per-image totals sum **exactly** to `target_replicas`
- [ ] Fail-mode 1 (kill member): assignments shift on the next `apply`
- [ ] Fail-mode 2 (delete pool): member recreates within 60 s
- [ ] Fail-mode 3 (corrupt JSON): member logs the error and keeps serving
- [ ] `stress-e2e.py --rate 5 --duration 60` — >95% success, warm-hit >80%
- [ ] `active_claims` climbs during the stress run and returns to 0
- [ ] Member memory stays under 200 MiB with 50 `SandboxWarmPool`s managed
- [ ] A second `fleetctl apply` while the first is in flight loses with
      `CASConflict` rather than overwriting a plan it never read
- [ ] `fleetctl apply -f demo/fleet-spec-drain.yaml` empties the fleet

## Troubleshooting

**`storage: NotAuthenticated`** — the WI binding didn't take:

```bash
kubectl -n multi-cluster-fleet describe sa fleet-member | grep iam.gke.io
gcloud iam service-accounts get-iam-policy $GSA_EMAIL
```

If you ran `setup-hub.sh`, note that it annotates each member's ServiceAccount
with a new per-member GSA, which redirects *every* Google API call from that
pod — including the bucket writes. That is why `--bucket` is required there.

**Capacity reports never appear** — check the member's logs for GCS 403s, then
bucket-level IAM for the GSA.

**Warm pools created but pods `Pending`** — not a fleet problem. That's the
sandbox controller or the GKE scheduler; `kubectl describe pod` in the target
cluster.

**Every hub call times out while bucket writes keep working** — routing, not
credentials. A GKE control plane's public IP is not a Google API endpoint, so
Private Google Access does not cover it. Use `--private-endpoint` when members
and hub share a VPC. Members read the hub ConfigMap once at startup, so
`kubectl rollout restart deployment/fleet-member` after any change to it.

**`fleetctl apply` succeeds but nothing reconciles** — compare the generation in
`assignments.json` against the `fleet.agent-sandbox.io/assignment-generation`
annotation on any managed pool, and check `--reconcile-interval`.

**Assignments keep churning mid-run** — you are probably using `--loop` for a
one-shot fill. A heartbeat that goes stale triggers a replan, and a replan
against a fleet that is already full will reap sandboxes.

**Two members in the same cluster fighting** — one replica per cluster is the
supported configuration (`replicas: 1`, `strategy: Recreate`). Confirm nobody
scaled it up.

## Cost notes

A 3-cluster Autopilot test fleet, idle: roughly $219/mo in cluster management
fees, ~$5/mo for the member pods, under $1/mo for GCS and Artifact Registry.

Don't leave a test fleet running. §3i for the manual path,
`./deploy/create-gke-standard-fleet.sh delete` for the XS driver.
