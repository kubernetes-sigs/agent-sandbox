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
// It runs an ordered list of phases (--phases), for example:
//
//   - fill: long-running background sandboxes so later phases measure a cluster at scale
//   - fill-pct:N: like fill, but tops the cluster up to N% total worker pod
//     utilization (counting pre-existing pods and earlier fills), so later
//     phases measure a cluster running near capacity
//   - probe: low-concurrency launches measuring clean per-sandbox launch latency
//   - throughput-mif:N: closed-loop churn capped at N in-flight, measuring sustained ready/sec
//
// A phase name is a kind plus optional hyphen-separated key:value arguments;
// every kind accepts label:<x> (e.g. throughput-mif:400-label:hot) so a run
// that repeats a kind keeps distinct phase names in reports. See parsePhase.
//
// Outputs (in --output-dir):
//
//   - summary.json: aggregate metrics per phase (ordered list)
//   - sandboxes.jsonl: per-sandbox lifecycle milestones (client + server timestamps)
//   - timeseries.jsonl: per-second event counts and gauges
//   - watch.jsonl.gz: full watch streams (pods, nodes, events, sandboxes) for offline analysis
//   - metrics.jsonl.gz: Prometheus samples scraped from the apiserver,
//     kube-controller-manager, kube-scheduler, the sandbox controller, and
//     kubelets (optional; see --collect-metrics)
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
	"maps"
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
	// Number is the 1-based index of this entry in Summary.Phases / Config.Phases.
	Number PhaseNumber `json:"phaseNumber"`
	Name   string      `json:"name"`
	// Kind is the phase's base kind (fill, probe, throughput, ...): Name
	// carries the phase's arguments (probe-label:x), so consumers picking
	// kind-specific output must use Kind, not Name.
	Kind            PhaseName `json:"kind,omitempty"`
	Requested       int       `json:"requested"`
	Created         int       `json:"created"`
	Ready           int       `json:"ready"`
	Failed          int       `json:"failed"`
	DurationSeconds float64   `json:"durationSeconds"`
	// StartOffsetSeconds is the phase's start relative to Summary.StartTime.
	StartOffsetSeconds float64 `json:"startOffsetSeconds"`

	Latency LatencyBreakdown `json:"latency"`

	// TimeToAllReadySeconds is first Create call -> last observed Ready, set
	// only when every record in the phase became Ready. It is the headline
	// metric for the claims-warm burst (how long until ALL claims are Ready).
	TimeToAllReadySeconds *float64 `json:"timeToAllReadySeconds,omitempty"`

	CreateThroughput *ThroughputStats `json:"createThroughput,omitempty"`
	ReadyThroughput  *ThroughputStats `json:"readyThroughput,omitempty"`

	// Per-worker-node rates, alongside the raw aggregates above.
	CreateThroughputPerNode *PerNodeRates `json:"createThroughputPerNode,omitempty"`
	ReadyThroughputPerNode  *PerNodeRates `json:"readyThroughputPerNode,omitempty"`

	// SustainedWindows holds the claims-warm-sustained phase's rolling
	// per-10s-window create->Ready stats (arrival-time bucketed): the
	// "latency holds over time" evidence. Set only for that phase.
	SustainedWindows []WindowedLatency `json:"sustainedWindows,omitempty"`
}

// Summary is written to summary.json at the end of the test.
type Summary struct {
	RunID     string          `json:"runID"`
	StartTime time.Time       `json:"startTime"`
	EndTime   time.Time       `json:"endTime"`
	Config    Config          `json:"config"`
	Cluster   *ClusterInfo    `json:"cluster,omitempty"`
	Phases    []*PhaseSummary `json:"phases"` // ordered by run sequence
	// APFVerification records which APF flow schema / priority level the
	// harness's claim POSTs classified into (preflight dry-run; claims
	// phases only). The create-ack calibration contract is Exempt=true.
	APFVerification *APFVerification `json:"apfVerification,omitempty"`
}

