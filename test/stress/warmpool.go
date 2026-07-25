// Copyright 2026 The Kubernetes Authors.
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

package main

// warmpool-overcreate and warmpool-unschedulable: SandboxWarmPool controller
// invariant phases (correctness gates, not performance measurements).
//
// Both phases encode the invariants from issue 1215 (warm pools over-created
// replicas ~10x off a stale informer cache, and churned delete/create forever
// on unschedulable pods), fixed by the expectations-gated reconciler. They
// PASS on a fixed controller and FAIL loudly on a pre-fix one, so the
// scenario doubles as a regression gate and as live evidence when the
// reconciler's create/delete paths change.
//
// warmpool-overcreate: creates --wp-pools SandboxWarmPools of --wp-replicas
// each in one burst and watches every pool-owned Sandbox in the namespace.
// Invariants asserted once all pools are Ready (plus a short settle window):
//
//   - Exactly pools x replicas DISTINCT Sandbox creates (each distinct UID
//     observed on the sandboxes watch is the result of exactly one successful
//     POST, so this is the POST-equivalent count; failed POSTs never produce
//     an object and are invisible here). Legitimate replacements — a create
//     observed AFTER a delete of a prior member of the same pool — are
//     counted and reported separately, tolerated up to
//     --wp-replacement-tolerance in total.
//   - The concurrent live population never exceeds the target: per pool,
//     peak live members <= replicas at all times (the fixed controller gates
//     creates on the whole live population, terminating included).
//
// Ordering makes the replacement attribution race-free: events for one
// resource arrive in resourceVersion order on a single watch, and the fixed
// controller only creates a replacement after its own informer observed the
// deletion, so the DELETED event precedes the replacement's ADDED event in
// our stream too.
//
// warmpool-unschedulable: creates ONE pool of --wp-unsched-replicas whose
// template requests --wp-unsched-cpu CPUs (default 1000 — no machine
// anywhere near that exists in any cloud today, so the pods are robustly
// Unschedulable), then observes for --wp-unsched-watch. Invariants:
//
//   - Zero delete/recreate churn: no pool sandbox is deleted and no extra
//     one is created, i.e. the member UID set stays exactly stable (the
//     pre-fix controller replaced "stuck" sandboxes past the readiness grace
//     with equally unschedulable ones, forever).
//   - Exactly one WarmPoolNotProgressing Warning event for the pool, no
//     duplicates. The controller emits it when unschedulable sandboxes cross
//     the (fixed, 5-minute) readiness grace period; the self-scheduled
//     post-grace requeue is jittered up to +50%, so the event lands between
//     ~5m02s and ~7m35s after pool creation — the 8m default watch window
//     covers the worst case.
//
// Neither phase registers tracker records (the controller, not the harness,
// creates the Sandboxes); results are attached to the summary as structured
// reports (WarmPoolOvercreateReport / WarmPoolUnschedulableReport) instead.

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

// warmPoolNotProgressingReason is the Event reason the SandboxWarmPool
// controller emits when a pool holds unschedulable sandboxes past the
// readiness grace period (see extensions/controllers, issue 1215).
const warmPoolNotProgressingReason = "WarmPoolNotProgressing"

// warmPoolSettleWindow is how long warmpool-overcreate keeps observing after
// all pools report Ready: the pre-fix controller cleaned up its surplus
// AFTER the fill, so trailing deletes/creates must land in the accounting.
const warmPoolSettleWindow = 15 * time.Second

