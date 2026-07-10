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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

type expectedVCT struct {
	name string
	size string
}

func TestCreateSandboxClaimVolumeClaimTemplates(t *testing.T) {
	testCases := []struct {
		name                string
		policy              extensionsv1beta1.VolumeClaimTemplatesPolicy
		templateVCTs        []sandboxv1beta1.PersistentVolumeClaimTemplate
		claimVCTs           []sandboxv1beta1.PersistentVolumeClaimTemplate
		expectedErrorReason string // If non-empty, verifies validation failure with this Ready condition reason
		verifySuccess       func(t *testing.T, tc *framework.TestContext, namespace string, claim *extensionsv1beta1.SandboxClaim)
	}{
		{
			name:                "policy=Disallowed, custom claim VCTs rejected",
			policy:              extensionsv1beta1.VolumeClaimTemplatesPolicyDisallowed,
			claimVCTs:           []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("custom-data", "2Gi")},
			expectedErrorReason: "VolumeClaimTemplatesError",
		},
		{
			name:         "policy=Disallowed, empty claim VCTs bypasses policy check (Warm start adoption)",
			policy:       extensionsv1beta1.VolumeClaimTemplatesPolicyDisallowed,
			templateVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("workspace", "1Gi")},
			claimVCTs:    nil,
			verifySuccess: func(t *testing.T, tc *framework.TestContext, namespace string, claim *extensionsv1beta1.SandboxClaim) {
				claimUpdated := &extensionsv1beta1.SandboxClaim{}
				err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: namespace}, claimUpdated)
				require.NoError(t, err)

				require.NotEmpty(t, claimUpdated.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
				assignedSandboxName := claimUpdated.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation]

				sandbox := &sandboxv1beta1.Sandbox{}
				err = tc.Get(t.Context(), types.NamespacedName{Name: assignedSandboxName, Namespace: namespace}, sandbox)
				require.NoError(t, err)

				require.Equal(t, string(sandboxv1beta1.SandboxLaunchTypeWarm), sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])
				require.Len(t, sandbox.Spec.VolumeClaimTemplates, 1)
				require.Equal(t, "workspace", sandbox.Spec.VolumeClaimTemplates[0].Name)
			},
		},
		{
			name:         "policy=Allowed, new custom claim VCTs merged successfully (Cold start)",
			policy:       extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			templateVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("workspace", "1Gi")},
			claimVCTs:    []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("custom-data", "2Gi")},
			verifySuccess: func(t *testing.T, tc *framework.TestContext, namespace string, claim *extensionsv1beta1.SandboxClaim) {
				verifySandboxAndPVCs(t, tc, namespace, claim.Name, []expectedVCT{
					{name: "workspace", size: "1Gi"},
					{name: "custom-data", size: "2Gi"},
				})
			},
		},
		{
			name:                "policy=Allowed, overriding template volume name is forbidden",
			policy:              extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			templateVCTs:        []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("workspace", "1Gi")},
			claimVCTs:           []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("workspace", "2Gi")},
			expectedErrorReason: "VolumeClaimTemplatesError",
		},
		{
			name:         "policy=Overrides, new custom claim VCTs merged successfully (Cold start)",
			policy:       extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides,
			templateVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("workspace", "1Gi")},
			claimVCTs:    []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("custom-data", "2Gi")},
			verifySuccess: func(t *testing.T, tc *framework.TestContext, namespace string, claim *extensionsv1beta1.SandboxClaim) {
				verifySandboxAndPVCs(t, tc, namespace, claim.Name, []expectedVCT{
					{name: "workspace", size: "1Gi"},
					{name: "custom-data", size: "2Gi"},
				})
			},
		},
		{
			name:         "policy=Overrides, claim VCT overrides template volume spec (Cold start)",
			policy:       extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides,
			templateVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("workspace", "1Gi")},
			claimVCTs:    []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("workspace", "3Gi")},
			verifySuccess: func(t *testing.T, tc *framework.TestContext, namespace string, claim *extensionsv1beta1.SandboxClaim) {
				verifySandboxAndPVCs(t, tc, namespace, claim.Name, []expectedVCT{
					{name: "workspace", size: "3Gi"},
				})
			},
		},
		{
			name:                "policy=empty (default), treated as Disallowed",
			policy:              "",
			claimVCTs:           []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("custom-data", "2Gi")},
			expectedErrorReason: "VolumeClaimTemplatesError",
		},
		{
			name:                "policy=Allowed, empty VCT volume name rejected",
			policy:              extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			claimVCTs:           []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("", "2Gi")},
			expectedErrorReason: "VolumeClaimTemplatesError",
		},
		{
			name:                "policy=Allowed, duplicate VCT volume name rejected",
			policy:              extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			claimVCTs:           []sandboxv1beta1.PersistentVolumeClaimTemplate{customVCT("custom-data", "2Gi"), customVCT("custom-data", "2Gi")},
			expectedErrorReason: "VolumeClaimTemplatesError",
		},
	}

	for _, tcCase := range testCases {
		t.Run(tcCase.name, func(t *testing.T) {
			tc := framework.NewTestContext(t)

			ns := &corev1.Namespace{}
			ns.Name = fmt.Sprintf("vct-e2e-%d", time.Now().UnixNano())
			require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

			template := createTemplateForVCT(t, tc, ns.Name, "vct-template", tcCase.policy, tcCase.templateVCTs)
			warmPool := createWarmPoolForVCT(t, tc, ns.Name, "vct-warmpool", template.Name)

			if tcCase.claimVCTs == nil && tcCase.expectedErrorReason == "" {
				// Wait for the warm pool to populate at least one warm sandbox to support warm start
				sandboxWarmpoolID := types.NamespacedName{Namespace: ns.Name, Name: warmPool.Name}
				require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), sandboxWarmpoolID))
			}

			claim := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vct-claim",
					Namespace: ns.Name,
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef:          extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
					VolumeClaimTemplates: tcCase.claimVCTs,
				},
			}
			require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

			if tcCase.expectedErrorReason != "" {
				tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1beta1.SandboxConditionReady), tcCase.expectedErrorReason))
			} else {
				tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)
				tcCase.verifySuccess(t, tc, ns.Name, claim)
			}
		})
	}
}

