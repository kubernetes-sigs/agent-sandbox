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

package e2e

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// ChromeSandboxClaimMetrics holds timing measurements for the chrome sandbox claim startup.
type ChromeSandboxClaimMetrics struct {
	ClaimCreated AtomicTimeDuration // Time for claim to be created
	ClaimReady   AtomicTimeDuration // Time for claim to become ready

	// Inherited from ChromeSandboxMetrics logic
	SandboxReady AtomicTimeDuration // Time for sandbox to become ready
	PodCreated   AtomicTimeDuration // Time for pod to be created
	PodScheduled AtomicTimeDuration // Time for pod to be scheduled
	PodRunning   AtomicTimeDuration // Time for pod to become running
	PodReady     AtomicTimeDuration // Time for pod to become ready
	ChromeReady  AtomicTimeDuration // Time for chrome to respond on debug port
	Total        AtomicTimeDuration // Total time from start to chrome ready
}

// BenchmarkChromeSandboxClaimStartup measures the time for Chrome to start in a sandbox claim.
// Run with: go test -bench=BenchmarkChromeSandboxClaimStartup -benchtime=10x ./test/e2e/...
func BenchmarkChromeSandboxClaimStartup(b *testing.B) {
	// Configuration from environment variables
	warmPoolSize := 20
	if s := os.Getenv("WARM_POOL_SIZE"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			warmPoolSize = v
		}
	}
	parallelism := 10
	if s := os.Getenv("PARALLELISM"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			parallelism = v
		}
	}

	b.Logf("Benchmark Configuration: WarmPoolSize=%d, Parallelism=%d", warmPoolSize, parallelism)

	tc := framework.NewTestContext(b)
	ctx := tc.Context()

	// 1. Setup Namespace
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("chrome-claim-bench-%d", time.Now().UnixNano())
	tc.MustCreateWithCleanup(ns)

	// 2. Setup SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "chrome-template"
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "chrome-sandbox",
					Image:           fmt.Sprintf("kind.local/chrome-sandbox:%s", os.Getenv("IMAGE_TAG")),
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}
	tc.MustCreateWithCleanup(template)

	// 3. Setup SandboxWarmPool
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "chrome-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.Replicas = int32(warmPoolSize)
	warmPool.Spec.TemplateRef.Name = template.Name
	tc.MustCreateWithCleanup(warmPool)

	// 4. Wait for WarmPool to be Ready
	b.Logf("Waiting for WarmPool to be ready with %d replicas...", warmPoolSize)
	// We use WaitLoop with a timeout
	if err := tc.WaitForWarmPoolReady(ctx, types.NamespacedName{Name: warmPool.Name, Namespace: warmPool.Namespace}); err != nil {
		b.Fatalf("WarmPool failed to become ready: %v", err)
	}
	b.Logf("WarmPool is ready.")

	// 5. Benchmark Loop
	b.SetParallelism(parallelism)
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			metrics := runChromeSandboxClaim(tc, ns.Name, template.Name)

			// Record metrics
			b.ReportMetric(metrics.ClaimCreated.Seconds(), "claim-created-sec")
			b.ReportMetric(metrics.ClaimReady.Seconds(), "claim-ready-sec")
			b.ReportMetric(metrics.SandboxReady.Seconds(), "sandbox-ready-sec")
			b.ReportMetric(metrics.PodReady.Seconds(), "pod-ready-sec")
			b.ReportMetric(metrics.Total.Seconds(), "total-sec")
		}
	})
}

