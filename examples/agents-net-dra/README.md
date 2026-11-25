# Policy Enforcement Point Egress with `dra.agents.net`

This example isolates `agent-sandbox` workloads (`Sandbox`, `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim`) and routes egress to a Policy Enforcement Point (PEP) using the [`dra.agents.net`](https://github.com/aojea/agents.net/tree/main/kubernetes) Kubernetes DRA + NRI driver. No changes to `agent-sandbox` CRDs or controllers are needed.

## How it works

`Sandbox` and `SandboxTemplate` expose a standard Kubernetes `PodSpec` at `spec.podTemplate.spec`. Workloads attach `dra.agents.net` by referencing a `ResourceClaimTemplate` in `podTemplate.spec.resourceClaims`:

```text
  Cluster Admin                            Namespace User
  DeviceClass: dra.agents.net              ResourceClaimTemplate: agents-net-egress
   (FromClass DriverConfig:                 (FromClaim DriverConfig:
    nicBlockMode, adapterMode,               policyEnforcementPoint, caTrust,
    proxy.port, limits)                      confinementMode, custom)
            |                                         |
            +--------------------+--------------------+
                                 v
        Sandbox / SandboxTemplate (podTemplate.spec.resourceClaims)
                                 |
        kube-controller-manager creates one Pod-owned ResourceClaim per Pod
        (deleted automatically on Sandbox suspend or delete)
                                 v
        Node DaemonSet: dra-agents-net (DRA + NRI)
         - PrepareResourceClaims: opens egress.sock, writes CA bundles
         - RunPodSandbox: isolates Pod netns, binds 127.0.0.1:15001, updates claim status
         - CreateContainer: mounts /run/agents.net and /var/run/agent-ca.pem, sets proxy/CA env
                                 |
        HTTP/2 or HTTP/1.1 CONNECT + attested Agents-Net-* headers
                                 v
        Policy Enforcement Point (PEP)
        (agents.net connect-proxy, Envoy, gke-labs/service-portals, corporate proxy)
```

Using a standard `ResourceClaimTemplate` has three practical properties:

- `kube-controller-manager` sets the Pod as the owner of each generated `ResourceClaim`. Suspending or deleting a `Sandbox` deletes the `ResourceClaim` and releases the node capacity unit; resuming creates a new claim.
- Every `SandboxWarmPool` replica gets its own `ResourceClaim`. When a `SandboxClaim` adopts a warm Pod, `dra-agents-net`'s node-scoped Pod informer picks up `agents.x-k8s.io/claim-uid` and `agents.x-k8s.io/claim-name` and forwards them to the PEP.
- The default `install.yaml` uses `adapterMode: ExplicitProxy` (no sidecar; proxy-aware clients use `127.0.0.1:15001` and clients that ignore `HTTP_PROXY` fail closed). For transparent TCP, UDP, and DNS interception of clients that ignore proxy env vars, configure `adapterMode: TunSidecar` with `agents-net-tun2connect` inside the Pod (see [`agents.net/kubernetes`](https://github.com/aojea/agents.net/tree/main/kubernetes)).

Under `confinementMode: NetworkPolicyDenyAll`, `dra-agents-net` flushes non-loopback routes and attaches `tc clsact` ingress/egress drop filters on every non-loopback NIC inside the Pod netns (`nicBlockMode: DropAll`, default in `DeviceClass`). On clusters with KEP-6313 Network-Isolated Pods, set `confinementMode: PodNetworkIsolated` instead.

## Prerequisites

1. Kubernetes 1.36+ (GA DRA `resource.k8s.io/v1` and default-enabled `DRAConsumableCapacity`) with containerd NRI enabled.
2. Standard `agent-sandbox` (core and extensions controllers).
3. `dra-agents-net` installed in `kube-system`:

   ```bash
   kubectl apply -f https://raw.githubusercontent.com/aojea/agents.net/main/kubernetes/install.yaml
   kubectl -n kube-system rollout status daemonset/dra-agents-net
   kubectl get deviceclass dra.agents.net
   kubectl get resourceslices --field-selector=spec.driver=dra.agents.net
   ```

4. A reachable PEP supporting HTTP `CONNECT` (for example `agents.net` `connect-proxy`, Envoy, `gke-labs/service-portals`, or a corporate proxy), its CA ConfigMap (`pep-ca`), and any custom workload CA Secret (`corp-ca-tls`) referenced in [manifests/01-resource-claim-template.yaml](manifests/01-resource-claim-template.yaml).

## Walkthrough

### 1. Create the namespace and `ResourceClaimTemplate`

Update `policyEnforcementPoint.url`, `tls`, `auth`, and `caTrust` in [manifests/01-resource-claim-template.yaml](manifests/01-resource-claim-template.yaml) for your PEP, then apply:

```bash
kubectl apply -f manifests/00-namespace.yaml
kubectl apply -f manifests/01-resource-claim-template.yaml
```

### 2. Deploy a `Sandbox`

Apply [manifests/02-sandbox.yaml](manifests/02-sandbox.yaml) and wait for readiness:

```bash
kubectl apply -f manifests/02-sandbox.yaml
kubectl -n agents-net-demo wait --for=condition=Ready sandbox/coding-agent --timeout=60s
kubectl -n agents-net-demo get sandbox,resourceclaim,pod
```

### 3. Check confinement and proxy env vars

```bash
# Proxy and CA env vars injected by NRI CreateContainer
kubectl -n agents-net-demo exec coding-agent -- sh -c \
  'env | grep -E "PROXY|AGENT|CA_CERT|SSL_CERT" | sort'

# Non-loopback routes are flushed
kubectl -n agents-net-demo exec coding-agent -- cat /proc/net/route

# Direct egress bypassing the proxy fails
kubectl -n agents-net-demo exec -i coding-agent -- python3 - <<'PY'
import os, socket

try:
    socket.create_connection(("1.1.1.1", 443), timeout=2).close()
    raise SystemExit("UNEXPECTED: direct egress succeeded")
except OSError as e:
    print("Direct egress blocked:", e)

print("HTTP_PROXY =", os.environ.get("HTTP_PROXY"))
print("SSL_CERT_FILE =", os.environ.get("SSL_CERT_FILE"))
PY
```

### 4. Check suspend and resume

Suspending deletes the backing Pod and its Pod-owned `ResourceClaim`. Resuming creates a fresh `ResourceClaim`:

```bash
kubectl -n agents-net-demo patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n agents-net-demo wait --for=delete pod/coding-agent --timeout=30s
kubectl -n agents-net-demo get resourceclaims

kubectl -n agents-net-demo patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n agents-net-demo wait --for=condition=Ready sandbox/coding-agent --timeout=60s
kubectl -n agents-net-demo get resourceclaims
```

### 5. Deploy a `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim`

Apply [manifests/03-warmpool.yaml](manifests/03-warmpool.yaml):

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