// WarmPoolOvercreateReport carries the warmpool-overcreate phase's observed
// counts and verdict inputs into summary.json (PhaseSummary.WarmPoolOvercreate).
type WarmPoolOvercreateReport struct {
	Pools           int `json:"pools"`
	ReplicasPerPool int `json:"replicasPerPool"`
	// TargetSandboxes is Pools * ReplicasPerPool: the exact number of
	// distinct creates a correct controller performs (plus replacements).
	TargetSandboxes int `json:"targetSandboxes"`
	// DistinctCreates counts distinct pool-owned Sandbox UIDs observed on
	// the watch: the POST-equivalent count (one successful create call per
	// UID). Invariant: DistinctCreates == TargetSandboxes + Replacements.
	DistinctCreates int `json:"distinctCreates"`
	// Replacements are creates observed after a delete of a prior member of
	// the same pool (legitimate: e.g. the stuck-sandbox GC replacing a
	// genuinely wedged sandbox). Tolerated up to --wp-replacement-tolerance.
	Replacements int `json:"replacements"`
	// OverCreates are creates beyond the pool's target that were NOT covered
	// by a previously observed delete: the issue-1215 over-creation shape.
	// Invariant: 0.
	OverCreates int `json:"overCreates"`
	// ObservedDeletes counts pool-member DELETED watch events during the
	// observation window (cleanup is excluded; observation stops before it).
	ObservedDeletes int `json:"observedDeletes"`
	// MaxPoolPeakLive is the largest concurrent live-member count any single
	// pool reached. Invariant: <= ReplicasPerPool.
	MaxPoolPeakLive int `json:"maxPoolPeakLive"`
	// PoolsExceedingTarget counts pools whose peak live population exceeded
	// their replica target at any moment. Invariant: 0.
	PoolsExceedingTarget int `json:"poolsExceedingTarget"`
	// PeakLiveTotal is the largest concurrent live-member count across all
	// pools combined (informational; bounded by pools x replicas when the
	// per-pool invariant holds).
	PeakLiveTotal int `json:"peakLiveTotal"`
	// TimeToAllReadySeconds is first pool Create -> every pool reporting
	// readyReplicas >= replicas, set only when all pools became Ready.
	TimeToAllReadySeconds *float64 `json:"timeToAllReadySeconds,omitempty"`
}

// WarmPoolUnschedulableReport carries the warmpool-unschedulable phase's
// observations into summary.json (PhaseSummary.WarmPoolUnschedulable).
type WarmPoolUnschedulableReport struct {
	Replicas   int    `json:"replicas"`
	CPURequest string `json:"cpuRequest"`
	// WatchSeconds is the observation window measured from pool creation.
	WatchSeconds float64 `json:"watchSeconds"`
	// DistinctCreates counts distinct pool-owned Sandbox UIDs observed.
	// Invariant: == Replicas (no recreates).
	DistinctCreates int `json:"distinctCreates"`
	// ObservedDeletes counts pool-member DELETED watch events. Invariant: 0
	// (the pre-fix controller churned delete->create on unschedulable pods).
	ObservedDeletes int `json:"observedDeletes"`
	// UIDsStable is the combined churn verdict: exactly Replicas distinct
	// UIDs, none deleted.
	UIDsStable bool `json:"uidsStable"`
	// NotProgressingEvents is the deduplication-aware occurrence count of
	// Warning WarmPoolNotProgressing events for the pool (sum of each event
	// object's count field, min 1). Invariant: exactly 1.
	NotProgressingEvents int `json:"notProgressingEvents"`
	// FirstEventOffsetSeconds is pool creation -> first NotProgressing event
	// observed (expected between the 5m readiness grace and ~1.5x it).
	FirstEventOffsetSeconds *float64 `json:"firstEventOffsetSeconds,omitempty"`
}

// poolAccountant consumes the shared sandboxes watch stream (via a Tracker
// observer) and keeps create/delete/live accounting per SandboxWarmPool.
type poolAccountant struct {
	mu        sync.Mutex
	namespace string
	pools     map[string]*poolAccount
	// liveAll / peakLiveAll track the concurrent live population across all
	// pools (exact: updated on every observed create/delete).
	liveAll     int
	peakLiveAll int
}

type poolAccount struct {
	replicas int
	// seen holds every distinct member UID ever observed (creates).
	seen map[types.UID]struct{}
	// live holds currently existing member UIDs.
	live     map[types.UID]struct{}
	peakLive int
	// deleteCredits counts observed deletes not yet matched to a
	// replacement create.
	deleteCredits int
	replacements  int
	overCreates   int
	deletes       int
}

func newPoolAccountant(namespace string, poolNames []string, replicas int) *poolAccountant {
	a := &poolAccountant{
		namespace: namespace,
		pools:     make(map[string]*poolAccount, len(poolNames)),
	}
	for _, name := range poolNames {
		a.pools[name] = &poolAccount{
			replicas: replicas,
			seen:     make(map[types.UID]struct{}),
			live:     make(map[types.UID]struct{}),
		}
	}
	return a
}

// owningWarmPool returns the name of the SandboxWarmPool owning u, or "".
func owningWarmPool(u *unstructured.Unstructured) string {
	for _, ref := range u.GetOwnerReferences() {
		if ref.Kind == "SandboxWarmPool" {
			return ref.Name
		}
	}
	return ""
}

