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

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/utils"
	"sigs.k8s.io/agent-sandbox/internal/version"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	LaunchTypeWarm    = "warm"    // Pod from a SandboxWarmPool
	LaunchTypeCold    = "cold"    // Pod not from a SandboxWarmPool
	LaunchTypeUnknown = "unknown" // Used when Sandbox is nil during failure

	// SuspendResult values for agent_sandbox_suspend_total.
	SuspendResultRequested     = "requested"
	SuspendResultPodTerminated = "pod_terminated"
	SuspendResultError         = "error"

	// ResumeResult values for agent_sandbox_resume_total.
	ResumeResultRequested = "requested"
	ResumeResultReady     = "ready"
	ResumeResultError     = "error"

	// OwnedByNone is the fallback owned_by label value when a Sandbox is not
	// controlled by an extensions resource (SandboxClaim or SandboxWarmPool).
	OwnedByNone = "None"

	// UnknownTemplateSentinel is the fallback sandbox_template label value on
	// Sandbox-scoped metrics when no template annotation is present.
	UnknownTemplateSentinel = "unknown"

	// ClientAnnotation is the annotation key for the client request time.
	ClientAnnotation = "agents.x-k8s.io/client-first-requested-at"

	// Sandbox status values for the agent_sandbox_warmpool_size metric.
	// Derived from Sandbox.Status.Conditions, not Pod state.
	SandboxStatusReady     = "ready"
	SandboxStatusPending   = "pending"
	SandboxStatusSucceeded = "succeeded"
	SandboxStatusFailed    = "failed"

	// ObservabilityAnnotation is the annotation key for the time the controller first observed the claim.
	ObservabilityAnnotation = "agents.x-k8s.io/controller-first-observed-at"

	// ClaimFirstReadyAnnotation is the annotation key for the time the SandboxClaim first reached Ready state.
	// It is usually an RFC3339Nano timestamp, but may be ClaimFirstReadyUnknownSentinel
	// when the controller has to backfill the guard after the original timestamp Patch fails.
	ClaimFirstReadyAnnotation = "agents.x-k8s.io/claim-first-ready-at"

	// ClaimFirstReadyUnknownSentinel marks a claim as already counted when the controller
	// can no longer recover the original first-ready timestamp.
	ClaimFirstReadyUnknownSentinel = "unknown"

	// WebhookAnnotation is the annotation key for the time the webhook first saw the claim.
	WebhookAnnotation = "agents.x-k8s.io/webhook-first-observed-at"
)

