// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const (
	sandboxTemplateRefHash          = sandboxv1beta1.SandboxTemplateRefHashLabel
	warmPoolSandboxLabel            = sandboxv1beta1.SandboxWarmPoolLabel
	sandboxCreateDeleteMaxBatchSize = 300
	autoscalerSafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	// sandboxWarmPoolLabelIndex is the cache field index over the warmPoolSandboxLabel
	// value on warm sandboxes, so reconcilePool's member lookup is O(pool members) instead
	// of O(sandboxes-in-namespace).
	sandboxWarmPoolLabelIndex = ".metadata.labels[" + warmPoolSandboxLabel + "]"

	// DefaultWarmPoolReadinessGracePeriod is how long a pool sandbox may stay
	// non-Ready before the reconciler considers it stuck and replaces it,
	// unless overridden via SandboxWarmPoolReconciler.ReadinessGracePeriod.
	DefaultWarmPoolReadinessGracePeriod = 5 * time.Minute

	// expectationsPendingRequeueDelay is the fallback requeue used when create
	// or delete work is skipped because previously issued writes have not been
	// observed by the informer cache yet. Watch events normally retrigger the
	// pool much sooner; this only guards against lost events.
	expectationsPendingRequeueDelay = 30 * time.Second

	// DefaultUnschedulableRecheckInterval is the rate-limited retry interval
	// for a pool holding unschedulable sandboxes instead of churning
	// delete/create (#1215), unless overridden via
	// SandboxWarmPoolReconciler.UnschedulableRecheckInterval.
	DefaultUnschedulableRecheckInterval = time.Minute

	// graceRequeueSlack pads the self-scheduled post-grace requeue so the
	// re-evaluation lands strictly after the deadline despite clock jitter.
	graceRequeueSlack = 2 * time.Second
)

// graceRequeueJitterFactor spreads the self-scheduled post-grace requeues of
// a fleet warmed together (whose grace deadlines therefore cluster) across a
// wait.Jitter window of up to +50% of the remaining grace, instead of letting
// every pool re-reconcile inside the same ~2s slack window (thundering herd
// of unschedulable-pod checks + status updates at 500-18K pools). No single
// pool's post-grace evaluation is delayed more than 1.5x its remaining grace.
// Package variable so deterministic fake-clock tests can zero it.
var graceRequeueJitterFactor = 0.5

const (

	// Event reasons surfaced on the SandboxWarmPool when the pool cannot make
	// progress toward spec.replicas (and when progress resumes).
	reasonWarmPoolNotProgressing = "WarmPoolNotProgressing"
	reasonWarmPoolProgressing    = "WarmPoolProgressing"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object.
type SandboxWarmPoolReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	MaxBatchSize           int
	EnableWarmPoolEviction bool
	// ReadinessGracePeriod is how long a pool sandbox may stay non-Ready
	// before it is considered stuck (delete-and-replace, or held if its pod
	// is unschedulable). Zero means DefaultWarmPoolReadinessGracePeriod.
	ReadinessGracePeriod time.Duration
	// UnschedulableRecheckInterval is the requeue interval while a pool holds
	// unschedulable sandboxes past the readiness grace period. Zero means
	// DefaultUnschedulableRecheckInterval.
	UnschedulableRecheckInterval time.Duration
	// Recorder emits pool-level Events (e.g. WarmPoolNotProgressing). May be
	// nil (tests); all uses are nil-guarded.
	Recorder events.EventRecorder

	// expectations tracks in-flight sandbox creations/deletions per pool so a
	// reconcile never re-creates toward the target off a cache that has not
	// observed its own previous writes (#1215). Access via exp().
	expectations *warmPoolExpectations
	expOnce      sync.Once

	// notProgressingMu guards notProgressing, the set of pools currently held
	// in a not-progressing state (used to emit transition events exactly once).
	notProgressingMu sync.Mutex
	notProgressing   map[types.NamespacedName]struct{}

	// now is a test hook for the reconciler's clock; nil means time.Now.
	now func() time.Time

	// ReplenishDelay defers creation of replacement sandboxes after pool
	// members drop out of the pool (e.g. a burst of SandboxClaims adopting
	// warm sandboxes). Deferring lets the claim burst consume the API server
	// budget first instead of racing it with replacement creates. Zero (the
	// default) disables deferral and preserves the immediate-refill behavior.
	//
	// Caveat for SUSTAINED arrivals (measured in a 300-claim warm-adoption
	// benchmark): the hold re-arms on every observed member drop, so while
	// claims keep arriving the hold re-arms indefinitely, refill never
	// starts, and the pool drains to zero. For sustained load prefer
	// MaxRefillRate — a paced refill stream that coexists with adoption —
	// combined with a small or zero delay.
	ReplenishDelay time.Duration

	// MaxRefillRate, when > 0, caps the rate (sandbox creates per second,
	// PER POOL) at which replacement sandboxes are created, via a per-pool
	// token bucket. It turns deficit-burst refill (one reconcile firing the
	// whole deficit through slowStartBatch) into a smooth stream, so at
	// sustained claim rates refill does not periodically flood the write
	// path and compete with claim adoption. Zero (the default) leaves refill
	// unshaped — the full-deficit slowStartBatch behavior.
	//
	// Semantics vs ReplenishDelay: the delay defers the START of refill;
	// the rate shapes its FLOW once started. The bucket holds at most one
	// second of creates (capacity = max(1, rate)), so refill resumes from a
	// hold or an idle period with at most a 1×rate initial burst.
	//
	// Semantics vs the expectations gate: shaping decides how many creates
	// this pass may ISSUE; the gate then decides whether issuing anything is
	// safe against the cache. Tokens taken for a pass the gate refuses are
	// refunded (no write was issued, so no API budget was spent).
	//
	// Sizing guidance (measured in a 300-claim warm-adoption benchmark:
	// 300-deficit fill through a single reconcile's slowStartBatch):
	//   - sandbox CREATE stage: ~85/s with API Priority and Fairness
	//     queueing creates (~50ms mean queue wait), ~240/s burst without
	//     APF shaping;
	//   - pod scheduling: ~70/s (kube-scheduler default --kube-api-qps=50);
	//   - pod start (cached image): ~2.5-3.5s, fully pipelined;
	//   - net: a 300-member pool went 0 -> 300 Ready in ~6.4s.
	// One pool's deficit is processed serially under its single reconcile
	// key, so per-pool refill throughput tops out at
	//   min(create ~85/s, scheduler share, MaxRefillRate).
	// For a sustained claim arrival rate R/s aggregate refill must be >= R:
	//   pools needed  >= ceil(R / per_pool_rate)
	//   pool replicas >= R × (refill_p99 + replenish hold)   (shock absorber)
	// e.g. 500 claims/s at ~70/s per pool => >= 8 pools of ~1-2k replicas.
	// Run --sandbox-warm-pool-concurrent-workers >= pool count so distinct
	// pools refill in parallel (parallelism across pools is free; a per-pool
	// create-parallelism knob is NOT needed — slowStartBatch already reaches
	// 128+-way parallelism inside a batch and the measured limiter is write
	// RTT and the scheduler, not batch width).
	MaxRefillRate float64

	// replenishMu guards replenishState and refillState. Distinct pools may
	// reconcile concurrently when MaxConcurrentReconciles > 1.
	replenishMu sync.Mutex
	// replenishState tracks, per pool, the last observed member count and any
	// active replenish hold. Only used when ReplenishDelay > 0.
	replenishState map[types.NamespacedName]*replenishDeferState
	// refillState tracks, per pool, the token bucket that paces replacement
	// creates. Only used when MaxRefillRate > 0.
	refillState map[types.NamespacedName]*refillBucket
}

