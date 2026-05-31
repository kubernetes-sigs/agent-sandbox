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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func TestSandboxTemplateReconcileNetworkPolicy(t *testing.T) {
	templateDefault := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c1", Image: "img"}}},
			},
		},
	}

	templateWithNP := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-custom", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			NetworkPolicy: &extensionsv1beta1.NetworkPolicySpec{
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

	templateOptOut := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-optout", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			NetworkPolicyManagement: extensionsv1beta1.NetworkPolicyManagementUnmanaged,
			NetworkPolicy: &extensionsv1beta1.NetworkPolicySpec{
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
		templateToReconcile   *extensionsv1beta1.SandboxTemplate
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
				if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 {
					t.Fatalf("Expected Default Ingress rule to contain exactly 1 peer source, got %d", len(np.Spec.Ingress[0].From))
				}
				peer1 := np.Spec.Ingress[0].From[0]
				if peer1.PodSelector == nil || peer1.NamespaceSelector == nil {
					t.Fatalf("Expected both PodSelector and NamespaceSelector to be non-nil")
				}
				if peer1.PodSelector.MatchLabels["app"] != "sandbox-router" ||
					peer1.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "agent-sandbox-system" {
					t.Errorf("Expected first Ingress peer to target sandbox-router in agent-sandbox-system namespace")
				}
				if len(np.Spec.Egress) != 1 {
					t.Fatalf("Expected 1 Default Egress rule, got %d", len(np.Spec.Egress))
				}
				if len(np.Spec.Egress[0].To) != 2 {
					t.Fatalf("Expected 2 Egress peers (IPv4 and IPv6), got %d", len(np.Spec.Egress[0].To))
				}
				if np.Spec.Egress[0].To[0].IPBlock == nil || np.Spec.Egress[0].To[0].IPBlock.CIDR != "0.0.0.0/0" {
					t.Fatalf("Expected Default Egress IPBlock 0.0.0.0/0")
				}
				ipv6Peer := np.Spec.Egress[0].To[1]
				if ipv6Peer.IPBlock == nil || ipv6Peer.IPBlock.CIDR != "::/0" {
					t.Fatalf("Expected Default Egress IPv6 IPBlock ::/0")
				}
				hasIPv6LinkLocalExcept := slices.Contains(ipv6Peer.IPBlock.Except, "fe80::/10")
				if !hasIPv6LinkLocalExcept {
					t.Errorf("Expected IPv6 Egress Except list to contain fe80::/10, got %v", ipv6Peer.IPBlock.Except)
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
				if np.Spec.PodSelector.MatchLabels[sandboxTemplateRefHash] != expectedHash {
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
				Recorder: events.NewFakeRecorder(10),
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
