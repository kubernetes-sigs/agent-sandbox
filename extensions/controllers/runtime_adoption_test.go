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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/runtimeadoption"
)

func TestInitializeLaunchTypeSkipsOnlyRedundantRuntimeClassReads(t *testing.T) {
	const cold, warm = sandboxv1beta1.SandboxLaunchTypeCold, sandboxv1beta1.SandboxLaunchTypeWarm
	for _, tc := range []struct {
		name, existing, requested, runtimeClass, handler string
		protected, unavailable, missing                  bool
		reads                                            int
	}{
		{name: "cold ordinary steady state", existing: cold, requested: cold, runtimeClass: "alias", handler: "runc"},
		{name: "cold strict alias steady state", existing: cold, requested: cold, runtimeClass: "alias", handler: runtimeadoption.StrictHandler},
		{name: "cold deleted class steady state", existing: cold, requested: cold, runtimeClass: "alias"},
		{name: "protected cold state", existing: cold, requested: cold, runtimeClass: "alias", protected: true, unavailable: true},
		{name: "strict warm label", existing: warm, requested: warm, runtimeClass: runtimeadoption.StrictHandler, unavailable: true},
		{name: "strict alias warm label despite stale cache", existing: warm, requested: warm, runtimeClass: "alias", handler: runtimeadoption.StrictHandler, unavailable: true, reads: 1},
		{name: "strict alias cold label requested warm", existing: cold, requested: warm, runtimeClass: "alias", handler: runtimeadoption.StrictHandler, unavailable: true, reads: 1},
		{name: "strict alias warm label requested cold", existing: warm, requested: cold, runtimeClass: "alias", handler: runtimeadoption.StrictHandler, unavailable: true, reads: 1},
		{name: "strict alias unlabelled cold recovery", requested: cold, runtimeClass: "alias", handler: runtimeadoption.StrictHandler, unavailable: true, reads: 1},
		{name: "strict alias unlabelled warm recovery", requested: warm, runtimeClass: "alias", handler: runtimeadoption.StrictHandler, unavailable: true, reads: 1},
		{name: "ordinary alias unlabelled warm recovery", requested: warm, runtimeClass: "alias", handler: "runc", reads: 1},
		{name: "ordinary alias existing warm label", existing: warm, requested: warm, runtimeClass: "alias", handler: "runc", reads: 1},
		{name: "missing alias unlabelled recovery", requested: cold, runtimeClass: "alias", missing: true, reads: 1},
		{name: "missing alias warm recovery", existing: warm, requested: warm, runtimeClass: "alias", missing: true, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme()
			sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default"}}
			sandbox.Spec.PodTemplate.Spec.RuntimeClassName = &tc.runtimeClass
			if tc.existing != "" {
				sandbox.Labels = map[string]string{sandboxv1beta1.SandboxLaunchTypeLabel: tc.existing}
			}
			if tc.protected {
				sandbox.Status.RuntimeAdoption = &sandboxv1beta1.RuntimeAdoptionStatus{Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{AttemptID: "attempt-0000000001"}}
			}
			// A stale cache must not classify an alias as safe for warm
			// recovery after the RuntimeClass has been replaced on the API.
			cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox,
				&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: tc.runtimeClass}, Handler: "runc"}).Build()
			readerBuilder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.handler != "" {
				readerBuilder.WithObjects(&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: tc.runtimeClass}, Handler: tc.handler})
			}
			reads := 0
			reader := readerBuilder.WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
				reads++
				return c.Get(ctx, key, object, opts...)
			}}).Build()
			r := &SandboxClaimReconciler{Client: cached, APIReader: reader, Scheme: scheme}
			err := r.initializeSandboxLaunchTypeLabel(t.Context(), sandbox, tc.requested)
			switch {
			case tc.unavailable:
				require.ErrorIs(t, err, ErrRuntimeAdoptionUnavailable)
			case tc.missing:
				require.True(t, k8serrors.IsNotFound(err), "got %v", err)
			default:
				require.NoError(t, err)
			}
			require.Equal(t, tc.reads, reads)
			var observed sandboxv1beta1.Sandbox
			require.NoError(t, cached.Get(t.Context(), client.ObjectKeyFromObject(sandbox), &observed))
			expected := tc.existing
			if expected == "" && err == nil {
				expected = tc.requested
			}
			require.Equal(t, expected, observed.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])
		})
	}
}

