# OpenClaw Sandbox (gVisor + Template/Claim)

Runs [OpenClaw](https://github.com/openclaw/openclaw) inside the Agent Sandbox using the
`SandboxTemplate` + `SandboxWarmPool` + `SandboxClaim` pattern, under gVisor, with a
persistent workspace PVC and a NodePort for local access.

If you just want the minimal port-forward flow with a plain `Sandbox` CR, see
[`examples/openclaw-sandbox`](../openclaw-sandbox/). This directory is the
production-shaped variant.

## What you get

- `SandboxTemplate` with `runtimeClassName: gvisor`, an init container that seeds
  config from a ConfigMap, sshd inside the container, and a 2Gi PVC mounted at
  `/root/.openclaw`.
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

4. **Permissive namespace.** This template runs as root and starts sshd, so it
   will not pass the hardened policy in
   [`examples/policy/vap/secure-sandbox-policy.yaml`](../policy/vap/secure-sandbox-policy.yaml).
   Deploy into a namespace where that ValidatingAdmissionPolicy is not enforced.

## Usage

```bash
./run-test-kind.sh
```

The script pulls the image, loads it into kind, applies the manifests, waits for
the claim's pod to become ready, checks the gateway via `http://127.0.0.1:30789`,
and runs the persistence test.

To apply manually:

```bash
TOKEN="$(openssl rand -hex 32)"
kubectl apply -f openclaw-config.yaml
sed "s/dummy-token-for-sandbox/${TOKEN}/g" openclaw-template.yaml | kubectl apply -f -
kubectl apply -f openclaw-warmpool.yaml
kubectl apply -f openclaw-claim.yaml
kubectl apply -f kind-service.yaml          # or gke-service.yaml on GKE
```

On kind, the gateway is then reachable at `http://127.0.0.1:30789` (assuming
the port mapping above).

On GKE, apply `gke-service.yaml` instead and wait for an external IP:

```bash
kubectl get svc openclaw-gateway -w
```

Then browse to `http://<EXTERNAL-IP>:18789`.

<!-- > [!IMPORTANT]
> **GKE Secure Context / Port-forwarding Note:**
> gVisor's network namespace isolation prevents `kubectl port-forward` from working. Because of this, when accessing the gateway over plain HTTP using the public GKE LoadBalancer IP, your browser will treat the context as insecure and block the Web Crypto/Device Identity pairing APIs (yielding a `control ui requires device identity` error).
>
> To test this on GKE:
> 1. Get the secret token from your running pod:
>    `kubectl exec <POD_NAME> -- printenv OPENCLAW_GATEWAY_TOKEN`
> 2. Open Chrome and navigate to `chrome://flags/#unsafely-treat-insecure-origin-as-secure`.
> 3. Enable the flag and add your GKE IP address (e.g., `http://<EXTERNAL-IP>:18789`).
> 4. Relaunch Chrome and browse to `http://<EXTERNAL-IP>:18789/?token=<TOKEN>`. -->

The GKE node pool must be created with `--sandbox type=gvisor`. Pods using
`runtimeClassName: gvisor` will only schedule onto sandbox-enabled nodes — if
none exist, they'll stay `Pending` indefinitely. To add one to an existing
cluster:

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

## Persistence model

PVCs are named `<vctName>-<sandboxName>` and owned by the `Sandbox` CR. So:

- **Delete the pod** → controller respawns the pod, reattaches the same PVC →
  workspace data persists. The `run-test-kind.sh` persistence test exercises
  exactly this path.
- **Delete the `Sandbox`** (or the `SandboxClaim` with `shutdownPolicy: Delete`)
  → PVC is garbage-collected, data is gone.

Do not put `volumeClaimTemplates` on the `SandboxClaim`. Per
`extensions/controllers/sandboxclaim_controller.go:1491`, a claim with VCTs
bypasses the warm pool entirely and cold-starts a fresh sandbox.

<!-- ## Known limitations

- **Service selector is broad.** The Service targets all pods labeled
  `sandbox: openclaw-template-sandbox`, which includes both the claimed pod and
  any warm-pool replenishment pod. With `replicas: 1` and one claim this is
  usually fine, but production deployments should narrow the selector via
  `claim.spec.additionalPodMetadata.labels` (subject to the controller's
  `AllowedLabelDomains` allowlist).
- **Exposure is environment-specific.** `kind-service.yaml` (NodePort) only
  works locally with the kind port mapping above; `gke-service.yaml`
  (LoadBalancer) provisions a public IP on GKE and costs money while it's up.
  For production-shaped, multi-tenant exposure on gVisor (which breaks
  `kubectl port-forward` to a pod), use the sandbox-router pattern shown in
  [`../vscode-sandbox/scripts/apply.sh`](../vscode-sandbox/scripts/apply.sh)
  with [`clients/python/agentic-sandbox-client/sandbox-router`](../../clients/python/agentic-sandbox-client/sandbox-router/).
- **SSH access is in the container but not exposed.** sshd runs inside the pod
  but the Service only forwards 18789. Add a second port to the Service if you
  need SSH from outside. -->
