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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
