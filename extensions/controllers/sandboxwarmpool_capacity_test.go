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
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestClaimAwarePoolDoesNotReplenishOccupiedCapacity(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid", Generation: 1},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas: new(int32(3)), ReplenishmentCapacity: new(int32(3)),
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	objects := []runtime.Object{pool, template}
	for i := range 3 {
		name := fmt.Sprintf("claimed-%d", i)
		objects = append(objects, &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name),
			Annotations:     map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxClaim", Name: name, UID: types.UID(name + "-claim"), Controller: new(true)}},
		}})
	}
	c := newFakeClient(scheme, objects...)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: 3}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(ctx, list))
	require.Len(t, list.Items, 3, "occupied capacity must not be duplicated as standby")
	require.NoError(t, c.Get(ctx, request.NamespacedName, pool))
	require.Equal(t, new(int32(3)), pool.Status.OccupiedReplicas)
	require.Equal(t, new(int32(0)), pool.Status.DesiredStandbyReplicas)
	require.Zero(t, pool.Status.Replicas)
}

func TestClaimAwarePoolLaunchLabelFailureKeepsObservedCapacity(t *testing.T) {
	ctx := context.Background()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), ReplenishmentCapacity: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}}}
	sb := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-stock")
	sb.UID = "stock-uid"
	sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxWarmPoolKind, Name: pool.Name, UID: pool.UID, Controller: new(true)}}
	sb.Annotations = map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)}
	delete(sb.Labels, sandboxv1beta1.SandboxLaunchTypeLabel)
	c := newFakeClient(newTestScheme(), pool, template, sb)
	r := &SandboxWarmPoolReconciler{Client: interceptor.NewClient(c, interceptor.Funcs{Update: func(ctx context.Context, delegate client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
			return k8serrors.NewConflict(schema.GroupResource{Group: sandboxv1beta1.GroupVersion.Group, Resource: "sandboxes"}, obj.GetName(), fmt.Errorf("ownership changed"))
		}
		return delegate.Update(ctx, obj, opts...)
	}}), Scheme: newTestScheme(), MaxBatchSize: 1}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.Error(t, err)
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(ctx, list))
	require.Len(t, list.Items, 1, "a failed metadata write does not release the observed standby slot")
}

func TestClaimAwarePoolSourceSafetyBeforeInventoryWrites(t *testing.T) {
	for _, scenario := range []string{"conflicting source", "repair conflict", "crossed auxiliary snapshot", "claimed missing name"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			template := createTemplate("default")
			pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(0)), ReplenishmentCapacity: new(int32(0)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}, UpdateStrategy: &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType}}}
			sb := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-stock")
			sb.UID = "stock-uid"
			sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxWarmPoolKind, Name: pool.Name, UID: pool.UID, Controller: new(true)}}
			sb.Annotations = map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)}
			funcs := interceptor.Funcs{}
			switch scenario {
			case "conflicting source":
				sb.Annotations[sourceWarmPoolUIDAnnotation] = "other-pool-uid"
				sb.Spec.PodTemplate.Spec.Containers[0].Image = "stale-image"
			case "repair conflict":
				delete(sb.Annotations, sourceWarmPoolUIDAnnotation)
				funcs.Patch = func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return k8serrors.NewConflict(schema.GroupResource{Resource: "sandboxes"}, sb.Name, fmt.Errorf("adopted during repair"))
				}
			case "crossed auxiliary snapshot":
				reads := 0
				funcs.List = func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := delegate.List(ctx, list, opts...); err != nil {
						return err
					}
					if sandboxes, ok := list.(*sandboxv1beta1.SandboxList); ok {
						reads++
						if reads > 1 {
							for i := range sandboxes.Items {
								sandboxes.Items[i].ResourceVersion = "newer-observation"
							}
						}
					}
					return nil
				}
			case "claimed missing name":
				delete(sb.Annotations, sourceWarmPoolNameAnnotation)
				sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind, Name: "consumer", UID: "claim-uid", Controller: new(true)}}
			}
			originalOwner := sb.OwnerReferences[0]
			c := newFakeClient(newTestScheme(), pool, template, sb)
			r := &SandboxWarmPoolReconciler{Client: interceptor.NewClient(c, funcs), Scheme: newTestScheme(), MaxBatchSize: 1}
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
			if scenario == "conflicting source" || scenario == "repair conflict" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Positive(t, result.RequeueAfter)
			}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pool), pool))
			require.Nil(t, pool.Status.OccupiedReplicas)
			require.Nil(t, pool.Status.DesiredStandbyReplicas)
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sb), sb), "unknown accounting must suspend every inventory deletion path")
			require.Equal(t, originalOwner, sb.OwnerReferences[0])
			if scenario == "claimed missing name" {
				require.Equal(t, pool.Name, sb.Annotations[sourceWarmPoolNameAnnotation])
				_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
				require.NoError(t, err)
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pool), pool))
				require.Equal(t, new(int32(1)), pool.Status.OccupiedReplicas)
			}
		})
	}
}

