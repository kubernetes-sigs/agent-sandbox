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
	"slices"
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
// SandboxClaim → refill cycle with a caller-specified RuntimeClassName.
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

	cluster, err := tc.ClusterInfo(t.Context())
	require.NoError(t, err)

	replicas := int32(2)
	if isVMRuntime(runtimeClass) && cluster.TotalCPUCapacity < int64(replicas) {
		replicas = int32(cluster.TotalCPUCapacity)
	}
	if replicas < 1 {
		t.Skip("not enough CPU capacity for warm pool replicas")
	}
	t.Logf("[config] runtimeClass=%s replicas=%d k8s=%s provider=%s cpus=%d",
		runtimeClass, replicas, cluster.KubernetesVersion, cluster.Provider, cluster.TotalCPUCapacity)

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
	t.Logf("Waiting for WarmPool to reach %d ready replicas (runtimeClass=%s)...", replicas, runtimeClass)
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), warmPoolID))

	// Verify pool sandboxes carry the RuntimeClassName.
	sandboxList := &sandboxv1beta1.SandboxList{}
	require.NoError(t, tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)))
	var poolSandboxes []sandboxv1beta1.Sandbox
	for i := range sandboxList.Items {
		sb := &sandboxList.Items[i]
		if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(sb, warmPool) {
			poolSandboxes = append(poolSandboxes, *sb)
		}
	}
	require.Len(t, poolSandboxes, int(replicas), "expected %d pool sandboxes", replicas)

	for i := range poolSandboxes {
		sb := &poolSandboxes[i]
		require.Equal(t, rcPtr, sb.Spec.PodTemplate.Spec.RuntimeClassName,
			"Sandbox %s RuntimeClassName should match requested value", sb.Name)

		pod := &corev1.Pod{}
		pod.Name = sb.Name
		pod.Namespace = ns.Name
		tc.MustWaitForObject(pod, predicates.ReadyConditionIsTrue)
		require.Equal(t, rcPtr, pod.Spec.RuntimeClassName,
			"Pod %s RuntimeClassName should match requested value", pod.Name)
	}

	// --- Claim 1: consume a sandbox, verify pool refills ---
	claim1 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-claim-1",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
			Lifecycle:   claimLifecycle,
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim1))
	t.Logf("Waiting for claim-1 to be ready...")
	tc.MustWaitForObject(claim1, predicates.ReadyConditionIsTrue)

	t.Logf("Waiting for pool to observe consumed sandbox...")
	require.Eventually(t, func() bool {
		pool := &extensionsv1beta1.SandboxWarmPool{}
		if err := tc.Get(t.Context(), warmPoolID, pool); err != nil {
			return false
		}
		return pool.Status.ReadyReplicas < replicas
	}, framework.DefaultTimeout, time.Second, "pool should observe consumed sandbox")

	t.Logf("Waiting for pool to refill to %d replicas...", replicas)
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), warmPoolID))

	// --- Claim 2: verify the refilled pool serves another claim ---
	claim2 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-claim-2",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
			Lifecycle:   claimLifecycle,
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim2))
	t.Logf("Waiting for claim-2 to be ready...")
	tc.MustWaitForObject(claim2, predicates.ReadyConditionIsTrue)

	t.Logf("RuntimeClass %q lifecycle test passed: pool fill → claim → refill → claim", runtimeClass)
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

	coldDuration := baselineColdStart(t, tc, ns.Name, podSpec)

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

	claimDuration, _ := baselineWarmClaim(t, tc, ns.Name, warmPool.Name)

	t.Logf("=== Startup Comparison (runtimeClass=%s) ===", runtimeClass)
	t.Logf("  Cold start:  %s", coldDuration)
	t.Logf("  Warm claim:  %s", claimDuration)
	if claimDuration > 0 {
		speedup := float64(coldDuration) / float64(claimDuration)
		t.Logf("  Speedup:     %.1fx", speedup)
	}
}

