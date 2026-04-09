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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func createSandboxTemplate(_ *framework.TestContext, ns *corev1.Namespace, name string) *extensionsv1alpha1.SandboxTemplate {
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = name
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

func createSandboxWarmPool(_ *framework.TestContext, ns *corev1.Namespace, template *extensionsv1alpha1.SandboxTemplate, updateStrategy *extensionsv1alpha1.SandboxWarmPoolUpdateStrategy) *extensionsv1alpha1.SandboxWarmPool {
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	warmPool.Spec.UpdateStrategy = updateStrategy
	return warmPool
}

func updateSandboxTemplateSpec(_ *framework.TestContext, template *extensionsv1alpha1.SandboxTemplate) {
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

// Test basic rollout strategy for warmpool - default, onReplenish, recreate
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
			verify: verifySandboxStaysSame,
		},
		{
			name: "recreate",
			strategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
				Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
			},
			verify: verifySandboxRecreated,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := framework.NewTestContext(t)

			ns := &corev1.Namespace{}
			ns.Name = fmt.Sprintf("warmpool-rollout-%s-%d", c.name, time.Now().UnixNano())
			require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

			// Create a SandboxTemplate
			template := createSandboxTemplate(tc, ns, "test-template")
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

// Test that multiple warmpools with "recreate" strategy and different templates are isolated from each other, i.e,
// updating one template affects only the warmpool associated with that template.
func TestWarmPoolRolloutMultiTemplateIsolation(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-isolation-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create two SandboxTemplates
	templateA := createSandboxTemplate(tc, ns, "template-a")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateA))

	templateB := createSandboxTemplate(tc, ns, "template-b")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateB))

	// Create two SandboxWarmPools, each pointing to a different template
	warmPoolA := createSandboxWarmPool(tc, ns, templateA, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	warmPoolA.Name = "warmpool-a"
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPoolA))

	warmPoolB := createSandboxWarmPool(tc, ns, templateB, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	warmPoolB.Name = "warmpool-b"
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPoolB))

	// Wait for both warm pools to be ready
	idA := types.NamespacedName{Namespace: ns.Name, Name: warmPoolA.Name}
	idB := types.NamespacedName{Namespace: ns.Name, Name: warmPoolB.Name}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), idA))
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), idB))

	// Get sandbox names for both
	sbList := &sandboxv1alpha1.SandboxList{}
	require.NoError(t, tc.List(t.Context(), sbList, client.InNamespace(ns.Name)))

	var sbNameA, sbNameB string
	for _, sb := range sbList.Items {
		if sb.DeletionTimestamp.IsZero() {
			if metav1.IsControlledBy(&sb, warmPoolA) {
				sbNameA = sb.Name
			} else if metav1.IsControlledBy(&sb, warmPoolB) {
				sbNameB = sb.Name
			}
		}
	}
	require.NotEmpty(t, sbNameA, "expected to find sandbox for warmpool A")
	require.NotEmpty(t, sbNameB, "expected to find sandbox for warmpool B")

	// Update Template A
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: templateA.Name, Namespace: templateA.Namespace}, templateA))
	updateSandboxTemplateSpec(tc, templateA)
	require.NoError(t, tc.Update(t.Context(), templateA))

	// Verify WarmPool A's sandbox is recreated
	verifySandboxRecreated(t, tc, ns, sbNameA, idA)

	// Verify WarmPool B's sandbox stays the same (same name, not deleted)
	sb := &sandboxv1alpha1.Sandbox{}
	err := tc.Get(t.Context(), types.NamespacedName{Name: sbNameB, Namespace: ns.Name}, sb)
	require.NoError(t, err, "Sandbox B should still exist")
	require.True(t, sb.DeletionTimestamp.IsZero(), "Sandbox B should not be marked for deletion")
}

// Test updating warmpool to point to a different template with the same spec
func TestWarmPoolRolloutSwitchTemplate(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-switch-template-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create two SandboxTemplates with identical specs but different names
	templateA := createSandboxTemplate(tc, ns, "template-a")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateA))

	templateB := createSandboxTemplate(tc, ns, "template-b")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateB))

	// Create a SandboxWarmPool pointing to Template A
	warmPool := createSandboxWarmPool(tc, ns, templateA, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	sandboxWarmpoolID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      warmPool.Name,
	}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))

	// Get the sandbox name
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

	// Update WarmPool to point to Template B
	require.NoError(t, tc.Get(t.Context(), sandboxWarmpoolID, warmPool))
	warmPool.Spec.TemplateRef.Name = "template-b"
	require.NoError(t, tc.Update(t.Context(), warmPool))

	// Since the strategy is Recreate, it should recreate the sandbox even if the spec is identical,
	// because the template reference changed.
	// Wait for the old sandbox to be deleted or marked for deletion.
	verifySandboxRecreated(t, tc, ns, poolSandboxName, sandboxWarmpoolID)
}

// Test that metadata updates to the template does not trigger a rollout
func TestWarmPoolRolloutMetadataUpdate(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-metadata-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate with initial labels in pod template
	template := createSandboxTemplate(tc, ns, "test-template")
	template.Spec.PodTemplate.ObjectMeta = sandboxv1alpha1.PodMetadata{
		Labels: map[string]string{"initial-label": "value"},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// Create a SandboxWarmPool with strategy Recreate
	warmPool := createSandboxWarmPool(tc, ns, template, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	sandboxWarmpoolID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      warmPool.Name,
	}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))

	// Get the initial sandbox name
	sandboxList := &sandboxv1alpha1.SandboxList{}
	require.NoError(t, tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)))
	var initialSandboxName string
	for _, sb := range sandboxList.Items {
		if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
			initialSandboxName = sb.Name
			break
		}
	}
	require.NotEmpty(t, initialSandboxName, "expected to find a pool sandbox")

	// Update the labels in the template's pod template metadata
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: template.Name, Namespace: template.Namespace}, template))
	template.Spec.PodTemplate.ObjectMeta.Labels["new-label"] = "new-value"
	require.NoError(t, tc.Update(t.Context(), template))

	// Verify that no rollout occurs (sandbox remains the same)
	// Wait a bit to be sure no deletion happens
	time.Sleep(5 * time.Second) // Wait for potential reconciliation

	sb := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: initialSandboxName, Namespace: ns.Name}, sb))
	require.True(t, sb.DeletionTimestamp.IsZero(), "Sandbox should not be marked for deletion")
}