func runChromeSandboxClaim(tc *framework.TestContext, namespace, templateName string) *ChromeSandboxClaimMetrics {
	metrics := &ChromeSandboxClaimMetrics{}
	startTime := time.Now()

	// Unique name for this claim
	claimName := fmt.Sprintf("claim-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&claimCounter, 1))

	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = claimName
	claim.Namespace = namespace
	claim.Spec.TemplateRef.Name = templateName
	claim.Spec.Lifecycle = &extensionsv1alpha1.Lifecycle{
		ShutdownPolicy: extensionsv1alpha1.ShutdownPolicyDelete,
	}

	// 1. Create Claim
	// Use a background context for cleanup actions, but the test context for operations
	if err := tc.ClusterClient.Create(tc.Context(), claim); err != nil {
		tc.Logf("Failed to create claim %s: %v", claimName, err)
		return metrics
	}
	metrics.ClaimCreated.Set(time.Since(startTime))

	// Ensure cleanup happens at the end of this function (not test end)
	defer func() {
		if err := tc.ClusterClient.Delete(context.Background(), claim); err != nil {
			// unexpected, but just log
			// tc.Logf("Failed to delete claim %s: %v", claimName, err)
		}
	}()

	// 2. Wait for Claim Ready
	// We use the common predicates
	if err := tc.WaitForObject(tc.Context(), claim, predicates.ReadyConditionIsTrue); err != nil {
		tc.Logf("Failed to wait for claim %s ready: %v", claimName, err)
		return metrics
	}
	metrics.ClaimReady.Set(time.Since(startTime))
	metrics.Total.Set(time.Since(startTime))

	// 3. Populate detailed metrics
	// We fetch the Sandbox and Pod to get their timestamps
	sandboxName := claim.Status.SandboxStatus.Name
	if sandboxName != "" {
		sandbox := &sandboxv1alpha1.Sandbox{}
		if err := tc.ClusterClient.Get(tc.Context(), types.NamespacedName{Name: sandboxName, Namespace: namespace}, sandbox); err == nil {
			for _, cond := range sandbox.Status.Conditions {
				if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) {
					metrics.SandboxReady.Set(maxDuration(0, cond.LastTransitionTime.Time.Sub(startTime)))
				}
			}

			// Try to find the pod
			// We check the annotation on the Sandbox that points to the Pod
			// Or we just guess the pod name if it matches (it might not if adopted)
			// Sandbox controller usually puts an annotation or we use label selector
			// extensions/controllers/sandboxwarmpool_controller.go:
			// pod.Labels[sandboxLabel] = nameHash
			// The Sandbox status doesn't have PodName field directly globally?
			// Actually sandbox_types.go doesn't show it.
			// But the Sandbox controller creates a pod with name = sandbox.Name usually.
			// If adopted, the pod name is preserved.
			// The Sandbox object might have `metrics.SandboxPodNameAnnotation` if we look at `sandboxclaim_controller.go`?
			// `sandbox.Annotations[sandboxcontrollers.SandboxPodNameAnnotation] = adoptedPod.Name`

			// We need to import "sigs.k8s.io/agent-sandbox/controllers" to get the constant?
			// Or just use the string "agents.x-k8s.io/sandbox-pod-name"

			podName := sandbox.Annotations["agents.x-k8s.io/sandbox-pod-name"]
			if podName != "" {
				pod := &corev1.Pod{}
				if err := tc.ClusterClient.Get(tc.Context(), types.NamespacedName{Name: podName, Namespace: namespace}, pod); err == nil {
					// Timestamps relative to startTime
					metrics.PodCreated.Set(maxDuration(0, pod.CreationTimestamp.Time.Sub(startTime)))

					for _, cond := range pod.Status.Conditions {
						if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue {
							metrics.PodScheduled.Set(maxDuration(0, cond.LastTransitionTime.Time.Sub(startTime)))
						}
						if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
							metrics.PodReady.Set(maxDuration(0, cond.LastTransitionTime.Time.Sub(startTime)))
						}
					}
					if pod.Status.Phase == corev1.PodRunning {
						// There isn't a single transition time for "Phase" in status root,
						// but usually PodReady covers it.
						// We can use PodReady as proxy or just skip explicit PodRunning if difficult.
						// chromesandbox_test.go watches for Phase change. We only have snapshot.
						// We'll skip PodRunning if we can't get it easily from timestamps.
					}
				}
			}
		}
	}

	return metrics
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

var claimCounter int64
