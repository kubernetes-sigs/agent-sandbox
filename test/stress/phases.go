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

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// stressTest bundles the shared state used by the test phases.
type stressTest struct {
	cfg     Config
	tracker *Tracker
	// mutateClient issues all of the harness's mutating requests (creates
	// and deletes). With --client-connections=1 it shares the watch client's
	// single HTTP/2 connection (historical behavior); with N>1 its requests
	// are sharded over N dedicated connections so create bursts neither
	// queue on the ~100-stream per-connection cap nor congest the watches
	// (see clientconns.go).
	mutateClient dynamic.Interface
	// nsClient creates/deletes the extra namespaces of the multi-namespace
	// sustained phase (cluster-scoped).
	nsClient      dynamic.ResourceInterface
	sandboxClient dynamic.ResourceInterface
	// Extensions clients (extensions.agents.x-k8s.io/v1beta1), used by the
	// claims-warm phases. The extensions controller must be deployed
	// (deploy-to-kube --extensions) for those phases to work.
	templateClient dynamic.ResourceInterface
	warmPoolClient dynamic.ResourceInterface
	claimClient    dynamic.ResourceInterface
	namespace      string
	// profiler captures apiserver CPU profiles during throughput levels
	// (nil when --profile-apiserver is false).
	profiler *apiserverProfiler
	// ctrlProfiler captures controller CPU/heap profiles during the
	// claims-warm burst (nil when --profile-controller is false or the
	// phase does not run).
	ctrlProfiler *controllerProfiler
}

// buildSandboxObject returns a minimal long-running Sandbox.
// The container sleeps forever; sandboxes are torn down by deletion, so we can
// measure readiness (launch) independently of workload duration.
//
// The command traps SIGTERM and exits immediately: a bare `sleep` as PID 1
// gets no default SIGTERM disposition, so the kubelet would wait out the full
// grace period and SIGKILL (observed as exit code 137 and ~1s of extra
// deletion latency). The `& wait` is required because sh does not run traps
// while a foreground child is running. terminationGracePeriodSeconds=1 is the
// backstop if the trap fails. automountServiceAccountToken is disabled so
// kubelet does not project a token volume (noticeable under high churn).
func buildSandboxObject(id types.NamespacedName, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agents.x-k8s.io/v1beta1",
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":      id.Name,
				"namespace": id.Namespace,
			},
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"spec": map[string]any{
						"restartPolicy":                 "Never",
						"terminationGracePeriodSeconds": int64(1),
						"automountServiceAccountToken":  false,
						"containers": []any{
							map[string]any{
								"name":            "main",
								"image":           image,
								"imagePullPolicy": "IfNotPresent",
								"command":         []string{"sh", "-c", "trap 'exit 0' TERM INT; sleep infinity & wait"},
							},
						},
					},
				},
			},
		},
	}
}

// createSandbox registers a record and issues the Create call.
// Create errors are recorded on the SandboxRecord rather than returned:
// individual failures should not abort the run, they are reported in the summary.
func (s *stressTest) createSandbox(ctx context.Context, id types.NamespacedName, name Phase, number PhaseNumber) error {
	sandbox := buildSandboxObject(id, s.cfg.Image)
	s.tracker.Register(id, name, number)
	_, err := s.sandboxClient.Create(ctx, sandbox, metav1.CreateOptions{})
	s.tracker.MarkCreateReturned(id, err)
	if err != nil {
		log.Printf("[%s] failed to create sandbox %s: %v", name, id.Name, err)
	}
	return err
}

// deleteSandbox issues the Delete call and records the timestamp.
func (s *stressTest) deleteSandbox(ctx context.Context, id types.NamespacedName) {
	if ctx.Err() != nil {
		// Shutting down; namespace cleanup will remove remaining sandboxes.
		return
	}
	s.tracker.MarkDeleteCalled(id)
	if err := s.sandboxClient.Delete(ctx, id.Name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			s.tracker.MarkGone(id)
			return
		}
		s.tracker.MarkError(id, fmt.Sprintf("delete failed: %v", err))
		log.Printf("failed to delete sandbox %s: %v", id.Name, err)
	}
}

