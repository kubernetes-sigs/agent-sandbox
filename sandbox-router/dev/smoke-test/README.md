# sandbox-router smoke test

End-to-end smoke test for the Go sandbox-router on a kind cluster. The script verifies that the deployed router actually does what the unit tests promise: serves traffic, populates the Pod-IP cache from a real informer, invalidates entries when the upstream goes away, and (in the second pass) enforces TokenReview-based auth on every request.

## What it does

1. Builds the router image from `sandbox-router/Dockerfile` against the repo root.
2. Creates (or reuses) a kind cluster, loads the image, applies the example deploy manifests (`serviceaccount.yaml`, `rbac.yaml`, `service.yaml`, `deployment.yaml`).
3. Creates a fake "sandbox" Pod: the controller-stamped label `agents.x-k8s.io/sandbox-name-hash` plus an OwnerReference of `kind: Sandbox, apiVersion: agents.x-k8s.io/v1beta1` (so the cache sees and indexes it). The Pod runs `hashicorp/http-echo` so we can match on a known response body.
4. Sends an in-cluster `curl` through `sandbox-router-svc:8080`:
   - DNS-form routing (no `X-Sandbox-Uid`) → expects 200 with `smoke-ok`.
   - UID-cache hit (`X-Sandbox-Uid` matching the OwnerReference UID) → expects 200.
5. Scrapes `/metrics` and asserts the new collectors are present (`sandbox_router_authz_decisions_total`, `sandbox_router_cache_invalidations_total`, `sandbox_router_requests_total`).
6. Deletes the sandbox Pod and immediately re-sends a request with the same UID to exercise the active-invalidation path.
7. Patches the deployment to `--authz-mode=tokenreview --authz-tokenreview-require-token=true`, then verifies that:
   - Requests without `Authorization: Bearer` get 401.
   - Requests with a fresh projected ServiceAccount token (`kubectl create token default`) succeed.

## Requirements

- `kind`, `kubectl`, `docker` on `$PATH`.
- No CRDs required — the test uses a fake Pod with a fake `Sandbox` OwnerReference. The router does not validate that the owner actually exists; it only reads the UID off the reference.

## Run

```sh
./sandbox-router/dev/smoke-test/run.sh
```

The script is idempotent (will reuse an existing cluster of the same name) and tears the cluster down on exit. Override behavior:

| Env | Default | Purpose |
|---|---|---|
| `CLUSTER_NAME` | `sandbox-router-smoke` | kind cluster name. |
| `ROUTER_IMAGE` | `kind.local/sandbox-router-go:smoke` | Image tag built and loaded. |
| `KEEP_CLUSTER` | `0` | Set to `1` to skip teardown so you can `kubectl exec` around. |

Typical wall-clock: ~3 minutes on a warm Docker (image build dominates).

## When to run it

Not on every PR — kind plus image build is too slow for the normal CI tier. Run it manually before tagging a release, after touching anything in `cache/`, `proxy/`, `authz/`, or the deploy manifests, and as part of the next-release verification checklist.
