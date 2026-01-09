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

// TestWarmPoolSandboxWatcher verifies that Sandboxes created from WarmPools watch the right underlying pods.
func TestWarmPoolSandboxWatcher(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace with unique name to avoid conflicts
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

	// Wait for warm pool to create a pod
	var poolPodName string
	require.Eventually(t, func() bool {
		podList := &corev1.PodList{}
		if err := tc.List(t.Context(), podList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, pod := range podList.Items {
			if _, hasLabel := pod.Labels["agents.x-k8s.io/pool"]; hasLabel && pod.DeletionTimestamp.IsZero() {
				poolPodName = pod.Name
				return true
			}
		}
		return false
	}, 60*time.Second, 2*time.Second)

	// Create a SandboxClaim to adopt the pod
	claim := &extensionsv1alpha1.SandboxClaim{}
	claim.Name = "test-claim"
	claim.Namespace = ns.Name
	claim.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for claim to create sandbox
	var sandbox *sandboxv1alpha1.Sandbox
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: ns.Name}, claim); err != nil {
			return false
		}
		if claim.Status.SandboxStatus.Name == "" {
			return false
		}
		sandbox = &sandboxv1alpha1.Sandbox{}
		return tc.Get(t.Context(), types.NamespacedName{Name: claim.Status.SandboxStatus.Name, Namespace: ns.Name}, sandbox) == nil
	}, 30*time.Second, 1*time.Second)

	// Wait for pod to be adopted by sandbox
	var adoptedPod *corev1.Pod
	require.Eventually(t, func() bool {
		adoptedPod = &corev1.Pod{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: poolPodName, Namespace: ns.Name}, adoptedPod); err != nil {
			return false
		}
		return metav1.IsControlledBy(adoptedPod, sandbox)
	}, 30*time.Second, 1*time.Second)

	// Wait for sandbox to become ready
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: ns.Name}, sandbox); err != nil {
			return false
		}
		for _, cond := range sandbox.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 30*time.Second, 1*time.Second)

	// Delete the pod
	require.NoError(t, tc.Delete(t.Context(), adoptedPod))

	// Verify sandbox status updates to reflect pod deletion.
	// NOTE: This is the critical step that verifies that the Sandbox is watching the right pod.
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: ns.Name}, sandbox); err != nil {
			return false
		}
		for _, cond := range sandbox.Status.Conditions {
			if cond.Type == "Ready" && cond.Status != metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond)
}
