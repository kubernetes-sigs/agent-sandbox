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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func TestSandboxClaimReconcile(t *testing.T) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
				},
			},
		},
	}

	templateWithNP := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-with-np",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
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
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						From: []networkingv1.NetworkPolicyPeer{
							{
								NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"ns-role": "ingress"}},
								PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ingress"}},
							},
						},
					},
				},

				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						To: []networkingv1.NetworkPolicyPeer{
							{
								PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "metrics"}},
							},
						},
					},
				},
			},
		},
	}

	templateWithNPDisabled := templateWithNP.DeepCopy()
	templateWithNPDisabled.Name = "test-template-np-disabled"
	templateWithNPDisabled.Spec.NetworkPolicy = nil

	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"}},
	}

	uncontrolledSandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default"},
		Spec: sandboxv1alpha1.SandboxSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
					},
				},
				Spec: template.Spec.PodTemplate.Spec,
			},
		},
	}

	controlledSandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-claim", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim", Name: "test-claim", UID: "claim-uid", Controller: ptr.To(true),
			}},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
					},
				},
				Spec: template.Spec.PodTemplate.Spec,
			},
		},
	}

	controlledSandbox.Spec.PodTemplate.Spec.DNSPolicy = corev1.DNSNone
	controlledSandbox.Spec.PodTemplate.Spec.DNSConfig = &corev1.PodDNSConfig{
		Nameservers: []string{"8.8.8.8", "1.1.1.1"},
	}

	controlledSandboxWithDefault := controlledSandbox.DeepCopy()
	controlledSandboxWithDefault.Spec.PodTemplate.Spec.AutomountServiceAccountToken = ptr.To(false)

	templateWithAutomount := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "automount-template", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(true), Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}}},
			},
		},
	}

	claimForAutomount := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "automount-claim", Namespace: "default", UID: "claim-uid-automount"},
		Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "automount-template"}},
	}

	templateOptOut := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-opt-out",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			NetworkPolicyManagement: extensionsv1alpha1.NetworkPolicyManagementUnmanaged,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
				},
			},
			NetworkPolicy: &extensionsv1alpha1.NetworkPolicySpec{
				Egress: []networkingv1.NetworkPolicyEgressRule{{}},
			},
		},
	}

	existingNPToDelete := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-opt-out-network-policy", // Must match the expected generated name
			Namespace: "default",
		},
		Spec: networkingv1.NetworkPolicySpec{}, // The contents don't matter, we just want it to exist
	}

	// Represents a policy that was created in the past, but the template has since changed
	outdatedNPToUpdate := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-with-np-network-policy", // Matches templateWithNP
			Namespace: "default",
		},
		Spec: networkingv1.NetworkPolicySpec{
			// Purposely outdated/wrong spec
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"old-label": "outdated"},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}

	claimOptOut := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-opt-out", Namespace: "default", UID: "claim-uid-opt-out"},
		Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-opt-out"}},
	}

	readySandbox := controlledSandboxWithDefault.DeepCopy()
	readySandbox.Status.Conditions = []metav1.Condition{{
		Type:    string(sandboxv1alpha1.SandboxConditionReady),
		Status:  metav1.ConditionTrue,
		Reason:  "SandboxReady",
		Message: "Sandbox is ready",
	}}
	readySandbox.Status.PodIPs = []string{"10.244.0.6"}

	// Validation Functions
	validateSandboxHasDefaultAutomountToken := func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, template *extensionsv1alpha1.SandboxTemplate) {
		expectedSpec := template.Spec.PodTemplate.Spec.DeepCopy()
		expectedSpec.AutomountServiceAccountToken = ptr.To(false)

		expectedSpec.DNSPolicy = corev1.DNSNone
		expectedSpec.DNSConfig = &corev1.PodDNSConfig{
			Nameservers: []string{"8.8.8.8", "1.1.1.1"},
		}
		if diff := cmp.Diff(&sandbox.Spec.PodTemplate.Spec, expectedSpec); diff != "" {
			t.Errorf("unexpected sandbox spec:\n%s", diff)
		}
	}

	validateSandboxAutomountTrue := func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
		if sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken == nil || !*sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken {
			t.Error("expected AutomountServiceAccountToken to be true")
		}
	}

	validateSandboxDNSUntouched := func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
		// Prove that the air-gapped fix works: DNS should not be overridden!
		if sandbox.Spec.PodTemplate.Spec.DNSPolicy == corev1.DNSNone {
			t.Errorf("Expected DNSPolicy to remain untouched, but it was set to None")
		}
		if sandbox.Spec.PodTemplate.Spec.DNSConfig != nil {
			t.Errorf("Expected DNSConfig to be nil, but got %v", sandbox.Spec.PodTemplate.Spec.DNSConfig)
		}
	}

	testCases := []struct {
		name              string
		claimToReconcile  *extensionsv1alpha1.SandboxClaim
		existingObjects   []client.Object
		expectSandbox     bool
		expectError       bool
		expectedCondition metav1.Condition
		expectedPodIPs    []string
		validateSandbox   func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, template *extensionsv1alpha1.SandboxTemplate)
	}{
		{
			name:             "sandbox is created when a claim is made",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: validateSandboxHasDefaultAutomountToken,
		},
		{
			name:             "sandbox is created with automount token enabled",
			claimToReconcile: claimForAutomount,
			existingObjects:  []client.Object{templateWithAutomount},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: validateSandboxAutomountTrue,
		},
		{
			name:             "sandbox is not created when template is not found",
			claimToReconcile: claim,
			existingObjects:  []client.Object{},
			expectSandbox:    false,
			expectError:      false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "TemplateNotFound", Message: `SandboxTemplate "test-template" not found`,
			},
		},
		{
			name:             "sandbox exists but is not controlled by claim",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, uncontrolledSandbox},
			expectSandbox:    true,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: sandbox \"test-claim\" is not controlled by claim \"test-claim\". Please use a different claim name or delete the sandbox manually",
			},
		},
		{
			name:             "sandbox exists and is controlled by claim",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, controlledSandboxWithDefault},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: validateSandboxHasDefaultAutomountToken,
		},
		{
			name:             "sandbox exists but template is not found",
			claimToReconcile: claim,
			existingObjects:  []client.Object{readySandbox},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "SandboxReady", Message: "Sandbox is ready",
			},
			expectedPodIPs:  []string{"10.244.0.6"},
			validateSandbox: validateSandboxHasDefaultAutomountToken,
		},
		{
			name:             "sandbox is ready",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, readySandbox},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "SandboxReady", Message: "Sandbox is ready",
			},
			expectedPodIPs:  []string{"10.244.0.6"},
			validateSandbox: validateSandboxHasDefaultAutomountToken,
		},
		{
			name: "sandbox is created with network policy enabled",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim-np", Namespace: "default", UID: "claim-np-uid"},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-with-np"}},
			},
			existingObjects: []client.Object{templateWithNP},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: validateSandboxDNSUntouched,
		},
		{
			name: "Scenario A: Creates Default Secure Policy (Strict Isolation) when template has none",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-default-np", Namespace: "default", UID: "uid-default-np"},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"}},
			},
			existingObjects: []client.Object{template},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				// Verify DNS Bypass is successfully injected
				if sandbox.Spec.PodTemplate.Spec.DNSPolicy != corev1.DNSNone {
					t.Errorf("Expected DNSPolicy to be 'None', got %q", sandbox.Spec.PodTemplate.Spec.DNSPolicy)
				}
				if sandbox.Spec.PodTemplate.Spec.DNSConfig == nil || len(sandbox.Spec.PodTemplate.Spec.DNSConfig.Nameservers) != 2 {
					t.Fatalf("Expected injected DNSConfig with 2 public nameservers")
				}
				if sandbox.Spec.PodTemplate.Spec.DNSConfig.Nameservers[0] != "8.8.8.8" {
					t.Errorf("Expected first nameserver to be 8.8.8.8, got %q", sandbox.Spec.PodTemplate.Spec.DNSConfig.Nameservers[0])
				}
			},
		},
		{
			name:             "Existing NetworkPolicy is deleted when template opts out and removes custom policy",
			claimToReconcile: claimOptOut, // Uses the template with disableDefaultNetworkPolicy: true
			existingObjects:  []client.Object{templateOptOut, existingNPToDelete},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name: "Existing NetworkPolicy is updated when template spec changes and a new sandboxclaim is created",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim-update", Namespace: "default", UID: "claim-update-uid"},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-with-np"}},
			},
			// Seed the cluster with the correct template, but the wrong/outdated network policy
			existingObjects: []client.Object{templateWithNP, outdatedNPToUpdate},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name:             "NetworkPolicy is not created when template has NetworkPolicyManagement set to Unmanaged",
			claimToReconcile: claimOptOut,
			existingObjects:  []client.Object{templateOptOut},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name: "trace context is propagated from claim to sandbox",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "trace-claim", Namespace: "default", UID: "trace-uid",
					Annotations: map[string]string{asmetrics.TraceContextAnnotation: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
				},
				Spec: extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"}},
			},
			existingObjects: []client.Object{template},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				if val, ok := sandbox.Annotations[asmetrics.TraceContextAnnotation]; !ok || val != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
					t.Errorf("expected trace context annotation to be propagated, got %q", val)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)

			// Logic to determine which claim to use (Default to 'claim' if nil)
			claimToUse := tc.claimToReconcile
			if claimToUse == nil {
				claimToUse = claim // Fallback for older tests
			}

			allObjects := append(tc.existingObjects, claimToUse)
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjects...).WithStatusSubresource(claimToUse).Build()

			reconciler := &SandboxClaimReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: events.NewFakeRecorder(10),
				Tracer:   asmetrics.NewNoOp(),
			}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: claimToUse.Name, Namespace: "default"},
			}
			_, err := reconciler.Reconcile(context.Background(), req)
			if tc.expectError && err == nil {
				t.Fatal("expected an error but got none")
			}
			if !tc.expectError && err != nil {
				t.Fatalf("reconcile: (%v)", err)
			}

			var sandbox sandboxv1alpha1.Sandbox
			err = client.Get(context.Background(), req.NamespacedName, &sandbox)
			if tc.expectSandbox && err != nil {
				t.Fatalf("get sandbox: (%v)", err)
			}
			if !tc.expectSandbox && !k8errors.IsNotFound(err) {
				t.Fatalf("expected sandbox to not exist, but got err: %v", err)
			}

			if tc.expectSandbox {
				// Verify the controller injected the template hash label so the NP can find the pod
				expectedHash := sandboxcontrollers.NameHash(claimToUse.Spec.TemplateRef.Name)
				if val, exists := sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash]; !exists || val != expectedHash {
					t.Errorf("expected Sandbox PodTemplate to have label '%s' with value %q, got %q", sandboxTemplateRefHash, expectedHash, val)
				}
			}

			if tc.validateSandbox != nil {
				tc.validateSandbox(t, &sandbox, template)
			}

			var updatedClaim extensionsv1alpha1.SandboxClaim
			if err := client.Get(context.Background(), req.NamespacedName, &updatedClaim); err != nil {
				t.Fatalf("get sandbox claim: (%v)", err)
			}
			if len(updatedClaim.Status.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got %d", len(updatedClaim.Status.Conditions))
			}
			condition := updatedClaim.Status.Conditions[0]
			if tc.expectedCondition.Reason == "ReconcilerError" {
				if condition.Reason != "ReconcilerError" {
					t.Errorf("expected condition reason %q, got %q", "ReconcilerError", condition.Reason)
				}
			} else {
				if len(tc.expectedPodIPs) > 0 {
					if diff := cmp.Diff(tc.expectedPodIPs, updatedClaim.Status.SandboxStatus.PodIPs); diff != "" {
						t.Errorf("unexpected PodIPs:\n%s", diff)
					}
				}
				if diff := cmp.Diff(tc.expectedCondition, condition, cmp.Comparer(ignoreTimestamp)); diff != "" {
					t.Errorf("unexpected condition:\n%s", diff)
				}
			}
		})
	}
}

