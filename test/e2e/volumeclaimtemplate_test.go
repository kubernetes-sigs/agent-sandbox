// Copyright 2025 The Kubernetes Authors.
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

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestSandboxVolumeClaimTemplates(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-vct-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	sandboxObj := &sandboxv1alpha1.Sandbox{}
	sandboxObj.Name = "vct-sandbox"
	sandboxObj.Namespace = ns.Name
	sandboxObj.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: "/data",
						},
					},
				},
			},
		},
	}
	sandboxObj.Spec.VolumeClaimTemplates = []sandboxv1alpha1.PersistentVolumeClaimTemplate{
		{
			EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
				Name: "data",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)

	// Wait for the sandbox to become ready
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       "vct-sandbox",
			ServiceFQDN:   fmt.Sprintf("vct-sandbox.%s.svc.cluster.local", ns.Name),
			Replicas:      1,
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1alpha1.SandboxConditionInitialized),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
					Reason:             sandboxv1alpha1.SandboxReasonInitialized,
					Message:            "Service and PVCs are provisioned",
				},
				{
					Type:               string(sandboxv1alpha1.SandboxConditionSuspended),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 1,
					Reason:             sandboxv1alpha1.SandboxReasonNotSuspended,
					Message:            "Sandbox is operational and not suspended",
				},
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
					Reason:             sandboxv1alpha1.SandboxReasonReady,
					Message:            "Sandbox is operational",
				},
			},
		}),
	}
	require.NoError(t, tc.WaitForObject(t.Context(), sandboxObj, p...))

	// Verify the PVC was created with the expected name (template name + "-" + sandbox name)
	pvc := &corev1.PersistentVolumeClaim{}
	pvcName := "data-vct-sandbox"
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: pvcName, Namespace: ns.Name}, pvc))

	// Verify PVC is owned by the sandbox
	require.Len(t, pvc.OwnerReferences, 1)
	require.Equal(t, "Sandbox", pvc.OwnerReferences[0].Kind)
	require.Equal(t, sandboxObj.Name, pvc.OwnerReferences[0].Name)
	require.Equal(t, sandboxObj.UID, pvc.OwnerReferences[0].UID)
	require.NotNil(t, pvc.OwnerReferences[0].Controller)
	require.True(t, *pvc.OwnerReferences[0].Controller)

	// Verify PVC has the sandbox label
	require.Equal(t, nameHash, pvc.Labels["agents.x-k8s.io/sandbox-name-hash"])

	// Verify the pod has a PVC volume mounted
	pod := &corev1.Pod{}
	pod.Name = "vct-sandbox"
	pod.Namespace = ns.Name
	tc.MustExist(pod)

	// Find the "data" volume in the pod spec and verify it references the PVC
	var found bool
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == "data" {
			require.NotNil(t, vol.PersistentVolumeClaim, "expected data volume to be a PVC volume")
			require.Equal(t, pvcName, vol.PersistentVolumeClaim.ClaimName)
			found = true
			break
		}
	}
	require.True(t, found, "expected pod to have a 'data' volume backed by PVC")
}
