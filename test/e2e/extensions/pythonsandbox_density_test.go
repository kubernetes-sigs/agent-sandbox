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
	"fmt"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// PythonSandboxMetrics holds timing measurements for the Python sandbox startup and workload execution.
type PythonSandboxMetrics struct {
	SandboxReady AtomicTimeDuration `json:"sandbox_ready"`
	PodCreated   AtomicTimeDuration `json:"pod_created"`
	PodScheduled AtomicTimeDuration `json:"pod_scheduled"`
	PodRunning   AtomicTimeDuration `json:"pod_running"`
	PodReady     AtomicTimeDuration `json:"pod_ready"`
	PythonReady  AtomicTimeDuration `json:"python_ready"`
	Total        AtomicTimeDuration `json:"total"`
	PythonStats  map[string]any     `json:"python_stats,omitempty"` // from JSON output
}

// MarshalJSON customizes JSON serialization for PythonSandboxMetrics.
func (m *PythonSandboxMetrics) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"sandbox_ready": m.SandboxReady.Seconds(),
		"pod_created":   m.PodCreated.Seconds(),
		"pod_scheduled": m.PodScheduled.Seconds(),
		"pod_running":   m.PodRunning.Seconds(),
		"pod_ready":     m.PodReady.Seconds(),
		"python_ready":  m.PythonReady.Seconds(),
		"total":         m.Total.Seconds(),
		"python_stats":  m.PythonStats,
	})
}

// TestPythonSandboxDensity runs high-density performance sweeps provisioning Python AI agent sandboxes on target node pools.
func TestPythonSandboxDensity(t *testing.T) {
	if !*runPerfLoadTest {
		t.Skip("Skipping Python Sandbox density test. Pass -run-perf-load-test flag to run.")
	}
	if *density <= 0 {
		t.Fatalf("Density must be positive")
	}

	tc := framework.NewTestContext(t)

	// Select target worker node
	targetNode := *nodeName
	if targetNode == "" {
		var err error
		targetNode, err = getFirstWorkerNode(tc)
		if err != nil {
			t.Fatalf("Failed to get a worker node: %v", err)
		}
	}
	t.Logf("Selected node for density test: %s", targetNode)

	densityCount := *density
	t.Logf("Running density test with %d pods on node %s", densityCount, targetNode)

	// Create unique test namespace with privileged Pod Security Standard
	ns := &corev1.Namespace{}
	nodeHash := hashString(targetNode)
	ns.Name = fmt.Sprintf("perf-py-%s-%d-%d", nodeHash, densityCount, time.Now().UnixNano()%1000000)
	ns.Labels = map[string]string{
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/audit":   "privileged",
		"pod-security.kubernetes.io/warn":    "privileged",
	}
	tc.MustCreateWithCleanup(ns)

	// Create HostPath PersistentVolume pointing to the host node's MovieLens dataset path (/tmp/movielens)
	pvName := fmt.Sprintf("movielens-pv-%s", ns.Name)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadOnlyMany,
				corev1.ReadWriteMany,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/tmp/movielens",
				},
			},
		},
	}
	tc.MustCreateWithCleanup(pv)

	// Create PersistentVolumeClaim bound to the HostPath PV
	emptyStorageClass := ""
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "movielens-pvc",
			Namespace: ns.Name,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
			VolumeName:       pvName,
			StorageClassName: &emptyStorageClass,
		},
	}
	tc.MustCreateWithCleanup(pvc)

	// Mount python_workload.py script into ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-density-script",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"benchmark_density.py": loadPythonBenchmarkOrPanic(),
		},
	}
	tc.MustCreateWithCleanup(cm)

	var wg sync.WaitGroup
	metricsCh := make(chan *PythonSandboxMetrics, densityCount)

	// Provision sandboxes concurrently with a 1.0s orchestrator deployment stagger delay
	for i := range densityCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			metrics := runPythonSandboxPerf(tc, ns.Name, fmt.Sprintf("python-sandbox-%d", idx), targetNode)
			metricsCh <- metrics
		}(i)
		time.Sleep(1 * time.Second)
	}

	wg.Wait()
	close(metricsCh)

	var allMetrics []*PythonSandboxMetrics
	for m := range metricsCh {
		allMetrics = append(allMetrics, m)
	}

	// Calculate latency summary statistics and write density_metrics.json
	logAndSavePythonMetricsStats(t, tc.ArtifactsDir(), allMetrics)
}

func loadPythonBenchmarkOrPanic() string {
	path := os.Getenv("BENCHMARK_SCRIPT_PATH")
	if path == "" {
		path = "test/e2e/extensions/python_workload.py"
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join("..", "..", "..", "test", "e2e", "extensions", "python_workload.py")
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("could not read %s: %v", path, err))
	}
	return string(b)
}

func pythonSandboxPerf(namespace, name, nodeName string) *sandboxv1beta1.Sandbox {
	sandbox := &sandboxv1beta1.Sandbox{}
	sandbox.Name = name
	sandbox.Namespace = namespace
	sandbox.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
			Containers: []corev1.Container{
				func() corev1.Container {
					img := os.Getenv("PYTHON_SANDBOX_IMAGE")
					if img == "" {
						img = "us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/python-runtime-sandbox:latest-main"
					}
					return corev1.Container{
						Name:            "python-sandbox",
						Image:           img,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{
							{ContainerPort: 8888},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("100Mi"),
								corev1.ResourceCPU:    resource.MustParse("15m"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("2G"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "benchmark-script",
								MountPath: "/scripts",
							},
						},
					}
				}(),
			},
			Volumes: []corev1.Volume{
				{
					Name: "benchmark-script",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "python-density-script",
							},
						},
					},
				},
			},
		},
	}
	sandbox.Spec.PodTemplate.Spec.Containers[0].VolumeMounts = append(
		sandbox.Spec.PodTemplate.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "data-vol", MountPath: "/data", ReadOnly: true},
	)
	sandbox.Spec.PodTemplate.Spec.Volumes = append(
		sandbox.Spec.PodTemplate.Spec.Volumes,
		corev1.Volume{
			Name: "data-vol",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "movielens-pvc",
					ReadOnly:  true,
				},
			},
		},
	)
	if *runtimeClassName != "" {
		sandbox.Spec.PodTemplate.Spec.RuntimeClassName = runtimeClassName
	}
	return sandbox
}

