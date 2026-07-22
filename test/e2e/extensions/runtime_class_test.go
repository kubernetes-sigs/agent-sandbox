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
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runtimeClassPtrFromEnv returns a pointer to the SANDBOX_RUNTIME_CLASS value
// for use in PodSpec.RuntimeClassName. "default" means the cluster's default
// runtime (runc on most clusters) — returns nil so RuntimeClassName is unset.
func isVMRuntime(runtimeClass string) bool {
	return strings.HasPrefix(runtimeClass, "kata")
}

func runtimeClassPtrFromEnv(value string) *string {
	if value == "default" {
		return nil
	}
	return &value
}

// TestRuntimeClassLifecycle validates the full SandboxTemplate → WarmPool →
// SandboxClaim lifecycle with a caller-specified RuntimeClassName.
//
// Set SANDBOX_RUNTIME_CLASS to the desired RuntimeClass name (e.g. gvisor,
// kata-qemu, kata-clh). Use "default" for the cluster's default runtime
// (leaves RuntimeClassName unset). The test is skipped when the variable is
// unset, so existing CI is unaffected.
func TestRuntimeClassLifecycle(t *testing.T) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		t.Skip("SANDBOX_RUNTIME_CLASS not set — skipping runtime class lifecycle test")
	}

	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("runtime-class-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// SandboxTemplate with the requested RuntimeClassName.
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-template",
			Namespace: ns.Name,
		},
	}
	rcPtr := runtimeClassPtrFromEnv(runtimeClass)
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
		Spec: corev1.PodSpec{
			RuntimeClassName: rcPtr,
			Containers: []corev1.Container{
				{
					Name:            "pause",
					Image:           "registry.k8s.io/pause:3.10",
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// WarmPool with a single replica.
	replicas := int32(1)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-warmpool",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	warmPoolID := types.NamespacedName{Name: warmPool.Name, Namespace: ns.Name}
	t.Logf("Waiting for WarmPool to be ready (runtimeClass=%s)...", runtimeClass)
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), warmPoolID))

	// Find the sandbox created by the warm pool.
	var poolSandbox *sandboxv1beta1.Sandbox
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1beta1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for i := range sandboxList.Items {
			sb := &sandboxList.Items[i]
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(sb, warmPool) {
				poolSandbox = sb
				return true
			}
		}
		return false
	}, defaultTestTimeout, defaultPollingInterval, "expected to find a pool sandbox")

	// Verify the sandbox's PodTemplate carries the RuntimeClassName.
	require.Equal(t, rcPtr, poolSandbox.Spec.PodTemplate.Spec.RuntimeClassName,
		"Sandbox RuntimeClassName should match requested value")

	// Verify the underlying pod exists, is Ready, and has the RuntimeClassName.
	sandboxID := types.NamespacedName{Name: poolSandbox.Name, Namespace: ns.Name}
	require.NoError(t, tc.WaitForSandboxReady(t.Context(), sandboxID))

	pod := &corev1.Pod{}
	pod.Name = poolSandbox.Name
	pod.Namespace = ns.Name
	tc.MustWaitForObject(pod, predicates.ReadyConditionIsTrue)
	require.Equal(t, rcPtr, pod.Spec.RuntimeClassName,
		"Pod RuntimeClassName should match requested value")

	// Claim a sandbox from the warm pool.
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	t.Logf("Waiting for SandboxClaim to be ready...")
	tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)

	t.Logf("RuntimeClass %q lifecycle test passed", runtimeClass)
}