func TestPoolDeletionLosesReservationRace(t *testing.T) {
	for _, cause := range []string{"stale", "excess", "stuck"} {
		t.Run(cause, func(t *testing.T) {
			ctx := context.Background()
			template := createTemplate("default")
			pool := &extensionsv1beta1.SandboxWarmPool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
				Spec: extensionsv1beta1.SandboxWarmPoolSpec{
					Replicas: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
				},
			}
			sandbox := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-member")
			sandbox.UID = "sandbox-uid"
			sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pool, extensionsv1beta1.GroupVersion.WithKind(extensionsv1beta1.SandboxWarmPoolKind))}
			sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
			sandbox.Status.Conditions = []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}}
			switch cause {
			case "stale":
				pool.Spec.UpdateStrategy = &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType}
				sandbox.Spec.PodTemplate.Spec.Containers[0].Image = "stale-image"
				sandbox.Labels[sandboxv1beta1.SandboxTemplateHashLabel] = "stale-hash"
			case "excess":
				pool.Spec.Replicas = new(int32(0))
			case "stuck":
				sandbox.Status.Conditions = nil
				sandbox.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
			}
			var deletes int
			c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(template, pool, sandbox).
				WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
				WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
				WithIndex(&sandboxv1beta1.Sandbox{}, RuntimeAdoptionPoolUIDIndex, runtimeAdoptionPoolUIDIndexer).
				WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					options := &client.DeleteOptions{}
					options.ApplyOptions(opts)
					require.NotNil(t, options.Preconditions)
					require.Equal(t, obj.GetUID(), *options.Preconditions.UID)
					require.Equal(t, obj.GetResourceVersion(), *options.Preconditions.ResourceVersion)
					fresh := &sandboxv1beta1.Sandbox{}
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), fresh))
					fresh.Status.RuntimeAdoption = &sandboxv1beta1.RuntimeAdoptionStatus{Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid", PoolUID: pool.UID}}
					require.NoError(t, c.Status().Update(ctx, fresh))
					return c.Delete(ctx, obj, opts...)
				}}).Build()
			r := &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme(), MaxBatchSize: 1}
			_, err := r.reconcilePool(ctx, pool)
			require.NoError(t, err)
			require.Equal(t, 1, deletes)
			require.True(t, r.exp().SatisfiedExpectations(client.ObjectKeyFromObject(pool)), "failed deletion must clear its expectation")
			observed := &sandboxv1beta1.Sandbox{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sandbox), observed))
			require.NotNil(t, observed.Status.RuntimeAdoption.Reservation)
			require.True(t, observed.DeletionTimestamp.IsZero())
			var members sandboxv1beta1.SandboxList
			require.NoError(t, c.List(ctx, &members, client.InNamespace(pool.Namespace)))
			require.Len(t, members.Items, 1, "a failed precondition must not free capacity for a replacement")
			_, err = r.reconcilePool(ctx, pool)
			require.NoError(t, err)
			require.Equal(t, 1, deletes, "the next snapshot must exclude the reserved member from cleanup")
			require.NoError(t, c.List(ctx, &members, client.InNamespace(pool.Namespace)))
			require.Len(t, members.Items, 1)
		})
	}
}

func TestPoolExcessDeletionConflictDoesNotStopLaterBatches(t *testing.T) {
	ctx := t.Context()
	scheme := newTestScheme()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(0)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	objects := []runtime.Object{template, pool}
	for i, name := range []string{"reserved", "later-one", "later-two"} {
		sandbox := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-"+name)
		sandbox.UID = types.UID(name)
		sandbox.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Duration(i) * time.Minute))
		sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pool, extensionsv1beta1.GroupVersion.WithKind(extensionsv1beta1.SandboxWarmPoolKind))}
		sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
		sandbox.Status.Conditions = []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}}
		objects = append(objects, sandbox)
	}
	var deletes atomic.Int32
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, RuntimeAdoptionPoolUIDIndex, runtimeAdoptionPoolUIDIndexer).
		WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes.Add(1)
			if obj.GetUID() == "reserved" {
				fresh := &sandboxv1beta1.Sandbox{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), fresh))
				fresh.Status.RuntimeAdoption = &sandboxv1beta1.RuntimeAdoptionStatus{Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{
					AttemptID: "attempt-0000000001", ClaimUID: "claim-uid", PoolUID: pool.UID,
				}}
				require.NoError(t, c.Status().Update(ctx, fresh))
			}
			return c.Delete(ctx, obj, opts...)
		}}).Build()
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: 3}
	_, err := r.reconcilePool(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, int32(3), deletes.Load(), "a conflict in the first batch must not stop later candidates")
	require.False(t, r.exp().IsPendingDeletion(client.ObjectKeyFromObject(pool), "reserved"))
	var members sandboxv1beta1.SandboxList
	require.NoError(t, c.List(ctx, &members, client.InNamespace(pool.Namespace)))
	require.Len(t, members.Items, 1)
	require.Equal(t, types.UID("reserved"), members.Items[0].UID)
	require.NotNil(t, members.Items[0].Status.RuntimeAdoption.Reservation)
}

