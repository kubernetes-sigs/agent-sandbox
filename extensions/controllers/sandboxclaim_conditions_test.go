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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func TestSandboxClaimReadyConditionUsesClaimGeneration(t *testing.T) {
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
	}
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "DependenciesReady",
				Message:            "Pod is Ready",
				ObservedGeneration: 2,
			}},
		},
	}

	want := sandbox.Status.Conditions[0]
	want.ObservedGeneration = claim.Generation
	got := (&SandboxClaimReconciler{}).computeReadyCondition(claim, sandbox, nil, false)

	if got != want {
		t.Errorf("computeReadyCondition() = %+v, want %+v", got, want)
	}
}

func TestSandboxClaimFinishedConditionUsesClaimGeneration(t *testing.T) {
	finishedAt := metav1.NewTime(time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC))

	testCases := []struct {
		name              string
		claimFinished     *metav1.Condition
		sandboxGeneration int64
		sandboxFinishedAt metav1.Time
		wantFinishedAt    metav1.Time
	}{
		{
			name:              "first mirror",
			sandboxGeneration: 2,
			sandboxFinishedAt: finishedAt,
			wantFinishedAt:    finishedAt,
		},
		{
			// The claim keeps its own LastTransitionTime, which
			// ttlSecondsAfterFinished counts from, while the generation moves.
			name: "already mirrored",
			claimFinished: &metav1.Condition{
				Type:               string(sandboxv1beta1.SandboxConditionFinished),
				Status:             metav1.ConditionTrue,
				Reason:             sandboxv1beta1.SandboxReasonPodSucceeded,
				Message:            "Pod completed successfully",
				ObservedGeneration: 2,
				LastTransitionTime: finishedAt,
			},
			sandboxGeneration: 3,
			sandboxFinishedAt: metav1.NewTime(finishedAt.Add(time.Minute)),
			wantFinishedAt:    finishedAt,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			claim := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Generation: 4},
			}
			if tc.claimFinished != nil {
				claim.Status.Conditions = []metav1.Condition{*tc.claimFinished}
			}
			sandbox := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Generation: tc.sandboxGeneration},
				Status: sandboxv1beta1.SandboxStatus{
					Conditions: []metav1.Condition{{
						Type:               string(sandboxv1beta1.SandboxConditionFinished),
						Status:             metav1.ConditionTrue,
						Reason:             sandboxv1beta1.SandboxReasonPodSucceeded,
						Message:            "Pod completed successfully",
						ObservedGeneration: tc.sandboxGeneration,
						LastTransitionTime: tc.sandboxFinishedAt,
					}},
				},
			}

			want := sandbox.Status.Conditions[0]
			want.ObservedGeneration = claim.Generation
			want.LastTransitionTime = tc.wantFinishedAt
			(&SandboxClaimReconciler{}).syncFinishedCondition(claim, sandbox, false)

			got := meta.FindStatusCondition(claim.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
			if got == nil || *got != want {
				t.Errorf("claim Finished condition = %+v, want %+v", got, want)
			}
			if gotGeneration := sandbox.Status.Conditions[0].ObservedGeneration; gotGeneration != tc.sandboxGeneration {
				t.Errorf("sandbox Finished ObservedGeneration = %d, want %d unchanged", gotGeneration, tc.sandboxGeneration)
			}
		})
	}
}
