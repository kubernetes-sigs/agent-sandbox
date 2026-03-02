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

package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	// PodStatusReady indicates the pod is ready.
	PodStatusReady = "ready"
	// PodStatusPending indicates the pod is pending.
	PodStatusPending = "pending"
	// PodStatusSucceeded indicates the pod has succeeded.
	PodStatusSucceeded = "succeeded"
	// PodStatusFailed indicates the pod has failed.
	PodStatusFailed = "failed"
	// PodStatusOther indicates any other pod status.
	PodStatusOther = "other"
	// PodStatusAll indicates the total count of all pods in the pool.
	PodStatusAll = "*"
)

// Metrics encapsulates all Prometheus metrics for the agent-sandbox.
// It implements the prometheus.Collector interface to provide "Pull"-based metrics.
type Metrics struct {
	client       client.Reader
	warmPoolSize *prometheus.Desc
}

// NewMetrics creates a new Metrics instance as a prometheus.Collector.
func NewMetrics(reg prometheus.Registerer, c client.Reader) *Metrics {
	m := &Metrics{
		client: c,
		warmPoolSize: prometheus.NewDesc(
			"agent_sandbox_warmpool_size",
			"Monitor the point-in-time status of the warmpool. Purpose is to be able to alert on WarmPool exhaustion.",
			[]string{"pod_status", "warmpool_name", "sandbox_template"},
			nil,
		),
	}
	// Register with the provided registry (usually the controller-runtime global registry)
	reg.MustRegister(m)
	return m
}

// Describe implements prometheus.Collector.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.warmPoolSize
}

// Collect implements prometheus.Collector.
// This is called by Prometheus during setiap scrape.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	// List all SandboxWarmPools across all namespaces
	var warmPools extensionsv1alpha1.SandboxWarmPoolList
	if err := m.client.List(ctx, &warmPools); err != nil {
		return
	}

	for _, wp := range warmPools.Items {
		m.collectForPool(ctx, &wp, ch)
	}
}

func (m *Metrics) collectForPool(ctx context.Context, wp *extensionsv1alpha1.SandboxWarmPool, ch chan<- prometheus.Metric) {
	// List all pods in the same namespace as the warmpool
	podList := &corev1.PodList{}
	if err := m.client.List(ctx, podList, &client.ListOptions{Namespace: wp.Namespace}); err != nil {
		return
	}

	counts := map[string]float64{
		PodStatusReady:     0,
		PodStatusPending:   0,
		PodStatusSucceeded: 0,
		PodStatusFailed:    0,
		PodStatusOther:     0,
		PodStatusAll:       0,
	}

	for _, pod := range podList.Items {
		// Skip pods that are being deleted
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		// Verify ownership: Pod must be owned by this SandboxWarmPool
		ownedByPool := false
		for _, ref := range pod.OwnerReferences {
			if ref.UID == wp.UID {
				ownedByPool = true
				break
			}
		}

		if !ownedByPool {
			continue
		}

		counts[PodStatusAll]++

		isReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}

		if isReady {
			counts[PodStatusReady]++
		} else {
			switch pod.Status.Phase {
			case corev1.PodPending:
				counts[PodStatusPending]++
			case corev1.PodSucceeded:
				counts[PodStatusSucceeded]++
			case corev1.PodFailed:
				counts[PodStatusFailed]++
			default:
				counts[PodStatusOther]++
			}
		}
	}

	for status, count := range counts {
		ch <- prometheus.MustNewConstMetric(
			m.warmPoolSize,
			prometheus.GaugeValue,
			count,
			status,
			wp.Name,
			wp.Spec.TemplateRef.Name,
		)
	}
}
