# Configuration

The `agent-sandbox-controller` supports several command-line flags to tune performance and scalability under high load or in large clusters.

## Concurrency Settings

* `--sandbox-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the Sandbox controller.
* `--sandbox-claim-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the SandboxClaim controller.
* `--sandbox-warm-pool-concurrent-workers` (default: 1): The maximum number of concurrent reconciles for the SandboxWarmPool controller.
* `--kube-api-qps` (default: -1 ; no rate limiting): The maximum Queries Per Second (QPS) sent to the Kubernetes API server from the controller.
* `--kube-api-burst` (default: 10): The maximum burst for throttle requests to the Kubernetes API server.

## Deployment Example

To deploy the controller with custom concurrency settings, modify the `args` of the `agent-sandbox-controller` container within the project's installation manifests. 

If using the core controller, update `manifest.yaml`:

```yaml
      containers:
      - name: agent-sandbox-controller
        image: ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller 
        args:
        - --leader-elect=true
        - --sandbox-concurrent-workers=10
        - --kube-api-qps=50
        - --kube-api-burst=100
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
        - --sandbox-claim-concurrent-workers=10
        - --sandbox-warm-pool-concurrent-workers=10
        - --kube-api-qps=50
        - --kube-api-burst=100
```
**Using `kubectl patch` (Live Cluster):**
If you have already deployed the controller (e.g., via `make deploy-kind`) and want to apply these concurrency flags dynamically to the running cluster, you can use a JSON patch:

```bash
kubectl patch deployment agent-sandbox-controller \
  -n agent-sandbox-system \
  --type='json' \
  -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-claim-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--sandbox-warm-pool-concurrent-workers=10"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kube-api-qps=50"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kube-api-burst=100"}
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
        - --sandbox-claim-concurrent-workers=10
        - --sandbox-warm-pool-concurrent-workers=10
        - --kube-api-qps=50
        - --kube-api-burst=100
```
Then include the patch in your `kustomization.yaml`:
```yaml
patches:
  - path: patch-args.yaml
```