// TestRuntimeClassStartupComparison measures the difference between creating a
// sandbox from scratch (cold start) and claiming one from a pre-warmed pool.
// Both use the RuntimeClassName from the SANDBOX_RUNTIME_CLASS env var.
//
// Run with:
//
//	SANDBOX_RUNTIME_CLASS=gvisor go test ./test/e2e/extensions/... -run TestRuntimeClassStartupComparison -v -timeout 5m
func TestRuntimeClassStartupComparison(t *testing.T) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		t.Skip("SANDBOX_RUNTIME_CLASS not set — skipping startup comparison test")
	}

	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("runtime-bench-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	podSpec := corev1.PodSpec{
		RuntimeClassName: runtimeClassPtrFromEnv(runtimeClass),
		Containers: []corev1.Container{
			{
				Name:            "pause",
				Image:           "registry.k8s.io/pause:3.10",
				ImagePullPolicy: corev1.PullIfNotPresent,
			},
		},
	}

	// --- Cold start: create Sandbox directly, measure time to Ready ---
	t.Logf("Measuring cold start (runtimeClass=%s)...", runtimeClass)
	coldSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cold-sandbox",
			Namespace: ns.Name,
		},
	}
	coldSandbox.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}

	coldStart := time.Now()
	require.NoError(t, tc.CreateWithCleanup(t.Context(), coldSandbox))
	tc.MustWaitForObject(coldSandbox, predicates.ReadyConditionIsTrue)
	coldDuration := time.Since(coldStart)
	t.Logf("Cold start ready in %s", coldDuration)

	// --- Warm pool setup ---
	t.Logf("Setting up warm pool...")
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bench-template",
			Namespace: ns.Name,
		},
	}
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	replicas := int32(1)
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
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	warmPoolID := types.NamespacedName{Name: warmPool.Name, Namespace: ns.Name}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), warmPoolID))
	t.Logf("Warm pool ready, measuring claim...")

	// --- Warm claim: measure time from claim creation to Ready ---
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bench-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
		},
	}

	claimStart := time.Now()
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))
	tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)
	claimDuration := time.Since(claimStart)
	t.Logf("Warm claim ready in %s", claimDuration)

	// --- Report comparison ---
	t.Logf("=== Startup Comparison (runtimeClass=%s) ===", runtimeClass)
	t.Logf("  Cold start:  %s", coldDuration)
	t.Logf("  Warm claim:  %s", claimDuration)
	if claimDuration > 0 {
		speedup := float64(coldDuration) / float64(claimDuration)
		t.Logf("  Speedup:     %.1fx", speedup)
	}
}