func TestPoolExcludesReservedAndConsumedCandidates(t *testing.T) {
	ctx := context.Background()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}}
	var candidates []sandboxv1beta1.Sandbox
	var objects []runtime.Object
	for _, name := range []string{"reserved", "consumed", "orphan"} {
		sandbox := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-"+name)
		sandbox.UID = types.UID(name)
		sandbox.Status.RuntimeAdoption = &sandboxv1beta1.RuntimeAdoptionStatus{}
		if name == "consumed" {
			sandbox.Status.RuntimeAdoption.Consumed = &sandboxv1beta1.RuntimeAdoptionConsumption{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid"}
		} else {
			sandbox.Status.RuntimeAdoption.Reservation = &sandboxv1beta1.RuntimeAdoptionReservation{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid"}
		}
		if name != "orphan" {
			sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pool, extensionsv1beta1.GroupVersion.WithKind(extensionsv1beta1.SandboxWarmPoolKind))}
		}
		require.ErrorIs(t, isAdoptable(sandbox), ErrRuntimeAdoptionUnavailable)
		objects = append(objects, sandbox)
		candidates = append(candidates, *sandbox)
	}
	c := newFakeClient(newTestScheme(), objects...)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme()}
	active, _, err := r.filterActiveSandboxes(ctx, client.ObjectKeyFromObject(pool), pool, candidates, template, "", nil)
	require.NoError(t, err)
	require.Empty(t, active)
	for i := range candidates {
		observed := &sandboxv1beta1.Sandbox{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&candidates[i]), observed))
		require.Equal(t, candidates[i].OwnerReferences, observed.OwnerReferences)
		require.True(t, observed.DeletionTimestamp.IsZero())
	}
}

func TestPendingReservationConsumesPoolCapacityAfterMetadataTransfer(t *testing.T) {
	ctx := context.Background()
	template := createTemplate("default")
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(1)), TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}}}
	claim := &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: pool.Namespace, UID: "claim-uid"}}
	sandbox := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-reserved")
	sandbox.UID = "reserved-sandbox"
	delete(sandbox.Labels, warmPoolSandboxLabel)
	sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(claim, extensionsv1beta1.GroupVersion.WithKind("SandboxClaim"))}
	sandbox.Status.RuntimeAdoption = &sandboxv1beta1.RuntimeAdoptionStatus{Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{
		AttemptID: "attempt-0000000001", ClaimUID: claim.UID, PoolUID: pool.UID,
	}}
	c := newFakeClient(newTestScheme(), template, pool, sandbox)
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: newTestScheme(), MaxBatchSize: 1}
	_, err := r.reconcilePool(ctx, pool)
	require.NoError(t, err)
	var members sandboxv1beta1.SandboxList
	require.NoError(t, c.List(ctx, &members, client.InNamespace(pool.Namespace)))
	require.Len(t, members.Items, 1, "unresolved reservation still occupies a pool slot")
	require.Zero(t, pool.Status.ReadyReplicas)
	for _, completed := range []string{"commit", "destruction"} {
		t.Run(completed, func(t *testing.T) {
			finished := sandbox.DeepCopy()
			if completed == "commit" {
				finished.Status.RuntimeAdoption.CommitDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			} else {
				finished.Status.RuntimeAdoption.TerminalEvidenceDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			writer := newFakeClient(newTestScheme(), template, pool, finished)
			reconciler := &SandboxWarmPoolReconciler{Client: writer, Scheme: newTestScheme(), MaxBatchSize: 1}
			_, err := reconciler.reconcilePool(ctx, pool.DeepCopy())
			require.NoError(t, err)
			require.NoError(t, writer.List(ctx, &members, client.InNamespace(pool.Namespace)))
			require.Len(t, members.Items, 2, "confirmed outcome releases capacity for exactly one replacement")
		})
	}
}

func TestRuntimeBlueprintUsesSharedSecureDefaults(t *testing.T) {
	var cases []struct {
		Name            string                            `json:"name"`
		Template        extensionsv1beta1.SandboxTemplate `json:"template"`
		PodSpec         corev1.PodSpec                    `json:"podSpec"`
		ExpectedPodSpec corev1.PodSpec                    `json:"expectedPodSpec"`
	}
	readProtocolFixture(t, "agent-sandbox-secure-defaults.json", &cases)
	require.Len(t, cases, 8)
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			actual := tc.PodSpec.DeepCopy()
			ApplySandboxSecureDefaults(&tc.Template, actual)
			require.Equal(t, tc.ExpectedPodSpec, *actual)
			sandbox := &sandboxv1beta1.Sandbox{Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: *tc.Template.Spec.SandboxBlueprint.DeepCopy()}}
			sandbox.Spec.PodTemplate.Spec = *actual
			require.True(t, runtimeBlueprintMatchesTemplate(sandbox, &tc.Template))
			sandbox.Spec.PodTemplate.Spec.Containers[0].Image = "changed:latest"
			require.False(t, runtimeBlueprintMatchesTemplate(sandbox, &tc.Template))
		})
	}
}
