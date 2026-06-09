# Kata-Isolated Per-Owner Agent Warm Pool on AKS

This example shows how to combine the `agent-sandbox` extension CRDs with
the **AKS Pod Sandboxing** feature (Kata Containers on Microsoft Hyper-V)
to run a fleet of owner-aware AI agents on Azure Kubernetes Service.
Every user gets their own agent process inside its own lightweight VM,
and all users share a single public IP fronted by a header-routed proxy
so a request tagged for Alice always lands on Alice's pod.

The agent itself is a small FastAPI shim over an OpenAI-compatible
chat endpoint (Microsoft Foundry, Azure OpenAI, vanilla OpenAI, …).
It reads the caller's identity from the `X-Owner` HTTP header, bakes
it into the system prompt, and tells the model to begin every reply
with `I am <owner>'s agent.` That gives you a **falsifiable,
end-to-end proof of routing**: if Alice ever sees a reply that begins
with `I am bob's agent`, the routing is broken.

## What you get

- A `SandboxTemplate` that pins
  `runtimeClassName: kata-vm-isolation` (the AKS Pod Sandboxing
  RuntimeClass) so every pod produced from it is scheduled onto a Kata
  micro-VM, plus a node selector and toleration that pin the pods to
  the Kata-capable node pool, plus a `NetworkPolicy` that allows only
  the router pods to reach the agent port.
- A `SandboxWarmPool` that pre-creates a handful of Kata sandboxes so
  claims do not pay the Kata cold-start cost.
- A `SandboxClaim` that a regular user submits to grab one agent
  sandbox out of the pool.
- A `sandbox-router` Deployment + two Services (ClusterIP for
  in-cluster, LoadBalancer for the public Azure LB IP) that proxies
  user requests to the correct per-user pod based on HTTP headers.
- A runnable Go program (`client/`) that uses the Go SDK to
  provision a sandbox per owner (one-shot or long-lived), chats with
  it through the public IP, and asserts the reply names the right
  owner.
- An agent that keeps an in-process per-owner chat history, so a
  `-reuse`d sandbox supports real multi-turn conversations.

## Files in this directory

| File | Purpose |
| --- | --- |
| [`agent/`](agent/) | Source for the owner-aware FastAPI agent: `agent.py`, `Dockerfile`, `requirements.txt`. Build it into your ACR (Step 2). |
| [`sandboxtemplate.yaml`](sandboxtemplate.yaml) | Admin-owned blueprint: agent container + AKS Pod Sandboxing runtime + OpenAI-compatible endpoint Secret references + per-template NetworkPolicy. |
| [`sandboxwarmpool.yaml`](sandboxwarmpool.yaml) | Pre-warms N Kata agent pods so claim adoption is fast. |
| [`sandboxclaim.yaml`](sandboxclaim.yaml) | User-facing claim that adopts one agent sandbox from the pool. |
| [`router.yaml`](router.yaml) | The sandbox-router `Deployment`, a `ClusterIP` Service for in-cluster callers, and a `LoadBalancer` Service for the public Azure LB IP. |
| [`client/main.go`](client/main.go) | Runnable Go program that uses the agent-sandbox Go SDK to provision an agent sandbox (one-shot or `-reuse`d) and chat with it through the public IP. |

## Prerequisites

All commands below assume you have the Azure CLI (`az`) and `kubectl`
available locally, and that `az login` has selected the subscription
you want to deploy into.

1. **An AKS cluster with the `agent-sandbox` controller plus its
   extensions installed.** Follow the install instructions in the
   project root README of this repository. You need the `Sandbox` core
   CRD and the `SandboxTemplate` / `SandboxWarmPool` / `SandboxClaim`
   extension CRDs.

