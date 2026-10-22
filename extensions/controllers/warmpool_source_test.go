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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestWarmPoolCreationPreservesSource(t *testing.T) {
	ctx := context.Background()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	scheme := newTestScheme()
	c := newFakeClient(scheme, template, pool)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: 1}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(ctx, list))
	require.Len(t, list.Items, 1)
	sb := &list.Items[0]
	require.Equal(t, "pool", sb.Annotations["agents.x-k8s.io/source-warmpool-name"])
	require.Equal(t, "pool-uid", sb.Annotations["agents.x-k8s.io/source-warmpool-uid"])
	require.NotContains(t, sb.Spec.PodTemplate.ObjectMeta.Annotations, "agents.x-k8s.io/source-warmpool-uid")
}

func TestWarmPoolAmbiguousPOSTWaitsForObservation(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	var posts atomic.Int32
	lostResponse := errors.New("connection lost after server accepted POST")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(pool).WithObjects(pool, template).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if posts.Add(1) == 1 {
				return lostResponse
			}
			return nil
		}}).Build()
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: 2}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	_, err := r.Reconcile(ctx, request)
	require.ErrorIs(t, err, lostResponse)
	require.NoError(t, c.Get(ctx, request.NamespacedName, pool))
	pool.Spec.Replicas = new(int32(2))
	require.NoError(t, c.Update(ctx, pool))
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, int32(1), posts.Load(), "ambiguous POST must keep its operation pending")
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(ctx, list))
	require.Len(t, list.Items, 1)
	h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: r.exp()}
	h.Create(ctx, event.CreateEvent{Object: &list.Items[0]}, nil)
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, int32(2), posts.Load(), "observation must unblock replenishment without waiting for expiry")
}

func TestWarmCreationObservedAfterClaimTransfer(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	c := newFakeClient(scheme, pool, template)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: 1}
	key := client.ObjectKeyFromObject(pool)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	expectationKey := warmPoolIdentity{NamespacedName: key, UID: pool.UID}
	require.False(t, r.exp().SatisfiedExpectations(expectationKey))
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, c.List(ctx, list))
	require.Len(t, list.Items, 1)
	sb := &list.Items[0]
	sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxClaim", Name: "consumer", UID: "claim-uid", Controller: new(true)}}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: r.exp()}
	h.Create(ctx, event.CreateEvent{Object: sb}, q)
	require.True(t, r.exp().SatisfiedExpectations(expectationKey), "first cache observation after adoption must confirm this warm POST")
}

func TestSourceEventsOnlyConfirmMatchingWarmOperations(t *testing.T) {
	ctx := context.Background()
	e := newWarmPoolExpectations()
	identity := warmPoolIdentity{NamespacedName: client.ObjectKey{Namespace: "default", Name: "pool"}, UID: "current-pool-uid"}
	require.True(t, e.TryExpectCreations(identity, []string{"current-operation"}))
	h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: e}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "claimed", UID: "sandbox-uid",
		Annotations: map[string]string{sourceWarmPoolNameAnnotation: "pool", sourceWarmPoolUIDAnnotation: "current-pool-uid", warmPoolCreateTokenAnnotation: "current-operation"},
		Labels:      map[string]string{sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeCold},
	}}
	h.Create(ctx, event.CreateEvent{Object: sb}, q)
	require.False(t, e.SatisfiedExpectations(identity), "cold creation must not confirm a warm operation")
	sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	sb.Annotations[sourceWarmPoolUIDAnnotation] = "old-pool-uid"
	h.Create(ctx, event.CreateEvent{Object: sb}, q)
	require.False(t, e.SatisfiedExpectations(identity), "same-name old pool event must not confirm the current pool")
	sb.Annotations[sourceWarmPoolUIDAnnotation] = "current-pool-uid"
	sb.Annotations[warmPoolCreateTokenAnnotation] = "old-operation"
	h.Create(ctx, event.CreateEvent{Object: sb}, q)
	require.False(t, e.SatisfiedExpectations(identity), "an old batch must not confirm a later operation")
	sb.Annotations[warmPoolCreateTokenAnnotation] = "current-operation"
	h.Update(ctx, event.UpdateEvent{ObjectOld: sb.DeepCopy(), ObjectNew: sb}, q)
	require.True(t, e.SatisfiedExpectations(identity))
	require.True(t, e.TryExpectCreations(identity, []string{"next-operation"}))
	h.Create(ctx, event.CreateEvent{Object: sb}, q)
	h.Delete(ctx, event.DeleteEvent{Object: sb}, q)
	require.False(t, e.SatisfiedExpectations(identity), "duplicate observations must not confirm the next operation")
	require.Equal(t, 1, q.Len(), "claimed source events must wake the originating pool")
}

func TestColdClaimKeepsResolvedPoolUID(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "original-pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "cold-consumer", Namespace: "default", UID: "cold-claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(claim).WithObjects(pool, template, claim).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*extensionsv1beta1.SandboxTemplate); ok {
				// The template was resolved using the original pool. A subsequent
				// name-only lookup must not assign the newly recreated pool's UID.
				require.NoError(t, c.Delete(ctx, pool))
				replacement := pool.DeepCopy()
				replacement.ResourceVersion = ""
				replacement.UID = "replacement-pool-uid"
				require.NoError(t, c.Create(ctx, replacement))
			}
			return nil
		}}).Build()
	r := &SandboxClaimReconciler{Client: c, Scheme: scheme, WarmSandboxQueue: queue.NewSimpleSandboxQueue(), Tracer: asmetrics.NewNoOp()}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	require.NoError(t, err)
	sb := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(claim), sb))
	require.Equal(t, "original-pool-uid", sb.Annotations["agents.x-k8s.io/source-warmpool-uid"])
	require.Equal(t, "pool", sb.Annotations["agents.x-k8s.io/source-warmpool-name"])
	require.True(t, metav1.IsControlledBy(sb, claim))
	require.NotContains(t, sb.Annotations, "agents.x-k8s.io/warmpool-create-token")
}

func TestWarmClaimPreservesSourceFromOwner(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name}},
	}
	sb := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-warm")
	sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxWarmPool", Name: pool.Name, UID: pool.UID, Controller: new(true)}}
	sb.Status.PodIPs = []string{"10.0.0.1"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(claim).WithObjects(pool, template, claim, sb).Build()
	warmQueue := queue.NewSimpleSandboxQueue()
	warmQueue.Add(queue.GetNamespacedWarmPoolName(pool.Namespace, pool.Name), queue.SandboxKey{Namespace: sb.Namespace, Name: sb.Name})
	r := &SandboxClaimReconciler{Client: c, Scheme: scheme, WarmSandboxQueue: warmQueue, Tracer: asmetrics.NewNoOp()}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sb), sb))
	require.True(t, metav1.IsControlledBy(sb, claim))
	require.Equal(t, "pool-uid", sb.Annotations["agents.x-k8s.io/source-warmpool-uid"])
	require.Equal(t, "pool", sb.Annotations["agents.x-k8s.io/source-warmpool-name"])
	require.NotContains(t, sb.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
}
