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

package controllers

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// ensureSandboxObservabilityAnnotations stamps optional trace context, and
// controller-first-observed-at when stage recording will not run (Suspended).
// For Running sandboxes, ObservabilityAnnotation is stamped in
// recordStageLatencies so it can be batched with stage-latency-recorded.
// Annotation persistence is best-effort: patch failures are logged and do not
// fail reconcile.
func (r *SandboxReconciler) ensureSandboxObservabilityAnnotations(ctx context.Context, sandbox *sandboxv1beta1.Sandbox) {
	logger := log.FromContext(ctx)
	tc := r.Tracer.GetTraceContext(ctx)
	needObservability := sandbox.Annotations == nil || sandbox.Annotations[asmetrics.ObservabilityAnnotation] == ""
	needTraceContext := tc != "" && (sandbox.Annotations == nil || sandbox.Annotations[asmetrics.TraceContextAnnotation] == "")

	// Running path: defer ObservabilityAnnotation to recordStageLatencies for a
	// single metadata patch with stage-latency-recorded.
	persistObservabilityNow := needObservability &&
		sandbox.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended
	if !persistObservabilityNow {
		needObservability = false
	}
	if !needObservability && !needTraceContext {
		return
	}

	statusCopy := sandbox.Status.DeepCopy()
	patch := client.MergeFrom(sandbox.DeepCopy())
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string)
	}
	if needObservability {
		sandbox.Annotations[asmetrics.ObservabilityAnnotation] = time.Now().Format(time.RFC3339Nano)
	}
	if needTraceContext {
		sandbox.Annotations[asmetrics.TraceContextAnnotation] = tc
	}
	if err := r.Patch(ctx, sandbox, patch); err != nil {
		logger.Error(err, "failed to patch sandbox observability annotations; will retry")
		sandbox.Status = *statusCopy
		return
	}
	sandbox.Status = *statusCopy
}

// recordChildReconcileError increments the child reconcile error counter with an allowlisted reason.
func (r *SandboxReconciler) recordChildReconcileError(sandbox *sandboxv1beta1.Sandbox, resource, hint string, err error) {
	if err == nil || sandbox == nil {
		return
	}
	asmetrics.RecordChildReconcileError(sandbox.Namespace, resource, asmetrics.ClassifyReconcileError(err, hint))
}

type pendingStageLatency struct {
	stage   string
	latency time.Duration
}

// recordStageLatencies observes Ready-path stage latencies once per stage.
// Annotation persistence is best-effort: patch failures are logged and do not
// fail reconcile or affect Ready. Metrics are emitted only after a successful
// patch so a retry cannot double-count.
func (r *SandboxReconciler) recordStageLatencies(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod, svc *corev1.Service) {
	logger := log.FromContext(ctx)
	now := time.Now()

	// Capture merge base before mutating annotations so ObservabilityAnnotation
	// and stage-latency-recorded can be written in a single patch.
	statusCopy := sandbox.Status.DeepCopy()
	patch := client.MergeFrom(sandbox.DeepCopy())
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string)
	}

	needObservability := sandbox.Annotations[asmetrics.ObservabilityAnnotation] == ""
	if needObservability {
		sandbox.Annotations[asmetrics.ObservabilityAnnotation] = now.Format(time.RFC3339Nano)
	}

	t0 := sandboxStageStartTime(sandbox, now)
	labels := asmetrics.LabelsFromSandbox(sandbox)

	recorded := asmetrics.ParseStageLatencyRecorded(sandbox.Annotations[asmetrics.StageLatencyRecordedAnnotation])
	updated := needObservability
	var pending []pendingStageLatency

	observe := func(stage string, reached bool, endTime time.Time) {
		if !reached {
			return
		}
		if _, ok := recorded[stage]; ok {
			return
		}
		if endTime.IsZero() {
			endTime = now
		}
		// Stages reached before the controller started observing (warm launch or
		// pre-existing sandboxes after upgrade) are marked recorded without
		// observing, to avoid a spike of near-zero histogram samples.
		if endTime.Before(t0) {
			recorded[stage] = struct{}{}
			updated = true
			logger.V(4).Info("Skipping pre-observation stage latency", "stage", stage)
			return
		}
		latency := endTime.Sub(t0)
		pending = append(pending, pendingStageLatency{stage: stage, latency: latency})
		recorded[stage] = struct{}{}
		updated = true
	}

	observe(asmetrics.StagePodCreated, podCreated(pod), podCreatedTime(pod, now))
	observe(asmetrics.StagePodScheduled, podScheduled(pod), podConditionTransitionTime(pod, corev1.PodScheduled, now))
	observe(asmetrics.StagePodRunning, podRunning(pod), podRunningTime(pod, now))
	observe(asmetrics.StagePodReady, podReadyWithIP(pod), podConditionTransitionTime(pod, corev1.PodReady, now))

	if len(sandbox.Spec.VolumeClaimTemplates) > 0 {
		// Skip PVC Gets once pvc_bound is already recorded; observe() would no-op anyway.
		if _, already := recorded[asmetrics.StagePVCBound]; !already {
			allBound, boundAt := r.pvcsBound(ctx, sandbox, now)
			observe(asmetrics.StagePVCBound, allBound, boundAt)
		}
	}

	svcRequired := serviceRequired(sandbox, svc)
	if svcRequired {
		observe(asmetrics.StageServiceReady, svc != nil, serviceReadyTime(svc, now))
	}

	if !updated {
		return
	}

	if len(recorded) > 0 {
		sandbox.Annotations[asmetrics.StageLatencyRecordedAnnotation] = asmetrics.FormatStageLatencyRecorded(recorded)
	}
	if err := r.Patch(ctx, sandbox, patch); err != nil {
		// Best-effort telemetry: never surface annotation patch failures into
		// Ready/reconcile errors. Metrics were not emitted, so a later reconcile
		// can retry without double-counting.
		logger.Error(err, "failed to patch sandbox stage latency annotations; will retry")
		sandbox.Status = *statusCopy
		return
	}
	sandbox.Status = *statusCopy

	for _, p := range pending {
		asmetrics.RecordStageLatency(p.latency, labels.Namespace, labels.LaunchType, labels.Template, labels.OwnedBy, p.stage)
		logger.V(4).Info("Recorded sandbox stage latency", "stage", p.stage, "latencyMs", p.latency.Milliseconds())
	}
}

