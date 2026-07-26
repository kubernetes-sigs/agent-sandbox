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
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// controllerProfiler captures CPU and heap profiles from the agent-sandbox
// controller's pprof endpoints, which live on the metrics port (:8080) when
// the controller runs with --enable-pprof (CPU) / --enable-pprof-debug
// (heap and the other debug profiles). Endpoints are reached through the
// apiserver pod proxy, so no direct pod network path is needed and the same
// admin kubeconfig used for everything else suffices.
//
// All captures are best-effort: a controller deployed without the pprof
// flags just yields a logged 404 and the run continues.
type controllerProfiler struct {
	kube      *kubernetes.Clientset
	outputDir string
}

const (
	controllerNamespace   = "agent-sandbox-system"
	controllerPodSelector = "app=agent-sandbox-controller"
	controllerMetricsPort = 8080
)

func newControllerProfiler(restConfig *rest.Config, outputDir string) (*controllerProfiler, error) {
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return &controllerProfiler{kube: kube, outputDir: outputDir}, nil
}

// podNames lists the current controller pods. With leader election only one
// does the work, but profiling every replica is cheap and avoids guessing
// the leader.
func (p *controllerProfiler) podNames(ctx context.Context) []string {
	pods, err := p.kube.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{LabelSelector: controllerPodSelector})
	if err != nil {
		log.Printf("[pprof] listing controller pods: %v", err)
		return nil
	}
	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		names = append(names, pods.Items[i].Name)
	}
	return names
}

func (p *controllerProfiler) capture(ctx context.Context, pod, endpoint, outFile string, params map[string]string) {
	req := p.kube.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/namespaces/%s/pods/http:%s:%d/proxy/debug/pprof/%s",
			controllerNamespace, pod, controllerMetricsPort, endpoint))
	for k, v := range params {
		req = req.Param(k, v)
	}
	raw, err := req.DoRaw(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[pprof] controller %s profile from %s failed: %v", endpoint, pod, err)
		}
		return
	}
	path := filepath.Join(p.outputDir, outFile)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		log.Printf("[pprof] writing %s: %v", path, err)
		return
	}
	log.Printf("[pprof] captured controller %s profile from %s (%d bytes)", endpoint, pod, len(raw))
}

// CaptureCPUProfile records one CPU profile of the given duration from every
// controller pod, starting after delay. Synchronous; run in a goroutine
// alongside the phase so the profile window covers the load being measured
// (e.g. delay=0 started right as the claims burst fires).
// Analyze with: go tool pprof -top pprof-controller-<phase>-<pod>.pprof.
func (p *controllerProfiler) CaptureCPUProfile(ctx context.Context, phase Phase, delay, duration time.Duration) {
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return
	}

	ctx, cancel := context.WithTimeout(ctx, duration+30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, pod := range p.podNames(ctx) {
		wg.Go(func() {
			p.capture(ctx, pod, "profile",
				fmt.Sprintf("pprof-controller-%s-%s.pprof", phase, pod),
				map[string]string{"seconds": strconv.Itoa(int(duration.Seconds()))})
		})
	}
	wg.Wait()
}

// CaptureHeapProfile snapshots the heap profile of every controller pod
// (requires --enable-pprof-debug). label distinguishes multiple snapshots
// within one phase (e.g. burst-start vs burst-end).
// Analyze with: go tool pprof -top -sample_index=inuse_space <file>.
func (p *controllerProfiler) CaptureHeapProfile(ctx context.Context, phase Phase, label string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for _, pod := range p.podNames(ctx) {
		p.capture(ctx, pod, "heap",
			fmt.Sprintf("pprof-controller-heap-%s-%s-%s.pprof", phase, label, pod), nil)
	}
}
