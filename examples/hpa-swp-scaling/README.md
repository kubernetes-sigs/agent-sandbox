# SandboxWarmPool Scaling with HPA (GKE Specific)

This example demonstrates how to use the Kubernetes Horizontal Pod Autoscaler (HPA) to scale a `SandboxWarmPool` based on custom metrics emitted by the agent sandbox controller.

> **Note:**
> This example is tailored to Google Cloud Platform (GCP) / Google Kubernetes Engine (GKE). It depends on GKE Managed Service for Prometheus and the Stackdriver Custom Metrics Adapter.

## Overview

In this example, we show how to scale a pool of warm sandboxes dynamically based on the rate of sandbox claims being created. This allows the system to maintain a ready supply of sandboxes as demand increases.

## Prerequisites

- A Google Kubernetes Engine (GKE) cluster.
- GKE Managed Service for Prometheus enabled.
- Custom Metrics Adapter installed on your cluster as described in the [GKE Documentation](https://docs.cloud.google.com/kubernetes-engine/docs/tutorials/autoscaling-metrics#step1).

## Steps to Run

1. **Setup Agent Sandbox**: 
   Create the namespace and apply the template and warm pool manifests.
   ```bash
   kubectl create namespace hpa-test
   kubectl apply -f python-sandbox-template.yaml
   kubectl apply -f sandboxwarmpool.yaml
   ```

2. **Expose metrics via GKE Managed Service for Prometheus**:
   Apply the `pod-monitoring.yaml` to enable metric scraping.
   ```bash
   kubectl apply -f pod-monitoring.yaml
   ```

3. **Configure the HPA**:
   Once the custom metric is exposed in Prometheus, you can connect it to the HPA configuration. We set the guardrails for scaling:
   - **Minimum Capacity**: 10 sandboxes 
   - **Maximum Capacity**: 100 sandboxes (sets a hard budget ceiling).
   - **Metric**: Raw Prometheus metric `agent_sandbox_claim_creation_total`, which the GKE Custom Metrics Adapter exposes to the HPA as `prometheus.googleapis.com|agent_sandbox_claim_creation_total|counter`. This is a counter metric that is incremented every time a sandbox claim is created.
   - **The Target**: 0.5 rate of claims created per second. The HPA will adjust the warmpool replicas to maintain this target.

   > **Note:**
   > In the GKE Custom Metrics Adapter, metrics from Prometheus Managed Service are mapped to the `prometheus.googleapis.com|...` format. In this example, `hpa.yaml` uses the adapter-mapped metric name `prometheus.googleapis.com|agent_sandbox_claim_creation_total|counter`, and the counter is evaluated as a rate of change (rate per second).

   ```bash
   kubectl apply -f hpa.yaml
   ```

4. **Generate load** to trigger scaling:
   ```bash
   python create-claim.py
   ```

5. **Verify scaling**:
   Run the following command to watch the HPA scale:
   ```bash
   kubectl get hpa -n hpa-test -w
   ```
   *Note: If you have `ts` from `moreutils` installed and want timestamped output, you can use: `kubectl get hpa -n hpa-test -w | ts '[%Y-%m-%d %H:%M:%S]'`*

   Example output:
   ```text
[2026-04-12 20:49:24] NAME                 REFERENCE                             TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
[2026-04-12 20:49:24] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m    10        100       10         2d23h
[2026-04-12 20:52:27] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   250m/500m   10        100       10         2d23h
[2026-04-12 20:52:42] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   250m/500m   10        100       16         2d23h
[2026-04-12 20:52:58] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   750m/500m   10        100       23         2d23h
[2026-04-12 20:53:13] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   750m/500m   10        100       46         2d23h
[2026-04-12 20:53:28] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1/500m      10        100       92         2d23h
[2026-04-12 20:53:43] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1/500m      10        100       100        2d23h
[2026-04-12 20:53:58] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   999m/500m   10        100       100        2d23h
[2026-04-12 20:54:14] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   999m/500m   10        100       100        2d23h
[2026-04-12 20:55:00] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1/500m      10        100       100        2d23h
[2026-04-12 20:55:31] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   999m/500m   10        100       100        2d23h
[2026-04-12 20:56:01] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   866m/500m   10        100       100        2d23h
[2026-04-12 20:56:31] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   999m/500m   10        100       100        2d23h
[2026-04-12 20:57:02] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1132m/500m   10        100       100        2d23h
[2026-04-12 20:57:17] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1132m/500m   10        100       100        2d23h
[2026-04-12 20:57:32] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   982m/500m    10        100       100        2d23h
[2026-04-12 20:58:02] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   999m/500m    10        100       100        2d23h
[2026-04-12 20:58:33] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1016m/500m   10        100       100        2d23h
[2026-04-12 20:59:03] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1/500m       10        100       100        2d23h
[2026-04-12 21:00:04] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   999m/500m    10        100       100        2d23h
[2026-04-12 21:00:34] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1/500m       10        100       100        2d23h
[2026-04-12 21:01:04] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   999m/500m    10        100       100        2d23h
[2026-04-12 21:02:05] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   1/500m       10        100       100        2d23h
[2026-04-12 21:02:35] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   749m/500m    10        100       100        2d23h
[2026-04-12 21:02:50] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   272m/500m    10        100       100        2d23h
[2026-04-12 21:03:06] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   266m/500m    10        100       100        2d23h
[2026-04-12 21:03:21] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   16m/500m     10        100       100        2d23h
[2026-04-12 21:03:51] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m       10        100       100        2d23h
[2026-04-12 21:07:23] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m       10        100       100        2d23h
[2026-04-12 21:07:38] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m       10        100       96         2d23h
[2026-04-12 21:07:53] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m       10        100       43         2d23h
[2026-04-12 21:08:08] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m       10        100       43         2d23h
[2026-04-12 21:08:23] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m       10        100       10         2d23h
[2026-04-12 21:08:39] agent-warmpool-hpa   SandboxWarmPool/python-sdk-warmpool   0/500m       10        100       10         2d23h
   ```
  