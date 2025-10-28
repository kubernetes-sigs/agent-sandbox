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
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

func TestSandboxClaimReconcile(t *testing.T) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: v1alpha1.PodTemplate{
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

func TestSandboxClaimPodAdoption(t *testing.T) {
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

	// Create a warm pool with a SandboxWarmPool controller reference
	warmPoolUID := types.UID("warmpool-uid-123")
	poolNameHash := sandboxcontrollers.NameHash("test-pool")

	createWarmPoolPod := func(name string, creationTime metav1.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				CreationTimestamp: creationTime,
				Labels: map[string]string{
					poolLabel:              poolNameHash,
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
						Kind:       "SandboxWarmPool",
						Name:       "test-pool",
						UID:        warmPoolUID,
						Controller: func(b bool) *bool { return &b }(true),
					},
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
		}
	}

	createPodWithDifferentController := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       "other-controller",
						UID:        "other-uid-456",
						Controller: func(b bool) *bool { return &b }(true),
					},
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
		}
	}

	createDeletingPod := func(name string) *corev1.Pod {
		pod := createWarmPoolPod(name, metav1.Now())
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		// Add a finalizer so the fake client accepts the object with deletionTimestamp
		pod.Finalizers = []string{"test-finalizer"}
		return pod
	}

	testCases := []struct {
		name                string
		existingObjects     []client.Object
		expectPodAdoption   bool
		expectedAdoptedPod  string // name of the pod that should be adopted
		expectSandboxCreate bool
	}{
		{
			name: "adopts oldest pod from warm pool",
			existingObjects: []client.Object{
				template,
				claim,
				createWarmPoolPod("pool-pod-1", metav1.Time{Time: metav1.Now().Add(-3600)}), // oldest
				createWarmPoolPod("pool-pod-2", metav1.Time{Time: metav1.Now().Add(-1800)}),
				createWarmPoolPod("pool-pod-3", metav1.Now()),
			},
			expectPodAdoption:   true,
			expectedAdoptedPod:  "pool-pod-1",
			expectSandboxCreate: true,
		},
		{
			name: "creates sandbox without adoption when no warm pool pods exist",
			existingObjects: []client.Object{
				template,
				claim,
			},
			expectPodAdoption:   false,
			expectSandboxCreate: true,
		},
		{
			name: "skips pods with different controller",
			existingObjects: []client.Object{
				template,
				claim,
				createPodWithDifferentController("other-pod-1"),
				createWarmPoolPod("pool-pod-1", metav1.Now()),
			},
			expectPodAdoption:   true,
			expectedAdoptedPod:  "pool-pod-1",
			expectSandboxCreate: true,
		},
		{
			name: "skips pods being deleted",
			existingObjects: []client.Object{
				template,
				claim,
				createDeletingPod("deleting-pod"),
				createWarmPoolPod("pool-pod-1", metav1.Now()),
			},
			expectPodAdoption:   true,
			expectedAdoptedPod:  "pool-pod-1",
			expectSandboxCreate: true,
		},
		{
			name: "no adoption when only ineligible pods exist",
			existingObjects: []client.Object{
				template,
				claim,
				createPodWithDifferentController("other-pod-1"),
				createDeletingPod("deleting-pod"),
			},
			expectPodAdoption:   false,
			expectSandboxCreate: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tc.existingObjects...).
				WithStatusSubresource(claim).
				Build()

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

			ctx := context.Background()
			_, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			// Verify sandbox was created
			var sandbox v1alpha1.Sandbox
			err = client.Get(ctx, req.NamespacedName, &sandbox)
			if tc.expectSandboxCreate && err != nil {
				t.Fatalf("expected sandbox to be created but got error: %v", err)
			}
			if !tc.expectSandboxCreate && !k8errors.IsNotFound(err) {
				t.Fatalf("expected sandbox not to be created but it exists")
			}

			if tc.expectPodAdoption {
				// Verify the adopted pod has correct labels and owner reference
				var adoptedPod corev1.Pod
				err = client.Get(ctx, types.NamespacedName{
					Name:      tc.expectedAdoptedPod,
					Namespace: "default",
				}, &adoptedPod)
				if err != nil {
					t.Fatalf("failed to get adopted pod: %v", err)
				}

				// Verify pool labels were removed
				if _, exists := adoptedPod.Labels[poolLabel]; exists {
					t.Errorf("expected pool label to be removed from adopted pod")
				}
				if _, exists := adoptedPod.Labels[sandboxTemplateRefHash]; exists {
					t.Errorf("expected sandbox template ref label to be removed from adopted pod")
				}
			} else if tc.expectSandboxCreate {
				// Verify no pod name label when no adoption occurred
				if sandbox.Labels != nil {
					if podName, exists := sandbox.Labels[sandboxcontrollers.SanboxPodNameAnnotation]; exists {
						t.Errorf("expected no pod name label but found %q", podName)
					}
				}
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
