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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// metricSample is one line of metrics.jsonl.gz: a single Prometheus sample
// from one component at one scrape. Long/narrow format for direct DuckDB
// ingestion; counters and histogram buckets are cumulative, so per-window
// rates come from diffing consecutive scrapes (see analyze-fsync.py for the
// same pattern).
type metricSample struct {
	TS       time.Time       `json:"ts"`
	Source   string          `json:"source"`   // kube-apiserver | kube-controller-manager | kube-scheduler | agent-sandbox-controller | kubelet
	Instance string          `json:"instance"` // pod or node name
	Metric   string          `json:"metric"`   // family name (+ _bucket/_sum/_count for histograms and summaries)
	Labels   json.RawMessage `json:"labels,omitempty"`
	Value    float64         `json:"value"`
}

// promScraper periodically collects Prometheus metrics from the control
// plane (apiserver, kube-controller-manager, kube-scheduler), the
// agent-sandbox controller, and every kubelet, writing parsed samples to
// metrics.jsonl.gz. Everything is best-effort: a source that fails to
// resolve or scrape is logged and skipped, never fatal.
//
// Not scraped (deliberately): cadvisor (very large, container-level resource
// data) and etcd (requires client certificates).
type promScraper struct {
	kube *kubernetes.Clientset

	mu   sync.Mutex
	file *os.File
	bufw *bufio.Writer
	gzw  *gzip.Writer
	enc  *json.Encoder

	samples int
	closed  bool
}

func newPromScraper(restConfig *rest.Config, filePath string) (*promScraper, error) {
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	f, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("creating metrics file %s: %w", filePath, err)
	}
	bufw := bufio.NewWriterSize(f, 1<<20)
	gzw := gzip.NewWriter(bufw)
	return &promScraper{
		kube: kube,
		file: f,
		bufw: bufw,
		gzw:  gzw,
		enc:  json.NewEncoder(gzw),
	}, nil
}

// Close flushes and closes the output file. Safe to call more than once.
func (s *promScraper) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	log.Printf("[metrics] wrote %d samples", s.samples)
	if err := s.gzw.Close(); err != nil {
		return err
	}
	if err := s.bufw.Flush(); err != nil {
		return err
	}
	return s.file.Close()
}

// promSource is one metrics endpoint, addressed via the apiserver
// (direct for the apiserver itself, pod/node proxy for everything else),
// so no direct network path to nodes or the control plane is needed.
type promSource struct {
	source   string
	instance string
	path     string
}

// resolveSources discovers the current scrape targets. Re-resolved on every
// cycle so pod restarts and node changes are picked up.
func (s *promScraper) resolveSources(ctx context.Context) []promSource {
	sources := []promSource{{source: "kube-apiserver", instance: "kube-apiserver", path: "/metrics"}}

	forPods := func(namespace, source, scheme string, port int, labelSelectors ...string) {
		for _, labelSelector := range labelSelectors {
			pods, err := s.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
			if err != nil {
				log.Printf("[metrics] listing %s pods: %v", source, err)
				continue
			}
			if len(pods.Items) == 0 {
				continue
			}
			for i := range pods.Items {
				name := pods.Items[i].Name
				sources = append(sources, promSource{
					source:   source,
					instance: name,
					path:     fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%s:%d/proxy/metrics", namespace, scheme, name, port),
				})
			}
			return
		}
	}
	// Control-plane components serve HTTPS with delegated authz; the
	// apiserver pod proxy handles that when given the https scheme prefix.
	// kOps labels control-plane pods k8s-app=..., kubeadm/kind use component=...
	forPods("kube-system", "kube-controller-manager", "https", 10257, "k8s-app=kube-controller-manager", "component=kube-controller-manager")
	forPods("kube-system", "kube-scheduler", "https", 10259, "k8s-app=kube-scheduler", "component=kube-scheduler")
	forPods("agent-sandbox-system", "agent-sandbox-controller", "http", 8080, "app=agent-sandbox-controller")
	// cilium-agent: CNI/endpoint latency lives here when cilium is the CNI
	// and prometheus metrics are enabled (e.g. kOps
	// cluster.spec.networking.cilium.enablePrometheusMetrics=true, which
	// serves the agent on :9090 — verified empirically; 9962, upstream
	// cilium's newer default, is not listening on kOps). Best-effort: if
	// cilium is absent or metrics are disabled, the scrape is logged and
	// skipped.
	forPods("kube-system", "cilium-agent", "http", 9090, "k8s-app=cilium")

	nodes, err := s.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[metrics] listing nodes: %v", err)
	} else {
		for i := range nodes.Items {
			name := nodes.Items[i].Name
			sources = append(sources, promSource{
				source:   "kubelet",
				instance: name,
				path:     fmt.Sprintf("/api/v1/nodes/%s/proxy/metrics", name),
			})
		}
	}
	return sources
}

