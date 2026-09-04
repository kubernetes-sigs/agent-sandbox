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
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func TestSandboxClaimReconcile(t *testing.T) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
			},
		}},
		},
	}

	templateWithNP := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-with-np",
			Namespace: "default",
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
						Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
					},
				},
			},
		}}, NetworkPolicy: &extensionsv1beta1.NetworkPolicySpec{
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

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	warmPoolWithNP := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-with-np", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-with-np"}},
	}

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
	}

	uncontrolledSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-claim", Namespace: "default",
			Annotations: map[string]string{sandboxv1beta1.SandboxTemplateRefAnnotation: "test-template"},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
			},
			Spec: template.Spec.PodTemplate.Spec,
		}},
		},
	}

	controlledSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-claim", Namespace: "default",
			Annotations: map[string]string{sandboxv1beta1.SandboxTemplateRefAnnotation: "test-template"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxClaimKind,
				Name:       "test-claim",
				UID:        "claim-uid",
				Controller: new(true),
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
			},
			Spec: template.Spec.PodTemplate.Spec,
		}},
		},
	}

	controlledSandbox.Spec.PodTemplate.Spec.DNSPolicy = corev1.DNSNone
	controlledSandbox.Spec.PodTemplate.Spec.DNSConfig = &corev1.PodDNSConfig{
		Nameservers: []string{"8.8.8.8", "1.1.1.1"},
	}

	controlledSandboxWithDefault := controlledSandbox.DeepCopy()
	controlledSandboxWithDefault.Spec.PodTemplate.Spec.AutomountServiceAccountToken = new(false)

	templateWithAutomount := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "automount-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{AutomountServiceAccountToken: new(true), Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}}},
		}},
		},
	}

	warmPoolForAutomount := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "automount-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "automount-template"}},
	}

	claimForAutomount := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "automount-claim", Namespace: "default", UID: "claim-uid-automount"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "automount-warmpool"}},
	}

	templateWithEnv := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-env", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test-container", Image: "test-image", Env: []corev1.EnvVar{{Name: "EXISTING_VAR", Value: "template-value"}}}},
			},
		}},
		},
	}

	templateWithEnvOverride := templateWithEnv.DeepCopy()
	templateWithEnvOverride.Name = "test-template-env-override"
	templateWithEnvOverride.Spec.EnvVarsInjectionPolicy = extensionsv1beta1.EnvVarsInjectionPolicyOverrides

	templateWithEnvAllowed := templateWithEnv.DeepCopy()
	templateWithEnvAllowed.Name = "test-template-env-allowed"
	templateWithEnvAllowed.Spec.EnvVarsInjectionPolicy = extensionsv1beta1.EnvVarsInjectionPolicyAllowed

	warmPoolWithEnv := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-env", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-env"}},
	}

	warmPoolWithEnvOverride := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-env-override", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-env-override"}},
	}

	warmPoolWithEnvAllowed := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-env-allowed", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-env-allowed"}},
	}

	claimWithEnv := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env", Namespace: "default", UID: "claim-env-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-env-override"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "NEW_VAR", Value: "claim-value"}},
		},
	}

	claimWithNewEnvDisallowed := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-new-env-disallowed", Namespace: "default", UID: "claim-new-env-disallowed-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "NEW_VAR", Value: "claim-value"}},
		},
	}

	claimWithEnvConflict := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env-conflict", Namespace: "default", UID: "claim-env-conflict-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-env"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "EXISTING_VAR", Value: "claim-override-value"}},
		},
	}

	claimWithEnvOverride := claimWithEnvConflict.DeepCopy()
	claimWithEnvOverride.Name = "test-claim-env-override"
	claimWithEnvOverride.UID = "claim-env-override-uid"
	claimWithEnvOverride.Spec.WarmPoolRef.Name = "test-warmpool-env-override"

	claimWithEnvAllowedSuccess := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env-allowed-success", Namespace: "default", UID: "claim-env-allowed-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-env-allowed"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "NEW_VAR_ALLOWED", Value: "claim-value"}},
		},
	}

	templateWithEnvAllowedAndEnvFrom := templateWithEnvAllowed.DeepCopy()
	templateWithEnvAllowedAndEnvFrom.Name = "test-template-env-allowed-envfrom"
	templateWithEnvAllowedAndEnvFrom.Spec.PodTemplate.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
		{
			ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "some-configmap"},
			},
		},
	}

	warmPoolWithEnvAllowedAndEnvFrom := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-env-allowed-envfrom", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-env-allowed-envfrom"}},
	}

	claimWithEnvAllowedAndEnvFrom := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env-allowed-envfrom", Namespace: "default", UID: "claim-env-allowed-envfrom-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-env-allowed-envfrom"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "NEW_VAR_ALLOWED", Value: "claim-value"}},
		},
	}

	templateWithInitEnvFromAllowed := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-init-envfrom-allowed", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			EnvVarsInjectionPolicy: extensionsv1beta1.EnvVarsInjectionPolicyAllowed,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						InitContainers: []corev1.Container{
							{
								Name:  "init-setup",
								Image: "init-image",
								EnvFrom: []corev1.EnvFromSource{
									{
										ConfigMapRef: &corev1.ConfigMapEnvSource{
											LocalObjectReference: corev1.LocalObjectReference{Name: "some-configmap"},
										},
									},
								},
							},
						},
						Containers: []corev1.Container{{Name: "app-container", Image: "app-image"}},
					},
				},
			},
		},
	}

	warmPoolWithInitEnvFromAllowed := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-init-envfrom-allowed", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-init-envfrom-allowed"}},
	}

	claimWithInitEnvFromAllowed := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-init-envfrom-allowed", Namespace: "default", UID: "claim-init-envfrom-allowed-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-init-envfrom-allowed"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "NEW_VAR_ALLOWED", Value: "claim-value", ContainerName: "init-setup"}},
		},
	}

	claimWithInitEnvFromAllowedButInjectsToApp := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-init-envfrom-allowed-app", Namespace: "default", UID: "claim-init-envfrom-allowed-app-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-init-envfrom-allowed"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "NEW_VAR_ALLOWED", Value: "claim-value", ContainerName: "app-container"}},
		},
	}

	claimWithNoEnvAndEnvFrom := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-no-env-envfrom", Namespace: "default", UID: "claim-no-env-envfrom-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-env-allowed-envfrom"},
		},
	}

	claimWithEnvOverrideNotAllowed := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-env-override-not-allowed", Namespace: "default", UID: "claim-override-not-allowed-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-env-allowed"},
			Env:         []extensionsv1beta1.EnvVar{{Name: "EXISTING_VAR", Value: "claim-override-value"}},
		},
	}

	templateMultiContainer := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-multi-container", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app-container", Image: "app-image"},
					{Name: "sidecar-container", Image: "sidecar-image"},
				},
			},
		}}, EnvVarsInjectionPolicy: extensionsv1beta1.EnvVarsInjectionPolicyOverrides,
		},
	}

	warmPoolMultiContainer := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-multi-container", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-multi-container"}},
	}

	claimTargetAppContainer := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-target-app", Namespace: "default", UID: "uid-target-app"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-multi-container"},
			Env: []extensionsv1beta1.EnvVar{
				{Name: "APP_ENV", Value: "injected", ContainerName: "app-container"},
			},
		},
	}

	claimTargetInvalid := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-target-invalid", Namespace: "default", UID: "uid-target-invalid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-multi-container"},
			Env: []extensionsv1beta1.EnvVar{
				{Name: "INVALID_ENV", Value: "injected", ContainerName: "does-not-exist"},
			},
		},
	}

	templateWithInitContainer := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template-init-container", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "init-setup", Image: "init-image"}},
				Containers:     []corev1.Container{{Name: "app-container", Image: "app-image"}},
			},
		}}, EnvVarsInjectionPolicy: extensionsv1beta1.EnvVarsInjectionPolicyOverrides,
		},
	}

	warmPoolWithInitContainer := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-init-container", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-init-container"}},
	}

	claimTargetInitContainer := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-target-init", Namespace: "default", UID: "uid-target-init"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-init-container"},
			Env: []extensionsv1beta1.EnvVar{
				{Name: "INIT_ENV", Value: "injected-init", ContainerName: "init-setup"},
			},
		},
	}

	readySandbox := controlledSandboxWithDefault.DeepCopy()
	readySandbox.Status.Conditions = []metav1.Condition{{
		Type:    string(sandboxv1beta1.SandboxConditionReady),
		Status:  metav1.ConditionTrue,
		Reason:  "SandboxReady",
		Message: "Sandbox is ready",
	}}
	readySandbox.Status.PodIPs = []string{"10.244.0.6"}
	readySandbox.Status.ServiceFQDN = "test-claim.default.svc.cluster.local"

	// Ready sandbox with no Service: PodIPs are set but the Sandbox controller
	// never wrote a ServiceFQDN. The claim must mirror the empty value, not
	// fabricate or retain one.
	readySandboxNoService := readySandbox.DeepCopy()
	readySandboxNoService.Status.ServiceFQDN = ""

	// Validation Functions
	validateSandboxHasDefaultAutomountToken := func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, template *extensionsv1beta1.SandboxTemplate) {
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

	validateSandboxAutomountTrue := func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
		if sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken == nil || !*sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken {
			t.Error("expected AutomountServiceAccountToken to be true")
		}
	}

	validateSandboxDNSUntouched := func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
		// Prove that the air-gapped fix works: DNS should not be overridden!
		if sandbox.Spec.PodTemplate.Spec.DNSPolicy == corev1.DNSNone {
			t.Errorf("Expected DNSPolicy to remain untouched, but it was set to None")
		}
		if sandbox.Spec.PodTemplate.Spec.DNSConfig != nil {
			t.Errorf("Expected DNSConfig to be nil, but got %v", sandbox.Spec.PodTemplate.Spec.DNSConfig)
		}
	}

	testCases := []struct {
		name                string
		claimToReconcile    *extensionsv1beta1.SandboxClaim
		existingObjects     []client.Object
		allowedDomains      []string
		expectSandbox       bool
		expectError         bool
		expectedCondition   metav1.Condition
		expectedPodIPs      []string
		expectedServiceFQDN string
		validateSandbox     func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, template *extensionsv1beta1.SandboxTemplate)
		expectDeletedNP     string // Asserts this NP is completely gone
		expectRetainedNP    string // Asserts this NP survived the reconcile loop
	}{
		{
			name:             "sandbox is created when a claim is made",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, warmPool},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: validateSandboxHasDefaultAutomountToken,
		},
		{
			name:             "sandbox is created with automount token enabled",
			claimToReconcile: claimForAutomount,
			existingObjects:  []client.Object{templateWithAutomount, warmPoolForAutomount},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: validateSandboxAutomountTrue,
		},
		{
			name:             "sandbox is not created when template is not found",
			claimToReconcile: claim,
			existingObjects:  []client.Object{warmPool},
			expectSandbox:    false,
			expectError:      false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "TemplateNotFound", Message: `SandboxTemplate "test-template" not found`,
			},
		},
		{
			name:             "sandbox is not created when warmpool is not found",
			claimToReconcile: claim,
			existingObjects:  []client.Object{},
			expectSandbox:    false,
			expectError:      false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "WarmPoolNotFound", Message: `SandboxWarmPool "test-warmpool" not found`,
			},
		},
		{
			name:             "sandbox exists but is not controlled by claim",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, warmPool, uncontrolledSandbox},
			expectSandbox:    true,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: sandbox \"test-claim\" is not controlled by claim \"test-claim\". Please use a different claim name or delete the sandbox manually",
			},
		},
		{
			name:             "sandbox exists and is controlled by claim",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, warmPool, controlledSandboxWithDefault},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, template *extensionsv1beta1.SandboxTemplate) {
				validateSandboxHasDefaultAutomountToken(t, sandbox, template)

				expectedHash := SandboxTemplateRefHash(template.Name)
				if val := sandbox.Labels[sandboxTemplateRefHash]; val != expectedHash {
					t.Errorf("expected Sandbox metadata to have label %q=%q, got %q", sandboxTemplateRefHash, expectedHash, val)
				}
			},
		},
		{
			name:             "sandbox exists but template is not found",
			claimToReconcile: claim,
			existingObjects:  []client.Object{warmPool, readySandbox},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "SandboxReady", Message: "Sandbox is ready",
			},
			expectedPodIPs:      []string{"10.244.0.6"},
			expectedServiceFQDN: "test-claim.default.svc.cluster.local",
			validateSandbox:     validateSandboxHasDefaultAutomountToken,
		},
		{
			name:             "sandbox is ready",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, warmPool, readySandbox},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "SandboxReady", Message: "Sandbox is ready",
			},
			expectedPodIPs:      []string{"10.244.0.6"},
			expectedServiceFQDN: "test-claim.default.svc.cluster.local",
			validateSandbox:     validateSandboxHasDefaultAutomountToken,
		},
		{
			name:             "sandbox is ready but has no service, mirrored ServiceFQDN stays empty",
			claimToReconcile: claim,
			existingObjects:  []client.Object{template, warmPool, readySandboxNoService},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "SandboxReady", Message: "Sandbox is ready",
			},
			expectedPodIPs:      []string{"10.244.0.6"},
			expectedServiceFQDN: "",
		},
		{
			name: "sandbox is created with network policy enabled",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim-np", Namespace: "default", UID: "claim-np-uid"},
				Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool-with-np"}},
			},
			existingObjects: []client.Object{templateWithNP, warmPoolWithNP},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: validateSandboxDNSUntouched,
		},
		{
			name: "Scenario A: Creates Default Secure Policy (Strict Isolation) when template has none",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-default-np", Namespace: "default", UID: "uid-default-np"},
				Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1beta1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
				expectedHash := SandboxTemplateRefHash("test-template")
				if val := sandbox.Labels[sandboxTemplateRefHash]; val != expectedHash {
					t.Errorf("expected Sandbox metadata to have label %q=%q, got %q", sandboxTemplateRefHash, expectedHash, val)
				}

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
				warmPool,
				&networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-claim-network-policy", // Matches the claim name
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxClaimKind,
							Name:       "test-claim",
							UID:        "claim-uid",
							Controller: new(true),
						}},
					},
				},
			},
			expectSandbox: false, // Controller will fail to build sandbox, which is correct
			expectError:   false, // Controller survives the reconcile loop without crashing
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1beta1.SandboxConditionReady),
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
				warmPool,
				&networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-claim-network-policy",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind, Name: "test-claim", UID: "claim-uid", Controller: new(true),
						}},
					},
				},
			},
			expectSandbox: true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1beta1.SandboxConditionReady),
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
				warmPool,
				&networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-claim-network-policy",
						Namespace: "default",
					},
				},
			},
			expectSandbox: true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1beta1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			expectRetainedNP: "test-claim-network-policy", // Assert it survived the GC!
		},
		{
			name: "trace context is propagated from claim to sandbox",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "trace-claim", Namespace: "default", UID: "trace-uid",
					Annotations: map[string]string{asmetrics.TraceContextAnnotation: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
				if val, ok := sandbox.Annotations[asmetrics.TraceContextAnnotation]; !ok || val != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
					t.Errorf("expected trace context annotation to be propagated, got %q", val)
				}
			},
		},
		{
			name: "sandbox is created with additional metadata from claim",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-with-meta", Namespace: "default", UID: "uid-meta"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels:      map[string]string{"sandbox.users.io/user-label": "user-value"},
						Annotations: map[string]string{"user-annotation": "user-value"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
				if val, ok := sandbox.Spec.PodTemplate.ObjectMeta.Labels["sandbox.users.io/user-label"]; !ok || val != "user-value" {
					t.Errorf("expected sandbox.users.io/user-label to be propagated, got %q", val)
				}
				if val, ok := sandbox.Spec.PodTemplate.ObjectMeta.Annotations["user-annotation"]; !ok || val != "user-value" {
					t.Errorf("expected user-annotation to be propagated, got %q", val)
				}
			},
		},
		{
			name: "claim with label without domain is rejected",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-no-domain-label", Namespace: "default", UID: "uid-no-domain-label"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"label-without-domain": "value"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name: "claim with too long label value is rejected",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-long-label", Namespace: "default", UID: "uid-long-label"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"sandbox.users.io/user-label": "a-very-long-value-that-exceeds-sixty-three-characters-limit-which-is-sixty-four"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name: "claim with invalid label pattern is rejected",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-invalid-label", Namespace: "default", UID: "uid-invalid-label"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"sandbox.users.io/user-label": "invalid@value"},
					},
				},
			},
			existingObjects: []client.Object{template},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name: "claim with invalid label key is rejected",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-invalid-key", Namespace: "default", UID: "uid-invalid-key"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"sandbox.users.io/invalid@key": "value"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name: "claim with restricted domain label is rejected",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-restricted-label", Namespace: "default", UID: "uid-restricted-label"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"kubernetes.io/restricted": "value"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "InvalidMetadata",
			},
		},
		{
			name: "claim with safe-to-evict annotation is accepted",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-safe-to-evict", Namespace: "default", UID: "uid-safe-to-evict"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Annotations: map[string]string{"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
				if val, ok := sandbox.Spec.PodTemplate.ObjectMeta.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]; !ok || val != "false" {
					t.Errorf("expected cluster-autoscaler.kubernetes.io/safe-to-evict to be propagated, got %q", val)
				}
			},
		},
		{
			name: "claim with spoofed router app label is rejected",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-spoofed-app-label", Namespace: "default", UID: "uid-spoofed-app-label"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"app": "sandbox-router"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			expectSandbox:   false,
			expectError:     false,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1beta1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "InvalidMetadata",
				Message: "invalid additionalPodMetadata: failed to validate label \"app\": restricted system label value: \"app\"=\"sandbox-router\" is not allowed in AdditionalPodMetadata",
			},
		},
		{
			name: "claim with custom allowed domain is accepted",
			claimToReconcile: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-custom-domain", Namespace: "default", UID: "uid-custom-domain"},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
					AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"custom.company.com/my-label": "my-value"},
					},
				},
			},
			existingObjects: []client.Object{template, warmPool},
			allowedDomains:  []string{"sandbox.users.io", "custom.company.com"},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
				if val, ok := sandbox.Spec.PodTemplate.ObjectMeta.Labels["custom.company.com/my-label"]; !ok || val != "my-value" {
					t.Errorf("expected custom.company.com/my-label to be propagated, got %q", val)
				}
			},
		},
		{
			name:             "sandbox is created with injected environment variables from claim",
			claimToReconcile: claimWithEnv,
			existingObjects:  []client.Object{templateWithEnvOverride, warmPoolWithEnvOverride},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
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
			existingObjects:  []client.Object{templateWithEnvAllowed, warmPoolWithEnvAllowed},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
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
			name:             "sandbox claim with Env bypasses available warm pool candidate",
			claimToReconcile: claimWithEnv,
			existingObjects: []client.Object{
				templateWithEnvOverride,
				&extensionsv1beta1.SandboxWarmPool{
					ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool-env-override", Namespace: "default", UID: "wp-env-override-uid"},
					Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template-env-override"}},
				},
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "adoptable-warm-sandbox",
						Namespace: "default",
						Labels: map[string]string{
							warmPoolSandboxLabel:   sandboxcontrollers.NameHash("test-warmpool-env-override"),
							sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template-env-override"),
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxWarmPoolKind,
							Name:       "test-warmpool-env-override",
							UID:        "wp-env-override-uid",
							Controller: new(true),
						}},
					},
				},
			},
			expectSandbox: true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
				if sandbox.Name != "test-claim-env" {
					t.Errorf("Expected newly created sandbox to have the claim's name 'test-claim-env' (bypassing warm pool candidate), got %q", sandbox.Name)
				}
			},
		},
		{
			name:             "sandbox creation fails when claim overrides environment variable and policy is Allowed (not Overrides)",
			claimToReconcile: claimWithEnvOverrideNotAllowed,
			existingObjects:  []client.Object{templateWithEnvAllowed, warmPoolWithEnvAllowed},
			expectSandbox:    false,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: environment variable override is not allowed by the template policy for variable \"EXISTING_VAR\"",
			},
		},
		{
			name:             "sandbox creation fails when claim environment variable conflicts with template and override is not allowed",
			claimToReconcile: claimWithEnvConflict,
			existingObjects:  []client.Object{templateWithEnv, warmPoolWithEnv},
			expectSandbox:    false,
			expectError:      false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "EnvVarsInjectionRejected", Message: "environment variable injection rejected: environment variable injection is not allowed by the template policy",
			},
		},
		{
			name:             "sandbox creation fails when claim injects new environment variable and policy is disallowed",
			claimToReconcile: claimWithNewEnvDisallowed,
			existingObjects:  []client.Object{template, warmPool},
			expectSandbox:    false,
			expectError:      false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "EnvVarsInjectionRejected", Message: "environment variable injection rejected: environment variable injection is not allowed by the template policy",
			},
		},
		{
			name:             "sandbox creation fails when claim injects env var but template has EnvFrom and policy is Allowed",
			claimToReconcile: claimWithEnvAllowedAndEnvFrom,
			existingObjects:  []client.Object{templateWithEnvAllowedAndEnvFrom, warmPoolWithEnvAllowedAndEnvFrom},
			expectSandbox:    false,
			expectError:      false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "EnvVarsInjectionRejected", Message: "environment variable injection rejected: container \"test-container\" uses EnvFrom sources; Allowed policy cannot safely prevent overriding EnvFrom-provided variables",
			},
		},
		{
			name:             "sandbox creation fails when claim injects env var but template has EnvFrom in init container and policy is Allowed",
			claimToReconcile: claimWithInitEnvFromAllowed,
			existingObjects:  []client.Object{templateWithInitEnvFromAllowed, warmPoolWithInitEnvFromAllowed},
			expectSandbox:    false,
			expectError:      false,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "EnvVarsInjectionRejected", Message: "environment variable injection rejected: container \"init-setup\" uses EnvFrom sources; Allowed policy cannot safely prevent overriding EnvFrom-provided variables",
			},
		},
		{
			name:             "sandbox is created when claim injects env var to a container without EnvFrom even if another container has EnvFrom",
			claimToReconcile: claimWithInitEnvFromAllowedButInjectsToApp,
			existingObjects:  []client.Object{templateWithInitEnvFromAllowed, warmPoolWithInitEnvFromAllowed},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
				env := sandbox.Spec.PodTemplate.Spec.Containers[0].Env
				if len(env) != 1 {
					t.Errorf("Expected 1 environment variable, got %d", len(env))
				}
				if env[0].Name != "NEW_VAR_ALLOWED" || env[0].Value != "claim-value" {
					t.Errorf("Expected NEW_VAR_ALLOWED=claim-value, got %s=%s", env[0].Name, env[0].Value)
				}
			},
		},
		{
			name:             "sandbox is created when template has EnvFrom but claim does not inject any env vars",
			claimToReconcile: claimWithNoEnvAndEnvFrom,
			existingObjects:  []client.Object{templateWithEnvAllowedAndEnvFrom, warmPoolWithEnvAllowedAndEnvFrom},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
		},
		{
			name:             "sandbox is created with overridden environment variable when template allows override",
			claimToReconcile: claimWithEnvOverride,
			existingObjects:  []client.Object{templateWithEnvOverride, warmPoolWithEnvOverride},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
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
			existingObjects:  []client.Object{templateMultiContainer, warmPoolMultiContainer},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
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
			existingObjects:  []client.Object{templateMultiContainer, warmPoolMultiContainer},
			expectSandbox:    false,
			expectError:      true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "ReconcilerError", Message: "Error seen: target container \"does-not-exist\" not found in template for environment variable \"INVALID_ENV\"",
			},
		},
		{
			name:             "sandbox is created with env var injected into init container",
			claimToReconcile: claimTargetInitContainer,
			existingObjects:  []client.Object{templateWithInitContainer, warmPoolWithInitContainer},
			expectSandbox:    true,
			expectedCondition: metav1.Condition{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady", Message: "Sandbox is not ready",
			},
			validateSandbox: func(t *testing.T, sandbox *sandboxv1beta1.Sandbox, _ *extensionsv1beta1.SandboxTemplate) {
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
				Client:              client,
				Scheme:              scheme,
				WarmSandboxQueue:    queue.NewSimpleSandboxQueue(),
				Recorder:            events.NewFakeRecorder(10),
				Tracer:              asmetrics.NewNoOp(),
				AllowedLabelDomains: tc.allowedDomains,
			}

			// Pre-populate PodQueue with any existing pods
			for _, obj := range allObjects {
				if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					if isAdoptable(sb) != nil {
						continue
					}
					warmPoolName := getWarmPoolName(sb)
					namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(sb.Namespace, warmPoolName)
					key := queue.SandboxKey{Namespace: sb.Namespace, Name: sb.Name}
					reconciler.WarmSandboxQueue.Add(namespacedWarmPoolName, key)
				}
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

			var sandbox sandboxv1beta1.Sandbox
			err = client.Get(context.Background(), req.NamespacedName, &sandbox)
			if tc.expectSandbox && err != nil {
				t.Fatalf("get sandbox: (%v)", err)
			}
			if !tc.expectSandbox && !k8errors.IsNotFound(err) {
				t.Fatalf("expected sandbox to not exist, but got err: %v", err)
			}

			if tc.expectSandbox {
				// Verify the controller injected the template hash label so the NP can find the pod
				templateName := sandbox.Annotations[sandboxv1beta1.SandboxTemplateRefAnnotation]
				if templateName == "" {
					t.Fatalf("expected sandbox to have template ref annotation, but it was missing")
				}
				expectedHash := SandboxTemplateRefHash(templateName)
				if val, exists := sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash]; !exists || val != expectedHash {
					t.Errorf("expected Sandbox PodTemplate to have label '%s' with value %q, got %q", sandboxTemplateRefHash, expectedHash, val)
				}
			}

			if tc.validateSandbox != nil {
				tc.validateSandbox(t, &sandbox, template)
			}

			var updatedClaim extensionsv1beta1.SandboxClaim
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
				if tc.expectedCondition.Message != "" && condition.Message != tc.expectedCondition.Message {
					t.Errorf("expected condition message %q, got %q", tc.expectedCondition.Message, condition.Message)
				}
			} else {
				if len(tc.expectedPodIPs) > 0 {
					if diff := cmp.Diff(tc.expectedPodIPs, updatedClaim.Status.SandboxStatus.PodIPs); diff != "" {
						t.Errorf("unexpected PodIPs:\n%s", diff)
					}

				}
				// Asserted unconditionally: cases with no bound sandbox (or a
				// sandbox with no Service) expect the empty string, so a stale
				// mirror fails the test just as a missing one does.
				if got := updatedClaim.Status.SandboxStatus.ServiceFQDN; got != tc.expectedServiceFQDN {
					t.Errorf("expected mirrored ServiceFQDN %q, got %q", tc.expectedServiceFQDN, got)
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
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-template", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "cleanup-template"}},
	}

	createClaim := func(name string, policy extensionsv1beta1.ShutdownPolicy) *extensionsv1beta1.SandboxClaim {
		pastTime := metav1.Time{Time: time.Now().Add(-2 * time.Hour).Truncate(time.Second)}
		return &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
			Spec: extensionsv1beta1.SandboxClaimSpec{
				WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "cleanup-warmpool"},
				Lifecycle: &extensionsv1beta1.Lifecycle{
					ShutdownPolicy: policy,
					ShutdownTime:   &pastTime,
				},
			},
		}
	}

	// Helper to create a Sandbox.
	createSandbox := func(claimName string, isExpired bool) *sandboxv1beta1.Sandbox {
		reason := "SandboxReady"
		status := metav1.ConditionTrue
		if isExpired {
			reason = "SandboxExpired"
			status = metav1.ConditionFalse
		}

		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      claimName,
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind, Name: claimName, UID: types.UID(claimName), Controller: new(true)},
				},
			},
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
			Status: sandboxv1beta1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:   string(sandboxv1beta1.SandboxConditionReady),
						Status: status,
						Reason: reason,
					},
				},
			},
		}
	}

	testCases := []struct {
		name                       string
		claim                      *extensionsv1beta1.SandboxClaim
		sandboxIsExpired           bool
		isWarmPool                 bool
		sandboxNotOwned            bool // sandbox exists at statusName but belongs to a different owner
		expectClaimDeleted         bool
		expectSandboxDeleted       bool
		expectSandboxStatusCleared bool // SandboxStatus.Name, PodIPs, and ServiceFQDN must be empty
		expectStatus               string
	}{
		{
			name:                 "Policy=Retain -> Should Retain Claim but DELETE Sandbox",
			claim:                createClaim("retain-claim", extensionsv1beta1.ShutdownPolicyRetain),
			sandboxIsExpired:     false,
			expectClaimDeleted:   false,
			expectSandboxDeleted: true, // Controller explicitly deletes Sandbox here.
			expectStatus:         extensionsv1beta1.ClaimExpiredReason,
		},
		{
			name:                 "Policy=Retain (Sandbox from Warm Pool) -> Should Retain Claim but DELETE Sandbox",
			claim:                createClaim("retain-claim-warm-pool", extensionsv1beta1.ShutdownPolicyRetain),
			sandboxIsExpired:     false,
			isWarmPool:           true,
			expectClaimDeleted:   false,
			expectSandboxDeleted: true, // Controller explicitly deletes Sandbox here.
			expectStatus:         extensionsv1beta1.ClaimExpiredReason,
		},
		{
			name:                       "Policy=Retain, Sandbox not owned by claim -> skip deletion, SandboxStatus cleared",
			claim:                      createClaim("retain-claim-unowned", extensionsv1beta1.ShutdownPolicyRetain),
			sandboxNotOwned:            true,
			expectClaimDeleted:         false,
			expectSandboxDeleted:       false,
			expectSandboxStatusCleared: true,
			expectStatus:               extensionsv1beta1.ClaimExpiredReason,
		},
		{
			name:               "Policy=Delete && Sandbox Expired -> Should Delete Claim",
			claim:              createClaim("delete-claim-synced", extensionsv1beta1.ShutdownPolicyDelete),
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
			claim:                createClaim("delete-claim-race", extensionsv1beta1.ShutdownPolicyDelete),
			sandboxIsExpired:     false,
			expectClaimDeleted:   true,
			expectSandboxDeleted: false, // Same as above: FakeClient doesn't simulate GC.
			expectStatus:         "",
		},
		{
			name:               "Policy=DeleteForeground && Sandbox Running -> Should Delete Claim with foreground propagation",
			claim:              createClaim("delete-fg-claim", extensionsv1beta1.ShutdownPolicyDeleteForeground),
			sandboxIsExpired:   false,
			expectClaimDeleted: true,
			// FakeClient doesn't simulate GC or foreground propagation,
			// so the Sandbox will remain. The important thing is the Claim is deleted.
			expectSandboxDeleted: false,
			expectStatus:         "",
		},
		{
			name:                 "Policy=DeleteForeground && Sandbox Expired -> Should Delete Claim with foreground propagation",
			claim:                createClaim("delete-fg-claim-expired", extensionsv1beta1.ShutdownPolicyDeleteForeground),
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
					{APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind, Name: "other-claim", UID: "other-uid", Controller: func() *bool { b := true; return &b }()},
				}
				tc.claim.Status.SandboxStatus.Name = sandbox.Name
				// Seed the mirrored fields so the cleared-status assertions
				// below actually exercise the clear path (against the
				// zero-value fixture they pass vacuously).
				tc.claim.Status.SandboxStatus.PodIPs = []string{"10.0.0.99"}
				tc.claim.Status.SandboxStatus.ServiceFQDN = "previous-sandbox.default.svc.cluster.local"
			}

			client := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(template, warmPool, tc.claim, sandbox).
				WithStatusSubresource(tc.claim).Build()

			reconciler := &SandboxClaimReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				Tracer:           asmetrics.NewNoOp(),
				WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
			}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: tc.claim.Name, Namespace: "default"}}
			var err error
			for range 2 {
				_, err = reconciler.Reconcile(context.Background(), req)
				if err != nil {
					t.Fatalf("reconcile failed: %v", err)
				}
			}

			// 1. Verify Claim
			var fetchedClaim extensionsv1beta1.SandboxClaim
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
					if cond.Type == string(sandboxv1beta1.SandboxConditionReady) && cond.Reason == tc.expectStatus {
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
					if fetchedClaim.Status.SandboxStatus.ServiceFQDN != "" {
						t.Errorf("expected SandboxStatus.ServiceFQDN to be empty, got %q", fetchedClaim.Status.SandboxStatus.ServiceFQDN)
					}
				}
			}

			// 2. Verify Sandbox
			var fetchedSandbox sandboxv1beta1.Sandbox

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

