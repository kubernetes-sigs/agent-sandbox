# Enabling urunc on Kubernetes

## Overview

This example demonstrates how to run an Agent Sandbox as a VM, using
[urunc](https://github.com/urunc-dev/urunc) as the container runtime.

By default, Agent Sandbox uses standard container runtimes that provide
OS-level isolation, where all sandboxes share the host node's kernel. `urunc`
gives each sandbox its own dedicated kernel inside a microVM. The workload
itself is unchanged. This example runs the existing
[`python-runtime-sandbox`](../python-runtime-sandbox) FastAPI server on port
`8888`, only *packaged* for deployment over `urunc`.

### About `urunc`

`urunc` is a CNCF Sandbox project that acts as a container runtime for
unikernels and single-application kernels. It is agnostic to both the type of
sandbox and the guest. It supports VM-based and software-based sandboxes, and
guests ranging from unikernels to general-purpose kernels such as Linux and
BSDs. The key benefit of `urunc`'s design is
[near-container spawn times](https://dl.acm.org/doi/10.1145/3642977.3652096)
while
[consuming as few resources as possible](https://nubificus.co.uk/blog/runtime_benchmarking_rpi/).

### How urunc differs from other sandboxed container runtimes

Like other sandboxed container runtimes, `urunc` does not execute applications
directly on the host. Instead workloads run inside a sandbox either in the form
of a microVM or of a software-based sandbox (a userspace kernel), adding an
extra layer of isolation between the application and the host.

`urunc` differs from other runtimes like gVisor or Kata Containers in two ways.
First, it spawns every container in its own sandbox rather than one sandbox per
pod, which separates the trusted and untrusted parts of a deployment. Second, it
requires no component inside the sandbox (alongside the workload) or on the host
(alongside the sandbox monitor process).

Because of its design, `urunc` expects every workload to ship with its own
kernel, either as a unikernel or as a normal application packaged together
with a kernel. For an existing container image, that means attaching a Linux
kernel for `urunc` to boot the microVM with.

To simplify the above process, [bunny](https://github.com/nubificus/bunny) can
be used. It is a BuildKit frontend that builds unikernels and kernels, or
simply attaches a Linux kernel to an existing Linux container image.

### Example architecture

```text
   Control plane / HTTP client
          │
          │  HTTP (port 8888)
          │  ───── Sandbox Service ─── OR ─── sandbox-router (X-Sandbox-ID) ───┐
          │                                                                    │
          ▼                                                                    │
   Pod (runtimeClassName: urunc)                                               │
   ┌────────────────────────────────────┐                                      │
   │  microVM (QEMU) + Linux kernel     │                                      │
   │  ┌──────────────────────────────┐  │                                      │
   │  │ urunit (init)                │  │                                      │
   │  │  └─ python-runtime (main.py) │◀─┼──────────────────────────────────────┘
   │  │     - GET  /                 │  │
   │  │     - POST /execute          │  │
   │  └──────────────────────────────┘  │
   └────────────────────────────────────┘
          ▲
          │
   agent-sandbox controller
   (Sandbox, or SandboxClaim against a SandboxTemplate + SandboxWarmPool)
```

## Prerequisites

1. A **Linux** node with **hardware virtualization (KVM)**. `urunc` boots the
   workload in a microVM, so `/dev/kvm` must be present on every node that will
   schedule these sandboxes.
2. A Kubernetes cluster. This example was executed on a single node
   [k3s](https://k3s.io/) cluster.
3. `envsubst` (from GNU gettext). The Sandbox manifest is a template.
4. Docker with BuildKit, since `bunny` runs as a BuildKit frontend. BuildKit is
   part of the default Docker installation since v23.0. The image must be
   pushed to an OCI registry the cluster can pull from.

## Step 1: Run the Setup Script

`urunc` provides a DaemonSet that installs all supported monitors and `urunc`
itself, and configures both `containerd` and `urunc`. The exact commands are in
the [documentation of `urunc`](https://urunc.io/tutorials/How-to-urunc-on-k8s/).
For this example, [`setup.sh`](./setup.sh) wraps them: it applies the
urunc-deploy RBAC and DaemonSet, waits for the rollout, and registers the
`urunc` RuntimeClass.

For details on available `[OPTIONS...]`, please see the script itself.

```shell
./setup.sh                    # kind, kubeadm, EKS, ...
./setup.sh --flavor k3s       # k3s requires a different kustomize overlay
```

`urunc-deploy` installs the `urunc` binaries and the
[supported sandbox monitors](https://urunc.io/hypervisor-support/) on each node,
sets up the [urunc configuration](https://urunc.io/configuration), updates the
containerd configuration, reloads containerd and finally labels the node
`urunc.io/urunc-runtime=true`. The script is safe to re-run on a node that
already has `urunc`.

The RuntimeClass is registered as `urunc` unless `--runtime-class-name` was
specified. Export the name that was used. The `Sandbox` manifest in Step 4
requests the runtime through `${RUNTIME_CLASS_NAME}`, and an unset variable
expands to nothing, which leaves the pod on the node's default runtime with no
error to show for it.

```shell
export RUNTIME_CLASS_NAME=urunc
```

Confirm it worked:

```shell
kubectl get runtimeclass "${RUNTIME_CLASS_NAME}"
kubectl get nodes -l urunc.io/urunc-runtime=true
```

## Step 2: Install the Agent Sandbox Controller

The Agent Sandbox controller must be installed on the cluster before creating a
`Sandbox` resource. See the
[Installation Guide](../../README.md#installation).

## Step 3: Build the Sandbox Image

`bunny` builds the image straight from the normal Containerfile/Dockerfile.
The only change it needs is a `#syntax=harbor.nbfc.io/nubificus/bunny:latest`
directive on the first line, which tells BuildKit to hand the file to the
`bunny` frontend instead of the stock Dockerfile one. Nothing in `main.py` or
in the Dockerfile's instructions changes.

Rather than edit the Dockerfile that `python-runtime-sandbox` shares with the
other examples, prepend the line into a copy:

```shell
export IMAGE=<registry>/python-runtime-sandbox-urunc:latest

{ echo '#syntax=harbor.nbfc.io/nubificus/bunny:latest'; \
  cat ../python-runtime-sandbox/Dockerfile; } > /tmp/Dockerfile.urunc

docker build -f /tmp/Dockerfile.urunc -t ${IMAGE} ../python-runtime-sandbox
docker push ${IMAGE}
```

That is the whole build. `bunny` compiles the Dockerfile itself and then
attaches the extra pieces: a Linux kernel to boot over QEMU, and
[urunit](https://github.com/nubificus/urunit), which will be the init inside the
sandbox. `urunit` is optional, but it helps to set up the process environment
inside the sandbox as specified by the OCI config (e.g. uid/gid, wd, etc.).

> NOTE: To specify a different VMM, simply set the following label:
> `LABEL "com.urunc.unikernel.hypervisor"="cloud-hypervisor"`. `bunny` will
> fetch the appropriate kernel for that VMM. In the case of Firecracker, a
> block-based snapshotter (e.g. `devmapper`, `blockfile`) is required.

## Step 4: Deploy an Agent Sandbox

The manifest below (`sandbox-urunc.yaml`) defines a `Sandbox` that requests the
RuntimeClass registered in Step 1. The RuntimeClass carries its own scheduling
`nodeSelector`, so the pod lands on a `urunc`-enabled node without any explicit
selector here.

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: urunc-example
spec:
  service: true
  podTemplate:
    metadata:
      labels:
        sandbox: urunc
    spec:
      runtimeClassName: ${RUNTIME_CLASS_NAME}
      containers:
      - name: python-runtime
        image: ${IMAGE}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8888
          name: http
```

Both `${RUNTIME_CLASS_NAME}` and `${IMAGE}` were exported in Steps 1 and 3;
`envsubst` substitutes them here:

```shell
envsubst < sandbox-urunc.yaml | kubectl apply -f -
kubectl wait --for=condition=Ready sandbox/urunc-example --timeout=5m
```

`spec.service: true` is what makes the controller create the headless Service
that gives the sandbox its stable in-cluster hostname. Reach it from any other
pod in the cluster:

```shell
kubectl run client --rm -it --image=curlimages/curl --restart=Never -- curl -s http://urunc-example:8888
```

```json
{"status":"ok","message":"Sandbox Runtime is active."}
```

Execute a command inside the microVM:

```shell
kubectl run client --rm -it --image=curlimages/curl --restart=Never -- curl -s -X POST http://urunc-example:8888/execute -H 'Content-Type: application/json' -d '{"command":"echo hello world"}'
```

```json
{"stdout":"hello world\n","stderr":"","exit_code":0}
```

## Step 5: Verify the Isolation

Unlike standard containers, which share the host's kernel, `urunc` provides a
dedicated kernel for the sandbox. A difference between the host and sandbox
kernel versions proves the workload was executed inside a VM with a different
kernel.

> **NOTE**: Unlike Kata Containers, `urunc` runs no agent inside the guest, so
> `kubectl exec` is unavailable. Therefore, the check below goes through the
> sandbox's own `/execute` endpoint. `kubectl port-forward` is likewise
> unavailable, as with any VM-based runtime.

**1. Check the host node kernel:**

```shell
kubectl get nodes -o wide
# Note the KERNEL-VERSION of the node (e.g., 5.15.0-191-generic)
```

**2. Check the sandbox kernel**, via the runtime's own `/execute` endpoint:

```shell
kubectl run client --rm -it --image=curlimages/curl --restart=Never -- curl -s -X POST http://urunc-example:8888/execute -H 'Content-Type: application/json' -d '{"command":"uname -r"}'
```

```json
{"stdout":"6.18.0urunc\n","stderr":"","exit_code":0}
```

* **Success**: the output is a different kernel version. This proves the agent
  is running inside its own microVM with its own kernel, isolated from the host.
* **Failure**: the output is identical to the host node's kernel. This indicates
  the pod fell back to the default runtime; check that `runtimeClassName`
  survived to the pod with
  `kubectl get pod urunc-example -o jsonpath='{.spec.runtimeClassName}'`.

## Sizing the microVM

`urunc` derives the guest's memory and vCPU count from the container's resource
limits, and falls back to the defaults in
[its configuration](https://urunc.io/configuration) (`default_memory_mb = 256`,
`default_vcpus = 1`) when none are set. That default is enough for this FastAPI
server, but a heavier workload needs an explicit limit:

```yaml
resources:
  limits:
    memory: 1Gi
    cpu: "2"
```

## Cleanup

```shell
# From a fresh shell, re-export RUNTIME_CLASS_NAME and IMAGE first, as in Steps 1 and 3.
envsubst < sandbox-urunc.yaml | kubectl delete -f - --ignore-not-found
```

To remove `urunc` from the nodes, run the setup script in reverse:

```shell
./setup.sh --uninstall                  # kind, kubeadm, EKS, ...
./setup.sh --uninstall --flavor k3s     # k3s
```

Uninstalling is a [two-stage handoff](https://urunc.io/tutorials/How-to-urunc-on-k8s/#urunc-deploy-in-k3s)
between two DaemonSets, coordinated through a node label, and the script
sequences it. Deleting urunc-deploy fires its
`preStop` hook, which restores the containerd configuration, removes the
binaries and monitors, and re-labels the node `urunc.io/urunc-runtime=cleanup`.
That label is the node selector of urunc-cleanup, which then drops the label,
reloads the container runtime and waits for the node to be `Ready` again.

The script also deletes the RuntimeClass and the RBAC afterwards. Neither is
removed by urunc-deploy itself: nothing in it references the RuntimeClass, and
both DaemonSets share the `urunc-deploy-sa` ServiceAccount, so the RBAC has to
outlive the cleanup stage.

## Additional resources

- [urunc documentation](https://urunc.io)
- Charalampos Mainas, Ioannis Plakas, Georgios Ntoutsos, Anastassios Nanos.
  [Sandboxing Functions for Efficient and Secure Multi-tenant Serverless Deployments](https://dl.acm.org/doi/10.1145/3642977.3652096).
  SESAME '24: 2nd Workshop on SErverless Systems, Applications and MEthodologies, pp. 25-31, 2024.
- [Sandboxed containers in a Raspberry Pi](https://nubificus.co.uk/blog/runtime_benchmarking_rpi/)
