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
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/utils"
	"sigs.k8s.io/agent-sandbox/internal/version"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	LaunchTypeWarm    = "warm"    // Pod from a SandboxWarmPool
	LaunchTypeCold    = "cold"    // Pod not from a SandboxWarmPool
	LaunchTypeUnknown = "unknown" // Used when Sandbox is nil during failure

	OwnedByNone            = "None"
	OwnedBySandboxClaim    = extensionsv1beta1.SandboxClaimKind
	OwnedBySandboxWarmPool = extensionsv1beta1.SandboxWarmPoolKind

	// Stage names for agent_sandbox_stage_latency_ms.
	StagePodCreated   = "pod_created"
	StagePodScheduled = "pod_scheduled"
	StagePodRunning   = "pod_running"
	StagePodReady     = "pod_ready"
	StagePVCBound     = "pvc_bound"
	StageServiceReady = "service_ready"

	// Child resource names for agent_sandbox_child_reconcile_errors_total.
	ResourcePod           = "pod"
	ResourcePVC           = "pvc"
	ResourceService       = "service"
	ResourceNetworkPolicy = "networkpolicy"

	// Allowlisted reconcile error reasons.
	ReasonCreateFailed      = "create_failed"
	ReasonUpdateConflict    = "update_conflict"
	ReasonOwnershipConflict = "ownership_conflict"
	ReasonAdoptRefused      = "adopt_refused"
	ReasonDeleteFailed      = "delete_failed"
	ReasonForbidden         = "forbidden"
	ReasonOther             = "other"

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

	// CreationLatencyRecordedAnnotation marks a SandboxClaim whose startup/creation latency
	// has already been recorded, preventing double-recording (e.g. after a suspend/resume).
	CreationLatencyRecordedAnnotation = "agents.x-k8s.io/creation-latency-recorded"

	// StageLatencyRecordedAnnotation holds a comma-separated set of stage names whose
	// latency has already been recorded for a Sandbox, preventing double-recording.
	StageLatencyRecordedAnnotation = "agents.x-k8s.io/stage-latency-recorded"
)

// creationLatencyBuckets are shared by SandboxCreationLatency and SandboxStageLatency.
var creationLatencyBuckets = []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 240000, 300000, 600000}

