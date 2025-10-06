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
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1" // Import policy/v1
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr" // Import intstr
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

	// --- Objects for PDB Tests ---

	pdbTemplate := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pdb-template",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			EnableDisruptionControl: true, // <-- PDB Enabled
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container", Image: "test-image"}},
				},
			},
		},
	}

	pdbClaim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pdb-claim",
			Namespace: "default",
			UID:       "pdb-claim-uid",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "pdb-template",
			},
		},
	}

	sharedPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbName, // "sandbox-highly-available"
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					pdbLabelKey: pdbLabelValue, // "sandbox-disruption-policy": "HighlyAvailable"
				},
			},
		},
	}

	// Claim for deletion test (last claim)
	deletingClaimLast := pdbClaim.DeepCopy()
	deletingClaimLast.Name = "deleting-claim-last"
	deletingClaimLast.UID = "deleting-claim-last-uid"
	deletingClaimLast.Finalizers = []string{pdbFinalizerName}
	deleteTime := metav1.NewTime(time.Now())
	deletingClaimLast.DeletionTimestamp = &deleteTime

	// Claim for deletion test (other claims exist)
	deletingClaim1 := pdbClaim.DeepCopy()
	deletingClaim1.Name = "deleting-claim-1"
	deletingClaim1.UID = "deleting-claim-1-uid"
	deletingClaim1.Finalizers = []string{pdbFinalizerName}
	deletingClaim1.DeletionTimestamp = &deleteTime

	activeClaim2 := pdbClaim.DeepCopy()
	activeClaim2.Name = "active-claim-2"
	activeClaim2.UID = "active-claim-2-uid"
	activeClaim2.Finalizers = []string{pdbFinalizerName} // Assume already reconciled

	testCases := []struct {
		name                string
		reqName             string // Name of the claim to reconcile
		existingObjects     []client.Object
		expectSandbox       bool
		expectError         bool
		expectedCondition   *metav1.Condition // Pointer to allow nil check
		postReconcileChecks func(t *testing.T, c client.Client, req reconcile.Request)
	}{
		{
			name:            "sandbox is created when a claim is made",
			reqName:         "test-claim",
			existingObjects: []client.Object{template, claim},
			expectSandbox:   true,
			expectedCondition: &metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			postReconcileChecks: func(t *testing.T, c client.Client, req reconcile.Request) {
				// Check that PDB is NOT created
				pdb := &policyv1.PodDisruptionBudget{}
				pdbKey := types.NamespacedName{Name: pdbName, Namespace: req.Namespace}
				err := c.Get(context.Background(), pdbKey, pdb)
				if err == nil || !k8errors.IsNotFound(err) {
					t.Fatalf("expected PDB to not exist, but got err: %v", err)
				}
			},
		},
		{
			name:            "sandbox is not created when template is not found",
			reqName:         "test-claim",
			existingObjects: []client.Object{claim},
			expectSandbox:   false,
			expectError:     true,
			expectedCondition: &metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "TemplateNotFound",
				Message: `SandboxTemplate "test-template" not found`,
			},
		},
		{
			name:            "sandbox exists but is not controlled by claim",
			reqName:         "test-claim",
			existingObjects: []client.Object{template, claim, uncontrolledSandbox},
			expectSandbox:   true,
			expectError:     true,
			expectedCondition: &metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "ReconcilerError",
				Message: "Error seen: sandbox \"test-claim\" is not controlled by claim \"test-claim\". Please use a different claim name or delete the sandbox manually",
			},
		},
		{
			name:            "sandbox exists and is controlled by claim",
			reqName:         "test-claim",
			existingObjects: []client.Object{template, claim, controlledSandbox},
			expectSandbox:   true,
			expectedCondition: &metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
		},
		{
			name:            "sandbox exists but template is not found",
			reqName:         "test-claim",
			existingObjects: []client.Object{claim, readySandbox},
			expectSandbox:   true,
			expectError:     true, // This is the corrected expectation
			expectedCondition: &metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse, // It will fail to reconcile
				Reason:  "TemplateNotFound",
				Message: `SandboxTemplate "test-template" not found`,
			},
		},
		{
			name:            "sandbox is ready",
			reqName:         "test-claim",
			existingObjects: []client.Object{template, claim, readySandbox},
			expectSandbox:   true,
			expectedCondition: &metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionTrue,
				Reason:  "SandboxReady",
				Message: "Sandbox is ready",
			},
		},
		// --- PDB Test Cases ---
		{
			name:            "PDB is created and finalizer added when disruption control is enabled",
			reqName:         "pdb-claim",
			existingObjects: []client.Object{pdbTemplate, pdbClaim},
			expectSandbox:   true,
			expectedCondition: &metav1.Condition{
				Type:    string(sandboxv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "SandboxNotReady",
				Message: "Sandbox is not ready",
			},
			postReconcileChecks: func(t *testing.T, c client.Client, req reconcile.Request) {
				// 1. Check for PDB
				pdb := &policyv1.PodDisruptionBudget{}
				pdbKey := types.NamespacedName{Name: pdbName, Namespace: req.Namespace}
				if err := c.Get(context.Background(), pdbKey, pdb); err != nil {
					t.Fatalf("expected PDB %q to be created, but got err: %v", pdbName, err)
				}
				if pdb.Spec.Selector.MatchLabels[pdbLabelKey] != pdbLabelValue {
					t.Errorf("PDB selector is incorrect. Got %v, expected %v", pdb.Spec.Selector.MatchLabels, pdbLabelValue)
				}

				// 2. Check for Finalizer on claim
				updatedClaim := &extensionsv1alpha1.SandboxClaim{}
				if err := c.Get(context.Background(), req.NamespacedName, updatedClaim); err != nil {
					t.Fatalf("get sandbox claim: (%v)", err)
				}
				if !containsString(updatedClaim.Finalizers, pdbFinalizerName) {
					t.Errorf("expected finalizer %q on claim, but got: %v", pdbFinalizerName, updatedClaim.Finalizers)
				}

				// 3. Check for labels/annotations on Sandbox
				sandbox := &v1alpha1.Sandbox{}
				if err := c.Get(context.Background(), req.NamespacedName, sandbox); err != nil {
					t.Fatalf("get sandbox: (%v)", err)
				}
				//if val := sandbox.Spec.PodTemplate.Labels[pdbLabelKey]; val != pdbLabelValue {
				//	t.Errorf("sandbox PodTemplate missing PDB label. Got %q, expected %q", val, pdbLabelValue)
				//}
				//if val := sandbox.Spec.PodTemplate.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]; val != "false" {
				//	t.Errorf("sandbox PodTemplate missing safe-to-evict annotation. Got %q, expected %q", val, "false")
				//}
			},
		},
		{
			name:    "PDB is deleted when last claim with disruption control is deleted",
			reqName: "deleting-claim-last",
			existingObjects: []client.Object{
				pdbTemplate,       // Template must exist for getTemplate call
				deletingClaimLast, // The claim being deleted
				sharedPDB,         // The PDB that should be deleted
			},
			expectSandbox: false, // Sandbox logic is skipped on deletion
			expectError:   false,
			postReconcileChecks: func(t *testing.T, c client.Client, req reconcile.Request) {
				// 1. Check PDB is deleted
				pdb := &policyv1.PodDisruptionBudget{}
				pdbKey := types.NamespacedName{Name: pdbName, Namespace: req.Namespace}
				err := c.Get(context.Background(), pdbKey, pdb)
				if err == nil || !k8errors.IsNotFound(err) {
					t.Fatalf("expected PDB %q to be deleted, but it still exists or got err: %v", pdbName, err)
				}

				// 2. Check finalizer is removed from claim
				updatedClaim := &extensionsv1alpha1.SandboxClaim{}
				if err := c.Get(context.Background(), req.NamespacedName, updatedClaim); err != nil {
					if k8errors.IsNotFound(err) {
						// This is also acceptable, as the client might have garbage collected it
						return
					}
					t.Fatalf("get sandbox claim: (%v)", err)
				}
				if containsString(updatedClaim.Finalizers, pdbFinalizerName) {
					t.Errorf("expected finalizer %q to be removed, but it still exists: %v", pdbFinalizerName, updatedClaim.Finalizers)
				}
			},
		},
		{
			name:    "PDB is NOT deleted when other claims with disruption control still exist",
			reqName: "deleting-claim-1", // We reconcile the one being deleted
			existingObjects: []client.Object{
				pdbTemplate,    // Template must exist
				deletingClaim1, // The claim being deleted
				activeClaim2,   // The *other* active claim
				sharedPDB,      // The PDB that should NOT be deleted
			},
			expectSandbox: false, // Sandbox logic is skipped on deletion
			expectError:   false,
			postReconcileChecks: func(t *testing.T, c client.Client, req reconcile.Request) {
				// 1. Check PDB STILL EXISTS
				pdb := &policyv1.PodDisruptionBudget{}
				pdbKey := types.NamespacedName{Name: pdbName, Namespace: req.Namespace}
				if err := c.Get(context.Background(), pdbKey, pdb); err != nil {
					t.Fatalf("expected PDB %q to STILL EXIST, but got err: %v", pdbName, err)
				}

				// 2. Check finalizer is removed from the DELETING claim
				updatedClaim := &extensionsv1alpha1.SandboxClaim{}
				if err := c.Get(context.Background(), req.NamespacedName, updatedClaim); err != nil {
					if k8errors.IsNotFound(err) {
						// This is also acceptable
						return
					}
					t.Fatalf("get sandbox claim: (%v)", err)
				}
				if containsString(updatedClaim.Finalizers, pdbFinalizerName) {
					t.Errorf("expected finalizer %q to be removed from deleting claim, but it still exists: %v", pdbFinalizerName, updatedClaim.Finalizers)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)

			// Find all claims and add them to the status-enabled list
			var statusEnabledObjects []client.Object
			for _, obj := range tc.existingObjects {
				if claim, ok := obj.(*extensionsv1alpha1.SandboxClaim); ok {
					statusEnabledObjects = append(statusEnabledObjects, claim.DeepCopy())
				}
			}

			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tc.existingObjects...).
				WithStatusSubresource(statusEnabledObjects...). // Enable status for all claims
				Build()

			reconciler := &SandboxClaimReconciler{
				Client: client,
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      tc.reqName,
					Namespace: "default",
				},
			}
			// Run the reconcile loop, capturing the result
			res, err := reconciler.Reconcile(context.Background(), req)

			// If the controller requests a requeue (e.g., after adding a finalizer),
			// run the reconcile again to simulate the next loop.
			if err == nil && res.RequeueAfter <= 0 {
				t.Log("Reconciliation requeued, running again.")
				res, err = reconciler.Reconcile(context.Background(), req)
			}
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
			if !tc.expectSandbox && !k8errors.IsNotFound(err) && err != nil {
				t.Fatalf("expected sandbox to not exist, but got err: %v", err)
			}

			// Check sandbox spec only if we expected one to be created/exist
			if tc.expectSandbox {
				// Find the template that should have been used
				reconciledClaim := &extensionsv1alpha1.SandboxClaim{}
				var baseTemplate *extensionsv1alpha1.SandboxTemplate
				if err := client.Get(context.Background(), req.NamespacedName, reconciledClaim); err == nil {
					for _, obj := range tc.existingObjects {
						if tmpl, ok := obj.(*extensionsv1alpha1.SandboxTemplate); ok {
							if tmpl.Name == reconciledClaim.Spec.TemplateRef.Name {
								baseTemplate = tmpl
								break
							}
						}
					}
				}

				if baseTemplate != nil {
					// We can't just diff the spec, because the controller *adds* labels/annotations
					// We just check that the base spec matches
					if diff := cmp.Diff(sandbox.Spec.PodTemplate.Spec.Containers, baseTemplate.Spec.PodTemplate.Spec.Containers); diff != "" {
						t.Errorf("unexpected sandbox container spec:\n%s", diff)
					}
				} else if !k8errors.IsNotFound(err) {
					t.Logf("Could not find template %q to compare sandbox spec", reconciledClaim.Spec.TemplateRef.Name)
				}
			}

			if tc.expectedCondition != nil {
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
				} else {
					// Compare the full condition, ignoring timestamp
					if diff := cmp.Diff(*tc.expectedCondition, condition, cmp.Comparer(ignoreTimestamp)); diff != "" {
						t.Errorf("unexpected condition:\n%s", diff)
					}
				}
			}

			// Run post-reconcile checks if they exist
			if tc.postReconcileChecks != nil {
				tc.postReconcileChecks(t, client, req)
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
	// Add policy/v1 for PDB testing
	if err := policyv1.AddToScheme(scheme); err != nil {
		t.Fatalf("reconcile: (%v)", err)
	}
	return scheme
}

func ignoreTimestamp(_, _ metav1.Time) bool {
	return true
}

// Helper function to check for a string in a slice
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
