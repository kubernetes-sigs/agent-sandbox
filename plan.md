# Multi-tenant Sandbox — Remaining Work

Running tracker for the multi-tenant Sandbox feature
(`Sandbox.Spec.Image`). Update as items land. See also commit history and
[controllers/sandbox_multitenant.go](controllers/sandbox_multitenant.go),
[internal/pool/](internal/pool/), [pod-agent/](pod-agent/).

## Status (2026-05-15)

End-to-end SSH path works on kind. Validated:
1. `kubectl apply` Sandbox with `spec.image: docker.io/library/debian:bookworm-slim`.
2. Controller picks/creates pool pod, mints session token Secret, dials pod-agent gRPC, calls `CreateSandbox`.
3. Pod-agent mounts overlay, spawns bwrap, bwrap exec's worker which serves gRPC on Unix socket inside the sandbox.
4. SSH client connects to pool pod IP:2222. Pod-agent (russh) authenticates via username=UID + password=token. Pod-agent opens `Worker.OpenShell` over UDS. Worker allocates PTY, forks `/bin/bash`, streams.

Code review pending — pause feature work.

## Done

- CRD: `Spec.Image`, `Spec.Workspace`, `Status.Host`, `Status.SSHHost`, `Status.SSHPort`, `Status.SSHSecretName`, `Status.Endpoints`.
- Controller: branch in `reconcileChildResources` for multi-tenant.
- Pool: image-hash-based selector + bare-pod provisioner + per-pod Service (proxy=8080, ssh=2222).
- HTTPRoute (typed `gwv1.HTTPRoute`, path-based `/s/<uid>/`). Disabled by default (`--gateway-name=""`).
- Watches: Sandbox reconcile enqueued on pool-pod label-managed events (`enqueueSandboxesForPoolPod` in [controllers/sandbox_controller.go](controllers/sandbox_controller.go)).
- Proto: `agentsandbox.podagent.v1.PodAgentService` + `agentsandbox.worker.v1.WorkerService` (Ping + Exec + OpenShell). Buf codegen via `//go:generate sh -c "cd proto && buf generate"`.
- Go gRPC client: `pool.GRPCAgentClient`, `pool.AgentClientPool` (cleartext, conn cache by pod name).
- Pod-agent (Rust): bwrap launcher + overlay mount; gRPC server + russh SSH server. SSH delegates shell spawn to worker via UDS gRPC (`OpenShell` bidi stream).
- Worker (Rust): `Ping`, `Exec` (stdout/stderr stream), `OpenShell` (PTY + bash + bidi stdin/resize/stdout/exit).
- SSH session tokens: 32-byte hex, controller-minted, stored in Secret `<sandbox>-ssh` (owned by Sandbox, GC'd on delete).
- GC: periodic sweep (30 min) reaps orphan tenants. No finalizers.
- Wiring flags: `--pool-agent-image`, `--gateway-*`, `--pool-pvc-*`.
- Deploy-kind: `make deploy-kind` builds both images and auto-injects `--pool-agent-image=kind.local/pod-agent:<tag>`.
- Test script: [`test-ssh.sh`](test-ssh.sh) end-to-end smoke (sandbox apply → SSH in via sshpass).

## Decisions made during build (worth remembering)

- **Two-binary single Rust crate** for `pod-agent` and `agent-sandbox-worker`. Workspace not used; they share deps + proto codegen via `build.rs`.
- **No finalizers, ever** (hard rule). Fast-delete works via a Sandbox-informer `DeleteFunc` handler that fires on the in-memory delete event with the last-cached CR object intact — controller spawns a best-effort goroutine that calls `pod-agent.DeleteSandbox`. The periodic `pool.GC` sweep is the durable backup; it runs once on controller boot and then every 30 minutes.
- **No CRD split**: `Sandbox.Spec.PodTemplate` and `Spec.Image` are mutually exclusive but live on the same CRD. Multi-tenant mode is detected by `pool.IsMultiTenant(s)` = `s.Spec.Image != nil`. To be re-evaluated upstream.
- **Pool pods are bare Pods** (not StatefulSet/owned). Managed via labels `agents.x-k8s.io/managed-by=sandbox-controller-pool` + `agents.x-k8s.io/pool-image-hash=<hash(image_ref)>`. Lifecycle deferred (no idle GC yet).
- **OwnerRef not usable for pool pod ↔ Sandbox**: pool pod hosts N sandboxes; only one controllerRef is allowed. Use `Watches` + `enqueueSandboxesForPoolPod`.
- **SSH terminates in pod-agent**, not worker. Pod-agent does auth (username = UID, password = token from Secret), then delegates shell spawn to worker via UDS gRPC (`WorkerService.OpenShell`). Cleanly ports to Firecracker: swap `UnixStream` for `VsockStream` in [pod-agent/src/worker_client.rs](pod-agent/src/worker_client.rs).
- **No `nsenter` / `setns`**: worker already runs inside the sandbox namespaces, so spawning the shell from worker is naturally in the right context. nsenter approach was tried; bwrap supervisor PID was outside the namespaces (needed `find_inner_pid`); whole path scrapped in favor of UDS-to-worker.
- **bwrap root mount is `--bind`, not `--ro-bind`** ([pod-agent/src/tenant.rs](pod-agent/src/tenant.rs)). Overlay enforces RO/RW layering on its own; `--ro-bind /` would block bind-mount target creation (worker binary mount). Comment in code captures this.
- **bwrap stdout/stderr → file** under `/run/sandboxes/<uid>/bwrap.{stdout,stderr}.log` for debugging.
- **Worker bind path**: `/usr/local/bin/agent-sandbox-worker` (RO bind from host); socket dir `/run/agentsandbox` is RW bind of `/run/sandboxes/<uid>`. Worker creates socket at `/run/agentsandbox/worker.sock`; pod-agent reads same file at host path `/run/sandboxes/<uid>/worker.sock`.
- **Host key not persisted**: regenerated on each pod-agent restart. Clients warn on host-key-changed. Persisting to PVC is a small follow-up.
- **`hostUsers: true`** in pool pod spec (forced). Kind doesn't support nested userns sysfs mount. Real clusters with kubelet `UserNamespacesSupport` + containerd subuid maps can flip back to `false`.
- **Endpoint = Pod IP** (not Service DNS) for now. Service exists per pool pod with port 2222 but DNS depends on coredns + namespace + cluster domain. Pod IP is what `test-ssh.sh` uses.
- **Status fields for SSH split**: `sshHost` + `sshPort` (separate, not `host:port`) — ssh CLI can't parse `user@host:port`.

## Deferred (review-time decisions)

- **Provisioner two-pool-pods race**: simultaneous reconciles on first user of a given image both call `CreateNew` and create two pool pods. Wasteful but correct (each gets bound to its own Sandbox). De-dup would need an in-flight map keyed by image hash; not worth the code today.
- **Pod-agent auth**: gRPC is cleartext + unauthenticated. Plan: use Kubernetes projected ServiceAccount tokens (JWTs) as bearer creds — controller mounts/uses its own SA token, pod-agent verifies via TokenReview (or by validating the JWT against the cluster's OIDC issuer). Defers mTLS entirely.
- **bwrap stdout/stderr logs are wiped on `Tenant::stop`** (`std::fs::remove_dir_all(&self.paths.run_root)`). Lost for postmortem; not worth a redesign now. Future: move logs to PVC subpath or stream to pod-agent stdout.
- **SSH host key regenerated on pod-agent restart**: clients get host-key-changed warnings. Will revisit when auth model is redesigned.
- **Selector full-namespace list per Choose**: O(N) over all Sandboxes in the namespace. Add a field index on `status.host.podName` via `IndexField` when we add a status-poll loop.
- **Tests for multi-tenant path**: deferred until the feature stops moving. The Selector/Provisioner/multitenant reconcile/GC paths are all churning during this phase; tests written today get rewritten in a week.
- **Tenant exit signaling**: today the controller never relearns that a tenant has exited (process died, OOM). Plan: pod-agent emits a status stream the controller subscribes to (or a watch-style RPC). Until then, the periodic-poll story (`Controller: status polling`) is the planned mechanism. Open design point: stream-from-agent vs. controller-polls.
- **`WorkerServiceClient` re-dialed per SSH session**: cheap but wasteful. Caching the `Channel` per uid in `Entry` would amortize. Punted because eager dial at `CreateSandbox` time has to handle the "worker not yet listening" race; lazy connect is simpler.

## Open knowns / suspected issues

- Worker binary is **glibc-linked** (debian-bookworm builder). User images must be glibc-based. Alpine (musl) base will fail. Static-link with musl target in builder is a small follow-up.
- `pre_exec` closure in worker calls `setsid()` + `ioctl(TIOCSCTTY)` — fine for our use but skips error-context wrapping.
- `worker_client::connect` opens a fresh channel per SSH session. Could pool; cheap as-is.
- `WORKER_BIN_IN_SANDBOX = /usr/local/bin/agent-sandbox-worker`. Hard-coded path; collides with anything user image already has there. Improbable but worth flagging.
- The race-loser path in `CreateSandbox` (two concurrent calls for same uid) `tokio::spawn`s a fire-and-forget `tenant.stop()` for the loser. Survives but logs nothing on stop error.
- `Tenant::stop()` keeps PVC upper but `rm -rf /run/sandboxes/<uid>` — that deletes the `bwrap.{stdout,stderr}.log` we just added for debugging. Decide: keep logs post-delete or accept the loss.
- `GRPCAgentClient` and `AgentClientPool` do no liveness check; if a pool pod is gone the controller will hit dial errors until next reconcile.

## Next (after review)

### Pod-agent: HTTP reverse proxy
- Proxy port (default 8080) accepts `/s/<uid>/...` from the gateway.
- Tunnels HTTP to the tenant. Two viable shapes:
  - (a) Worker also serves HTTP on a second Unix socket (`http.sock`); pod-agent reverse-proxies via hyper.
  - (b) Bidi gRPC tunnel RPC over `worker.sock`.
- Recommend (a) — simpler, plays well with WebSockets.
- HTTPRoute backend resolves to a per-pool-pod **headless Service** (created by Provisioner when `--gateway-name` is set, i.e. `Provisioner.CreateService = true`). No-op otherwise.

### Pod-agent: boot recovery
- On startup scan `/var/lib/sandboxes/*/upper` + `is_mountpoint(merged)` + procs.
- For each found: rebuild `Registry` entry. If overlay still mounted but no `bwrap` child → mark `PhaseFailed` and let the controller re-create.
- Restart caveat today: worker socket is in tmpfs `/run/sandboxes` (emptydir Memory) — gone on pod restart. So boot recovery should NOT try to reattach to live tenants; only clean up orphan PVC dirs and re-emit Failed state so controller re-creates.

### Pod-agent: isolation hardening
- **Per-tenant network namespace + egress proxy** (port moat
  `net/mod.rs`, `net/nftables.rs`, `platform/linux.rs`). Today all
  sandboxes share the pool pod's netns (so curl etc. work, but no
  isolation; `/etc/resolv.conf` and `/etc/hosts` are bind-mounted from
  the host so DNS works). Target shape, mirroring moat:
  1. Each sandbox gets a /24 in `10.<base>.<id>.0/24`. host_ip=.1,
     sandbox_ip=.2.
  2. `ip netns add` per sandbox; veth pair `veth-h-<id>` ↔
     `veth-s-<id>`; sandbox-side moved into the netns; default route
     via host_ip; `lo` up.
  3. nftables host table (`agentsandbox`):
     - DNAT all outbound from sandbox → `127.0.0.1:proxy_port`
       (egress proxy port on the pool pod).
     - Per-sandbox chain hooked into prerouting/forward/postrouting.
  4. Enable `net.ipv4.ip_forward=1`.
  5. bwrap **no** `--unshare-net`; instead a `pre_exec` closure does
     `setns(netns_fd, CLONE_NEWNET)` between fork and exec to drop
     bwrap into the pre-created netns.
  6. Egress proxy in-pod (envoy, agentgateway, or our own): policy
     per-host allowlists, optional TLS-MITM with a per-sandbox CA.
  7. Teardown: delete per-sandbox nft chain, delete veth, delete
     netns; cleanup stale `agentsandbox-*` netns on pod-agent boot.
  Pool pod needs `CAP_NET_ADMIN` (already on) + nftables/iproute2
  binaries (already installed).
- Per-tenant cgroup v2 leaf at `/sys/fs/cgroup/sandboxes/<uid>` with cpu/mem limits from CR (Spec.Workspace.Size already there; add ResourceRequirements later).
- Landlock and seccomp profiles (moat reference; keep optional).

### Controller: status polling
- Periodic `GetSandbox(uid)` per multi-tenant Sandbox to refresh `Ready` + `Finished` conditions when the tenant exits unexpectedly.
- Drives re-create on `PhaseFailed`.

### Controller: pool pod lifecycle
- **Self-terminating idle pool pods**: prefer letting the pod decide when
  it's idle rather than centralizing the decision in the controller. Plan:
  pod-agent tracks "last sandbox activity" (last CreateSandbox / live
  tenant). After `--idle-timeout` (e.g. 10 min) with zero active sandboxes,
  pod-agent exits gracefully (status SIGTERM-equivalent → process 0).
  Container terminates → pod phase becomes `Succeeded`. Controller's
  existing GC sweep deletes `Succeeded` pool pods (and optionally their
  PVCs, depending on persistence iteration v1/v2/v3).
  - Race: a Sandbox reconcile could pick a pod that's mid-shutdown.
    Already covered by our `ResourceExhausted`/`Unavailable` →
    `clearPoolBinding` retry path; the new attempt picks a different
    pool pod or provisions one.
  - Centralized-GC alternative (if self-terminate proves messy): the
    30-min sweep enumerates pool pods, counts bound Sandboxes per pod,
    and deletes pods idle for ≥ N minutes. Simpler but less responsive.
- Re-provision policy on pool pod death: today next reconcile rebinds to whatever pool-pod selector picks; PVC subpath retains state for resume.
- **Orphan PVC GC**: with the `GenerateName` naming scheme, a partial create (PVC created, pod create failed) leaves an orphan PVC. The pool-pod GC sweep should also list PVCs labeled `agents.x-k8s.io/managed-by=sandbox-controller-pool` with no matching pod and delete them.
- **Pod-delete-then-resume**: the PVC outlives any single pod (same name as the pod, no `-state` suffix). Future work: support deleting a pool pod and re-creating it with the same name + PVC binding so tenants resume without rescheduling state.

### Tighten GC reap predicate
Today `GC.sweepPod` reaps a tenant when its `sandbox_uid` is not in the
live (non-deleting) Sandbox set. That misses one orphan flavour: a
controller crash between `CreateSandbox` succeeding on pool pod A and
the Status write that pinned the Sandbox to pod A. After restart, the
controller picks a fresh pool pod B, calls `CreateSandbox` again
(idempotent — same uid), the tenant on B is live, but A still has a
tenant for the same uid. `Sandbox.Status.Host.PodName == B`, so the GC's
"is uid alive?" check answers yes and doesn't sweep A.

Fix: extend the predicate to also reap when uid is alive **but
`Sandbox.Status.Host.PodName != this_pod`**. Same logic in
`sweepPod`, ~5 lines. Pair with the current 30-min interval (or lower
to 5 min later) for bounded orphan lifetime.

### Capacity by requested resources
Today the Selector counts tenants vs the per-pool `--capacity` flag. Real
scheduling should bin-pack based on each Sandbox's requested resources
(storage size against pool PVC size, eventually CPU/mem). Requires:
- `Sandbox.Spec.Resources.Storage` (and later CPU/mem) field.
- `Selector` sums currently-bound Sandboxes' requested sizes per pool pod
  vs the pod's PVC size.
- Bin-packing across multiple pool pods.

Deferred until we have a concrete user with non-uniform Sandbox sizes;
for now `--capacity` is a fine proxy.

### Persistence iteration plan (from chat)
- **v1 (current)**: pod dies → tenants die. PVC kept; new pool pod reuses subpath. Verify in e2e once boot-recovery lands.
- **v2**: S3 sync of upper subdir (port moat snapshot/session-state). PV becomes cache.
- **v3**: ephemeral PV, S3-backed only.

### SSH polish
- Persist host key to `/var/lib/sandboxes/.ssh_host_key`.
- Pubkey auth: `Sandbox.Spec.SSH.AuthorizedKeys []string` → passed in `CreateSandboxRequest` → pod-agent stores per-uid → `auth_publickey` checks.

### Tests
- Unit: `Selector.Choose` (capacity + sticky), `Provisioner.EnsureOne` (idempotent), `GC.SweepOnce` (orphan + ErrNotFound passthrough).
- envtest: Sandbox with `Spec.Image` → pool pod created, status binds, HTTPRoute applied (when GW set).
- e2e on kind: full path, including `bwrap` actually exec'ing in the pool pod (depends on kind node kernel + `hostUsers` feature gate).

### Misc / known caveats
- `hostUsers: false` requires kubelet `UserNamespacesSupport` (GA 1.33) **and** containerd configured with subuid/subgid maps. Kind nodes don't ship that by default → sysfs mount fails (`error mounting "sysfs" ... operation not permitted`). Currently defaulted to `true` in [internal/pool/podspec.go](internal/pool/podspec.go).
- OCI image volume requires k8s ≥ 1.33.
- Kind nodes are containers themselves → bwrap-in-kind-node is double-nested. Some kernels won't allow overlayfs-in-userns; document workaround.
- Gateway API CRDs not installed by default. `--gateway-name=""` keeps HTTPRoute disabled.
- Pod-agent gRPC is cleartext in-cluster. mTLS deferred.

### Upstream / API design
- The `Sandbox` CRD is now overloaded: `Spec.PodTemplate` OR `Spec.Image`. Open a discussion (mailing list / SIG) about the right long-term shape (two CRDs? Mode enum? union types?). Currently we keep one CRD per user's preference.
