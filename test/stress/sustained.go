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

// claims-warm-sustained: an open-loop, sustained-rate SandboxClaim phase.
//
// Where the other phases either launch cold sandboxes or fire requests as an
// all-at-once burst, this phase models steady-state arrivals: SandboxClaims
// arrive as a Poisson process at --sustained-rate claims/s for
// --sustained-seconds, each claim is deleted --claim-dwell after it becomes
// Ready, and the warm pool controller refills continuously in the
// background. The question it answers is the one a burst cannot: do
// create->Ready latencies HOLD over time, or do they degrade as adoption,
// refill, and teardown churn compound? Degradation shows up directly in the
// rolling per-10s-window p50/p90/p99 (summary.json sustainedWindows, also
// printed in the report).
//
// Pool sizing (per namespace): replicas = ceil(rate/namespaces x
// --sustained-pool-headroom seconds). The pool drains at the arrival rate and
// each consumed sandbox is replaced only after the controller's replenishment
// latency, so the steady-state inventory deficit is rate x refill-latency.
// The headroom must therefore cover the controller's WORST-CASE refill
// latency: any delay the controller applies before replenishing consumed
// sandboxes, plus the cold sandbox launch p99 on the cluster under test. The
// 10s default assumes prompt replenishment; raise it if the controller defers
// refill, or the pool runs dry and claims cold-start (visible as
// window-latency cliffs — a real degradation signal, but of the pool sizing,
// not the adoption path).
//
// Cluster capacity: peak concurrent pods ~= pool total (kept full by refill)
// + rate x (ready-latency + dwell + deletion pipeline) adopted-but-not-yet-
// deleted sandboxes; checkClusterCapacity budgets this.
//
// --sustained-namespaces=N pre-creates N namespaces (<run-ns>-s1..sN), each
// with its own template + pool, and spreads arrivals round-robin across them.
// Namespaces are a natural sharding key, so this makes the phase usable for
// controller sharding experiments: a sharded controller needs arrivals spread
// over multiple namespaces before shards share the load. With N=1 the phase
// runs entirely in the main test namespace.

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// sustainedWindow is the fixed aggregation window for the phase's rolling
// create->Ready latency stats (summary sustainedWindows + live progress logs).
const sustainedWindow = 10 * time.Second

// poissonSchedule yields successive arrival offsets of a homogeneous Poisson
// process with the given rate (events/second): inter-arrival gaps are
// exponentially distributed with mean 1/rate. Offsets are absolute from the
// schedule origin, so a pacing loop that sleeps until start+offset never
// accumulates drift (a late arrival does not delay subsequent ones).
type poissonSchedule struct {
	rate float64
	rng  *rand.Rand
	next time.Duration
}

// newPoissonSchedule returns a schedule for rate events/second (must be > 0).
func newPoissonSchedule(rate float64, rng *rand.Rand) *poissonSchedule {
	return &poissonSchedule{rate: rate, rng: rng}
}

// Next returns the offset of the next arrival from the schedule origin.
// Offsets are strictly increasing.
func (p *poissonSchedule) Next() time.Duration {
	gap := time.Duration(p.rng.ExpFloat64() / p.rate * float64(time.Second))
	// ExpFloat64 can (rarely) round to a zero duration at high rates; keep
	// offsets strictly increasing so arrivals stay ordered.
	if gap <= 0 {
		gap = time.Nanosecond
	}
	p.next += gap
	return p.next
}

// sustainedPoolReplicasPerNamespace returns the SandboxWarmPool replica count
// created in EACH of the phase's namespaces:
// ceil(rate/namespaces x headroom-seconds). See the package comment for the
// sizing rationale.
func sustainedPoolReplicasPerNamespace(cfg Config) int {
	if cfg.SustainedNamespaces < 1 || cfg.SustainedRate <= 0 {
		return 0
	}
	perNS := cfg.SustainedRate / float64(cfg.SustainedNamespaces)
	return int(math.Ceil(perNS * cfg.SustainedPoolHeadroom.Seconds()))
}