// TestSandboxClaimCleanupPolicy verifies that the Claim deletes itself
// based on its own timestamp, and deletes the Sandbox if Policy=Retain.
func TestSandboxClaimCleanupPolicy(t *testing.T) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-template", Namespace: "default"},
		Spec:       extensionsv1alpha1.SandboxTemplateSpec{PodTemplate: sandboxv1alpha1.PodTemplate{}},
	}

	createClaim := func(name string, policy extensionsv1alpha1.ShutdownPolicy) *extensionsv1alpha1.SandboxClaim {
		pastTime := metav1.Time{Time: time.Now().Add(-2 * time.Hour).Truncate(time.Second)}
		return &extensionsv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
			Spec: extensionsv1alpha1.SandboxClaimSpec{
				TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "cleanup-template"},
				Lifecycle: &extensionsv1alpha1.Lifecycle{
					ShutdownPolicy: policy,
					ShutdownTime:   &pastTime,
				},
			},
		}
	}

	// Helper to create a Sandbox.
	createSandbox := func(claimName string, isExpired bool) *sandboxv1alpha1.Sandbox {
		reason := "SandboxReady"
		status := metav1.ConditionTrue
		if isExpired {
			reason = "SandboxExpired"
			status = metav1.ConditionFalse
		}

		return &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      claimName,
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim", Name: claimName, UID: types.UID(claimName), Controller: ptr.To(true)},
				},
			},
			Spec: sandboxv1alpha1.SandboxSpec{PodTemplate: sandboxv1alpha1.PodTemplate{}},
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:   string(sandboxv1alpha1.SandboxConditionReady),
						Status: status,
						Reason: reason,
					},
				},
			},
		}
	}

	testCases := []struct {
		name                 string
		claim                *extensionsv1alpha1.SandboxClaim
		sandboxIsExpired     bool
		expectClaimDeleted   bool
		expectSandboxDeleted bool
		expectStatus         string
	}{
		{
			name:                 "Policy=Retain -> Should Retain Claim but DELETE Sandbox",
			claim:                createClaim("retain-claim", extensionsv1alpha1.ShutdownPolicyRetain),
			sandboxIsExpired:     false,
			expectClaimDeleted:   false,
			expectSandboxDeleted: true, // Controller explicitly deletes Sandbox here.
			expectStatus:         extensionsv1alpha1.ClaimExpiredReason,
		},
		{
			name:               "Policy=Delete && Sandbox Expired -> Should Delete Claim",
			claim:              createClaim("delete-claim-synced", extensionsv1alpha1.ShutdownPolicyDelete),
			sandboxIsExpired:   true,
			expectClaimDeleted: true,
			// In unit tests (FakeClient), deleting the Parent (Claim) does NOT automatically delete the Child (Sandbox).
			// Since our controller only deletes the Claim and relies on K8s GC for the Sandbox,
			// the Sandbox will technically remain in the FakeClient. This is expected behavior for tests.
			expectSandboxDeleted: false,
			expectStatus:         "",
		},
		{
			name:                 "Policy=Delete && Sandbox Running -> Should Delete Claim immediately",
			claim:                createClaim("delete-claim-race", extensionsv1alpha1.ShutdownPolicyDelete),
			sandboxIsExpired:     false,
			expectClaimDeleted:   true,
			expectSandboxDeleted: false, // Same as above: FakeClient doesn't simulate GC.
			expectStatus:         "",
		},
		{
			name:               "Policy=DeleteForeground && Sandbox Running -> Should Delete Claim with foreground propagation",
			claim:              createClaim("delete-fg-claim", extensionsv1alpha1.ShutdownPolicyDeleteForeground),
			sandboxIsExpired:   false,
			expectClaimDeleted: true,
			// FakeClient doesn't simulate GC or foreground propagation,
			// so the Sandbox will remain. The important thing is the Claim is deleted.
			expectSandboxDeleted: false,
			expectStatus:         "",
		},
		{
			name:                 "Policy=DeleteForeground && Sandbox Expired -> Should Delete Claim with foreground propagation",
			claim:                createClaim("delete-fg-claim-expired", extensionsv1alpha1.ShutdownPolicyDeleteForeground),
			sandboxIsExpired:     true,
			expectClaimDeleted:   true,
			expectSandboxDeleted: false,
			expectStatus:         "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			sandbox := createSandbox(tc.claim.Name, tc.sandboxIsExpired)
			client := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(template, tc.claim, sandbox).
				WithStatusSubresource(tc.claim).Build()

			reconciler := &SandboxClaimReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: events.NewFakeRecorder(10),
				Tracer:   asmetrics.NewNoOp(),
			}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: tc.claim.Name, Namespace: "default"}}
			_, err := reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			// 1. Verify Claim
			var fetchedClaim extensionsv1alpha1.SandboxClaim
			err = client.Get(context.Background(), req.NamespacedName, &fetchedClaim)

			if tc.expectClaimDeleted {
				if !k8errors.IsNotFound(err) {
					t.Errorf("Expected Claim to be deleted, but it still exists")
				}
			} else {
				if err != nil {
					t.Errorf("Expected Claim to exist, but got error: %v", err)
				}
				// Verify Status Message for Retained Claims
				foundReason := false
				for _, cond := range fetchedClaim.Status.Conditions {
					if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Reason == tc.expectStatus {
						foundReason = true
					}
				}
				if !foundReason {
					t.Errorf("Expected status reason %q, but not found", tc.expectStatus)
				}
			}

			// 2. Verify Sandbox
			var fetchedSandbox sandboxv1alpha1.Sandbox
			err = client.Get(context.Background(), req.NamespacedName, &fetchedSandbox)

			if tc.expectSandboxDeleted {
				if !k8errors.IsNotFound(err) {
					t.Error("Expected Sandbox to be deleted (explicitly by controller), but it still exists")
				}
			} else {
				// For Policy=Delete.
				// We verify it still exists to ensure the controller didn't delete it explicitly (which would be redundant).
				if k8errors.IsNotFound(err) {
					t.Error("Expected Sandbox to persist (FakeClient has no GC), but it was deleted")
				}
			}
		})
	}
}

