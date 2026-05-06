# Argo CD + SDK Routing: Canary Rollout Example

> [!WARNING]
> This is an example demonstrating the pattern of client-side stochastic routing. It is intended for educational purposes and needs to be reviewed and adapted for production use (e.g., adding robust error handling, security considerations, and proper namespace management).

This example demonstrates how to achieve a **Canary Rollout** without touching the core Go controllers, by combining **GitOps (Argo CD)** with **Client-Side Routing (SDK/Application)**. 

## How This Pattern Works

1.  **GitOps Controlled Weights**: A standard Kubernetes `ConfigMap` acts as the control plane for the rollout. Argo CD manages this ConfigMap. To shift traffic, the operator simply edits the YAML in Git and Argo CD syncs it to the cluster.
2.  **Smart Client**: The Python SDK/application reads this ConfigMap from the cluster prior to creating a `SandboxClaim`. It uses the percentage declared in the ConfigMap to generate a random number and determine which `SandboxWarmPool` to target.

---

## Step 1: Install Argo CD

To install Argo CD on your cluster, run the following commands:

```bash
# Create namespace
kubectl create namespace argocd

# Apply the standard install manifests
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

*(Optional) To access the Argo CD UI:*
```bash
# Port-forward the server
kubectl port-forward svc/argocd-server -n argocd 8080:443

# Get the initial admin password
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" | base64 -d; echo
```

---

## Step 2: Setup the Routing Configuration and Pools

In a real-world scenario, you would hook the files into an Argo CD `Application`. For this localized example, apply them directly using `kubectl`:

```bash
kubectl apply -f templates.yaml
kubectl apply -f pools.yaml
kubectl apply -f canary-config.yaml
```

This creates:
*   Two `SandboxTemplate` resources: `sandbox-python-template-v1` and `sandbox-python-template-v2`.
*   Two `SandboxWarmPool` resources: `python-pool-v1` and `python-pool-v2`.
*   A `ConfigMap` containing the routing weights.

---

## Step 3: Run the SDK Router

Ensure you have the Python Kubernetes client installed:
```bash
pip install kubernetes
```

Run the script to create a Sandbox Claim. You must provide a unique name for the claim:
```bash
python3 sdk_router.py my-test-claim
```

Alternatively, you can run the Go version:
```bash
go run main.go my-test-claim
```

The script will:
1.  Read the `canary-routing-config` ConfigMap.
2.  Stochastically decide which pool to use.
3.  Create an actual `SandboxClaim` resource in the cluster.

You can verify the claim was created and bound to a sandbox:
```bash
kubectl get sandboxclaim my-test-claim -o yaml
```

## Step 4: Advancing the Canary Rollout (Simulating Argo CD Sync)

To advance the rollout, you would normally commit a change to your Git repository. Here, patch the ConfigMap manually to simulate an Argo CD sync progressing the canary to 80%:

```bash
kubectl patch configmap canary-routing-config --type=merge -p '{"data":{"canary_percentage":"80"}}'
```

If you create more claims now, you will see the majority of them being routed to the Canary (`python-pool-v2`) pool.

---

## Step 5: Run Automated E2E Test

We have provided an automated end-to-end script that handles applying resources, simulating traffic at different percentages, and verifying the distribution.

To run it:
```bash
python3 test_e2e.py
```

The script will output a summary showing that traffic correctly shifted as the percentage increased, and will clean up the test claims afterwards.

---

## Step 6: Automated Analysis Job (Argo Rollouts Simulation)

To simulate how Argo Rollouts automatically decides to advance the canary, we have provided an analysis script that checks if all active test claims are `Ready`.

Argo Rollouts can run this script as a Kubernetes `Job`. If the script exits with `0` (Success), Argo advances the rollout. If it exits with `1` (Failure), Argo rolls back.

To test the analysis script manually:

1.  Create a claim using the SDK router:
    ```bash
    python3 sdk_router.py test-claim-for-analysis
    ```
2.  Run the analysis script immediately:
    ```bash
    python3 analysis_job.py
    ```

If the sandbox is successfully bound and ready, the script will print `Analysis PASSED` and exit with code `0`. If you delete the pools or force a failure, it will print `Analysis FAILED` and exit with code `1`.