2. **A Kata-capable AKS node pool with the `kata-vm-isolation`
   RuntimeClass registered.** AKS exposes Kata Containers through the
   "Pod Sandboxing" feature, which requires Azure Linux nodes and a
   Gen2 VM SKU that supports nested virtualisation (D-series v3/v4/v5,
   etc.). The RuntimeClass name is `kata-vm-isolation` on the rollouts
   that this example targets (newer Microsoft documentation sometimes
   calls it `kata-mshv-vm-isolation` — same feature, different label;
   the template, selectors and taints are all wired to
   `kata-vm-isolation`). Create the pool with
   `--workload-runtime KataMshvVmIsolation` — the RuntimeClass is
   registered automatically when at least one such node joins the
   cluster:

   ```bash
   # One-time per cluster: register the preview feature (if not already).
   az feature register --namespace Microsoft.ContainerService --name KataVMIsolationPreview
   az provider register --namespace Microsoft.ContainerService

   # Add a Kata-capable node pool (Azure Linux + Gen2 D-series). AKS
   # will label the nodes `kubernetes.azure.com/kata-vm-isolation=true`
   # and taint them `kata=enabled:NoSchedule` so general workloads do
   # not land on the pool by accident.
   az aks nodepool add \
     --resource-group <rg> \
     --cluster-name <aks-cluster> \
     --name sandboxagent \
     --os-sku AzureLinux \
     --workload-runtime KataMshvVmIsolation \
     --node-vm-size Standard_D4s_v3 \
     --node-count 2

   # Verify:
   kubectl get runtimeclass kata-vm-isolation
   kubectl get nodes -L kubernetes.azure.com/kata-vm-isolation -L agentpool
   ```

   [`sandboxtemplate.yaml`](sandboxtemplate.yaml) already pins
   scheduling to nodes labelled
   `kubernetes.azure.com/kata-vm-isolation: "true"` and adds a matching
   toleration for the `kata=enabled:NoSchedule` taint that AKS applies
   to those nodes.

3. **A NetworkPolicy-aware CNI on the cluster.** AKS supports policy
   enforcement via Azure Network Policy Manager, Calico, or Cilium
   (Azure CNI Powered by Cilium). Pick one at cluster create time —
   e.g. `--network-plugin azure --network-policy cilium` for Azure CNI
   Powered by Cilium. Clusters without a policy engine will silently
   ignore the `NetworkPolicy` this example renders and your isolation
   guarantees will be weaker than what this document claims.

4. **An Azure Container Registry attached to the cluster.** Used to
   build and host the agent image in Step 2. AKS attaches an ACR with:

   ```bash
   az aks update --resource-group <rg> --name <aks-cluster> --attach-acr <your-acr>
   ```

5. **An OpenAI-compatible REST endpoint** (e.g., Azure OpenAI,
   Microsoft Foundry, OpenAI directly, or a self-hosted service like
   vLLM). You will need the base URL, an API key, and a model name.
   See "Switching providers" below for details.

6. **Go 1.22+** on your workstation if you want to run the SDK demo
   in Step 6.

## Step 0 — Create the demo namespace

Everything in this example — the `Secret`, `SandboxTemplate`,
`SandboxWarmPool`, `SandboxClaim`, router `Deployment` and Services —
lives in a dedicated namespace so you can tear the whole demo down
with one `kubectl delete namespace`. Every YAML file in this directory
pins `metadata.namespace: sandbox-agent-demo`, and every `kubectl`
command in the steps below passes `-n sandbox-agent-demo` explicitly so
nothing accidentally lands in `default`.

```bash
kubectl create namespace sandbox-agent-demo
```

If you would rather use a different namespace, do a find-and-replace
on `sandbox-agent-demo` across all the YAML files in this directory
(and in [`client/main.go`](client/main.go)) before you start.

## Step 1 — Create the model endpoint Secret

**What you need:** an **OpenAI-compatible v1 REST endpoint** for chat
completions — i.e. something that accepts `POST /chat/completions`
against the OpenAI Python client. Any provider that speaks that API
will work without code changes: Microsoft Foundry, Azure OpenAI,
OpenAI directly, Together, Anyscale, a self-hosted vLLM, etc. Pick
one, grab its base URL, API key, and model (or deployment) name.

The agent reads those three values from a `Secret` named
`azure-foundry` in the same namespace as the template:

| Key | Description | Example (Microsoft Foundry / Azure OpenAI) | Example (OpenAI) |
| --- | --- | --- | --- |
| `OPENAI_BASE_URL` | OpenAI-compatible v1 base URL | `https://<resource>.openai.azure.com/openai/v1/` | `https://api.openai.com/v1` |
| `OPENAI_API_KEY` | API key for that endpoint | Azure OpenAI key | OpenAI API key |
| `LLM_MODEL` | Model (or deployment) name to call | `gpt-4o` (deployment name) | `gpt-4o` (model name) |

