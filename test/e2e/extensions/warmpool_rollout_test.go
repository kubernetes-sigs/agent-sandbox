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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultTestTimeout     = 30 * time.Second
	defaultPollingInterval = 1 * time.Second
)

func createSandboxTemplate(ns *corev1.Namespace, name string) *extensionsv1beta1.SandboxTemplate {
	template := &extensionsv1beta1.SandboxTemplate{}
	template.Name = name
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
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

func createSandboxWarmPool(ns *corev1.Namespace, template *extensionsv1beta1.SandboxTemplate, updateStrategy *extensionsv1beta1.SandboxWarmPoolUpdateStrategy) *extensionsv1beta1.SandboxWarmPool {
	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	replicas := int32(1)
	warmPool.Spec.Replicas = &replicas
	warmPool.Spec.UpdateStrategy = updateStrategy
	return warmPool
}

func updateSandboxTemplateSpec(template *extensionsv1beta1.SandboxTemplate) {
	template.Spec.PodTemplate.Spec.Containers[0].Env = append(template.Spec.PodTemplate.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  "TEST_ENV",
		Value: "updated",
	})
}

func verifySandboxRecreated(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, poolSandboxName string, sandboxWarmpoolID types.NamespacedName) {
	require.Eventually(t, func() bool {
		sb := &sandboxv1beta1.Sandbox{}
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

	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	require.NoError(t, tc.Get(t.Context(), sandboxWarmpoolID, warmPool))

	verifySandboxHasUpdatedSpec(t, tc, ns, poolSandboxName, warmPool)

	// Wait for the warm pool to be ready again
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))
}

func verifySandboxHasUpdatedSpec(t *testing.T, tc *framework.TestContext, ns *corev1.Namespace, excludeSandboxName string, warmPool *extensionsv1beta1.SandboxWarmPool) {
	var newSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1beta1.SandboxList{}
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

	newSb := &sandboxv1beta1.Sandbox{}
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

// TestWarmPoolRollout checks that a template update reaches a real Recreate
// pool through the watch. The default and OnReplenish strategies, template ref
// switches, metadata-only drift, and isolation between pools are covered by
// unit tests in sandboxwarmpool_controller_test.go.
func TestWarmPoolRollout(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-rollout-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := createSandboxTemplate(ns, "test-template")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	warmPool := createSandboxWarmPool(ns, template, &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{
		Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType,
	})
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	sandboxWarmpoolID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      warmPool.Name,
	}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))

	var poolSandboxName string
	require.Eventually(t, func() bool {
		sandboxList := &sandboxv1beta1.SandboxList{}
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

	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: template.Name, Namespace: template.Namespace}, template))
	updateSandboxTemplateSpec(template)
	require.NoError(t, tc.Update(t.Context(), template))

	verifySandboxRecreated(t, tc, ns, poolSandboxName, sandboxWarmpoolID)
}
