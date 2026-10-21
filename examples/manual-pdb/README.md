# Manual PodDisruptionBudget (PDB) Configuration

This example shows how to protect Sandboxes from voluntary disruptions (node drains, cluster upgrades, cluster-autoscaler scale-down) using standard Kubernetes PodDisruptionBudgets. The controller does not create PDBs for you; you apply and delete them yourself.

## How It Works

The controller labels each sandbox pod based on what owns its Sandbox:

| Sandbox owned by | Pod label |
| --- | --- |
| A SandboxClaim (adopted from a warm pool or newly created) | `agents.x-k8s.io/claim-uid` |
| A SandboxWarmPool (idle, not yet claimed) | `agents.x-k8s.io/warm-pool-sandbox` |
| Anything else (for example, a Sandbox created directly) | Neither |

A pod never has both labels: when a claim adopts a warm pool Sandbox, the controller removes the warm pool label and adds the claim label in a single update. That lets one PDB per namespace select either set:

| PDB | Selects | `minAvailable` | Effect |
| --- | --- | --- | --- |
| [`claimed-sandboxes-pdb.yaml`](claimed-sandboxes-pdb.yaml) | Pods with `agents.x-k8s.io/claim-uid` | `1000000` | No claimed sandbox is voluntarily evicted. |
| [`warmpool-pdb.yaml`](warmpool-pdb.yaml) (optional) | Pods with `agents.x-k8s.io/warm-pool-sandbox` and without `agents.x-k8s.io/claim-uid` | `1` | At least one idle warm pool pod in the namespace stays available during drains. |

When a claim adopts a warm pool pod, the pod moves from the warm pool PDB to the claimed-sandboxes PDB.

The two selectors are mutually exclusive: one requires `agents.x-k8s.io/claim-uid`, the other requires it to be absent. Keep it that way. If a pod matches more than one PDB, the eviction API rejects every eviction for it ("This pod has more than one PodDisruptionBudget"), and drains fail. Don't add other PDBs that select these pods.

The warm pool PDB covers every warm pool in the namespace. To limit it to one pool, add a label from that pool's SandboxTemplate `podTemplate` to the PDB's `matchLabels`. Keep `minAvailable` below the pool's `replicas`; otherwise no idle pod can be evicted, and drains of the nodes running them never finish.

Without the optional warm pool PDB, idle warm pool pods can be evicted freely. They hold no user state: the warm pool marks them `cluster-autoscaler.kubernetes.io/safe-to-evict: "true"` by default, and an evicted pod is recreated by its Sandbox.

These selectors depend on labels the controller sets on sandbox pods. If a future release changes those labels, update the selectors: a PDB whose selector matches no pods protects nothing and reports no error.

### Why `minAvailable` Is Always an Integer

For an integer `minAvailable`, the PDB controller just counts the pods the selector matches. For `maxUnavailable` or a percentage `minAvailable`, it has to read the expected pod count from the `/scale` subresource of each pod's owner. Sandbox pods are owned by a Sandbox, which has no `/scale` subresource, so such a PDB reports:

```
Warning  CalculateExpectedPodCountFailed  poddisruptionbudget/<name>  Failed to calculate the number of expected pods: sandboxes.agents.x-k8s.io does not implement the scale subresource
```

When this happens, the PDB controller falls back to `disruptionsAllowed: 0` without updating `currentHealthy` or `expectedPods`. Evictions are blocked, but the PDB status can't be used for monitoring, and the warning repeats on every sync.

To block evictions no matter how many sandboxes exist, `claimed-sandboxes-pdb.yaml` sets `minAvailable` to a number larger than any realistic pod count. Don't set it to the current number of sandboxes: when another sandbox is added, one sandbox becomes evictable, and not necessarily the new one.

### Pods That Aren't Ready

Both PDBs set `unhealthyPodEvictionPolicy: AlwaysAllow`, so drains can still evict pods that aren't Ready (for example, crash-looping pods or pods stuck pulling an image). Remove it if you want to keep those pods too.