func TestSandboxClaimMirrorsFinishedConditionAndSchedulesTTL(t *testing.T) {
	scheme := newScheme(t)
	ttl := int32(120)
	finishedAt := metav1.NewTime(time.Now().Add(-30 * time.Second))

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ttl-mirror-claim", Namespace: "default", UID: "ttl-mirror-claim"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "ttl-mirror-warmpool"},
			Lifecycle:   &extensionsv1beta1.Lifecycle{TTLSecondsAfterFinished: &ttl},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "ttl-mirror-template", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ttl-mirror-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "ttl-mirror-template"}},
	}

	controller := true
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name,
			Namespace: claim.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxClaimKind,
				Name:       claim.Name,
				UID:        claim.UID,
				Controller: &controller,
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
		Status: sandboxv1beta1.SandboxStatus{Conditions: []metav1.Condition{{
			Type:               string(sandboxv1beta1.SandboxConditionFinished),
			Status:             metav1.ConditionTrue,
			Reason:             sandboxv1beta1.SandboxReasonPodSucceeded,
			LastTransitionTime: finishedAt,
		}}},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, template, warmPool, sandbox).
		WithStatusSubresource(claim).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           client,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}
	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Greater(t, result.RequeueAfter, time.Duration(0))

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, client.Get(context.Background(), req.NamespacedName, updatedClaim))
	finishedCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
	require.NotNil(t, finishedCondition)
	require.Equal(t, sandboxv1beta1.SandboxReasonPodSucceeded, finishedCondition.Reason)
	readyCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCondition)
	require.Equal(t, "SandboxNotReady", readyCondition.Reason)
}

func TestSandboxClaimTTLAfterFinishedCleanupPolicy(t *testing.T) {
	scheme := newScheme(t)
	ttlZero := int32(0)
	finishedAt := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "cleanup-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-template", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
	}

	createClaim := func(name string, policy extensionsv1beta1.ShutdownPolicy) *extensionsv1beta1.SandboxClaim {
		return &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
			Spec: extensionsv1beta1.SandboxClaimSpec{
				WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "cleanup-warmpool"},
				Lifecycle: &extensionsv1beta1.Lifecycle{
					ShutdownPolicy:          policy,
					TTLSecondsAfterFinished: &ttlZero,
				},
			},
			Status: extensionsv1beta1.SandboxClaimStatus{Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionFinished),
				Status:             metav1.ConditionTrue,
				Reason:             sandboxv1beta1.SandboxReasonPodSucceeded,
				LastTransitionTime: finishedAt,
			}}},
		}
	}

	controller := true
	createSandbox := func(claim *extensionsv1beta1.SandboxClaim) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      claim.Name,
				Namespace: claim.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: extensionsv1beta1.GroupVersion.String(),
					Kind:       extensionsv1beta1.SandboxClaimKind,
					Name:       claim.Name,
					UID:        claim.UID,
					Controller: &controller,
				}},
			},
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
			Status: sandboxv1beta1.SandboxStatus{Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionFinished),
				Status:             metav1.ConditionTrue,
				Reason:             sandboxv1beta1.SandboxReasonPodSucceeded,
				LastTransitionTime: finishedAt,
			}}},
		}
	}

	testCases := []struct {
		name                 string
		policy               extensionsv1beta1.ShutdownPolicy
		expectClaimDeleted   bool
		expectSandboxDeleted bool
	}{
		{
			name:                 "retain deletes sandbox and preserves finished condition",
			policy:               extensionsv1beta1.ShutdownPolicyRetain,
			expectClaimDeleted:   false,
			expectSandboxDeleted: true,
		},
		{
			name:                 "delete foreground deletes claim",
			policy:               extensionsv1beta1.ShutdownPolicyDeleteForeground,
			expectClaimDeleted:   true,
			expectSandboxDeleted: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			claim := createClaim(tc.name, tc.policy)
			sandbox := createSandbox(claim)
			client := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(claim, sandbox, warmPool, template).
				WithStatusSubresource(claim).
				Build()

			reconciler := &SandboxClaimReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				Tracer:           asmetrics.NewNoOp(),
				WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
			}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}
			result, err := reconciler.Reconcile(context.Background(), req)
			require.NoError(t, err)
			require.Greater(t, result.RequeueAfter, time.Duration(0))

			updatedClaim := &extensionsv1beta1.SandboxClaim{}
			require.NoError(t, client.Get(context.Background(), req.NamespacedName, updatedClaim))
			readyCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
			require.NotNil(t, readyCondition)
			require.Equal(t, extensionsv1beta1.ClaimExpiredReason, readyCondition.Reason)
			finishedCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
			require.NotNil(t, finishedCondition)
			require.Equal(t, sandboxv1beta1.SandboxReasonPodSucceeded, finishedCondition.Reason)

			updatedSandbox := &sandboxv1beta1.Sandbox{}
			require.NoError(t, client.Get(context.Background(), req.NamespacedName, updatedSandbox))

			result, err = reconciler.Reconcile(context.Background(), req)
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)

			err = client.Get(context.Background(), req.NamespacedName, updatedClaim)
			if tc.expectClaimDeleted {
				require.True(t, k8errors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				readyCondition = meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
				require.NotNil(t, readyCondition)
				require.Equal(t, extensionsv1beta1.ClaimExpiredReason, readyCondition.Reason)
				finishedCondition = meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
				require.NotNil(t, finishedCondition)
				require.Equal(t, sandboxv1beta1.SandboxReasonPodSucceeded, finishedCondition.Reason)
			}

			err = client.Get(context.Background(), req.NamespacedName, updatedSandbox)
			if tc.expectSandboxDeleted {
				require.True(t, k8errors.IsNotFound(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSandboxClaimTTLCleanupRequiresPersistedExpiredStatus(t *testing.T) {
	scheme := newScheme(t)
	ttlZero := int32(0)
	finishedAt := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stale-ttl-claim",
			Namespace: "default",
			UID:       "stale-ttl-claim",
			Annotations: map[string]string{
				ObservabilityAnnotation: time.Now().Format(time.RFC3339Nano),
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "stale-warmpool"},
			Lifecycle: &extensionsv1beta1.Lifecycle{
				ShutdownPolicy:          extensionsv1beta1.ShutdownPolicyDelete,
				TTLSecondsAfterFinished: &ttlZero,
			},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "stale-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}}},
		}}},
	}

	controller := true
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name,
			Namespace: claim.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxClaimKind,
				Name:       claim.Name,
				UID:        claim.UID,
				Controller: &controller,
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
		Status: sandboxv1beta1.SandboxStatus{Conditions: []metav1.Condition{{
			Type:               string(sandboxv1beta1.SandboxConditionFinished),
			Status:             metav1.ConditionTrue,
			Reason:             sandboxv1beta1.SandboxReasonPodSucceeded,
			LastTransitionTime: finishedAt,
		}}},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, template, warmPool, sandbox).
		WithStatusSubresource(claim).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           client,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}
	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Greater(t, result.RequeueAfter, time.Duration(0))

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, client.Get(context.Background(), req.NamespacedName, updatedClaim))
	readyCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCondition)
	require.Equal(t, extensionsv1beta1.ClaimExpiredReason, readyCondition.Reason)
	finishedCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
	require.NotNil(t, finishedCondition)
	require.Equal(t, sandboxv1beta1.SandboxReasonPodSucceeded, finishedCondition.Reason)

	require.NoError(t, client.Get(context.Background(), req.NamespacedName, &sandboxv1beta1.Sandbox{}))

	result, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)

	err = client.Get(context.Background(), req.NamespacedName, &extensionsv1beta1.SandboxClaim{})
	require.True(t, k8errors.IsNotFound(err))
}

// TestSandboxProvisionEvent verifies that Sandbox creation emits "SandboxProvisioned".
func TestSandboxProvisionEvent(t *testing.T) {
	scheme := newScheme(t)
	claimName := "provision-event-claim"

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: types.UID(claimName)},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{}}},
	}

	fakeRecorder := events.NewFakeRecorder(10)
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, template, warmPool).
		WithStatusSubresource(claim).Build()

	reconciler := &SandboxClaimReconciler{
		Client:           client,
		Scheme:           scheme,
		Recorder:         fakeRecorder,
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
		Tracer:           asmetrics.NewNoOp(),
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

func TestCreateSandboxPropagatesVolumeClaimTemplates(t *testing.T) {
	scheme := newScheme(t)
	claimName := "vct-claim"

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: types.UID(claimName)},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "vct-warmpool"},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "vct-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "vct-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "vct-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		},
			VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, template, warmPool).
		WithStatusSubresource(claim).Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify sandbox was created with volumeClaimTemplates
	sandbox := &sandboxv1beta1.Sandbox{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: claimName, Namespace: "default"}, sandbox)
	if err != nil {
		t.Fatalf("Failed to get sandbox: %v", err)
	}

	if len(sandbox.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected 1 volumeClaimTemplate, got %d", len(sandbox.Spec.VolumeClaimTemplates))
	}
	if sandbox.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Errorf("expected volumeClaimTemplate name 'data', got %q", sandbox.Spec.VolumeClaimTemplates[0].Name)
	}
	expectedStorage := resource.MustParse("1Gi")
	actualStorage := sandbox.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if !actualStorage.Equal(expectedStorage) {
		t.Errorf("expected storage %s, got %s", expectedStorage.String(), actualStorage.String())
	}
}

// testNetworkedPodIP is a placeholder Pod IP used by warm-pool sandbox
// fixtures to mark "backing Pod exists and is networked".
const testNetworkedPodIP = "10.244.0.5"

func TestSandboxClaimSandboxAdoption(t *testing.T) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
					},
				},
			},
		}},
		},
	}

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid",
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{
				Name: "test-pool",
			},
		},
	}
	claimPastWarmCandidateGracePeriod := claim.DeepCopy()
	claimPastWarmCandidateGracePeriod.CreationTimestamp = metav1.NewTime(time.Now().Add(-warmCandidateGracePeriod - time.Second))

	warmPoolUID := types.UID("warmpool-uid-123")
	poolNameHash := sandboxcontrollers.NameHash("test-pool")

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: warmPoolUID},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	createWarmPoolSandbox := func(name string, creationTime metav1.Time, ready bool) *sandboxv1beta1.Sandbox {
		conditionStatus := metav1.ConditionFalse
		if ready {
			conditionStatus = metav1.ConditionTrue
		}
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				Generation:        1,
				CreationTimestamp: creationTime,
				Labels: map[string]string{
					warmPoolSandboxLabel:                  poolNameHash,
					sandboxTemplateRefHash:                SandboxTemplateRefHash("test-template"),
					sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: extensionsv1beta1.GroupVersion.String(),
						Kind:       extensionsv1beta1.SandboxWarmPoolKind,
						Name:       "test-pool",
						UID:        warmPoolUID,
						Controller: new(true),
					},
				},
			},
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				ObjectMeta: sandboxv1beta1.PodMetadata{
					Annotations: map[string]string{
						autoscalerSafeToEvictAnnotation: "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
			Status: sandboxv1beta1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1beta1.SandboxConditionReady),
						Status:             conditionStatus,
						ObservedGeneration: 1,
						Reason:             "DependenciesReady",
					},
				},
				// PodIPs marks that the backing Pod exists and is networked.
				PodIPs: []string{testNetworkedPodIP},
			},
		}
	}

	createSandboxWithDifferentController := func(name string) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
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
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
		}
	}

	createDeletingSandbox := func(name string) *sandboxv1beta1.Sandbox {
		sb := createWarmPoolSandbox(name, metav1.Now(), true)
		now := metav1.Now()
		sb.DeletionTimestamp = &now
		sb.Finalizers = []string{"test-finalizer"}
		return sb
	}

	// createRotatingSandbox simulates a warm-pool sandbox whose backing Pod has
	// been deleted or is not networked yet while its queue entry is still present.
	createRotatingSandbox := func(name string, creationTime metav1.Time) *sandboxv1beta1.Sandbox {
		sb := createWarmPoolSandbox(name, creationTime, false)
		sb.Status.PodIPs = nil
		return sb
	}

	// matchingBlueprintHash is the blueprint hash the warm pool controller would stamp on a
	// fresh sandbox for this template; a candidate carrying it is considered current under Recreate.
	matchingBlueprintHash, hashErr := computeSandboxBlueprintHash(template)
	if hashErr != nil {
		t.Fatalf("failed to compute blueprint hash: %v", hashErr)
	}

	// withBlueprintHash stamps the blueprint hash label the SandboxClaim controller inspects
	// when deciding whether a candidate is fresh under the Recreate strategy (issue #764).
	withBlueprintHash := func(sb *sandboxv1beta1.Sandbox, blueprintHash string) *sandboxv1beta1.Sandbox {
		sb.Labels[sandboxv1beta1.SandboxTemplateHashLabel] = blueprintHash
		return sb
	}

	recreateStrategy := &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType}
	onReplenishStrategy := &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType}

	testCases := []struct {
		name                    string
		existingObjects         []client.Object
		warmPoolStrategy        *extensionsv1beta1.SandboxWarmPoolUpdateStrategy
		expectSandboxAdoption   bool
		expectedAdoptedSandbox  string
		expectedAnnotations     map[string]string
		expectedPodAnnotations  map[string]string
		expectedLabels          map[string]string
		expectedPodLabels       map[string]string
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
			name: "adopts ready sandboxes from queue prioritizing ready state",
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
			name: "creates new sandbox when all warm pool sandboxes are non-ready",
			existingObjects: []client.Object{
				template,
				claim,
				createWarmPoolSandbox("not-ready-1", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, false),
				createWarmPoolSandbox("not-ready-2", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, false),
			},
			expectSandboxAdoption:   false,
			expectNewSandboxCreated: true,
		},
		{
			name: "skips stale ready condition and adopts current ready sandbox",
			existingObjects: []client.Object{
				template,
				claim,
				func() client.Object {
					sb := createWarmPoolSandbox("stale-ready", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, true)
					sb.Generation = 2
					sb.Status.Conditions[0].ObservedGeneration = 1
					return sb
				}(),
				createWarmPoolSandbox("current-ready", metav1.Now(), true),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "current-ready",
			expectNewSandboxCreated: false,
		},
		{
			name: "falls through to cold creation after grace expires when warm candidates lack PodIPs",
			existingObjects: []client.Object{
				template,
				claimPastWarmCandidateGracePeriod,
				createRotatingSandbox("rotating-sb-1", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}),
				createRotatingSandbox("rotating-sb-2", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}),
			},
			expectSandboxAdoption:   false,
			expectNewSandboxCreated: true,
		},
		{
			name: "creates new sandbox when networked warm pool sandbox is not ready",
			existingObjects: []client.Object{
				template,
				claim,
				createRotatingSandbox("rotating-sb", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}),
				createWarmPoolSandbox("not-ready-with-pod", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, false),
			},
			expectSandboxAdoption:   false,
			expectNewSandboxCreated: true,
		},
		{
			name: "resolves adoption-patch conflict on the same candidate",
			existingObjects: []client.Object{
				template,
				claim,
				createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true),
				createWarmPoolSandbox("pool-sb-2", metav1.Now(), true),
			},
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "pool-sb-1",
			// The first adoption patch conflicts; the committed assignment must be
			// completed on a fresh base for the SAME candidate, never by switching
			// to the next candidate (the assignment-flip amplification defect).
			expectNewSandboxCreated: false,
			simulateConflicts:       1,
		},
		{
			name: "preserves template eviction annotation false when adopting sandbox",
			existingObjects: []client.Object{
				func() client.Object {
					tCopy := template.DeepCopy()
					if tCopy.Spec.PodTemplate.ObjectMeta.Annotations == nil {
						tCopy.Spec.PodTemplate.ObjectMeta.Annotations = make(map[string]string)
					}
					tCopy.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation] = "false"
					return tCopy
				}(),
				claim,
				func() client.Object {
					sb := createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true)
					sb.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation] = "false"
					return sb
				}(),
				createWarmPoolSandbox("pool-sb-2", metav1.Time{Time: metav1.Now().Add(-30 * time.Minute)}, true),
			},
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "pool-sb-1",
			expectedPodAnnotations: map[string]string{
				autoscalerSafeToEvictAnnotation: "false",
			},
			expectNewSandboxCreated: false,
		},
		{
			name: "preserves template eviction annotation false when template lookup fails (fallback path)",
			existingObjects: []client.Object{
				claim,
				func() client.Object {
					sb := createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true)
					sb.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation] = "false"
					return sb
				}(),
				createWarmPoolSandbox("pool-sb-2", metav1.Time{Time: metav1.Now().Add(-30 * time.Minute)}, true),
			},
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "pool-sb-1",
			expectedPodAnnotations: map[string]string{
				autoscalerSafeToEvictAnnotation: "false",
			},
			expectNewSandboxCreated: false,
		},
		{
			name: "preserves claim eviction annotation true when adopting sandbox",
			existingObjects: []client.Object{
				template,
				func() client.Object {
					cCopy := claim.DeepCopy()
					if cCopy.Spec.AdditionalPodMetadata.Annotations == nil {
						cCopy.Spec.AdditionalPodMetadata.Annotations = make(map[string]string)
					}
					cCopy.Spec.AdditionalPodMetadata.Annotations[autoscalerSafeToEvictAnnotation] = "true"
					return cCopy
				}(),
				func() client.Object {
					sb := createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true)
					sb.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation] = "true"
					return sb
				}(),
				createWarmPoolSandbox("pool-sb-2", metav1.Time{Time: metav1.Now().Add(-30 * time.Minute)}, true),
			},
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "pool-sb-1",
			expectedPodAnnotations: map[string]string{
				autoscalerSafeToEvictAnnotation: "true",
			},
			expectNewSandboxCreated: false,
		},
		{
			name: "rejects unowned sandboxes with mock labels",
			existingObjects: []client.Object{
				template,
				claim,
				func() client.Object {
					sb := createWarmPoolSandbox("unowned-sb", metav1.Now(), true)
					sb.OwnerReferences = nil
					return sb
				}(),
			},
			expectSandboxAdoption:   false,
			expectNewSandboxCreated: true,
		},
		{
			name: "propagates and overwrites created-by label during adoption",
			existingObjects: []client.Object{
				template,
				func() client.Object {
					cCopy := claim.DeepCopy()
					if cCopy.Labels == nil {
						cCopy.Labels = make(map[string]string)
					}
					cCopy.Labels[sandboxv1beta1.CreatedByLabel] = "go-client"
					return cCopy
				}(),
				func() client.Object {
					sb := createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true)
					if sb.Labels == nil {
						sb.Labels = make(map[string]string)
					}
					sb.Labels[sandboxv1beta1.CreatedByLabel] = "controller"
					return sb
				}(),
			},
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "pool-sb-1",
			expectedLabels: map[string]string{
				sandboxv1beta1.CreatedByLabel: "go-client",
			},
			expectedPodLabels: map[string]string{
				sandboxv1beta1.CreatedByLabel: "go-client",
			},
			expectNewSandboxCreated: false,
		},
		{
			name: "removes created-by label during adoption when claim lacks it",
			existingObjects: []client.Object{
				template,
				claim.DeepCopy(),
				func() client.Object {
					sb := createWarmPoolSandbox("pool-sb-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true)
					if sb.Labels == nil {
						sb.Labels = make(map[string]string)
					}
					sb.Labels[sandboxv1beta1.CreatedByLabel] = "controller"
					return sb
				}(),
			},
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "pool-sb-1",
			expectedLabels: map[string]string{
				sandboxv1beta1.CreatedByLabel: "",
			},
			expectedPodLabels: map[string]string{
				sandboxv1beta1.CreatedByLabel: "",
			},
			expectNewSandboxCreated: false,
		},
		{
			name:             "skips stale candidate under Recreate strategy and cold starts",
			warmPoolStrategy: recreateStrategy,
			existingObjects: []client.Object{
				template,
				claim,
				withBlueprintHash(createWarmPoolSandbox("stale-recreate", metav1.Now(), true), "stale-hash"),
			},
			expectSandboxAdoption:   false,
			expectNewSandboxCreated: true,
		},
		{
			name:             "adopts fresh candidate under Recreate strategy",
			warmPoolStrategy: recreateStrategy,
			existingObjects: []client.Object{
				template,
				claim,
				withBlueprintHash(createWarmPoolSandbox("fresh-recreate", metav1.Now(), true), matchingBlueprintHash),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "fresh-recreate",
			expectNewSandboxCreated: false,
		},
		{
			// Regression for issue #764: a candidate whose blueprint-hash label is missing (e.g. a
			// sandbox created by an older controller version before the label existed) but whose
			// blueprint is still semantically current must be adopted, not dropped. The pool
			// controller's isSandboxStale keeps such a sandbox (falls back to a semantic comparison
			// on a hash mismatch and finds it fresh), so it is never deleted or replaced. If the
			// claim dropped it without requeue, a Recreate pool full of pre-label sandboxes would be
			// permanently unadoptable and every claim would needlessly cold-start against a "full" pool.
			name:             "adopts semantically-current candidate with missing hash label under Recreate",
			warmPoolStrategy: recreateStrategy,
			existingObjects: []client.Object{
				template,
				claim,
				func() client.Object {
					sb := createWarmPoolSandbox("prelabel-recreate", metav1.Now(), true)
					// No SandboxTemplateHashLabel is stamped, but the blueprint carries the same
					// secure defaults the controller applies at creation, so it is semantically current.
					ApplySandboxSecureDefaults(template, &sb.Spec.PodTemplate.Spec)
					return sb
				}(),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "prelabel-recreate",
			expectNewSandboxCreated: false,
		},
		{
			name:             "adopts stale candidate under OnReplenish strategy (no regression)",
			warmPoolStrategy: onReplenishStrategy,
			existingObjects: []client.Object{
				template,
				claim,
				withBlueprintHash(createWarmPoolSandbox("stale-onreplenish", metav1.Now(), true), "stale-hash"),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "stale-onreplenish",
			expectNewSandboxCreated: false,
		},
		{
			name:             "adopts stale candidate when warm pool strategy unset (defaults to OnReplenish)",
			warmPoolStrategy: nil,
			existingObjects: []client.Object{
				template,
				claim,
				withBlueprintHash(createWarmPoolSandbox("default-stale", metav1.Now(), true), "stale-hash"),
			},
			expectSandboxAdoption:   true,
			expectedAdoptedSandbox:  "default-stale",
			expectNewSandboxCreated: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			// The update strategy lives on the SandboxWarmPool spec; give each case its own pool so
			// the claim controller reads the intended strategy (issue #764).
			caseWarmPool := warmPool.DeepCopy()
			caseWarmPool.Spec.UpdateStrategy = tc.warmPoolStrategy
			var fakeClient client.Client = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(append(tc.existingObjects, caseWarmPool)...).
				WithStatusSubresource(claim).
				Build()

			if tc.simulateConflicts > 0 {
				fakeClient = &conflictClient{
					Client:       fakeClient,
					maxConflicts: tc.simulateConflicts,
				}
			}

			// 1. Initialize the Queue
			warmSandboxQueue := queue.NewSimpleSandboxQueue()

			// 2. Seed stale queue entries too; production can retain keys after
			//    a sandbox stops being adoptable, and pop-side validation must
			//    reject them.
			for _, obj := range tc.existingObjects {
				if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					warmPoolName := getWarmPoolName(sb)
					if warmPoolName == "" {
						continue
					}
					namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(sb.Namespace, warmPoolName)
					key := queue.SandboxKey{Namespace: sb.Namespace, Name: sb.Name, NodeName: sb.Status.NodeName}
					warmSandboxQueue.Add(namespacedWarmPoolName, key)
				}
			}

			// 3. Inject the seeded Queue into the Reconciler
			reconciler := &SandboxClaimReconciler{
				Client:           fakeClient,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				WarmSandboxQueue: warmSandboxQueue,
				Tracer:           asmetrics.NewNoOp(),
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
				var adoptedSandbox sandboxv1beta1.Sandbox
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
				expectedTemplateHash := SandboxTemplateRefHash(template.Name)
				if val := adoptedSandbox.Labels[sandboxTemplateRefHash]; val != expectedTemplateHash {
					t.Errorf("expected adopted sandbox to retain template ref label %q=%q, got %q", sandboxTemplateRefHash, expectedTemplateHash, val)
				}
				if val := adoptedSandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel]; val != sandboxv1beta1.SandboxLaunchTypeWarm {
					t.Errorf("expected adopted sandbox to have launch type label %q, got %q; labels=%v", sandboxv1beta1.SandboxLaunchTypeWarm, val, adoptedSandbox.Labels)
				}

				// Verify eviction annotation is either matched against expected value or removed by default
				if len(tc.expectedPodAnnotations) > 0 {
					for key, expected := range tc.expectedPodAnnotations {
						val, exists := adoptedSandbox.Spec.PodTemplate.ObjectMeta.Annotations[key]
						if !exists {
							t.Errorf("expected pod template annotation %q to exist on adopted sandbox", key)
						} else if val != expected {
							t.Errorf("expected pod template annotation %q=%q, got %q", key, expected, val)
						}
					}
				} else {
					if _, exists := adoptedSandbox.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation]; exists {
						t.Errorf("expected eviction annotation to be removed from adopted sandbox")
					}
				}

				// 2. Verify SandboxID label was added to pod template
				expectedUID := string(types.UID("claim-uid"))
				if val := adoptedSandbox.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1beta1.SandboxIDLabel]; val != expectedUID {
					t.Errorf("expected pod template to have SandboxID label %q, got %q", expectedUID, val)
				}

				// 3. Verify claim is the controller owner
				controllerRef := metav1.GetControllerOf(&adoptedSandbox)
				if controllerRef == nil || controllerRef.UID != claim.UID {
					t.Errorf("expected adopted sandbox to be controlled by claim, got %v", controllerRef)
				}

				for key, expected := range tc.expectedAnnotations {
					require.Equal(t, expected, adoptedSandbox.Annotations[key])
				}

				for key, expected := range tc.expectedLabels {
					require.Equal(t, expected, adoptedSandbox.Labels[key])
				}

				for key, expected := range tc.expectedPodLabels {
					require.Equal(t, expected, adoptedSandbox.Spec.PodTemplate.ObjectMeta.Labels[key])
				}

				// 5. Verify the claim records the assigned sandbox annotation
				var updatedClaim extensionsv1beta1.SandboxClaim
				if err := fakeClient.Get(ctx, req.NamespacedName, &updatedClaim); err != nil {
					t.Fatalf("failed to get updated claim: %v", err)
				}
				require.Equal(t, tc.expectedAdoptedSandbox, updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])

			} else if tc.expectNewSandboxCreated {
				// Verify a new sandbox was created with the claim's name
				var sandbox sandboxv1beta1.Sandbox
				err = fakeClient.Get(ctx, req.NamespacedName, &sandbox)
				if err != nil {
					t.Fatalf("expected sandbox to be created but got error: %v", err)
				}
				if val := sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel]; val != sandboxv1beta1.SandboxLaunchTypeCold {
					t.Errorf("expected new sandbox to have launch type label %q, got %q; labels=%v", sandboxv1beta1.SandboxLaunchTypeCold, val, sandbox.Labels)
				}
			}
		})
	}
}

