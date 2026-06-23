# Agent Sandbox Extensions Examples

This directory contains an example of a `SandboxTemplate`, a `SandboxWarmPool` for creating a pool of Pods for Sandboxes to use, and a `SandboxClaim` for requesting Sandboxes from the `SandboxWarmPool`.

## Other Examples

*   **[gke-swap](gke-swap/)**: Demonstrates how to configure GKE node memory swap with dedicated Local SSDs to drastically increase Chrome pod density from 110 to 170 pods per node.
*   **[kata-aks](kata-aks/)**: Shows how to combine extension CRDs with AKS Pod Sandboxing (Kata Containers on Hyper-V) for secure VM-isolated agent warm pools.

## Core Resources

*   `sandboxtemplate.yaml`: Admin-owned blueprint for sandboxes.
*   `sandboxwarmpool.yaml`: Pre-warms N sandboxes to avoid cold-start costs.
*   `sandboxclaim.yaml`: User-facing claim to adopt a sandbox from the pool.