// Config holds the test parameters.
type Config struct {
	Namespace         string        `json:"namespace"`
	OutputDir         string        `json:"outputDir"`
	Image             string        `json:"image"`
	Cleanup           bool          `json:"cleanup"`
	Timeout           time.Duration `json:"timeoutNanos"`
	PerSandboxTimeout time.Duration `json:"perSandboxTimeoutNanos"`

	CreateConcurrency int `json:"createConcurrency"`

	// Phases is the ordered list of phase names to run (see package comment).
	Phases []string `json:"phases"`

	// FillPerNode sizes the plain fill phase relative to the cluster: fill
	// creates FillPerNode * worker-node-count sandboxes. (fill-pct:N phases
	// size themselves from pod capacity instead; each phase's resolved size
	// is recorded in its summary.json entry as "requested".)
	FillPerNode int `json:"fillPerNode"`

	ProbeCount       int           `json:"probeCount"`
	ProbeConcurrency int           `json:"probeConcurrency"`
	ProbeInterval    time.Duration `json:"probeIntervalNanos"`

	ThroughputCount int `json:"throughputCount"`
	// ThroughputMinSeconds is the minimum duration of each throughput level;
	// levels keep churning past ThroughputCount until this much time has
	// elapsed (0 = count-based only).
	ThroughputMinSeconds float64 `json:"throughputMinSeconds"`

	// ClaimsWarmCount sizes the claims-warm phase: the SandboxWarmPool replica
	// count and the number of SandboxClaims fired simultaneously against it.
	// The cluster needs ClaimsWarmCount spare pod slots for the pool itself,
	// and up to ~2x transiently while the pool replenishes claimed sandboxes.
	ClaimsWarmCount int `json:"claimsWarmCount"`

	// claims-warm-sustained parameters (see sustained.go for the full model).
	// SustainedRate is the target claim arrival rate in claims/s (Poisson).
	SustainedRate float64 `json:"sustainedRate"`
	// SustainedSeconds is the duration of the arrival window in seconds.
	SustainedSeconds float64 `json:"sustainedSeconds"`
	// ClaimDwell is how long each sustained claim is held after Ready before
	// it is deleted (steady-state churn realism: adoption + refill + teardown
	// all run concurrently).
	ClaimDwell time.Duration `json:"claimDwellNanos"`
	// SustainedNamespaces spreads the sustained phase's pools and claims
	// round-robin across N pre-created namespaces (shard testing).
	SustainedNamespaces int `json:"sustainedNamespaces"`
	// SustainedPoolHeadroom sizes each namespace's warm pool:
	// replicas = ceil(rate/namespaces * headroom-seconds). It must cover the
	// controller's worst-case refill latency (any replenishment delay + cold
	// launch p99), or the pool runs dry and claims cold-start.
	SustainedPoolHeadroom time.Duration `json:"sustainedPoolHeadroomNanos"`
	// SustainedLifecycleBudget is the assumed per-claim ready+delete pipeline
	// time (everything outside the dwell) used when estimating the phase's
	// peak concurrent pods in resolvePhases. If the cluster's
	// Ready/delete path is slower than this under load, raise it so the
	// capacity check demands enough headroom to keep queueing out of the
	// latency measurement.
	SustainedLifecycleBudget time.Duration `json:"sustainedLifecycleBudgetNanos"`

	// ClientConnections shards the harness's own mutating requests across N
	// HTTP/2 connections (1 = share the watches' single connection, the
	// historical behavior). The apiserver caps ~100 concurrent streams per
	// connection, so wide create bursts on one connection queue inside the
	// harness and inflate measured create-ack; see clientconns.go.
	ClientConnections int `json:"clientConnections"`

	CollectMetrics  bool          `json:"collectMetrics"`
	MetricsInterval time.Duration `json:"metricsIntervalNanos"`

	// ProfileAPIServer captures a kube-apiserver CPU profile during each
	// throughput level (pprof-apiserver-<phase>.pprof).
	ProfileAPIServer bool `json:"profileAPIServer"`

	// ProfileController captures agent-sandbox-controller CPU and heap
	// profiles DURING the claims-warm burst (pprof-controller-*.pprof);
	// requires the controller to run with --enable-pprof / --enable-pprof-debug.
	ProfileController bool `json:"profileController"`
}

