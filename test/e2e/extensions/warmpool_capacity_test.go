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

package extensions

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	capacitySourceName = "agents.x-k8s.io/source-warmpool-name"
	capacitySourceUID  = "agents.x-k8s.io/source-warmpool-uid"
	capacityTimeout    = 2 * time.Minute
)

var (
	capacityPoolResource    = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"}
	capacitySandboxResource = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
)

func newCapacityFixture(t *testing.T, capacity *int64, replicas int64) (*framework.TestContext, *unstructured.Unstructured) {
	t.Helper()
	tc := framework.NewTestContext(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("warmpool-capacity-%d", time.Now().UnixNano())}}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))
	template := newWarmPoolTemplate(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))
	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": extensionsv1beta1.GroupVersion.String(),
		"kind":       "SandboxWarmPool",
		"metadata":   map[string]any{"name": "test-pool", "namespace": ns.Name},
		"spec": map[string]any{
			"replicas":           replicas,
			"sandboxTemplateRef": map[string]any{"name": template.Name},
		},
	}}
	if capacity != nil {
		require.NoError(t, unstructured.SetNestedField(pool.Object, *capacity, "spec", "replenishmentCapacity"))
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), pool))
	return tc, pool
}

func capacityPool(t *testing.T, tc *framework.TestContext, pool *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	latest, err := tc.DynamicClient().Resource(capacityPoolResource).Namespace(pool.GetNamespace()).Get(t.Context(), pool.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	return latest
}

func waitForCapacityState(t *testing.T, tc *framework.TestContext, pool *unstructured.Unstructured, occupied, target, standby int64) []sandboxv1beta1.Sandbox {
	t.Helper()
	var inventory []sandboxv1beta1.Sandbox
	require.Eventually(t, func() bool {
		latest, err := tc.DynamicClient().Resource(capacityPoolResource).Namespace(pool.GetNamespace()).Get(t.Context(), pool.GetName(), metav1.GetOptions{})
		if err != nil {
			return false
		}
		for field, want := range map[string]int64{"occupiedReplicas": occupied, "desiredStandbyReplicas": target, "replicas": standby, "readyReplicas": standby} {
			got, found, err := unstructured.NestedInt64(latest.Object, "status", field)
			optionalZero := want == 0 && (field == "replicas" || field == "readyReplicas")
			if err != nil || (!found && !optionalZero) || got != want {
				return false
			}
		}
		observed, _, _ := unstructured.NestedInt64(latest.Object, "status", "observedGeneration")
		if observed != latest.GetGeneration() {
			return false
		}
		list := &sandboxv1beta1.SandboxList{}
		if tc.List(t.Context(), list, client.InNamespace(pool.GetNamespace())) != nil {
			return false
		}
		inventory = nil
		for _, sb := range list.Items {
			if metav1.IsControlledBy(&sb, pool) {
				if !sb.DeletionTimestamp.IsZero() || !isSandboxReady(&sb) {
					return false
				}
				inventory = append(inventory, sb)
			}
		}
		return int64(len(inventory)) == standby
	}, capacityTimeout, 500*time.Millisecond, "pool must converge to occupied=%d, target=%d, Ready standby=%d", occupied, target, standby)
	t.Logf("pool %s: occupied=%d target=%d standby=%d Ready=%d", pool.GetName(), occupied, target, standby, standby)
	return inventory
}

func claimCapacitySandbox(t *testing.T, tc *framework.TestContext, pool *unstructured.Unstructured, name string, cold bool) (*extensionsv1beta1.SandboxClaim, *sandboxv1beta1.Sandbox) {
	t.Helper()
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pool.GetNamespace()},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.GetName()}},
	}
	if cold {
		claim.Spec.Env = []extensionsv1beta1.EnvVar{{Name: "CONSUMER", Value: name}}
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))
	waitForClaimReady(t, tc, claim)
	sb := &sandboxv1beta1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Namespace: claim.Namespace, Name: claim.Status.SandboxStatus.Name}, sb))
	require.True(t, metav1.IsControlledBy(sb, claim))
	require.Equal(t, pool.GetName(), sb.Annotations[capacitySourceName])
	require.Equal(t, string(pool.GetUID()), sb.Annotations[capacitySourceUID])
	launchType := sandboxv1beta1.SandboxLaunchTypeWarm
	if cold {
		launchType = sandboxv1beta1.SandboxLaunchTypeCold
	}
	require.Equal(t, launchType, sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])
	return claim, sb
}