// TestRuntimeClassBurstRecovery measures how a warm pool behaves under
// sustained batch load that exceeds pool refill capacity. Calibration (warm
// baseline and batched refill rate) runs once before the pool-size loop.
// Claims fire in batches of workers×2 with 100ms reconciler settle between
// batches, stopping when ReadyReplicas ≤ 1 (pool depleted) or after 2×N
// total claims. The depletion curve shows how many batches the pool absorbs
// before cold starts appear.
//
// Per-claim data is written to a CSV file for analysis. Set SANDBOX_REPORT_DIR
// to control output location (default: current directory).
//
// Run with:
//
//	SANDBOX_RUNTIME_CLASS=default SANDBOX_POOL_SIZES=4,6,8 go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 30m
func TestRuntimeClassBurstRecovery(t *testing.T) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		t.Skip("SANDBOX_RUNTIME_CLASS not set — skipping burst recovery test")
	}

	rcPtr := runtimeClassPtrFromEnv(runtimeClass)
	workloadSec := benchWorkloadSec()

	reportDir := os.Getenv("SANDBOX_REPORT_DIR")
	if reportDir == "" {
		reportDir = "."
	}

	tc0 := framework.NewTestContext(t)
	clusterID := tc0.ClusterIdentity(t.Context())
	workers, err := tc0.WorkerNodes(t.Context())
	require.NoError(t, err)
	instanceType := "unknown"
	if len(workers) > 0 && workers[0].InstanceType != "" {
		instanceType = workers[0].InstanceType
	}
	dateStr := time.Now().Format("20060102")
	subDir := fmt.Sprintf("%s_%s_%s_%s", clusterID, instanceType, dateStr, runtimeClass)
	reportDir = filepath.Join(reportDir, subDir)
	if _, err := os.Stat(reportDir); err == nil {
		for i := 2; ; i++ {
			candidate := fmt.Sprintf("%s_%d", reportDir, i)
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				reportDir = candidate
				break
			}
		}
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatalf("cannot create report dir %s: %v", reportDir, err)
	}
	t.Logf("[config] cluster=%s instanceType=%s reportDir=%s", clusterID, instanceType, reportDir)

	// --- Calibrate once: warm baseline + batched refill rate ---
	cpus, err := tc0.ClusterCPUCapacity(t.Context())
	require.NoError(t, err)
	calibPoolSize := max(4, len(workers)*2)
	if isVMRuntime(runtimeClass) && int64(calibPoolSize) > cpus {
		calibPoolSize = int(cpus)
	}
	t.Logf("[calibrate] Creating pool-%d to measure warm baseline and batched refill rate (%d workers)...",
		calibPoolSize, len(workers))
	calibNS := &corev1.Namespace{}
	calibNS.Name = fmt.Sprintf("burst-calib-%d", time.Now().UnixNano())
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), calibNS))

	calibTemplate := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "calib-template",
			Namespace: calibNS.Name,
		},
	}
	calibTemplate.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: workloadPodSpec(rcPtr, workloadSec)}
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), calibTemplate))

	calibReplicas := int32(calibPoolSize)
	calibPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "calib-pool",
			Namespace: calibNS.Name,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &calibReplicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: calibTemplate.Name},
		},
	}
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), calibPool))
	calibPoolID := types.NamespacedName{Name: calibPool.Name, Namespace: calibNS.Name}
	// Use generous timeout for calibration — we don't know refill rate yet
	calibTimeout := 5 * time.Minute
	calibCtx, calibCancel := context.WithTimeout(t.Context(), calibTimeout)
	defer calibCancel()
	require.NoError(t, tc0.WaitForWarmPoolReady(calibCtx, calibPoolID))

	// Measure warm baseline from a single claim
	calibClaim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "calib-warm",
			Namespace: calibNS.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: calibPool.Name},
		},
	}
	calibStart := time.Now()
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), calibClaim))
	require.NoError(t, tc0.WaitForObject(calibCtx, calibClaim, predicates.ReadyConditionIsTrue))
	warmBaseline := time.Since(calibStart)

	// Wait for pool to recover, then drain all and measure batched refill
	require.NoError(t, tc0.WaitForWarmPoolReady(calibCtx, calibPoolID))
	for i := range calibPoolSize {
		claim := &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("calib-drain-%d", i),
				Namespace: calibNS.Name,
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{
				WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: calibPool.Name},
			},
		}
		require.NoError(t, tc0.CreateWithCleanup(t.Context(), claim))
		require.NoError(t, tc0.WaitForObject(calibCtx, claim, predicates.ReadyConditionIsTrue))
	}

	refillStart := time.Now()
	require.NoError(t, tc0.WaitForWarmPoolReady(calibCtx, calibPoolID))
	batchRefill := time.Since(refillStart)
	refillPerSlot := batchRefill / time.Duration(calibPoolSize)

	warmColdThreshold := time.Second
	t.Logf("[calibrate] warm=%.3fs, pool-%d refill=%.3fs (%.3fs/slot), threshold=%.3fs",
		warmBaseline.Seconds(), calibPoolSize, batchRefill.Seconds(),
		refillPerSlot.Seconds(), warmColdThreshold.Seconds())

	// Clean up calibration pool to prevent CrashLoopBackOff interference
	t.Logf("[calibrate] cleaning up calibration namespace")
	require.NoError(t, tc0.Delete(t.Context(), calibNS))
	require.NoError(t, tc0.WaitForObjectNotFound(t.Context(), calibNS))

	calcBatchSize := func(poolSize int) int {
		return min(max(4, poolSize/2), 8)
	}

	type claimRecord struct {
		batch        int
		claimIndex   int
		latency      time.Duration
		wallOffset   time.Duration
		readyAtStart int32
	}

	classifyClaim := func(d time.Duration) string {
		if d < warmColdThreshold {
			return "warm"
		}
		return "cold"
	}

	estimateSubtestDuration := func(poolSize int) time.Duration {
		bs := calcBatchSize(poolSize)
		fill := float64(refillPerSlot) * float64(poolSize)
		numBatches := float64((poolSize*2 + bs - 1) / bs)
		perBatch := float64(refillPerSlot) + float64(100*time.Millisecond)
		return time.Duration((fill + numBatches*perBatch) * 2)
	}

	var totalEstimate time.Duration
	poolSizes := benchPoolSizes(cpus)
	for _, ps := range poolSizes {
		totalEstimate += estimateSubtestDuration(ps)
	}
	totalEstimate += calibTimeout
	t.Logf("[budget] estimated total: %s — recommended: -timeout %s",
		totalEstimate.Round(time.Second),
		(totalEstimate + 2*time.Minute).Round(time.Minute))

	for i, poolSize := range poolSizes {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}

		// Skip if remaining test time won't fit this subtest
		subtestEstimate := estimateSubtestDuration(poolSize)
		if deadline, ok := t.Context().Deadline(); ok {
			remaining := time.Until(deadline)
			if subtestEstimate > remaining {
				t.Logf("[budget] skipping pool-%d: estimated %s exceeds remaining %s",
					poolSize, subtestEstimate.Round(time.Second), remaining.Round(time.Second))
				continue
			}
		}

		poolSize := poolSize
		t.Run(fmt.Sprintf("pool-%d", poolSize), func(t *testing.T) {
			tc := framework.NewTestContext(t)

			if isVMRuntime(runtimeClass) {
				cpus, err := tc.ClusterCPUCapacity(t.Context())
				require.NoError(t, err)
				if cpus == 0 {
					t.Skip("no schedulable worker nodes found")
				}
				if int64(poolSize) > cpus {
					t.Skipf("pool size %d exceeds worker CPU capacity (%d vCPUs) — not practical for VM runtime %q",
						poolSize, cpus, runtimeClass)
				}
				t.Logf("[capacity] %d worker vCPUs available, pool size %d — OK", cpus, poolSize)
			}

			// Proportional timeout for this subtest's operations
			poolFillTimeout := time.Duration(float64(refillPerSlot)*float64(poolSize)*3) + 30*time.Second
			claimTimeout := time.Duration(float64(refillPerSlot)*float64(poolSize)) + 30*time.Second

			// --- Setup ---
			t.Logf("[setup] Creating namespace, template, warm pool (size=%d, workload=%ds, fillTimeout=%s)...",
				poolSize, workloadSec, poolFillTimeout.Round(time.Millisecond))
			ns := &corev1.Namespace{}
			ns.Name = fmt.Sprintf("burst-%d-%d", poolSize, time.Now().UnixNano())
			require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

			podSpec := workloadPodSpec(rcPtr, workloadSec)

			template := &extensionsv1beta1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "burst-template",
					Namespace: ns.Name,
				},
			}
			template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}
			require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

			replicas := int32(poolSize)
			warmPool := &extensionsv1beta1.SandboxWarmPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "burst-pool",
					Namespace: ns.Name,
				},
				Spec: extensionsv1beta1.SandboxWarmPoolSpec{
					Replicas:    &replicas,
					TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
				},
			}
			require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

			warmPoolID := types.NamespacedName{Name: warmPool.Name, Namespace: ns.Name}
			fillCtx, fillCancel := context.WithTimeout(t.Context(), poolFillTimeout)
			defer fillCancel()
			require.NoError(t, tc.WaitForWarmPoolReady(fillCtx, warmPoolID))
			t.Logf("[setup] Pool ready")

			batchSize := calcBatchSize(poolSize)

			// --- CSV setup ---
			csvPath := filepath.Join(reportDir, fmt.Sprintf("burst_recovery_%s_pool%d.csv", runtimeClass, poolSize))
			csvFile, err := os.Create(csvPath)
			require.NoError(t, err, "failed to create CSV report")
			defer csvFile.Close()
			cw := csv.NewWriter(csvFile)
			defer cw.Flush()

			cw.Write([]string{"# cluster_id", clusterID})
			cw.Write([]string{"# instance_type", instanceType})
			cw.Write([]string{"# runtime_class", runtimeClass})
			cw.Write([]string{"# pool_size", strconv.Itoa(poolSize)})
			cw.Write([]string{"# workload_sec", strconv.Itoa(workloadSec)})
			cw.Write([]string{"# warm_baseline_sec", fmt.Sprintf("%.3f", warmBaseline.Seconds())})
			cw.Write([]string{"# warm_cold_threshold_sec", fmt.Sprintf("%.3f", warmColdThreshold.Seconds())})
			cw.Write([]string{"# calib_pool_size", strconv.Itoa(calibPoolSize)})
			cw.Write([]string{"# batch_refill_sec", fmt.Sprintf("%.3f", batchRefill.Seconds())})
			cw.Write([]string{"# refill_per_slot_sec", fmt.Sprintf("%.3f", refillPerSlot.Seconds())})
			cw.Write([]string{"# batch_size", strconv.Itoa(batchSize)})
			cw.Write([]string{"# max_claims", strconv.Itoa(poolSize * 2)})
			cw.Write([]string{"# settle_ms", "100"})
			cw.Write([]string{"batch", "claim", "latency_sec", "type", "wall_offset_sec", "ready_at_start"})

			var claimCounter atomic.Int64
			var allRecords []claimRecord

			fireBatch := func(batchNum, count int, readyAtStart int32, testStart time.Time) []claimRecord {
				records := make([]claimRecord, count)
				errs := make([]error, count)

				claimCtx, claimCancel := context.WithTimeout(t.Context(), claimTimeout)
				defer claimCancel()

				var wg sync.WaitGroup
				for i := range count {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						claim := &extensionsv1beta1.SandboxClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      fmt.Sprintf("claim-%d-%d", batchNum, claimCounter.Add(1)),
								Namespace: ns.Name,
							},
							Spec: extensionsv1beta1.SandboxClaimSpec{
								WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
							},
						}
						start := time.Now()
						if err := tc.CreateWithCleanup(claimCtx, claim); err != nil {
							errs[idx] = err
							return
						}
						if err := tc.WaitForObject(claimCtx, claim, predicates.ReadyConditionIsTrue); err != nil {
							errs[idx] = err
							return
						}
						records[idx] = claimRecord{
							batch:        batchNum,
							claimIndex:   idx + 1,
							latency:      time.Since(start),
							wallOffset:   time.Since(testStart),
							readyAtStart: readyAtStart,
						}
					}(i)
				}
				wg.Wait()

				for i, e := range errs {
					require.NoError(t, e, "batch %d claim %d failed", batchNum, i+1)
				}
				return records
			}

			maxClaims := poolSize * 2

			// --- Header ---
			t.Logf("=======================================================================")
			t.Logf("  Burst Recovery: runtime=%s pool=%d workload=%ds", runtimeClass, poolSize, workloadSec)
			t.Logf("  warm=%.3fs  refill/slot=%.3fs (pool-%d: %.3fs)  threshold=%.3fs",
				warmBaseline.Seconds(), refillPerSlot.Seconds(),
				calibPoolSize, batchRefill.Seconds(), warmColdThreshold.Seconds())
			t.Logf("  batchSize=%d  maxClaims=%d  settle=100ms", batchSize, maxClaims)
			t.Logf("=======================================================================")

			// --- Batched drain loop ---
			testStart := time.Now()
			totalClaims := 0
			batchNum := 0

			for totalClaims < maxClaims {
				batchNum++

				if batchNum > 1 {
					time.Sleep(100 * time.Millisecond)
				}

				var poolStatus extensionsv1beta1.SandboxWarmPool
				require.NoError(t, tc.Get(t.Context(), warmPoolID, &poolStatus))
				readyBefore := poolStatus.Status.ReadyReplicas

				if readyBefore <= 1 && totalClaims > poolSize {
					t.Logf("[drain] pool depleted (ready=%d) after %d batches, %d claims",
						readyBefore, batchNum-1, totalClaims)
					break
				}

				count := min(batchSize, maxClaims-totalClaims)
				t.Logf("[batch %d] firing %d claims (ready=%d/%d, total=%d/%d)",
					batchNum, count, readyBefore, poolSize, totalClaims, maxClaims)

				records := fireBatch(batchNum, count, readyBefore, testStart)
				allRecords = append(allRecords, records...)
				totalClaims += count
			}

			// --- Data table ---
			t.Logf("-----------------------------------------------------------------------")
			t.Logf("%-6s %-6s %-14s %-6s %-16s %-6s",
				"BATCH", "CLAIM", "LATENCY(sec)", "TYPE", "WALL_OFFSET(sec)", "READY")
			for _, r := range allRecords {
				claimType := classifyClaim(r.latency)
				t.Logf("%-6d %-6d %-14.3f %-6s %-16.3f %-6d",
					r.batch, r.claimIndex,
					r.latency.Seconds(),
					claimType,
					r.wallOffset.Seconds(),
					r.readyAtStart)

				cw.Write([]string{
					strconv.Itoa(r.batch),
					strconv.Itoa(r.claimIndex),
					fmt.Sprintf("%.3f", r.latency.Seconds()),
					claimType,
					fmt.Sprintf("%.3f", r.wallOffset.Seconds()),
					strconv.Itoa(int(r.readyAtStart)),
				})
			}

			// --- Summary ---
			totalDuration := time.Since(testStart)
			warmCount := 0
			greenCount := 0
			greyZoneCount := 0
			overColdCount := 0
			var worstStart time.Duration
			greenThreshold := time.Duration(float64(warmBaseline) * 1.2)
			for _, r := range allRecords {
				if classifyClaim(r.latency) == "warm" {
					warmCount++
				}
				if r.latency <= greenThreshold {
					greenCount++
				}
				if r.latency > greenThreshold && r.latency <= warmColdThreshold {
					greyZoneCount++
				}
				if r.latency > batchRefill {
					overColdCount++
				}
				if r.latency > worstStart {
					worstStart = r.latency
				}
			}
			t.Logf("=======================================================================")
			t.Logf("  Total batches:       %d (batch_size=%d)", batchNum, batchSize)
			t.Logf("  Total claims:        %d (%d warm, %d cold)", totalClaims, warmCount, totalClaims-warmCount)
			t.Logf("  Green (<=warm):      %d", greenCount)
			t.Logf("  Grey (warm..1s):     %d", greyZoneCount)
			t.Logf("  Worst start:         %.3fs", worstStart.Seconds())
			t.Logf("  Over cold start:     %d (>%.3fs)", overColdCount, batchRefill.Seconds())
			t.Logf("  Total duration(sec): %.3f", totalDuration.Seconds())
			t.Logf("  Throughput:          %.1f claims/sec", float64(totalClaims)/totalDuration.Seconds())
			t.Logf("  CSV report:          %s", csvPath)
			t.Logf("=======================================================================")

			cw.Write([]string{})
			cw.Write([]string{"# total_batches", strconv.Itoa(batchNum)})
			cw.Write([]string{"# total_claims", strconv.Itoa(totalClaims)})
			cw.Write([]string{"# warm_claims", strconv.Itoa(warmCount)})
			cw.Write([]string{"# cold_claims", strconv.Itoa(totalClaims - warmCount)})
			cw.Write([]string{"# green_claims", strconv.Itoa(greenCount)})
			cw.Write([]string{"# grey_zone_claims", strconv.Itoa(greyZoneCount)})
			cw.Write([]string{"# worst_start_sec", fmt.Sprintf("%.3f", worstStart.Seconds())})
			cw.Write([]string{"# over_cold_claims", strconv.Itoa(overColdCount)})
			cw.Write([]string{"# total_duration_sec", fmt.Sprintf("%.3f", totalDuration.Seconds())})
			cw.Write([]string{"# throughput_claims_per_sec", fmt.Sprintf("%.1f", float64(totalClaims)/totalDuration.Seconds())})
		})
	}
}

