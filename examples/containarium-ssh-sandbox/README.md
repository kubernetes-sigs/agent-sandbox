# Containarium SSH Sandbox Example

This example demonstrates an **SSH-reachable, MCP-native access pattern** for
the `Sandbox` CRD: the agent holds only an SSH key, never a kube-apiserver
token or cluster credentials. Every SSH session is pinned by a forced command
straight into an MCP server, so the key can start that server and nothing
else — see [See also](#see-also) for the specific runtime image this example
runs.

## Why SSH-based access?

Most agent runtimes reach a sandbox via `kubectl exec`, which requires the
agent (or its orchestrator) to hold cluster credentials. This example shows an
alternative access pattern layered on top of the `Sandbox` CRD.

**Where the cluster credential is actually spent matters.** The sibling
[mcp-server-sandbox](../mcp-server-sandbox) example in this repo is explicit
about its own tradeoff: `kubectl exec` is its MCP transport, so — by that
example's own README — "every MCP request from the host hops through
`kubectl exec` → kube-apiserver," meaning a cluster-scoped credential is
spent on *every single agent call, for the life of the session*. Reaching
this example's box (via `kubectl port-forward`, a `NodePort`, or the
sshpiper gateway below) is a one-time, **operator-side** exposure step, no
different in kind from setting up any bastion or tunnel — it still needs
someone with cluster access to provision. What changes is what the *agent*
holds afterward: from that point on, every MCP call the agent makes goes
over the SSH key alone, and that credential surface doesn't grow with usage
or ever touch kube-apiserver again. That's the property being traded for:
not "zero cluster access anywhere," but "the agent's ongoing credential
doesn't scale with the number of calls it makes, and can't be used for
anything cluster-scoped even if it leaks."

Concretely:

- **SSH as the transport, hard-pinned by a forced command**: the agent
  connects with a plain SSH keypair to [dropbear](https://matt.ucc.asn.au/dropbear/dropbear.html)
  (not OpenSSH — it runs cleanly rootless with no privsep/PAM dance). Every
  session is forced (`-c /usr/local/bin/agent-box`) straight into the MCP
  server: the key can start `agent-box` and nothing else, no matter what
  command the client sends. The blast radius of a leaked credential is one
  box's MCP surface, not a shell on the cluster.
- **MCP inside the box**: Containarium's `agent-box` binary runs in the
  container and exposes typed tools (`shell_exec`, `read_file`, `write_file`,
  `list_directory`, `move_file`, `delete_file`, plus process and log tools)
  over stdio — reachable by wrapping the MCP command in SSH. Any MCP-speaking
  agent (Claude Code, Cursor, OpenCode) works with zero client library.
- **Sandbox CRD as the substrate**: stable identity, persistent workspace via
  `volumeClaimTemplates`, and lifecycle management come from agent-sandbox;
  `automountServiceAccountToken: false` keeps the box credential-free.

Because every session is forced into `agent-box`, plain `ssh host 'echo ok'`
style commands won't do what you expect — see "Reach the box over SSH" below.

## Prerequisites

- A Kubernetes cluster (e.g., [kind](https://kind.sigs.k8s.io/)).
- `agent-sandbox` controller installed
  ([installation guide](https://agent-sandbox.sigs.k8s.io/docs/getting_started/)).
- `ssh`, `ssh-keygen`, and `kubectl` (Usage step 1 generates the keypairs).
- (Optional) A RuntimeClass such as `gvisor` for stronger isolation; the
  manifest includes a commented-out `runtimeClassName` line.

## Building agent-box from source

By default every manifest here pins the published
`ghcr.io/footprintai/containarium-agent-box:v0.52.1` tag. If you'd rather not
trust a pre-built image sight unseen, [`Dockerfile`](Dockerfile) builds the
same image from source — it clones
[Containarium](https://github.com/FootprintAI/Containarium) at that same
release tag and reproduces `images/agent-box/Dockerfile` there stage-for-stage
(rootless dropbear, forced command into `agent-box`, nothing else):

```bash
docker build -t containarium-agent-box:local .
kind load docker-image containarium-agent-box:local --name <your-kind-cluster>
```

Then point the manifests at your build instead of the published tag — either
edit `image:` directly in `containarium-sandbox.yaml` / `gateway-demo.yaml`,
or override it at apply time without touching the files:

```bash
sed 's#ghcr.io/footprintai/containarium-agent-box:v0.52.1#containarium-agent-box:local#' \
  containarium-sandbox.yaml | kubectl apply -f -
```

(and the same `sed` against `gateway-demo.yaml`, which pins the tag twice —
once per box). To build a different upstream release, pass
`--build-arg CONTAINARIUM_REF=vX.Y.Z` to `docker build`.

## Usage

`run-test-kind.sh` runs this whole sequence on kind — the gateway path plus a
local smoke test — and verifies each with a real MCP handshake. To do it by
hand:

**1. Generate the keys and create the Secrets.** Three keypairs: the agent's
(client), the gateway's SSH host key, and the gateway→box upstream key. The
box authorizes **only the upstream key** — the agent's key lives at the
gateway, never on the box. That separation is the whole point (see
[Why SSH-based access?](#why-ssh-based-access)).

```bash
ssh-keygen -t ed25519 -f ./agent_ed25519       -N ""   # agent  → gateway
ssh-keygen -t ed25519 -f ./piper_host_ed25519  -N ""   # gateway host key
ssh-keygen -t ed25519 -f ./upstream_ed25519    -N ""   # gateway → box

# the box trusts only the gateway's upstream key
kubectl create secret generic containarium-ssh-key \
  --from-file=authorized_keys=./upstream_ed25519.pub
# the gateway authenticates clients against the agent's key
# (each Pipe references this Secret by name)
kubectl create secret generic agent-authorized-keys \
  --from-file=authorized_keys=./agent_ed25519.pub
# the gateway's own host key and its upstream (gateway -> box) key
kubectl create secret generic sshpiper-server-key \
  --from-file=server_key=./piper_host_ed25519
kubectl create secret generic sshpiper-upstream-key --type=kubernetes.io/ssh-auth \
  --from-file=ssh-privatekey=./upstream_ed25519
```

**2. Deploy the gateway and the boxes.** The SSH username selects the box, so
create two and route each with a `Pipe`:

```bash
# the sshpiper Pipe CRD, then the gateway
kubectl apply -f https://raw.githubusercontent.com/FootprintAI/Containarium/v0.52.1/charts/containarium-k8s/crds/pipe.yaml
kubectl apply -f sshpiper.yaml

# two boxes (box-a, box-b) and their routing rules — plain manifests, as-is
kubectl apply -f gateway-demo.yaml
# the controller creates each Pod a moment after its Sandbox CR; wait (bounded,
# ~60s) for the Pod to exist before waiting on readiness, or `kubectl wait` can
# error with "no matching resources found" on a fresh apply
for b in box-a box-b; do
  for _ in $(seq 30); do
    kubectl get pod --selector="sandbox=$b" -o name | grep -q . && break
    sleep 2
  done
  kubectl wait --for=condition=ready pod --selector="sandbox=$b" --timeout=120s
done
```

`gateway-demo.yaml` is two `Sandbox`es plus two `Pipe`s, no templating — each
Pipe points `from.authorized_keys_secret` at the `agent-authorized-keys`
Secret above, so the agent's key isn't baked into the manifest.

**3. Reach a box.** One public SSH endpoint, plain TCP, no kubectl in the
path — the username *is* the box:

```bash
ssh -i agent_ed25519 -o StrictHostKeyChecking=accept-new \
  -p 32022 box-a@<node-or-lb-address>
```

This does **not** open a shell: the box's dropbear is pinned by a forced
command to `agent-box`, so the session *is* the MCP server. Confirm it with
one round of MCP:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0.0.1"}}}' \
  | ssh -i agent_ed25519 -o StrictHostKeyChecking=accept-new \
      -p 32022 box-a@<node-or-lb-address>
# -> {"result":{...,"serverInfo":{"name":"containarium-agent-box",...}}}
```

**4. Point an MCP agent at it.** The per-box config is just the username — no
per-box tunnels:

```json
{
  "mcpServers": {
    "box-a": { "command": "ssh", "args": ["-i", "agent_ed25519", "-o", "StrictHostKeyChecking=accept-new", "-p", "32022", "box-a@<gateway>"] },
    "box-b": { "command": "ssh", "args": ["-i", "agent_ed25519", "-o", "StrictHostKeyChecking=accept-new", "-p", "32022", "box-b@<gateway>"] }
  }
}
```

### Local smoke test, without the gateway

For a one-box check on kind you can skip the gateway and `kubectl
port-forward` straight to the pod. This is a **local convenience, not the
access model**: the machine running port-forward already holds a kubeconfig,
so the SSH layer buys you nothing here, and it doesn't work under gVisor
([agent-sandbox#158](https://github.com/kubernetes-sigs/agent-sandbox/issues/158)).
In this mode the box must authorize the **agent's** key directly — there is no
gateway to bridge it, so it uses its own Secret (`direct-box-authorized-keys`),
distinct from the gateway boxes, which trust only the upstream key:

```bash
kubectl create secret generic direct-box-authorized-keys \
  --from-file=authorized_keys=./agent_ed25519.pub
kubectl apply -f containarium-sandbox.yaml       # a single box: containarium-ssh-sandbox
# wait (bounded, ~60s) for the Pod to exist before waiting on readiness (see above)
for _ in $(seq 30); do
  kubectl get pod --selector=sandbox=containarium-ssh-sandbox -o name | grep -q . && break
  sleep 2
done
kubectl wait --for=condition=ready pod --selector=sandbox=containarium-ssh-sandbox --timeout=120s

kubectl port-forward pod/containarium-ssh-sandbox 2222:2222 &
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0.0.1"}}}' \
  | ssh -i ./agent_ed25519 -o StrictHostKeyChecking=accept-new \
      -p 2222 agent@localhost
```

> **Why not `ssh agent@box-a.<namespace>.svc.cluster.local` directly?** That
> name is cluster-internal: CoreDNS resolves it and the pod IP sits on the
> cluster's pod network, so it's reachable only from *inside* the cluster —
> which is exactly how the gateway (itself a pod) reaches it. And in the
> gateway topology the box authorizes only the gateway's key, not yours, so
> even from inside the cluster your agent key won't authenticate. The gateway
> is what bridges both gaps — cluster edge and credential.

## How the gateway works

`ssh <box>@<gateway>` crosses the cluster edge and routes to the right pod by
username, over a two-hop key chain where each key does exactly one thing:

```text
agent --(agent key)--> sshpiper :22 / NodePort --(upstream key)--> sandbox pod :2222
                       routes by username              the only key the box trusts
```

The agent's key starts a session at the gateway under one username; the
gateway's upstream key — a Secret only sshpiper reads — is what the box's
dropbear authorizes; and every session still lands in the forced-command MCP
server. Compare `kubectl exec` as a transport: the client spends a
cluster-scoped credential on *every* call, forever. Here the cluster
credential is spent once, by the operator, at deploy time — the agent's
ongoing credential is a single SSH key that can do nothing cluster-scoped even
if it leaks.

## Hardening notes

- Constrain the agent's file operations by setting `AGENTBOX_ROOT` in the
  container env; all `agent-box` file-ops paths are then resolved against
  that root.
- Pair the Sandbox with a default-deny NetworkPolicy (see
  [composing-sandbox-nw-policies](../composing-sandbox-nw-policies)) so the
  box can't reach the cluster network even with shell access.
- Give the gateway and its boxes a dedicated namespace. The sshpiper Role
  (`sshpiper.yaml`) grants `get`/`list`/`watch` on Secrets in its own
  namespace — it has to, since it resolves the Secret each `Pipe` references at
  connect time. In a shared namespace like `default` that reach extends to
  every Secret there; isolating the example in its own namespace scopes the
  Role's blast radius to just the gateway and box Secrets.
- Pin the image to a digest in production.
- In the gateway topology, pin the box's host key in the Pipe via
  `known_hosts_data` instead of `ignore_hostkey: true` (used in the Pipes
  for demo brevity) — it closes the gateway→box MITM window.
- For a stable host key (so an SSH gateway/client can pin it across pod
  restarts instead of trusting-on-first-use every time), mount an OpenSSH
  keypair Secret at `/etc/agent-box-hostkey/{host_key,host_key_rsa}` — the
  entrypoint converts and uses it if present, otherwise it generates a fresh
  ephemeral host key on every start. Omitted from this example for
  simplicity; see `images/agent-box/entrypoint.sh` in Containarium.

## Cleanup

```bash
# gateway path
kubectl delete --ignore-not-found -f sshpiper.yaml
kubectl delete --ignore-not-found -f gateway-demo.yaml
kubectl delete --ignore-not-found secret containarium-ssh-key agent-authorized-keys sshpiper-server-key sshpiper-upstream-key

# or the local smoke test
kubectl delete --ignore-not-found -f containarium-sandbox.yaml
kubectl delete --ignore-not-found secret direct-box-authorized-keys
```

## See also

This example runs [Containarium](https://github.com/FootprintAI/Containarium)'s
`agent-box` image as the MCP runtime (dropbear + forced command, as described
above) — see [Building agent-box from source](#building-agent-box-from-source)
for how it's built. Containarium's own Kubernetes backend automates this same
access pattern at fleet scale — one `Pipe` per box, key Secrets kept in sync
as boxes come and go, a sentinel fronting many clusters behind one public
address. See its
[K8s agent-box runtime design doc](https://github.com/FootprintAI/Containarium/blob/main/docs/K8S-AGENT-BOX-RUNTIME-DESIGN.md)
if you want that context.
