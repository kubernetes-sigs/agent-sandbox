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
}

// BenchmarkChromeSandboxClaimStartup measures the time for Chrome to start in a sandbox claim.
// Run with: go test -bench=BenchmarkChromeSandboxClaimStartup -benchtime=10x ./test/e2e/...
func BenchmarkChromeSandboxClaimStartup(b *testing.B) {
	// Configuration from environment variables
	warmPoolSize := 6
	if s := os.Getenv("WARM_POOL_SIZE"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			warmPoolSize = v
		}
	}
	parallelism := 3
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
		}
	})
}

func runChromeSandboxClaim(tc *framework.TestContext, namespace, templateName string) *ChromeSandboxClaimMetrics {
	metrics := &ChromeSandboxClaimMetrics{}

	// Unique name for this claim
	claimName := fmt.Sprintf("claim-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&claimCounter, 1))

	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = claimName
	claim.Namespace = namespace
	claim.Spec.TemplateRef.Name = templateName
	claim.Spec.Lifecycle = &extensionsv1alpha1.Lifecycle{
		ShutdownPolicy: extensionsv1alpha1.ShutdownPolicyDelete,
	}

	startTime := time.Now()

	// 1. Create Claim
	// Use a background context for cleanup actions, but the test context for operations
	if err := tc.ClusterClient.Create(tc.Context(), claim); err != nil {
		tc.Logf("Failed to create claim %s: %v", claimName, err)
		return metrics
	}
	metrics.ClaimCreated.Set(time.Since(startTime))
	tc.Logf("Created claim %s", claimName)


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
	tc.Logf("Claim %s is ready", claimName)


	return metrics
}


var claimCounter int64
