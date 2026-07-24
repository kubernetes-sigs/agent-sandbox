# Agents as a Service: the multi-user platform pattern

This example distills a real multi-tenant "AI agent as a service" platform
([Talaria / ai-agent-service](https://github.com/aditya-shantanu/ai-agent-service))
into pure agent-sandbox resources and kubectl. Every user of such a platform
gets a personal, long-lived [Hermes Agent](https://github.com/NousResearch/hermes-agent)
that is **provisioned in ~2 seconds from a warm pool, suspended when idle to
save cost, and woken transparently when the user returns** — with all state
(conversations, memory, skills) surviving on a PVC.

Where the [hermes-agent](../hermes-agent) example runs one Hermes in one
`Sandbox`, this example shows the **platform shape**: one template, one warm
pool, one tiny claim per user, and `operatingMode` as the cost dial. It ships
in two layers:

1. **The resource layer** (steps 1–7): pure manifests + kubectl, so every
   mechanism is visible.
2. **The service layer** (step 8): a small included **gateway**
   (`gateway/`, ~250 lines) that makes it directly usable — signup API,
   per-user bearer tokens, a `/u/{user}/**` proxy that transparently
   **wakes suspended agents on connect**, and an idle sweeper that suspends
   them again. Deploy it and you have a working multi-user agent service.

What it demonstrates, feature by feature:

| agent-sandbox feature | Role in the platform |
|---|---|
| `SandboxTemplate` with `volumeClaimTemplates` | Identical agent pods; per-user state on a PVC at `/opt/data` |
| `envVarsInjectionPolicy` / `volumeClaimTemplatesPolicy: Disallowed` | Claims that would bypass the warm pool are **rejected loudly**, not cold-started silently |
| `service: true` | Stable per-sandbox DNS (`status.serviceFQDN`) for a fronting gateway to route on |
| `SandboxWarmPool` | Signup latency is pre-paid: image pull, PVC provision, agent boot all happen before any user exists |
| `SandboxClaim` + `warmPoolRef` | "Signup" = create a ~10-line claim; adoption is a label flip, Ready in ~2s |
| `additionalPodMetadata` | The one warm-compatible per-claim customization (user label on the pod) |
| `Sandbox.spec.operatingMode` | Suspend = delete only the pod (PVC + Service survive); resume = recreate pod, reattach the same PVC |
| Cascade delete | Deleting the claim garbage-collects sandbox **and** PVC — account deletion in one call |

The economics this pattern buys (measured in the full platform, on GKE):
an always-on agent costs ~$270/month on comparable hardware; suspend-when-idle
plus warm pools, Spot nodes and node-level swap take that to **~$0.14/agent/month
at scale** — the user-visible price being one cold-start pause per
return-after-absence.

## Prerequisites

- A Kubernetes cluster with a default StorageClass (kind and GKE both work
  as-is). Nodes must be **amd64** (the Hermes image is amd64-only).
- agent-sandbox with extensions installed:

```sh
VERSION=v0.5.3
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/sandbox-with-extensions.yaml
kubectl -n agent-sandbox-system rollout status deployment --timeout=180s
```

Or run everything below in one shot with `./run-test-kind.sh` (expects a
kind cluster + the manifest above already applied).

## 1. Install the platform resources

```sh
kubectl apply -f 00-prereqs.yaml -f 10-sandboxtemplate.yaml -f 20-sandboxwarmpool.yaml
```

Watch the two warm spares boot (first time pulls the ~GB agent image —
subsequent operations never pay that again; that is the point of the pool):

```sh
kubectl -n hermes-demo get sandboxes -l agents.x-k8s.io/warm-pool-sandbox -w
```

Wait until both show `READY: True`. Each spare already has its PVC bound and
Hermes serving on ports 9119 (dashboard) and 8642 (OpenAI-compatible API).

## 2. "Signup": claim an agent (~2s)

A user signing up is one small claim — `warmPoolRef` plus a pod label:

```sh
kubectl apply -f 30-claim-alice.yaml
kubectl -n hermes-demo wait sandboxclaim hermes-alice --for=condition=Ready --timeout=60s
```

Inspect what happened:

```sh
SB=$(kubectl -n hermes-demo get sandboxclaim hermes-alice -o jsonpath='{.status.sandbox.name}')
echo "alice's sandbox: $SB"
kubectl -n hermes-demo get sandboxes --show-labels
```

Three things worth noticing:

- **The sandbox keeps its pool-generated name** (`hermes-pool-…`), not the
  claim's name — adoption re-labels an existing warm sandbox rather than
  creating one. A platform must always resolve user → sandbox through
  `claim.status.sandbox.name`.
- The `agents.x-k8s.io/warm-pool-sandbox` label is **gone** from the adopted
  sandbox (it left the pool's inventory), and the pool has already started
  replenishing back to 2 spares.
- The pod carries the claim's `additionalPodMetadata` label
  (`sandbox.users.io/hermes-user=alice`), and
  `kubectl -n hermes-demo get sandbox $SB -o jsonpath='{.status.serviceFQDN}'`
  shows the stable DNS name a gateway would proxy to.

## 3. Talk to the agent

```sh
POD=$(kubectl -n hermes-demo get sandbox "$SB" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
kubectl -n hermes-demo port-forward "pod/$POD" 9119:9119 8642:8642 &
PF_PID=$!
```

- Dashboard: open http://localhost:9119 — basic auth `platform` /
  `demo-password-change-me` (from `00-prereqs.yaml`).
- OpenAI-compatible API:

```sh
curl -s -H "Authorization: Bearer demo-api-server-key-16chars-min" \
  http://localhost:8642/v1/models
```

(Actual chat completions additionally need a real LLM key in the
`hermes-provider-keys` Secret; every infrastructure step in this walkthrough
works without one.)

Plant a piece of state we can check after the suspend/resume cycle:

```sh
kubectl -n hermes-demo exec "$POD" -- sh -c 'echo "remember me" > /opt/data/marker.txt'
```

## 4. Suspend: the cost dial

In the full platform an idle sweeper does this automatically after ~15s of
inactivity; here we flip the dial by hand:

```sh
kill "$PF_PID" 2>/dev/null || true  # stop the port-forward
kubectl -n hermes-demo patch sandbox "$SB" --type merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n hermes-demo wait --for=delete "pod/$POD" --timeout=120s
```

Now look at what suspension actually is — **only the pod is gone**:

```sh
kubectl -n hermes-demo get pods                # no pod for $SB
kubectl -n hermes-demo get pvc "data-$SB"      # STILL Bound — all state retained
kubectl -n hermes-demo get svc "$SB"           # per-sandbox Service retained too
kubectl -n hermes-demo get sandbox "$SB" \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}'
```

A suspended agent costs its PVC and nothing else. This is where the ~1000×
cost reduction lives.

## 5. Resume: the user comes back

```sh
kubectl -n hermes-demo patch sandbox "$SB" --type merge -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n hermes-demo wait sandbox "$SB" --for=condition=Ready --timeout=180s
```

(It is safe to flip back to `Running` even while suspension is still in
progress — never wait for `Suspended=True` before resuming. A real platform
holds the user's first request during this wait: "wake-on-connect".)

Verify state survived the pod's death:

```sh
POD=$(kubectl -n hermes-demo get sandbox "$SB" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
kubectl -n hermes-demo exec "$POD" -- cat /opt/data/marker.txt   # -> remember me
```

The same PVC was reattached to a brand-new pod. In the full platform this is
also why dashboard login sessions survive a wake (the session-cookie HMAC
secret is stable pod env) and why the agent's conversations, memory and
skills all persist.

Deriving a user-facing state from the API (as the platform's gateway does):

| `spec.operatingMode` | Conditions | User-facing state |
|---|---|---|
| Running | `Ready=True` | Ready |
| Running | not Ready | Waking (or Provisioning) |
| Suspended | `Suspended=True` | Suspended |
| Suspended | not yet | Suspending |

## 6. Policy enforcement: rejection beats silent cold starts

A claim that injects env (or volumeClaimTemplates) forces a cold start that
bypasses everything the warm pool pre-paid. This template sets both
injection policies to `Disallowed`, so such claims fail **loudly**:

```sh
kubectl apply -f 40-claim-rejected.yaml
sleep 5
kubectl -n hermes-demo get sandboxclaim hermes-mallory -o yaml | grep -A4 conditions:
kubectl -n hermes-demo describe sandboxclaim hermes-mallory | tail -5
```

The claim never binds a sandbox (`EnvVarsInjectionRejected`) — a
misconfigured client gets an error instead of silently degrading every
signup to a cold provision.

```sh
kubectl delete -f 40-claim-rejected.yaml
```

## 7. Teardown: account deletion is one cascade

```sh
kubectl -n hermes-demo delete sandboxclaim hermes-alice
sleep 10
kubectl -n hermes-demo get sandbox "$SB"      # gone
kubectl -n hermes-demo get pvc "data-$SB"     # gone — the user's data with it
```

This cascade is also why the platform **never sets `spec.lifecycle` on
claims**: any claim-expiry path deletes the sandbox and garbage-collects the
user's PVC — their entire memory. The cost dial is `operatingMode`, which
nothing fights.

## 8. Make it a service: deploy the gateway

Everything so far was kubectl. The included gateway (`gateway/gateway.py`)
is the piece that turns the resources into something users can actually
consume — the same control loop the full platform implements, in ~250
readable lines: signup creates the claim and mints a token (only its
SHA-256 is stored, as a claim annotation), the proxy resolves user →
`claim.status.sandbox.name` → `sandbox.status.serviceFQDN`, wakes suspended
agents on connect, and an idle sweeper flips them back to `Suspended`.

Build and deploy (kind shown; for other clusters push the image to your
registry and adjust `50-gateway.yaml`):

```sh
docker build -t aaas-gateway:demo gateway/
kind load docker-image aaas-gateway:demo --name <your-kind-cluster>
kubectl apply -f 50-gateway.yaml
kubectl -n hermes-demo rollout status deploy/aaas-gateway
kubectl -n hermes-demo port-forward svc/aaas-gateway 8080:8080 &
GW_PF_PID=$!
```

Sign up a user — one POST, Ready in ~2s off the warm pool:

```sh
curl -s -X POST localhost:8080/users -H 'Content-Type: application/json' \
  -d '{"user":"bob"}'
# -> {"user":"bob","token":"<shown-once>","state":"Ready","sandbox":"hermes-pool-..."}
TOKEN=<token from the response>
```

Use the agent through the gateway (OpenAI-compatible API under
`/u/bob/v1/`, dashboard HTML at `/u/bob/`). The token is accepted **only**
in the `Authorization` header — never in the URL, where it would leak into
access logs, browser history and Referer headers:

```sh
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/u/bob/v1/models
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/u/bob/ | head -3   # dashboard HTML
```

To browse the dashboard interactively, port-forward straight to **bob's**
pod (resolved through his claim, as in step 3 — a real platform would
instead exchange the token for a session cookie rather than ask a browser
to send bearer headers):

```sh
BOB_SB=$(kubectl -n hermes-demo get sandboxclaim hermes-bob -o jsonpath='{.status.sandbox.name}')
BOB_POD=$(kubectl -n hermes-demo get sandbox "$BOB_SB" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
kubectl -n hermes-demo port-forward "pod/$BOB_POD" 9119:9119 &
```

Now watch the whole point happen. Stop making requests for ~60s
(`IDLE_TIMEOUT`), and the sweeper suspends bob's agent:

```sh
kubectl -n hermes-demo get pods -w        # bob's pod terminates
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/users/bob   # {"state":"Suspended",...}
```

Then just use it again — the gateway holds the request while the agent
resumes (**wake-on-connect**; a few seconds on kind):

```sh
time curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/u/bob/v1/models
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/users/bob   # {"state":"Ready",...}
```

The user experiences a pause; nothing was lost. Account deletion (the claim
cascade from step 7) also requires the user's token:

```sh
curl -X DELETE -H "Authorization: Bearer $TOKEN" localhost:8080/users/bob
```

Full cleanup:

```sh
kill "$GW_PF_PID" 2>/dev/null || true
kubectl delete namespace hermes-demo
```

## From demo to production

The included gateway is a deliberately compact distillation of a production
platform, [**ai-agent-service**](https://github.com/aditya-shantanu/ai-agent-service),
which hardens every piece shown here:

- a Go gateway with an **adaptive idle sweeper** (suspend 15s after an
  isolated request, but never mid-conversation) and a **cron waker**
  (suspended agents wake for their own scheduled jobs, with zero user
  traffic); wake-on-connect measured at p50 ~4s on kind/runc, ~20–24s on
  GKE under gVisor (dominated by PD attach);
- production hardening: gVisor (GKE Sandbox) runtime class, controller-managed
  per-sandbox NetworkPolicy scoped to the gateway, Spot nodes with local-SSD
  swap (62 agents/node measured), Terraform, and a latency benchmark that
  prices the suspend/resume UX tax against an always-alive baseline.

## Known limitations

- The walkthrough uses `kubectl port-forward`, which is **not supported for
  gVisor-sandboxed pods** — that's why this demo pins runc. Under GKE
  Sandbox, front the agents with an in-cluster gateway instead.
- The Hermes image is amd64-only; on arm64 Macs use a kind-on-colima/amd64
  setup or a cloud cluster.
- Shared platform credentials (dashboard/API) are the warm-pool trade-off:
  pod env exists before the user does. Per-user isolation in the full
  platform comes from per-user gateway tokens plus the NetworkPolicy
  boundary, not from per-pod secrets.
