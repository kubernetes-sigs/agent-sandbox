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

// stress is a load-testing harness for the Sandbox controller.
//
// It creates N sandboxes and waits for them to become Ready, recording a
// per-stage latency breakdown (controller, scheduler, kubelet, status
// propagation) plus create/ready throughput.
//
// Outputs (in --output-dir):
//
//   - summary.json: aggregate metrics
//   - sandboxes.jsonl: per-sandbox lifecycle milestones (client + server timestamps)
//   - timeseries.jsonl: per-second event counts and gauges
//   - watch.jsonl.gz: full watch streams (pods, nodes, events, sandboxes) for offline analysis
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// WatchEventRecord defines the schema for each line in watch.jsonl.gz.
type WatchEventRecord struct {
	Timestamp time.Time       `json:"timestamp"`
	Resource  string          `json:"resource"`
	Type      watch.EventType `json:"type"`
	Object    any             `json:"object"`
}

// ClusterInfo describes the cluster the test ran against.
// Nodes / PodCapacity / PreexistingPods count only worker nodes: control-plane
// nodes are excluded because sandboxes are not scheduled there.
type ClusterInfo struct {
	KubernetesVersion string `json:"kubernetesVersion"`
	Nodes             int    `json:"nodes"`
	PodCapacity       int    `json:"podCapacity"`
	PreexistingPods   int    `json:"preexistingPods"`
}

// PhaseSummary holds the aggregate results for one phase.
type PhaseSummary struct {
	Requested       int     `json:"requested"`
	Created         int     `json:"created"`
	Ready           int     `json:"ready"`
	Failed          int     `json:"failed"`
	DurationSeconds float64 `json:"durationSeconds"`

	Latency LatencyBreakdown `json:"latency"`

	CreateThroughput *ThroughputStats `json:"createThroughput,omitempty"`
	ReadyThroughput  *ThroughputStats `json:"readyThroughput,omitempty"`

	// Per-worker-node rates, alongside the raw aggregates above.
	CreateThroughputPerNode *PerNodeRates `json:"createThroughputPerNode,omitempty"`
	ReadyThroughputPerNode  *PerNodeRates `json:"readyThroughputPerNode,omitempty"`
}

// Summary is written to summary.json at the end of the test.
type Summary struct {
	RunID     string                  `json:"runID"`
	StartTime time.Time               `json:"startTime"`
	EndTime   time.Time               `json:"endTime"`
	Config    Config                  `json:"config"`
	Cluster   *ClusterInfo            `json:"cluster,omitempty"`
	Phases    map[Phase]*PhaseSummary `json:"phases"`
}

// Config holds the test parameters.
type Config struct {
	Namespace         string        `json:"namespace"`
	OutputDir         string        `json:"outputDir"`
	Image             string        `json:"image"`
	Cleanup           bool          `json:"cleanup"`
	Timeout           time.Duration `json:"timeout"`
	PerSandboxTimeout time.Duration `json:"perSandboxTimeout"`

	SandboxCount      int `json:"sandboxCount"`
	CreateConcurrency int `json:"createConcurrency"`
}

