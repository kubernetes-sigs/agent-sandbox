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

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

func TestComputeReadyCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &SandboxReconciler{}

	testCases := []struct {
		name           string
		generation     int64
		err            error
		svc            *corev1.Service
		pod            *corev1.Pod
		expectedStatus metav1.ConditionStatus
		expectedReason string
	}{
		{
			name:       "all ready",
			generation: 1,
			err:        nil,
			svc:        &corev1.Service{},
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
			name:           "error",
			generation:     1,
			err:            errors.New("test error"),
			svc:            &corev1.Service{},
			pod:            &corev1.Pod{},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "ReconcilerError",
		},
		{
			name:       "pod not ready",
			generation: 1,
			err:        nil,
			svc:        &corev1.Service{},
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
			name:       "pod running but not ready",
			generation: 1,
			err:        nil,
			svc:        &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name:       "pod pending",
			generation: 1,
			err:        nil,
			svc:        &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name:       "service not ready",
			generation: 1,
			err:        nil,
			svc:        nil,
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
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name:           "all not ready",
			generation:     1,
			err:            nil,
			svc:            nil,
			pod:            nil,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Generation: tc.generation},
			}
			condition := r.computeReadyCondition(sandbox, tc.err, tc.svc, tc.pod)
			g.Expect(condition.Type).To(Equal(string(sandboxv1alpha1.SandboxConditionReady)))
			g.Expect(condition.ObservedGeneration).To(Equal(tc.generation))
			g.Expect(condition.Status).To(Equal(tc.expectedStatus))
			g.Expect(condition.Reason).To(Equal(tc.expectedReason))
		})
	}
}
