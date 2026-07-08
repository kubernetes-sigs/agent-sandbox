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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

var (
	runPerfLoadTest  = flag.Bool("run-perf-load-test", false, "Whether to run the performance density load test.")
	nodeName         = flag.String("node-name", "", "The Kubernetes node to schedule sandboxes on. If empty, the first worker node is selected.")
	density          = flag.Int("density", 20, "The number of pods/sandboxes to provision.")
	imageTag         = flag.String("image-tag", "latest", "The tag of the Chrome Sandbox image.")
	imagePrefix      = flag.String("image-prefix", "kind.local/", "The prefix of the Chrome Sandbox image.")
	runtimeClassName = flag.String("runtime-class-name", "", "The RuntimeClassName to use for the sandbox pods.")
)

// AtomicTimeDuration is a wrapper around time.Duration that allows for concurrent updates and retrievals.
type AtomicTimeDuration struct {
	mu  sync.RWMutex
	d   time.Duration
	set bool
}

func (s *AtomicTimeDuration) Seconds() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Seconds()
}

func (s *AtomicTimeDuration) IsEmpty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.set
}

func (s *AtomicTimeDuration) Set(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		s.d = d
		s.set = true
	}
}

func (s *AtomicTimeDuration) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.String()
}

// ChromeSandboxMetrics holds timing measurements for the chrome sandbox startup.
type ChromeSandboxMetrics struct {
	SandboxReady AtomicTimeDuration `json:"sandbox_ready"`
	PodCreated   AtomicTimeDuration `json:"pod_created"`
	PodScheduled AtomicTimeDuration `json:"pod_scheduled"`
	PodRunning   AtomicTimeDuration `json:"pod_running"`
	PodReady     AtomicTimeDuration `json:"pod_ready"`
	ChromeReady  AtomicTimeDuration `json:"chrome_ready"`
	Total        AtomicTimeDuration `json:"total"`
}

// MarshalJSON customizes JSON serialization for ChromeSandboxMetrics.
func (m *ChromeSandboxMetrics) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]float64{
		"sandbox_ready": m.SandboxReady.Seconds(),
		"pod_created":   m.PodCreated.Seconds(),
		"pod_scheduled": m.PodScheduled.Seconds(),
		"pod_running":   m.PodRunning.Seconds(),
		"pod_ready":     m.PodReady.Seconds(),
		"chrome_ready":  m.ChromeReady.Seconds(),
		"total":         m.Total.Seconds(),
	})
}

func hashString(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

func TestChromeSandboxDensity(t *testing.T) {
	if !*runPerfLoadTest {
		t.Skip("Skipping Chrome Sandbox density test. Pass -run-perf-load-test flag to run.")
	}
	if *density <= 0 {
		t.Fatalf("Density must be positive")
	}

	t.Logf("DEBUG: KUBECONFIG env: %s", os.Getenv("KUBECONFIG"))
	t.Logf("DEBUG: Resolved KUBECONFIG: %s", framework.GetKubeconfig())

	tc := framework.NewTestContext(t)

	// Get target node
	targetNode := *nodeName
	if targetNode == "" {
		var err error
		targetNode, err = getFirstWorkerNode(tc)
		if err != nil {
			t.Fatalf("Failed to get a worker node: %v", err)
		}
	}
	t.Logf("Selected node for density test: %s", targetNode)

	// Get density count
	densityCount := *density
	t.Logf("Running density test with %d pods on node %s", densityCount, targetNode)

	ns := &corev1.Namespace{}
	nodeHash := hashString(targetNode)
	ns.Name = fmt.Sprintf("perf-d-%s-%d-%d", nodeHash, densityCount, time.Now().UnixNano()%1000000)
	tc.MustCreateWithCleanup(ns)

	var wg sync.WaitGroup
	metricsCh := make(chan *ChromeSandboxMetrics, densityCount)

	for i := range densityCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			metrics := runChromeSandboxPerf(tc, ns.Name, fmt.Sprintf("chrome-sandbox-%d", idx), targetNode)
			metricsCh <- metrics
		}(i)
	}

	wg.Wait()
	close(metricsCh)

	var allMetrics []*ChromeSandboxMetrics
	for m := range metricsCh {
		allMetrics = append(allMetrics, m)
	}

	// Log and save metrics
	logAndSaveMetricsStats(t, tc.ArtifactsDir(), allMetrics)
}