// GVRs shared by the watchers and the claims-warm phase (the extension GVRs
// exist only when the extensions controller is deployed; see hasClaimsPhase).
var (
	gvrNamespaces       = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	gvrSandboxes        = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
	gvrSandboxTemplates = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxtemplates"}
	gvrSandboxWarmPools = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"}
	gvrSandboxClaims    = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
)

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
	flag.BoolVar(&cfg.Cleanup, "cleanup", true, "Whether to delete the namespace at the end of the test")
	flag.StringVar(&cfg.Image, "image", "debian:latest", "Container image to use for Sandboxes (must provide sh and sleep)")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Minute, "Timeout for the entire test run")
	flag.DurationVar(&cfg.PerSandboxTimeout, "per-sandbox-timeout", 5*time.Minute, "Timeout for a single sandbox to become ready / be deleted")
	flag.IntVar(&cfg.CreateConcurrency, "create-concurrency", 20, "Number of concurrent workers creating Sandboxes (fill and throughput phases)")
	phasesFlag := flag.String("phases", "probe,throughput-mif:50", "Comma-separated phase names to run in order (fill, fill-pct:N, probe, claims-warm, claims-warm-sustained, throughput-mif:N); phases accept hyphen-separated key:value arguments, e.g. throughput-mif:400-label:hot")
	flag.IntVar(&cfg.FillPerNode, "fill-per-node", 10, "Number of long-running background Sandboxes per worker node for the fill phase")
	flag.IntVar(&cfg.ProbeCount, "probe-count", 20, "Number of latency probe Sandboxes for the probe phase")
	flag.IntVar(&cfg.ProbeConcurrency, "probe-concurrency", 1, "Number of concurrent latency probes; keep low for clean latency numbers")
	flag.DurationVar(&cfg.ProbeInterval, "probe-interval", 0, "Delay between latency probes")
	flag.IntVar(&cfg.ThroughputCount, "throughput-count", 200, "Number of Sandboxes to churn per throughput phase (before --throughput-min-seconds)")
	flag.Float64Var(&cfg.ThroughputMinSeconds, "throughput-min-seconds", 45, "Minimum duration of each throughput phase; levels churn beyond -throughput-count until this much time has elapsed (0 = count-based only)")
	flag.IntVar(&cfg.ClaimsWarmCount, "claims-warm-count", 300, "Warm pool size and number of simultaneous SandboxClaims for the claims-warm phase (requires the extensions controller)")
	flag.Float64Var(&cfg.SustainedRate, "sustained-rate", 300, "Target SandboxClaim arrival rate in claims/s (Poisson-jittered) for the claims-warm-sustained phase (requires the extensions controller)")
	flag.Float64Var(&cfg.SustainedSeconds, "sustained-seconds", 60, "Duration of the claims-warm-sustained arrival window in seconds")
	flag.DurationVar(&cfg.ClaimDwell, "claim-dwell", 5*time.Second, "How long each sustained claim is held after Ready before deletion")
	flag.IntVar(&cfg.SustainedNamespaces, "sustained-namespaces", 1, "Spread the sustained phase's pools and claims across N pre-created namespaces (1 = run in the test namespace)")
	flag.DurationVar(&cfg.SustainedPoolHeadroom, "sustained-pool-headroom", 10*time.Second, "Warm pool sizing for the sustained phase: each namespace's pool has ceil(rate/namespaces * headroom-seconds) replicas; must cover the controller's worst-case refill latency")
	flag.DurationVar(&cfg.SustainedLifecycleBudget, "sustained-lifecycle-budget", 5*time.Second, "Assumed per-claim ready+delete pipeline time (beyond --claim-dwell) used to size the sustained phase's pod-capacity estimate; raise it if the cluster's Ready/delete path is slower under load")
	flag.IntVar(&cfg.ClientConnections, "client-connections", 1, "Shard the harness's mutating API requests across N HTTP/2 connections; 1 = single connection shared with watches (historical behavior, subject to the apiserver's ~100-streams-per-connection cap)")
	flag.BoolVar(&cfg.CollectMetrics, "collect-metrics", true, "Whether to scrape Prometheus metrics from the control plane, the sandbox controller, and kubelets to metrics.jsonl.gz")
	flag.DurationVar(&cfg.MetricsInterval, "metrics-interval", 15*time.Second, "Interval between Prometheus metrics scrapes")
	flag.BoolVar(&cfg.ProfileAPIServer, "profile-apiserver", true, "Capture a kube-apiserver CPU profile during each throughput level (pprof-apiserver-<phase>.pprof)")
	flag.BoolVar(&cfg.ProfileController, "profile-controller", true, "Capture agent-sandbox-controller CPU+heap profiles during the claims-warm burst (best-effort; the controller must run with --enable-pprof / --enable-pprof-debug)")
	flag.Parse()

	for part := range strings.SplitSeq(*phasesFlag, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		cfg.Phases = append(cfg.Phases, name)
	}
	if len(cfg.Phases) == 0 {
		return fmt.Errorf("--phases must list at least one phase")
	}
	// Parse the phase list and validate each phase's flag-derived
	// configuration up front, before any cluster interaction.
	phases, err := parsePhases(cfg.Phases, cfg)
	if err != nil {
		return err
	}
	if cfg.Timeout <= 0 || cfg.PerSandboxTimeout <= 0 {
		return fmt.Errorf("timeouts must be > 0: timeout=%v per-sandbox-timeout=%v", cfg.Timeout, cfg.PerSandboxTimeout)
	}
	if cfg.FillPerNode < 0 || cfg.ProbeCount < 0 || cfg.ThroughputCount < 0 || cfg.ClaimsWarmCount < 0 {
		return fmt.Errorf("counts must be >= 0: fill-per-node=%d probe=%d throughput=%d claims-warm=%d", cfg.FillPerNode, cfg.ProbeCount, cfg.ThroughputCount, cfg.ClaimsWarmCount)
	}
	if cfg.ClientConnections < 1 {
		return fmt.Errorf("--client-connections must be >= 1, got %d", cfg.ClientConnections)
	}
	if cfg.ClaimDwell < 0 {
		return fmt.Errorf("--claim-dwell must be >= 0, got %v", cfg.ClaimDwell)
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
	log.Printf("Starting stress test run %s: phases=%v fill-per-node=%d probe=%d throughput=%d, writing results to %s",
		runID, cfg.Phases, cfg.FillPerNode, cfg.ProbeCount, cfg.ThroughputCount, cfg.OutputDir)

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

	// Mutating requests (creates/deletes) optionally get their own sharded
	// connections so wide create bursts neither queue on the apiserver's
	// ~100-streams-per-connection HTTP/2 cap nor congest the watch streams
	// that timestamp Ready. --client-connections=1 keeps the historical
	// single shared connection (mutateClient == dynamicClient).
	mutateClient := dynamicClient
	if cfg.ClientConnections > 1 {
		mutateConfig := rest.CopyConfig(restConfig)
		if err := configureCreateConnections(mutateConfig, cfg.ClientConnections); err != nil {
			return fmt.Errorf("failed to configure create-path connections: %w", err)
		}
		mutateClient, err = dynamic.NewForConfig(mutateConfig)
		if err != nil {
			return fmt.Errorf("failed to build sharded mutate client: %w", err)
		}
		log.Printf("Mutating requests sharded across %d dedicated HTTP/2 connections (watches keep their own connection)", cfg.ClientConnections)
	}
	// Connection calibration: the apiserver caps each HTTP/2 connection at
	// ~100 concurrent streams, so a create burst wider than 100*connections
	// queues on the client's own transport and inflates the measured
	// create-ack without touching the server. The widest burst is
	// create-concurrency for the sandbox phases, and the full claim count
	// for claims-warm (all claims fire at once, not concurrency-capped).
	// Warn instead of failing: a run may deliberately probe that shape.
	burstWidth := cfg.CreateConcurrency
	if hasClaimsPhase(phases) && cfg.ClaimsWarmCount > burstWidth {
		burstWidth = cfg.ClaimsWarmCount
	}
	if minConns := (burstWidth + 99) / 100; cfg.ClientConnections < minConns {
		log.Printf("WARNING: --client-connections=%d < ceil(max create burst %d / 100)=%d — create-ack latency will include client-side HTTP/2 stream queueing",
			cfg.ClientConnections, burstWidth, minConns)
	}

	clusterInfo, err := inspectCluster(ctx, restConfig, dynamicClient)
	if err != nil {
		return fmt.Errorf("failed to inspect cluster: %w", err)
	}
	log.Printf("Cluster: kubernetes %s, %d worker nodes, pod capacity %d, %d pre-existing worker pods",
		clusterInfo.KubernetesVersion, clusterInfo.Nodes, clusterInfo.PodCapacity, clusterInfo.PreexistingPods)
	if err := resolvePhases(phases, cfg, clusterInfo); err != nil {
		return err
	}
	for i, p := range phases {
		log.Printf("Phase #%d %s", i+1, p.Description())
	}

	// Create namespace
	nsClient := dynamicClient.Resource(gvrNamespaces)
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

	// APF preflight (claims phases only): verify the harness's claim POSTs
	// classify into the exempt priority level before any latency-bearing
	// phase runs; the verdict is logged and recorded in summary.json.
	var apfVerification *APFVerification
	if hasClaimsPhase(phases) {
		apfVerification = verifyClaimPostPriorityLevel(ctx, restConfig, cfg.Namespace)
		logAPFVerification(apfVerification)
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
	// Only watch SandboxClaims when a claims phase runs: the extensions
	// CRDs may not be installed otherwise, and a missing CRD would make the
	// watcher retry-loop for the whole run.
	if hasClaimsPhase(phases) {
		gvrList = append(gvrList, gvrSandboxClaims)
	}

	// recordEvent builds the shared watch callback: milestone tracking first
	// (cheap and time-sensitive), then the watch log write.
	recordEvent := func(gvr schema.GroupVersionResource) func(event WatchEventRecord) error {
		return func(event WatchEventRecord) error {
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
		}
	}

	for _, gvr := range gvrList {
		taskRunner.RunAsync(ctx, func(ctx context.Context) error {
			return watchResource(ctx, dynamicClient, gvr, recordEvent(gvr))
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
		for _, number := range slices.Sorted(maps.Keys(counts)) {
			c := counts[number]
			fmt.Fprintf(&line, " %s#%d: created=%d ready=%d deleted=%d failed=%d |",
				c.Name, number, c.Created, c.Ready, c.Deleted, c.Failed)
		}
		if writeToFileChannel != nil {
			fmt.Fprintf(&line, " watch-queue=%d/%d", len(writeToFileChannel), cap(writeToFileChannel))
		}
		log.Print(line.String())
		return nil
	})

	// CPU-profile the apiserver during each throughput level (it is the
	// dominant control-plane CPU consumer under churn).
	var profiler *apiserverProfiler
	if cfg.ProfileAPIServer {
		profiler, err = newAPIServerProfiler(restConfig, cfg.OutputDir)
		if err != nil {
			return fmt.Errorf("failed to build apiserver profiler: %w", err)
		}
	}

	// CPU/heap-profile the sandbox controller during the claims phases
	// (the controller is the suspected bottleneck of the adoption path).
	var ctrlProfiler *controllerProfiler
	if cfg.ProfileController && hasClaimsPhase(phases) {
		ctrlProfiler, err = newControllerProfiler(restConfig, cfg.OutputDir)
		if err != nil {
			return fmt.Errorf("failed to build controller profiler: %w", err)
		}
	}

	// All mutating clients are built from mutateClient (sharded when
	// --client-connections > 1); the watches above stay on dynamicClient.
	test := &stressTest{
		cfg:            cfg,
		tracker:        tracker,
		namespace:      cfg.Namespace,
		profiler:       profiler,
		ctrlProfiler:   ctrlProfiler,
		mutateClient:   mutateClient,
		nsClient:       mutateClient.Resource(gvrNamespaces),
		sandboxClient:  mutateClient.Resource(gvrSandboxes).Namespace(cfg.Namespace),
		templateClient: mutateClient.Resource(gvrSandboxTemplates).Namespace(cfg.Namespace),
		warmPoolClient: mutateClient.Resource(gvrSandboxWarmPools).Namespace(cfg.Namespace),
		claimClient:    mutateClient.Resource(gvrSandboxClaims).Namespace(cfg.Namespace),
	}

	phaseResults := make([]phaseResult, 0, len(phases))
	var phaseErr error
	for i, phase := range phases {
		number := PhaseNumber(i + 1)
		result := phaseResult{
			number: number,
			name:   phase.Name(),
			offset: time.Since(testStartTime),
		}
		start := time.Now()
		if err := phase.Run(ctx, test, number); err != nil {
			result.duration = time.Since(start)
			phaseResults = append(phaseResults, result)
			phaseErr = fmt.Errorf("%s#%d phase: %w", phase.Name(), number, err)
			log.Printf("aborting after error: %v", phaseErr)
			break
		}
		result.duration = time.Since(start)
		phaseResults = append(phaseResults, result)
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
	summary := buildSummary(runID, testStartTime, cfg, clusterInfo, tracker, phaseResults, phases)
	summary.APFVerification = apfVerification
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

// phaseResult records wall-clock timing for one completed (or aborted) phase.
type phaseResult struct {
	number   PhaseNumber
	name     PhaseName
	offset   time.Duration // start relative to the test start
	duration time.Duration
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

func buildSummary(runID string, startTime time.Time, cfg Config, clusterInfo *ClusterInfo, tracker *Tracker,
	phaseResults []phaseResult, phases []Phase) *Summary {
	records := tracker.Records()

	requested := func(number PhaseNumber) int {
		if i := int(number) - 1; i >= 0 && i < len(phases) {
			return phases[i].Requested()
		}
		return 0
	}

	summary := &Summary{
		RunID:     runID,
		StartTime: startTime,
		EndTime:   time.Now(),
		Config:    cfg,
		Cluster:   clusterInfo,
		Phases:    make([]*PhaseSummary, 0, len(phaseResults)),
	}

	recordsByPhase := make(map[PhaseNumber][]SandboxRecord)
	for _, record := range records {
		recordsByPhase[record.PhaseNumber] = append(recordsByPhase[record.PhaseNumber], record)
	}

	for _, result := range phaseResults {
		phaseRecords := recordsByPhase[result.number]
		// Throughput levels overshoot the configured count when
		// -throughput-min-seconds keeps them churning; every record was a
		// real request.
		req := max(requested(result.number), len(phaseRecords))
		var kind PhaseName
		if i := int(result.number) - 1; i >= 0 && i < len(phases) {
			kind = phases[i].Kind()
		}
		ps := &PhaseSummary{
			Number:                result.number,
			Name:                  string(result.name),
			Kind:                  kind,
			Requested:             req,
			DurationSeconds:       result.duration.Seconds(),
			StartOffsetSeconds:    result.offset.Seconds(),
			Latency:               computeLatencyBreakdown(phaseRecords),
			TimeToAllReadySeconds: computeTimeToAllReady(phaseRecords),
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
		if kind == PhaseClaimsWarmSustained {
			ps.SustainedWindows = computeWindowedLatencies(phaseRecords, sustainedWindow)
		}
		if clusterInfo != nil {
			ps.CreateThroughputPerNode = ps.CreateThroughput.perNode(clusterInfo.Nodes)
			ps.ReadyThroughputPerNode = ps.ReadyThroughput.perNode(clusterInfo.Nodes)
		}
		summary.Phases = append(summary.Phases, ps)
	}

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

	printBreakdown := func(b LatencyBreakdown) {
		fmt.Printf("    create ack (apiserver):        %s\n", formatLatency(b.CreateAck))
		fmt.Printf("    create -> pod created:         %s\n", formatLatency(b.CreateToPodCreated))
		fmt.Printf("    pod created -> scheduled:      %s\n", formatLatency(b.PodCreatedToScheduled))
		fmt.Printf("    scheduled -> pod running:      %s\n", formatLatency(b.ScheduledToPodRunning))
		fmt.Printf("    pod running -> pod ready:      %s\n", formatLatency(b.PodRunningToPodReady))
		fmt.Printf("    pod ready -> sandbox ready:    %s\n", formatLatency(b.PodReadyToSandboxReady))
		fmt.Printf("    END-TO-END (create -> ready):  %s\n", formatLatency(b.EndToEndReady))
	}

	for _, ps := range summary.Phases {
		fmt.Printf("\n--- #%d %s: %d requested, %d created, %d ready, %d failed (%.1fs) ---\n",
			ps.Number, ps.Name, ps.Requested, ps.Created, ps.Ready, ps.Failed, ps.DurationSeconds)

		switch ps.Kind {
		case PhaseProbe:
			fmt.Println("  Launch latency breakdown:")
			printBreakdown(ps.Latency)
		case PhaseClaimsWarm:
			// Claim records have no pod milestones (the pods were pre-warmed),
			// so only the claim-level intervals are meaningful. CreateAck
			// isolates the client's Create call from controller binding
			// latency (both are also in sandboxes.jsonl per claim).
			fmt.Printf("  claim create ack (apiserver):    %s\n", formatLatency(ps.Latency.CreateAck))
			fmt.Printf("  claim create -> claim Ready:     %s\n", formatLatency(ps.Latency.EndToEndReady))
			if ps.TimeToAllReadySeconds != nil {
				fmt.Printf("  time until ALL claims Ready:     %.2fs\n", *ps.TimeToAllReadySeconds)
			} else {
				fmt.Printf("  time until ALL claims Ready:     n/a (not all claims became Ready)\n")
			}
			fmt.Printf("  claim ready throughput:          %s\n", formatThroughput(ps.ReadyThroughput))
		case PhaseClaimsWarmSustained:
			// Like claims-warm, only claim-level intervals are meaningful; the
			// headline evidence is the per-window trend, not one aggregate.
			cfg := summary.Config
			fmt.Printf("  target arrivals:                 %.1f/s (Poisson) for %.0fs across %d namespace(s); pool %d/ns (headroom %s), dwell %s\n",
				cfg.SustainedRate, cfg.SustainedSeconds, cfg.SustainedNamespaces,
				sustainedPoolReplicasPerNamespace(cfg), cfg.SustainedPoolHeadroom, cfg.ClaimDwell)
			fmt.Printf("  claim create throughput:         %s\n", formatThroughput(ps.CreateThroughput))
			fmt.Printf("  claim create ack (apiserver):    %s\n", formatLatency(ps.Latency.CreateAck))
			fmt.Printf("  claim create -> claim Ready:     %s\n", formatLatency(ps.Latency.EndToEndReady))
			fmt.Printf("  claim ready throughput:          %s\n", formatThroughput(ps.ReadyThroughput))
			fmt.Printf("  rolling %.0fs windows by arrival time (create -> Ready):\n", sustainedWindow.Seconds())
			for _, w := range ps.SustainedWindows {
				if w.Arrivals == 0 {
					fmt.Printf("    [%4.0fs-%4.0fs) arrivals=0\n", w.StartOffsetSeconds, w.EndOffsetSeconds)
					continue
				}
				lat := "no readies"
				if w.Latency != nil {
					lat = fmt.Sprintf("p50=%-8s p90=%-8s p99=%-8s max=%s",
						formatMs(w.Latency.P50Ms), formatMs(w.Latency.P90Ms), formatMs(w.Latency.P99Ms), formatMs(w.Latency.MaxMs))
				}
				fmt.Printf("    [%4.0fs-%4.0fs) arrivals=%-5d ready=%-5d %s\n",
					w.StartOffsetSeconds, w.EndOffsetSeconds, w.Arrivals, w.Ready, lat)
			}
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

// watchResource will watch the given resource until the context is cancelled, or the callback function returns an error.
func watchResource(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, callback func(event WatchEventRecord) error) error {
	var resourceVersion string
	iface := dynamicClient.Resource(gvr)

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

		watcher, err := iface.Watch(ctx, listOptions)
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

	for range concurrency {
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
