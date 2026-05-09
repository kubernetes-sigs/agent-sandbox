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

// nolint:revive
package metrics

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricsCollectTimeout = 5 * time.Second
)

// AgentSandboxesMetricKey is used to aggregate counts for identical Sandboxes metric label combinations.
type AgentSandboxesMetricKey struct {
	Namespace      string
	ReadyCondition string
	Expired        string
	LaunchType     string
	Template       string
}

// NewAgentSandboxesConstMetric creates a new Prometheus ConstMetric for the agent_sandboxes gauge.
func NewAgentSandboxesConstMetric(count int, key AgentSandboxesMetricKey) prometheus.Metric {
	return prometheus.MustNewConstMetric(
		AgentSandboxesDesc,
		prometheus.GaugeValue,
		float64(count),
		key.Namespace,
		key.ReadyCondition,
		key.Expired,
		key.LaunchType,
		key.Template,
	)
}

// RegisterSandboxCollector registers the custom Prometheus collector for sandbox counts.
func RegisterSandboxCollector(c client.Client, logger logr.Logger) {
	collector := NewSandboxCollector(c, logger)
	if err := metrics.Registry.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			logger.Error(err, "Failed to register SandboxCollector")
		} else {
			logger.Info("SandboxCollector already registered, ignoring")
		}
	}
}

// SandboxCollector is a custom Prometheus collector that dynamically fetches sandbox counts.
type SandboxCollector struct {
	client             client.Client
	logger             logr.Logger
	agentSandboxesDesc *prometheus.Desc
}

// NewSandboxCollector initializes a SandboxCollector.
func NewSandboxCollector(c client.Client, logger logr.Logger) *SandboxCollector {
	return &SandboxCollector{
		client:             c,
		logger:             logger,
		agentSandboxesDesc: AgentSandboxesDesc,
	}
}

// Describe sends the metric descriptor to the channel.
func (c *SandboxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.agentSandboxesDesc
}

// Collect fetches sandboxes, calculates labels, and sends metrics to the channel.
// UnsafeDisableDeepCopy avoids O(N) deep-copy overhead on every scrape; safe here because
// Collect only reads fields for label aggregation and never mutates or retains the objects.
// A GaugeVec updated in the Reconcile loop would be more performant (O(1) per scrape),
// but this is a known trade-off to keep the Reconcile loop simpler.
func (c *SandboxCollector) Collect(ch chan<- prometheus.Metric) {
	var sandboxList sandboxv1alpha1.SandboxList
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectTimeout)
	defer cancel()

	if err := c.client.List(ctx, &sandboxList, client.UnsafeDisableDeepCopy); err != nil {
		c.logger.Error(err, "Failed to list sandboxes for metrics collection")
		return
	}

	counts := make(map[AgentSandboxesMetricKey]int)
	for _, sandbox := range sandboxList.Items {
		readyConditionStr := "false"
		expiredStr := "false"
		readyCond := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionReady))
		if readyCond != nil {
			if readyCond.Status == metav1.ConditionTrue {
				readyConditionStr = "true"
			}
			if readyCond.Reason == sandboxv1alpha1.SandboxReasonExpired {
				expiredStr = "true"
			}
		}

		launchTypeStr := LaunchTypeCold
		if _, ok := sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; ok && sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation] != "" {
			launchTypeStr = LaunchTypeWarm
		}

		sandboxTemplateStr := "unknown"
		// If a user manually creates a Sandbox without a SandboxClaim, it won't have the
		// SandboxTemplateRefAnnotation. The collector correctly handles this by defaulting to "unknown".
		if template, ok := sandbox.Annotations[sandboxv1alpha1.SandboxTemplateRefAnnotation]; ok && template != "" {
			sandboxTemplateStr = template
		}

		key := AgentSandboxesMetricKey{
			Namespace:      sandbox.Namespace,
			ReadyCondition: readyConditionStr,
			Expired:        expiredStr,
			LaunchType:     launchTypeStr,
			Template:       sandboxTemplateStr,
		}
		counts[key]++
	}

	for key, count := range counts {
		ch <- NewAgentSandboxesConstMetric(count, key)
	}
}
