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
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// AgentSandboxClaimsMetricKey is used to aggregate counts for identical SandboxClaims metric label combinations.
type AgentSandboxClaimsMetricKey struct {
	Namespace   string
	ReadyStatus string
	Reason      string
	Template    string
}

// NewAgentSandboxClaimsConstMetric creates a new Prometheus ConstMetric for the agent_sandbox_claims gauge.
func NewAgentSandboxClaimsConstMetric(count int, key AgentSandboxClaimsMetricKey) prometheus.Metric {
	return prometheus.MustNewConstMetric(
		AgentSandboxClaimsDesc,
		prometheus.GaugeValue,
		float64(count),
		key.Namespace,
		key.ReadyStatus,
		key.Reason,
		key.Template,
	)
}

// RegisterSandboxClaimCollector registers the custom Prometheus collector for sandbox claim counts.
func RegisterSandboxClaimCollector(c client.Client, logger logr.Logger) {
	collector := NewSandboxClaimCollector(c, logger)
	if err := metrics.Registry.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			logger.Error(err, "Failed to register SandboxClaimCollector")
		} else {
			logger.Info("SandboxClaimCollector already registered, ignoring")
		}
	}
}

// SandboxClaimCollector is a custom Prometheus collector that dynamically fetches sandbox claim counts.
type SandboxClaimCollector struct {
	client                 client.Client
	logger                 logr.Logger
	agentSandboxClaimsDesc *prometheus.Desc
}

// NewSandboxClaimCollector initializes a SandboxClaimCollector.
func NewSandboxClaimCollector(c client.Client, logger logr.Logger) *SandboxClaimCollector {
	return &SandboxClaimCollector{
		client:                 c,
		logger:                 logger,
		agentSandboxClaimsDesc: AgentSandboxClaimsDesc,
	}
}

// Describe sends the metric descriptor to the channel.
func (c *SandboxClaimCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.agentSandboxClaimsDesc
}

// Collect fetches sandbox claims, calculates labels, and sends metrics to the channel.
func (c *SandboxClaimCollector) Collect(ch chan<- prometheus.Metric) {
	var claimList extensionsv1alpha1.SandboxClaimList
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectTimeout)
	defer cancel()

	if err := c.client.List(ctx, &claimList); err != nil {
		c.logger.Error(err, "Failed to list sandbox claims for metrics collection")
		return
	}

	counts := make(map[AgentSandboxClaimsMetricKey]int)
	for _, claim := range claimList.Items {
		readyStatusStr := "false"
		reasonStr := "Unknown"

		readyCond := meta.FindStatusCondition(claim.Status.Conditions, string(sandboxv1alpha1.SandboxConditionReady))
		if readyCond != nil {
			if readyCond.Status == metav1.ConditionTrue {
				readyStatusStr = "true"
			}
			reasonStr = readyCond.Reason
		}

		templateStr := claim.Spec.TemplateRef.Name

		key := AgentSandboxClaimsMetricKey{
			Namespace:   claim.Namespace,
			ReadyStatus: readyStatusStr,
			Reason:      reasonStr,
			Template:    templateStr,
		}
		counts[key]++
	}

	for key, count := range counts {
		ch <- NewAgentSandboxClaimsConstMetric(count, key)
	}
}