var (
	// ClaimStartupLatency measures the time from SandboxClaim creation to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the resolved SandboxTemplateRef used to create the Sandbox.
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
	// - sandbox_template: the resolved SandboxTemplateRef used to create the Sandbox.
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
			Buckets: creationLatencyBuckets,
		},
		[]string{"namespace", "launch_type", "sandbox_template"},
	)

	// SandboxStageLatency measures time from Sandbox first-observed to each Ready-path stage.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the SandboxTemplateRef
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None"
	// - stage: allowlisted stage name (see Stage* constants).
	SandboxStageLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_stage_latency_ms",
			Help: "Latency from Sandbox controller first-observed time to each Ready-path stage in milliseconds. " +
				"Stages reached before first observation (warm launch or pre-existing sandboxes) are omitted to avoid near-zero samples.",
			Buckets: creationLatencyBuckets,
		},
		[]string{"namespace", "launch_type", "sandbox_template", "owned_by", "stage"},
	)

	// ChildReconcileErrors counts Sandbox child-resource reconcile failures.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - resource: "pod" | "pvc" | "service" | "networkpolicy"
	// - reason: allowlisted reconcile error reason.
	ChildReconcileErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_child_reconcile_errors_total",
			Help: "Total number of Sandbox child-resource reconcile failures, labeled by namespace, resource, and allowlisted reason.",
		},
		[]string{"namespace", "resource", "reason"},
	)

	// TemplateReconcileErrors counts SandboxTemplate controller reconcile failures.
	// Labels:
	// - namespace: the namespace of the template
	// - reason: allowlisted reconcile error reason.
	TemplateReconcileErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_template_reconcile_errors_total",
			Help: "Total number of SandboxTemplate reconcile failures, labeled by namespace and allowlisted reason.",
		},
		[]string{"namespace", "reason"},
	)

	// SandboxClaimCreationTotal calculates the total number of SandboxClaims created.
	// Labels:
	// - namespace: the namespace of the claim
	// - sandbox_template: the SandboxTemplateRef
	// - launch_type: "warm", "cold", "unknown"
	// - warmpool_name: the requested warm pool reference name (from SandboxClaim spec.warmPoolRef.name).
	// - pod_condition: "ready", "not_ready".
	// - created_by: the component that created the claim (e.g. "go-client", "python-client", "controller", "unknown").
	SandboxClaimCreationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_claim_creation_total",
			Help: "Total number of SandboxClaims created, labeled by namespace, sandbox template, launch type, warmpool name, pod condition, and created_by.",
		},
		[]string{"namespace", "sandbox_template", "launch_type", "warmpool_name", "pod_condition", "created_by"},
	)

	// AgentSandboxesDesc describes the agent_sandboxes metric point-in-time counts.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - ready_condition: "true" | "false"
	// - expired: "true" | "false"
	// - launch_type: "warm" | "cold"
	// - sandbox_template: sandboxTemplateRef.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	// - created_by: the component that created the sandbox (e.g. "go-client", "python-client", "controller", "unknown").
	AgentSandboxesDesc = prometheus.NewDesc(
		"agent_sandboxes",
		"Monitor the point-in-time number of sandboxes in the cluster.",
		[]string{"namespace", "ready_condition", "expired", "launch_type", "sandbox_template", "owned_by", "created_by"},
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

// Init registers custom metrics with the global controller-runtime registry.
func init() {
	metrics.Registry.MustRegister(ClaimStartupLatency)
	metrics.Registry.MustRegister(ClaimControllerStartupLatency)
	metrics.Registry.MustRegister(SandboxCreationLatency)
	metrics.Registry.MustRegister(SandboxStageLatency)
	metrics.Registry.MustRegister(ChildReconcileErrors)
	metrics.Registry.MustRegister(TemplateReconcileErrors)
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

// RecordSandboxCreationLatency records the measured latency duration for a sandbox creation.
func RecordSandboxCreationLatency(duration time.Duration, namespace, launchType, templateName string) {
	SandboxCreationLatency.WithLabelValues(namespace, launchType, templateName).Observe(float64(duration.Milliseconds()))
}

// RecordStageLatency records the measured latency for a single Sandbox Ready-path stage.
// The stage value is normalized to the allowlist; unknown stages become ReasonOther-equivalent "other".
func RecordStageLatency(duration time.Duration, namespace, launchType, templateName, ownedBy, stage string) {
	SandboxStageLatency.WithLabelValues(
		namespace,
		launchType,
		templateName,
		ownedBy,
		NormalizeStage(stage),
	).Observe(float64(duration.Milliseconds()))
}

// RecordChildReconcileError increments the child reconcile error counter.
func RecordChildReconcileError(namespace, resource, reason string) {
	ChildReconcileErrors.WithLabelValues(namespace, NormalizeResource(resource), NormalizeReason(reason)).Inc()
}

// RecordTemplateReconcileError increments the template reconcile error counter.
func RecordTemplateReconcileError(namespace, reason string) {
	TemplateReconcileErrors.WithLabelValues(namespace, NormalizeReason(reason)).Inc()
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

// NormalizeStage returns an allowlisted stage name or "other".
func NormalizeStage(stage string) string {
	switch stage {
	case StagePodCreated, StagePodScheduled, StagePodRunning, StagePodReady, StagePVCBound, StageServiceReady:
		return stage
	default:
		return ReasonOther
	}
}

// NormalizeResource returns an allowlisted child resource name or "other".
func NormalizeResource(resource string) string {
	switch resource {
	case ResourcePod, ResourcePVC, ResourceService, ResourceNetworkPolicy:
		return resource
	default:
		return ReasonOther
	}
}

// NormalizeReason returns an allowlisted reconcile error reason or "other".
func NormalizeReason(reason string) string {
	switch reason {
	case ReasonCreateFailed, ReasonUpdateConflict, ReasonOwnershipConflict, ReasonAdoptRefused, ReasonDeleteFailed, ReasonForbidden, ReasonOther:
		return reason
	default:
		return ReasonOther
	}
}

// ClassifyReconcileError maps an API error and optional semantic hint to an allowlisted reason.
// Forbidden and conflict take precedence over the hint so label values stay tied to API semantics.
func ClassifyReconcileError(err error, hint string) string {
	if err != nil {
		if apierrors.IsForbidden(err) {
			return ReasonForbidden
		}
		if apierrors.IsConflict(err) {
			return ReasonUpdateConflict
		}
	}
	return NormalizeReason(hint)
}

// SandboxMetricLabels holds common Prometheus labels derived from a Sandbox.
type SandboxMetricLabels struct {
	Namespace  string
	LaunchType string
	Template   string
	OwnedBy    string
}

// LabelsFromSandbox derives metric labels from a Sandbox's metadata and controller owner.
func LabelsFromSandbox(sandbox *sandboxv1beta1.Sandbox) SandboxMetricLabels {
	labels := SandboxMetricLabels{
		Namespace:  sandbox.Namespace,
		LaunchType: LaunchTypeCold,
		Template:   "unknown",
		OwnedBy:    OwnedByNone,
	}
	if sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] == sandboxv1beta1.SandboxLaunchTypeWarm {
		labels.LaunchType = LaunchTypeWarm
	}
	if template, ok := sandbox.Annotations[sandboxv1beta1.SandboxTemplateRefAnnotation]; ok && template != "" {
		labels.Template = template
	}
	controllerRef := metav1.GetControllerOf(sandbox)
	if g, k := utils.GetGroupKind(controllerRef); g == extensionsv1beta1.GroupVersion.Group &&
		(k == extensionsv1beta1.SandboxClaimKind || k == extensionsv1beta1.SandboxWarmPoolKind) {
		labels.OwnedBy = k
	}
	return labels
}

// ParseStageLatencyRecorded returns the set of stages already recorded from the annotation value.
// Unknown tokens are ignored so they cannot collide with allowlisted stage names.
func ParseStageLatencyRecorded(value string) map[string]struct{} {
	recorded := make(map[string]struct{})
	if value == "" {
		return recorded
	}
	for stage := range strings.SplitSeq(value, ",") {
		stage = strings.TrimSpace(stage)
		switch stage {
		case StagePodCreated, StagePodScheduled, StagePodRunning, StagePodReady, StagePVCBound, StageServiceReady:
			recorded[stage] = struct{}{}
		}
	}
	return recorded
}

// FormatStageLatencyRecorded serializes recorded stage names as a stable comma-separated list.
func FormatStageLatencyRecorded(recorded map[string]struct{}) string {
	if len(recorded) == 0 {
		return ""
	}
	stages := make([]string, 0, len(recorded))
	for stage := range recorded {
		stages = append(stages, stage)
	}
	slices.Sort(stages)
	return strings.Join(stages, ",")
}

// RecordSandboxClaimCreation increments the total count of created sandbox claims.
// The createdBy value is automatically normalized.
func RecordSandboxClaimCreation(namespace, templateName, launchType, warmPoolName, podCondition, createdBy string) {
	SandboxClaimCreationTotal.WithLabelValues(namespace, templateName, launchType, warmPoolName, podCondition, NormalizeCreatedBy(createdBy)).Inc()
}
