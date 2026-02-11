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
	"context"
	"fmt"
	"hash/fnv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	control "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	templateVersionLabel = "template-version"
	testAnnotation       = "test-annotation"
	PoolLabel            = "agents.x-k8s.io/pool"
)

// NameHash generates an FNV-1a hash from a string and returns
// it as a fixed-length hexadecimal string.
func NameHash(objectName string) string {
	h := fnv.New32a()
	h.Write([]byte(objectName))
	hashValue := h.Sum32()
	return fmt.Sprintf("%08x", hashValue)
}

// TestWarmPoolTemplateUpdate verifies the current behavior of SandboxWarmPool when
// the underlying SandboxTemplate is updated. It confirms that:
// 1. Existing pods in the warm pool are NOT updated.
// 2. New SandboxClaims adopt the stale pods from the pool.
// 3. The adopted Sandbox spec shows the NEW template version, but the underlying pod is OLD.
// 4. When the WarmPool replenishes, the NEW pod uses the UPDATED template.
func TestWarmPoolTemplateUpdate(t *testing.T) {
	tc := framework.NewTestContext(t)
	ctx := context.Background()

	// Set up a namespace with unique name to avoid conflicts
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-upd-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(ctx, ns))
	t.Logf("Created namespace: %s", ns.Name)

	// Create a SandboxTemplate - V1
	v1Label := "v1"
	v1Annotation := "version-1"
	v1Command := "echo 'V1 Template Running'; sleep 3600"
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						templateVersionLabel: v1Label,
					},
					Annotations: map[string]string{
						testAnnotation: v1Annotation,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "main",
							Image:   "busybox",
							Command: []string{"sh", "-c", v1Command},
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(ctx, template))
	t.Logf("Created SandboxTemplate V1: %s", template.Name)

	// Create a SandboxWarmPool
	warmPool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-warmpool",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
			Replicas:    1,
		},
	}
	require.NoError(t, tc.CreateWithCleanup(ctx, warmPool))
	t.Logf("Created SandboxWarmPool: %s", warmPool.Name)

	// Wait for warm pool to create a pod and verify it's V1
	var initialPoolPod *corev1.Pod
	require.Eventually(t, func() bool {
		podList := &corev1.PodList{}
		if err := tc.List(ctx, podList, client.InNamespace(ns.Name), client.MatchingLabels{PoolLabel: NameHash(warmPool.Name)}); err != nil || len(podList.Items) == 0 {
			return false
		}
		for _, p := range podList.Items {
			if p.DeletionTimestamp.IsZero() && p.Status.Phase == corev1.PodRunning {
				initialPoolPod = &p
				return true
			}
		}
		return false
	}, 60*time.Second, 2*time.Second, "Waiting for initial WarmPool pod to be running")

	require.NotNil(t, initialPoolPod, "Failed to find a running pod in the warm pool")
	require.Equal(t, v1Label, initialPoolPod.Labels[templateVersionLabel], "Initial pool pod should have V1 label")
	require.Equal(t, v1Annotation, initialPoolPod.Annotations[testAnnotation], "Initial pool pod should have V1 annotation")
	t.Logf("Initial WarmPool pod %s is Running and is V1", initialPoolPod.Name)

	// Update the SandboxTemplate to V2
	v2Label := "v2"
	v2Annotation := "version-2"
	v2Command := "echo 'V2 Template Running'; sleep 3600"

	// Get the latest version of the template to avoid optimistic lock errors
	currentTemplate := &extensionsv1alpha1.SandboxTemplate{}
	require.NoError(t, tc.Get(ctx, types.NamespacedName{Name: template.Name, Namespace: ns.Name}, currentTemplate))
	template = currentTemplate

	template.Spec.PodTemplate.ObjectMeta.Labels[templateVersionLabel] = v2Label
	if template.Spec.PodTemplate.ObjectMeta.Annotations == nil {
		template.Spec.PodTemplate.ObjectMeta.Annotations = make(map[string]string)
	}
	template.Spec.PodTemplate.ObjectMeta.Annotations[testAnnotation] = v2Annotation
	template.Spec.PodTemplate.Spec.Containers[0].Command = []string{"sh", "-c", v2Command}
	require.NoError(t, tc.Update(ctx, template))
	t.Logf("Updated SandboxTemplate to V2: %s", template.Name)

	// Wait and verify the pool pod remains V1
	time.Sleep(15 * time.Second) // Give controllers time to potentially (but not expectedly) react
	latestInitialPod := &corev1.Pod{}
	require.NoError(t, tc.Get(ctx, types.NamespacedName{Name: initialPoolPod.Name, Namespace: ns.Name}, latestInitialPod))
	require.Equal(t, v1Label, latestInitialPod.Labels[templateVersionLabel], "Initial pool pod should STILL have V1 label")
	require.Equal(t, v1Annotation, latestInitialPod.Annotations[testAnnotation], "Initial pool pod should STILL have V1 annotation")
	t.Logf("Initial WarmPool pod %s remains V1 after template update", latestInitialPod.Name)

	// Create a SandboxClaim
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(ctx, claim))
	t.Logf("Created SandboxClaim: %s", claim.Name)

	// Wait for claim to be Ready and get the Sandbox
	var sandbox *sandboxv1alpha1.Sandbox
	require.Eventually(t, func() bool {
		if err := tc.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: ns.Name}, claim); err != nil {
			return false
		}
		if claim.Status.SandboxStatus.Name == "" {
			return false
		}
		sandbox = &sandboxv1alpha1.Sandbox{}
		return tc.Get(ctx, types.NamespacedName{Name: claim.Status.SandboxStatus.Name, Namespace: ns.Name}, sandbox) == nil
	}, 60*time.Second, 2*time.Second, "Waiting for SandboxClaim to be Ready and Sandbox to exist")

	// Verify Sandbox Spec is V2
	require.Equal(t, v2Label, sandbox.Spec.PodTemplate.ObjectMeta.Labels[templateVersionLabel], "Sandbox spec should show V2 label")
	require.Equal(t, v2Annotation, sandbox.Spec.PodTemplate.ObjectMeta.Annotations[testAnnotation], "Sandbox spec should show V2 annotation")
	t.Logf("Sandbox %s spec reflects V2", sandbox.Name)

	// Verify the Adopted Pod is the one from the pool and is still V1
	adoptedPodName := sandbox.Annotations[control.SandboxPodNameAnnotation]
	require.Equal(t, initialPoolPod.Name, adoptedPodName, "Sandbox should adopt the pod from the warm pool")
	adoptedPod := &corev1.Pod{}
	require.NoError(t, tc.Get(ctx, types.NamespacedName{Name: adoptedPodName, Namespace: ns.Name}, adoptedPod))
	require.Equal(t, v1Label, adoptedPod.Labels[templateVersionLabel], "Adopted pod should STILL have V1 label")
	require.Equal(t, v1Annotation, adoptedPod.Annotations[testAnnotation], "Adopted pod should STILL have V1 annotation")
	t.Logf("Adopted pod %s for Sandbox %s is still V1", adoptedPod.Name, sandbox.Name)

	// Verify WarmPool Replenishment Pod is V2
	var replenishmentPod *corev1.Pod
	require.Eventually(t, func() bool {
		podList := &corev1.PodList{}
		if err := tc.List(ctx, podList, client.InNamespace(ns.Name), client.MatchingLabels{PoolLabel: NameHash(warmPool.Name)}); err != nil {
			return false
		}
		if len(podList.Items) == 0 {
			return false
		}
		// Find the new pod, which is not the initially adopted one
		found := false
		for _, p := range podList.Items {
			if p.Name != adoptedPodName && p.DeletionTimestamp.IsZero() && p.Status.Phase == corev1.PodRunning {
				replenishmentPod = &p
				found = true
				break
			}
		}
		return found
	}, 60*time.Second, 5*time.Second, "Waiting for WarmPool to replenish with a new pod")

	require.NotNil(t, replenishmentPod, "Should have found a replenishment pod")
	require.Equal(t, v2Label, replenishmentPod.Labels[templateVersionLabel], "Replenishment pool pod should have V2 label")
	require.Equal(t, v2Annotation, replenishmentPod.Annotations[testAnnotation], "Replenishment pool pod should have V2 annotation")
	t.Logf("WarmPool replenished with new pod %s, which is V2", replenishmentPod.Name)
}
