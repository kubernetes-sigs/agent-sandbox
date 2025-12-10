# Gateway API Support for KinD Clusters

This directory contains resources to enable Kubernetes Gateway API support within KinD (Kubernetes in Docker) clusters.

## Problem

The standard Kubernetes Gateway API implementation relies on a LoadBalancer service to provision an external IP address for Gateway resources. KinD clusters, running locally within Docker containers, do not have a built-in cloud provider to service LoadBalancer requests. This limitation prevents the Gateway API from functioning correctly in a typical KinD setup, making it difficult to test components like the Agentic Sandbox SDK client that depend on Gateway resources.

## Solution: Using `cloud-provider-kind`

To bridge the gap, we leverage [`cloud-provider-kind`](https://github.com/kubernetes-sigs/cloud-provider-kind), a Kubernetes Cloud Provider for KinD. This tool simulates cloud provider functionalities, most importantly for our use case, the ability to service `LoadBalancer` type services and provision external IPs for Gateway resources within the KinD cluster.

`cloud-provider-kind` monitors KinD clusters and automatically creates the necessary constructs (like Docker containers acting as load balancers) to expose services and Gateways. It has alpha support for the Gateway API, implementing Gateway and HTTPRoute functionalities.

By running `cloud-provider-kind` alongside our KinD cluster, the Gateway API controller can successfully provision an IP address for Gateway resources, making them accessible. This allows us to test and develop applications that utilize the Gateway API within a local KinD environment, with the Gateway behaving as if it were provisioned by a cloud provider.

### Components in this Directory

-   **`gateway-kind.yaml`**: This manifest deploys the necessary Gateway API resources (like GatewayClass) and a sample Gateway and HTTPRoute to be managed by `cloud-provider-kind`.
-   **`run-test-kind.sh`**: This script automates:
    1.  Creation of the KinD cluster.
    2.  Installation and running of `cloud-provider-kind`.
    3.  Deployment of the Gateway resources from `gateway-kind.yaml`.
    4.  Execution of tests against the Gateway endpoint.
-   **`python-sandbox-template.yaml`**: A sample SandboxTemplate manifest used for testing a full e2e scenario with the Agentic Sandbox.

By using these components, we can create Gateway resources and route traffic to services within the KinD cluster, enabling comprehensive e2e testing of the Agentic Sandbox SDK client.
