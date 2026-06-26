# SandboxWarmPool Scale-to-Zero with KEDA on GKE

This example demonstrates how to use [KEDA](https://keda.sh/) to scale a `SandboxWarmPool`
**down to zero** (and back up) based on custom metrics emitted by the agent sandbox controller.

> **Note:**
> The walkthrough below targets Google Kubernetes Engine (GKE) — it uses GKE Managed Service for
> Prometheus (GMP) and Workload Identity Federation for GKE. The recommended Prometheus variant is
> itself portable: on another platform, point the scaler's `serverAddress` at your own
> Prometheus-compatible query endpoint instead of the GMP frontend.

## Why KEDA instead of HPA?

The [`hpa-swp-scaling`](../hpa-swp-scaling) example scales the same warm pool using a native
Horizontal Pod Autoscaler. That example **cannot scale to zero**:

- The native Kubernetes HPA enforces `minReplicas >= 1`.
- The only way around it, the `HPAScaleToZero` feature gate, is **alpha** and not available on
  managed GKE clusters.

KEDA is purpose-built for this. A KEDA `ScaledObject` supports `minReplicaCount: 0`:

- KEDA performs the **0 → 1 "activation"** itself, scaling the warm pool directly through its
  `/scale` subresource when the metric crosses an activation threshold.
- It then delegates the **1 → N** range to a `HorizontalPodAutoscaler` it creates and manages
  under the hood.
- When activity stops, KEDA scales the pool back to **0**, so an idle pool costs nothing.

KEDA also brings its own external metrics server, so this example does **not** need the Stackdriver
Custom Metrics Adapter that the HPA example relies on.

The `SandboxWarmPool` CRD is already compatible: it exposes a `/scale` subresource and allows
`spec.replicas: 0`.

## Overview

We scale a pool of warm sandboxes based on the **rate of sandbox claims** being created. When no
claims are arriving, the pool drains to zero. As soon as claims start, KEDA activates the pool and
scales it up to keep a ready supply of sandboxes ahead of demand.

## Choosing a metric source

This example ships **two** `ScaledObject` variants that scale on the _same_ claim-creation metric.
Pick one:

|                             | `scaledobject-prometheus.yaml` (recommended)                      | `scaledobject-stackdriver.yaml`                                            |
| --------------------------- | ----------------------------------------------------------------- | -------------------------------------------------------------------------- |
| **How it reads the metric** | KEDA `prometheus` scaler → in-cluster GMP query frontend (PromQL) | KEDA `gcp-stackdriver` scaler → Cloud Monitoring directly                  |
| **Extra components**        | Must run the GMP query frontend (step 4a)                         | None                                                                       |
| **KEDA → GCP auth**         | Not needed (KEDA hits an in-cluster HTTP service)                 | Workload Identity + `roles/monitoring.viewer` on `keda-operator`           |
| **Query style**             | PromQL `rate()` / `sum()` — same as the HPA example               | Cloud Monitoring filter + `ALIGN_RATE` aligner                             |
| **Latency**                 | Lower                                                             | Higher (Cloud Monitoring ingestion delay)                                  |
| **Other GCP metrics**       | Prometheus metrics only                                           | Any Cloud Monitoring metric (Pub/Sub, LBs, …)                              |
| **KEDA support status**     | Actively maintained                                               | **Deprecated/frozen** in KEDA                                              |

**Recommendation:** use the **Prometheus** variant — lower latency, actively maintained, and it
reuses the exact metric query from the HPA example. Reach for the **Stackdriver** variant only if
you'd rather not run the GMP frontend, or you also want to scale on native GCP metrics — accepting
that the `gcp-stackdriver` scaler is deprecated.

The **Steps to Run** below use the recommended Prometheus variant. If you prefer Stackdriver, see
[Alternative: GCP Stackdriver scaler](#alternative-gcp-stackdriver-scaler-gcp-only-deprecated).

> **Note:** Neither variant uses the _Custom Metrics Stackdriver Adapter_ that the HPA example
> depends on. KEDA ships its own external metrics server, so the adapter is not needed here.

## Prerequisites

- A Google Kubernetes Engine (GKE) cluster.
- [Workload Identity Federation for GKE](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity)
  enabled (used by the Prometheus query frontend to read Cloud Monitoring).
- GKE Managed Service for Prometheus enabled.
- The agent sandbox controller installed (in the `agent-sandbox-system` namespace).

## Steps to Run

1. **Install KEDA** into its own namespace using Helm:

   ```bash
   helm repo add kedacore https://kedacore.github.io/charts
   helm repo update
   helm install keda kedacore/keda --create-namespace --namespace keda
   ```

   Verify the operator is running:

   ```bash
   kubectl get pods -n keda
   ```

2. **Set up the Agent Sandbox resources**:

   ```bash
   kubectl create namespace keda-test
   kubectl apply -f python-sandbox-template.yaml
   kubectl apply -f sandboxwarmpool.yaml
   ```

   The warm pool starts at `replicas: 0` — KEDA takes ownership of this field.

3. **Expose the controller metric via GKE Managed Service for Prometheus**:
   Apply the `pod-monitoring.yaml` to scrape the controller's `/metrics` endpoint. This exposes
   `agent_sandbox_claim_creation_total{sandbox_template="..."}` into GMP.

   ```bash
   kubectl apply -f pod-monitoring.yaml
   ```

4. **Deploy the GMP query frontend** so KEDA can run PromQL queries against GMP. KEDA's Prometheus
   scaler talks to a standard Prometheus query API; on GMP that API is provided by the `frontend`
   proxy (image `gke.gcr.io/prometheus-engine/frontend`, listens on `:9090`, authenticating to
   Cloud Monitoring via Workload Identity). Deploy the official manifest into the `keda-test`
   namespace, substituting your project ID:

   ```bash
   curl -s https://raw.githubusercontent.com/GoogleCloudPlatform/prometheus-engine/v0.15.3/examples/frontend.yaml \
     | sed "s/\$PROJECT_ID/$(gcloud config get-value project)/g" \
     | kubectl apply -n keda-test -f -
   ```

   This creates a `frontend` Deployment and a headless `frontend` Service. The `ScaledObject`
   reaches it at `http://frontend.keda-test.svc:9090`. Confirm the pods are running:

   ```bash
   kubectl get pods -n keda-test -l app=frontend
   ```

   > [!NOTE]
   > **Image pull issues:** If the frontend pods fail with `ImagePullBackOff` for
   > `gke.gcr.io/prometheus-engine/frontend`, patch the deployment to pull from GKE's regional
   > Artifact Registry (matching your cluster's region, e.g. `us-central1`):
   >
   > ```bash
   > kubectl set image deployment/frontend frontend=us-central1-artifactregistry.gcr.io/gke-release/gke-release/prometheus-engine/frontend:v0.18.0-gke.2 -n keda-test
   > ```

   **Authorize the frontend proxy via GKE Workload Identity.** The proxy runs as the `default`
   Kubernetes Service Account (KSA) in `keda-test`; authorize it to read Cloud Monitoring:

   ```bash
   PROJECT_ID=$(gcloud config get-value project)
   GSA_NAME="gmp-frontend-sa"
   KSA_NAME="default"
   NAMESPACE="keda-test"

   # 1. Create a Google Service Account (GSA)
   gcloud iam service-accounts create $GSA_NAME \
     --description="GSA for Prometheus query frontend in GKE" \
     --display-name="GMP Query Frontend SA" \
     --project=$PROJECT_ID

   # 2. Grant the GSA the Monitoring Viewer role
   gcloud projects add-iam-policy-binding $PROJECT_ID \
     --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
     --role="roles/monitoring.viewer"

   # 3. Allow the KSA to impersonate the GSA
   gcloud iam service-accounts add-iam-policy-binding $GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com \
     --role="roles/iam.workloadIdentityUser" \
     --member="serviceAccount:$PROJECT_ID.svc.id.goog[$NAMESPACE/$KSA_NAME]" \
     --project=$PROJECT_ID

   # 4. Annotate the KSA in your cluster
   kubectl annotate serviceaccount -n $NAMESPACE $KSA_NAME \
     iam.gke.io/gcp-service-account=$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com \
     --overwrite

   # 5. Restart the deployment to reload the projected credentials
   kubectl rollout restart deployment/frontend -n $NAMESPACE
   ```

5. **Apply the ScaledObject**:

   ```bash
   kubectl apply -f scaledobject-prometheus.yaml
   ```

   The `ScaledObject` connects the claim-creation metric to the warm pool. The key guardrails
   (the Prometheus scaler uses `threshold` and `activationThreshold`):
   - **`minReplicaCount: 0`** — true scale-to-zero.
   - **`maxReplicaCount: 100`** — hard budget ceiling.
   - **`threshold: "0.5"`** — 0.5 claims/sec per replica, driving the 1 → N math (same target as the
     HPA example).
   - **`activationThreshold: "0.01"`** — the 0 ↔ 1 gate: any real claim activity lifts the pool off
     zero; it returns to zero only after the rate stays at/under this value for `cooldownPeriod`.

6. **Generate load** to trigger scaling:

   ```bash
   python3 create-claim.py
   ```

7. **Verify scale-to-zero** in both directions:

   ```bash
   # The ScaledObject and (once active) the KEDA-managed HPA:
   kubectl get scaledobject -n keda-test
   kubectl get hpa -n keda-test -w        # the HPA only exists while replicas >= 1

   # The warm pool itself:
   kubectl get swp -n keda-test -w
   ```

   You should see the pool go `0 -> N` shortly after the load starts, and return to `0` after the
   load stops and the `cooldownPeriod` (60s) elapses.

## Alternative: GCP Stackdriver scaler (GCP-only, deprecated)

Instead of running the GMP query frontend, you can read the same metric directly from Cloud
Monitoring with KEDA's `gcp-stackdriver` scaler. This removes the frontend proxy, but it is
**GCP-only** and the scaler is **deprecated/frozen in KEDA** — prefer the Prometheus path above for
new and long-lived setups.

> [!WARNING]
> The KEDA [gcp-stackdriver scaler](https://keda.sh/docs/2.20/scalers/gcp-stackdriver/) will not
> receive new modifications. For long-lived setups, query GMP via the frontend proxy and the
> standard Prometheus scaler (steps 4–5 above).

To use it, **replace steps 4–5** with the following:

1. **Grant KEDA read access to Cloud Monitoring.** The `gcp-stackdriver` scaler authenticates as the
   `keda-operator` service account via Workload Identity. Bind it `roles/monitoring.viewer`
   (replace `$PROJECT_ID`):

   ```bash
   gcloud projects add-iam-policy-binding $PROJECT_ID \
     --role=roles/monitoring.viewer \
     --member="principal://iam.googleapis.com/projects/$(gcloud projects describe $PROJECT_ID --format='value(projectNumber)')/locations/global/workloadIdentityPools/$PROJECT_ID.svc.id.goog/subject/ns/keda/sa/keda-operator"
   ```

2. **Set your project ID** in `scaledobject-stackdriver.yaml` (the `projectId` field). The bundled
   `TriggerAuthentication` (`podIdentity.provider: gcp`) wires this identity into the scaler.

3. **Apply it** (instead of `scaledobject-prometheus.yaml`):

   ```bash
   kubectl apply -f scaledobject-stackdriver.yaml
   ```

   This variant uses `targetValue`/`activationTargetValue` instead of
   `threshold`/`activationThreshold`; the values and behavior match the Prometheus variant.

Then continue with **Generate load** and **Verify** (steps 6–7 above).

## How scale-to-zero works here

- **Activation value vs. target value**: the _activation_ value (`activationThreshold` for the
  Prometheus scaler, `activationTargetValue` for Stackdriver) decides _whether_ the pool runs at all
  (the 0 ↔ 1 transition). The _target_ value (`threshold` / `targetValue`) decides _how many_
  replicas to run once it's active (the 1 → N math). KEDA handles activation directly and creates
  the HPA only for the active range.
- **Activation works even from zero**: the claim-creation counter is emitted by the controller and
  increments whenever a `SandboxClaim` is created — regardless of how many warm sandboxes exist. So
  even with the pool at 0, a new claim raises the rate, KEDA activates the pool, and the pool scales
  up to absorb subsequent demand.
- **Why filter on `sandbox_template`, not `warmpool_name`**: when the pool is at 0, an incoming
  claim can't be served warm, so the controller records it as a _cold_ launch with
  `warmpool_name="none"`. A `warmpool_name`-scoped query would therefore never increment at zero and
  the pool would never wake. The `sandbox_template` label is recorded on both warm and cold paths,
  so it reliably captures activity from zero.
- **Idle is free**: with no claim activity the rate falls to 0, KEDA scales the pool back to 0, and
  the warm pool consumes no compute at rest.
- **Expect ~1–2 min to wake up.** The metric is a _rate_ over a ~1 minute window
  (PromQL `rate(...[1m])` / Stackdriver `ALIGN_RATE` over 60s), layered on the 15s scrape interval —
  and, on the Stackdriver path, Cloud Monitoring ingestion delay. So `pollingInterval: 15s` is not
  the bottleneck; lowering it won't speed activation. The Prometheus path is the faster of the two.

## Troubleshooting

- **Pool never scales up from 0.** Confirm the metric is actually present:
  `kubectl describe scaledobject -n keda-test agent-warmpool-scaledobject` (look at the
  `ScaledObjectReady` / `ActiveScalers` conditions), and check the operator logs:
  `kubectl logs -n keda deploy/keda-operator`. A `metric-not-found` / empty-result usually means the
  query labels don't match — verify you filtered on `sandbox_template` and generated at least one
  claim.
- **`forbidden` errors in the operator logs** against `sandboxwarmpools` or
  `sandboxwarmpools/scale` mean KEDA lacks RBAC to scale the custom resource. KEDA's default install
  usually covers this (its operator role grants `*/scale`), but if not, grant the `keda-operator`
  service account `get/list/watch` on `sandboxwarmpools` and `get/update/patch` on
  `sandboxwarmpools/scale` via a `ClusterRole`/`ClusterRoleBinding`.
- **Prometheus variant returns no data.** Check the GMP frontend pods are `Running`
  (`kubectl get pods -n keda-test -l app=frontend`) and that the frontend's KSA was authorized with
  `roles/monitoring.viewer` (step 4). Port-forward and query it directly to confirm:
  `kubectl port-forward -n keda-test svc/frontend 9090` then browse `http://localhost:9090`.
- **Stackdriver variant: the HPA briefly shows a huge negative value**
  (`-9223372036854775808m`, i.e. `MinInt64`). This is the sentinel KEDA emits when a poll returns
  **no value** — usually because Cloud Monitoring returned a transient `code = Internal` error on
  the rate query (`kubectl logs -n keda deploy/keda-operator | grep "error getting metric value"`).
  It is **not** a real metric and `valueIfNull` does not cover it (that only handles successful-but-
  empty results, not failed RPCs). Diagnose persistence vs. flakiness:
  ```bash
  # Failed polls in the last 30 min vs ~120 total (one every 15s):
  kubectl logs -n keda deploy/keda-operator --since=30m | grep -c "error getting metric value"
  ```
  A small count means transient Google-side errors — the HPA holds its last-good value and scaling
  still works. The `fallback` block in `scaledobject-stackdriver.yaml` keeps the 1 → N range stable
  through these blips. If it's most/all polls, the query is being rejected — test it in **Cloud
  Console → Metrics Explorer (PromQL)** with
  `sum(rate(agent_sandbox_claim_creation_total{sandbox_template="python-sandbox-template"}[1m]))`.
  Either way, the **Prometheus variant avoids this entirely** (it never calls the Cloud Monitoring
  `timeSeries.list` API).

## Sources

- KEDA Prometheus scaler: <https://keda.sh/docs/2.15/scalers/prometheus/> — a generic,
  backend-agnostic scaler. It is not GCP-specific; it queries any Prometheus-compatible API.
- GMP query frontend (the proxy that exposes GMP over the Prometheus query API):
  <https://cloud.google.com/stackdriver/docs/managed-prometheus/query-api-ui> and the manifest at
  `https://github.com/GoogleCloudPlatform/prometheus-engine/blob/main/examples/frontend.yaml`.
  The "Prometheus → GMP frontend" approach is the generic KEDA Prometheus scaler pointed at this
  Google-provided frontend; it is not a single packaged KEDA recipe.
- KEDA gcp-stackdriver scaler (deprecated): <https://keda.sh/docs/2.20/scalers/gcp-stackdriver/>.
