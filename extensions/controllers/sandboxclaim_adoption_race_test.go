// Copyright 2026 The Kubernetes Authors.
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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// makeWarmPoolSandbox creates a warm pool sandbox suitable as an adoption candidate.
// It has the required labels, owner references, and optionally a Ready condition.
func makeWarmPoolSandbox(name string, poolNameHash, templateHash string, warmPoolUID types.UID, ready bool, annotations map[string]string) *sandboxv1alpha1.Sandbox {
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	replicas := int32(1)
	if annotations == nil {
		annotations = map[string]string{}
	}
	return &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
			Annotations:       annotations,
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: templateHash,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
				Kind:       "SandboxWarmPool",
				Name:       "pool",
				UID:        warmPoolUID,
				Controller: ptr.To(true),
			}},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: &replicas,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "pause"}},
				},
			},
		},
		Status: sandboxv1alpha1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:   string(sandboxv1alpha1.SandboxConditionReady),
				Status: conditionStatus,
				Reason: "DependenciesReady",
			}},
		},
	}
}

// TestAdoptedSandboxGetsClaimedByAnnotation verifies that adopting a warm pool
// sandbox writes the claimed-by annotation with the claim's UID.
// Regression test for #127, #478.
func TestAdoptedSandboxGetsClaimedByAnnotation(t *testing.T) {
	scheme := newScheme(t)

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "pause"}},
				},
			},
		},
	}

	poolNameHash := sandboxcontrollers.NameHash("pool")
	templateHash := sandboxcontrollers.NameHash("tpl")
	warmPoolUID := types.UID("pool-uid")

	sandbox := makeWarmPoolSandbox("pool-sb-0", poolNameHash, templateHash, warmPoolUID, true, nil)

	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adopt-claim",
			Namespace: "default",
			UID:       "adopt-claim-uid-123",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, sandbox, claim).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}, &extensionsv1alpha1.SandboxClaim{}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:                  fakeClient,
		Scheme:                  scheme,
		Tracer:                  asmetrics.NewNoOp(),
		MaxConcurrentReconciles: 1,
	}

	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "adopt-claim", Namespace: "default"},
	})
	require.NoError(t, err)

	// Fetch the sandbox after reconciliation
	var updated sandboxv1alpha1.Sandbox
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "pool-sb-0", Namespace: "default",
	}, &updated))

	// Verify the sandbox was adopted (warm pool labels removed, claim is controller)
	_, hasWarmPoolLabel := updated.Labels[warmPoolSandboxLabel]
	require.False(t, hasWarmPoolLabel,
		"warm pool label should be removed after adoption")

	controllerRef := metav1.GetControllerOf(&updated)
	require.NotNil(t, controllerRef,
		"adopted sandbox must have a controller owner reference")
	require.Equal(t, claim.UID, controllerRef.UID,
		"adopted sandbox must be owned by the claim")

	claimedBy, hasClaimed := updated.Annotations[sandboxv1alpha1.SandboxClaimedByAnnotation]
	require.True(t, hasClaimed,
		"adopted sandbox must have the %s annotation — "+
			"the controller should write it during adoption to prevent cross-claim races",
		sandboxv1alpha1.SandboxClaimedByAnnotation)
	require.Equal(t, string(claim.UID), claimedBy,
		"claimed-by annotation must match the adopting claim's UID")
}

// TestSinglePreClaimedSandboxIsSkipped verifies that a sandbox with a
// claimed-by annotation from another claim is skipped; the claim cold-starts.
// Regression test for #127, #478.
func TestSinglePreClaimedSandboxIsSkipped(t *testing.T) {
	scheme := newScheme(t)

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "pause"}},
				},
			},
		},
	}

	poolNameHash := sandboxcontrollers.NameHash("pool")
	templateHash := sandboxcontrollers.NameHash("tpl")
	warmPoolUID := types.UID("pool-uid")

	// Pre-annotated sandbox: claimed by a different UID
	preClaimed := makeWarmPoolSandbox(
		"pre-claimed-sb", poolNameHash, templateHash, warmPoolUID, true,
		map[string]string{sandboxv1alpha1.SandboxClaimedByAnnotation: "some-other-uid"},
	)

	// The claim that owns the reservation must exist in the cluster
	// so the candidate filter treats the annotation as a live reservation.
	otherClaim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-claim",
			Namespace: "default",
			UID:       "some-other-uid",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
		},
	}

	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "skip-claim",
			Namespace: "default",
			UID:       "skip-claim-uid-456",
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, preClaimed, otherClaim, claim).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}, &extensionsv1alpha1.SandboxClaim{}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:                  fakeClient,
		Scheme:                  scheme,
		Tracer:                  asmetrics.NewNoOp(),
		MaxConcurrentReconciles: 1,
	}

	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "skip-claim", Namespace: "default"},
	})
	require.NoError(t, err)

	// Verify the pre-claimed sandbox was NOT adopted.
	var sb sandboxv1alpha1.Sandbox
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "pre-claimed-sb", Namespace: "default",
	}, &sb))

	// Warm pool labels must still be present (not removed by adoption).
	_, hasWarmPoolLabel := sb.Labels[warmPoolSandboxLabel]
	require.True(t, hasWarmPoolLabel,
		"pre-claimed sandbox must retain its warm pool label — it should not have been adopted")

	_, hasTemplateHash := sb.Labels[sandboxTemplateRefHash]
	require.True(t, hasTemplateHash,
		"pre-claimed sandbox must retain its template hash label — it should not have been adopted")

	// Owner reference must still point to SandboxWarmPool, not SandboxClaim.
	controllerRef := metav1.GetControllerOf(&sb)
	require.NotNil(t, controllerRef,
		"pre-claimed sandbox must still have a controller owner")
	require.Equal(t, "SandboxWarmPool", controllerRef.Kind,
		"pre-claimed sandbox owner must still be SandboxWarmPool, not SandboxClaim")

	// The annotation must be unchanged.
	require.Equal(t, "some-other-uid", sb.Annotations[sandboxv1alpha1.SandboxClaimedByAnnotation],
		"pre-claimed sandbox annotation must be untouched")

	// Verify the claim cold-started: a new sandbox with the claim's name exists.
	var coldStartSandbox sandboxv1alpha1.Sandbox
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: "skip-claim", Namespace: "default",
	}, &coldStartSandbox),
		"claim should have cold-started and created a sandbox named after itself")

	coldRef := metav1.GetControllerOf(&coldStartSandbox)
	require.NotNil(t, coldRef, "cold-started sandbox must have a controller owner")
	require.Equal(t, claim.UID, coldRef.UID,
		"cold-started sandbox must be owned by the claim")
}