func TestSandboxClaimClearsAssignedWarmPoolSandboxThatIsNotReady(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()
	warmPoolUID := types.UID("warmpool-uid-123")
	poolNameHash := sandboxcontrollers.NameHash("test-pool")

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}}},
				},
			},
		},
	}
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: warmPoolUID},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       types.UID("claim-uid"),
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "rotating-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name}},
	}
	rotatingSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rotating-sb",
			Namespace: "default",
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: sandboxcontrollers.NameHash(template.Name),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       "SandboxWarmPool",
				Name:       warmPool.Name,
				UID:        warmPoolUID,
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}}},
				},
			},
		},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:   string(sandboxv1beta1.SandboxConditionReady),
				Status: metav1.ConditionFalse,
				Reason: "PodRecreating",
			}},
			PodIPs: nil,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, rotatingSandbox).
		WithStatusSubresource(claim).
		Build()
	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
		Tracer:           asmetrics.NewNoOp(),
	}

	assigned, err := reconciler.getOrCreateSandbox(ctx, claim, template)
	require.Nil(t, assigned)
	require.ErrorIs(t, err, errAdoptionConflict)

	var updatedClaim extensionsv1beta1.SandboxClaim
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, &updatedClaim))
	require.NotContains(t, updatedClaim.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation)

	var updatedSandbox sandboxv1beta1.Sandbox
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rotatingSandbox.Name, Namespace: rotatingSandbox.Namespace}, &updatedSandbox))
	controllerRef := metav1.GetControllerOf(&updatedSandbox)
	require.NotNil(t, controllerRef)
	require.Equal(t, extensionsv1beta1.SandboxWarmPoolKind, controllerRef.Kind)
}

func TestSandboxClaimResolvesAssignedSandboxReadinessAuthoritatively(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()
	_, template, warmPool, _ := newOptimisticLockTestObjects()
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "rotating-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name}},
	}
	serverSandbox := newPoolCandidateSandbox("rotating-sb")
	staleSandbox := serverSandbox.DeepCopy()
	ready := meta.FindStatusCondition(staleSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, ready)
	ready.Status = metav1.ConditionFalse
	ready.Reason = "DependenciesNotReady"

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, serverSandbox).
		Build()
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == staleSandbox.Name {
				staleSandbox.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
		Tracer:           asmetrics.NewNoOp(),
	}

	assigned, err := reconciler.getOrCreateSandbox(ctx, claim, template)
	require.NoError(t, err)
	require.Equal(t, serverSandbox.Name, assigned.Name)

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(ctx, client.ObjectKeyFromObject(claim), updatedClaim))
	require.Equal(t, serverSandbox.Name, updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation],
		"a stale unready cache view must not clear the committed assignment")

	updatedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rawClient.Get(ctx, client.ObjectKeyFromObject(serverSandbox), updatedSandbox))
	require.True(t, metav1.IsControlledBy(updatedSandbox, claim),
		"the authoritative ready candidate must complete adoption")
}

func TestGetCandidateRequeuesUnnetworkedWarmPoolSandboxes(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()
	warmPoolUID := types.UID("warmpool-uid-123")
	poolName := "test-pool"
	key := queue.SandboxKey{Namespace: "default", Name: "rotating-sb", NodeName: "node-a"}
	rotatingSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				warmPoolSandboxLabel:   sandboxcontrollers.NameHash(poolName),
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       "SandboxWarmPool",
				Name:       poolName,
				UID:        warmPoolUID,
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Status: sandboxv1beta1.SandboxStatus{PodIPs: nil},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: key.Namespace, UID: types.UID("claim-uid")},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: poolName}},
	}
	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(key.Namespace, poolName)
	warmSandboxQueue.Add(namespacedWarmPoolName, key)
	reconciler := &SandboxClaimReconciler{
		Client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(rotatingSandbox).Build(),
		Scheme:           scheme,
		WarmSandboxQueue: warmSandboxQueue,
		Tracer:           asmetrics.NewNoOp(),
	}

	candidate, _, pendingNetworkCandidates, err := reconciler.getCandidate(ctx, claim)
	require.NoError(t, err)
	require.Nil(t, candidate)
	require.Equal(t, 1, pendingNetworkCandidates)

	requeued, ok := warmSandboxQueue.Get(namespacedWarmPoolName)
	require.True(t, ok, "unnetworked candidate should be returned to the queue")
	require.Equal(t, key, requeued)
}

func newWarmCandidateGraceFixture(t *testing.T, claimCreated time.Time, withCandidate bool) (client.Client, *SandboxClaimReconciler, reconcile.Request, *sandboxv1beta1.Sandbox) {
	t.Helper()
	scheme := newScheme(t)
	poolName := "test-pool"
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
			PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "workspace", Image: "workspace:latest"}},
			}},
		}},
	}
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default", UID: "pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-claim",
			Namespace:         "default",
			UID:               "claim-uid",
			CreationTimestamp: metav1.NewTime(claimCreated),
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: poolName}},
	}
	objects := []client.Object{template, warmPool, claim}
	var candidate *sandboxv1beta1.Sandbox
	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	if withCandidate {
		candidate = &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pending-sandbox",
				Namespace: "default",
				Labels: map[string]string{
					warmPoolSandboxLabel:   sandboxcontrollers.NameHash(poolName),
					sandboxTemplateRefHash: sandboxcontrollers.NameHash(template.Name),
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: extensionsv1beta1.GroupVersion.String(),
					Kind:       extensionsv1beta1.SandboxWarmPoolKind,
					Name:       poolName,
					UID:        warmPool.UID,
					Controller: new(true),
				}},
			},
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: template.Spec.PodTemplate,
			}},
			Status: sandboxv1beta1.SandboxStatus{NodeName: "node-a"},
		}
		objects = append(objects, candidate)
		warmSandboxQueue.Add(
			queue.GetNamespacedWarmPoolName(candidate.Namespace, poolName),
			queue.SandboxKey{Namespace: candidate.Namespace, Name: candidate.Name, NodeName: candidate.Status.NodeName},
		)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(claim).
		Build()
	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		WarmSandboxQueue: warmSandboxQueue,
		Tracer:           asmetrics.NewNoOp(),
	}
	return fakeClient, reconciler, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(claim)}, candidate
}

func TestSandboxClaimWarmCandidateGrace(t *testing.T) {
	tests := []struct {
		name          string
		claimCreated  time.Time
		withCandidate bool
		wantRequeue   bool
		wantCold      bool
	}{
		{
			name:          "pending candidate briefly requeues",
			claimCreated:  time.Now(),
			withCandidate: true,
			wantRequeue:   true,
		},
		{
			name:         "truly empty pool cold starts immediately",
			claimCreated: time.Now(),
			wantCold:     true,
		},
		{
			name:          "pending candidate past deadline cold starts",
			claimCreated:  time.Now().Add(-warmCandidateGracePeriod - time.Second),
			withCandidate: true,
			wantCold:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient, reconciler, req, _ := newWarmCandidateGraceFixture(t, tc.claimCreated, tc.withCandidate)
			result, err := reconciler.Reconcile(context.Background(), req)
			require.NoError(t, err)
			if tc.wantRequeue {
				require.Greater(t, result.RequeueAfter, time.Duration(0))
				require.LessOrEqual(t, result.RequeueAfter, warmCandidateRetryInterval)
			} else {
				require.Zero(t, result.RequeueAfter)
			}

			var coldSandbox sandboxv1beta1.Sandbox
			err = fakeClient.Get(context.Background(), req.NamespacedName, &coldSandbox)
			if tc.wantCold {
				require.NoError(t, err)
				require.True(t, metav1.IsControlledBy(&coldSandbox, &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{UID: "claim-uid"}}))
			} else {
				require.True(t, k8errors.IsNotFound(err), "cold sandbox should not be created during grace")
				var updatedClaim extensionsv1beta1.SandboxClaim
				require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &updatedClaim))
				require.Empty(t, updatedClaim.Status.Conditions, "grace retry should not publish a failure condition")
			}
		})
	}
}

func TestSandboxClaimAdoptsCandidateThatBecomesNetworkReadyDuringGrace(t *testing.T) {
	fakeClient, reconciler, req, candidate := newWarmCandidateGraceFixture(t, time.Now(), true)
	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Greater(t, result.RequeueAfter, time.Duration(0))

	var networked sandboxv1beta1.Sandbox
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(candidate), &networked))
	networked.Status.PodIPs = []string{"10.0.0.8"}
	networked.Status.Conditions = []metav1.Condition{{
		Type:   string(sandboxv1beta1.SandboxConditionReady),
		Status: metav1.ConditionTrue,
		Reason: "Ready",
	}}
	require.NoError(t, fakeClient.Update(context.Background(), &networked))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	var adopted sandboxv1beta1.Sandbox
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(candidate), &adopted))
	var claim extensionsv1beta1.SandboxClaim
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &claim))
	require.True(t, metav1.IsControlledBy(&adopted, &claim))
	require.Equal(t, candidate.Name, claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
}

func TestGetCandidateRequeuesRecreateCandidateOnTemplateLookupError(t *testing.T) {
	scheme := newScheme(t)

	warmPoolUID := types.UID("warmpool-uid-req")
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}

	// The Recreate warm pool exists but its referenced SandboxTemplate does not, so
	// getTemplateForWarmPool returns ErrTemplateNotFound and the lazy hash computation fails.
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: warmPoolUID},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			TemplateRef:    extensionsv1beta1.SandboxTemplateRef{Name: "missing-template"},
			UpdateStrategy: &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType},
		},
	}

	candidate := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recreate-candidate",
			Namespace: "default",
			Labels: map[string]string{
				warmPoolSandboxLabel:                    sandboxcontrollers.NameHash("test-pool"),
				sandboxTemplateRefHash:                  SandboxTemplateRefHash("missing-template"),
				sandboxv1beta1.SandboxLaunchTypeLabel:   sandboxv1beta1.SandboxLaunchTypeWarm,
				sandboxv1beta1.SandboxTemplateHashLabel: "some-hash",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       "SandboxWarmPool",
				Name:       "test-pool",
				UID:        warmPoolUID,
				Controller: new(true),
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:   string(sandboxv1beta1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
				Reason: "Ready",
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(warmPool, claim, candidate).Build()

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(candidate.Namespace, "test-pool")
	key := queue.SandboxKey{Namespace: candidate.Namespace, Name: candidate.Name}
	warmSandboxQueue.Add(namespacedWarmPoolName, key)

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		WarmSandboxQueue: warmSandboxQueue,
		Tracer:           asmetrics.NewNoOp(),
	}

	sb, _, _, err := reconciler.getCandidate(context.Background(), claim)
	if err != nil {
		t.Fatalf("getCandidate returned unexpected error: %v", err)
	}
	if sb != nil {
		t.Fatalf("expected no candidate to be adopted, got %q", sb.Name)
	}

	// The candidate must have been requeued rather than drained.
	got, ok := warmSandboxQueue.Get(namespacedWarmPoolName)
	if !ok {
		t.Fatalf("expected Recreate candidate to be requeued, but queue is empty")
	}
	if got.Name != candidate.Name {
		t.Fatalf("expected requeued candidate %q, got %q", candidate.Name, got.Name)
	}
}
func TestSandboxEventHandler_Delete_RemovesGhostPods(t *testing.T) {
	q := queue.NewSimpleSandboxQueue()
	handler := &sandboxEventHandler{sandboxQueue: q}

	warmPoolName := "test-warmpool"
	namespacedWarmPoolName := queue.GetNamespacedWarmPoolName("default", warmPoolName)
	key := queue.SandboxKey{Namespace: "default", Name: "ghost-pod"}

	// 1. Add the pod to the queue
	q.Add(namespacedWarmPoolName, key)

	// 2. Create the mock Sandbox object that is being deleted
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ghost-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       warmPoolName,
			}},
		},
	}

	// 3. Fire the Delete event
	handler.Delete(context.Background(), event.DeleteEvent{Object: sb}, nil)

	// 4. Verify the Ghost Pod was removed from the queue
	_, ok := q.Get(namespacedWarmPoolName)
	if ok {
		t.Errorf("Expected the deleted sandbox to be removed from the queue")
	}
}

func TestWarmPoolEventHandler_Delete_RemovesEntireQueue(t *testing.T) {
	q := queue.NewSimpleSandboxQueue()
	handler := &warmPoolEventHandler{sandboxQueue: q}

	warmPoolName := "old-warmpool"
	key := queue.SandboxKey{Namespace: "default", Name: "abandoned-pod"}

	// 1. Add a pod to this warmpool's queue using namespace-aware index
	namespacedWarmPoolName := queue.GetNamespacedWarmPoolName("default", warmPoolName)
	q.Add(namespacedWarmPoolName, key)

	// 2. Create the mock SandboxWarmPool object that is being deleted
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      warmPoolName,
			Namespace: "default",
		},
	}

	// 3. Fire the Delete event
	handler.Delete(context.Background(), event.DeleteEvent{Object: warmPool}, nil)

	// 4. Verify the entire queue was wiped out
	_, ok := q.Get(namespacedWarmPoolName)
	if ok {
		t.Errorf("Expected the entire queue to be removed when the warmpool was deleted")
	}
}

// TestSandboxClaimNoReAdoption verifies that a second reconcile does not adopt another
// sandbox from the warm pool when the claim already owns one.
func TestSandboxClaimNoReAdoption(t *testing.T) {
	scheme := newScheme(t)

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	poolNameHash := sandboxcontrollers.NameHash("test-pool")

	// Claim that already adopted a sandbox (name recorded in status)
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
		Status: extensionsv1beta1.SandboxClaimStatus{
			SandboxStatus: extensionsv1beta1.SandboxStatus{Name: "adopted-sb"},
		},
	}

	// The previously adopted sandbox (owned by claim, different name)
	adoptedSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "adopted-sb", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind,
				Name: "test-claim", UID: "claim-uid", Controller: new(true),
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
	}

	// Another warm pool sandbox that should NOT be adopted
	poolSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pool-sb-extra", Namespace: "default",
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adoptedSandbox, poolSandbox).
		WithStatusSubresource(claim).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Verify the pool sandbox was NOT adopted (still has warm pool labels)
	var extra sandboxv1beta1.Sandbox
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "pool-sb-extra", Namespace: "default"}, &extra); err != nil {
		t.Fatalf("failed to get pool sandbox: %v", err)
	}
	if _, ok := extra.Labels[warmPoolSandboxLabel]; !ok {
		t.Error("pool sandbox should still have warm pool label (should not have been adopted)")
	}

	var updatedAdopted sandboxv1beta1.Sandbox
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "adopted-sb", Namespace: "default"}, &updatedAdopted); err != nil {
		t.Fatalf("failed to get adopted sandbox: %v", err)
	}
	if val := updatedAdopted.Labels[sandboxv1beta1.SandboxLaunchTypeLabel]; val != sandboxv1beta1.SandboxLaunchTypeWarm {
		t.Errorf("expected previously adopted sandbox to have launch type label %q, got %q; labels=%v", sandboxv1beta1.SandboxLaunchTypeWarm, val, updatedAdopted.Labels)
	}
}

func TestRecordCreationLatencyMetric(t *testing.T) {
	ctx := context.Background()
	pastTime := metav1.Time{Time: time.Now().Add(-10 * time.Second)}

	testCases := []struct {
		name                           string
		claim                          *extensionsv1beta1.SandboxClaim
		oldStatus                      *extensionsv1beta1.SandboxClaimStatus
		sandbox                        *sandboxv1beta1.Sandbox
		expectedObservations           int
		expectedControllerObservations int
		expectedAnnotation             bool
		setupReconciler                func(r *SandboxClaimReconciler)
	}{
		{
			name: "records success on first ready transition (with webhook annotation)",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "new-ready",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						asmetrics.WebhookAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:            &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations: 1,
			expectedAnnotation:   true,
		},
		{
			name: "skips recording when webhook annotation is missing",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "webhook-missing", CreationTimestamp: pastTime},
				Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:            &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations: 0,
			expectedAnnotation:   true,
		},
		{
			name: "ignores ready condition = false",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "not-ready", CreationTimestamp: pastTime},
				Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse}},
				},
			},
			oldStatus:            &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations: 0,
			expectedAnnotation:   false,
		},
		{
			name: "ignores success if status was already ready in previous loop",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "already-ready", CreationTimestamp: pastTime},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus: &extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
			expectedObservations: 0,
			expectedAnnotation:   true, // backfilled!
		},
		{
			name: "uses unknown launch type when sandbox is nil",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "unknown",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						asmetrics.WebhookAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Unknown"}},
				},
			},
			oldStatus:            &extensionsv1beta1.SandboxClaimStatus{},
			sandbox:              nil,
			expectedObservations: 1,
			expectedAnnotation:   true,
		},
		{
			name: "records controller latency using stored time",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "stored-time",
					Namespace:         "default",
					UID:               "uid-stored-time",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						asmetrics.ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
						asmetrics.WebhookAnnotation:       time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:                      &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations:           1,
			expectedControllerObservations: 1,
			expectedAnnotation:             true,
			setupReconciler: func(r *SandboxClaimReconciler) {
				key := types.NamespacedName{Name: "stored-time", Namespace: "default"}
				r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now().Add(-5 * time.Second), uid: "uid-stored-time"})
			},
		},
		{
			name: "skips claim startup latency if webhook duration is negative but records controller latency",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "future-webhook",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						asmetrics.WebhookAnnotation:       time.Now().Add(5 * time.Second).Format(time.RFC3339Nano),
						asmetrics.ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:                      &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations:           0,
			expectedControllerObservations: 1,
			expectedAnnotation:             true,
		},
		{
			name: "does not re-record when first-ready annotation already exists (e.g. after resume)",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "resumed",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						asmetrics.ClaimFirstReadyAnnotation: time.Now().Add(-1 * time.Second).Format(time.RFC3339Nano),
						asmetrics.WebhookAnnotation:         time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
						asmetrics.ObservabilityAnnotation:   time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			// Simulate resume: Ready went False (suspended) -> True again.
			oldStatus: &extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse}},
			},
			expectedObservations:           0,
			expectedControllerObservations: 0,
			expectedAnnotation:             true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset the metrics registry for a clean test
			asmetrics.ClaimStartupLatency.Reset()
			asmetrics.ClaimControllerStartupLatency.Reset()

			scheme := newScheme(t)
			warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "tpl"}}}
			// The claim must exist in the fake client so Patch can stamp the first-ready annotation.
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(warmPool, tc.claim).Build()
			r := &SandboxClaimReconciler{Client: fakeClient}

			if tc.setupReconciler != nil {
				tc.setupReconciler(r)
			}

			err := r.recordCreationLatencyMetric(ctx, tc.claim, tc.oldStatus, tc.sandbox)
			require.NoError(t, err)

			// Verify the metric was observed in the Prometheus registry
			count := testutil.CollectAndCount(asmetrics.ClaimStartupLatency)
			if count != tc.expectedObservations {
				t.Errorf("expected %d observations for ClaimStartupLatency, got %d", tc.expectedObservations, count)
			}

			countController := testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency)
			if countController != tc.expectedControllerObservations {
				t.Errorf("expected %d observations for ClaimControllerStartupLatency, got %d", tc.expectedControllerObservations, countController)
			}

			// Verify the annotation was stamped/updated in the fake client
			updatedClaim := &extensionsv1beta1.SandboxClaim{}
			err = fakeClient.Get(ctx, types.NamespacedName{Name: tc.claim.Name, Namespace: tc.claim.Namespace}, updatedClaim)
			require.NoError(t, err)

			hasAnnotation := updatedClaim.Annotations[asmetrics.ClaimFirstReadyAnnotation] != ""
			if hasAnnotation != tc.expectedAnnotation {
				t.Errorf("expected annotation presence to be %t, got %t", tc.expectedAnnotation, hasAnnotation)
			}
		})
	}
}

