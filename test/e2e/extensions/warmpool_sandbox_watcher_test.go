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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newWarmPoolTemplate(namespace string) *extensionsv1beta1.SandboxTemplate {
	template := &extensionsv1beta1.SandboxTemplate{}
	template.Name = "test-template"
	template.Namespace = namespace
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
		Spec: &corev1.PodSpec{
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

func waitForWarmPoolSandboxReady(t *testing.T, tc *framework.TestContext, namespace string, warmPool *extensionsv1beta1.SandboxWarmPool) {
	t.Helper()

	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1beta1.SandboxList{}
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

func waitForClaimReady(t *testing.T, tc *framework.TestContext, claim *extensionsv1beta1.SandboxClaim) {
	t.Helper()

	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, claim); err != nil {
			return false
		}
		return claim.Status.SandboxStatus.Name != "" && isClaimReady(claim)
	}, 30*time.Second, 1*time.Second, "claim should become ready")
}

func isSandboxReady(sb *sandboxv1beta1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(sandboxv1beta1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func isClaimReady(claim *extensionsv1beta1.SandboxClaim) bool {
	for _, cond := range claim.Status.Conditions {
		if cond.Type == string(sandboxv1beta1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func TestWarmPoolSandboxWatcher(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-watcher-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := newWarmPoolTemplate(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// Create a SandboxWarmPool
	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	replicas := int32(1)
	warmPool.Spec.Replicas = &replicas
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Wait for warm pool Sandbox to become ready
	waitForWarmPoolSandboxReady(t, tc, ns.Name, warmPool)

	// Create a SandboxClaim to adopt the warm pool sandbox
	claim := &extensionsv1beta1.SandboxClaim{}
	claim.Name = "test-claim"
	claim.Namespace = ns.Name
	claim.Spec.WarmPoolRef.Name = warmPool.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for claim to be ready with sandbox name in status
	waitForClaimReady(t, tc, claim)

	// Verify the adopted sandbox is now owned by the claim
	adoptedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{
		Name:      claim.Status.SandboxStatus.Name,
		Namespace: ns.Name,
	}, adoptedSandbox))
	require.True(t, metav1.IsControlledBy(adoptedSandbox, claim), "adopted sandbox should be controlled by claim")

	adoptedPod := &corev1.Pod{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: adoptedSandbox.Name, Namespace: ns.Name}, adoptedPod))

	// Wait for the sandbox controller to finish adopting the warm pool pod.
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{
			Name:      adoptedSandbox.Name,
			Namespace: ns.Name,
		}, adoptedSandbox); err != nil {
			return false
		}

		if err := tc.Get(t.Context(), types.NamespacedName{Name: adoptedSandbox.Name, Namespace: ns.Name}, adoptedPod); err != nil {
			return false
		}

		if !metav1.IsControlledBy(adoptedPod, adoptedSandbox) {
			return false
		}

		_, hasSandboxLabel := adoptedPod.Labels["agents.x-k8s.io/sandbox-name-hash"]
		return hasSandboxLabel
	}, 30*time.Second, 500*time.Millisecond, "sandbox controller should adopt the pod before deletion")

	// Delete the pod and verify sandbox status updates
	require.NoError(t, tc.Delete(t.Context(), adoptedPod))

	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), types.NamespacedName{Name: adoptedSandbox.Name, Namespace: ns.Name}, adoptedSandbox); err != nil {
			return false
		}
		for _, cond := range adoptedSandbox.Status.Conditions {
			if cond.Type == string(sandboxv1beta1.SandboxConditionReady) && cond.Status != metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond, "sandbox should become not-ready after pod deletion")
}