// refillBucket is the per-pool token bucket behind MaxRefillRate. Tokens
// accrue at MaxRefillRate per second up to a capacity of max(1, rate) — one
// second of creates — so a long-idle pool cannot bank a large burst.
type refillBucket struct {
	// tokens currently available; one token = one replacement create.
	tokens float64
	// last is when tokens were last accrued.
	last time.Time
}

// replenishDeferState is the per-pool bookkeeping behind ReplenishDelay.
type replenishDeferState struct {
	// lastMembers is the active member count at the previous observation,
	// plus any replacements created in that reconcile that may not be visible
	// in the informer cache yet. A subsequent observation below this value
	// means members were consumed (adopted/claimed/GC'd/deleted), not that
	// our own creates are still propagating.
	lastMembers int32
	// deferUntil suppresses replacement creation while in the future. It is
	// re-armed on every observed drop, so replenishment starts only after the
	// burst that is draining the pool has settled for a full ReplenishDelay.
	deferUntil time.Time
}

// observeMembersForReplenish records the pool's current active member count
// and returns how long replacement creation should be deferred (zero means
// create immediately). It must be called exactly once per reconcile so the
// baseline stays fresh.
func (r *SandboxWarmPoolReconciler) observeMembersForReplenish(key types.NamespacedName, currentReplicas, desiredReplicas int32, now time.Time) time.Duration {
	if r.ReplenishDelay <= 0 {
		return 0
	}

	r.replenishMu.Lock()
	defer r.replenishMu.Unlock()

	st, ok := r.replenishState[key]
	if !ok {
		// First observation of this pool (new pool or controller restart):
		// there is no baseline to detect a drop against, so replenish
		// immediately. Initial pool fill and scale-ups are never deferred.
		if r.replenishState == nil {
			r.replenishState = make(map[types.NamespacedName]*replenishDeferState)
		}
		r.replenishState[key] = &replenishDeferState{lastMembers: currentReplicas}
		return 0
	}

	dropped := currentReplicas < st.lastMembers
	st.lastMembers = currentReplicas

	if currentReplicas >= desiredReplicas {
		// Pool is full (or over-provisioned): nothing to defer.
		st.deferUntil = time.Time{}
		return 0
	}
	if dropped {
		// Re-arm on every drop: while an adoption burst is still draining the
		// pool, keep replacement creates out of its window.
		st.deferUntil = now.Add(r.ReplenishDelay)
	}
	if remaining := st.deferUntil.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

// noteReplenishCreates raises the pool's member baseline by the number of
// replacements just created. Until the informer cache catches up, subsequent
// reconciles may not see these creates; counting them in the baseline keeps
// drop detection accurate — a stale low count registers as a drop (deferring
// briefly) instead of re-arming nothing. (Duplicate creates off the stale
// count are separately prevented by the expectations gate.)
func (r *SandboxWarmPoolReconciler) noteReplenishCreates(key types.NamespacedName, created int32) {
	if r.ReplenishDelay <= 0 || created <= 0 {
		return
	}
	r.replenishMu.Lock()
	defer r.replenishMu.Unlock()
	if st, ok := r.replenishState[key]; ok {
		st.lastMembers += created
	}
}

// forgetReplenishState drops the per-pool replenish and refill bookkeeping
// for a deleted pool.
func (r *SandboxWarmPoolReconciler) forgetReplenishState(key types.NamespacedName) {
	r.replenishMu.Lock()
	defer r.replenishMu.Unlock()
	delete(r.replenishState, key)
	delete(r.refillState, key)
}

// takeRefillTokens grants up to want replacement creates from the pool's
// token bucket and returns how many were granted plus, when the grant fell
// short, how long until the next whole token accrues (the requeue interval
// that keeps the paced stream flowing without relying on watch events).
//
// Tokens are consumed for every granted create up front; failed creates are
// deliberately NOT refunded — a failed POST spends the same API-server budget
// the rate exists to protect, and the controller's error backoff already
// paces retries. (Creates the expectations gate refuses to issue are the one
// exception: no POST happens, so the caller refunds via refundRefillTokens.)
// When MaxRefillRate is zero the bucket is bypassed entirely and behavior is
// byte-identical to the unshaped path.
func (r *SandboxWarmPoolReconciler) takeRefillTokens(key types.NamespacedName, want int32, now time.Time) (int32, time.Duration) {
	if r.MaxRefillRate <= 0 || want <= 0 {
		return want, 0
	}
	capacity := math.Max(1, r.MaxRefillRate)

	r.replenishMu.Lock()
	defer r.replenishMu.Unlock()

	b, ok := r.refillState[key]
	if !ok {
		if r.refillState == nil {
			r.refillState = make(map[types.NamespacedName]*refillBucket)
		}
		// First observation of this pool (new pool or controller restart):
		// start with a full bucket so small deficits are served immediately;
		// anything beyond one second's worth is paced from the start.
		b = &refillBucket{tokens: capacity, last: now}
		r.refillState[key] = b
	} else if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(capacity, b.tokens+r.MaxRefillRate*elapsed.Seconds())
		b.last = now
	}

	// Only narrow the float64 token count to int32 when the bucket holds
	// fewer tokens than the request: then 0 <= b.tokens < float64(want) <=
	// MaxInt32 and the conversion is exact. Converting unconditionally is
	// implementation-defined for capacities above MaxInt32 (a huge-but-finite
	// MaxRefillRate passes the flag validation, which only rejects
	// NaN/Inf/negative): amd64 wraps to MinInt32, turning the grant negative
	// — no creates issued AND no pacing requeue armed, wedging the pool
	// (arm64 happens to saturate to MaxInt32). Guarding here (rather than
	// clamping the flag) keeps the behavior defined on every platform and
	// also covers programmatically constructed reconcilers.
	granted := want
	if b.tokens < float64(want) {
		granted = int32(b.tokens)
	}
	b.tokens -= float64(granted)
	if granted >= want {
		return granted, 0
	}
	// Ceil so the requeue never lands a hair before the token exists.
	wait := time.Duration(math.Ceil((1 - b.tokens) / r.MaxRefillRate * float64(time.Second)))
	return granted, wait
}

// refundRefillTokens returns tokens that were taken for creates which were
// never issued (the expectations gate refused the pass). No POST happened, so
// none of the API-server budget the rate protects was spent — unlike failed
// creates, which are deliberately not refunded.
func (r *SandboxWarmPoolReconciler) refundRefillTokens(key types.NamespacedName, n int32) {
	if r.MaxRefillRate <= 0 || n <= 0 {
		return
	}
	r.replenishMu.Lock()
	defer r.replenishMu.Unlock()
	if b, ok := r.refillState[key]; ok {
		b.tokens = math.Min(math.Max(1, r.MaxRefillRate), b.tokens+float64(n))
	}
}

