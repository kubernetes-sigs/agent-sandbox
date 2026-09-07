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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

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
