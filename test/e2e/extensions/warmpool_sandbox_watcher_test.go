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

func TestWarmPoolSandboxWatcher(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-watcher-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "test-template"
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

	// Create a SandboxWarmPool
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Wait for warm pool Sandbox to become ready
	var poolSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, sb := range sandboxList.Items {
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
				for _, cond := range sb.Status.Conditions {
					if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
						poolSandboxName = sb.Name
						return true
					}
				}
			}
		}
		return false
	}, 60*time.Second, 2*time.Second, "warm pool sandbox should become ready")

	// Find the pod name from the pool sandbox
	poolSandbox := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, poolSandbox))

	// Create a SandboxClaim to adopt the warm pool sandbox
	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = "test-claim"
	claim.Namespace = ns.Name
	claim.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for claim to be ready with sandbox name in status
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: ns.Name}, claim); err != nil {
			return false
		}
		if claim.Status.SandboxStatus.Name == "" {
			return false
		}
		for _, cond := range claim.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 30*time.Second, 1*time.Second, "claim should become ready")

	// Verify the adopted sandbox is now owned by the claim
	adoptedSandbox := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{
		Name:      claim.Status.SandboxStatus.Name,
		Namespace: ns.Name,
	}, adoptedSandbox))
	require.True(t, metav1.IsControlledBy(adoptedSandbox, claim), "adopted sandbox should be controlled by claim")

	// Find the pod belonging to the adopted sandbox
	podName := adoptedSandbox.Name
	if ann, ok := adoptedSandbox.Annotations["agents.x-k8s.io/pod-name"]; ok && ann != "" {
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
			if cond.Type == "Ready" && cond.Status != metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond, "sandbox should become not-ready after pod deletion")
}
