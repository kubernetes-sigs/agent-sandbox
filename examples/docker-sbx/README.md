# Docker Sandboxes (`docker-sbx`) in an Agent Sandbox

This example runs [Docker Sandboxes](https://docs.docker.com/ai/sandboxes/) (the `sbx` CLI, packaged as `docker-sbx`) in a `Sandbox` pod. The pod hosts the `sbx` daemon, and each agent you start with `sbx` runs in its own microVM with its own kernel, filesystem, network, and Docker daemon.

```text
 Kubernetes node (KVM)
 └── Sandbox pod (docker-sbx image, privileged)
     └── sbx daemon (PID 1)   ← network policy + credential-injecting proxy
         ├── microVM "demo"
         └── microVM "claude-workspace"
```

The Sandbox provides identity, storage, and scheduling. `sbx` adds per-agent hypervisor isolation, an egress allow-list, and API keys that stay outside the agent's VM.

## Prerequisites

1. A cluster with the agent-sandbox controller and at least one node with **KVM**: `/dev/kvm` present (bare metal or nested virtualization) and the `erofs` filesystem in the kernel. This typically does not work on Kind. Check the node, then label it (`sandbox.yaml` selects `kvm=true`):

   ```bash
   ls -l /dev/kvm && { grep -w erofs /proc/filesystems || sudo modprobe erofs; }
   kubectl label node <node> kvm=true
   ```

2. A namespace that allows `privileged` pods (see [Limitations](#limitations)).
3. `kubectl`, and Docker to build the image.
4. A [Docker account](https://docs.docker.com/ai/sandboxes/install/#sign-in) and [personal access token](https://docs.docker.com/security/access-tokens/personal-access-tokens/). `sbx` requires a signed-in Docker user.
5. An API key for the agent you want to run, such as Anthropic for `claude`.

## Files

- `Dockerfile`: `ubuntu:24.04` plus the pinned `docker-sbx` package from Docker's apt repository.
- `entrypoint.sh`: runs `sbx login --password-stdin`, then `sbx daemon start` as PID 1.
- `sandbox.yaml`: the `Sandbox`, with `/dev/kvm`, a 50 GiB `data` PVC for `sbx` state (VM disks, image cache, credentials), and a 5 GiB `workspace` PVC.

## How to Use

### 1. Build and publish the image

```bash
docker build -t REGISTRY/docker-sbx:0.46.0 .
docker push REGISTRY/docker-sbx:0.46.0
```

Set `image:` in `sandbox.yaml` to the pushed tag. If your cluster's runtime shares your local image store, build with `-t docker-sbx:local` and skip the push.

### 2. Create the credentials Secret

```bash
kubectl create secret generic docker-sbx-credentials \
  --from-literal=DOCKER_USERNAME="your_docker_username" \
  --from-literal=DOCKER_PAT="your_docker_access_token"
```

### 3. Deploy the Sandbox

```bash
kubectl apply -f sandbox.yaml
kubectl wait --for=condition=ready pod/docker-sbx --timeout=5m
```

The pod is ready once the daemon is running. A failed sign-in makes the container exit (`CrashLoopBackOff`); see `kubectl logs docker-sbx`.

### 4. Verify and initialize

```bash
kubectl exec docker-sbx -- sbx diagnose
kubectl exec docker-sbx -- sbx policy init balanced
```

The Virtualization and Authentication checks must pass. `sbx` needs a one-time global network policy before it starts a sandbox. `balanced` allows typical development traffic, such as AI providers and package registries. Use `deny-all` plus `sbx policy allow network <host>:<port>` for a stricter allow-list.

### 5. Start a microVM

By default `sbx` sizes a VM from the host's CPUs and memory, so pass `--cpus` and `--memory` to keep VMs inside the pod limits in `sandbox.yaml`:

```bash
kubectl exec docker-sbx -- sbx create --name demo --cpus 3 --memory 6g shell /workspace
kubectl exec docker-sbx -- sbx exec demo -- uname -r
kubectl exec docker-sbx -- uname -r
```

The two kernel versions differ: the first is the microVM's guest kernel, the second is the node's.

### 6. Run an agent

Pass the model key through stdin so it never appears in the pod spec. The `sbx` proxy injects it into outbound requests, so the agent's VM never sees the real value:

```bash
kubectl exec -i docker-sbx -- sbx secret set anthropic <<< "$ANTHROPIC_API_KEY"
kubectl exec -it docker-sbx -- sbx run --cpus 3 --memory 6g claude /workspace
```

The `workspace` PVC is mounted into the microVM at the same path. See the [supported agents](https://docs.docker.com/ai/sandboxes/agents/).

## Cleanup

```bash
kubectl exec docker-sbx -- sbx rm --force demo
kubectl delete -f sandbox.yaml
kubectl delete secret docker-sbx-credentials
kubectl delete pvc -l sandbox=docker-sbx   # also deletes all VM disks and stored secrets
```

## Configuration

- **VM size:** keep the sum of running VMs, plus headroom for the daemon, under the pod memory limit.
- **VM disks:** `DOCKER_SANDBOXES_ROOT_SIZE` (default 20 GB) and `DOCKER_SANDBOXES_DOCKER_SIZE` (default 10 GB) apply at creation, for example `kubectl exec docker-sbx -- env DOCKER_SANDBOXES_ROOT_SIZE=40g sbx create ...`. Size the `data` PVC for every sandbox you keep.
- **Egress:** `sbx policy ls` shows the rules and `sbx policy log` shows blocked requests. The pod also needs outbound access to Docker and your model provider.
- **Newer `sbx`:** `docker build --build-arg DOCKER_SBX_VERSION=<version> .`. Commands and flags change between releases, so check `sbx --help`.

## Limitations

- **Privileged:** `sandbox.yaml` sets `privileged: true`. The microVMs isolate agents from each other and from the pod, but a microVM escape lands in a privileged container, so use a dedicated node pool. Clusters enforcing `baseline` or `restricted` Pod Security, or a policy engine such as Kyverno or Gatekeeper, will block it.
- **Credentials at rest:** with no OS keychain, `sbx` stores the Docker token and API keys on the `data` PVC, protected only by file permissions. Use an encrypted `StorageClass` and restrict `exec` into the pod.
- **Lifecycle:** running VMs stop when the pod restarts, but their disks persist. `sbx run` or `sbx exec` starts them again.
- **Not part of agent-sandbox:** `sbx` is a Docker product with its own [terms and releases](https://github.com/docker/sbx-releases).

## Troubleshooting

- **`ContainerCreating` with `hostPath type check failed: /dev/kvm is not a character device`** (`kubectl describe pod docker-sbx`): the node has no `/dev/kvm`. Check that only KVM nodes carry the `kvm=true` label.
- **`CrashLoopBackOff` with `docker access-token request failed`:** the username or token in `docker-sbx-credentials` is wrong.
- **`sbx diagnose` reports no hypervisor, or sandboxes fail to start:** recheck the KVM and `erofs` prerequisites on the node (`kubectl exec docker-sbx -- grep -w erofs /proc/filesystems` shows the node kernel's view).
- **Daemon logs:** `kubectl exec docker-sbx -- cat /data/state/sandboxes/sandboxes/sandboxd/daemon.log`.

## References

- [Install Docker Sandboxes](https://docs.docker.com/ai/sandboxes/install/): platform requirements, including KVM on Linux
- [Docker Sandboxes architecture](https://docs.docker.com/ai/sandboxes/architecture/)
- [windows-sandbox](../windows-sandbox): another example that runs a KVM-based guest in a `Sandbox`
