/*
Copyright 2026 The Kubernetes Authors.

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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func TestSandboxTemplateReconcileNetworkPolicy(t *testing.T) {
	templateDefault := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c1", Image: "img"}}},
			},
		},
	}

	templateWithNP := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-custom", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			NetworkPolicy: &extensionsv1alpha1.NetworkPolicySpec{
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						From: []networkingv1.NetworkPolicyPeer{
							{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ingress"}}},
						},
					},
				},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						To: []networkingv1.NetworkPolicyPeer{
							{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "metrics"}}},
						},
					},
				},
			},
		},
	}

	templateOptOut := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-optout", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			NetworkPolicyManagement: extensionsv1alpha1.NetworkPolicyManagementUnmanaged,
			NetworkPolicy: &extensionsv1alpha1.NetworkPolicySpec{
				Egress: []networkingv1.NetworkPolicyEgressRule{{}}, // Should be ignored
			},
		},
	}

	existingNPToDelete := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-optout-network-policy", Namespace: "default"},
		Spec:       networkingv1.NetworkPolicySpec{},
	}

	outdatedNPToUpdate := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-custom-network-policy", Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"old-label": "outdated"}}, // Will be overwritten
		},
	}

	testCases := []struct {
		name                  string
		templateToReconcile   *extensionsv1alpha1.SandboxTemplate
		existingObjects       []client.Object
		expectNetworkPolicy   bool
		validateNetworkPolicy func(t *testing.T, np *networkingv1.NetworkPolicy)
	}{
		{
			name:                "Creates Default Secure Policy (Strict Isolation) when template has none",
			templateToReconcile: templateDefault,
			existingObjects:     []client.Object{templateDefault},
			expectNetworkPolicy: true,
			validateNetworkPolicy: func(t *testing.T, np *networkingv1.NetworkPolicy) {
				if len(np.Spec.PolicyTypes) != 2 {
					t.Errorf("Expected 2 PolicyTypes, got %d", len(np.Spec.PolicyTypes))
				}
				if len(np.Spec.Ingress) != 1 || np.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app"] != "sandbox-router" {
					t.Errorf("Expected Default Ingress rule to target sandbox-router")
				}
				if len(np.Spec.Egress) != 1 || np.Spec.Egress[0].To[0].IPBlock.CIDR != "0.0.0.0/0" {
					t.Fatalf("Expected Default Egress IPBlock 0.0.0.0/0")
				}
				expectedLabelKey := "agents.x-k8s.io/sandbox-template-ref-hash"
				if _, ok := np.Spec.PodSelector.MatchLabels[expectedLabelKey]; !ok {
					t.Errorf("Expected PodSelector MatchLabels to contain %q", expectedLabelKey)
				}
			},
		},
		{
			name:                "Creates custom network policy when defined in template",
			templateToReconcile: templateWithNP,
			existingObjects:     []client.Object{templateWithNP},
			expectNetworkPolicy: true,
			validateNetworkPolicy: func(t *testing.T, np *networkingv1.NetworkPolicy) {
				expectedHash := sandboxcontrollers.NameHash("test-template-custom")
				if np.Spec.PodSelector.MatchLabels[sandboxTemplateLabel] != expectedHash {
					t.Errorf("unexpected pod selector hash")
				}
				if np.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app"] != "ingress" {
					t.Errorf("unexpected custom ingress rule")
				}
			},
		},
		{
			name:                "NetworkPolicy is not created when template is Unmanaged",
			templateToReconcile: templateOptOut,
			existingObjects:     []client.Object{templateOptOut},
			expectNetworkPolicy: false,
		},
		{
			name:                "Existing NetworkPolicy is deleted when template updates to Unmanaged",
			templateToReconcile: templateOptOut,
			existingObjects:     []client.Object{templateOptOut, existingNPToDelete},
			expectNetworkPolicy: false,
		},
		{
			name:                "Existing NetworkPolicy is updated when template spec changes",
			templateToReconcile: templateWithNP,
			existingObjects:     []client.Object{templateWithNP, outdatedNPToUpdate},
			expectNetworkPolicy: true,
			validateNetworkPolicy: func(t *testing.T, np *networkingv1.NetworkPolicy) {
				if _, exists := np.Spec.PodSelector.MatchLabels["old-label"]; exists {
					t.Errorf("expected old outdated labels to be removed")
				}
				if np.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app"] != "ingress" {
					t.Errorf("expected updated ingress rule with app: ingress")
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t) // Assuming newScheme is in your other test file (it's package level)
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.existingObjects...).Build()

			reconciler := &SandboxTemplateReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(10),
				Tracer:   asmetrics.NewNoOp(),
			}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: tc.templateToReconcile.Name, Namespace: "default"},
			}

			_, err := reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("reconcile: (%v)", err)
			}

			var np networkingv1.NetworkPolicy
			npName := types.NamespacedName{Name: tc.templateToReconcile.Name + "-network-policy", Namespace: req.Namespace}
			err = client.Get(context.Background(), npName, &np)

			if tc.expectNetworkPolicy && err != nil {
				t.Fatalf("expected network policy to exist, got err: %v", err)
			}
			if !tc.expectNetworkPolicy && !k8errors.IsNotFound(err) {
				t.Fatalf("expected network policy to not exist (err: %v)", err)
			}

			if tc.expectNetworkPolicy && tc.validateNetworkPolicy != nil {
				tc.validateNetworkPolicy(t, &np)
			}
		})
	}
}
