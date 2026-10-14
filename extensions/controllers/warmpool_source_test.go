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
	"k8s.io/utils/ptr"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestWarmPoolCreationPreservesSource(t *testing.T) {
	ctx := context.Background()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas: ptr.To(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
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
	sb.OwnerReferences = []metav1.OwnerReference{{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxWarmPool", Name: pool.Name, UID: pool.UID, Controller: ptr.To(true)}}
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
