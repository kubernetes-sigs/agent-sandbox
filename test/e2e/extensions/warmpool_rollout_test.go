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

package extensions

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func createSandboxTemplate(tc *framework.TestContext, ns *corev1.Namespace) *extensionsv1alpha1.SandboxTemplate {
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
	return template
}

func createSandboxWarmPool(tc *framework.TestContext, ns *corev1.Namespace, template *extensionsv1alpha1.SandboxTemplate, updateStrategy *extensionsv1alpha1.SandboxWarmPoolUpdateStrategy) *extensionsv1alpha1.SandboxWarmPool {
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	warmPool.Spec.UpdateStrategy = updateStrategy
	return warmPool
}

func updateSandboxTemplateSpec(tc *framework.TestContext, template *extensionsv1alpha1.SandboxTemplate) {
	template.Spec.PodTemplate.Spec.Containers[0].Env = append(template.Spec.PodTemplate.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  "TEST_ENV",
		Value: "updated",
	})
}

func verifySandboxStaysSame(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName) {
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))
	sb := &sandboxv1alpha1.Sandbox{}
	err := tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, sb)
	require.NoError(t, err, "Sandbox should still exist")
	require.True(t, sb.DeletionTimestamp.IsZero(), "Sandbox should not be marked for deletion")
}

func verifySandboxRecreated(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName) {
	require.Eventually(t, func() bool {
		sb := &sandboxv1alpha1.Sandbox{}
		err := tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, sb)
		if k8serrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			return false
		}
		return !sb.DeletionTimestamp.IsZero()
	}, 30*time.Second, 1*time.Second, "old sandbox should be deleted or marked for deletion")

	// Wait for the warm pool to be ready again
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))
}

func TestWarmPoolRollout(t *testing.T) {
	cases := []struct {
		name     string
		strategy *extensionsv1alpha1.SandboxWarmPoolUpdateStrategy
		verify   func(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName)
	}{
		{
			name:     "default",
			strategy: nil,
			verify:   verifySandboxStaysSame,
		},
		{
			name: "onreplenish",
			strategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
				Type: extensionsv1alpha1.OnReplenishSandboxWarmPoolUpdateStrategyType,
			},
			verify:   verifySandboxStaysSame,
		},
		{
			name: "recreate",
			strategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
				Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
			},
			verify:   verifySandboxRecreated,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := framework.NewTestContext(t)

			ns := &corev1.Namespace{}
			ns.Name = fmt.Sprintf("warmpool-rollout-%s-%d", c.name, time.Now().UnixNano())
			require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

			// Create a SandboxTemplate
			template := createSandboxTemplate(tc, ns)
			require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

			// Create a SandboxWarmPool
			warmPool := createSandboxWarmPool(tc, ns, template, c.strategy)
			require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

			sandboxWarmpoolID := types.NamespacedName{
				Namespace: ns.Name,
				Name:      warmPool.Name,
			}
			require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))

			// Get the pool sandbox name
			sandboxList := &sandboxv1alpha1.SandboxList{}
			require.NoError(t, tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)))
			var poolSandboxName string
			for _, sb := range sandboxList.Items {
				if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
					poolSandboxName = sb.Name
					break
				}
			}
			require.NotEmpty(t, poolSandboxName, "expected to find a pool sandbox")

			// Update the SandboxTemplate by adding an environment variable
			require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: template.Name, Namespace: template.Namespace}, template))
			updateSandboxTemplateSpec(tc, template)
			require.NoError(t, tc.Update(t.Context(), template))

			// Verify the SandboxWarmPool rollout
			c.verify(t, tc, ns, poolSandboxName, sandboxWarmpoolID)
		})
	}
}
