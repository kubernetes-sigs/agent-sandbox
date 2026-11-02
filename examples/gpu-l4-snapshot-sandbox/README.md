# Snapshot and Restore an L4 GPU Sandbox on GKE

This example runs a `Sandbox` on an NVIDIA L4 GPU with GKE Sandbox (gVisor),
takes a GKE Pod Snapshot of it, and then suspends and resumes it. The resumed
pod restores from the snapshot. Its process memory **and its GPU memory**
come back as they were, and the L4 is free while the Sandbox is suspended.

For whole-pod snapshots of GPU pods, GKE uses the NVIDIA `cuda-checkpoint`
tool. The tool copies the GPU state into process memory, and gVisor then
checkpoints that memory with the rest of the pod. On restore, the tool moves
the state back onto the GPU. See
[About Pod snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/pod-snapshots).

```text
 Sandbox (Running)          PodSnapshotManualTrigger
 L4 GPU, CUDA state  ---------------------------->  PodSnapshot (GCS)
        |                                                 |
        | operatingMode: Suspended                        |
        v                                                 |
 Sandbox (Suspended)                                      |
 no pod, L4 released                                      |
        |                                                 |
        | operatingMode: Running                          |
        v                                                 v
 Sandbox (Running)  <---- GKE restores the new pod from the latest
 same CUDA state          Ready snapshot in the Sandbox's own group
```

## Files

| File | Purpose |
|---|---|
| `image/` | GPU state probe. It keeps a boot nonce in host memory and a buffer and a counter in GPU memory. |
| `00-storageconfig.yaml` | `PodSnapshotStorageConfig` that points to a GCS bucket |
| `10-podsnapshotpolicy.yaml` | Per-sandbox policy with manual triggers |
| `20-sandbox.yaml` | gVisor `Sandbox` that requests one L4, and the pod `ServiceAccount` |
| `30-snapshot-trigger.yaml` | `PodSnapshotManualTrigger` for the sandbox pod |
| `run-test-gke.sh` | Runs the walkthrough below as one script that asserts each result |

## 1. Prerequisites

- A GKE Standard cluster at version 1.35.3 or later with Pod Snapshots
  enabled. See
  [Prepare for Pod snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots-prepare).
- A gVisor node pool with L4 GPUs on a G2 machine type. Pod Snapshots
  supports L4 on `g2-standard-4` through `g2-standard-96`. GKE Sandbox
  supports only the `latest` and `default` driver versions:

  ```sh
  export CLUSTER=<your-cluster> LOCATION=<zone-with-l4>
  gcloud container node-pools create l4-gvisor \
      --cluster="$CLUSTER" --location="$LOCATION" \
      --node-version=1.35.8-gke.1036000 \
      --machine-type=g2-standard-8 \
      --accelerator=type=nvidia-l4,count=1,gpu-driver-version=default \
      --image-type=cos_containerd --sandbox=type=gvisor \
      --num-nodes=1
  ```

  > **Check the GPU before you continue.** This example was validated on
  > node version `1.35.8-gke.1036000` with driver `580.126.09`. On
  > `1.36.4-gke.2046003`, both `latest` and `default` install driver
  > `580.173.02`, and each gVisor GPU pod fails with
  > `FailedCreatePodSandBox: ... cannot read client sync file: waiting for
  > sandbox to start: EOF`. Before you deploy, run `nvidia-smi` in a gVisor
  > pod that requests `nvidia.com/gpu: 1`. If the pod does not start, use
  > a different node version.

- The [agent-sandbox controller](../../README.md#installation) (v1.0.0 or
  later). This example uses only the core `Sandbox`, so the extensions are
  not necessary.
- `envsubst`, `kubectl`, `gcloud`, `python3`, and Docker.

## 2. Snapshot storage bucket and IAM

GKE stores snapshots in Cloud Storage. The pod's Kubernetes ServiceAccount
reads and writes them through Workload Identity:

```sh
export PROJECT_ID=<your-project> SNAPSHOT_BUCKET=<your-bucket> REGION=<cluster-region>
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')

gcloud storage buckets create "gs://$SNAPSHOT_BUCKET" --location="$REGION" \
    --uniform-bucket-level-access --enable-hierarchical-namespace --soft-delete-duration=0

MEMBER="principal://iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$PROJECT_ID.svc.id.goog/subject/ns/default/sa/gpu-l4-snapshot-sa"
gcloud storage buckets add-iam-policy-binding "gs://$SNAPSHOT_BUCKET" --member="$MEMBER" --role=roles/storage.bucketViewer
gcloud storage buckets add-iam-policy-binding "gs://$SNAPSHOT_BUCKET" --member="$MEMBER" --role=roles/storage.objectUser

# The GKE service agent deletes snapshot objects. Without this grant, deleted
# PodSnapshots stay in Terminating on their finalizer.
gcloud storage buckets add-iam-policy-binding "gs://$SNAPSHOT_BUCKET" \
    --member="serviceAccount:service-$PROJECT_NUMBER@container-engine-robot.iam.gserviceaccount.com" \
    --role=roles/storage.objectUser
```

Keep the bucket in the cluster's region. A GPU snapshot includes the GPU
memory that the process uses, so expect larger objects than for CPU-only
pods.

## 3. Build the GPU state probe image

The probe keeps state in the places that a snapshot must capture:

- A boot nonce in **process memory**.
- A float32 buffer (`BUFFER_MIB`, 256 MiB by default) seeded from the nonce,
  and a counter, both in **GPU memory**. `POST /bump` adds 1 to the counter
  and to each buffer element on the GPU. After a bump, the current values
  exist only on the device.

A restored pod reports the same nonce, counter, and checksum. A cold-started
pod reports a new nonce, a zero counter, and a different checksum.

```sh
export DEMO_IMAGE=$REGION-docker.pkg.dev/$PROJECT_ID/<repo>/gpu-snapshot-probe:v1
docker buildx build --platform linux/amd64 --push -t "$DEMO_IMAGE" image/
```

## 4. Deploy the snapshot config and the Sandbox

The manifests create their objects in the `default` namespace, and the
commands below use unqualified names. If your current namespace is
different, run `kubectl config set-context --current --namespace=default`
first.

```sh
envsubst < 00-storageconfig.yaml | kubectl apply -f -
kubectl apply -f 10-podsnapshotpolicy.yaml
envsubst < 20-sandbox.yaml | kubectl apply -f -
kubectl wait sandbox/gpu-l4-sandbox --for=condition=Ready --timeout=900s
```

## 5. Put state on the GPU

```sh
call() { kubectl exec gpu-l4-sandbox -c probe -- python -c \
    "import urllib.request as u;print(u.urlopen(u.Request('http://localhost:8888$2',data=b'' if '$1'=='POST' else None,method='$1')).read().decode())"; }
call POST /bump; call POST /bump; call POST /bump
call GET /state
```

```text
{"boot_nonce": "<nonce>", "counter": 3, "checksum": "<sum>", "gpu": "NVIDIA L4", "buffer_mib": 256}
```

## 6. Take the snapshot

The pod name is the same as the Sandbox name. Each trigger needs a new name:

```sh
export TRIGGER_SUFFIX=v1
envsubst < 30-snapshot-trigger.yaml | kubectl apply -f -
kubectl wait --for=condition=Triggered podsnapshotmanualtrigger/gpu-l4-sandbox-v1 --timeout=900s
SNAP=$(kubectl get podsnapshotmanualtrigger gpu-l4-sandbox-v1 -o jsonpath='{.status.snapshotCreated.name}')
kubectl wait --for=condition=Ready "podsnapshots.podsnapshot.gke.io/$SNAP" --timeout=900s
```

Wait for the snapshot's `Ready` condition, not only for the trigger. The
trigger completes when the checkpoint is written, but a restore uses only
`Ready` snapshots. The pod keeps running during the upload, because the
policy sets `postCheckpoint: resume`.

## 7. Suspend: release the GPU

```sh
kubectl patch sandbox gpu-l4-sandbox --type merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl wait --for=delete pod/gpu-l4-sandbox --timeout=300s
```

The controller deletes the pod, and the L4 is free for other workloads. The
`Sandbox` object and the snapshot in GCS stay.

## 8. Resume: restore the CUDA state

```sh
kubectl patch sandbox gpu-l4-sandbox --type merge -p '{"spec":{"operatingMode":"Running"}}'
kubectl wait sandbox/gpu-l4-sandbox --for=condition=Ready --timeout=900s
call GET /state
```