```bash
kubectl create secret generic azure-foundry \
  -n sandbox-agent-demo \
  --from-literal=OPENAI_BASE_URL="https://<your-endpoint>/v1/" \
  --from-literal=OPENAI_API_KEY="<your-api-key>" \
  --from-literal=LLM_MODEL="<model-name>"
```

The Secret name (`azure-foundry`) and the three keys above are what
[`sandboxtemplate.yaml`](sandboxtemplate.yaml) references via
`secretKeyRef`, and [`agent/agent.py`](agent/agent.py) reads them
out of the environment under those exact names. The Secret name is
historical — feel free to rename it to something provider-neutral if
you prefer, but update both files to match.

### Switching providers

Because the agent only assumes the OpenAI v1 `/chat/completions`
shape, swapping providers is a Secret-only change: point
`OPENAI_BASE_URL` and `OPENAI_API_KEY` at the new endpoint and put
the right model identifier in `LLM_MODEL`. No image rebuild, no code
edits.

## Step 2 — Build the agent image into your ACR

[`agent/`](agent/) contains the full source: `agent.py` (FastAPI +
the OpenAI client + per-owner chat history), a `Dockerfile`, and
`requirements.txt`. Build it into your ACR with `az acr build` — no
local Docker needed:

```bash
ACR_NAME=<your-acr-name>          # the short name, not the full login server
export AGENT_IMAGE="$ACR_NAME.azurecr.io/kata-owner-agent:v1"

az acr build \
  --registry "$ACR_NAME" \
  --image kata-owner-agent:v1 \
  --file agent/Dockerfile \
  agent/
```

`$AGENT_IMAGE` is what [`sandboxtemplate.yaml`](sandboxtemplate.yaml)
substitutes via `envsubst` in the next step.

## Step 3 — Apply the template and warm pool (admin)

These two objects are typically managed by a cluster admin or platform
team and are reused across many users:

```bash
envsubst < sandboxtemplate.yaml | kubectl apply -f -
kubectl apply -f sandboxwarmpool.yaml
```

Watch the pool fill with pre-warmed Kata sandboxes:

```bash
kubectl get sandboxwarmpool kata-aks-warmpool -n sandbox-agent-demo -w
kubectl get sandboxes -n sandbox-agent-demo
kubectl get pods -n sandbox-agent-demo -o wide   # RUNTIME_CLASS column should show kata-vm-isolation
```

Confirm the NetworkPolicy was rendered for the template:

```bash
kubectl get networkpolicy -n sandbox-agent-demo
```

## Step 4 — Claim a sandbox (user)

A normal user (or the Go SDK on their behalf, see Step 6) creates a
`SandboxClaim`:

```bash
kubectl apply -f sandboxclaim.yaml
kubectl get sandboxclaim my-aks-agent -n sandbox-agent-demo -o yaml
```

The controller adopts one of the pre-warmed Kata pods into the claim
and writes the adopted sandbox name into `status.sandbox.name`. The
`SandboxWarmPool` then provisions a replacement to keep the pool at
its target size.

> **`kubectl port-forward` will not work for Kata pods.** kubelet
> implements port-forward by entering the *host* network namespace and
> dialing `127.0.0.1:<port>`, but the agent listener lives inside the
> Kata guest VM, not on the host. The connection is refused. Reach
> the agent through the in-cluster router (Step 5) or `kubectl exec`
> into the pod and curl `127.0.0.1:8080` from there.

## Step 5 — Stand up the sandbox-router

The router is a small reverse proxy. It takes incoming HTTP requests
carrying three headers — `X-Sandbox-ID`, `X-Sandbox-Namespace`,
`X-Sandbox-Port` — and forwards each request to the matching per-user
pod via the headless Service that `sandboxtemplate.yaml` instructs the
controller to create (`service: true`). It also forwards every other
header through untouched, which is how `X-Owner` reaches the agent.

The router is the **only** ingress source allowed by the template's
NetworkPolicy, so all sandbox-bound traffic must flow through it.

### Pick a router image

