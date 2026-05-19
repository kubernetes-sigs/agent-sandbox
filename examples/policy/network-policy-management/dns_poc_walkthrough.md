# GKE Sandbox NodeLocal DNS & NetworkPolicy Walkthrough

This walkthrough provides a step-by-step, reproducible Proof-of-Concept (PoC) to verify how GKE **NodeLocal DNSCache** (`169.254.20.10`) and internal cluster DNS function inside a sandboxed `runtimeClassName: gvisor` workload.

Use this guide to understand how strict "Secure-by-Default" network policies interact with GKE's local DNS, and how to verify your configuration.

---

## Goal
Verify that GKE NodeLocal DNSCache (`169.254.20.10`) is fully compatible with GKE Sandbox (gVisor), and demonstrate how to configure templates to allow internal DNS lookups without compromising security.

## Prerequisites
* A GKE cluster (Autopilot or Standard with Dataplane V2).
* NodeLocal DNSCache enabled in the cluster (`kubectl get pods -n kube-system -l k8s-app=node-local-dns` should show running pods).
* The Agent Sandbox controller installed (`agent-sandbox-system`).

---

## Scenario A: Default Egress Policy (Blocked)

By default, if a `SandboxTemplate` does not specify custom egress rules, the controller automatically applies the strict **Secure Default** policy. This default policy explicitly blocks the link-local range (`169.254.0.0/16`), which includes the NodeLocal DNSCache IP.

### 1. Apply the manifests
Create and apply a template and a claim that rely on secure defaults:

`dns-poc-template.yaml`:
```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: dns-poc-template
  namespace: default
spec:
  networkPolicyManagement: Managed
  podTemplate:
    metadata:
      labels:
        sandbox-type: debug-dns
    spec:
      runtimeClassName: gvisor
      containers:
      - name: debug-container
        image: alpine:3.20
        command: ["/bin/sh", "-c", "sleep 3600"]
```

`dns-poc-claim.yaml`:
```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: dns-poc-claim
  namespace: default
spec:
  sandboxTemplateRef:
    name: dns-poc-template
  warmpool: none
```

```bash
kubectl apply -f dns-poc-template.yaml
kubectl apply -f dns-poc-claim.yaml
```

### 2. Test DNS Resolution
Wait for the pod `dns-poc-claim` to become `Running`, and run an `nslookup` query targeting the local cache IP:

```bash
kubectl exec dns-poc-claim -n default -c debug-container -- nslookup google.com 169.254.20.10
```

**Expected Result**: 
The query **times out completely** because the outbound UDP/TCP packets are blocked by the CNI at the pod's egress interface:
```none
;; communications error to 169.254.20.10#53: timed out
```

---

## Scenario B: Bypassing to `kube-dns` VIP (Blocked)

To bypass the blocked link-local cache, you might try to point your pod's DNS resolvers to GKE's standard `kube-dns` Service VIP (e.g., `10.96.0.10` or similar).

### 1. Find your standard `kube-dns` Service IP
```bash
kubectl get svc -n kube-system kube-dns
```

### 2. Test the standard Service IP
Run `nslookup` targeting your `kube-dns` ClusterIP:

```bash
kubectl exec dns-poc-claim -n default -c debug-container -- nslookup google.com <KUBE_DNS_SERVICE_IP>
```

**Expected Result**: 
The query **times out**. 

**Why?** GKE translates (DNAT) the Service VIP to the actual `kube-dns` pod IP, which resides inside the cluster network. Because your secure default egress rules explicitly block all RFC1918 private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), the CNI drops the outbound query at the egress hook.

---

## Scenario C: Custom unblocked Egress (Succeeded!)

To allow GKE Sandbox pods to resolve names natively using the high-performance NodeLocal DNS Cache, we must customize the template's `networkPolicy` to explicitly allow egress to the NodeLocal DNSCache IP (`169.254.20.10/32`) on UDP/TCP port 53, while keeping the rest of the link-local range securely blocked.

### 1. Apply the unblocked manifests
Create and apply a template that blocks the general link-local range but adds a narrowly scoped rule for NodeLocal DNSCache:

`dns-autopilot-template.yaml`:
```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: dns-autopilot-template
  namespace: default
spec:
  networkPolicyManagement: Managed
  networkPolicy:
    ingress:
      - from:
        - podSelector:
            matchLabels:
              app: sandbox-router
    egress:
      # 1. Allow outbound IPv4 to the public internet (excluding private subnets and link-local)
      - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 10.0.0.0/8
              - 172.16.0.0/12
              - 192.168.0.0/16
              - 169.254.0.0/16
      # 2. Allow outbound IPv6
      - to:
        - ipBlock:
            cidr: "::/0"
            except:
              - "fc00::/7"
      # 3. Allow narrow egress to GKE NodeLocal DNSCache specifically (UDP/TCP 53)
      - to:
        - ipBlock:
            cidr: 169.254.20.10/32
        ports:
          - protocol: UDP
            port: 53
          - protocol: TCP
            port: 53
  podTemplate:
    metadata:
      labels:
        sandbox-type: autopilot-dns
    spec:
      runtimeClassName: gvisor
      containers:
      - name: debug-container
        image: alpine:3.20
        command: ["/bin/sh", "-c", "sleep 3600"]
```

`dns-autopilot-claim.yaml`:
```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: dns-autopilot-claim
  namespace: default
spec:
  sandboxTemplateRef:
    name: dns-autopilot-template
  warmpool: none
```

```bash
kubectl apply -f dns-autopilot-template.yaml
kubectl apply -f dns-autopilot-claim.yaml
```

### 2. Test DNS Resolution
Stream the package installation logs and run a `dig` query targeting GKE's local cache IP:

```bash
# Install DNS utilities inside the running Alpine pod
kubectl exec dns-autopilot-claim -n default -c debug-container -- apk add bind-tools

# Execute the query
kubectl exec dns-autopilot-claim -n default -c debug-container -- dig @169.254.20.10 google.com
```

**Expected Result**: 
The query **succeeds natively and instantly (typically ~10ms)**!

```none
;; QUESTION SECTION:
;google.com.                    IN      A

;; ANSWER SECTION:
google.com.             30      IN      A       142.251.189.102
google.com.             30      IN      A       142.251.189.100

;; Query time: 10 msec
;; SERVER: 169.254.20.10#53(169.254.20.10) (UDP)
```

## How the Network Architecture Handles This Under the Hood

It is a common misconception that GKE's strict "default-deny" ingress NetworkPolicy blocks the returning UDP/TCP DNS responses. In reality, GKE's Dataplane V2 (Cilium) uses stateful connection tracking (`conntrack`), meaning allowed outbound queries automatically permit returning responses.

Instead, the root cause lies in **gVisor's isolated network namespace** and how it interacts with GKE's eBPF-based DNS redirection:

### 1. The Standard GKE Path (Socket-Level Interception)
In standard GKE pods (without gVisor), DNS queries targeting `169.254.20.10` are intercepted using **host-level eBPF socket hooks (`cgroup/connect`)**. 

When a standard container creates a socket targeting `169.254.20.10`, the eBPF program intercepts the syscall in-memory and redirects the connection. This occurs *before* any L3 network packets are assembled, completely **bypassing the virtual network interface (`eth0`) and any egress NetworkPolicy rules**.

### 2. The gVisor Path (L3 Packet Interception)
Because gVisor (`runsc`) runs its own isolated, virtualized user-space network stack (`gonet`), the host kernel's socket-level eBPF hooks **cannot see or hook the socket creation inside the sandbox**.

As a result:
* gVisor is forced to assemble the DNS query as a standard **L3 network packet** and send it out of `eth0`.
* The moment the packet leaves `eth0`, it becomes subject to the pod's egress `NetworkPolicy` rules.
* If the `networkPolicy` blocks `169.254.0.0/16` (which the default Secure Policy does), the CNI drops the packet at the egress filter.
* If you explicitly permit egress to `169.254.20.10/32` on port 53 in your custom `networkPolicy.egress` list, the packet safely traverses `eth0` into the host namespace, where GKE's host-level routing tables intercept and direct it to GKE's local DNS cache container.