// clockNow returns the reconciler's current time (time.Now unless a test
// injected a fake clock).
func (r *SandboxWarmPoolReconciler) clockNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// readinessGracePeriod returns the configured readiness grace period, falling
// back to the default for zero-value construction (tests, bare literals).
func (r *SandboxWarmPoolReconciler) readinessGracePeriod() time.Duration {
	if r.ReadinessGracePeriod > 0 {
		return r.ReadinessGracePeriod
	}
	return DefaultWarmPoolReadinessGracePeriod
}

// unschedulableRecheckInterval returns the configured unschedulable-hold
// requeue interval, falling back to the default for zero-value construction.
func (r *SandboxWarmPoolReconciler) unschedulableRecheckInterval() time.Duration {
	if r.UnschedulableRecheckInterval > 0 {
		return r.UnschedulableRecheckInterval
	}
	return DefaultUnschedulableRecheckInterval
}

// exp returns the reconciler's expectations tracker.
//
// In production the tracker is initialized before any reconcile runs:
// SetupWithManager calls exp() while building the sandbox watch handler, and
// the manager only starts workers after setup. The sync.Once exists for
// zero-value construction (tests build SandboxWarmPoolReconciler literals and
// some drive reconcilePool from multiple goroutines): a bare
// `if r.expectations == nil` lazy-init would be an unsynchronized read/write
// of the field — a data race with no happens-before edge publishing the
// tracker's contents — whereas Once gives mutual exclusion plus safe
// publication, at the cost of one atomic load per call after init.
func (r *SandboxWarmPoolReconciler) exp() *warmPoolExpectations {
	r.expOnce.Do(func() {
		if r.expectations == nil {
			r.expectations = newWarmPoolExpectations()
		}
	})
	return r.expectations
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/finalizers,verbs=get;update;patch
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile implements the reconciliation loop for SandboxWarmPool.
func (r *SandboxWarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the SandboxWarmPool instance
	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	if err := r.Get(ctx, req.NamespacedName, warmPool); err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Info("SandboxWarmPool resource not found. Ignoring since object must be deleted")
			r.forgetPool(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get SandboxWarmPool")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !warmPool.DeletionTimestamp.IsZero() {
		logger.Info("SandboxWarmPool is being deleted")
		r.forgetPool(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Save old status for comparison
	oldStatus := warmPool.Status.DeepCopy()

	// Reconcile the pool (create or delete Sandboxes as needed)
	requeueAfter, err := r.reconcilePool(ctx, warmPool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update status if it has changed
	if err := r.updateStatus(ctx, oldStatus, warmPool); err != nil {
		logger.Error(err, "Failed to update SandboxWarmPool status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// forgetPool drops per-pool bookkeeping (expectations, not-progressing state,
// replenish/refill shaping state) once a pool is gone or terminating.
func (r *SandboxWarmPoolReconciler) forgetPool(key types.NamespacedName) {
	r.exp().Forget(key)
	r.notProgressingMu.Lock()
	delete(r.notProgressing, key)
	r.notProgressingMu.Unlock()
	r.forgetReplenishState(key)
}

// reconcilePool ensures the correct number of pre-allocated sandboxes exist in the pool.
// It returns an optional requeue delay (used when work was deliberately held
// back, e.g. unsatisfied expectations or unschedulable sandboxes) and any
// errors encountered.
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) (time.Duration, error) {
	logger := log.FromContext(ctx)

	poolKey := types.NamespacedName{Namespace: warmPool.Namespace, Name: warmPool.Name}
	var requeueAfter time.Duration

	// Compute hash of the warm pool name for the pool label
	poolNameHash := sandboxcontrollers.NameHash(warmPool.Name)

	// List all Sandbox CRs with the warm pool label
	sandboxList := &sandboxv1beta1.SandboxList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		warmPoolSandboxLabel: poolNameHash,
	})

	if err := r.List(ctx, sandboxList,
		client.InNamespace(warmPool.Namespace),
		client.MatchingFields{sandboxWarmPoolLabelIndex: poolNameHash},
	); err != nil {
		logger.Error(err, "Failed to list sandboxes")
		return 0, err
	}

	// Fetch template and compute hash once to avoid repeated expensive operations,
	// only currentSandboxBlueprintHash is used for staleness checks,
	// currentPodTemplateHash is kept as a value for DeprecatedSandboxPodTemplateHashLabel
	// for external consumer compatibility
	template, currentPodTemplateHash, currentSandboxBlueprintHash, tmplErr := r.fetchTemplateAndHash(ctx, warmPool)

	// Delete stale pods, filter pods by ownership and adopt orphans.
	// terminatingReplicas counts pool-owned sandboxes that are deleting (or
	// were deleted by us but not yet observed by the cache): they are not
	// active, but they still occupy capacity until fully gone.
	activeSandboxes, terminatingReplicas, allErrors := r.filterActiveSandboxes(ctx, poolKey, warmPool, sandboxList.Items, template, currentSandboxBlueprintHash, tmplErr)

	now := r.clockNow()
	var healthySandboxes []sandboxv1beta1.Sandbox
	unschedulableReplicas := int32(0)
	// nextGraceDeadline is the time remaining until the earliest readiness
	// grace deadline among not-yet-Ready sandboxes (0 = none pending).
	var nextGraceDeadline time.Duration
	for _, sb := range activeSandboxes {
		if !isSandboxReady(&sb) && !sb.CreationTimestamp.IsZero() {
			age := now.Sub(sb.CreationTimestamp.Time)
			if age <= r.readinessGracePeriod() {
				// Not Ready but still within the grace period. In a quiet
				// cluster nothing else touches the Sandbox objects of a pool
				// that settles at Ready=False (pod FailedScheduling events do
				// not), so without a self-scheduled requeue the post-grace
				// evaluation would only ever run on ambient traffic or the
				// ~10h resync — leaving both the stuck-sandbox GC and the
				// unschedulable-hold/NotProgressing signal unreachable.
				// Requeue for the earliest grace deadline so the evaluation
				// is deterministic.
				if remaining := r.readinessGracePeriod() - age + graceRequeueSlack; nextGraceDeadline == 0 || remaining < nextGraceDeadline {
					nextGraceDeadline = remaining
				}
				healthySandboxes = append(healthySandboxes, sb)
				continue
			}
			// Deleting an unschedulable sandbox only produces an equally
			// unschedulable replacement: under a capacity shortfall the old
			// delete-and-replace behavior becomes an unbounded delete->create
			// loop (#1215). Hold the sandbox and retry on a rate-limited
			// requeue instead; the scheduler will place it when capacity
			// frees up.
			if r.isSandboxPodUnschedulable(ctx, &sb) {
				unschedulableReplicas++
				healthySandboxes = append(healthySandboxes, sb)
				continue
			}
			logger.Info("Deleting stuck warm pool sandbox",
				"sandbox", sb.Name,
				"age", age.Round(time.Second))
			r.exp().ExpectDeletion(poolKey, sb.UID)
			if err := r.Delete(ctx, &sb); err != nil {
				r.exp().DeletionObserved(poolKey, sb.UID)
				logger.Error(err, "Failed to delete stuck sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				// The sandbox still exists; keep counting it as active so the
				// create path cannot overshoot spec.replicas.
				healthySandboxes = append(healthySandboxes, sb)
				continue
			}
			// Successfully deleted: it now occupies capacity as terminating
			// until the deletion is observed; the replacement is created on a
			// later reconcile once it no longer counts against the target.
			terminatingReplicas++
			continue
		}
		healthySandboxes = append(healthySandboxes, sb)
	}
	activeSandboxes = healthySandboxes

	desiredReplicas := int32(1)
	if warmPool.Spec.Replicas != nil {
		desiredReplicas = *warmPool.Spec.Replicas
	}
	currentReplicas := int32(len(activeSandboxes))
	// totalReplicas is the pool's whole live population: active plus
	// terminating-but-still-present. Creates are gated on this so the
	// population can never balloon past spec.replicas while deletes lag (#1215).
	totalReplicas := currentReplicas + terminatingReplicas

	logger.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
		"terminating", terminatingReplicas,
		"unschedulable", unschedulableReplicas,
		"poolName", warmPool.Name,
		"poolNameHash", poolNameHash)

	warmPool.Status.Replicas = currentReplicas
	warmPool.Status.Selector = labelSelector.String()

	// Calculate ready replicas by checking Sandbox Ready condition
	readyReplicas := int32(0)
	for i := range activeSandboxes {
		if isSandboxReady(&activeSandboxes[i]) {
			readyReplicas++
		}
	}
	warmPool.Status.ReadyReplicas = readyReplicas

	// Surface the pool's observed generation so clients can gate on
	// status.observedGeneration == metadata.generation before trusting the
	// replica counts (eliminates read-after-write races on scale/spec updates).
	warmPool.Status.ObservedGeneration = warmPool.Generation

	maxBatchSize := int32(r.MaxBatchSize)

	// Record the observed member count and check whether replacement creation
	// should yield to a recent member drop (claim adoption burst). No-op when
	// ReplenishDelay is zero.
	replenishHold := r.observeMembersForReplenish(poolKey, currentReplicas, desiredReplicas, now)

	// Create new sandboxes if we need more.
	// Hard invariant: never create while the existing population (active,
	// including non-Ready, plus terminating-still-present) already covers
	// spec.replicas.
	//
	// The hold branch is keyed on the ACTIVE deficit (currentReplicas), not
	// the population deficit (totalReplicas): a drop that is still draining
	// as terminating members must keep the hold's wake-up armed, so that once
	// the deletions are observed the remaining hold window (not a watch-event
	// race) decides when refill starts.
	if replenishHold > 0 && currentReplicas < desiredReplicas && tmplErr == nil {
		// Members recently dropped out of the pool (e.g. adopted by a burst
		// of claims). Defer replacement creation so the burst gets the API
		// server budget first; status above still reflects actual counts.
		// The refill token bucket is untouched during the hold (its
		// capacity caps carryover at one second of creates), so when the
		// hold expires the paced stream starts fresh: delay defers the
		// START of refill, MaxRefillRate shapes its FLOW.
		// V(4): fires on every reconcile while the hold re-arms (one per
		// adoption during a claim burst) — routine pacing, not a
		// lifecycle event.
		logger.V(4).Info("Deferring pool replenishment after recent member drop",
			"deficit", desiredReplicas-currentReplicas,
			"requeueAfter", replenishHold)
		requeueAfter = minNonZeroDuration(requeueAfter, replenishHold)
	} else if totalReplicas < desiredReplicas && tmplErr == nil {
		deficit := min(desiredReplicas-totalReplicas, maxBatchSize)
		// Shape first, then gate: the token bucket decides how many creates
		// this pass may issue, and the expectations gate below decides
		// whether issuing them against the current cache is safe.
		sandboxesToCreate, tokenWait := r.takeRefillTokens(poolKey, deficit, now)
		if sandboxesToCreate < deficit {
			// V(4): fires on every paced pass (a 300-deficit refill is
			// ~rate*seconds of them) — routine pacing, not a lifecycle
			// event.
			logger.V(4).Info("Pacing pool replenishment",
				"deficit", deficit,
				"granted", sandboxesToCreate,
				"tokenWait", tokenWait)
			if sandboxesToCreate == 0 {
				// Token bucket empty: nothing will be created this pass,
				// so no Sandbox watch event will trigger the next
				// reconcile — requeue for when the next token accrues.
				requeueAfter = minNonZeroDuration(requeueAfter, tokenWait)
			}
			// When a partial batch IS granted, rely on the Owns(&Sandbox)
			// watch instead: each create's informer event first lowers the
			// creation expectation and then schedules a reconcile, so the
			// next pass runs against a cache that already contains the new
			// Sandbox. Requeueing on tokenWait as well would race that
			// delivery and burn the pass on an unsatisfied expectations
			// gate (a stale currentReplicas can no longer duplicate
			// creates — the gate blocks that — but the futile wakeup and
			// its 30s fallback would stall the paced stream).
		}
		if sandboxesToCreate > 0 {
			sandboxCR, err := r.buildSandboxCR(warmPool, poolNameHash, template, currentPodTemplateHash, currentSandboxBlueprintHash)
			switch {
			case err != nil:
				logger.Error(err, "Failed to build sandbox CR blueprint")
				allErrors = errors.Join(allErrors, err)
			// TryExpectCreations atomically checks that every create and delete
			// this controller previously issued for the pool has been observed by
			// the informer cache, and records the new in-flight creates. If prior
			// writes are still unobserved the cached list above is stale and
			// creating against it would overshoot the target (the #1215 runaway),
			// so we skip and let the watch (or a fallback requeue) retrigger us.
			// The granted tokens are refunded: no POST was issued for them.
			case !r.exp().TryExpectCreations(poolKey, int(sandboxesToCreate)):
				logger.Info("Skipping sandbox creation: waiting for in-flight creates/deletes to be observed",
					"poolName", warmPool.Name)
				r.refundRefillTokens(poolKey, sandboxesToCreate)
				requeueAfter = minNonZeroDuration(requeueAfter, expectationsPendingRequeueDelay)
			default:
				logger.Info("Creating new pool sandboxes", "count", sandboxesToCreate)
				// Parallel sandbox creation with adaptive slow-start batching (starts with 1 and doubles on success)
				successes, createErr := slowStartBatch(ctx, int(sandboxesToCreate), 1, func(_ int) error {
					return r.createPoolSandbox(ctx, warmPool, sandboxCR)
				})
				// Creates that never happened will never produce a watch event;
				// lower their expectations immediately so the pool is not blocked
				// until the expectations timeout. lower cannot be negative
				// (slowStartBatch reports at most the requested count of
				// successes); the > 0 guard only skips a no-op tracker call on
				// the everything-succeeded path.
				if lower := int(sandboxesToCreate) - successes; lower > 0 {
					r.exp().LowerCreations(poolKey, lower)
				}
				r.noteReplenishCreates(poolKey, int32(successes))
				if createErr != nil {
					logger.Error(createErr, "Failed to create pool sandboxes")
					allErrors = errors.Join(allErrors, createErr)
				}
			}
		}
	}

	// Delete excess sandboxes if we have too many. Like creates, excess
	// deletes are computed from the cached list, so they are skipped while
	// expectations are unsatisfied: a stale list could otherwise show a
	// phantom surplus and delete healthy sandboxes.
	if currentReplicas > desiredReplicas {
		if !r.exp().SatisfiedExpectations(poolKey) {
			logger.Info("Skipping excess sandbox deletion: waiting for in-flight creates/deletes to be observed",
				"poolName", warmPool.Name)
			requeueAfter = minNonZeroDuration(requeueAfter, expectationsPendingRequeueDelay)
		} else {
			sandboxesToDelete := min(currentReplicas-desiredReplicas, maxBatchSize)
			logger.Info("Deleting excess sandboxes", "count", sandboxesToDelete)

			// Prioritize deleting unready sandboxes before ready ones,
			// then newest first within each group.
			slices.SortFunc(activeSandboxes, func(a, b sandboxv1beta1.Sandbox) int {
				aReady := isSandboxReady(&a)
				bReady := isSandboxReady(&b)
				if aReady != bReady {
					if aReady {
						return 1 // a ready, b not ready -> b first (delete unready first)
					}
					return -1 // b ready, a not ready -> a first
				}
				return b.CreationTimestamp.Compare(a.CreationTimestamp.Time) // newest first
			})

			toDeleteCount := min(sandboxesToDelete, int32(len(activeSandboxes)))
			// Parallel sandbox deletion with adaptive slow-start batching (starts with 1 and doubles on success)
			_, deleteErr := slowStartBatch(ctx, int(toDeleteCount), 1, func(idx int) error {
				sb := &activeSandboxes[idx]
				r.exp().ExpectDeletion(poolKey, sb.UID)
				err := r.Delete(ctx, sb)
				if err == nil {
					return nil
				}
				// No delete watch event will lower this expectation: on
				// NotFound the object is already gone (its delete event may
				// have fired before the expectation was raised), and on any
				// other error nothing was deleted. Observe synthetically so
				// the pool is not blocked until the expectations timeout —
				// the same recovery kube's ReplicaSet controller applies to
				// failed deletes.
				r.exp().DeletionObserved(poolKey, sb.UID)
				if k8serrors.IsNotFound(err) {
					// Not an error for the batch: the desired outcome
					// (sandbox gone) already holds.
					return nil
				}
				logger.Error(err, "Failed to delete sandbox", "sandbox", sb.Name, "namespace", sb.Namespace)
				return err
			})
			if deleteErr != nil {
				logger.Error(deleteErr, "Failed to delete pool sandboxes")
				allErrors = errors.Join(allErrors, deleteErr)
			}
		}
	}

	// Surface (and clear) the not-progressing signal. A pool with
	// unschedulable sandboxes past the readiness grace period cannot make
	// progress toward spec.replicas until cluster capacity frees up; degrade
	// visibly instead of churning.
	if unschedulableReplicas > 0 {
		r.setNotProgressing(warmPool, poolKey, true, fmt.Sprintf(
			"%d/%d sandboxes are unschedulable past the %s readiness grace period; holding them instead of replacing (replacements would be equally unschedulable)",
			unschedulableReplicas, desiredReplicas, r.readinessGracePeriod()))
		requeueAfter = minNonZeroDuration(requeueAfter, r.unschedulableRecheckInterval())
	} else {
		r.setNotProgressing(warmPool, poolKey, false, "")
	}

	// Self-schedule the post-grace evaluation for not-yet-Ready sandboxes so
	// the stuck-GC and the unschedulable-hold run on time even in a cluster
	// with no ambient traffic. Jittered so a fleet warmed together does not
	// re-reconcile in one synchronized post-grace spike (wait.Jitter treats
	// factor <= 0 as a default, so guard the tests' zeroed factor).
	if nextGraceDeadline > 0 && graceRequeueJitterFactor > 0 {
		nextGraceDeadline = wait.Jitter(nextGraceDeadline, graceRequeueJitterFactor)
	}
	requeueAfter = minNonZeroDuration(requeueAfter, nextGraceDeadline)

	if tmplErr != nil && !k8serrors.IsNotFound(tmplErr) {
		allErrors = errors.Join(allErrors, tmplErr)
	}

	return requeueAfter, allErrors
}

// minNonZeroDuration returns the smaller of two requeue delays, treating zero
// as "no requeue requested".
func minNonZeroDuration(a, b time.Duration) time.Duration {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	return min(a, b)
}

// setNotProgressing tracks the pool's not-progressing state and emits a
// transition Event: a Warning when the pool stops progressing and a Normal
// event once progress resumes. Repeated reconciles in the same state do not
// re-emit.
func (r *SandboxWarmPoolReconciler) setNotProgressing(warmPool *extensionsv1beta1.SandboxWarmPool, poolKey types.NamespacedName, notProgressing bool, message string) {
	r.notProgressingMu.Lock()
	_, was := r.notProgressing[poolKey]
	if notProgressing == was {
		r.notProgressingMu.Unlock()
		return
	}
	if notProgressing {
		if r.notProgressing == nil {
			r.notProgressing = make(map[types.NamespacedName]struct{})
		}
		r.notProgressing[poolKey] = struct{}{}
	} else {
		delete(r.notProgressing, poolKey)
	}
	r.notProgressingMu.Unlock()

	if r.Recorder == nil {
		return
	}
	if notProgressing {
		r.Recorder.Eventf(warmPool, nil, corev1.EventTypeWarning, reasonWarmPoolNotProgressing, "Reconciling", "%s", message)
	} else {
		r.Recorder.Eventf(warmPool, nil, corev1.EventTypeNormal, reasonWarmPoolProgressing, "Reconciling", "Warm pool is progressing again")
	}
}

// isSandboxPodUnschedulable reports whether the sandbox's backing pod is
// currently unschedulable (PodScheduled=False with reason Unschedulable).
// Missing pods or pods without a definitive PodScheduled=False/Unschedulable
// condition report false, preserving the delete-and-replace behavior for
// genuinely stuck sandboxes.
func (r *SandboxWarmPoolReconciler) isSandboxPodUnschedulable(ctx context.Context, sb *sandboxv1beta1.Sandbox) bool {
	// The backing pod normally shares the sandbox's name; a sandbox that
	// adopted a warm pod tracks the pod name in an annotation (same
	// resolution the sandbox controller uses).
	podName := sb.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]
	if podName == "" {
		podName = sb.Name
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: podName}, pod); err != nil {
		return false
	}
	if !pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled {
			return cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable
		}
	}
	return false
}

// resolveUpdateStrategy returns the effective update strategy for the warm pool,
// defaulting to OnReplenish when unspecified or unknown.
func resolveUpdateStrategy(warmPool *extensionsv1beta1.SandboxWarmPool) extensionsv1beta1.SandboxWarmPoolUpdateStrategyType {
	if warmPool.Spec.UpdateStrategy == nil {
		return extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType
	}
	switch warmPool.Spec.UpdateStrategy.Type {
	case extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType:
		return extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType
	default:
		return extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType
	}
}

// adoptSandbox sets this warmpool as the owner of an orphaned sandbox.
func (r *SandboxWarmPoolReconciler) adoptSandbox(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool, sb *sandboxv1beta1.Sandbox) error {
	if err := controllerutil.SetControllerReference(warmPool, sb, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference for warm pool sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	setWarmLaunchTypeLabelIfNeeded(sb)
	return r.Update(ctx, sb)
}

func setWarmLaunchTypeLabelIfNeeded(sb *sandboxv1beta1.Sandbox) bool {
	if sb.Labels == nil {
		sb.Labels = make(map[string]string)
	}
	if sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] == sandboxv1beta1.SandboxLaunchTypeWarm {
		return false
	}
	sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	return true
}

// filterActiveSandboxes filters the list of sandboxes, deleting stale ones and adopting orphans.
// It returns the pool's active sandboxes plus the number of pool-owned
// terminating sandboxes: ones with a deletion timestamp, ones this controller
// deleted but whose deletion the cache has not observed yet, and ones deleted
// as stale in this pass. Terminating sandboxes are excluded from active (and
// so from Ready accounting), but still occupy capacity, so the create path
// must count them against spec.replicas (#1215).
func (r *SandboxWarmPoolReconciler) filterActiveSandboxes(ctx context.Context, poolKey types.NamespacedName, warmPool *extensionsv1beta1.SandboxWarmPool, sandboxes []sandboxv1beta1.Sandbox, template *extensionsv1beta1.SandboxTemplate, currentSandboxBlueprintHash string, tmplErr error) ([]sandboxv1beta1.Sandbox, int32, error) {
	logger := log.FromContext(ctx)
	var activeSandboxes []sandboxv1beta1.Sandbox
	terminatingReplicas := int32(0)
	var allErrors error

	vettedHashes := make(map[string]bool)

	// Determine the update strategy, defaulting to OnReplenish if not specified or unknown.
	updateStrategy := resolveUpdateStrategy(warmPool)
	if raw := warmPool.Spec.UpdateStrategy; raw != nil &&
		raw.Type != "" &&
		raw.Type != extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType &&
		raw.Type != extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType {
		logger.Info("Unknown update strategy, defaulting to OnReplenish", "strategy", raw.Type)
	}

	for _, sb := range sandboxes {
		controllerRef := metav1.GetControllerOf(&sb)
		isOrphan := controllerRef == nil
		isControlledByPool := controllerRef != nil && controllerRef.UID == warmPool.UID

		if !sb.DeletionTimestamp.IsZero() {
			// Terminating pool members are no longer active, but they still
			// occupy capacity until fully gone: count them so create gating
			// cannot balloon the population while deletes lag (#1215).
			if isControlledByPool {
				terminatingReplicas++
			}
			continue
		}

		// A sandbox this controller already deleted may still show up in the
		// (lagging) cache without a deletion timestamp; treat it as
		// terminating, not active.
		if isControlledByPool && r.exp().IsPendingDeletion(poolKey, sb.UID) {
			terminatingReplicas++
			continue
		}

		if !isOrphan && !isControlledByPool {
			logger.Info("Ignoring sandbox with different controller", "sandbox", sb.Name, "controller", controllerRef.Name)
			continue
		}

		if tmplErr == nil && (updateStrategy == extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType || isOrphan) {
			if r.isSandboxStale(ctx, &sb, template, currentSandboxBlueprintHash, vettedHashes) {
				logger.Info("Deleting stale sandbox", "sandbox", sb.Name, "isOrphan", isOrphan)
				// Only pool-owned sandboxes get deletion expectations: the
				// watch handler can only map owned delete events back to the
				// pool, and only owned sandboxes count against the target.
				if isControlledByPool {
					r.exp().ExpectDeletion(poolKey, sb.UID)
				}
				if err := r.Delete(ctx, &sb); err != nil {
					if isControlledByPool {
						r.exp().DeletionObserved(poolKey, sb.UID)
					}
					logger.Error(err, "Failed to delete stale sandbox", "sandbox", sb.Name)
					allErrors = errors.Join(allErrors, err)
				} else if isControlledByPool {
					terminatingReplicas++
				}
				continue
			}
		}

		if isControlledByPool && setWarmLaunchTypeLabelIfNeeded(&sb) {
			if err := r.Update(ctx, &sb); err != nil {
				logger.Error(err, "Failed to update sandbox launch type label", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				continue
			}
		}

		if isOrphan {
			logger.Info("Adopting orphaned sandbox", "sandbox", sb.Name)
			if err := r.adoptSandbox(ctx, warmPool, &sb); err != nil {
				logger.Error(err, "Failed to adopt sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				continue
			}
		}

		activeSandboxes = append(activeSandboxes, sb)
	}
	return activeSandboxes, terminatingReplicas, allErrors
}

// computePodTemplateHash computes a hash of the sandbox template's Spec.PodTemplate.
func computePodTemplateHash(template *extensionsv1beta1.SandboxTemplate) (string, error) {
	specJSON, err := json.Marshal(template.Spec.PodTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to marshal pod template for hashing: %w", err)
	}
	return sandboxcontrollers.NameHash(string(specJSON)), nil
}

// computeSandboxBlueprintHash computes a hash of the sandbox template's Spec.SandboxBlueprint.
func computeSandboxBlueprintHash(template *extensionsv1beta1.SandboxTemplate) (string, error) {
	specJSON, err := json.Marshal(template.Spec.SandboxBlueprint)
	if err != nil {
		return "", fmt.Errorf("failed to marshal sandbox blueprint for hashing: %w", err)
	}
	return sandboxcontrollers.NameHash(string(specJSON)), nil
}

// fetchTemplateAndHash fetches the sandbox template and computes its hash.
func (r *SandboxWarmPoolReconciler) fetchTemplateAndHash(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) (*extensionsv1beta1.SandboxTemplate, string, string, error) {
	logger := log.FromContext(ctx)
	template, tmplErr := r.getTemplate(ctx, warmPool)
	var currentPodTemplateHash, currentSandboxBlueprintHash string
	if tmplErr == nil {
		currentPodTemplateHash, tmplErr = computePodTemplateHash(template)
	}
	if tmplErr == nil {
		currentSandboxBlueprintHash, tmplErr = computeSandboxBlueprintHash(template)
	}

	if tmplErr != nil {
		logger.Error(tmplErr, "Failed to get sandbox template and hash", "templateRef", warmPool.Spec.TemplateRef.Name)
	}
	return template, currentPodTemplateHash, currentSandboxBlueprintHash, tmplErr
}

// buildSandboxCR constructs the base Sandbox CR (with pod template and volume claim templates) for the warm pool.
func (r *SandboxWarmPoolReconciler) buildSandboxCR(
	warmPool *extensionsv1beta1.SandboxWarmPool,
	poolNameHash string,
	template *extensionsv1beta1.SandboxTemplate,
	currentPodTemplateHash string,
	currentSandboxBlueprintHash string,
) (*sandboxv1beta1.Sandbox, error) {
	sandboxLabels := map[string]string{
		warmPoolSandboxLabel:                                 poolNameHash,
		sandboxTemplateRefHash:                               SandboxTemplateRefHash(warmPool.Spec.TemplateRef.Name),
		sandboxv1beta1.SandboxLaunchTypeLabel:                sandboxv1beta1.SandboxLaunchTypeWarm,
		sandboxv1beta1.DeprecatedSandboxPodTemplateHashLabel: currentPodTemplateHash,
		sandboxv1beta1.SandboxTemplateHashLabel:              currentSandboxBlueprintHash,
		sandboxv1beta1.CreatedByLabel:                        "controller",
	}

	// Build annotations for the Sandbox CR
	sandboxAnnotations := map[string]string{
		sandboxv1beta1.SandboxTemplateRefAnnotation: warmPool.Spec.TemplateRef.Name,
	}

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", warmPool.Name),
			Namespace:    warmPool.Namespace,
			Labels:       sandboxLabels,
			Annotations:  sandboxAnnotations,
		},
		// Deep-copy the entire shared blueprint
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy(),
		},
	}

	// Propagate pool and template labels to pod template for consistency and targeting
	if sandbox.Spec.PodTemplate.ObjectMeta.Labels == nil {
		sandbox.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[warmPoolSandboxLabel] = poolNameHash
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash] = SandboxTemplateRefHash(warmPool.Spec.TemplateRef.Name)
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.DeprecatedSandboxPodTemplateHashLabel] = currentPodTemplateHash
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.SandboxTemplateHashLabel] = currentSandboxBlueprintHash

	// Respect the template's custom eviction annotation if explicitly specified.
	// Only apply the default eviction behavior if the annotation is not defined.
	if _, exists := sandbox.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation]; !exists {
		if r.EnableWarmPoolEviction {
			if sandbox.Spec.PodTemplate.ObjectMeta.Annotations == nil {
				sandbox.Spec.PodTemplate.ObjectMeta.Annotations = make(map[string]string)
			}
			sandbox.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation] = "true"
		}
	}

	// Apply secure defaults to the sandbox pod spec
	ApplySandboxSecureDefaults(template, &sandbox.Spec.PodTemplate.Spec)

	// Set controller reference so the Sandbox is owned by the SandboxWarmPool
	if err := ctrl.SetControllerReference(warmPool, sandbox, r.Scheme); err != nil {
		return nil, fmt.Errorf("SetControllerReference for Sandbox failed: %w", err)
	}

	return sandbox, nil
}