func TestRecordCreationLatencyMetric_ClaimFirstReadyAnnotation(t *testing.T) {
	ctx := context.Background()
	pastTime := metav1.Time{Time: time.Now().Add(-10 * time.Second)}

	t.Run("stamps claim-first-ready-at annotation on first Ready transition", func(t *testing.T) {
		asmetrics.ClaimStartupLatency.Reset()
		asmetrics.ClaimControllerStartupLatency.Reset()

		claim := &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "stamp-test",
				Namespace:         "default",
				CreationTimestamp: pastTime,
				Annotations: map[string]string{
					asmetrics.WebhookAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
				},
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
			Status: extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
		}

		scheme := newScheme(t)
		warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "tpl"}}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(warmPool, claim).Build()
		r := &SandboxClaimReconciler{Client: fakeClient}

		err := r.recordCreationLatencyMetric(ctx, claim, &extensionsv1beta1.SandboxClaimStatus{}, nil)
		require.NoError(t, err)

		// Verify the annotation was stamped.
		updated := &extensionsv1beta1.SandboxClaim{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "stamp-test", Namespace: "default"}, updated))
		if updated.Annotations[asmetrics.ClaimFirstReadyAnnotation] == "" {
			t.Fatal("expected ClaimFirstReadyAnnotation to be stamped, but it is empty")
		}

		// Verify metric was recorded exactly once.
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency))
	})

	t.Run("skips metric recording when claim-first-ready-at annotation already set", func(t *testing.T) {
		asmetrics.ClaimStartupLatency.Reset()
		asmetrics.ClaimControllerStartupLatency.Reset()

		claim := &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "already-stamped",
				Namespace:         "default",
				CreationTimestamp: pastTime,
				Annotations: map[string]string{
					asmetrics.WebhookAnnotation:         time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					asmetrics.ClaimFirstReadyAnnotation: time.Now().Add(-1 * time.Second).Format(time.RFC3339Nano),
				},
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
			Status: extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
		}

		scheme := newScheme(t)
		warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "tpl"}}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(warmPool, claim).Build()
		r := &SandboxClaimReconciler{Client: fakeClient}

		// Transition from not-Ready to Ready — but the annotation says we already recorded.
		err := r.recordCreationLatencyMetric(ctx, claim, &extensionsv1beta1.SandboxClaimStatus{}, nil)
		require.NoError(t, err)

		// No metrics should be recorded.
		require.Equal(t, 0, testutil.CollectAndCount(asmetrics.ClaimStartupLatency))
		require.Equal(t, 0, testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency))
	})

	t.Run("readiness flap does not double-count metrics", func(t *testing.T) {
		asmetrics.ClaimStartupLatency.Reset()
		asmetrics.ClaimControllerStartupLatency.Reset()

		claim := &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "flap-test",
				Namespace:         "default",
				UID:               "uid-flap",
				CreationTimestamp: pastTime,
				Annotations: map[string]string{
					asmetrics.WebhookAnnotation:       time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					asmetrics.ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
				},
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
			Status: extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
		}

		scheme := newScheme(t)
		warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "tpl"}}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(warmPool, claim).Build()
		r := &SandboxClaimReconciler{Client: fakeClient}

		key := types.NamespacedName{Name: "flap-test", Namespace: "default"}
		r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now().Add(-5 * time.Second), uid: "uid-flap"})

		// First Ready transition — should record.
		err := r.recordCreationLatencyMetric(ctx, claim, &extensionsv1beta1.SandboxClaimStatus{}, nil)
		require.NoError(t, err)
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency), "first Ready should record claim startup latency")
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency), "first Ready should record controller startup latency")

		// Simulate readiness flap: Ready → NotReady → Ready.
		// Re-read the claim to pick up the stamped annotation.
		require.NoError(t, fakeClient.Get(ctx, key, claim))

		// Re-populate observedTimes (as the UpdateFunc predicate would).
		r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now().Add(-5 * time.Second), uid: "uid-flap"})

		// Second Ready transition — oldStatus shows not-Ready (simulating the flap back),
		// but the annotation guard should prevent recording.
		err = r.recordCreationLatencyMetric(ctx, claim, &extensionsv1beta1.SandboxClaimStatus{}, nil)
		require.NoError(t, err)

		// Counts should remain at 1 — no double-counting.
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency), "readiness flap should not double-count claim startup latency")
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency), "readiness flap should not double-count controller startup latency")

		// observedTimes entry should be drained.
		_, loaded := r.observedTimes.Load(key)
		require.False(t, loaded, "observedTimes entry should be drained after annotation guard")
	})

	t.Run("annotation patch failure returns error and metrics are still recorded", func(t *testing.T) {
		asmetrics.ClaimStartupLatency.Reset()
		asmetrics.ClaimControllerStartupLatency.Reset()

		claim := &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "patch-fail",
				Namespace:         "default",
				UID:               "uid-patch-fail",
				CreationTimestamp: pastTime,
				Annotations: map[string]string{
					asmetrics.WebhookAnnotation:       time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					asmetrics.ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
				},
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
			Status: extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
		}

		scheme := newScheme(t)
		warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "tpl"}}}
		inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(warmPool, claim).Build()
		// Fail only the first Patch (the annotation stamp in the happy path),
		// then succeed on subsequent Patches (the backfill in the retry).
		fc := &claimPatchFailClient{Client: inner, err: fmt.Errorf("simulated patch failure"), maxFailures: 1}
		r := &SandboxClaimReconciler{Client: fc}

		key := types.NamespacedName{Name: "patch-fail", Namespace: "default"}
		r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now().Add(-5 * time.Second), uid: "uid-patch-fail"})

		// First call: metrics should be recorded but the Patch should fail.
		err := r.recordCreationLatencyMetric(ctx, claim, &extensionsv1beta1.SandboxClaimStatus{}, nil)
		require.Error(t, err, "expected error when annotation patch fails")
		require.Contains(t, err.Error(), "stamp claim first-ready annotation")

		// Metrics were recorded before the Patch attempt.
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency), "metrics should be recorded before Patch")
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency), "controller metrics should be recorded before Patch")

		// Re-read the claim from the fake client to simulate a real reconciler
		// re-fetching the object (the in-memory annotation was set but not persisted).
		retryClaim := &extensionsv1beta1.SandboxClaim{}
		require.NoError(t, inner.Get(ctx, key, retryClaim))
		require.Empty(t, retryClaim.Annotations[asmetrics.ClaimFirstReadyAnnotation], "annotation should not be persisted after failed Patch")

		// Simulate the retry reconcile: updateStatus already persisted Ready=True,
		// so on retry originalClaimStatus will show Ready=True. The oldReady guard
		// prevents duplicate recording and backfills the annotation.
		retryOldStatus := &extensionsv1beta1.SandboxClaimStatus{
			Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
		}
		err = r.recordCreationLatencyMetric(ctx, retryClaim, retryOldStatus, nil)
		require.NoError(t, err)

		// Counts should remain at 1 — no duplicate recording on retry.
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency), "retry should not double-count")
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency), "retry should not double-count controller metrics")

		// Verify the backfill annotation was stamped.
		updated := &extensionsv1beta1.SandboxClaim{}
		require.NoError(t, inner.Get(ctx, key, updated))
		require.Equal(t, asmetrics.ClaimFirstReadyUnknownSentinel, updated.Annotations[asmetrics.ClaimFirstReadyAnnotation], "retry should backfill annotation")
	})

	t.Run("patch failure followed by mid-flap backfills annotation and prevents double-count", func(t *testing.T) {
		asmetrics.ClaimStartupLatency.Reset()
		asmetrics.ClaimControllerStartupLatency.Reset()

		claim := &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "flap-patch-fail",
				Namespace:         "default",
				UID:               "uid-flap-patch-fail",
				CreationTimestamp: pastTime,
				Annotations: map[string]string{
					asmetrics.WebhookAnnotation:       time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					asmetrics.ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
				},
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
			Status: extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
		}

		scheme := newScheme(t)
		warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "tpl"}}}
		inner := fake.NewClientBuilder().WithScheme(scheme).WithObjects(warmPool, claim).Build()
		// Fail the first Patch (happy-path annotation stamp), succeed on the second (backfill).
		fc := &claimPatchFailClient{Client: inner, err: fmt.Errorf("simulated patch failure"), maxFailures: 1}
		r := &SandboxClaimReconciler{Client: fc}

		key := types.NamespacedName{Name: "flap-patch-fail", Namespace: "default"}
		r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now().Add(-5 * time.Second), uid: "uid-flap-patch-fail"})

		// Step 1: First Ready transition. Metrics recorded, Patch fails.
		err := r.recordCreationLatencyMetric(ctx, claim, &extensionsv1beta1.SandboxClaimStatus{}, nil)
		require.Error(t, err)
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency))
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency))

		// Re-read the claim to simulate a real reconciler re-fetching.
		// The in-memory annotation was set but not persisted.
		flapClaim := &extensionsv1beta1.SandboxClaim{}
		require.NoError(t, inner.Get(ctx, key, flapClaim))
		require.Empty(t, flapClaim.Annotations[asmetrics.ClaimFirstReadyAnnotation])

		// Step 2: Mid-flap — claim went not-Ready, but was previously Ready.
		// newReady=False, oldReady=True, annotation missing → backfill fires.
		flapClaim.Status.Conditions = []metav1.Condition{{
			Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse,
		}}
		readyOldStatus := &extensionsv1beta1.SandboxClaimStatus{
			Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
		}
		err = r.recordCreationLatencyMetric(ctx, flapClaim, readyOldStatus, nil)
		require.NoError(t, err)

		// Verify backfill was stamped.
		updated := &extensionsv1beta1.SandboxClaim{}
		require.NoError(t, inner.Get(ctx, key, updated))
		require.Equal(t, asmetrics.ClaimFirstReadyUnknownSentinel, updated.Annotations[asmetrics.ClaimFirstReadyAnnotation])

		// Step 3: Flap back to Ready — newReady=True, oldReady=False.
		// The annotation guard should prevent re-recording.
		updated.Status.Conditions = []metav1.Condition{{
			Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue,
		}}
		r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now().Add(-5 * time.Second), uid: "uid-flap-patch-fail"})
		err = r.recordCreationLatencyMetric(ctx, updated, &extensionsv1beta1.SandboxClaimStatus{}, nil)
		require.NoError(t, err)

		// Counts should remain at 1 — no double-count despite the flap.
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency), "mid-flap backfill should prevent double-count")
		require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimControllerStartupLatency), "mid-flap backfill should prevent double-count")
	})
}

// claimPatchFailClient wraps a client.Client and fails the first maxFailures
// Patch calls that write the claim-first-ready annotation, then delegates to
// the inner client.
type claimPatchFailClient struct {
	client.Client
	err         error
	failures    int
	maxFailures int // 0 means fail forever
}

func (c *claimPatchFailClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
		data, err := patch.Data(obj)
		if err == nil && bytes.Contains(data, []byte(asmetrics.ClaimFirstReadyAnnotation)) {
			if c.maxFailures == 0 || c.failures < c.maxFailures {
				c.failures++
				return c.err
			}
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestClientClaimLatencyMetric(t *testing.T) {
	ctx := context.Background()
	pastTime := metav1.Time{Time: time.Now().Add(-10 * time.Second)}

	testCases := []struct {
		name                 string
		claim                *extensionsv1beta1.SandboxClaim
		oldStatus            *extensionsv1beta1.SandboxClaimStatus
		expectedObservations int
	}{
		{
			name: "records client latency using annotation",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "client-time",
					Namespace:         "default",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						asmetrics.ClientAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
					},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "tpl"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:            &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations: 1,
		},
		{
			name: "ignores client latency if annotation is missing",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "no-client-time",
					Namespace:         "default",
					CreationTimestamp: pastTime,
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "tpl"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:            &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations: 0,
		},
		{
			name: "ignores client latency if annotation is not in parsable format",
			claim: &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "invalid-client-time",
					Namespace:         "default",
					CreationTimestamp: pastTime,
					Annotations: map[string]string{
						asmetrics.ClientAnnotation: "1713689880",
					},
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "tpl"}},
				Status: extensionsv1beta1.SandboxClaimStatus{
					Conditions: []metav1.Condition{{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			},
			oldStatus:            &extensionsv1beta1.SandboxClaimStatus{},
			expectedObservations: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			asmetrics.ClientClaimStartupLatency.Reset()

			fakeClient := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.claim).Build()
			r := &SandboxClaimReconciler{Client: fakeClient}

			require.NoError(t, r.recordCreationLatencyMetric(ctx, tc.claim, tc.oldStatus, nil))

			count := testutil.CollectAndCount(asmetrics.ClientClaimStartupLatency)
			if count != tc.expectedObservations {
				t.Errorf("expected %d observations for ClientClaimStartupLatency, got %d", tc.expectedObservations, count)
			}
		})
	}
}

func TestSandboxClaimCreationMetric(t *testing.T) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
	}

	t.Run("Cold Start", func(t *testing.T) {
		asmetrics.SandboxClaimCreationTotal.Reset()
		scheme := newScheme(t)
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, warmPool, claim).WithStatusSubresource(claim).Build()
		reconciler := &SandboxClaimReconciler{
			Client:           client,
			Scheme:           scheme,
			Recorder:         events.NewFakeRecorder(10),
			WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
			Tracer:           asmetrics.NewNoOp(),
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: "default"}}
		_, err := reconciler.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify metric
		val := testutil.ToFloat64(asmetrics.SandboxClaimCreationTotal.WithLabelValues("default", "test-template", asmetrics.LaunchTypeCold, "test-warmpool", "not_ready", "unknown"))
		if val != 1 {
			t.Errorf("expected metric count 1, got %v", val)
		}

		// Verify created Sandbox labels are absent
		sb := &sandboxv1beta1.Sandbox{}
		if err := client.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: "default"}, sb); err != nil {
			t.Fatalf("failed to get created sandbox: %v", err)
		}
		if val, exists := sb.Labels[sandboxv1beta1.CreatedByLabel]; exists && val != "" {
			t.Errorf("expected sandbox created-by label to be absent, got %q", val)
		}
		if val, exists := sb.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.CreatedByLabel]; exists && val != "" {
			t.Errorf("expected sandbox pod template created-by label to be absent, got %q", val)
		}
	})

	t.Run("Warm Start", func(t *testing.T) {
		asmetrics.SandboxClaimCreationTotal.Reset()

		// Create a warm pool sandbox
		poolNameHash := sandboxcontrollers.NameHash("test-warmpool")
		warmSandbox := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "warm-sb",
				Namespace: "default",
				Labels: map[string]string{
					warmPoolSandboxLabel:          poolNameHash,
					sandboxTemplateRefHash:        sandboxcontrollers.NameHash("test-template"),
					sandboxv1beta1.CreatedByLabel: "controller",
				},
				Annotations: map[string]string{
					sandboxv1beta1.SandboxTemplateRefAnnotation: "test-template",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: extensionsv1beta1.GroupVersion.String(),
						Kind:       extensionsv1beta1.SandboxWarmPoolKind,
						Name:       "test-warmpool",
						UID:        "pool-uid",
						Controller: new(true),
					},
				},
			},
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
			Status: sandboxv1beta1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
				}},
				PodIPs: []string{testNetworkedPodIP},
			},
		}

		scheme := newScheme(t)
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, warmPool, claim, warmSandbox).WithStatusSubresource(claim).Build()
		warmSandboxQueue := queue.NewSimpleSandboxQueue()
		if isAdoptable(warmSandbox) == nil {
			warmPoolName := getWarmPoolName(warmSandbox)
			namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(warmSandbox.Namespace, warmPoolName)
			key := queue.SandboxKey{Namespace: warmSandbox.Namespace, Name: warmSandbox.Name, NodeName: warmSandbox.Status.NodeName}
			warmSandboxQueue.Add(namespacedWarmPoolName, key)
		}

		reconciler := &SandboxClaimReconciler{
			Client:           client,
			Scheme:           scheme,
			Recorder:         events.NewFakeRecorder(10),
			WarmSandboxQueue: warmSandboxQueue,
			Tracer:           asmetrics.NewNoOp(),
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: "default"}}
		_, err := reconciler.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		// Verify metric
		val := testutil.ToFloat64(asmetrics.SandboxClaimCreationTotal.WithLabelValues("default", "test-template", asmetrics.LaunchTypeWarm, "test-warmpool", "ready", "unknown"))
		if val != 1 {
			t.Errorf("expected metric count 1, got %v", val)
		}

		// Verify adopted Sandbox labels are removed (since claim lacks it)
		sb := &sandboxv1beta1.Sandbox{}
		if err := client.Get(context.Background(), types.NamespacedName{Name: "warm-sb", Namespace: "default"}, sb); err != nil {
			t.Fatalf("failed to get adopted sandbox: %v", err)
		}
		if val, exists := sb.Labels[sandboxv1beta1.CreatedByLabel]; exists && val != "" {
			t.Errorf("expected sandbox created-by label to be absent, got %q", val)
		}
		if val, exists := sb.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.CreatedByLabel]; exists && val != "" {
			t.Errorf("expected sandbox pod template created-by label to be absent, got %q", val)
		}
	})
}

func TestGetLaunchType(t *testing.T) {
	testCases := []struct {
		name    string
		sandbox *sandboxv1beta1.Sandbox
		want    string
	}{
		{
			name: "nil sandbox is unknown",
			want: asmetrics.LaunchTypeUnknown,
		},
		{
			name: "warm launch label is warm",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
					},
				},
			},
			want: asmetrics.LaunchTypeWarm,
		},
		{
			name:    "missing launch label defaults cold",
			sandbox: &sandboxv1beta1.Sandbox{},
			want:    asmetrics.LaunchTypeCold,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, getLaunchType(tc.sandbox))
		})
	}
}

func TestInitializeSandboxLaunchTypeLabel(t *testing.T) {
	testCases := []struct {
		name          string
		existingLabel string
		launchType    string
		want          string
	}{
		{
			name:          "existing warm label is not overwritten by cold",
			existingLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
			launchType:    sandboxv1beta1.SandboxLaunchTypeCold,
			want:          sandboxv1beta1.SandboxLaunchTypeWarm,
		},
		{
			name:          "existing cold label is not overwritten by warm",
			existingLabel: sandboxv1beta1.SandboxLaunchTypeCold,
			launchType:    sandboxv1beta1.SandboxLaunchTypeWarm,
			want:          sandboxv1beta1.SandboxLaunchTypeCold,
		},
		{
			name:       "missing label is initialized",
			launchType: sandboxv1beta1.SandboxLaunchTypeWarm,
			want:       sandboxv1beta1.SandboxLaunchTypeWarm,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			sandbox := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			}
			if tc.existingLabel != "" {
				sandbox.Labels = map[string]string{
					sandboxv1beta1.SandboxLaunchTypeLabel: tc.existingLabel,
				}
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(sandbox).
				Build()
			reconciler := &SandboxClaimReconciler{
				Client: fakeClient,
				Scheme: scheme,
			}

			err := reconciler.initializeSandboxLaunchTypeLabel(context.Background(), sandbox, tc.launchType)
			require.NoError(t, err)

			var updated sandboxv1beta1.Sandbox
			err = fakeClient.Get(context.Background(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, &updated)
			require.NoError(t, err)
			require.Equal(t, tc.want, updated.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])
		})
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := sandboxv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: (%v)", err)
	}
	if err := extensionsv1beta1.AddToScheme(scheme); err != nil {
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
	if sandbox, ok := obj.(*sandboxv1beta1.Sandbox); ok {
		if c.conflictCount < c.maxConflicts {
			c.conflictCount++
			return k8errors.NewConflict(sandboxv1beta1.Resource("sandboxes"), sandbox.Name, fmt.Errorf("simulated conflict"))
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *conflictClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if sandbox, ok := obj.(*sandboxv1beta1.Sandbox); ok {
		if c.conflictCount < c.maxConflicts {
			c.conflictCount++
			return k8errors.NewConflict(sandboxv1beta1.Resource("sandboxes"), sandbox.Name, fmt.Errorf("simulated conflict"))
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestSandboxClaimTimingPredicates(t *testing.T) {
	r := &SandboxClaimReconciler{}
	pred := r.getTimingPredicate()

	claim1 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "uid-1"},
	}
	claim2 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "uid-2"},
	}
	key := types.NamespacedName{Name: "test-claim", Namespace: "default"}
	pastTime := time.Now().Add(-10 * time.Second)

	testCases := []struct {
		name    string
		setup   func(r *SandboxClaimReconciler)
		trigger func(p predicate.Predicate) bool
		verify  func(t *testing.T, r *SandboxClaimReconciler)
	}{
		{
			name: "Create stores time and UID",
			trigger: func(p predicate.Predicate) bool {
				return p.Create(event.CreateEvent{Object: claim1})
			},
			verify: func(t *testing.T, r *SandboxClaimReconciler) {
				entry, ok := r.observedTimes.Load(key)
				if !ok {
					t.Fatal("Expected entry in map after Create")
				}
				if entry.uid != "uid-1" {
					t.Errorf("Expected UID uid-1, got %s", entry.uid)
				}
			},
		},
		{
			name: "Update with same UID preserves",
			setup: func(r *SandboxClaimReconciler) {
				r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now(), uid: "uid-1"})
			},
			trigger: func(p predicate.Predicate) bool {
				return p.Update(event.UpdateEvent{ObjectNew: claim1, ObjectOld: claim1})
			},
			verify: func(t *testing.T, r *SandboxClaimReconciler) {
				entry, ok := r.observedTimes.Load(key)
				if !ok {
					t.Fatal("Expected entry in map after Update")
				}
				if entry.uid != "uid-1" {
					t.Errorf("Expected UID uid-1, got %s", entry.uid)
				}
			},
		},
		{
			name: "Update with different UID overwrites",
			setup: func(r *SandboxClaimReconciler) {
				r.observedTimes.Store(key, observedTimeEntry{timestamp: pastTime, uid: "uid-1"})
			},
			trigger: func(p predicate.Predicate) bool {
				return p.Update(event.UpdateEvent{ObjectNew: claim2, ObjectOld: claim1})
			},
			verify: func(t *testing.T, r *SandboxClaimReconciler) {
				entry, ok := r.observedTimes.Load(key)
				if !ok {
					t.Fatal("Expected entry in map after Update with new UID")
				}
				if entry.uid != "uid-2" {
					t.Errorf("Expected UID uid-2 after update, got %s", entry.uid)
				}
				if !entry.timestamp.After(pastTime) {
					t.Error("Expected timestamp to be updated to a newer value")
				}
			},
		},
		{
			name: "Delete with mismatch UID does not delete",
			setup: func(r *SandboxClaimReconciler) {
				r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now(), uid: "uid-2"})
			},
			trigger: func(p predicate.Predicate) bool {
				return p.Delete(event.DeleteEvent{Object: claim1}) // claim1 has uid-1
			},
			verify: func(t *testing.T, r *SandboxClaimReconciler) {
				_, ok := r.observedTimes.Load(key)
				if !ok {
					t.Error("Entry should NOT be deleted when UID mismatches")
				}
			},
		},
		{
			name: "Delete with matching UID deletes",
			setup: func(r *SandboxClaimReconciler) {
				r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now(), uid: "uid-1"})
			},
			trigger: func(p predicate.Predicate) bool {
				return p.Delete(event.DeleteEvent{Object: claim1}) // claim1 has uid-1
			},
			verify: func(t *testing.T, r *SandboxClaimReconciler) {
				_, ok := r.observedTimes.Load(key)
				if ok {
					t.Error("Entry should be deleted when UID matches")
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r.observedTimes = observedTimeMap{} // Reset map for each test case
			if tc.setup != nil {
				tc.setup(r)
			}
			res := tc.trigger(pred)
			if !res {
				t.Error("expected predicate to return true")
			}
			tc.verify(t, r)
		})
	}
}

func TestObservedTimeMapCompareAndDeletePreservesNewerUID(t *testing.T) {
	key := types.NamespacedName{Name: "test-claim", Namespace: "default"}
	oldEntry := observedTimeEntry{timestamp: time.Now().Add(-10 * time.Second), uid: "uid-1"}
	newEntry := observedTimeEntry{timestamp: time.Now(), uid: "uid-2"}

	var observed observedTimeMap
	observed.Store(key, oldEntry)

	staleLoaded, ok := observed.Load(key)
	require.True(t, ok)
	require.Equal(t, oldEntry, staleLoaded)

	// Simulate a concurrent UpdateFunc overwriting the entry for a recreated claim.
	observed.Store(key, newEntry)

	deleted := observed.CompareAndDelete(key, staleLoaded)
	require.False(t, deleted, "stale cleanup must not delete a newer UID entry")

	current, ok := observed.Load(key)
	require.True(t, ok)
	require.Equal(t, newEntry, current)
}

