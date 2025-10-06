// Copyright 2025 The Kubernetes Authors.
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

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestSandboxPolicyReconciler_Reconcile covers creation and no-op scenarios.
func TestSandboxPolicyReconciler_Reconcile(t *testing.T) {
	sandboxName := "test-sandbox"
	sandboxNs := "default"
	sandboxKey := types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}

	// A pod that would be created by the main sandbox-controller
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			Labels: map[string]string{
				// Assume this label is present for the selector to match
				sandboxLabel: NameHash(sandboxName),
			},
		},
	}

	testCases := []struct {
		name             string
		initialSandbox   *sandboxv1alpha1.Sandbox
		expectPDB        bool
		expectAnnotation bool
	}{
		{
			name: "should create PDB and add annotation when annotation is present",
			initialSandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1alpha1.PDBRequiredAnnotation: "true",
					},
				},
			},
			expectPDB:        true,
			expectAnnotation: true,
		},
		{
			name: "should do nothing when annotation is absent",
			initialSandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
			},
			expectPDB:        false,
			expectAnnotation: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(Scheme).
				WithRuntimeObjects(tc.initialSandbox.DeepCopy(), existingPod.DeepCopy()).
				Build()

			r := &SandboxPolicyReconciler{Client: fakeClient}
			req := ctrl.Request{NamespacedName: sandboxKey}

			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			// Check PDB state
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Get(context.Background(), sandboxKey, pdb)
			if tc.expectPDB {
				require.NoError(t, err, "Expected PDB to be created")
			} else {
				require.True(t, k8serrors.IsNotFound(err), "Expected PDB to not exist")
			}

			// Check Pod annotation state
			pod := &corev1.Pod{}
			err = r.Get(context.Background(), sandboxKey, pod)
			require.NoError(t, err)

			_, hasAnnotation := pod.Annotations[safeToEvictAnnotation]
			require.Equal(t, tc.expectAnnotation, hasAnnotation)
		})
	}
}

// TestSandboxPolicyReconciler_Cleanup tests the update/cleanup scenario.
func TestSandboxPolicyReconciler_Cleanup(t *testing.T) {
	sandboxName := "cleanup-sandbox"
	sandboxNs := "default"
	sandboxKey := types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}

	initialSandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			Annotations: map[string]string{
				sandboxv1alpha1.PDBRequiredAnnotation: "true",
			},
		},
	}
	initialPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			Labels:    map[string]string{sandboxLabel: NameHash(sandboxName)},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(Scheme).
		WithRuntimeObjects(initialSandbox, initialPod).
		Build()

	r := &SandboxPolicyReconciler{Client: fakeClient}
	req := ctrl.Request{NamespacedName: sandboxKey}

	// First reconcile: Create the PDB and add annotation
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Verify PDB was created
	pdb := &policyv1.PodDisruptionBudget{}
	require.NoError(t, r.Get(context.Background(), sandboxKey, pdb), "PDB should exist after first reconcile")

	// Verify Pod has annotation
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), sandboxKey, pod))
	require.Equal(t, "false", pod.Annotations[safeToEvictAnnotation])

	// Update the Sandbox to remove the annotation
	updatedSandbox := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, r.Get(context.Background(), sandboxKey, updatedSandbox))
	delete(updatedSandbox.Annotations, sandboxv1alpha1.PDBRequiredAnnotation)
	require.NoError(t, r.Update(context.Background(), updatedSandbox))

	// Second reconcile: Should clean up the PDB and annotation
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Verify PDB was deleted
	err = r.Get(context.Background(), sandboxKey, pdb)
	require.True(t, k8serrors.IsNotFound(err), "PDB should be deleted after annotation is removed")

	// Verify Pod annotation was removed
	require.NoError(t, r.Get(context.Background(), sandboxKey, pod))
	_, hasAnnotation := pod.Annotations[safeToEvictAnnotation]
	require.False(t, hasAnnotation, "Pod annotation should be removed")
}
