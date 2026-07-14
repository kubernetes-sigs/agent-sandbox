# OpenClaw Sandbox (gVisor + Template/Claim)

Runs [OpenClaw](https://github.com/openclaw/openclaw) inside the Agent Sandbox using the
`SandboxTemplate` + `SandboxWarmPool` + `SandboxClaim` pattern, under gVisor, with a
persistent workspace PVC. Ships access flows for both kind (NodePort) and GKE (LoadBalancer + IAP tunnel).

## What you get

- `SandboxTemplate` with `runtimeClassName: gvisor`, an init container that seeds
  config from a ConfigMap, and a 2Gi PVC mounted at `/workspace/.openclaw`.
- **Security-hardened context** running as non-root user `1000:1000` with all linux capabilities dropped (`capabilities.drop: ["ALL"]`) and `allowPrivilegeEscalation: false`.
- `SandboxWarmPool` with one pre-warmed replica so claims resolve quickly.
- `SandboxClaim` that adopts a sandbox from the pool.
- Two Service variants — pick one for your environment:
  - `kind-service.yaml` — `NodePort` on `30789`, paired with the kind port mapping below.
  - `gke-service.yaml` — `LoadBalancer`, GKE provisions an external IP.
- `run-test-kind.sh` that applies everything, verifies the gateway via NodePort,
  and asserts the PVC survives a pod restart.

## Prerequisites

1. **Kind cluster with gVisor and NodePort mapping.** You need a KIND cluster configured with both gVisor containerd patches/mounts and the `extraPortMappings` for port `30789`.

   A complete, pre-configured [kind-config.yaml](kind-config.yaml) is provided in this directory. You can launch your cluster directly with:
   ```bash
   kind create cluster --name agent-sandbox --config kind-config.yaml
   ```

2. **gVisor available in the cluster.** A `RuntimeClass` named `gvisor` must
   exist and `runsc` must be installed on the node image. Register the `RuntimeClass` with:
   ```bash
   kubectl apply -f - <<EOF
   apiVersion: node.k8s.io/v1
   kind: RuntimeClass
   metadata:
     name: gvisor
   handler: runsc
   EOF
   ```
   See the [gVisor Kubernetes quickstart](https://gvisor.dev/docs/user_guide/quick_start/kubernetes/) for node installation details.

3. **`agent-sandbox` controllers installed**, including the extensions CRDs
   (`SandboxTemplate`, `SandboxWarmPool`, `SandboxClaim`).

4. **Hardened security policy compliant.** This template runs as non-root
   and drops all capabilities, making it compatible with the hardened policy in
   [`examples/policy/vap/secure-sandbox-policy.yaml`](../policy/vap/secure-sandbox-policy.yaml).

## Usage

For kind, the easiest path is the end-to-end test script:

```bash
./run-test-kind.sh
```

It pulls the image, loads it into kind, applies the manifests, waits for the
claim's pod to become ready, checks the gateway via `http://127.0.0.1:30789`,
and runs the PVC persistence test.

### Apply manually

```bash
TOKEN="$(openssl rand -hex 32)"
kubectl apply -f openclaw-config.yaml
sed "s/dummy-token-for-sandbox/${TOKEN}/g" openclaw-template.yaml | kubectl apply -f -
kubectl apply -f openclaw-warmpool.yaml
kubectl apply -f openclaw-claim.yaml
kubectl apply -f kind-service.yaml          # or gke-service.yaml on GKE
```

> **Note on `openclaw-config.yaml`** — the `allowedOrigins` list is pinned to
> the access ports used below (`30789` for kind NodePort, `18789` for the GKE
> IAP tunnel's local port). If you change either port, update `allowedOrigins`
> to match — under `--bind=lan` OpenClaw refuses to start with an unknown
> origin. For quick local-dev testing where you don't want to maintain the
> list, swap `allowedOrigins` for `"dangerouslyAllowHostHeaderOriginFallback": true`
> as a local-only escape hatch (not for anything exposed beyond your laptop).

## Accessing the UI

### On kind

The gateway is reachable directly at `http://127.0.0.1:30789` (assuming the
`extraPortMappings` from the kind config above). `localhost` satisfies the
browser's secure-context requirement, so the token form will accept input.

### On GKE

GKE needs three things: a sandbox-enabled node pool, the `LoadBalancer` Service,
and an IAP tunnel to satisfy OpenClaw's secure-context requirement. The
LoadBalancer's public IP loads the UI but the token form rejects input over
plain HTTP — IAP gives you a `localhost:PORT` that bypasses both the
secure-context gate and gVisor's port-forward incompatibility.

**1. Create a sandbox-enabled node pool** (one-time per cluster). Pods using
`runtimeClassName: gvisor` will only schedule onto nodes in a sandbox-enabled
pool — if none exist, they'll stay `Pending` indefinitely.

```bash
gcloud container node-pools create sandbox-pool \
  --cluster=CLUSTER_NAME \
  --location=CLUSTER_LOCATION \
  --sandbox type=gvisor \
  --machine-type=e2-standard-4 \
  --image-type=cos_containerd
```

See the [GKE Sandbox docs](https://cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods)
for caveats (no GPUs, Standard mode only, etc.).

**2. Apply `gke-service.yaml`** instead of `kind-service.yaml`. Wait for the
external IP to provision (only needed for visibility — the IAP tunnel uses the
NodePort under the hood):

```bash
kubectl get svc openclaw-gateway -w
```

**3. Open an IAP TCP tunnel** to one of the cluster nodes. IAP routes from your
laptop's `localhost:18789` straight to the node's NodePort — bypassing
`kubectl port-forward` (which doesn't work under gVisor) and putting the
browser in a secure context.

```bash
# Find the NodePort GKE allocated and pick any node
NODE_PORT="$(kubectl get svc openclaw-gateway -o jsonpath='{.spec.ports[0].nodePort}')"
NODE="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"

# One-time firewall rule so IAP can reach the NodePort
gcloud compute firewall-rules create allow-iap-openclaw \
  --direction=INGRESS \
  --source-ranges=35.235.240.0/20 \
  --allow="tcp:${NODE_PORT}" \
  --network=default
# If you ever change the NodePort (e.g., drop the pin in gke-service.yaml so GKE auto-allocates), update the rule to match the new port:
#   gcloud compute firewall-rules update allow-iap-openclaw --allow="tcp:${NODE_PORT}"
# Re-running the create above with a different port fails because the rule
# name already exists.

# Open the tunnel (leave running)
gcloud compute start-iap-tunnel "$NODE" "$NODE_PORT" \
  --local-host-port=localhost:18789 \
  --zone=CLUSTER_ZONE
```

Then browse to `http://localhost:18789`. Common failure modes:

- `PERMISSION_DENIED` on `start-iap-tunnel` → add the IAP tunnel IAM role:
  `gcloud projects add-iam-policy-binding PROJECT_ID --member=user:YOU@example.com --role=roles/iap.tunnelResourceAccessor`
- Connection times out → wrong NodePort or firewall rule didn't apply; re-run the lookup
- `502 Bad Gateway` → no ready endpoint behind the Service; check `kubectl get endpoints openclaw-gateway`

## Retrieve the gateway token

Whichever access path you took, the UI will prompt for a token. The value is
the random string `sed` injected into the template's `OPENCLAW_GATEWAY_TOKEN`
env var at apply time. Pull it back out of the running pod:

```bash
SANDBOX_NAME=$(kubectl get sandboxclaim openclaw-sandbox-claim -o jsonpath='{.status.sandbox.name}')
POD=$(kubectl get sandbox "$SANDBOX_NAME" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
kubectl exec "$POD" -- printenv OPENCLAW_GATEWAY_TOKEN
```

Copy the printed value into the UI's token form.

## Browser pairing / device authorization

OpenClaw uses a zero-trust device authorization policy. Even with a valid token,
the first connection from a new browser tab shows a **"pairing required"**
message. To approve without copy-pasting IDs:

```bash
# 1. Get the active pod name
SANDBOX_NAME=$(kubectl get sandboxclaim openclaw-sandbox-claim -o jsonpath='{.status.sandbox.name}')
POD_NAME=$(kubectl get sandbox $SANDBOX_NAME -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')

# 2. Find the pending request ID and approve it
REQUEST_ID=$(kubectl exec $POD_NAME -- node dist/index.js devices list | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -n 1)

if [ -n "$REQUEST_ID" ]; then
  echo "Found pending pairing request: $REQUEST_ID"
  kubectl exec $POD_NAME -- node dist/index.js devices approve $REQUEST_ID
  echo "Successfully paired! Refresh your browser tab to access the dashboard."
else
  echo "No pending pairing requests found. Make sure you have accessed the browser page first!"
fi
```

The pairing is stored under `/workspace/.openclaw`, which is on the PVC — so it
survives pod restarts. You only do this once per browser.

## Provider API keys

After pairing, the dashboard loads but chat will fail until OpenClaw has a
provider API key (Anthropic, OpenAI, Gemini, etc.). Inject it as a Kubernetes
Secret and mount it into the container as an environment variable.

### Before first deployment (recommended)

Set the API key up before running the initial apply from the Usage section.

1. Create the Secret (Anthropic shown; use whichever provider you want):

```bash
kubectl create secret generic openclaw-provider-keys \
  --from-literal=ANTHROPIC_API_KEY="sk-ant-..."
```

2. Uncomment the `ANTHROPIC_API_KEY` env block in `openclaw-template.yaml`
   (placeholder is already there next to the existing env vars). For other
   providers, swap the var name (`OPENAI_API_KEY`, `GEMINI_API_KEY`, etc.) —
   the secret key name and env name should match.

3. Continue with the standard apply flow from the Usage section.

### Adding a key to an existing deployment (destructive)

**This deletes the PVC, so device pairing state and any workspace files are
lost.** You'll need to re-pair your browser afterwards.

The `SandboxClaim` controller snapshots the template's pod spec into the
`Sandbox` at adoption time and never re-syncs. Updating the template and
deleting the pod won't help — the Sandbox controller respawns from the
unchanged snapshot. Recreating just the claim isn't enough either: the new
claim can adopt a stale warm-pool spare built from the pre-update template.
You need to tear down claim + warmpool + template together, then redeploy.

1. Create or update the Secret with your API key:

```bash
kubectl create secret generic openclaw-provider-keys \
  --from-literal=ANTHROPIC_API_KEY="sk-ant-..." \
  --dry-run=client -o yaml | kubectl apply -f -
```

2. Uncomment the API key env block in `openclaw-template.yaml`.

3. Tear down claim, warmpool, and template (in that order):

```bash
kubectl delete -f openclaw-claim.yaml
kubectl delete -f openclaw-warmpool.yaml
kubectl delete -f openclaw-template.yaml
```

4. Redeploy with the updated template:

```bash
TOKEN="$(openssl rand -hex 32)"
sed "s/dummy-token-for-sandbox/${TOKEN}/g" openclaw-template.yaml | kubectl apply -f -
kubectl apply -f openclaw-warmpool.yaml
kubectl apply -f openclaw-claim.yaml
```

5. Wait for the new pod, retrieve the new gateway token, and re-pair your
   browser using the "Browser pairing / device authorization" section above:

```bash
SANDBOX_NAME=$(kubectl get sandboxclaim openclaw-sandbox-claim -o jsonpath='{.status.sandbox.name}')
POD=$(kubectl get sandbox "$SANDBOX_NAME" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
kubectl wait --for=condition=ready pod/"$POD" --timeout=180s
kubectl exec "$POD" -- printenv OPENCLAW_GATEWAY_TOKEN
```

To inject multiple provider keys at once, use `envFrom` instead:

```yaml
envFrom:
  - secretRef:
      name: openclaw-provider-keys
```

See [OpenClaw provider docs](https://docs.openclaw.ai/providers) for
the env-var name per provider.

## Persistence model

PVCs are named `<vctName>-<sandboxName>` and owned by the `Sandbox` CR. So:

- **Delete the pod** → controller respawns the pod, reattaches the same PVC →
  workspace data persists. The `run-test-kind.sh` persistence test exercises
  exactly this path.
- **Delete the `Sandbox`** (or the `SandboxClaim` with `shutdownPolicy: Delete`)
  → PVC is garbage-collected, data is gone.

Do not put `volumeClaimTemplates` on the `SandboxClaim`. A claim containing its own VCTs will bypass the pre-warmed pool entirely and trigger a cold start of a fresh sandbox, defeating the purpose of the warm pool.

## Known limitations

- **`kubectl port-forward` does not work under gVisor.** The application binds
  inside gVisor's user-space netstack; `kubectl port-forward` enters the host
  kernel's view of the pod's network namespace and finds nothing listening.
  Use a Service path (NodePort on kind, IAP tunnel on GKE) instead.
- **Exposure is environment-specific.** `kind-service.yaml` only works locally
  with the kind port mapping; `gke-service.yaml` provisions a public IP on GKE.