// TestRuntimeClassBurstRecovery measures how a warm pool behaves under
// sustained batch load that exceeds pool refill capacity. A single pool is
// reused across all pool sizes — scaled from 0 to the target between subtests.
//
// Before entering the subtest loop the test measures three baselines:
// cold start (single bare sandbox), pool fill, and warm claim latency.
//
// Each subtest fires claims in dynamically sized batches with 100ms settle
// between batches, stopping when ReadyReplicas ≤ 1 and at least poolSize
// claims have been issued, or after 2×poolSize total claims.
//
// Set SANDBOX_LONGEVITY to a Go duration (e.g. "2h", "30m") to run in
// longevity mode: batches fire continuously until the deadline with adaptive
// batch sizing — batch size decreases on pool depletion and increases when
// ready replicas recover above 50%. Use a single pool size and set -timeout
// accordingly. Set SANDBOX_BATCH_CAP to disable adaptive sizing.
//
// Per-claim data is written to a CSV file for analysis. Set SANDBOX_REPORT_DIR
// to control output location (default: current directory).
//
// Run with:
//
//	SANDBOX_RUNTIME_CLASS=default SANDBOX_POOL_SIZES=4,6,8 go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 30m
//	SANDBOX_RUNTIME_CLASS=kata-clh SANDBOX_POOL_SIZES=4 SANDBOX_LONGEVITY=2h go test ./test/e2e/extensions/... -run TestRuntimeClassBurstRecovery -v -timeout 3h
func TestRuntimeClassBurstRecovery(t *testing.T) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		t.Skip("SANDBOX_RUNTIME_CLASS not set — skipping burst recovery test")
	}

	rcPtr := runtimeClassPtrFromEnv(runtimeClass)
	workloadSec := benchWorkloadSec()
	longevity := benchLongevity()

	reportDir := os.Getenv("SANDBOX_REPORT_DIR")
	if reportDir == "" {
		reportDir = "artifacts"
	}

	tc0 := framework.NewTestContext(t)
	cluster, err := tc0.ClusterInfo(t.Context())
	require.NoError(t, err)
	instanceType := "unknown"
	if len(cluster.Workers) > 0 && cluster.Workers[0].InstanceType != "" {
		instanceType = cluster.Workers[0].InstanceType
	}
	dateStr := time.Now().Format("20060102")
	subDir := fmt.Sprintf("%s_%s_%s_%s", cluster.Identity, instanceType, dateStr, runtimeClass)
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
	t.Logf("[config] cluster=%s instanceType=%s reportDir=%s", cluster.Identity, instanceType, reportDir)

	// --- Shared resources: one namespace, template, pool reused across subtests ---
	cpus := cluster.TotalCPUCapacity
	if isVMRuntime(runtimeClass) && cpus == 0 {
		t.Skip("skipping VM runtime burst test: no worker CPU capacity reported")
	}

	fillTimeout := 5 * time.Minute

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("burst-%d", time.Now().UnixNano())
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), ns))

	// Measure cold start before template creation so longevity can derive workload duration.
	coldBaseline := baselineColdStart(t, tc0, ns.Name, workloadPodSpec(rcPtr, workloadSec))

	if longevity > 0 && os.Getenv("SANDBOX_WORKLOAD_SEC") == "" {
		workloadSec = max(10, int(coldBaseline.Seconds()*5))
		t.Logf("[longevity] workload overridden to %ds (coldStart×5)", workloadSec)
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burst-template",
			Namespace: ns.Name,
		},
	}
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: workloadPodSpec(rcPtr, workloadSec)}
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), template))

	zeroReplicas := int32(0)
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burst-pool",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &zeroReplicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), pool))
	poolID := types.NamespacedName{Name: pool.Name, Namespace: ns.Name}

	settleDur := benchSettleDuration()

	calibReplicas := int32(4)
	if isVMRuntime(runtimeClass) && int64(calibReplicas) > cpus {
		calibReplicas = int32(cpus)
	}
	baselinePoolFill(t, tc0, pool, poolID, calibReplicas, fillTimeout)

	if settleDur > 0 {
		t.Logf("[settle] waiting %s for controller to drain after calibration fill", settleDur)
		time.Sleep(settleDur)
	}

	warmBaseline, calibClaim := baselineWarmClaim(t, tc0, ns.Name, pool.Name)
	warmColdThreshold := time.Second
	t.Logf("[baseline] cold=%.3fs warm=%.3fs threshold=%.3fs",
		coldBaseline.Seconds(), warmBaseline.Seconds(), warmColdThreshold.Seconds())

	// Scale to 0 before entering subtest loop
	require.NoError(t, tc0.Delete(t.Context(), calibClaim))
	framework.MustUpdateObject(tc0.ClusterClient, pool, func(p *extensionsv1beta1.SandboxWarmPool) {
		p.Spec.Replicas = &zeroReplicas
	})
	drainCtx, drainCancel := context.WithTimeout(t.Context(), fillTimeout)
	require.NoError(t, tc0.WaitForWarmPoolReady(drainCtx, poolID))
	drainCancel()

	batchCap := benchBatchCap()
	calcBatchSize := func(poolSize int) int {
		return min(max(4, poolSize/2), batchCap)
	}

	isUnder1s := func(d time.Duration) bool {
		return d < warmColdThreshold
	}

	var globalClaimCounter atomic.Int64
	poolSizes, err := benchPoolSizes(cpus)
	if err != nil {
		t.Fatalf("cannot determine pool sizes: %v", err)
	}
	if longevity > 0 && os.Getenv("SANDBOX_POOL_SIZES") == "" {
		poolSizes = []int{int(cpus)}
	}

	for _, poolSize := range poolSizes {
		// Allow up to 300% CPU overprovisioning for VM runtimes — scheduler
		// queues excess VMs while the larger pool improves warm hit ratio.
		if isVMRuntime(runtimeClass) && int64(poolSize) > cpus*3 {
			t.Logf("[skip] pool size %d exceeds 300%% of worker CPU capacity (%d vCPUs)", poolSize, cpus)
			continue
		}
		if longevity > 0 && poolSize < 20 {
			t.Logf("[skip] longevity mode requires pool size ≥ 20 (got %d)", poolSize)
			continue
		}

		preBatchSize := calcBatchSize(poolSize)
		if longevity > 0 && coldBaseline > 0 {
			preBatchSize = max(4, int(float64(poolSize)*0.3/coldBaseline.Seconds()))
			if os.Getenv("SANDBOX_BATCH_CAP") != "" {
				preBatchSize = min(preBatchSize, batchCap)
			}
		}

		var poolFillTime time.Duration
		if longevity > 0 {
			minReady := int32(min(2*preBatchSize, poolSize))
			framework.MustUpdateObject(tc0.ClusterClient, pool, func(p *extensionsv1beta1.SandboxWarmPool) {
				r := int32(poolSize)
				p.Spec.Replicas = &r
			})
			t.Logf("[longevity] scaling pool to %d, waiting for %d ready replicas...", poolSize, minReady)
			start := time.Now()
			fillCtx, fillCancel := context.WithTimeout(t.Context(), fillTimeout)
			require.NoError(t, tc0.WaitForWarmPoolMinReady(fillCtx, poolID, minReady))
			fillCancel()
			poolFillTime = time.Since(start)
			t.Logf("[longevity] pool has %d+ ready in %.3fs (fill continues in background)", minReady, poolFillTime.Seconds())
		} else {
			poolFillTime = baselinePoolFill(t, tc0, pool, poolID, int32(poolSize), fillTimeout)
		}

		t.Run(fmt.Sprintf("pool-%d", poolSize), func(t *testing.T) {
			tc := framework.NewTestContext(t)
			poolStart := time.Now()

			if settleDur > 0 && longevity == 0 {
				t.Logf("[settle] waiting %s for controller work queue to drain after fill", settleDur)
				time.Sleep(settleDur)
			}

			tracker := newMilestoneTracker(t.Context(), t, tc.DynamicClient(), ns.Name)
			defer tracker.Stop()

			claimTimeout := poolFillTime + 30*time.Second
			greenThreshold := 500 * time.Millisecond
			t.Logf("[setup] Pool-%d filled in %.3fs", poolSize, poolFillTime.Seconds())

			batchSize := calcBatchSize(poolSize)
			if longevity > 0 && coldBaseline > 0 {
				batchSize = max(4, int(float64(poolSize)*0.3/coldBaseline.Seconds()))
				if os.Getenv("SANDBOX_BATCH_CAP") != "" {
					batchSize = min(batchSize, batchCap)
				}
			}

			interBatchDelay := 100 * time.Millisecond
			if longevity > 0 && coldBaseline > 0 {
				interBatchDelay = max(50*time.Millisecond,
					coldBaseline*time.Duration(batchSize)/time.Duration(poolSize))
			}

			// --- CSV setup ---
			csvPath := filepath.Join(reportDir, fmt.Sprintf("burst_recovery_%s_pool%d.csv", runtimeClass, poolSize))
			csvFile, err := os.Create(csvPath)
			require.NoError(t, err, "failed to create CSV report")
			defer csvFile.Close()
			cw := csv.NewWriter(csvFile)
			defer cw.Flush()

			_ = cw.Write([]string{"# cluster_id", cluster.Identity})
			_ = cw.Write([]string{"# worker_count", strconv.Itoa(len(cluster.Workers))})
			_ = cw.Write([]string{"# total_cpu_capacity", strconv.FormatInt(cpus, 10)})
			_ = cw.Write([]string{"# instance_type", instanceType})
			_ = cw.Write([]string{"# runtime_class", runtimeClass})
			_ = cw.Write([]string{"# pool_size", strconv.Itoa(poolSize)})
			_ = cw.Write([]string{"# workload_sec", strconv.Itoa(workloadSec)})
			_ = cw.Write([]string{"# cold_baseline_sec", fmt.Sprintf("%.3f", coldBaseline.Seconds())})
			_ = cw.Write([]string{"# warm_baseline_sec", fmt.Sprintf("%.3f", warmBaseline.Seconds())})
			_ = cw.Write([]string{"# warm_cold_threshold_sec", fmt.Sprintf("%.3f", warmColdThreshold.Seconds())})
			_ = cw.Write([]string{"# pool_fill_sec", fmt.Sprintf("%.3f", poolFillTime.Seconds())})
			_ = cw.Write([]string{"# batch_size", strconv.Itoa(batchSize)})
			if longevity > 0 {
				_ = cw.Write([]string{"# longevity", longevity.String()})
				_ = cw.Write([]string{"# max_claims", "unlimited"})
			} else {
				_ = cw.Write([]string{"# max_claims", strconv.Itoa(poolSize * 2)})
			}
			_ = cw.Write([]string{"# settle_sec", strconv.Itoa(int(settleDur.Seconds()))})
			_ = cw.Write([]string{"# inter_batch_delay_ms", strconv.Itoa(int(interBatchDelay.Milliseconds()))})
			_ = cw.Write([]string{"batch", "claim", "batch_size", "latency_sec", "timestamp", "wall_offset_sec", "ready_at_start",
				"create_ack_ms", "adoption_ms", "schedule_ms", "runtime_ms", "propagate_ms", "e2e_ms", "is_warm"})
			cw.Flush()

			var allRecords []claimRecord

			var summCw *csv.Writer
			if longevity > 0 {
				summPath := filepath.Join(reportDir, fmt.Sprintf("burst_summary_%s_pool%d.csv", runtimeClass, poolSize))
				sf, serr := os.Create(summPath)
				require.NoError(t, serr, "failed to create summary CSV")
				defer sf.Close()
				summCw = csv.NewWriter(sf)
				defer summCw.Flush()
				_ = summCw.Write([]string{"wall_min", "batch_from", "batch_to", "batch_size", "direction",
					"claims", "ready_avg", "latency_avg_sec", "latency_p50_sec", "latency_p95_sec",
					"best_sec", "worst_sec", "green", "grey", "over_1s", "warm_ratio", "throughput_per_sec"})
				summCw.Flush()
				t.Logf("[csv] summary: %s", summPath)
			}

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
						claimName := fmt.Sprintf("claim-%d-%d", poolSize, globalClaimCounter.Add(1))
						claim := &extensionsv1beta1.SandboxClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      claimName,
								Namespace: ns.Name,
							},
							Spec: extensionsv1beta1.SandboxClaimSpec{
								WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name},
							},
						}
						claim.Spec.Lifecycle = claimLifecycle
						tracker.Register(claimName)
						createStart := time.Now()
						tracker.MarkCreateCalled(claimName, createStart)
						if err := tc.CreateWithCleanup(claimCtx, claim); err != nil {
							errs[idx] = err
							return
						}
						tracker.MarkCreateReturned(claimName, time.Now())
						if err := tracker.WaitReady(claimCtx, claimName); err != nil {
							errs[idx] = err
							return
						}
						bd, bdErr := tracker.CollectBreakdown(claimCtx, tc.ClusterClient, claimName)
						if bdErr != nil {
							t.Logf("[breakdown] claim %s: %v", claimName, bdErr)
						}
						records[idx] = claimRecord{
							batch:        batchNum,
							claimIndex:   idx + 1,
							createTime:   createStart,
							latency:      time.Since(createStart),
							wallOffset:   time.Since(testStart),
							readyAtStart: readyAtStart,
							breakdown:    bd,
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
			initialBatchSize := batchSize
			adaptiveBatch := longevity > 0 && os.Getenv("SANDBOX_BATCH_CAP") == ""

			// --- Header ---
			t.Logf("=======================================================================")
			t.Logf("  Burst Recovery: runtime=%s pool=%d workload=%ds", runtimeClass, poolSize, workloadSec)
			t.Logf("  cold=%.3fs  warm=%.3fs  fill=%.3fs  threshold=%.3fs",
				coldBaseline.Seconds(), warmBaseline.Seconds(), poolFillTime.Seconds(), warmColdThreshold.Seconds())
			if longevity > 0 {
				t.Logf("  batchSize=%d  longevity=%s  adaptive=%v  settle=%s  delay=%s",
					batchSize, longevity, adaptiveBatch, settleDur, interBatchDelay)
			} else {
				t.Logf("  batchSize=%d  maxClaims=%d  settle=%s  inter_batch=%s", batchSize, maxClaims, settleDur, interBatchDelay)
			}
			t.Logf("=======================================================================")

			// --- Batched drain loop ---
			testStart := time.Now()
			totalClaims := 0
			batchNum := 0
			minBatch, maxBatch := batchSize, batchSize
			deadline := time.Time{}
			const summaryInterval = 10
			var windowRecords []claimRecord
			windowBatchFrom := 1
			windowStartBatchSize := batchSize
			var windowReadySum float64
			var windowBatchCount int
			if longevity > 0 {
				deadline = testStart.Add(longevity)
			}
			shouldContinue := func() bool {
				if !deadline.IsZero() {
					return time.Now().Before(deadline)
				}
				return totalClaims < maxClaims
			}

			for shouldContinue() {
				batchNum++

				if batchNum > 1 {
					time.Sleep(interBatchDelay)
				}

				var poolStatus extensionsv1beta1.SandboxWarmPool
				require.NoError(t, tc.Get(t.Context(), poolID, &poolStatus))
				readyBefore := poolStatus.Status.ReadyReplicas

				if readyBefore <= 1 && totalClaims >= poolSize && deadline.IsZero() {
					t.Logf("[drain] pool depleted (ready=%d) after %d batches, %d claims",
						readyBefore, batchNum-1, totalClaims)
					break
				}

				if adaptiveBatch {
					if readyBefore < int32(poolSize/2) && batchSize > 1 {
						batchSize--
						t.Logf("[adapt] batch_size→%d (ready %d < pool/2)", batchSize, readyBefore)
					} else if readyBefore > int32(poolSize)-int32(batchSize) && batchSize < initialBatchSize {
						batchSize++
						t.Logf("[adapt] batch_size→%d (ready %d > pool-batch)", batchSize, readyBefore)
					}
					minBatch = min(minBatch, batchSize)
					maxBatch = max(maxBatch, batchSize)
				}

				count := batchSize
				if deadline.IsZero() {
					count = min(batchSize, maxClaims-totalClaims)
				}
				if !deadline.IsZero() {
					t.Logf("[batch %d] firing %d claims (ready=%d/%d, total=%d, remaining=%s)",
						batchNum, count, readyBefore, poolSize, totalClaims, time.Until(deadline).Truncate(time.Second))
				} else {
					t.Logf("[batch %d] firing %d claims (ready=%d/%d, total=%d/%d)",
						batchNum, count, readyBefore, poolSize, totalClaims, maxClaims)
				}

				records := fireBatch(batchNum, count, readyBefore, testStart)
				allRecords = append(allRecords, records...)
				totalClaims += count

				for _, r := range records {
					bd := r.breakdown
					_ = cw.Write([]string{
						strconv.Itoa(r.batch),
						strconv.Itoa(r.claimIndex),
						strconv.Itoa(batchSize),
						fmt.Sprintf("%.3f", r.latency.Seconds()),
						r.createTime.UTC().Format("2006-01-02T15:04:05.000Z"),
						fmt.Sprintf("%.3f", r.wallOffset.Seconds()),
						strconv.Itoa(int(r.readyAtStart)),
						fmt.Sprintf("%.1f", bd.CreateAckMs),
						fmt.Sprintf("%.1f", bd.AdoptionMs),
						fmt.Sprintf("%.1f", bd.ScheduleMs),
						fmt.Sprintf("%.1f", bd.RuntimeMs),
						fmt.Sprintf("%.1f", bd.PropagateMs),
						fmt.Sprintf("%.1f", bd.EndToEndMs),
						strconv.FormatBool(bd.IsWarm),
					})
				}
				cw.Flush()

				if summCw != nil {
					windowRecords = append(windowRecords, records...)
					windowReadySum += float64(readyBefore)
					windowBatchCount++
					if batchNum%summaryInterval == 0 || !shouldContinue() {
						emitBatchSummary(summCw, windowRecords, windowBatchFrom, batchNum,
							batchSize, windowStartBatchSize, testStart, greenThreshold, warmColdThreshold,
							windowReadySum, windowBatchCount)
						summCw.Flush()
						windowRecords = nil
						windowBatchFrom = batchNum + 1
						windowStartBatchSize = batchSize
						windowReadySum = 0
						windowBatchCount = 0
					}
				}
			}

			if summCw != nil && len(windowRecords) > 0 {
				emitBatchSummary(summCw, windowRecords, windowBatchFrom, batchNum,
					batchSize, windowStartBatchSize, testStart, greenThreshold, warmColdThreshold,
					windowReadySum, windowBatchCount)
				summCw.Flush()
			}

			if longevity == 0 {
				t.Logf("-----------------------------------------------------------------------")
				t.Logf("%-6s %-6s %-12s %-24s %-14s %-6s  %-10s %-10s %-10s %-10s %-10s %-10s %-6s",
					"BATCH", "CLAIM", "LATENCY(s)", "TIMESTAMP", "WALL_OFF(s)", "READY",
					"ACK_MS", "ADOPT_MS", "SCHED_MS", "RUNTIME_MS", "PROP_MS", "E2E_MS", "WARM")
				for _, r := range allRecords {
					bd := r.breakdown
					t.Logf("%-6d %-6d %-12.3f %-24s %-14.3f %-6d  %-10.1f %-10.1f %-10.1f %-10.1f %-10.1f %-10.1f %-6v",
						r.batch, r.claimIndex,
						r.latency.Seconds(),
						r.createTime.UTC().Format("2006-01-02T15:04:05.000Z"),
						r.wallOffset.Seconds(),
						r.readyAtStart,
						bd.CreateAckMs, bd.AdoptionMs, bd.ScheduleMs,
						bd.RuntimeMs, bd.PropagateMs, bd.EndToEndMs, bd.IsWarm)
				}
			}

			// --- Summary ---
			totalDuration := time.Since(testStart)
			var firstCreate, lastReady time.Time
			under1sCount := 0
			greenCount := 0
			greyZoneCount := 0
			overColdCount := 0
			var worstStart time.Duration
			for _, r := range allRecords {
				createTime := testStart.Add(r.wallOffset - r.latency)
				readyTime := testStart.Add(r.wallOffset)
				if firstCreate.IsZero() || createTime.Before(firstCreate) {
					firstCreate = createTime
				}
				if readyTime.After(lastReady) {
					lastReady = readyTime
				}
				if isUnder1s(r.latency) {
					under1sCount++
				}
				if r.latency <= greenThreshold {
					greenCount++
				}
				if r.latency > greenThreshold && r.latency <= warmColdThreshold {
					greyZoneCount++
				}
				if r.latency > poolFillTime {
					overColdCount++
				}
				if r.latency > worstStart {
					worstStart = r.latency
				}
			}
			var timeToAllReadySec float64
			if !firstCreate.IsZero() && !lastReady.IsZero() {
				timeToAllReadySec = lastReady.Sub(firstCreate).Seconds()
			}

			t.Logf("=======================================================================")
			t.Logf("  Total batches:       %d (batch_size=%d)", batchNum, batchSize)
			if adaptiveBatch {
				t.Logf("  Adaptive batch:      %d → [%d, %d]", initialBatchSize, minBatch, maxBatch)
			}
			if longevity > 0 {
				t.Logf("  Longevity:           %s", longevity)
			}
			t.Logf("  Total claims:        %d (%d under1s, %d over1s)", totalClaims, under1sCount, totalClaims-under1sCount)
			t.Logf("  Green (<=500ms):     %d", greenCount)
			t.Logf("  Grey (500ms..1s):    %d", greyZoneCount)
			t.Logf("  Worst start:         %.3fs", worstStart.Seconds())
			t.Logf("  Over cold start:     %d (>%.3fs)", overColdCount, poolFillTime.Seconds())
			t.Logf("  Time to all ready:   %.3fs", timeToAllReadySec)
			t.Logf("  Total duration(sec): %.3f", totalDuration.Seconds())
			t.Logf("  Throughput:          %.1f claims/sec", float64(totalClaims)/totalDuration.Seconds())
			t.Logf("  CSV report:          %s", csvPath)
			t.Logf("=======================================================================")

			_ = cw.Write([]string{})
			_ = cw.Write([]string{"# total_batches", strconv.Itoa(batchNum)})
			if adaptiveBatch {
				_ = cw.Write([]string{"# adaptive_batch", "true"})
				_ = cw.Write([]string{"# min_batch_size", strconv.Itoa(minBatch)})
				_ = cw.Write([]string{"# max_batch_size", strconv.Itoa(maxBatch)})
			}
			_ = cw.Write([]string{"# total_claims", strconv.Itoa(totalClaims)})
			_ = cw.Write([]string{"# under_1s_claims", strconv.Itoa(under1sCount)})
			_ = cw.Write([]string{"# over_1s_claims", strconv.Itoa(totalClaims - under1sCount)})
			_ = cw.Write([]string{"# green_claims", strconv.Itoa(greenCount)})
			_ = cw.Write([]string{"# grey_zone_claims", strconv.Itoa(greyZoneCount)})
			_ = cw.Write([]string{"# worst_start_sec", fmt.Sprintf("%.3f", worstStart.Seconds())})
			_ = cw.Write([]string{"# over_cold_claims", strconv.Itoa(overColdCount)})
			_ = cw.Write([]string{"# total_duration_sec", fmt.Sprintf("%.3f", totalDuration.Seconds())})
			_ = cw.Write([]string{"# time_to_all_ready_sec", fmt.Sprintf("%.3f", timeToAllReadySec)})
			_ = cw.Write([]string{"# throughput_claims_per_sec", fmt.Sprintf("%.1f", float64(totalClaims)/totalDuration.Seconds())})

			if longevity == 0 || t.Failed() || os.Getenv("SANDBOX_DEBUG") != "" {
				label := fmt.Sprintf("pool%d", poolSize)
				if longevity > 0 {
					label = fmt.Sprintf("longevity-pool%d", poolSize)
				}
				tc.DumpControllerLogsSince(poolStart, label)
			}
		})

		// Scale pool to 0 and wait for all pods (including Terminating) to be gone
		framework.MustUpdateObject(tc0.ClusterClient, pool, func(p *extensionsv1beta1.SandboxWarmPool) {
			p.Spec.Replicas = &zeroReplicas
		})
		drainCtx, drainCancel := context.WithTimeout(t.Context(), fillTimeout)
		require.NoError(t, tc0.WaitForWarmPoolReady(drainCtx, poolID))
		drainCancel()

		// Wait for all pods (including Terminating) to be fully gone before
		// scaling to the next pool size. This ensures CPU capacity is restored,
		// which is critical for kata where VM termination takes 5-10s per pod.
		podDrainTimeout := time.Duration(poolSize)*10*time.Second + 30*time.Second
		podDrainCtx, podDrainCancel := context.WithTimeout(t.Context(), podDrainTimeout)
		t.Logf("[drain] waiting for all pods in %s to terminate (timeout %s)", ns.Name, podDrainTimeout)
		if err := waitForNoPods(podDrainCtx, tc0.ClusterClient, ns.Name); err != nil {
			t.Logf("[drain] WARNING: %v — proceeding anyway", err)
		}
		podDrainCancel()
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