func main() {
	// Setup context that cancels on timeout or signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	var cfg Config
	flag.IntVar(&cfg.SandboxCount, "sandbox-count", 100, "Number of Sandboxes to create")
	flag.IntVar(&cfg.CreateConcurrency, "create-concurrency", 10, "Number of concurrent workers creating Sandboxes")
	flag.StringVar(&cfg.Namespace, "namespace", "", "Kubernetes namespace to run the test in. If empty, a timestamped name is generated.")
	flag.StringVar(&cfg.OutputDir, "output-dir", "./stress-results", "Directory to write results to")
	flag.BoolVar(&cfg.Cleanup, "cleanup", true, "Whether to delete the namespace at the end of the test")
	flag.StringVar(&cfg.Image, "image", "debian:latest", "Container image to use for Sandboxes (must provide sh and sleep)")
	flag.DurationVar(&cfg.Timeout, "timeout", 15*time.Minute, "Timeout for the entire test run")
	flag.DurationVar(&cfg.PerSandboxTimeout, "per-sandbox-timeout", 5*time.Minute, "Timeout waiting for sandboxes to become Ready after creates finish")
	flag.Parse()

	if cfg.SandboxCount <= 0 {
		return fmt.Errorf("--sandbox-count must be > 0")
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	// Create unique run ID and directories
	runID := time.Now().Format("20060102-150405")
	if cfg.Namespace == "" {
		cfg.Namespace = fmt.Sprintf("sandbox-stress-%s", runID)
	}

	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		return fmt.Errorf("failed to create run directory %s: %w", cfg.OutputDir, err)
	}
	log.Printf("Starting stress test run %s: creating %d Sandboxes (create-concurrency=%d), writing results to %s",
		runID, cfg.SandboxCount, cfg.CreateConcurrency, cfg.OutputDir)

	// Initialize kubernetes client config
	restConfig, err := getRestConfig()
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	restConfig.QPS = -1.0 // No client side rate-limiting

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to build dynamic client: %w", err)
	}

	clusterInfo, err := inspectCluster(ctx, restConfig, dynamicClient)
	if err != nil {
		return fmt.Errorf("failed to inspect cluster: %w", err)
	}
	log.Printf("Cluster: kubernetes %s, %d worker nodes, pod capacity %d, %d pre-existing worker pods",
		clusterInfo.KubernetesVersion, clusterInfo.Nodes, clusterInfo.PodCapacity, clusterInfo.PreexistingPods)
	checkClusterCapacity(cfg, clusterInfo)

	// Create namespace
	nsClient := dynamicClient.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"})
	nsObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name": cfg.Namespace,
			},
		},
	}
	log.Printf("Creating namespace: %s", cfg.Namespace)
	if _, err := nsClient.Create(ctx, nsObj, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create namespace %s: %w", cfg.Namespace, err)
	}

	// Clean up namespace at the end if requested
	if cfg.Cleanup {
		defer func() {
			log.Printf("Cleaning up namespace: %s", cfg.Namespace)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cleanupCancel()
			if err := nsClient.Delete(cleanupCtx, cfg.Namespace, metav1.DeleteOptions{}); err != nil {
				log.Printf("failed to delete namespace %s: %v", cfg.Namespace, err)
			}
		}()
	}

	tracker := NewTracker()
	taskRunner := NewTaskRunner(cancel)

	// Start watch recording to file
	writeToFileChannel := make(chan WatchEventRecord, 4096)
	watchFilePath := filepath.Join(cfg.OutputDir, "watch.jsonl.gz")
	taskRunner.RunAsync(ctx, func(ctx context.Context) error {
		return runWriter(ctx, watchFilePath, writeToFileChannel)
	})

	// Setup and start watchers.
	// We capture cluster-wide, we want as much data as possible,
	// and expect this test to be run on a dedicated cluster.
	gvrList := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "", Version: "v1", Resource: "nodes"},
		{Group: "", Version: "v1", Resource: "events"},
		{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"},
	}

	for _, gvr := range gvrList {
		taskRunner.RunAsync(ctx, func(ctx context.Context) error {
			return watchResource(ctx, dynamicClient, gvr, func(event WatchEventRecord) error {
				// Update milestone tracking first: it is cheap and time-sensitive,
				// while the file write may block briefly on the writer.
				if u, ok := event.Object.(*unstructured.Unstructured); ok {
					tracker.HandleWatchEvent(gvr.Resource, event.Type, u)
				} else if event.Object != nil {
					return fmt.Errorf("unhandled type in event %T", event.Object)
				}

				if writeToFileChannel != nil {
					select {
					case writeToFileChannel <- event:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			})
		})
	}

	// Wait briefly for watches to establish
	time.Sleep(2 * time.Second)

	// Start progress reporter
	testStartTime := time.Now()
	taskRunner.RunPeriodic(ctx, 5*time.Second, func() error {
		counts := tracker.Snapshot()[PhaseCreate]
		line := fmt.Sprintf("[progress +%s] created=%d ready=%d failed=%d",
			time.Since(testStartTime).Round(time.Second), counts.Created, counts.Ready, counts.Failed)
		if writeToFileChannel != nil {
			line += fmt.Sprintf(" | watch-queue=%d/%d", len(writeToFileChannel), cap(writeToFileChannel))
		}
		log.Print(line)
		return nil
	})

	sandboxClient := dynamicClient.Resource(schema.GroupVersionResource{
		Group:    "agents.x-k8s.io",
		Version:  "v1beta1",
		Resource: "sandboxes",
	}).Namespace(cfg.Namespace)

	phaseStart := time.Now()
	phaseErr := runCreatePhase(ctx, cfg, tracker, sandboxClient)
	phaseDuration := time.Since(phaseStart)
	if phaseErr != nil {
		log.Printf("create phase error: %v", phaseErr)
	}

	// Give the watchers a moment to observe trailing events.
	if ctx.Err() == nil {
		time.Sleep(2 * time.Second)
	}

	// Write outputs even if the phase failed: partial data is still useful.
	summary := buildSummary(runID, testStartTime, cfg, clusterInfo, tracker, phaseDuration)
	if err := writeOutputs(cfg.OutputDir, summary, tracker); err != nil {
		if phaseErr == nil {
			phaseErr = err
		} else {
			log.Printf("failed to write outputs: %v", err)
		}
	}

	printReport(summary, clusterInfo)

	// Stop the watchers and wait for the watch log to be flushed,
	// even when the phase failed.
	cancel()
	waitErr := taskRunner.Wait()

	if phaseErr != nil {
		return phaseErr
	}
	return waitErr
}

// runCreatePhase creates cfg.SandboxCount long-running sandboxes and waits for
// them to become Ready. Readiness is the measured event; sandboxes sleep forever
// so Finished latency is not conflated with the workload duration.
func runCreatePhase(ctx context.Context, cfg Config, tracker *Tracker, sandboxClient dynamic.ResourceInterface) error {
	log.Printf("[create] creating %d sandboxes (create-concurrency=%d)", cfg.SandboxCount, cfg.CreateConcurrency)

	names := make([]types.NamespacedName, 0, cfg.SandboxCount)
	for i := range cfg.SandboxCount {
		names = append(names, types.NamespacedName{Name: fmt.Sprintf("stress-%d", i), Namespace: cfg.Namespace})
	}

	if _, err := ForkJoin(ctx, names, cfg.CreateConcurrency, func(id types.NamespacedName) (struct{}, error) {
		// The command traps SIGTERM and exits immediately: a bare `sleep` as PID 1
		// gets no default SIGTERM disposition, so the kubelet would wait out the full
		// grace period and SIGKILL (observed as exit code 137 and ~1s of extra
		// deletion latency). The `& wait` is required because sh does not run traps
		// while a foreground child is running.
		// terminationGracePeriodSeconds=1 is the backstop if the trap fails.
		sandbox := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "agents.x-k8s.io/v1beta1",
				"kind":       "Sandbox",
				"metadata": map[string]any{
					"name":      id.Name,
					"namespace": id.Namespace,
				},
				"spec": map[string]any{
					"podTemplate": map[string]any{
						"spec": map[string]any{
							"restartPolicy":                 "Never",
							"terminationGracePeriodSeconds": int64(1),
							"containers": []any{
								map[string]any{
									"name":            "main",
									"image":           cfg.Image,
									"imagePullPolicy": "IfNotPresent",
									"command":         []string{"sh", "-c", "trap 'exit 0' TERM INT; sleep 5 & wait"},
								},
							},
						},
					},
				},
			},
		}

		tracker.Register(id, PhaseCreate)
		_, err := sandboxClient.Create(ctx, sandbox, metav1.CreateOptions{})
		tracker.MarkCreateReturned(id, err)
		if err != nil {
			log.Printf("[create] failed to create sandbox %s: %v", id.Name, err)
		}
		// Per-sandbox create failures are recorded; do not abort the phase.
		return struct{}{}, nil
	}); err != nil {
		return err
	}

	log.Printf("[create] all create workers finished; waiting for Ready...")

	lastReady := -1
	lastProgress := time.Now()
	for {
		counts := tracker.Snapshot()[PhaseCreate]
		if counts.Created == 0 {
			return fmt.Errorf("[create] all %d sandbox creations failed", counts.Failed)
		}
		if counts.Ready >= counts.Created {
			log.Printf("[create] all %d created sandboxes are Ready (%d failed to create)", counts.Created, counts.Failed)
			return nil
		}
		if counts.Ready != lastReady {
			lastReady = counts.Ready
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) > cfg.PerSandboxTimeout {
			return fmt.Errorf("[create] stalled: %d/%d sandboxes Ready with no progress for %v", counts.Ready, counts.Created, cfg.PerSandboxTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// checkClusterCapacity warns when the test configuration will exceed spare cluster
// pod capacity: in that case latency and throughput results measure queueing
// for capacity rather than the sandbox launch pipeline.
func checkClusterCapacity(cfg Config, info *ClusterInfo) {
	needed := cfg.SandboxCount
	spare := info.PodCapacity - info.PreexistingPods
	if spare <= 0 {
		log.Printf("WARNING: cluster has no spare pod slots.")
		return
	}
	switch {
	case needed > spare:
		log.Printf("WARNING: test needs up to %d concurrent pods but the cluster only has %d spare pod slots; results will measure capacity queueing, not launch performance. Reduce --sandbox-count or add nodes.", needed, spare)
	case needed > spare*9/10:
		log.Printf("WARNING: test needs up to %d concurrent pods, over 90%% of the %d spare pod slots; scheduling may interfere with measurements.", needed, spare)
	}
}

// inspectCluster records the apiserver version and counts worker-node pod
// capacity / pre-existing pods. Control-plane nodes are excluded: their pod
// slots are not available to sandboxes, and including them would understate
// how close the test is to the capacity cliff.
func inspectCluster(ctx context.Context, restConfig *rest.Config, dynamicClient dynamic.Interface) (*ClusterInfo, error) {
	info := &ClusterInfo{}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}
	version, err := discoveryClient.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("getting server version: %w", err)
	}
	info.KubernetesVersion = version.GitVersion

	nodeList, err := dynamicClient.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	controlPlaneNodes := make(map[string]struct{})
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if isControlPlaneNode(node) {
			controlPlaneNodes[node.GetName()] = struct{}{}
			continue
		}
		info.Nodes++
		podCapacityString, found, err := unstructured.NestedString(node.Object, "status", "capacity", "pods")
		if err != nil || !found {
			continue
		}
		if podCapacity, err := strconv.Atoi(podCapacityString); err == nil {
			info.PodCapacity += podCapacity
		}
	}

	podList, err := dynamicClient.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	for i := range podList.Items {
		nodeName, _, _ := unstructured.NestedString(podList.Items[i].Object, "spec", "nodeName")
		if _, onControlPlane := controlPlaneNodes[nodeName]; onControlPlane {
			continue
		}
		info.PreexistingPods++
	}

	return info, nil
}