// runFillPhase creates cfg.FillCount long-running sandboxes and waits for all of
// them to become Ready. These stay running for the rest of the test, so the
// probe and throughput phases measure performance on a cluster at scale.
func (s *stressTest) runFillPhase(ctx context.Context, number PhaseNumber) error {
	count := s.cfg.FillCount
	if count == 0 {
		return nil
	}
	log.Printf("[fill#%d] creating %d background sandboxes (create-concurrency=%d)", number, count, s.cfg.CreateConcurrency)

	names := make([]types.NamespacedName, 0, count)
	for i := range count {
		// Include phase number so repeated fill entries do not collide on name.
		names = append(names, types.NamespacedName{Name: fmt.Sprintf("p%d-fill-%d", number, i), Namespace: s.namespace})
	}

	if _, err := ForkJoin(ctx, names, s.cfg.CreateConcurrency, func(id types.NamespacedName) (struct{}, error) {
		// Errors are recorded per-sandbox; do not abort the phase.
		_ = s.createSandbox(ctx, id, PhaseFill, number)
		return struct{}{}, nil
	}); err != nil {
		return err
	}

	// Wait for all successfully-created fill sandboxes to become Ready.
	// If we stop making progress for PerSandboxTimeout, give up and report.
	lastReady := -1
	lastProgress := time.Now()
	for {
		counts := s.tracker.Snapshot()[number]
		if counts.Created == 0 {
			return fmt.Errorf("[fill#%d] all %d sandbox creations failed", number, counts.Failed)
		}
		if counts.Ready >= counts.Created {
			log.Printf("[fill#%d] all %d created sandboxes are Ready (%d failed to create)",
				number, counts.Created, counts.Failed)
			return nil
		}
		if counts.Ready != lastReady {
			lastReady = counts.Ready
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > s.cfg.PerSandboxTimeout {
			return fmt.Errorf("[fill#%d] stalled: %d/%d sandboxes Ready with no progress for %v", number, counts.Ready, counts.Created, s.cfg.PerSandboxTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// runProbePhase measures clean launch latency at the current cluster scale.
// Probes run at low concurrency (default 1) so they never queue on cluster
// capacity or on each other; each probe is deleted once measured so the
// background scale stays constant.
func (s *stressTest) runProbePhase(ctx context.Context, number PhaseNumber) error {
	count := s.cfg.ProbeCount
	if count == 0 {
		return nil
	}
	log.Printf("[probe#%d] launching %d probe sandboxes (concurrency=%d, interval=%v)", number, count, s.cfg.ProbeConcurrency, s.cfg.ProbeInterval)

	indices := make([]int, count)
	for i := range indices {
		indices[i] = i
	}

	_, err := ForkJoin(ctx, indices, s.cfg.ProbeConcurrency, func(i int) (struct{}, error) {
		id := types.NamespacedName{Name: fmt.Sprintf("p%d-probe-%d", number, i), Namespace: s.namespace}

		if err := s.createSandbox(ctx, id, PhaseProbe, number); err == nil {
			if err := s.tracker.WaitReady(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
				s.tracker.MarkError(id, err.Error())
				log.Printf("[probe#%d] %s: %v", number, id.Name, err)
			}

			// Delete the probe and wait for its Pod to go away, so each probe
			// sees the same background load.
			s.deleteSandbox(ctx, id)
			if err := s.tracker.WaitGone(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
				s.tracker.MarkError(id, err.Error())
				log.Printf("[probe#%d] %s: %v", number, id.Name, err)
			}
		}

		if s.cfg.ProbeInterval > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(s.cfg.ProbeInterval):
			}
		}
		return struct{}{}, nil
	})
	return err
}

// runThroughputLevel measures sustained sandbox launch throughput for one
// closed-loop churn level at the given max-in-flight cap: at most maxInFlight
// sandboxes exist at once, where a slot is held from just before Create until
// the backing Pod is observed deleted. This keeps the test below cluster
// capacity (maxPodsPerNode * nodes), so we measure the control plane pipeline
// rather than queueing for capacity.
//
// Because the slot is held through deletion, the measured rate includes the
// delete -> pod-gone pipeline (~4-5s per sandbox as of 2026-07, even on an
// idle cluster), not just launch. We keep it that way deliberately: capacity
// is only truly recycled once the pod is gone, and churn-heavy agent
// workloads pay this cost too. If we ever want pure launch throughput,
// release the slot at Ready instead and gate creates on a live-pod count.
//
// Multiple levels run back-to-back as separate phases (a max-in-flight sweep
// within a single run): each level fully drains (every pod observed deleted)
// before the next begins, so levels do not contaminate each other.
func (s *stressTest) runThroughputLevel(ctx context.Context, name Phase, number PhaseNumber, maxInFlight int) error {
	count := s.cfg.ThroughputCount
	if count == 0 {
		return nil
	}
	minDuration := time.Duration(s.cfg.ThroughputMinSeconds * float64(time.Second))
	log.Printf("[%s#%d] churning >=%d sandboxes for >=%s (max-in-flight=%d, create-concurrency=%d)", name, number, count, minDuration, maxInFlight, s.cfg.CreateConcurrency)

	if maxInFlight < 1 {
		return fmt.Errorf("[%s#%d] invalid max-in-flight=%d (must be >= 1)", name, number, maxInFlight)
	}

	slots := make(chan struct{}, maxInFlight)
	var lifecycleWG sync.WaitGroup

	// A level ends only once BOTH -throughput-count sandboxes have been
	// churned AND -throughput-min-seconds has elapsed. A fixed count alone
	// made high-rate levels too short for stable throughput samples and for
	// correlating with any side-car time series. Because both conditions are
	// monotonic and indices are handed out in order, the created names are
	// always a contiguous p<num>-tp<mif>-[0..n) range.
	start := time.Now()
	var nextIndex atomic.Int64
	var createWG sync.WaitGroup
	for range s.cfg.CreateConcurrency {
		createWG.Go(func() {
			for ctx.Err() == nil {
				i := int(nextIndex.Add(1)) - 1
				if i >= count && time.Since(start) >= minDuration {
					return
				}
				id := types.NamespacedName{Name: fmt.Sprintf("p%d-tp%d-%d", number, maxInFlight, i), Namespace: s.namespace}

				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}

				if err := s.createSandbox(ctx, id, name, number); err != nil {
					<-slots
					continue
				}

				lifecycleWG.Go(func() {
					defer func() { <-slots }()

					if err := s.tracker.WaitReady(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
						s.tracker.MarkError(id, err.Error())
						log.Printf("[%s#%d] %s: %v", name, number, id.Name, err)
					}

					s.deleteSandbox(ctx, id)
					if err := s.tracker.WaitGone(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
						s.tracker.MarkError(id, err.Error())
						log.Printf("[%s#%d] %s: %v", name, number, id.Name, err)
					}
				})
			}
		})
	}
	createWG.Wait()

	lifecycleWG.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	counts := s.tracker.Snapshot()[number]
	log.Printf("[%s#%d] done: %d created, %d ready, %d failed", name, number, counts.Created, counts.Ready, counts.Failed)
	return nil
}

// extensionsGroupVersion is the API group/version of the SandboxTemplate,
// SandboxWarmPool, and SandboxClaim extension resources.
const extensionsGroupVersion = "extensions.agents.x-k8s.io/v1beta1"

// buildTemplateObject returns a minimal SandboxTemplate wrapping the same
// long-running pod spec used by the raw-sandbox phases (see buildSandboxObject
// for why the command traps SIGTERM and the token mount is disabled).
func buildTemplateObject(id types.NamespacedName, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": extensionsGroupVersion,
			"kind":       "SandboxTemplate",
			"metadata": map[string]any{
				"name":      id.Name,
				"namespace": id.Namespace,
			},
			"spec": map[string]any{
				// Explicitly service-free: no per-sandbox headless Service,
				// so pool churn costs no Service/EndpointSlice writes. This
				// pins the current default (unset spec.service also skips
				// creation) so the benchmark stays service-free even if the
				// field's defaulting ever changes.
				"service": false,
				"podTemplate": map[string]any{
					"spec": map[string]any{
						"restartPolicy":                 "Never",
						"terminationGracePeriodSeconds": int64(1),
						"automountServiceAccountToken":  false,
						"containers": []any{
							map[string]any{
								"name":            "main",
								"image":           image,
								"imagePullPolicy": "IfNotPresent",
								"command":         []string{"sh", "-c", "trap 'exit 0' TERM INT; sleep infinity & wait"},
							},
						},
					},
				},
			},
		},
	}
}