// observe is a Tracker watch observer (see Tracker.AddObserver). It runs on
// the watch decode path and must stay cheap.
func (a *poolAccountant) observe(resource string, eventType watch.EventType, u *unstructured.Unstructured) {
	if resource != "sandboxes" || u.GetNamespace() != a.namespace {
		return
	}
	poolName := owningWarmPool(u)
	if poolName == "" {
		return
	}
	uid := u.GetUID()
	if uid == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	acct, ok := a.pools[poolName]
	if !ok {
		return
	}

	if eventType == watch.Deleted {
		if _, isLive := acct.live[uid]; isLive {
			delete(acct.live, uid)
			acct.deletes++
			acct.deleteCredits++
			a.liveAll--
		}
		return
	}

	// A create is the first sighting of a UID. That is normally the ADDED
	// event, but a re-established watch can start "at most recent" and hand
	// us a MODIFIED first — count that sighting, and ignore all later events
	// for a known UID.
	if _, known := acct.seen[uid]; known {
		return
	}
	acct.seen[uid] = struct{}{}
	acct.live[uid] = struct{}{}
	if len(acct.seen) > acct.replicas {
		// Beyond the initial fill: legitimate only as a replacement for an
		// already-observed deletion (see the package comment for why the
		// DELETED event is guaranteed to precede its replacement's ADDED).
		if acct.deleteCredits > 0 {
			acct.deleteCredits--
			acct.replacements++
		} else {
			acct.overCreates++
		}
	}
	if n := len(acct.live); n > acct.peakLive {
		acct.peakLive = n
	}
	a.liveAll++
	if a.liveAll > a.peakLiveAll {
		a.peakLiveAll = a.liveAll
	}
}

// poolTotals aggregates the accountant's per-pool state.
type poolTotals struct {
	distinctCreates      int
	replacements         int
	overCreates          int
	deletes              int
	maxPoolPeakLive      int
	poolsExceedingTarget int
	peakLiveAll          int
	// worstPools lists pools with over-creates or peak violations (sorted,
	// truncated by the caller for logging).
	worstPools []string
}

func (a *poolAccountant) totals() poolTotals {
	a.mu.Lock()
	defer a.mu.Unlock()
	var t poolTotals
	for name, acct := range a.pools {
		t.distinctCreates += len(acct.seen)
		t.replacements += acct.replacements
		t.overCreates += acct.overCreates
		t.deletes += acct.deletes
		if acct.peakLive > t.maxPoolPeakLive {
			t.maxPoolPeakLive = acct.peakLive
		}
		if acct.peakLive > acct.replicas {
			t.poolsExceedingTarget++
		}
		if acct.overCreates > 0 || acct.peakLive > acct.replicas {
			t.worstPools = append(t.worstPools, fmt.Sprintf("%s(creates=%d over=%d peak=%d)", name, len(acct.seen), acct.overCreates, acct.peakLive))
		}
	}
	t.peakLiveAll = a.peakLiveAll
	slices.Sort(t.worstPools)
	return t
}

// warmPoolEventCounter counts occurrences of one Event reason for one
// SandboxWarmPool from the shared core-v1 events watch. The controller emits
// through the events.k8s.io recorder; those events surface on the core v1
// events resource with reason/type/involvedObject preserved, and repeats are
// deduplicated into the SAME event object with a bumped count — so
// occurrences sums max(1, count) per distinct event UID, and a re-emit
// folded into an existing object still fails an exactly-once assertion.
type warmPoolEventCounter struct {
	mu        sync.Mutex
	namespace string
	pool      string
	reason    string
	firstSeen time.Time
	counts    map[types.UID]int64
}

func newWarmPoolEventCounter(namespace, pool, reason string) *warmPoolEventCounter {
	return &warmPoolEventCounter{
		namespace: namespace,
		pool:      pool,
		reason:    reason,
		counts:    make(map[types.UID]int64),
	}
}

