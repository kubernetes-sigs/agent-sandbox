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

const (
	defaultTestTimeout     = 30 * time.Second
	defaultPollingInterval = 1 * time.Second
)

func createSandboxTemplate(ns *corev1.Namespace, name string) *extensionsv1alpha1.SandboxTemplate {
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

func createSandboxWarmPool(ns *corev1.Namespace, template *extensionsv1alpha1.SandboxTemplate, updateStrategy *extensionsv1alpha1.SandboxWarmPoolUpdateStrategy) *extensionsv1alpha1.SandboxWarmPool {
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = 1
	warmPool.Spec.UpdateStrategy = updateStrategy
	return warmPool
}

func updateSandboxTemplateSpec(template *extensionsv1alpha1.SandboxTemplate) {
	template.Spec.PodTemplate.Spec.Containers[0].Env = append(template.Spec.PodTemplate.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  "TEST_ENV",
		Value: "updated",
	})
}

func verifySandboxStaysSame(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName) {
	// Wait a bit to be sure no deletion happens (controller processes updates asynchronously)
	time.Sleep(5 * time.Second)

	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))
	sb := &sandboxv1alpha1.Sandbox{}
	err := tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, sb)
	require.NoError(t, err, "Sandbox should still exist")
	require.True(t, sb.DeletionTimestamp.IsZero(), "Sandbox should not be marked for deletion")
}

func verifySandboxRecreated(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName, expectUpdate bool) {
	require.Eventually(t, func() bool {
		sb := &sandboxv1alpha1.Sandbox{}
		err := tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, sb)
		if k8serrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			t.Logf("Failed to get sandbox: %v", err)
			return false
		}
		return !sb.DeletionTimestamp.IsZero()
	}, defaultTestTimeout, defaultPollingInterval, "old sandbox should be deleted or marked for deletion")

	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	require.NoError(t, tc.Get(t.Context(), sandboxWarmpoolID, warmPool))

	if expectUpdate {
		verifySandboxHasUpdatedSpec(t, tc, ns, poolSandboxName, warmPool)
	}

	// Wait for the warm pool to be ready again
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))
}

func verifySandboxHasUpdatedSpec(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, excludeSandboxName string, warmPool *extensionsv1alpha1.SandboxWarmPool) {
	var newSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, s := range sandboxList.Items {
			if s.DeletionTimestamp.IsZero() && s.Name != excludeSandboxName && metav1.IsControlledBy(&s, warmPool) {
				newSandboxName = s.Name
				return true
			}
		}
		return false
	}, defaultTestTimeout, defaultPollingInterval, "expected to find a new pool sandbox")

	newSb := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: newSandboxName, Namespace: ns.Name}, newSb))

	require.NotEmpty(t, newSb.Spec.PodTemplate.Spec.Containers, "Sandbox should have containers")
	found := false
	for _, env := range newSb.Spec.PodTemplate.Spec.Containers[0].Env {
		if env.Name == "TEST_ENV" && env.Value == "updated" {
			found = true
			break
		}
	}
	require.True(t, found, "New sandbox should have the updated spec (env var TEST_ENV=updated)")
}