func updateCapacityPool(t *testing.T, tc *framework.TestContext, pool *unstructured.Unstructured, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := capacityPool(t, tc, pool)
		mutate(latest)
		_, err := tc.DynamicClient().Resource(capacityPoolResource).Namespace(pool.GetNamespace()).Update(t.Context(), latest, metav1.UpdateOptions{})
		return err
	}))
}

func restartCapacityController(t *testing.T, tc *framework.TestContext) {
	t.Helper()
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}}
	oldPods := &corev1.PodList{}
	require.NoError(t, tc.List(t.Context(), oldPods, client.InNamespace(deployment.Namespace), client.MatchingLabels{"app": "agent-sandbox-controller"}))
	oldUIDs := map[types.UID]bool{}
	for _, pod := range oldPods.Items {
		oldUIDs[pod.UID] = true
	}
	framework.MustUpdateObject(tc.ClusterClient, deployment, func(obj *appsv1.Deployment) {
		if obj.Spec.Template.Annotations == nil {
			obj.Spec.Template.Annotations = map[string]string{}
		}
		obj.Spec.Template.Annotations["e2e.agent-sandbox.dev/restart"] = time.Now().UTC().Format(time.RFC3339Nano)
	})
	require.Eventually(t, func() bool {
		if tc.Get(t.Context(), client.ObjectKeyFromObject(deployment), deployment) != nil || deployment.Status.ObservedGeneration != deployment.Generation || deployment.Status.UpdatedReplicas != *deployment.Spec.Replicas || deployment.Status.AvailableReplicas != *deployment.Spec.Replicas {
			return false
		}
		pods := &corev1.PodList{}
		if tc.List(t.Context(), pods, client.InNamespace(deployment.Namespace), client.MatchingLabels{"app": "agent-sandbox-controller"}) != nil {
			return false
		}
		var ready int32
		for _, pod := range pods.Items {
			if oldUIDs[pod.UID] {
				return false
			}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready++
				}
			}
		}
		return ready == *deployment.Spec.Replicas
	}, capacityTimeout, 500*time.Millisecond, "controller must restart with a new Ready Pod")
}

func TestWarmPoolStandbyCapacityLifecycle(t *testing.T) {
	tc, pool := newCapacityFixture(t, new(int64(3)), 3)
	initial := waitForCapacityState(t, tc, pool, 0, 3, 3)
	oldVersions := map[string]sandboxv1beta1.Sandbox{}
	for _, sb := range initial {
		oldVersions[sb.Name] = sb
	}
	first, firstSandbox := claimCapacitySandbox(t, tc, pool, "consumer-a-1", false)
	_, secondSandbox := claimCapacitySandbox(t, tc, pool, "consumer-a-2", false)
	_, thirdSandbox := claimCapacitySandbox(t, tc, pool, "consumer-b-1", false)
	require.NotEqual(t, secondSandbox.UID, thirdSandbox.UID)
	waitForCapacityState(t, tc, pool, 3, 0, 0)
	latest := capacityPool(t, tc, pool)
	require.Equal(t, pool.GetGeneration(), latest.GetGeneration(), "consumers must not patch pool spec")

	// A stale inventory observation must not be able to delete an adopted Sandbox.
	old := oldVersions[firstSandbox.Name]
	require.NotEmpty(t, old.UID)
	err := tc.DynamicClient().Resource(capacitySandboxResource).Namespace(first.Namespace).Delete(t.Context(), old.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &old.UID, ResourceVersion: &old.ResourceVersion},
	})
	require.True(t, k8serrors.IsConflict(err), "old standby DELETE must fail after adoption: %v", err)
	require.NoError(t, tc.Get(t.Context(), client.ObjectKeyFromObject(firstSandbox), firstSandbox))
	require.True(t, metav1.IsControlledBy(firstSandbox, first))

	restartCapacityController(t, tc)
	waitForCapacityState(t, tc, pool, 3, 0, 0)

	// Retain stops this consumer from replacing its deleted Sandbox. The test
	// finalizer keeps the CR visible while dependent Pods may already be gone.
	framework.MustUpdateObject(tc.ClusterClient, firstSandbox, func(obj *sandboxv1beta1.Sandbox) {
		obj.Finalizers = append(obj.Finalizers, holdDeleteFinalizer)
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), framework.DefaultTimeout)
		defer cancel()
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			obj := &sandboxv1beta1.Sandbox{}
			if err := tc.Get(ctx, client.ObjectKeyFromObject(firstSandbox), obj); err != nil {
				if k8serrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			obj.Finalizers = slices.DeleteFunc(obj.Finalizers, func(value string) bool { return value == holdDeleteFinalizer })
			return tc.Update(ctx, obj)
		}); err != nil {
			t.Errorf("remove test finalizer: %v", err)
		}
	})
	framework.MustUpdateObject(tc.ClusterClient, first, func(obj *extensionsv1beta1.SandboxClaim) {
		now := metav1.NewTime(time.Now().Add(-time.Second))
		obj.Spec.Lifecycle = &extensionsv1beta1.Lifecycle{ShutdownTime: &now, ShutdownPolicy: extensionsv1beta1.ShutdownPolicyRetain}
	})
	require.Eventually(t, func() bool {
		return tc.Get(t.Context(), client.ObjectKeyFromObject(firstSandbox), firstSandbox) == nil && !firstSandbox.DeletionTimestamp.IsZero()
	}, capacityTimeout, 500*time.Millisecond)
	waitForCapacityState(t, tc, pool, 3, 0, 0)
	framework.MustUpdateObject(tc.ClusterClient, firstSandbox, func(obj *sandboxv1beta1.Sandbox) {
		obj.Finalizers = slices.DeleteFunc(obj.Finalizers, func(value string) bool { return value == holdDeleteFinalizer })
	})
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), firstSandbox))
	waitForCapacityState(t, tc, pool, 2, 1, 1)
	require.NoError(t, tc.Get(t.Context(), client.ObjectKeyFromObject(first), first), "empty Retain Claim must still exist")
}

