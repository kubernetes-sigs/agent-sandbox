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

// fleet drives Sandbox creation across MANY clusters at an aggregate
// target rate, to demonstrate fleet-level launch throughput (the
// 1M-sandboxes-per-minute campaign). It is deliberately NOT the
// single-cluster stress harness: it watches ONLY sandboxes (a pod
// firehose from N clusters would make the driver the bottleneck - a
// lesson the single-cluster campaign paid for twice), it holds no
// per-pod state, and its artifacts are per-cluster jsonl files shaped
// for duckdb plus a fleet-level server-clock verdict.
//
// Phase 1 operates on pre-provisioned clusters: pass one kubeconfig per
// cluster. Sandboxes are distributed by hash of their ordinal ID, so
// the assignment is deterministic and even without coordination.
package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	_ "net/http/pprof" // driver self-profiling; the 10k/s ceiling is CPU/lock-bound, not I/O
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

type config struct {
	Kubeconfigs      []string
	Count            int
	Rate             float64
	Namespace        string
	Image            string
	OutputDir        string
	ConnsPerCluster  int
	CreatePerCluster int // concurrent creates per cluster
	Timeout          time.Duration
	SchedulerNames   []string
	PartitionLabel   string
}

func run() error {
	var cfg config
	var kubeconfigList string
	flag.StringVar(&kubeconfigList, "kubeconfigs", "", "comma-separated kubeconfig paths, one per cluster (or a glob, e.g. '/tmp/fleet/*.kubeconfig')")
	flag.IntVar(&cfg.Count, "count", 1000000, "total sandboxes to create across the fleet")
	flag.Float64Var(&cfg.Rate, "rate", 16667, "aggregate create rate across the fleet, sandboxes/s")
	flag.StringVar(&cfg.Namespace, "namespace", "", "namespace to create in (default fleet-<timestamp>)")
	flag.StringVar(&cfg.Image, "image", "debian:latest", "sandbox image (pre-pull it; the fleet run should not measure registry pulls)")
	flag.StringVar(&cfg.OutputDir, "output-dir", "", "artifact directory (default fleet-artifacts-<timestamp>)")
	flag.IntVar(&cfg.ConnsPerCluster, "conns-per-cluster", 8, "HTTP/2 connections per cluster for mutating requests (~100 concurrent streams each; must cover create-concurrency)")
	flag.IntVar(&cfg.CreatePerCluster, "create-concurrency", 512, "concurrent create workers per cluster. Sized for the FARTHEST clusters: a worker blocks one round trip per create, so sustaining R/s at RTT needs ~R*RTT workers. 256 workers capped a ~300ms-RTT (Mumbai/Sydney) cluster near ~500/s even when its Rate/N limiter allowed 1000/s; 512 covers a ~500ms effective RTT at 1000/s.")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Minute, "overall run timeout")
	schedulerNames := flag.String("scheduler-names", "", "comma-separated schedulerName lanes (matches the scaleup cluster config; empty = default scheduler)")
	flag.StringVar(&cfg.PartitionLabel, "scheduler-partition-label", "", "node label key for scheduler lane partitions")
	pprofAddr := flag.String("pprof-addr", "", "if set, serve net/http/pprof here (e.g. :6060) to profile the driver during a run")
	flag.Parse()

	if *pprofAddr != "" {
		go func() { log.Printf("pprof: %v", http.ListenAndServe(*pprofAddr, nil)) }()
	}

	if kubeconfigList == "" {
		return fmt.Errorf("--kubeconfigs is required")
	}
	if strings.ContainsAny(kubeconfigList, "*?[") {
		matches, err := filepath.Glob(kubeconfigList)
		if err != nil || len(matches) == 0 {
			return fmt.Errorf("no kubeconfigs match %q", kubeconfigList)
		}
		cfg.Kubeconfigs = matches
	} else {
		cfg.Kubeconfigs = strings.Split(kubeconfigList, ",")
	}
	if *schedulerNames != "" {
		cfg.SchedulerNames = strings.Split(*schedulerNames, ",")
	}
	runID := time.Now().UTC().Format("20060102-150405")
	if cfg.Namespace == "" {
		cfg.Namespace = "fleet-" + runID
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = "fleet-artifacts-" + runID
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clusters, err := connectClusters(ctx, cfg)
	if err != nil {
		return err
	}
	log.Printf("fleet: %d clusters, %d sandboxes total at %.0f/s aggregate (%.0f/s per cluster mean)",
		len(clusters), cfg.Count, cfg.Rate, cfg.Rate/float64(len(clusters)))

	rec, err := newRecorder(cfg.OutputDir, clusters)
	if err != nil {
		return err
	}
	defer rec.Close()

	for _, c := range clusters {
		c := c
		go c.watchSandboxes(ctx, cfg.Namespace, rec)
	}

	start := time.Now()
	if err := runCreates(ctx, cfg, clusters, rec, start); err != nil {
		return err
	}

	// Always write the summary, even if waitReady returns early: the
	// verdict is the fixed 60s window over server-side Ready stamps, which
	// is meaningful whether or not every last sandbox became Ready (a
	// heterogeneous fleet routinely has unschedulable overflow on its
	// smallest clusters). A summary + report that loudly show ready <
	// requested beat aborting before any result is written.
	werr := waitReady(ctx, cfg, clusters, rec, start)
	if serr := rec.WriteSummary(cfg, clusters, start); serr != nil {
		return serr
	}
	return werr
}

// clusterFor assigns a sandbox ordinal to a cluster: hash the ID so the
// distribution is even and deterministic but NOT a perfect round-robin -
// real request streams don't arrive pre-balanced across clusters, and a
// strided id%n assignment would flatter the result by assuming they do.
// The creator hashes every ordinal once up front into per-cluster id
// lists (see runCreates), so this is off the per-create hot path.
func clusterFor(id int, n int) int {
	h := fnv.New32a()
	fmt.Fprintf(h, "%d", id)
	return int(h.Sum32() % uint32(n))
}
