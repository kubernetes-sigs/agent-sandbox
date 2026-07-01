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

package e2e

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// TestWarmPoolParallelClaim performs a single run of parallel claiming to verify correctness.
func TestWarmPoolParallelClaim(t *testing.T) {
	tc := framework.NewTestContext(t)
	runWarmPoolParallelClaim(tc, 10)
}

// BenchmarkWarmPoolParallelClaim measures parallel claim latency under benchmark loop.
func BenchmarkWarmPoolParallelClaim(b *testing.B) {
	warmPoolSize := 10

	for b.Loop() {
		b.StopTimer() // We'll start the timer when we've done setup.
		tc := framework.NewTestContext(b)
		runWarmPoolParallelClaim(tc, warmPoolSize)

		b.StartTimer() // The benchmark framework expects the timer to be started.
	}
}

// runWarmPoolParallelClaim sets up a WarmPool of the specified size and claims all pods concurrently.
// It accepts optional callbacks for starting and stopping the benchmark timer.
func runWarmPoolParallelClaim(t *framework.TestContext, warmPoolSize int) {
	ctx := t.Context()

	// We can be invoked multiple times; make sure we stop our watches when we exit.
	// We use a 5 minute timeout mostly to keep reviewbots happy.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-parallel-%d", time.Now().UnixNano())
	if err := t.CreateWithCleanup(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "template",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:            "pause",
						Image:           "registry.k8s.io/pause:3.10",
						ImagePullPolicy: corev1.PullIfNotPresent,
					},
				},
			},
		}},
		},
	}
	if err := t.CreateWithCleanup(ctx, template); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	// Watch sandboxwarmpools, just for diagnostic purposes
	go func() {
		gvr := extensionsv1beta1.GroupVersion.WithResource("sandboxwarmpools")
		watchFilter := framework.WatchFilter{
			Namespace: ns.Name,
		}

		framework.MustWatch(ctx, t.ClusterClient, gvr, watchFilter, func(event watch.Event, obj *extensionsv1beta1.SandboxWarmPool) (bool, error) {
			t.Logf("SandboxWarmPool event %s %s/%s", event.Type, obj.Namespace, obj.Name)
			return false, nil
		})
	}()

	// Watch sandboxes, just for diagnostic purposes
	go func() {
		gvr := sandboxv1beta1.GroupVersion.WithResource("sandboxes")
		watchFilter := framework.WatchFilter{
			Namespace: ns.Name,
		}

		framework.MustWatch(ctx, t.ClusterClient, gvr, watchFilter, func(event watch.Event, obj *sandboxv1beta1.Sandbox) (bool, error) {
			t.Logf("Sandbox event %s %s/%s", event.Type, obj.Namespace, obj.Name)
			return false, nil
		})
	}()

	// Watch pods, just for diagnostic purposes
	go func() {
		gvr := corev1.SchemeGroupVersion.WithResource("pods")
		watchFilter := framework.WatchFilter{
			Namespace: ns.Name,
		}

		framework.MustWatch(ctx, t.ClusterClient, gvr, watchFilter, func(event watch.Event, obj *corev1.Pod) (bool, error) {
			t.Logf("Pod event %s %s/%s", event.Type, obj.Namespace, obj.Name)
			return false, nil
		})
	}()

	// Watch sandboxclaims and record timings
	sandboxClaims := NewConcurrentMap[types.NamespacedName, *extensionsv1beta1.SandboxClaim]()
	readyAt := NewConcurrentMap[types.NamespacedName, time.Time]()

	go func() {
		gvr := extensionsv1beta1.GroupVersion.WithResource("sandboxclaims")
		watchFilter := framework.WatchFilter{
			Namespace: ns.Name,
		}

		framework.MustWatch(ctx, t.ClusterClient, gvr, watchFilter, func(event watch.Event, obj *extensionsv1beta1.SandboxClaim) (bool, error) {
			t.Logf("SandboxClaim event %s %s/%s", event.Type, obj.Namespace, obj.Name)

			id := types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}
			sandboxClaims.Put(id, obj)
			if matches, err := predicates.ReadyConditionIsTrue.Matches(obj); matches {
				readyAt.PutIfAbsent(id, time.Now())
			} else if err != nil {
				return false, fmt.Errorf("ReadyConditionIsTrue condition evaluation error: %w", err)
			}

			return false, nil
		})
	}()

	replicas := int32(warmPoolSize)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warmpool",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	if err := t.CreateWithCleanup(ctx, warmPool); err != nil {
		t.Fatalf("failed to create warmpool: %v", err)
	}

	t.Logf("Waiting for WarmPool to be ready with %d replicas...", warmPoolSize)
	if err := t.WaitForWarmPoolReady(ctx, types.NamespacedName{Name: warmPool.Name, Namespace: warmPool.Namespace}); err != nil {
		t.Fatalf("warmpool failed to become ready: %v", err)
	}

	// Start the clock!
	t.Logf("BENCHMARK-START: starting benchmark run [claims=%d]", warmPoolSize)
	t.StartTimer()
	startTime := time.Now()

	// Create the claims (without waiting for them to be bound)
	for i := range warmPoolSize {
		claimName := fmt.Sprintf("claim-%d", i)
		claim := &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      claimName,
				Namespace: ns.Name,
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{
				WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
			},
		}
		if err := t.CreateWithCleanup(ctx, claim); err != nil {
			t.Fatalf("failed to create sandbox claim: %v", err)
		}
	}

	// Wait for all claims to be ready
	for readyAt.Len() != warmPoolSize {
		// Poll until context cancelled; note that this isn't timing critical, so we don't try to be too clever here.
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled before all claims were ready: %v", ctx.Err())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	t.StopTimer()
	t.Logf("BENCHMARK-END: Successfully claimed %d sandboxes", warmPoolSize)

	// Active iteration cleanup to prevent resource leak over multiple benchmark iterations
	if err := t.Delete(ctx, ns); err != nil {
		t.Fatalf("failed to delete namespace %s: %v", ns.Name, err)
	}
	if err := t.WaitForObjectNotFound(ctx, ns); err != nil {
		t.Fatalf("failed waiting for namespace %s deletion: %v", ns.Name, err)
	}

	maxClaimTime := time.Duration(0)
	for _, v := range readyAt.Snapshot() {
		timeUntilReady := v.Sub(startTime)
		if timeUntilReady > maxClaimTime {
			maxClaimTime = timeUntilReady
		}
	}
	t.ReportMetric(float64(maxClaimTime.Seconds()), fmt.Sprintf("sec/claim-%d-sandboxclaims-e2e", warmPoolSize))
}

