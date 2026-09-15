# OpenClaw Fleet on GKE: sub-second claims, per-employee workspaces, full lifecycle

## Customer requirements

This example exists to serve a concrete customer shape: an enterprise
preparing to deploy **~9,000 per-employee [OpenClaw](https://openclaw.ai)
sandboxes on GKE**, anchored on four architectural pillars — (a) sandbox
identity and storage isolation keyed by employee ID, (b) warm-pool
management that keeps cold-start latency inside tight targets, (c)
end-to-end lifecycle orchestration (provision, image/config rollout,
suspend/resume), and (d) scalability with **20 GB of NAS-backed storage per
employee** and dynamic storage allocation. The customer's PoC test
checklist defines the P0 bar, and **everything deployed by this example
meets every P0 row** — options that trade away a P0 (e.g. hard storage
isolation at the cost of warm claims, or Autopilot at the cost of the
storage daemon) appear only in the [appendix](#appendix-alternatives),
never in the main path.

| P0 | Requirement (from the PoC checklist) | Mechanism in this example | Verified by |
|---|---|---|---|
| 1 | Startup: warm-pool claims (record expected vs. actual), image-cache-accelerated batch creation | `SandboxWarmPool` + warm `SandboxClaim` adoption; image streaming + optional secondary boot disk | `run-test-gke.sh` §1, [`tools/measure-claim-latency.sh`](tools/measure-claim-latency.sh) |
| 2 | Deletion succeeds; all resources released | claim delete cascades sandbox/pod/Service; portal purges alias + workspace | `run-test-gke.sh` §2 |
| 3 | Sleep releases resources; wake-up duration recorded | `Sandbox.spec.operatingMode` (disk-tier) and GKE Pod Snapshots (memory-tier) | `run-test-gke.sh` §3, [`60-snapshots/`](60-snapshots/) |
| 4 | Image updates for sandboxes AND the warm pool, auto-restart post-update; CPU/memory scale-up plan | template bump → `Recreate` pool refresh → rate-limited per-employee rebuilds (same path resizes CPU/memory) | `run-test-gke.sh` §4, [`70-rolling-update.sh`](70-rolling-update.sh) |
| 5 | Storage: high-speed NAS ONLY (block SSD too expensive); NAS attach limits need a solution for 9,000 users × 20 GB | ONE shared Filestore **zonal (high-speed SSD NAS)** volume, per-employee subdirectories **late-bound** into running warm pods — 1 PV per ~5,000 users | `run-test-gke.sh` §5 |
| 6 | Distinct, persistent access address per sandbox (e.g. employee ID) | per-sandbox headless Service + `oc-<employee>` alias + Gateway API + [sandbox-router](../../sandbox-router/) path routing | `run-test-gke.sh` §6 |
| 10 | I/O performance testing on the NAS tier *(customer test team)* | ready-to-run fio harness against the deployed workspace volume | [`tools/fio-workspace-job.yaml`](tools/fio-workspace-job.yaml) |
| 11 | Multi-agent, Feishu/Lark integration, business validation *(customer test team)* | stock OpenClaw with persistent per-employee config; NetworkPolicy egress allows DNS + 443 (provider APIs, Feishu/Lark HTTPS/WSS); inbound webhooks ride the stable per-employee URL | template NetworkPolicy in [`20-openclaw-template.yaml`](20-openclaw-template.yaml) |

P1 rows (daily cost per runtime profile, DR backup/restore, security
testing) are addressed directionally in [Cost dials](#cost-dials),
[`60-snapshots/`](60-snapshots/) (workspace + snapshot restore paths), and
[SSO / IdP integration](#sso--idp-integration); the honest deltas live in
[Known gaps](#known-gaps).

The central design rule, from which everything else follows: **a warm claim
is only sub-second because it changes nothing about the running pod except
metadata.** Per-claim env vars or volumes force a cold start, so *everything
per-employee arrives at claim time without touching the pod spec* — the
employee's Filestore subdirectory is bind-mounted into the already-running
pod by a node daemon, and OpenClaw loads its profile, settings and assets
from that mount. One unified `SandboxTemplate` therefore serves every
employee.

## Architecture

```mermaid
flowchart LR
    subgraph employee [Employee]
        B[Browser<br/>OpenClaw UI]
    end

    subgraph gke [GKE Standard cluster]
        GW[Gateway API<br/>external LB<br/>optional IAP/SSO]
        RT[sandbox-router<br/>path routing + WebSockets]
        FP[fleet portal<br/>lifecycle API]

        subgraph ns [namespace openclaw-fleet]
            WP[SandboxWarmPool<br/>pre-started OpenClaw spares]
            SBX[Sandbox oc-alice's pod<br/>gVisor, OpenClaw :18789]
            ALIAS[Service oc-alice<br/>ExternalName alias]
            SVC[headless Service<br/>sandbox name]
        end

        SND[storage node daemon<br/>privileged DaemonSet]
    end

    FS[(Filestore RWX<br/>users/alice<br/>users/bob ...)]

    B -- "/router/openclaw-fleet/oc-alice/18789/" --> GW --> RT
    B -- "/employees (signup, wake, rebuild)" --> GW --> FP
    RT -- DNS --> ALIAS --> SVC --> SBX
    FP -- claim / suspend / rebuild --> WP
    FP -- "bind alice's subdir" --> SND
    SND -- "mount --bind" --> SBX
    SND --- FS
```

How a claim stays sub-second — the pod is already running before the
employee exists:

```mermaid
sequenceDiagram
    participant P as fleet portal
    participant K as API server
    participant C as claim controller
    participant D as storage daemon
    participant O as OpenClaw pod (warm, spin-waiting)

    Note over O: started minutes ago by the warm pool,<br/>waiting for /workspace/.ready
    P->>K: create SandboxClaim oc-alice
    C->>K: adopt warm sandbox (2-3 metadata writes)
    Note over C,K: SUB-SECOND: no scheduling, no image pull,<br/>no container start
    P->>D: bind users/alice -> pod emptyDir/.openclaw
    D->>O: mount --bind + write .ready signal
    O->>O: OpenClaw starts, HOME=/workspace,<br/>profile & state from the bound mount
    P->>K: alias Service oc-alice -> sandbox DNS
    P-->>P: report adopted_ms / bound_ms / app_ready_ms
```

The employee lifecycle (every transition is a portal endpoint and a
checklist item):

```mermaid
stateDiagram-v2
    [*] --> Ready: POST /employees<br/>(warm claim + bind, ~1-2s)
    Ready --> Suspended: POST .../suspend<br/>pod deleted, alias + workspace survive
    Suspended --> Ready: POST .../wake<br/>new pod + re-bind, seconds
    Ready --> Ready: POST .../rebuild<br/>rolling update, downtime = seconds
    Ready --> [*]: DELETE /employees/id<br/>?purge=true also deletes workspace
    Suspended --> [*]: DELETE
```

## Files

| File | Role |
|---|---|
| [`setup/provision-gke.sh`](setup/provision-gke.sh) | GKE Standard cluster: gVisor node pool, image streaming, Filestore CSI, Gateway API, snapshot bucket, controller install. [`setup/teardown-gke.sh`](setup/teardown-gke.sh) removes everything. |
| [`00-prereqs.yaml`](00-prereqs.yaml) | Namespace, fleet-shared OpenClaw base config, secret shapes (daemon token, provider keys). |
| [`10-storage.yaml`](10-storage.yaml) | Shared Filestore RWX PVC + privileged **storage node daemon** that late-binds `users/<employee>` into warm pods (token-authenticated, NetworkPolicy-scoped). |
| [`20-openclaw-template.yaml`](20-openclaw-template.yaml) | The unified `SandboxTemplate`: gVisor, real OpenClaw image, spin-wait entrypoint, injection policies `Disallowed`, per-sandbox Service, managed fleet NetworkPolicy. |
| [`30-warmpool.yaml`](30-warmpool.yaml) | `SandboxWarmPool` (`updateStrategy: Recreate`) + sizing notes for large fleets. |
| [`40-fleet-portal/`](40-fleet-portal/) | The lifecycle control plane (~300 lines of Python, pattern from [hermes-agents-as-a-service](../hermes-agents-as-a-service/)): signup/suspend/wake/rebuild/delete with per-phase timings. |
| [`50-router/`](50-router/) | Go [sandbox-router](../../sandbox-router/) + GKE Gateway + HTTPRoutes + optional IAP policy: the stable per-employee URL. |
| [`60-snapshots/`](60-snapshots/) | Memory-tier sleep/wake: GKE Pod Snapshots manifests + runbooks for both sleep tiers. |
| [`70-rolling-update.sh`](70-rolling-update.sh) | Rate-limited fleet-wide rebuild after a template change. |
| [`tools/`](tools/) | Claim-latency measurement (percentiles) and a fio job for NAS I/O testing. |
| [`run-test-gke.sh`](run-test-gke.sh) | Asserting end-to-end walkthrough of items 1–6. |

## Walkthrough

### 1. Provision the cluster

```sh
PROJECT_ID=<your-project> ./setup/provision-gke.sh
```

Creates a GKE **Standard** cluster (the storage daemon needs
`privileged` + writable `hostPath`, which Autopilot rejects — see
[Known gaps](#known-gaps) and the [appendix](#appendix-alternatives)),
with a gVisor node pool on `c3-standard-8` (non-E2 and the same series as
the measured results — Pod Snapshots restore only onto the same machine
series, so pick one series and keep the fleet on it), image streaming, the
Filestore CSI driver, Gateway API, and the agent-sandbox controller with
extensions.

### 2. Secrets, storage, template, pool

```sh
kubectl apply -f 00-prereqs.yaml
kubectl -n openclaw-fleet create secret generic storage-daemon-token \
  --from-literal=token="$(openssl rand -hex 24)"
kubectl apply -f 10-storage.yaml     # first Filestore bind takes minutes
kubectl apply -f 20-openclaw-template.yaml -f 30-warmpool.yaml
kubectl -n openclaw-fleet get sandboxwarmpool openclaw-fleet-pool -w
```

Warm spares go `Ready` while parked at their spin-wait — the OpenClaw
process itself starts only after an employee's workspace is bound.

### 3. Portal + router + gateway

```sh
docker build -t $REPO/fleet-portal:demo 40-fleet-portal/ && docker push $REPO/fleet-portal:demo
sed "s|image: fleet-portal:demo|image: $REPO/fleet-portal:demo|" 40-fleet-portal/portal.yaml | kubectl apply -f -
kubectl apply -f 50-router/10-router.yaml -f 50-router/20-gateway.yaml
kubectl -n openclaw-fleet get gateway openclaw-fleet-gateway -w   # wait for an address
```

### 4. Provision employees and measure

```sh
GW=$(kubectl -n openclaw-fleet get gateway openclaw-fleet-gateway -o jsonpath='{.status.addresses[0].value}')
curl -X POST http://$GW/employees -H 'Content-Type: application/json' -d '{"employee": "alice"}'
```

```json
{
  "employee": "alice",
  "path": "/router/openclaw-fleet/oc-alice/18789/",
  "timings": {"adopted_ms": ..., "bound_ms": ..., "app_ready_ms": ..., "total_ms": ...}
}
```

`adopted_ms` is the warm-pool claim — the sub-second number. `total_ms` is
what the employee actually waits, dominated by OpenClaw's own boot. Open
`http://$GW/router/openclaw-fleet/oc-alice/18789/` for alice's UI (add the
gateway origin to `controlUi.allowedOrigins` in
[`00-prereqs.yaml`](00-prereqs.yaml) first). For percentiles across a batch:
`PORTAL_URL=http://$GW tools/measure-claim-latency.sh 20`.

### 5. Sleep, wake, update, delete

```sh
curl -X POST http://$GW/employees/alice/suspend -H "Authorization: Bearer $TOKEN"
curl -X POST http://$GW/employees/alice/wake    -H "Authorization: Bearer $TOKEN"   # reports wake_ms
# fleet-wide image rollout after editing 20-openclaw-template.yaml:
PORTAL_URL=http://$GW ADMIN_TOKEN=... RATE=1 ./70-rolling-update.sh
curl -X DELETE "http://$GW/employees/alice?purge=true" -H "Authorization: Bearer $TOKEN"
```

Memory-tier hibernation (state of the running process preserved, not just
disk): [`60-snapshots/30-snapshot-hibernate.md`](60-snapshots/30-snapshot-hibernate.md).

### 6. Or run the whole thing as one asserting script

```sh
IMAGE_REPO=us-central1-docker.pkg.dev/<project>/fleet ./run-test-gke.sh
```

## Measured results

Measured 2026-09-14 on GKE Standard `1.36.4-gke.1247000`, gVisor node pool
on `c3-standard-8` (`GVISOR_MACHINE_TYPE=c3-standard-8`), image streaming
on, warm pool of 5, OpenClaw `2026.3.23`. `run-test-gke.sh` asserted every
row; percentiles are from `tools/measure-claim-latency.sh` over 5
provisions. One caveat: this run used the Filestore Basic HDD tier
(`standard-rwx`); the example now defaults to the P0-compliant high-speed
`zonal-rwx` tier, on which bind/boot numbers can only improve — re-run
`run-test-gke.sh` and the fio harness (checklist item 10) on the deployed
tier to record the numbers of record.

| Checklist item | Expected | Measured |
|---|---|---|
| Warm claim adoption (`adopted_ms`) | < 1 s | **p50 174 ms, max 201 ms** |
| + workspace bind (`bound_ms`) | sub-second | p50 177 ms |
| + OpenClaw boot (`app_ready_ms`) | seconds | p50 2.9 s |
| Signup end-to-end (`total_ms`) | a few seconds | **p50 3.26 s, max 3.32 s** |
| Cold start (pool empty, image cached via streaming) | ~3–15 s | 5.7 s end-to-end |
| Suspend → resources released | pod gone | ~1.7 s, pod deleted; Service/alias/workspace retained |
| Wake (`wake_ms`, disk tier: pod recreate + re-bind + boot) | seconds | 22.3 s |
| Rebuild downtime (`downtime_ms`, template image/CPU change) | seconds | 5.2 s |
| Deletion → everything released (incl. workspace purge) | complete | verified, no residue |

The warm-vs-cold contrast is the warm pool's value: the *claim* is ~174 ms
vs ~5.7 s cold — and cold assumes the image is already node-local (the
first-ever pull of the 1.2 GB OpenClaw image took ~27 s, which is what
image streaming / secondary boot disks remove for batch creation). Wake is
dominated by pod recreation and OpenClaw's boot; for wake-with-memory-state
(skipping the boot), see the Pod Snapshots tier in
[`60-snapshots/`](60-snapshots/).

## Design notes for large fleets

### Storage: why one shared volume + subdirectories

At 20 GB per employee, thousands of individual PVs hit two walls: NAS
volume-count limits and cost — P0-5 makes both explicit ("only high-speed
NAS", "a solution is required for 9,000 PVs"). This example uses **one
shared Filestore zonal (high-speed SSD NAS) volume with a per-employee
subdirectory late-bound into the running warm pod**: 1 PV per ~5,000
employees (zonal scales to 100 TiB/instance, so a 9,000-seat fleet shards
across 2 instances), and the only storage shape compatible with
**sub-second claims**, because nothing about the pod spec changes per
user. Isolation is directory-level: the bind mount scopes each pod to its
own subdirectory (the enforcing boundary is the daemon, not the storage
API — see [Known gaps](#known-gaps)).

**Dynamic allocation and resizing**: the shared instance grows online —
patch `fleet-master-pvc`'s storage request and the Filestore CSI driver
expands the instance with no pod restarts. Provision for the current
fleet, not the theoretical maximum, and grow ahead of demand. Two honest
limits, both in [Known gaps](#known-gaps): shrink is not supported on this
shape, and per-user quota enforcement is your fleet controller's job.
Per-employee snapshots are subdirectory copies (`snapshots/<id>`),
restorable in sub-seconds via re-bind.

Alternative storage shapes (per-user PVCs, Filestore multishares) trade
away P0-1 or P0-5 and therefore live in the
[appendix](#appendix-alternatives) only.

### Warm pool sizing and batch creation

Pool size covers the peak **claim rate**, not the fleet size: 9,000
employees who claim over a morning hour need warm spares ≈ peak
claims/minute × replenishment lag, not 9,000 spares. Shard high rates
across multiple pools (`--sandbox-warm-pool-concurrent-workers` ≥ pool
count) and shape refill bursts with the pool controller's
`MaxRefillRate`/`ReplenishDelay`. For the batch-creation path (pool refill,
cold starts), image pull dominates — enable
[image streaming](https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming)
(done by `provision-gke.sh`) and preload the OpenClaw image on a
[secondary boot disk](https://cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading)
(optional section in the provisioning script) so a node-pool's worth of
sandboxes starts without registry pulls.

### SSO / IdP integration

The demo runs unauthenticated at the edge (portal/router tokens only).
For production: enable **IAP on the Gateway** via the provided
[`50-router/30-iap-optional.yaml`](50-router/30-iap-optional.yaml)
(`GCPBackendPolicy`); enterprise SAML/OIDC IdPs plug into IAP through
[Workforce Identity Federation](https://cloud.google.com/iap/docs/use-workforce-identity-federation).
IAP asserts the employee's identity in signed headers; mapping that
identity to `oc-<employee>` authorization is the thin piece a production
portal adds (the router's `scoped-token`/`tokenreview` modes are the
building blocks — see its [README](../../sandbox-router/README.md)).

### Cost dials

Suspend (`operatingMode`) releases all compute while keeping identity +
storage: an employee active 8h/day costs ~1/3 of always-on. The portal's
idle sweeper (`IDLE_TIMEOUT`) automates this; Pod Snapshots
([`60-snapshots/`](60-snapshots/)) make the wake seamless for in-flight
sessions. Warm pools can follow the daily curve with HPA/KEDA
([hpa-swp-scaling](../hpa-swp-scaling/), [keda-scale-to-zero](../keda-scale-to-zero/)).

## Known gaps

What this example does **not** yet deliver against the customer's
requirements, found during live validation and a platform-level gap review.
None of these break a P0 checklist row as tested, but all of them are real
costs on the road from PoC to 9,000-seat production.

1. **The Autopilot trilemma is unresolved.** The customer wants zero node
   ops (Autopilot) + ~1 s warm claims + claim-time persistent workspaces
   *simultaneously*. The storage daemon needs `privileged` + writable
   `hostPath`, which GKE Warden rejects on Autopilot
   (`autogke-disallow-privilege`, `autogke-no-write-mode-hostpath`), so
   this example requires **GKE Standard**. Every known Autopilot path
   today gives something up (see [appendix](#appendix-alternatives));
   productizing storage late-binding as a managed, Autopilot-compatible
   capability is an open ask on the platform.
2. **The late-bind safety invariant is convention, not platform.**
   "Unbind before any pod teardown" lives in portal code; any out-of-band
   pod delete (kubectl, node drain, namespace delete) either wedges the
   pod in `Terminating` or — worse — lets emptyDir cleanup delete
   *through* the bind and wipe the employee's NFS workspace. The platform
   has no finalizer hook or `WorkspaceBinding`-style CRD for this
   lifecycle yet; the teardown rules below are the mitigation.
3. **Memory-tier sleep is a manual, GKE-only runbook with sharp edges.**
   Platform-native suspend is disk-tier (pod deleted; wake = reschedule +
   app boot, measured 22.3 s). Memory-intact wake rides GKE Pod Snapshots:
   manual trigger objects, gVisor-only, same-machine-series restores (never
   E2), and **the pod spec is the cache key — any P0-4 template update
   silently invalidates every hibernated session's snapshot** (they cold
   start, no error, no event). Namespace-wide trigger RBAC is the tenancy
   boundary, and there is a controller-version pin caveat. See
   [`60-snapshots/30-snapshot-hibernate.md`](60-snapshots/30-snapshot-hibernate.md).
4. **No in-platform idle detection or auto-suspend.** The suspend
   *primitive* is platform (`operatingMode`, lifecycle `shutdownTime`);
   deciding *when* is the operator's job. The portal's `IDLE_TIMEOUT`
   sweeper is demo-grade: in-memory clock, single replica, and it only
   sees portal traffic, not router traffic. Auto-suspend/scale-to-zero are
   on the agent-sandbox roadmap.
5. **Per-claim customization is deliberately narrow.** Warm claims allow
   only metadata; per-claim env/volumes force cold starts (and this
   template rejects them). There is **no per-claim CPU/memory sizing** —
   employee size tiers require one template + one warm pool per size
   class. Per-employee secrets ride the workspace mount, not pod env.
6. **Dynamic storage resizing is grow-only, and quotas are unenforced.**
   The shared zonal instance grows online, but cannot shrink, and nothing
   stops one employee filling space budgeted for another — per-user quota
   enforcement (or a move to an enforcing storage shape, with its P0
   trade-offs) is required before production.
7. **Image-cache acceleration for batch creation is half-delivered.**
   Image streaming is automated; the stronger secondary-boot-disk preload
   is a documented sketch in `provision-gke.sh`, deliberately not
   automated. P0-1's batch bar currently rests on streaming alone (first
   ever pull of the 1.2 GB image: ~27 s; streamed cold start: 5.7 s).
8. **Sub-second is conditional on warm spares.** Pool-empty claims degrade
   gracefully to ~5.7 s cold starts; refill throughput (~85 sandbox
   creates/s cluster-wide, serialized per pool) bounds batch onboarding.
   The full 9,000-seat shape — including ~2 Service objects per employee
   (~18k Services) and cluster-DNS load — has not been validated at scale;
   documented validation covers pools of 1,000–2,500 and 10–20 claims/s.
9. **The portal and daemon are example-grade glue.** Single-replica Flask
   portal with in-memory state and shared admin token; privileged daemon
   with node-root hostPath whose single shared token authorizes
   bind/unbind/delete on any workspace; NetworkPolicies that are silently
   unenforced without Dataplane V2 (the provisioning script enables it —
   verify on existing clusters). The customer's web-portal ask (async
   jobs, bulk rate-limited ops, monitoring, audit, SSO+RBAC) is a
   production build on these patterns, not a shipped component.
10. **Measured numbers predate the tier switch.** The results table was
    recorded on `standard-rwx` (HDD); the example now deploys `zonal-rwx`
    (high-speed SSD NAS) to meet P0-5. Bind and boot latencies should be
    equal or better on SSD — re-measure on the deployed tier, and run
    checklist item 10's fio harness there.

## Production hardening notes

The storage daemon and portal are example-grade orchestration, kept small
so every mechanism is visible. A production fleet manager adds: a durable
job queue with rate-limited bulk operations, audit logging, per-user
storage quotas, HA for the portal, TLS + real edge auth throughout, and the
router's non-demo authorization modes. The daemon's HTTP contract
(`bind`/`unbind`/`delete`) is deliberately tiny so it can be replaced by a
proper controller reconciling a `WorkspaceBinding`-style CRD.

Two teardown rules learned from live validation (both encoded in the
portal, both worth a finalizer in production, as in
[latebind-storage-gke-sandbox](../latebind-storage-gke-sandbox/)):

1. **Always unbind before any pod teardown begins.** If the bind is still
   mounted when termination starts, the emptyDir cleanup deletes THROUGH
   it and wipes the employee's NFS workspace; if it is unbound first, the
   workspace provably survives.
2. **Pods deleted outside the portal wedge in `Terminating`.** Any path
   that bypasses the unbind (kubectl delete pod, namespace deletion, node
   drain) leaves the mount in place and the kubelet cannot clean the
   volume — recover with a manual daemon `unbind` (or force-delete when
   discarding the cluster). A finalizer on the claim/sandbox is the
   production answer.

## <a name="appendix-alternatives"></a>Appendix: alternatives that do not meet the P0 bar

Everything in the example's main path satisfies every P0 checklist row.
The options below solve real problems, but each one trades away a P0 —
they are documented here so the trade is made consciously, never by
default.

### Filestore multishares / per-user PVCs (trades away P0-1, and per-user PVCs also P0-5)

| | Shared volume + subdirectories (main path) | [Filestore multishares](https://cloud.google.com/filestore/docs/multishares) | Per-user `volumeClaimTemplates` PVCs |
|---|---|---|---|
| PVs for 9,000 users | **1 per ~5,000 users** | 9,000 (80 shares/instance ⇒ ~113 Enterprise instances) | 9,000 — hits NAS attachment limits (**violates P0-5**) |
| Claim-time attach | instant `mount --bind` into a running pod | PVC attach ⇒ pod restart ⇒ **cold start ~15 s (violates P0-1)** | same cold-start violation |
| Isolation | directory-level, enforced by the daemon | true per-share capacity isolation, per-PV observability, CMEK | true per-PV isolation |
| Resizing | grow-only on the instance; quotas your controller's job | per-share resize, both directions | per-PVC resize |
| Snapshots | subdirectory copies, sub-second restore via bind | not supported on multishare instances | storage-class dependent |

Choose multishares only if hard storage-API isolation outranks warm
claims for your fleet — and record that this abandons the ~1 s claim
target for persistent-workspace users.

### Autopilot paths (trade away P0-1 or add an approval gate)

The daemon's `privileged` + writable `hostPath` are Warden-rejected on
Autopilot, so the main path requires GKE Standard. If zero node ops is
non-negotiable:

- **Autopilot + customer-managed
  [WorkloadAllowlist](https://cloud.google.com/kubernetes-engine/docs/how-to/autopilot-privileged-allowlists)**:
  exempts exactly these two constraints for approved workloads — keeps
  every P0 intact, but customer-managed allowlists require eligibility
  approval through Cloud Customer Care, so it cannot be the default here.
- **Autopilot without the daemon**: pre-provisioned per-user PVCs (e.g.
  multishares) with direct-built sandboxes — zero node ops, but claim
  time moves from ~1 s to ~15 s (**violates P0-1**) and inherits the
  per-user-PV shape above.

### Demo-cost storage tier (trades away P0-5)

`standard-rwx` (Filestore Basic HDD) halves the demo's storage cost and
is fine for a throwaway validation cluster, but it is not "high-speed
NAS" — switch `10-storage.yaml` back only for clusters whose numbers you
will not present as the PoC record.