// isControlPlaneNode reports whether a node carries a control-plane / master role label.
func isControlPlaneNode(u *unstructured.Unstructured) bool {
	labels := u.GetLabels()
	if labels == nil {
		return false
	}
	if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	if _, ok := labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	return false
}

func buildSummary(runID string, startTime time.Time, cfg Config, clusterInfo *ClusterInfo, tracker *Tracker, phaseDuration time.Duration) *Summary {
	records := tracker.Records()

	summary := &Summary{
		RunID:     runID,
		StartTime: startTime,
		EndTime:   time.Now(),
		Config:    cfg,
		Cluster:   clusterInfo,
		Phases:    make(map[Phase]*PhaseSummary),
	}

	ps := &PhaseSummary{
		Requested:       cfg.SandboxCount,
		DurationSeconds: phaseDuration.Seconds(),
		Latency:         computeLatencyBreakdown(records),
	}
	var createTimes, readyTimes []time.Time
	for i := range records {
		rec := &records[i]
		if !rec.CreateReturned.IsZero() {
			ps.Created++
			createTimes = append(createTimes, rec.CreateReturned)
		}
		if !rec.SandboxReady.IsZero() {
			ps.Ready++
			readyTimes = append(readyTimes, rec.SandboxReady)
		}
		if rec.Error != "" {
			ps.Failed++
		}
	}
	ps.CreateThroughput = computeThroughputStats(createTimes)
	ps.ReadyThroughput = computeThroughputStats(readyTimes)
	if clusterInfo != nil {
		ps.CreateThroughputPerNode = ps.CreateThroughput.perNode(clusterInfo.Nodes)
		ps.ReadyThroughputPerNode = ps.ReadyThroughput.perNode(clusterInfo.Nodes)
	}
	summary.Phases[PhaseCreate] = ps

	return summary
}

