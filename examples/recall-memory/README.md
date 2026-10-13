# Agent Memory with Recall

A sandbox is thrown away when the task is over, and everything the agent learned goes with it. This example keeps that memory outside the sandbox, in a memory server the sandbox reaches with a short-lived key. The key is tied to the sandbox's `SandboxClaim` so a claim created again under the same name gets its memory back.

The memory server is [Recall](https://github.com/Polign/recall) (Apache-2.0) on [polign_db](https://github.com/Polign/polign). The pattern does not depend on it. Any server that verifies a projected token with TokenReview and follows the owner references below can do the same.

## The token exchange

The sandbox pod carries one a projected service account token with audience `recall`. The service account does not mount a default token.

```yaml
volumes:
  - name: recall-token
    projected:
      sources:
        - serviceAccountToken:
            path: token
            audience: recall
            expirationSeconds: 3600
```

The agent posts that token to `POST /v1/auth/kubernetes`. The server creates a `TokenReview` with the same audience. The API server answers with the service account and the pod's name in `authentication.kubernetes.io/pod-name`. Based on this information, the server mints a 15 minute key.

- The tenant is the service account's namespace (`agents`).
- The agent is the claim's name, found as below.
- The session is the pod's name, so the record still says which pod did the work.

When the key expires, the agent exchanges its token again. A key copied out of a pod dies at its TTL. As the token dies with the TTL, nothing else can mint a key for a deleted pod.

## Identity from the SandboxClaim

A pod handed out by a warm pool has a generated name like `recall-agents-x7k2q` which changes every time, so we cannot use it for agent's identity. The claim's name however last across pod creation. The controllers already record the chain as controller owner reference.

```
Pod recall-agents-x7k2q
  ownerReferences: Sandbox recall-agents-x7k2q   (agents.x-k8s.io)
    ownerReferences: SandboxClaim run-1          (extensions.agents.x-k8s.io)
```

The server reads the pod, then the Sandbox, then the SandboxClaim, and uses `run-1` as the agent. The API server writes owner references, so the pod cannot choose them. At each hop the server checks that the reference is matched, so a pod left over from a deleted claim gets no key for a new claim with the same name.

| Owner of the pod's Sandbox | Agent |
| --- | --- |
| a `SandboxClaim` | the claim's name |
| a `SandboxWarmPool` | none: refused until the sandbox is claimed |
| nothing (a `Sandbox` created directly) | the pod's name |

The RBAC for this is read-only, plus TokenReview:

```yaml
rules:
  - apiGroups: [authentication.k8s.io]
    resources: [tokenreviews]
    verbs: [create]
  - apiGroups: [""]
    resources: [pods]
    verbs: [get]
  - apiGroups: [agents.x-k8s.io]
    resources: [sandboxes]
    verbs: [get]
  - apiGroups: [extensions.agents.x-k8s.io]
    resources: [sandboxclaims]
    verbs: [get]
```

## Network policy

A template with no `networkPolicy` gets the Secure-by-Default policy including public egress only, and public DNS resolvers. The sandbox could not resolve or reach an in-cluster memory server. `sandboxtemplate.yaml` allows egress to cluster DNS, to the `recall` pods on port 8080, and to the public internet. Every other pod stay out of reach. (verified in `run-kind.sh`)

## Prerequisites

- docker, [kind](https://kind.sigs.k8s.io/), kubectl, openssl and perl
- No account and no API key. Both images are public:
  - `ghcr.io/polign/recall:0.17.0`
  - `ghcr.io/polign/polign-server:0.14.0`

kind's default network plugin enforces NetworkPolicy, so the policy is real on a plain kind cluster.

## Usage

```sh
./run-kind.sh
```

The script creates a kind cluster named `recall-memory` (or reuses it), installs agent-sandbox v1.0.6, applies the files below, and checks from inside the sandboxes.

1. A sandbox still in the warm pool is refused a key. It only gets the key when claim `run-1` takes that same pod.
2. Claim `run-1` gets a key for `agents:run-1`, remembers a preference twice, and saves working state.
3. The claim is deleted and created again. The new pod gets a key for `agents:run-1` and resumes with the saved working state.
4. Claim `run-2` cannot read `agents:run-1` (HTTP 403) or resume `run-1`. It errors out with `namespace_mismatch`.
5. The sandbox cannot directly reach the database underneath Recall.

Each check exits non-zero with a `FAIL:` line if the result is wrong.

To apply the files by hand on a cluster that already runs agent-sandbox-

```sh
kubectl apply -f namespace.yaml -f serviceaccount.yaml
kubectl -n recall create secret generic polign-key \
  --from-literal=api-key="plgn_$(openssl rand -hex 8)_$(openssl rand -hex 32)"
kubectl apply -f recall-server.yaml
kubectl apply -f sandboxtemplate.yaml -f sandboxwarmpool.yaml
kubectl apply -f sandboxclaim.yaml
```

Then, inside the claimed pod:

```sh
curl -sS -H 'Content-Type: application/json' \
  -d "{\"token\":\"$(cat $RECALL_TOKEN_FILE)\"}" \
  $RECALL_URL/v1/auth/kubernetes
```

## Files

| File | What it is |
| --- | --- |
| `namespace.yaml` | Namespaces `agents` (the sandboxes) and `recall` (the server). |
| `serviceaccount.yaml` | The `runner` service account, with no default token. |
| `recall-server.yaml` | polign_db, `recall serve`, their Services, and the ClusterRole above. |
| `sandboxtemplate.yaml` | The projected token, `dnsPolicy: ClusterFirst`, and the network policy. |
| `sandboxwarmpool.yaml` | Two warm sandboxes. |
| `sandboxclaim.yaml` | Claim `run-1`. |
| `run-kind.sh` | The whole example on kind, with the checks above. |

## What we measured

kind v0.33 on an Apple Silicon laptop, Kubernetes v1.37 node, runc, agent-sandbox v1.0.6, recall 0.17.0, polign_db 0.14.0, 2026-10-12. Four runs of `run-kind.sh` on a fresh cluster, then 200 more recall calls and 100 more exchanges from inside a claimed sandbox.

| What | Result |
| --- | --- |
| Claim to pod Ready, from the warm pool | 0.2 to 0.5 s |
| Token exchange: TokenReview, three reads (pod, Sandbox, SandboxClaim), key mint | p50 5.8 to 6.4 ms, p99 13 to 53 ms |
| recall over REST with a key | p50 1.4 ms, p99 2.6 to 6.8 ms |
| Claim deleted and created again | new pod, same `agents:run-1`, current value `helix` with `vim` as replaced, working state resumed |
| Key for `run-2` against `run-1` | HTTP 403 on recall, `namespace_mismatch` on resume |

One recall call took 2.5 s in one run, right after the first writes. It did not repeat in 400 later calls.

## What this example does not do

- **No gVisor or Kata numbers.** The run above uses runc. A sandbox runtime will show a slower cold start, which a warm pool hides.
- **Resume needs leases.** The resume tools need a store with leases. polign_db has them. On a store without them, the server keeps memory but does not serve the resume tools.
- **Revocation is not instant across replicas.** Each replica caches key checks for 30 seconds by default. The replica that revokes a key refuses it at once; the others refuse it when their cache entry expires.
- **A deleted claim's key lives until its TTL.** Nothing can mint or renew a key for the deleted pod, but a key minted before the delete works for up to 15 minutes.
- **One polign_db replica on a local volume.** Fine for kind. A real install points polign_db at a bucket.

## Hardening notes

- **Key TTL.** `-kubernetes-key-ttl` is set to 15 minutes. For less exposure, this can be shortened.
- **Audience.** The server only accepts tokens minted for audience `recall`. A token for the API server cannot be replayed against it, and a `recall` token is useless against the API server.
- **Narrow the RBAC per tenant.** TokenReview needs a ClusterRoleBinding. The `get` on pods, sandboxes and sandboxclaims does not: bind those with a RoleBinding in each tenant namespace instead, so the server can read nothing outside them.
- **Fence the store.** The sandbox's egress policy already keeps it away from polign_db. An ingress policy on polign_db that admits only the `recall` pods adds a second wall for everything else in the cluster.
- **Pin by digest.** The manifests pin tags. Pin digests in production.

## See also

- [sandbox-ksa](../sandbox-ksa/README.md): a sandbox with its own service account.
- [containarium-execution-scoped-token](../containarium-execution-scoped-token/README.md): run-scoped credentials in a reused sandbox.
- [network-policy-management](../policy/network-policy-management/README.md): how template network policies work.