// ConcurrentMap is a simple thread-safe map.
type ConcurrentMap[K comparable, V any] struct {
	m     map[K]V
	mutex sync.RWMutex
}

// NewConcurrentMap creates a new ConcurrentMap.
func NewConcurrentMap[K comparable, V any]() *ConcurrentMap[K, V] {
	return &ConcurrentMap[K, V]{
		m: make(map[K]V),
	}
}

// Put adds (or overwrites) a key-value pair to the map.
func (m *ConcurrentMap[K, V]) Put(key K, value V) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.m[key] = value
}

// PutIfAbsent adds a key-value pair to the map, only if the key is not found.
// It returns true if the key was added.
func (m *ConcurrentMap[K, V]) PutIfAbsent(key K, value V) bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if _, ok := m.m[key]; ok {
		return false
	}
	m.m[key] = value
	return true
}

// Get returns a value from the map.
func (m *ConcurrentMap[K, V]) Get(key K) (V, bool) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	value, ok := m.m[key]
	return value, ok
}

// Has checks if a key exists in the map.
func (m *ConcurrentMap[K, V]) Has(key K) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	_, ok := m.m[key]
	return ok
}

// Len returns the number of items in the map.
func (m *ConcurrentMap[K, V]) Len() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return len(m.m)
}

// Snapshot returns a copy of the values in the map.
func (m *ConcurrentMap[K, V]) Snapshot() map[K]V {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	return maps.Clone(m.m)
}