func verifyOnReplenishLifecycle(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName) {
	// Verify old sandbox stays same initially
	verifySandboxStaysSame(t, tc, ns, poolSandboxName, sandboxWarmpoolID)

	// Delete the old sandbox to trigger replenishment
	sb := &sandboxv1alpha1.Sandbox{}
	err := tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, sb)
	require.NoError(t, err, "Sandbox should still exist")
	require.NoError(t, tc.Delete(t.Context(), sb), "Failed to delete sandbox for replenishment")

	// Wait for the old sandbox to be gone
	require.Eventually(t, func() bool {
		err := tc.Get(t.Context(), types.NamespacedName{Name: poolSandboxName, Namespace: ns.Name}, &sandboxv1alpha1.Sandbox{})
		return k8serrors.IsNotFound(err)
	}, defaultTestTimeout, defaultPollingInterval, "old sandbox should be deleted")

	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	require.NoError(t, tc.Get(t.Context(), sandboxWarmpoolID, warmPool))

	verifySandboxHasUpdatedSpec(t, tc, ns, poolSandboxName, warmPool)

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
			verify:   verifyOnReplenishLifecycle,
		},
		{
			name: "onreplenish",
			strategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
				Type: extensionsv1alpha1.OnReplenishSandboxWarmPoolUpdateStrategyType,
			},
			verify: verifyOnReplenishLifecycle,
		},
		{
			name: "recreate",
			strategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
				Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
			},
			verify: func(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName) {
				verifySandboxRecreated(t, tc, ns, poolSandboxName, sandboxWarmpoolID, true)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := framework.NewTestContext(t)

			ns := &corev1.Namespace{}
			ns.Name = fmt.Sprintf("warmpool-rollout-%s-%d", c.name, time.Now().UnixNano())
			require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

			// Create a SandboxTemplate
			template := createSandboxTemplate(ns, "test-template")
			require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

			// Create a SandboxWarmPool
			warmPool := createSandboxWarmPool(ns, template, c.strategy)
			require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

			sandboxWarmpoolID := types.NamespacedName{
				Namespace: ns.Name,
				Name:      warmPool.Name,
			}
			require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))

			// Get the pool sandbox name
			var poolSandboxName string
			require.Eventually(t, func() bool {
				sandboxList := &sandboxv1alpha1.SandboxList{}
				if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
					return false
				}
				for _, sb := range sandboxList.Items {
					if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
						poolSandboxName = sb.Name
						return true
					}
				}
				return false
			}, 10*time.Second, defaultPollingInterval, "expected to find a pool sandbox")

			// Update the SandboxTemplate by adding an environment variable
			require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: template.Name, Namespace: template.Namespace}, template))
			updateSandboxTemplateSpec(template)
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
	templateA := createSandboxTemplate(ns, "template-a")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateA))

	templateB := createSandboxTemplate(ns, "template-b")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateB))

	// Create two SandboxWarmPools, each pointing to a different template
	warmPoolA := createSandboxWarmPool(ns, templateA, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	warmPoolA.Name = "warmpool-a"
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPoolA))

	warmPoolB := createSandboxWarmPool(ns, templateB, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
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
	var sbNameA, sbNameB string
	require.Eventually(t, func() bool {
		sbList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sbList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		sbNameA, sbNameB = "", "" // Reset in case of retry
		for _, sb := range sbList.Items {
			if sb.DeletionTimestamp.IsZero() {
				if metav1.IsControlledBy(&sb, warmPoolA) {
					sbNameA = sb.Name
				} else if metav1.IsControlledBy(&sb, warmPoolB) {
					sbNameB = sb.Name
				}
			}
		}
		return sbNameA != "" && sbNameB != ""
	}, defaultTestTimeout, defaultPollingInterval, "expected to find sandboxes for both warm pools")

	// Update Template A
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: templateA.Name, Namespace: templateA.Namespace}, templateA))
	updateSandboxTemplateSpec(templateA)
	require.NoError(t, tc.Update(t.Context(), templateA))

	// Verify WarmPool A's sandbox is recreated
	verifySandboxRecreated(t, tc, ns, sbNameA, idA, true)

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
	templateA := createSandboxTemplate(ns, "template-a")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateA))

	templateB := createSandboxTemplate(ns, "template-b")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), templateB))

	// Create a SandboxWarmPool pointing to Template A
	warmPool := createSandboxWarmPool(ns, templateA, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	sandboxWarmpoolID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      warmPool.Name,
	}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))

	// Get the sandbox name
	var poolSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, sb := range sandboxList.Items {
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
				poolSandboxName = sb.Name
				return true
			}
		}
		return false
	}, defaultTestTimeout, defaultPollingInterval, "expected to find a pool sandbox")

	// Update WarmPool to point to Template B
	require.NoError(t, tc.Get(t.Context(), sandboxWarmpoolID, warmPool))
	warmPool.Spec.TemplateRef.Name = templateB.Name
	require.NoError(t, tc.Update(t.Context(), warmPool))

	// Since the strategy is Recreate, it should recreate the sandbox even if the spec is identical,
	// because the template reference changed.
	// Wait for the old sandbox to be deleted or marked for deletion.
	verifySandboxRecreated(t, tc, ns, poolSandboxName, sandboxWarmpoolID, false)

	// Verify the new sandbox has the updated template ref annotation
	var newSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, s := range sandboxList.Items {
			if s.DeletionTimestamp.IsZero() && s.Name != poolSandboxName {
				newSandboxName = s.Name
				return true
			}
		}
		return false
	}, defaultTestTimeout, defaultPollingInterval, "expected to find a new pool sandbox")

	newSb := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: newSandboxName, Namespace: ns.Name}, newSb))

	require.Equal(t, templateB.Name, newSb.Annotations[sandboxv1alpha1.SandboxTemplateRefAnnotation], "Sandbox should use the new template name")
}

// Test that metadata updates to the template does not trigger a rollout
func TestWarmPoolRolloutMetadataUpdate(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-metadata-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate with initial labels in pod template
	template := createSandboxTemplate(ns, "test-template")
	template.Spec.PodTemplate.ObjectMeta = sandboxv1alpha1.PodMetadata{
		Labels: map[string]string{"initial-label": "value"},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	// Create a SandboxWarmPool with strategy Recreate
	warmPool := createSandboxWarmPool(ns, template, &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	sandboxWarmpoolID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      warmPool.Name,
	}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))

	// Get the initial sandbox name
	var initialSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1alpha1.SandboxList{}
		if err := tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)); err != nil {
			return false
		}
		for _, sb := range sandboxList.Items {
			if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
				initialSandboxName = sb.Name
				return true
			}
		}
		return false
	}, defaultTestTimeout, defaultPollingInterval, "expected to find a pool sandbox")

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
