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
	"time" // Added for shutdownTime

	"github.com/google/go-cmp/cmp" // Added for ignoring fields
	corev1 "k8s.io/api/core/v1"
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
		name              string
		existingObjects   []client.Object
		expectSandbox     bool
		expectError       bool
		expectedCondition metav1.Condition
	}{
		{
			name:            "sandbox is created when a claim is made",
			existingObjects: []client.Object{template, claim},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name:            "sandbox is not created when template is not found",
			existingObjects: []client.Object{claim},
			expectSandbox:   false,
			expectError:     true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "TemplateNotFound",
				Message: `SandboxTemplate "test-template" not found`,
			},
		},
		{
			name:            "sandbox exists but is not controlled by claim",
			existingObjects: []client.Object{template, claim, uncontrolledSandbox},
			expectSandbox:   true,
			expectError:     true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "ReconcilerError",
				Message: "Error seen: sandbox \"test-claim\" is not controlled by claim \"test-claim\". Please use a different claim name or delete the sandbox manually",
			},
		},
		{
			name:            "sandbox exists and is controlled by claim",
			existingObjects: []client.Object{template, claim, controlledSandbox},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name:            "sandbox exists but template is not found",
			existingObjects: []client.Object{claim, readySandbox},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionTrue,
				Reason:  "SandboxReady",
				Message: "Sandbox is ready",
			},
		},
		{
			name:            "sandbox is ready",
			existingObjects: []client.Object{template, claim, readySandbox},
			expectSandbox:   true,
			expectedCondition: metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionTrue,
				Reason:  "SandboxReady",
				Message: "Sandbox is ready",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.existingObjects...).WithStatusSubresource(claim).Build()
			reconciler := &SandboxClaimReconciler{
				Client: client,
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-claim",
					Namespace: "default",
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

			if tc.expectSandbox {
				if diff := cmp.Diff(sandbox.Spec.PodTemplate.Spec, template.Spec.PodTemplate.Spec); diff != "" {
					t.Errorf("unexpected sandbox spec:\n%s", diff)
				}
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
			}
			if diff := cmp.Diff(tc.expectedCondition, condition, cmp.Comparer(ignoreTimestamp)); diff != "" {
				t.Errorf("unexpected condition:\n%s", diff)
			}
		})
	}
}

func TestSandboxClaimShutdownTime(t *testing.T) {
	// 1. Setup static "fake" times for deterministic tests
	fakeTemplateTime := &metav1.Time{Time: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}
	fakeOverrideTime := &metav1.Time{Time: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)}

	// 2. Define the base template
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate:  corev1.PodTemplateSpec{}, // Not relevant for this test
			ShutdownTime: fakeTemplateTime,
		},
	}

	// 3. Define test cases
	testCases := []struct {
		name                 string
		claimToReconcile     *extensionsv1alpha1.SandboxClaim
		existingObjects      []client.Object
		expectedShutdownTime *metav1.Time
	}{
		{
			name: "should use template shutdownTime when claim has none",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-default-time", Namespace: "default", UID: "uid-1"},
				Spec:       extensionsv1alpha1.SandboxClaimSpec{TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"}},
			},
			existingObjects:      []client.Object{template},
			expectedShutdownTime: fakeTemplateTime, // Expects the template's time
		},
		{
			name: "should use claim shutdownTime as an override",
			claimToReconcile: &extensionsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-override-time", Namespace: "default", UID: "uid-2"},
				Spec: extensionsv1alpha1.SandboxClaimSpec{
					TemplateRef:  extensionsv1alpha1.SandboxTemplateRef{Name: "test-template"},
					ShutdownTime: fakeOverrideTime, // Override is set here
				},
			},
			existingObjects:      []client.Object{template},
			expectedShutdownTime: fakeOverrideTime, // Expects the claim's time
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
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

			// ACT
			_, err := reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("reconcile: (%v)", err)
			}

			// ASSERT
			// Check that the created Sandbox has the correct shutdownTime
			var sandbox v1alpha1.Sandbox
			err = client.Get(context.Background(), req.NamespacedName, &sandbox)
			if err != nil {
				t.Fatalf("get sandbox: (%v)", err)
			}

			// Use a comparer that treats the time values as equal
			timeComparer := cmp.Comparer(func(x, y *metav1.Time) bool {
				if x == nil && y == nil {
					return true
				}
				if x != nil && y != nil {
					return x.Time.Equal(y.Time)
				}
				return false
			})

			if diff := cmp.Diff(tc.expectedShutdownTime, sandbox.Spec.ShutdownTime, timeComparer); diff != "" {
				t.Errorf("unexpected sandbox ShutdownTime (-want +got):\n%s", diff)
			}
		})
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("reconcile: (%v)", err)
	}
	if err := extensionsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("reconcile: (%v)", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("reconcile: (%v)", err)
	}
	return scheme
}

func ignoreTimestamp(_, _ metav1.Time) bool {
	return true
}
