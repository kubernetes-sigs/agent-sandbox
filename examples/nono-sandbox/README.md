# nono with Kubernetes Agent Sandbox

This example runs [`nono`](https://github.com/nolabs-ai/nono) inside a Kubernetes
[`Sandbox`](https://github.com/kubernetes-sigs/agent-sandbox). It demonstrates
defense in depth for an AI agent:

- agent-sandbox isolates and manages the pod;
- nono limits the agent process inside the pod;
- each delegated tool can run with a narrower policy than the agent;
- network and command decisions are recorded in a signed audit trail.

The demo starts a disposable [Grafana Loki](https://grafana.com/docs/loki/latest/)
service, seeds a payments incident, and lets the agent invoke
[LogCLI](https://grafana.com/docs/loki/latest/query/logcli/) to retrieve it.

![Architecture and workflow for the nono agent-sandbox example](assets/nono.png)

## Why another sandbox?

The two layers protect different boundaries. A container runtime or microVM
isolates the pod from the node, cluster, and neighboring workloads. nono limits
what the agent and each delegated tool can access inside that pod, including
files, credentials, network destinations, API paths, and command arguments.

If an agent or tool is exploited, the runtime contains untrusted code within the
pod while nono limits the authority available to the compromised process. The
layers complement each other rather than competing or duplicating.

Many users of nono already run it inside a container or microVM.

## Security boundaries

- **Agent filesystem:** can use `/workspace` and the bundled workload, but cannot
  read mounted secrets or protected audit state.
- **Agent network:** can call the approved OpenAI endpoint. Other hosts, methods,
  and paths are denied.
- **Agent credential:** receives a session-scoped phantom, never the provider key.
- **LogCLI invocation:** can run one fixed incident query. Changed flags, queries,
  limits, and management commands are denied.
- **LogCLI network:** can call only the Loki query-range endpoint.
- **LogCLI credential:** receives a tool-scoped Loki address and phantom token.
  The agent cannot see the source token or destination details.

The test proves command and network policy independently. Modified LogCLI
arguments are rejected before execution. A permitted `labels` invocation starts,
but its API request is rejected by the L7 policy. The exact incident query passes
both controls and returns the seeded log.

## RuntimeClass support

- **Default runc or containerd:** supported when the node kernel enables Landlock
  (LSM available in Linux 5.13+).
- **Kata Containers with `kata-qemu`:** supported out of the box. Kata adds a
  second isolation layer for the pod and node, while nono continues to constrain
  the agent and tools inside the guest. See
  [Multi-Layer Sandboxing of AI Workloads in Kubernetes](https://pradiptabanerjee.medium.com/multi-layer-sandboxing-of-ai-workloads-in-kubernetes-627dc649425b)
  from Red Hat for a reference architecture using Kata and nono together.
- **firecracker:** supported when the guest kernel enables Landlock
- **gVisor:** not currently supported because its Sentry lacks the required
  Landlock syscalls. But we are working with the gVisor team to add Landlock
  support. See [google/gvisor#13439](https://github.com/google/gvisor/issues/13439).

Kata adds pod and node isolation while nono continues to constrain the agent and
individual tools inside the guest. The cluster must already have the `kata-qemu`
RuntimeClass and the required agent-sandbox extension CRDs before using the
Kustomize variant.

## Requirements

- Docker
- kind
- kubectl
- OpenSSL
- Python 3
- tar
- an existing kind cluster, or permission to create one
- the agent-sandbox controller and `agents.x-k8s.io/v1beta1` CRDs
- a container runtime whose Linux kernel exposes Landlock

Create the demo cluster if it does not already exist:

```bash
kind create cluster --name agent-sandbox --config kind-config.yaml
```

The smoke-test script does not create or delete the cluster. Install the
agent-sandbox controller and CRDs into it:

```bash
AGENT_SANDBOX_VERSION=v0.5.4
kubectl apply -f \
  "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml"
kubectl rollout status deployment/agent-sandbox-controller \
  --namespace agent-sandbox-system --timeout=120s
```

Then check the cluster and APIs:

```bash
kind get clusters
kubectl cluster-info --context kind-agent-sandbox
kubectl api-resources --api-group=agents.x-k8s.io
kubectl api-resources --api-group=extensions.agents.x-k8s.io
```

The kind node image does not provide its own kernel. A kind node is a container,
so it uses the Docker host kernel on Linux or the Docker Desktop VM kernel on
macOS and Windows. No special kind node image is required for a typical current
setup. The test detects missing Landlock support and nono fails closed rather
than running without filesystem enforcement.

## Run the demo

From this directory:

```bash
KIND_CLUSTER_NAME=agent-sandbox ./run-test-kind.sh
```

The script:

1. builds the agent image and loads it into kind;
2. creates short-lived audit and Loki TLS identities;
3. starts Loki and seeds a synthetic incident;
4. creates the Sandbox and runs the policy checks;
5. finalizes and verifies the signed audit session;
6. exports a portable audit bundle and removes temporary resources.

A successful run includes:

```text
[tool-ok] exact LogCLI incident query returned the seeded Loki log
[tool-ok] L7 policy blocked LogCLI from the labels endpoint
[tool-ok] invocation policy blocked altered LogCLI arguments
[tool-ok] invocation policy blocked LogCLI deletion management
[audit-ok] event chain, Merkle root, and audit ledger verified
[audit-ok] DSSE signature matched the pinned public key
```

The script retains the audit PVC by default. To remove it during cleanup:

```bash
KEEP_AUDIT_PVC=false ./run-test-kind.sh
```

Use `AUDIT_ARTIFACT_ROOT` to select a different local export directory.

## Credentials

No real API key is required for the offline demo. The checked-in Kubernetes
Secret contains non-functional OpenAI and Loki placeholders.

nono reads the source credential in the trusted supervisor and gives the agent a
random session-scoped phantom. For an approved request, the proxy validates the
phantom and injects the source credential immediately before forwarding. The raw
provider key is not exposed to the agent process.

The local Loki service has authentication disabled. Its placeholder token still
exercises the same tool-scoped credential flow used with an authenticated Loki
gateway. The outer agent receives neither the Loki address nor its token. Those
values are available only to the approved LogCLI child.

For production, replace the demo Secret through your normal secret manager. Do not
commit real credentials. Restrict Secret RBAC and pod debug access, and enable
Kubernetes encryption at rest.

## Loki TLS

Loki uses HTTPS because nono rejects plaintext managed-credential upstreams that
are not on loopback. Each run creates a temporary private CA and a separate
CA-signed certificate for the Loki service. nono receives only the CA certificate
as its trust anchor. All TLS private keys are removed during cleanup.

The kind node pulls the pinned Loki image for its own platform. The script does
not load the multi-platform Loki image through `kind load docker-image`, which
avoids a known missing-blob failure with some Docker Desktop image stores. See
[kind issue #3795](https://github.com/kubernetes-sigs/kind/issues/3795).

## Signed audit

The demo records network decisions, command-policy decisions, executable identity,
session lifecycle, and sandbox metadata. It commits the event stream and writable
state with Merkle roots, then signs the completed session using a short-lived P-256
key.

The signing key is available only to the trusted supervisor. Verification uses the
separately retained public key. Successful runs export an ignored local directory:

```text
audit-artifacts/<run-id>/
```

The script prints a verification command similar to:

```bash
XDG_STATE_HOME=./audit-artifacts/<run-id>/state \
  nono audit verify <session-id> \
  --public-key-file ./audit-artifacts/<run-id>/audit-signing-public.pem
```

Protect exported audit data as security-sensitive operational metadata. For
production, store finalized sessions in immutable external storage and manage the
signing identity outside the workload cluster.

## Troubleshooting

### Sandbox resource is unknown

Install the agent-sandbox CRDs and controller before running the example:

```text
no matches for kind "Sandbox" in version "agents.x-k8s.io/v1beta1"
```

### kind reports no nodes

Confirm that `KIND_CLUSTER_NAME` matches an existing cluster shown by
`kind get clusters`.

### Landlock is unavailable

Use a node or Kata guest kernel with Landlock enabled. nono intentionally fails
closed instead of running without filesystem enforcement.

### LogCLI reports Bad Gateway

Temporarily change `nono_proxy::reverse=error` to
`nono_proxy::reverse=warn` in the Sandbox environment. The next run will report
the underlying TLS, DNS, or connection error.

Expected policy denials are presented as `[tool-ok]` or `[policy-ok]`. Their full
structured records remain available in the signed audit trail.