// sustainedExpectedClaims is the expected arrival count (rate x duration);
// the Poisson process makes the actual count vary around it.
func sustainedExpectedClaims(cfg Config) int {
	return int(math.Round(cfg.SustainedRate * cfg.SustainedSeconds))
}

// sustainedNamespaceClients bundles the per-namespace resource clients used
// by the sustained phase. Mutating clients come from the (optionally
// sharded) mutate client; see configureCreateConnections.
type sustainedNamespaceClients struct {
	name     string
	template dynamic.ResourceInterface
	pool     dynamic.ResourceInterface
	claim    dynamic.ResourceInterface
}

func (s *stressTest) sustainedClientsFor(namespace string) sustainedNamespaceClients {
	return sustainedNamespaceClients{
		name:     namespace,
		template: s.mutateClient.Resource(gvrSandboxTemplates).Namespace(namespace),
		pool:     s.mutateClient.Resource(gvrSandboxWarmPools).Namespace(namespace),
		claim:    s.mutateClient.Resource(gvrSandboxClaims).Namespace(namespace),
	}
}

// runClaimsWarmSustainedPhase drives the sustained-rate claim workload
// described in the package comment.
func (s *stressTest) runClaimsWarmSustainedPhase(ctx context.Context, number PhaseNumber) error {
	phase := PhaseClaimsWarmSustained
	rate := s.cfg.SustainedRate
	duration := time.Duration(s.cfg.SustainedSeconds * float64(time.Second))
	nsCount := s.cfg.SustainedNamespaces
	poolReplicas := sustainedPoolReplicasPerNamespace(s.cfg)
	expected := sustainedExpectedClaims(s.cfg)

	templateName := fmt.Sprintf("p%d-stemplate", number)
	poolName := fmt.Sprintf("p%d-spool", number)

	// Clean up claims, pools, templates, and extra namespaces even when the
	// phase fails partway (including mid-namespace-creation): later phases
	// assume spare capacity is back. The closure sees the final slice.
	namespaces := make([]sustainedNamespaceClients, 0, nsCount)
	defer func() { s.cleanupSustained(ctx, number, namespaces, poolName, templateName) }()

	// Resolve and (for N>1) pre-create the namespaces.
	if nsCount == 1 {
		namespaces = append(namespaces, s.sustainedClientsFor(s.namespace))
	} else {
		for i := 1; i <= nsCount; i++ {
			name := fmt.Sprintf("%s-s%d", s.namespace, i)
			if err := s.createNamespace(ctx, name); err != nil {
				return fmt.Errorf("[%s#%d] failed to create namespace %s: %w", phase, number, name, err)
			}
			namespaces = append(namespaces, s.sustainedClientsFor(name))
		}
		log.Printf("[%s#%d] pre-created %d namespaces (%s-s1..s%d)", phase, number, nsCount, s.namespace, nsCount)
	}

	// Provision one template + pool per namespace, then wait for every pool
	// to be fully ready: pool provisioning is setup, not measurement.
	log.Printf("[%s#%d] provisioning %d warm pool(s) of %d replicas (rate %.1f/s, headroom %s)",
		phase, number, nsCount, poolReplicas, rate, s.cfg.SustainedPoolHeadroom)
	for _, nsc := range namespaces {
		templateID := types.NamespacedName{Name: templateName, Namespace: nsc.name}
		poolID := types.NamespacedName{Name: poolName, Namespace: nsc.name}
		if _, err := nsc.template.Create(ctx, buildTemplateObject(templateID, s.cfg.Image), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("[%s#%d] failed to create sandbox template in %s: %w", phase, number, nsc.name, err)
		}
		if _, err := nsc.pool.Create(ctx, buildWarmPoolObject(poolID, templateName, poolReplicas), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("[%s#%d] failed to create warm pool in %s: %w", phase, number, nsc.name, err)
		}
	}
	nsIndices := make([]int, len(namespaces))
	for i := range nsIndices {
		nsIndices[i] = i
	}
	if _, err := ForkJoin(ctx, nsIndices, nsCount, func(i int) (struct{}, error) {
		return struct{}{}, s.waitWarmPoolReady(ctx, namespaces[i].pool, phase, poolName, poolReplicas, number)
	}); err != nil {
		return err
	}

	// Profile a mid-run slice: by 10s in, arrivals + refill + teardown all
	// overlap, which is the steady state this phase exists to measure. The
	// controller profile (when enabled) covers the same window so server- and
	// controller-side cost correlate.
	if s.profiler != nil {
		go s.profiler.CaptureCPUProfile(ctx, phase, 10*time.Second, 30*time.Second)
	}
	if s.ctrlProfiler != nil {
		go s.ctrlProfiler.CaptureCPUProfile(ctx, phase, 10*time.Second, 30*time.Second)
	}

	log.Printf("[%s#%d] arrivals: %.1f/s Poisson for %s (~%d claims) across %d namespace(s), dwell %s after Ready",
		phase, number, rate, duration, expected, nsCount, s.cfg.ClaimDwell)

	// Live per-window progress, so degradation is visible while running.
	windowLogCtx, stopWindowLog := context.WithCancel(ctx)
	defer stopWindowLog()
	go s.logSustainedWindows(windowLogCtx, number)

	// Open-loop arrival pacing: sleep until start+offset for each scheduled
	// arrival. Arrivals are never gated on earlier claims completing — that
	// is the difference between measuring a target rate and a closed loop.
	sched := newPoissonSchedule(rate, rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())))
	start := time.Now()
	launched := 0
	var wg sync.WaitGroup
	for {
		offset := sched.Next()
		if offset > duration {
			break
		}
		if wait := time.Until(start.Add(offset)); wait > 0 {
			select {
			case <-ctx.Done():
				wg.Wait()
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		nsc := namespaces[launched%len(namespaces)]
		id := types.NamespacedName{Name: fmt.Sprintf("p%d-sclaim-%d", number, launched), Namespace: nsc.name}
		launched++
		wg.Go(func() {
			s.runSustainedClaimLifecycle(ctx, nsc.claim, id, poolName, number)
		})
	}

	elapsed := time.Since(start)
	log.Printf("[%s#%d] arrival window done: %d claims launched over %s (%.1f/s achieved vs %.1f/s target); draining in-flight lifecycles",
		phase, number, launched, elapsed.Round(time.Millisecond), float64(launched)/elapsed.Seconds(), rate)

	// Drain: every lifecycle is bounded (WaitReady/WaitGone timeouts + dwell).
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	counts := s.tracker.Snapshot()[number]
	if counts.Created == 0 {
		return fmt.Errorf("[%s#%d] all %d claim creations failed", phase, number, counts.Failed)
	}
	log.Printf("[%s#%d] done: %d created, %d ready, %d deleted, %d failed",
		phase, number, counts.Created, counts.Ready, counts.Deleted, counts.Failed)
	return nil
}

// runSustainedClaimLifecycle is one claim's full arc: create -> observed
// Ready -> dwell -> delete -> observed gone. Failures are recorded on the
// record (they surface as Failed in the summary), never aborting the phase.
func (s *stressTest) runSustainedClaimLifecycle(ctx context.Context, claimClient dynamic.ResourceInterface, id types.NamespacedName, poolName string, number PhaseNumber) {
	if err := s.createClaim(ctx, claimClient, id, poolName, PhaseClaimsWarmSustained, number); err != nil {
		return
	}

	if err := s.tracker.WaitReady(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
		s.tracker.MarkError(id, err.Error())
		log.Printf("[%s#%d] %s: %v", PhaseClaimsWarmSustained, number, id.Name, err)
		// Fall through: delete the claim regardless, to keep churn realistic.
	}

	// Dwell: hold the claim after Ready so adopted sandboxes occupy capacity
	// like real workloads do, then release it (delete -> refill continues).
	if s.cfg.ClaimDwell > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.ClaimDwell):
		}
	}
	if ctx.Err() != nil {
		// Shutting down; cleanup/namespace deletion removes the claim.
		return
	}

	s.tracker.MarkDeleteCalled(id)
	if err := claimClient.Delete(ctx, id.Name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			s.tracker.MarkGone(id)
			return
		}
		s.tracker.MarkError(id, fmt.Sprintf("delete failed: %v", err))
		log.Printf("[%s#%d] failed to delete claim %s: %v", PhaseClaimsWarmSustained, number, id.Name, err)
		return
	}
	if err := s.tracker.WaitGone(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
		s.tracker.MarkError(id, err.Error())
		log.Printf("[%s#%d] %s: %v", PhaseClaimsWarmSustained, number, id.Name, err)
	}
}