// ---------------------------------------------------------------------------
// Parameterized benchmarks
// ---------------------------------------------------------------------------

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
		return strings.Split(v, ",")
	}
	return []string{"registry.k8s.io/pause:3.10"}
}

func benchWorkloadSec() int {
	if v := os.Getenv("SANDBOX_WORKLOAD_SEC"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n
		}
	}
	return 30
}

func workloadPodSpec(rcPtr *string, workloadSec int) corev1.PodSpec {
	container := corev1.Container{
		Name:            "workload",
		ImagePullPolicy: corev1.PullIfNotPresent,
	}
	if workloadSec == 0 {
		container.Image = "registry.k8s.io/pause:3.10"
	} else {
		container.Image = "busybox:1.36"
		container.Command = []string{"sleep", strconv.Itoa(workloadSec)}
	}
	return corev1.PodSpec{
		RuntimeClassName: rcPtr,
		Containers:       []corev1.Container{container},
	}
}

func benchPoolSizes(cpuCapacity int64) []int {
	if v := os.Getenv("SANDBOX_POOL_SIZES"); v != "" {
		var sizes []int
		for _, s := range strings.Split(v, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
				sizes = append(sizes, n)
			}
		}
		if len(sizes) > 0 {
			return sizes
		}
	}
	if cpuCapacity > 0 {
		return []int{int(cpuCapacity)}
	}
	return []int{4, 8, 16}
}

