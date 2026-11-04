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
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func capacityInteractionSandbox(pool *extensionsv1beta1.SandboxWarmPool, template *extensionsv1beta1.SandboxTemplate, suffix string) *sandboxv1beta1.Sandbox {
	sb := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, suffix)
	sb.UID = types.UID(sb.Name)
	sb.Annotations = map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)}
	sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxWarmPoolKind, Name: pool.Name, UID: pool.UID, Controller: new(true)}}
	sb.Status.Conditions = []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	return sb
}

func reconcileCapacityInteraction(t *testing.T, r *SandboxWarmPoolReconciler, pool *extensionsv1beta1.SandboxWarmPool) ctrl.Result {
	t.Helper()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(), request.NamespacedName, pool))
	return result
}

func capacityInteractionStandby(t *testing.T, r *SandboxWarmPoolReconciler, pool *extensionsv1beta1.SandboxWarmPool) []sandboxv1beta1.Sandbox {
	t.Helper()
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, r.List(context.Background(), list, client.InNamespace(pool.Namespace)))
	var standby []sandboxv1beta1.Sandbox
	for _, sb := range list.Items {
		if metav1.IsControlledBy(&sb, pool) {
			standby = append(standby, sb)
		}
	}
	return standby
}

func TestClaimAwarePoolRefillDelayAndRate(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	pool := newReplenishTestPool(6)
	pool.Spec.ReplenishmentCapacity = new(int32(6))
	template := createTemplate(pool.Namespace)
	used := capacityInteractionSandbox(pool, template, "-used")
	used.OwnerReferences = nil
	used2 := capacityInteractionSandbox(pool, template, "-used2")
	used2.OwnerReferences = nil
	stock := []*sandboxv1beta1.Sandbox{
		capacityInteractionSandbox(pool, template, "-a"),
		capacityInteractionSandbox(pool, template, "-b"),
		capacityInteractionSandbox(pool, template, "-c"),
		capacityInteractionSandbox(pool, template, "-d"),
	}
	scheme := newTestScheme()
	r := &SandboxWarmPoolReconciler{
		Client: newFakeClient(scheme, pool, template, used, used2, stock[0], stock[1], stock[2], stock[3]),
		Scheme: scheme, MaxBatchSize: 6, ReplenishDelay: 5 * time.Second, MaxRefillRate: 1,
		now: func() time.Time { return now },
	}
	reconcileCapacityInteraction(t, r, pool)
	require.Equal(t, new(int32(4)), pool.Status.DesiredStandbyReplicas)
	require.Len(t, capacityInteractionStandby(t, r, pool), 4)

	// Lost standby still uses the existing delay, but refills only to B-C, not R.
	for _, sb := range stock[1:] {
		require.NoError(t, r.Delete(ctx, sb))
	}
	result := reconcileCapacityInteraction(t, r, pool)
	require.Equal(t, 5*time.Second, result.RequeueAfter)
	require.Len(t, capacityInteractionStandby(t, r, pool), 1)
	now = now.Add(2 * time.Second)
	result = reconcileCapacityInteraction(t, r, pool)
	require.Equal(t, 3*time.Second, result.RequeueAfter)
	require.Len(t, capacityInteractionStandby(t, r, pool), 1)

	now = now.Add(3 * time.Second)
	reconcileCapacityInteraction(t, r, pool)
	require.Len(t, capacityInteractionStandby(t, r, pool), 2, "the expired hold must not bypass the refill rate")
	syncPoolExpectations(r, pool)
	result = reconcileCapacityInteraction(t, r, pool)
	require.Equal(t, time.Second, result.RequeueAfter)
	require.Len(t, capacityInteractionStandby(t, r, pool), 2, "an empty bucket must not issue another create")

	// A concurrent same-source cold instance reduces the remaining refill target.
	used3 := capacityInteractionSandbox(pool, template, "-used3")
	used3.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind, Name: "consumer", UID: "consumer-uid", Controller: new(true)}}
	used3.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeCold
	require.NoError(t, r.Create(ctx, used3))
	now = now.Add(time.Second)
	reconcileCapacityInteraction(t, r, pool)
	require.Len(t, capacityInteractionStandby(t, r, pool), 3)
	syncPoolExpectations(r, pool)
	now = now.Add(2 * time.Second)
	reconcileCapacityInteraction(t, r, pool)
	require.Len(t, capacityInteractionStandby(t, r, pool), 3, "accrued tokens cannot refill beyond the reduced target")
	require.Equal(t, new(int32(3)), pool.Status.OccupiedReplicas)
	require.Equal(t, new(int32(3)), pool.Status.DesiredStandbyReplicas)
}