func benchSettleDuration() time.Duration {
	if v := os.Getenv("SANDBOX_SETTLE_SEC"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 2 * time.Second
}

func benchBatchCap() int {
	if v := os.Getenv("SANDBOX_BATCH_CAP"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 {
			return n
		}
	}
	return 10
}

func benchLongevity() time.Duration {
	if v := os.Getenv("SANDBOX_LONGEVITY"); v != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

var claimTTL = func() int32 {
	if v := os.Getenv("SANDBOX_TTL"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return int32(n)
		}
	}
	return 0
}()

var claimLifecycle = &extensionsv1beta1.Lifecycle{
	ShutdownPolicy:          extensionsv1beta1.ShutdownPolicyDelete,
	TTLSecondsAfterFinished: &claimTTL,
}

// ---------------------------------------------------------------------------
// Baseline measurements
// ---------------------------------------------------------------------------

func baselineColdStart(t *testing.T, tc *framework.TestContext, ns string, podSpec corev1.PodSpec) time.Duration {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cold-baseline-%d", time.Now().UnixNano()),
			Namespace: ns,
		},
	}
	sandbox.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}

	t.Logf("[baseline] measuring cold start...")
	start := time.Now()
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))
	tc.MustWaitForObject(sandbox, predicates.ReadyConditionIsTrue)
	d := time.Since(start)

	require.NoError(t, tc.Delete(t.Context(), sandbox))
	t.Logf("[baseline] cold start: %.3fs", d.Seconds())
	return d
}

