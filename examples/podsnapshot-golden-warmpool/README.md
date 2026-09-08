# Golden-Snapshot Warm Pools with GKE Pod Snapshots

Snapshot one "primer" sandbox — memory and filesystem — and have every
sandbox the `SandboxWarmPool` pre-warms afterward boot **already restored
from that snapshot**. One expensive initialization (clone a repo, warm a
cache, load a model into RAM), then N claims adopt pre-warmed sandboxes that
start from that exact live state.

This is a different pattern from the two the docs already cover:

- [Sandbox Snapshots](https://agent-sandbox.sigs.k8s.io/docs/sandbox/snapshots/)
  and the [Python SDK snapshot extension](../../clients/python/agentic-sandbox-client/k8s_agent_sandbox/gke_extensions/snapshots/README.md)
  park and resume **one session onto its own snapshots** (grouping by
  `agents.x-k8s.io/sandbox-name-hash` isolates every sandbox).
- [hermes-agents-as-a-service](../hermes-agents-as-a-service) suspends and
  resumes with **disk state only** — live process memory is lost.

Here the [`PodSnapshotPolicy`](./05-podsnapshotpolicy.yaml) groups snapshots
by the **shared template label** instead of the per-sandbox hash. GKE matches
restores by a hash of the (distilled) pod spec, not by pod identity, so every
new pod the template produces — including warm-pool members — automatically
restores from the latest `Ready` snapshot in the group.

```text
 primer sandbox                        SandboxWarmPool
 (memory + rootfs seeded)              (fresh members)
        |                                    |
        | PodSnapshotManualTrigger           | pod matches policy +
        v                                    | latest Ready snapshot
 golden snapshot  ---------------------->    v
 (GCS bucket)      restored on boot    members start with the
                                       primer's memory + files
                                             |
                                             v
                                   claims adopt pre-warmed,
                                   pre-initialized sandboxes
```

## Files

| File | Purpose |
|---|---|
| `image/` | Tiny state probe: in-memory boot nonce + counter, rootfs marker file |
| `00-storageconfig.yaml` | `PodSnapshotStorageConfig` → GCS bucket |
| `05-podsnapshotpolicy.yaml` | The golden-pool policy: template-label grouping, manual triggers |
| `10-sandboxtemplate.yaml` | gVisor `SandboxTemplate` + the pod `ServiceAccount` |
| `20-sandboxwarmpool.yaml` | Pool of 2 pre-warmed sandboxes |
| `25-primer-claim.yaml` | The claim whose sandbox becomes the golden image |
| `30-snapshot-trigger.yaml` | `PodSnapshotManualTrigger` for the primer pod |
| `40-worker-claims.yaml` | Claims that adopt restored pool members |
| `50-refresh-claim.yaml` | Claim adopting the freshest member after a snapshot-then-roll (§8) |
| `run-test-gke.sh` | The full walkthrough below as one asserting script |

## 1. Prerequisites

- A GKE cluster (Autopilot or Standard ≥ 1.35.3) with Pod Snapshots enabled
  and, on Standard, a gVisor node pool — see
  [Prepare for Pod snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots-prepare):

  ```sh
  gcloud container clusters create-auto podsnap-demo \
      --region us-central1 --enable-pod-snapshots
  ```

- The [agent-sandbox controller with extensions](../../README.md#installation)
  (v1.0.0+) installed.
- `envsubst`, `kubectl`, `gcloud`, `python3`, and Docker.

## 2. Snapshot storage bucket + IAM

Snapshots are stored in Cloud Storage; the **pod's** Kubernetes
ServiceAccount reads and writes them through Workload Identity:

```sh
export PROJECT_ID=<your-project> SNAPSHOT_BUCKET=<your-bucket>
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')

gcloud storage buckets create "gs://$SNAPSHOT_BUCKET" --location=us-central1 \
    --uniform-bucket-level-access --enable-hierarchical-namespace --soft-delete-duration=0

MEMBER="principal://iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$PROJECT_ID.svc.id.goog/subject/ns/default/sa/podsnap-golden-sa"
gcloud storage buckets add-iam-policy-binding "gs://$SNAPSHOT_BUCKET" --member="$MEMBER" --role=roles/storage.bucketViewer
gcloud storage buckets add-iam-policy-binding "gs://$SNAPSHOT_BUCKET" --member="$MEMBER" --role=roles/storage.objectUser

# GKE's service agent deletes snapshot objects (retention pruning and
# kubectl delete podsnapshots) — without this grant, deleted PodSnapshots
# hang in Terminating on their finalizer:
gcloud storage buckets add-iam-policy-binding "gs://$SNAPSHOT_BUCKET" \
    --member="serviceAccount:service-$PROJECT_NUMBER@container-engine-robot.iam.gserviceaccount.com" \
    --role=roles/storage.objectUser
```

Keep the bucket in the cluster's region, and note the docs' warning that
soft delete plus parallel composite uploads inflates storage cost — hence
`--soft-delete-duration=0`.

## 3. Build the state probe image

The probe holds state exactly where a snapshot must capture it: a boot nonce
and counter in **process memory**, and a marker file on the **container
rootfs** (deliberately not a volume). A restored pod keeps all three; a
cold-started pod has a fresh nonce, a zero counter, and no file.

```sh
export DEMO_IMAGE=us-central1-docker.pkg.dev/$PROJECT_ID/<repo>/podsnap-probe:v1
docker buildx build --platform linux/amd64 --push -t "$DEMO_IMAGE" image/
```

## 4. Deploy the snapshot config, template, and pool

```sh
envsubst < 00-storageconfig.yaml | kubectl apply -f -
kubectl apply -f 05-podsnapshotpolicy.yaml
envsubst < 10-sandboxtemplate.yaml | kubectl apply -f -
kubectl apply -f 20-sandboxwarmpool.yaml
kubectl wait sandboxwarmpool/podsnap-golden-pool --for=jsonpath='{.status.readyReplicas}'=2 --timeout=600s
```

These first two pool members **cold-start** — no snapshot exists yet.

## 5. Prime and take the golden snapshot

Claim a sandbox and seed both kinds of state:

```sh
kubectl apply -f 25-primer-claim.yaml
kubectl wait --for=condition=Ready sandboxclaim/podsnap-primer --timeout=300s
PRIMER_POD=$(kubectl get sandboxclaim podsnap-primer -o jsonpath='{.status.sandbox.name}')

probe() { kubectl exec "$1" -c probe -- python -c "import urllib.request as u;print(u.urlopen(u.Request(\"http://localhost:8888$2\",data=${3:-None},method=\"${4:-GET}\")).read().decode())"; }
probe "$PRIMER_POD" /bump 'b""' POST; probe "$PRIMER_POD" /bump 'b""' POST; probe "$PRIMER_POD" /bump 'b""' POST
probe "$PRIMER_POD" /write 'b"golden-v1"' POST
probe "$PRIMER_POD" /state
```

```text
{"boot_nonce": "8489ca83d8c34536ac7eb5c677db1349", "counter": 3, "marker": "golden-v1"}
```

Snapshot the primer's pod (pod name = Sandbox name):

```sh
export PRIMER_POD TRIGGER_SUFFIX=v1
envsubst < 30-snapshot-trigger.yaml | kubectl apply -f -
kubectl wait --for=condition=Triggered podsnapshotmanualtrigger/golden-v1 --timeout=600s
kubectl get podsnapshots.podsnapshot.gke.io   # wait for Ready
```

## 6. Roll the pool — members now boot restored

Pool members that existed **before** the snapshot keep their cold state;
replacements restore automatically:

```sh
kubectl delete sandbox -l agents.x-k8s.io/warm-pool-sandbox
kubectl wait sandboxwarmpool/podsnap-golden-pool --for=jsonpath='{.status.readyReplicas}'=2 --timeout=600s
for POD in $(kubectl get pods -l agents.x-k8s.io/warm-pool-sandbox -o jsonpath='{.items[*].metadata.name}'); do probe "$POD" /state; done
```

```text
{"boot_nonce": "8489ca83d8c34536ac7eb5c677db1349", "counter": 3, "marker": "golden-v1"}
{"boot_nonce": "8489ca83d8c34536ac7eb5c677db1349", "counter": 3, "marker": "golden-v1"}
```

The nonce **equals the primer's** — these processes were never cold-started;
their memory (and the rootfs marker) came from the snapshot. Claims now adopt
pre-initialized sandboxes:

```sh
kubectl apply -f 40-worker-claims.yaml
kubectl wait --for=condition=Ready sandboxclaim/podsnap-worker-1 sandboxclaim/podsnap-worker-2 --timeout=300s
```

## 7. Subsequent snapshots: latest wins — for new pods only

```sh
probe "$PRIMER_POD" /bump 'b""' POST                 # counter 3 -> 4
probe "$PRIMER_POD" /write 'b"golden-v2"' POST
export TRIGGER_SUFFIX=v2
envsubst < 30-snapshot-trigger.yaml | kubectl apply -f -
```

Once the v2 snapshot is `Ready`:

- **Existing** pool members still carry v1 state — a pool can hold **mixed
  generations** until its members are consumed or rolled.
- Every pod created **after** restores from v2 (`counter: 4`,
  `marker: golden-v2`). Roll the pool again to converge it.
- The policy's `maxSnapshotCountPerGroup: 3` prunes the oldest snapshots;
  pin a specific one for a pod with the `podsnapshot.gke.io/ps-name`
  annotation if you need to opt out of latest-wins.

## 8. Keep the pool on the latest snapshot

The pool only converges when its members are replaced, so pre-warmed
sandboxes can lag the newest snapshot (§7). To keep a small pool always
serving the latest state, roll it as part of every snapshot — **gated on the
snapshot's `Ready` condition**, not on the trigger completing (the trigger
finishes when the checkpoint is written; restore only considers `Ready`
snapshots, so rolling early re-warms from the previous one):

```sh
kubectl patch sandboxwarmpool podsnap-golden-pool --type merge -p '{"spec":{"replicas":1}}'
# ...take a snapshot as in §5, then:
kubectl wait --for=condition=Ready "podsnapshots.podsnapshot.gke.io/$SNAP" --timeout=600s
kubectl delete sandbox -l agents.x-k8s.io/warm-pool-sandbox
kubectl wait sandboxwarmpool/podsnap-golden-pool --for=jsonpath='{.status.readyReplicas}'=1 --timeout=600s
kubectl apply -f 50-refresh-claim.yaml     # adopts a member with the newest state
```

Two properties worth knowing:

- **The re-warm gap costs latency, not correctness.** A claim arriving while
  the pool refills cold-starts from the same template — and that pod also
  matches the snapshot, so it restores the latest state anyway; it just
  skips the pre-warm speedup.
- **This is best-effort freshness.** A claim can still adopt the old member
  in the instant between snapshot-Ready and the roll. A consumer that must
  have a specific snapshot should pin it with the
  `podsnapshot.gke.io/ps-name` annotation via the claim's
  `additionalPodMetadata` instead. Snapshot less often than the pool
  re-warms, or the pool spends its life rebooting.
- **Rolls don't scale linearly.** This example is validated at
  `replicas: 2`; a label-wide delete on a large pool is a synchronized
  restore storm — restores queue, per-pod resume latency grows with wave
  position, and node pod-density limits become the wall before the snapshot
  machinery does. Roll large pools in batches.

## Keep in mind

- **One user's session can become everyone's golden image.** The primer and
  workers necessarily share the group label, so the policy cannot tell them
  apart: a `PodSnapshotManualTrigger` aimed at a worker pod — typo or anyone
  with trigger RBAC in the namespace — publishes that user's live session as
  the state every future sandbox boots from. Keep `triggerConfig.type:
  manual`, treat trigger-creation RBAC as the tenancy boundary, snapshot only
  from a dedicated primer, and use a dedicated group label key (this example
  uses `golden-pool`, not a generic `app` label another workload might carry).
- **Restored members are memory clones of the primer.** The identical
  `boot_nonce` that proves the demo is also the risk: any secret, cached
  token, or PRNG state resident before the snapshot is identical in — and
  disclosed to — every future claimant's sandbox. Prime only deterministic,
  shareable state; generate per-session secrets and reseed RNGs after
  adoption, never before the snapshot.
- **Claim env injection is rejected — leave it that way.** This template
  keeps `envVarsInjectionPolicy` at its default (`Disallowed`), so a claim
  carrying `spec.env` fails with `EnvVarsInjectionRejected` instead of
  quietly cold-starting outside the golden state (verified on GKE). Don't
  enable injection on a golden template.
- **The SandboxTemplate is the cache key.** Snapshot matching hashes the
  pod spec, so editing the template invalidates existing snapshots — when
  the newest snapshot in the group no longer matches a new pod's spec, GKE
  silently cold-starts it (no error, no event); take a fresh snapshot from
  the updated template to re-arm the pool.
- **This is a GKE-level pattern, not an SDK one.** The Python SDK's
  `PodSnapshotSandboxClient` deliberately scopes restores to one sandbox;
  first-class warm-pool restore is tracked upstream in
  [#208](https://github.com/kubernetes-sigs/agent-sandbox/issues/208).

## Cleanup

```sh
kubectl delete -f 40-worker-claims.yaml -f 25-primer-claim.yaml \
    -f 20-sandboxwarmpool.yaml --ignore-not-found
envsubst < 10-sandboxtemplate.yaml | kubectl delete --ignore-not-found -f -
kubectl delete podsnapshotmanualtriggers --all
kubectl delete podsnapshots.podsnapshot.gke.io --all   # also deletes the GCS objects
kubectl delete -f 05-podsnapshotpolicy.yaml --ignore-not-found
envsubst < 00-storageconfig.yaml | kubectl delete --ignore-not-found -f -
```