The router is a **separate component** from the agent you built in
Step 2. The agent is the per-user FastAPI pod that talks to Foundry;
the router is a small reverse proxy that sits in front of every agent
pod and forwards requests based on the `X-Sandbox-*` headers. Its
source lives in the main `agent-sandbox` repo (not in this directory),
and the project publishes it as a public SIG staging image so you do
not need to build it yourself:

```text
us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandbox-router:latest-main
```

This is a public Google Artifact Registry image — AKS can pull it
directly without any registry attach, credentials, or workload-identity
plumbing. For a pinned deployment, replace `latest-main` with the
project release tag you are using.

```bash
export ROUTER_IMAGE="us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandbox-router:latest-main"
```

### Deploy the router

[`router.yaml`](router.yaml) declares two Services pointed at the same
router pods — a `ClusterIP` (`sandbox-router-svc`) for in-cluster
callers and the NetworkPolicy, plus a `LoadBalancer`
(`sandbox-router-public`) that AKS fronts with an Azure Standard Load
Balancer public IP.

> ⚠️ **Demo-only authentication.** [`router.yaml`](router.yaml) sets
> `ALLOW_UNAUTHENTICATED_ROUTER=true` so the public LoadBalancer
> accepts every request unconditionally. **This is fine for a throwaway
> demo cluster you are about to delete; it is not fine for anything
> else** — anyone who finds the public IP can use the router as an open
> proxy into your sandbox pods. For a real deployment, mint a token,
> store it in a Secret, plumb it in as `ROUTER_AUTH_TOKEN`, and have
> callers send `Authorization: Bearer <token>`. The shape to copy lives
> at [`clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml`](../../../clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml).
> Note that the Go SDK used in Step 6 does not yet plumb `Authorization`
> headers, so swapping to token auth requires either an SDK change or
> bypassing the SDK for the HTTP layer.

Substitute the image and apply (`envsubst` ships in the
`gettext-base` package on most distros):

```bash
envsubst < router.yaml | kubectl apply -f -

kubectl rollout status deployment/sandbox-router-deployment -n sandbox-agent-demo
kubectl get pods -n sandbox-agent-demo -l app=sandbox-router
```

Wait for AKS to allocate the public IP (usually under a minute):

```bash
kubectl get svc sandbox-router-public -n sandbox-agent-demo -w
# When EXTERNAL-IP shows a real address, grab it:
export ROUTER_BASE_URL="http://$(kubectl get svc sandbox-router-public \
  -n sandbox-agent-demo -o jsonpath='{.status.loadBalancer.ingress[0].ip}')"
echo "$ROUTER_BASE_URL"
```

That URL is the single public entrypoint for every user's sandbox.

### Smoke-test from your laptop

Pick any pod from the warm pool and send a chat request to it,
identifying yourself with `X-Owner`:

```bash
SANDBOX=$(kubectl get sandbox -n sandbox-agent-demo \
  -l agents.x-k8s.io/warm-pool-sandbox \
  -o jsonpath='{.items[0].metadata.name}')

curl -s "$ROUTER_BASE_URL/chat" \
  -H 'Content-Type: application/json' \
  -H "X-Sandbox-ID: $SANDBOX" \
  -H 'X-Sandbox-Namespace: sandbox-agent-demo' \
  -H 'X-Sandbox-Port: 8080' \
  -H 'X-Owner: alice' \
  -d '{"prompt":"In one sentence, introduce yourself."}'
```

You should see something like:

```json
{"owner":"alice","reply":"I am alice's agent. I'm alice's personal AI assistant in a private sandbox.","history_turns":1}
```

Repeat with `-H 'X-Owner: bob'` and a different pod name — the reply
will begin with `I am bob's agent.` That `I am <owner>'s agent.`
prefix is the routing proof: it can only show the right name if the
agent really did receive the `X-Owner` header from the router, which
means the request really did land on the per-user pod you addressed.

## Step 6 — Chat with the agent via the Go SDK

For real workloads each user (or each agent process) should manage
their own `SandboxClaim` via the Go SDK rather than hand-applying
YAML. [`client/main.go`](client/main.go) is a tiny CLI that does
exactly that. It supports two modes:

