# Agent Sandbox Load Testing

This directory contains configuration files for running load tests on the Agent Sandbox using [ClusterLoader2](https://github.com/kubernetes/perf-tests/tree/master/clusterloader2).

## Prerequisites

1.  **Kubernetes Cluster**: You need a running Kubernetes cluster. A local [Kind](https://kind.sigs.k8s.io/) cluster is sufficient for development testing.
2.  **Agent Sandbox Controller**: The controller and CRDs must be installed on the cluster.
3.  **ClusterLoader2**: The `clusterloader` binary must be installed.

## Setup

### 1. Install Agent Sandbox

If you haven't already, deploy the Agent Sandbox controller to your cluster. From the root of the repository:

```bash
make deploy-kind
```

### 2. Install ClusterLoader2

You can install `clusterloader2` using `go install`:

```bash
go install k8s.io/perf-tests/clusterloader2/cmd/clusterloader@latest
```

Alternatively, you can download a release from the perf-tests repository.

## Running the Load Test

The load test is defined in `agent-sandbox-load-test.yaml`. It creates a specified number of Sandboxes using the template in `sandbox-template.yaml` and measures startup latency.

To run the test against a Kind cluster:

```bash
clusterloader --testconfig=agent-sandbox-load-test.yaml \
  --provider=kind \
  --kubeconfig=$HOME/.kube/config \
  --v=2
```

**Note:** Ensure you are in the `load-test/` directory when running this command, as the configuration references `sandbox-template.yaml` via a relative path.

## Configuration

*   **`agent-sandbox-load-test.yaml`**: The main test definition.
    *   `replicasPerNamespace`: Controls the number of Sandboxes to create.
    *   `tuningSets`: Controls the rate of creation (QPS).
*   **`sandbox-template.yaml`**: The definition of the Sandbox resource created during the test.
