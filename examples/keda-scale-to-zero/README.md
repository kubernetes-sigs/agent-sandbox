# SandboxWarmPool Scale-to-Zero with KEDA on GKE

This example uses [KEDA](https://keda.sh/) to scale a `SandboxWarmPool` **down to zero** (and back
up) based on the rate of sandbox claims, using the KEDA **Prometheus scaler** against GKE Managed
Service for Prometheus (GMP).

> **Note:**
> The walkthrough targets Google Kubernetes Engine (GKE) — it uses GKE Managed Service for
> Prometheus (GMP) and Workload Identity Federation for GKE, and queries GMP's **hosted Prometheus
> endpoint** directly (no in-cluster proxy). The Prometheus scaler is portable: off-GKE, point its
> `serverAddress` at your own Prometheus-compatible query endpoint.

## Why KEDA instead of HPA?

A native Horizontal Pod Autoscaler **cannot scale to zero**:

- The native Kubernetes HPA enforces `minReplicas >= 1`.
- The only escape, the `HPAScaleToZero` feature gate, is **alpha** and unavailable on managed GKE.

A KEDA `ScaledObject` supports `minReplicaCount: 0`:

- KEDA performs the **0 → 1 "activation"** itself, scaling the warm pool directly through its
  `/scale` subresource when the metric crosses `activationThreshold`.
- It delegates the **1 → N** range to a `HorizontalPodAutoscaler` it creates and manages.
- When activity stops, KEDA scales the pool back to **0**, so an idle pool costs nothing.

The `SandboxWarmPool` CRD is already compatible: it exposes a `/scale` subresource and allows
`spec.replicas: 0`. KEDA also ships its own external metrics server, so this needs **no** Custom
Metrics Stackdriver Adapter.

## Overview

We scale a pool of warm sandboxes on the **rate of sandbox claims** being created. With no claims the
pool drains to zero; as soon as claims arrive KEDA activates the pool and scales it up to keep a
ready supply ahead of demand.

## Prerequisites

- A Google Kubernetes Engine (GKE) cluster.
- [Workload Identity Federation for GKE](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity)
  enabled (lets KEDA read GMP).
- GKE Managed Service for Prometheus enabled.
- The agent sandbox controller installed (in the `agent-sandbox-system` namespace).

## Steps to Run

