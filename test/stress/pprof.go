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
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// apiserverProfiler captures Go CPU profiles from the kube-apiserver's
// /debug/pprof endpoint during test phases. Motivation: at ~87 sandboxes/s
// the apiserver burns 4.5-5.5 cores of the single control-plane node's 8,
// and that CPU pressure inflates every client's observed API latency
// (etcd update 4.4 -> 16ms, PUT sandboxes 8 -> 43ms across the sweep).
// Profiling overhead is a few percent for the duration of the capture.
//
// Requires the admin kubeconfig (cluster-admin covers the /debug/pprof/*
// nonResourceURLs); capture failures are logged and never fail the run.
type apiserverProfiler struct {
	kube      *kubernetes.Clientset
	outputDir string
}

func newAPIServerProfiler(restConfig *rest.Config, outputDir string) (*apiserverProfiler, error) {
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return &apiserverProfiler{kube: kube, outputDir: outputDir}, nil
}

// CaptureCPUProfile records one CPU profile of the given duration, starting
// after delay (to skip a phase's warm-up), and writes it to
// pprof-apiserver-<phase>.pprof (the standard pprof format: gzip-compressed
// profile.proto). Synchronous; run it in a goroutine alongside the phase.
// Analyze with: go tool pprof -top <file>.
func (p *apiserverProfiler) CaptureCPUProfile(ctx context.Context, phase Phase, delay, duration time.Duration) {
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return
	}

	ctx, cancel := context.WithTimeout(ctx, duration+30*time.Second)
	defer cancel()

	raw, err := p.kube.CoreV1().RESTClient().Get().
		AbsPath("/debug/pprof/profile").
		Param("seconds", strconv.Itoa(int(duration.Seconds()))).
		DoRaw(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[pprof] apiserver CPU profile for %s failed: %v", phase, err)
		}
		return
	}

	path := filepath.Join(p.outputDir, fmt.Sprintf("pprof-apiserver-%s.pprof", phase))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		log.Printf("[pprof] writing %s: %v", path, err)
		return
	}
	log.Printf("[pprof] captured apiserver CPU profile for %s (%d bytes)", phase, len(raw))
}