// TestSandboxProvisionEvent verifies that Sandbox creation emits "SandboxProvisioned".
func TestSandboxProvisionEvent(t *testing.T) {
	scheme := newScheme(t)
	claimName := "provision-event-claim"

	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: types.UID(claimName)},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
		},
	}

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec:       extensionsv1alpha1.SandboxTemplateSpec{PodTemplate: sandboxv1alpha1.PodTemplate{}},
	}

	fakeRecorder := events.NewFakeRecorder(10)
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, template).
		WithStatusSubresource(claim).Build()

	reconciler := &SandboxClaimReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: fakeRecorder,
		Tracer:   asmetrics.NewNoOp(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify 'SandboxProvisioned' Event
	expectedMsg := fmt.Sprintf("Normal SandboxProvisioned Created Sandbox %q", claimName)
	foundProvisionEvent := false
	// Drain the channel
Loop:
	for {
		select {
		case event := <-fakeRecorder.Events:
			if event == expectedMsg {
				foundProvisionEvent = true
				break Loop
			}
		default:
			break Loop
		}
	}
	if !foundProvisionEvent {
		t.Errorf("Expected event %q not found", expectedMsg)
	}
}

func TestSandboxClaimSandboxAdoption(t *testing.T) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
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

	warmPoolUID := types.UID("warmpool-uid-123")
	poolNameHash := sandboxcontrollers.NameHash("test-pool")

	createWarmPoolSandbox := func(name string, creationTime metav1.Time, ready bool) *sandboxv1alpha1.Sandbox {
		conditionStatus := metav1.ConditionFalse
		if ready {
			conditionStatus = metav1.ConditionTrue
		}
		replicas := int32(1)
		return &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				CreationTimestamp: creationTime,
				Labels: map[string]string{
					warmPoolSandboxLabel:   poolNameHash,
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
						Kind:       "SandboxWarmPool",
						Name:       "test-pool",
						UID:        warmPoolUID,
						Controller: ptr.To(true),
					},
				},
			},
			Spec: sandboxv1alpha1.SandboxSpec{
				Replicas: &replicas,
				PodTemplate: sandboxv1alpha1.PodTemplate{
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
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:   string(sandboxv1alpha1.SandboxConditionReady),
						Status: conditionStatus,
						Reason: "DependenciesReady",
					},
				},
			},
		}
	}

	createSandboxWithDifferentController := func(name string) *sandboxv1alpha1.Sandbox {
		replicas := int32(1)
		return &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					warmPoolSandboxLabel:   poolNameHash,
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       "other-controller",
						UID:        "other-uid-456",
						Controller: ptr.To(true),
					},
				},
			},
			Spec: sandboxv1alpha1.SandboxSpec{
				Replicas: &replicas,
				PodTemplate: sandboxv1alpha1.PodTemplate{
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
	}

	createDeletingSandbox := func(name string) *sandboxv1alpha1.Sandbox {
		sb := createWarmPoolSandbox(name, metav1.Now(), true)
		now := metav1.Now()
		sb.DeletionTimestamp = &now
		sb.Finalizers = []string{"test-finalizer"}
		return sb
	}

	testCases := []struct {
		name                    string
		existingObjects         []client.Object
		expectSandboxAdoption   bool
		expectedAdoptedSandbox  string
		expectNewSandboxCreated bool
		simulateConflicts       int
	}{
		{
			name: "adopts oldest ready sandbox from warm pool",
			existingObjects: []client.Object{
				template,
				claim,
				createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-3600 * time.Second)}, true),
				createWarmPoolSandbox("pool-sb-2", metav1.Time{Time: metav1.Now().Add(-1800 * time.Second)}, true),
				createWarmPoolSandbox("pool-sb-3", metav1.Now(), true),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "pool-sb-1",
			expectNewSandboxCreated: false,
		},
		{
			name: "creates new sandbox when no warm pool sandboxes exist",
			existingObjects: []client.Object{
				template,
				claim,
			},
			expectSandboxAdoption:   false,
			expectNewSandboxCreated: true,
		},
		{
			name: "skips sandboxes with different controller",
			existingObjects: []client.Object{
				template,
				claim,
				createSandboxWithDifferentController("other-sb-1"),
				createWarmPoolSandbox("pool-sb-1", metav1.Now(), true),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "pool-sb-1",
			expectNewSandboxCreated: false,
		},
		{
			name: "skips sandboxes being deleted",
			existingObjects: []client.Object{
				template,
				claim,
				createDeletingSandbox("deleting-sb"),
				createWarmPoolSandbox("pool-sb-1", metav1.Now(), true),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "pool-sb-1",
			expectNewSandboxCreated: false,
		},
		{
			name: "creates new sandbox when only ineligible warm pool sandboxes exist",
			existingObjects: []client.Object{
				template,
				claim,
				createSandboxWithDifferentController("other-sb-1"),
				createDeletingSandbox("deleting-sb"),
			},
			expectSandboxAdoption:   false,
			expectNewSandboxCreated: true,
		},
		{
			name: "prioritizes ready sandboxes over not-ready ones",
			existingObjects: []client.Object{
				template,
				claim,
				createWarmPoolSandbox("not-ready", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, false),
				createWarmPoolSandbox("middle-ready", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true),
				createWarmPoolSandbox("young-ready", metav1.Now(), true),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "middle-ready",
			expectNewSandboxCreated: false,
		},
		{
			name: "adopts oldest non-ready sandbox when no ready sandboxes exist",
			existingObjects: []client.Object{
				template,
				claim,
				createWarmPoolSandbox("not-ready-1", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, false),
				createWarmPoolSandbox("not-ready-2", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, false),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "not-ready-1",
			expectNewSandboxCreated: false,
		},
		{
			name: "retries on conflict when adopting sandbox",
			existingObjects: []client.Object{
				template,
				claim,
				createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true),
				createWarmPoolSandbox("pool-sb-2", metav1.Now(), true),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "pool-sb-2",
			expectNewSandboxCreated: false,
			simulateConflicts:       1, // Fail update on the first sandbox, succeed on the second
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			var fakeClient client.Client = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tc.existingObjects...).
				WithStatusSubresource(claim).
				Build()

			if tc.simulateConflicts > 0 {
				fakeClient = &conflictClient{
					Client:       fakeClient,
					maxConflicts: tc.simulateConflicts,
				}
			}

			reconciler := &SandboxClaimReconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				Recorder: events.NewFakeRecorder(10),
				Tracer:   asmetrics.NewNoOp(),
			}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-claim",
					Namespace: "default",
				},
			}

			ctx := context.Background()
			_, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			if tc.expectSandboxAdoption {
				// Verify the adopted sandbox has correct labels and owner reference
				var adoptedSandbox sandboxv1alpha1.Sandbox
				err = fakeClient.Get(ctx, types.NamespacedName{
					Name:      tc.expectedAdoptedSandbox,
					Namespace: "default",
				}, &adoptedSandbox)
				if err != nil {
					t.Fatalf("failed to get adopted sandbox: %v", err)
				}

				// 1. Verify warm pool labels were removed
				if _, exists := adoptedSandbox.Labels[warmPoolSandboxLabel]; exists {
					t.Errorf("expected warm pool label to be removed from adopted sandbox")
				}
				if _, exists := adoptedSandbox.Labels[sandboxTemplateRefHash]; exists {
					t.Errorf("expected template ref label to be removed from adopted sandbox")
				}

				// 2. Verify SandboxID label was added to pod template
				expectedUID := string(types.UID("claim-uid"))
				if val := adoptedSandbox.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1alpha1.SandboxIDLabel]; val != expectedUID {
					t.Errorf("expected pod template to have SandboxID label %q, got %q", expectedUID, val)
				}

				// 3. Verify claim is the controller owner
				controllerRef := metav1.GetControllerOf(&adoptedSandbox)
				if controllerRef == nil || controllerRef.UID != claim.UID {
					t.Errorf("expected adopted sandbox to be controlled by claim, got %v", controllerRef)
				}

			} else if tc.expectNewSandboxCreated {
				// Verify a new sandbox was created with the claim's name
				var sandbox sandboxv1alpha1.Sandbox
				err = fakeClient.Get(ctx, req.NamespacedName, &sandbox)
				if err != nil {
					t.Fatalf("expected sandbox to be created but got error: %v", err)
				}
			}
		})
	}
}

