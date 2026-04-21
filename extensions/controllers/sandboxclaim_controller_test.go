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
				APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim", Name: "test-claim", UID: "claim-uid", Controller: new(true),
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
	controlledSandboxWithDefault.Spec.PodTemplate.Spec.AutomountServiceAccountToken = new(false)

	templateWithAutomount := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "automount-template", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{AutomountServiceAccountToken: new(true), Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}}},
			},
		},
	}

	claimForAutomount := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "automount-claim", Namespace: "default", UID: "claim-uid-automount"},
		Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "automount-template"}},
	}

	templateWithEnv := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-env", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container", Image: "test-image", Env: []corev1.EnvVar{{Name: "EXISTING_VAR", Value: "template-value"}}}},
				},
			},
		},
	}

	templateWithEnvOverride := templateWithEnv.DeepCopy()
	templateWithEnvOverride.Name = "test-template-env-override"
	templateWithEnvOverride.Spec.EnvVarsInjectionPolicy = extensionsv1alpha1.EnvVarsInjectionPolicyOverrides

	templateWithEnvAllowed := templateWithEnv.DeepCopy()
	templateWithEnvAllowed.Name = "test-template-env-allowed"
	templateWithEnvAllowed.Spec.EnvVarsInjectionPolicy = extensionsv1alpha1.EnvVarsInjectionPolicyAllowed

	nonePolicy := extensionsv1alpha1.WarmPoolPolicyNone

	claimWithEnv := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env", Namespace: "default", UID: "claim-env-uid"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-env-override"},
			WarmPool:    &nonePolicy,
			Env:         []extensionsv1alpha1.EnvVar{{Name: "NEW_VAR", Value: "claim-value"}},
		},
	}

	claimWithNewEnvDisallowed := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-new-env-disallowed", Namespace: "default", UID: "claim-new-env-disallowed-uid"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
			WarmPool:    &nonePolicy,
			Env:         []extensionsv1alpha1.EnvVar{{Name: "NEW_VAR", Value: "claim-value"}},
		},
	}

	claimWithEnvConflict := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env-conflict", Namespace: "default", UID: "claim-env-conflict-uid"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-env"},
			WarmPool:    &nonePolicy,
			Env:         []extensionsv1alpha1.EnvVar{{Name: "EXISTING_VAR", Value: "claim-override-value"}},
		},
	}

	claimWithEnvOverride := claimWithEnvConflict.DeepCopy()
	claimWithEnvOverride.Name = "test-claim-env-override"
	claimWithEnvOverride.UID = "claim-env-override-uid"
	claimWithEnvOverride.Spec.TemplateRef.Name = "test-template-env-override"

	claimWithEnvAllowedSuccess := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env-allowed-success", Namespace: "default", UID: "claim-env-allowed-uid"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-env-allowed"},
			WarmPool:    &nonePolicy,
			Env:         []extensionsv1alpha1.EnvVar{{Name: "NEW_VAR_ALLOWED", Value: "claim-value"}},
		},
	}

	claimWithEnvOverrideNotAllowed := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env-override-not-allowed", Namespace: "default", UID: "claim-override-not-allowed-uid"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-env-allowed"},
			WarmPool:    &nonePolicy,
			Env:         []extensionsv1alpha1.EnvVar{{Name: "EXISTING_VAR", Value: "claim-override-value"}},
		},
	}

	templateMultiContainer := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-multi-container", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			EnvVarsInjectionPolicy: extensionsv1alpha1.EnvVarsInjectionPolicyOverrides,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app-container", Image: "app-image"},
						{Name: "sidecar-container", Image: "sidecar-image"},
					},
				},
			},
		},
	}

	claimTargetAppContainer := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-target-app", Namespace: "default", UID: "uid-target-app"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-multi-container"},
			WarmPool:    &nonePolicy,
			Env: []extensionsv1alpha1.EnvVar{
				{Name: "APP_ENV", Value: "injected", ContainerName: "app-container"},
			},
		},
	}

	claimTargetInvalid := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-target-invalid", Namespace: "default", UID: "uid-target-invalid"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-multi-container"},
			WarmPool:    &nonePolicy,
			Env: []extensionsv1alpha1.EnvVar{
				{Name: "INVALID_ENV", Value: "injected", ContainerName: "does-not-exist"},
			},
		},
	}

	templateWithInitContainer := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-init-container", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			EnvVarsInjectionPolicy: extensionsv1alpha1.EnvVarsInjectionPolicyOverrides,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init-setup", Image: "init-image"}},
					Containers:     []corev1.Container{{Name: "app-container", Image: "app-image"}},
				},
			},
		},
	}

	claimTargetInitContainer := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-target-init", Namespace: "default", UID: "uid-target-init"},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template-init-container"},
			WarmPool:    &nonePolicy,
			Env: []extensionsv1alpha1.EnvVar{
				{Name: "INIT_ENV", Value: "injected-init", ContainerName: "init-setup"},
			},
		},
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
		expectedSpec.AutomountServiceAccountToken = new(false)

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
		expectDeletedNP   string // Asserts this NP is completely gone
		expectRetainedNP  string // Asserts this NP survived the reconcile loop
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
			name:             "Existing NetworkPolicy is safely deleted (and controller survives) if SandboxTemplate is suddenly deleted",
			claimToReconcile: claim,
			existingObjects: []client.Object{
				&networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-claim-network-policy", // Matches the claim name
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim", Name: "test-claim", UID: "claim-uid", Controller: new(true),
						}},
					},
				},
			},
			expectSandbox: false, // Controller will fail to build sandbox, which is correct
			expectError:   false, // Controller survives the reconcile loop without crashing
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "TemplateNotFound",
				Message: `SandboxTemplate "test-template" not found`,
			},
			expectDeletedNP: "test-claim-network-policy", // Assert it was deleted
		},
		{
			name:             "Deprecated per-claim NetworkPolicy is aggressively deleted by Claim controller",
			claimToReconcile: claim,
			existingObjects: []client.Object{
				template,
				&networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-claim-network-policy",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim", Name: "test-claim", UID: "claim-uid", Controller: new(true),
						}},
					},
				},
			},
			expectSandbox: true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			expectDeletedNP: "test-claim-network-policy", // Assert it was deleted
		},
		{
			name:             "User-created NetworkPolicy with reserved name is PRESERVED because it lacks the claim OwnerReference",
			claimToReconcile: claim,
			existingObjects: []client.Object{
				template,
				&networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-claim-network-policy",
						Namespace: "default",
					},
				},
			},
			expectSandbox: true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			expectRetainedNP: "test-claim-network-policy", // Assert it survived the GC!
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
		{
			name: "sandbox is created with additional metadata from claim",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-with-meta", Namespace: "default", UID: "uid-meta"},
				Spec: extensionsv1alpha1.SandboxClaimSpec{
					TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
					AdditionalPodMetadata: sandboxv1alpha1.PodMetadata{
						Labels:      map[string]string{"user-label": "user-value"},
						Annotations: map[string]string{"user-annotation": "user-value"},
					},
				},
			},
			existingObjects: []client.Object{template},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				if val, ok := sandbox.Spec.PodTemplate.ObjectMeta.Labels["user-label"]; !ok || val != "user-value" {
					t.Errorf("expected user-label to be propagated, got %q", val)
				}
				if val, ok := sandbox.Spec.PodTemplate.ObjectMeta.Annotations["user-annotation"]; !ok || val != "user-value" {
					t.Errorf("expected user-annotation to be propagated, got %q", val)
				}
			},
		},
		{
			name: "claim with too long label value is rejected",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-long-label", Namespace: "default", UID: "uid-long-label"},
				Spec: extensionsv1alpha1.SandboxClaimSpec{
					TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
					AdditionalPodMetadata: sandboxv1alpha1.PodMetadata{
						Labels: map[string]string{"user-label": "a-very-long-value-that-exceeds-sixty-three-characters-limit-which-is-sixty-four"},
					},
				},
			},
			existingObjects: []client.Object{template},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name: "claim with invalid label pattern is rejected",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-invalid-label", Namespace: "default", UID: "uid-invalid-label"},
				Spec: extensionsv1alpha1.SandboxClaimSpec{
					TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
					AdditionalPodMetadata: sandboxv1alpha1.PodMetadata{
						Labels: map[string]string{"user-label": "invalid@value"},
					},
				},
			},
			existingObjects: []client.Object{template},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name: "claim with restricted domain label is rejected",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-restricted-label", Namespace: "default", UID: "uid-restricted-label"},
				Spec: extensionsv1alpha1.SandboxClaimSpec{
					TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
					AdditionalPodMetadata: sandboxv1alpha1.PodMetadata{
						Labels: map[string]string{"kubernetes.io/restricted": "value"},
					},
				},
			},
			existingObjects: []client.Object{template},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name:             "sandbox is created with injected environment variables from claim",
			claimToReconcile: claimWithEnv,
			existingObjects:  []client.Object{templateWithEnvOverride},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				env := sandbox.Spec.PodTemplate.Spec.Containers[0].Env
				if len(env) != 2 {
					t.Errorf("Expected 2 environment variables, got %d", len(env))
				}
				if env[0].Name != "EXISTING_VAR" || env[0].Value != "template-value" {
					t.Errorf("Expected EXISTING_VAR=template-value, got %s=%s", env[0].Name, env[0].Value)
				}
				if env[1].Name != "NEW_VAR" || env[1].Value != "claim-value" {
					t.Errorf("Expected NEW_VAR=claim-value, got %s=%s", env[1].Name, env[1].Value)
				}
			},
		},
		{
			name:             "sandbox is created with injected new environment variable when policy is Allowed",
			claimToReconcile: claimWithEnvAllowedSuccess,
			existingObjects:  []client.Object{templateWithEnvAllowed},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				env := sandbox.Spec.PodTemplate.Spec.Containers[0].Env
				if len(env) != 2 {
					t.Errorf("Expected 2 environment variables, got %d", len(env))
				}
				if env[0].Name != "EXISTING_VAR" || env[0].Value != "template-value" {
					t.Errorf("Expected EXISTING_VAR=template-value, got %s=%s", env[0].Name, env[0].Value)
				}
				if env[1].Name != "NEW_VAR_ALLOWED" || env[1].Value != "claim-value" {
					t.Errorf("Expected NEW_VAR_ALLOWED=claim-value, got %s=%s", env[1].Name, env[1].Value)
				}
			},
		},
		{
			name:             "sandbox creation fails when claim overrides environment variable and policy is Allowed (not Overrides)",
			claimToReconcile: claimWithEnvOverrideNotAllowed,
			existingObjects:  []client.Object{templateWithEnvAllowed},
			expectSandbox:    false,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: environment variable override is not allowed by the template policy for variable \"EXISTING_VAR\"",
			},
		},
		{
			name:             "sandbox creation fails when claim environment variable conflicts with template and override is not allowed",
			claimToReconcile: claimWithEnvConflict,
			existingObjects:  []client.Object{templateWithEnv},
			expectSandbox:    false,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: environment variable injection is not allowed by the template policy",
			},
		},
		{
			name:             "sandbox creation fails when claim injects new environment variable and policy is disallowed",
			claimToReconcile: claimWithNewEnvDisallowed,
			existingObjects:  []client.Object{template},
			expectSandbox:    false,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: environment variable injection is not allowed by the template policy",
			},
		},
		{
			name:             "sandbox is created with overridden environment variable when template allows override",
			claimToReconcile: claimWithEnvOverride,
			existingObjects:  []client.Object{templateWithEnvOverride},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				env := sandbox.Spec.PodTemplate.Spec.Containers[0].Env
				if len(env) != 1 {
					t.Errorf("Expected 1 environment variable, got %d", len(env))
				}
				if env[0].Name != "EXISTING_VAR" || env[0].Value != "claim-override-value" {
					t.Errorf("Expected EXISTING_VAR=claim-override-value, got %s=%s", env[0].Name, env[0].Value)
				}
			},
		},
		{
			name:             "sandbox is created with env var injected into specific container",
			claimToReconcile: claimTargetAppContainer,
			existingObjects:  []client.Object{templateMultiContainer},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				containers := sandbox.Spec.PodTemplate.Spec.Containers
				if len(containers) != 2 {
					t.Fatalf("Expected 2 containers, got %d", len(containers))
				}
				if len(containers[0].Env) != 1 {
					t.Fatalf("Expected 1 env var in app-container, got %d", len(containers[0].Env))
				}
				if containers[0].Env[0].Name != "APP_ENV" || containers[0].Env[0].Value != "injected" {
					t.Errorf("Expected APP_ENV=injected, got %s=%s", containers[0].Env[0].Name, containers[0].Env[0].Value)
				}
				if len(containers[1].Env) != 0 {
					t.Errorf("Expected 0 env vars in sidecar-container, got %d", len(containers[1].Env))
				}
			},
		},
		{
			name:             "sandbox creation fails when claim targets non-existent container",
			claimToReconcile: claimTargetInvalid,
			existingObjects:  []client.Object{templateMultiContainer},
			expectSandbox:    false,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: target container \"does-not-exist\" not found in template for environment variable \"INVALID_ENV\"",
			},
		},
		{
			name:             "sandbox is created with env var injected into init container",
			claimToReconcile: claimTargetInitContainer,
			existingObjects:  []client.Object{templateWithInitContainer},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1alpha1.Sandbox, _ *extensionsv1alpha1.SandboxTemplate) {
				initContainers := sandbox.Spec.PodTemplate.Spec.InitContainers
				if len(initContainers) != 1 {
					t.Fatalf("Expected 1 init container, got %d", len(initContainers))
				}
				if len(initContainers[0].Env) != 1 {
					t.Fatalf("Expected 1 env var in init-setup, got %d", len(initContainers[0].Env))
				}
				if initContainers[0].Env[0].Name != "INIT_ENV" || initContainers[0].Env[0].Value != "injected-init" {
					t.Errorf("Expected INIT_ENV=injected-init, got %s=%s", initContainers[0].Env[0].Name, initContainers[0].Env[0].Value)
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
			if tc.expectedCondition.Reason == "ReconcilerError" || tc.expectedCondition.Reason == "InvalidMetadata" {
				if condition.Reason != tc.expectedCondition.Reason {
					t.Errorf("expected condition reason %q, got %q", tc.expectedCondition.Reason, condition.Reason)
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

			// Assert NetworkPolicy Cleanup and Preservation
			if tc.expectDeletedNP != "" {
				var np networkingv1.NetworkPolicy
				err := client.Get(context.Background(), types.NamespacedName{Name: tc.expectDeletedNP, Namespace: "default"}, &np)
				if !k8errors.IsNotFound(err) {
					t.Errorf("expected NetworkPolicy %q to be DELETED, but it was found or got err: %v", tc.expectDeletedNP, err)
				}
			}

			if tc.expectRetainedNP != "" {
				var np networkingv1.NetworkPolicy
				err := client.Get(context.Background(), types.NamespacedName{Name: tc.expectRetainedNP, Namespace: "default"}, &np)
				if err != nil {
					t.Errorf("expected NetworkPolicy %q to be RETAINED, but it was missing or got err: %v", tc.expectRetainedNP, err)
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
					{APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim", Name: claimName, UID: types.UID(claimName), Controller: new(true)},
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
		name                       string
		claim                      *extensionsv1alpha1.SandboxClaim
		sandboxIsExpired           bool
		isWarmPool                 bool
		sandboxNotOwned            bool // sandbox exists at statusName but belongs to a different owner
		expectClaimDeleted         bool
		expectSandboxDeleted       bool
		expectSandboxStatusCleared bool // SandboxStatus.Name and PodIPs must be empty
		expectStatus               string
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
			name:                 "Policy=Retain (Sandbox from Warm Pool) -> Should Retain Claim but DELETE Sandbox",
			claim:                createClaim("retain-claim-warm-pool", extensionsv1alpha1.ShutdownPolicyRetain),
			sandboxIsExpired:     false,
			isWarmPool:           true,
			expectClaimDeleted:   false,
			expectSandboxDeleted: true, // Controller explicitly deletes Sandbox here.
			expectStatus:         extensionsv1alpha1.ClaimExpiredReason,
		},
		{
			name:                       "Policy=Retain, Sandbox not owned by claim -> skip deletion, SandboxStatus cleared",
			claim:                      createClaim("retain-claim-unowned", extensionsv1alpha1.ShutdownPolicyRetain),
			sandboxNotOwned:            true,
			expectClaimDeleted:         false,
			expectSandboxDeleted:       false,
			expectSandboxStatusCleared: true,
			expectStatus:               extensionsv1alpha1.ClaimExpiredReason,
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

			// Hack: Simulate warmPool adopted sandbox
			if tc.isWarmPool {
				sandbox.Name = "warm-pool-sandbox-adopted"
				tc.claim.Status.SandboxStatus.Name = sandbox.Name
			}

			// Simulate a sandbox that exists at statusName but belongs to a different owner.
			if tc.sandboxNotOwned {
				sandbox.Name = "foreign-sandbox"
				sandbox.OwnerReferences = []metav1.OwnerReference{
					{APIVersion: "extensions.agents.x-k8s.io/v1alpha1", Kind: "SandboxClaim", Name: "other-claim", UID: "other-uid", Controller: func() *bool { b := true; return &b }()},
				}
				tc.claim.Status.SandboxStatus.Name = sandbox.Name
			}

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

				if tc.expectSandboxStatusCleared {
					if fetchedClaim.Status.SandboxStatus.Name != "" {
						t.Errorf("expected SandboxStatus.Name to be empty, got %q", fetchedClaim.Status.SandboxStatus.Name)
					}
					if fetchedClaim.Status.SandboxStatus.PodIPs != nil {
						t.Errorf("expected SandboxStatus.PodIPs to be nil, got %v", fetchedClaim.Status.SandboxStatus.PodIPs)
					}
				}
			}

			// 2. Verify Sandbox
			var fetchedSandbox sandboxv1alpha1.Sandbox

			// The Sandbox might now have different name than the claim!
			err = client.Get(context.Background(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, &fetchedSandbox)

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
						Controller: new(true),
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
						Controller: new(true),
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
		expectedAnnotations     map[string]string
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
			name: "corrects stale pod-name annotation when adopting sandbox",
			existingObjects: []client.Object{
				template,
				claim,
				func() client.Object {
					sb := createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true)
					sb.Annotations = map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "stale-pod-name",
					}
					return sb
				}(),
				createWarmPoolSandbox("pool-sb-2", metav1.Time{Time: metav1.Now().Add(-30 * time.Minute)}, true),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "pool-sb-1",
			expectNewSandboxCreated: false,
		},
		{
			name: "accepts existing correct pod-name annotation when adopting sandbox",
			existingObjects: []client.Object{
				template,
				claim,
				func() client.Object {
					sb := createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true)
					sb.Annotations = map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "pool-sb-1",
						"test.annotation/preserved":              "true",
					}
					return sb
				}(),
				createWarmPoolSandbox("pool-sb-2", metav1.Time{Time: metav1.Now().Add(-30 * time.Minute)}, true),
			},
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "pool-sb-1",
			expectedAnnotations: map[string]string{
				"test.annotation/preserved": "true",
			},
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

				// 4. Verify the adopted sandbox records the adopted pod name
				if val := adoptedSandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; val != adoptedSandbox.Name {
					t.Errorf("expected adopted sandbox to have %q annotation %q, got %q; annotations=%v", sandboxv1alpha1.SandboxPodNameAnnotation, adoptedSandbox.Name, val, adoptedSandbox.Annotations)
				}

				for key, expected := range tc.expectedAnnotations {
					if val := adoptedSandbox.Annotations[key]; val != expected {
						t.Errorf("expected adopted sandbox to preserve annotation %q=%q, got %q; annotations=%v", key, expected, val, adoptedSandbox.Annotations)
					}
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
				Name: "test-claim", UID: "claim-uid", Controller: new(true),
			}},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas:    new(int32(1)),
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
			Replicas:    new(int32(1)),
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
						Controller: new(true),
					},
				},
			},
			Spec: sandboxv1alpha1.SandboxSpec{
				Replicas:    new(int32(1)),
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
						Controller: new(true),
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

	t.Run("errors when custom environment variables are provided with a warm pool", func(t *testing.T) {
		scheme := newScheme(t)
		claimWithEnv := baseClaim.DeepCopy()
		defaultPolicy := extensionsv1alpha1.WarmPoolPolicyDefault
		claimWithEnv.Spec.WarmPool = &defaultPolicy
		claimWithEnv.Spec.Env = []extensionsv1alpha1.EnvVar{{Name: "CUSTOM_ENV", Value: "test-value"}}

		existingObjects := []client.Object{
			template,
			claimWithEnv,
			createWarmPoolSandbox("pool-sb-1", "test-pool", true),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(existingObjects...).
			WithStatusSubresource(claimWithEnv).
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
		if err == nil {
			t.Fatalf("expected reconcile to fail with an error, but it succeeded")
		}

		expectedErr := "custom environment variables are not supported when using a warm pool"
		if err.Error() != expectedErr {
			t.Errorf("expected error %q, got %q", expectedErr, err.Error())
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