func TestGetOrRecordObservedTime(t *testing.T) {
	claim1 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "uid-1"},
	}
	claim2 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "uid-2"},
	}
	pastTime := time.Now().Add(-10 * time.Second)

	testCases := []struct {
		name               string
		claimToRecord      *extensionsv1beta1.SandboxClaim
		initialKey         types.NamespacedName
		initialEntry       *observedTimeEntry
		expectedUID        types.UID
		expectNewTimestamp bool
		expectedReturnTime time.Time
	}{
		{
			name:               "New Entry stores time and returns it",
			claimToRecord:      claim1,
			expectedUID:        "uid-1",
			expectNewTimestamp: true,
		},
		{
			name:               "Existing Entry with same UID returns loaded timestamp",
			claimToRecord:      claim1,
			initialKey:         types.NamespacedName{Name: claim1.Name, Namespace: claim1.Namespace},
			initialEntry:       &observedTimeEntry{timestamp: pastTime, uid: "uid-1"},
			expectedUID:        "uid-1",
			expectNewTimestamp: false,
			expectedReturnTime: pastTime,
		},
		{
			name:               "Existing Entry with different UID overwrites and returns new timestamp",
			claimToRecord:      claim2,
			initialKey:         types.NamespacedName{Name: claim1.Name, Namespace: claim1.Namespace},
			initialEntry:       &observedTimeEntry{timestamp: pastTime, uid: claim1.UID},
			expectedUID:        "uid-2",
			expectNewTimestamp: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &SandboxClaimReconciler{}
			if tc.initialEntry != nil {
				r.observedTimes.Store(tc.initialKey, *tc.initialEntry)
			}

			res := r.getOrRecordObservedTime(tc.claimToRecord)

			// Verify map state for the recorded claim
			recordedKey := types.NamespacedName{Name: tc.claimToRecord.Name, Namespace: tc.claimToRecord.Namespace}
			entry, ok := r.observedTimes.Load(recordedKey)
			if !ok {
				t.Fatal("Expected entry in map")
			}

			if entry.uid != tc.expectedUID {
				t.Errorf("Expected UID %s, got %s", tc.expectedUID, entry.uid)
			}

			if tc.expectNewTimestamp {
				// Expect a new timestamp
				if entry.timestamp.IsZero() {
					t.Error("Expected timestamp to be set")
				}
				if tc.initialEntry != nil && entry.timestamp.Equal(tc.initialEntry.timestamp) {
					t.Error("Expected a different timestamp than the initial one")
				}
				if !res.Equal(entry.timestamp) {
					t.Error("Expected returned time to match stored time")
				}
			} else {
				// Expect specific timestamp
				if !entry.timestamp.Equal(tc.expectedReturnTime) {
					t.Errorf("Expected timestamp %v, got %v", tc.expectedReturnTime, entry.timestamp)
				}
				if !res.Equal(tc.expectedReturnTime) {
					t.Errorf("Expected returned time %v, got %v", tc.expectedReturnTime, res)
				}
			}
		})
	}
}

func TestSandboxClaimReconcileCleanup(t *testing.T) {
	newReconcilerFor := func(t *testing.T, objs ...client.Object) *SandboxClaimReconciler {
		t.Helper()
		scheme := newScheme(t)
		objs = append(objs, &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"}, Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}}})
		fc := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objs...).
			WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
			Build()
		return &SandboxClaimReconciler{
			Client:           fc,
			Scheme:           scheme,
			WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
			Recorder:         events.NewFakeRecorder(10),
			Tracer:           asmetrics.NewNoOp(),
		}
	}

	makeReadyClaim := func(name string) *extensionsv1beta1.SandboxClaim {
		return &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name),
				Annotations: map[string]string{
					asmetrics.ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
				},
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{
				WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
			},
			Status: extensionsv1beta1.SandboxClaimStatus{
				Conditions: []metav1.Condition{{
					Type:   string(sandboxv1beta1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				}},
			},
		}
	}

	ctrlBool := true
	makeOwnedReadySandbox := func(cl *extensionsv1beta1.SandboxClaim) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cl.Name,
				Namespace: cl.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: extensionsv1beta1.GroupVersion.String(),
					Kind:       extensionsv1beta1.SandboxClaimKind,
					Name:       cl.Name,
					UID:        cl.UID,
					Controller: &ctrlBool,
				}},
			},
			Status: sandboxv1beta1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type:   string(sandboxv1beta1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				}},
			},
		}
	}

	reconcileAll := func(t *testing.T, r *SandboxClaimReconciler, claims []*extensionsv1beta1.SandboxClaim) {
		t.Helper()
		for _, cl := range claims {
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cl.Name, Namespace: cl.Namespace}}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("Reconcile(%s): %v", cl.Name, err)
			}
		}
	}

	testCases := []struct {
		name        string
		build       func(t *testing.T) (*SandboxClaimReconciler, []*extensionsv1beta1.SandboxClaim)
		action      func(t *testing.T, r *SandboxClaimReconciler, claims []*extensionsv1beta1.SandboxClaim)
		wantEntries int
	}{
		{
			// Reconcile on a missing claim removes the observedTimes entry via the NotFound fallback.
			name: "NotFound reconcile removes stale entry",
			build: func(t *testing.T) (*SandboxClaimReconciler, []*extensionsv1beta1.SandboxClaim) {
				cl := makeReadyClaim("stale-claim")
				r := newReconcilerFor(t)
				key := types.NamespacedName{Name: cl.Name, Namespace: cl.Namespace}
				r.observedTimes.Store(key, observedTimeEntry{timestamp: time.Now(), uid: cl.UID})
				return r, []*extensionsv1beta1.SandboxClaim{cl}
			},
			action: func(t *testing.T, r *SandboxClaimReconciler, claims []*extensionsv1beta1.SandboxClaim) {
				reconcileAll(t, r, claims)
			},
			wantEntries: 0,
		},
		{
			// CreateFunc adds an entry; recordControllerStartupLatency removes it on the first
			// Not-Ready → Ready transition detected by recordCreationLatencyMetric.
			name: "new claim transitioning to Ready cleans its entry",
			build: func(t *testing.T) (*SandboxClaimReconciler, []*extensionsv1beta1.SandboxClaim) {
				cl := &extensionsv1beta1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "new-claim", Namespace: "default", UID: "new-claim"},
					Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				}
				r := newReconcilerFor(t, cl, makeOwnedReadySandbox(cl))
				return r, []*extensionsv1beta1.SandboxClaim{cl}
			},
			action: func(t *testing.T, r *SandboxClaimReconciler, claims []*extensionsv1beta1.SandboxClaim) {
				pred := r.getTimingPredicate()
				for _, cl := range claims {
					pred.Create(event.CreateEvent{Object: cl})
				}
				reconcileAll(t, r, claims)
			},
			wantEntries: 0,
		},
		{
			// Simulates a controller restart where the informer replays UpdateFunc for existing,
			// already-Ready claims. The reconciler must correctly clean up the observed time entries.
			name: "already-Ready claims are cleaned up after restart simulation",
			build: func(t *testing.T) (*SandboxClaimReconciler, []*extensionsv1beta1.SandboxClaim) {
				const n = 10
				objs := make([]client.Object, 0, n*2)
				claims := make([]*extensionsv1beta1.SandboxClaim, n)
				for i := range n {
					cl := makeReadyClaim(fmt.Sprintf("ready-claim-%d", i))
					claims[i] = cl
					objs = append(objs, cl, makeOwnedReadySandbox(cl))
				}
				return newReconcilerFor(t, objs...), claims
			},
			action: func(t *testing.T, r *SandboxClaimReconciler, claims []*extensionsv1beta1.SandboxClaim) {
				pred := r.getTimingPredicate()
				for _, cl := range claims {
					pred.Update(event.UpdateEvent{ObjectOld: cl, ObjectNew: cl})
				}
				reconcileAll(t, r, claims)
			},
			wantEntries: 0,
		},
		{

			// Simulates an update for already ready claim.
			// The reconciler must correctly clean up the observed time entries.
			name: "post-Ready UpdateFunc re-creates entry that is then cleaned on next reconcile",
			build: func(t *testing.T) (*SandboxClaimReconciler, []*extensionsv1beta1.SandboxClaim) {
				cl := &extensionsv1beta1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "post-ready-claim", Namespace: "default", UID: "post-ready"},
					Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"}},
				}
				r := newReconcilerFor(t, cl, makeOwnedReadySandbox(cl))
				return r, []*extensionsv1beta1.SandboxClaim{cl}
			},
			action: func(t *testing.T, r *SandboxClaimReconciler, claims []*extensionsv1beta1.SandboxClaim) {
				pred := r.getTimingPredicate()
				// Step 1: CreateFunc → entry added
				for _, cl := range claims {
					pred.Create(event.CreateEvent{Object: cl})
				}
				// Step 2: First reconcile → Not-Ready → Ready transition → entry cleaned.
				reconcileAll(t, r, claims)
				// Step 3: Post-Ready UpdateFunc
				for _, cl := range claims {
					pred.Update(event.UpdateEvent{ObjectOld: cl, ObjectNew: cl})
				}
				// Step 4: Reconcile with old=Ready, new=Ready
				reconcileAll(t, r, claims)
			},
			wantEntries: 0,
		},
		{
			// DeleteFunc is the sole cleanup path for entries that accumulated after a restart.
			// Firing it for each claim fully drains the map.
			name: "DeleteFunc drains entries accumulated after restart simulation",
			build: func(t *testing.T) (*SandboxClaimReconciler, []*extensionsv1beta1.SandboxClaim) {
				const n = 10
				objs := make([]client.Object, 0, n*2)
				claims := make([]*extensionsv1beta1.SandboxClaim, n)
				for i := range n {
					cl := makeReadyClaim(fmt.Sprintf("rescue-claim-%d", i))
					claims[i] = cl
					objs = append(objs, cl, makeOwnedReadySandbox(cl))
				}
				return newReconcilerFor(t, objs...), claims
			},
			action: func(t *testing.T, r *SandboxClaimReconciler, claims []*extensionsv1beta1.SandboxClaim) {
				pred := r.getTimingPredicate()
				for _, cl := range claims {
					pred.Update(event.UpdateEvent{ObjectOld: cl, ObjectNew: cl})
				}
				reconcileAll(t, r, claims)
				for _, cl := range claims {
					pred.Delete(event.DeleteEvent{Object: cl})
				}
			},
			wantEntries: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r, claims := tc.build(t)

			tc.action(t, r, claims)

			gotEntries := countObservedTimesEntries(r)
			if gotEntries != tc.wantEntries {
				t.Errorf("observedTimes has %d entries, want %d", gotEntries, tc.wantEntries)
			}
		})
	}
}

// countObservedTimesEntries returns the number of live entries in the observedTimes map.
func countObservedTimesEntries(r *SandboxClaimReconciler) int {
	count := 0
	r.observedTimes.inner.Range(func(_, _ any) bool { count++; return true })
	return count
}

func TestVerifySandboxCandidate_NamespaceIsolation(t *testing.T) {
	templateName := "test-template"
	templateHash := sandboxcontrollers.NameHash(templateName)

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "namespace-a",
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{
				Name: "test-warmpool",
			},
		},
	}

	// 1. Valid Sandbox (Same Namespace)
	validSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-sandbox",
			Namespace: "namespace-a",
			Labels: map[string]string{
				sandboxTemplateRefHash: templateHash,
				warmPoolSandboxLabel:   "pool-hash-123",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-warmpool",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
		},
	}

	// 2. Invalid Sandbox (Different Namespace, but identical hash)
	invalidSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-sandbox",
			Namespace: "namespace-b",
			Labels: map[string]string{
				sandboxTemplateRefHash: templateHash,
				warmPoolSandboxLabel:   "pool-hash-123",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-warmpool",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
		},
	}

	// Test Valid: Should return nil (no error)
	if err := verifySandboxCandidate(validSandbox, claim); err != nil {
		t.Errorf("Expected valid sandbox in the same namespace to be accepted, but got: %v", err)
	}

	// Test Invalid: Should return an error about cross-namespace adoption
	err := verifySandboxCandidate(invalidSandbox, claim)
	if err == nil {
		t.Fatal("FATAL: Cross-namespace sandbox was successfully verified! The namespace check is missing.")
	} else if !errors.Is(err, ErrCrossNamespaceAdoption) {
		t.Errorf("Expected ErrCrossNamespaceAdoption, but got a different error: %v", err)
	}
}

func TestSandboxClaimClearsAssignedSandboxOwnedByAnotherClaim(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fromLabel       bool
		ownerAPIVersion string
	}{
		{name: "annotation", ownerAPIVersion: extensionsv1beta1.GroupVersion.String()},
		{name: "deprecated label", fromLabel: true, ownerAPIVersion: extensionsv1beta1.GroupVersion.String()},
		{name: "deprecated label with v1alpha1 owner reference", fromLabel: true, ownerAPIVersion: "extensions.agents.x-k8s.io/v1alpha1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			ctx := context.Background()

			claim := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "claim-uid",
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
				},
			}
			if tc.fromLabel {
				claim.Labels = map[string]string{
					extensionsv1beta1.DeprecatedAssignedSandboxNameLabel: "lost-sandbox",
				}
			} else {
				claim.Annotations = map[string]string{
					extensionsv1beta1.AssignedSandboxNameAnnotation: "lost-sandbox",
				}
			}
			lostSandbox := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "lost-sandbox",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: tc.ownerAPIVersion,
						Kind:       "SandboxClaim",
						Name:       "other-claim",
						UID:        "other-claim-uid",
						Controller: ptr.To(true), // nolint:modernize
					}},
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(claim, lostSandbox).
				Build()
			reconciler := &SandboxClaimReconciler{
				Client:           fakeClient,
				Scheme:           scheme,
				Tracer:           asmetrics.NewNoOp(),
				WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
			}

			sandbox, err := reconciler.getOrCreateSandbox(ctx, claim, nil)
			require.NoError(t, err)
			require.Nil(t, sandbox)

			var updatedSandbox sandboxv1beta1.Sandbox
			require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(lostSandbox), &updatedSandbox))
			controllerRef := metav1.GetControllerOf(&updatedSandbox)
			require.NotNil(t, controllerRef)
			require.Equal(t, types.UID("other-claim-uid"), controllerRef.UID)

			var updatedClaim extensionsv1beta1.SandboxClaim
			require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(claim), &updatedClaim))
			if tc.fromLabel {
				require.NotContains(t, updatedClaim.Labels, extensionsv1beta1.DeprecatedAssignedSandboxNameLabel)
			} else {
				require.NotContains(t, updatedClaim.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation)
			}
		})
	}
}

// TestSandboxClaimPreventsDuplicateAdoptionDuringCacheLag verifies that during informer cache lag,
// the assigned sandbox annotation on the claim is used to identify the previously adopted Sandbox,
// preventing duplicate adoptions from the warm pool.
func TestSandboxClaimPreventsDuplicateAdoptionDuringCacheLag(t *testing.T) {
	scheme := newScheme(t)

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "adopted-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "warmpool-uid-123"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	adoptedSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "adopted-sb",
			Namespace:  "default",
			UID:        "adopted-sb-uid",
			Generation: 1,
			Labels: map[string]string{
				extensionsv1beta1.SandboxIDLabel: "claim-uid-123",
				sandboxTemplateRefHash:           sandboxcontrollers.NameHash("test-template"),
				warmPoolSandboxLabel:             sandboxcontrollers.NameHash("test-pool"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					extensionsv1beta1.SandboxIDLabel: "claim-uid-123",
				},
			},
		}},
		},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "Ready",
			}},
		},
	}

	// Another sandbox in the warm pool that we want to make sure doesn't get adopted
	poolNameHash := sandboxcontrollers.NameHash("test-pool")
	extraSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-sb-extra",
			Namespace: "default",
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
			PodIPs: []string{testNetworkedPodIP},
		},
	}

	// Simulate informer cache lag: while cacheStale is true, every Get of the
	// adopted sandbox returns the frozen warm-pool-owned view, no matter what
	// was patched.
	cacheStale := true
	adoptedSandbox.ResourceVersion = "100"
	staleSandbox := adoptedSandbox.DeepCopy()
	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adoptedSandbox, extraSandbox).
		WithStatusSubresource(claim).
		Build()
	fakeClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "adopted-sb" && cacheStale {
				staleSandbox.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	if isAdoptable(extraSandbox) == nil {
		warmPoolName := getWarmPoolName(extraSandbox)
		namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(extraSandbox.Namespace, warmPoolName)
		key := queue.SandboxKey{Namespace: extraSandbox.Namespace, Name: extraSandbox.Name, NodeName: extraSandbox.Status.NodeName}
		warmSandboxQueue.Add(namespacedWarmPoolName, key)
	}

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: warmSandboxQueue,
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}

	// Run reconcile. completeAdoption patches the sandbox in place and the client
	// writes the server response back into the object, so adoption finalizes in this
	// same pass: no error, no polling requeue (later passes are watch-driven via the
	// Owns() Sandbox watch), and the claim status records the adopted sandbox.
	res, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Expected reconcile to finalize adoption without error, got error: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("Expected no requeue (convergence is watch-driven), got %+v", res)
	}

	// Verify that the claim status WAS finalized with the adopted sandbox in the same pass.
	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}

	if updatedClaim.Status.SandboxStatus.Name != "adopted-sb" {
		t.Errorf("expected claim status to be finalized with 'adopted-sb' in the adoption pass, got %q", updatedClaim.Status.SandboxStatus.Name)
	}

	// Adoption is finalized from a Ready warm-pool candidate.
	readyCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if readyCondition == nil {
		t.Fatal("expected Ready condition to be set after the adoption pass")
	}
	if readyCondition.Reason != "Ready" {
		t.Errorf("expected Ready condition reason %q after the adoption pass, got %q (message: %q)", "Ready", readyCondition.Reason, readyCondition.Message)
	}

	// Verify that the extra warm sandbox was NOT adopted (it should still have its warm pool labels)
	var extra sandboxv1beta1.Sandbox
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-extra", Namespace: "default"}, &extra); err != nil {
		t.Fatalf("failed to get extra warm sandbox: %v", err)
	}
	if _, ok := extra.Labels[warmPoolSandboxLabel]; !ok {
		t.Error("expected extra warm sandbox to still have warm pool label, meaning it was not incorrectly adopted during cache lag")
	}

	// Run reconcile AGAIN while the cache is STILL stale. The pass re-sends the
	// idempotent adoption patch and must neither error, nor wipe the finalized
	// status, nor adopt the extra warm sandbox.
	res, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Expected stale-cache Reconcile to succeed, but failed: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("Expected no requeue on the stale-cache pass, got %+v", res)
	}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if updatedClaim.Status.SandboxStatus.Name != "adopted-sb" {
		t.Errorf("expected claim status to remain 'adopted-sb' during cache lag, got %q", updatedClaim.Status.SandboxStatus.Name)
	}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-extra", Namespace: "default"}, &extra); err != nil {
		t.Fatalf("failed to get extra warm sandbox: %v", err)
	}
	if _, ok := extra.Labels[warmPoolSandboxLabel]; !ok {
		t.Error("expected extra warm sandbox to still have warm pool label during cache lag (should not have been adopted)")
	}

	// Cache converges: the next pass takes the fast path (status lookup,
	// IsControlledBy) and is stable.
	cacheStale = false
	res, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Expected post-convergence Reconcile to succeed, but failed: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("Expected no requeue after convergence, got %+v", res)
	}

	// Verify that the claim status is unchanged.
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if updatedClaim.Status.SandboxStatus.Name != "adopted-sb" {
		t.Errorf("expected claim status to remain 'adopted-sb' after convergence, got %q", updatedClaim.Status.SandboxStatus.Name)
	}

	var adopted sandboxv1beta1.Sandbox
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "adopted-sb", Namespace: "default"}, &adopted); err != nil {
		t.Fatalf("failed to get adopted sandbox: %v", err)
	}
	if val := adopted.Labels[sandboxv1beta1.SandboxLaunchTypeLabel]; val != sandboxv1beta1.SandboxLaunchTypeWarm {
		t.Errorf("expected assigned adopted sandbox to have launch type label %q, got %q; labels=%v", sandboxv1beta1.SandboxLaunchTypeWarm, val, adopted.Labels)
	}

	// Verify that the extra warm sandbox was STILL NOT adopted (it should still have its warm pool labels)
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-extra", Namespace: "default"}, &extra); err != nil {
		t.Fatalf("failed to get extra warm sandbox: %v", err)
	}
	if _, ok := extra.Labels[warmPoolSandboxLabel]; !ok {
		t.Error("expected extra warm sandbox to still have warm pool label after convergence (should not have been adopted)")
	}
}

// TestSandboxClaimAdoptionCacheLagRepatchesIdempotently verifies that while the informer
// cache keeps returning the stale (warm-pool-owned) view of an already-adopted sandbox,
// every pass still finalizes the claim without error and without a polling requeue
// (convergence is watch-driven via the Owns() Sandbox watch), and the finalized status is
// never wiped. A stale pass costs at most one doomed re-patch (rejected by the optimistic
// lock) that is then resolved from an authoritative read — an accepted trade-off of
// finalizing in-pass without per-claim in-memory dedup state.
func TestSandboxClaimAdoptionCacheLagRepatchesIdempotently(t *testing.T) {
	scheme := newScheme(t)

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "adopted-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "warmpool-uid-123"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	adoptedSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "adopted-sb",
			Namespace:  "default",
			UID:        "adopted-sb-uid",
			Generation: 1,
			Labels: map[string]string{
				extensionsv1beta1.SandboxIDLabel: "claim-uid-123",
				sandboxTemplateRefHash:           sandboxcontrollers.NameHash("test-template"),
				warmPoolSandboxLabel:             sandboxcontrollers.NameHash("test-pool"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					extensionsv1beta1.SandboxIDLabel: "claim-uid-123",
				},
			},
		}},
		},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "Ready",
			}},
		},
	}

	// Frozen warm-pool-owned view: served on every Get to simulate an informer
	// cache that has not converged yet, no matter what was patched.
	adoptedSandbox.ResourceVersion = "100"
	staleSandbox := adoptedSandbox.DeepCopy()

	sandboxPatches := 0
	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adoptedSandbox).
		WithStatusSubresource(claim).
		Build()
	fakeClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "adopted-sb" {
				staleSandbox.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
				sandboxPatches++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}

	// Pass 1: adoption patches the sandbox and finalizes the claim in the same pass.
	res, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("pass 1: expected nil error, got: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("pass 1: expected no requeue, got %+v", res)
	}
	patchesAfterLastPass := sandboxPatches
	if patchesAfterLastPass == 0 {
		t.Fatal("pass 1: expected the adoption patch to be sent")
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
		t.Fatalf("pass 1: failed to get claim: %v", err)
	}
	if updatedClaim.Status.SandboxStatus.Name != "adopted-sb" {
		t.Fatalf("pass 1: expected status to be finalized with 'adopted-sb', got %q", updatedClaim.Status.SandboxStatus.Name)
	}

	// Passes 2 and 3: cache still stale — each pass sends at most one doomed adoption
	// re-patch (rejected by the optimistic lock and resolved from the authoritative
	// read), returns without error, and leaves the finalized status intact.
	for pass := 2; pass <= 3; pass++ {
		res, err = reconciler.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("pass %d: expected nil error, got: %v", pass, err)
		}
		if !res.IsZero() {
			t.Fatalf("pass %d: expected no requeue, got %+v", pass, res)
		}
		if sandboxPatches <= patchesAfterLastPass {
			t.Fatalf("pass %d: expected the idempotent adoption re-patch while cache lags, got %d (was %d)", pass, sandboxPatches, patchesAfterLastPass)
		}
		patchesAfterLastPass = sandboxPatches
		if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
			t.Fatalf("pass %d: failed to get claim: %v", pass, err)
		}
		if updatedClaim.Status.SandboxStatus.Name != "adopted-sb" {
			t.Errorf("pass %d: expected finalized status to be preserved during cache lag, got %q", pass, updatedClaim.Status.SandboxStatus.Name)
		}
	}
}

// TestSandboxClaimAdoptionCacheLagPreservesFinalizedStatus verifies that a claim whose
// status was already finalized with a sandbox (e.g. a controller restart racing a stale
// informer) does NOT have its SandboxStatus.Name/PodIPs wiped or its Ready condition
// downgraded by a pass that reads the stale (warm-pool-owned) view: the pass re-sends
// the idempotent adoption patch and re-finalizes from the authoritative object.
func TestSandboxClaimAdoptionCacheLagPreservesFinalizedStatus(t *testing.T) {
	scheme := newScheme(t)

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "adopted-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
		// Status already finalized on a previous pass.
		Status: extensionsv1beta1.SandboxClaimStatus{
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "Sandbox is ready",
				LastTransitionTime: metav1.Now(),
			}},
			SandboxStatus: extensionsv1beta1.SandboxStatus{
				Name:        "adopted-sb",
				PodIPs:      []string{"10.1.2.3"},
				ServiceFQDN: "adopted-sb.default.svc.cluster.local",
			},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "warmpool-uid-123"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	adoptedSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adopted-sb",
			Namespace: "default",
			UID:       "adopted-sb-uid",
			Labels: map[string]string{
				extensionsv1beta1.SandboxIDLabel: "claim-uid-123",
				sandboxTemplateRefHash:           sandboxcontrollers.NameHash("test-template"),
				warmPoolSandboxLabel:             sandboxcontrollers.NameHash("test-pool"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					extensionsv1beta1.SandboxIDLabel: "claim-uid-123",
				},
			},
		}},
		},
		// The sandbox is live and Ready; informer lag on the ownership patch does not
		// erase its status, so the stale view still carries it.
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs:      []string{"10.1.2.3"},
			ServiceFQDN: "adopted-sb.default.svc.cluster.local",
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "Sandbox is ready",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}

	// Frozen warm-pool-owned view: served on every Get to simulate an informer
	// cache that has not converged yet, no matter what was patched.
	adoptedSandbox.ResourceVersion = "100"
	staleSandbox := adoptedSandbox.DeepCopy()

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adoptedSandbox).
		WithStatusSubresource(claim).
		Build()
	fakeClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "adopted-sb" {
				staleSandbox.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	// Fresh reconciler, as after a controller restart.
	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}

	res, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected nil error during cache lag, got: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("expected no requeue, got %+v", res)
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if updatedClaim.Status.SandboxStatus.Name != "adopted-sb" {
		t.Errorf("expected finalized SandboxStatus.Name to be preserved, got %q", updatedClaim.Status.SandboxStatus.Name)
	}
	if len(updatedClaim.Status.SandboxStatus.PodIPs) != 1 || updatedClaim.Status.SandboxStatus.PodIPs[0] != "10.1.2.3" {
		t.Errorf("expected finalized PodIPs to be preserved, got %v", updatedClaim.Status.SandboxStatus.PodIPs)
	}
	if updatedClaim.Status.SandboxStatus.ServiceFQDN != "adopted-sb.default.svc.cluster.local" {
		t.Errorf("expected finalized ServiceFQDN to be preserved, got %q", updatedClaim.Status.SandboxStatus.ServiceFQDN)
	}
	readyCondition := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if readyCondition == nil {
		t.Fatal("expected Ready condition to still be present")
	}
	if readyCondition.Status != metav1.ConditionTrue || readyCondition.Reason != "Ready" {
		t.Errorf("expected Ready condition to be preserved (True/Ready), got %s/%s", readyCondition.Status, readyCondition.Reason)
	}
}

