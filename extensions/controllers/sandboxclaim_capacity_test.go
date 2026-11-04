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
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestClaimAwareAdoptionRejectsPreviousPoolIncarnation(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "recorded"}[recorded], func(t *testing.T) {
			claim, template, pool, sb := newOptimisticLockTestObjects()
			pool.Spec.ReplenishmentCapacity = new(int32(1))
			sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxWarmPoolKind, Name: pool.Name, UID: "previous-pool-uid", Controller: new(true)}}
			sb.Annotations = map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: "previous-pool-uid"}
			sb.Labels[warmPoolSandboxLabel] = "pool-member"
			sb.Labels[sandboxTemplateRefHash] = SandboxTemplateRefHash(template.Name)
			sb.Status.PodIPs = []string{"10.0.0.1"}
			if !recorded {
				delete(claim.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation)
			}
			c := newFakeClient(newTestScheme(), claim, template, pool, sb)
			r := &SandboxClaimReconciler{Client: c, APIReader: c, Scheme: newTestScheme(), WarmSandboxQueue: queue.NewSimpleSandboxQueue()}
			r.WarmSandboxQueue.Add(queue.GetNamespacedWarmPoolName(pool.Namespace, pool.Name), queue.SandboxKey{Namespace: sb.Namespace, Name: sb.Name})
			if recorded {
				_, err := r.getOrCreateSandbox(context.Background(), claim, template)
				require.ErrorIs(t, err, errAdoptionConflict)
			} else {
				candidate, _, _, err := r.getCandidate(context.Background(), claim)
				require.NoError(t, err)
				require.Nil(t, candidate, "pool names cannot authorize adoption across incarnations")
			}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sb), sb))
			require.Equal(t, "previous-pool-uid", string(metav1.GetControllerOf(sb).UID))
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(claim), claim))
			require.Empty(t, claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
		})
	}
}

func TestClaimAwareAdoptionRecoversAfterSourceOnlyRepair(t *testing.T) {
	ctx := context.Background()
	claim, template, pool, sb := newOptimisticLockTestObjects()
	pool.Spec.ReplenishmentCapacity = new(int32(1))
	delete(claim.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation)
	sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxWarmPoolKind, Name: pool.Name, UID: pool.UID, Controller: new(true)}}
	sb.Annotations = map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: "conflicting-uid"}
	sb.Labels[warmPoolSandboxLabel] = "pool-member"
	sb.Labels[sandboxTemplateRefHash] = SandboxTemplateRefHash(template.Name)
	sb.Status.PodIPs = []string{"10.0.0.1"}
	c := newFakeClient(newTestScheme(), claim, template, pool, sb)
	r := &SandboxClaimReconciler{Client: c, APIReader: c, Scheme: newTestScheme(), WarmSandboxQueue: queue.NewSimpleSandboxQueue()}
	h := &sandboxEventHandler{sandboxQueue: r.WarmSandboxQueue}
	h.Create(ctx, event.CreateEvent{Object: sb}, nil)
	candidate, _, _, err := r.getCandidate(ctx, claim)
	require.NoError(t, err)
	require.Nil(t, candidate, "conflicting provenance cannot be adopted")
	beforeRepair := sb.DeepCopy()
	sb.Annotations[sourceWarmPoolUIDAnnotation] = string(pool.UID)
	require.NoError(t, c.Update(ctx, sb))
	h.Update(ctx, event.UpdateEvent{ObjectOld: beforeRepair, ObjectNew: sb}, nil)
	adopted, _, err := r.adoptSandboxFromCandidates(ctx, claim)
	require.NoError(t, err)
	require.NotNil(t, adopted, "source-only repair must make Ready inventory claimable again")
	require.Equal(t, sb.UID, adopted.UID)
	require.True(t, metav1.IsControlledBy(adopted, claim))
	require.Equal(t, sandboxv1beta1.SandboxLaunchTypeWarm, adopted.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])
}

func TestClaimAwareAdoptionKeepsAlreadyBoundPreviousIncarnation(t *testing.T) {
	claim, template, pool, sb := newOptimisticLockTestObjects()
	pool.Spec.ReplenishmentCapacity = new(int32(1))
	sb.Annotations = map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: "previous-pool-uid"}
	c := newFakeClient(newTestScheme(), claim, template, pool, sb)
	r := &SandboxClaimReconciler{Client: c, APIReader: c, Scheme: newTestScheme()}
	bound, err := r.getOrCreateSandbox(context.Background(), claim, template)
	require.NoError(t, err)
	require.Equal(t, sb.UID, bound.UID)
	require.True(t, metav1.IsControlledBy(bound, claim))
	require.Equal(t, "previous-pool-uid", bound.Annotations[sourceWarmPoolUIDAnnotation])
}