// TestSandboxClaimNoReAdoption verifies that a second reconcile does not adopt another
// sandbox from the warm pool when the claim already owns one.
func TestSandboxClaimNoReAdoption(t *testing.T) {
	scheme := newScheme(t)

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img"}},
				},
			},
		},
	}

	poolNameHash := sandboxcontrollers.NameHash("test-pool")

	// Claim that already adopted a sandbox (name recorded in status)
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"}},
		Status: extensionsv1alpha1.SandboxClaimStatus{
			SandboxStatus: extensionsv1alpha1.SandboxStatus{Name: "adopted-sb"},
		},
	}

	// The previously adopted sandbox (owned by claim, different name)
	adoptedSandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "adopted-sb", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim",
				Name: "test-claim", UID: "claim-uid", Controller: ptr.To(true),
			}},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas:    ptr.To(int32(1)),
			PodTemplate: sandboxv1alpha1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}},
		},
	}

	// Another warm pool sandbox that should NOT be adopted
	poolSandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pool-sb-extra", Namespace: "default",
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas:    ptr.To(int32(1)),
			PodTemplate: sandboxv1alpha1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}},
		},
		Status: sandboxv1alpha1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, claim, adoptedSandbox, poolSandbox).
		WithStatusSubresource(claim).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
		Tracer:   asmetrics.NewNoOp(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Verify the pool sandbox was NOT adopted (still has warm pool labels)
	var extra sandboxv1alpha1.Sandbox
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool-sb-extra", Namespace: "default"}, &extra); err != nil {
		t.Fatalf("failed to get pool sandbox: %v", err)
	}
	if _, ok := extra.Labels[warmPoolSandboxLabel]; !ok {
		t.Error("pool sandbox should still have warm pool label (should not have been adopted)")
	}
}

