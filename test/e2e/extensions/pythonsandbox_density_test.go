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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
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
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	remoteexec "k8s.io/client-go/util/exec"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// Shared density load testing flags (runPerfLoadTest, density, nodeName, runtimeClassName),
// AtomicTimeDuration, and helpers (getFirstWorkerNode, hashString) are defined in density_helpers_test.go.

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
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "kubernetes.io/hostname",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{targetNode},
								},
							},
						},
					},
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

	// Build restConfig and coreClient once for in-process pod exec
	restConfig, err := clientcmd.BuildConfigFromFlags("", framework.GetKubeconfig())
	if err != nil {
		t.Fatalf("Failed to build rest config: %v", err)
	}
	restConfig.QPS = 50
	restConfig.Burst = 100

	coreClient, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("Failed to create core v1 client: %v", err)
	}

	var wg sync.WaitGroup
	metricsCh := make(chan *PythonSandboxMetrics, densityCount)

	// Provision sandboxes concurrently with a 1.8s orchestrator deployment stagger delay
	for i := range densityCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			metrics := runPythonSandboxPerf(tc, restConfig, coreClient, ns.Name, fmt.Sprintf("python-sandbox-%d", idx), targetNode)
			metricsCh <- metrics
		}(i)
		time.Sleep(1800 * time.Millisecond)
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
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "benchmark-script",
								MountPath: "/scripts",
							},
							{
								Name:      "data-vol",
								MountPath: "/data",
								ReadOnly:  true,
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
				{
					Name: "data-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "movielens-pvc",
							ReadOnly:  true,
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

func runPythonSandboxPerf(tc *framework.TestContext, restConfig *rest.Config, coreClient corev1client.CoreV1Interface, namespace, name, nodeName string) *PythonSandboxMetrics {
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

	if pyStats, err := runPythonBenchmarkExec(pyCtx, restConfig, coreClient, podID); err != nil {
		tc.Errorf("Failed to wait for python %s benchmark: %v", name, err)
	} else {
		metrics.PythonReady.Set(time.Since(startTime))
		metrics.Total.Set(time.Since(startTime))
		metrics.PythonStats = pyStats
	}

	return metrics
}

func execInPod(ctx context.Context, restConfig *rest.Config, coreClient corev1client.CoreV1Interface, podID types.NamespacedName, container string, command []string) (string, string, int, error) {
	req := coreClient.RESTClient().Post().
		Resource("pods").
		Name(podID.Name).
		Namespace(podID.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdoutBuf,
		Stderr: &stderrBuf,
		Tty:    false,
	})

	exitCode := 0
	if err != nil {
		var exitErr remoteexec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitStatus()
		} else {
			return stdoutBuf.String(), stderrBuf.String(), 0, err
		}
	}
	return stdoutBuf.String(), stderrBuf.String(), exitCode, nil
}

func waitForPythonServerReady(ctx context.Context, restConfig *rest.Config, coreClient corev1client.CoreV1Interface, podID types.NamespacedName) error {
	pollDuration := 1 * time.Second
	probeCmd := []string{"python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8888/')"}
	for {
		_, _, exitCode, err := execInPod(ctx, restConfig, coreClient, podID, "python-sandbox", probeCmd)
		if err == nil && exitCode == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("python readiness polling canceled: %w", ctx.Err())
		case <-time.After(pollDuration):
		}
	}
}

func runPythonBenchmarkExec(ctx context.Context, restConfig *rest.Config, coreClient corev1client.CoreV1Interface, podID types.NamespacedName) (map[string]any, error) {
	if err := waitForPythonServerReady(ctx, restConfig, coreClient, podID); err != nil {
		return nil, err
	}

	pyPostCmd := []string{"python3", "-c", "import urllib.request, json; req = urllib.request.Request('http://localhost:8888/execute', data=json.dumps({'command': 'python3 /scripts/benchmark_density.py'}).encode(), headers={'Content-Type': 'application/json'}); print(urllib.request.urlopen(req).read().decode())"}
	stdout, stderr, exitCode, err := execInPod(ctx, restConfig, coreClient, podID, "python-sandbox", pyPostCmd)
	if err != nil {
		return nil, fmt.Errorf("pod exec benchmark failed: %w, stdout: %s, stderr: %s", err, stdout, stderr)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("benchmark command exited with code %d: stderr: %s", exitCode, stderr)
	}

	var res struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		return nil, fmt.Errorf("failed to unmarshal REST API output: %w, raw: %s", err, stdout)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("benchmark command exited with code %d: stderr: %s", res.ExitCode, res.Stderr)
	}

	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	lastLine := lines[len(lines)-1]
	var m map[string]any
	if err := json.Unmarshal([]byte(lastLine), &m); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metrics JSON from last line (%q): %w, stdout: %s", lastLine, err, res.Stdout)
	}
	return m, nil
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