func TestWarmPoolStandbyCapacityColdAndResize(t *testing.T) {
	tc, pool := newCapacityFixture(t, new(int64(1)), 3)
	waitForCapacityState(t, tc, pool, 0, 1, 1)
	_, first := claimCapacitySandbox(t, tc, pool, "cold-a", true)
	waitForCapacityState(t, tc, pool, 1, 0, 0)
	_, second := claimCapacitySandbox(t, tc, pool, "cold-b", true)
	waitForCapacityState(t, tc, pool, 2, 0, 0)
	for _, sb := range []*sandboxv1beta1.Sandbox{first, second} {
		require.Empty(t, sb.Annotations["agents.x-k8s.io/warmpool-create-token"], "cold creation must not acknowledge a warm POST")
	}
	updateCapacityPool(t, tc, pool, func(obj *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(obj.Object, int64(3), "spec", "replenishmentCapacity"))
	})
	waitForCapacityState(t, tc, pool, 2, 1, 1)
	updateCapacityPool(t, tc, pool, func(obj *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(obj.Object, int64(0), "spec", "replenishmentCapacity"))
	})
	waitForCapacityState(t, tc, pool, 2, 0, 0)
	updateCapacityPool(t, tc, pool, func(obj *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(obj.Object, int64(3), "spec", "replenishmentCapacity"))
	})
	waitForCapacityState(t, tc, pool, 2, 1, 1)
	for _, sb := range []*sandboxv1beta1.Sandbox{first, second} {
		uid := sb.UID
		require.NoError(t, tc.Get(t.Context(), client.ObjectKeyFromObject(sb), sb))
		require.Equal(t, uid, sb.UID)
		require.True(t, sb.DeletionTimestamp.IsZero(), "resizing must not delete occupied instances")
	}
}

func TestWarmPoolStandbyCapacityValidation(t *testing.T) {
	tc, pool := newCapacityFixture(t, new(int64(0)), 0)
	waitForCapacityState(t, tc, pool, 0, 0, 0)
	resource := tc.DynamicClient().Resource(capacityPoolResource).Namespace(pool.GetNamespace())
	for _, value := range []int64{-1, -2147483648} {
		invalid := capacityPool(t, tc, pool)
		require.NoError(t, unstructured.SetNestedField(invalid.Object, value, "spec", "replenishmentCapacity"))
		_, err := resource.Update(t.Context(), invalid, metav1.UpdateOptions{})
		require.True(t, k8serrors.IsInvalid(err), "negative capacity UPDATE must fail validation: %v", err)
		invalid.SetName("invalid-capacity")
		invalid.SetUID("")
		invalid.SetResourceVersion("")
		_, err = resource.Create(t.Context(), invalid, metav1.CreateOptions{})
		require.True(t, k8serrors.IsInvalid(err), "negative capacity CREATE must fail validation: %v", err)
	}
	for _, null := range []bool{false, true} {
		invalid := capacityPool(t, tc, pool)
		if null {
			require.NoError(t, unstructured.SetNestedField(invalid.Object, nil, "spec", "replenishmentCapacity"))
		} else {
			unstructured.RemoveNestedField(invalid.Object, "spec", "replenishmentCapacity")
		}
		_, err := resource.Update(t.Context(), invalid, metav1.UpdateOptions{})
		require.True(t, k8serrors.IsInvalid(err), "capacity mode removal (null=%v) must fail CEL: %v", null, err)
	}
	fixed := capacityPool(t, tc, pool)
	fixed.SetName("fixed-pool")
	fixed.SetUID("")
	fixed.SetResourceVersion("")
	fixed.SetGeneration(0)
	delete(fixed.Object, "status")
	unstructured.RemoveNestedField(fixed.Object, "spec", "replenishmentCapacity")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), fixed))
	fixed = capacityPool(t, tc, fixed)
	require.NoError(t, unstructured.SetNestedField(fixed.Object, int64(0), "spec", "replenishmentCapacity"))
	_, err := resource.Update(t.Context(), fixed, metav1.UpdateOptions{})
	require.True(t, k8serrors.IsInvalid(err), "enabling capacity on an existing fixed pool must fail CEL: %v", err)
}

