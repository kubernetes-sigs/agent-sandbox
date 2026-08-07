# KEP-968: Auto Suspend/Resume Lifecycle for Claw-like Sandboxes

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [High-Level Design](#high-level-design)
    - [Idle Detection (External Signals)](#idle-detection-external-signals)
    - [Cron Pre-Wakeup (Schedule-Reader Sidecar)](#cron-pre-wakeup-schedule-reader-sidecar)
    - [Sandbox Lifecycle Daemon (Informer-Backed)](#sandbox-lifecycle-daemon-informer-backed)
    - [Wakeup Proxy (Traffic Broker)](#wakeup-proxy-traffic-broker)
    - [API Changes](#api-changes)
    - [Implementation Guidance](#implementation-guidance)
  - [Implementation Plan (PRs)](#implementation-plan-prs)
- [Scalability](#scalability)
- [Future Work: OpenClaw-Internal Idle Signaling](#future-work-openclaw-internal-idle-signaling)
  - [What OpenClaw already exposes](#what-openclaw-already-exposes)
  - [Gap](#gap)
  - [Options (ranked)](#options-ranked)
  - [Trade-off](#trade-off)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

This KEP adds server-side lifecycle management so an **OpenClaw** gateway running in a
Sandbox can scale to zero when idle and come back automatically — **without any change to
OpenClaw itself**. OpenClaw keeps its default configuration, its heartbeat, and its own
internal cron scheduler. All lifecycle intelligence lives in two components — an always-on
**Wakeup Proxy** and an informer-backed **Sandbox Lifecycle Daemon** — plus a small
**schedule-reader sidecar** that reads
OpenClaw's persisted cron schedule from the PVC. No new controller is added to the sandbox
controller-manager.

## Motivation

Sandbox suspend/resume ([KEP-119], [KEP-694]) is driven by clients flipping
`spec.operatingMode`. An OpenClaw gateway has no client babysitting it: idle, it keeps
burning resources; suspended, nothing wakes it for traffic or for its own scheduled crons.

A hard constraint shapes this design: **OpenClaw is treated as a black box.** We will not
add endpoints to it, change its config, or disable its heartbeat or cron. Therefore the
control plane cannot ask OpenClaw "are you idle?" or "when is your next job?" — it must
derive both from externally observable signals and from state OpenClaw already persists.

### Goals

- Suspend an idle OpenClaw Sandbox automatically, using only signals observable from outside
  the OpenClaw container.
- Wake a suspended OpenClaw Sandbox on inbound traffic (proxy) and shortly before its next
  internal cron run (scheduled pre-wakeup), so jobs run on time.
- Preserve OpenClaw state (workspace, history, `openclaw.sqlite`) across suspend/resume.
- Add **no** controller-manager controller and require **zero** changes to OpenClaw.

### Non-Goals

- Any modification to OpenClaw (code, config, heartbeat, or cron).
- Large-scale (100k) optimization, central state databases, or diskless storage.
- A new controller in the sandbox controller-manager.
- CRIU/memory checkpointing of the running process (future).
- A new connection/transport SDK (tracked separately).

## Proposal

### User Stories

- *As an OpenClaw operator*, my gateway Sandbox suspends itself after a quiet window with no
  traffic and no internal activity — I changed nothing in OpenClaw to get this.
- *As an OpenClaw user*, my first message after idle transparently wakes the gateway via the
  proxy; my workspace and history are intact.
- *As an OpenClaw user with crons*, a job scheduled for 12:00 finds the gateway already warm
  because the daemon read the schedule and woke it at ~11:58.

### High-Level Design

```
Traffic (HTTP/WS) ───▶ Wakeup Proxy ──(resume)──▶ Lifecycle Daemon ───┐
                       (activity timer,            (informer-backed)   │ patch operatingMode
                        buffers, replays)                              ▼
   pod CPU (metrics-server) ──────────▶ idle?  ── suspend / wake ──▶ Sandbox
                                                                       │
                                              ┌────────────────────────┴───────────┐
                                              │ Sandbox Pod                          │
                                              │  OpenClaw (unmodified)  +  sidecar    │
                                              │  /root/.openclaw (PVC) ◀─ read-only ─┘│
                                              │            └─ openclaw.sqlite          │
                                              └───────────────────────────────────────┘
```

#### Idle Detection (External Signals)

Because we cannot ask OpenClaw whether it is busy, the daemon combines two black-box signals
and only suspends when **both** indicate quiet for `agents.x-k8s.io/max-idle-time`:

1. **Proxy activity.** The Wakeup Proxy is the gateway's front door and already sees every
   request and WebSocket connection. It tracks last-activity time and open-connection count;
   idle requires no traffic and zero open connections.
2. **Pod CPU floor.** The daemon reads pod CPU from metrics-server/cAdvisor. OpenClaw's
   *internal* work — cron jobs, heartbeat cycles, background tasks — is invisible to the
   proxy but spikes CPU. Requiring CPU below a threshold prevents suspending mid-job without
   any OpenClaw API.

While suspended, OpenClaw's heartbeat simply does not fire; it resumes on the next wake. Note
that an active default heartbeat keeps CPU periodically warm, so suspension occurs only in
genuinely quiet windows — an accepted trade-off of leaving OpenClaw untouched.

This external inference is an interim approach. A higher-fidelity option — having OpenClaw
push an idle signal from inside via its hook system — is described in
[Future Work: OpenClaw-Internal Idle Signaling](#future-work-openclaw-internal-idle-signaling).

#### Cron Pre-Wakeup (Schedule-Reader Sidecar)

OpenClaw persists its cron schedule in `openclaw.sqlite` on the PVC. The
[`cron_jobs`](https://github.com/openclaw/openclaw) table stores a pre-computed
`next_run_at_ms` per job and an `enabled` flag, so no cron-expression parsing is needed.

A small **schedule-reader sidecar** runs in the Sandbox pod, mounts the same PVC read-only,
and exposes one endpoint:

```jsonc
// GET http://<pod>:<sidecar-port>/next-cron
{ "nextRunAtMs": 1749556800000 }   // = SELECT MIN(next_run_at_ms) FROM cron_jobs WHERE enabled = 1
```

This keeps OpenClaw itself untouched — the sidecar is a separate infra container reading
shared state, not part of OpenClaw. Just before suspending, the daemon calls `/next-cron`,
subtracts a configurable pre-warm lead (e.g. 2 min), and stores the result in
`agents.x-k8s.io/next-wakeup`. OpenClaw's built-in catch-up (it re-reads the DB on boot and
runs overdue jobs) remains the backstop if a wake is ever missed.

#### Sandbox Lifecycle Daemon (Informer-Backed)

The daemon is a **standalone Deployment** (PR 2) exposing
`POST /v1/sandbox/suspend`, `POST /v1/sandbox/resume`, `GET /v1/sandbox/status`. It replaces
the prototype's `List(all)` poll loop with a client-go **SharedInformer** over Sandboxes plus
a delaying workqueue:

- **Idle-suspend.** When proxy-idle and CPU-floor both hold for `max-idle-time`, read
  `/next-cron` from the sidecar, write `next-wakeup = nextRun − lead`, then patch
  `spec.operatingMode: Suspended`.
- **Scheduled pre-wakeup.** For suspended Sandboxes carrying `next-wakeup`, enqueue with a
  delay (`AddAfter`) equal to the time remaining; on dequeue, patch `operatingMode: Running`
  and clear the annotation. No periodic full scan, no controller.

Resume **always** acts on the existing suspended Sandbox by patching `operatingMode: Running`
so the same PVC is remounted and the user's state returns. Resume **never** uses a
`SandboxWarmPool`: warm pools only produce *new*, already-mounted instances and cannot
restore a specific suspended gateway's state. Initial provisioning of brand-new gateways
(which may use a warm pool for fast cold starts) is a separate concern and **out of scope**
for this KEP.

#### Wakeup Proxy (Traffic Broker)

The always-on proxy (PR 4, on the Go `sandbox-router` [PR #838]) handles
**wake-on-traffic** and owns the activity timer for idle detection. On a request to a
suspended Sandbox it buffers the connection, calls the daemon's `/v1/sandbox/resume`, watches
the informer cache for `Ready: True`, then replays the buffered request. It holds no durable
state.

#### API Changes

- **`SandboxClaim.spec.operatingMode`** (`Running` | `Suspended`): tenants control lifecycle
  through their namespaced claim; the claim controller mirrors it to the adopted Sandbox
  (PR 1).
- **No new CRDs.** Scheduling uses annotations: `agents.x-k8s.io/max-idle-time`,
  `agents.x-k8s.io/next-wakeup`.
- **No OpenClaw API changes.** The only addition to the pod is the schedule-reader sidecar in
  the `SandboxTemplate`.

#### Implementation Guidance

- **Schedule-reader sidecar**: a ~50-line Go/Node container opening `openclaw.sqlite`
  read-only (SQLite WAL allows concurrent reads while OpenClaw runs) and serving `/next-cron`.
  Add it alongside the OpenClaw container in
  `examples/openclaw-sandbox/openclaw-template-claim.yaml`, mounting `workspaces-pvc`
  read-only. Couples only to the `cron_jobs.{enabled,next_run_at_ms}` columns.
- **Daemon**: replace `internal/lifecycle/daemon/server.go`'s ticker +`List(all)` loop
  (`checkAndWakeupSandboxes`) with a `cache.SharedIndexInformer` + a
  `workqueue.RateLimitingInterface`; schedule wakeups with `queue.AddAfter`. Make resume
  asynchronous (patch and return; the proxy observes readiness via the cache).
- **Remove the prototype's warm-pool resume fallback.** The current `handleResume` in
  `internal/lifecycle/daemon/server.go` creates a `SandboxClaim` against `openclaw-warmpool`
  when the Sandbox is not found and treats that as a resume. This is incorrect — it leases a
  fresh, blank instance with none of the user's state. Resume must only patch the existing
  Sandbox's `operatingMode`; if the Sandbox does not exist, that is an error, not a lease.
- When clearing `next-wakeup` in a merge patch, set the key to JSON `null` (a `MergeFrom`
  delete of a map key otherwise no-ops and re-triggers the wake every reconcile).
- Treat an unreachable sidecar / no enabled jobs as "no scheduled wakeup" (rely on traffic
  wake + OpenClaw catch-up).
- **PVC persistence**: mount OpenClaw's `/root/.openclaw` on a `volumeClaimTemplates` PVC so
  workspace, history, and `openclaw.sqlite` survive pod deletion.

### Implementation Plan (PRs)

The work is split into independently reviewable PRs. PR 1 and PR 2 are the foundation and can
land in parallel; the rest build on PR 2.

| PR | Title | Size | Depends on | Unlocks |
| :-- | :-- | :-- | :-- | :-- |
| **1** | Propagate `operatingMode` to `SandboxClaim` | L | — | Tenant-driven suspend/resume via the namespaced API |
| **2** | Informer-based Lifecycle Daemon | XL | — | Correct suspend/resume control surface + scheduled wake |
| **3** | Idle detection & auto-suspend | L | 2, (4) | **Automatic** suspend of inactive sandboxes |
| **4** | Wake-on-traffic in the sandbox-router (Wakeup Proxy) | L | 2 | **Resume on traffic / API invocation** |
| **5** | Schedule-reader sidecar + pre-wakeup wiring | M | 2 | On-time wake for OpenClaw's internal crons |
| **6** | Example manifests + E2E | M | 2–5 | Verifies the full suspend↔resume contract |

**PR 1 — Propagate `operatingMode` to `SandboxClaim`.**
Add `spec.operatingMode` (`Running` | `Suspended`) to `SandboxClaimSpec`; the claim
controller mirrors it onto the adopted Sandbox; regenerate clients/deepcopy.
Files: `extensions/api/v1beta1/sandboxclaim_types.go`,
`extensions/controllers/sandboxclaim_controller.go`, `clients/k8s/`.

**PR 2 — Informer-based Lifecycle Daemon.**
Replace `internal/lifecycle/daemon/server.go`'s `List(all)` ticker with a
`SharedIndexInformer` + delaying `workqueue`; schedule wakeups via `AddAfter`; make resume
asynchronous; clear the `next-wakeup` annotation with JSON `null`; **remove the warm-pool
resume fallback**; implement `GET /v1/sandbox/status`.
Files: `internal/lifecycle/daemon/server.go`, `cmd/sandbox-lifecycle-daemon/main.go`,
`internal/lifecycle/daemon/lifecycle-daemon-deployment.yaml`.

**PR 3 — Idle detection & auto-suspend.**
Daemon enforces the idle policy: pod CPU floor via metrics-server **and** the proxy's
activity signal must both be quiet for `agents.x-k8s.io/max-idle-time` before patching
`operatingMode: Suspended`. (The CPU half can land before PR 4; the traffic half consumes
PR 4's activity feed.)

**PR 4 — Wake-on-traffic in the sandbox-router (Wakeup Proxy).**
On the Go router ([PR #838]): detect a suspended target, buffer the request/WS handshake,
call the daemon's `/v1/sandbox/resume`, watch the informer cache for `Ready: True`, then
replay. Track last-activity + open-connection count and expose it to the daemon for PR 3.

**PR 5 — Schedule-reader sidecar + pre-wakeup wiring.**
New sidecar container reading `openclaw.sqlite` (`cron_jobs.{enabled,next_run_at_ms}`)
read-only and serving `GET /next-cron`; add it to the `SandboxTemplate` mounting the PVC
read-only; daemon queries it just before suspend and sets `next-wakeup = nextRun − lead`.

**PR 6 — Example manifests + E2E.**
Finalize `examples/openclaw-sandbox/openclaw-template-claim.yaml` (PVC `volumeClaimTemplates`,
sidecar, router/proxy wiring) and extend `test/e2e/extensions/openclaw_e2e_test.go` to cover
auto-suspend-on-idle, resume-on-traffic, scheduled pre-wakeup, and PVC state retention.

> [!NOTE]
> Out of scope (separate KEPs): the JS/TS SDK, warm-pool-based fast provisioning of new
> gateways, CRIU/memory checkpointing, and large-fleet (100k) optimization.

## Scalability

This KEP targets a single OpenClaw gateway per Sandbox with per-Sandbox PVC persistence;
large-fleet optimization is out of scope. The one scale-relevant choice is using a
SharedInformer in the daemon instead of a periodic `List(all)`, keeping API-server cost flat
as the number of watched Sandboxes grows. The sidecar's `/next-cron` query is a single
indexed read invoked only at suspend time; CPU probes hit metrics-server, not OpenClaw.

## Future Work: OpenClaw-Internal Idle Signaling

The MVP infers idleness from outside (proxy activity + CPU floor) precisely because OpenClaw
is treated as a black box. A higher-fidelity future option is to have OpenClaw **tell us**
when it is entering an idle state, using its own extension points — without forking its core.
This section records what we found to be feasible.

### What OpenClaw already exposes

- **Internal hook system** (`src/hooks/`): installable "hook packs" dispatched on lifecycle
  events — `agent:bootstrap`, `gateway:startup`, `gateway:shutdown`, `gateway:pre-restart`,
  `message:received` / `message:sent`, `session:compact`/`reset`, `command:new`/`reset`. A
  handler is `(event) => Promise<void>`, so it can perform outbound I/O (e.g. an HTTP POST to
  the daemon).
- **Safe-stop preflight** (`createSafeGatewayRestartPreflight()` in
  `src/infra/restart-coordinator.ts`): an authoritative count of in-flight work
  (queue + pending replies + embedded runs + active tasks) with a `safe` flag.
- **Cron next-run** (`cron_jobs.next_run_at_ms`) for the wake time.

### Gap

There is **no event emitted when work drains to zero** today — the preflight is pull-only,
and `gateway:shutdown` / `pre-restart` fire only after something already decided to stop, not
on idle onset. So an internal idle signal needs a small amount of new logic, not merely a
subscription.

### Options (ranked)

1. **Sandbox-lifecycle hook pack (recommended).** Ship an installable hook pack that
   subscribes to `message`/`command`/`session` events to track activity, debounces an idle
   timer, and when `createSafeGatewayRestartPreflight().safe` holds with no activity for the
   window, POSTs `{ idle: true, nextWakeup }` to the daemon. This moves idle detection inside
   OpenClaw via its **supported** extension API (not a core change), is authoritative about
   internal work the CPU/proxy heuristics only approximate, and can carry `nextWakeup` too —
   removing the need for the schedule-reader sidecar.
2. **Gateway plugin.** OpenClaw's plugin SDK (`src/plugin-sdk/`, `extensionAPI.ts`) can run
   background logic and reach gateway state; heavier than a hook pack but more capable (e.g.
   richer drain coordination on `gateway:shutdown` for a clean suspend).
3. **Agent skill / tool.** Rejected for the lifecycle signal: invocation depends on the model
   choosing to call it, so it is non-deterministic and the wrong layer for a control signal.
   (It could still serve as a manual "suspend me now" affordance.)

### Trade-off

An internal signal is more accurate (no false-suspend during low-CPU-but-busy waits, e.g.
awaiting a slow tool/LLM) and collapses idle + next-wakeup into a single push, but it
re-introduces an OpenClaw-side component (hook pack/plugin) to build and maintain — the exact
coupling the MVP avoids. Recommended sequencing: ship external inference now (PRs 3–4), then
add the hook pack as an opt-in, higher-fidelity source the daemon prefers when present,
falling back to external inference when it is absent.

## Alternatives

1. **Add `/v1/health/idle` and `/v1/cron/next` to OpenClaw.** Rejected by constraint:
   OpenClaw must not be modified. External signals (proxy + CPU) and the read-only sidecar
   provide the equivalent information without touching it.
2. **A new controller in the sandbox controller-manager.** Rejected: it expands the core
   operator's surface and couples lifecycle to the controller's release cycle. An
   informer-backed standalone daemon gives the same watch efficiency while staying
   independently deployable.
3. **Catch-up only (no pre-wakeup).** Simpler, but scheduled jobs would fire only when the
   gateway is next woken by traffic — arbitrarily late. The sidecar read gives on-time crons
   at the cost of coupling to two stable DB columns.
4. **Duplicating OpenClaw's schedule into Kubernetes CronJobs.** Rejected: forks the source
   of truth. Reading the persisted `next_run_at_ms` keeps OpenClaw's scheduler authoritative.
5. **Polling daemon (the existing prototype).** Rejected: `List(all)` every few seconds and
   blocking per-request resume waits are wasteful — hence the informer.

[KEP-119]: ../119-sandbox-suspended-state/README.md
[KEP-694]: ../694-kep-for-suspend-and-resume-for-beta/README.md
[PR #838]: https://github.com/kubernetes-sigs/agent-sandbox/pull/838