// ScrapeAll fetches and records all sources once, concurrently.
func (s *promScraper) ScrapeAll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, src := range s.resolveSources(ctx) {
		wg.Go(func() {
			if err := s.scrapeOne(ctx, src); err != nil && ctx.Err() == nil {
				log.Printf("[metrics] scraping %s (%s): %v", src.source, src.instance, err)
			}
		})
	}
	wg.Wait()
}

func (s *promScraper) scrapeOne(ctx context.Context, src promSource) error {
	ts := time.Now()
	raw, err := s.kube.CoreV1().RESTClient().Get().AbsPath(src.path).DoRaw(ctx)
	if err != nil {
		// Component metrics endpoints often return non-Status error bodies,
		// which client-go reports as just "unknown"; include the HTTP code
		// and body so failures (401/403 vs 503) are distinguishable in CI.
		var statusErr *apierrors.StatusError
		if errors.As(err, &statusErr) {
			return fmt.Errorf("HTTP %d: %s (%w)", statusErr.Status().Code, strings.TrimSpace(string(raw)), err)
		}
		return err
	}

	// The parser panics if constructed without a validation scheme.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("parsing metrics: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	for name, family := range families {
		for _, m := range family.Metric {
			if err := s.encodeMetricLocked(ts, src, name, family.GetType(), m); err != nil {
				return err
			}
		}
	}
	return nil
}

// encodeMetricLocked writes the samples for one metric. Must hold s.mu.
func (s *promScraper) encodeMetricLocked(ts time.Time, src promSource, name string, mtype dto.MetricType, m *dto.Metric) error {
	labels := make(map[string]string, len(m.Label)+1)
	for _, lp := range m.Label {
		labels[lp.GetName()] = lp.GetValue()
	}

	emit := func(metric string, extraKey, extraVal string, value float64) error {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
		var labelsJSON json.RawMessage
		if len(labels) > 0 || extraKey != "" {
			all := labels
			if extraKey != "" {
				all = make(map[string]string, len(labels)+1)
				maps.Copy(all, labels)
				all[extraKey] = extraVal
			}
			b, err := json.Marshal(all)
			if err != nil {
				return err
			}
			labelsJSON = b
		}
		s.samples++
		return s.enc.Encode(metricSample{
			TS: ts, Source: src.source, Instance: src.instance,
			Metric: metric, Labels: labelsJSON, Value: value,
		})
	}

	switch mtype {
	case dto.MetricType_COUNTER:
		return emit(name, "", "", m.Counter.GetValue())
	case dto.MetricType_GAUGE:
		return emit(name, "", "", m.Gauge.GetValue())
	case dto.MetricType_UNTYPED:
		return emit(name, "", "", m.Untyped.GetValue())
	case dto.MetricType_HISTOGRAM:
		h := m.Histogram
		for _, b := range h.Bucket {
			le := strconv.FormatFloat(b.GetUpperBound(), 'g', -1, 64)
			if err := emit(name+"_bucket", "le", le, float64(b.GetCumulativeCount())); err != nil {
				return err
			}
		}
		if err := emit(name+"_sum", "", "", h.GetSampleSum()); err != nil {
			return err
		}
		return emit(name+"_count", "", "", float64(h.GetSampleCount()))
	case dto.MetricType_SUMMARY:
		sm := m.Summary
		for _, q := range sm.Quantile {
			qv := strconv.FormatFloat(q.GetQuantile(), 'g', -1, 64)
			if err := emit(name, "quantile", qv, q.GetValue()); err != nil {
				return err
			}
		}
		if err := emit(name+"_sum", "", "", sm.GetSampleSum()); err != nil {
			return err
		}
		return emit(name+"_count", "", "", float64(sm.GetSampleCount()))
	default:
		return nil
	}
}
