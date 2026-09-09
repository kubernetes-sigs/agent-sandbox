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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// benchWorkloadSec returns the workload sleep duration from SANDBOX_WORKLOAD_SEC (default 30).
func benchWorkloadSec() int {
	if v := os.Getenv("SANDBOX_WORKLOAD_SEC"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n
		}
	}
	return 30
}

// workloadPodSpec returns a PodSpec with the given RuntimeClass and a sleep or pause container.
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

// waitForNoPods polls until no pods remain in the namespace, including Terminating ones.
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

// emitBatchSummary writes an aggregated summary row (p50/p95, green/grey/cold counts) to the summary CSV.
func emitBatchSummary(cw *csv.Writer, records []claimRecord, batchFrom, batchTo int,
	batchSize, startBatchSize int, testStart time.Time,
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
	green, grey, cold := 0, 0, 0
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
		if !r.breakdown.IsWarm {
			cold++
		} else if r.latency <= time.Second {
			green++
		} else {
			grey++
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
	var throughput float64
	if len(records) > 1 {
		earliest := records[0].wallOffset - records[0].latency
		latest := records[0].wallOffset
		for _, r := range records[1:] {
			if ct := r.wallOffset - r.latency; ct < earliest {
				earliest = ct
			}
			if r.wallOffset > latest {
				latest = r.wallOffset
			}
		}
		if dur := latest - earliest; dur > 0 {
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
		strconv.Itoa(cold),
		fmt.Sprintf("%.1f", throughput),
	})
}

// TestRuntimeClassBurstRecovery measures how a warm pool behaves under
// sustained batch load that exceeds pool refill capacity. A fresh pool is
// created for each pool size to avoid stale controller state (expectations
// tracker, observedGeneration) from degrading fill times across iterations.
//
// Before entering the subtest loop the test measures three baselines:
// cold start (single bare sandbox), calibration pool fill, and warm claim
// latency. The calibration pool is deleted and fully drained before the
// main loop begins.
//
// Each subtest fires claims in dynamically sized batches with 100ms delay
// between batches, always continuing to 2×poolSize total claims. Pool
// depletion (ReadyReplicas ≤ 1) is logged but does not stop the test —
// claims past depletion exercise the cold fallback path.
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

	// Calibration: measure warm claim baseline using a temporary pool.
	calibReplicas := int32(4)
	if isVMRuntime(runtimeClass) && int64(calibReplicas) > cpus {
		calibReplicas = int32(cpus)
	}
	calibPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "calib-pool",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &calibReplicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	require.NoError(t, tc0.CreateWithCleanup(t.Context(), calibPool))
	calibPoolID := types.NamespacedName{Name: calibPool.Name, Namespace: ns.Name}
	baselinePoolFill(t, tc0, calibPool, calibPoolID, calibReplicas, fillTimeout)

	warmBaseline, calibClaim := baselineWarmClaim(t, tc0, ns.Name, calibPool.Name)
	t.Logf("[baseline] cold=%.3fs warm=%.3fs",
		coldBaseline.Seconds(), warmBaseline.Seconds())

	// Delete calibration pool and wait for full cleanup.
	require.NoError(t, tc0.Delete(t.Context(), calibClaim))
	require.NoError(t, tc0.Delete(t.Context(), calibPool))
	calibDrainCtx, calibDrainCancel := context.WithTimeout(t.Context(), fillTimeout)
	require.NoError(t, waitForNoPods(calibDrainCtx, tc0.ClusterClient, ns.Name),
		"calibration drain must complete before burst iterations")
	calibDrainCancel()

	batchCap := benchBatchCap()
	calcBatchSize := func(poolSize int) int {
		return min(max(4, poolSize/2), batchCap)
	}

	var globalClaimCounter atomic.Int64
	poolSizes, err := benchPoolSizes(cpus)
	if err != nil {
		t.Fatalf("cannot determine pool sizes: %v", err)
	}
	if longevity > 0 && os.Getenv("SANDBOX_POOL_SIZES") == "" {
		poolSizes = []int{int(cpus)}
	}
	t.Logf("[plan] pool sizes=%v cpuCapacity=%d runtime=%s", poolSizes, cpus, runtimeClass)

	for i, poolSize := range poolSizes {
		// Allow up to 300% CPU overprovisioning for VM runtimes — scheduler
		// queues excess VMs while the larger pool improves warm hit ratio.
		if isVMRuntime(runtimeClass) && int64(poolSize) > cpus*3 {
			t.Logf("[skip] pool-%d: exceeds 300%% of worker CPU capacity (%d vCPUs)", poolSize, cpus)
			continue
		}
		if longevity > 0 && poolSize < 20 {
			t.Logf("[skip] pool-%d: longevity mode requires pool size ≥ 20", poolSize)
			continue
		}
		t.Logf("[start] pool-%d (%d/%d)", poolSize, i+1, len(poolSizes))

		// Fresh pool per iteration: clean controller state (expectations
		// tracker, observedGeneration, status). Reusing a pool across
		// iterations carried stale state that degraded fill times.
		replicas := int32(poolSize)
		pool := &extensionsv1beta1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("burst-pool-%d", poolSize),
				Namespace: ns.Name,
			},
			Spec: extensionsv1beta1.SandboxWarmPoolSpec{
				Replicas:    &replicas,
				TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
			},
		}
		require.NoError(t, tc0.CreateWithCleanup(t.Context(), pool))
		poolID := types.NamespacedName{Name: pool.Name, Namespace: ns.Name}

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
			t.Logf("[longevity] filling pool to %d, waiting for %d ready replicas...", poolSize, minReady)
			start := time.Now()
			fillCtx, fillCancel := context.WithTimeout(t.Context(), fillTimeout)
			require.NoError(t, tc0.WaitForWarmPoolMinReady(fillCtx, poolID, minReady))
			fillCancel()
			poolFillTime = time.Since(start)
			t.Logf("[longevity] pool has %d+ ready in %.3fs (fill continues in background)", minReady, poolFillTime.Seconds())
		} else {
			start := time.Now()
			fillCtx, fillCancel := context.WithTimeout(t.Context(), fillTimeout)
			require.NoError(t, tc0.WaitForWarmPoolReady(fillCtx, poolID))
			fillCancel()
			poolFillTime = time.Since(start)
			t.Logf("[fill] pool-%d filled in %.3fs", poolSize, poolFillTime.Seconds())
		}

		t.Run(fmt.Sprintf("pool-%d", poolSize), func(t *testing.T) {
			tc := framework.NewTestContext(t)
			poolStart := time.Now()

			tracker := newMilestoneTracker(t.Context(), t, tc.DynamicClient(), ns.Name)
			defer tracker.Stop()

			claimTimeout := poolFillTime + 30*time.Second
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
			_ = cw.Write([]string{"# kubernetes_version", cluster.KubernetesVersion})
			_ = cw.Write([]string{"# sandbox_version", cluster.SandboxVersion})
			_ = cw.Write([]string{"# provider", cluster.Provider})
			_ = cw.Write([]string{"# worker_count", strconv.Itoa(len(cluster.Workers))})
			_ = cw.Write([]string{"# total_cpu_capacity", strconv.FormatInt(cpus, 10)})
			_ = cw.Write([]string{"# total_ram_capacity_bytes", strconv.FormatInt(cluster.TotalRAMCapacity, 10)})
			_ = cw.Write([]string{"# preexisting_pods", strconv.Itoa(cluster.PreexistingPods)})
			_ = cw.Write([]string{"# allocated_cpu_millis", strconv.FormatInt(cluster.AllocatedCPUMillis, 10)})
			_ = cw.Write([]string{"# allocated_ram_bytes", strconv.FormatInt(cluster.AllocatedRAM, 10)})
			_ = cw.Write([]string{"# instance_type", instanceType})
			_ = cw.Write([]string{"# runtime_class", runtimeClass})
			_ = cw.Write([]string{"# pool_size", strconv.Itoa(poolSize)})
			_ = cw.Write([]string{"# workload_sec", strconv.Itoa(workloadSec)})
			_ = cw.Write([]string{"# cold_baseline_sec", fmt.Sprintf("%.3f", coldBaseline.Seconds())})
			_ = cw.Write([]string{"# warm_baseline_sec", fmt.Sprintf("%.3f", warmBaseline.Seconds())})
			_ = cw.Write([]string{"# green_threshold_sec", "1.000"})
			_ = cw.Write([]string{"# pool_fill_sec", fmt.Sprintf("%.3f", poolFillTime.Seconds())})
			_ = cw.Write([]string{"# batch_size", strconv.Itoa(batchSize)})
			if longevity > 0 {
				_ = cw.Write([]string{"# longevity", longevity.String()})
				_ = cw.Write([]string{"# max_claims", "unlimited"})
			} else {
				_ = cw.Write([]string{"# max_claims", strconv.Itoa(poolSize * 2)})
			}
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
					"best_sec", "worst_sec", "green", "grey", "cold", "throughput_per_sec"})
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
			t.Logf("  cold=%.3fs  warm=%.3fs  fill=%.3fs",
				coldBaseline.Seconds(), warmBaseline.Seconds(), poolFillTime.Seconds())
			if longevity > 0 {
				t.Logf("  batchSize=%d  longevity=%s  adaptive=%v  delay=%s",
					batchSize, longevity, adaptiveBatch, interBatchDelay)
			} else {
				t.Logf("  batchSize=%d  maxClaims=%d  inter_batch=%s", batchSize, maxClaims, interBatchDelay)
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

				if readyBefore <= 1 && totalClaims >= poolSize && totalClaims < poolSize+batchSize && deadline.IsZero() {
					t.Logf("[drain] pool depleted (ready=%d) after %d batches, %d claims — continuing to %d",
						readyBefore, batchNum-1, totalClaims, maxClaims)
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
						strconv.Itoa(count),
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
							batchSize, windowStartBatchSize, testStart,
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
					batchSize, windowStartBatchSize, testStart,
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
			greenCount := 0
			greyCount := 0
			coldCount := 0
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
				if !r.breakdown.IsWarm {
					coldCount++
				} else if r.latency <= time.Second {
					greenCount++
				} else {
					greyCount++
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
			t.Logf("  Total claims:        %d", totalClaims)
			t.Logf("  Green (≤1s, warm):   %d", greenCount)
			t.Logf("  Grey (>1s, warm):    %d", greyCount)
			t.Logf("  Cold (fallback):     %d", coldCount)
			t.Logf("  Worst start:         %.3fs", worstStart.Seconds())
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
			_ = cw.Write([]string{"# green_claims", strconv.Itoa(greenCount)})
			_ = cw.Write([]string{"# grey_claims", strconv.Itoa(greyCount)})
			_ = cw.Write([]string{"# cold_claims", strconv.Itoa(coldCount)})
			_ = cw.Write([]string{"# worst_start_sec", fmt.Sprintf("%.3f", worstStart.Seconds())})
			_ = cw.Write([]string{"# total_duration_sec", fmt.Sprintf("%.3f", totalDuration.Seconds())})
			_ = cw.Write([]string{"# time_to_all_ready_sec", fmt.Sprintf("%.3f", timeToAllReadySec)})
			_ = cw.Write([]string{"# throughput_claims_per_sec", fmt.Sprintf("%.1f", float64(totalClaims)/totalDuration.Seconds())})

			var podList corev1.PodList
			require.NoError(t, tc.List(t.Context(), &podList, client.InNamespace(ns.Name)),
				"listing pods for per-node distribution")
			nodePods := make(map[string]int)
			for i := range podList.Items {
				nodePods[podList.Items[i].Spec.NodeName]++
			}
			for _, w := range cluster.Workers {
				_ = cw.Write([]string{"# pods_on_node", w.Name, strconv.Itoa(nodePods[w.Name])})
			}

			if longevity == 0 || t.Failed() || os.Getenv("SANDBOX_DEBUG") != "" {
				label := fmt.Sprintf("pool%d", poolSize)
				if longevity > 0 {
					label = fmt.Sprintf("longevity-pool%d", poolSize)
				}
				tc.DumpControllerLogsSince(poolStart, label)
			}
		})

		// Delete pool and all claims, then wait for full cleanup.
		// Pool deletion cascades to unclaimed sandboxes via ownerReference.
		// Claimed sandboxes are owned by claims (adoption transferred
		// ownership), so delete claims explicitly too.
		var claimList extensionsv1beta1.SandboxClaimList
		require.NoError(t, tc0.ClusterClient.List(t.Context(), &claimList, client.InNamespace(ns.Name)),
			"listing claims for cleanup")
		for i := range claimList.Items {
			if err := tc0.ClusterClient.Delete(t.Context(), &claimList.Items[i]); client.IgnoreNotFound(err) != nil {
				require.NoError(t, err, "deleting claim %s", claimList.Items[i].Name)
			}
		}
		require.NoError(t, tc0.Delete(t.Context(), pool))

		drainTimeout := time.Duration(poolSize)*10*time.Second + 30*time.Second
		drainCtx, drainCancel := context.WithTimeout(t.Context(), drainTimeout)
		t.Logf("[drain] waiting for pods in %s to terminate (timeout %s)", ns.Name, drainTimeout)
		require.NoError(t, waitForNoPods(drainCtx, tc0.ClusterClient, ns.Name),
			"pod drain must complete before next pool iteration")
		drainCancel()
	}
}