// sandboxRecordExport is the sandboxes.jsonl shape: SandboxRecord fields
// (flattened via embedding; zeros omitted by omitempty/omitzero tags) plus
// *Ms offsets from CreateCalled for offline analysis.
type sandboxRecordExport struct {
	*SandboxRecord
	CreateAckMs    *float64 `json:"createAckMs,omitempty"`
	PodCreatedMs   *float64 `json:"podCreatedMs,omitempty"`
	PodScheduledMs *float64 `json:"podScheduledMs,omitempty"`
	PodRunningMs   *float64 `json:"podRunningMs,omitempty"`
	PodReadyMs     *float64 `json:"podReadyMs,omitempty"`
	SandboxReadyMs *float64 `json:"sandboxReadyMs,omitempty"`
}

func sandboxRecordJSON(rec *SandboxRecord) sandboxRecordExport {
	msSinceCreate := func(t time.Time) *float64 {
		if t.IsZero() || rec.CreateCalled.IsZero() {
			return nil
		}
		ms := toMs(t.Sub(rec.CreateCalled))
		return &ms
	}
	return sandboxRecordExport{
		SandboxRecord:  rec,
		CreateAckMs:    msSinceCreate(rec.CreateReturned),
		PodCreatedMs:   msSinceCreate(rec.PodCreated),
		PodScheduledMs: msSinceCreate(rec.PodScheduled),
		PodRunningMs:   msSinceCreate(rec.PodRunning),
		PodReadyMs:     msSinceCreate(rec.PodReady),
		SandboxReadyMs: msSinceCreate(rec.SandboxReady),
	}
}