func TestClaimAwarePoolCapacityTargets(t *testing.T) {
	for _, tc := range []struct {
		name               string
		replicas, capacity *int32
		occupied, target   int32
	}{
		{"legacy default", nil, nil, 3, 1},
		{"legacy zero", new(int32(0)), nil, 3, 0},
		{"opt-in default", nil, new(int32(3)), 1, 1},
		{"zero capacity", new(int32(3)), new(int32(0)), 0, 0},
		{"zero replicas", new(int32(0)), new(int32(3)), 1, 0},
		{"capacity below replicas", new(int32(3)), new(int32(2)), 1, 1},
		{"capacity above replicas", new(int32(2)), new(int32(5)), 1, 2},
		{"occupancy exceeds capacity", new(int32(3)), new(int32(2)), 4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := createTemplate("default")
			pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid", Generation: 1}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: tc.replicas, ReplenishmentCapacity: tc.capacity, TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}}}
			objects := []runtime.Object{pool, template}
			for i := range tc.occupied {
				objects = append(objects, &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("used-%d", i), Namespace: pool.Namespace, UID: types.UID(fmt.Sprintf("used-%d", i)), Annotations: map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)}}})
			}
			c := newFakeClient(newTestScheme(), objects...)
			r := &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme(), MaxBatchSize: 3}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			list := &sandboxv1beta1.SandboxList{}
			require.NoError(t, c.List(context.Background(), list))
			require.Len(t, list.Items, int(tc.occupied+tc.target))
			require.NoError(t, c.Get(context.Background(), req.NamespacedName, pool))
			if tc.capacity == nil {
				require.Nil(t, pool.Status.OccupiedReplicas)
				require.Nil(t, pool.Status.DesiredStandbyReplicas)
			} else {
				require.Equal(t, new(tc.occupied), pool.Status.OccupiedReplicas)
				require.Equal(t, new(tc.target), pool.Status.DesiredStandbyReplicas)
			}
		})
	}
	// Status counts are int32; subtraction is deliberately wider.
	require.Equal(t, int32(math.MaxInt32-1), desiredStandbyReplicas(&extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(math.MaxInt32)), ReplenishmentCapacity: new(int32(math.MaxInt32))}, 1))
	require.Zero(t, desiredStandbyReplicas(&extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(math.MaxInt32)), ReplenishmentCapacity: new(int32(0))}, math.MaxInt32))
}