func TestWarmPoolStandbyCapacityDefault(t *testing.T) {
	tc, pool := newCapacityFixture(t, nil, 3)
	waitFixed := func() {
		t.Helper()
		require.Eventually(t, func() bool {
			latest, err := tc.DynamicClient().Resource(capacityPoolResource).Namespace(pool.GetNamespace()).Get(t.Context(), pool.GetName(), metav1.GetOptions{})
			if err != nil {
				return false
			}
			for _, field := range []string{"occupiedReplicas", "desiredStandbyReplicas"} {
				_, present, err := unstructured.NestedInt64(latest.Object, "status", field)
				if err != nil || present {
					return false
				}
			}
			ready, _, _ := unstructured.NestedInt64(latest.Object, "status", "readyReplicas")
			list := &sandboxv1beta1.SandboxList{}
			if tc.List(t.Context(), list, client.InNamespace(pool.GetNamespace())) != nil {
				return false
			}
			var standby int64
			for _, sb := range list.Items {
				if metav1.IsControlledBy(&sb, pool) && sb.DeletionTimestamp.IsZero() && isSandboxReady(&sb) {
					standby++
				}
			}
			return ready == 3 && standby == 3
		}, capacityTimeout, 500*time.Millisecond, "fixed mode must retain three Ready standby instances and omit new status counters")
	}
	waitFixed()
	claimCapacitySandbox(t, tc, pool, "fixed-consumer", false)
	waitFixed()
}

func TestWarmPoolStandbyCapacitySourceRecovery(t *testing.T) {
	tc, pool := newCapacityFixture(t, new(int64(3)), 3)
	waitForCapacityState(t, tc, pool, 0, 3, 3)
	claim, occupied := claimCapacitySandbox(t, tc, pool, "source-consumer", false)
	inventory := waitForCapacityState(t, tc, pool, 1, 2, 2)
	framework.MustUpdateObject(tc.ClusterClient, occupied, func(obj *sandboxv1beta1.Sandbox) { delete(obj.Annotations, capacitySourceUID) })
	waitUnknown := func() {
		t.Helper()
		require.Eventually(t, func() bool {
			latest, err := tc.DynamicClient().Resource(capacityPoolResource).Namespace(pool.GetNamespace()).Get(t.Context(), pool.GetName(), metav1.GetOptions{})
			if err != nil {
				return false
			}
			for _, field := range []string{"occupiedReplicas", "desiredStandbyReplicas"} {
				_, found, _ := unstructured.NestedFieldNoCopy(latest.Object, "status", field)
				if found {
					return false
				}
			}
			return true
		}, capacityTimeout, 500*time.Millisecond, "incomplete provenance must clear both counters")
	}
	waitUnknown()
	events := &corev1.EventList{}
	require.Eventually(t, func() bool {
		if tc.List(t.Context(), events, client.InNamespace(pool.GetNamespace())) != nil {
			return false
		}
		for _, event := range events.Items {
			if event.InvolvedObject.UID == pool.GetUID() && event.Type == corev1.EventTypeWarning && event.Reason == "WarmPoolAccountingIncomplete" {
				return true
			}
		}
		return false
	}, capacityTimeout, 500*time.Millisecond, "unknown accounting must emit a Warning")
	updateCapacityPool(t, tc, pool, func(obj *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(obj.Object, int64(0), "spec", "replenishmentCapacity"))
	})
	waitUnknown()
	require.Never(t, func() bool {
		for _, original := range inventory {
			current := &sandboxv1beta1.Sandbox{}
			if tc.Get(t.Context(), client.ObjectKeyFromObject(&original), current) != nil || current.UID != original.UID || !current.DeletionTimestamp.IsZero() {
				return true
			}
		}
		return false
	}, 3*time.Second, 250*time.Millisecond, "unknown accounting must pause shrinking even when capacity is zero")
	framework.MustUpdateObject(tc.ClusterClient, occupied, func(obj *sandboxv1beta1.Sandbox) {
		obj.Annotations[capacitySourceUID] = string(pool.GetUID())
	})
	waitForCapacityState(t, tc, pool, 1, 0, 0)
	updateCapacityPool(t, tc, pool, func(obj *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(obj.Object, int64(3), "spec", "replenishmentCapacity"))
	})
	waitForCapacityState(t, tc, pool, 1, 2, 2)

	// Removing the consumer with orphan propagation does not make its used
	// instance clean standby. Persistent source identity still charges capacity.
	claims := schema.GroupVersionResource{Group: extensionsv1beta1.GroupVersion.Group, Version: extensionsv1beta1.GroupVersion.Version, Resource: "sandboxclaims"}
	require.NoError(t, tc.DynamicClient().Resource(claims).Namespace(claim.Namespace).Delete(t.Context(), claim.Name, metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationOrphan)}))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), claim))
	require.Eventually(t, func() bool {
		return tc.Get(t.Context(), client.ObjectKeyFromObject(occupied), occupied) == nil && metav1.GetControllerOf(occupied) == nil
	}, capacityTimeout, 500*time.Millisecond)
	waitForCapacityState(t, tc, pool, 1, 2, 2)
	updateCapacityPool(t, tc, pool, func(obj *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(obj.Object, int64(0), "spec", "replenishmentCapacity"))
	})
	waitForCapacityState(t, tc, pool, 1, 0, 0)
	require.NoError(t, tc.Get(t.Context(), client.ObjectKeyFromObject(occupied), occupied))
	require.Nil(t, metav1.GetControllerOf(occupied))
	require.True(t, occupied.DeletionTimestamp.IsZero(), "used orphan must neither be adopted nor deleted")
}