func writeOutputs(outputDir string, summary *Summary, tracker *Tracker) error {
	records := tracker.Records()
	slices.SortFunc(records, func(a, b SandboxRecord) int { return a.CreateCalled.Compare(b.CreateCalled) })

	// Per-sandbox milestone records.
	recordsFile, err := os.Create(filepath.Join(outputDir, "sandboxes.jsonl"))
	if err != nil {
		return fmt.Errorf("failed to create sandboxes.jsonl: %w", err)
	}
	defer recordsFile.Close()
	encoder := json.NewEncoder(recordsFile)
	for i := range records {
		if err := encoder.Encode(sandboxRecordJSON(&records[i])); err != nil {
			return fmt.Errorf("failed to encode sandbox record: %w", err)
		}
	}

	// Per-second timeseries.
	timeseriesFile, err := os.Create(filepath.Join(outputDir, "timeseries.jsonl"))
	if err != nil {
		return fmt.Errorf("failed to create timeseries.jsonl: %w", err)
	}
	defer timeseriesFile.Close()
	timeseriesEncoder := json.NewEncoder(timeseriesFile)
	for _, point := range buildTimeseries(records) {
		if err := timeseriesEncoder.Encode(point); err != nil {
			return fmt.Errorf("failed to encode timeseries point: %w", err)
		}
	}

	// Aggregate summary.
	summaryBytes, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal summary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "summary.json"), summaryBytes, 0644); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}

	return nil
}

func formatLatency(stats *LatencyStats) string {
	if stats == nil {
		return "n=0"
	}
	return fmt.Sprintf("n=%-5d min=%-8s mean=%-8s p50=%-8s p90=%-8s p99=%-8s max=%s",
		stats.Count, formatMs(stats.MinMs), formatMs(stats.MeanMs), formatMs(stats.P50Ms), formatMs(stats.P90Ms), formatMs(stats.P99Ms), formatMs(stats.MaxMs))
}