func baselineWarmClaim(t *testing.T, tc *framework.TestContext, ns, poolName string) (time.Duration, *extensionsv1beta1.SandboxClaim) {
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("warm-baseline-%d", time.Now().UnixNano()),
			Namespace: ns,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: poolName},
			Lifecycle:   claimLifecycle,
		},
	}

	t.Logf("[baseline] measuring warm claim...")
	start := time.Now()
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))
	tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)
	d := time.Since(start)
	t.Logf("[baseline] warm claim: %.3fs", d.Seconds())
	return d, claim
}

func baselinePoolFill(t *testing.T, tc *framework.TestContext, pool *extensionsv1beta1.SandboxWarmPool, poolID types.NamespacedName, replicas int32, timeout time.Duration) time.Duration {
	framework.MustUpdateObject(tc.ClusterClient, pool, func(p *extensionsv1beta1.SandboxWarmPool) {
		p.Spec.Replicas = &replicas
	})

	t.Logf("[baseline] filling pool to %d replicas...", replicas)
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	require.NoError(t, tc.WaitForWarmPoolReady(ctx, poolID))
	d := time.Since(start)
	t.Logf("[baseline] pool-%d filled in %.3fs", replicas, d.Seconds())
	return d
}

// waitForNoPods polls until no pods remain in the namespace, including those
// in Terminating phase. Returns nil when the namespace is empty, or an error
// if the context expires with pods still present.
func waitForNoPods(ctx context.Context, cl *framework.ClusterClient, namespace string) error {
	for {
		var podList corev1.PodList
		if err := cl.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("listing pods in %s: %w", namespace, err)
		}
		if len(podList.Items) == 0 {
			return nil
		}
		terminating := 0
		for i := range podList.Items {
			if podList.Items[i].DeletionTimestamp != nil {
				terminating++
			}
		}
		cl.Logf("[drain] %d pods remaining (%d terminating) in %s", len(podList.Items), terminating, namespace)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d pods still in %s after timeout (%d terminating)", len(podList.Items), namespace, terminating)
		case <-time.After(2 * time.Second):
		}
	}
}

