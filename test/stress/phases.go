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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// stressTest bundles the shared state used by the test phases.
type stressTest struct {
	cfg           Config
	tracker       *Tracker
	sandboxClient dynamic.ResourceInterface
	namespace     string
}

// buildSandboxObject returns a minimal long-running Sandbox.
// The container sleeps forever; sandboxes are torn down by deletion, so we can
// measure readiness (launch) independently of workload duration.
//
// terminationGracePeriodSeconds=1 keeps deletion fast: as PID 1, sleep does not
// get the default SIGTERM disposition and will only die on SIGKILL after the
// grace period. automountServiceAccountToken is disabled so kubelet does not
// project a token volume (noticeable under high churn).
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
								"command":         []string{"sleep", "infinity"},
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
func (s *stressTest) createSandbox(ctx context.Context, id types.NamespacedName, phase Phase) error {
	sandbox := buildSandboxObject(id, s.cfg.Image)
	s.tracker.Register(id, phase)
	_, err := s.sandboxClient.Create(ctx, sandbox, metav1.CreateOptions{})
	s.tracker.MarkCreateReturned(id, err)
	if err != nil {
		log.Printf("[%s] failed to create sandbox %s: %v", phase, id.Name, err)
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
func (s *stressTest) runFillPhase(ctx context.Context) error {
	count := s.cfg.FillCount
	if count == 0 {
		return nil
	}
	log.Printf("[fill] creating %d background sandboxes (create-concurrency=%d)", count, s.cfg.CreateConcurrency)

	names := make([]types.NamespacedName, 0, count)
	for i := range count {
		names = append(names, types.NamespacedName{Name: fmt.Sprintf("fill-%d", i), Namespace: s.namespace})
	}

	if _, err := ForkJoin(ctx, names, s.cfg.CreateConcurrency, func(id types.NamespacedName) (struct{}, error) {
		// Errors are recorded per-sandbox; do not abort the phase.
		_ = s.createSandbox(ctx, id, PhaseFill)
		return struct{}{}, nil
	}); err != nil {
		return err
	}

	// Wait for all successfully-created fill sandboxes to become Ready.
	// If we stop making progress for PerSandboxTimeout, give up and report.
	lastReady := -1
	lastProgress := time.Now()
	for {
		counts := s.tracker.Snapshot()[PhaseFill]
		if counts.Created == 0 {
			return fmt.Errorf("[fill] all %d sandbox creations failed", counts.Failed)
		}
		if counts.Ready >= counts.Created {
			log.Printf("[fill] all %d created sandboxes are Ready (%d failed to create)",
				counts.Created, counts.Failed)
			return nil
		}
		if counts.Ready != lastReady {
			lastReady = counts.Ready
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > s.cfg.PerSandboxTimeout {
			return fmt.Errorf("[fill] stalled: %d/%d sandboxes Ready with no progress for %v", counts.Ready, counts.Created, s.cfg.PerSandboxTimeout)
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
func (s *stressTest) runProbePhase(ctx context.Context) error {
	count := s.cfg.ProbeCount
	if count == 0 {
		return nil
	}
	log.Printf("[probe] launching %d probe sandboxes (concurrency=%d, interval=%v)", count, s.cfg.ProbeConcurrency, s.cfg.ProbeInterval)

	indices := make([]int, count)
	for i := range indices {
		indices[i] = i
	}

	_, err := ForkJoin(ctx, indices, s.cfg.ProbeConcurrency, func(i int) (struct{}, error) {
		id := types.NamespacedName{Name: fmt.Sprintf("probe-%d", i), Namespace: s.namespace}

		if err := s.createSandbox(ctx, id, PhaseProbe); err == nil {
			if err := s.tracker.WaitReady(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
				s.tracker.MarkError(id, err.Error())
				log.Printf("[probe] %s: %v", id.Name, err)
			}

			// Delete the probe and wait for its Pod to go away, so each probe
			// sees the same background load.
			s.deleteSandbox(ctx, id)
			if err := s.tracker.WaitGone(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
				s.tracker.MarkError(id, err.Error())
				log.Printf("[probe] %s: %v", id.Name, err)
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

// runThroughputPhase measures sustained sandbox launch throughput using a
// closed loop: at most cfg.MaxInFlight sandboxes exist at once, where a slot is
// held from just before Create until the backing Pod is observed deleted.
// This keeps the test below cluster capacity (maxPodsPerNode * nodes), so we
// measure the control plane pipeline rather than queueing for capacity.
//
// Because the slot is held through deletion, the measured rate includes the
// delete -> pod-gone pipeline (~4-5s per sandbox as of 2026-07, even on an
// idle cluster), not just launch. We keep it that way deliberately: capacity
// is only truly recycled once the pod is gone, and churn-heavy agent
// workloads pay this cost too. If we ever want pure launch throughput,
// release the slot at Ready instead and gate creates on a live-pod count.
func (s *stressTest) runThroughputPhase(ctx context.Context) error {
	count := s.cfg.ThroughputCount
	if count == 0 {
		return nil
	}
	log.Printf("[throughput] churning %d sandboxes (max-in-flight=%d, create-concurrency=%d)", count, s.cfg.MaxInFlight, s.cfg.CreateConcurrency)

	if s.cfg.MaxInFlight < 1 {
		return fmt.Errorf("[throughput] invalid max-in-flight=%d (must be >= 1)", s.cfg.MaxInFlight)
	}

	slots := make(chan struct{}, s.cfg.MaxInFlight)
	var lifecycleWG sync.WaitGroup

	names := make([]types.NamespacedName, 0, count)
	for i := range count {
		names = append(names, types.NamespacedName{Name: fmt.Sprintf("tp-%d", i), Namespace: s.namespace})
	}

	if _, err := ForkJoin(ctx, names, s.cfg.CreateConcurrency, func(id types.NamespacedName) (struct{}, error) {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return struct{}{}, nil
		}

		if err := s.createSandbox(ctx, id, PhaseThroughput); err != nil {
			<-slots
			return struct{}{}, nil
		}

		lifecycleWG.Go(func() {
			defer func() { <-slots }()

			if err := s.tracker.WaitReady(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
				s.tracker.MarkError(id, err.Error())
				log.Printf("[throughput] %s: %v", id.Name, err)
			}

			s.deleteSandbox(ctx, id)
			if err := s.tracker.WaitGone(ctx, id, s.cfg.PerSandboxTimeout); err != nil && ctx.Err() == nil {
				s.tracker.MarkError(id, err.Error())
				log.Printf("[throughput] %s: %v", id.Name, err)
			}
		})
		return struct{}{}, nil
	}); err != nil {
		return err
	}

	lifecycleWG.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	counts := s.tracker.Snapshot()[PhaseThroughput]
	log.Printf("[throughput] done: %d created, %d ready, %d failed", counts.Created, counts.Ready, counts.Failed)
	return nil
}