func TestClaimAwarePoolCountsLiveObjectsNotReadinessOrClaimRecords(t *testing.T) {
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), ReplenishmentCapacity: new(int32(5)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}}}
	objects := []runtime.Object{pool, template, &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "empty-retained", Namespace: pool.Namespace, UID: "empty-claim"}, Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name}}}}
	for i, state := range []string{"ready", "pending", "suspended", "terminating", "other-owner"} {
		sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: state, Namespace: pool.Namespace, UID: types.UID(state), Annotations: map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)}, Labels: map[string]string{warmPoolSandboxLabel: sandboxcontrollers.NameHash(pool.Name)}}, Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()}}
		// Keep a stale membership label: none of these used instances is stock.
		if i == 0 {
			sb.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
		}
		if state == "terminating" {
			sb.DeletionTimestamp = new(metav1.NewTime(time.Now()))
			sb.Finalizers = []string{"test/finalizer"}
		}
		if state == "suspended" {
			sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
		}
		if state == "other-owner" {
			sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-owner", Controller: new(true)}}
		}
		objects = append(objects, sb)
	}
	c := newFakeClient(newTestScheme(), objects...)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme(), MaxBatchSize: 3}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(context.Background(), list))
	require.Len(t, list.Items, 5, "dirty orphans and other-owned instances must not be adopted or deleted")
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pool), pool))
	require.Equal(t, new(int32(5)), pool.Status.OccupiedReplicas)
	require.Equal(t, new(int32(0)), pool.Status.DesiredStandbyReplicas)
	for _, sb := range list.Items {
		require.False(t, metav1.IsControlledBy(&sb, pool))
	}
}

func TestClaimAwarePoolReadFailureClearsAccounting(t *testing.T) {
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{ReplenishmentCapacity: new(int32(1))}, Status: extensionsv1beta1.SandboxWarmPoolStatus{OccupiedReplicas: new(int32(0)), DesiredStandbyReplicas: new(int32(1))}}
	c := newFakeClient(newTestScheme(), pool)
	r := &SandboxWarmPoolReconciler{Client: interceptor.NewClient(c, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return fmt.Errorf("population unavailable")
	}}), Scheme: newTestScheme()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.ErrorContains(t, err, "population unavailable")
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pool), pool))
	require.Nil(t, pool.Status.OccupiedReplicas)
	require.Nil(t, pool.Status.DesiredStandbyReplicas)
}

func TestClaimAwarePoolRoutesSandboxWithoutSourceThroughClaimOwner(t *testing.T) {
	claim := &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "default", UID: "claim-uid"}, Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "pool"}}}
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "used", Namespace: claim.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind, Name: claim.Name, UID: claim.UID, Controller: new(true)}}}}
	c := newFakeClient(newTestScheme(), claim, sb)
	h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: newWarmPoolExpectations(), reader: c}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: sb.DeepCopy(), ObjectNew: sb}, q)
	require.Equal(t, 1, q.Len(), "source metadata-only degradation must still wake the linked pool")
}

func TestClaimAwarePoolPausesForUnattributedClaimOwnedSandbox(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid", Generation: 1},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), ReplenishmentCapacity: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
		Status:     extensionsv1beta1.SandboxWarmPoolStatus{OccupiedReplicas: new(int32(0)), DesiredStandbyReplicas: new(int32(1))},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name}},
	}
	// Binding status has not caught up. Ownership still proves that a live
	// instance is linked to this name, but not which pool incarnation supplied it.
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "claimed", Namespace: "default", UID: "claimed-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxClaim", Name: claim.Name, UID: claim.UID, Controller: new(true)}},
	}}
	c := newFakeClient(scheme, pool, template, claim, sb)
	recorder := events.NewFakeRecorder(10)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: 1, Recorder: recorder}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	_, err := r.Reconcile(ctx, request)
	require.Error(t, err, "missing source must not silently become zero occupancy")
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(ctx, list))
	require.Len(t, list.Items, 1, "accounting failure must suspend inventory writes")
	require.NoError(t, c.Get(ctx, request.NamespacedName, pool))
	require.Nil(t, pool.Status.OccupiedReplicas)
	require.Nil(t, pool.Status.DesiredStandbyReplicas)
	select {
	case warning := <-recorder.Events:
		require.Contains(t, warning, "WarmPoolAccountingIncomplete")
	default:
		t.Fatal("accounting failure must emit a Warning Event")
	}
	_, err = r.Reconcile(ctx, request)
	require.Error(t, err)
	require.Empty(t, recorder.Events, "unchanged accounting failures must not emit repeated warnings")
	sb.Annotations = map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)}
	require.NoError(t, c.Update(ctx, sb))
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, request.NamespacedName, pool))
	require.Equal(t, new(int32(1)), pool.Status.OccupiedReplicas)
	require.Equal(t, new(int32(0)), pool.Status.DesiredStandbyReplicas)
	require.Contains(t, <-recorder.Events, "WarmPoolAccountingRecovered")
}

