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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// TestSandboxPodFailurePolicyRecreate verifies that a Failed pod is deleted and
// replaced when podFailurePolicy=Recreate, and that PVC data survives across the
// recreation (first boot writes a marker and exits 1; second boot sees it and sleeps).
func TestSandboxPodFailurePolicyRecreate(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-pod-failure-recreate-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recreate-failed-sandbox",
			Namespace: ns.Name,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:  "busybox",
							Image: "busybox:1.36",
							Command: []string{"sh", "-c",
								`if [ -f /data/booted ]; then sleep infinity; else echo surviving > /data/booted; exit 1; fi`,
							},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "data",
								MountPath: "/data",
							}},
						}},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				}},
			},
			OperatingMode:    sandboxv1beta1.SandboxOperatingModeRunning,
			PodFailurePolicy: sandboxv1beta1.PodFailurePolicyRecreate,
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))

	podKey := types.NamespacedName{Name: sandbox.Name, Namespace: ns.Name}
	// Capture the first Pod's UID as soon as it is created. Waiting for a
	// transient Failed or NotFound window is racy because the controller deletes
	// Failed Pods for recreate immediately and then reuses the same Pod name.
	initialPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: ns.Name}}
	require.NoError(t, tc.WaitForObject(t.Context(), initialPod))
	require.NoError(t, tc.Get(t.Context(), podKey, initialPod))
	require.NotEmpty(t, initialPod.UID, "expected initial pod UID to be assigned")
	initialPodUID := initialPod.UID

	nameHash := NameHash(sandbox.Name)
	require.NoError(t, tc.WaitForObject(t.Context(), sandbox, predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
		Service:       sandbox.Name,
		ServiceFQDN:   fmt.Sprintf("%s.%s.svc.cluster.local", sandbox.Name, ns.Name),
		LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
		Conditions: []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
			Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
			Message:            "Pod is Ready; Service Exists",
		}},
	})))

	pvc := &corev1.PersistentVolumeClaim{}
	pvcName := "data-" + sandbox.Name
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: pvcName, Namespace: ns.Name}, pvc))
	require.Len(t, pvc.OwnerReferences, 1)
	require.Equal(t, sandbox.UID, pvc.OwnerReferences[0].UID)

	pod := &corev1.Pod{}
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), podKey, pod); err != nil {
			return false
		}
		return pod.Status.Phase == corev1.PodRunning && pod.UID != initialPodUID
	}, 60*time.Second, time.Second, "expected recreate policy to replace the initial pod with a running pod")

	var foundVolume bool
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == "data" && vol.PersistentVolumeClaim != nil {
			require.Equal(t, pvcName, vol.PersistentVolumeClaim.ClaimName)
			foundVolume = true
			break
		}
	}
	require.True(t, foundVolume, "expected remounted PVC volume on recreated pod")
}