// buildWarmPoolObject returns a SandboxWarmPool of the given size backed by
// the given SandboxTemplate.
func buildWarmPoolObject(id types.NamespacedName, templateName string, replicas int) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": extensionsGroupVersion,
			"kind":       "SandboxWarmPool",
			"metadata": map[string]any{
				"name":      id.Name,
				"namespace": id.Namespace,
			},
			"spec": map[string]any{
				"replicas": int64(replicas),
				"sandboxTemplateRef": map[string]any{
					"name": templateName,
				},
			},
		},
	}
}

// buildClaimObject returns a SandboxClaim against the given warm pool.
func buildClaimObject(id types.NamespacedName, poolName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": extensionsGroupVersion,
			"kind":       "SandboxClaim",
			"metadata": map[string]any{
				"name":      id.Name,
				"namespace": id.Namespace,
			},
			"spec": map[string]any{
				"warmPoolRef": map[string]any{
					"name": poolName,
				},
			},
		},
	}
}

// createClaim registers a record and issues the SandboxClaim Create call via
// the given (namespace-bound) client. Like createSandbox, Create errors are
// recorded on the record rather than aborting the phase.
func (s *stressTest) createClaim(ctx context.Context, claimClient dynamic.ResourceInterface, id types.NamespacedName, poolName string, phase Phase, number PhaseNumber) error {
	claim := buildClaimObject(id, poolName)
	s.tracker.RegisterClaim(id, phase, number)
	_, err := claimClient.Create(ctx, claim, metav1.CreateOptions{})
	s.tracker.MarkCreateReturned(id, err)
	if err != nil {
		log.Printf("[%s] failed to create claim %s: %v", phase, id.Name, err)
	}
	return err
}