func TestWarmPoolStandbyCapacityPoolRecreation(t *testing.T) {
	tc, pool := newCapacityFixture(t, new(int64(1)), 1)
	waitForCapacityState(t, tc, pool, 0, 1, 1)
	claim, occupied := claimCapacitySandbox(t, tc, pool, "old-pool-consumer", false)
	waitForCapacityState(t, tc, pool, 1, 0, 0)
	oldUID := pool.GetUID()
	require.NoError(t, tc.Delete(t.Context(), pool))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pool))
	replacement := pool.DeepCopy()
	replacement.SetUID("")
	replacement.SetResourceVersion("")
	replacement.SetGeneration(0)
	replacement.SetCreationTimestamp(metav1.Time{})
	replacement.SetManagedFields(nil)
	delete(replacement.Object, "status")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), replacement))
	require.NotEqual(t, oldUID, replacement.GetUID())
	waitForCapacityState(t, tc, replacement, 0, 1, 1)
	require.NoError(t, tc.Get(t.Context(), client.ObjectKeyFromObject(occupied), occupied))
	require.Equal(t, string(oldUID), occupied.Annotations[capacitySourceUID], "new pool must not rewrite the old source UID")
	require.True(t, metav1.IsControlledBy(occupied, claim))
	claimCapacitySandbox(t, tc, replacement, "new-pool-consumer", false)
	waitForCapacityState(t, tc, replacement, 1, 0, 0)
}

func TestWarmPoolStandbyCapacitySandboxUIDPrecondition(t *testing.T) {
	tc, pool := newCapacityFixture(t, new(int64(0)), 0)
	old := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "recreated-instance", Namespace: pool.GetNamespace()},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
			PodTemplate: newWarmPoolTemplate(pool.GetNamespace()).Spec.PodTemplate,
		}},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), old))
	oldUID := old.UID
	require.NoError(t, tc.Delete(t.Context(), old))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), old))
	replacement := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Namespace: old.Namespace}, Spec: old.Spec}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), replacement))
	require.NotEqual(t, oldUID, replacement.UID)
	err := tc.DynamicClient().Resource(capacitySandboxResource).Namespace(old.Namespace).Delete(t.Context(), old.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &oldUID}})
	require.True(t, k8serrors.IsConflict(err), "old UID must not delete a same-name replacement: %v", err)
	require.NoError(t, tc.Get(t.Context(), client.ObjectKeyFromObject(replacement), replacement))
	require.True(t, replacement.DeletionTimestamp.IsZero())
}
