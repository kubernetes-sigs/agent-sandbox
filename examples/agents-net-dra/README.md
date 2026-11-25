# Policy Enforcement Point Egress for Agent Sandbox via `dra.agents.net` (DRA + NRI)

This example shows how to isolate `agent-sandbox` workloads (`Sandbox`, `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim`) and forward all TCP, UDP, and DNS egress to an external **Policy Enforcement Point (PEP)** using the standalone [`dra.agents.net`](https://github.com/aojea/agents.net/tree/main/kubernetes) Kubernetes Network Driver — **with zero changes to `agent-sandbox` CRDs or controllers**.

## How It Works

Because `Sandbox` and `SandboxTemplate` already expose a standard Kubernetes `PodSpec` under `spec.podTemplate.spec`, workloads attach `dra.agents.net` directly via standard Kubernetes Dynamic Resource Allocation (`DeviceClass` + `ResourceClaimTemplate` or `ResourceClaim`):

```text
  Cluster Admin                                  Namespace User / Workload
  ─────────────                                  ─────────────────────────
  DeviceClass: dra.agents.net                    ResourceClaimTemplate: agents-net-egress
   (FromClass DriverConfig:                       (FromClaim DriverConfig:
    nicBlockMode: DropAll,                         policyEnforcementPoint.url + tls + auth,
    adapterMode: ExplicitProxy :15001,             caTrust, confinementMode, custom metadata)
    maxConnections, host CA/mTLS defaults)                      │
                      │                                         │
                      ▼                                         ▼
              Sandbox / SandboxTemplate (podTemplate.spec.resourceClaims[].resourceClaimTemplateName)
                                                │
                   kube-controller-manager creates one Pod-owned ResourceClaim per Pod
                   (automatically garbage-collected on Sandbox suspend or delete!)
                                                │
                                                ▼
              Node DaemonSet: dra-agents-net (DRA DRAConsumableCapacity + NRI + socketat)
               - PrepareResourceClaims: opens egress.sock, materializes CA bundles, patches status
               - RunPodSandbox: verifies loopback-only (KEP-6313) OR flushes routes + installs
                 pure-netlink tc clsact matchall DROP on every non-loopback NIC; binds 127.0.0.1:15001
               - CreateContainer: injects /run/agents.net, /var/run/agent-ca.pem, proxy + CA env vars
                                                │
                   HTTP/2 or HTTP/1.1 CONNECT + RFC 9298 connect-udp + attested Agents-Net-*
                                                ▼
              Policy Enforcement Point (PEP)
               (agents.net connect-proxy, gke-labs/service-portals, Envoy, or corporate gateway)
```

### Why Use Standard DRA `ResourceClaimTemplate` Instead of Custom Sandbox Fields?

- **Zero CRD or controller coupling:** `agent-sandbox` stays strictly focused on sandbox lifecycle while any Kubernetes Network Driver (DRA + NRI) plugs into `podTemplate.spec.resourceClaims`.
- **Automatic lifecycle & capacity release:** Because `kube-controller-manager` sets the backing `Pod` as the owner of each generated `ResourceClaim`, suspending or deleting a `Sandbox` automatically deletes the `ResourceClaim` and releases the node's `DRAConsumableCapacity` unit; resuming creates a fresh `ResourceClaim`.
- **Multi-replica `SandboxWarmPool` support out of the box:** Every warm replica created from a `SandboxTemplate` receives its own independent `ResourceClaim`, and `dra-agents-net`'s node-scoped Pod informer picks up `agents.x-k8s.io/claim-uid` and `agents.x-k8s.io/claim-name` automatically when a `SandboxClaim` adopts a warm Pod.
- **CNI-independent L2 isolation:** Under `NetworkPolicyDenyAll`, `dra-agents-net`'s default `nicBlockMode: DropAll` installs pure-netlink `tc clsact` `matchall` `DROP` filters (ingress and egress) on every non-loopback NIC inside the Pod netns — blocking all direct network traffic even on CNIs without `NetworkPolicy` enforcement (cluster admins can opt out in `DeviceClass` via `nicBlockMode: Disabled`; `FromClaim` cannot override it). On clusters with KEP-6313 Network-Isolated Pods, switch `confinementMode` to `PodNetworkIsolated` to eliminate CNI attachment altogether.

## Prerequisites

1. A Kubernetes **v1.36+** cluster (for GA DRA `resource.k8s.io/v1` and default-enabled `DRAConsumableCapacity`) with **containerd NRI** enabled.
2. Unmodified **`agent-sandbox`** (core + extensions controllers) installed.
3. **`dra-agents-net`** installed in `kube-system` from [`github.com/aojea/agents.net/kubernetes`](https://github.com/aojea/agents.net/tree/main/kubernetes):

   ```bash
   kubectl apply -f https://raw.githubusercontent.com/aojea/agents.net/main/kubernetes/install.yaml
   ```

   Verify the driver is healthy and advertising one shared `agents-net` device per node:

   ```bash
   kubectl -n kube-system rollout status daemonset/dra-agents-net
   kubectl get deviceclass dra.agents.net
   kubectl get resourceslices --field-selector=spec.driver=dra.agents.net
   ```

4. A reachable **Policy Enforcement Point (PEP)** supporting HTTP `CONNECT` (and optionally RFC 9298 `connect-udp`), such as `agents.net` `connect-proxy`, `gke-labs/service-portals`, Envoy, or a corporate proxy, along with its CA ConfigMap (`pep-ca`) and any custom workload CA Secret (`corp-ca-tls`) referenced in [manifests/01-resource-claim-template.yaml](manifests/01-resource-claim-template.yaml).

## Step-by-Step Walkthrough

### 1. Create the Namespace and `ResourceClaimTemplate`

Update `policyEnforcementPoint.url`, `tls`, `auth`, and `caTrust` in [manifests/01-resource-claim-template.yaml](manifests/01-resource-claim-template.yaml) for your PEP, then apply:

```bash
kubectl apply -f manifests/00-namespace.yaml
kubectl apply -f manifests/01-resource-claim-template.yaml
```

### 2. Deploy a Standalone `Sandbox`

Apply [manifests/02-sandbox.yaml](manifests/02-sandbox.yaml) and wait for readiness:

```bash
kubectl apply -f manifests/02-sandbox.yaml
kubectl -n agents-net-demo wait --for=condition=Ready sandbox/coding-agent --timeout=60s
```

Inspect the Pod-owned `ResourceClaim` generated automatically by `kube-controller-manager` and prepared by `dra-agents-net`:

```bash
kubectl -n agents-net-demo get sandbox,resourceclaim,pod
```

### 3. Verify Confinement and Proxied Egress

Run probes inside `coding-agent` (1 container, non-root, `automountServiceAccountToken: false`, all Linux capabilities dropped):

```bash
# 1. Inspect proxy and CA env vars injected by NRI CreateContainer
kubectl -n agents-net-demo exec coding-agent -- sh -c \
  'env | grep -E "PROXY|AGENT|CA_CERT|SSL_CERT" | sort'

# 2. Verify non-loopback routes were flushed
kubectl -n agents-net-demo exec coding-agent -- cat /proc/net/route

# 3. Verify direct egress bypassing the proxy is blocked, while HTTPS via HTTPS_PROXY reaches the PEP
kubectl -n agents-net-demo exec -i coding-agent -- python3 - <<'PY'
import os, socket, urllib.request

try:
    socket.create_connection(("1.1.1.1", 443), timeout=2).close()
    raise SystemExit("UNEXPECTED: direct egress succeeded")
except OSError as e:
    print("Direct egress blocked:", e)

print("HTTP_PROXY =", os.environ.get("HTTP_PROXY"))
print("SSL_CERT_FILE =", os.environ.get("SSL_CERT_FILE"))
PY
```

### 4. Verify Automatic Capacity Release on Suspend & Resume

Suspending the `Sandbox` deletes the backing Pod; Kubernetes GC immediately deletes the Pod-owned `ResourceClaim` and releases the node's `DRAConsumableCapacity` unit. Resuming creates a fresh `ResourceClaim`:

```bash
kubectl -n agents-net-demo patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n agents-net-demo wait --for=delete pod/coding-agent --timeout=30s
kubectl -n agents-net-demo get resourceclaims

kubectl -n agents-net-demo patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n agents-net-demo wait --for=condition=Ready sandbox/coding-agent --timeout=60s
kubectl -n agents-net-demo get resourceclaims
```

### 5. Deploy a `SandboxTemplate` + `SandboxWarmPool` + `SandboxClaim`

Apply [manifests/03-warmpool.yaml](manifests/03-warmpool.yaml) to pre-warm two confined Sandboxes and adopt one via `SandboxClaim`:

```bash
kubectl apply -f manifests/03-warmpool.yaml
kubectl -n agents-net-demo wait --for=condition=Ready sandboxclaim/coding-session --timeout=60s
kubectl -n agents-net-demo get sandboxwarmpool,sandboxclaim,sandbox,resourceclaim
```

## Cleanup

```bash
kubectl delete -f manifests/03-warmpool.yaml --ignore-not-found
kubectl delete -f manifests/02-sandbox.yaml --ignore-not-found
kubectl delete -f manifests/01-resource-claim-template.yaml --ignore-not-found
kubectl delete -f manifests/00-namespace.yaml --ignore-not-found
```
