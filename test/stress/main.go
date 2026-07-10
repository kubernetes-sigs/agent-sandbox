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
// It runs up to three phases, all optional:
//
//   - fill: create long-running background sandboxes so later phases measure a
//     cluster at scale (e.g. 10k pods on a suitably sized cluster).
//   - probe: create sandboxes at low concurrency and measure launch latency
//     cleanly, with a per-stage breakdown (controller, scheduler, kubelet,
//     status propagation).
//   - throughput: churn sandboxes (create -> ready -> delete) in a closed loop
//     capped at --max-in-flight, measuring sustained launches/second without
//     queueing on cluster capacity (maxPodsPerNode * nodes).
//
// Outputs (in --output-dir):
//
//   - summary.json: aggregate metrics per phase
//   - sandboxes.jsonl: per-sandbox lifecycle milestones (client + server timestamps)
//   - timeseries.jsonl: per-second event counts and gauges
//   - watch.jsonl.gz: full watch streams (pods, nodes, events, sandboxes) for offline analysis
//   - metrics.jsonl.gz: Prometheus samples scraped from the apiserver,
//     kube-controller-manager, kube-scheduler, the sandbox controller, and
//     kubelets, in a long format for direct DuckDB ingestion
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
	"strings"
	"sync"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config holds the test parameters.
type Config struct {
	Namespace         string        `json:"namespace"`
	OutputDir         string        `json:"outputDir"`
	Image             string        `json:"image"`
	Cleanup           bool          `json:"cleanup"`
	RecordWatch       bool          `json:"recordWatch"`
	Timeout           time.Duration `json:"-"`
	PerSandboxTimeout time.Duration `json:"-"`

	CreateConcurrency int `json:"createConcurrency"`

	FillCount int `json:"fillCount"`

	ProbeCount       int           `json:"probeCount"`
	ProbeConcurrency int           `json:"probeConcurrency"`
	ProbeInterval    time.Duration `json:"-"`

	ThroughputCount int `json:"throughputCount"`
	MaxInFlight     int `json:"maxInFlight"`

	CollectMetrics  bool          `json:"collectMetrics"`
	MetricsInterval time.Duration `json:"-"`
}

// MarshalJSON renders durations as strings for readability.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config
	return json.Marshal(struct {
		alias
		Timeout           string `json:"timeout"`
		PerSandboxTimeout string `json:"perSandboxTimeout"`
		ProbeInterval     string `json:"probeInterval"`
		MetricsInterval   string `json:"metricsInterval"`
	}{
		alias:             alias(c),
		Timeout:           c.Timeout.String(),
		PerSandboxTimeout: c.PerSandboxTimeout.String(),
		ProbeInterval:     c.ProbeInterval.String(),
		MetricsInterval:   c.MetricsInterval.String(),
	})
}