// createPoolSandbox creates a full Sandbox CR for the warm pool using a pre-built sandboxCR.
func (r *SandboxWarmPoolReconciler) createPoolSandbox(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool, sandboxCR *sandboxv1beta1.Sandbox) error {
	logger := log.FromContext(ctx)
	sandbox := sandboxCR.DeepCopy()
	if err := r.Create(ctx, sandbox); err != nil {
		logger.Error(err, "Failed to create pool sandbox")
		return err
	}

	logger.Info("Created new pool sandbox", "sandbox", sandbox.Name, "poolName", warmPool.Name)
	return nil
}

// updateStatus updates the status of the SandboxWarmPool if it has changed.
func (r *SandboxWarmPoolReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1beta1.SandboxWarmPoolStatus, warmPool *extensionsv1beta1.SandboxWarmPool) error {
	logger := log.FromContext(ctx)

	// Check if status has changed
	if equality.Semantic.DeepEqual(oldStatus, &warmPool.Status) {
		return nil
	}

	oldWarmPool := warmPool.DeepCopy()
	oldWarmPool.Status = *oldStatus
	patch := client.MergeFrom(oldWarmPool)

	if err := r.Status().Patch(ctx, warmPool, patch); err != nil {
		return fmt.Errorf("failed to update SandboxWarmPool status: %w", err)
	}

	logger.Info("Updated SandboxWarmPool status", "replicas", warmPool.Status.Replicas, "readyReplicas", warmPool.Status.ReadyReplicas)
	return nil
}