func formatMs(ms float64) string {
	if ms >= 10000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

func formatThroughput(stats *ThroughputStats) string {
	if stats == nil {
		return "n/a"
	}
	return fmt.Sprintf("overall=%.2f/s steady=%.2f/s best10s=%.2f/s best60s=%.2f/s (n=%d over %.1fs)",
		stats.OverallPerSecond, stats.SteadyStatePerSecond, stats.Best10sPerSecond, stats.Best60sPerSecond, stats.Count, stats.DurationSeconds)
}

func formatPerNodeRates(rates *PerNodeRates) string {
	if rates == nil {
		return "n/a"
	}
	return fmt.Sprintf("overall=%.2f/s steady=%.2f/s best10s=%.2f/s best60s=%.2f/s (%d worker nodes)",
		rates.OverallPerSecond, rates.SteadyStatePerSecond, rates.Best10sPerSecond, rates.Best60sPerSecond, rates.WorkerNodes)
}

func printReport(summary *Summary, clusterInfo *ClusterInfo) {
	fmt.Println("\n================= STRESS TEST RESULTS =================")
	if clusterInfo != nil {
		fmt.Printf("Cluster: kubernetes %s, %d worker nodes, pod capacity %d, %d pre-existing worker pods\n",
			clusterInfo.KubernetesVersion, clusterInfo.Nodes, clusterInfo.PodCapacity, clusterInfo.PreexistingPods)
	}

	ps, ok := summary.Phases[PhaseCreate]
	if !ok {
		fmt.Println("(no results)")
		fmt.Println("=======================================================")
		return
	}
	fmt.Printf("\n--- create: %d requested, %d created, %d ready, %d failed (%.1fs) ---\n",
		ps.Requested, ps.Created, ps.Ready, ps.Failed, ps.DurationSeconds)
	fmt.Println("  Launch latency breakdown:")
	b := ps.Latency
	fmt.Printf("    create ack (apiserver):        %s\n", formatLatency(b.CreateAck))
	fmt.Printf("    create -> pod created:         %s\n", formatLatency(b.CreateToPodCreated))
	fmt.Printf("    pod created -> scheduled:      %s\n", formatLatency(b.PodCreatedToScheduled))
	fmt.Printf("    scheduled -> pod running:      %s\n", formatLatency(b.ScheduledToPodRunning))
	fmt.Printf("    pod running -> pod ready:      %s\n", formatLatency(b.PodRunningToPodReady))
	fmt.Printf("    pod ready -> sandbox ready:    %s\n", formatLatency(b.PodReadyToSandboxReady))
	fmt.Printf("    END-TO-END (create -> ready):  %s\n", formatLatency(b.EndToEndReady))
	fmt.Printf("  create throughput:               %s\n", formatThroughput(ps.CreateThroughput))
	fmt.Printf("  ready throughput:                %s\n", formatThroughput(ps.ReadyThroughput))
	fmt.Printf("  ready throughput per node:       %s\n", formatPerNodeRates(ps.ReadyThroughputPerNode))
	fmt.Println("\n=======================================================")
	fmt.Println("Detailed outputs: summary.json, sandboxes.jsonl, timeseries.jsonl, watch.jsonl.gz")
}
func getRestConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = "bin/KUBECONFIG"
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err == nil {
		return config, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

// watchResource will watch the given resource until the context is cancelled, or the callback function returns an error.
func watchResource(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, callback func(event WatchEventRecord) error) error {
	var resourceVersion string

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		listOptions := metav1.ListOptions{
			Watch:           true,
			ResourceVersion: resourceVersion,
		}

		watcher, err := dynamicClient.Resource(gvr).Watch(ctx, listOptions)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				// If the resourceVersion is too old (410 Gone), reset so we can re-establish the watch.
				if apiStatus, ok := err.(apierrors.APIStatus); ok && apiStatus.Status().Code == 410 {
					resourceVersion = ""
				}

				log.Printf("watch error for %v, retrying: %v", gvr, err)
				time.Sleep(1 * time.Second)
				continue
			}
		}

	innerLoop:
		for {
			select {
			case <-ctx.Done():
				watcher.Stop()
				return ctx.Err()
			case event, ok := <-watcher.ResultChan():
				if !ok {
					break innerLoop
				}

				if event.Type == watch.Error {
					log.Printf("watch event error for %v, resetting resource version: %v", gvr, event.Object)
					resourceVersion = ""
					watcher.Stop()
					break innerLoop
				}

				if event.Object != nil {
					if u, ok := event.Object.(metav1.Object); ok {
						resourceVersion = u.GetResourceVersion()
					} else {
						return fmt.Errorf("unhandled type in event %T", event.Object)
					}
				}

				rec := WatchEventRecord{
					Timestamp: time.Now(),
					Resource:  gvr.Resource,
					Type:      event.Type,
					Object:    event.Object,
				}

				if err := callback(rec); err != nil {
					return err
				}
			}
		}
	}
}

