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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newWarmPoolTemplate(namespace string) *extensionsv1alpha1.SandboxTemplate {
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "test-template"
	template.Namespace = namespace
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
	return template
}

func waitForWarmPoolSandboxReady(t *testing.T, tc *framework.TestContext, namespace string, warmPool *extensionsv1alpha1.SandboxWarmPool) {
	t.Helper()

	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(namespace)); err != nil {
			return false
		}
		for _, sb := range sandboxList.Items {
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) && isSandboxReady(&sb) {
				return true
			}
		}
		return false
	}, 60*time.Second, 2*time.Second, "warm pool sandbox should become ready")
}

func waitForClaimReady(t *testing.T, tc *framework.TestContext, claim *extensionsv1alpha1.SandboxClaim) {
	t.Helper()

	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, claim); err != nil {
			return false
		}
		return claim.Status.SandboxStatus.Name != "" && isClaimReady(claim)
	}, 30*time.Second, 1*time.Second, "claim should become ready")
}

func isSandboxReady(sb *sandboxv1alpha1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func isClaimReady(claim *extensionsv1alpha1.SandboxClaim) bool {
	for _, cond := range claim.Status.Conditions {
		if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func requirePodNameAnnotationWhenReady(
	t *testing.T,
	tc *framework.TestContext,
	namespace string,
	claim *extensionsv1alpha1.SandboxClaim,
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Use a direct API watch to avoid the async subscription race in framework.Watch
	sandboxWatcher, err := tc.DynamicClient().Resource(
		sandboxv1alpha1.GroupVersion.WithResource("sandboxes"),
	).Namespace(namespace).Watch(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	defer sandboxWatcher.Stop()

	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out watching adopted sandbox readiness: %v", ctx.Err())
		case event, ok := <-sandboxWatcher.ResultChan():
			require.True(t, ok, "sandbox watch closed before observing adopted sandbox readiness")
			require.NotEqual(t, watch.Error, event.Type, "received error event while watching sandboxes")

			if event.Type == watch.Deleted {
				continue
			}

			u, ok := event.Object.(*unstructured.Unstructured)
			require.True(t, ok, "unexpected sandbox watch event object type: %T", event.Object)

			sb := &sandboxv1alpha1.Sandbox{}
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, sb))

			controllerRef := metav1.GetControllerOf(sb)
			if controllerRef == nil || controllerRef.Kind != "SandboxClaim" || controllerRef.Name != claim.Name {
				continue
			}
			if !isSandboxReady(sb) {
				continue
			}
			if sb.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation] == "" {
				t.Fatalf("observed adopted sandbox %s Ready=True without %s annotation", sb.Name, sandboxv1alpha1.SandboxPodNameAnnotation)
			}
			return
		}
	}
}

func TestWarmPoolSandboxWatcher(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-watcher-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := newWarmPoolTemplate(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// Create a SandboxWarmPool
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Wait for warm pool Sandbox to become ready
	waitForWarmPoolSandboxReady(t, tc, ns.Name, warmPool)

	// Create a SandboxClaim to adopt the warm pool sandbox
	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = "test-claim"
	claim.Namespace = ns.Name
	claim.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for claim to be ready with sandbox name in status
	waitForClaimReady(t, tc, claim)

	// Verify the adopted sandbox is now owned by the claim
	adoptedSandbox := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{
		Name:      claim.Status.SandboxStatus.Name,
		Namespace: ns.Name,
	}, adoptedSandbox))
	require.True(t, metav1.IsControlledBy(adoptedSandbox, claim), "adopted sandbox should be controlled by claim")

	// Find the pod belonging to the adopted sandbox
	podName := adoptedSandbox.Name
	if ann, ok := adoptedSandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; ok && ann != "" {
		podName = ann
	}
	adoptedPod := &corev1.Pod{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: podName, Namespace: ns.Name}, adoptedPod))

	// Delete the pod and verify sandbox status updates
	require.NoError(t, tc.Delete(t.Context(), adoptedPod))

	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: adoptedSandbox.Name, Namespace: ns.Name}, adoptedSandbox); err != nil {
			return false
		}
		for _, cond := range adoptedSandbox.Status.Conditions {
			if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status != metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond, "sandbox should become not-ready after pod deletion")
}

// TestWarmPoolPodNameAnnotationBeforeReady verifies that a warm-pool sandbox
// records its pod-name annotation before adoption can be observed as Ready.
func TestWarmPoolPodNameAnnotationBeforeReady(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-ready-annotation-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := newWarmPoolTemplate(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Start from a Ready warm-pool Sandbox so the claim reconcile path must adopt it
	waitForWarmPoolSandboxReady(t, tc, ns.Name, warmPool)

	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = "test-claim"
	claim.Namespace = ns.Name
	claim.Spec.TemplateRef.Name = template.Name

	// Creating the claim should not observe Ready before the pod-name annotation is set
	requirePodNameAnnotationWhenReady(t, tc, ns.Name, claim)
}