1. **Install KEDA** into its own namespace. Follow the [GKE KEDA scale-to-zero tutorial](https://docs.cloud.google.com/kubernetes-engine/docs/tutorials/scale-to-zero-using-keda#kubectl) to install it using Helm or `kubectl`.

   Once installed, verify that KEDA is running:
   ```bash
   kubectl get pods -n keda   # wait for keda-operator + metrics-apiserver to be Running
   ```

2. **Set up the Agent Sandbox resources.** Define names, namespace, and project, then apply the
   templated manifests with `envsubst`:

   ```bash
   export NAMESPACE="keda-test"
   export TEMPLATE_NAME="python-sandbox-template"
   export WARM_POOL_NAME="python-sdk-warmpool"
   export PROJECT_ID="$(gcloud config get-value project)"

   kubectl create namespace $NAMESPACE
   envsubst < python-sandbox-template.yaml | kubectl apply -f -
   envsubst < sandboxwarmpool.yaml | kubectl apply -f -
   ```

   The warm pool starts at `replicas: 1` (the default when omitted) before KEDA's ScaledObject is applied. Once KEDA is applied, it takes ownership of this field. We omit `replicas` in the manifest to avoid stomping the autoscaled count back to 0 on subsequent updates.

3. **Expose the controller metric via GMP.** Apply `pod-monitoring.yaml` to scrape the controller's
   `/metrics` endpoint. This ingests `agent_sandbox_claim_creation_total{warmpool_name="..."}` into
   Cloud Monitoring.

   ```bash
   kubectl apply -f pod-monitoring.yaml
   ```

4. **Authorize KEDA to query GMP (Workload Identity).** The scaler authenticates to the hosted GMP
   endpoint as the `keda-operator` service account. Grant it `roles/monitoring.viewer`:

   ```bash
   gcloud projects add-iam-policy-binding $PROJECT_ID \
     --role=roles/monitoring.viewer \
     --member="principal://iam.googleapis.com/projects/$(gcloud projects describe $PROJECT_ID --format='value(projectNumber)')/locations/global/workloadIdentityPools/$PROJECT_ID.svc.id.goog/subject/ns/keda/sa/keda-operator"
   ```

   The `TriggerAuthentication` (`podIdentity: gcp`) that wires this identity in is bundled in
   `scaledobject-prometheus.yaml`, so there's nothing else to apply here.

5. **Apply the ScaledObject:**

   ```bash
   envsubst < scaledobject-prometheus.yaml | kubectl apply -f -
   ```

   It points the Prometheus scaler at GMP's hosted endpoint
   (`https://monitoring.googleapis.com/v1/projects/$PROJECT_ID/location/global/prometheus`). Key
   fields:
   - **`minReplicaCount: 0`** — true scale-to-zero.
   - **`maxReplicaCount`** — hard budget ceiling.
   - **`threshold`** — the 1 → N target, as an `AverageValue` (KEDA's default `metricType`), so
     `desired = claim_rate / threshold`. Read `1/threshold` as "seconds of demand buffered as ready
     sandboxes"; set it ≥ your per-sandbox replenishment time.
   - **`activationThreshold`** — the 0 ↔ 1 gate, compared to the **raw** total rate.
   - **query** — `sum(rate(...) >= 0) or vector(0)`; the `>= 0` per-series filter drops stale
     `NaN` sub-series.

6. **Generate load** to trigger scaling:

   ```bash
   python3 create-claim.py   # reads NAMESPACE / WARM_POOL_NAME from the env you exported
   ```

7. **Verify scale-to-zero** in both directions:

   ```bash
   kubectl get scaledobject -n $NAMESPACE
   kubectl get hpa -n $NAMESPACE -w   # the KEDA-managed HPA persists even at zero replicas
   kubectl get swp -n $NAMESPACE -w   # pool: 0 -> N under load, back to 0 after load + cooldownPeriod
   ```

## How scale-to-zero works here

- **Activation vs. target**: `activationThreshold` decides *whether* the pool runs (0 ↔ 1); it's
  compared to the **raw** query result. `threshold` decides *how many* replicas once active (1 → N);
  it's an averaged target. KEDA handles activation directly and creates the HPA only for 1 → N.
- **Why `AverageValue` (KEDA default), not `Value`**: with `AverageValue` the HPA divides the metric
  by replicas, so the math collapses to `desired = total_rate / threshold` — self-regulating,
  independent of current replicas. `Value` computes `current × metric/target`, which compounds every
  cycle and runs away to `maxReplicaCount`. The claim metric is a cluster-wide total with 1:1
  claim→sandbox, so this is buffer sizing and `AverageValue` gives the right `pool ∝ rate`.
- **Choosing `threshold`**: `pool = claim_rate / threshold`, so `1/threshold` = seconds of demand
  held as ready sandboxes. Set `threshold ≈ 1/T`, where `T` is the per-sandbox replenishment time
  (Sandbox → Pod `Ready`, from `agent_sandbox_creation_latency_ms`). Lower `threshold` → bigger pool.
  Because `pool = rate × T`, cutting `T` (pre-pull the image) helps more than shrinking `threshold`.
- **Activation is not instant; full size takes time too**: The first-ever activation takes ~1 scrape
  interval for the counter series to be born. KEDA wakes `0 → 1` once the metric is scraped and
  queryable, but the HPA scales `1 → N` only once `rate(...[1m])` fills its window — so the first burst
  after idle ramps over ~1–2 min and may cold-start meanwhile.
- **Idle is free**: with no claims the rate falls to 0 and KEDA scales the pool back to 0 after
  `cooldownPeriod`.

## Sources

- KEDA Prometheus scaler (see the "Google Managed Prometheus" section for the hosted-endpoint +
  `podIdentity: gcp` setup): <https://keda.sh/docs/2.20/scalers/prometheus/>
- GMP hosted query endpoint (`monitoring.googleapis.com/v1/projects/PROJECT_ID/location/global/prometheus`):
  <https://cloud.google.com/stackdriver/docs/managed-prometheus/query-api-ui>
- KEDA `metricType` default (`AverageValue`):
  <https://keda.sh/docs/2.20/reference/scaledobject-spec/>
