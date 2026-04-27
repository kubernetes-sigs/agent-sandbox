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
	"fmt"
	"os"
	"strconv"
	"sync"
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
	ClaimReady AtomicTimeDuration // Time for claim to become ready
}

// BenchmarkChromeSandboxClaimStartup measures the time for Chrome to start in a sandbox claim.
// Run with: go test -bench=BenchmarkChromeSandboxClaimStartup -benchtime=10x ./test/e2e/...
// To add parallelism, use the -cpu flag (e.g., -cpu=1,2,4).
// Make sure that WARM_POOL_SIZE is set appropriately to account for the number of parallel
// test iterations.
func BenchmarkChromeSandboxClaimStartup(b *testing.B) {
	// Configuration from environment variables
	warmPoolSize := 10
	if s := os.Getenv("WARM_POOL_SIZE"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			warmPoolSize = v
		}
	}
	b.Logf("Benchmark Configuration: WarmPoolSize=%d", warmPoolSize)

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
	imageName := chromeSandboxImageName()
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "chrome-sandbox",
					Image:           imageName,
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
	var (
		mu                 sync.Mutex
		totalClaimReadySec float64
		totalClaims        int
	)

	// 5. Benchmark Loop
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			metrics := runChromeSandboxClaim(tc, ns.Name, template.Name)

			mu.Lock()
			totalClaimReadySec += metrics.ClaimReady.Seconds()
			totalClaims++
			mu.Unlock()
		}
	})

	if totalClaims > 0 {
		b.ReportMetric(totalClaimReadySec/float64(totalClaims), "claim-ready-sec/op")
	}
}

func runChromeSandboxClaim(tc *framework.TestContext, namespace, templateName string) *ChromeSandboxClaimMetrics {
	metrics := &ChromeSandboxClaimMetrics{}

	// Unique name for this claim
	claimName := fmt.Sprintf("claim-%d-%d", time.Now().UnixNano(), claimCounter.Add(1))

	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = claimName
	claim.Namespace = namespace
	claim.Spec.TemplateRef.Name = templateName
	claim.Spec.Lifecycle = &extensionsv1alpha1.Lifecycle{
		ShutdownPolicy: extensionsv1alpha1.ShutdownPolicyDelete,
	}

	startTime := time.Now()

	// 1. Create Claim
	// This will automatically delete the claim at the end of the test.
	tc.MustCreateWithCleanup(claim)
	tc.Logf("Created claim %s", claimName)

	// 2. Wait for Claim Ready
	// We use the common predicates
	tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)

	metrics.ClaimReady.Set(time.Since(startTime))
	tc.Logf("Claim %s is ready", claimName)

	return metrics
}

var claimCounter atomic.Int64