## Usage Steps

### Step 1: Apply the Template

```bash
kubectl apply -f sandboxtemplate.yaml -n default
```

### Step 2: Apply the WarmPool

The WarmPool references the template, with the desired number of sandboxes set to 2.

```bash
kubectl apply -f sandboxwarmpool.yaml -n default
```

### Step 3: Apply the PDBs (Once per Namespace)

Apply the PDBs to **every** namespace where you run Sandboxes. `warmpool-pdb.yaml` is optional.

```bash
kubectl apply -f claimed-sandboxes-pdb.yaml -n default
kubectl apply -f warmpool-pdb.yaml -n default
```

### Step 4: Create Sandbox Claims

```bash
kubectl apply -f sandboxclaim.yaml -n default
```

### Step 5: Check the PDBs

```bash
kubectl get pdb -n default
```

The claimed pod is counted by `claimed-sandboxes`, and the remaining idle warm pool pods by `secure-warmpool`:

```
NAME                MIN AVAILABLE   MAX UNAVAILABLE   ALLOWED DISRUPTIONS   AGE
claimed-sandboxes   1000000         N/A               0                     1m
secure-warmpool     1               N/A               1                     1m
```

`kubectl get pdb -o yaml` shows `currentHealthy` and `expectedPods` for each PDB. The `claimed-sandboxes` PDB reports the condition `DisruptionAllowed=False` with reason `InsufficientPods`; that's expected, since `minAvailable` is deliberately larger than the pod count.

Evicting the claimed pod (for example, with `kubectl drain`) fails with `Cannot evict pod as it would violate the pod's disruption budget`. `kubectl drain` keeps retrying indefinitely unless you pass `--timeout`. With two idle warm pool pods, one can be evicted; the second is refused until the first one's replacement is Ready.

## Protect Only Some Claims

To protect only the claims that opt in, add a label to the claimed-sandboxes PDB selector:

```yaml
  selector:
    matchLabels:
      sandbox.users.io/disruption-protection: "true"
    matchExpressions:
    - key: agents.x-k8s.io/claim-uid
      operator: Exists
```

Then set the label on each claim that should be protected:

```yaml
spec:
  additionalPodMetadata:
    labels:
      sandbox.users.io/disruption-protection: "true"
```

Label keys in `additionalPodMetadata` must use a domain from the controller's allowlist (`sandbox.users.io` by default).

## Sandboxes Created Without a Claim

A Sandbox that isn't owned by a SandboxClaim or SandboxWarmPool (for example, one created directly) gets neither label, so neither PDB above covers it. To protect those Sandboxes, add a third PDB. Every sandbox pod carries `agents.x-k8s.io/sandbox-name-hash`, and the two `DoesNotExist` expressions keep this PDB from overlapping the other two:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: direct-sandboxes
  namespace: default
spec:
  minAvailable: 1000000
  unhealthyPodEvictionPolicy: AlwaysAllow
  selector:
    matchExpressions:
    - key: agents.x-k8s.io/sandbox-name-hash
      operator: Exists
    - key: agents.x-k8s.io/claim-uid
      operator: DoesNotExist
    - key: agents.x-k8s.io/warm-pool-sandbox
      operator: DoesNotExist
```

To protect only some of these Sandboxes, add a label to their `spec.podTemplate.metadata.labels` and the same label to this PDB's `matchLabels`.

## Lifecycle & Cleanup

These PDBs are not owned by any Sandbox resource, so they stay in the namespace until you delete them. Sandboxes created or deleted later are picked up automatically; you don't need to update the PDBs.

If you delete `claimed-sandboxes-pdb.yaml` while claims are still running in the namespace, those sandboxes immediately lose protection. Coordinate with your team before deleting it.

Controller-managed PDBs are proposed in [#1346](https://github.com/kubernetes-sigs/agent-sandbox/issues/1346).