// ClusterInfo describes the cluster the test ran against.
// Nodes / PodCapacity / PreexistingPods count only worker nodes: control-plane
// nodes are excluded because sandboxes are not scheduled there.
type ClusterInfo struct {
	KubernetesVersion string `json:"kubernetesVersion"`
	Nodes             int    `json:"nodes"`
	PodCapacity       int64  `json:"podCapacity"`
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
	flag.StringVar(&cfg.Namespace, "namespace", "", "Kubernetes namespace to run the test in. If empty, a timestamped name is generated.")
	flag.StringVar(&cfg.OutputDir, "output-dir", "./stress-results", "Directory to write results to")
	flag.StringVar(&cfg.Image, "image", "debian:latest", "Container image to use for Sandboxes (must provide sh and sleep)")
	flag.BoolVar(&cfg.Cleanup, "cleanup", true, "Whether to delete the namespace at the end of the test")
	flag.BoolVar(&cfg.RecordWatch, "record-watch", true, "Whether to record all watch events to watch.jsonl.gz")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Minute, "Timeout for the entire test run")
	flag.DurationVar(&cfg.PerSandboxTimeout, "per-sandbox-timeout", 5*time.Minute, "Timeout for a single sandbox to become ready / be deleted")
	flag.IntVar(&cfg.CreateConcurrency, "create-concurrency", 20, "Number of concurrent workers creating Sandboxes (fill and throughput phases)")
	flag.IntVar(&cfg.FillCount, "fill-count", 0, "Number of long-running background Sandboxes to create before measuring (0 to skip)")
	flag.IntVar(&cfg.ProbeCount, "probe-count", 20, "Number of latency probe Sandboxes to launch (0 to skip)")
	flag.IntVar(&cfg.ProbeConcurrency, "probe-concurrency", 1, "Number of concurrent latency probes; keep low for clean latency numbers")
	flag.DurationVar(&cfg.ProbeInterval, "probe-interval", 0, "Delay between latency probes")
	flag.IntVar(&cfg.ThroughputCount, "throughput-count", 200, "Number of Sandboxes to churn through in the throughput phase (0 to skip)")
	flag.IntVar(&cfg.MaxInFlight, "max-in-flight", 50, "Maximum Sandboxes alive at once during the throughput phase; keep below spare cluster pod capacity")
	flag.BoolVar(&cfg.CollectMetrics, "collect-metrics", true, "Whether to scrape Prometheus metrics from the control plane, the sandbox controller, and kubelets to metrics.jsonl.gz")
	flag.DurationVar(&cfg.MetricsInterval, "metrics-interval", 15*time.Second, "Interval between Prometheus metrics scrapes")
	flag.Parse()

	if cfg.FillCount < 0 || cfg.ProbeCount < 0 || cfg.ThroughputCount < 0 {
		return fmt.Errorf("counts must be >= 0: fill=%d probe=%d throughput=%d", cfg.FillCount, cfg.ProbeCount, cfg.ThroughputCount)
	}
	if cfg.Timeout <= 0 || cfg.PerSandboxTimeout <= 0 {
		return fmt.Errorf("timeouts must be > 0: timeout=%v per-sandbox-timeout=%v", cfg.Timeout, cfg.PerSandboxTimeout)
	}
	if (cfg.FillCount > 0 || cfg.ThroughputCount > 0) && cfg.CreateConcurrency <= 0 {
		return fmt.Errorf("--create-concurrency must be > 0 when fill and/or throughput phases are enabled")
	}
	if cfg.ProbeCount > 0 && cfg.ProbeConcurrency <= 0 {
		return fmt.Errorf("--probe-concurrency must be > 0 when --probe-count > 0")
	}
	if cfg.ThroughputCount > 0 && cfg.MaxInFlight <= 0 {
		return fmt.Errorf("--max-in-flight must be > 0 when --throughput-count > 0")
	}

	if cfg.FillCount+cfg.ProbeCount+cfg.ThroughputCount == 0 {
		return fmt.Errorf("nothing to do: all of --fill-count, --probe-count, --throughput-count are 0")
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
	log.Printf("Starting stress test run %s: fill=%d probe=%d throughput=%d (max-in-flight=%d), writing results to %s",
		runID, cfg.FillCount, cfg.ProbeCount, cfg.ThroughputCount, cfg.MaxInFlight, cfg.OutputDir)

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
	var writeToFileChannel chan WatchEventRecord
	if cfg.RecordWatch {
		writeToFileChannel = make(chan WatchEventRecord, 4096)
		watchFilePath := filepath.Join(cfg.OutputDir, "watch.jsonl.gz")
		taskRunner.RunAsync(ctx, func(ctx context.Context) error {
			return runWriter(ctx, watchFilePath, writeToFileChannel)
		})
	}

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

	// Periodically scrape Prometheus metrics from the control plane, the
	// sandbox controller, and kubelets. Cumulative counters snapshotted on
	// an interval can be diffed per phase offline.
	var scraper *promScraper
	if cfg.CollectMetrics {
		scraper, err = newPromScraper(restConfig, filepath.Join(cfg.OutputDir, "metrics.jsonl.gz"))
		if err != nil {
			return fmt.Errorf("failed to start metrics scraper: %w", err)
		}
		defer scraper.Close()
		scraper.ScrapeAll(ctx) // baseline snapshot before any load
		taskRunner.RunPeriodic(ctx, cfg.MetricsInterval, func() error {
			scraper.ScrapeAll(ctx)
			return nil
		})
	}

	// Wait briefly for watches to establish
	time.Sleep(2 * time.Second)

	// Start progress reporter
	testStartTime := time.Now()
	taskRunner.RunPeriodic(ctx, 5*time.Second, func() error {
		counts := tracker.Snapshot()
		var line strings.Builder
		fmt.Fprintf(&line, "[progress +%s]", time.Since(testStartTime).Round(time.Second))
		for _, phase := range []Phase{PhaseFill, PhaseProbe, PhaseThroughput} {
			c, ok := counts[phase]
			if !ok {
				continue
			}
			fmt.Fprintf(&line, " %s: created=%d ready=%d deleted=%d failed=%d |",
				phase, c.Created, c.Ready, c.Deleted, c.Failed)
		}
		if writeToFileChannel != nil {
			fmt.Fprintf(&line, " watch-queue=%d/%d", len(writeToFileChannel), cap(writeToFileChannel))
		}
		log.Print(line.String())
		return nil
	})

	test := &stressTest{
		cfg:       cfg,
		tracker:   tracker,
		namespace: cfg.Namespace,
		sandboxClient: dynamicClient.Resource(schema.GroupVersionResource{
			Group:    "agents.x-k8s.io",
			Version:  "v1beta1",
			Resource: "sandboxes",
		}).Namespace(cfg.Namespace),
	}

	// Run the phases, recording wall-clock windows for throughput calculations.
	phaseDurations := make(map[Phase]time.Duration)
	var phaseErr error
	for _, phase := range []struct {
		name Phase
		fn   func(context.Context) error
	}{
		{PhaseFill, test.runFillPhase},
		{PhaseProbe, test.runProbePhase},
		{PhaseThroughput, test.runThroughputPhase},
	} {
		start := time.Now()
		if err := phase.fn(ctx); err != nil {
			phaseErr = fmt.Errorf("%s phase: %w", phase.name, err)
			phaseDurations[phase.name] = time.Since(start)
			log.Printf("aborting after error: %v", phaseErr)
			break
		}
		phaseDurations[phase.name] = time.Since(start)
	}

	// Give the watchers a moment to observe trailing events.
	if ctx.Err() == nil {
		time.Sleep(2 * time.Second)
	}

	// Final metrics snapshot so cumulative counters cover the whole run.
	if scraper != nil && ctx.Err() == nil {
		scraper.ScrapeAll(ctx)
	}

	// Write outputs even if a phase failed: partial data is still useful.
	summary := buildSummary(runID, testStartTime, cfg, clusterInfo, tracker, phaseDurations)
	if err := writeOutputs(cfg.OutputDir, summary, tracker); err != nil {
		if phaseErr == nil {
			phaseErr = err
		} else {
			log.Printf("failed to write outputs: %v", err)
		}
	}

	printReport(summary, clusterInfo)

	// Stop the watchers and wait for the watch log to be flushed,
	// even when a phase failed.
	cancel()
	waitErr := taskRunner.Wait()

	if phaseErr != nil {
		return phaseErr
	}
	return waitErr
}