// runWriter drains eventChan to a gzip-compressed JSONL file.
// The full watch stream (particularly pods and events) is large at scale, so we compress it.
func runWriter(ctx context.Context, filePath string, eventChan <-chan WatchEventRecord) error {
	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create watch file %s: %w", filePath, err)
	}
	defer f.Close()

	bufWriter := bufio.NewWriterSize(f, 1<<20)
	defer bufWriter.Flush()

	gzWriter := gzip.NewWriter(bufWriter)
	defer gzWriter.Close()

	encoder := json.NewEncoder(gzWriter)

	for {
		select {
		case event := <-eventChan:
			if err := encoder.Encode(event); err != nil {
				return fmt.Errorf("failed to encode event: %w", err)
			}
		case <-ctx.Done():
			// Drain any events that are already queued before exiting.
			for {
				select {
				case event := <-eventChan:
					if err := encoder.Encode(event); err != nil {
						return fmt.Errorf("failed to encode event: %w", err)
					}
				default:
					return ctx.Err()
				}
			}
		}
	}
}

// TaskRunner manages multiple tasks that are run in parallel,
// dealing with cancelled context and collecting errors.
type TaskRunner struct {
	onError func()

	mutex sync.Mutex
	tasks []*parallelTask
}

func NewTaskRunner(onError func()) *TaskRunner {
	return &TaskRunner{
		onError: onError,
	}
}

type parallelTask struct {
	mutex sync.Mutex
	done  bool
	err   error
}

// RunAsync runs the given function asynchronously.
// Note that ctx is passed through, fn must honor context cancellation.
func (r *TaskRunner) RunAsync(ctx context.Context, fn func(ctx context.Context) error) {
	task := &parallelTask{}

	r.mutex.Lock()
	r.tasks = append(r.tasks, task)
	r.mutex.Unlock()

	go func() {
		err := fn(ctx)

		task.mutex.Lock()
		task.done = true
		task.err = err
		task.mutex.Unlock()

		if err != nil {
			r.onError()
		}
	}()
}

func ForkJoin[K comparable, V any](ctx context.Context, items []K, concurrency int, fn func(item K) (V, error)) (map[K]V, error) {
	var mutex sync.Mutex
	var errs []error
	results := make(map[K]V, len(items))

	if concurrency <= 0 {
		concurrency = 1
	}

	var wg sync.WaitGroup
	jobs := make(chan int, concurrency)

	for w := 0; w < concurrency; w++ {
		wg.Go(func() {
			for i := range jobs {
				k := items[i]
				select {
				case <-ctx.Done():
					return
				default:
					v, err := fn(k)
					mutex.Lock()
					if err != nil {
						errs = append(errs, err)
					} else {
						results[k] = v
					}
					mutex.Unlock()
				}
			}
		})
	}

	for i := range items {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			return nil, ctx.Err()
		}
	}

	close(jobs)
	wg.Wait()
	if len(errs) > 0 {
		return results, errors.Join(errs...)
	}
	return results, nil
}

// RunPeriodic runs the given function periodically until the context is done,
// or until the function returns an error.
func (r *TaskRunner) RunPeriodic(ctx context.Context, interval time.Duration, fn func() error) {
	task := &parallelTask{}

	r.mutex.Lock()
	r.tasks = append(r.tasks, task)
	r.mutex.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var err error

	tickLoop:
		for {
			select {
			case <-ctx.Done():
				err = ctx.Err()
				break tickLoop
			case <-ticker.C:
				err = fn()
				if err != nil {
					break tickLoop
				}
			}
		}

		task.mutex.Lock()
		task.done = true
		task.err = err
		task.mutex.Unlock()

		if err != nil {
			r.onError()
		}
	}()
}

// Error returns the errors encountered by the tasks.
func (r *TaskRunner) Error() error {
	var errs []error

	r.mutex.Lock()
	defer r.mutex.Unlock()

	for _, task := range r.tasks {
		task.mutex.Lock()
		if task.err != nil {
			if !errors.Is(task.err, context.Canceled) && !errors.Is(task.err, context.DeadlineExceeded) {
				errs = append(errs, task.err)
			}
		}
		task.mutex.Unlock()
	}

	return errors.Join(errs...)
}

// Wait waits for all tasks to complete (with no deadline or cancellation).
func (r *TaskRunner) Wait() error {
	for {
		r.mutex.Lock()
		allDone := true
		for _, task := range r.tasks {
			task.mutex.Lock()
			if !task.done {
				allDone = false
				task.mutex.Unlock()
				break
			}
			task.mutex.Unlock()
		}
		r.mutex.Unlock()

		if allDone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return r.Error()
}
