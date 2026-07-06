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

package main

import (
	"bufio"
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// WatchEventRecord defines the schema for each line in watch.jsonl.
type WatchEventRecord struct {
	Timestamp time.Time       `json:"timestamp"`
	Resource  string          `json:"resource"`
	Type      watch.EventType `json:"type"`
	Object    any             `json:"object"`
}

// LatencyStats holds calculated latency percentiles.
type LatencyStats struct {
	P50Ms float64 `json:"p50_ms"`
	P90Ms float64 `json:"p90_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
}

// StressTestSummary is written to summary.json at the end of the test.
type StressTestSummary struct {
	TotalCreated         int          `json:"totalCreated"`
	TotalReady           int          `json:"totalReady"`
	TotalFinished        int          `json:"totalFinished"`
	TotalDurationMs      float64      `json:"totalDurationMs"`
	CreateLatencyStats   LatencyStats `json:"createLatencyStats"`
	ReadyLatencyStats    LatencyStats `json:"readyLatencyStats"`
	FinishedLatencyStats LatencyStats `json:"finishedLatencyStats"`
}

var (
	createdCount  atomic.Int32
	readyCount    atomic.Int32
	finishedCount atomic.Int32

	stateMu                  sync.Mutex
	sandboxCreatedMap        = make(map[types.NamespacedName]time.Time)
	sandboxCreatedSuccessMap = make(map[types.NamespacedName]time.Time)
	sandboxReadyMap          = make(map[types.NamespacedName]time.Time)
	sandboxFinishedMap       = make(map[types.NamespacedName]time.Time)
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
	sandboxCount := 100
	flag.IntVar(&sandboxCount, "sandbox-count", sandboxCount, "Number of Sandboxes to create")

	createConcurrency := 10
	flag.IntVar(&createConcurrency, "create-concurrency", createConcurrency, "Number of concurrent workers creating Sandboxes")
	namespace := flag.String("namespace", "sandbox-stress-test", "Kubernetes namespace to run the test in. If default, a timestamp suffix is added.")
	outputDir := "./stress-results"
	flag.StringVar(&outputDir, "output-dir", outputDir, "Directory to write results to")
	cleanup := flag.Bool("cleanup", true, "Whether to delete the namespace at the end of the test")
	imageName := flag.String("image", "debian:latest", "Container image to use for Sandboxes")
	timeout := flag.Duration("timeout", 15*time.Minute, "Timeout for the entire test run")
	flag.Parse()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	log.Printf("Starting stress test: creating %d Sandboxes using %s, create-concurrency=%d", sandboxCount, *imageName, createConcurrency)

	// Create unique run ID and directories
	runID := time.Now().Format("20060102-150405")
	if *namespace == "sandbox-stress-test" {
		*namespace = fmt.Sprintf("sandbox-stress-%s", runID)
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create run directory %s: %w", outputDir, err)
	}
	log.Printf("Writing watch events and results to directory: %s", outputDir)

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

	// Create namespace
	nsClient := dynamicClient.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"})
	nsObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name": *namespace,
			},
		},
	}
	log.Printf("Creating namespace: %s", *namespace)
	_, err = nsClient.Create(ctx, nsObj, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create namespace %s: %w", *namespace, err)
	}

	// Clean up namespace at the end if requested
	if *cleanup {
		defer func() {
			log.Printf("Cleaning up namespace: %s", *namespace)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cleanupCancel()
			if err := nsClient.Delete(cleanupCtx, *namespace, metav1.DeleteOptions{}); err != nil {
				log.Printf("failed to delete namespace %s: %v", *namespace, err)
			}
		}()
	}

	taskRunner := NewTaskRunner(cancel)

	// Start watch recording to file
	writeToFileChannel := make(chan WatchEventRecord, 1024)
	watchFilePath := filepath.Join(outputDir, "watch.jsonl")

	taskRunner.RunAsync(ctx, func(ctx context.Context) error {
		return runWriter(ctx, watchFilePath, writeToFileChannel)
	})

	// Setup and start watchers
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
				select {
				case writeToFileChannel <- event:
				case <-ctx.Done():
					return ctx.Err()
				}

				if event.Object != nil {
					if u, ok := event.Object.(*unstructured.Unstructured); ok {
						handleWatchEvent(gvr, event.Type, u)
					} else {
						return fmt.Errorf("unhandled type in event %T", event.Object)
					}
				}

				return nil
			})
		})
	}

	// Wait briefly for watches to establish
	time.Sleep(2 * time.Second)

	// Start progress reporter
	taskRunner.RunPeriodic(ctx, 5*time.Second, func() error {
		created := createdCount.Load()
		ready := readyCount.Load()
		finished := finishedCount.Load()
		log.Printf("[Progress] Created: %d/%d | Ready: %d | Finished: %d", created, sandboxCount, ready, finished)
		return nil
	})

	testStartTime := time.Now()

	// Launch Sandbox creation workers
	sandboxClient := dynamicClient.Resource(schema.GroupVersionResource{
		Group:    "agents.x-k8s.io",
		Version:  "v1beta1",
		Resource: "sandboxes",
	}).Namespace(*namespace)

	var names []types.NamespacedName
	for i := 0; i < sandboxCount; i++ {
		name := fmt.Sprintf("stress-%d", i)
		names = append(names, types.NamespacedName{
			Name:      name,
			Namespace: *namespace,
		})
	}

	if _, err := ForkJoin(ctx, names, createConcurrency, func(id types.NamespacedName) (types.UID, error) {
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
							"restartPolicy": "Never",
							"containers": []any{
								map[string]any{
									"name":            "main",
									"image":           *imageName,
									"imagePullPolicy": "IfNotPresent",
									"command":         []string{"sleep", "5"},
								},
							},
						},
					},
				},
			},
		}

		startTime := time.Now()
		stateMu.Lock()
		sandboxCreatedMap[id] = startTime
		stateMu.Unlock()

		created, err := sandboxClient.Create(ctx, sandbox, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("creating sandbox %q: %w", id, err)
		}

		endTime := time.Now()
		stateMu.Lock()
		sandboxCreatedSuccessMap[id] = endTime
		stateMu.Unlock()

		createdCount.Add(1)
		return created.GetUID(), nil
	}); err != nil {
		return err
	}
	log.Printf("All Sandbox creation workers finished. Waiting for Sandboxes to settle...")

	// Wait until all successfully created Sandboxes are Finished, or timeout.
	// Since Pods sleep for 5 seconds, they should finish shortly after readiness.

	for {
		// Note: we don't necessarily see a pod become Ready before it is Finished,
		// so we only wait for finished.

		created := createdCount.Load()
		finished := finishedCount.Load()
		if finished == created {
			log.Printf("All %d created Sandboxes reached Finished state", created)
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// This is not timing critical, the timestamps in the maps are authoritative
		time.Sleep(100 * time.Millisecond)
	}

	testDuration := time.Since(testStartTime)

	// Generate summary & percentiles
	stateMu.Lock()
	var createDurations []time.Duration
	var readyDurations []time.Duration
	var finishedDurations []time.Duration

	for name, createStart := range sandboxCreatedMap {
		if createEnd, ok := sandboxCreatedSuccessMap[name]; ok {
			createDurations = append(createDurations, createEnd.Sub(createStart))
		}
		if readyTime, ok := sandboxReadyMap[name]; ok {
			readyDurations = append(readyDurations, readyTime.Sub(createStart))
		}
		if finishedTime, ok := sandboxFinishedMap[name]; ok {
			finishedDurations = append(finishedDurations, finishedTime.Sub(createStart))
		}
	}
	stateMu.Unlock()

	summary := StressTestSummary{
		TotalCreated:    int(createdCount.Load()),
		TotalReady:      int(readyCount.Load()),
		TotalFinished:   int(finishedCount.Load()),
		TotalDurationMs: toMs(testDuration),
		CreateLatencyStats: LatencyStats{
			P50Ms: toMs(getPercentile(createDurations, 50)),
			P90Ms: toMs(getPercentile(createDurations, 90)),
			P95Ms: toMs(getPercentile(createDurations, 95)),
			P99Ms: toMs(getPercentile(createDurations, 99)),
		},
		ReadyLatencyStats: LatencyStats{
			P50Ms: toMs(getPercentile(readyDurations, 50)),
			P90Ms: toMs(getPercentile(readyDurations, 90)),
			P95Ms: toMs(getPercentile(readyDurations, 95)),
			P99Ms: toMs(getPercentile(readyDurations, 99)),
		},
		FinishedLatencyStats: LatencyStats{
			P50Ms: toMs(getPercentile(finishedDurations, 50)),
			P90Ms: toMs(getPercentile(finishedDurations, 90)),
			P95Ms: toMs(getPercentile(finishedDurations, 95)),
			P99Ms: toMs(getPercentile(finishedDurations, 99)),
		},
	}

	summaryBytes, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal summary: %w", err)
	}

	summaryPath := filepath.Join(outputDir, "summary.json")
	if err := os.WriteFile(summaryPath, summaryBytes, 0644); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}

	fmt.Println("\n================= STRESS TEST RESULTS =================")
	fmt.Printf("Total Duration:      %.2fs\n", testDuration.Seconds())
	fmt.Printf("Created:            %d sandboxes\n", summary.TotalCreated)
	fmt.Printf("Ready:              %d sandboxes\n", summary.TotalReady)
	fmt.Printf("Finished:           %d sandboxes\n", summary.TotalFinished)
	fmt.Println("\nLatency Percentiles (ms):")
	fmt.Printf("  Create:   p50=%.1fms, p90=%.1fms, p95=%.1fms, p99=%.1fms\n",
		summary.CreateLatencyStats.P50Ms, summary.CreateLatencyStats.P90Ms,
		summary.CreateLatencyStats.P95Ms, summary.CreateLatencyStats.P99Ms)
	fmt.Printf("  Ready:    p50=%.1fms, p90=%.1fms, p95=%.1fms, p99=%.1fms\n",
		summary.ReadyLatencyStats.P50Ms, summary.ReadyLatencyStats.P90Ms,
		summary.ReadyLatencyStats.P95Ms, summary.ReadyLatencyStats.P99Ms)
	fmt.Printf("  Finished: p50=%.1fms, p90=%.1fms, p95=%.1fms, p99=%.1fms\n",
		summary.FinishedLatencyStats.P50Ms, summary.FinishedLatencyStats.P90Ms,
		summary.FinishedLatencyStats.P95Ms, summary.FinishedLatencyStats.P99Ms)
	fmt.Println("=======================================================")

	fmt.Println("\nWrote detailed events to:", outputDir)

	cancel()
	return taskRunner.Wait()
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
					if u, ok := event.Object.(*unstructured.Unstructured); ok {
						resourceVersion = u.GetResourceVersion()
					} else if metaObj, ok := event.Object.(metav1.Object); ok {
						resourceVersion = metaObj.GetResourceVersion()
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

func handleWatchEvent(gvr schema.GroupVersionResource, _ watch.EventType, u *unstructured.Unstructured) {
	if gvr.Resource != "sandboxes" {
		return
	}

	id := types.NamespacedName{
		Name:      u.GetName(),
		Namespace: u.GetNamespace(),
	}
	stateMu.Lock()
	defer stateMu.Unlock()

	if _, ok := sandboxCreatedMap[id]; !ok {
		return
	}

	if isSandboxReady(u) {
		if _, ok := sandboxReadyMap[id]; !ok {
			sandboxReadyMap[id] = time.Now()
			readyCount.Add(1)
		}
	}

	if isSandboxFinished(u) {
		if _, ok := sandboxFinishedMap[id]; !ok {
			sandboxFinishedMap[id] = time.Now()
			finishedCount.Add(1)
		}
	}
}

func isSandboxReady(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, condVal := range conditions {
		cond, ok := condVal.(map[string]any)
		if !ok {
			continue
		}
		cType, _ := cond["type"].(string)
		cStatus, _ := cond["status"].(string)
		if cType == "Ready" && cStatus == "True" {
			return true
		}
	}
	return false
}

func isSandboxFinished(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, condVal := range conditions {
		cond, ok := condVal.(map[string]any)
		if !ok {
			continue
		}
		cType, _ := cond["type"].(string)
		cStatus, _ := cond["status"].(string)
		if cType == "Finished" && cStatus == "True" {
			return true
		}
	}
	return false
}

func runWriter(ctx context.Context, filePath string, eventChan <-chan WatchEventRecord) error {
	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create watch file %s: %w", filePath, err)
	}
	defer f.Close()

	bufWriter := bufio.NewWriter(f)
	defer bufWriter.Flush()
	encoder := json.NewEncoder(bufWriter)

	for {
		select {
		case event := <-eventChan:
			if err := encoder.Encode(event); err != nil {
				return fmt.Errorf("failed to encode event: %w", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func getPercentile(durations []time.Duration, pct float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	slices.Sort(durations)
	// Note this is not accurate for small N, but should be fine for large N.
	idx := int(float64(len(durations)) * pct / 100.0)
	if idx >= len(durations) {
		idx = len(durations) - 1
	}
	return durations[idx]
}

func toMs(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
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
			if !errors.Is(task.err, context.Canceled) {
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
	}

	return r.Error()
}