// logSustainedWindows logs the most recent COMPLETE rolling window's
// create->Ready percentiles every sustainedWindow, so latency degradation is
// visible live rather than only in the post-run summary.
func (s *stressTest) logSustainedWindows(ctx context.Context, number PhaseNumber) {
	ticker := time.NewTicker(sustainedWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var recs []SandboxRecord
		for _, rec := range s.tracker.Records() {
			if rec.PhaseNumber == number {
				recs = append(recs, rec)
			}
		}
		windows := computeWindowedLatencies(recs, sustainedWindow)
		if len(windows) < 2 {
			continue
		}
		// The last window is still filling; report the one before it.
		w := windows[len(windows)-2]
		if w.Arrivals == 0 {
			continue
		}
		line := "no readies yet"
		if w.Latency != nil {
			line = fmt.Sprintf("p50=%s p90=%s p99=%s", formatMs(w.Latency.P50Ms), formatMs(w.Latency.P90Ms), formatMs(w.Latency.P99Ms))
		}
		log.Printf("[%s#%d] window [%3.0fs-%3.0fs): arrivals=%d ready=%d create->Ready %s",
			PhaseClaimsWarmSustained, number, w.StartOffsetSeconds, w.EndOffsetSeconds, w.Arrivals, w.Ready, line)
	}
}