func TestRecordCreationLatencyMetric(t *testing.T) {
	ctx := context.Background()
	pastTime := metav1.Time{Time: time.Now().Add(-10 * time.Second)}

	testCases := []struct {
		name                           string
		claim                          *extensionsv1alpha1.SandboxClaim
		oldStatus                      *extensionsv1alpha1.SandboxClaimStatus
		sandbox                        *sandboxv1alpha1.Sandbox
		expectedObservations           int
		expectedControllerObservations int
		setupReconciler                func(r *SandboxClaimReconciler)
	}{
		{
			name: "records success on first ready transition",
			claim: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "new-ready", CreationTimestamp: pastTime},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"}},
				Status: extensionsv1alpha1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:            &extensionsv1alpha1.SandboxClaimStatus{},
			expectedObservations: 1,
		},
		{
			name: "ignores ready condition = false",
			claim: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "not-ready", CreationTimestamp: pastTime},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"}},
				Status: extensionsv1alpha1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse}},
				},
			},
			oldStatus:            &extensionsv1alpha1.SandboxClaimStatus{},
			expectedObservations: 0,
		},
		{
			name: "ignores success if status was already ready in previous loop",
			claim: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "already-ready", CreationTimestamp: pastTime},
				Status: extensionsv1alpha1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus: &extensionsv1alpha1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
			expectedObservations: 0,
		},
		{
			name: "uses unknown launch type when sandbox is nil",
			claim: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "unknown", CreationTimestamp: pastTime},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"}},
				Status: extensionsv1alpha1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Unknown"}},
				},
			},
			oldStatus:            &extensionsv1alpha1.SandboxClaimStatus{},
			sandbox:              nil,
			expectedObservations: 1,
		},
		{
			name: "records controller latency using stored time",
			claim: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "stored-time",
					Namespace:         "default",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					},
				},
				Spec: extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"}},
				Status: extensionsv1alpha1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:                      &extensionsv1alpha1.SandboxClaimStatus{},
			expectedObservations:           1,
			expectedControllerObservations: 1,
			setupReconciler: func(r *SandboxClaimReconciler) {
				key := types.NamespacedName{Name: "stored-time", Namespace: "default"}
				r.observedTimes.Store(key, time.Now().Add(-5*time.Second))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset the metrics registry for a clean test
			asmetrics.ClaimStartupLatency.Reset()
			asmetrics.ClaimControllerStartupLatency.Reset()

			r := &SandboxClaimReconciler{}

			if tc.setupReconciler != nil {
				tc.setupReconciler(r)
			}

			r.recordCreationLatencyMetric(ctx, tc.claim, tc.oldStatus, tc.sandbox)

			// Verify the metric was observed in the Prometheus registry
			count := testutil.CollectAndCount(asmetrics.ClaimStartupLatency)
			if count != tc.expectedObservations {
				t.Errorf("expected %d observations for ClaimStartupLatency, got %d", tc.expectedObservations, count)
			}

			countController := testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency)
			if countController != tc.expectedControllerObservations {
				t.Errorf("expected %d observations for ClaimControllerStartupLatency, got %d", tc.expectedControllerObservations, countController)
			}
		})
	}
}

