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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// A Pod, Service, or PVC created before v1.0 names its Sandbox with
// apiVersion v1alpha1. Once v1alpha1 is no longer served, the garbage
// collector cannot resolve that owner, so reconciling the Sandbox must
// rewrite the reference to the served version.
func TestReconcileRefreshesV1alpha1OwnerReferences(t *testing.T) {
	const (
		sandboxName     = "sandbox-name"
		sandboxNs       = "sandbox-ns"
		nameHash        = "name-hash"
		pvcTemplateName = "workspace"
	)
	alphaRef := sandboxControllerRef(sandboxName)
	alphaRef.APIVersion = "agents.x-k8s.io/v1alpha1"

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs, UID: sandboxUID},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
				Service: new(true),
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: pvcTemplateName},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
						},
					},
				}},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandboxName,
			Namespace:       sandboxNs,
			Labels:          map[string]string{sandboxv1beta1.SandboxNameHashLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{alphaRef},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandboxName,
			Namespace:       sandboxNs,
			Labels:          map[string]string{sandboxv1beta1.SandboxNameHashLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{alphaRef},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  map[string]string{sandboxv1beta1.SandboxNameHashLabel: nameHash},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pvcTemplateName + "-" + sandboxName,
			Namespace:       sandboxNs,
			OwnerReferences: []metav1.OwnerReference{alphaRef},
		},
	}

	r := &SandboxReconciler{
		Client:        newFakeClient(sandbox, pod, service, pvc),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}
	ctx := t.Context()

	_, _, err := r.reconcilePod(ctx, sandbox.DeepCopy(), nameHash, nil)
	require.NoError(t, err)
	_, err = r.reconcileService(ctx, sandbox.DeepCopy(), nameHash)
	require.NoError(t, err)
	require.NoError(t, r.reconcilePVCs(ctx, sandbox.DeepCopy(), nameHash))

	key := types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}
	livePod := &corev1.Pod{}
	require.NoError(t, r.Get(ctx, key, livePod))
	liveService := &corev1.Service{}
	require.NoError(t, r.Get(ctx, key, liveService))
	livePVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: sandboxNs}, livePVC))

	for kind, refs := range map[string][]metav1.OwnerReference{
		"Pod":     livePod.OwnerReferences,
		"Service": liveService.OwnerReferences,
		"PVC":     livePVC.OwnerReferences,
	} {
		require.Len(t, refs, 1, kind)
		require.Equal(t, sandboxv1beta1.GroupVersion.String(), refs[0].APIVersion, kind)
		require.Equal(t, sandboxUID, refs[0].UID, kind)
	}
}

func TestOwnerRefreshPreservesConcurrentDependentOwner(t *testing.T) {
	for _, kind := range []string{"Pod", "Service", "PVC"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			sandbox := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: sandboxUID},
				Spec: sandboxv1beta1.SandboxSpec{
					SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
						PodTemplate:          sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "test"}}}},
						Service:              new(true),
						VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "workspace"}}},
					},
					OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			}
			owner := sandboxControllerRef(sandbox.Name)
			owner.APIVersion = "agents.x-k8s.io/v1alpha1"
			metadata := metav1.ObjectMeta{
				Name: sandbox.Name, Namespace: sandbox.Namespace,
				Labels:          map[string]string{sandboxv1beta1.SandboxNameHashLabel: "hash"},
				OwnerReferences: []metav1.OwnerReference{owner},
			}
			var object client.Object
			switch kind {
			case "Pod":
				object = &corev1.Pod{ObjectMeta: metadata, Spec: *sandbox.Spec.PodTemplate.Spec.DeepCopy()}
			case "Service":
				object = &corev1.Service{ObjectMeta: metadata, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone}}
			case "PVC":
				metadata.Name = "workspace-" + sandbox.Name
				object = &corev1.PersistentVolumeClaim{ObjectMeta: metadata}
			}
			raw := newFakeClient(sandbox, object)
			additional := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other-owner", UID: "other-uid"}
			concurrentWrite := true
			cached := interceptor.NewClient(raw, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					data, err := patch.Data(obj)
					require.NoError(t, err)
					if concurrentWrite && strings.Contains(string(data), `"ownerReferences"`) {
						concurrentWrite = false
						live := object.DeepCopyObject().(client.Object)
						require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(object), live))
						live.SetOwnerReferences(append(live.GetOwnerReferences(), additional))
						require.NoError(t, c.Update(ctx, live))
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			r := &SandboxReconciler{Client: cached, Scheme: Scheme, Tracer: asmetrics.NewNoOp(), ClusterDomain: "cluster.local"}
			reconcile := func() error {
				switch kind {
				case "Pod":
					_, _, err := r.reconcilePod(ctx, sandbox.DeepCopy(), "hash", nil)
					return err
				case "Service":
					_, err := r.reconcileService(ctx, sandbox.DeepCopy(), "hash")
					return err
				default:
					return r.reconcilePVCs(ctx, sandbox.DeepCopy(), "hash")
				}
			}
			err := reconcile()
			require.True(t, k8errors.IsConflict(err), "stale %s owner patch must conflict: %v", kind, err)
			live := object.DeepCopyObject().(client.Object)
			require.NoError(t, raw.Get(ctx, client.ObjectKeyFromObject(object), live))
			require.Equal(t, []metav1.OwnerReference{owner, additional}, live.GetOwnerReferences())
			require.NoError(t, reconcile())
			require.NoError(t, raw.Get(ctx, client.ObjectKeyFromObject(object), live))
			owner.APIVersion = sandboxv1beta1.GroupVersion.String()
			require.Equal(t, []metav1.OwnerReference{owner, additional}, live.GetOwnerReferences())
		})
	}
}

