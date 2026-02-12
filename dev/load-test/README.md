# Agent Sandbox Load Testing

This directory contains configuration files for running load tests on the Agent Sandbox using [ClusterLoader2](https://github.com/kubernetes/perf-tests/tree/master/clusterloader2).

## Prerequisites

1.  **Kubernetes Cluster**: You need a running Kubernetes cluster. 
2.  **Agent Sandbox Controller**: The controller and CRDs must be installed on the cluster.
3.  **Go Lang**: The clusterloader2 uses Go to execute the load tests.

## Setup

### 1. Install Agent Sandbox Controller

You can install the agent-sandbox controller by following the instructions: https://github.com/kubernetes-sigs/agent-sandbox#installation.

### 2. Install ClusterLoader2

Follow the instructions to install [ClusterLoader2](https://github.com/kubernetes/perf-tests/blob/master/clusterloader2/docs/GETTING_STARTED.md#clusterloader2). This creates a new local repository as a sibling to this repository.

The expected directory structure is:

```text
workspace/
├── agent-sandbox/          # This repository
│   └── dev/
│       └── load-test/
│           └── agent-sandbox-load-test.yaml
└── perf-tests/             # Cloned from kubernetes/perf-tests
    └── clusterloader2/
```

## Running the Load Test

The load test is defined in `agent-sandbox-load-test.yaml`. 

It creates a specified number of Sandboxes using the template in `cluster-loader-sandbox.yaml` and measures startup latency.

### 1. Build the cluster loader

Make sure your current directory is: `perf-tests/clusterloader2`. Build the cluster loader first.

```bash
go build -o clusterloader2 ./cmd/clusterloader.go
```

### 2. Run the load test

To run the test against your Kubernetes cluster, execute the command below:

```bash
./clusterloader2 
--testconfig=../../agent-sandbox/dev/load-test/agent-sandbox-load-test.yaml 
--kubeconfig=$HOME/.kube/config
--provider=gke
```

To run the test against against your local kind Kubernetes cluster, please follow
the [kind installation](https://kind.sigs.k8s.io/docs/user/quick-start#installation) guide.

Then execute the command below:

```bash
./clusterloader2 
--testconfig=../../agent-sandbox/dev/load-test/agent-sandbox-load-test.yaml 
--kubeconfig=$HOME/.kube/config
--provider=kind
```

**Note:** Ensure you are in the `clusterloader2/` directory when running this command, as the configuration references `agent-sandbox-load-test.yaml` via a relative path.

### 3. Verify results

Once the test is run, the results will be saved in `junit.xml` under the `clusterloader2/` directory.
The result will look like this.

```xml
<?xml version="1.0" encoding="UTF-8"?>
  <testsuite name="ClusterLoaderV2" tests="0" failures="0" errors="0" time="57.957">
      <testcase name="agent-sandbox-load-test overall (../../agent-sandbox-initial-playing/load-test/agent-sandbox-load-test.yaml)" classname="ClusterLoaderV2" time="57.955555557"></testcase>
      <testcase name="agent-sandbox-load-test: [step: 01] Start Startup Latency Measurement [00] - SandboxStartupLatency" classname="ClusterLoaderV2" time="0.225971844"></testcase>
      <testcase name="agent-sandbox-load-test: [step: 02] Create Sandboxes" classname="ClusterLoaderV2" time="2.012305727"></testcase>
      <testcase name="agent-sandbox-load-test: [step: 03] Wait for Sandboxes to be Ready [00] - WaitForSandboxes" classname="ClusterLoaderV2" time="5.095579777"></testcase>
      <testcase name="agent-sandbox-load-test: [step: 04] Gather Results [00] - SandboxStartupLatency" classname="ClusterLoaderV2" time="0.126157956"></testcase>
  </testsuite>
```
