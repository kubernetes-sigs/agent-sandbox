# Warm Pool Quickstart

This directory contains reference YAML for the three extension CRDs that power
the warm-pool workflow: `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim`.

## Files

*   `sandboxtemplate.yaml`: Admin-owned blueprint for sandboxes, with persistent storage via `volumeClaimTemplates`.
*   `sandboxwarmpool.yaml`: Pre-warms N sandboxes to avoid cold-start costs.
*   `sandbox-claim.yaml`: User-facing claim to adopt a sandbox from the pool.
*   `secure-sandboxtemplate.yaml`: Hardened template with gVisor runtime and tight NetworkPolicy (DNS-only egress, ingress locked to the Istio ingress gateway).
*   `llm.yaml`: Template that grants access to a private LLM endpoint via `hostAliases` and a scoped egress NetworkPolicy.

## Related examples

*   **[kata-aks](../kata-aks/)**: Full end-to-end example — Python agent, Go client, and sandbox-router — on AKS with Kata Containers (Hyper-V) isolation.
