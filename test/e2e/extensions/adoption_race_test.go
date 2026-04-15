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

package extensions

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestClaimSkipsPreAnnotatedSandbox verifies that a sandbox with a pre-existing
// claimed-by annotation is not adopted by a new SandboxClaim. The claim should
// cold-start a fresh sandbox instead.
func TestClaimSkipsPreAnnotatedSandbox(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("skip-preannotated-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "skip-template"
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// Create a SandboxWarmPool with 1 replica
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "skip-pool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Wait for the warm pool sandbox to become ready
	var poolSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, sb := range sandboxList.Items {
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
				for _, cond := range sb.Status.Conditions {
					if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
						poolSandboxName = sb.Name
						return true
					}
				}
			}
		}
		return false
	}, 60*time.Second, 2*time.Second, "warm pool sandbox should become ready")

	// Pre-annotate the sandbox with a claimed-by annotation to simulate a prior reservation
	poolSandbox := &sandboxv1alpha1.Sandbox{}
	poolSandbox.Name = poolSandboxName
	poolSandbox.Namespace = ns.Name
	framework.MustUpdateObject(tc.ClusterClient, poolSandbox, func(sb *sandboxv1alpha1.Sandbox) {
		if sb.Annotations == nil {
			sb.Annotations = make(map[string]string)
		}
		sb.Annotations[sandboxv1alpha1.SandboxClaimedByAnnotation] = "some-other-uid"
	})

	// Create a claim referencing the same template
	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = "skip-claim"
	claim.Namespace = ns.Name
	claim.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for the claim to have a sandbox name in status (either adopted or cold-started)
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: ns.Name}, claim); err != nil {
			return false
		}
		return claim.Status.SandboxStatus.Name != ""
	}, 60*time.Second, 2*time.Second, "claim should get a sandbox name in status")

	// Verify the claim did NOT adopt the reserved sandbox -- it cold-started a new one
	require.NotEqual(t, poolSandboxName, claim.Status.SandboxStatus.Name,
		"claim should not adopt the pre-annotated sandbox; expected cold-start")

	// Verify the reserved sandbox still has its warm pool owner reference (not adopted)
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, poolSandbox))
	require.True(t, metav1.IsControlledBy(poolSandbox, warmPool),
		"reserved sandbox should still be controlled by the warm pool")
}

// TestClaimAdoptionWritesClaimedByAnnotation verifies that when a claim adopts
// a warm pool sandbox, the adopted sandbox receives a claimed-by annotation
// set to the adopting claim's UID.
func TestClaimAdoptionWritesClaimedByAnnotation(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("adoption-annotation-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "adopt-template"
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// Create a SandboxWarmPool with 1 replica (no pre-annotation)
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "adopt-pool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Wait for the warm pool sandbox to become ready
	var poolSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, sb := range sandboxList.Items {
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
				for _, cond := range sb.Status.Conditions {
					if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
						poolSandboxName = sb.Name
						return true
					}
				}
			}
		}
		return false
	}, 60*time.Second, 2*time.Second, "warm pool sandbox should become ready")

	// Create a claim to adopt the sandbox
	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = "adopt-claim"
	claim.Namespace = ns.Name
	claim.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for the claim to become ready
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: ns.Name}, claim); err != nil {
			return false
		}
		if claim.Status.SandboxStatus.Name == "" {
			return false
		}
		for _, cond := range claim.Status.Conditions {
			if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 60*time.Second, 2*time.Second, "claim should become ready")

	// Verify the claim adopted the warm pool sandbox
	require.Equal(t, poolSandboxName, claim.Status.SandboxStatus.Name,
		"claim should adopt the warm pool sandbox")

	// Verify the adopted sandbox has the claimed-by annotation set to the claim's UID
	adoptedSandbox := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{
		Name:      claim.Status.SandboxStatus.Name,
		Namespace: ns.Name,
	}, adoptedSandbox))

	require.Contains(t, adoptedSandbox.Annotations, sandboxv1alpha1.SandboxClaimedByAnnotation,
		"adopted sandbox should have the claimed-by annotation")
	require.Equal(t, string(claim.UID), adoptedSandbox.Annotations[sandboxv1alpha1.SandboxClaimedByAnnotation],
		"claimed-by annotation should equal the claim's UID")
}