func TestClaimAwarePoolIgnoresKnownOtherIncarnations(t *testing.T) {
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "new-pool-uid"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), ReplenishmentCapacity: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}}}
	old := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "old-used", Namespace: pool.Namespace, UID: "old-used-uid", Annotations: map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: "old-pool-uid"}, Labels: map[string]string{warmPoolSandboxLabel: sandboxcontrollers.NameHash(pool.Name)}}}
	other := old.DeepCopy()
	other.Name, other.UID = "other-used", "other-used-uid"
	other.Annotations[sourceWarmPoolNameAnnotation] = "other-pool"
	other.Annotations[sourceWarmPoolUIDAnnotation] = "other-pool-uid"
	c := newFakeClient(newTestScheme(), pool, template, old, other)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme(), MaxBatchSize: 1}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(context.Background(), list))
	require.Len(t, list.Items, 3)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pool), pool))
	require.Equal(t, new(int32(0)), pool.Status.OccupiedReplicas)
	require.Equal(t, new(int32(1)), pool.Status.DesiredStandbyReplicas)
}

func TestClaimAwarePoolCapacityUpdatesAndRestart(t *testing.T) {
	ctx := context.Background()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(3)), ReplenishmentCapacity: new(int32(3)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}}}
	used := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "used", Namespace: pool.Namespace, UID: "used-uid", Annotations: map[string]string{sourceWarmPoolNameAnnotation: pool.Name, sourceWarmPoolUIDAnnotation: string(pool.UID)}}}
	c := newFakeClient(newTestScheme(), pool, template, used)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme(), MaxBatchSize: 3}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	for _, capacity := range []int32{0, 3} {
		require.NoError(t, c.Get(ctx, req.NamespacedName, pool))
		pool.Spec.ReplenishmentCapacity = new(capacity)
		require.NoError(t, c.Update(ctx, pool))
		syncPoolExpectations(r, pool)
		_, err = r.Reconcile(ctx, req)
		require.NoError(t, err)
		list := &sandboxv1beta1.SandboxList{}
		require.NoError(t, c.List(ctx, list))
		require.Len(t, list.Items, int(1+max(int32(0), capacity-1)))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(used), used), "capacity changes cannot delete occupied instances")
	}
	// A fresh controller has no prior in-memory accounting or operation ledger.
	r = &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme(), MaxBatchSize: 3}
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, req.NamespacedName, pool))
	require.Equal(t, new(int32(1)), pool.Status.OccupiedReplicas)
	require.Equal(t, new(int32(2)), pool.Status.DesiredStandbyReplicas)
	require.Equal(t, int32(2), pool.Status.Replicas)
}

func TestClaimAwarePoolRepairsOwnedInventoryBeforeManagingIt(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid", Generation: 1},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), ReplenishmentCapacity: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	sb := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-legacy")
	sb.UID = "inventory-uid"
	sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxWarmPool", Name: pool.Name, UID: pool.UID, Controller: new(true)}}
	c := newFakeClient(scheme, pool, template, sb)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: 1}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err, "a provable owner allows metadata repair, not guessed occupancy")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sb), sb))
	require.Equal(t, "pool-uid", sb.Annotations[sourceWarmPoolUIDAnnotation])
	require.NoError(t, c.Get(ctx, request.NamespacedName, pool))
	require.Nil(t, pool.Status.OccupiedReplicas, "repair must be observed before publishing accounting")
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, request.NamespacedName, pool))
	require.Equal(t, new(int32(0)), pool.Status.OccupiedReplicas)
	require.Equal(t, int32(1), pool.Status.Replicas)
}