var (
	// ClaimStartupLatency measures the time from the webhook first observing the SandboxClaim to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the resolved SandboxTemplateRef used to create the Sandbox, or "__unknown__"
	//   when the Sandbox carries no template annotation, or when no Sandbox was resolved at all.
	ClaimStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_claim_startup_latency_ms",
			Help: "End-to-end latency from a mutating admission webhook first observing the SandboxClaim (the " +
				"agents.x-k8s.io/webhook-first-observed-at annotation) to the claim reaching Ready in milliseconds. " +
				"Note: Only recorded for claims carrying that annotation.",
			// Buckets for latency from 100ms to 4 minutes
			Buckets: []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "sandbox_template"},
	)

	// ClaimControllerStartupLatency measures the time from controller first observed timestamp to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the resolved SandboxTemplateRef used to create the Sandbox, or "__unknown__"
	//   when the Sandbox carries no template annotation, or when no Sandbox was resolved at all.
	ClaimControllerStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_claim_controller_startup_latency_ms",
			Help: "Latency from the controller first observing the SandboxClaim to the claim reaching Ready in milliseconds.",
			// Buckets for latency from 100ms to 4 minutes
			Buckets: []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "sandbox_template"},
	)

	// ClientClaimStartupLatency measures the time from client request to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the resolved SandboxTemplateRef used to create the Sandbox, or "__unknown__"
	//   when the Sandbox carries no template annotation, or when no Sandbox was resolved at all.
	ClientClaimStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_client_claim_startup_latency_ms",
			Help: "End-to-end latency from the client-recorded request time (the " +
				"agents.x-k8s.io/client-first-requested-at annotation) to the claim reaching Ready in milliseconds. " +
				"Note: Only recorded for claims carrying that annotation, and may be affected by clock skew " +
				"between the client and controller.",
			// Buckets for latency from 100ms to 4 minutes
			Buckets: []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "sandbox_template"},
	)

	// SandboxCreationLatency measures the time from Sandbox creation to the Sandbox Ready condition.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - launch_type: "warm", "cold"
	// - sandbox_template: the SandboxTemplateRef, or "__unknown__" when the Sandbox carries no template
	//   annotation. This metric is only recorded for a resolved Sandbox, so the no-Sandbox case cannot occur.
	SandboxCreationLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_creation_latency_ms",
			Help: "Latency from Sandbox creation to the Sandbox Ready condition in milliseconds. " +
				"Note: For warm launches the Sandbox is created by the SandboxWarmPool, so this measures the " +
				"pool's provisioning time; a claim may adopt the Sandbox before or after it becomes Ready.",
			// Buckets for latency from 50ms to 10 minutes
			Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 240000, 300000, 600000},
		},
		[]string{"namespace", "launch_type", "sandbox_template"},
	)

	// SandboxSuspendTotal counts Sandbox suspend lifecycle transitions by result.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - sandbox_template: the SandboxTemplateRef annotation, or "unknown" when absent.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	// - result: "requested" | "pod_terminated" | "error".
	SandboxSuspendTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_suspend_total",
			Help: "Total number of Sandbox suspend lifecycle events by result.",
		},
		[]string{"namespace", "sandbox_template", "owned_by", "result"},
	)

	// SandboxSuspendLatency measures the time from the controller first observing operatingMode=Suspended
	// to the Sandbox reaching Suspended=True (PodTerminated).
	// Labels:
	// - namespace: the namespace of the sandbox
	// - sandbox_template: the SandboxTemplateRef annotation, or "unknown" when absent.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	SandboxSuspendLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_suspend_latency_ms",
			Help: "Latency from the controller first observing operatingMode=Suspended to the Sandbox reaching Suspended=True (PodTerminated), in milliseconds.",
			// Buckets for latency from 50ms to 10 minutes
			Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 240000, 300000, 600000},
		},
		[]string{"namespace", "sandbox_template", "owned_by"},
	)

	// SandboxResumeTotal counts Sandbox resume lifecycle transitions by result.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - sandbox_template: the SandboxTemplateRef annotation, or "unknown" when absent.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	// - result: "requested" | "ready" | "error".
	SandboxResumeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_resume_total",
			Help: "Total number of Sandbox resume lifecycle events by result.",
		},
		[]string{"namespace", "sandbox_template", "owned_by", "result"},
	)

	// SandboxResumeLatency measures the time from the controller first observing a Sandbox leaving
	// a fully-suspended state (operatingMode set back to Running) to the Sandbox Ready condition becoming True.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - sandbox_template: the SandboxTemplateRef annotation, or "unknown" when absent.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	SandboxResumeLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_resume_latency_ms",
			Help: "Latency from the controller first observing operatingMode=Running (after a fully-suspended state) to Sandbox Ready, in milliseconds.",
			// Buckets for latency from 50ms to 10 minutes; resume recreates the Pod.
			Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 240000, 300000, 600000},
		},
		[]string{"namespace", "sandbox_template", "owned_by"},
	)

	// SandboxFinishedTotal counts Sandboxes whose backing Pod reached a terminal phase (Finished=True).
	// Labels:
	// - namespace: the namespace of the sandbox
	// - sandbox_template: the SandboxTemplateRef annotation, or "unknown" when absent.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	// - reason: "PodSucceeded" | "PodFailed".
	SandboxFinishedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_finished_total",
			Help: "Total number of Sandboxes whose backing Pod reached a terminal phase (Finished=True).",
		},
		[]string{"namespace", "sandbox_template", "owned_by", "reason"},
	)

	// SandboxExpiredTotal counts Sandboxes that reached their shutdownTime and expired.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - sandbox_template: the SandboxTemplateRef annotation, or "unknown" when absent.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	// - shutdown_policy: "Delete" | "Retain".
	SandboxExpiredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_expired_total",
			Help: "Total number of Sandboxes that reached their shutdownTime and expired.",
		},
		[]string{"namespace", "sandbox_template", "owned_by", "shutdown_policy"},
	)

	// SandboxClaimCreationTotal counts the Sandboxes created or adopted for a SandboxClaim.
	// Labels:
	// - namespace: the namespace of the claim
	// - sandbox_template: the SandboxTemplateRef, or "__unknown__" when the created or adopted
	//   Sandbox carries no template annotation.
	// - launch_type: "warm", "cold"
	// - warmpool_name: for warm launches, the SandboxWarmPool owning the adopted Sandbox; always
	//   set, because verifySandboxCandidate rejects a candidate with no pool owner before adoption.
	//   For cold launches, the requested warm pool reference name (from
	//   SandboxClaim spec.warmPoolRef.name).
	// - pod_condition: "ready" when the Sandbox has Ready=True at creation/adoption time; otherwise "not_ready".
	// - created_by: the component that created the claim (e.g. "go-client", "python-client", "controller", "unknown").
	SandboxClaimCreationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_claim_creation_total",
			Help: "Total number of Sandboxes created or adopted for a SandboxClaim.",
		},
		[]string{"namespace", "sandbox_template", "launch_type", "warmpool_name", "pod_condition", "created_by"},
	)

	// AgentSandboxesDesc describes the agent_sandboxes metric point-in-time counts.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - ready_condition: "true" | "false"
	// - expired: "true" | "false"
	// - launch_type: "warm" | "cold"
	// - sandbox_template: sandboxTemplateRef, or "unknown" when the Sandbox carries no template annotation.
	//   Note this sentinel differs from the "__unknown__" the SandboxClaim metrics use, so the two
	//   families do not join on sandbox_template for templateless Sandboxes.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	// - created_by: the component that created the sandbox (e.g. "go-client", "python-client", "controller", "unknown").
	AgentSandboxesDesc = prometheus.NewDesc(
		"agent_sandboxes",
		"Monitor the point-in-time number of sandboxes in the cluster.",
		[]string{"namespace", "ready_condition", "expired", "launch_type", "sandbox_template", "owned_by", "created_by"},
		nil,
	)

	// AgentSandboxWarmPoolSizeDesc describes the agent_sandbox_warmpool_size metric.
	// Labels:
	// - namespace: the namespace of the warm pool
	// - warmpool_name: the name of the warm pool
	// - sandbox_template: the SandboxTemplateRef
	// - sandbox_status: "ready" | "pending" | "succeeded" | "failed".
	AgentSandboxWarmPoolSizeDesc = prometheus.NewDesc(
		"agent_sandbox_warmpool_size",
		"Number of sandboxes currently in each warm pool, labeled by sandbox status.",
		[]string{"namespace", "warmpool_name", "sandbox_template", "sandbox_status"},
		nil,
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

// init registers custom metrics with the global controller-runtime registry.
func init() {
	metrics.Registry.MustRegister(ClaimStartupLatency)
	metrics.Registry.MustRegister(ClaimControllerStartupLatency)
	metrics.Registry.MustRegister(ClientClaimStartupLatency)
	metrics.Registry.MustRegister(SandboxCreationLatency)
	metrics.Registry.MustRegister(SandboxSuspendTotal)
	metrics.Registry.MustRegister(SandboxSuspendLatency)
	metrics.Registry.MustRegister(SandboxResumeTotal)
	metrics.Registry.MustRegister(SandboxResumeLatency)
	metrics.Registry.MustRegister(SandboxFinishedTotal)
	metrics.Registry.MustRegister(SandboxExpiredTotal)
	metrics.Registry.MustRegister(SandboxClaimCreationTotal)
	metrics.Registry.MustRegister(BuildInfo)
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

// RecordClientClaimStartupLatency records the duration since the client request time.
func RecordClientClaimStartupLatency(ctx context.Context, startTime time.Time, launchType, templateName string) {
	duration := float64(time.Since(startTime).Milliseconds())
	if duration < 0 {
		logger := log.FromContext(ctx)
		logger.V(1).Info("negative latency", "duration", duration, "launchType", launchType, "templateName", templateName)
		return
	}
	ClientClaimStartupLatency.WithLabelValues(launchType, templateName).Observe(duration)
}

// RecordSandboxCreationLatency records the measured latency duration for a sandbox creation.
func RecordSandboxCreationLatency(duration time.Duration, namespace, launchType, templateName string) {
	SandboxCreationLatency.WithLabelValues(namespace, launchType, templateName).Observe(float64(duration.Milliseconds()))
}

// NormalizeCreatedBy returns the createdBy label normalized to a known allow-list
// (go-client, python-client, controller) or "unknown" for anything else.
func NormalizeCreatedBy(createdBy string) string {
	switch createdBy {
	case "go-client", "python-client", "controller":
		return createdBy
	default:
		return "unknown"
	}
}

// NormalizeOwnedBy returns the owned_by label normalized to a known allow-list
// (SandboxClaim, SandboxWarmPool, None).
func NormalizeOwnedBy(ownedBy string) string {
	switch ownedBy {
	case extensionsv1beta1.SandboxClaimKind, extensionsv1beta1.SandboxWarmPoolKind:
		return ownedBy
	default:
		return OwnedByNone
	}
}

// NormalizeSuspendResult returns the suspend result label normalized to a known allow-list
// (requested, pod_terminated, error) or "unknown" for anything else.
func NormalizeSuspendResult(result string) string {
	switch result {
	case SuspendResultRequested, SuspendResultPodTerminated, SuspendResultError:
		return result
	default:
		return "unknown"
	}
}

// NormalizeResumeResult returns the resume result label normalized to a known allow-list
// (requested, ready, error) or "unknown" for anything else.
func NormalizeResumeResult(result string) string {
	switch result {
	case ResumeResultRequested, ResumeResultReady, ResumeResultError:
		return result
	default:
		return "unknown"
	}
}

// NormalizeFinishedReason returns the finished reason label normalized to a known allow-list
// (PodSucceeded, PodFailed) or "unknown" for anything else.
func NormalizeFinishedReason(reason string) string {
	switch reason {
	case sandboxv1beta1.SandboxReasonPodSucceeded, sandboxv1beta1.SandboxReasonPodFailed:
		return reason
	default:
		return "unknown"
	}
}

// NormalizeShutdownPolicy returns the shutdown_policy label normalized to a known allow-list
// (Delete, Retain), defaulting to Retain when unset or unrecognized.
func NormalizeShutdownPolicy(policy string) string {
	switch sandboxv1beta1.ShutdownPolicy(policy) {
	case sandboxv1beta1.ShutdownPolicyDelete:
		return string(sandboxv1beta1.ShutdownPolicyDelete)
	default:
		return string(sandboxv1beta1.ShutdownPolicyRetain)
	}
}

// SandboxTemplateLabel extracts the sandbox_template label value from a Sandbox's annotations,
// defaulting to "unknown" when absent.
func SandboxTemplateLabel(sandbox *sandboxv1beta1.Sandbox) string {
	if sandbox != nil {
		if template, ok := sandbox.Annotations[sandboxv1beta1.SandboxTemplateRefAnnotation]; ok && template != "" {
			return template
		}
	}
	return UnknownTemplateSentinel
}

// SandboxOwnedByLabel extracts the normalized owned_by label value ("SandboxClaim",
// "SandboxWarmPool", or "None") from a Sandbox's controller owner reference.
func SandboxOwnedByLabel(sandbox *sandboxv1beta1.Sandbox) string {
	if sandbox == nil {
		return OwnedByNone
	}
	controllerRef := metav1.GetControllerOf(sandbox)
	// Owner references keep the apiVersion that was current when they
	// were written; sandboxes created before the v1beta1 upgrade still
	// carry the v1alpha1 group version. Match on group, not version.
	if g, k := utils.GetGroupKind(controllerRef); g == extensionsv1beta1.GroupVersion.Group &&
		(k == extensionsv1beta1.SandboxClaimKind || k == extensionsv1beta1.SandboxWarmPoolKind) {
		return k
	}
	return OwnedByNone
}

// SandboxShutdownPolicyLabel extracts the normalized shutdown_policy label value ("Delete" or "Retain")
// from a Sandbox's spec.
func SandboxShutdownPolicyLabel(sandbox *sandboxv1beta1.Sandbox) string {
	if sandbox != nil && sandbox.Spec.ShutdownPolicy != nil {
		return NormalizeShutdownPolicy(string(*sandbox.Spec.ShutdownPolicy))
	}
	return string(sandboxv1beta1.ShutdownPolicyRetain)
}

// RecordSandboxSuspend increments the total count of Sandbox suspend lifecycle events.
func RecordSandboxSuspend(namespace, templateName, ownedBy, result string) {
	if templateName == "" {
		templateName = UnknownTemplateSentinel
	}
	SandboxSuspendTotal.WithLabelValues(namespace, templateName, NormalizeOwnedBy(ownedBy), NormalizeSuspendResult(result)).Inc()
}

// RecordSandboxSuspendLatency records the measured latency duration for a Sandbox suspend.
func RecordSandboxSuspendLatency(duration time.Duration, namespace, templateName, ownedBy string) {
	if templateName == "" {
		templateName = UnknownTemplateSentinel
	}
	SandboxSuspendLatency.WithLabelValues(namespace, templateName, NormalizeOwnedBy(ownedBy)).Observe(float64(duration.Milliseconds()))
}

// RecordSandboxResume increments the total count of Sandbox resume lifecycle events.
func RecordSandboxResume(namespace, templateName, ownedBy, result string) {
	if templateName == "" {
		templateName = UnknownTemplateSentinel
	}
	SandboxResumeTotal.WithLabelValues(namespace, templateName, NormalizeOwnedBy(ownedBy), NormalizeResumeResult(result)).Inc()
}

// RecordSandboxResumeLatency records the measured latency duration for a Sandbox resume.
func RecordSandboxResumeLatency(duration time.Duration, namespace, templateName, ownedBy string) {
	if templateName == "" {
		templateName = UnknownTemplateSentinel
	}
	SandboxResumeLatency.WithLabelValues(namespace, templateName, NormalizeOwnedBy(ownedBy)).Observe(float64(duration.Milliseconds()))
}

// RecordSandboxFinished increments the total count of Sandboxes reaching Finished=True.
func RecordSandboxFinished(namespace, templateName, ownedBy, reason string) {
	if templateName == "" {
		templateName = UnknownTemplateSentinel
	}
	SandboxFinishedTotal.WithLabelValues(namespace, templateName, NormalizeOwnedBy(ownedBy), NormalizeFinishedReason(reason)).Inc()
}

// RecordSandboxExpired increments the total count of Sandboxes that expired.
func RecordSandboxExpired(namespace, templateName, ownedBy, shutdownPolicy string) {
	if templateName == "" {
		templateName = UnknownTemplateSentinel
	}
	SandboxExpiredTotal.WithLabelValues(namespace, templateName, NormalizeOwnedBy(ownedBy), NormalizeShutdownPolicy(shutdownPolicy)).Inc()
}

// RecordSandboxClaimCreation increments the total count of Sandboxes created or adopted for a SandboxClaim.
// The createdBy value is automatically normalized.
func RecordSandboxClaimCreation(namespace, templateName, launchType, warmPoolName, podCondition, createdBy string) {
	SandboxClaimCreationTotal.WithLabelValues(namespace, templateName, launchType, warmPoolName, podCondition, NormalizeCreatedBy(createdBy)).Inc()
}
