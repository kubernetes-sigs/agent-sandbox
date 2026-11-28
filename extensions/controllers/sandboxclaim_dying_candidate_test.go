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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// A warm-pool member whose Pod is terminating or has ended can still have its
// pod IPs, so getCandidate's not-Ready fallback could hand it to a claim, which
// would then bind to a Pod that is going away. A member without pod IPs, such
// as a Pod that failed before it got one, must not count as a pending-network
// candidate either, or the claim waits out the warm-candidate grace period
// before it cold-starts. The member must be skipped but stay queued: the
// Sandbox recreates an evicted Pod, and with failedPodPolicy Recreate a failed
// one, after which the member is usable again.
func TestGetCandidateSkipsMemberWhosePodIsEnding(t *testing.T) {
	cases := []struct {
		name   string
		podIPs []string
		// omitWarmPool leaves the SandboxWarmPool out of the client, so
		// getCandidate cannot resolve the pool's update strategy.
		omitWarmPool bool
	}{
		{name: "with pod IPs", podIPs: []string{"10.0.0.7"}},
		{name: "without pod IPs"},
		{name: "without pod IPs and warm pool lookup fails", omitWarmPool: true},
	}
	for _, reason := range []string{
		sandboxv1beta1.SandboxReasonPodTerminating,
		sandboxv1beta1.SandboxReasonPodSucceeded,
		sandboxv1beta1.SandboxReasonPodFailed,
		sandboxv1beta1.SandboxReasonExpired,
	} {
		for _, tc := range cases {
			t.Run(reason+"/"+tc.name, func(t *testing.T) {
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
						PodIPs:   tc.podIPs,
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
				objs := []client.Object{template, dying}
				if !tc.omitWarmPool {
					objs = append(objs, warmPool)
				}
				warmSandboxQueue := queue.NewSimpleSandboxQueue()
				namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(key.Namespace, poolName)
				warmSandboxQueue.Add(namespacedWarmPoolName, key)
				reconciler := &SandboxClaimReconciler{
					Client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
					Scheme:           scheme,
					WarmSandboxQueue: warmSandboxQueue,
					Tracer:           asmetrics.NewNoOp(),
				}

				candidate, _, pending, err := reconciler.getCandidate(context.Background(), claim)
				require.NoError(t, err)
				require.Nil(t, candidate, "a member whose Ready reason is %s must not be handed to a claim", reason)
				require.Zero(t, pending, "a member whose Ready reason is %s must not hold the claim as a pending-network candidate", reason)

				requeued, ok := warmSandboxQueue.Get(namespacedWarmPoolName)
				require.True(t, ok, "a member whose Ready reason is %s must stay queued", reason)
				require.Equal(t, key, requeued)
			})
		}
	}
}

// With only an ending member in the pool, the claim must not adopt it and must
// not wait on it as a pending-network candidate, so it cold-starts instead of
// binding to a Pod that is going away. The member keeps its pool ownership.
func TestAdoptSandboxFromCandidatesSkipsOnlyMemberWhosePodIsEnding(t *testing.T) {
	for _, reason := range []string{
		sandboxv1beta1.SandboxReasonPodTerminating,
		sandboxv1beta1.SandboxReasonPodSucceeded,
		sandboxv1beta1.SandboxReasonPodFailed,
		sandboxv1beta1.SandboxReasonExpired,
	} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
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
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, warmPool, claim, dying).Build()
			warmSandboxQueue := queue.NewSimpleSandboxQueue()
			warmSandboxQueue.Add(queue.GetNamespacedWarmPoolName(key.Namespace, poolName), key)
			reconciler := &SandboxClaimReconciler{
				Client:           c,
				Scheme:           scheme,
				WarmSandboxQueue: warmSandboxQueue,
				Tracer:           asmetrics.NewNoOp(),
			}

			adopted, pending, err := reconciler.adoptSandboxFromCandidates(ctx, claim)
			require.NoError(t, err)
			require.Nil(t, adopted, "the claim must not adopt a member whose Ready reason is %s", reason)
			require.Zero(t, pending, "a member whose Ready reason is %s must not hold the claim as a pending-network candidate", reason)

			live := &sandboxv1beta1.Sandbox{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(dying), live))
			require.False(t, metav1.IsControlledBy(live, claim), "the member must keep its pool ownership")
		})
	}
}
