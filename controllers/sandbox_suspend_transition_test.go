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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// TestReconcileRunningToSuspendedKeepsServiceAndBumpsObservedGeneration covers
// a Ready, Running Sandbox with a Service flipped to Suspended at generation 2.
// The first pass deletes the Pod (Suspended=False, PodTerminating); the second
// observes it gone (Suspended=True, PodTerminated). Both passes keep the
// Service and its status fields.
func TestReconcileRunningToSuspendedKeepsServiceAndBumpsObservedGeneration(t *testing.T) {
	tests := []struct {
		name            string
		passes          int
		suspendedStatus metav1.ConditionStatus
		suspendedReason string
		readyMessage    string
	}{
		{
			name:            "one reconcile: pod terminating",
			passes:          1,
			suspendedStatus: metav1.ConditionFalse,
			suspendedReason: sandboxv1beta1.SandboxReasonSuspendedPodTerminating,
			readyMessage:    "Sandbox is suspending",
		},
		{
			name:            "two reconciles: pod terminated",
			passes:          2,
			suspendedStatus: metav1.ConditionTrue,
			suspendedReason: sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
			readyMessage:    "Sandbox is suspended",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "suspend-transition",
					Namespace:  "default",
					UID:        sandboxUID,
					Generation: 1,
				},
				Spec: sandboxv1beta1.SandboxSpec{
					SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
						PodTemplate: sandboxv1beta1.PodTemplate{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test-container"}},
							},
						},
						Service: new(true),
					},
					OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			}

			r := &SandboxReconciler{
				Client:        newFakeClient(sandbox),
				Scheme:        Scheme,
				Tracer:        asmetrics.NewNoOp(),
				ClusterDomain: "cluster.local",
			}
			ctx := t.Context()
			key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
			req := ctrl.Request{NamespacedName: key}

			// Gen 1: create Pod and Service, then mark the Pod Ready.
			_, err := r.Reconcile(ctx, req)
			require.NoError(t, err)

			pod := &corev1.Pod{}
			require.NoError(t, r.Get(ctx, key, pod))
			pod.Status = corev1.PodStatus{
				Phase:  corev1.PodRunning,
				PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			}
			require.NoError(t, r.Status().Update(ctx, pod))

			_, err = r.Reconcile(ctx, req)
			require.NoError(t, err)

			running := &sandboxv1beta1.Sandbox{}
			require.NoError(t, r.Get(ctx, key, running))
			ready := meta.FindStatusCondition(running.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
			require.NotNil(t, ready)
			require.Equal(t, metav1.ConditionTrue, ready.Status)
			wantService := sandbox.Name
			wantFQDN := sandbox.Name + ".default.svc.cluster.local"
			require.Equal(t, wantService, running.Status.Service)
			require.Equal(t, wantFQDN, running.Status.ServiceFQDN)

			// Gen 2: flip to Suspended. The fake client does not bump generation on
			// update, so set it by hand.
			running.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
			running.Generation = 2
			require.NoError(t, r.Update(ctx, running))

			for range tc.passes {
				_, err = r.Reconcile(ctx, req)
				require.NoError(t, err)
			}

			err = r.Get(ctx, key, &corev1.Pod{})
			require.True(t, k8serrors.IsNotFound(err), "pod should be deleted, got err=%v", err)
			require.NoError(t, r.Get(ctx, key, &corev1.Service{}), "service should be retained")

			got := &sandboxv1beta1.Sandbox{}
			require.NoError(t, r.Get(ctx, key, got))

			suspended := meta.FindStatusCondition(got.Status.Conditions, string(sandboxv1beta1.SandboxConditionSuspended))
			require.NotNil(t, suspended)
			require.Equal(t, tc.suspendedStatus, suspended.Status)
			require.Equal(t, tc.suspendedReason, suspended.Reason)
			require.Equal(t, int64(2), suspended.ObservedGeneration)

			ready = meta.FindStatusCondition(got.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
			require.NotNil(t, ready)
			require.Equal(t, metav1.ConditionFalse, ready.Status)
			require.Equal(t, sandboxv1beta1.SandboxReasonSuspended, ready.Reason)
			require.Equal(t, tc.readyMessage, ready.Message)
			require.Equal(t, int64(2), ready.ObservedGeneration)

			require.Equal(t, wantService, got.Status.Service)
			require.Equal(t, wantFQDN, got.Status.ServiceFQDN)
		})
	}
}
