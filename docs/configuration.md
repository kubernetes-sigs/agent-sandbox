# Configuration

The `agent-sandbox-controller` supports several command-line flags to tune performance and scalability under high load or in large clusters.

For a comprehensive guide on optimizing for high throughput — including API connection tuning, warm pool refill shaping, write reduction, and ready-to-use configs for burst and sustained traffic profiles — see [Performance Tuning](performance-tuning.md).

## Concurrency Settings

* `--sandbox-concurrent-workers` (default: 100): The maximum number of concurrent reconciles for the Sandbox controller.
* `--sandbox-claim-concurrent-workers` (default: 50): The maximum number of concurrent reconciles for the SandboxClaim controller.
* `--sandbox-warm-pool-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the SandboxWarmPool controller.
* `--sandbox-warm-pool-max-batch-size` (default: 300): The maximum number of sandboxes the SandboxWarmPool controller will create/delete in a single batch.
* `--sandbox-warm-pool-readiness-grace-period` (default: `5m`): How long a warm pool sandbox may stay non-Ready before the SandboxWarmPool controller considers it stuck and replaces it (or holds it, if its pod is unschedulable). Raise this for images with long initialization or clusters with slow node auto-provisioning. Must be a positive duration.
* `--sandbox-warm-pool-unschedulable-recheck-interval` (default: `1m`): Requeue interval at which the SandboxWarmPool controller re-checks a pool holding unschedulable sandboxes past the readiness grace period. Must be a positive duration.
* `--kube-api-qps` (default: -1, no client-side rate limiting): Client-side QPS limit for the Kubernetes API client.
* `--kube-api-burst` (default: 10): The maximum burst for client-side throttling of the Kubernetes API client.

## Cluster Settings

* `--cluster-domain` (default: `cluster.local`): The Kubernetes cluster domain used to
  construct service FQDNs. Only change this if your cluster is configured with a non-default
  domain (e.g. `my-company.local`).

## Deployment Example

To deploy the controller with custom concurrency settings, modify the `args` of the `agent-sandbox-controller` container within the project's installation manifests. 

If using the core controller, update `sandbox.yaml`:

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller 
        args:
        - --leader-elect=true
        - --sandbox-concurrent-workers=10
```

If you are deploying the extensions controller (which includes the core controllers + extensions), update the args in `extensions.yaml` instead:

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller 
        args:
        - --leader-elect=true
        - --extensions
        - --sandbox-concurrent-workers=10
        - --sandbox-claim-concurrent-workers=100
        - --sandbox-warm-pool-concurrent-workers=10
        - --sandbox-warm-pool-max-batch-size=500
```
**Using `kubectl patch` (Live Cluster):**
If you have already deployed the controller (e.g., via `make deploy-kind`) and want to apply these concurrency flags dynamically to the running cluster, you can use a JSON patch:

```bash
kubectl patch deployment agent-sandbox-controller \
  -n agent-sandbox-system \
  --type='json' \
  -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-claim-concurrent-workers=100"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-max-batch-size=500"}
  ]'
```
This method safely appends the new flags without overwriting existing necessary arguments like `--leader-elect=true` or `--extensions=true`.

**Using Kustomize:**
If you prefer applying patches via Kustomize rather than modifying the base manifests directly, you can create a patch file (e.g., `patch-args.yaml`):

```yaml
# patch-args.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent-sandbox-controller
  namespace: agent-sandbox-system
spec:
  template:
    spec:
      containers:
      - name: agent-sandbox-controller
        args:
        - --sandbox-concurrent-workers=10
        - --sandbox-claim-concurrent-workers=100
        - --sandbox-warm-pool-concurrent-workers=10
        - --sandbox-warm-pool-max-batch-size=500
```
Then include the patch in your `kustomization.yaml`:
```yaml
patches:
  - path: patch-args.yaml
```

## SandboxClaim label-domain allowlist

Since v0.5.0, label keys in `SandboxClaim.spec.additionalPodMetadata.labels`
must carry a domain prefix from an allowlist; claims with any other label
domain are rejected with `Ready=False, reason=InvalidMetadata`. The default
allowlist is `sandbox.users.io` (subdomains of an allowed domain also pass).

The extensions controller reads the allowlist **once at startup** from
`/etc/sandbox-config/allowed-label-domains`, mounted from an optional
ConfigMap named `agent-sandbox-config` in the controller's namespace. The
file's contents **replace** the default rather than extending it, so include
`sandbox.users.io` if existing consumers rely on it. Domains are separated
by newlines or commas. An empty (or separator-only) file does not disable
the allowlist — the controller treats it the same as an absent file and
falls back to the `sandbox.users.io` default.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: agent-sandbox-config
  namespace: agent-sandbox-system
data:
  allowed-label-domains: |
    example.com
    sandbox.users.io
```

After creating or changing the ConfigMap, restart the controller so it
re-reads the file:

```sh
kubectl -n agent-sandbox-system rollout restart deploy/agent-sandbox-controller
```

Annotations in `additionalPodMetadata` are governed separately by a
restricted-domain blocklist (with `cluster-autoscaler.kubernetes.io/safe-to-evict`
exempted), not by this allowlist.