func (r *SandboxWarmPoolReconciler) getTemplate(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) (*extensionsv1beta1.SandboxTemplate, error) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: warmPool.Namespace,
			Name:      warmPool.Spec.TemplateRef.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
		if !k8serrors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox template %q: %w", warmPool.Spec.TemplateRef.Name, err)
		}
		return nil, err
	}

	return template, nil
}

// isSandboxStale checks if the sandbox version matches the current template.
// It uses a cache (vettedHashes) to avoid repeated expensive DeepEqual calls
// for sandboxes with the same hash.
func (r *SandboxWarmPoolReconciler) isSandboxStale(
	ctx context.Context,
	sandbox *sandboxv1beta1.Sandbox,
	template *extensionsv1beta1.SandboxTemplate,
	currentSandboxBlueprintHash string,
	vettedHashes map[string]bool,
) bool {
	sandboxHash := sandbox.Labels[sandboxv1beta1.SandboxTemplateHashLabel]

	// If the templateRefHash doesn't match, it's stale.
	if sandbox.Labels[sandboxTemplateRefHash] != SandboxTemplateRefHash(template.Name) {
		return true
	}

	// Check if the sandbox is unowned (orphaned).
	controllerRef := metav1.GetControllerOf(sandbox)
	isOrphan := controllerRef == nil
	if isOrphan {
		// Always perform full semantic comparison for orphans.
		return !compareSandboxBlueprint(template, &sandbox.Spec.SandboxBlueprint)
	}

	// If hashes match, it's fresh.
	if sandboxHash != "" && sandboxHash == currentSandboxBlueprintHash {
		return false
	}

	// If currentSandboxBlueprintHash is empty, it means we failed to compute it.
	// In this case, we should log an error and treat it as NOT stale to avoid
	// mass-deleting existing sandboxes due to a marshal failure.
	if currentSandboxBlueprintHash == "" {
		log.FromContext(ctx).Error(nil, "currentSandboxBlueprintHash is empty, skipping staleness check", "sandbox", sandbox.Name)
		return false
	}

	// Check if we've already evaluated this specific old version.
	if sandboxHash != "" {
		if isStale, found := vettedHashes[sandboxHash]; found {
			return isStale
		}
	}

	// Perform a semantic comparison of the sandbox blueprint.
	// We normalize the pod spec by applying the same secure defaults
	// used during creation to avoid false positives from controller-injected fields.
	isStale := !compareSandboxBlueprint(template, &sandbox.Spec.SandboxBlueprint)

	// Save the result for the next sandbox with this same hash.
	if sandboxHash != "" {
		vettedHashes[sandboxHash] = isStale
	}

	return isStale
}

