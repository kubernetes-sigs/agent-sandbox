# Policy Enforcement Point Egress with `dra.agents.net` (`HostTun`)

This example isolates `agent-sandbox` workloads (`Sandbox`, `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim`) and routes egress to a mandatory mTLS Policy Enforcement Point (PEP) using [`dra.agents.net`](https://github.com/aojea/agents.net/tree/main/kubernetes) (`HostTun` default). No sidecar container, no Pod capabilities, no `HTTP_PROXY` configuration, and no changes to `agent-sandbox` CRDs or controllers are needed.

## How it works

`Sandbox` and `SandboxTemplate` expose a standard Kubernetes `PodSpec` at `spec.podTemplate.spec`. Workloads attach `dra.agents.net` by referencing a `ResourceClaimTemplate` in `podTemplate.spec.resourceClaims`:

```text
  Cluster Admin                                  Namespace User
  DeviceClass: dra.agents.net-demo               ResourceClaimTemplate: agents-net-egress
   (FromClass DriverConfig:                       (FromClaim DriverConfig:
    confinementMode, nicBlockMode,                 caTrust from ConfigMap/ClusterTrustBundle,
    adapterMode=HostTun, mandatory mTLS PEP,       optional ingressPorts)
    attested custom attributes, limits)
               |                                             |
               +----------------------+----------------------+
                                      v
            Sandbox / SandboxTemplate (podTemplate.spec.resourceClaims)
            (unprivileged container only: runAsNonRoot, drop ALL, no sidecar)
                                      |
            kube-controller-manager creates one Pod-owned ResourceClaim per Pod
            (deleted automatically when the Sandbox is suspended or deleted)
                                      v
            Node DaemonSet: dra-agents-net (DRA + NRI)
             - PrepareResourceClaims: opens egress.sock, writes CA bundle & resolv.conf
             - RunPodSandbox: isolates eth0, creates persistent tun0 in Pod netns,
               runs userspace tun2connect (gVisor TCP/IP + VirtualDNS 198.18.0.0/15) on host
             - CreateContainer: replaces /etc/resolv.conf -> 198.19.255.253, mounts CA bundle
                                      |
            Mandatory mTLS + HTTP/2 CONNECT & RFC 9298 connect-udp
            + node-attested Agents-Net-Principal / Generation / Attributes headers
                                      v
            Policy Enforcement Point (PEP)
            (agents.net connect-proxy, Envoy, gke-labs/service-portals, corporate proxy)
```

Using `HostTun` (the default `adapterMode`) with a standard `ResourceClaimTemplate` provides four practical properties:

- **Zero sidecar and zero Pod privileges:** `dra-agents-net` creates `tun0` inside the Pod network namespace during NRI `RunPodSandbox` and keeps the `/dev/net/tun` file descriptor on the host. The Sandbox Pod runs completely unprivileged (`runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `capabilities: {drop: ["ALL"]}`) with no sidecar container, no `/dev/net/tun` device, and no `egress.sock` mount.
- **Transparent TCP, UDP, and DNS:** NRI `CreateContainer` replaces `/etc/resolv.conf` with `nameserver 198.19.255.253` (avoiding Kubernetes `search`/`ndots:5` suffix hijacking). The host-side `VirtualDNS` allocates synthetic IPv4 addresses from RFC 2544 benchmarking space (`198.18.0.0/15`, avoiding collisions with CGNAT `100.64.0.0/10`) and IPv6 from RFC 6666 discard-only space (`100::/64`), failing closed locally on any unmapped synthetic address. Unmodified applications that ignore `HTTP_PROXY` work transparently over TCP and UDP.
- **Admin-enforced trust boundary:** `dra-agents-net` accepts `confinementMode`, `nicBlockMode`, `adapterMode`, `policyEnforcementPoint` (mandatory mTLS), `custom` attributes, and limits only from cluster-admin `DeviceClass` (`FromClass`). Namespace `ResourceClaimTemplate` objects can only layer `caTrust` (`ConfigMap` or `ClusterTrustBundle`), `ingressPorts`, and `ExplicitProxy` overrides.
- **Automatic lifecycle binding:** `kube-controller-manager` sets the Pod as owner of each `ResourceClaim`. Suspending or deleting a `Sandbox` deletes the `ResourceClaim` and releases the node slot; resuming creates a fresh claim. Each `SandboxWarmPool` replica receives its own `ResourceClaim`.

### Compatibility notes with `agent-sandbox`

- **`confinementMode: NetworkPolicyDenyAll` (required today):** `agent-sandbox` currently gates `Sandbox` `Ready=True` and `SandboxWarmPool` candidate adoption on a non-empty `status.podIPs`. `NetworkPolicyDenyAll` keeps a CNI Pod IP while isolating traffic, so unmodified `agent-sandbox` works today. (`PodNetworkIsolated` under KEP-6313 assigns no Pod IP and requires relaxing the `podIPs` readiness check in `agent-sandbox`.)
- **`sandbox-router` ingress:** Under `nicBlockMode: DropAll` (default), `dra-agents-net` flushes `eth0` routes and blocks `eth0` with `tc clsact` drop filters, which also blocks direct `sandbox-router` → `PodIP:8888` traffic (use `dra-agents-net`'s mTLS ingress bridge / `agents-net-ingress-gateway` instead). If you want `sandbox-router` and kubelet HTTP/TCP probes to keep using `PodIP` over `eth0`, set `nicBlockMode: Disabled` in the `DeviceClass` and enforce egress deny-all with a CNI `NetworkPolicy`; `HostTun` automatically installs source-IP policy routing so `PodIP` replies still exit `eth0`.

## Quickstart (automated kind demo)

Run [demo.sh](demo.sh) to spin up a disposable kind `v1.36.1` cluster with containerd NRI enabled, install `agent-sandbox` (core + extensions), install `dra-agents-net` with ephemeral mTLS PKI, deploy an in-cluster HTTPS/UDP echo target and `agents.net` `connect-proxy` PEP, and verify `Sandbox`, suspend/resume, and `SandboxWarmPool` + `SandboxClaim` end-to-end:

```bash
# Build dra-agents-net and connect-proxy from a local agents.net checkout:
AGENTS_NET_DIR=/path/to/agents.net ./demo.sh

# Or run against an existing Kubernetes 1.36+ cluster with NRI enabled:
USE_EXISTING_CLUSTER=true KUBE_CONTEXT=<context> AGENTS_NET_DIR=/path/to/agents.net ./demo.sh
```

Set `KEEP_CLUSTER=true` to keep the disposable kind cluster after the verification checks pass so you can inspect the resources interactively.

## Manual walkthrough

### Prerequisites

1. Kubernetes 1.36+ (GA DRA `resource.k8s.io/v1` and default-enabled `DRAConsumableCapacity`) with containerd NRI enabled.
2. `agent-sandbox` (core and extensions controllers).
3. `dra-agents-net` installed in `kube-system` with its node mTLS client credentials mounted at `/etc/dra-agents-net/client` and PEP CA certificate at `/etc/dra-agents-net/ca/ca.crt`:

   ```bash
   kubectl apply -f https://raw.githubusercontent.com/aojea/agents.net/main/kubernetes/install.yaml
   kubectl -n kube-system rollout status daemonset/dra-agents-net
   kubectl get resourceslices --field-selector=spec.driver=dra.agents.net
   ```

4. A reachable PEP supporting mTLS HTTP/2 or HTTP/1.1 `CONNECT` (for example `agents.net` `connect-proxy`, Envoy, `gke-labs/service-portals`, or a corporate proxy) and a `demo-ca` `ConfigMap` in `agents-net-demo` containing the trusted CA bundle.

### 1. Create the namespace, `DeviceClass`, and `ResourceClaimTemplate`

Update `policyEnforcementPoint.url`, `tls`, and `auth` in [manifests/01-device-class.yaml](manifests/01-device-class.yaml) for your PEP, then apply:

```bash
kubectl apply -f manifests/00-namespace.yaml
kubectl apply -f manifests/01-device-class.yaml
kubectl apply -f manifests/02-resource-claim-template.yaml
```

### 2. Deploy a `Sandbox`

Apply [manifests/03-sandbox.yaml](manifests/03-sandbox.yaml) and wait for readiness:

```bash
kubectl apply -f manifests/03-sandbox.yaml
kubectl -n agents-net-demo wait --for=condition=Ready sandbox/coding-agent --timeout=60s
kubectl -n agents-net-demo get sandbox,resourceclaim,pod
```

### 3. Verify `HostTun` confinement and transparent egress

```bash
# NRI replaces /etc/resolv.conf with the HostTun virtual DNS resolver (198.19.255.253)
kubectl -n agents-net-demo exec coding-agent -- cat /etc/resolv.conf

# All non-loopback routes point exclusively to tun0 (eth0 routes are flushed)
kubectl -n agents-net-demo exec coding-agent -- cat /proc/net/route

# Verify zero proxy env vars, transparent allowed egress, and fail-closed denials
kubectl -n agents-net-demo exec -i coding-agent -- python3 - <<'PY'
import os, socket, urllib.error, urllib.request

assert not os.environ.get("HTTP_PROXY") and not os.environ.get("HTTPS_PROXY")

with urllib.request.urlopen("https://echo.agents-net-demo.svc.cluster.local:8443/hello", timeout=10) as resp:
    print("Allowed HTTPS:", resp.status, resp.read().decode())

try:
    urllib.request.urlopen("https://blocked.example:443/", timeout=5)
    raise SystemExit("UNEXPECTED: blocked.example succeeded")
except (urllib.error.URLError, OSError) as e:
    print("Denied host blocked:", e)

try:
    socket.create_connection(("198.18.9.9", 443), timeout=5).close()
    raise SystemExit("UNEXPECTED: unmapped synthetic IP succeeded")
except OSError as e:
    print("Unmapped synthetic IP failed closed:", e)
PY
```

### 4. Verify suspend and resume

Suspending deletes the backing Pod and its Pod-owned `ResourceClaim`. Resuming creates a fresh `ResourceClaim` and re-attaches `HostTun`:

```bash
kubectl -n agents-net-demo patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n agents-net-demo wait --for=delete pod/coding-agent --timeout=30s
kubectl -n agents-net-demo get resourceclaims

kubectl -n agents-net-demo patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n agents-net-demo wait --for=condition=Ready sandbox/coding-agent --timeout=60s
kubectl -n agents-net-demo get resourceclaims
```

### 5. Deploy a `SandboxTemplate`, `SandboxWarmPool`, and `SandboxClaim`

Apply [manifests/04-warmpool.yaml](manifests/04-warmpool.yaml):

```bash
kubectl apply -f manifests/04-warmpool.yaml
kubectl -n agents-net-demo wait --for=condition=Ready sandboxclaim/coding-session --timeout=60s
kubectl -n agents-net-demo get sandboxwarmpool,sandboxclaim,sandbox,resourceclaim
```

## Cleanup

```bash
kubectl delete -f manifests/04-warmpool.yaml --ignore-not-found
kubectl delete -f manifests/03-sandbox.yaml --ignore-not-found
kubectl delete -f manifests/02-resource-claim-template.yaml --ignore-not-found
kubectl delete -f manifests/01-device-class.yaml --ignore-not-found
kubectl delete -f manifests/00-namespace.yaml --ignore-not-found
```
