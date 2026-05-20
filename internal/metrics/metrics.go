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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/version"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	LaunchTypeWarm    = "warm"    // Pod from a SandboxWarmPool
	LaunchTypeCold    = "cold"    // Pod not from a SandboxWarmPool
	LaunchTypeUnknown = "unknown" // Used when Sandbox is nil during failure

	// ObservabilityAnnotation is the annotation key for the time the controller first observed the claim.
	ObservabilityAnnotation = "agents.x-k8s.io/controller-first-observed-at"

	// WebhookAnnotation is the annotation key for the time the webhook first saw the claim.
	WebhookAnnotation = "agents.x-k8s.io/webhook-first-observed-at"
)

// AgentSandboxesMetricKey is used to aggregate counts for identical Sandboxes metric label combinations.
type AgentSandboxesMetricKey struct {
	Namespace      string
	ReadyCondition string
	Expired        string
	LaunchType     string
	Template       string
	OwnedBy        string
}

// Slice returns the label values in the order defined by the metric.
func (k AgentSandboxesMetricKey) Slice() []string {
	return []string{
		k.Namespace,
		k.ReadyCondition,
		k.Expired,
		k.LaunchType,
		k.Template,
		k.OwnedBy,
	}
}

var (
	// ClaimStartupLatency measures the time from SandboxClaim creation to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the SandboxTemplateRef.
	ClaimStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_claim_startup_latency_ms",
			Help: "End-to-end latency from SandboxClaim creation to Sandbox Ready state in milliseconds.",
			// Buckets for latency from 100ms to 4 minutes
			Buckets: []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "sandbox_template"},
	)

	// ClaimControllerStartupLatency measures the time from controller first observed timestamp to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the SandboxTemplateRef.
	ClaimControllerStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_claim_controller_startup_latency_ms",
			Help: "Latency from controller first observed SandboxClaim to Sandbox Ready state in milliseconds.",
			// Buckets for latency from 100ms to 4 minutes
			Buckets: []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "sandbox_template"},
	)

	// SandboxCreationLatency measures the time from Sandbox creation to Pod Ready state.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the SandboxTemplateRef.
	SandboxCreationLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_creation_latency_ms",
			Help: "Latency from Sandbox creation to Pod Ready state in milliseconds. For warm launches, this measures controller synchronization overhead since the Pod is pre-provisioned.",
			// Buckets for latency from 50ms to 10 minutes
			Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 240000, 300000, 600000},
		},
		[]string{"namespace", "launch_type", "sandbox_template"},
	)

	// SandboxClaimCreationTotal calculates the total number of SandboxClaims created.
	// Labels:
	// - namespace: the namespace of the claim
	// - sandbox_template: the SandboxTemplateRef
	// - launch_type: "warm", "cold", "unknown"
	// - warmpool_name: the name of the warm pool (if applicable)
	// - pod_condition: "ready", "not_ready".
	SandboxClaimCreationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_claim_creation_total",
			Help: "Total number of SandboxClaims created, labeled by namespace, sandbox template, launch type, warmpool name, and pod condition.",
		},
		[]string{"namespace", "sandbox_template", "launch_type", "warmpool_name", "pod_condition"},
	)

	// AgentSandboxes monitor the point-in-time number of sandboxes in the cluster.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - ready_condition: "true" | "false"
	// - expired: "true" | "false"
	// - launch_type: "warm" | "cold"
	// - sandbox_template: sandboxTemplateRef.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	AgentSandboxes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agent_sandboxes",
			Help: "Monitor the point-in-time number of sandboxes in the cluster.",
		},
		[]string{"namespace", "ready_condition", "expired", "launch_type", "sandbox_template", "owned_by"},
	)

	buildVersionInfo = version.Get()

	// BuildInfo exposes agent-sandbox-controller build metadata as a constant gauge.
	BuildInfo = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "agent_sandbox_build_info",
			Help: "Agent sandbox controller build metadata exposed as labels with a constant value of 1.",
			ConstLabels: prometheus.Labels{
				"git_version": buildVersionInfo.GitVersion,
				"git_commit":  buildVersionInfo.GitSHA,
				"build_date":  buildVersionInfo.BuildDate,
				"go_version":  buildVersionInfo.GoVersion,
				"compiler":    buildVersionInfo.Compiler,
				"platform":    buildVersionInfo.Platform,
			},
		},
		func() float64 { return 1 },
	)
)