func runPythonSandboxPerf(tc *framework.TestContext, namespace, name, nodeName string) *PythonSandboxMetrics {
	ctx, cancel := context.WithTimeout(tc.Context(), 10*time.Minute)
	defer cancel()
	metrics := &PythonSandboxMetrics{}
	startTime := time.Now()

	sandboxObj := pythonSandboxPerf(namespace, name, nodeName)
	if err := tc.CreateWithCleanup(ctx, sandboxObj); err != nil {
		tc.Errorf("Failed to create sandbox %s: %v", name, err)
		return metrics
	}

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

	pyCtx, pyCancel := context.WithTimeout(ctx, 8*time.Minute)
	defer pyCancel()

	var podIdx int
	_, _ = fmt.Sscanf(name, "python-sandbox-%d", &podIdx)
	select {
	case <-pyCtx.Done():
		tc.Errorf("Context canceled during stagger delay for %s: %v", name, pyCtx.Err())
		return metrics
	case <-time.After(time.Duration(podIdx) * 1 * time.Second):
	}

	if pyStats, err := runPythonBenchmarkExec(pyCtx, podID); err != nil {
		tc.Errorf("Failed to wait for python %s benchmark: %v", name, err)
	} else {
		metrics.PythonReady.Set(time.Since(startTime))
		metrics.Total.Set(time.Since(startTime))
		metrics.PythonStats = pyStats
	}

	return metrics
}

func runPythonBenchmarkExec(ctx context.Context, podID types.NamespacedName) (map[string]any, error) {
	pollDuration := 3 * time.Second
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("python readiness polling canceled: %w", ctx.Err())
		default:
			pyPostCmd := "import urllib.request, json; req = urllib.request.Request('http://localhost:8888/execute', data=json.dumps({'command': 'python3 /scripts/benchmark_density.py'}).encode(), headers={'Content-Type': 'application/json'}); print(urllib.request.urlopen(req).read().decode())"
			cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", podID.Namespace, podID.Name, "-c", "python-sandbox", "--", "python3", "-c", pyPostCmd)
			out, err := cmd.CombinedOutput()
			if err != nil {
				time.Sleep(pollDuration)
				continue
			}

			var res struct {
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal(out, &res); err == nil && res.ExitCode == 0 {
				var m map[string]any
				lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
				lastLine := lines[len(lines)-1]
				if err := json.Unmarshal([]byte(lastLine), &m); err == nil {
					return m, nil
				}
			}
			fmt.Printf("REST API EXEC FAILED: %v, OUTPUT: %s\n", err, string(out))
			time.Sleep(pollDuration)
		}
	}
}

func logAndSavePythonMetricsStats(t *testing.T, artifactsDir string, metrics []*PythonSandboxMetrics) {
	var sandboxReady, podCreated, podScheduled, podRunning, podReady, pythonReady, total, ttfe, pyExecSec, pyMaxRss []float64
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
		if !m.PythonReady.IsEmpty() {
			pythonReady = append(pythonReady, m.PythonReady.Seconds())
		}
		if !m.Total.IsEmpty() {
			total = append(total, m.Total.Seconds())
		}
		if pyStats := m.PythonStats; pyStats != nil {
			if v, ok := pyStats["sandbox_ttfe_ms"].(float64); ok {
				// ttfe is passed as ms from Python, convert to seconds to match other fields
				ttfe = append(ttfe, v/1000.0)
			}
			if v, ok := pyStats["exec_seconds"].(float64); ok {
				pyExecSec = append(pyExecSec, v)
			}
			if v, ok := pyStats["max_rss_mb"].(float64); ok {
				pyMaxRss = append(pyMaxRss, v)
			}
		}
	}

	slices.Sort(sandboxReady)
	slices.Sort(podCreated)
	slices.Sort(podScheduled)
	slices.Sort(podRunning)
	slices.Sort(podReady)
	slices.Sort(pythonReady)
	slices.Sort(total)
	slices.Sort(ttfe)
	slices.Sort(pyExecSec)
	slices.Sort(pyMaxRss)

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

	summarize := func(arr []float64) map[string]float64 {
		return map[string]float64{
			"count": float64(len(arr)),
			"avg":   avg(arr),
			"p99":   p99(arr),
		}
	}

	stats := map[string]any{
		"density": len(metrics),
		"workload_performance": map[string]any{
			"avg_python_exec_seconds": avg(pyExecSec),
			"p99_python_exec_seconds": p99(pyExecSec),
			"avg_python_max_rss_mb":   avg(pyMaxRss),
			"p99_python_max_rss_mb":   p99(pyMaxRss),
		},
		"infrastructure_latencies_summary": map[string]any{
			"sandbox_ready": summarize(sandboxReady),
			"pod_created":   summarize(podCreated),
			"pod_scheduled": summarize(podScheduled),
			"pod_running":   summarize(podRunning),
			"pod_ready":     summarize(podReady),
			"python_ready":  summarize(pythonReady),
			"total":         summarize(total),
			"ttfe":          summarize(ttfe),
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