// TestMultipleClaimsRespectReservations verifies that claims skip sandboxes
// with a claimed-by annotation. 4 of 5 are reserved; none may be adopted.
// Regression test for #127, #478.
func TestMultipleClaimsRespectReservations(t *testing.T) {
	scheme := newScheme(t)

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "pause"}},
				},
			},
		},
	}

	warmPoolUID := types.UID("pool-uid")
	poolNameHash := sandboxcontrollers.NameHash("pool")
	templateHash := sandboxcontrollers.NameHash("tpl")

	objs := []client.Object{template}

	// Create 5 pool sandboxes: 4 reserved, 1 free
	for i := 0; i < 5; i++ {
		replicas := int32(1)
		annotations := map[string]string{}
		if i < 4 {
			annotations[sandboxv1alpha1.SandboxClaimedByAnnotation] = fmt.Sprintf("other-claim-uid-%d", i)
		}
		sb := &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmt.Sprintf("pool-sb-%d", i),
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(i) * time.Second)),
				Annotations:       annotations,
				Labels: map[string]string{
					warmPoolSandboxLabel:   poolNameHash,
					sandboxTemplateRefHash: templateHash,
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
					Kind:       "SandboxWarmPool",
					Name:       "pool",
					UID:        warmPoolUID,
					Controller: ptr.To(true),
				}},
			},
			Spec: sandboxv1alpha1.SandboxSpec{
				Replicas: &replicas,
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "pause"}},
					},
				},
			},
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type:   string(sandboxv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: "DependenciesReady",
				}},
			},
		}
		objs = append(objs, sb)
	}

	// Create stub claims for the reserved UIDs so the candidate
	// filter treats the annotations as live reservations.
	for i := 0; i < 4; i++ {
		stub := &extensionsv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("other-claim-%d", i),
				Namespace: "default",
				UID:       types.UID(fmt.Sprintf("other-claim-uid-%d", i)),
			},
			Spec: extensionsv1alpha1.SandboxClaimSpec{
				TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
			},
		}
		objs = append(objs, stub)
	}

	// Create 3 claims
	for i := 0; i < 3; i++ {
		c := &extensionsv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("claim-%d", i),
				Namespace: "default",
				UID:       types.UID(fmt.Sprintf("test-claim-uid-%d", i)),
			},
			Spec: extensionsv1alpha1.SandboxClaimSpec{
				TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
			},
		}
		objs = append(objs, c)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}, &extensionsv1alpha1.SandboxClaim{}).
		Build()

	reconciler := &SandboxClaimReconciler{
		Client:                  fakeClient,
		Scheme:                  scheme,
		Tracer:                  asmetrics.NewNoOp(),
		MaxConcurrentReconciles: 1,
	}

	// Reconcile each claim sequentially
	for i := 0; i < 3; i++ {
		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      fmt.Sprintf("claim-%d", i),
				Namespace: "default",
			},
		})
		require.NoError(t, err)
	}

	// Check: no claim should have adopted a reserved sandbox (pool-sb-0 through pool-sb-3)
	var sandboxList sandboxv1alpha1.SandboxList
	require.NoError(t, fakeClient.List(context.Background(), &sandboxList))

	for _, sb := range sandboxList.Items {
		ownerRef := metav1.GetControllerOf(&sb)
		if ownerRef == nil || ownerRef.Kind != "SandboxClaim" {
			continue
		}
		// Check if this was a reserved sandbox
		for i := 0; i < 4; i++ {
			if sb.Name == fmt.Sprintf("pool-sb-%d", i) {
				t.Errorf("claim %s adopted reserved sandbox %s (annotation %s=%s) — "+
					"the controller must skip sandboxes with a claimed-by annotation",
					ownerRef.Name, sb.Name, sandboxv1alpha1.SandboxClaimedByAnnotation,
					fmt.Sprintf("other-claim-uid-%d", i))
			}
		}
		t.Logf("claim %s → sandbox %s", ownerRef.Name, sb.Name)
	}
}
