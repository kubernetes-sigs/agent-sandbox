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

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// WarmPoolSizeMetricKey is used to aggregate counts for identical WarmPool metric label combinations.
type WarmPoolSizeMetricKey struct {
	Namespace     string
	WarmPoolName  string
	Template      string
	SandboxStatus string
}

// WarmPoolCollector is a custom Prometheus collector that dynamically fetches warm pool sizes.
type WarmPoolCollector struct {
	client client.Client
	logger logr.Logger
	desc   *prometheus.Desc
}

// NewWarmPoolCollector initializes a WarmPoolCollector.
func NewWarmPoolCollector(c client.Client, logger logr.Logger) *WarmPoolCollector {
	return &WarmPoolCollector{
		client: c,
		logger: logger,
		desc:   AgentSandboxWarmPoolSizeDesc,
	}
}

// RegisterWarmPoolCollector registers the custom Prometheus collector for warm pool sizes.
func RegisterWarmPoolCollector(c client.Client, logger logr.Logger) {
	collector := NewWarmPoolCollector(c, logger)
	if err := metrics.Registry.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			logger.Error(err, "Failed to register WarmPoolCollector")
		} else {
			logger.Info("WarmPoolCollector already registered, ignoring")
		}
	}
}

// Describe sends the metric descriptor to the channel.
func (c *WarmPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect fetches warm pools and sandboxes, calculates labels, and sends metrics to the channel.
func (c *WarmPoolCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectTimeout)
	defer cancel()

	var warmPoolList extensionsv1beta1.SandboxWarmPoolList
	if err := c.client.List(ctx, &warmPoolList, client.UnsafeDisableDeepCopy); err != nil {
		c.logger.Error(err, "Failed to list warm pools for metrics collection")
		return
	}

	type poolInfo struct {
		Name      string
		Namespace string
		Template  string
	}
	poolMap := make(map[string]poolInfo)
	counts := make(map[WarmPoolSizeMetricKey]int)

	statuses := []string{SandboxStatusReady, SandboxStatusPending, SandboxStatusSucceeded, SandboxStatusFailed}

	for _, pool := range warmPoolList.Items {
		key := pool.Namespace + "/" + pool.Name
		poolMap[key] = poolInfo{
			Name:      pool.Name,
			Namespace: pool.Namespace,
			Template:  pool.Spec.TemplateRef.Name,
		}

		// Pre-populate counts map with zero entries for every status.
		for _, status := range statuses {
			key := WarmPoolSizeMetricKey{
				Namespace:     pool.Namespace,
				WarmPoolName:  pool.Name,
				Template:      pool.Spec.TemplateRef.Name,
				SandboxStatus: status,
			}
			counts[key] = 0
		}
	}

	var sandboxList sandboxv1beta1.SandboxList
	if err := c.client.List(ctx, &sandboxList, client.HasLabels{sandboxv1beta1.SandboxWarmPoolLabel}, client.UnsafeDisableDeepCopy); err != nil {
		c.logger.Error(err, "Failed to list sandboxes for metrics collection")
		return
	}

	for _, sb := range sandboxList.Items {
		// Resolve the owning warm pool via the sandbox's controlling OwnerReference.
		ctrl := metav1.GetControllerOf(&sb)
		if ctrl == nil || ctrl.Kind != "SandboxWarmPool" {
			continue
		}

		pool, exists := poolMap[sb.Namespace+"/"+ctrl.Name]
		if !exists {
			// Orphaned sandbox from a deleted pool, skip.
			continue
		}

		// Resolve template name: prefer sb.Annotations[sandboxv1beta1.SandboxTemplateRefAnnotation]; fall back to pool's template.
		templateName := pool.Template
		if t, annotOk := sb.Annotations[sandboxv1beta1.SandboxTemplateRefAnnotation]; annotOk && t != "" {
			templateName = t
		}

		status := classifySandboxStatus(&sb)

		key := WarmPoolSizeMetricKey{
			Namespace:     pool.Namespace,
			WarmPoolName:  pool.Name,
			Template:      templateName,
			SandboxStatus: status,
		}
		counts[key]++
	}

	for key, count := range counts {
		ch <- prometheus.MustNewConstMetric(
			c.desc,
			prometheus.GaugeValue,
			float64(count),
			key.Namespace,
			key.WarmPoolName,
			key.Template,
			key.SandboxStatus,
		)
	}
}

func classifySandboxStatus(sb *sandboxv1beta1.Sandbox) string {
	if finished := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished)); finished != nil && finished.Status == metav1.ConditionTrue {
		switch finished.Reason {
		case sandboxv1beta1.SandboxReasonPodSucceeded:
			return SandboxStatusSucceeded
		case sandboxv1beta1.SandboxReasonPodFailed:
			return SandboxStatusFailed
		}
	}
	if ready := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady)); ready != nil && ready.Status == metav1.ConditionTrue {
		return SandboxStatusReady
	}
	return SandboxStatusPending
}