// observe is a Tracker watch observer (see Tracker.AddObserver).
func (c *warmPoolEventCounter) observe(resource string, eventType watch.EventType, u *unstructured.Unstructured) {
	if resource != "events" || eventType == watch.Deleted || u.GetNamespace() != c.namespace {
		return
	}
	if reason, _, _ := unstructured.NestedString(u.Object, "reason"); reason != c.reason {
		return
	}
	if etype, _, _ := unstructured.NestedString(u.Object, "type"); etype != "Warning" {
		return
	}
	if kind, _, _ := unstructured.NestedString(u.Object, "involvedObject", "kind"); kind != "SandboxWarmPool" {
		return
	}
	if name, _, _ := unstructured.NestedString(u.Object, "involvedObject", "name"); name != c.pool {
		return
	}
	count, found, _ := unstructured.NestedInt64(u.Object, "count")
	if !found || count < 1 {
		count = 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.firstSeen.IsZero() {
		c.firstSeen = time.Now()
	}
	if count > c.counts[u.GetUID()] {
		c.counts[u.GetUID()] = count
	}
}

// occurrences returns the deduplication-aware event count and the time the
// first matching event was observed (zero when none was).
func (c *warmPoolEventCounter) occurrences() (int, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := int64(0)
	for _, n := range c.counts {
		total += n
	}
	return int(total), c.firstSeen
}

// buildUnschedulableTemplateObject returns a SandboxTemplate whose pod can
// never schedule: the main container requests cpu CPUs (the
// --wp-unsched-cpu default of "1000" exceeds any machine shape sold by any
// cloud today by more than 2x, so the scheduler reports
// PodScheduled=False/Unschedulable everywhere — which is exactly the
// condition the controller's unschedulable hold keys on). Built on
// buildTemplateObject so the rest of the spec matches the other phases.
func buildUnschedulableTemplateObject(id types.NamespacedName, image, cpu string) *unstructured.Unstructured {
	obj := buildTemplateObject(id, image)
	// Mutate the freshly built map directly: the unstructured helpers
	// deep-copy via DeepCopyJSON, which rejects the []string command value
	// buildTemplateObject uses, and a copy is pointless on our own object.
	podSpec := obj.Object["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)
	container := podSpec["containers"].([]any)[0].(map[string]any)
	container["resources"] = map[string]any{
		"requests": map[string]any{
			"cpu":    cpu,
			"memory": "64Mi",
		},
	}
	return obj
}

// runWarmPoolOvercreatePhase creates --wp-pools pools of --wp-replicas and
// asserts the exactly-N-creates and never-above-target invariants (see the
// package comment).
func (s *stressTest) runWarmPoolOvercreatePhase(ctx context.Context, number PhaseNumber) error {
	pools := s.cfg.WPPools
	replicas := s.cfg.WPReplicas
	target := pools * replicas
	image := s.cfg.WPImage
	if image == "" {
		image = s.cfg.Image
	}

	templateID := types.NamespacedName{Name: fmt.Sprintf("p%d-wp-template", number), Namespace: s.namespace}
	poolNames := make([]string, 0, pools)
	for i := range pools {
		poolNames = append(poolNames, fmt.Sprintf("p%d-wp-pool-%d", number, i))
	}

	log.Printf("[%s#%d] creating %d pools x %d replicas = %d sandboxes (image %s, replacement tolerance %d)",
		PhaseWarmPoolOvercreate, number, pools, replicas, target, image, s.cfg.WPReplacementTolerance)

	// Observe pool-owned sandboxes from BEFORE the first pool exists so the
	// very first creates are accounted.
	acct := newPoolAccountant(s.namespace, poolNames, replicas)
	removeObserver := s.tracker.AddObserver(acct.observe)
	observing := true
	stopObserving := func() {
		if observing {
			removeObserver()
			observing = false
		}
	}
	defer stopObserving()

	if _, err := s.templateClient.Create(ctx, buildTemplateObject(templateID, image), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("[%s#%d] failed to create sandbox template: %w", PhaseWarmPoolOvercreate, number, err)
	}
	// Clean up even when the phase fails partway: later phases assume the
	// cluster's spare capacity is back. stopObserving runs first so cleanup
	// deletes never pollute the accounting.
	defer s.cleanupWarmPoolPhase(ctx, PhaseWarmPoolOvercreate, number, poolNames, templateID, stopObserving)

	start := time.Now()
	if _, err := ForkJoin(ctx, poolNames, s.cfg.CreateConcurrency, func(name string) (struct{}, error) {
		id := types.NamespacedName{Name: name, Namespace: s.namespace}
		if _, err := s.warmPoolClient.Create(ctx, buildWarmPoolObject(id, templateID.Name, replicas), metav1.CreateOptions{}); err != nil {
			return struct{}{}, fmt.Errorf("failed to create warm pool %s: %w", name, err)
		}
		return struct{}{}, nil
	}); err != nil {
		return fmt.Errorf("[%s#%d] %w", PhaseWarmPoolOvercreate, number, err)
	}

	// Pool provisioning is part of the observation (the over-creation race
	// lives in the fill), but readiness itself is just the phase's endpoint.
	readyErr := s.waitAllWarmPoolsReady(ctx, PhaseWarmPoolOvercreate, number, replicas, pools)
	var timeToAllReady *float64
	if readyErr == nil {
		d := time.Since(start).Seconds()
		timeToAllReady = &d
		log.Printf("[%s#%d] all %d pools Ready after %.1fs; settling %s to catch trailing churn",
			PhaseWarmPoolOvercreate, number, pools, d, warmPoolSettleWindow)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(warmPoolSettleWindow):
		}
	}

	stopObserving()

	totals := acct.totals()
	report := &WarmPoolOvercreateReport{
		Pools:                 pools,
		ReplicasPerPool:       replicas,
		TargetSandboxes:       target,
		DistinctCreates:       totals.distinctCreates,
		Replacements:          totals.replacements,
		OverCreates:           totals.overCreates,
		ObservedDeletes:       totals.deletes,
		MaxPoolPeakLive:       totals.maxPoolPeakLive,
		PoolsExceedingTarget:  totals.poolsExceedingTarget,
		PeakLiveTotal:         totals.peakLiveAll,
		TimeToAllReadySeconds: timeToAllReady,
	}
	s.setWarmPoolOvercreateReport(number, report)

	log.Printf("[%s#%d] observed: %d distinct creates (target %d), %d replacements, %d over-creates, %d deletes, max pool peak %d/%d, global peak %d/%d",
		PhaseWarmPoolOvercreate, number, report.DistinctCreates, target, report.Replacements, report.OverCreates,
		report.ObservedDeletes, report.MaxPoolPeakLive, replicas, report.PeakLiveTotal, target)

	var problems []string
	if readyErr != nil {
		problems = append(problems, readyErr.Error())
	}
	if report.OverCreates > 0 {
		problems = append(problems, fmt.Sprintf("%d sandbox creates beyond target without a preceding delete (over-creation)", report.OverCreates))
	}
	if want := target + report.Replacements; report.DistinctCreates != want {
		problems = append(problems, fmt.Sprintf("%d distinct creates, want exactly %d (target %d + %d replacements)", report.DistinctCreates, want, target, report.Replacements))
	}
	if report.Replacements > s.cfg.WPReplacementTolerance {
		problems = append(problems, fmt.Sprintf("%d replacements exceed tolerance %d", report.Replacements, s.cfg.WPReplacementTolerance))
	}
	if report.MaxPoolPeakLive > replicas {
		problems = append(problems, fmt.Sprintf("peak concurrent population exceeded target in %d pool(s): max %d > %d replicas", report.PoolsExceedingTarget, report.MaxPoolPeakLive, replicas))
	}
	if len(problems) > 0 {
		detail := ""
		if len(totals.worstPools) > 0 {
			shown := totals.worstPools
			if len(shown) > 5 {
				shown = shown[:5]
			}
			detail = fmt.Sprintf(" (worst pools: %s)", strings.Join(shown, ", "))
		}
		return fmt.Errorf("[%s#%d] warm pool invariants violated: %s%s", PhaseWarmPoolOvercreate, number, strings.Join(problems, "; "), detail)
	}
	log.Printf("[%s#%d] PASS: exactly %d creates for %d pools x %d replicas (%d tolerated replacements), population never exceeded target",
		PhaseWarmPoolOvercreate, number, report.DistinctCreates, pools, replicas, report.Replacements)
	return nil
}

// waitAllWarmPoolsReady polls the phase's pools until every one reports
// readyReplicas >= want. Progress-stall detection mirrors the fill phase
// (total ready count must advance within PerSandboxTimeout).
func (s *stressTest) waitAllWarmPoolsReady(ctx context.Context, phase PhaseName, number PhaseNumber, want, pools int) error {
	lastReadySum := int64(-1)
	lastProgress := time.Now()
	for {
		list, err := s.warmPoolClient.List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("[%s#%d] failed to list warm pools: %w", phase, number, err)
		}
		readyPools := 0
		readySum := int64(0)
		for i := range list.Items {
			ready, _, _ := unstructured.NestedInt64(list.Items[i].Object, "status", "readyReplicas")
			readySum += ready
			if ready >= int64(want) {
				readyPools++
			}
		}
		if readyPools >= pools {
			return nil
		}
		if readySum != lastReadySum {
			lastReadySum = readySum
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > s.cfg.PerSandboxTimeout {
			return fmt.Errorf("[%s#%d] pools stalled: %d/%d pools Ready (%d total ready replicas) with no progress for %v", phase, number, readyPools, pools, readySum, s.cfg.PerSandboxTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// runWarmPoolUnschedulablePhase creates one pool with an impossible resource
// request and asserts the hold-don't-churn and exactly-one-NotProgressing
// invariants (see the package comment).
func (s *stressTest) runWarmPoolUnschedulablePhase(ctx context.Context, number PhaseNumber) error {
	replicas := s.cfg.WPUnschedReplicas
	window := s.cfg.WPUnschedWatch

	templateID := types.NamespacedName{Name: fmt.Sprintf("p%d-wpu-template", number), Namespace: s.namespace}
	poolID := types.NamespacedName{Name: fmt.Sprintf("p%d-wpu-pool", number), Namespace: s.namespace}

	log.Printf("[%s#%d] creating pool %s with %d unschedulable replicas (cpu request %s); observing for %s (readiness grace 5m + jittered requeue means the NotProgressing event lands between ~5m and ~7m35s)",
		PhaseWarmPoolUnschedulable, number, poolID.Name, replicas, s.cfg.WPUnschedCPU, window)

	acct := newPoolAccountant(s.namespace, []string{poolID.Name}, replicas)
	events := newWarmPoolEventCounter(s.namespace, poolID.Name, warmPoolNotProgressingReason)
	removeAcct := s.tracker.AddObserver(acct.observe)
	removeEvents := s.tracker.AddObserver(events.observe)
	observing := true
	stopObserving := func() {
		if observing {
			removeAcct()
			removeEvents()
			observing = false
		}
	}
	defer stopObserving()

	if _, err := s.templateClient.Create(ctx, buildUnschedulableTemplateObject(templateID, s.cfg.Image, s.cfg.WPUnschedCPU), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("[%s#%d] failed to create sandbox template: %w", PhaseWarmPoolUnschedulable, number, err)
	}
	defer s.cleanupWarmPoolPhase(ctx, PhaseWarmPoolUnschedulable, number, []string{poolID.Name}, templateID, stopObserving)

	start := time.Now()
	if _, err := s.warmPoolClient.Create(ctx, buildWarmPoolObject(poolID, templateID.Name, replicas), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("[%s#%d] failed to create warm pool: %w", PhaseWarmPoolUnschedulable, number, err)
	}

	// The controller should create the members promptly (they just never
	// become Ready); require that before the quiet window starts.
	for acct.totals().distinctCreates < replicas {
		if time.Since(start) > s.cfg.PerSandboxTimeout {
			return fmt.Errorf("[%s#%d] pool created only %d/%d sandboxes within %v", PhaseWarmPoolUnschedulable, number, acct.totals().distinctCreates, replicas, s.cfg.PerSandboxTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	log.Printf("[%s#%d] %d pool sandboxes created; holding quiet until +%s", PhaseWarmPoolUnschedulable, number, replicas, window)

	// Quiet observation window, measured from pool creation. No touches: the
	// controller's self-scheduled post-grace requeue must fire on its own.
	deadline := start.Add(window)
	lastLog := time.Now()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(5*time.Second, time.Until(deadline))):
		}
		if time.Since(lastLog) >= 30*time.Second {
			t := acct.totals()
			n, _ := events.occurrences()
			log.Printf("[%s#%d] +%s: creates=%d deletes=%d notProgressingEvents=%d",
				PhaseWarmPoolUnschedulable, number, time.Since(start).Round(time.Second), t.distinctCreates, t.deletes, n)
			lastLog = time.Now()
		}
	}

	stopObserving()
	totals := acct.totals()
	occurrences, firstSeen := events.occurrences()
	report := &WarmPoolUnschedulableReport{
		Replicas:             replicas,
		CPURequest:           s.cfg.WPUnschedCPU,
		WatchSeconds:         time.Since(start).Seconds(),
		DistinctCreates:      totals.distinctCreates,
		ObservedDeletes:      totals.deletes,
		UIDsStable:           totals.distinctCreates == replicas && totals.deletes == 0,
		NotProgressingEvents: occurrences,
	}
	if !firstSeen.IsZero() {
		off := firstSeen.Sub(start).Seconds()
		report.FirstEventOffsetSeconds = &off
	}
	s.setWarmPoolUnschedulableReport(number, report)

	log.Printf("[%s#%d] observed over %s: %d distinct creates (want %d), %d deletes (want 0), %d NotProgressing event(s) (want exactly 1)",
		PhaseWarmPoolUnschedulable, number, window, report.DistinctCreates, replicas, report.ObservedDeletes, occurrences)

	var problems []string
	if report.ObservedDeletes > 0 {
		problems = append(problems, fmt.Sprintf("%d pool sandbox deletes observed (delete/recreate churn on unschedulable pods)", report.ObservedDeletes))
	}
	if report.DistinctCreates != replicas {
		problems = append(problems, fmt.Sprintf("%d distinct sandbox creates for %d replicas (member UIDs not stable)", report.DistinctCreates, replicas))
	}
	switch {
	case occurrences == 0:
		problems = append(problems, fmt.Sprintf("no %s Warning event within %s (expected exactly one between the 5m readiness grace and ~1.5x it)", warmPoolNotProgressingReason, window))
	case occurrences > 1:
		problems = append(problems, fmt.Sprintf("%d %s Warning occurrences, want exactly 1 (duplicate emission)", occurrences, warmPoolNotProgressingReason))
	}
	if len(problems) > 0 {
		return fmt.Errorf("[%s#%d] warm pool invariants violated: %s", PhaseWarmPoolUnschedulable, number, strings.Join(problems, "; "))
	}
	log.Printf("[%s#%d] PASS: %d unschedulable sandboxes held with stable UIDs, exactly one %s event at +%.0fs",
		PhaseWarmPoolUnschedulable, number, replicas, warmPoolNotProgressingReason, *report.FirstEventOffsetSeconds)
	return nil
}

// cleanupWarmPoolPhase deletes the phase's pools and template, then waits
// (best-effort) for the pool-owned sandboxes to drain so later phases start
// from the same spare capacity. stopObserving is called first so cleanup
// deletes never land in the phase's accounting. Failures are logged, not
// returned; namespace deletion is the backstop.
func (s *stressTest) cleanupWarmPoolPhase(ctx context.Context, phase PhaseName, number PhaseNumber, poolNames []string, templateID types.NamespacedName, stopObserving func()) {
	stopObserving()
	if ctx.Err() != nil {
		// Shutting down; namespace cleanup will remove remaining objects.
		return
	}
	log.Printf("[%s#%d] cleaning up %d pool(s) and template %s", phase, number, len(poolNames), templateID.Name)

	_, _ = ForkJoin(ctx, poolNames, max(s.cfg.CreateConcurrency, 1), func(name string) (struct{}, error) {
		if err := s.warmPoolClient.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("[%s#%d] failed to delete warm pool %s: %v", phase, number, name, err)
		}
		return struct{}{}, nil
	})
	if err := s.templateClient.Delete(ctx, templateID.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("[%s#%d] failed to delete template %s: %v", phase, number, templateID.Name, err)
	}

	s.waitOwnedSandboxesDrained(ctx, phase, number)
}

// setWarmPoolOvercreateReport stores the phase's report for the summary.
// Reports are stored even when the phase fails its assertions, so the
// violation counts land in summary.json alongside the error.
func (s *stressTest) setWarmPoolOvercreateReport(number PhaseNumber, report *WarmPoolOvercreateReport) {
	s.wpMu.Lock()
	defer s.wpMu.Unlock()
	if s.wpOvercreate == nil {
		s.wpOvercreate = make(map[PhaseNumber]*WarmPoolOvercreateReport)
	}
	s.wpOvercreate[number] = report
}

// setWarmPoolUnschedulableReport stores the phase's report for the summary.
func (s *stressTest) setWarmPoolUnschedulableReport(number PhaseNumber, report *WarmPoolUnschedulableReport) {
	s.wpMu.Lock()
	defer s.wpMu.Unlock()
	if s.wpUnschedulable == nil {
		s.wpUnschedulable = make(map[PhaseNumber]*WarmPoolUnschedulableReport)
	}
	s.wpUnschedulable[number] = report
}

// attachWarmPoolReports copies the warm-pool phases' reports onto the
// matching PhaseSummary entries (buildSummary is tracker-driven and knows
// nothing about phase-private results).
func (s *stressTest) attachWarmPoolReports(summary *Summary) {
	s.wpMu.Lock()
	defer s.wpMu.Unlock()
	for _, ps := range summary.Phases {
		if r, ok := s.wpOvercreate[ps.Number]; ok {
			ps.WarmPoolOvercreate = r
		}
		if r, ok := s.wpUnschedulable[ps.Number]; ok {
			ps.WarmPoolUnschedulable = r
		}
	}
}

// warmPoolOvercreatePhase adapts warmpool-overcreate to the Phase interface
// (see phase.go): flag validation up front, sizing against the inspected
// cluster in Resolve, then runWarmPoolOvercreatePhase.
type warmPoolOvercreatePhase struct {
	raw PhaseName

	// Set by Resolve.
	pools    int
	replicas int
}

func (p *warmPoolOvercreatePhase) Name() PhaseName { return p.raw }

func (p *warmPoolOvercreatePhase) Kind() PhaseName { return PhaseWarmPoolOvercreate }

func (p *warmPoolOvercreatePhase) Validate(cfg Config) error {
	if cfg.WPPools <= 0 || cfg.WPReplicas <= 0 {
		return fmt.Errorf("phase %q requires --wp-pools > 0 and --wp-replicas > 0", p.raw)
	}
	if cfg.WPReplacementTolerance < 0 {
		return fmt.Errorf("--wp-replacement-tolerance must be >= 0 for phase %q", p.raw)
	}
	if cfg.CreateConcurrency <= 0 {
		return fmt.Errorf("--create-concurrency must be > 0 for phase %q", p.raw)
	}
	return nil
}

func (p *warmPoolOvercreatePhase) Resolve(cfg Config, _ *ClusterInfo, resident int) (int, int) {
	p.pools, p.replicas = cfg.WPPools, cfg.WPReplicas
	// The phase itself asserts the live population never exceeds
	// pools*replicas, and its cleanup waits for the drain, so nothing
	// accumulates past the phase.
	return resident, resident + p.pools*p.replicas
}

func (p *warmPoolOvercreatePhase) Description() string {
	return fmt.Sprintf("%s: %d pools x %d replicas = %d controller-created sandboxes (invariant gate)",
		p.raw, p.pools, p.replicas, p.pools*p.replicas)
}

func (p *warmPoolOvercreatePhase) Requested() int { return p.pools * p.replicas }

func (p *warmPoolOvercreatePhase) Run(ctx context.Context, test *stressTest, number PhaseNumber) error {
	return test.runWarmPoolOvercreatePhase(ctx, number)
}

// warmPoolUnschedulablePhase adapts warmpool-unschedulable to the Phase
// interface (see phase.go).
type warmPoolUnschedulablePhase struct {
	raw PhaseName

	// Set by Resolve.
	replicas int
}

func (p *warmPoolUnschedulablePhase) Name() PhaseName { return p.raw }

func (p *warmPoolUnschedulablePhase) Kind() PhaseName { return PhaseWarmPoolUnschedulable }

func (p *warmPoolUnschedulablePhase) Validate(cfg Config) error {
	if cfg.WPUnschedReplicas <= 0 {
		return fmt.Errorf("phase %q requires --wp-unsched-replicas > 0", p.raw)
	}
	if cfg.WPUnschedWatch <= 0 {
		return fmt.Errorf("phase %q requires --wp-unsched-watch > 0", p.raw)
	}
	if _, err := resource.ParseQuantity(cfg.WPUnschedCPU); err != nil {
		return fmt.Errorf("--wp-unsched-cpu %q is not a valid quantity: %w", cfg.WPUnschedCPU, err)
	}
	return nil
}

func (p *warmPoolUnschedulablePhase) Resolve(cfg Config, _ *ClusterInfo, resident int) (int, int) {
	p.replicas = cfg.WPUnschedReplicas
	// The pool's pods stay Pending/Unschedulable by design, so the phase
	// consumes no pod slots: it raises neither the resident count nor the
	// peak.
	return resident, resident
}

func (p *warmPoolUnschedulablePhase) Description() string {
	return fmt.Sprintf("%s: one pool with %d permanently unschedulable replicas, quiet watch window (invariant gate)",
		p.raw, p.replicas)
}

func (p *warmPoolUnschedulablePhase) Requested() int { return p.replicas }

func (p *warmPoolUnschedulablePhase) Run(ctx context.Context, test *stressTest, number PhaseNumber) error {
	return test.runWarmPoolUnschedulablePhase(ctx, number)
}
