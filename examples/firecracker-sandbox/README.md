# Firecracker Sandbox

## Overview

This example shows how to run an Agent Sandbox on top of
[Kata Containers with the Firecracker VMM](https://github.com/kata-containers/kata-containers)
(`kata-fc`). The result is a microVM-isolated sandbox that boots in roughly
**125 ms** with a minimal memory footprint — a much better density story than
the QEMU-based `kata-qemu` runtime for serverless-style workloads.

The sandbox container runs a small Python runtime (a
[FastAPI](https://fastapi.tiangolo.com/) server on port `8888`) that exposes
an HTTP API for driving the sandbox — see the [Architecture](#architecture)
diagram below for the full endpoint list.

## Architecture

```text
   Control plane / HTTP client
          │
          │  HTTP (port 8888)
          │  ───── port-forward ─── OR ─── sandbox-router (X-Sandbox-ID) ───┐
          │                                                                 │
          ▼                                                                 │
   Firecracker Pod (kata-fc)                                                │
   ┌──────────────────────────────┐                                         │
   │  sandbox runtime (main.py)   │◀────────────────────────────────────────┘
   │  - GET  /health              │
   │  - GET  /metrics             │
   │  - POST /init                │
   │  - GET  /envs                │
   │  - GET  /files               │
   │  - POST /files               │
   │  - POST /exec                │
   └──────────────────────────────┘
          ▲
          │
   agent-sandbox controller
   (creates SandboxClaim / Sandbox from SandboxTemplate + SandboxWarmPool)
```

## Prerequisites

1. A **Linux** node with **hardware virtualization (KVM)** — this is a hard
   requirement of Kata Containers + Firecracker; **macOS and Windows hosts
   cannot run kata-fc** (they do not expose `/dev/kvm`). You can verify on
   the target node with `ls /dev/kvm` and `kvm-ok` (or
   `egrep -c '(vmx|svm)' /proc/cpuinfo` for CPU flag support). On GCP this
   means an **N2** instance with
   [nested virtualization enabled](https://cloud.google.com/compute/docs/instances/nested-virtualization/managing-constraint);
   on bare metal, enable VT-x or AMD-V in the BIOS and load the KVM kernel
   modules (`modprobe kvm && modprobe kvm_intel` or `kvm_amd`).
2. A **container runtime with devmapper** (or another thin-provisioning
   snapshotter that Firecracker can use). Firecracker does **not** support
   overlayfs / virtio-fs for the rootfs, so a stock `containerd` with the
   default overlayfs snapshotter will fail at pod-sandbox creation
   (`FailedCreatePodSandBox ... ENOENT`). On a custom node, configure
   containerd's `[plugins."io.containerd.snapshotter.v1.devmapper"]` or use
   a kata-deploy distribution that bundles a compatible snapshotter. You can
   verify with `ctr plugins ls | grep devmapper` and require the status to
   be `ok`.
3. A Kubernetes cluster with the agent-sandbox controller installed
   (see the top-level [`README.md`](../../README.md)).
4. [`kubectl`](https://kubernetes.io/docs/tasks/tools/) configured to talk to
   the cluster.
5. Docker (or compatible builder) to build the runtime image, and a way to
   push it somewhere the cluster can pull from.

## Step 1 — Install the agent-sandbox controller

Follow the [installation guide](../../README.md#installation) to install the
controller and CRDs into the cluster.

## Step 2 — Prepare the nodes

Run [`setup.sh`](./setup.sh) on a **Linux host with KVM access** to install
Kata Containers via `kata-deploy`, register the `kata-fc` RuntimeClass, and
label the nodes:

```shell
./setup.sh                       # defaults: kata 3.2.0, label kata-firecracker=true
./setup.sh --kata-version 3.6.0  # or pin a specific version
```

The script verifies `/dev/kvm` is present before doing anything destructive.

## Step 3 — Build and push the runtime image

```shell
export IMAGE=<registry>/firecracker-sandbox:latest
docker build -t ${IMAGE} .
docker push    ${IMAGE}
```

For a local kind cluster you can skip the push and just `kind load docker-image`:

```shell
kind load docker-image ${IMAGE}
```

## Step 4 — Create the SandboxTemplate and Warmpool

```shell
envsubst < sandbox-template-firecracker.yaml | kubectl apply -f -
```

This creates:

* `SandboxTemplate/firecracker-runtime-template` — the pod spec that will be
  materialized for each sandbox.
* `SandboxWarmPool/firecracker-runtime-warmpool` — keeps 2 warm microVMs ready
  for instant claim.

Verify the pool is populated:

```shell
kubectl get sandboxtemplate,sandboxwarmpool,sandbox
```

## Step 5 — (Optional) deploy a single bare Sandbox

If you just want a one-off sandbox without the warm pool:

```shell
envsubst < sandbox-firecracker.yaml | kubectl apply -f -
kubectl get sandbox firecracker-example
```

## Step 6 — Verify the runtime

Confirm the pod is running under `kata-fc`:

```shell
kubectl get pod -l sandbox=firecracker -o jsonpath='{.items[0].spec.runtimeClassName}'
# expected: kata-fc
```

## Step 7 — Run the verification script

The script talks to the HTTP API exposed by the sandbox pod over plain
HTTP (no SDK dependencies):

```shell
# Port-forward the runtime endpoint of the warm pod
POD=$(kubectl get pod -l sandbox=firecracker -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward pod/$POD 8888:8888 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null' EXIT

# Wait until the local endpoint is reachable before running the client
for i in $(seq 1 30); do
  curl -sf http://127.0.0.1:8888/health >/dev/null 2>&1 && break
  sleep 0.2
done

export SANDBOX_BASE_URL=http://127.0.0.1:8888
pip install requests
python test_client.py
```

> **Note:** This example's runtime uses a deliberately minimal endpoint
> contract (`/exec`, `/files`) that differs from the reference
> [`python-runtime-sandbox`](../python-runtime-sandbox/) (`/execute`,
> `/upload`, `/download`). The `k8s_agent_sandbox` Python SDK targets the
> latter contract, so this verification script only covers the direct-HTTP
> transport.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `0/1 nodes are available: ... predicate mismatch` | Node not labeled | Run `setup.sh` again, or `kubectl label node <name> kata-firecracker=true` |
| `Failed to create pod sandbox: ... kata-fc` | RuntimeClass not registered | `kubectl get runtimeclass kata-fc`; rerun `setup.sh` if missing |
| `/dev/kvm not found` | KVM module not loaded | See [Prerequisites](#prerequisites) — load the KVM kernel modules on the target node |
| Pod stuck in `ContainerCreating` on kind/minikube | Image not loaded into the node | `kind load docker-image ...` or `minikube image load ...` |
| `port-forward` fails with `no endpoints` | Warmpool has no ready replicas | Wait for `SandboxWarmPool` status to show `readyReplicas: 2` |

## Cleanup

Delete the example's own resources:

```shell
kubectl delete sandbox firecracker-example   # if deployed directly
kubectl delete sandboxwarmpool firecracker-runtime-warmpool
kubectl delete sandboxtemplate firecracker-runtime-template
```

> **Administrator-only — shared cluster resources.** `RuntimeClass/kata-fc` and
> the `kata-deploy` DaemonSet are cluster-wide; other workloads and sandbox
> examples may depend on them. Only delete them when you are sure nothing else
> needs Kata on this cluster:
>
> ```shell
> kubectl delete runtimeclass kata-fc         # removes the kata-fc RuntimeClass
> kubectl delete -n kube-system daemonset kata-deploy  # uninstalls Kata from nodes
> ```

## Related

* [`kata-gke-sandbox`](../kata-gke-sandbox/) — the QEMU-based Kata example
* [`python-runtime-sandbox`](../python-runtime-sandbox/) — the reference Python runtime
* [#1237](https://github.com/kubernetes-sigs/agent-sandbox/issues/1237) — Guest Coordination Protocol
* [Kata Containers — Firecracker](https://github.com/kata-containers/kata-containers/tree/main/docs/design/arch-images#firecracker)