type claimRecord struct {
	batch        int
	claimIndex   int
	createTime   time.Time
	latency      time.Duration
	wallOffset   time.Duration
	readyAtStart int32
	breakdown    milestoneBreakdown
}

func emitBatchSummary(cw *csv.Writer, records []claimRecord, batchFrom, batchTo int,
	batchSize, startBatchSize int, testStart time.Time,
	greenThreshold, warmColdThreshold time.Duration,
	readySum float64, batchCount int) {
	if len(records) == 0 {
		return
	}
	wallMin := time.Since(testStart).Minutes()
	direction := "="
	if batchSize > startBatchSize {
		direction = "+"
	} else if batchSize < startBatchSize {
		direction = "-"
	}
	readyAvg := readySum / float64(max(1, batchCount))
	latencies := make([]float64, len(records))
	var latencySum float64
	best := records[0].latency.Seconds()
	worst := 0.0
	green, grey, over1s, warm := 0, 0, 0, 0
	for i, r := range records {
		s := r.latency.Seconds()
		latencies[i] = s
		latencySum += s
		if s < best {
			best = s
		}
		if s > worst {
			worst = s
		}
		if r.latency <= greenThreshold {
			green++
		} else if r.latency <= warmColdThreshold {
			grey++
		} else {
			over1s++
		}
		if r.breakdown.IsWarm {
			warm++
		}
	}
	slices.Sort(latencies)
	avg := latencySum / float64(len(latencies))
	p50 := latencies[len(latencies)/2]
	p95Idx := int(float64(len(latencies)) * 0.95)
	if p95Idx >= len(latencies) {
		p95Idx = len(latencies) - 1
	}
	p95 := latencies[p95Idx]
	warmRatio := float64(warm) / float64(len(records))
	var throughput float64
	if len(records) > 1 {
		first := records[0].wallOffset - records[0].latency
		last := records[len(records)-1].wallOffset
		if dur := last - first; dur > 0 {
			throughput = float64(len(records)) / dur.Seconds()
		}
	}
	_ = cw.Write([]string{
		fmt.Sprintf("%.1f", wallMin),
		strconv.Itoa(batchFrom),
		strconv.Itoa(batchTo),
		strconv.Itoa(batchSize),
		direction,
		strconv.Itoa(len(records)),
		fmt.Sprintf("%.1f", readyAvg),
		fmt.Sprintf("%.3f", avg),
		fmt.Sprintf("%.3f", p50),
		fmt.Sprintf("%.3f", p95),
		fmt.Sprintf("%.3f", best),
		fmt.Sprintf("%.3f", worst),
		strconv.Itoa(green),
		strconv.Itoa(grey),
		strconv.Itoa(over1s),
		fmt.Sprintf("%.2f", warmRatio),
		fmt.Sprintf("%.1f", throughput),
	})
}

func benchPoolSizes(cpuCapacity int64) ([]int, error) {
	if v := os.Getenv("SANDBOX_POOL_SIZES"); v != "" {
		var sizes []int
		for s := range strings.SplitSeq(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return nil, fmt.Errorf("invalid SANDBOX_POOL_SIZES value %q: %w", s, err)
			}
			if n <= 0 {
				return nil, fmt.Errorf("invalid SANDBOX_POOL_SIZES value %q: must be positive", s)
			}
			sizes = append(sizes, n)
		}
		return sizes, nil
	}
	if cpuCapacity > 0 {
		half := max(int(cpuCapacity/2), 1)
		full := int(cpuCapacity)
		double := full * 2
		return []int{half, full, double}, nil
	}
	return nil, fmt.Errorf("cluster reported 0 worker CPU capacity — cannot derive pool sizes")
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
