# Gateway API Support for KinD Clusters

This directory contains resources to enable Kubernetes Gateway API support within KinD (Kubernetes in Docker) clusters. Since KinD doesn't have a native load balancer, we use `cloud-provider-kind` to provision an IP address for the Gateway resource. This enables the agentic-sandbox-client to connect to sandboxes in the cluster via a Kubernetes Gateway.

### Step-by-step Guide for Local Kind Cluster

This guide explains how to manually set up the Sandbox Router and Gateway API on a local KinD cluster.

1.  **Setup Agentic Sandbox and Python SDK Client:** Follow the instructions in the [main client README](../README.md), which includes prerequisites, installing the SDK, and deploying the Sandbox Router.

2.  **Install and Run cloud-provider-kind:**
    This component provides a load balancer implementation for KinD.

    Install the latest version and run `cloud-provider-kind` in the background, enabling the Gateway API controller::
    ```bash
    make deploy-cloud-provider-kind
    ```
    
    *See [cloud-provider-kind documentation](https://github.com/kubernetes-sigs/cloud-provider-kind) for more details.*

3.  **Deploy KinD Gateway Resources:**
    Apply the Gateway, and HTTPRoute from this directory:
    ```bash
    kubectl apply -f gateway-kind.yaml
    ```
    
4.  **Test the Setup:**
    a.  **Check Gateway Status:** After a short time, verify that the Gateway has been assigned an IP address by `cloud-provider-kind`:
    ```bash  
    kubectl get gateway
    ```
    You should see output similar to this, with an ADDRESS populated:
    ```bash
    NAME           CLASS                 ADDRESS       PROGRAMMED   AGE
    kind-gateway   cloud-provider-kind   192.168.8.3   True         2m
    ```
    If the ADDRESS is empty, wait a bit longer or check the `cloud-provider-kind` logs.
    
    b.  **Run Test Client:** Use the `agentic-sandbox-client` in Gateway mode to test the end-to-end flow:
    ```bash  
    python ../test_client.py --gateway_name="kind-gateway"
    ```

5. **Clean up:** To stop the cloud-provider-kind process, run:
    ```bash
    make kill-cloud-provider-kind
    ```

### Automated Setup & Test

For a fully automated setup and test run, you can use the `run-test-kind.sh` script provided in this directory:

```bash
./run-test-kind.sh
```