// comparePodSpecs checks if the pod spec in the sandbox is semantically equal to the template,
// normalizing for fields that the controller populates by default.
func comparePodSpecs(template *extensionsv1beta1.SandboxTemplate, actualSandboxSpec *corev1.PodSpec) bool {
	// Create what the sandbox SHOULD look like if it were created from the current template.
	expectedSpec := template.Spec.PodTemplate.Spec.DeepCopy()
	ApplySandboxSecureDefaults(template, expectedSpec)

	// Compare the actual sandbox spec to the expected "perfect" spec.
	// Since both have now undergone the exact same defaulting logic,
	// any remaining difference is a TRUE template drift.
	return equality.Semantic.DeepEqual(expectedSpec, actualSandboxSpec)
}

// compareVolumeClaimTemplates checks if the volume claim templates in the sandbox are equal to the template.
// Only each entry's name and spec are compared, as changes in metadata (like labels, annotations) are not tracked for staleness.
// Note: Comparison is index-based (order-sensitive) to stay consistent with computeSandboxBlueprintHash (+listType=atomic).
// Making this comparison order-independent without also sorting the templates in computeSandboxBlueprintHash
// would cause reordered warm sandboxes to fail the hash label check on every reconcile.
func compareVolumeClaimTemplates(template *extensionsv1beta1.SandboxTemplate, actualVCTs []sandboxv1beta1.PersistentVolumeClaimTemplate) bool {
	if len(template.Spec.SandboxBlueprint.VolumeClaimTemplates) != len(actualVCTs) {
		return false
	}

	for i, tmplVCT := range template.Spec.SandboxBlueprint.VolumeClaimTemplates {
		actualVCT := actualVCTs[i]
		if tmplVCT.Name != actualVCT.Name || !equality.Semantic.DeepEqual(tmplVCT.Spec, actualVCT.Spec) {
			return false
		}
	}

	return true
}

