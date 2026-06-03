# Agent Sandbox Rapid Burst Test (Test Recipes)

## Overview

This script and configuration executes a burst-oriented load test against the Agent Sandbox system
on a Kubernetes cluster using [ClusterLoader2](https://github.com/kubernetes/perf-tests) (CL2).

This README applies specifically to the **Rapid Burst Test**, which is located in
`dev/load-test/test-recipes/`.

The test is designed to measure the performance and scalability of the system by creating a large
number of SandboxClaim resources in discrete, rapid bursts. It configures and deploys a Prometheus
server within the cluster to gather detailed performance metrics, including SandboxClaim startup
latency.

## Prerequisites

Before running this test, ensure the following prerequisites are met:

- **Go Environment**: A working Go installation is required to compile and run ClusterLoader2.
- **Kubernetes Cluster**: You must have `kubectl` access configured for a target GKE cluster. The
  script will use the configuration found at `$HOME/.kube/config`.
- **Source Code Repositories**: You must have the following repositories cloned to your local
  machine, typically in your `$HOME` directory:
  - `perf-tests`: The official Kubernetes performance testing repository containing ClusterLoader2.
  - `agent-sandbox`: The main project repository.
- **`agent-sandbox-controller`**: The agent-sandbox controller extensions and manifests should be
  installed in the target cluster.
  - If you have made local changes to the controller, you can build the image using
    ```bash
    cd ~/agent-sandbox
    ./dev/tools/push-images --image-prefix=path/to/your/repo --controller-only
    ```
  - Generate the manifests using `cd ~/agent-sandbox && make release-manifests TAG=123`. The
    manifests will be generated in `~/agent-sandbox/release_assets`. Search for the
    `image: registry.k8s.io/agent-sandbox/agent-sandbox-controller:123` line in the generated
    extensions and manifest files and replace the image with your image:tag.
  - We recommend adding and adjusting the below configurations in the `extensions.yaml` generated
    manifest to whatever values are appropriate for your cluster size:
    ```yaml
    containers:
      - name: agent-sandbox-controller
        image: path/to/your/image:your-tag
        args:
          - --leader-elect=true
          - --extensions
          - --enable-pprof-debug
          - --enable-tracing
          - --zap-log-level=debug
          - --zap-encoder=json
          - --kube-api-qps=1000
          - --kube-api-burst=1000
          - --sandbox-concurrent-workers=1000
          - --sandbox-claim-concurrent-workers=1000
          - --sandbox-warm-pool-concurrent-workers=1000
    ```
  - If you are using tracing, see [GKE OTLP Metrics](https://docs.cloud.google.com/stackdriver/docs/otlp-metrics/deploy-collector)
    for how to deploy the collector.
  - Apply your modified manifests to your cluster to install the agent-sandbox controller.
    ```bash
    cd ~/agent-sandbox
    kubectl apply -f release_assets/manifest.yaml
    kubectl apply -f release_assets/extensions.yaml
    ```

## Running the Test

**Execute**: Run the script from your terminal:

```bash
cd dev/load-test/test-recipes
./run_rapid_burst.sh
```

You can optionally pass in a name which will be appended to the output directory for the test
artifacts.

```bash
./run_rapid_burst.sh test1
```

Note that you may need to first run `chmod +x run_rapid_burst.sh` once.

### Running with HPA and CapacityBuffer Scaling

You can enable Horizontal Pod Autoscaler (HPA) and GKE CapacityBuffer scaling by setting environment variables before running the script. This allows the system to dynamically scale the pre-warmed sandbox pool as demand spikes and ensure underlying standby node capacity is provisioned.

**Prerequisites:** This mode requires additional cluster setup beyond the base test prerequisites. `ENABLE_HPA=true` requires the external/custom metric pipeline used by the SandboxWarmPool HPA to be available on the cluster (for example, GKE Managed Service for Prometheus plus the Custom Metrics Adapter, as described in the linked HPA example below). `ENABLE_CAPACITY_BUFFER=true` requires the GKE `CapacityBuffer` feature/CRD to be installed and enabled on the cluster; otherwise the resource creation will fail.

```bash
ENABLE_HPA=true ENABLE_CAPACITY_BUFFER=true WARMPOOL_SIZE=10 ./run_rapid_burst.sh
```

When `ENABLE_CAPACITY_BUFFER=true` is set, the test automatically introduces a 5-minute pause after creating the `CapacityBuffer` resource to allow GKE node auto-provisioning to spin up the required standby nodes before initiating the rapid burst loops.

> [!NOTE]
> **GKE Cluster Autoscaler CRD Version Caching Issue:**
> If you recently upgraded `SandboxWarmPool` CRD versions in your cluster (e.g., from `v1alpha1` to `v1beta1`), the GKE Cluster Autoscaler (CA) may fail, resulting in `there is no pod template reference in buffer status` errors. This is a known GKE issue where the Cluster Autoscaler caches CRD schemas and versions. To resolve this, you may need to recreate or trigger a restart of the GKE Cluster Autoscaler to force a schema cache refresh.`

## Configuration

The primary test parameters can be modified by editing the variables at the top of the
`run_rapid_burst.sh` script or by passing overrides to clusterloader2.

- **`BURST_SIZE`**: The number of SandboxClaim resources to create in each burst iteration.
- **`QPS`**: The maximum creation rate (Queries Per Second) for SandboxClaim objects.
- **`TOTAL_BURSTS`**: The total number of burst cycles to run.
- **`WARMPOOL_SIZE`**: The target number of pre-warmed sandboxes to maintain.
- **`RUNTIME_CLASS`**: The RuntimeClassName for the underlying SandboxTemplate linked to the pool, such as `gvisor`.

The total number of claims created by the test will be `BURST_SIZE * TOTAL_BURSTS`.

### Autoscaling & Capacity Buffer Parameters

For more details on HPA configuration and scaling behavior, refer to the [HPA SandboxWarmPool Scaling Example](../../../examples/hpa-swp-scaling/README.md).

- **`ENABLE_HPA`**: Set to `true` to deploy a HorizontalPodAutoscaler targeting the SandboxWarmPool (default: `false`). Note: The SandboxWarmPool is automatically provisioned under the name `warmpool-0` by this recipe, and both HPA and CapacityBuffer target this resource.
- **`HPA_MIN_REPLICAS`**: The minimum number of pre-warmed sandboxes for the HPA (default: `1000`).
- **`HPA_MAX_REPLICAS`**: The maximum ceiling for the HPA (default: `2000`).
- **`HPA_TARGET_VALUE`**: The target creation rate of SandboxClaims per second (default: `0.5`).
- **`HPA_METRIC_NAME`**: The external metric name queried by the HPA to perform scaling (default: `"prometheus.googleapis.com|agent_sandbox_claim_creation_total|counter"`).
- **`ENABLE_CAPACITY_BUFFER`**: Set to `true` to deploy a GKE `CapacityBuffer` resource (default: `false`).
- **`BUFFER_PERCENTAGE`**: The percentage of extra capacity to maintain in standby (default: `200`).
- **`PROVISIONING_STRATEGY`**: The GKE provisioning strategy for standby capacity (default: `buffer.gke.io/standby-capacity`).
- **`CAPACITY_BUFFER_PAUSE_DURATION`**: The cooldown/pause duration to sleep after deploying the GKE CapacityBuffer to allow node auto-provisioning to spin up standby capacity (default: `5m`).

## Output

All artifacts for a given test run, including the full CL2 log, generated test overrides, and
Prometheus reports, will be saved to a timestamped directory located at `${TEST_DIR}/tmp/${RUN_ID}`.
