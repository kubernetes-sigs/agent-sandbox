# Hyperlight micro-VM Sandboxes

## Overview

This example runs the [`python-runtime-sandbox`](../python-runtime-sandbox) API
inside a [Hyperlight](https://github.com/hyperlight-dev/hyperlight) micro-VM,
with [hyperlight-unikraft](https://github.com/hyperlight-dev/hyperlight-unikraft)
(`hluk`) as the monitor. The guest kernel is a
[Unikraft](https://unikraft.org) unikernel that runs ordinary Linux binaries,
here CPython 3.12 with numpy, pandas, scipy and scikit-learn.

The micro-VM runs in an **ordinary pod**. The container is `hluk` and the
guest it boots, nothing else. The only thing the pod needs from the node is
`/dev/kvm`, which the
[Hyperlight device plugin](https://github.com/hyperlight-dev/hyperlight-on-kubernetes)
hands out as a resource. There is no RuntimeClass, no privileged container,
no agent in the guest, and nothing to install on the nodes but the plugin.

The guest can be packaged in two ways:

| Image | What the container runs | Time from `hluk` start to a serving runtime* | Image size (compressed / unpacked) |
|---|---|---|---|
| rootfs (`Containerfile`) | `hluk run`: boots the kernel, unpacks the rootfs, imports the data-science stack, starts the runtime | ~9 s | 140 MB / 440 MB |
| snapshot (`Containerfile.snapshot`) | `hluk snapshot run`: resumes a guest saved after those imports, right before the runtime starts | under 200 ms | 250 MB / 900 MB |

\* Measured with `hluk` v0.20.1 on one x86_64 KVM host, from process start
to the first successful `GET /`. Pod scheduling and image pull come on top.

The snapshot image is what makes a `SandboxWarmPool` cheap to keep full. When
a claim takes a pod, its replacement serves in milliseconds, not seconds.

### How it differs from the other VM examples

| | [firecracker-sandbox](../firecracker-sandbox) | [urunc-sandbox](../urunc-sandbox) | this example |
|---|---|---|---|
| Runtime | Kata Containers (`kata-fc`) | urunc | the node's |
| What the node needs | Kata + a RuntimeClass | urunc + a RuntimeClass | `/dev/kvm` and the device plugin |
| VMM | Firecracker | QEMU (default) | Hyperlight (`hluk`) |
| Guest kernel | Linux | Linux | Unikraft |
| Guest agent | kata-agent | none (urunit as init) | none |
| Devices | virtio | virtio | none: file and socket calls are host function calls |
| Storage | devmapper / block snapshotter | any snapshotter | any snapshotter (rootfs is an initrd) |
| Resume from a saved VM | no | no | yes (`hluk snapshot run`) |

Hyperlight has no virtual devices. `hluk` carries out every socket call the
guest makes on a real socket in the **pod's network namespace**. The guest
therefore shares the pod's IP address and routes, and any `NetworkPolicy`
that applies to the pod applies to the guest, as it would to a regular
container. `hluk` also installs the pod's `/etc/resolv.conf` in the guest, a
resumed snapshot included, so the guest resolves names as the pod does. The
template in this example takes the controller's default egress, which allows
public addresses only, and adds the cluster DNS to it, because a resolver the
guest cannot reach makes every lookup wait out its timeouts. Cluster services
stay out of reach.

### Architecture

```text
   Control plane / HTTP client
          │
          │  HTTP (port 8888)
          │  ───── Sandbox Service ─── OR ─── sandbox-router (X-Sandbox-ID) ──┐
          ▼                                                                   │
   Pod (no runtimeClassName; limits: hyperlight.dev/hypervisor: 1)            │
   ┌──────────────────────────────────────────────────────────┐               │
   │  hluk (the container; owns the guest's real sockets)     │◀──────────────┘
   │  ┌────────────────────────────────────────────────────┐  │
   │  │ Hyperlight micro-VM, Unikraft kernel               │  │
   │  │  └─ /app/main.py (stdlib HTTP server)              │  │
   │  │     - GET  /            - POST /upload             │  │
   │  │     - POST /execute     - GET  /download/{path}    │  │
   │  │                         - GET  /list/{path}        │  │
   │  │                         - GET  /exists/{path}      │  │
   │  └────────────────────────────────────────────────────┘  │
   │                              ▲ /dev/kvm                  │
   └──────────────────────────────┼───────────────────────────┘
          ▲                       │ injected by the Hyperlight device plugin
          │                       │ (a DaemonSet; CDI)
   agent-sandbox controller       Node
   (SandboxClaim against a SandboxTemplate + SandboxWarmPool)
```

`main.py` speaks the same API as
[`python-runtime-sandbox/main.py`](../python-runtime-sandbox/main.py), so the
[k8s-agent-sandbox](../../clients/python/agentic-sandbox-client) SDK's
commands and files APIs work unchanged. Like it, it serves requests
concurrently and runs each command in a session of its own, so a timeout
kills everything the command started. It uses only the standard library, so
the guest rootfs is the published `agent` rootfs plus one file.

## Prerequisites

1. **Linux nodes with KVM**, so bare metal or VMs with nested
   virtualization. The guest runs on the node's CPU, so the images are
   x86_64 for x86_64 nodes and arm64 for arm64 nodes.
2. The **Hyperlight device plugin** on those nodes. It advertises the
   `hyperlight.dev/hypervisor` resource and injects `/dev/kvm` into the
   containers that request it, through the
   [Container Device Interface](https://github.com/cncf-tags/container-device-interface).
   containerd 2.0 and CRI-O enable CDI by default. For containerd 1.7, set
   `enable_cdi = true` in its CRI plugin configuration. Label the nodes and
   install the plugin from its repository, pinned to a published image. The
   plugin creates the device node for user 65534, which the template's pods
   run as:

   ```shell
   kubectl label node <node> hyperlight.dev/enabled=true hyperlight.dev/hypervisor=kvm
   export IMAGE=ghcr.io/hyperlight-dev/hyperlight-device-plugin:8cfa5cf@sha256:21891c11ad9eff25844438872019a431a6ef7e5abcd112ef5edcf765a772b4c1
   export DEVICE_COUNT=2000 DEVICE_UID=65534 DEVICE_GID=65534
   curl -fsSL https://raw.githubusercontent.com/hyperlight-dev/hyperlight-on-kubernetes/8cfa5cfa02572150f9c611ecd2d076003228a106/deploy/manifests/device-plugin.yaml \
     | envsubst '$IMAGE $DEVICE_COUNT $DEVICE_UID $DEVICE_GID' | kubectl apply -f -
   kubectl -n hyperlight-system rollout status daemonset/hyperlight-device-plugin
   ```

   On a [kind](https://kind.sigs.k8s.io) cluster, mount the host's `/dev/kvm`
   into the node with `extraMounts` in the cluster configuration, and apply
   `deploy/local/device-plugin.yaml` from the same commit instead. That
   manifest moves the kubelet's termination log out of `/dev`. `DEVICE_COUNT`
   is how many pods per node may hold the device at once. `/dev/kvm` is
   shared, so the count is a scheduling limit, not a count of anything
   physical.
3. The **agent-sandbox controller** installed, as in the
   [quickstart](../quickstart).
4. **`hluk`**, **Docker** and an OCI registry on the build machine, to build
   the guest rootfs and its images. `hluk` v0.20.1 is the release the rootfs
   is pinned to in `rootfs.Dockerfile`:

   ```shell
   curl -fsSL https://raw.githubusercontent.com/hyperlight-dev/hyperlight-unikraft/main/install.sh \
     | HLUK_VERSION=v0.20.1 sh
   ```

Tag the images with the `hluk` release they are built for. A snapshot only
loads under the `hluk` that saved it, and with a new tag per release a node
never runs a cached image built for the previous one:

```shell
export REPO=registry.example.com/python-runtime-sandbox-hyperlight
```

## Step 1: Build the guest rootfs

`rootfs.Dockerfile` adds `main.py` to the published `agent` rootfs. `hluk
build` builds it with Docker and converts it to the CPIO the guest boots from:

```shell
hluk build                       # writes .hluk/rootfs.cpio
cp .hluk/rootfs.cpio initrd.cpio
```

You can try the runtime locally, with no cluster:

```shell
hluk run &                       # reads hluk.toml and serves on 127.0.0.1:8888
curl -s localhost:8888/
curl -s -X POST localhost:8888/execute -H 'Content-Type: application/json' \
  -d '{"command":"python3 -c \"import sklearn; print(sklearn.__version__)\""}'
```

The runtime's own tests run on the build machine, with no guest:

```shell
python3 -m pytest test_main.py
```

## Step 2: Build the image

Both images hold the static `hluk` release binary and the guest, from
`scratch`. There is no shell and no libc in the container, nothing but the
monitor. The two are pinned together, because the driver inside the rootfs
speaks the protocol of the `hluk` release it was built for.

Either the rootfs image, which boots the guest:

```shell
export IMAGE=${REPO}:hluk-v0.20.1
docker build -f Containerfile -t ${IMAGE} .
docker push ${IMAGE}
```

or the snapshot image, which resumes it. `hluk snapshot save` boots the guest
once, imports the modules `main.py` uses, and saves the guest right before
it runs anything:

```shell
hluk snapshot save --initrd initrd.cpio -o snapshot --scratch-mb 1536 \
  --warm-exec "import email.message, email.parser, email.policy, email.utils, http.server, json, logging, re, shutil, socketserver, subprocess, tempfile, urllib.parse"
export IMAGE=${REPO}:hluk-v0.20.1-snapshot
docker build -f Containerfile.snapshot -t ${IMAGE} .
docker push ${IMAGE}
```

The rootfs image runs `hluk run --scratch-mb max`, so the guest gets the
pod's memory limit less the 64 MiB `hluk` keeps for itself. The pod names
the guest's memory once, as its limit. The snapshot image keeps the memory
size the snapshot was saved with, 1536 MiB here, which is the size the
`agent` rootfs is tested with. `hluk` says so on stderr when the pod's limit
cannot hold that size and its own share.

> **NOTE:** A snapshot is tied to the `hluk` release that saved it, and
> `hluk` refuses one saved by another release. `hluk snapshot key` prints
> the key a release loads. Rebuild the snapshot image, under a new tag,
> whenever you move to another `hluk` release. The snapshot also holds the
> guest's memory, so the image is about twice the size of the rootfs image.
>
> The snapshot saves the start of the runtime, not the commands it runs.
> Each `/execute` starts a new process, so `python3 -c "import pandas"` takes
> the same few seconds on either image.

## Step 3: Deploy a template, a warm pool and a claim

`${IMAGE}` is the image you built last, either one:

```shell
envsubst < sandbox-template.yaml | kubectl apply -f -
kubectl get sandboxwarmpool hyperlight-runtime-warmpool
kubectl wait --for=condition=Ready sandboxclaim/hyperlight-claim --timeout=5m
```

The template's pods have no `runtimeClassName`. They run as user 65534 with
every capability dropped, a read-only root filesystem and the default seccomp
profile. Each asks for one `hyperlight.dev/hypervisor`, which schedules it
onto a node with the plugin and gets it `/dev/kvm`. A liveness probe
restarts a guest that has answered nothing for a minute. The claim adopts
one of the warm pool's sandboxes, which keeps its pool-generated name in
`.status.sandbox.name`. The pool then starts a replacement, which resumes
from the snapshot when the snapshot image is used. On a
[kind](https://kind.sigs.k8s.io) cluster on one x86_64 host, with the images
already on the node, the pool's two pods were Ready 2 s after the apply
with the snapshot image and 11 s with the rootfs image. Each replacement pod
was Ready a second after its container started.

Run a command in the micro-VM. The template's `networkPolicy` admits the
sandbox-router, as the controller's default does, and pods labelled
`app.kubernetes.io/name=hyperlight-client` on port 8888, so a client pod with
that label can reach the claimed sandbox at its pod IP address:

```shell
POD_IP=$(kubectl get sandboxclaim hyperlight-claim -o jsonpath='{.status.sandbox.podIPs[0]}')
kubectl run client --rm -it --image=curlimages/curl --restart=Never \
  --labels=app.kubernetes.io/name=hyperlight-client -- \
  curl -s -X POST http://${POD_IP}:8888/execute \
  -H 'Content-Type: application/json' -d '{"command":"uname -a"}'
```

```json
{"stdout": "Unikraft unikraft 5.15.148-Ijiraq 0.21.0 x86_64 GNU/Linux\n", "stderr": "", "exit_code": 0, "timed_out": false}
```

The guest reports a Unikraft kernel, not the node's Linux kernel, so the
command ran inside the micro-VM.

The pods are ordinary pods, so `kubectl port-forward` reaches the runtime
from outside the cluster. The
[k8s-agent-sandbox](../../clients/python/agentic-sandbox-client) SDK drives
it as it does the `python-runtime-sandbox` image, here through a tunnel to
the [sandbox-router](../../sandbox-router), which has to be installed:

```python
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxLocalTunnelConnectionConfig

with SandboxClient(connection_config=SandboxLocalTunnelConnectionConfig()) as client:
    sandbox = client.create_sandbox(warmpool="hyperlight-runtime-warmpool", namespace="default")
    print(sandbox.commands.run("python3 -c 'import sklearn; print(sklearn.__version__)'").stdout)
    sandbox.files.write("hello.txt", "hello from the micro-VM\n")
    print(sandbox.files.read("hello.txt"))
```

`kubectl exec` has nothing to run. The image has no shell, and a shell in the
container would not be inside the micro-VM anyway. Use the runtime's
`/execute` endpoint.

## Limitations

- **Uploads are kept in guest memory**, as is everything the guest writes,
  because the guest's filesystem is in its memory. The runtime refuses an
  upload larger than `SANDBOX_MAX_UPLOAD_BYTES`, 100 MiB by default. Set it
  with `--env` in the image's entrypoint to change it. A workspace that
  outgrows the guest's memory fails writes with `ENOSPC`, as a full tmpfs
  does. A command that runs out of memory fails or is killed with exit code
  137. The guest and the files in its workspace stay.
- **No POSIX semaphores.** The guest has no `/dev/shm`, so joblib, which
  scikit-learn uses, runs serially and prints a warning saying so.
- **The `agent` rootfs's OpenSSL does not find its CA bundle**
  ([hyperlight-unikraft#234](https://github.com/hyperlight-dev/hyperlight-unikraft/issues/234)).
  The rootfs carries Debian's bundle at `/etc/ssl/certs/ca-certificates.crt`,
  but its OpenSSL looks at `/usr/lib/ssl/cert.pem`, so HTTPS from a guest
  built straight on it fails certificate verification. `rootfs.Dockerfile`
  links the one to the other, so this example's guest verifies. A rootfs of
  your own needs the same line until a release carries the fix.

## Cleanup

```shell
envsubst < sandbox-template.yaml | kubectl delete -f - --ignore-not-found
```

## Additional resources

- [hyperlight-on-kubernetes](https://github.com/hyperlight-dev/hyperlight-on-kubernetes):
  the device plugin, and how Hyperlight runs in unprivileged pods
- [hyperlight-unikraft](https://github.com/hyperlight-dev/hyperlight-unikraft):
  `hluk`, its guest runtimes, how snapshots work
  ([docs/execution.md](https://github.com/hyperlight-dev/hyperlight-unikraft/blob/main/docs/execution.md))
  and how guest networking works
  ([docs/net.md](https://github.com/hyperlight-dev/hyperlight-unikraft/blob/main/docs/net.md))