// compareSandboxBlueprint checks if the sandbox blueprint in the sandbox is semantically equal to the template,
// ignoring metadata differences and only comparing the fields that are relevant for staleness detection.
func compareSandboxBlueprint(template *extensionsv1beta1.SandboxTemplate, actualSandboxSpec *sandboxv1beta1.SandboxBlueprint) bool {
	return comparePodSpecs(template, &actualSandboxSpec.PodTemplate.Spec) &&
		compareVolumeClaimTemplates(template, actualSandboxSpec.VolumeClaimTemplates) &&
		equality.Semantic.DeepEqual(template.Spec.Service, actualSandboxSpec.Service)
}

// sandboxWarmPoolLabelIndexer extracts the warmPoolSandboxLabel value for the
// sandboxWarmPoolLabelIndex cache field index. Shared with tests so fake clients
// register the same index the manager does.
func sandboxWarmPoolLabelIndexer(obj client.Object) []string {
	if v, ok := obj.GetLabels()[warmPoolSandboxLabel]; ok {
		return []string{v}
	}
	return nil
}

// sandboxTemplateRefNameIndexer extracts the template reference name for the
// TemplateRefField cache field index. Shared with tests so fake clients
// register the same index the manager does.
func sandboxTemplateRefNameIndexer(obj client.Object) []string {
	wp := obj.(*extensionsv1beta1.SandboxWarmPool)
	if wp.Spec.TemplateRef.Name == "" {
		return nil
	}
	return []string{wp.Spec.TemplateRef.Name}
}

