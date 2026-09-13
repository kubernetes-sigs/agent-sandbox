# Execution-Scoped Token Example

A runnable answer to
[#1580](https://github.com/kubernetes-sigs/agent-sandbox/issues/1580) — the
authorization–execution lifetime mismatch — for one credential class: model
provider API keys.

One long-lived `Sandbox` runs two executions back to back. Each gets its own
short-lived credential, delivered through `sandboxd`'s
`ProcessConfig.env_vars`, and that credential is **revoked when the process
exits** rather than left to expire. The box is never destroyed. The real
provider key never enters the box at all.

`run-test-kind.sh` proves four things on kind and prints each with the status
code and response body it actually observed:

| # | Check | Passes when |
|---|-------|-------------|
| 1 | accepted during the run | the gateway accepts the run's token and the response is the **provider's**, not one of the gateway's own refusals |
| 2 | direct egress blocked | from inside the sandbox, a direct TCP connect to the provider's address fails, while the same probe reaches the gateway |
| 3 | dead after exit | that **same** token, replayed after the process exited, gets `401 gateway token revoked` — with ~30 minutes of validity still left on it |
| 4 | reuse is safe | a second run in the **same** `Sandbox` gets a new `run_id` and a working token of its own, while the first one's stays dead |

## The problem, and the shape of the fix

A credential provisioned into a reusable sandbox lives as long as the sandbox,
not as long as the execution that justified it. Two runs in one box end up with
the union of both runs' authority, and a credential from a finished run stays
usable by whatever runs next.

Three things have to be true to close that, and this example does all three
because any two of them still leave a hole:

1. **The real key is never in the box.** It lives in a credential proxy — here,
   Containarium's standalone model gateway — which verifies the caller's scoped
   token, injects the real key, and proxies upstream. What the box holds is
   worthless anywhere else.
2. **The token reaches one process, not the pod.** It goes in
   `ProcessConfig.env_vars` on `ProcessService.Start`. Not the `Sandbox` spec,
   not the `SandboxClaim`, not a mounted `Secret`, not the workspace volume —
   so it is not readable via `get pod`, and the next run does not inherit it.
3. **The token is revoked at `ExitEvent`, not left to expire.** Removing a
   variable from a process's environment does not invalidate a copy that
   already left the process. Only revocation does. Check 3 makes this concrete:
   it replays a copy of the token that never went back into the sandbox, and
   the gateway refuses it while ~30 minutes of its TTL remain.

Dropping (1) means a leaked key is a real key. Dropping (2) means the
credential outlives the run by the life of the pod. Dropping (3) means it
outlives the run by its TTL — which is the specific gap
[#1580](https://github.com/kubernetes-sigs/agent-sandbox/issues/1580)
describes and the one this example is really about.

### On fresh sandboxes vs. reuse

The maintainers' guidance on #1580 is that claiming a fresh instance from a
`SandboxWarmPool` is the intended pattern, because reuse leaks more than
credentials — processes, disk state, installed packages — and that is correct.
This example is not an argument against that. It is for the cases where reuse
is deliberate (a warm box whose expensive setup you want to keep across runs),
and it addresses only the credential dimension of the leak. The other
dimensions are [what it does not do](#what-this-example-does-not-do).

## Prerequisites

- [kind](https://kind.sigs.k8s.io/), `kubectl`, `helm`, `go`, and `getent`.
- Outbound network access from the cluster: the gateway proxies to the real
  provider, and the host resolves the provider's address for check 2.
- **No provider API key is required.** See
  [Reading check 1 without a provider key](#reading-check-1-without-a-provider-key).

You do **not** need an existing cluster; `run-test-kind.sh` creates one. You
also do not need to install the agent-sandbox controller first — the script
does that too, and skips both steps if they are already in place.

### Why Cilium, and not a plain kind cluster

A `NetworkPolicy` only does something if the cluster's CNI enforces it. kind's
default CNI (kindnet) **does not**: it accepts the object and drops nothing. On
a default kind cluster check 2 would pass without the policy having any effect
— a check that cannot fail, which is worse than no check.

So `kind-config.yaml` sets `disableDefaultCNI: true` and `run-test-kind.sh`
installs Cilium over it.
[`examples/demo-cilium-egress`](../demo-cilium-egress) is the precedent in this
repo for reaching for Cilium when an egress claim has to actually hold.

The probe hardens this further: it dials the provider **by IP** (resolved on
the host, passed in, never resolved inside the box) so a DNS failure cannot
masquerade as enforcement, and it runs the identical probe against the
*allowed* gateway first. The runner requires that positive control to succeed
before it will call check 2 a pass.

> On hosts with a reduced capability bounding set (nested containers, some CI
> runners) Cilium's `clean-cilium-state` init container fails with `unable to
> apply caps: operation not permitted`, because it asks for `CAP_SYS_MODULE` by
> name. Set
> `CILIUM_HELM_EXTRA_ARGS='--set securityContext.privileged=true'` to make
> Cilium take what the node actually has instead of naming capabilities.

## Usage

```bash
./run-test-kind.sh
```

That is the whole thing. It creates the kind cluster, installs Cilium and the
agent-sandbox controller, generates a throwaway HMAC secret and gateway admin
token, deploys the gateway and the `Sandbox`, asserts the box holds no
credentials of its own, and runs `runner/`.

Useful overrides:

| Variable | Default | Purpose |
|---|---|---|
| `PROVIDER_API_KEY` | a placeholder | A real key makes check 1 show `HTTP 200`. |
| `KEEP_CLUSTER` | `false` | Keep the kind cluster after the run. |
| `KIND_CLUSTER_NAME` | `execution-scoped-token` | Reuses the cluster if it already exists. |
| `CILIUM_HELM_EXTRA_ARGS` | empty | See the note above. |
| `NAMESPACE` | `default` | Namespace for the gateway, the Sandbox, and the policy. |

To drive it by hand instead, the pieces are: apply `gateway.yaml`,
`sandbox.yaml` and `networkpolicy.yaml`; port-forward `svc/model-gateway` and
the sandbox pod's `:9090`; then
`go run ./examples/containarium-execution-scoped-token/runner --help`.

## Files

| File | What it is |
|---|---|
| `kind-config.yaml` | kind cluster with the default CNI **disabled**, for Cilium. |
| `gateway.yaml` | The credential proxy: `Deployment` + `Service`. The only pod holding a real provider key. |
| `sandbox.yaml` | The `Sandbox`: `sandboxd`, `automountServiceAccountToken: false`, and deliberately **no** `env:` block. |
| `networkpolicy.yaml` | Sandbox egress restricted to the gateway `Service` and cluster DNS. Nothing else. |
| `runner/` | Mint → `Start` with the token in `env_vars` → read to `ExitEvent` → revoke. Then asserts the four checks. |
| `run-test-kind.sh` | Brings the whole thing up on kind and runs the runner. |
| `Dockerfile` | Builds the gateway from source at the pinned tag, if you would rather not run a published image. |

## Why the runner uses the generated `processv1` stubs directly

The runner talks to `sandboxd` through
`sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1` rather than the
higher-level [`clients/go/sandbox`](../../clients/go/sandbox) package, and the
reason is specific to this example.

The high-level client's only hook for environment variables is `Options.Env`,
which injects them into the `SandboxClaim`. That puts the token in the pod spec
for the life of the sandbox — readable by anyone with `get pod`, and inherited
by every later run in that box. That is exactly the anti-pattern this example
exists to replace, so it would quietly invert the thing being demonstrated.
`ProcessConfig.env_vars` belongs to one process invocation and ends with it.

The high-level client is still the right tool for creating and deleting the
`Sandbox` claim itself. This example uses a plain manifest for that, matching
the other manifest-driven examples here.

### Suggested follow-up upstream

A `WithEnv(map[string]string)` call option on the high-level client's
`Commands.Run` — per-call, landing in `ProcessConfig.env_vars` rather than in
the claim — would let the high-level client express this pattern too, and would
make this example's drop to the generated stubs unnecessary. It looks like a
small, self-contained addition; happy to send it as a separate PR if
maintainers want it.

## Why the `sandboxd` image is pinned by digest

`sandboxd` has no released image. As of this PR the latest release (`v1.0.2`)
ships manifests only, and `registry.k8s.io/agent-sandbox/sandboxd` has no
`v1.0.2` tag. The only published builds are on the k8s staging registry, where
the moving `latest-main` tag sits alongside per-commit tags.

`sandbox.yaml` therefore pins the **per-commit staging tag plus its digest**:

```
us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandboxd:v20260912-v1.0.2-17-g3565f96@sha256:0cf52cfc…
```

The two together say which commit built it (`g3565f96`) *and* guarantee the
bytes cannot change under a reader who runs this next month — which
`latest-main` alone does not. The alternative, building `sandboxd` from
`packages/sandboxd/Dockerfile` at a tagged commit, works and needs no
registry, but it makes the example's happy path depend on a local image build
and a `kind load`; the digest pin keeps `run-test-kind.sh` a single command.
**When a release image exists, this pin should become that release tag** — it
is the one line in this example that is expected to change.

The gateway image is a normal published release tag,
`ghcr.io/footprintai/containarium-model-gateway:v0.78.1`. `Dockerfile` builds
the identical binary from source at that same tag if you would rather not run a
prebuilt image, or if your cluster cannot reach that registry.

## Reading check 1 without a provider key

Check 1 asserts that the **gateway** accepted the run's token. It deliberately
does not assert `HTTP 200`, because with no real provider key the provider
itself answers `401` — and a status-only assertion would confuse "the gateway
refused the credential" with "the provider refused the key".

The discriminator is the body. The gateway's own refusals are plain text and
finite:

```
missing gateway token
invalid gateway token: …
token not valid for provider …
gateway token revoked
gateway holds no key for provider …
```

Anything else means the gateway verified the token, injected the real key, and
forwarded the call — so the response came from the provider. Without a key that
looks like:

```
HTTP 401 — {"type":"error","error":{"type":"authentication_error","message":"API key is invalid."},...}
```

which is unmistakably the provider's, and is what check 1 passes on. Set
`PROVIDER_API_KEY` to a real key and the same check shows `HTTP 200`. The
runner prints the full refusal list alongside the observed body so a reader can
verify the distinction rather than take it on trust.

## What this example does **not** do

It handles credentials, and only credentials. It does **not**:

- **Reset the filesystem between runs.** Whatever run 1 wrote to `/workspace`
  is still there for run 2.
- **Kill processes left behind by a run.** A backgrounded process from run 1
  keeps running — and keeps whatever it already read out of its environment,
  including the token. Revocation is what makes that copy useless; nothing here
  stops it from existing.
- **Reset the environment block, restart language kernels, or release bound
  ports.**

Those are `Setup()` / `Clean()` in
[KEP-539.2](../../docs/keps/539.2-runtime-standardization/README.md#reusable-sandboxes-setup--cleanup),
not this example's job, and the right layer for them is the runtime. What this
example does suggest for that specification is one addition to `Clean()`'s
implementation considerations, beside "Environment Variables: Resetting the
environment block": **revoke** every credential issued for the run, rather than
only removing it from the environment — because removal from an environment
block does not invalidate a copy that already left it.

It also covers exactly one credential class. Git tokens, cloud storage
credentials and the rest need the same three properties, but the proxy that
holds them is not this one.

## Hardening notes

- **Give this its own namespace.** The example uses `default` for brevity. The
  egress policy selects on a pod label, so a second workload in the same
  namespace is unaffected by it — which is not what you want.
- **Pin images by digest in production** (the `sandboxd` image already is).
- **Pair this with a `default-deny` egress policy** for the namespace, so a pod
  that is missing the label fails closed rather than open. See
  [composing-sandbox-nw-policies](../composing-sandbox-nw-policies).
- **Restrict ingress too.** This policy sets `policyTypes: [Egress]` only.
- **The HMAC secret is the trust root** between the minter and the gateway.
  Here `run-test-kind.sh` generates a throwaway one; in production it is a
  rotated secret that the sandbox never sees. Note that the sandbox only ever
  receives an already-signed token, never the secret.
- **Consider a shorter TTL as well.** Revocation and expiry are complementary:
  expiry bounds the damage when the revoke call itself fails. The 30-minute TTL
  here is deliberately long so that check 3 cannot pass by accident.
- **Make the revoke path fail loudly.** The runner revokes in a `defer` so
  every exit path — including errors and cancellation — ends the credential,
  under a context detached from the run's own, so a cancelled run still
  revokes. A revoke that fails needs to be alarming, not logged and forgotten.

## Cleanup

`run-test-kind.sh` cleans up after itself: it deletes the cluster it created,
or (with `KEEP_CLUSTER=true`, or when reusing a cluster you already had) just
the objects it applied. By hand:

```bash
kubectl delete --ignore-not-found -f networkpolicy.yaml -f sandbox.yaml -f gateway.yaml
kubectl delete --ignore-not-found secret model-gateway-auth model-gateway-provider-keys
kind delete cluster --name execution-scoped-token
```

## See also

- [#1580](https://github.com/kubernetes-sigs/agent-sandbox/issues/1580) — the
  feature request this example answers, including the maintainer guidance it
  follows.
- [KEP-539.2](../../docs/keps/539.2-runtime-standardization) — `sandboxd`, and
  the `Setup()` / `Clean()` primitives this example deliberately stays out of.
- [nono-sandbox](../nono-sandbox) — credential brokering plus filesystem and
  network enforcement inside the box, the complement to this example's
  process-scoped delivery.
- [demo-cilium-egress](../demo-cilium-egress) — per-identity egress policy with
  Cilium, at more depth than the single policy here.
- [containarium-ssh-sandbox](../containarium-ssh-sandbox) — the sibling
  example: keeping cluster credentials out of the agent's hands.
- [Containarium](https://github.com/FootprintAI/Containarium) — the runtime the
  gateway comes from. `internal/modelgateway` is the proxy, and
  `internal/runlease` is the mint-and-revoke-per-run lifecycle this example's
  runner reproduces in miniature.
