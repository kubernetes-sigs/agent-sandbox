# Managed Outbound Egress for Sandboxes on GKE Dataplane v2

This example shows how to give Agent Sandboxes **managed outbound internet access** on a GKE Dataplane v2 cluster: a hard default-deny floor, configurable IP/CIDR and FQDN allowlists, and a forward-proxy "egress gateway" that holds upstream credentials so the sandbox itself never sees them. The goal is to let agents reach the dependencies and external APIs they actually need (PyPI, model APIs, internal data stores) while preventing exfiltration to arbitrary destinations.

GKE Dataplane v2 is implemented on top of Cilium, but **GKE's managed Cilium intentionally does not expose all of upstream Cilium's surface.** Section §1.1 spells out what is and isn't available. This example uses the supported primitives end-to-end.

## 1. Overview

Three layers, each with a single responsibility:

1. **Default-deny floor** — without an allow rule, the sandbox has zero egress. This is the fail-closed property CUJ SAND-EGRESS-1 requires. Two manifests, pick one: a standard `NetworkPolicy` per namespace (no flag, works on every Dataplane v2 cluster) or a `CiliumClusterwideNetworkPolicy` (cluster-wide, requires a cluster flag).
2. **Allowlists** — per-template `NetworkPolicy` for internal IP/CIDR + port destinations, `FQDNNetworkPolicy` for external hostnames. Multiple policies union, so you can mix and match.
3. **Egress gateway** — a forward-proxy Deployment (Squid) that the sandbox routes HTTP(S) traffic through via `HTTPS_PROXY`. The proxy holds the upstream credentials via GKE Workload Identity, so the sandbox itself never carries a credential. This is the OSS analog of the "Agent Gateway" pattern in CUJ SAND-EGRESS-2 / EGRESS-3 / AUTH-1.

The forward-proxy variant of the gateway (rather than `CiliumEgressGatewayPolicy`) is required on GKE Dataplane v2 — see §1.1.

### 1.1. What GKE's managed Cilium does NOT expose

GKE Dataplane v2 ships a managed Cilium that is locked down compared to upstream Cilium. Understanding the limits up front prevents a lot of wasted YAML:

| Upstream Cilium feature | Available on GKE DPv2? |
| --- | --- |
| Standard Kubernetes `NetworkPolicy` (L3/L4) | Yes — always on, regardless of the `networkPolicy.enabled` toggle (that's the legacy Calico flag). |
| `CiliumNetworkPolicy` (L3/L4) | Yes, but the CRD is only installed when you opt into `--enable-cilium-clusterwide-network-policy`. For pure L3/L4, standard `NetworkPolicy` does the same job with no flag. |
| `CiliumClusterwideNetworkPolicy` | Yes, requires `--enable-cilium-clusterwide-network-policy`. Capped at 1000 policies per cluster. **GKE Warden rejects any CCNP with L7 rules or node selectors. GKE also requires `spec.ingress` to be present** (upstream Cilium makes it optional) — see [default-deny-egress.yaml](default-deny-egress.yaml) for the GKE-shaped pattern. |
| `CiliumNetworkPolicy.toFQDNs` | Not the supported FQDN path on GKE. Use `FQDNNetworkPolicy` (`networking.gke.io/v1alpha1`) instead — see [allowlist-fqdn.yaml](allowlist-fqdn.yaml). |
| `CiliumNetworkPolicy` L7 HTTP rules (`rules.http`) | **Not supported.** GKE blocks L7 application-data filtering in CNP. Path/method filtering belongs in the forward proxy. |
| `CiliumEgressGatewayPolicy` | **Not supported at all.** This CRD only exists on self-managed Cilium, which conflicts with Dataplane v2 (mutually exclusive). The "named gateway" pattern on GKE uses a Deployment + Service that pods route to via `HTTPS_PROXY` — see [egress-gateway.yaml](egress-gateway.yaml). |

This example uses each tool for what it's actually good at on GKE: standard `NetworkPolicy` for L3/L4, `FQDNNetworkPolicy` for FQDN allowlists, a forward proxy for L7 path filtering and per-request credential injection.

## 2. Prerequisites

- **GKE cluster with Dataplane v2 + CCNP + FQDNNetworkPolicy enabled.** Concrete commands used to validate this example:

  ```sh
  # GKE Standard. --enable-dataplane-v2 selects Dataplane v2 (Cilium-based).
  # CCNP and FQDNNetworkPolicy are both opt-in flags on Standard.
  gcloud container clusters create cilium-poc-std \
    --project=<PROJECT_ID> \
    --zone="us-central1-a" \
    --enable-ip-alias \
    --scopes="https://www.googleapis.com/auth/cloud-platform" \
    --num-nodes=1 \
    --machine-type="e2-standard-2" \
    --enable-cilium-clusterwide-network-policy \
    --enable-fqdn-network-policy \
    --enable-dataplane-v2

  # GKE Autopilot. Most Standard flags drop out (Autopilot manages nodes,
  # IP aliasing, machine types, scopes itself). Dataplane v2 is the default
  # and --enable-dataplane-v2 is REJECTED. CCNP still needs the opt-in flag;
  # FQDNNetworkPolicy may be on or off depending on cluster version, so
  # passing the flag explicitly is the safest bet.
  gcloud container clusters create-auto cilium-poc-autopilot \
    --project=<PROJECT_ID> \
    --region="us-central1" \
    --enable-cilium-clusterwide-network-policy \
    --enable-fqdn-network-policy

  # Verify the CRDs landed (both clusters):
  kubectl api-resources | grep -iE 'cilium|fqdn'
  # expect: ciliumclusterwidenetworkpolicies, fqdnnetworkpolicies
  ```

  For existing clusters, the same flags work as `gcloud container clusters update` arguments to add CCNP / FQDNNetworkPolicy after the fact.

- **agent-sandbox controller installed.** See the repo [README](../../../README.md) for installation. The extensions controllers (`SandboxTemplate`, `SandboxClaim`) must be installed.

- **GKE Workload Identity** enabled on the cluster if you intend to use the egress-gateway pattern in §6. The gateway Pod's KSA must be bound to a GSA that upstream APIs can IAM-allowlist.

- **A demo namespace.** `kubectl create namespace agent-sandbox-demo`. The egress-gateway manifest also creates `agent-sandbox-egress`.

- **NodeLocal DNSCache caveat** (GKE Autopilot in particular). On clusters that use NodeLocal DNSCache, the pod's `/etc/resolv.conf` `nameserver` is the link-local address `169.254.20.10`, not the kube-dns Service IP. Any `NetworkPolicy` with `policyTypes: [Egress]` selecting sandbox pods must explicitly allow egress to that ipBlock or DNS resolution will silently fail with `Could not resolve proxy`:

  ```yaml
  - to:
      - ipBlock:
          cidr: 169.254.20.10/32
    ports:
      - protocol: UDP
        port: 53
      - protocol: TCP
        port: 53
  ```

  The shipped `egress-gateway.yaml` already includes this rule; if you add your own NetworkPolicies that select sandbox pods, include it there too. NodeLocal DNS is on by default on Autopilot and opt-in (`--addons NodeLocalDNS`) on Standard — when in doubt, check `kubectl exec ... -- cat /etc/resolv.conf` from inside a sandbox pod.

- **Build and push the gateway image.** The egress gateway runs a custom Squid image (built from [gateway/Dockerfile](gateway/)) — see §7.5 for why Canonical's `ubuntu/squid` won't work. One-time build:

  ```sh
  IMAGE=gcr.io/PROJECT_ID/agent-sandbox-egress-gateway:1
  docker build -t "$IMAGE" examples/policy/cilium/gateway/
  docker push "$IMAGE"

  # Then point egress-gateway.yaml at $IMAGE (replace gcr.io/PROJECT_ID/...).
  ```

- **Create the gateway CA** (Secret + ConfigMap). **Required before applying `egress-gateway.yaml`** — the gateway pods mount these and will not start without them.

  *Why two objects holding the same CA?* Squid uses ssl_bump to terminate TLS for destinations in `bump_targets` (e.g., `httpbin.org`) so it can see inside the HTTPS request — this is what enables `Authorization`-header injection on HTTPS (AUTH-1 HTTPS half) and `urlpath_regex` rules on HTTPS (EGRESS-4c HTTPS half). To do this without TLS errors in the sandbox, Squid signs a per-destination cert with our CA's **private key**, and the sandbox needs to trust our CA's **public cert**. Two pieces, two access scopes:

  | Object | Holds | Mounted into |
  | --- | --- | --- |
  | `Secret/egress-gateway-ca` (kubernetes.io/tls) | `tls.crt` + `tls.key` | Gateway pod only (Squid needs the private key to sign) |
  | `ConfigMap/gateway-ca-bundle` | `ca.crt` only | Sandbox pods only (just need the public cert to validate signed leafs) |

  The CA private key is the crown jewel — anyone who has it can impersonate any TLS site to any sandbox. Keeping it in a Secret limits exposure to the gateway Deployment. The ConfigMap (no key) is safe to mount everywhere.

  See §7.5 for security implications of TLS interception and how to rotate the CA. If you don't need ssl_bump at all (plaintext-only injection, no HTTPS path filtering), you can skip the Secret and the bump-related config in `egress-gateway.yaml` — but you give up the HTTPS halves of AUTH-1 and EGRESS-4c.

  One-time per cluster:

  ```sh
  # Generate a self-signed CA — valid 10 years.
  openssl req -x509 -newkey rsa:4096 -nodes \
    -keyout /tmp/gateway-ca.key -out /tmp/gateway-ca.crt \
    -subj "/CN=agent-sandbox egress gateway CA" -days 3650

  # Make sure the namespaces exist.
  kubectl create namespace agent-sandbox-egress --dry-run=client -o yaml | kubectl apply -f -
  kubectl create namespace agent-sandbox-demo   --dry-run=client -o yaml | kubectl apply -f -

  # Secret for Squid (full CA — cert + key) so it can sign per-destination certs.
  kubectl create secret tls egress-gateway-ca \
    -n agent-sandbox-egress \
    --cert=/tmp/gateway-ca.crt --key=/tmp/gateway-ca.key \
    --dry-run=client -o yaml | kubectl apply -f -

  # ConfigMap for sandboxes (cert only) so they can trust bumped TLS.
  kubectl create configmap gateway-ca-bundle \
    -n agent-sandbox-demo \
    --from-file=ca.crt=/tmp/gateway-ca.crt \
    --dry-run=client -o yaml | kubectl apply -f -

  shred -u /tmp/gateway-ca.key /tmp/gateway-ca.crt
  ```

  See §7.5 for the rationale (why this is admin-created out-of-band, security considerations, rotation).

## 3. CUJ coverage

The CUJs that motivate this example are summarized below:

| CUJ | Primitive | Manifest | Coverage |
| --- | --- | --- | --- |
| SAND-EGRESS-1 — default block |  `NetworkPolicy` (no flag) **or** `CiliumClusterwideNetworkPolicy` (flag) | [default-deny-egress-netpol.yaml](default-deny-egress-netpol.yaml), [default-deny-egress.yaml](default-deny-egress.yaml) | Full |
| SAND-EGRESS-2 — named gateway routing, fail-closed |  Forward-proxy Deployment + Service; default-deny ensures fail-closed | [egress-gateway.yaml](egress-gateway.yaml) | **Static routing half: full.** **Dynamic per-claim selection half (`USE_CALLER_GATEWAY` / `GATEWAY_PATH`): not solvable by Cilium alone — needs a SandboxClaim API addition + admission webhook. See §9.** |
| SAND-EGRESS-3 — IAM-bound by Agent Identity |  Gateway pod via GKE Workload Identity → IAM allowlist on the GSA | [egress-gateway.yaml](egress-gateway.yaml) | **Coarse half: full** — every sandbox routed through the gateway shares the gateway's GSA, and upstream APIs can IAM-allowlist that one identity. **Per-agent identity half: not solvable by Cilium alone — needs per-sandbox identity provisioning + a delegate-gateway pattern + IAM check at egress. See §9.** |
| SAND-EGRESS-4a — IP/subnet + port | Standard `NetworkPolicy.egress` | [allowlist-cidr.yaml](allowlist-cidr.yaml) | Full |
| SAND-EGRESS-4b — FQDN with wildcards | `FQDNNetworkPolicy` (`networking.gke.io/v1alpha1`) | [allowlist-fqdn.yaml](allowlist-fqdn.yaml) | Full within FQDNNetworkPolicy limits (≤50 IPs per policy, no CNAME, no ClusterIP/Headless) |
| SAND-EGRESS-4c — URL / path filtering |  Squid `urlpath_regex` ACLs — fire on plaintext HTTP, and on bumped HTTPS via `ssl_bump` (TLS interception, see §7.5) | [egress-gateway.yaml](egress-gateway.yaml) | Full for traffic routed through the proxy. HTTPS targets must be in the `bump_targets` ACL; spliced targets remain opaque to path filtering. |
| SAND-EGRESS-5 — org policy disallowing public egress |  Admission control — out of scope of Cilium itself; lives in [`examples/policy/vap/restrict-template-egress-policy.yaml`](../vap/restrict-template-egress-policy.yaml) (and binding). Rejects `SandboxTemplate` whose `networkPolicy.egress` allows `0.0.0.0/0` or `::/0`. Namespace-opt-in via label. Verifiable via [`../vap/restrict-template-egress-test.yaml`](../vap/restrict-template-egress-test.yaml). | [`../vap/`](../vap/) | **Verified.** Complete for the standard tenant model (tenants create `SandboxClaim` / `SandboxTemplate`, not arbitrary policies). For self-service environments where tenants can also create raw NetworkPolicies, CNPs, or FQDNNetworkPolicies, additional sibling VAPs are defense-in-depth — see §9. |
| SAND-AUTH-1 — creds bound to identity, not in sandbox |  Forward-proxy holds creds via GKE Workload Identity (GCP-OAuth upstreams) plus a Kubernetes Secret mounted only on the gateway initContainer (static-bearer upstreams); sandbox uses `automountServiceAccountToken: false` (the agent-sandbox default); Squid `request_header_add` injects bearer tokens — on plaintext HTTP always, and on HTTPS destinations matched by `bump_targets` (see §7.5) | [egress-gateway.yaml](egress-gateway.yaml), the SandboxTemplate files | Secret-isolation half: shipped and verifiable. Per-request injection: full for plaintext HTTP and for bumped HTTPS destinations; spliced HTTPS destinations remain end-to-end TLS (no injection possible by design). |

## 4. Composition modes

The `SandboxTemplate.spec.networkPolicyManagement` field decides who owns the Kubernetes-native `NetworkPolicy` for sandboxes from this template. Both modes work with the other resources in this directory — but how you should *shape* the built-in NetworkPolicy depends on which mode you pick, and getting this wrong is the most common foot-gun in this layering. Read this section before editing either SandboxTemplate file.

### How NetworkPolicies compose

**Allows are union'd, not intersected.** Both standard K8s `NetworkPolicy` and `CiliumNetworkPolicy` stack additively — if any selecting policy allows traffic, the traffic is allowed. There is **no DENY rule type in vanilla K8s NetworkPolicy.** That means:

- Layering a "more restrictive" policy on top of a permissive one does **not** restrict the pod. It either adds (if the new allow extends the union) or is redundant (if it's a subset of an existing allow).
- A "defense-in-depth" stack where one layer is permissive and another is restrictive collapses to the permissive one — the union dominates.

For real subtraction-style restriction at the policy layer you need either (a) a single tight policy that's the intersection of intents, or (b) Cilium-specific `egressDeny` rules in a `CiliumClusterwideNetworkPolicy`, which **do** take precedence over allow rules across all policies.

### Unmanaged

The agent-sandbox controller creates no `NetworkPolicy`. All isolation comes from the resources in this directory: the default-deny floor + per-template allowlists + the forward proxy. One policy system to reason about, no layering surprises. If any of these policies is deleted or misapplied, the floor goes with them — that's the trade-off vs. a controller-managed baseline.

See [sandboxtemplate-cilium-only.yaml](sandboxtemplate-cilium-only.yaml).

### Defense in depth (`Managed`)

The agent-sandbox controller generates `my-agent-sandbox-network-policy` in the namespace. **For this mode to actually be defense-in-depth, that built-in policy must be the TIGHT floor**, not a permissive one — every allow it grants ends up in the union with whatever you layer on top, and nothing layered on top can take it back.

The shipped [sandboxtemplate-defense-in-depth.yaml](sandboxtemplate-defense-in-depth.yaml) configures the built-in NetworkPolicy to permit only:

- egress to **cluster DNS** (kube-dns) — needed for any name resolution
- egress to the **forward proxy service** (`egress-gateway.agent-sandbox-egress.svc:3128`) — sandboxes route HTTP(S) here via `HTTPS_PROXY`
- ingress from the **sandbox-router** — the user-facing entry point

That's it. No `0.0.0.0/0`. With this floor, **adding** more specific allows on top via `allowlist-fqdn.yaml` / `allowlist-cidr.yaml` actually expands what the sandbox can reach (because the union widens) — which is exactly the "controlled allow-list extension" model that makes sense for defense-in-depth. The agent-sandbox-managed policy survives even if the per-template policies are deleted; the per-template policies open holes deliberately.

**Common mistake to avoid:** configuring `Managed` with a permissive built-in (`spec.networkPolicy.egress: 0.0.0.0/0 except RFC1918`) and assuming Cilium/FQDN policies layered on top will restrict it. They won't — the union always wins. Use Unmanaged if you want Cilium to be authoritative, or use the tight-built-in pattern above. Don't mix.

**Cilium `egressDeny` is the escape hatch** if you genuinely need subtraction. A `CiliumClusterwideNetworkPolicy` with `egressDeny` rules can carve specific destinations out of any policy's allows, including the built-in. Useful for things like "no matter what any other policy says, never let sandboxes reach `169.254.169.254`." Vanilla K8s NetworkPolicy can't do this.

### Picking a mode

- **Cilium-only (Unmanaged)** is the right default if you're using the resources in this directory as your isolation story. Simpler to reason about, no overlap concerns.
- **Managed with tight built-in** is right if you want a controller-managed baseline that survives misconfigurations elsewhere — at the cost of slightly more YAML to maintain (the built-in policy spec lives in the SandboxTemplate itself).
- **Managed with permissive built-in is wrong** for this example's layering pattern. Use one of the two above instead.

## 5. The manifests

Apply in this order. All commands assume the `agent-sandbox-demo` namespace exists.

| Order | File | What it does |
| --- | --- | --- |
| 1 | [default-deny-egress-netpol.yaml](default-deny-egress-netpol.yaml) *or* [default-deny-egress.yaml](default-deny-egress.yaml) | Default-deny floor. **NetPol variant**: per-namespace, egress-only, no flag — recommended for first-time testing. **CCNP variant**: cluster-wide, bi-directional (allows sandbox-router ingress and cluster DNS egress by default; everything else denied). Requires `--enable-cilium-clusterwide-network-policy`. The CCNP variant is bi-directional because GKE's CCNP schema requires `spec.ingress`; the egress-only CCNP shape that works on upstream Cilium is not accepted by GKE. |
| 2 | [egress-gateway.yaml](egress-gateway.yaml) | Creates `agent-sandbox-egress` namespace, the Squid Deployment + Service + ConfigMap, the GSA-bound ServiceAccount, and the NetworkPolicies allowing sandbox→gateway and ingress filtering on the gateway. Apply before sandboxes so their `HTTPS_PROXY` resolves. **Requires (a) the custom gateway image built from [gateway/Dockerfile](gateway/) and pushed to a registry, see §2 prerequisites; (b) the `egress-gateway-ca` Secret AND the `gateway-ca-bundle` ConfigMap to exist first — see §7.5 for the openssl + kubectl commands.** |
| 3 | [sandboxtemplate-cilium-only.yaml](sandboxtemplate-cilium-only.yaml) *or* [sandboxtemplate-defense-in-depth.yaml](sandboxtemplate-defense-in-depth.yaml) | Bundles a SandboxTemplate, a SandboxWarmPool that references it, and a sample SandboxClaim that adopts from the pool. Pick one mode. Both set `HTTPS_PROXY` on the sandbox container and set the `app: my-agent-sandbox` label at template level (not on the claim) so warm-pool pods carry it before adoption. |
| 4 | [allowlist-fqdn.yaml](allowlist-fqdn.yaml) | FQDN allowlist for external destinations (PyPI, model APIs, googleapis). Requires `--enable-fqdn-network-policy`. |
| 5 | [allowlist-cidr.yaml](allowlist-cidr.yaml) | CIDR + port allowlist for internal services. Ships pointing at the `echo` deploy from `test-targets.yaml`; swap the selector for your real internal destinations. |
| 6 (optional) | [test-targets.yaml](test-targets.yaml) | Tiny `traefik/whoami` echo server used by the verify steps in §6 and §7. Not part of the production pattern — apply only when running the verification. |

If your agents only call external APIs through the proxy and don't talk to internal services directly, you can skip step 5. If you don't need external HTTPS at all, you can skip step 4. Skip step 6 unless you're running through the verify steps.

## 6. Verify

Once a sandbox pod is running, exec into it. Substitute your sandbox pod name (the agent-sandbox controller names pods after the claim).

```sh
SANDBOX=$(kubectl get pod -n agent-sandbox-demo -l app=my-agent-sandbox -o jsonpath='{.items[0].metadata.name}')
```

**Default-deny floor.** Apply only the default-deny manifest and the SandboxTemplate (no allowlists, no gateway). The sandbox should not reach anything:

```sh
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- curl --max-time 5 https://www.google.com
# expect: connect timeout
```

**Egress gateway routing.** After applying [egress-gateway.yaml](egress-gateway.yaml) and the SandboxTemplate (which sets `HTTPS_PROXY`):

```sh
# Allowed by the Squid ACL — succeeds.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- curl -sI https://pypi.org/simple/requests/
# expect: HTTP/2 200

# Not in the Squid ACL — proxy returns 403.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- curl -sI https://www.facebook.com
# expect: HTTP/1.1 403 Forbidden  (from Squid, not from upstream)

# Bypass attempt: direct connection to an arbitrary IP — blocked by default-deny.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 https://1.1.1.1
# expect: connect timeout
```

**FQDN allowlist (direct, no proxy).** Apply [allowlist-fqdn.yaml](allowlist-fqdn.yaml). FQDNNetworkPolicy filters *connections to resolved IPs*, not DNS lookups themselves — so blocked FQDNs fail at connect time, not at DNS:

```sh
# Allowed FQDN — request reaches upstream. The exact status code depends on
# the API (here, OpenAI returns 421 for unauthenticated HEAD /), what matters
# is that TLS+HTTP got through.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 10 -sI https://api.openai.com
# expect: HTTP/2 <some status>

# Un-allowed FQDN — connect timeout. -v shows curl resolved the DNS but
# couldn't connect to the resolved IP.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 -sIv https://www.facebook.com 2>&1 | head -20
# expect: "Trying <IP>:443..." then "Connection timed out"
```

**Forward proxy ACL (via Squid).** With `HTTPS_PROXY` set (default in the SandboxTemplate), every external request goes through Squid:

```sh
# Allowed by Squid's allowed_hosts.txt — Squid CONNECT tunnels to upstream.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- curl --max-time 10 -sI https://pypi.org/simple/requests/
# expect: HTTP/1.1 200 Connection established  (the Squid tunnel)
# expect: HTTP/2 200                            (the upstream response)

# Denied by Squid's allowed_hosts.txt. With ssl_bump enabled (this example),
# Squid first returns "200 Connection established" to allow the TLS
# ClientHello to come through so it can peek at the SNI; THEN it evaluates
# the ACL with the SNI and silently closes the tunnel on deny. curl reports
# the TLS handshake failure (exit 35), not a 403.
#
# Either of these two outcomes is correct — they both mean denied:
#   Outcome A (legacy, no ssl_bump): HTTP/1.1 403 Forbidden + curl exits 22ish
#   Outcome B (current, with ssl_bump): HTTP/1.1 200 Connection established
#     followed by curl exit 35 ("SSL connect error")
#
# The authoritative signal is the gateway's access log — look for TCP_DENIED.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- curl --max-time 10 -sI https://www.facebook.com
# expect (with ssl_bump): "HTTP/1.1 200 Connection established" then exit 35
# Then check the log on whichever gateway pod served the request:
#   for POD in $(kubectl get pod -n agent-sandbox-egress -l app=egress-gateway -o jsonpath='{.items[*].metadata.name}'); do
#     kubectl exec -n agent-sandbox-egress "$POD" -- tail -5 /var/log/squid/access.log | grep facebook || true
#   done
# expect: TCP_DENIED/... CONNECT www.facebook.com:443 - HIER_NONE/-
```

**CIDR + port allowlist (SAND-EGRESS-4a).** First deploy the verify target:

```sh
kubectl apply -f examples/policy/cilium/test-targets.yaml
kubectl rollout status -n agent-sandbox-demo deploy/echo
```

Then exec into the sandbox and try direct (no-proxy) HTTP to the echo Service. With [allowlist-cidr.yaml](allowlist-cidr.yaml) applied, the connection succeeds (the allowlist opens the hole for `app: echo` on port 80). Without it, default-deny blocks it.

```sh
# Allowed by allowlist-cidr.yaml — succeeds, returns whoami's banner.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 -s http://echo.agent-sandbox-demo.svc.cluster.local/
# expect: a "Hostname: echo-xxx ..." block including the request headers

# Un-allowed in-cluster destination — default-deny floor drops the connect.
# Use the kubernetes.default service (always present) as a probe target;
# it's not in any allowlist.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 -v https://kubernetes.default.svc/ 2>&1 | head -10
# expect: connect timeout (NOT TLS error — the connect itself is dropped)
```

To prove the *port* dimension specifically, hit echo's pod IP on a port the allowlist does NOT open:

```sh
ECHO_IP=$(kubectl get pod -n agent-sandbox-demo -l app=echo -o jsonpath='{.items[0].status.podIP}')

# Port 80 — in the allowlist, connects.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 -v "telnet://$ECHO_IP:80" 2>&1 | head -5
# expect: "Connected to <IP>"

# Port 8080 — NOT in the allowlist, drops.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 -v "telnet://$ECHO_IP:8080" 2>&1 | head -5
# expect: "Trying ...:8080..." then connect timeout
```

**Gateway identity.** The sandbox has no SA token mounted; only the gateway pod can mint upstream credentials:

```sh
# Sandbox: no SA token, no ability to call the K8s API or the GCP metadata server.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- ls /var/run/secrets/kubernetes.io/serviceaccount
# expect: ls: cannot access ...: No such file or directory

# Gateway: GKE Workload Identity tells the metadata server which GSA we're bound to.
GATEWAY=$(kubectl get pod -n agent-sandbox-egress -l app=egress-gateway -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n agent-sandbox-egress "$GATEWAY" -- curl -s -H 'Metadata-Flavor: Google' \
  http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/email
# expect: egress-gateway@PROJECT_ID.iam.gserviceaccount.com (after you've replaced PROJECT_ID and granted the binding)
```

**Squid access log** (the source of truth for what the gateway actually allowed/denied). The shipped config uses Squid's default file-based logging — `kubectl logs` only shows Squid's own startup output (configuration parse messages, listener bind), not per-request access lines. Tail the access log inside the pod:

```sh
GATEWAY=$(kubectl get pod -n agent-sandbox-egress -l app=egress-gateway -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n agent-sandbox-egress "$GATEWAY" -- tail -f /var/log/squid/access.log
# expect lines like:
#   TCP_TUNNEL/200 ... CONNECT pypi.org:443 - HIER_DIRECT/151.101.192.223
#   TCP_DENIED_ABORTED/403 ... CONNECT www.facebook.com:443 - HIER_NONE/-

# Note: NONE_NONE/000 "transaction-end-before-headers" entries from
# 169.254.0.0/16 are kubelet TCP probes hitting port 3128 every few seconds.
# Filter them out with: ... | grep -v transaction-end-before-headers
```

Why not stream to `kubectl logs`? The image runs `squid -N` as PID 1 (see `gateway/Dockerfile`); Squid then drops worker privileges to user `proxy` (UID 13) per the `squid-openssl` package's `cache_effective_user` default. Configuring `access_log stdio:/dev/stderr` would resolve to the container's PID-1 stdio handle owned by the root that originally invoked Squid — after the privilege drop, the worker can't write to it and the container crashes. If you want access lines in `kubectl logs`, run a sidecar that tails `/var/log/squid/access.log` and writes to its own stdout.

## 7. Per-request credential injection (AUTH-1)

[egress-gateway.yaml](egress-gateway.yaml) ships three layers of credential isolation that together cover CUJ SAND-AUTH-1:

1. **GKE Workload Identity on the gateway ServiceAccount** for upstreams that accept GCP OAuth tokens (Vertex AI, GCS, BigQuery, anything that honors the metadata server). No static credentials, short-lived tokens minted on demand.
2. **A Kubernetes `Secret`** (`egress-gateway-credentials`) for upstreams that use static API keys (OpenAI, Anthropic, internal APIs). Only the gateway pod's initContainer mounts the Secret; the main Squid container does not even inherit the env vars.
3. **A Squid `request_header_add` directive** that injects the bearer token into outbound requests matching a per-upstream ACL. The directive in the shipped template fires for the `internal_api` ACL, which points at `echo.agent-sandbox-demo.svc` (the verify target from [test-targets.yaml](test-targets.yaml)); add more `acl` + `request_header_add` pairs for additional upstreams.

The initContainer (`render-config`) reads the Secret as env vars, runs a small `sed` script over `squid.conf.template`, and writes the rendered `squid.conf` to an `emptyDir` volume that the main Squid container mounts. The unrendered template is what lives in the ConfigMap (and on disk in this repo), so the manifest is safe to commit; the rendered config with real credentials exists only in pod-scoped memory.

### Plaintext HTTP vs HTTPS

`request_header_add` and `urlpath_regex` ACLs fire only on HTTP requests that Squid can parse. For HTTPS, this means **only on destinations Squid is configured to intercept (bump)**. Destinations not in the `bump_targets` ACL are spliced — TLS goes end-to-end and Squid sees only the SNI.

The shipped config bumps `httpbin.org` (for verification) and splices everything else. Bump narrowly: anything in `bump_targets` grants the proxy full visibility into the encrypted traffic to that destination. §7.5 covers the ssl_bump setup including how to add bump targets.

An alternative architecture worth knowing about, especially if you don't want a CA in your trust chain:

- **Delegate microservice instead of a proxy.** Replace the forward proxy with a small in-cluster API (`http://egress.svc/v1/openai/chat`, etc.) that uses the bound GSA or the mounted Secret to make the *outbound* call itself. Sandboxes call the delegate over plaintext HTTP in-cluster; the delegate calls the upstream over HTTPS. This is the cleanest model when you control the call shape, and it's the architecture most agent-platform vendors have settled on. Trade-off: a custom service to maintain instead of a config-driven proxy.

### Verifying credential isolation

Even without HTTPS injection, the secret-isolation half of AUTH-1 is demonstrable:

```sh
# Sandbox has no SA token and no env access to the secret.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- ls /var/run/secrets/kubernetes.io/serviceaccount
# expect: No such file or directory

kubectl exec -n agent-sandbox-demo "$SANDBOX" -- printenv | grep -i bearer
# expect: nothing

# Sandbox cannot reach the K8s API to read the Secret either — kubernetes.default.svc
# is not in any allowlist, so the default-deny floor blocks it.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 -sk https://kubernetes.default.svc/api/v1/namespaces/agent-sandbox-egress/secrets/egress-gateway-credentials
# expect: connect timeout

# Gateway's main container also does NOT have the secret in its env — only the
# initContainer did, scoped to its lifetime.
GATEWAY=$(kubectl get pod -n agent-sandbox-egress -l app=egress-gateway -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n agent-sandbox-egress "$GATEWAY" -- printenv INTERNAL_API_BEARER
# expect: nothing

# But the rendered Squid config inside the gateway DOES contain the bearer
# token (this is by design — that's where Squid reads it from). Anyone with
# exec on the gateway pod can read it; that's an admin-only surface.
kubectl exec -n agent-sandbox-egress "$GATEWAY" -- grep -E "request_header_add" /etc/squid/squid.conf
# expect: request_header_add Authorization "Bearer <your-token>" internal_api
```

### Verifying the injection end-to-end

Deploy the echo server (whoami — returns its request headers in the response body), then route a plaintext HTTP request through the proxy and look for the injected `Authorization` header in echo's response:

```sh
# Deploy the test target. The SandboxTemplate already labels its pods so
# allowlist-cidr.yaml lets sandboxes reach echo:80 directly, and Squid is
# already configured to inject Authorization for echo.agent-sandbox-demo.svc.
kubectl apply -f examples/policy/cilium/test-targets.yaml
kubectl rollout status -n agent-sandbox-demo deploy/echo

# Call echo THROUGH the proxy. We use --proxy explicitly (most reliable;
# beats curl's env-var handling) and `env -u NO_PROXY` to drop the
# .svc/.cluster.local exclusion. We also use the FULL FQDN because Squid's
# DNS resolver does not append /etc/resolv.conf search domains.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- \
  env -u NO_PROXY \
  curl --max-time 10 -s \
    --proxy http://egress-gateway.agent-sandbox-egress.svc:3128 \
    http://echo.agent-sandbox-demo.svc.cluster.local/
# expect: a "Hostname: echo-xxx" block, AND a line:
#   Authorization: Bearer REPLACE_ME_internal_api_bearer_token
# That line is what Squid injected — the sandbox never had the bearer value.
```

Three things to notice in the output:

- The `Authorization` header is in echo's response, proving Squid added it on the wire.
- The bearer value matches what's in the Secret, which only the gateway's initContainer could read.
- The sandbox process that issued the curl never saw the bearer in any of its own env, files, or arguments — it just talked to a forward proxy that did the right thing.

For comparison, hit echo directly (bypassing the proxy) to see what an unaugmented request looks like:

```sh
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- env -u HTTPS_PROXY -u HTTP_PROXY \
  curl --max-time 5 -s http://echo.agent-sandbox-demo.svc.cluster.local/
# expect: same "Hostname:" block, but NO Authorization header
```

## 7.5. TLS interception (ssl_bump) for HTTPS path filtering and HTTPS header injection

Squid is configured to **bump** TLS for destinations in the `bump_targets` ACL — terminate the client's TLS at the proxy, see the inner HTTP request (URL, headers, body), then re-encrypt to the upstream. This is what unlocks CUJ SAND-EGRESS-4c (URL/path filtering) and the HTTPS half of CUJ SAND-AUTH-1 (header injection) for the chosen destinations.

> ⚠️ **Security note.** Bumping a destination means Squid sees everything you'd see if you owned that destination's TLS endpoint. Anyone with the gateway CA's private key can impersonate any bumped destination to any sandbox. Bump narrowly (`httpbin.org` in the shipped example, not `*.something.com`), guard the CA key, and rotate it like any other root credential. Destinations not in `bump_targets` get `splice` — end-to-end TLS, proxy sees only the SNI.

### Why a custom Squid image

Canonical's `ubuntu/squid:6.6-24.04_edge` image is built with `--with-gnutls`, **not** `--with-openssl`. GnuTLS can do client-side TLS but ssl_bump (server-side TLS termination plus dynamic per-destination certificate generation) requires OpenSSL — specifically the `security_file_certgen` helper, which is only built when Squid is configured with `--with-openssl`. The Canonical image does not ship that binary.

The fix is the small image at [gateway/Dockerfile](gateway/) — `ubuntu:24.04` plus the `squid-openssl` package, which is the Ubuntu-packaged Squid built against OpenSSL with all the cert-generation helpers. See §2 for the one-time `docker build` + `docker push` commands. The image is the same one used by both the `init-ssl-db` initContainer and the main `squid` container in [egress-gateway.yaml](egress-gateway.yaml).

### One-time setup: create the gateway CA

The CA is admin-created out-of-band. Generate it once, create two Kubernetes objects: a `kubernetes.io/tls` Secret in `agent-sandbox-egress` (Squid uses the cert AND key to sign per-destination certs), and a ConfigMap with just the cert in `agent-sandbox-demo` (sandboxes mount it to trust bumped TLS).

```sh
# Generate a self-signed CA — valid 10 years, 4096-bit RSA.
openssl req -x509 -newkey rsa:4096 -nodes \
  -keyout /tmp/gateway-ca.key -out /tmp/gateway-ca.crt \
  -subj "/CN=agent-sandbox egress gateway CA" \
  -days 3650

# Secret for Squid: full CA (cert + key) so it can sign per-destination certs.
kubectl create namespace agent-sandbox-egress --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret tls egress-gateway-ca \
  --namespace agent-sandbox-egress \
  --cert=/tmp/gateway-ca.crt \
  --key=/tmp/gateway-ca.key

# ConfigMap for sandboxes: just the cert. Sandboxes need this to trust the
# per-destination certs Squid signs at runtime.
kubectl create namespace agent-sandbox-demo --dry-run=client -o yaml | kubectl apply -f -
kubectl create configmap gateway-ca-bundle \
  --namespace agent-sandbox-demo \
  --from-file=ca.crt=/tmp/gateway-ca.crt

# Clean up.
shred -u /tmp/gateway-ca.key /tmp/gateway-ca.crt
```

Re-create both objects whenever you rotate the CA. Sandbox pods need to be restarted so their `build-ca-bundle` initContainer picks up the new cert; Squid picks up the new Secret on next pod restart.

> **Rotation gotcha.** When you regenerate the CA, both the Secret and the ConfigMap must be updated to match (mismatched fingerprints = sandbox TLS verify fails on every bumped destination), AND **both the gateway and sandbox pods must be restarted**. The gateway re-reads the Secret on pod restart via its subPath mount. The sandbox's combined trust bundle is assembled by the `build-ca-bundle` initContainer at pod start and is *not* hot-reloaded when the ConfigMap changes — existing sandboxes keep using the old CA until the pod is recreated. To force a rebuild: `kubectl delete pod -n agent-sandbox-demo -l app=my-agent-sandbox --all` (the warm pool recreates them with the current ConfigMap).

### What the manifests do with these objects

- `egress-gateway.yaml` mounts the Secret at `/etc/squid/ca/` and points `tls-cert` / `tls-key` at it in `http_port ... ssl-bump ...`.
- An `init-ssl-db` initContainer initializes Squid's per-destination cert cache (in an emptyDir) so the running Squid can write the certs it generates.
- The SandboxTemplates' `build-ca-bundle` initContainer concatenates the system trust store (`/etc/ssl/certs/ca-certificates.crt`) with the gateway CA into an emptyDir, and the agent container reads it via `CURL_CA_BUNDLE` / `SSL_CERT_FILE` / `REQUESTS_CA_BUNDLE` / `NODE_EXTRA_CA_CERTS`. This keeps trust for spliced destinations (which still present real public certs) AND for bumped destinations (which present per-site certs signed by our CA).

### Verifying HTTPS path filtering (EGRESS-4c) and HTTPS injection (AUTH-1 HTTPS half)

The shipped config bumps `httpbin.org` and allows it through Squid. httpbin echoes the request URL and headers back, which lets us verify both path filtering and header injection on actual HTTPS.

```sh
SANDBOX=$(kubectl get pod -n agent-sandbox-demo -l app=my-agent-sandbox -o jsonpath='{.items[0].metadata.name}')

# 1. Bumped destination, allowed path — TLS validates (against our CA), Squid
#    forwards, and `args` field shows the URL it actually received.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- \
  env -u NO_PROXY \
  curl --max-time 10 -s \
    --proxy http://egress-gateway.agent-sandbox-egress.svc:3128 \
    https://httpbin.org/get
# expect: a JSON response. The "headers" object includes
#   "X-Demo-Injection": "verify-only-not-a-credential"
# That's the AUTH-1 HTTPS half: Squid added the header inside the bumped TLS.
# We inject a clearly-non-credential header for httpbin (not the real
# INTERNAL_API_BEARER) so this verify step never leaks a real token to an
# external host — see the `verify_httpbin` ACL in egress-gateway.yaml.

# 2. Bumped destination, BLOCKED path — Squid returns 403 from inside the
#    bumped TLS, the request never reaches httpbin.
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- \
  env -u NO_PROXY \
  curl --max-time 10 -s -o /dev/null -w '%{http_code}\n' \
    --proxy http://egress-gateway.agent-sandbox-egress.svc:3128 \
    https://httpbin.org/anything/blocked
# expect: 403

# 3. Spliced destination — TLS goes end-to-end, no injection, no path
#    filtering possible. Sandbox sees OpenAI's real cert (validated against
#    the system trust store half of the combined bundle).
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- \
  env -u NO_PROXY \
  curl --max-time 10 -sI \
    --proxy http://egress-gateway.agent-sandbox-egress.svc:3128 \
    https://api.openai.com
# expect: HTTP/2 <some status>. No Authorization injected — api.openai.com
# is not in bump_targets, Squid can't see the request.

# 4. Adding a new bump target: edit the Squid `bump_targets` ACL in
#    egress-gateway.yaml, re-apply, then restart the gateway Deployment.
#    The squid.conf the running container reads is rendered by the
#    `render-config` initContainer into an emptyDir at pod start, so a
#    ConfigMap edit alone does NOT reach the live Squid — only new pods
#    pick up the updated template:
#      kubectl apply -f examples/policy/cilium/egress-gateway.yaml
#      kubectl rollout restart -n agent-sandbox-egress deploy/egress-gateway
#    After the rollout, the ssl_bump pipeline picks the new SNI up at
#    step 1 of the next request.
```

A useful debug trick if step 1 fails with a TLS error in the sandbox: print the cert Squid presented and verify the chain ends at your CA.

```sh
kubectl exec -n agent-sandbox-demo "$SANDBOX" -- \
  env -u NO_PROXY \
  openssl s_client -connect httpbin.org:443 \
    -proxy egress-gateway.agent-sandbox-egress.svc:3128 \
    -CAfile /etc/ssl/certs/ca-bundle-with-gateway.pem </dev/null 2>&1 | grep -E "(subject|issuer|Verify)"
# expect: subject=CN=httpbin.org (Squid-generated leaf)
#         issuer=CN=agent-sandbox egress gateway CA  (your CA)
#         Verify return code: 0 (ok)
```

## 8. Scale, performance, and limits

- **Policy count scales with templates, not claims.** All per-template policies select on `app: my-agent-sandbox` (set per-template via `spec.podTemplate.metadata.labels` on the `SandboxTemplate` itself — *not* on `SandboxClaim.additionalPodMetadata`, which arrives too late for warm-pool pods that exist before adoption). One set of policies per template, not one per sandbox.
- **FQDNNetworkPolicy resolved-IP cap of 50.** Wildcards like `*.googleapis.com` can fan out widely. Split into multiple FQDNNetworkPolicies if you hit the cap, or use the forward proxy for high-cardinality destinations.
- **Forward-proxy capacity.** The Squid Deployment is a chokepoint for all egress from sandboxes routed through it. Size the replica count and node placement accordingly; the manifest ships 2 replicas as a starting point. For multi-tenant isolation, consider one gateway Deployment per major upstream-API destination.
- **DNS behavior under FQDNNetworkPolicy.** Lookups (`nslookup`, `dig`) are not filtered — only the connections to resolved IPs are. Agents can enumerate DNS even when blocked from connecting. If DNS enumeration is a concern, override `dnsPolicy` on the sandbox pod to use public resolvers and remove cluster DNS access in the allowlist.
- **TTL semantics.** FQDNNetworkPolicy honors the DNS TTL: connections established within the TTL keep working past expiry (via conntrack), but new connections after expiry require a fresh successful resolution.
- **ssl_bump cost.** Each bumped destination requires Squid to generate (and cache) a per-site leaf cert signed by the gateway CA. Cache is sized 4 MB by default (`dynamic_cert_mem_cache_size=4MB`) and persisted to a 4 MB on-disk database (`-M 4MB` in `init-ssl-db`); raise both if you bump many distinct destinations. Cert generation is CPU-bound; expect noticeable latency on the *first* TLS handshake to each new bumped destination, then cache hits thereafter.

## 9. Roadmap items this example does NOT cover

Two CUJs need work that lives **upstream of the network-policy layer** — no Cilium / NetworkPolicy / proxy config can satisfy them on its own. Both are real, implementable features in agent-sandbox; each is its own KEP and PR (or sequence of PRs).

### CUJ SAND-EGRESS-2, dynamic gateway selection (`USE_CALLER_GATEWAY` / `GATEWAY_PATH`)

The static routing half — every sandbox in a given template routes through a named gateway — is what this example ships. The **dynamic per-claim selection half** is missing because Cilium policy is static cluster state, not per-claim runtime routing. To build it:

1. **API addition to `SandboxClaim`.** A field describing the gateway choice — `spec.egressGateway: {policy: USE_CALLER_GATEWAY|named|none, gatewayRef?: {name, namespace}}`. Backward-compatible.
2. **Resolution logic.** A mutating admission webhook (or controller pass at claim adoption) that, for `policy: USE_CALLER_GATEWAY`, consults whatever "caller AGW" registry the platform maintains and stamps the adopted pod with `agents.x-k8s.io/egress-gateway: <name>`. For `policy: named`, stamps from the ref directly.
3. **Per-gateway NetworkPolicy + `HTTPS_PROXY` injection.** Each gateway in the cluster has a NetworkPolicy keyed on the label above; the webhook also sets `HTTPS_PROXY` on the container to the matching gateway's Service. The default-deny floor in this example handles the fail-closed half automatically — if the label is never stamped, no gateway is reachable and egress is denied. The controller should also emit a clear Event explaining why.

This overlaps significantly with the roadmap item *"Network Policy Attach at Claim Time"* — same mechanism (claim-level policy reference → resolution at adoption → label injection → CNI enforces), different API vocabulary. Probably worth one KEP that covers both.

### CUJ SAND-EGRESS-3, IAM bound to per-Agent Identity

The coarse half — every sandbox routed through the gateway shares the gateway's GSA, and upstream APIs IAM-allowlist that one identity — is shipped. The **per-Agent Identity half** is fundamentally an agent-sandbox-core gap, not a Cilium one: Cilium has no concept of "identity" beyond pod labels, no hook for calling an external IAM service, and no way to mint per-call credentials. Whatever solution lands, Cilium is unchanged.

To make a concrete example tangible: the admin wants to write rules like *"agent-alice can call Vertex AI, agent-bob can call Cloud Storage but not Vertex AI"* — and have the upstream APIs see those calls as actually coming from alice and bob (so the upstream's own logs, quotas, and IAM checks make sense). Today every sandbox is the same identity, so this is undefined.

Three pieces have to stack, each gated by the previous:

1. **Per-sandbox identity provisioning.** The roadmap entry *"Dynamic Identity Provisioning"* — at claim time, mint a per-claim Kubernetes ServiceAccount bound to a per-Agent GSA via Workload Identity Federation (or issue a per-pod SVID via SPIRE). agent-sandbox does not model this today, and **this is the prerequisite for everything else** — without distinct per-claim identities, "per-agent IAM" has nothing to act on. Likely its own KEP.
2. **Trustworthy identity propagation to the gateway.** The gateway needs to know *which* agent is making each call, in a way the sandbox itself cannot forge. Three reasonable shapes:
   - **Source-IP lookup.** Gateway queries the K8s API: source IP → pod → KSA → Agent. Sandbox sends nothing — it can't forge its own source IP. Cheapest and most K8s-native.
   - **mTLS with per-claim cert (SPIFFE/SPIRE).** Cert subject is the identity, rooted in SPIRE node attestation. Cleanest but adds SPIRE infrastructure.
   - **JWT in `Authorization` header.** Sandbox process can read its own JWT — weakest isolation; rejected for this CUJ.
3. **IAM-aware gateway** (Squid is the wrong tool). Squid's `request_header_add` injects *static* credentials; it cannot call out to IAM per request or mint per-call tokens. Two off-the-shelf shapes:
   - **Envoy + `ext_authz` filter.** Envoy proxies the connection and delegates auth decisions to a small gRPC service (which does identity lookup, IAM check, and credential minting). This is the architecture most production deployments converge on.
   - **Delegate microservice.** Replace the proxy with a custom in-cluster HTTP API (`POST /v1/vertex/predict`); sandbox calls it in plaintext; the service handles identity + IAM + upstream call. Less general than Envoy+ext_authz, easier to ship for a specific call surface.

Practically, the work has to land in order: piece 1 (a KEP for per-claim identity in agent-sandbox core) unblocks pieces 2 and 3. Until then, this CUJ stays at the coarse-gateway-identity level.

### CUJ SAND-EGRESS-5, admission-time "no public egress"

Cilium can't satisfy this CUJ on its own — Cilium *enforces* policies, it has no opinion on which policies users are allowed to create. Stopping a misconfiguration at admission is upstream of the data path entirely.

A `ValidatingAdmissionPolicy` + binding for the `SandboxTemplate` path is shipped at [`examples/policy/vap/restrict-template-egress-policy.yaml`](../vap/restrict-template-egress-policy.yaml) and [`-binding.yaml`](../vap/restrict-template-egress-binding.yaml). It rejects any `SandboxTemplate` whose `spec.networkPolicy.egress` allows `0.0.0.0/0` or `::/0`, with namespace-opt-in via the label `agent-sandbox.x-k8s.io/restrict-public-egress=true`. See [`examples/policy/vap/README.md`](../vap/README.md) for deployment and verify steps and [`../vap/restrict-template-egress-test.yaml`](../vap/restrict-template-egress-test.yaml) for a deliberately-bad fixture you can apply to confirm enforcement.

#### How "complete" is this?

It depends on your tenant model. In the **standard agent-sandbox tenant posture** — tenants have RBAC for `SandboxClaim` (and possibly `SandboxTemplate`) but not for raw NetworkPolicies, CiliumNetworkPolicies, FQDNNetworkPolicies, or the Squid ConfigMap — the shipped policy is **complete**. The `SandboxTemplate.spec.networkPolicy.egress` field is the only knob tenants have to grant public egress, and admission closes it.

For richer tenant models, or as **defense-in-depth against admin mistakes**, you'd want sibling policies (each its own VAP — CEL doesn't usefully match across unrelated Kinds, so they don't share an expression). Roughly:

| Sibling policy on | Matters when | Risk if you don't ship it |
| --- | --- | --- |
| Raw `NetworkPolicy` (`networking.k8s.io/v1`) | Tenants can create NetworkPolicies in their namespace | A user creates a NetworkPolicy with `to: ipBlock.cidr: 0.0.0.0/0`, granting public egress directly |
| `CiliumNetworkPolicy` / `CiliumClusterwideNetworkPolicy` | Tenants can write CNPs (uncommon — CNP CRD is cluster-installed) | Same shape via `toCIDR: 0.0.0.0/0` or `toEntities: world` |
| `FQDNNetworkPolicy` | Tenants can write FQDN policies | `matches[].pattern: "*"` or top-level-TLD wildcards |
| `egress-gateway-config` ConfigMap edits | Admin self-defense against typos | An accidental `*` in `allowed_hosts.txt` re-opens the proxy to anywhere |
| `SandboxTemplate.spec.networkPolicyManagement: Unmanaged` without a compensating policy | Tenants choose Unmanaged AND no cluster-wide default-deny exists | Sandbox has no policy → wide-open egress |

If you've applied the default-deny floor from this example ([`default-deny-egress.yaml`](default-deny-egress.yaml) or [`default-deny-egress-netpol.yaml`](default-deny-egress-netpol.yaml)) — Unmanaged just means agent-sandbox won't generate a NetworkPolicy itself, not that none exists.

The shipped policy is a starting point that demonstrates the pattern; production deployments typically layer several. The repo's other admission examples — [`examples/policy/kyverno/`](../kyverno/) and [`examples/policy/opa-gatekeeper/`](../opa-gatekeeper/) — show how to express richer rules in those engines (Kyverno and Rego can match across multiple Kinds more ergonomically than CEL can).

### Smaller out-of-scope items

- **Concrete IAM bindings.** `iam.gke.io/gcp-service-account` values and upstream IAM policies are project-specific. Substitute `PROJECT_ID` and the GSA email before applying.
- **Non-GKE clusters.** This example targets GKE Dataplane v2. On vanilla upstream Cilium most of these primitives (`toFQDNs`, L7 HTTP rules, `CiliumEgressGatewayPolicy`) are available directly and you would write a different example.