// TestSandboxClaimFreshAdoptionStaleCacheKeepsFinalizedStatus verifies that after the
// primary warm-adoption entry point (adoptSandboxFromCandidates) finalizes status on the
// adoption pass, later passes that still read the stale (warm-pool-owned) view stay
// error-free, need no polling requeue, and never wipe the finalized status — the stale
// pass just re-sends the idempotent adoption patch.
func TestSandboxClaimFreshAdoptionStaleCacheKeepsFinalizedStatus(t *testing.T) {
	scheme := newScheme(t)

	// No assigned-sandbox annotation: adoption goes through the candidate queue.
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "warmpool-uid-123"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	warmSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-sb",
			Namespace: "default",
			UID:       "warm-sb-uid",
			Labels: map[string]string{
				warmPoolSandboxLabel:   sandboxcontrollers.NameHash("test-pool"),
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
		},
	}

	// Frozen warm-pool-owned view: served on every Get to simulate an informer
	// cache that never converges within the test, no matter what was patched.
	warmSandbox.ResourceVersion = "100"
	staleSandbox := warmSandbox.DeepCopy()

	sandboxPatches := 0
	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, warmSandbox).
		WithStatusSubresource(claim).
		Build()
	fakeClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "warm-sb" {
				staleSandbox.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
				sandboxPatches++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	warmSandboxQueue.Add(
		queue.GetNamespacedWarmPoolName("default", "test-pool"),
		queue.SandboxKey{Namespace: "default", Name: "warm-sb"},
	)

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: warmSandboxQueue,
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}

	// Pass 1: fresh adoption through adoptSandboxFromCandidates; status is finalized.
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("pass 1: expected nil error, got: %v", err)
	}
	patchesAfterFirstPass := sandboxPatches
	if patchesAfterFirstPass == 0 {
		t.Fatal("pass 1: expected the adoption patch to be sent")
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if updatedClaim.Status.SandboxStatus.Name != "warm-sb" {
		t.Fatalf("pass 1: expected status to be finalized with 'warm-sb', got %q", updatedClaim.Status.SandboxStatus.Name)
	}

	// Passes 2 and 3: cache still shows the warm-pool owner — each pass re-finalizes
	// from the freshly re-patched object without error, without a polling requeue,
	// and without wiping the status finalized on pass 1.
	for pass := 2; pass <= 3; pass++ {
		res, err := reconciler.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("pass %d: expected nil error, got: %v", pass, err)
		}
		if !res.IsZero() {
			t.Fatalf("pass %d: expected no requeue, got %+v", pass, res)
		}
		if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, updatedClaim); err != nil {
			t.Fatalf("pass %d: failed to get claim: %v", pass, err)
		}
		if updatedClaim.Status.SandboxStatus.Name != "warm-sb" {
			t.Errorf("pass %d: expected finalized status to be preserved during cache lag, got %q", pass, updatedClaim.Status.SandboxStatus.Name)
		}
	}
}

func TestSandboxClaimPreventsAdoptionFromWrongWarmPool(t *testing.T) {
	scheme := newScheme(t)

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Labels: map[string]string{
				"agents.x-k8s.io/sandbox-name": "wrong-pool-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "correct-pool"},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "correct-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "correct-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "correct-template"}},
	}

	wrongPoolSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-pool-sb",
			Namespace: "default",
			UID:       "wrong-sb-uid",
			Labels: map[string]string{
				warmPoolSandboxLabel:   sandboxcontrollers.NameHash("wrong-pool"),
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("correct-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "wrong-pool",
				UID:        "wrong-pool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, wrongPoolSandbox).
		WithStatusSubresource(claim).
		Build()

	warmSandboxQueue := queue.NewSimpleSandboxQueue()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: warmSandboxQueue,
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Expected reconcile to succeed (fall through and create new sandbox), but failed: %v", err)
	}

	var sb sandboxv1beta1.Sandbox
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "wrong-pool-sb", Namespace: "default"}, &sb); err != nil {
		t.Fatalf("failed to get sandbox: %v", err)
	}
	if _, ok := sb.Labels[warmPoolSandboxLabel]; !ok {
		t.Error("expected wrong pool sandbox to still have warm pool label, meaning it was not incorrectly adopted")
	}

	var newSb sandboxv1beta1.Sandbox
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, &newSb); err != nil {
		t.Fatalf("expected a new sandbox to be created with claim name, but got error: %v", err)
	}

	if newSb.UID == "wrong-sb-uid" {
		t.Error("expected created sandbox to be a new one, not the wrong one from label")
	}
}

func TestSandboxClaimRecoveryWhenTemplateCreated(t *testing.T) {
	scheme := newScheme(t)
	claimName := "recovery-claim"
	templateName := "recovery-template"
	warmPoolName := "recovery-warmpool"

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			UID:       "claim-uid",
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPoolName},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: "default",
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: warmPoolName, Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: templateName}},
	}

	// Step 1: Reconcile without template
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, warmPool).
		WithStatusSubresource(claim).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}

	// Should return no error but RequeueAfter because template is missing
	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error when template is missing, but got %v", err)
	}
	if result.RequeueAfter != 1*time.Minute {
		t.Errorf("expected RequeueAfter to be 1 minute, got %v", result.RequeueAfter)
	}

	// Verify status is set to TemplateNotFound
	var updatedClaim extensionsv1beta1.SandboxClaim
	if err := fakeClient.Get(context.Background(), req.NamespacedName, &updatedClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	cond := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if cond == nil || cond.Reason != "TemplateNotFound" {
		t.Errorf("expected status reason 'TemplateNotFound', got %v", cond)
	}

	// Step 2: Create template and reconcile again
	if err := fakeClient.Create(context.Background(), template); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error when template exists, but got %v", err)
	}

	// Verify sandbox is created
	var sandbox sandboxv1beta1.Sandbox
	if err := fakeClient.Get(context.Background(), req.NamespacedName, &sandbox); err != nil {
		t.Fatalf("expected sandbox to be created, but got error: %v", err)
	}
}

func TestMapWarmPoolToClaims(t *testing.T) {
	scheme := newScheme(t)
	warmPoolName := "test-warmpool"

	claim1 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPoolName}},
	}
	claim2 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-2", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPoolName}},
	}
	claimOther := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-other", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "other-warmpool"}},
	}
	// Bound claim: already has a sandbox recorded in status, so pool events must
	// not re-enqueue it (its reconciles are driven by the claim/sandbox watches).
	claimBound := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-bound", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPoolName}},
		Status: extensionsv1beta1.SandboxClaimStatus{
			SandboxStatus: extensionsv1beta1.SandboxStatus{Name: "adopted-sandbox"},
		},
	}
	// Deleting claim: Reconcile returns immediately for it, so it is skipped too.
	claimDeleting := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "claim-deleting",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
			Finalizers:        []string{"test-finalizer"},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPoolName}},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: warmPoolName, Namespace: "default"},
	}

	// We need to manually set up the indexer on the fake client's indexer if it supports it,
	// or we can mock the List behavior. Fake client from controller-runtime does NOT use indexers by default
	// unless configured with WithIndex.

	// Let's use the WithIndex option on the fake client builder to support the matchingFields query!
	fakeClientWithIndex := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim1, claim2, claimOther, claimBound, claimDeleting, warmPool).
		WithIndex(&extensionsv1beta1.SandboxClaim{}, extensionsv1beta1.WarmPoolRefField, func(obj client.Object) []string {
			c := obj.(*extensionsv1beta1.SandboxClaim)
			if c.Spec.WarmPoolRef.Name == "" {
				return nil
			}
			return []string{c.Spec.WarmPoolRef.Name}
		}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client: fakeClientWithIndex,
		Scheme: scheme,
	}

	requests := reconciler.mapWarmPoolToClaims(context.Background(), warmPool)

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests (unbound claims only), got %d: %v", len(requests), requests)
	}

	expectedNames := map[string]bool{"claim-1": true, "claim-2": true}
	for _, req := range requests {
		if !expectedNames[req.Name] {
			t.Errorf("unexpected claim name in requests: %s", req.Name)
		}
		if req.Namespace != "default" {
			t.Errorf("expected namespace 'default', got %s", req.Namespace)
		}
	}
}

// TestWarmPoolMapWatchPredicate pins the event classes the pool->claims map watch
// reacts to: status-only pool updates (generation unchanged) must be filtered out,
// while spec changes (generation bump) still pass so unbound claims wake up.
func TestWarmPoolMapWatchPredicate(t *testing.T) {
	pred := predicate.GenerationChangedPredicate{}

	oldPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-warmpool",
			Namespace:       "default",
			Generation:      1,
			ResourceVersion: "100",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(5))},
	}

	// Status-only update: resourceVersion moves, generation does not.
	statusOnlyPool := oldPool.DeepCopy()
	statusOnlyPool.ResourceVersion = "101"
	statusOnlyPool.Status.ReadyReplicas = 3

	if pred.Update(event.UpdateEvent{ObjectOld: oldPool, ObjectNew: statusOnlyPool}) {
		t.Errorf("expected status-only pool update (generation unchanged) to be filtered out")
	}

	// Spec update: the API server bumps metadata.generation.
	specChangedPool := oldPool.DeepCopy()
	specChangedPool.ResourceVersion = "102"
	specChangedPool.Generation = 2
	specChangedPool.Spec.Replicas = new(int32(10))

	if !pred.Update(event.UpdateEvent{ObjectOld: oldPool, ObjectNew: specChangedPool}) {
		t.Errorf("expected spec (generation) pool update to pass the predicate")
	}
}

func TestMapSandboxTemplateToClaims(t *testing.T) {
	scheme := newScheme(t)
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "target-template", Namespace: "default"},
	}
	warmPool1 := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	warmPool2 := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-2", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	otherTemplatePool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "other-template-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "other-template"}},
	}
	otherNamespacePool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "other-namespace-pool", Namespace: "other"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}

	claim1 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool1.Name}},
	}
	claim2 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-2", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool2.Name}},
	}
	boundClaim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "bound-claim", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool1.Name},
			AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{"example.com/reconcile": "required"},
			},
		},
		Status: extensionsv1beta1.SandboxClaimStatus{
			SandboxStatus: extensionsv1beta1.SandboxStatus{Name: "bound-sandbox"},
		},
	}
	deletingClaim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleting-claim",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
			Finalizers:        []string{"test-finalizer"},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool1.Name}},
	}
	otherTemplateClaim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "other-template-claim", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: otherTemplatePool.Name}},
	}
	otherNamespaceClaim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "other-namespace-claim", Namespace: "other"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: otherNamespacePool.Name}},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(warmPool1, warmPool2, otherTemplatePool, otherNamespacePool, claim1, claim2, boundClaim, deletingClaim, otherTemplateClaim, otherNamespaceClaim).
		WithIndex(&extensionsv1beta1.SandboxWarmPool{}, extensionsv1beta1.TemplateRefField, sandboxTemplateRefNameIndexer).
		WithIndex(&extensionsv1beta1.SandboxClaim{}, extensionsv1beta1.WarmPoolRefField, func(obj client.Object) []string {
			claim := obj.(*extensionsv1beta1.SandboxClaim)
			if claim.Spec.WarmPoolRef.Name == "" {
				return nil
			}
			return []string{claim.Spec.WarmPoolRef.Name}
		}).
		Build()

	reconciler := &SandboxClaimReconciler{Client: fakeClient, Scheme: scheme}
	requests := reconciler.mapSandboxTemplateToClaims(context.Background(), template)

	require.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: claim1.Name, Namespace: claim1.Namespace}},
		{NamespacedName: types.NamespacedName{Name: claim2.Name, Namespace: claim2.Namespace}},
		{NamespacedName: types.NamespacedName{Name: boundClaim.Name, Namespace: boundClaim.Namespace}},
	}, requests)
}

func TestSandboxTemplateCreatePredicate(t *testing.T) {
	pred := sandboxTemplateCreatePredicate()
	template := &extensionsv1beta1.SandboxTemplate{}

	require.True(t, pred.Create(event.CreateEvent{Object: template}))
	require.False(t, pred.Update(event.UpdateEvent{ObjectOld: template, ObjectNew: template.DeepCopy()}))
	require.False(t, pred.Delete(event.DeleteEvent{Object: template}))
	require.False(t, pred.Generic(event.GenericEvent{Object: template}))
}

func TestSandboxClaimLegacyLabelMigration(t *testing.T) {
	scheme := newScheme(t)

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim-legacy",
			Namespace: "default",
			UID:       "claim-uid-legacy",
			Labels: map[string]string{
				extensionsv1beta1.DeprecatedAssignedSandboxNameLabel: "adopted-sb-legacy",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
		},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	adoptedSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adopted-sb-legacy",
			Namespace: "default",
			UID:       "adopted-sb-legacy-uid",
			Labels: map[string]string{
				extensionsv1beta1.SandboxIDLabel: "claim-uid-legacy",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxClaimKind,
				Name:       "test-claim-legacy",
				UID:        "claim-uid-legacy",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					extensionsv1beta1.SandboxIDLabel: "claim-uid-legacy",
				},
			},
		}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adoptedSandbox).
		WithStatusSubresource(claim).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim-legacy", Namespace: "default"}}

	// Run reconcile
	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Expected reconcile to succeed, but got error: %v", err)
	}

	// Verify that the claim was migrated: DeprecatedAssignedSandboxNameLabel removed, AssignedSandboxNameAnnotation added
	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}

	require.NotContains(t, updatedClaim.Labels, extensionsv1beta1.DeprecatedAssignedSandboxNameLabel)
	require.Equal(t, "adopted-sb-legacy", updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
}

func TestIsAdoptable_RejectsUnowned(t *testing.T) {
	// 1. Create a warm pool template hash
	poolNameHash := sandboxcontrollers.NameHash("test-pool")
	templateHash := sandboxcontrollers.NameHash("test-template")

	// 2. Mock an unowned Sandbox (no OwnerReferences)
	unownedSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unowned-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: templateHash,
			},
		},
		Status: sandboxv1beta1.SandboxStatus{PodIPs: []string{testNetworkedPodIP}},
	}

	// 3. Verify it is rejected
	err := isAdoptable(unownedSandbox)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unowned")

	// 4. Mock an owned Sandbox (pointing to SandboxWarmPool)
	ownedSandbox := unownedSandbox.DeepCopy()
	ownedSandbox.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       extensionsv1beta1.SandboxWarmPoolKind,
			Name:       "test-pool",
			UID:        "pool-uid-123",
			Controller: ptr.To(true), // nolint:modernize
		},
	}

	// 5. Verify it is accepted
	err = isAdoptable(ownedSandbox)
	require.NoError(t, err)

	// 5b. Mock a warm Sandbox created by a pre-v1beta1 pool controller: the
	// owner reference apiVersion stays v1alpha1 across an in-place upgrade
	// because storage migration never rewrites owner references.
	legacyOwnedSandbox := unownedSandbox.DeepCopy()
	legacyOwnedSandbox.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
			Kind:       extensionsv1beta1.SandboxWarmPoolKind,
			Name:       "test-pool",
			UID:        "pool-uid-123",
			Controller: ptr.To(true), // nolint:modernize
		},
	}

	// 5c. Verify it is still adoptable (version-agnostic group+kind match)
	err = isAdoptable(legacyOwnedSandbox)
	require.NoError(t, err)

	// 5d. A controller from a different group must still be rejected
	foreignGroupSandbox := unownedSandbox.DeepCopy()
	foreignGroupSandbox.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "apps/v1",
			Kind:       extensionsv1beta1.SandboxWarmPoolKind,
			Name:       "test-pool",
			UID:        "pool-uid-123",
			Controller: ptr.To(true), // nolint:modernize
		},
	}
	err = isAdoptable(foreignGroupSandbox)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not managed by warm pool")

	// 6. Mock an owned Sandbox pointing to a different kind (e.g. SandboxClaim, which is NOT WarmPool)
	ownedByClaimSandbox := unownedSandbox.DeepCopy()
	ownedByClaimSandbox.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       extensionsv1beta1.SandboxClaimKind,
			Name:       "test-claim",
			UID:        "claim-uid-123",
			Controller: ptr.To(true), // nolint:modernize
		},
	}

	// 7. Verify it is rejected
	err = isAdoptable(ownedByClaimSandbox)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not managed by warm pool")
}

func TestSandboxClaimAdoptionStrategy(t *testing.T) {
	scheme := newScheme(t)

	createWarmPoolSandboxWithNode := func(name string, creationTime metav1.Time, ready bool, nodeName string) *sandboxv1beta1.Sandbox {
		conditionStatus := metav1.ConditionFalse
		if ready {
			conditionStatus = metav1.ConditionTrue
		}
		poolNameHash := sandboxcontrollers.NameHash("test-pool")
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				Generation:        1,
				CreationTimestamp: creationTime,
				Labels: map[string]string{
					warmPoolSandboxLabel:   poolNameHash,
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: extensionsv1beta1.GroupVersion.String(),
						Kind:       extensionsv1beta1.SandboxWarmPoolKind,
						Name:       "test-pool",
						UID:        "warmpool-uid",
						Controller: ptr.To(true), // nolint:modernize
					},
				},
			},
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
				},
			}},
			},
			Status: sandboxv1beta1.SandboxStatus{
				NodeName: nodeName,
				PodIPs:   []string{testNetworkedPodIP},
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1beta1.SandboxConditionReady),
						Status:             conditionStatus,
						ObservedGeneration: 1,
						Reason:             "DependenciesReady",
					},
				},
			},
		}
	}

	createClaim := func(name string) *extensionsv1beta1.SandboxClaim {
		return &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name + "-uid"),
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{
				WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{
					Name: "test-pool",
				},
			},
		}
	}

	testCases := []struct {
		name                   string
		existingSandboxes      []*sandboxv1beta1.Sandbox
		otherObjects           []client.Object
		expectedAdoptedSandbox string
		expectColdStart        bool
		expectedRemainingKeys  []string
	}{
		{
			name: "picks oldest ready sandbox (FIFO queue order)",
			existingSandboxes: []*sandboxv1beta1.Sandbox{
				createWarmPoolSandboxWithNode("sb-old-ready", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true, "node-2"),
				createWarmPoolSandboxWithNode("sb-young-ready", metav1.Now(), true, "node-1"),
				createWarmPoolSandboxWithNode("sb-old-not-ready", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, false, "node-3"),
			},
			expectedAdoptedSandbox: "sb-old-ready",
			expectedRemainingKeys:  []string{"sb-young-ready", "sb-old-not-ready"},
		},
		{
			name: "skips unready sandbox to adopt younger ready sandbox",
			existingSandboxes: []*sandboxv1beta1.Sandbox{
				createWarmPoolSandboxWithNode("sb-old-unready", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, false, "node-3"),
				createWarmPoolSandboxWithNode("sb-young-ready", metav1.Now(), true, "node-1"),
			},
			expectedAdoptedSandbox: "sb-young-ready",
			expectedRemainingKeys:  []string{"sb-old-unready"},
		},
		{
			name: "cold starts and preserves queue when all sandboxes are unready",
			existingSandboxes: []*sandboxv1beta1.Sandbox{
				createWarmPoolSandboxWithNode("sb-old-unready", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, false, "node-2"),
				createWarmPoolSandboxWithNode("sb-young-unready", metav1.Now(), false, "node-1"),
			},
			expectColdStart:       true,
			expectedRemainingKeys: []string{"sb-old-unready", "sb-young-unready"},
		},
		{
			name: "picks sandbox on the node with most remaining warmpool sandboxes (NodeSpread balancing)",
			existingSandboxes: []*sandboxv1beta1.Sandbox{
				createWarmPoolSandboxWithNode("sb-node1-oldest", metav1.Time{Time: metav1.Now().Add(-2 * time.Hour)}, true, "node-1"),
				createWarmPoolSandboxWithNode("sb-node2-younger-1", metav1.Time{Time: metav1.Now().Add(-1 * time.Hour)}, true, "node-2"),
				createWarmPoolSandboxWithNode("sb-node2-younger-2", metav1.Now(), true, "node-2"),
			},
			expectedAdoptedSandbox: "sb-node2-younger-1",
			expectedRemainingKeys:  []string{"sb-node1-oldest", "sb-node2-younger-2"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			template := &extensionsv1beta1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
				Spec:       extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}},
			}

			claim := createClaim("test-claim")

			warmPool := &extensionsv1beta1.SandboxWarmPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
					UID:       "warmpool-uid",
				},
				Spec: extensionsv1beta1.SandboxWarmPoolSpec{
					TemplateRef: extensionsv1beta1.SandboxTemplateRef{
						Name: "test-template",
					},
				},
			}

			var allObjects []client.Object
			allObjects = append(allObjects, template, claim, warmPool)
			allObjects = append(allObjects, tc.otherObjects...)
			for _, sb := range tc.existingSandboxes {
				allObjects = append(allObjects, sb)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(allObjects...).
				WithStatusSubresource(claim).
				Build()

			warmSandboxQueue := queue.NewSimpleSandboxQueue()
			for _, sb := range tc.existingSandboxes {
				key := queue.SandboxKey{Namespace: sb.Namespace, Name: sb.Name, NodeName: sb.Status.NodeName}
				namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(sb.Namespace, "test-pool")
				warmSandboxQueue.Add(namespacedWarmPoolName, key)
			}

			reconciler := &SandboxClaimReconciler{
				Client:           fakeClient,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				WarmSandboxQueue: warmSandboxQueue,
				Tracer:           asmetrics.NewNoOp(),
			}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
			_, err := reconciler.Reconcile(context.Background(), req)
			require.NoError(t, err)

			if tc.expectColdStart {
				var coldSandbox sandboxv1beta1.Sandbox
				err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: claim.Name}, &coldSandbox)
				require.NoError(t, err)
				require.Equal(t, sandboxv1beta1.SandboxLaunchTypeCold, coldSandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])
			} else {
				var adoptedSandbox sandboxv1beta1.Sandbox
				err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: tc.expectedAdoptedSandbox}, &adoptedSandbox)
				require.NoError(t, err)

				controllerRef := metav1.GetControllerOf(&adoptedSandbox)
				require.NotNil(t, controllerRef)
				require.Equal(t, claim.UID, controllerRef.UID)
			}

			// Verify that the expected remaining sandbox keys are still queued properly (regression test)
			var actualRemaining []string
			namespacedWarmPoolName := queue.GetNamespacedWarmPoolName("default", "test-pool")
			for {
				key, ok := warmSandboxQueue.Get(namespacedWarmPoolName)
				if !ok {
					break
				}
				actualRemaining = append(actualRemaining, key.Name)
			}
			require.Equal(t, tc.expectedRemainingKeys, actualRemaining)
		})
	}
}

func TestCreateSandboxClaimVolumeClaimTemplatesSuccess(t *testing.T) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "vct-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		},
			VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "vct-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "vct-template"}},
	}

	testCases := []struct {
		name                   string
		claimVCTs              []sandboxv1beta1.PersistentVolumeClaimTemplate
		policy                 extensionsv1beta1.VolumeClaimTemplatesPolicy
		expectedVCTs           []string
		expectedStorage        string
		expectColdStart        bool
		setupWarmPoolSandbox   bool
		expectSandboxAdoption  bool
		expectedAdoptedSandbox string
	}{
		{
			name: "policy=Overrides overrides template volume",
			claimVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("5Gi"),
							},
						},
					},
				},
			},
			policy:          extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides,
			expectedVCTs:    []string{"data"},
			expectedStorage: "5Gi",
			expectColdStart: true,
		},
		{
			name: "policy=Allowed allows adding new custom volumes",
			claimVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "custom"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("2Gi"),
							},
						},
					},
				},
			},
			policy:          extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			expectedVCTs:    []string{"data", "custom"},
			expectedStorage: "2Gi", // for custom volume
			expectColdStart: true,
		},
		{
			name:                   "bypasses VCT policy check if claim requests no custom volumes",
			claimVCTs:              nil,
			policy:                 extensionsv1beta1.VolumeClaimTemplatesPolicyDisallowed,
			expectedVCTs:           []string{"data"},
			expectedStorage:        "1Gi",
			expectColdStart:        false,
			setupWarmPoolSandbox:   true,
			expectSandboxAdoption:  true,
			expectedAdoptedSandbox: "warm-sandbox",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			claimName := "vct-claim-" + tc.name

			claim := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: types.UID(claimName)},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef:          extensionsv1beta1.SandboxWarmPoolRef{Name: "vct-warmpool"},
					VolumeClaimTemplates: tc.claimVCTs,
				},
			}

			// Copy of template with VCT policy set
			templateCopy := template.DeepCopy()
			templateCopy.Spec.VolumeClaimTemplatesPolicy = tc.policy

			var existingObjects []client.Object
			existingObjects = append(existingObjects, claim, templateCopy, warmPool)

			var readyWarmSandbox *sandboxv1beta1.Sandbox
			warmSandboxQueue := queue.NewSimpleSandboxQueue()
			if tc.setupWarmPoolSandbox {
				poolNameHash := sandboxcontrollers.NameHash("vct-warmpool")
				readyWarmSandbox = &sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warm-sandbox",
						Namespace: "default",
						Labels: map[string]string{
							warmPoolSandboxLabel:   poolNameHash,
							sandboxTemplateRefHash: sandboxcontrollers.NameHash("vct-template"),
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxWarmPoolKind,
							Name:       "vct-warmpool",
							UID:        "pool-uid-123",
							Controller: ptr.To(true), // nolint:modernize
						}},
					},
					Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "test"}}},
					}},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{{
							Type:   string(sandboxv1beta1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						}},
						PodIPs: []string{testNetworkedPodIP},
					},
				}
				existingObjects = append(existingObjects, readyWarmSandbox)
				warmSandboxQueue.Add(queue.GetNamespacedWarmPoolName("default", "vct-warmpool"), queue.SandboxKey{Namespace: "default", Name: "warm-sandbox"})
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(existingObjects...).
				WithStatusSubresource(claim).
				Build()

			reconciler := &SandboxClaimReconciler{
				Client:           fakeClient,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				Tracer:           asmetrics.NewNoOp(),
				WarmSandboxQueue: warmSandboxQueue,
			}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}
			_, err := reconciler.Reconcile(context.Background(), req)
			require.NoError(t, err)

			if tc.expectSandboxAdoption {
				var adopted sandboxv1beta1.Sandbox
				require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: tc.expectedAdoptedSandbox, Namespace: "default"}, &adopted))
				controllerRef := metav1.GetControllerOf(&adopted)
				require.NotNil(t, controllerRef)
				require.Equal(t, claim.UID, controllerRef.UID)

				// Verify claim's AssignedSandboxName annotation
				var updatedClaim extensionsv1beta1.SandboxClaim
				require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &updatedClaim))
				require.Equal(t, tc.expectedAdoptedSandbox, updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
				return
			}

			// Verify newly created cold-started sandbox with propagated/merged VolumeClaimTemplates
			sandbox := &sandboxv1beta1.Sandbox{}
			err = fakeClient.Get(context.Background(), types.NamespacedName{Name: claimName, Namespace: "default"}, sandbox)
			require.NoError(t, err)

			if tc.expectColdStart {
				require.Equal(t, string(sandboxv1beta1.SandboxLaunchTypeCold), sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel])
			}

			require.Len(t, sandbox.Spec.VolumeClaimTemplates, len(tc.expectedVCTs))
			for i, name := range tc.expectedVCTs {
				require.Equal(t, name, sandbox.Spec.VolumeClaimTemplates[i].Name)
				if name == "data" && tc.policy == extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides {
					actualStorage := sandbox.Spec.VolumeClaimTemplates[i].Spec.Resources.Requests[corev1.ResourceStorage]
					require.True(t, actualStorage.Equal(resource.MustParse(tc.expectedStorage)))
				}
				if name == "custom" {
					actualStorage := sandbox.Spec.VolumeClaimTemplates[i].Spec.Resources.Requests[corev1.ResourceStorage]
					require.True(t, actualStorage.Equal(resource.MustParse(tc.expectedStorage)))
				}
			}
		})
	}
}