// warmPoolControllerKey resolves the SandboxWarmPool that controls obj, if any.
func warmPoolControllerKey(obj client.Object) (types.NamespacedName, bool) {
	controllerRef := metav1.GetControllerOf(obj)
	if controllerRef == nil {
		return types.NamespacedName{}, false
	}
	gv, err := schema.ParseGroupVersion(controllerRef.APIVersion)
	if err != nil || gv.Group != extensionsv1beta1.GroupVersion.Group || controllerRef.Kind != "SandboxWarmPool" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: controllerRef.Name}, true
}

// warmPoolSandboxEventHandler wraps the standard enqueue-for-owner handler so
// the expectations tracker observes owned sandbox add/delete events before the
// owning pool is enqueued. This ordering guarantees that by the time a
// reconcile triggered by one of our own writes runs, the corresponding
// expectation has already been lowered.
type warmPoolSandboxEventHandler struct {
	handler.EventHandler
	expectations *warmPoolExpectations
}

func (h *warmPoolSandboxEventHandler) Create(ctx context.Context, evt event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if key, ok := warmPoolControllerKey(evt.Object); ok {
		h.expectations.CreationObserved(key)
	}
	h.EventHandler.Create(ctx, evt, q)
}

func (h *warmPoolSandboxEventHandler) Delete(ctx context.Context, evt event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if key, ok := warmPoolControllerKey(evt.Object); ok {
		h.expectations.DeletionObserved(key, evt.Object.GetUID())
	}
	h.EventHandler.Delete(ctx, evt, q)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	if r.MaxBatchSize <= 0 {
		r.MaxBatchSize = sandboxCreateDeleteMaxBatchSize
	}

	// Index sandboxes by the warm pool label value
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &sandboxv1beta1.Sandbox{},
		sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer); err != nil {
		return fmt.Errorf("failed to index sandboxes by warm pool label: %w", err)
	}

	// Index warm pools by the template reference name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &extensionsv1beta1.SandboxWarmPool{},
		extensionsv1beta1.TemplateRefField, sandboxTemplateRefNameIndexer); err != nil {
		return fmt.Errorf("failed to index warm pools by template reference name: %w", err)
	}

	// Equivalent to Owns(&Sandbox{}), plus expectation observation on
	// add/delete events (see warmPoolSandboxEventHandler).
	sandboxHandler := &warmPoolSandboxEventHandler{
		EventHandler: handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(),
			&extensionsv1beta1.SandboxWarmPool{}, handler.OnlyControllerOwner()),
		expectations: r.exp(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1beta1.SandboxWarmPool{}).
		Watches(&sandboxv1beta1.Sandbox{}, sandboxHandler).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Watches(
			&extensionsv1beta1.SandboxTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.findWarmPoolsForTemplate),
		).
		Complete(r)
}

// findWarmPoolsForTemplate returns a list of reconcile.Requests for all SandboxWarmPools that reference the template.
func (r *SandboxWarmPoolReconciler) findWarmPoolsForTemplate(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	template, ok := obj.(*extensionsv1beta1.SandboxTemplate)
	if !ok {
		return nil
	}

	warmPools := &extensionsv1beta1.SandboxWarmPoolList{}
	if err := r.List(ctx, warmPools, client.InNamespace(template.Namespace), client.MatchingFields{extensionsv1beta1.TemplateRefField: template.Name}); err != nil {
		logger.Error(err, "Failed to list warm pools for template", "template", template.Name)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(warmPools.Items))
	for _, wp := range warmPools.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      wp.Name,
				Namespace: wp.Namespace,
			},
		})
	}
	return requests
}

// slowStartBatch is a helper that runs a given function fn multiple times in parallel batches.
// It starts with initialBatchSize, and doubles the batch size for each successful batch.
// If any execution of fn returns an error, it stops and returns the first encountered error.
func slowStartBatch(ctx context.Context, count int, initialBatchSize int, fn func(int) error) (int, error) {
	remaining := count
	successes := 0

	for batchSize := min(remaining, initialBatchSize); batchSize > 0; batchSize = min(2*batchSize, remaining) {
		if ctx.Err() != nil {
			return successes, ctx.Err()
		}

		eg, _ := errgroup.WithContext(ctx)
		var batchSuccesses atomic.Int64

		for i := range batchSize {
			index := successes + i
			eg.Go(func() error {
				if err := fn(index); err != nil {
					return err
				}
				batchSuccesses.Add(1)
				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			successes += int(batchSuccesses.Load())
			return successes, err
		}

		successes += int(batchSuccesses.Load())
		remaining -= batchSize
	}

	return successes, nil
}