// checkClusterCapacity warns when the test configuration will exceed spare cluster
// pod capacity: in that case latency and throughput results measure queueing
// for capacity rather than the sandbox launch pipeline.
//
// Phases run sequentially, so peak concurrent test pods is fill plus the
// larger of the probe/throughput in-flight caps (fill sandboxes stay up).
func checkClusterCapacity(cfg Config, info *ClusterInfo) {
	extra := 0
	if cfg.ProbeCount > 0 && cfg.ProbeConcurrency > extra {
		extra = cfg.ProbeConcurrency
	}
	if cfg.ThroughputCount > 0 && cfg.MaxInFlight > extra {
		extra = cfg.MaxInFlight
	}
	needed := cfg.FillCount + extra
	spare := int(info.PodCapacity) - info.PreexistingPods
	if spare <= 0 {
		return
	}
	switch {
	case needed > spare:
		log.Printf("WARNING: test needs up to %d concurrent pods but the cluster only has %d spare pod slots; results will measure capacity queueing, not launch performance. Reduce --fill-count/--max-in-flight or add nodes.", needed, spare)
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
		podsStr, found, err := unstructured.NestedString(node.Object, "status", "capacity", "pods")
		if err != nil || !found {
			continue
		}
		if pods, err := strconv.ParseInt(podsStr, 10, 64); err == nil {
			info.PodCapacity += pods
		}
	}

	podList, err := dynamicClient.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	for i := range podList.Items {
		nodeName, _, _ := unstructured.NestedString(podList.Items[i].Object, "spec", "nodeName")
		if nodeName == "" {
			// Ignore pods that are not scheduled to a node.
			continue
		}
		if _, onControlPlane := controlPlaneNodes[nodeName]; onControlPlane {
			// Ignore pods that are scheduled to a control-plane node.
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

func buildSummary(runID string, startTime time.Time, cfg Config, clusterInfo *ClusterInfo, tracker *Tracker, phaseDurations map[Phase]time.Duration) *Summary {
	records := tracker.Records()

	requested := map[Phase]int{
		PhaseFill:       cfg.FillCount,
		PhaseProbe:      cfg.ProbeCount,
		PhaseThroughput: cfg.ThroughputCount,
	}

	summary := &Summary{
		RunID:     runID,
		StartTime: startTime,
		EndTime:   time.Now(),
		Config:    cfg,
		Cluster:   clusterInfo,
		Phases:    make(map[Phase]*PhaseSummary),
	}

	recordsByPhase := make(map[Phase][]SandboxRecord)
	for _, record := range records {
		recordsByPhase[record.Phase] = append(recordsByPhase[record.Phase], record)
	}

	for phase, phaseRecords := range recordsByPhase {
		ps := &PhaseSummary{
			Requested:       requested[phase],
			DurationSeconds: phaseDurations[phase].Seconds(),
			Latency:         computeLatencyBreakdown(phaseRecords),
		}
		var createTimes, readyTimes []time.Time
		for i := range phaseRecords {
			rec := &phaseRecords[i]
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
		summary.Phases[phase] = ps
	}

	return summary
}

// sandboxRecordJSON builds the sandboxes.jsonl object for a record:
// zero times and empty strings are omitted, and *Ms offsets from CreateCalled
// are added for convenient offline analysis.
func sandboxRecordJSON(rec *SandboxRecord) map[string]any {
	out := map[string]any{
		"name":      rec.Name,
		"namespace": rec.Namespace,
		"phase":     rec.Phase,
	}
	if rec.Error != "" {
		out["error"] = rec.Error
	}
	if rec.PodUID != "" {
		out["podUID"] = rec.PodUID
	}
	if rec.NodeName != "" {
		out["nodeName"] = rec.NodeName
	}
	if rec.ContainerID != "" {
		out["containerID"] = rec.ContainerID
	}

	putTime := func(key string, t time.Time) {
		if !t.IsZero() {
			out[key] = t
		}
	}
	putTime("createCalled", rec.CreateCalled)
	putTime("createReturned", rec.CreateReturned)
	putTime("podCreated", rec.PodCreated)
	putTime("podScheduled", rec.PodScheduled)
	putTime("podRunning", rec.PodRunning)
	putTime("podReady", rec.PodReady)
	putTime("sandboxReady", rec.SandboxReady)
	putTime("sandboxFinished", rec.SandboxFinished)
	putTime("deleteCalled", rec.DeleteCalled)
	putTime("podDeleted", rec.PodDeleted)
	putTime("sandboxDeleted", rec.SandboxDeleted)

	putTime("serverSandboxCreated", rec.ServerSandboxCreated)
	putTime("serverPodCreated", rec.ServerPodCreated)
	putTime("serverPodScheduled", rec.ServerPodScheduled)
	putTime("serverPodReady", rec.ServerPodReady)
	putTime("serverSandboxReady", rec.ServerSandboxReady)

	putMsSinceCreate := func(key string, t time.Time) {
		if t.IsZero() || rec.CreateCalled.IsZero() {
			return
		}
		out[key] = toMs(t.Sub(rec.CreateCalled))
	}
	putMsSinceCreate("createAckMs", rec.CreateReturned)
	putMsSinceCreate("podCreatedMs", rec.PodCreated)
	putMsSinceCreate("podScheduledMs", rec.PodScheduled)
	putMsSinceCreate("podRunningMs", rec.PodRunning)
	putMsSinceCreate("podReadyMs", rec.PodReady)
	putMsSinceCreate("sandboxReadyMs", rec.SandboxReady)

	return out
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

	printBreakdown := func(b LatencyBreakdown) {
		fmt.Printf("    create ack (apiserver):        %s\n", formatLatency(b.CreateAck))
		fmt.Printf("    create -> pod created:         %s\n", formatLatency(b.CreateToPodCreated))
		fmt.Printf("    pod created -> scheduled:      %s\n", formatLatency(b.PodCreatedToScheduled))
		fmt.Printf("    scheduled -> pod running:      %s\n", formatLatency(b.ScheduledToPodRunning))
		fmt.Printf("    pod running -> pod ready:      %s\n", formatLatency(b.PodRunningToPodReady))
		fmt.Printf("    pod ready -> sandbox ready:    %s\n", formatLatency(b.PodReadyToSandboxReady))
		fmt.Printf("    END-TO-END (create -> ready):  %s\n", formatLatency(b.EndToEndReady))
	}

	for _, phase := range []Phase{PhaseFill, PhaseProbe, PhaseThroughput} {
		ps, ok := summary.Phases[phase]
		if !ok {
			continue
		}
		fmt.Printf("\n--- %s: %d requested, %d created, %d ready, %d failed (%.1fs) ---\n",
			phase, ps.Requested, ps.Created, ps.Ready, ps.Failed, ps.DurationSeconds)

		switch phase {
		case PhaseProbe:
			fmt.Println("  Launch latency breakdown:")
			printBreakdown(ps.Latency)
		default:
			fmt.Printf("  end-to-end ready latency:        %s\n", formatLatency(ps.Latency.EndToEndReady))
			fmt.Printf("  create throughput:               %s\n", formatThroughput(ps.CreateThroughput))
			fmt.Printf("  ready throughput:                %s\n", formatThroughput(ps.ReadyThroughput))
			fmt.Printf("  ready throughput per node:       %s\n", formatPerNodeRates(ps.ReadyThroughputPerNode))
		}
	}
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

// WatchEventRecord defines the schema for each line in watch.jsonl.gz.
type WatchEventRecord struct {
	Timestamp time.Time       `json:"timestamp"`
	Resource  string          `json:"resource"`
	Type      watch.EventType `json:"type"`
	Object    any             `json:"object"`
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