func TestCreateSandboxClaimVolumeClaimTemplatesErrors(t *testing.T) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "vct-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "vct-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "vct-template"}},
	}

	testCases := []struct {
		name                 string
		claimVCTs            []sandboxv1beta1.PersistentVolumeClaimTemplate
		templateVCTs         []sandboxv1beta1.PersistentVolumeClaimTemplate
		policy               extensionsv1beta1.VolumeClaimTemplatesPolicy
		expectedError        error
		expectedMessageMatch string
	}{
		{
			name: "disallowed policy",
			claimVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "custom"}},
			},
			policy:               extensionsv1beta1.VolumeClaimTemplatesPolicyDisallowed,
			expectedError:        ErrVolumeClaimTemplatesDisallowed,
			expectedMessageMatch: "volume claim templates are disallowed by the template",
		},
		{
			name: "forbidden override policy",
			claimVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"}},
			},
			templateVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"}},
			},
			policy:               extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			expectedError:        ErrVolumeClaimTemplatesOverrideForbidden,
			expectedMessageMatch: "cannot override template volume \"data\"",
		},
		{
			name: "default empty policy is treated as Disallowed",
			claimVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "custom"}},
			},
			policy:               "",
			expectedError:        ErrVolumeClaimTemplatesDisallowed,
			expectedMessageMatch: "volume claim templates are disallowed by the template",
		},
		{
			name: "empty volume name in claim VCTs",
			claimVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: ""}},
			},
			policy:               extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			expectedError:        ErrVolumeClaimTemplatesInvalid,
			expectedMessageMatch: "name at index 0 is empty",
		},
		{
			name: "duplicate volume name in claim VCTs",
			claimVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"}},
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"}},
			},
			policy:               extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			expectedError:        ErrVolumeClaimTemplatesInvalid,
			expectedMessageMatch: "duplicate name \"data\"",
		},
		{
			name:      "empty volume name in template VCTs",
			claimVCTs: nil,
			templateVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: ""}},
			},
			policy:               extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			expectedError:        ErrVolumeClaimTemplatesInvalid,
			expectedMessageMatch: "name at index 0 is empty",
		},
		{
			name:      "duplicate volume name in template VCTs",
			claimVCTs: nil,
			templateVCTs: []sandboxv1beta1.PersistentVolumeClaimTemplate{
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"}},
				{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"}},
			},
			policy:               extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			expectedError:        ErrVolumeClaimTemplatesInvalid,
			expectedMessageMatch: "duplicate name \"data\"",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			claimName := "vct-error-claim-" + tc.name

			claim := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: types.UID(claimName)},
				Spec: extensionsv1beta1.SandboxClaimSpec{
					WarmPoolRef:          extensionsv1beta1.SandboxWarmPoolRef{Name: "vct-warmpool"},
					VolumeClaimTemplates: tc.claimVCTs,
				},
			}

			templateCopy := template.DeepCopy()
			templateCopy.Spec.VolumeClaimTemplates = tc.templateVCTs
			templateCopy.Spec.VolumeClaimTemplatesPolicy = tc.policy

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(claim, templateCopy, warmPool).
				WithStatusSubresource(claim).
				Build()

			reconciler := &SandboxClaimReconciler{
				Client:           fakeClient,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				Tracer:           asmetrics.NewNoOp(),
				WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
			}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}
			_, err := reconciler.Reconcile(context.Background(), req)
			require.NoError(t, err)

			// Verify claim condition reflects the error status
			updatedClaim := &extensionsv1beta1.SandboxClaim{}
			err = fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim)
			require.NoError(t, err)

			cond := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
			require.NotNil(t, cond)
			require.Equal(t, metav1.ConditionFalse, cond.Status)
			require.Equal(t, "VolumeClaimTemplatesError", cond.Reason)
			require.Contains(t, cond.Message, tc.expectedMessageMatch)
		})
	}
}

func TestSandboxClaimReconcile_PatchErrorPreservesStatus(t *testing.T) {
	scheme := newScheme(t)
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}},
		},
	}
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
		Status: extensionsv1beta1.SandboxClaimStatus{
			SandboxStatus: extensionsv1beta1.SandboxStatus{Name: "warm-sandbox"},
		},
	}
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-sandbox", Namespace: "default",
			Labels: map[string]string{
				sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind,
				Name: "test-claim", UID: "claim-uid", Controller: new(true),
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			OperatingMode:    sandboxv1beta1.SandboxOperatingModeRunning,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, sandbox).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					return k8errors.NewInternalError(fmt.Errorf("simulated patch failure"))
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.Error(t, err)

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	err = fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim)
	require.NoError(t, err)
	require.Equal(t, "warm-sandbox", updatedClaim.Status.SandboxStatus.Name, "status.sandbox.name must not be wiped out when metadata patch fails")
}

func TestSandboxClaimReconcile_TransientLookupErrorPreservesStatus(t *testing.T) {
	scheme := newScheme(t)

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}},
		},
	}
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
		Status: extensionsv1beta1.SandboxClaimStatus{
			SandboxStatus: extensionsv1beta1.SandboxStatus{Name: "warm-sandbox"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "warm-sandbox" {
					return k8errors.NewInternalError(fmt.Errorf("simulated transient lookup timeout"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.Error(t, err)

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	err = fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim)
	require.NoError(t, err)
	require.Equal(t, "warm-sandbox", updatedClaim.Status.SandboxStatus.Name, "status.sandbox.name must not be wiped out when sandbox lookup fails with transient error")
}

func TestReconcilePropagatesAnnotationPatchError(t *testing.T) {
	asmetrics.ClaimStartupLatency.Reset()
	asmetrics.ClaimControllerStartupLatency.Reset()

	// Build a not-Ready claim with a Ready owned sandbox.
	// When Reconcile runs, the claim transitions to Ready (because the sandbox is Ready),
	// updateStatus persists Ready=True, and recordCreationLatencyMetric fires.
	// Pre-set ObservabilityAnnotation so initializeAnnotations does not Patch
	// (which would be intercepted by claimPatchFailClient before we reach the
	// annotation stamp in recordCreationLatencyMetric).
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "patch-error-claim",
			Namespace: "default",
			UID:       "uid-patch-error-claim",
			Annotations: map[string]string{
				asmetrics.ObservabilityAnnotation: time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
				asmetrics.WebhookAnnotation:       time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano),
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
		},
	}

	ctrlBool := true
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name,
			Namespace: claim.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxClaim",
				Name:       claim.Name,
				UID:        claim.UID,
				Controller: &ctrlBool,
			}},
		},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type:   string(sandboxv1beta1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
				Reason: "Ready",
			}},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-warmpool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	scheme := newScheme(t)
	inner := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, sandbox, warmPool).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
		Build()

	// Fail the first Patch (annotation stamp), succeed on subsequent Patches (backfill on retry).
	fc := &claimPatchFailClient{Client: inner, err: fmt.Errorf("simulated patch failure"), maxFailures: 1}

	r := &SandboxClaimReconciler{
		Client:           fc,
		Scheme:           scheme,
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
	}

	// Populate observedTimes as the Create predicate would.
	pred := r.getTimingPredicate()
	pred.Create(event.CreateEvent{Object: claim})

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// First Reconcile: claim transitions not-Ready → Ready, metrics are recorded,
	// but the annotation Patch fails. Reconcile must surface the error.
	_, err := r.Reconcile(t.Context(), req)
	require.Error(t, err, "Reconcile should propagate annotation patch error")
	require.Contains(t, err.Error(), "stamp claim first-ready annotation")

	// Metrics were recorded before the failed Patch.
	require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency), "metrics should be recorded before Patch")

	// Second Reconcile (retry): the claim is already Ready (updateStatus persisted it),
	// so the oldReady guard fires. The backfill Patch succeeds (maxFailures exhausted).
	// No duplicate metric recording.
	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err, "retry Reconcile should succeed after backfill")

	require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ClaimStartupLatency), "retry should not double-count")
}

type mockTracer struct {
	asmetrics.Instrumenter
	capturedAttrs map[string]string
}

func (m *mockTracer) StartSpan(ctx context.Context, _ metav1.Object, _ string, attrs map[string]string) (context.Context, func()) {
	if len(attrs) > 0 {
		m.capturedAttrs = attrs
	}
	return ctx, func() {}
}

func (m *mockTracer) GetTraceContext(_ context.Context) string {
	return ""
}

func (m *mockTracer) IsRecording(_ context.Context) bool {
	return true
}

func (m *mockTracer) AddEvent(_ context.Context, _ string, _ map[string]string) {}

func TestReconcile_TracingNormalization(t *testing.T) {
	claimName := "tracing-test-claim"
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			UID:       "uid-claim-1",
			Labels: map[string]string{
				sandboxv1beta1.CreatedByLabel: "invalid-value",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-warmpool"},
		},
	}

	scheme := newScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithStatusSubresource(claim).
		Build()

	mt := &mockTracer{}
	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           mt,
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	_ = err

	require.NotNil(t, mt.capturedAttrs)
	require.Equal(t, "unknown", mt.capturedAttrs[sandboxv1beta1.CreatedByLabel], "created-by label must be normalized in span attributes")
}

func TestSandboxStatusRelevantChange(t *testing.T) {
	tests := []struct {
		name     string
		oldSb    *sandboxv1beta1.Sandbox
		newSb    *sandboxv1beta1.Sandbox
		expected bool
	}{
		{
			name:     "No relevant change",
			oldSb:    &sandboxv1beta1.Sandbox{},
			newSb:    &sandboxv1beta1.Sandbox{},
			expected: false,
		},
		{
			name:  "DeletionTimestamp changed",
			oldSb: &sandboxv1beta1.Sandbox{},
			newSb: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			},
			expected: true,
		},
		{
			name: "PodIPs changed",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					PodIPs: []string{"10.0.0.1"},
				},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					PodIPs: []string{"10.0.0.1", "10.0.0.2"},
				},
			},
			expected: true,
		},
		{
			name: "Ready condition changed",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse},
					},
				},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
		{
			// Expiry has no condition type of its own: hasSandboxExpiredCondition
			// reads the Ready condition's Reason. Here Status stays False and only
			// the Reason flips (SandboxNotReady -> SandboxExpired), so this must be
			// treated as relevant -- a Status-only comparison would silently drop
			// expiry propagation to claims.
			name: "Ready condition Reason changed to Expired, Status unchanged",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "SandboxNotReady"},
					},
				},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: sandboxv1beta1.SandboxReasonExpired},
					},
				},
			},
			expected: true,
		},
		{
			name: "Finished condition changed",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionFinished), Status: metav1.ConditionFalse},
					},
				},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionFinished), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
		{
			name: "ServiceFQDN set",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					ServiceFQDN: "sb.default.svc.cluster.local",
				},
			},
			expected: true,
		},
		{
			name: "ServiceFQDN changed",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					ServiceFQDN: "old-sb.default.svc.cluster.local",
				},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					ServiceFQDN: "new-sb.default.svc.cluster.local",
				},
			},
			expected: true,
		},
		{
			// The Sandbox controller clears serviceFQDN when the Service is
			// deleted; the claim must reconcile to clear its mirror too.
			name: "ServiceFQDN cleared",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					ServiceFQDN: "sb.default.svc.cluster.local",
				},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{},
			},
			expected: true,
		},
		{
			name: "Irrelevant condition changed",
			oldSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionSuspended), Status: metav1.ConditionFalse},
					},
				},
			},
			newSb: &sandboxv1beta1.Sandbox{
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(sandboxv1beta1.SandboxConditionSuspended), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := sandboxStatusRelevantChange(tt.oldSb, tt.newSb)
			require.Equal(t, tt.expected, actual)
		})
	}
}

// newOptimisticLockTestObjects builds a claim already annotated with an
// adopted, claim-owned, Ready sandbox — the shape of the pass that finalizes
// (or re-finalizes) a bound claim's status.
func newOptimisticLockTestObjects() (*extensionsv1beta1.SandboxClaim, *extensionsv1beta1.SandboxTemplate, *extensionsv1beta1.SandboxWarmPool, *sandboxv1beta1.Sandbox) {
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "adopted-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
	}
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "warmpool-uid-123"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}
	adopted := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adopted-sb",
			Namespace: "default",
			UID:       "adopted-sb-uid",
			Labels: map[string]string{
				extensionsv1beta1.SandboxIDLabel: "claim-uid-123",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxClaimKind,
				Name:       "test-claim",
				UID:        "claim-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second)),
			}},
		},
	}
	return claim, template, warmPool, adopted
}

// TestSandboxClaimStatusPatchCarriesOptimisticLock verifies the claim status
// patch embeds the base object's resourceVersion, so a patch computed from a
// stale cache view fails with a 409 instead of committing a stale overwrite.
func TestSandboxClaimStatusPatchCarriesOptimisticLock(t *testing.T) {
	scheme := newScheme(t)
	claim, template, warmPool, adopted := newOptimisticLockTestObjects()

	var statusPatchData []byte
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adopted).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && subResourceName == "status" {
					data, err := patch.Data(obj)
					if err != nil {
						return err
					}
					statusPatchData = data
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("expected reconcile to succeed, got: %v", err)
	}

	if statusPatchData == nil {
		t.Fatal("expected a claim status patch to be issued")
	}
	if !strings.Contains(string(statusPatchData), `"resourceVersion"`) {
		t.Errorf("expected the status patch to carry an optimistic-lock resourceVersion precondition, got: %s", statusPatchData)
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, "adopted-sb", updatedClaim.Status.SandboxStatus.Name)
}

// histogramSampleCount sums the observation counts across all series of a histogram
// collector by gathering it through a dedicated registry. (testutil.CollectAndCount
// counts series, not observations, so it cannot detect duplicate records landing in
// an existing series — and >1 observations per claim is exactly the #940 failure
// mode this suite guards against.)
func histogramSampleCount(t *testing.T, c prometheus.Collector) uint64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("failed to register collector: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	var total uint64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			total += m.GetHistogram().GetSampleCount()
		}
	}
	return total
}

// TestSandboxClaimStaleStatusPatchConflictDroppedWithoutMetrics verifies the
// #940 fix end to end: an authoritative pass records the startup-latency
// histograms EXACTLY once, and a later pass that computed status from a stale
// (pre-Ready) cache view has its optimistic-lock 409 dropped as benign — no
// reconcile error, no requeue storm, no stale status commit, and no second
// histogram observation. The guarded failure mode is >1 observations per
// claim (328 observations for 320 claims measured on post-#1118 main), so the
// assertions pin exact sample counts, not series counts.
func TestSandboxClaimStaleStatusPatchConflictDroppedWithoutMetrics(t *testing.T) {
	scheme := newScheme(t)
	claim, template, warmPool, adopted := newOptimisticLockTestObjects()
	// Annotations normally stamped by the webhook / an earlier controller pass,
	// required for the two startup-latency histograms to record at all.
	claim.Annotations[asmetrics.WebhookAnnotation] = time.Now().Add(-5 * time.Second).Format(time.RFC3339Nano)
	claim.Annotations[asmetrics.ObservabilityAnnotation] = time.Now().Add(-4 * time.Second).Format(time.RFC3339Nano)

	asmetrics.ClaimStartupLatency.Reset()
	asmetrics.ClaimControllerStartupLatency.Reset()

	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "extensions.agents.x-k8s.io", Resource: "sandboxclaims"},
		claim.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	// Pass 1 runs against live state; pass 2 serves a frozen pre-Ready claim
	// view and rejects its doomed status patch with the optimistic-lock 409.
	staleClaimView := false
	var preReadyClaim *extensionsv1beta1.SandboxClaim
	statusPatchAttempts := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adopted).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if cl, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && key.Name == "test-claim" && staleClaimView {
					preReadyClaim.DeepCopyInto(cl)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && subResourceName == "status" {
					statusPatchAttempts++
					if staleClaimView {
						return conflict
					}
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	// Pass 1 (authoritative): the status patch persists and the Ready
	// transition is observed exactly once.
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("pass 1: expected nil error, got: %v", err)
	}
	if statusPatchAttempts != 1 {
		t.Fatalf("pass 1: expected exactly 1 status patch, got %d", statusPatchAttempts)
	}
	if got := histogramSampleCount(t, asmetrics.ClaimStartupLatency); got != 1 {
		t.Fatalf("pass 1: expected exactly 1 ClaimStartupLatency observation, got %d", got)
	}
	if got := histogramSampleCount(t, asmetrics.ClaimControllerStartupLatency); got != 1 {
		t.Fatalf("pass 1: expected exactly 1 ClaimControllerStartupLatency observation, got %d", got)
	}

	// Freeze the stale view: the claim as a lagging cache would serve it —
	// binding annotation present, status not yet converged. A view stale
	// enough to predate the committed Ready status also predates the
	// first-ready annotation stamped in the same pass, so drop it too:
	// otherwise the annotation guard alone would mask what this test pins
	// (the authoritative-write gate on metric recording).
	bound := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, bound))
	preReadyClaim = bound.DeepCopy()
	preReadyClaim.Status = extensionsv1beta1.SandboxClaimStatus{}
	delete(preReadyClaim.Annotations, asmetrics.ClaimFirstReadyAnnotation)
	staleClaimView = true

	// Pass 2 (stale): the recomputed "fresh" Ready transition must be
	// arbitrated away by the server-side optimistic lock.
	res, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("pass 2: expected the stale status conflict to be dropped as benign (nil error), got: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("pass 2: expected no requeue (convergence is watch-driven), got %+v", res)
	}
	if statusPatchAttempts != 2 {
		t.Errorf("pass 2: expected the doomed stale patch to be attempted once (2 attempts total), got %d", statusPatchAttempts)
	}

	// Exactly-once metrics: a count >1 here is the #940 duplicate-observation
	// regression this test exists to catch.
	if got := histogramSampleCount(t, asmetrics.ClaimStartupLatency); got != 1 {
		t.Errorf("expected exactly 1 ClaimStartupLatency observation after the stale pass (>1 = #940 duplicate records), got %d", got)
	}
	if got := histogramSampleCount(t, asmetrics.ClaimControllerStartupLatency); got != 1 {
		t.Errorf("expected exactly 1 ClaimControllerStartupLatency observation after the stale pass (>1 = #940 duplicate records), got %d", got)
	}

	// The stale pass must not have regressed the persisted status. (Unfreeze
	// the stale view so this reads the live object.)
	staleClaimView = false
	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, "adopted-sb", updatedClaim.Status.SandboxStatus.Name, "dropped conflict must not clear the committed binding")
	readyCond := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCond, "dropped conflict must not remove the committed Ready condition")
	require.Equal(t, metav1.ConditionTrue, readyCond.Status, "dropped conflict must not flip the committed Ready condition")
}

// TestSandboxClaimAdoptionConflictRetriedInPass verifies that a 409 on the
// optimistically locked claim update recording a warm-pool adoption is retried
// in the same pass against a fresh read (retry.RetryOnConflict shape): the
// adoption completes without surfacing an error and without burning the
// candidate or deferring to another reconcile pass.
func TestSandboxClaimAdoptionConflictRetriedInPass(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}
	poolNameHash := sandboxcontrollers.NameHash("test-pool")
	candidate := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-sb-1",
			Namespace: "default",
			UID:       "pool-sb-1-uid",
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type:   string(sandboxv1beta1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
				Reason: "Ready",
			}},
		},
	}

	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "extensions.agents.x-k8s.io", Resource: "sandboxclaims"},
		claim.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	// The first claim Update (the adoption annotation write) conflicts, as if
	// the cached base predated an earlier write; the in-pass retry re-reads and
	// succeeds.
	conflictOnce := true
	claimUpdates := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, candidate).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
					claimUpdates++
					if conflictOnce {
						conflictOnce = false
						return conflict
					}
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	warmSandboxQueue.Add(
		queue.GetNamespacedWarmPoolName("default", "test-pool"),
		queue.SandboxKey{Namespace: "default", Name: "pool-sb-1"},
	)

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: warmSandboxQueue,
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	res, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected the adoption conflict to be resolved in-pass, got error: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("expected adoption to complete in this pass with no requeue, got %+v", res)
	}
	if claimUpdates < 2 {
		t.Errorf("expected the conflicted update to be retried in-pass (>=2 claim updates), got %d", claimUpdates)
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, "pool-sb-1", updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation],
		"in-pass retry must record the adoption on the fresh base")
	require.Equal(t, "pool-sb-1", updatedClaim.Status.SandboxStatus.Name,
		"adoption must finalize status in the same pass")

	updatedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-1", Namespace: "default"}, updatedSandbox))
	controllerRef := metav1.GetControllerOf(updatedSandbox)
	require.NotNil(t, controllerRef)
	require.Equal(t, types.UID("claim-uid-123"), controllerRef.UID, "candidate must be owned by the claim after the retried adoption")
}

// newPoolCandidateSandbox builds a Ready, adoptable warm-pool member for the
// default/test-pool + test-template fixtures.
func newPoolCandidateSandbox(name string) *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			UID:        types.UID(name + "-uid"),
			Generation: 1,
			Labels: map[string]string{
				warmPoolSandboxLabel:   sandboxcontrollers.NameHash("test-pool"),
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "Ready",
			}},
		},
	}
}

// TestSandboxClaimAdoptionCompletionConflictDoesNotSwitchCandidates pins the
// no-flip invariant: a 409 on the adoption patch is resolved on the SAME
// candidate, never by switching to the next one mid-pass (measured: stale
// candidate views flipped one claim through 8 sandboxes under load).
func TestSandboxClaimAdoptionCompletionConflictDoesNotSwitchCandidates(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid-123"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	sb1 := newPoolCandidateSandbox("pool-sb-1")
	sb2 := newPoolCandidateSandbox("pool-sb-2")

	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
		sb1.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, sb1, sb2).
		WithStatusSubresource(claim).
		Build()

	// The first adoption patch against pool-sb-1 conflicts, as if the sandbox
	// controller wrote the candidate between the cache read and the patch.
	conflictOnce := true
	patchAttempts := map[string]int{}
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok {
				patchAttempts[sb.Name]++
				if sb.Name == "pool-sb-1" && conflictOnce {
					conflictOnce = false
					return conflict
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	poolKey := queue.GetNamespacedWarmPoolName("default", "test-pool")
	warmSandboxQueue.Add(poolKey, queue.SandboxKey{Namespace: "default", Name: "pool-sb-1"})
	warmSandboxQueue.Add(poolKey, queue.SandboxKey{Namespace: "default", Name: "pool-sb-2"})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: warmSandboxQueue,
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("expected the adoption-patch conflict to be resolved on a fresh base, got error: %v", err)
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, "pool-sb-1", updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation],
		"the committed assignment must never be overwritten with the next candidate on an adoption-patch conflict")
	require.Equal(t, "pool-sb-1", updatedClaim.Status.SandboxStatus.Name, "status must bind the originally assigned candidate")

	adopted := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rawClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-1", Namespace: "default"}, adopted))
	adoptedRef := metav1.GetControllerOf(adopted)
	require.NotNil(t, adoptedRef)
	require.Equal(t, types.UID("claim-uid-123"), adoptedRef.UID, "the assigned candidate must end up adopted")

	untouched := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rawClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-2", Namespace: "default"}, untouched))
	untouchedRef := metav1.GetControllerOf(untouched)
	require.NotNil(t, untouchedRef)
	require.Equal(t, "SandboxWarmPool", untouchedRef.Kind, "the second candidate must not be touched")
	require.Zero(t, patchAttempts["pool-sb-2"], "no adoption patch may be issued against the next candidate")
	require.Equal(t, 2, patchAttempts["pool-sb-1"], "exactly one doomed patch plus one fresh-base re-patch")
}