The output must be identical to step 5: the same `boot_nonce`, `counter: 3`,
and the same `checksum`. The process did not start again, and the buffer
came back from the snapshot onto the GPU. A further `call POST /bump`
returns `counter: 4`, which shows that the restored CUDA context works.

GKE reports **no error and no event** when a restore does not match a
snapshot. It cold-starts the pod silently. Thus, the in-band nonce check is
the only reliable proof of a restore.

## Use the Python SDK instead

The [Python SDK snapshot extension](../../clients/python/agentic-sandbox-client/k8s_agent_sandbox/gke_extensions/snapshots/README.md)
(`pip install k8s-agent-sandbox`) runs steps 6 to 8 for you. It creates the
`PodSnapshotManualTrigger`, waits for the snapshot to become `Ready`, and
sets `operatingMode`. The SDK works with a `SandboxClaim`, so it needs the
agent-sandbox extensions. It also needs a `SandboxTemplate` and a
`SandboxWarmPool` that carry the pod template from `20-sandbox.yaml`.
Keep the `gpu-snapshot-demo: l4` label so that the same
`PodSnapshotPolicy` applies. Then call
`sandbox = PodSnapshotSandboxClient().create_sandbox(warmpool=...)`,
`sandbox.suspend()`, and `sandbox.resume()`. By default, `suspend()` takes
a snapshot first. `resume()` restores from the latest `Ready` snapshot and
reports its UID in the response. The measured results below come from the
`kubectl` flow, not from the SDK.

## Measured results

One run on GKE Standard, control plane `1.36.4-gke.2046003`, node pool
`1.35.8-gke.1036000` on `g2-standard-8` (driver `580.126.09`), and
agent-sandbox controller v1.0.8, with the default 256 MiB GPU buffer:

| Step | Result |
|---|---|
| Snapshot, from trigger to `Ready` | 13 s |
| Resume, from `operatingMode: Running` to `Ready` | 5 s |
| Snapshot size in GCS | 893 MiB (`pages.img` 892 MiB) |
| State after restore | Same `boot_nonce`, `counter`, and `checksum`. A further bump returns `counter: 4`. |

The resume time does not include a node scale-up. When the node pool
scales from zero, add the time to get an L4 node.

## Keep in mind

- **The pod spec is the cache key.** GKE matches a restore to a snapshot by
  a hash of the pod spec. An edit to `20-sandbox.yaml` between the snapshot
  and the resume causes a silent cold start.
- **Restore on the same hardware.** Keep the restore on the same machine
  series and GPU type as the snapshot. Pin the node pool through
  `nodeSelector`, as `20-sandbox.yaml` does with
  `cloud.google.com/gke-accelerator: nvidia-l4`.
- **Memory peaks during snapshot and restore.** `cuda-checkpoint` copies the
  GPU memory into process memory. Set the memory limit to allow for the GPU
  memory that the process uses. A large model needs a much larger limit than
  this probe.
- **Not supported:** Multi-Instance GPU (MIG), GPU time-sharing, and TPUs.
  GKE Sandbox runs only CUDA workloads on GPUs.
- **The restored pod gets a new IP.** Clients that cache `status.podIPs`
  must read them again.
- **Trigger RBAC is the access boundary.** A user who can create
  `PodSnapshotManualTrigger` objects in the namespace can checkpoint the
  memory of each sandbox in it, including secrets in memory.
- To start a warm pool from one primed snapshot, see
  [podsnapshot-golden-warmpool](../podsnapshot-golden-warmpool).

## Cleanup

```sh
envsubst < 20-sandbox.yaml | kubectl delete --ignore-not-found -f -
kubectl delete podsnapshotmanualtrigger gpu-l4-sandbox-v1 --ignore-not-found
kubectl delete "podsnapshots.podsnapshot.gke.io/$SNAP"   # also deletes the GCS objects
kubectl delete -f 10-podsnapshotpolicy.yaml --ignore-not-found
envsubst < 00-storageconfig.yaml | kubectl delete --ignore-not-found -f -
gcloud container node-pools delete l4-gvisor --cluster="$CLUSTER" --location="$LOCATION"
```
