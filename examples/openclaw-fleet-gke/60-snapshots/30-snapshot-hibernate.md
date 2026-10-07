# Tier 2: snapshot hibernate / wake (memory + rootfs intact)

Verifies the P0 requirement at the deep tier: **GKE Pod Snapshots (GA)
checkpoint the pod's live memory and container rootfs to GCS; teardown
releases everything; wake restores the exact process state** — the
employee's OpenClaw session resumes mid-thought, not from a cold boot.

Prerequisites (all covered by the files in this directory plus the fleet
setup):

- GKE **Standard** cluster (≥ 1.35.3) with Pod Snapshots enabled and a
  **gVisor** node pool on a **non-E2** machine series — the fleet already
  runs `runtimeClassName: gvisor`.
- GCS bucket with **hierarchical namespace** enabled, **soft delete off**,
  in the **cluster's region**, with the Workload Identity IAM grants from
  [00-storageconfig.yaml](00-storageconfig.yaml).
- Applied config:

  ```sh
  export SNAPSHOT_BUCKET=<your-bucket>
  envsubst < 00-storageconfig.yaml | kubectl apply -f -
  kubectl apply -f 10-policy.yaml
  ```

> **Controller version caveat:** this runbook follows the GKE Pod Snapshots
> guide for Agent Sandbox, which pins the agent-sandbox controller at
> **v0.4.6**; the in-repo golden-snapshot example
> (`examples/podsnapshot-golden-warmpool/`) validates the same CRDs against
> v1.0.0+. If snapshots trigger but restores cold-start, check your
> controller version against the guide before debugging anything else.

## 1. Snapshot one employee's sandbox

Pod name = Sandbox name. Each trigger needs a **fresh name** — bump the
suffix per hibernation:

```sh
EMP=alice
SB=$(kubectl -n openclaw-fleet get sandboxclaim "oc-$EMP" -o jsonpath='{.status.sandbox.name}')

SNAP_START=$(date +%s)
kubectl -n openclaw-fleet apply -f - <<EOF
apiVersion: podsnapshot.gke.io/v1
kind: PodSnapshotManualTrigger
metadata:
  name: oc-$EMP-hib-1
  namespace: openclaw-fleet
spec:
  targetPod: $SB
EOF
kubectl -n openclaw-fleet wait --for=condition=Triggered \
    "podsnapshotmanualtrigger/oc-$EMP-hib-1" --timeout=600s
```

Then wait for the snapshot itself to be **Ready** — the trigger completes
when the checkpoint is written, but restore only considers `Ready`
snapshots, so tearing down early would wake from a previous snapshot (or
cold-start):

```sh
SNAP=$(kubectl -n openclaw-fleet get podsnapshotmanualtrigger "oc-$EMP-hib-1" \
    -o jsonpath='{.status.snapshotCreated.name}')
kubectl -n openclaw-fleet wait --for=condition=Ready \
    "podsnapshots.podsnapshot.gke.io/$SNAP" --timeout=600s
echo "snapshot duration: $(( $(date +%s) - SNAP_START ))s"
```

(`postCheckpoint: resume` in the policy means the pod keeps running while
the checkpoint uploads — the session is not interrupted yet.)

## 2. Hibernate: release the resources

Same dial as Tier 1 — **gate this on the snapshot being Ready** (step 1):

```sh
kubectl -n openclaw-fleet patch sandbox "$SB" --type merge \
    -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n openclaw-fleet wait --for=delete "pod/$SB" --timeout=120s
```

The pod (and its CPU/memory) is gone; the Sandbox object, Service, Filestore
workspace, and the snapshot in GCS remain.

## 3. Wake: restore with memory intact

Flip back to `Running`. The recreated pod matches
[10-policy.yaml](10-policy.yaml) and restores from the latest Ready snapshot
in **its own** per-sandbox group (`agents.x-k8s.io/sandbox-name-hash`) —
no restore-side object needed:

```sh
WAKE_START=$(date +%s)
kubectl -n openclaw-fleet patch sandbox "$SB" --type merge \
    -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n openclaw-fleet wait sandbox "$SB" --for=condition=Ready --timeout=300s
echo "wake-up duration: $(( $(date +%s) - WAKE_START ))s"
```

Verify it was a restore, not a cold start: any in-memory state the OpenClaw
runtime held before step 1 (open session, conversation buffer, loaded
context) is still there. A cold start would present a fresh process. Note
GKE reports **no error or event** on a failed match — it silently
cold-starts — so an in-band memory probe is the only reliable check.

## Constraints

- **gVisor only**; the fleet template already sets
  `runtimeClassName: gvisor`.
- **Same machine series on restore, and never E2.** Keep the fleet node
  pool homogeneous.
- **The pod spec is the cache key.** Any edit to `openclaw-fleet-template`
  between snapshot and wake changes the pod-spec hash and the restore
  silently cold-starts. Disk state on the Filestore workspace still
  survives (Tier 1 semantics are the floor).
- **The restored pod gets a new IP.** The per-sandbox Service re-resolves;
  consumers of `status.podIPs` must re-read.
- **Latest Ready snapshot wins** within the sandbox's group; the policy
  retains the last 3. To wake from a specific older snapshot, pin it with
  the `podsnapshot.gke.io/ps-name` pod annotation instead.
- **Trigger RBAC is the tenancy boundary**: anyone who can create
  `PodSnapshotManualTrigger` in `openclaw-fleet` can checkpoint any
  employee's live session (secrets and tokens resident in memory included).

## Cleanup

Deleting a PodSnapshot also deletes its GCS objects (this exercises the
service agent's bucket IAM — snapshots stuck in `Terminating` mean that
grant is missing):

```sh
kubectl -n openclaw-fleet delete podsnapshotmanualtrigger "oc-$EMP-hib-1" --ignore-not-found
kubectl -n openclaw-fleet delete "podsnapshots.podsnapshot.gke.io/$SNAP"
```