func TestSandboxClaimCreationMetric(t *testing.T) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
				},
			},
		},
	}

	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"}},
	}

	t.Run("Cold Start", func(t *testing.T) {
		asmetrics.SandboxClaimCreationTotal.Reset()
		scheme := newScheme(t)
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, claim).WithStatusSubresource(claim).Build()
		reconciler := &SandboxClaimReconciler{
			Client:   client,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
			Tracer:   asmetrics.NewNoOp(),
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: "default"}}
		_, err := reconciler.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify metric
		val := testutil.ToFloat64(asmetrics.SandboxClaimCreationTotal.WithLabelValues("default", "test-template", asmetrics.LaunchTypeCold, "none", "not_ready"))
		if val != 1 {
			t.Errorf("expected metric count 1, got %v", val)
		}
	})

	t.Run("Warm Start", func(t *testing.T) {
		asmetrics.SandboxClaimCreationTotal.Reset()

		// Create a warm pool sandbox
		poolNameHash := sandboxcontrollers.NameHash("test-pool")
		warmSandbox := &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "warm-sb",
				Namespace: "default",
				Labels: map[string]string{
					warmPoolSandboxLabel:   poolNameHash,
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
						Kind:       "SandboxWarmPool",
						Name:       "test-pool",
						UID:        "pool-uid",
						Controller: ptr.To(true),
					},
				},
			},
			Spec: sandboxv1alpha1.SandboxSpec{
				Replicas:    ptr.To(int32(1)),
				PodTemplate: sandboxv1alpha1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}},
			},
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
				}},
			},
		}

		scheme := newScheme(t)
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, claim, warmSandbox).WithStatusSubresource(claim).Build()
		reconciler := &SandboxClaimReconciler{
			Client:   client,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
			Tracer:   asmetrics.NewNoOp(),
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: "default"}}
		_, err := reconciler.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify metric
		val := testutil.ToFloat64(asmetrics.SandboxClaimCreationTotal.WithLabelValues("default", "test-template", asmetrics.LaunchTypeWarm, "test-pool", "ready"))
		if val != 1 {
			t.Errorf("expected metric count 1, got %v", val)
		}
	})
}

func newScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
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

type conflictClient struct {
	client.Client
	conflictCount int
	maxConflicts  int
}

func (c *conflictClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if sandbox, ok := obj.(*sandboxv1alpha1.Sandbox); ok {
		if c.conflictCount < c.maxConflicts {
			c.conflictCount++
			return k8errors.NewConflict(sandboxv1alpha1.Resource("sandboxes"), sandbox.Name, fmt.Errorf("simulated conflict"))
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestSandboxClaimWarmPoolPolicy(t *testing.T) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
				},
			},
		},
	}

	baseClaim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
		},
	}

	warmPoolUID := types.UID("warmpool-uid-123")

	createWarmPoolSandbox := func(name, poolName string, ready bool) *sandboxv1alpha1.Sandbox {
		conditionStatus := metav1.ConditionFalse
		if ready {
			conditionStatus = metav1.ConditionTrue
		}
		replicas := int32(1)
		return &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					warmPoolSandboxLabel:   sandboxcontrollers.NameHash(poolName),
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
						Kind:       "SandboxWarmPool",
						Name:       poolName,
						UID:        warmPoolUID,
						Controller: ptr.To(true),
					},
				},
			},
			Spec: sandboxv1alpha1.SandboxSpec{
				Replicas: &replicas,
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
					},
				},
			},
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:   string(sandboxv1alpha1.SandboxConditionReady),
						Status: conditionStatus,
						Reason: "DependenciesReady",
					},
				},
			},
		}
	}

	t.Run("skips warm pool when policy is none", func(t *testing.T) {
		scheme := newScheme(t)
		claimWithNone := baseClaim.DeepCopy()
		warmPoolNone := extensionsv1alpha1.WarmPoolPolicyNone
		claimWithNone.Spec.WarmPool = &warmPoolNone

		existingObjects := []client.Object{
			template,
			claimWithNone,
			createWarmPoolSandbox("pool-sb-1", "test-pool", true),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(existingObjects...).
			WithStatusSubresource(claimWithNone).
			Build()

		reconciler := &SandboxClaimReconciler{
			Client:   fakeClient,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
			Tracer:   asmetrics.NewNoOp(),
		}

		req := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"},
		}

		ctx := context.Background()
		_, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify a NEW sandbox was created (cold start, not adopted)
		var sandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, req.NamespacedName, &sandbox); err != nil {
			t.Fatalf("expected sandbox to be created but got error: %v", err)
		}

		// Verify the warm pool sandbox was NOT adopted (labels should still be present)
		var poolSandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool-sb-1", Namespace: "default"}, &poolSandbox); err != nil {
			t.Fatalf("failed to get pool sandbox: %v", err)
		}

		if _, exists := poolSandbox.Labels[warmPoolSandboxLabel]; !exists {
			t.Error("expected warm pool label to still be present on non-adopted sandbox")
		}
		if _, exists := poolSandbox.Labels[sandboxTemplateRefHash]; !exists {
			t.Error("expected template ref label to still be present on non-adopted sandbox")
		}
	})

	t.Run("adopts from specific warm pool only", func(t *testing.T) {
		scheme := newScheme(t)
		claimWithSpecificPool := baseClaim.DeepCopy()
		specificPool := extensionsv1alpha1.WarmPoolPolicy("test-pool")
		claimWithSpecificPool.Spec.WarmPool = &specificPool

		existingObjects := []client.Object{
			template,
			claimWithSpecificPool,
			createWarmPoolSandbox("pool1-sb", "test-pool", true),
			createWarmPoolSandbox("pool2-sb", "other-pool", true),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(existingObjects...).
			WithStatusSubresource(claimWithSpecificPool).
			Build()

		reconciler := &SandboxClaimReconciler{
			Client:   fakeClient,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
			Tracer:   asmetrics.NewNoOp(),
		}

		req := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"},
		}

		ctx := context.Background()
		_, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify sandbox from "test-pool" was adopted (labels removed, owned by claim)
		var adoptedSandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool1-sb", Namespace: "default"}, &adoptedSandbox); err != nil {
			t.Fatalf("failed to get adopted sandbox: %v", err)
		}

		if _, exists := adoptedSandbox.Labels[warmPoolSandboxLabel]; exists {
			t.Error("expected warm pool label to be removed from adopted sandbox")
		}

		controllerRef := metav1.GetControllerOf(&adoptedSandbox)
		if controllerRef == nil || controllerRef.UID != claimWithSpecificPool.UID {
			t.Errorf("expected adopted sandbox to be controlled by claim, got %v", controllerRef)
		}

		// Verify sandbox from "other-pool" was NOT adopted (labels still present)
		var otherPoolSandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool2-sb", Namespace: "default"}, &otherPoolSandbox); err != nil {
			t.Fatalf("failed to get other pool sandbox: %v", err)
		}

		if _, exists := otherPoolSandbox.Labels[warmPoolSandboxLabel]; !exists {
			t.Error("expected warm pool label to still be present on non-adopted sandbox from other pool")
		}
	})

	t.Run("falls back to cold start when specific pool has no sandboxes", func(t *testing.T) {
		scheme := newScheme(t)
		claimWithSpecificPool := baseClaim.DeepCopy()
		specificPool := extensionsv1alpha1.WarmPoolPolicy("nonexistent-pool")
		claimWithSpecificPool.Spec.WarmPool = &specificPool

		existingObjects := []client.Object{
			template,
			claimWithSpecificPool,
			createWarmPoolSandbox("pool-sb-1", "test-pool", true),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(existingObjects...).
			WithStatusSubresource(claimWithSpecificPool).
			Build()

		reconciler := &SandboxClaimReconciler{
			Client:   fakeClient,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
			Tracer:   asmetrics.NewNoOp(),
		}

		req := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"},
		}

		ctx := context.Background()
		_, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify a new sandbox was created via cold start
		var sandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, req.NamespacedName, &sandbox); err != nil {
			t.Fatalf("expected sandbox to be created but got error: %v", err)
		}

		// Verify the existing pool sandbox was NOT adopted
		var poolSandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool-sb-1", Namespace: "default"}, &poolSandbox); err != nil {
			t.Fatalf("failed to get pool sandbox: %v", err)
		}
		if _, exists := poolSandbox.Labels[warmPoolSandboxLabel]; !exists {
			t.Error("expected warm pool label to still be present on non-adopted sandbox")
		}
	})

	t.Run("default policy adopts from any matching warm pool", func(t *testing.T) {
		scheme := newScheme(t)
		claimWithDefault := baseClaim.DeepCopy()
		defaultPolicy := extensionsv1alpha1.WarmPoolPolicyDefault
		claimWithDefault.Spec.WarmPool = &defaultPolicy

		existingObjects := []client.Object{
			template,
			claimWithDefault,
			createWarmPoolSandbox("pool-sb-1", "test-pool", true),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(existingObjects...).
			WithStatusSubresource(claimWithDefault).
			Build()

		reconciler := &SandboxClaimReconciler{
			Client:   fakeClient,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
			Tracer:   asmetrics.NewNoOp(),
		}

		req := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"},
		}

		ctx := context.Background()
		_, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify the warm pool sandbox was adopted
		var adoptedSandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool-sb-1", Namespace: "default"}, &adoptedSandbox); err != nil {
			t.Fatalf("failed to get adopted sandbox: %v", err)
		}

		if _, exists := adoptedSandbox.Labels[warmPoolSandboxLabel]; exists {
			t.Error("expected warm pool label to be removed from adopted sandbox")
		}

		controllerRef := metav1.GetControllerOf(&adoptedSandbox)
		if controllerRef == nil || controllerRef.UID != claimWithDefault.UID {
			t.Errorf("expected adopted sandbox to be controlled by claim, got %v", controllerRef)
		}
	})

	t.Run("nil warmpool field uses default behavior", func(t *testing.T) {
		scheme := newScheme(t)
		claimWithNil := baseClaim.DeepCopy()
		// WarmPool is nil by default

		existingObjects := []client.Object{
			template,
			claimWithNil,
			createWarmPoolSandbox("pool-sb-1", "test-pool", true),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(existingObjects...).
			WithStatusSubresource(claimWithNil).
			Build()

		reconciler := &SandboxClaimReconciler{
			Client:   fakeClient,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
			Tracer:   asmetrics.NewNoOp(),
		}

		req := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"},
		}

		ctx := context.Background()
		_, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify the warm pool sandbox was adopted (nil = default = adopt from any)
		var adoptedSandbox sandboxv1alpha1.Sandbox
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool-sb-1", Namespace: "default"}, &adoptedSandbox); err != nil {
			t.Fatalf("failed to get adopted sandbox: %v", err)
		}

		if _, exists := adoptedSandbox.Labels[warmPoolSandboxLabel]; exists {
			t.Error("expected warm pool label to be removed from adopted sandbox")
		}
	})
}