func shortImageName(image string) string {
	parts := strings.Split(image, "/")
	return parts[len(parts)-1]
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
		image := image
		b.Run(shortImageName(image), func(b *testing.B) {
			podSpec := runtimeClassPodSpec(rcPtr, image)

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

				b.ReportMetric(time.Since(startTime).Seconds(), "sandbox-ready-sec")
			}
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
	cpus, err := tc0.ClusterCPUCapacity(b.Context())
	if err != nil {
		b.Fatalf("failed to detect cluster CPU capacity: %v", err)
	}
	poolSizes := benchPoolSizes(cpus)
	logBenchHeader(b, "WarmClaim", runtimeClass, poolSizes)
	rcPtr := runtimeClassPtrFromEnv(runtimeClass)

	for _, image := range benchImages() {
		image := image
		for _, poolSize := range poolSizes {
			poolSize := poolSize
			name := fmt.Sprintf("%s/pool-%d", shortImageName(image), poolSize)

			b.Run(name, func(b *testing.B) {
				tc := framework.NewTestContext(b)

				if isVMRuntime(runtimeClass) {
					cpus, err := tc.ClusterCPUCapacity(b.Context())
					if err != nil {
						b.Fatalf("failed to check cluster capacity: %v", err)
					}
					if int64(poolSize) > cpus {
						b.Skipf("pool size %d exceeds worker CPU capacity (%d vCPUs) — not practical for VM runtime %q",
							poolSize, cpus, runtimeClass)
					}
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
				for b.Loop() {
					claimName := fmt.Sprintf("claim-%d", benchSandboxCounter.Add(1))

					claim := &extensionsv1beta1.SandboxClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name:      claimName,
							Namespace: ns.Name,
						},
						Spec: extensionsv1beta1.SandboxClaimSpec{
							WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
						},
					}

					startTime := time.Now()
					tc.MustCreateWithCleanup(claim)
					tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)

					b.ReportMetric(time.Since(startTime).Seconds(), "claim-ready-sec")
				}
			})
		}
	}
}
