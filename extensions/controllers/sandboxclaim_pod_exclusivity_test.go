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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// TestWarmPoolPodExclusivity is a regression test for the 1:1 sandbox-to-pod
// invariant (#127). When more claims exist than warm pool sandboxes, each
// sandbox must be adopted by at most one claim.
func TestWarmPoolPodExclusivity(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	poolNameHash := sandboxcontrollers.NameHash("pool")
	templateHash := sandboxcontrollers.NameHash("tpl")
	warmPoolUID := types.UID("pool-uid")

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img"}},
				},
			},
		},
	}

	createPoolSandbox := func(name string) *sandboxv1alpha1.Sandbox {
		return &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
				Labels: map[string]string{
					warmPoolSandboxLabel:   poolNameHash,
					sandboxTemplateRefHash: templateHash,
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
					Kind:       "SandboxWarmPool",
					Name:       "pool",
					UID:        warmPoolUID,
					Controller: new(true),
				}},
			},
			Spec: sandboxv1alpha1.SandboxSpec{
				Replicas: new(int32(1)),
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type:               string(sandboxv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					Reason:             "DependenciesReady",
					LastTransitionTime: metav1.NewTime(time.Now()),
				}},
				PodIPs: []string{"10.244.0.5"},
			},
		}
	}

	// 2 warm pool sandboxes, 3 claims — at least 1 claim must cold-start
	poolSb0 := createPoolSandbox("pool-sb-0")
	poolSb1 := createPoolSandbox("pool-sb-1")

	claims := make([]*extensionsv1alpha1.SandboxClaim, 3)
	for i := range claims {
		claims[i] = &extensionsv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("claim-%d", i),
				Namespace: "default",
				UID:       types.UID(fmt.Sprintf("claim-%d-uid", i)),
			},
			Spec: extensionsv1alpha1.SandboxClaimSpec{
				TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "tpl"},
			},
		}
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, poolSb0, poolSb1).
		WithStatusSubresource(&extensionsv1alpha1.SandboxClaim{})
	for _, cl := range claims {
		builder = builder.WithObjects(cl)
	}
	fc := builder.Build()

	testQueue := queue.NewSimpleSandboxQueue()
	testQueue.Add(templateHash, queue.SandboxKey{Namespace: "default", Name: "pool-sb-0"})
	testQueue.Add(templateHash, queue.SandboxKey{Namespace: "default", Name: "pool-sb-1"})

	reconciler := &SandboxClaimReconciler{
		Client: fc, Scheme: scheme,
		WarmSandboxQueue: testQueue,
		Recorder:         events.NewFakeRecorder(10),
		Tracer:           asmetrics.NewNoOp(), MaxConcurrentReconciles: 1,
	}

	// Reconcile all 3 claims sequentially
	for _, cl := range claims {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: cl.Name, Namespace: "default"},
		})
		require.NoError(t, err, "reconcile %s", cl.Name)
	}

	// Collect all sandboxes and build sandbox → []owning claims
	var allSandboxes sandboxv1alpha1.SandboxList
	require.NoError(t, fc.List(ctx, &allSandboxes, client.InNamespace("default")))

	sandboxToOwners := make(map[string][]string) // sandbox name → [claim names]
	for _, sb := range allSandboxes.Items {
		ref := metav1.GetControllerOf(&sb)
		if ref != nil && ref.Kind == "SandboxClaim" {
			sandboxToOwners[sb.Name] = append(sandboxToOwners[sb.Name], ref.Name)
		}
	}

	// Each sandbox must be owned by at most one claim
	warmPoolNames := map[string]bool{"pool-sb-0": true, "pool-sb-1": true}
	warmAdoptions := 0
	for sbName, owners := range sandboxToOwners {
		require.NotEmpty(t, owners,
			"sandbox %s has no claim owner — warm pool sandboxes should be adopted",
			sbName)
		if warmPoolNames[sbName] {
			warmAdoptions++
		}
	}

	// Each claim must own exactly one sandbox
	claimToSandbox := make(map[string][]string) // claim name → [sandbox names]
	for sbName, owners := range sandboxToOwners {
		for _, owner := range owners {
			claimToSandbox[owner] = append(claimToSandbox[owner], sbName)
		}
	}
	for _, cl := range claims {
		owned := claimToSandbox[cl.Name]
		require.Len(t, owned, 1,
			"claim %s owns %v — each claim must own exactly one sandbox",
			cl.Name, owned)
	}

	t.Logf("Sandbox assignments: %v", sandboxToOwners)

	require.Equal(t, 2, warmAdoptions, "both warm pool sandboxes should be adopted")
}