func TestSandboxClaimPredicates(t *testing.T) {
	r := &SandboxClaimReconciler{}
	pred := r.getTimingPredicate()

	testCases := []struct {
		name        string
		trigger     func(p predicate.Predicate) bool
		expectedKey types.NamespacedName
	}{
		{
			name: "CreateFunc stores time",
			trigger: func(p predicate.Predicate) bool {
				return p.Create(event.CreateEvent{
					Object: &extensionsv1alpha1.SandboxClaim{
						ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default"},
					},
				})
			},
			expectedKey: types.NamespacedName{Name: "test-claim", Namespace: "default"},
		},
		{
			name: "UpdateFunc stores time",
			trigger: func(p predicate.Predicate) bool {
				return p.Update(event.UpdateEvent{
					ObjectNew: &extensionsv1alpha1.SandboxClaim{
						ObjectMeta: metav1.ObjectMeta{Name: "test-claim-update", Namespace: "default"},
					},
				})
			},
			expectedKey: types.NamespacedName{Name: "test-claim-update", Namespace: "default"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r.observedTimes = sync.Map{} // Reset map for each test case
			res := tc.trigger(pred)
			if !res {
				t.Error("expected predicate to return true")
			}

			if _, ok := r.observedTimes.Load(tc.expectedKey); !ok {
				t.Errorf("expected time to be stored in observedTimes map for key %v", tc.expectedKey)
			}
		})
	}
}