func TestClaimAwarePoolTerminatingStandbyRetainsSlot(t *testing.T) {
	ctx := context.Background()
	pool := newReplenishTestPool(5)
	pool.Spec.ReplenishmentCapacity = new(int32(3))
	template := createTemplate(pool.Namespace)
	used := capacityInteractionSandbox(pool, template, "-used")
	used.OwnerReferences = nil
	active := capacityInteractionSandbox(pool, template, "-active")
	terminating := capacityInteractionSandbox(pool, template, "-terminating")
	terminating.Finalizers = []string{"test.agents.x-k8s.io/drain"}
	scheme := newTestScheme()
	r := &SandboxWarmPoolReconciler{Client: newFakeClient(scheme, pool, template, used, active, terminating), Scheme: scheme, MaxBatchSize: 5}
	require.NoError(t, r.Delete(ctx, terminating))
	reconcileCapacityInteraction(t, r, pool)
	require.Len(t, capacityInteractionStandby(t, r, pool), 2, "a draining standby still occupies one of the two target slots")
	require.Equal(t, int32(1), pool.Status.Replicas, "terminating standby is excluded from active status")
	require.Equal(t, new(int32(1)), pool.Status.OccupiedReplicas, "pool-owned draining inventory is not occupied capacity")
	require.Equal(t, new(int32(2)), pool.Status.DesiredStandbyReplicas)

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(terminating), terminating))
	terminating.Finalizers = nil
	require.NoError(t, r.Update(ctx, terminating))
	reconcileCapacityInteraction(t, r, pool)
	require.Len(t, capacityInteractionStandby(t, r, pool), 2, "exactly one replacement may be created after the draining object disappears")
	syncPoolExpectations(r, pool)
	reconcileCapacityInteraction(t, r, pool)
	require.Equal(t, int32(2), pool.Status.Replicas)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(used), used), "the occupied instance must remain untouched")
}

func TestClaimAwarePoolTemplateReplacement(t *testing.T) {
	for _, strategy := range []extensionsv1beta1.SandboxWarmPoolUpdateStrategyType{
		extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType,
		extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType,
	} {
		t.Run(string(strategy), func(t *testing.T) {
			ctx := context.Background()
			pool := newReplenishTestPool(4)
			pool.Spec.ReplenishmentCapacity = new(int32(3))
			pool.Spec.UpdateStrategy = &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: strategy}
			template := createTemplate(pool.Namespace)
			template.Spec.PodTemplate.Spec.Containers[0].Image = "image-v1"
			used := capacityInteractionSandbox(pool, template, "-used")
			used.OwnerReferences = nil
			originalUsed := used.DeepCopy()
			scheme := newTestScheme()
			r := &SandboxWarmPoolReconciler{Client: newFakeClient(scheme, pool, template, used), Scheme: scheme, MaxBatchSize: 4}
			reconcileCapacityInteraction(t, r, pool)
			syncPoolExpectations(r, pool)
			reconcileCapacityInteraction(t, r, pool)
			require.Len(t, capacityInteractionStandby(t, r, pool), 2)
			require.Equal(t, new(int32(2)), pool.Status.DesiredStandbyReplicas)

			template.Spec.PodTemplate.Spec.Containers[0].Image = "image-v2"
			require.NoError(t, r.Update(ctx, template))
			reconcileCapacityInteraction(t, r, pool)
			if strategy == extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType {
				require.Empty(t, capacityInteractionStandby(t, r, pool), "Recreate deletes stale standby before replacing it")
				syncPoolExpectations(r, pool)
				reconcileCapacityInteraction(t, r, pool)
				stock := capacityInteractionStandby(t, r, pool)
				require.Len(t, stock, 2, "replacement is limited by effective standby target, not spec.replicas")
				for _, sb := range stock {
					require.Equal(t, "image-v2", sb.Spec.PodTemplate.Spec.Containers[0].Image)
				}
			} else {
				stock := capacityInteractionStandby(t, r, pool)
				require.Len(t, stock, 2)
				for _, sb := range stock {
					require.Equal(t, "image-v1", sb.Spec.PodTemplate.Spec.Containers[0].Image, "OnReplenish keeps existing standby")
				}
				require.NoError(t, r.Delete(ctx, &stock[0]))
				reconcileCapacityInteraction(t, r, pool)
				stock = capacityInteractionStandby(t, r, pool)
				require.Len(t, stock, 2)
				images := []string{stock[0].Spec.PodTemplate.Spec.Containers[0].Image, stock[1].Spec.PodTemplate.Spec.Containers[0].Image}
				require.ElementsMatch(t, []string{"image-v1", "image-v2"}, images)
			}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(used), used))
			require.Equal(t, originalUsed.Spec, used.Spec, "template rollout cannot replace or update occupied instances")
			require.Equal(t, originalUsed.OwnerReferences, used.OwnerReferences)
			syncPoolExpectations(r, pool)
			reconcileCapacityInteraction(t, r, pool)
			require.Equal(t, new(int32(1)), pool.Status.OccupiedReplicas)
			require.Equal(t, new(int32(2)), pool.Status.DesiredStandbyReplicas)
			require.Equal(t, int32(2), pool.Status.Replicas)
		})
	}
}