func customVCT(name, size string) sandboxv1beta1.PersistentVolumeClaimTemplate {
	return sandboxv1beta1.PersistentVolumeClaimTemplate{
		EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{
			Name: name,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
}

func verifySandboxAndPVCs(t *testing.T, tc *framework.TestContext, namespace, claimName string, expected []expectedVCT) {
	t.Helper()
	sandbox := &sandboxv1beta1.Sandbox{}
	err := tc.Get(t.Context(), types.NamespacedName{Name: claimName, Namespace: namespace}, sandbox)
	require.NoError(t, err)

	// Verify cold start launch type label propagation
	require.Equal(t, string(sandboxv1beta1.SandboxLaunchTypeCold), sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])

	require.Len(t, sandbox.Spec.VolumeClaimTemplates, len(expected))
	for i, exp := range expected {
		require.Equal(t, exp.name, sandbox.Spec.VolumeClaimTemplates[i].Name)

		pvc := &corev1.PersistentVolumeClaim{}
		pvcName := fmt.Sprintf("%s-%s", exp.name, sandbox.Name)
		err = tc.Get(t.Context(), types.NamespacedName{Name: pvcName, Namespace: namespace}, pvc)
		require.NoError(t, err)
		require.Equal(t, exp.size, pvc.Spec.Resources.Requests.Storage().String())
	}
}

func createTemplateForVCT(t *testing.T, tc *framework.TestContext, namespace, name string, policy extensionsv1beta1.VolumeClaimTemplatesPolicy, vcts []sandboxv1beta1.PersistentVolumeClaimTemplate) *extensionsv1beta1.SandboxTemplate {
	t.Helper()
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			NetworkPolicyManagement:    extensionsv1beta1.NetworkPolicyManagementUnmanaged,
			VolumeClaimTemplatesPolicy: policy,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				VolumeClaimTemplates: vcts,
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "pause",
								Image: "registry.k8s.io/pause:3.10",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))
	return template
}

func createWarmPoolForVCT(t *testing.T, tc *framework.TestContext, namespace, name, templateName string) *extensionsv1beta1.SandboxWarmPool {
	t.Helper()
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    ptr.To[int32](1),
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: templateName},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))
	return warmPool
}
