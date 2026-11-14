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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// A warm-pool member whose Pod is terminating or has ended still has its
// pod IPs, so getCandidate's not-Ready fallback could hand it to a claim, which
// would then bind to a Pod that is going away. The member must be skipped but
// stay queued: the Sandbox recreates an evicted Pod, and with failedPodPolicy
// Recreate a failed one, after which the member is usable again.
func TestGetCandidateSkipsMemberWhosePodIsEnding(t *testing.T) {
	for _, reason := range []string{
		sandboxv1beta1.SandboxReasonPodTerminating,
		sandboxv1beta1.SandboxReasonPodSucceeded,
		sandboxv1beta1.SandboxReasonPodFailed,
	} {
		t.Run(reason, func(t *testing.T) {
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
			key := queue.SandboxKey{Namespace: "default", Name: "dying-sb", NodeName: "node-a"}
			dying := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      key.Name,
					Namespace: key.Namespace,
					Labels: map[string]string{
						warmPoolSandboxLabel:   sandboxcontrollers.NameHash(poolName),
						sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
					},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: extensionsv1beta1.GroupVersion.String(),
						Kind:       extensionsv1beta1.SandboxWarmPoolKind,
						Name:       poolName,
						UID:        types.UID("pool-uid"),
						Controller: new(true),
					}},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: template.Spec.PodTemplate,
				}},
				Status: sandboxv1beta1.SandboxStatus{
					NodeName: "node-a",
					PodIPs:   []string{"10.0.0.7"},
					Conditions: []metav1.Condition{{
						Type:   string(sandboxv1beta1.SandboxConditionReady),
						Status: metav1.ConditionFalse,
						Reason: reason,
					}},
				},
			}
			claim := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: key.Namespace, UID: types.UID("claim-uid")},
				Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: poolName}},
			}
			warmSandboxQueue := queue.NewSimpleSandboxQueue()
			namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(key.Namespace, poolName)
			warmSandboxQueue.Add(namespacedWarmPoolName, key)
			reconciler := &SandboxClaimReconciler{
				Client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, warmPool, dying).Build(),
				Scheme:           scheme,
				WarmSandboxQueue: warmSandboxQueue,
				Tracer:           asmetrics.NewNoOp(),
			}

			candidate, _, _, err := reconciler.getCandidate(context.Background(), claim)
			require.NoError(t, err)
			require.Nil(t, candidate, "a member whose Pod is %s must not be handed to a claim", reason)

			requeued, ok := warmSandboxQueue.Get(namespacedWarmPoolName)
			require.True(t, ok, "a member whose Pod is %s must stay queued", reason)
			require.Equal(t, key, requeued)
		})
	}
}