// createNamespace creates the named namespace, tolerating AlreadyExists.
func (s *stressTest) createNamespace(ctx context.Context, name string) error {
	nsObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name": name,
			},
		},
	}
	_, err := s.nsClient.Create(ctx, nsObj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// cleanupSustained removes everything the phase created: leftover claims
// (most were already deleted after their dwell), the pools and templates,
// and — for multi-namespace runs — the extra namespaces themselves. Like the
// probe/throughput deletes, failures are logged rather than returned so
// cleanup problems do not mask the phase's measurement, and the end-of-run
// namespace deletion is the backstop for the main namespace. That backstop
// only covers the main namespace, so when the phase context was cancelled
// (timeout or shutdown) and extra namespaces exist, cleanup still runs — on
// a context detached from the cancelled one — or the -sN namespaces would
// leak.
func (s *stressTest) cleanupSustained(ctx context.Context, number PhaseNumber, namespaces []sustainedNamespaceClients, poolName, templateName string) {
	if len(namespaces) == 0 {
		// The phase failed before any namespace was resolved.
		return
	}
	phase := PhaseClaimsWarmSustained
	if ctx.Err() != nil {
		if len(namespaces) == 1 && namespaces[0].name == s.namespace {
			// Shutting down, and everything lives in the main test
			// namespace: the end-of-run namespace deletion removes it.
			return
		}
		// Shutting down with extra namespaces created outside the main test
		// namespace: no backstop removes those, so clean up anyway on a
		// detached context, bounded like the end-of-run cleanup so a wedged
		// cluster cannot hang shutdown.
		log.Printf("[%s#%d] phase context cancelled; cleaning up extra namespaces on a detached context", phase, number)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
	}

	// Delete every created claim that was never OBSERVED deleted — including
	// ones whose delete was attempted but failed (or whose DELETED event the
	// watch missed), so a failed delete can't leak a claim into later phases.
	// Re-deleting an already-gone claim is just a cheap NotFound.
	var leftovers []types.NamespacedName
	for _, rec := range s.tracker.Records() {
		if rec.PhaseNumber == number && !rec.CreateReturned.IsZero() && rec.SandboxDeleted.IsZero() {
			leftovers = append(leftovers, types.NamespacedName{Name: rec.Name, Namespace: rec.Namespace})
		}
	}
	log.Printf("[%s#%d] cleaning up: %d leftover claims, %d pool(s)/template(s)", phase, number, len(leftovers), len(namespaces))
	_, _ = ForkJoin(ctx, leftovers, max(s.cfg.CreateConcurrency, 1), func(id types.NamespacedName) (struct{}, error) {
		claimClient := s.mutateClient.Resource(gvrSandboxClaims).Namespace(id.Namespace)
		if err := claimClient.Delete(ctx, id.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("[%s#%d] failed to delete claim %s/%s: %v", phase, number, id.Namespace, id.Name, err)
		}
		return struct{}{}, nil
	})

	for _, nsc := range namespaces {
		if err := nsc.pool.Delete(ctx, poolName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("[%s#%d] failed to delete warm pool %s/%s: %v", phase, number, nsc.name, poolName, err)
		}
		if err := nsc.template.Delete(ctx, templateName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("[%s#%d] failed to delete template %s/%s: %v", phase, number, nsc.name, templateName, err)
		}
	}

	// Multi-namespace runs: delete the extra namespaces (cascades sandboxes,
	// pods, and anything the claim deletes missed), then wait for them to go
	// so later phases see restored capacity. Best-effort with a stall bound.
	if len(namespaces) > 1 || namespaces[0].name != s.namespace {
		for _, nsc := range namespaces {
			if err := s.nsClient.Delete(ctx, nsc.name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				log.Printf("[%s#%d] failed to delete namespace %s: %v", phase, number, nsc.name, err)
			}
		}
		s.waitSustainedGone(ctx, number, func() (int, error) {
			remaining := 0
			for _, nsc := range namespaces {
				if _, err := s.nsClient.Get(ctx, nsc.name, metav1.GetOptions{}); err == nil {
					remaining++
				} else if !apierrors.IsNotFound(err) {
					return 0, err
				}
			}
			return remaining, nil
		}, "namespaces")
		return
	}

	// Single-namespace runs: wait for pool/claim-owned sandboxes in the main
	// namespace to disappear so their pods stop occupying capacity that a
	// later phase counts on.
	s.waitSustainedGone(ctx, number, func() (int, error) {
		return s.countOwnedSandboxes(ctx)
	}, "pool/claim-owned sandboxes")
}

// waitSustainedGone polls count() until it reaches zero, with the same
// progress-stall bound the other cleanup paths use. Best-effort: stalls and
// list errors are logged, not returned.
func (s *stressTest) waitSustainedGone(ctx context.Context, number PhaseNumber, count func() (int, error), what string) {
	phase := PhaseClaimsWarmSustained
	lastRemaining := -1
	lastProgress := time.Now()
	for {
		remaining, err := count()
		if err != nil {
			log.Printf("[%s#%d] cleanup progress check failed: %v", phase, number, err)
			return
		}
		if remaining == 0 {
			log.Printf("[%s#%d] cleanup complete", phase, number)
			return
		}
		if remaining != lastRemaining {
			lastRemaining = remaining
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > s.cfg.PerSandboxTimeout {
			log.Printf("[%s#%d] WARNING: %d %s still present after %v; later phases may see reduced spare capacity",
				phase, number, remaining, what, s.cfg.PerSandboxTimeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
