/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

func TestSandboxClaimReconcile(t *testing.T) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			},
		},
	}

	// New template with NetworkPolicy enabled for testing that feature.
	templateWithNP := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-with-np",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
							Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
						},
					},
				},
			},
			NetworkPolicy: &extensionsv1alpha1.NetworkPolicySpec{
				Enabled: true,
				IngressControllerSelectors: &extensionsv1alpha1.IngressSelector{
					NamespaceSelector: map[string]string{"ns-role": "ingress"},
					PodSelector:       map[string]string{"app": "ingress"},
				},
				AdditionalEgressRules: []extensionsv1alpha1.EgressRule{
					{
						Description: "Allow to metrics",
						ToPodSelector: map[string]string{
							"app": "metrics",
						},
						InNamespaceSelector: map[string]string{
							"ns-role": "monitoring",
						},
					},
				},
			},
		},
	}

	// New template with NetworkPolicy disabled.
	templateWithNPDisabled := templateWithNP.DeepCopy()
	templateWithNPDisabled.Name = "test-template-np-disabled"
	templateWithNPDisabled.Spec.NetworkPolicy.Enabled = false

	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "test-template",
			},
		},
	}

	uncontrolledSandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxSpec{
			PodTemplate: v1alpha1.PodTemplate{
				Spec: template.Spec.PodTemplate.Spec,
			},
		},
	}

	controlledSandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
					Kind:       "SandboxClaim",
					Name:       "test-claim",
					UID:        "claim-uid",
					Controller: func(b bool) *bool { return &b }(true),
				},
			},
		},
		Spec: v1alpha1.SandboxSpec{
			PodTemplate: v1alpha1.PodTemplate{
				Spec: template.Spec.PodTemplate.Spec,
			},
		},
	}

	readySandbox := controlledSandbox.DeepCopy()
	readySandbox.Status.Conditions = []metav1.Condition{
		{
			Type:   string(sandboxv1alpha1.SandboxConditionReady),
			Status: metav1.ConditionTrue,
		},
	}

	testCases := []struct {
		name                  string
		claimToReconcile      *extensionsv1alpha1.SandboxClaim
		existingObjects       []client.Object
		expectSandbox         bool
		expectNetworkPolicy   bool
		expectError           bool
		expectedCondition     metav1.Condition
		validateNetworkPolicy func(t *testing.T, np *networkingv1.NetworkPolicy)
	}{
		{
			name:             "sandbox is created when a claim is made",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template}, // FIX: Removed duplicate 'claim'
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name:             "sandbox is not created when template is not found",
			claimToReconcile: claim,
			existingObjects:  []client.Object{}, // FIX: Removed duplicate 'claim'
			expectSandbox:    false,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "TemplateNotFound",
				Message: `SandboxTemplate "test-template" not found`,
			},
		},
		{
			name:             "sandbox exists but is not controlled by claim",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, uncontrolledSandbox}, // FIX: Removed duplicate 'claim'
			expectSandbox:    true,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "ReconcilerError",
				Message: "Error seen: sandbox \"test-claim\" is not controlled by claim \"test-claim\". Please use a different claim name or delete the sandbox manually",
			},
		},
		{
			name:             "sandbox exists and is controlled by claim",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, controlledSandbox}, // FIX: Removed duplicate 'claim'
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name:             "sandbox exists but template is not found",
			claimToReconcile: claim,
			existingObjects:  []client.Object{readySandbox}, // FIX: Removed duplicate 'claim'
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionTrue,
				Reason:  "SandboxReady",
				Message: "Sandbox is ready",
			},
		},
		{
			name:             "sandbox is ready",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, readySandbox},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionTrue,
				Reason:  "SandboxReady",
				Message: "Sandbox is ready",
			},
		},
		{
			name: "sandbox is created with network policy enabled",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim-np", Namespace: "default", UID: "claim-np-uid"},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-with-np"}},
			},
			existingObjects:     []client.Object{templateWithNP},
			expectSandbox:       true,
			expectNetworkPolicy: true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			validateNetworkPolicy: func(t *testing.T, np *networkingv1.NetworkPolicy) {
				// 1. Check Owner Reference
				if diff := cmp.Diff(np.OwnerReferences[0].UID, types.UID("claim-np-uid")); diff != "" {
					t.Errorf("unexpected owner reference UID:\n%s", diff)
				}
				// 2. Check Pod Selector
				expectedHash := NameHash("test-claim-np")
				if diff := cmp.Diff(np.Spec.PodSelector.MatchLabels[sandboxLabel], expectedHash); diff != "" {
					t.Errorf("unexpected pod selector hash:\n%s", diff)
				}
				// 3. Check Ingress Rule Translation
				if len(np.Spec.Ingress) != 1 {
					t.Fatalf("expected 1 ingress rule, got %d", len(np.Spec.Ingress))
				}
				ingressRule := np.Spec.Ingress[0]
				if diff := cmp.Diff(ingressRule.From[0].NamespaceSelector.MatchLabels, map[string]string{"ns-role": "ingress"}); diff != "" {
					t.Errorf("unexpected ingress namespace selector:\n%s", diff)
				}
				// 4. Check Egress Rule Translation
				if len(np.Spec.Egress) != 2 { // 1 for DNS + 1 custom
					t.Fatalf("expected 2 egress rules, got %d", len(np.Spec.Egress))
				}
				egressRule := np.Spec.Egress[1] // Check the custom rule
				if diff := cmp.Diff(egressRule.To[0].PodSelector.MatchLabels, map[string]string{"app": "metrics"}); diff != "" {
					t.Errorf("unexpected egress pod selector:\n%s", diff)
				}
			},
		},
		{
			name: "sandbox is created with network policy disabled",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim-np-disabled", Namespace: "default", UID: "claim-np-disabled-uid"},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-np-disabled"}},
			},
			existingObjects:     []client.Object{templateWithNPDisabled},
			expectSandbox:       true,
			expectNetworkPolicy: false, // Should not create a NetworkPolicy
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			// Add the claim we are reconciling to the list of existing objects
			allObjects := append(tc.existingObjects, tc.claimToReconcile)
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).WithStatusSubresource(tc.claimToReconcile).Build()

			reconciler := &SandboxClaimReconciler{
				Client: client,
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      tc.claimToReconcile.Name,
					Namespace: tc.claimToReconcile.Namespace,
				},
			}
			_, err := reconciler.Reconcile(context.Background(), req)
			if tc.expectError && err == nil {
				t.Fatal("expected an error but got none")
			}
			if !tc.expectError && err != nil {
				t.Fatalf("reconcile: (%v)", err)
			}

			var sandbox v1alpha1.Sandbox
			err = client.Get(context.Background(), req.NamespacedName, &sandbox)
			if tc.expectSandbox && err != nil {
				t.Fatalf("get sandbox: (%v)", err)
			}
			if !tc.expectSandbox && !k8errors.IsNotFound(err) {
				t.Fatalf("expected sandbox to not exist, but got err: %v", err)
			}

			// Validate NetworkPolicy
			var np networkingv1.NetworkPolicy
			npName := types.NamespacedName{Name: req.Name + "-network-policy", Namespace: req.Namespace}
			err = client.Get(context.Background(), npName, &np)
			if tc.expectNetworkPolicy && err != nil {
				t.Fatalf("get network policy: (%v)", err)
			}
			if !tc.expectNetworkPolicy && !k8errors.IsNotFound(err) {
				t.Fatalf("expected network policy to not exist, but got err: %v", err)
			}
			if tc.validateNetworkPolicy != nil {
				tc.validateNetworkPolicy(t, &np)
			}

			var updatedClaim extensionsv1alpha1.SandboxClaim
			if err := client.Get(context.Background(), req.NamespacedName, &updatedClaim); err != nil {
				t.Fatalf("get sandbox claim: (%v)", err)
			}
			if len(updatedClaim.Status.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got %d", len(updatedClaim.Status.Conditions))
			}
			condition := updatedClaim.Status.Conditions[0]
			// don't compare message if we expect a reconciler error
			if tc.expectedCondition.Reason == "ReconcilerError" {
				if condition.Reason != "ReconcilerError" {
					t.Errorf("expected condition reason %q, got %q", "ReconcilerError", condition.Reason)
				}
			} else { // Only do a full diff if not expecting a generic reconciler error
				if diff := cmp.Diff(tc.expectedCondition, condition, cmp.Comparer(ignoreTimestamp)); diff != "" {
					t.Errorf("unexpected condition:\n%s", diff)
				}
			}
		})
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: (%v)", err)
	}
	if err := extensionsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: (%v)", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: (%v)", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: (%v)", err)
	}
	return scheme
}

func ignoreTimestamp(_, _ metav1.Time) bool {
	return true
}