- **One-shot** (default): create a fresh claim, send one prompt, tear
  the claim down on exit.
- **`-reuse`**: keep the same claim across invocations so each call
  lands on the same Kata pod — which is what unlocks multi-turn
  conversations, since the agent keeps per-owner history in process
  memory.

### Add the dependency (only if you're copying the client into your own project)

From the directory containing your `go.mod`:

```bash
go get sigs.k8s.io/agent-sandbox/clients/go/sandbox
```

The in-repo `client/` already has this wired up, so for this demo you
can just `go run ./client` from this directory.

### Point the client at the router

```bash
export ROUTER_BASE_URL="http://$(kubectl get svc sandbox-router-public \
  -n sandbox-agent-demo -o jsonpath='{.status.loadBalancer.ingress[0].ip}')"
echo "$ROUTER_BASE_URL"
```

### One-shot: claim, chat, tear down

```bash
go run ./client -name alice -msg "In one sentence, introduce yourself."
```

What happens end-to-end:

1. The SDK creates a `SandboxClaim`. The `kata-aks-warmpool` adopts a
   pre-warmed Kata pod into it, so the claim becomes Ready in a
   couple of seconds.
2. The client POSTs `/chat` to `$ROUTER_BASE_URL` with `X-Sandbox-ID`
   set to the adopted sandbox name and `X-Owner: alice`.
3. The router forwards to
   `<sandbox-name>.<namespace>.svc.cluster.local:8080`, preserving
   `X-Owner`. The agent reads it, asks the model to introduce itself
   as alice's agent, and returns the reply.
4. The client asserts the reply contains `i am alice's agent` (case
   insensitive) and fails loudly otherwise.
5. On exit the SDK deletes the claim, which cascades to the
   `Sandbox`, Pod, and headless Service. The warm pool immediately
   reconciles back to its target replica count.

Run it again with a different owner to see the routing proof:

```bash
go run ./client -name bob -msg "In one sentence, introduce yourself."
```

Bob's reply must begin with `I am bob's agent.` — if it ever names
the wrong owner, routing is broken.

### Multi-turn: `-reuse` to keep the same Kata pod across calls

```bash
go run ./client -reuse -name ryan -msg "my favorite color is teal. remember that."
go run ./client -reuse -name ryan -msg "what is my favorite color?"
go run ./client -reuse -name ryan -msg "summarize what we have discussed so far."
```

The first `-reuse` call creates a claim and caches the claim name at
`/tmp/kata-aks-client-<name>.claim`. Subsequent `-reuse` calls with
the same `-name` look up that cache, call `GetSandbox`, and re-bind
to the same pod. The agent prepends prior turns to each prompt, so
the model actually remembers what you told it. The log line includes
`turn=N` so you can see history accumulating.

Clear the conversation without destroying the sandbox:

```bash
go run ./client -reuse -name ryan -reset
```

Destroy the sandbox when you're done:

```bash
go run ./client -reuse -name ryan -delete
```

> **Caveats on multi-turn memory.** History lives in process memory
> in the agent pod. Restarting the pod (or the warm pool replacing
> it) wipes the history. The pod is shared by anyone with the same
> `X-Owner` value addressing the same Kata pod, so don't rely on
> `X-Owner` as a security boundary — the boundary is the Kata
> sandbox itself, plus the per-template NetworkPolicy.

### What about more than one user?

The same CLI scales to N users — just run it from N shells (or N
goroutines in your own program), one per owner. Each `-name` gets
its own claim, its own Kata pod, and (with `-reuse`) its own cached
history. There is nothing two-user-specific in the design; the
smoke-test commands above just happen to use two names.

## NetworkPolicy: how the lockdown works

[`sandboxtemplate.yaml`](sandboxtemplate.yaml) declares a
`networkPolicy` block, so the agent-sandbox controller renders a
single shared `NetworkPolicy` that applies to every sandbox produced
from `kata-aks-template`. The intent: **only the sandbox-router
pods may reach an agent pod; nothing else can.**

The relevant fragment:

```yaml
networkPolicyManagement: Managed
networkPolicy:
  ingress:
    - from:
      - podSelector:
          matchLabels:
            app: sandbox-router    # the proxy fleet that fronts every sandbox
      ports:
      - protocol: TCP
        port: 8080                 # the agent port
  egress:
    - ports:                       # DNS so the agent can resolve the model endpoint
      - protocol: UDP
        port: 53
      - protocol: TCP
        port: 53
    - ports:                       # HTTPS to the model endpoint
      - protocol: TCP
        port: 443
