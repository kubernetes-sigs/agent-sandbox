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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestComputeReadyCondition(t *testing.T) {
	r := &SandboxReconciler{}

	testCases := []struct {
		name           string
		sandbox        *sandboxv1alpha1.Sandbox
		err            error
		svc            *corev1.Service
		pod            *corev1.Pod
		expectedStatus metav1.ConditionStatus
		expectedReason string
	}{
		{
			name: "all ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expectedStatus: metav1.ConditionTrue,
			expectedReason: "DependenciesReady",
		},
		{
			name: "error",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err:            errors.New("test error"),
			svc:            &corev1.Service{},
			pod:            &corev1.Pod{},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "ReconcilerError",
		},
		{
			name: "pod not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "pod running but not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "pod pending",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "service not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: nil,
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "all not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err:            nil,
			svc:            nil,
			pod:            nil,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			condition := r.computeReadyCondition(tc.sandbox, tc.err, tc.svc, tc.pod)
			require.Equal(t, sandboxv1alpha1.SandboxConditionReady.String(), condition.Type)
			require.Equal(t, tc.sandbox.Generation, condition.ObservedGeneration)
			require.Equal(t, tc.expectedStatus, condition.Status)
			require.Equal(t, tc.expectedReason, condition.Reason)
		})
	}
}

func TestReconcilePod(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	nameHash := "name-hash"
	sandboxObj := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						"custom-label": "label-val",
					},
					Annotations: map[string]string{
						"custom-annotation": "anno-val",
					},
				},
			},
		},
	}
	testCases := []struct {
		name        string
		initialObjs []runtime.Object
		sandbox     *sandboxv1alpha1.Sandbox
		wantPod     *corev1.Pod
		expectErr   bool
	}{
		{
			name: "no-op if Pod already exists",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "foo",
							},
						},
					},
				},
			},
			sandbox: sandboxObj,
			wantPod: &corev1.Pod{ // Pod is not updated
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "foo",
						},
					},
				},
			},
		},
		{
			name:    "reconcilePod creates a new Pod",
			sandbox: sandboxObj,
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
						"custom-label":                      "label-val",
					},
					Annotations: map[string]string{
						"custom-annotation": "anno-val",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "agents.x-k8s.io/v1alpha1",
							Kind:               "Sandbox",
							Name:               sandboxName,
							Controller:         ptr.To(true),
							BlockOwnerDeletion: ptr.To(true),
						},
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
			},
		},
		{
			name: "delete pod if RunMode is Suspended",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					RunMode: sandboxv1alpha1.RunModeSuspended,
				},
			},
			wantPod: nil,
		},
		{
			name: "no-op if RunMode is Suspended and pod does not exist",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					RunMode: sandboxv1alpha1.RunModeSuspended,
				},
			},
			wantPod: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxReconciler{
				Client: fake.NewClientBuilder().WithScheme(Scheme).WithRuntimeObjects(tc.initialObjs...).Build(),
				Scheme: Scheme,
			}

			pod, err := r.reconcilePod(t.Context(), tc.sandbox, nameHash)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantPod, pod)
			// Validate the Pod from the "cluster" (fake client)
			if tc.wantPod != nil {
				livePod := &corev1.Pod{}
				err = r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, livePod)
				require.NoError(t, err)
				require.Equal(t, tc.wantPod, livePod)
			} else {
				livePod := &corev1.Pod{}
				err = r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, livePod)
				require.True(t, k8serrors.IsNotFound(err))
			}
		})
	}
}