func sandboxStageStartTime(sandbox *sandboxv1beta1.Sandbox, fallback time.Time) time.Time {
	if sandbox.Annotations != nil {
		if raw := sandbox.Annotations[asmetrics.ObservabilityAnnotation]; raw != "" {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				return t
			}
		}
	}
	if !sandbox.CreationTimestamp.IsZero() {
		return sandbox.CreationTimestamp.Time
	}
	return fallback
}

func podCreated(pod *corev1.Pod) bool {
	// A newly created Pod may not have UID populated yet on the returned object
	// (especially with the fake client); existence is enough for this stage.
	return pod != nil
}

func podCreatedTime(pod *corev1.Pod, fallback time.Time) time.Time {
	if pod == nil {
		return fallback
	}
	if !pod.CreationTimestamp.IsZero() {
		return pod.CreationTimestamp.Time
	}
	return fallback
}

func podScheduled(pod *corev1.Pod) bool {
	return podConditionTrue(pod, corev1.PodScheduled)
}

func podRunning(pod *corev1.Pod) bool {
	return pod != nil && pod.Status.Phase == corev1.PodRunning
}

func podRunningTime(pod *corev1.Pod, fallback time.Time) time.Time {
	if pod == nil {
		return fallback
	}
	// Prefer StartTime when present; otherwise fall back to reconcile wall clock.
	if pod.Status.StartTime != nil && !pod.Status.StartTime.IsZero() {
		return pod.Status.StartTime.Time
	}
	return fallback
}

func podReadyWithIP(pod *corev1.Pod) bool {
	if pod == nil || len(pod.Status.PodIPs) == 0 {
		return false
	}
	return podConditionTrue(pod, corev1.PodReady)
}

func podConditionTrue(pod *corev1.Pod, condType corev1.PodConditionType) bool {
	if pod == nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == condType {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podConditionTransitionTime(pod *corev1.Pod, condType corev1.PodConditionType, fallback time.Time) time.Time {
	if pod == nil {
		return fallback
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return fallback
}

func serviceRequired(sandbox *sandboxv1beta1.Sandbox, svc *corev1.Service) bool {
	if sandbox.Spec.Service != nil {
		return *sandbox.Spec.Service
	}
	return svc != nil
}

func serviceReadyTime(svc *corev1.Service, fallback time.Time) time.Time {
	if svc == nil {
		return fallback
	}
	if !svc.CreationTimestamp.IsZero() {
		return svc.CreationTimestamp.Time
	}
	return fallback
}

// pvcsBound reports whether every VCT-backed PVC is Bound, and the latest Bound observation time.
func (r *SandboxReconciler) pvcsBound(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, fallback time.Time) (bool, time.Time) {
	if len(sandbox.Spec.VolumeClaimTemplates) == 0 {
		return false, fallback
	}
	var latestBound time.Time
	for _, pvcTemplate := range sandbox.Spec.VolumeClaimTemplates {
		pvc := &corev1.PersistentVolumeClaim{}
		pvcName := pvcTemplate.Name + "-" + sandbox.Name
		if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: sandbox.Namespace}, pvc); err != nil {
			return false, fallback
		}
		if pvc.Status.Phase != corev1.ClaimBound {
			return false, fallback
		}
		boundAt := pvcBoundTransitionTime(pvc, fallback)
		if boundAt.After(latestBound) {
			latestBound = boundAt
		}
	}
	if latestBound.IsZero() {
		latestBound = fallback
	}
	return true, latestBound
}

func pvcBoundTransitionTime(_ *corev1.PersistentVolumeClaim, fallback time.Time) time.Time {
	// PVCs have no Bound condition with LastTransitionTime. Prefer the reconciler
	// observation time (fallback) over CreationTimestamp, which undercounts bind
	// latency for dynamically provisioned volumes.
	return fallback
}
