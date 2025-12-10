# Manual PodDisruptionBudget (PDB) Configuration

This example demonstrates how to protect Sandboxes from voluntary disruptions (e.g., node drains, cluster upgrades) using standard Kubernetes manifests, without relying on the controller's automated disruption control.

## How It Works (Shared Protection)

In this manual workflow, you create a **single Shared PDB** per namespace. This PDB acts as a "security blanket" that protects **all** Sandboxes in that namespace, provided they opt-in via labels.

### 1. The Matching Rule
Protection is **not automatic**. A Sandbox is only protected if its Template metadata matches the PDB selector exactly.

* **PDB Selector:** `matchLabels: { sandbox-disruption-policy: "manual-protection" }`
* **Sandbox Template:** Must have `labels: { sandbox-disruption-policy: "manual-protection" }`

Any Sandbox created in this namespace without this specific label will be ignored by the PDB and can be evicted at any time.

### 2. The "Math" of Shared PDBs
Because multiple Sandboxes share one PDB, we use `maxUnavailable: 0`.

* **Scenario:** You have 3 Sandboxes running in the `dev-team` namespace.
* **Logic:** `maxUnavailable: 0` tells Kubernetes: "You cannot voluntarily evict *any* pod that has this label."
* **Result:** All 3 Sandboxes are protected individually.

*Warning: Do not use `minAvailable: 1` for a shared PDB. If you have 3 sandboxes and require only 1 to be available, Kubernetes is allowed to evict the other 2 during maintenance.*

## Usage Steps

### Step 1: Apply the Template
The template includes the required labels and the `safe-to-evict` annotation.
```bash
kubectl apply -f sandbox-template.yaml
```

### Step 2: Apply the Shared PDB (Once per Namespace)
You must apply the PDB manifest to **every** namespace where you intend to run Sandboxes.

```bash
# Example for the 'default' namespace
kubectl apply -f shared-pdb.yaml -n default
```

### Step 3: Create Sandbox Claims
You can now create as many Claims as you want in that namespace.

```bash
# Example for the 'default' namespace
kubectl apply -f sandbox-claim.yaml -n default
```


### Lifecycle & Cleanup (Important)
Unlike the automated controller implementation, **manual PDBs do not clean themselves up.**

- **When you delete a Sandbox:** The PDB remains active in the namespace.

- **When you delete the last Sandbox:** You must manually delete the PDB if you want to remove the configuration.

- **Risk:** If you delete the PDB while other Sandboxes are still running in that namespace, those Sandboxes immediately lose protection. Coordinate with your team before deleting the shared PDB.