// TestMultipleClaimsRespectReservations verifies that when all warm pool
// sandboxes are pre-annotated with claimed-by values, new claims cold-start
// instead of adopting any reserved sandbox.
func TestMultipleClaimsRespectReservations(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("multi-reserve-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "multi-template"
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// Create a SandboxWarmPool with 3 replicas
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "multi-pool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 3
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Wait for all 3 warm pool sandboxes to become ready
	var reservedNames []string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		var readyNames []string
		for _, sb := range sandboxList.Items {
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
				for _, cond := range sb.Status.Conditions {
					if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
						readyNames = append(readyNames, sb.Name)
					}
				}
			}
		}
		if len(readyNames) == 3 {
			reservedNames = readyNames
			return true
		}
		return false
	}, 90*time.Second, 2*time.Second, "all 3 warm pool sandboxes should become ready")

	// Pre-annotate all 3 sandboxes with distinct claimed-by values
	for i, name := range reservedNames {
		sb := &sandboxv1alpha1.Sandbox{}
		sb.Name = name
		sb.Namespace = ns.Name
		uid := fmt.Sprintf("reserved-uid-%d", i)
		framework.MustUpdateObject(tc.ClusterClient, sb, func(s *sandboxv1alpha1.Sandbox) {
			if s.Annotations == nil {
				s.Annotations = make(map[string]string)
			}
			s.Annotations[sandboxv1alpha1.SandboxClaimedByAnnotation] = uid
		})
	}

	// Create 2 claims referencing the same template
	claim1 := &extensionsv1alpha1.SandboxClaim{}
	claim1.Name = "multi-claim-1"
	claim1.Namespace = ns.Name
	claim1.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim1))

	claim2 := &extensionsv1alpha1.SandboxClaim{}
	claim2.Name = "multi-claim-2"
	claim2.Namespace = ns.Name
	claim2.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim2))

	// Wait for both claims to have sandbox names in status
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim1.Name, Namespace: ns.Name}, claim1); err != nil {
			return false
		}
		return claim1.Status.SandboxStatus.Name != ""
	}, 60*time.Second, 2*time.Second, "claim1 should get a sandbox name in status")

	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim2.Name, Namespace: ns.Name}, claim2); err != nil {
			return false
		}
		return claim2.Status.SandboxStatus.Name != ""
	}, 60*time.Second, 2*time.Second, "claim2 should get a sandbox name in status")

	// Build a set of reserved names for efficient lookup
	reservedSet := make(map[string]struct{}, len(reservedNames))
	for _, name := range reservedNames {
		reservedSet[name] = struct{}{}
	}

	// Verify both claims cold-started (their sandbox names differ from all reserved names)
	_, claim1UsedReserved := reservedSet[claim1.Status.SandboxStatus.Name]
	require.False(t, claim1UsedReserved,
		"claim1 should not adopt any reserved sandbox; got %s", claim1.Status.SandboxStatus.Name)

	_, claim2UsedReserved := reservedSet[claim2.Status.SandboxStatus.Name]
	require.False(t, claim2UsedReserved,
		"claim2 should not adopt any reserved sandbox; got %s", claim2.Status.SandboxStatus.Name)

	// Verify no reserved sandbox had its owner reference changed to a SandboxClaim
	for _, name := range reservedNames {
		sb := &sandboxv1alpha1.Sandbox{}
		require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: name, Namespace: ns.Name}, sb))
		require.True(t, metav1.IsControlledBy(sb, warmPool),
			"reserved sandbox %s should still be controlled by the warm pool", name)
	}
}