// TestSandboxClaimDoesNotAdoptCandidateThatBecomesUnreadyAfterAssignment
// verifies that the authoritative conflict path revalidates readiness without
// switching to another candidate in the same reconcile pass.
func TestSandboxClaimDoesNotAdoptCandidateThatBecomesUnreadyAfterAssignment(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid-123"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	sb1 := newPoolCandidateSandbox("pool-sb-1")
	sb2 := newPoolCandidateSandbox("pool-sb-2")

	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
		sb1.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, sb1, sb2).
		WithStatusSubresource(claim).
		Build()

	becameUnready := false
	patchAttempts := map[string]int{}
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			sb, ok := obj.(*sandboxv1beta1.Sandbox)
			if !ok {
				return c.Patch(ctx, obj, patch, opts...)
			}
			patchAttempts[sb.Name]++
			if sb.Name == sb1.Name && !becameUnready {
				becameUnready = true
				fresh := &sandboxv1beta1.Sandbox{}
				if err := rawClient.Get(ctx, client.ObjectKeyFromObject(sb1), fresh); err != nil {
					return err
				}
				ready := meta.FindStatusCondition(fresh.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
				if ready == nil {
					return errors.New("candidate is missing Ready condition")
				}
				ready.Status = metav1.ConditionFalse
				ready.Reason = "DependenciesNotReady"
				ready.ObservedGeneration = fresh.Generation
				if err := rawClient.Update(ctx, fresh); err != nil {
					return err
				}
				return conflict
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	poolKey := queue.GetNamespacedWarmPoolName("default", "test-pool")
	warmSandboxQueue.Add(poolKey, queue.SandboxKey{Namespace: "default", Name: sb1.Name})
	warmSandboxQueue.Add(poolKey, queue.SandboxKey{Namespace: "default", Name: sb2.Name})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: warmSandboxQueue,
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.ErrorIs(t, err, errAdoptionConflict)

	afterConflict := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, afterConflict))
	require.NotContains(t, afterConflict.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation)
	require.Empty(t, afterConflict.Status.SandboxStatus.Name, "the conflicted pass must not bind another candidate")

	unready := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rawClient.Get(context.Background(), client.ObjectKeyFromObject(sb1), unready))
	unreadyRef := metav1.GetControllerOf(unready)
	require.NotNil(t, unreadyRef)
	require.Equal(t, extensionsv1beta1.SandboxWarmPoolKind, unreadyRef.Kind)
	require.False(t, isSandboxReady(unready))
	require.Equal(t, 1, patchAttempts[sb1.Name], "the fresh unready candidate must not be re-patched")
	require.Zero(t, patchAttempts[sb2.Name], "the conflicted pass must not switch candidates")

	readyAgain := unready.DeepCopy()
	readyCondition := meta.FindStatusCondition(readyAgain.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCondition)
	readyCondition.Status = metav1.ConditionTrue
	readyCondition.Reason = "Ready"
	readyCondition.ObservedGeneration = readyAgain.Generation
	require.NoError(t, rawClient.Update(context.Background(), readyAgain))

	handler := &sandboxEventHandler{sandboxQueue: warmSandboxQueue}
	handler.Update(context.Background(), event.UpdateEvent{ObjectOld: unready, ObjectNew: readyAgain}, nil)
	var queuedKeys []queue.SandboxKey
	var queuedNames []string
	for {
		key, ok := warmSandboxQueue.Get(poolKey)
		if !ok {
			break
		}
		queuedKeys = append(queuedKeys, key)
		queuedNames = append(queuedNames, key.Name)
	}
	require.ElementsMatch(t, []string{sb1.Name, sb2.Name}, queuedNames,
		"a candidate that becomes ready again must return to the warm-pool queue")
	for _, key := range queuedKeys {
		warmSandboxQueue.Add(poolKey, key)
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("expected the converged pass to adopt the next ready candidate: %v", err)
	}
	adopted := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rawClient.Get(context.Background(), client.ObjectKeyFromObject(sb2), adopted))
	require.True(t, metav1.IsControlledBy(adopted, claim))
}

// TestSandboxClaimStaleAdoptionRepatchIdempotentWithoutWrite verifies a stale
// pass costs one doomed re-patch, zero effective writes: the fresh read shows
// the linkage already true and status finalizes from it.
func TestSandboxClaimStaleAdoptionRepatchIdempotentWithoutWrite(t *testing.T) {
	scheme := newScheme(t)
	claim, template, warmPool, adopted := newOptimisticLockTestObjects()

	// Frozen pre-adoption view of the assigned sandbox: still pool-owned,
	// exactly what a lagging informer serves right after adoption committed.
	staleAdopted := newPoolCandidateSandbox(adopted.Name)
	staleAdopted.UID = adopted.UID
	staleAdopted.ResourceVersion = "1"

	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
		adopted.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, adopted).
		WithStatusSubresource(claim).
		Build()

	staleView := true
	stalePatchRejections := 0
	adoptionPatches := 0
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == adopted.Name && staleView {
				staleAdopted.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && sb.Name == adopted.Name {
				data, derr := patch.Data(obj)
				if derr != nil {
					return derr
				}
				if strings.Contains(string(data), `"ownerReferences"`) {
					adoptionPatches++
				}
				// Emulate the apiserver's optimistic-lock check: reject any
				// patch whose precondition is the stale base's resourceVersion.
				if strings.Contains(string(data), `"resourceVersion":"1"`) {
					stalePatchRejections++
					return conflict
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("expected the stale re-patch to resolve as already-complete, got error: %v", err)
	}

	if stalePatchRejections != 1 {
		t.Errorf("expected exactly 1 stale-base patch rejection, got %d", stalePatchRejections)
	}
	if adoptionPatches != 1 {
		t.Errorf("expected NO adoption re-patch once the fresh read shows the linkage already true (1 doomed attempt only), got %d", adoptionPatches)
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, adopted.Name, updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation], "the assignment must be untouched")
	require.Equal(t, adopted.Name, updatedClaim.Status.SandboxStatus.Name, "status must finalize from the authoritative sandbox")
}

// TestSandboxClaimAssignedSandboxDeletedTerminalCleanup verifies a deleted
// assigned sandbox is terminal: one doomed write, authoritative cleanup of
// the reference (annotation or deprecated label), benign AdoptionConflict,
// no in-pass rebinding; the next pass re-adopts cleanly.
func TestSandboxClaimAssignedSandboxDeletedTerminalCleanup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fromLabel bool
	}{
		{name: "annotation reference"},
		{name: "deprecated label reference", fromLabel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			_, template, warmPool, _ := newOptimisticLockTestObjects()

			claim := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "claim-uid-123",
				},
				Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
			}
			if tc.fromLabel {
				claim.Labels = map[string]string{extensionsv1beta1.DeprecatedAssignedSandboxNameLabel: "ghost-sb"}
			} else {
				claim.Annotations = map[string]string{extensionsv1beta1.AssignedSandboxNameAnnotation: "ghost-sb"}
			}
			// ghost-sb exists only in the (stale) cache view; the server never has it.
			ghost := newPoolCandidateSandbox("ghost-sb")
			spare := newPoolCandidateSandbox("pool-sb-2")

			rawClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(template, warmPool, claim, spare).
				WithStatusSubresource(claim).
				Build()

			staleView := true
			ghostWrites := 0
			cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "ghost-sb" && staleView {
						ghost.DeepCopyInto(sb)
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && sb.Name == "ghost-sb" {
						ghostWrites++
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})

			warmSandboxQueue := queue.NewSimpleSandboxQueue()
			poolKey := queue.GetNamespacedWarmPoolName("default", "test-pool")
			warmSandboxQueue.Add(poolKey, queue.SandboxKey{Namespace: "default", Name: "pool-sb-2"})

			reconciler := &SandboxClaimReconciler{
				Client:           cachedClient,
				APIReader:        rawClient,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				Tracer:           asmetrics.NewNoOp(),
				WarmSandboxQueue: warmSandboxQueue,
			}

			// Pass 1 (stale view): terminal cleanup, no rebind, benign conflict error
			// so the workqueue's per-item rate limiter paces the retry.
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
			_, err := reconciler.Reconcile(context.Background(), req)
			if err == nil || !errors.Is(err, errAdoptionConflict) {
				t.Fatalf("expected a benign adoption-conflict error pacing the retry, got: %v", err)
			}
			if ghostWrites != 1 {
				t.Errorf("expected exactly 1 doomed write against the deleted sandbox (terminal, not retried), got %d", ghostWrites)
			}

			afterPass1 := &extensionsv1beta1.SandboxClaim{}
			require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, afterPass1))
			require.NotContains(t, afterPass1.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation,
				"the dead annotation reference must be cleaned up authoritatively in the terminal pass")
			require.NotContains(t, afterPass1.Labels, extensionsv1beta1.DeprecatedAssignedSandboxNameLabel,
				"the dead deprecated-label reference must be cleaned up authoritatively in the terminal pass")
			require.Empty(t, afterPass1.Status.SandboxStatus.Name, "pass 1 must not rebind to another candidate in-pass")
			readyCond := meta.FindStatusCondition(afterPass1.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
			require.NotNil(t, readyCond)
			require.Equal(t, "AdoptionConflict", readyCond.Reason, "the terminal pass surfaces the benign AdoptionConflict reason")

			spareAfter := &sandboxv1beta1.Sandbox{}
			require.NoError(t, rawClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-2", Namespace: "default"}, spareAfter))
			spareRef := metav1.GetControllerOf(spareAfter)
			require.NotNil(t, spareRef)
			require.Equal(t, "SandboxWarmPool", spareRef.Kind, "the spare candidate must not be adopted by the stale pass")

			// Pass 2 (converged view): the cleaned claim re-adopts the spare candidate.
			staleView = false
			if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("pass 2: expected clean re-adoption after cleanup, got: %v", err)
			}
			afterPass2 := &extensionsv1beta1.SandboxClaim{}
			require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, afterPass2))
			require.Equal(t, "pool-sb-2", afterPass2.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
			require.Equal(t, "pool-sb-2", afterPass2.Status.SandboxStatus.Name, "the converged pass re-adopts cleanly")
		})
	}
}

// TestSandboxClaimAdoptionCompletionExhaustedContentionKeepsReference covers
// the exhausted-contention tail: persistent 409s end the pass with a benign
// AdoptionConflict, the committed reference KEPT, and the raw apiserver
// conflict text trimmed from the surfaced condition message.
func TestSandboxClaimAdoptionCompletionExhaustedContentionKeepsReference(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "pool-sb-1",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	assigned := newPoolCandidateSandbox("pool-sb-1")

	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
		assigned.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, assigned).
		WithStatusSubresource(claim).
		Build()

	// Persistent contention: every adoption patch against the (genuinely
	// still adoptable) assigned sandbox conflicts, exhausting the in-pass
	// fresh-base retries.
	sandboxPatches := 0
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && sb.Name == "pool-sb-1" {
				sandboxPatches++
				return conflict
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil || !errors.Is(err, errAdoptionConflict) {
		t.Fatalf("expected the exhausted contention to surface as a benign adoption conflict, got: %v", err)
	}
	if sandboxPatches < 2 {
		t.Errorf("expected the adoption patch to be retried on fresh bases before exhausting, got %d attempts", sandboxPatches)
	}

	after := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, after))
	require.Equal(t, "pool-sb-1", after.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation],
		"exhausted contention must KEEP the committed reference: the sandbox is still adoptable and assigned to us")
	readyCond := meta.FindStatusCondition(after.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCond)
	require.Equal(t, "AdoptionConflict", readyCond.Reason)
	require.NotContains(t, readyCond.Message, "please apply your changes",
		"the raw apiserver conflict text must be trimmed from the surfaced condition message")
	require.Contains(t, readyCond.Message, "conflicting concurrent write")
}

// TestSandboxClaimAdoptionCleanupFailureKeepsReferenceAndRetries covers the
// cleanup-failure branch: sandbox gone AND reference cleanup failing must
// surface a stable, terse AdoptionConflict (internals go to logs only), keep
// the reference, and complete cleanup on a healed pass.
func TestSandboxClaimAdoptionCleanupFailureKeepsReferenceAndRetries(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "ghost-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	// ghost-sb exists only in the (stale) cache view; the server never has it.
	ghost := newPoolCandidateSandbox("ghost-sb")

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim).
		WithStatusSubresource(claim).
		Build()

	claimUpdateFails := true
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "ghost-sb" {
				ghost.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && claimUpdateFails {
				return k8errors.NewInternalError(errors.New("etcd hiccup"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil || !errors.Is(err, errAdoptionConflict) {
		t.Fatalf("expected a benign adoption conflict carrying the cleanup failure, got: %v", err)
	}
	require.Contains(t, err.Error(), "reference cleanup failed", "the cleanup failure must be named in the surfaced error")
	require.NotContains(t, err.Error(), "etcd hiccup", "internal cleanup error text belongs in logs, not the surfaced error")

	after := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, after))
	readyCond := meta.FindStatusCondition(after.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCond)
	require.Equal(t, "AdoptionConflict", readyCond.Reason)
	require.NotContains(t, readyCond.Message, "etcd hiccup",
		"internal cleanup error text must never reach the condition message")
	require.NotContains(t, readyCond.Message, "Internal error occurred",
		"apiserver internal-error boilerplate must never reach the condition message")
	require.Equal(t, "ghost-sb", after.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation],
		"the reference stays for the next pass to clean when cleanup itself failed")

	// Next pass with the cleanup write healed: terminal cleanup completes.
	claimUpdateFails = false
	if _, err := reconciler.Reconcile(context.Background(), req); err == nil || !errors.Is(err, errAdoptionConflict) {
		t.Fatalf("expected the healed pass to finish terminal cleanup with the benign conflict, got: %v", err)
	}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, after))
	require.NotContains(t, after.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation,
		"the healed pass must clean the dead reference")
}

// TestSandboxClaimAdoptionAnnotationAndCompletionConflictsResolvedSamePass
// covers the two conflict mechanisms composing in ONE reconcile pass: the
// annotation write 409s and is retried in-pass on a fresh base, then the
// completion patch 409s and is resolved on the same candidate — the pass
// still finishes the adoption with no error and no requeue.
func TestSandboxClaimAdoptionAnnotationAndCompletionConflictsResolvedSamePass(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid-123"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	candidate := newPoolCandidateSandbox("pool-sb-1")

	claimConflict := k8errors.NewConflict(
		schema.GroupResource{Group: "extensions.agents.x-k8s.io", Resource: "sandboxclaims"},
		claim.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)
	sandboxConflict := k8errors.NewConflict(
		schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
		candidate.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, candidate).
		WithStatusSubresource(claim).
		Build()

	// First annotation Update 409s (stale claim base), then the first
	// adoption patch 409s (concurrent candidate write): both in one pass.
	claimConflictOnce := true
	sandboxConflictOnce := true
	claimUpdates := 0
	sandboxPatches := 0
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
				claimUpdates++
				if claimConflictOnce {
					claimConflictOnce = false
					return claimConflict
				}
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && sb.Name == "pool-sb-1" {
				sandboxPatches++
				if sandboxConflictOnce {
					sandboxConflictOnce = false
					return sandboxConflict
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	warmSandboxQueue.Add(
		queue.GetNamespacedWarmPoolName("default", "test-pool"),
		queue.SandboxKey{Namespace: "default", Name: "pool-sb-1"},
	)

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: warmSandboxQueue,
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	res, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected both conflicts resolved in the same pass, got error: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("expected no requeue, got %+v", res)
	}
	if claimUpdates < 2 {
		t.Errorf("expected the annotation write to be retried in-pass (>=2 claim updates), got %d", claimUpdates)
	}
	if sandboxPatches != 2 {
		t.Errorf("expected exactly one doomed adoption patch plus one fresh-base re-patch, got %d", sandboxPatches)
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, "pool-sb-1", updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
	require.Equal(t, "pool-sb-1", updatedClaim.Status.SandboxStatus.Name, "the pass must finalize the binding despite both conflicts")

	adopted := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rawClient.Get(context.Background(), types.NamespacedName{Name: "pool-sb-1", Namespace: "default"}, adopted))
	adoptedRef := metav1.GetControllerOf(adopted)
	require.NotNil(t, adoptedRef)
	require.Equal(t, types.UID("claim-uid-123"), adoptedRef.UID)
}

// TestSandboxClaimAdoptionCleanupCancellationPropagates verifies a canceled
// cleanup write is propagated as cancellation, not classified as a benign
// adoption conflict.
func TestSandboxClaimAdoptionCleanupCancellationPropagates(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "ghost-sb",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	ghost := newPoolCandidateSandbox("ghost-sb")

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim).
		WithStatusSubresource(claim).
		Build()

	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "ghost-sb" {
				ghost.DeepCopyInto(sb)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
				return fmt.Errorf("client rate limiter wait: %w", context.Canceled)
			}
			return c.Update(ctx, obj, opts...)
		},
	})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation to propagate, got: %v", err)
	}
	if errors.Is(err, errAdoptionConflict) {
		t.Fatalf("cancellation must not be classified as a benign adoption conflict, got: %v", err)
	}
}

// TestSandboxClaimAdoptionResolveCancellationPropagates verifies a canceled
// completion patch during authoritative resolution propagates as
// cancellation — client-go's retry maps interrupted attempts to nil, which
// without the guard reported success with a nil resolved sandbox (panic).
func TestSandboxClaimAdoptionResolveCancellationPropagates(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "pool-sb-1",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	assigned := newPoolCandidateSandbox("pool-sb-1")

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, assigned).
		WithStatusSubresource(claim).
		Build()

	// First adoption patch 409s so the pass enters authoritative resolution;
	// the resolution's own re-patch is then interrupted, as during shutdown.
	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
		assigned.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)
	patches := 0
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && sb.Name == "pool-sb-1" {
				patches++
				if patches == 1 {
					return conflict
				}
				return fmt.Errorf("client rate limiter wait: %w", context.Canceled)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation to propagate (not success or panic), got: %v", err)
	}
	if errors.Is(err, errAdoptionConflict) {
		t.Fatalf("cancellation must not be classified as a benign adoption conflict, got: %v", err)
	}

	require.GreaterOrEqual(t, patches, 2, "the pass must enter resolution (doomed patch) and be canceled on the re-patch")

	// The committed reference must be untouched for the next process to finish.
	after := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, rawClient.Get(context.Background(), req.NamespacedName, after))
	require.Equal(t, "pool-sb-1", after.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
}

// TestSandboxClaimAdoptionResolveConflictThenCancellationPropagates pins the
// conflict-then-cancel ordering: a conflict inside the resolution retry makes
// client-go report the conflict as the loop error, which must not mask the
// later cancellation as a benign AdoptionConflict.
func TestSandboxClaimAdoptionResolveConflictThenCancellationPropagates(t *testing.T) {
	scheme := newScheme(t)
	_, template, warmPool, _ := newOptimisticLockTestObjects()

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				extensionsv1beta1.AssignedSandboxNameAnnotation: "pool-sb-1",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	assigned := newPoolCandidateSandbox("pool-sb-1")

	conflict := k8errors.NewConflict(
		schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
		assigned.Name,
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)

	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, assigned).
		WithStatusSubresource(claim).
		Build()

	// Patch 1 (outer completeAdoption) and patch 2 (resolution attempt 1)
	// conflict; patch 3 (resolution attempt 2) is interrupted.
	patches := 0
	cachedClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && sb.Name == "pool-sb-1" {
				patches++
				if patches <= 2 {
					return conflict
				}
				return fmt.Errorf("client rate limiter wait: %w", context.Canceled)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	reconciler := &SandboxClaimReconciler{
		Client:           cachedClient,
		APIReader:        rawClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation to propagate past the earlier conflict, got: %v", err)
	}
	if errors.Is(err, errAdoptionConflict) {
		t.Fatalf("a cancellation after a conflict must not be masked as a benign adoption conflict, got: %v", err)
	}
	require.Equal(t, 3, patches, "two conflicted patches then the interrupted one")
}

func TestCreateSandboxToleratesAlreadyExists(t *testing.T) {
	scheme := newScheme(t)
	claimName := "already-exists-claim"

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: "claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
		Status: extensionsv1beta1.SandboxClaimStatus{
			SandboxStatus: extensionsv1beta1.SandboxStatus{Name: claimName},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	// Pre-create the sandbox owned by this claim to simulate a previous successful create.
	existingSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxClaim",
				Name:       claimName,
				UID:        "claim-uid",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	// Cache lag: first reconcile's sandbox Gets return NotFound (so Create
	// hits AlreadyExists); flip the flag afterwards to let the cache catch up.
	cacheStale := true

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, warmPool, template, existingSandbox).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == claimName && cacheStale {
					return k8errors.NewNotFound(
						schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
						key.Name,
					)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}

	// First reconcile: cache miss triggers AlreadyExists, converted to bounded requeue.
	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err, "sentinel should be converted to nil error")
	require.Equal(t, cacheLagRequeueDelay, result.RequeueAfter, "expected bounded requeue delay")

	// The guard short-circuits on the pre-existing SandboxStatus.Name, so the
	// status is preserved intact.
	pendingClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, pendingClaim))
	require.Equal(t, claimName, pendingClaim.Status.SandboxStatus.Name, "SandboxStatus.Name should not be wiped during cache lag")

	// Cache has caught up — sandbox will be visible on the next reconcile.
	cacheStale = false

	// Second reconcile: cache has caught up, sandbox is found and returned.
	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Verify the claim status points to the existing sandbox.
	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, claimName, updatedClaim.Status.SandboxStatus.Name)

	// Verify the Ready condition is set now that the sandbox is visible.
	readyCond := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCond, "expected Ready condition after cache converged")
	require.NotEqual(t, "SandboxCreatePending", readyCond.Reason, "expected pending reason to clear once cache converged")
}

// TestCreateSandboxAlreadyExistsRecoversViaAuthoritativeRead verifies that an
// AlreadyExists from a cold-start Create is resolved in the same pass: the
// APIReader read-back returns the live sandbox even though the informer cache
// never converges, so the claim binds immediately with no cache-lag requeue.
func TestCreateSandboxAlreadyExistsRecoversViaAuthoritativeRead(t *testing.T) {
	scheme := newScheme(t)
	claimName := "already-exists-live-read-claim"

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: "claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	// Pre-create the sandbox owned by this claim to simulate a previous successful create.
	existingSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxClaim",
				Name:       claimName,
				UID:        "claim-uid",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	// The cache never converges in this test: every cached sandbox Get stays
	// NotFound, so recovery can only come from the authoritative read.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, warmPool, template, existingSandbox).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == claimName {
					return k8errors.NewNotFound(
						schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
						key.Name,
					)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	apiReader := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(existingSandbox.DeepCopy()).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		APIReader:        apiReader,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter, "in-pass recovery must not fall back to the cache-lag requeue or any other delay")

	// The claim binds to the pre-existing sandbox in the first pass.
	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Equal(t, claimName, updatedClaim.Status.SandboxStatus.Name, "claim should bind to the live-read sandbox in the same pass")

	readyCond := meta.FindStatusCondition(updatedClaim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCond, "expected Ready condition after in-pass recovery")
	require.NotEqual(t, "SandboxCreatePending", readyCond.Reason, "in-pass recovery must not report SandboxCreatePending")
}

// TestCreateSandboxAlreadyExistsNameCollisionIsTerminal verifies that when the
// authoritative read-back shows the existing sandbox is NOT controlled by the
// claim, the pass reports the collision instead of adopting it or retrying on
// the cache-lag sentinel.
func TestCreateSandboxAlreadyExistsNameCollisionIsTerminal(t *testing.T) {
	scheme := newScheme(t)
	claimName := "already-exists-collision-claim"

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: "claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	// A sandbox with the claim's name but controlled by a DIFFERENT claim.
	foreignSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxClaim",
				Name:       "some-other-claim",
				UID:        "other-claim-uid",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, warmPool, template, foreignSandbox).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == claimName {
					return k8errors.NewNotFound(
						schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
						key.Name,
					)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	apiReader := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(foreignSandbox.DeepCopy()).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		APIReader:        apiReader,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.Error(t, err, "a name collision must surface as an error, not be adopted")
	require.ErrorContains(t, err, "is not controlled by claim")
	require.NotErrorIs(t, err, errSandboxAlreadyExists, "collision must not be classified as benign cache lag")

	// The claim must not bind to the foreign sandbox.
	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim))
	require.Empty(t, updatedClaim.Status.SandboxStatus.Name, "claim must not bind to a sandbox it does not control")
}

// TestCreateSandboxAlreadyExistsAuthoritativeReadFailure verifies the fallback
// when a CONFIGURED APIReader fails after an AlreadyExists: createSandbox must
// keep the cache-lag sentinel, the original AlreadyExists, and the read error
// all identifiable in the error chain, and Reconcile must convert the sentinel
// into the bounded cacheLagRequeueDelay requeue with a nil error.
func TestCreateSandboxAlreadyExistsAuthoritativeReadFailure(t *testing.T) {
	scheme := newScheme(t)
	claimName := "already-exists-read-failure-claim"

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default", UID: "claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"},
		},
	}

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	// Pre-create the sandbox owned by this claim to simulate a previous successful create.
	existingSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxClaim",
				Name:       claimName,
				UID:        "claim-uid",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "test"}},
			},
		}}},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(claim, warmPool, template, existingSandbox).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == claimName {
					return k8errors.NewNotFound(
						schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
						key.Name,
					)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	// The APIReader is configured but unhealthy: every read fails with a
	// distinguishable sentinel.
	readFailure := errors.New("authoritative reader unavailable")
	apiReader := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return readFailure
			},
		}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:           fakeClient,
		APIReader:        apiReader,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: queue.NewSimpleSandboxQueue(),
	}

	// Direct createSandbox call: the full error chain must stay inspectable.
	_, err := reconciler.createSandbox(context.Background(), claim.DeepCopy(), template)
	require.Error(t, err)
	require.ErrorIs(t, err, errSandboxAlreadyExists, "sentinel must survive for the bounded-requeue conversion")
	require.ErrorIs(t, err, readFailure, "read failure cause must stay identifiable via errors.Is")
	require.True(t, k8errors.IsAlreadyExists(err), "original AlreadyExists must stay identifiable")

	// Reconcile-level: the sentinel converts to the bounded requeue with nil error.
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}}
	result, reconcileErr := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, reconcileErr, "sentinel should be converted to nil error")
	require.Equal(t, cacheLagRequeueDelay, result.RequeueAfter, "expected bounded requeue delay when the authoritative read fails")
}
