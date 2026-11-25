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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// Objects that a controller created before v1.0 name their owner with
// apiVersion v1alpha1. Once v1alpha1 is no longer served, the garbage
// collector cannot resolve that owner, so reconciling the owner must rewrite
// the reference to the served version.

const alphaExtensionsAPIVersion = "extensions.agents.x-k8s.io/v1alpha1"

func alphaControllerRef(kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         alphaExtensionsAPIVersion,
		Kind:               kind,
		Name:               name,
		UID:                uid,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}
}

func requireRefreshedOwner(t *testing.T, refs []metav1.OwnerReference, uid types.UID) {
	t.Helper()
	require.Len(t, refs, 1)
	require.Equal(t, extensionsv1beta1.GroupVersion.String(), refs[0].APIVersion)
	require.Equal(t, uid, refs[0].UID)
}

func TestSandboxTemplateRefreshesV1alpha1NetworkPolicyOwner(t *testing.T) {
	scheme := newTestScheme()
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: "default", UID: "tpl-uid"},
	}
	// The spec already matches, so only the stale owner reference needs a write.
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "tpl-network-policy",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{alphaControllerRef("SandboxTemplate", "tpl", template.UID)},
		},
		Spec: buildDefaultNetworkPolicySpec(template.Name),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, np).Build()
	r := &SandboxTemplateReconciler{Client: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10), Tracer: asmetrics.NewNoOp()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "tpl", Namespace: "default"}})
	require.NoError(t, err)

	live := &networkingv1.NetworkPolicy{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: np.Name, Namespace: "default"}, live))
	requireRefreshedOwner(t, live.OwnerReferences, template.UID)
}

func TestSandboxWarmPoolRefreshesV1alpha1SandboxOwner(t *testing.T) {
	scheme := newTestScheme()
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
	}
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "pool-abcde",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{alphaControllerRef("SandboxWarmPool", "pool", pool.UID)},
		},
	}
	c := newFakeClient(scheme, pool, sb)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize, Recorder: events.NewFakeRecorder(10)}

	// A template error skips the staleness checks; the owner refresh does not depend on them.
	poolKey := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
	_, _, err := r.filterActiveSandboxes(context.Background(), poolKey, pool, []sandboxv1beta1.Sandbox{*sb}, nil, "", errors.New("template unavailable"))
	require.NoError(t, err)

	live := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: sb.Name, Namespace: "default"}, live))
	requireRefreshedOwner(t, live.OwnerReferences, pool.UID)
}

func TestSandboxClaimRefreshesV1alpha1SandboxOwner(t *testing.T) {
	scheme := newTestScheme()
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "pool"},
		},
	}
	claim.Status.SandboxStatus.Name = "pool-abcde" // adopted from the warm pool before the upgrade
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "pool-abcde",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{alphaControllerRef("SandboxClaim", "claim", claim.UID)},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
			PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox"}}}},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, sb).Build()
	r := &SandboxClaimReconciler{
		Client:           c,
		Scheme:           scheme,
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
	}

	got, err := r.reconcileActive(context.Background(), claim)
	require.NoError(t, err)
	require.NotNil(t, got)

	live := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: sb.Name, Namespace: "default"}, live))
	requireRefreshedOwner(t, live.OwnerReferences, claim.UID)
}

// Concurrent adoption replaces a pool owner after the cache read. An owner
// refresh must not put the old pool owner back or act on that stale sandbox.
func TestSandboxWarmPoolOwnerRefreshPreservesConcurrentAdoption(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
	}
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-sandbox", Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{alphaControllerRef("SandboxWarmPool", pool.Name, pool.UID)},
	}}
	raw := newFakeClient(scheme, pool, sb)
	stale := &sandboxv1beta1.Sandbox{}
	require.NoError(t, raw.Get(ctx, client.ObjectKeyFromObject(sb), stale))
	adopted := alphaControllerRef("SandboxClaim", "claim", "claim-uid")
	adopted.APIVersion = extensionsv1beta1.GroupVersion.String()
	writes := 0
	cached := interceptor.NewClient(raw, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			live := &sandboxv1beta1.Sandbox{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sb), live))
			live.OwnerReferences = []metav1.OwnerReference{adopted}
			require.NoError(t, c.Update(ctx, live))
			return c.Patch(ctx, obj, patch, opts...)
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("must not update a sandbox after its owner refresh conflicts")
			return nil
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			t.Fatal("must not delete a sandbox after its owner refresh conflicts")
			return nil
		},
	})
	r := &SandboxWarmPoolReconciler{Client: cached, Scheme: scheme}
	poolKey := client.ObjectKeyFromObject(pool)
	_, _, err := r.filterActiveSandboxes(ctx, poolKey, pool, []sandboxv1beta1.Sandbox{*stale}, nil, "", errors.New("no template"))
	require.True(t, k8errors.IsConflict(err), "a stale owner array must be rejected: %v", err)
	live := &sandboxv1beta1.Sandbox{}
	require.NoError(t, raw.Get(ctx, client.ObjectKeyFromObject(sb), live))
	require.Equal(t, []metav1.OwnerReference{adopted}, live.OwnerReferences)
	active, _, err := r.filterActiveSandboxes(ctx, poolKey, pool, []sandboxv1beta1.Sandbox{*live}, nil, "", errors.New("no template"))
	require.NoError(t, err)
	require.Empty(t, active, "the next pass must leave claim-owned sandboxes alone")
	require.Equal(t, 1, writes, "the next pass must not rewrite the adopted sandbox")
}

// A concurrent writer can add an unrelated owner while the claim refreshes its
// own reference. Reject the stale array, then preserve both owners on retry.
func TestSandboxClaimOwnerRefreshPreservesConcurrentOwner(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "pool"}},
	}
	claim.Status.SandboxStatus.Name = "pool-sandbox"
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-sandbox", Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{alphaControllerRef("SandboxClaim", claim.Name, claim.UID)},
	}}
	raw := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, sb).Build()
	additional := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other-owner", UID: "other-uid"}
	concurrentWrite := true
	cached := interceptor.NewClient(raw, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			require.NoError(t, err)
			if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && concurrentWrite && strings.Contains(string(data), `"ownerReferences"`) {
				concurrentWrite = false
				live := &sandboxv1beta1.Sandbox{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sb), live))
				live.OwnerReferences = append(live.OwnerReferences, additional)
				require.NoError(t, c.Update(ctx, live))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	r := &SandboxClaimReconciler{
		Client: cached, Scheme: scheme, WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
		Recorder: events.NewFakeRecorder(10), Tracer: asmetrics.NewNoOp(),
	}
	_, err := r.reconcileActive(ctx, claim)
	require.True(t, k8errors.IsConflict(err), "a stale owner array must be rejected: %v", err)
	live := &sandboxv1beta1.Sandbox{}
	require.NoError(t, raw.Get(ctx, client.ObjectKeyFromObject(sb), live))
	require.Len(t, live.OwnerReferences, 2)
	require.Equal(t, alphaExtensionsAPIVersion, live.OwnerReferences[0].APIVersion)
	require.Equal(t, additional, live.OwnerReferences[1])
	_, err = r.reconcileActive(ctx, claim)
	require.NoError(t, err)
	require.NoError(t, raw.Get(ctx, client.ObjectKeyFromObject(sb), live))
	require.Len(t, live.OwnerReferences, 2)
	require.Equal(t, extensionsv1beta1.GroupVersion.String(), live.OwnerReferences[0].APIVersion)
	require.Equal(t, additional, live.OwnerReferences[1])
}