func TestLegacyPVCOwnerRefreshDuringInactiveLifecycle(t *testing.T) {
	for _, mode := range []string{"deleting", "expired-first-pass", "expired-retained"} {
		for _, owned := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/owned=%t", mode, owned), func(t *testing.T) {
				sandbox := &sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: sandboxUID},
					Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
						VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
							{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "workspace"}},
							{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "missing"}},
						},
					}},
				}
				now := metav1.Now()
				if mode == "deleting" {
					sandbox.DeletionTimestamp = &now
					sandbox.Finalizers = []string{"example.test/hold"}
				} else {
					past := metav1.NewTime(time.Now().Add(-time.Hour))
					sandbox.Spec.Lifecycle.ShutdownTime = &past
					sandbox.Spec.Lifecycle.ShutdownPolicy = new(sandboxv1beta1.ShutdownPolicyRetain)
					if mode == "expired-retained" {
						setSandboxExpiredCondition(sandbox)
					}
				}
				owner := sandboxControllerRef(sandbox.Name)
				owner.APIVersion = "agents.x-k8s.io/v1alpha1"
				if !owned {
					owner.UID = "other-sandbox-uid"
				}
				pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
					Name: "workspace-" + sandbox.Name, Namespace: sandbox.Namespace,
					OwnerReferences: []metav1.OwnerReference{owner},
				}}
				raw := newFakeClient(sandbox, pvc)
				creates := 0
				c := interceptor.NewClient(raw, interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						creates++
						return nil
					},
				})
				r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp(), Recorder: events.NewFakeRecorder(10)}
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)})
				require.NoError(t, err)
				live := &corev1.PersistentVolumeClaim{}
				require.NoError(t, raw.Get(t.Context(), client.ObjectKeyFromObject(pvc), live))
				if owned {
					owner.APIVersion = sandboxv1beta1.GroupVersion.String()
				}
				require.Equal(t, []metav1.OwnerReference{owner}, live.OwnerReferences)
				require.Zero(t, creates, "inactive lifecycle repair must not recreate missing children")
			})
		}
	}
}

func TestLegacyOwnerRefreshPreservesAmbiguousPodRouting(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: sandboxUID},
		Spec:       sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(true)}},
	}
	hash := NameHash(sandbox.Name)
	owner := sandboxControllerRef(sandbox.Name)
	owner.APIVersion = "agents.x-k8s.io/v1alpha1"
	podMetadata := metav1.ObjectMeta{Namespace: sandbox.Namespace, Labels: map[string]string{sandboxv1beta1.SandboxNameHashLabel: hash}, OwnerReferences: []metav1.OwnerReference{owner}}
	a := &corev1.Pod{ObjectMeta: *podMetadata.DeepCopy()}
	a.Name = "a"
	b := &corev1.Pod{ObjectMeta: *podMetadata.DeepCopy()}
	b.Name = "b"
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace, Labels: map[string]string{"operator": "inspect"}, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"operator": "inspect"}},
	}
	c := newFakeClient(sandbox, a, b, service)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp(), Recorder: events.NewFakeRecorder(10)}
	_, err := r.reconcileChildResources(t.Context(), sandbox, nil)
	require.NoError(t, err)
	require.Empty(t, sandbox.Status.PodIPs)
	ready := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, ready)
	require.Equal(t, sandboxv1beta1.SandboxReasonMultiplePods, ready.Reason)
	for _, object := range []client.Object{a, b, service} {
		live := object.DeepCopyObject().(client.Object)
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(object), live))
		require.Equal(t, sandboxv1beta1.GroupVersion.String(), live.GetOwnerReferences()[0].APIVersion)
	}
	liveService := &corev1.Service{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(service), liveService))
	require.Equal(t, service.Spec, liveService.Spec)
	require.Equal(t, service.Labels, liveService.Labels)
}

func TestLegacyOwnerRefreshBypassesPodWriteBehind(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: sandboxUID}, Spec: sandboxv1beta1.SandboxSpec{OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning}}
	hash := NameHash(sandbox.Name)
	owner := sandboxControllerRef(sandbox.Name)
	owner.APIVersion = "agents.x-k8s.io/v1alpha1"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace, Labels: map[string]string{sandboxv1beta1.SandboxNameHashLabel: hash}, OwnerReferences: []metav1.OwnerReference{owner}}}
	c := newFakeClient(sandbox, pod)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}
	wd := &writeDeferral{clock: &deferredWriteClock{}, key: client.ObjectKeyFromObject(sandbox), window: time.Hour}
	_, _, err := r.reconcilePod(t.Context(), sandbox, hash, wd)
	require.NoError(t, err)
	require.False(t, wd.deferred, "owner changes must be synchronous even inside a fresh write-behind window")
	live := &corev1.Pod{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(pod), live))
	require.Equal(t, sandboxv1beta1.GroupVersion.String(), live.OwnerReferences[0].APIVersion)
}