func TestClaimAwarePoolReadinessGraceAndSchedulingHolds(t *testing.T) {
	zeroGraceJitter(t)
	for _, reason := range []string{corev1.PodReasonUnschedulable, corev1.PodReasonSchedulingGated} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().Truncate(time.Second)
			pool := newReplenishTestPool(4)
			pool.Spec.ReplenishmentCapacity = new(int32(3))
			template := createTemplate(pool.Namespace)
			used := capacityInteractionSandbox(pool, template, "-used")
			used.OwnerReferences = nil
			used2 := capacityInteractionSandbox(pool, template, "-used2")
			used2.OwnerReferences = nil
			stock := capacityInteractionSandbox(pool, template, "-pending")
			stock.CreationTimestamp = metav1.NewTime(now.Add(-15 * time.Second))
			stock.Status.Conditions = []metav1.Condition{
				{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse},
				{Type: string(sandboxv1beta1.SandboxConditionPodScheduled), Status: metav1.ConditionFalse, Reason: reason},
			}
			scheme := newTestScheme()
			r := &SandboxWarmPoolReconciler{
				Client: newFakeClient(scheme, pool, template, used, used2, stock), Scheme: scheme, MaxBatchSize: 4,
				ReadinessGracePeriod: 20 * time.Second, UnschedulableRecheckInterval: 10 * time.Second,
				now: func() time.Time { return now },
			}
			result := reconcileCapacityInteraction(t, r, pool)
			require.Equal(t, 5*time.Second+graceRequeueSlack, result.RequeueAfter, "the young standby must schedule its post-grace evaluation")
			require.Len(t, capacityInteractionStandby(t, r, pool), 1, "non-Ready standby occupies the effective target slot")
			require.Equal(t, int32(1), pool.Status.Replicas)
			require.Zero(t, pool.Status.ReadyReplicas)
			require.Equal(t, new(int32(1)), pool.Status.DesiredStandbyReplicas)

			now = now.Add(7 * time.Second)
			result = reconcileCapacityInteraction(t, r, pool)
			require.Equal(t, 10*time.Second, result.RequeueAfter)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(stock), stock), "a standby waiting for scheduling must survive the grace deadline")
			require.Len(t, capacityInteractionStandby(t, r, pool), 1, "held standby cannot trigger refill to spec.replicas")

			// Scheduling starts a fresh grace window for pulling images and starting containers.
			stock.Status.Conditions[1].Status = metav1.ConditionTrue
			stock.Status.Conditions[1].Reason = sandboxv1beta1.SandboxReasonPodScheduled
			stock.Status.Conditions[1].LastTransitionTime = metav1.NewTime(now)
			require.NoError(t, r.Update(ctx, stock))
			result = reconcileCapacityInteraction(t, r, pool)
			require.Equal(t, 20*time.Second+graceRequeueSlack, result.RequeueAfter)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(stock), stock))
			require.Len(t, capacityInteractionStandby(t, r, pool), 1)

			now = now.Add(20*time.Second + graceRequeueSlack)
			reconcileCapacityInteraction(t, r, pool)
			require.Empty(t, capacityInteractionStandby(t, r, pool), "stuck deletion must wait for observation before replacement")
			syncPoolExpectations(r, pool)
			reconcileCapacityInteraction(t, r, pool)
			require.Len(t, capacityInteractionStandby(t, r, pool), 1, "stuck replacement must still stop at B-C, not R")
			require.Equal(t, new(int32(2)), pool.Status.OccupiedReplicas)
			require.Equal(t, new(int32(1)), pool.Status.DesiredStandbyReplicas)
		})
	}
}

func TestClaimAwarePoolMissingTemplateUsesEffectiveTarget(t *testing.T) {
	pool := newReplenishTestPool(4)
	pool.Spec.ReplenishmentCapacity = new(int32(1))
	template := createTemplate(pool.Namespace)
	used := capacityInteractionSandbox(pool, template, "-used")
	used.OwnerReferences = nil
	scheme := newTestScheme()
	recorder := events.NewFakeRecorder(10)
	r := &SandboxWarmPoolReconciler{Client: newFakeClient(scheme, pool, used), Scheme: scheme, Recorder: recorder}

	result := reconcileCapacityInteraction(t, r, pool)
	require.Equal(t, new(int32(0)), pool.Status.DesiredStandbyReplicas)
	require.Equal(t, expectationsPendingRequeueDelay, result.RequeueAfter)
	require.Empty(t, recorder.Events, "a zero standby target does not need a template")
	require.NoError(t, r.Delete(context.Background(), used))
	reconcileCapacityInteraction(t, r, pool)
	require.Equal(t, new(int32(1)), pool.Status.DesiredStandbyReplicas)
	select {
	case event := <-recorder.Events:
		require.Contains(t, event, reasonSandboxTemplateError, "the missing template must warn once standby capacity is needed")
	default:
		t.Fatal("expected a missing-template warning after the occupied instance disappears")
	}
}
