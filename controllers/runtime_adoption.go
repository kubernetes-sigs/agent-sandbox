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
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/utils"
)

func runtimeAdoptionReady(sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod, now time.Time) bool {
	status, verification := sandbox.Status.RuntimeAdoption, sandbox.Status.RuntimeActivationVerification
	if status == nil || status.Reservation == nil || status.Grant == nil || verification == nil || pod == nil {
		return false
	}
	reservation, owner := status.Reservation, metav1.GetControllerOf(sandbox)
	return reservation.AttemptID != "" && reservation.TargetActivationID != "" && reservation.SandboxUID == sandbox.UID &&
		reservation.Namespace == sandbox.Namespace && utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, "SandboxClaim") &&
		owner.UID == reservation.ClaimUID && owner.Name == reservation.ClaimName &&
		status.TerminationRequestedTime == nil && status.TerminalEvidenceDigest == "" && sandbox.DeletionTimestamp == nil &&
		status.CommitDigest != "" && status.CommitDigest == verification.CommitDigest &&
		status.Grant.ContextDigest != "" && status.Grant.ContextDigest == verification.ContextDigest &&
		status.Grant.ReceiptDigest != "" && status.Grant.GrantDigest != "" &&
		verification.AttemptID == reservation.AttemptID && verification.ClaimUID == reservation.ClaimUID &&
		verification.TargetActivationID == reservation.TargetActivationID && verification.ReceiptDigest != "" &&
		verification.NodeUID != "" && verification.TaskStartTime > 0 && verification.RuntimeIncarnation != "" &&
		verification.ValidUntil.After(now) && meta.IsStatusConditionTrue(verification.Conditions, "Verified") &&
		verification.PodUID != "" && verification.PodUID == pod.UID && pod.Namespace == sandbox.Namespace &&
		metav1.IsControlledBy(pod, sandbox) && pod.DeletionTimestamp == nil && len(pod.Status.ContainerStatuses) == 1 &&
		verification.ContainerID != "" && verification.ContainerID == strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "containerd://")
}

// SandboxPodMetadataMatches lets the claim controller acknowledge the same
// transformation the core writer applies, using an API-server Pod readback.
func SandboxPodMetadataMatches(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod) bool {
	projectedPod := pod.DeepCopy()
	return !(&SandboxReconciler{}).updatePodMetadata(ctx, projectedPod, sandbox, NameHash(sandbox.Name))
}

// ExpectedSandboxPodMetadata returns the exact metadata patch for a live Pod.
// Admission uses the old API object so a stale pool-era write cannot overwrite
// the claim transformation after reservation.
func ExpectedSandboxPodMetadata(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod) metav1.ObjectMeta {
	projectedPod := pod.DeepCopy()
	(&SandboxReconciler{}).updatePodMetadata(ctx, projectedPod, sandbox, NameHash(sandbox.Name))
	return projectedPod.ObjectMeta
}

func (r *SandboxReconciler) reconcileRuntimeAdoptionPod(ctx context.Context, sandbox *sandboxv1beta1.Sandbox) (*corev1.Pod, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	fresh := &sandboxv1beta1.Sandbox{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(sandbox), fresh); err != nil {
		return nil, err
	}
	if fresh.UID != sandbox.UID {
		return nil, errors.New("reserved Sandbox was replaced")
	}
	adoption := fresh.Status.RuntimeAdoption
	if adoption == nil || adoption.Reservation == nil || adoption.Initialization == nil {
		return nil, errors.New("reserved Sandbox lost its durable runtime identity")
	}
	var pod corev1.Pod
	if err := reader.Get(ctx, client.ObjectKey{Namespace: fresh.Namespace, Name: resolvePodName(fresh)}, &pod); err != nil {
		// A reserved task can never be reconstructed by ordinary Pod creation.
		return nil, fmt.Errorf("read retained adoption Pod: %w", err)
	}
	if !metav1.IsControlledBy(&pod, fresh) {
		return nil, errors.New("retained adoption Pod has a different Sandbox owner")
	}
	if fresh.DeletionTimestamp != nil || pod.DeletionTimestamp != nil || adoption.TerminationRequestedTime != nil ||
		adoption.HoldEvidenceDigest == "" || adoption.TargetMetadataDigest != "" || adoption.CommitDigest != "" {
		return &pod, nil
	}
	before := pod.DeepCopy()
	if r.updatePodMetadata(ctx, &pod, fresh, NameHash(fresh.Name)) {
		if err := r.Patch(ctx, &pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, fmt.Errorf("write held adoption Pod metadata: %w", err)
		}
		// The patch response may include admission changes. A fresh GET is the
		// observable completion boundary, including when write deferral is on.
		writtenUID := pod.UID
		if err := reader.Get(ctx, client.ObjectKeyFromObject(&pod), &pod); err != nil {
			return nil, fmt.Errorf("read finalized adoption Pod metadata: %w", err)
		}
		if pod.UID != writtenUID || !SandboxPodMetadataMatches(ctx, fresh, &pod) {
			return nil, errors.New("adoption Pod metadata readback does not match the held target")
		}
	}
	return &pod, nil
}
