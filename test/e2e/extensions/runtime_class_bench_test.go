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

package extensions

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

var benchSandboxCounter atomic.Int64

func runtimeClassPodSpec(rcPtr *string, image string) corev1.PodSpec {
	return corev1.PodSpec{
		RuntimeClassName: rcPtr,
		Containers: []corev1.Container{
			{
				Name:            "bench",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
			},
		},
	}
}

func benchImages() []string {
	if v := os.Getenv("SANDBOX_IMAGES"); v != "" {
		var images []string
		for s := range strings.SplitSeq(v, ",") {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				images = append(images, trimmed)
			}
		}
		if len(images) > 0 {
			return images
		}
	}
	return []string{"registry.k8s.io/pause:3.10"}
}

func shortImageName(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

func logBenchHeader(b *testing.B, benchType string, runtimeClass string, poolSizes []int) {
	images := benchImages()
	b.Logf("=======================================================================")
	b.Logf("  Benchmark: %s", benchType)
	b.Logf("  SANDBOX_RUNTIME_CLASS = %s", runtimeClass)
	b.Logf("  SANDBOX_IMAGES        = %s", strings.Join(images, ", "))
	if len(poolSizes) > 0 {
		sizeStrs := make([]string, len(poolSizes))
		for i, s := range poolSizes {
			sizeStrs[i] = strconv.Itoa(s)
		}
		b.Logf("  SANDBOX_POOL_SIZES    = %s", strings.Join(sizeStrs, ", "))
	}
	b.Logf("=======================================================================")
}

// BenchmarkRuntimeClassColdStart measures cold sandbox creation latency per
// image. Each b.Loop() iteration creates a Sandbox directly and waits for Ready.
//
// Run with:
//
//	SANDBOX_RUNTIME_CLASS=default go test -v -run=^$ -bench=BenchmarkRuntimeClassColdStart -benchtime=5x ./test/e2e/extensions/... -timeout 10m
func BenchmarkRuntimeClassColdStart(b *testing.B) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		b.Skip("SANDBOX_RUNTIME_CLASS not set")
	}

	logBenchHeader(b, "ColdStart", runtimeClass, nil)
	rcPtr := runtimeClassPtrFromEnv(runtimeClass)

	for _, image := range benchImages() {
		b.Run(shortImageName(image), func(b *testing.B) {
			podSpec := runtimeClassPodSpec(rcPtr, image)

			var total time.Duration
			var worst time.Duration
			for b.Loop() {
				tc := framework.NewTestContext(b)

				ns := &corev1.Namespace{}
				ns.Name = fmt.Sprintf("bench-cold-%d", time.Now().UnixNano())
				tc.MustCreateWithCleanup(ns)

				sandbox := &sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("cold-%d", benchSandboxCounter.Add(1)),
						Namespace: ns.Name,
					},
				}
				sandbox.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}

				startTime := time.Now()
				tc.MustCreateWithCleanup(sandbox)
				tc.MustWaitForObject(sandbox, predicates.ReadyConditionIsTrue)

				d := time.Since(startTime)
				total += d
				if d > worst {
					worst = d
				}
			}
			b.ReportMetric(total.Seconds()/float64(b.N), "sandbox-ready-sec/op")
			b.ReportMetric(worst.Seconds(), "worst-sec")
		})
	}
}

// BenchmarkRuntimeClassWarmClaim measures warm pool claim latency across
// image × pool-size combinations. The template and pool are created once per
// sub-benchmark; each b.Loop() iteration claims a sandbox from the pool.
//
// Pool size must be >= benchtime count — if claims exhaust the pool the
// controller falls back to cold start, skewing the measurement.
//
// Run with:
//
//	SANDBOX_RUNTIME_CLASS=default go test -v -run=^$ -bench=BenchmarkRuntimeClassWarmClaim -benchtime=3x ./test/e2e/extensions/... -timeout 10m
func BenchmarkRuntimeClassWarmClaim(b *testing.B) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		b.Skip("SANDBOX_RUNTIME_CLASS not set")
	}

	tc0 := framework.NewTestContext(b)
	cluster, err := tc0.ClusterInfo(b.Context())
	if err != nil {
		b.Fatalf("failed to detect cluster info: %v", err)
	}
	poolSizes, err := benchPoolSizes(cluster.TotalCPUCapacity)
	if err != nil {
		b.Fatalf("cannot determine pool sizes: %v", err)
	}
	logBenchHeader(b, "WarmClaim", runtimeClass, poolSizes)
	rcPtr := runtimeClassPtrFromEnv(runtimeClass)

	for _, image := range benchImages() {
		for _, poolSize := range poolSizes {
			name := fmt.Sprintf("%s/pool-%d", shortImageName(image), poolSize)

			b.Run(name, func(b *testing.B) {
				tc := framework.NewTestContext(b)

				if isVMRuntime(runtimeClass) && int64(poolSize) > cluster.TotalCPUCapacity {
					b.Skipf("pool size %d exceeds worker CPU capacity (%d vCPUs) — not practical for VM runtime %q",
						poolSize, cluster.TotalCPUCapacity, runtimeClass)
				}

				ns := &corev1.Namespace{}
				ns.Name = fmt.Sprintf("bench-warm-%d", time.Now().UnixNano())
				tc.MustCreateWithCleanup(ns)

				podSpec := runtimeClassPodSpec(rcPtr, image)

				template := &extensionsv1beta1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "bench-template",
						Namespace: ns.Name,
					},
				}
				template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}
				tc.MustCreateWithCleanup(template)

				replicas := int32(poolSize)
				warmPool := &extensionsv1beta1.SandboxWarmPool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "bench-warmpool",
						Namespace: ns.Name,
					},
					Spec: extensionsv1beta1.SandboxWarmPoolSpec{
						Replicas:    &replicas,
						TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
					},
				}
				tc.MustCreateWithCleanup(warmPool)

				warmPoolID := types.NamespacedName{Name: warmPool.Name, Namespace: ns.Name}
				if err := tc.WaitForWarmPoolReady(b.Context(), warmPoolID); err != nil {
					b.Fatalf("WarmPool failed to become ready: %v", err)
				}
				b.Logf("WarmPool ready with %d replicas", poolSize)

				b.ResetTimer()
				var total time.Duration
				var worst time.Duration
				for b.Loop() {
					claimName := fmt.Sprintf("claim-%d", benchSandboxCounter.Add(1))

					claim := &extensionsv1beta1.SandboxClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name:      claimName,
							Namespace: ns.Name,
						},
						Spec: extensionsv1beta1.SandboxClaimSpec{
							WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
							Lifecycle:   claimLifecycle,
						},
					}

					startTime := time.Now()
					tc.MustCreateWithCleanup(claim)
					tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)

					d := time.Since(startTime)
					total += d
					if d > worst {
						worst = d
					}
				}
				b.ReportMetric(total.Seconds()/float64(b.N), "claim-ready-sec/op")
				b.ReportMetric(worst.Seconds(), "worst-sec")
			})
		}
	}
}
