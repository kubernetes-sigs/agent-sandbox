# Agent Sandbox Rapid Burst Test

## Overview

This script executes a burst-oriented load test against the Agent Sandbox system on a Kubernetes
cluster using [ClusterLoader2](https://github.com/kubernetes/perf-tests) (CL2).

The test is designed to measure the performance and scalability of the system by creating a large
number of SandboxClaim resources in discrete, rapid bursts. It configures and deploys a Prometheus
server within the cluster to gather detailed performance metrics, including SandboxClaim startup
latency.

## Prerequisites

Before running this test, ensure the following prerequisites are met:

* **Go Environment**: A working Go installation is required to compile and run ClusterLoader2.
* **Kubernetes Cluster**: You must have `kubectl` access configured for a target GKE cluster. The
  script will use the configuration found at `$HOME/.kube/config`.
* **Source Code Repositories**: You must have the following repositories cloned to your local
  machine, typically in your `$HOME` directory:
  * `perf-tests`: The official Kubernetes performance testing repository containing ClusterLoader2.
  * `agent-sandbox`: The main project repository.
    * `agent-sandbox-controller`: The agent-sandbox controller extensions and manifests should be
      installed in the target cluster.

## Running the Test

**Configure Paths**: Open the script and verify that the `CL2_DIR` and `AGENTS_DIR` variables point
to the correct locations of the repositories on your local filesystem.

**Execute**: Run the script from your terminal:

```bash
./run_load_test.sh
```

Note that you may need to first run `sudo chmod +x run_load_test.sh` once.

## Configuration

The primary test parameters can be modified by editing the variables at the top of the script:

* **`BURST_SIZE`**: The number of SandboxClaim resources to create in each burst iteration.
* **`QPS`**: The maximum creation rate (Queries Per Second) for SandboxClaim objects.
* **`TOTAL_BURSTS`**: The total number of burst cycles to run.
* **`WARMPOOL_SIZE`**: The target number of pre-warmed sandboxes to maintain.

The total number of claims created by the test will be `BURST_SIZE * TOTAL_BURSTS`.

## Output

All artifacts for a given test run, including the full CL2 log, generated test overrides, and
Prometheus reports, will be saved to a timestamped directory located at `${TEST_DIR}/tmp/${RUN_ID}`.