```

What this guarantees, given a NetworkPolicy-aware CNI:

- **No sandbox-to-sandbox traffic.** Alice's pod cannot connect to
  Bob's pod even though they share a namespace and a warm pool — the
  only allowed ingress source is `app=sandbox-router`.
- **No direct ingress from the LoadBalancer.** The LB Service targets
  the router pods, not the agent pods. Every external request enters
  via the router.
- **No egress to the Kubernetes API server, cloud metadata, or other
  in-cluster services.** The agent can only reach DNS and HTTPS
  endpoints, which is enough to call the model provider and nothing
  else.

Notes if you need to adjust the policy:

- If your sandbox-router pods run in a different namespace, add a
  `namespaceSelector` alongside the `podSelector` in the `from` block
  — pod-label matching alone is not cross-namespace.
- If sidecars (Istio, OTEL collector, etc.) inject into the sandbox
  pods, add their ports to the `ingress` rule or those sidecars will
  fail their health checks. The controller deliberately enforces
  default-deny ingress.
- For Cilium / other CNIs that own their own policy story, set
  `networkPolicyManagement: Unmanaged` in the template and let the
  CNI manage isolation. The controller will then skip creating any
  `NetworkPolicy` for this template.

After editing the template, re-apply it:

```bash
envsubst < sandboxtemplate.yaml | kubectl apply -f -
kubectl get networkpolicy -n sandbox-agent-demo   # confirm a policy for the template exists
```

## Cleanup

Deleting the namespace removes every object created by this example
in one shot, including the public LB IP:

```bash
kubectl delete namespace sandbox-agent-demo
```

If you used `-reuse` with the Go client, also remove the local
claim-name cache so a future run on a fresh namespace doesn't try to
adopt a dead claim:

```bash
rm -f /tmp/kata-aks-client-*.claim
```

If you would rather tear things down piecemeal:

```bash
kubectl delete -f sandboxclaim.yaml
kubectl delete -f sandboxwarmpool.yaml
envsubst < sandboxtemplate.yaml | kubectl delete -f -
envsubst < router.yaml | kubectl delete -f -
kubectl delete secret azure-foundry -n sandbox-agent-demo
rm -f /tmp/kata-aks-client-*.claim
```

If you no longer need the Kata-capable node pool, also remove it from
AKS:

```bash
az aks nodepool delete \
  --resource-group <rg> \
  --cluster-name <aks-cluster> \
  --name sandboxagent
```

## Notes on AKS Pod Sandboxing

- **Resource requests are not optional.** A Kata pod is a real
  micro-VM, and the kubelet sizes the guest VM from the sum of
  container requests/limits. The template sets explicit `cpu` and
  `memory` requests/limits to avoid guest OOM kills. Tune them for
  the model traffic you expect.
- **Cold start is slower than runc.** Booting a Kata micro-VM and
  pulling the image takes noticeably longer than a runc pod — which
  is exactly why the `SandboxWarmPool` matters here. For bursty
  workloads, drive `replicas` from an HPA on the
  `agent_sandbox_claim_creation_total` metric (the project's
  `examples/hpa-swp-scaling/` directory shows the wiring).
- **VM SKU matters.** AKS Pod Sandboxing requires Gen2 VMs with
  nested virtualisation. D-series v3/v4/v5 work; B-series burstable
  SKUs and most ARM SKUs do not. If pods stay `Pending` with a
  Kata-related scheduling error, the node pool SKU is almost always
  the cause.
- **Azure Linux only.** The `kata-vm-isolation` RuntimeClass is only
  registered on Azure Linux node pools. Ubuntu pools cannot host
  Kata sandboxes on AKS today.
- **Image pulling.** AKS Pod Sandboxing pulls images on the host (not
  inside the guest), so the standard ACR→AKS attach via
  `az aks update --attach-acr` is sufficient — no `imagePullSecrets`
  needed for either the agent image or the router image.
