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
          - --kube-api-qps=1000
          - --kube-api-burst=1000
          - --sandbox-concurrent-workers=1000
          - --sandbox-claim-concurrent-workers=1000
          - --sandbox-warm-pool-concurrent-workers=1000
    ```
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

## Configuration

The primary test parameters can be modified by editing the variables at the top of the
`run_rapid_burst.sh` script or by passing overrides to clusterloader2.

- **`BURST_SIZE`**: The number of SandboxClaim resources to create in each burst iteration.
- **`QPS`**: The maximum creation rate (Queries Per Second) for SandboxClaim objects.
- **`TOTAL_BURSTS`**: The total number of burst cycles to run.
- **`WARMPOOL_SIZE`**: The target number of pre-warmed sandboxes to maintain.
- **`RUNTIME_CLASS`**: The RuntimeClassName for the SandboxTemplate such as `gvisor`.

The total number of claims created by the test will be `BURST_SIZE * TOTAL_BURSTS`.

## Output

All artifacts for a given test run, including the full CL2 log, generated test overrides, and
Prometheus reports, will be saved to a timestamped directory located at `${TEST_DIR}/tmp/${RUN_ID}`.