// waitWarmPoolReady polls the pool (via the given namespace-bound client)
// until status.readyReplicas >= want, so the claim phases measure binding
// latency against a fully provisioned pool rather than sandbox launch
// latency. Progress-stall detection mirrors the fill phase: if readyReplicas
// stops advancing for PerSandboxTimeout, fail.
func (s *stressTest) waitWarmPoolReady(ctx context.Context, poolClient dynamic.ResourceInterface, phase Phase, poolName string, want int, number PhaseNumber) error {
	lastReady := int64(-1)
	lastProgress := time.Now()
	for {
		pool, err := poolClient.Get(ctx, poolName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("[%s#%d] failed to get warm pool %s: %w", phase, number, poolName, err)
		}
		ready, _, err := unstructured.NestedInt64(pool.Object, "status", "readyReplicas")
		if err != nil {
			return fmt.Errorf("[%s#%d] failed to read warm pool status: %w", phase, number, err)
		}
		if ready >= int64(want) {
			log.Printf("[%s#%d] warm pool %s ready: %d/%d replicas", phase, number, poolName, ready, want)
			return nil
		}
		if ready != lastReady {
			lastReady = ready
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > s.cfg.PerSandboxTimeout {
			return fmt.Errorf("[%s#%d] warm pool stalled: %d/%d replicas ready with no progress for %v", phase, number, ready, want, s.cfg.PerSandboxTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// runClaimsWarmPhase measures SandboxClaim -> Ready latency against a fully
// provisioned warm pool: it creates a SandboxTemplate and a SandboxWarmPool of
// ClaimsWarmCount replicas, waits for every replica to be Ready (pool
// provisioning is setup, not measurement), then fires ClaimsWarmCount claims
// as simultaneously as possible (one goroutine per claim, released together)
// and waits for every claim to be observed Ready.
//
// Metrics: the summary's CreateAck stats isolate the claim Create call
// (apiserver write path) from EndToEndReady (create -> claim Ready=True by
// watch); TimeToAllReadySeconds is first-create -> last-ready. Each record
// also carries the claim Ready condition's lastTransitionTime
// (serverSandboxReady in sandboxes.jsonl) as a server-side cross-check.
//
// Capacity: the pool itself needs ClaimsWarmCount pod slots, and the pool
// controller replenishes claimed sandboxes, so up to ~2x ClaimsWarmCount pods
// can transiently exist during and after the burst. Size the cluster with
// headroom (see checkClusterCapacity's warning) or throttle replenishment via
// --sandbox-warm-pool-concurrent-workers.
func (s *stressTest) runClaimsWarmPhase(ctx context.Context, number PhaseNumber) error {
	count := s.cfg.ClaimsWarmCount
	if count == 0 {
		return nil
	}

	templateID := types.NamespacedName{Name: fmt.Sprintf("p%d-claims-template", number), Namespace: s.namespace}
	poolID := types.NamespacedName{Name: fmt.Sprintf("p%d-claims-pool", number), Namespace: s.namespace}

	log.Printf("[%s#%d] provisioning warm pool %s with %d replicas", PhaseClaimsWarm, number, poolID.Name, count)
	if _, err := s.templateClient.Create(ctx, buildTemplateObject(templateID, s.cfg.Image), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("[%s#%d] failed to create sandbox template: %w", PhaseClaimsWarm, number, err)
	}
	if _, err := s.warmPoolClient.Create(ctx, buildWarmPoolObject(poolID, templateID.Name, count), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("[%s#%d] failed to create warm pool: %w", PhaseClaimsWarm, number, err)
	}

	ids := make([]types.NamespacedName, 0, count)
	for i := range count {
		ids = append(ids, types.NamespacedName{Name: fmt.Sprintf("p%d-claim-%d", number, i), Namespace: s.namespace})
	}

	// Clean up the claims, the pool, and the template even when the phase
	// fails partway: later phases assume the cluster's spare capacity is back.
	defer s.cleanupClaimsWarm(ctx, number, ids, poolID, templateID)

	if err := s.waitWarmPoolReady(ctx, s.warmPoolClient, PhaseClaimsWarm, poolID.Name, count, number); err != nil {
		return err
	}

	// Fire all claims at once: one goroutine per claim, all released by a
	// single channel close so the creates hit the apiserver as close to
	// simultaneously as the client allows (rate limiting is disabled).
	log.Printf("[%s#%d] firing %d claims simultaneously", PhaseClaimsWarm, number, count)

	// Profile the burst itself, not the aftermath: the controller CPU
	// profile window opens right as claim creation begins (the adoption
	// transaction is over within the first seconds), with heap snapshots
	// bracketing the burst. The apiserver profile runs over the same window
	// to correlate server-side cost. All best-effort. The phase can finish
	// well inside the 15s profile window (that is the goal), so the deferred
	// Wait keeps the process alive until the profiles are written.
	var profileWG sync.WaitGroup
	defer profileWG.Wait()
	if s.ctrlProfiler != nil {
		profileWG.Go(func() { s.ctrlProfiler.CaptureCPUProfile(ctx, PhaseClaimsWarm, 0, 15*time.Second) })
		profileWG.Go(func() { s.ctrlProfiler.CaptureHeapProfile(ctx, PhaseClaimsWarm, "burst-start") })
	}
	if s.profiler != nil {
		profileWG.Go(func() { s.profiler.CaptureCPUProfile(ctx, PhaseClaimsWarm, 0, 15*time.Second) })
	}

	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			<-release
			// Errors are recorded per-claim; do not abort the phase.
			_ = s.createClaim(ctx, s.claimClient, id, poolID.Name, PhaseClaimsWarm, number)
		})
	}
	close(release)
	wg.Wait()

	// Wait for all successfully-created claims to be observed Ready, with
	// the same progress-stall detection as the fill phase.
	lastReady := -1
	lastProgress := time.Now()
	for {
		counts := s.tracker.Snapshot()[number]
		if counts.Created == 0 {
			return fmt.Errorf("[%s#%d] all %d claim creations failed", PhaseClaimsWarm, number, counts.Failed)
		}
		if counts.Ready >= counts.Created {
			log.Printf("[%s#%d] all %d created claims are Ready (%d failed to create)",
				PhaseClaimsWarm, number, counts.Created, counts.Failed)
			if s.ctrlProfiler != nil {
				// Post-burst heap snapshot: diff against burst-start to see
				// what the adoption path allocated/retained.
				s.ctrlProfiler.CaptureHeapProfile(ctx, PhaseClaimsWarm, "burst-end")
			}
			return nil
		}
		if counts.Ready != lastReady {
			lastReady = counts.Ready
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > s.cfg.PerSandboxTimeout {
			return fmt.Errorf("[%s#%d] stalled: %d/%d claims Ready with no progress for %v", PhaseClaimsWarm, number, counts.Ready, counts.Created, s.cfg.PerSandboxTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// cleanupClaimsWarm deletes the claims, the pool, and the template, then
// waits (best-effort) for the pool- and claim-owned sandboxes to go away so
// later phases start from the same spare capacity. Failures are logged, not
// returned: like probe/throughput deletes, cleanup problems should not mask
// the phase's measurement, and namespace deletion is the backstop.
func (s *stressTest) cleanupClaimsWarm(ctx context.Context, number PhaseNumber, ids []types.NamespacedName, poolID, templateID types.NamespacedName) {
	if ctx.Err() != nil {
		// Shutting down; namespace cleanup will remove remaining objects.
		return
	}
	log.Printf("[%s#%d] cleaning up %d claims, pool %s, template %s", PhaseClaimsWarm, number, len(ids), poolID.Name, templateID.Name)

	_, _ = ForkJoin(ctx, ids, max(s.cfg.CreateConcurrency, 1), func(id types.NamespacedName) (struct{}, error) {
		if err := s.claimClient.Delete(ctx, id.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("[%s#%d] failed to delete claim %s: %v", PhaseClaimsWarm, number, id.Name, err)
		}
		return struct{}{}, nil
	})

	if err := s.warmPoolClient.Delete(ctx, poolID.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("[%s#%d] failed to delete warm pool %s: %v", PhaseClaimsWarm, number, poolID.Name, err)
	}
	if err := s.templateClient.Delete(ctx, templateID.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("[%s#%d] failed to delete template %s: %v", PhaseClaimsWarm, number, templateID.Name, err)
	}

	// Wait for the sandboxes owned by the pool or the claims to be deleted,
	// so their pods stop occupying capacity that a later phase counts on.
	// Best-effort: on timeout we log and move on.
	lastRemaining := -1
	lastProgress := time.Now()
	for {
		remaining, err := s.countOwnedSandboxes(ctx)
		if err != nil {
			log.Printf("[%s#%d] failed to list sandboxes during cleanup: %v", PhaseClaimsWarm, number, err)
			return
		}
		if remaining == 0 {
			log.Printf("[%s#%d] cleanup complete", PhaseClaimsWarm, number)
			return
		}
		if remaining != lastRemaining {
			lastRemaining = remaining
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > s.cfg.PerSandboxTimeout {
			log.Printf("[%s#%d] WARNING: %d pool/claim-owned sandboxes still present after %v; later phases may see reduced spare capacity", PhaseClaimsWarm, number, remaining, s.cfg.PerSandboxTimeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// countOwnedSandboxes counts sandboxes in the test namespace owned by a
// SandboxWarmPool or SandboxClaim. Fill-phase sandboxes have no owner and are
// excluded: they are supposed to stay up.
func (s *stressTest) countOwnedSandboxes(ctx context.Context) (int, error) {
	list, err := s.sandboxClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range list.Items {
		for _, ref := range list.Items[i].GetOwnerReferences() {
			if ref.Kind == "SandboxWarmPool" || ref.Kind == "SandboxClaim" {
				count++
				break
			}
		}
	}
	return count, nil
}