func getFirstWorkerNode(tc *framework.TestContext) (string, error) {
	ctx := tc.Context()
	nodes := &corev1.NodeList{}
	if err := tc.List(ctx, nodes); err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes found in the cluster")
	}

	// Prefer worker nodes without control-plane or master roles for Kind clusters,
	// and without NoSchedule taints
	for _, node := range nodes.Items {
		isControlPlane := false
		for k := range node.Labels {
			if strings.Contains(k, "control-plane") || strings.Contains(k, "master") {
				isControlPlane = true
				break
			}
		}
		hasNoScheduleTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				hasNoScheduleTaint = true
				break
			}
		}
		if !isControlPlane && !hasNoScheduleTaint {
			return node.Name, nil
		}
	}

	// Fallback to the first node
	return nodes.Items[0].Name, nil
}

func chromeSandboxPerf(namespace, name, nodeName string) *sandboxv1beta1.Sandbox {
	sandbox := &sandboxv1beta1.Sandbox{}
	sandbox.Name = name
	sandbox.Namespace = namespace
	sandbox.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
			Containers: []corev1.Container{
				{
					Name:            "chrome-sandbox",
					Image:           fmt.Sprintf("%schrome-sandbox:%s", *imagePrefix, *imageTag),
					ImagePullPolicy: corev1.PullIfNotPresent,
					Resources: corev1.ResourceRequirements{
						// Note the request < limits is required for Burstable QoS to enable swap.
						Requests: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("100Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
	}
	if *runtimeClassName != "" {
		sandbox.Spec.PodTemplate.Spec.RuntimeClassName = runtimeClassName
	}
	return sandbox
}

func isPodScheduled(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func runChromeSandboxPerf(tc *framework.TestContext, namespace, name, nodeName string) *ChromeSandboxMetrics {
	ctx, cancel := context.WithTimeout(tc.Context(), 5*time.Minute)
	defer cancel()
	metrics := &ChromeSandboxMetrics{}
	startTime := time.Now()

	sandboxObj := chromeSandboxPerf(namespace, name, nodeName)
	if err := tc.CreateWithCleanup(ctx, sandboxObj); err != nil {
		tc.Errorf("Failed to create sandbox %s: %v", name, err)
		return metrics
	}

	// Watch pod transitions. This blocks until the pod reaches the Running state.
	gvr := corev1.SchemeGroupVersion.WithResource("pods")
	watchFilter := framework.WatchFilter{
		Namespace: namespace,
		Name:      name,
	}

	_, err := framework.Watch(ctx, tc.ClusterClient, gvr, watchFilter, func(_ watch.Event, obj *corev1.Pod) (bool, error) {
		if metrics.PodCreated.IsEmpty() {
			metrics.PodCreated.Set(time.Since(startTime))
		}
		if metrics.PodScheduled.IsEmpty() && isPodScheduled(obj) {
			metrics.PodScheduled.Set(time.Since(startTime))
		}
		if metrics.PodRunning.IsEmpty() && obj.Status.Phase == corev1.PodRunning {
			metrics.PodRunning.Set(time.Since(startTime))
		}
		// Stop watching once the pod has successfully reached Running state
		return !metrics.PodRunning.IsEmpty(), nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		tc.Errorf("Failed watching pod %s: %v", name, err)
		return metrics
	}

	if err := tc.WaitForObject(ctx, sandboxObj, predicates.ReadyConditionIsTrue); err != nil {
		tc.Errorf("Failed waiting for sandbox %s ready: %v", name, err)
		return metrics
	}
	metrics.SandboxReady.Set(time.Since(startTime))

	podID := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	podObj := &corev1.Pod{}
	podObj.Name = podID.Name
	podObj.Namespace = podID.Namespace

	if err := tc.WaitForObject(ctx, podObj, predicates.ReadyConditionIsTrue); err != nil {
		tc.Errorf("Failed waiting for pod %s ready: %v", name, err)
		return metrics
	}
	metrics.PodReady.Set(time.Since(startTime))

	chromeCtx, chromeCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer chromeCancel()

	if err := waitForChromeReadyExec(chromeCtx, podID); err != nil {
		tc.Errorf("Failed to wait for chrome %s ready: %v", name, err)
	} else {
		metrics.ChromeReady.Set(time.Since(startTime))
		metrics.Total.Set(time.Since(startTime))
	}

	return metrics
}

func waitForChromeReadyExec(ctx context.Context, podID types.NamespacedName) error {
	pollDuration := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("chrome readiness polling canceled: %w", ctx.Err())
		default:
			// Execute wget directly in the chrome-sandbox container via kubectl exec to verify CDP is responsive
			cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", podID.Namespace, podID.Name, "-c", "chrome-sandbox", "--", "wget", "-qO-", "http://localhost:9222/json/version")
			out, err := cmd.CombinedOutput()
			if err != nil {
				time.Sleep(pollDuration)
				continue
			}
			if strings.Contains(string(out), "Browser") {
				return nil
			}
			time.Sleep(pollDuration)
		}
	}
}

func logAndSaveMetricsStats(t *testing.T, artifactsDir string, metrics []*ChromeSandboxMetrics) {
	var sandboxReady, podCreated, podScheduled, podRunning, podReady, chromeReady, total []float64
	for _, m := range metrics {
		if !m.SandboxReady.IsEmpty() {
			sandboxReady = append(sandboxReady, m.SandboxReady.Seconds())
		}
		if !m.PodCreated.IsEmpty() {
			podCreated = append(podCreated, m.PodCreated.Seconds())
		}
		if !m.PodScheduled.IsEmpty() {
			podScheduled = append(podScheduled, m.PodScheduled.Seconds())
		}
		if !m.PodRunning.IsEmpty() {
			podRunning = append(podRunning, m.PodRunning.Seconds())
		}
		if !m.PodReady.IsEmpty() {
			podReady = append(podReady, m.PodReady.Seconds())
		}
		if !m.ChromeReady.IsEmpty() {
			chromeReady = append(chromeReady, m.ChromeReady.Seconds())
		}
		if !m.Total.IsEmpty() {
			total = append(total, m.Total.Seconds())
		}
	}

	slices.Sort(sandboxReady)
	slices.Sort(podCreated)
	slices.Sort(podScheduled)
	slices.Sort(podRunning)
	slices.Sort(podReady)
	slices.Sort(chromeReady)
	slices.Sort(total)

	p99 := func(arr []float64) float64 {
		if len(arr) == 0 {
			return 0
		}
		idx := int(math.Ceil(float64(len(arr))*0.99)) - 1
		idx = max(idx, 0)
		idx = min(idx, len(arr)-1)
		return arr[idx]
	}

	avg := func(arr []float64) float64 {
		if len(arr) == 0 {
			return 0
		}
		sum := 0.0
		for _, v := range arr {
			sum += v
		}
		return sum / float64(len(arr))
	}

	t.Logf("Stats for %d sandboxes:", len(metrics))
	t.Logf("SandboxReady: Count=%d Avg=%.2fs, P99=%.2fs", len(sandboxReady), avg(sandboxReady), p99(sandboxReady))
	t.Logf("PodCreated:   Count=%d Avg=%.2fs, P99=%.2fs", len(podCreated), avg(podCreated), p99(podCreated))
	t.Logf("PodScheduled: Count=%d Avg=%.2fs, P99=%.2fs", len(podScheduled), avg(podScheduled), p99(podScheduled))
	t.Logf("PodRunning:   Count=%d Avg=%.2fs, P99=%.2fs", len(podRunning), avg(podRunning), p99(podRunning))
	t.Logf("PodReady:     Count=%d Avg=%.2fs, P99=%.2fs", len(podReady), avg(podReady), p99(podReady))
	t.Logf("ChromeReady:  Count=%d Avg=%.2fs, P99=%.2fs", len(chromeReady), avg(chromeReady), p99(chromeReady))
	t.Logf("Total:        Count=%d Avg=%.2fs, P99=%.2fs", len(total), avg(total), p99(total))

	summarize := func(arr []float64) map[string]float64 {
		return map[string]float64{
			"count": float64(len(arr)),
			"avg":   avg(arr),
			"p99":   p99(arr),
		}
	}

	// Save all raw metrics and stats to a JSON file in the artifacts directory
	stats := map[string]any{
		"density": len(metrics),
		"summary": map[string]any{
			"sandbox_ready": summarize(sandboxReady),
			"pod_created":   summarize(podCreated),
			"pod_scheduled": summarize(podScheduled),
			"pod_running":   summarize(podRunning),
			"pod_ready":     summarize(podReady),
			"chrome_ready":  summarize(chromeReady),
			"total":         summarize(total),
		},
		"raw": metrics,
	}

	filePath := filepath.Join(artifactsDir, "density_metrics.json")
	if fileData, err := json.MarshalIndent(stats, "", "  "); err == nil {
		if err := os.WriteFile(filePath, fileData, 0644); err != nil {
			t.Fatalf("Failed to write density metrics to %s: %v", filePath, err)
		} else {
			t.Logf("Density metrics saved to %s", filePath)
		}
	} else {
		t.Fatalf("Failed to marshal density metrics: %v", err)
	}
}