// Init registers custom metrics with the global controller-runtime registry.
func init() {
	metrics.Registry.MustRegister(ClaimStartupLatency)
	metrics.Registry.MustRegister(ClaimControllerStartupLatency)
	metrics.Registry.MustRegister(SandboxCreationLatency)
	metrics.Registry.MustRegister(SandboxClaimCreationTotal)
	metrics.Registry.MustRegister(AgentSandboxes)
	metrics.Registry.MustRegister(BuildInfo)
}

// CalculateAgentSandboxesMetricKey calculates the metric labels for a given Sandbox.
func CalculateAgentSandboxesMetricKey(sandbox *sandboxv1beta1.Sandbox) AgentSandboxesMetricKey {
	readyConditionStr := "false"
	expiredStr := "false"
	readyCond := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if readyCond != nil {
		if readyCond.Status == metav1.ConditionTrue {
			readyConditionStr = "true"
		}
		if readyCond.Reason == sandboxv1beta1.SandboxReasonExpired {
			expiredStr = "true"
		}
	}

	launchTypeStr := LaunchTypeCold
	if _, ok := sandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]; ok && sandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation] != "" {
		launchTypeStr = LaunchTypeWarm
	}

	sandboxTemplateStr := "unknown"
	if template, ok := sandbox.Annotations[sandboxv1beta1.SandboxTemplateRefAnnotation]; ok && template != "" {
		sandboxTemplateStr = template
	}

	apiVersion := extensionsv1beta1.GroupVersion.String()
	ownedByStr := "None"
	if controllerRef := metav1.GetControllerOf(sandbox); controllerRef != nil {
		if controllerRef.APIVersion == apiVersion {
			switch controllerRef.Kind {
			case "SandboxClaim":
				ownedByStr = "SandboxClaim"
			case "SandboxWarmPool":
				ownedByStr = "SandboxWarmPool"
			}
		}
	}

	return AgentSandboxesMetricKey{
		Namespace:      sandbox.Namespace,
		ReadyCondition: readyConditionStr,
		Expired:        expiredStr,
		LaunchType:     launchTypeStr,
		Template:       sandboxTemplateStr,
		OwnedBy:        ownedByStr,
	}
}


// RecordClaimStartupLatency records the duration since the provided start time.
func RecordClaimStartupLatency(startTime time.Time, launchType, templateName string) {
	duration := float64(time.Since(startTime).Milliseconds())
	ClaimStartupLatency.WithLabelValues(launchType, templateName).Observe(duration)
}

// RecordClaimControllerStartupLatency records the duration since the provided controller start time.
func RecordClaimControllerStartupLatency(startTime time.Time, launchType, templateName string) {
	duration := float64(time.Since(startTime).Milliseconds())
	ClaimControllerStartupLatency.WithLabelValues(launchType, templateName).Observe(duration)
}

// RecordSandboxCreationLatency records the measured latency duration for a sandbox creation.
func RecordSandboxCreationLatency(duration time.Duration, namespace, launchType, templateName string) {
	SandboxCreationLatency.WithLabelValues(namespace, launchType, templateName).Observe(float64(duration.Milliseconds()))
}

// RecordSandboxClaimCreation increments the total count of created sandbox claims.
func RecordSandboxClaimCreation(namespace, templateName, launchType, warmPoolName, podCondition string) {
	SandboxClaimCreationTotal.WithLabelValues(namespace, templateName, launchType, warmPoolName, podCondition).Inc()
}
