# KEP-1327: SandboxWarmPool health and template transition status

Status: provisional. Tracking: [#1326](https://github.com/kubernetes-sigs/agent-sandbox/issues/1326) and [#1327](https://github.com/kubernetes-sigs/agent-sandbox/issues/1327).

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [High-Level Design](#high-level-design)
    - [API Changes](#api-changes)
    - [Durable Observation and Deadlines](#durable-observation-and-deadlines)
    - [Implementation Guidance](#implementation-guidance)
  - [Compatibility and Delivery](#compatibility-and-delivery)
  - [Validation](#validation)
  - [Design Review Decisions](#design-review-decisions)
- [Scalability](#scalability)
- [Alternatives](#alternatives)
- [References](#references)
<!-- /toc -->

## Summary

Define a common status model for SandboxWarmPool replenishment health and template transitions. A warm pool stores idle resources that Claims consume and the controller replenishes. The API should expose current stock, its relationship to the desired template, and durable evidence about replenishment or template evaluation. This KEP covers the semantics and naming requested by Janet Kuo for both issues. Condition names, deadline semantics, and the durable observation protocol remain design decisions under review.

## Motivation

The existing status reports `replicas`, `readyReplicas`, `selector`, and `observedGeneration`. Template content can change independently of the pool's generation. OnReplenish preserves existing Ready members until they are consumed or removed, so a pool with Ready stock can still have an unevaluated template. Recreate removes stale members, but continuous consumption can keep a healthy pool below desired stock throughout a template transition.

Operators need to answer several questions with distinct evidence:

| Question | Evidence |
| --- | --- |
| How much Ready stock exists now? | Current inventory counters; Claim-specific eligibility still applies. |
| Does stock meet the configured target? | Current counters and `spec.replicas`. |
| Which effective template has the controller observed? | Template source identity and an effective revision. |
| Does replenishment face a diagnosed obstruction? | Template errors and member conditions, interpreted with grace/admission rules. |
| Has the effective template produced an observed Ready member? | A durable observation associated with that revision. |
| How much old-template stock remains? | Membership and freshness observations, including terminating stale members where relevant. |

The goals are durable, queryable status; correct behavior through Claim consumption and restarts; and a common API foundation for [RollingUpdate #426](https://github.com/kubernetes-sigs/agent-sandbox/issues/426). The design preserves each strategy's replacement policy and limits additional status to information with a defined consumer and evidence source.

## Proposal

### User Stories

An operator diagnosing an empty pool can see whether normal refill is under way, creation is blocked, or members remain unschedulable beyond the configured grace period.

A GitOps controller applying a template can identify the observed source and distinguish an unevaluated template from one with recorded readiness evidence. Its policy can separately check the desired amount of Ready stock.

A platform using OnReplenish can retain existing stock without declaring an untested template successful or treating a lack of rotation demand as a timeout.

### High-Level Design

The recommended direction is to expose inventory facts continuously, diagnose specific replenishment obstructions, and evaluate template evidence in the context of actual refill or rotation demand. Define condition meanings before selecting their names. Deadline selection follows the definition of the observation being bounded.

Descriptive state names in this table are concepts under discussion:

| Scenario | Inventory and template evidence | Required interpretation |
| --- | --- | --- |
| Healthy refill with continuous Claims | Ready stock fluctuates below target; members can leave immediately after becoming Ready. | Report stock accurately. A deficit alone establishes demand, not a diagnosed template fault. |
| Idle OnReplenish after a template edit | Old Ready members remain; the new revision can have no members. | Existing stock can serve eligible Claims; current-template evaluation is pending. |
| First natural refill under OnReplenish | A member is requested from the current effective revision. | Evaluate that refill attempt; retain the lazy rotation policy. |
| Recreate with a valid template | Stale members drain and fresh members become Ready. | Record revision-scoped readiness evidence and current rotation facts. |
| Unscheduled members past grace | Member status supplies a specific scheduling diagnosis. | Surface the obstruction while preserving usable stock facts. |
| Scheduling-gated members | Admission policy intentionally delays scheduling. | Describe the admission wait and apply an explicitly chosen deadline policy. |
| Missing or unreadable template | Template identity or content cannot be freshly evaluated. | Expose the lookup failure and preserve the previous observation with its identity. |
| Desired replicas equals zero | There is no requested idle stock; old members can still be terminating. | Describe the zero-stock intent and actual members. Empty stock supplies no new readiness evidence. |
| Restart or status conflict | Committed status and persisted objects remain authoritative. | Resume the same evaluation and retain committed evidence. |
| Corrected template | The effective revision changes. | Scope evaluation and result to the new revision. |

#### API Changes

Candidate shared facts are `updatedReplicas`, `updatedReadyReplicas`, and observed template identity. Counts cover active, pool-owned members, with ownership and termination filtering consistent with current accounting. A successful controller list establishes observed inventory.

The proposed `updatedReplicas` and `updatedReadyReplicas` fields are optional `*int32` values with `omitempty` and `Minimum=0`. A non-nil value records an observed count, including an explicit zero. An unavailable member list or unresolved effective revision sets both fields to nil, which serializes as omitted and means unknown. Publish both counts with the source identity used to evaluate them. Existing `replicas` and `readyReplicas` retain their `int32,omitempty` representation. The new counters carry the known-versus-unknown distinction independently of conditions, which describe the observation failure separately.

The observed source needs enough identity to reject a stale result after a template edit or delete/recreate. Candidate information is reference name, UID, generation, and an opaque effective revision. UID/generation establish the observed source; the effective revision groups content with equivalent replacement semantics. Their exact representation requires API review.

The effective revision must handle metadata-only changes, secure defaults, failed-pod-policy normalization, and ordered volume-claim templates. The current raw blueprint hash includes metadata and has a semantic fallback during staleness evaluation. Hash-label compatibility and [#2075's metadata behavior](https://github.com/kubernetes-sigs/agent-sandbox/pull/2075) must be preserved. Policy-only changes that influence defaulting require an explicit decision: the raw-hash fast path can accept an unchanged blueprint while semantic comparison uses defaults influenced by template policy. The implementation must define this equivalence without changing existing replacement behavior implicitly.

`updatedReplicas` and `updatedReadyReplicas` use the same effective-revision definition. An equivalent member can count even when its raw hash label differs. Terminating members stay outside the active counters. A rotation-completion predicate must also inspect terminating stale members; subtracting the two active counters alone does not establish that stale stock has disappeared. A stock-fullness or rotation-completion observation remains a current fact and may change after another Claim.

Add a `conditions` array with the agreed condition contract. Use `metav1.Condition`, map semantics keyed by type, observed pool generation, and persistent True/False/Unknown states. Template-scoped results also require their associated source/revision. A condition's presence alone conveys no success.

Candidate condition responsibilities for discussion are:

| Responsibility | Candidate name | Contract requiring review |
| --- | --- | --- |
| A diagnosed obstruction to required replenishment | `ReplenishmentBlocked` | True for an observed obstruction such as unresolved template or post-grace unschedulability; False when evaluated without such an obstruction; Unknown when observations are unavailable. Partial obstruction can coexist with useful Ready stock. |
| Durable evidence that a member matching the effective template reached Ready | `TemplateReadyObserved` | True means evidence was committed for the identified evaluation. False describes an active or expired evaluation without a committed readiness observation. Unknown means evaluation is deferred or its evidence cannot be established. |

These names illustrate the semantic split and remain proposals. Compare `Progressing` against the same scenario table. Keep derivable inventory fullness in the existing counters unless an additional condition has a demonstrated client need. Evidence of one Ready member has a narrower meaning than sustained runtime health; readiness probes and any stabilization requirement need documentation.

The proposed condition table makes the naming review concrete. Reason names are proposals alongside the condition names.

| Condition | Situation | Status and reason |
| --- | --- | --- |
| `ReplenishmentBlocked` | A required observation failed, so obstruction cannot be determined | Unknown / `ObservationFailed` |
| `ReplenishmentBlocked` | Desired stock is zero, or healthy stock satisfies demand and no members are held past grace | False / `NoReplenishmentRequired` |
| `ReplenishmentBlocked` | A create deficit requires a template that cannot be resolved or hashed | True / `SandboxTemplateError` |
| `ReplenishmentBlocked` | Required creation or replacement was rejected by the API server | True / `ReplenishmentError`; clear after a successful reevaluation |
| `ReplenishmentBlocked` | Pool-owned members remain unschedulable or scheduling-gated past grace | True / `HoldingUnschedulable`, `HoldingSchedulingGated`, or `HoldingUnschedulableAndSchedulingGated` |
| `ReplenishmentBlocked` | Refill is proceeding or waiting for its configured pacing budget | False / `ReplenishmentPending` |
| `TemplateReadyObserved` | Source lookup failure | Unknown / `TemplateObservationFailed`; retain previous evidence and its identity |
| `TemplateReadyObserved` | Source is available and Ready evidence for the current evaluation was committed, including with no demand or zero replicas | True / `ReadyObserved` |
| `TemplateReadyObserved` | No committed Ready observation and no evaluation demand, including zero replicas | Unknown / `EvaluationDeferred` |
| `TemplateReadyObserved` | Active evaluation has no committed Ready observation | False / `AwaitingReadyObservation` |
| `TemplateReadyObserved` | An enabled evaluation deadline expired with an intact evidence protocol and no Ready observation | False / `ReadinessObservationDeadlineExceeded` |
| `TemplateReadyObserved` | No committed Ready observation and evidence could have been lost during readiness changes, transfer, deletion, or controller unavailability | Unknown / `ObservationIncomplete` |

For replenishment, evaluate observation availability first, then zero demand, required template, API errors, and post-grace holds. Report both hold types using the combined reason when present. A missing template on a full idle OnReplenish pool affects template observation; it does not create a refill obstruction without a create deficit. These rules reuse the existing event diagnostics. A scheduling gate is an intentional admission wait, and its condition message must identify that wait explicitly.

The template condition is evaluated separately. Evaluate source availability first, then committed success, then demand. With an available source, committed success takes precedence over deferred evaluation at zero replicas or absent demand. Once committed, a positive observation survives Claim consumption, controller restart, and stock depletion within the same evaluation. A source lookup failure retains the previous evidence and its identity while reporting the current evaluation as Unknown. A successful lookup of the same source and evaluation restores `ReadyObserved`. For demanded work without committed success, an incomplete evidence protocol takes precedence over waiting or expired results. This prevents a stock-health update from overwriting template evidence.

Automation first verifies the pool generation and observed template identity. It can then distinguish `ReadyObserved`, `ReadinessObservationDeadlineExceeded`, and pending/unknown reasons. `TemplateReadyObserved=False` alone includes both waiting and expired evaluations. Ready-count checks remain available for clients that require a particular stock level.

#### Durable Observation and Deadlines

A Sandbox can become Ready and be claimed between two pool reconciliations. The current Claim controller can also adopt a non-Ready member with observed Pod IPs as a fallback. `completeAdoption` removes the warm-pool and blueprint-hash labels and transfers ownership to the Claim. The pool's ordinary membership query consequently loses that member before its eventual readiness may be observable. Changes in `updatedReadyReplicas` and in-memory watch notifications provide incomplete histories.

Choose and validate a durable observation protocol before enabling an expiry result. Three alternatives expose different costs:

| Protocol | Benefit | Requirement or tradeoff |
| --- | --- | --- |
| Persisted source provenance and readiness receipt | Evidence of a published Ready observation survives later status changes and ownership transfer. | The core Sandbox controller echoes an opaque spec request token with `Ready=True` in the same status write. The WarmPool controller owns provenance and compatibility checks. Preserve the receipt until the requester retires the request, including deletion and crash recovery. |
| Observation acknowledgment before relevant Claim transfer | An observed Ready member can leave after its evidence is committed. | Adds status persistence to a Claim path and changes latency/failure behavior. It also needs an explicit treatment for pre-Ready adoption; a readiness gate would change the current fallback policy. |
| Passive pool observation | Keeps ownership transfer and Claim latency unchanged. | Publish positive observations when available; return `ObservationIncomplete` when evidence can have been missed. A reliable negative deadline result remains unavailable in that case. |

This draft favors preserving Claim behavior while designing a bounded source receipt, with passive observation as a smaller alternative. Receipt lifetime, request retirement, and deletion handling require a concrete recovery protocol before implementation. A local watch callback or a best-effort write after transfer can lose the only evidence. The source receipt uses an additive API; adoption continues removing labels that current membership indexes interpret as pool ownership.

The source-receipt option proposes an optional typed observation request directly on `SandboxSpec`, outside its embedded `SandboxBlueprint`, and its receipt in `Sandbox.status`. This placement keeps the request out of `computeSandboxBlueprintHash`, `compareSandboxBlueprint`, and `SandboxTemplateSpec`, which also embeds the blueprint. The request declares intent; the receipt records the core controller's observation. Removing or replacing the request retires it and allows receipt cleanup, so the protocol has no separate acknowledgment field. The request carries an opaque requester-supplied token; the core controller copies it verbatim and uses no WarmPool API types or evaluation semantics. Pool-specific provenance belongs to the WarmPool controller. This option changes both the core `agents.x-k8s.io/v1beta1` API and the core Sandbox controller, and requires root-approver review. Field responsibilities are:

| Persisted fact | Writer | Consumer and responsibility |
| --- | --- | --- |
| Evaluation provenance | WarmPool controller | The WarmPool controller durably binds a request token to the pool UID, evaluation identifier, effective revision, Sandbox UID, and the Sandbox generation containing the published request. It owns all validation against that binding. |
| Observation request directly on `SandboxSpec` | WarmPool controller when creating or enrolling a pool-owned Sandbox | The core Sandbox controller consumes the opaque token and records the Sandbox readiness it observes. The requester preserves unrelated spec fields. Token contents have meaning only to the requester. |
| Readiness receipt in `Sandbox.status` | Core Sandbox controller | The receipt contains the request token, Sandbox UID, observed generation, and observed Ready state and transition time. The WarmPool controller validates this evidence against its enrollment binding. The core writer retains it through later `Ready=False` or `Ready=Unknown` updates while the request remains active. |
| Accepted evidence in `SandboxWarmPool.status` | WarmPool controller | Pool status evaluation uses this durable fact even after the member is claimed or deleted. |
| Request retirement in `Sandbox.spec` | WarmPool controller, after committing accepted evidence or durably retiring its evaluation | Remove or replace only the expected request token on the expected Sandbox UID, using a resource-version precondition. After Claim adoption this is a cross-owner write to a claim-owned Sandbox. The core controller can then clear the retired request's receipt. |

New pool members carry the request in their create call. Enrollment binds the UID and initial generation returned by creation, so this path adds no second spec write or generation bump. Re-enrolling an existing member uses a conditional spec update and binds the resulting generation. Request retirement also changes spec and advances generation. Observation bookkeeping stays outside effective runtime-revision comparison and Pod replacement decisions. The core controller observes the generation containing the request and writes only Sandbox status.

After `completeAdoption`, the Claim owns the Sandbox. Request retirement and removal of any receipt-retention finalizer then write spec or metadata across controller ownership. The durable option requires an explicit agreement for these narrowly scoped writes beyond the Mutating Spec convention's allowance for managed objects. Field ownership, conflicts with Claim updates, and cleanup after pool deletion must be specified before implementation.

For a Sandbox with an observation request, the core controller includes the receipt in the same atomic status write that publishes `Ready=True` for the generation it evaluated. It echoes the request token and recomputes readiness for that generation, including when the condition was already True. An already-True condition keeps its existing transition time; the token and observed generation identify the new calculation. A later status write preserves the receipt while the same token remains active, even across True-to-False changes or subsequent spec edits. Removing or replacing the token retires that request; cleanup can discard its receipt and the core can observe a new request. Each enrollment uses a fresh token, including a return to an earlier evaluation or configuration. This keeps one outstanding receipt per Sandbox. The extension controller derives positive passive observations from current status when the receipt protocol is unavailable.

All writers preserve fields owned by the others and use resource-version preconditions for protocol updates. On a conflict, the core writer rereads the Sandbox spec and request token, retains any receipt for the active request, and recomputes readiness for the current generation before retrying. A failed atomic write commits neither its new Ready value nor its receipt. If the response is lost, the writer reads back persisted state to determine whether the pair committed. Receipt cleanup also checks current state after conflicts, so cleanup of a retired request preserves a newer request's receipt. A receipt contains evidence of a successfully published Ready observation; short Pod readiness changes between core observations have only the coverage supplied by the existing core readiness calculation.

For a new member, the WarmPool controller persists a pending token binding with the pool evaluation before creating the Sandbox with that token. It completes enrollment with the name, UID, and generation returned by creation. Recovery of an uncertain create verifies the same token, pool provenance, and compatible configuration before completing that binding. For an existing member, it verifies pool ownership, persists the binding with the Sandbox UID, and publishes the request with a resource-version precondition. Enrollment records the generation returned by that update or verified recovery. A failed or uncertain publication keeps enrollment incomplete until its binding is recovered. The binding survives restart and ownership transfer. Enrollment must cover every Ready publication included in the evaluation; an enrollment race that allows an earlier True-to-False publication leaves that evaluation incomplete unless durable evidence recovers it. After Claim transfer, discovery uses the retained binding rather than the labels and owner reference that adoption removes. The WarmPool controller rejects unknown or retired tokens and validates the receipt's Sandbox UID and observed generation against enrollment. Claim-specific runtime changes invalidate compatibility for subsequent observations. Any equivalence rule for a changed generation requires an explicit effective-configuration comparison by the WarmPool controller. Evidence already committed for the original compatible generation remains valid for that evaluation.

The WarmPool controller commits accepted evidence before retiring the spec request. If the pool write fails or its outcome is uncertain, it preserves the request and receipt while reading back the result. A crash after the pool commit but before request retirement replays the same receipt idempotently. Retirement checks the expected token, Sandbox UID, and current resource version; a delayed retirement attempt for an earlier token preserves a newer request. Explicitly abandoning an evaluation also records that decision before removing its requests. Unexpected request removal or replacement before evidence is committed creates an observation gap for an active evaluation.

Receipt retention must cover deletion, ownership transfer, and controller restart. The protocol must establish retention protection before a member can lose evidence, define who releases it after request retirement, and recover pending receipts without a namespace-wide scan. Bounded extension-owned token bindings, recovery of request publication and retirement, retention, and discovery remain required design decisions before implementing the durable option.

The resulting guarantee is durable evidence of an eligible Ready publication for the identified evaluation. A negative deadline result additionally requires complete enrollment and recovery of every relevant source through the deadline. Pending recovery delays that decision. An established gap in enrollment, retention, discovery, or writer compatibility produces `Unknown / ObservationIncomplete` when success has not been committed, and suppresses `ReadinessObservationDeadlineExceeded`. A later loss of readiness preserves committed success for the same evaluation, while current inventory reflects the latest readiness.

An alternative keeps evaluation provenance and receipts entirely in `SandboxWarmPool.status`, owned by the pool controller. This confines the API additions to the extension group. The pool can persist each positive observation it receives, but a True-to-False change published between its observations can disappear before that write. Bounded tracking after adoption and retention through deletion still require design. The pool-only option reports `ObservationIncomplete` wherever its observation protocol permits evidence loss. Its storage location alone provides no guarantee that every core Ready publication was observed.

An evaluation identifier must fence delayed evidence separately from the effective revision. In an A-to-B-to-A template sequence, the second A evaluation gets a new identifier; the first A's delayed receipt cannot satisfy it. Replay of the same valid receipt is idempotent. Reject evidence for a previous pool UID, evaluation, or incompatible Sandbox generation. Retain bounded evidence for the active evaluation instead of an unbounded list of consumed members.

| Budget | What expiry establishes | Main design cost |
| --- | --- | --- |
| Total time to full current-template inventory | The stock-convergence target was not observed within the budget. | Sustained healthy consumption can prevent that target; expiry alone provides insufficient evidence of a template fault. |
| Time without recorded progress | No accepted progress evidence was committed during the interval. | Requires a durable progress signal and reset rules; aggregate Ready deltas miss consumed members. |
| Time for an explicit current-template readiness evaluation | The selected evaluation did not record its required readiness evidence within the budget. | Requires an evaluation start boundary and durable success evidence through Claim transfer and restart. |

The recommended direction for discussion is the third model, driven by actual refill or replacement demand. An idle OnReplenish pool awaits an evaluation opportunity. Creation errors and readiness waits need coverage in the same attempt definition. A total convergence budget remains an operational-policy option with its stock-based meaning stated explicitly.

Once selected, a deadline uses an optional `*int32` field in the pool spec with a `Seconds` suffix, such as `readinessObservationDeadlineSeconds`, and `+kubebuilder:validation:Minimum=1`. Omission disables expiry; explicit zero and negative values fail validation. Persist the attempt identity and start time before deadline-dependent actions. A no-progress model additionally needs persisted progress time and evidence rules. Schedule quiet-pool expiry explicitly and preserve the clock across retry/restart. `lastTransitionTime` alone cannot identify successive template attempts that share a condition status.

The proposed lifecycle rules for an explicit readiness-evaluation budget are:

| Event | Proposed behavior |
| --- | --- |
| First refill or Recreate replacement demand for an unevaluated revision | Persist a new evaluation identifier and start time before evaluated creation or replacement. Creation rejection then belongs to the same evaluation. |
| Idle OnReplenish after a template edit | Record the new source and deferred evaluation; begin the clock at natural refill demand. |
| Metadata-only edit with equivalent effective content | Refresh source identity and preserve the evaluation and committed evidence. |
| Reference name, template UID, or effective revision changes | Begin a new evaluation when demand exists. A return to an earlier revision creates a new evaluation identifier. |
| Positive-to-positive scaling or continued Claims | Preserve an active evaluation's start time. These events change stock demand without erasing a committed observation. |
| Scale to zero | Defer an unfinished evaluation and its timer; preserve committed success. New positive demand starts a new identifier for unfinished work. |
| Strategy change | Preserve an evaluation while refill or replacement demand continues; defer unfinished work when OnReplenish removes that demand. Resumed work starts a new identifier. |
| Deadline edit | Apply the current duration to the same start time; omission disables expiry. Re-enabling does not silently restart an active evaluation. |
| Controller downtime or admission wait | Include elapsed wall time in the observation budget; expose the specific wait or evidence uncertainty. The result describes failure to observe readiness within a budget, not the cause of the delay. |
| Failed/conflicting status write | Retry from persisted state; preserve the committed evaluation and timer. |
| Expiry followed by valid current-evaluation Ready evidence | Continue reconciling and transition to `ReadyObserved`; current member health remains independently visible. |

These rules are proposed for review with the observation protocol. Receipt retention must make the distinction between `ReadinessObservationDeadlineExceeded` and `ObservationIncomplete` enforceable. Until that recovery contract is settled, the deadline is a design option and passive uncertainty is explicit.

#### Implementation Guidance

Start from [the WarmPool API](../../../extensions/api/v1beta1/sandboxwarmpool_types.go), [the WarmPool controller](../../../extensions/controllers/sandboxwarmpool_controller.go), and [Claim adoption](../../../extensions/controllers/sandboxclaim_controller.go). The source-receipt option also requires [the core Sandbox API](../../../api/v1beta1/sandbox_types.go) and [the core controller's Ready calculation and status write](../../../controllers/sandbox_controller.go). Reuse membership/ownership filtering, readiness-grace logic, template comparison, and error evidence. The current in-memory `notProgressing` map controls event emission; durable evaluation history belongs in persisted state.

Use one defined owner for each condition and a coordinated pool status update. Recompute from a fresh pool object after conflicts; preserve unrelated conditions. Commit template source observation consistently with its associated outcome. Controller memory is an optimization over persisted facts.

Coordinate revision and inventory fields with #426 and metadata semantics with #2075. [Warmup concurrency #2104](https://github.com/kubernetes-sigs/agent-sandbox/pull/2104) also affects wait duration and the definition of outstanding warmup. Audit callers of shared lifecycle predicates so exposing status preserves adoption, deletion, and refill capacity behavior.

### Compatibility and Delivery

The first implementation can add current-template inventory and the agreed replenishment diagnosis under this common design. Durable template results follow once the evidence protocol is specified and its crash boundaries are tested. A KEP approval must state which parts are implementable; `provisional` records the current review status.

All proposed API additions are optional. Existing replacement, adoption, and stock-count behavior stays stable. New members receive observation requests at creation; updates and retirement advance Sandbox generation without changing the effective runtime configuration. Older controllers may preserve unknown fields without implementing the protocol; clients require a current observation and treat missing or unverified results as Unknown. Source-receipt enrollment must establish that the schema preserves the spec request and status receipt, and that the core writer supports atomic Ready/receipt writes. A legacy writer, interrupted upgrade, or client that drops the spec request can leave an evidence gap. Before rollout, define how such gaps, a downgrade, or an unsupported core writer invalidate active evaluation completeness and yield `ObservationIncomplete`. Optional deadlines remain disabled by default. Any accepted Claim acknowledgment gate or deletion-retention mechanism requires an explicit behavior-change review and migration guidance.

### Validation

Implementation acceptance requirements are:

| Layer | Requirement |
| --- | --- |
| Semantic table tests | Every scenario above has a result for stock, current health, template evidence, and timer eligibility. Include committed success at zero replicas, source lookup failure, and restoration of the same source. |
| API representation | Round-trip omitted and explicit-zero updated counters, the optional spec request, and its status receipt. Unavailable observations stay unknown. Reject negative counts and nonpositive deadline seconds; omission disables the deadline. |
| Real apiserver | Template-only edits preserve pool generation; status identifies the external source; metadata-equivalent changes retain the effective revision. New members carry the request at creation. Re-enrollment and retirement advance Sandbox generation without replacing Pods or changing blueprint hashes or comparisons. Verify that `SandboxTemplateSpec` has no observation request field. Status writes preserve spec intent and leave generation unchanged. |
| Conflict and crash boundaries | Reject/conflict status writes at attempt start and success; restart around those boundaries; verify durable identity, clock, and outcome. |
| Atomic source receipt | Publish `Ready=True` and its receipt in one core status write, then publish False before any extension observation. The receipt survives and is accepted once. Enroll an already-Ready member and require a fresh readiness calculation for the generation containing the request. Inject rejection, conflict, lost response, and a core restart around the atomic write; preserve other writers' fields and verify persisted state. |
| Core and extension separation | Supply arbitrary spec request tokens without WarmPool resources or API types. The core writer echoes each token with the observed Sandbox UID, generation, and Ready observation. The WarmPool controller rejects unknown, retired, or mismatched bindings. The core writer never modifies Sandbox spec. |
| Request publication and retirement | Create a member with its request in one call and verify that enrollment adds no second spec write. Fail or lose create and re-enrollment responses and recover the UID/generation binding before accepting evidence. Preserve the receipt while its token stays active. Remove or replace the token only after durable acceptance or evaluation retirement. Race delayed retirement and receipt cleanup with a newer request; preserve its token and receipt. Premature request removal produces `ObservationIncomplete` for an active evaluation without committed success. |
| Readiness coverage | Exercise a True-to-False publication before enrollment and a Pod readiness change between core observations. Attribute only the observations the selected protocol covers; an enrollment gap yields `ObservationIncomplete`. Current inventory follows later readiness while committed success persists. |
| Evidence attribution | Race Claim-specific configuration changes with the core Ready write. Reject incompatible generation/revision evidence and receipts from an old pool UID or A-to-B-to-A evaluation; preserve previously committed compatible success. |
| Receipt consumption | Fail the pool evidence write, lose its response, and restart before request retirement. Retain the spec request and source receipt, read back the pool result, and replay acceptance and retirement idempotently. |
| Retention and deadline recovery | Transfer or delete a source before consumption and restart each controller. Recover retained receipts before a negative deadline decision; a proven evidence gap yields `ObservationIncomplete`. Verify cleanup after request or evaluation retirement, including scale-to-zero and pool deletion. Exercise the agreed cross-owner request/finalizer writes on claim-owned Sandboxes and preserve unrelated Claim fields during conflicts. |
| Claim traffic | Exercise Ready-to-claim transfer between observations, pre-Ready adoption, receipt deletion, restart, and delayed signals through A-to-B-to-A revisions. Stock deficit alone must not produce a template-failure diagnosis. |
| Strategy and demand | Idle OnReplenish, first refill, Recreate, zero replicas, positive scaling, and strategy switches follow the selected contract. |
| Actual readiness | Kind exercises a good template, failing container/readiness probe, scheduling pressure, and recovery with real ownership transfer. |
| Capacity and cleanup | Terminating stale members and failed deletes remain visible to the relevant inventory/rotation predicates. |
| Compatibility | Schema validation preserves the spec request and status receipt; existing clients and hash labels retain their contracts. Exercise a legacy core writer and downgrade that retain fields but omit atomic receipts, and a client that drops a request; mark affected evaluations incomplete. Document condition lifecycle and recovery after upgrade. |

### Design Review Decisions

The requested review decisions are the two proposed condition responsibilities and names; revision equivalence under policy/defaulting changes; and the cost/guarantee tradeoff of Sandbox receipts, pool-only receipts, and passive observation. Selecting source receipts includes a generic request directly on `SandboxSpec` and a status receipt, generation-aware enrollment, guarded request retirement, extension-owned token bindings, and bounded retention/discovery through adoption and deletion. It also requires agreement on cross-owner spec/metadata writes after Claim adoption. Review needs to establish how the pool proves complete evidence recovery at a deadline and detects unsupported writers. These decisions determine which negative deadline result can be implemented reliably. The lifecycle table provides concrete rules to review once that choice is made.

## Scalability

Compute inventory in the existing reconciliation pass and reuse per-pass template comparisons. Keep persisted state bounded per pool, retaining the active evaluation and current result instead of an expanding member history. Write status only when an exposed fact changes and schedule the next relevant deadline boundary.

Measure additional writes per Claim and per replenished member before selecting the observation protocol, including spec writes and reconciliations caused by request publication and retirement. High-churn pools must avoid a linear history of consumed UIDs or a namespace-wide scan of past members. New metrics use bounded labels. Validation must measure write amplification for the selected protocol.

## Alternatives

Using only `observedGeneration` leaves external template changes untracked. A simple `Available` condition based on full stock conflates a target with the ability to provide a warm member; #1328 documents the project's decision.

A generic `Progressing` condition requires a definition for an idle pool, a busy replenishing pool, and lazy rotation. Its suitability is part of this KEP's review. Copying a workload rollout timer without that definition allows normal Claim traffic to change the apparent outcome.

Publishing inventory fields and durable health first can reduce implementation size. Incremental delivery should follow the combined KEP's agreed semantics so later template outcomes compose with the same model.

## References

- [Slack design discussion](https://kubernetes.slack.com/archives/C09QC8C676E/p1786602888722509): Janet requested one KEP covering both issues' condition semantics and naming.
- [Janet's architectural review on #1328](https://github.com/kubernetes-sigs/agent-sandbox/pull/1328#discussion_r3787632556).
- [#188 closing clarification](https://github.com/kubernetes-sigs/agent-sandbox/issues/188#issuecomment-5299659513).
- [#426 RollingUpdate proposal](https://github.com/kubernetes-sigs/agent-sandbox/issues/426#issuecomment-6436524333).
- [#2075 in-place metadata update](https://github.com/kubernetes-sigs/agent-sandbox/pull/2075).
