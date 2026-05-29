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

package metrics

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

func newFakeClientWithClaims(objects ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)
	_ = extensionsv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...)
}

func TestSandboxClaimCollector(t *testing.T) {
	testCases := []struct {
		name           string
		claims         []runtime.Object
		expectedCount  int
		expectedLabels map[string]int
	}{
		{
			name: "single ready claim",
			claims: []runtime.Object{
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-1",
						Namespace: "default",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "my-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
								Reason: "SandboxReady",
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"namespace:default ready_condition:true reason:SandboxReady sandbox_template:my-template": 1,
			},
		},
		{
			name: "pending claim missing sandbox",
			claims: []runtime.Object{
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-pending",
						Namespace: "default",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "my-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
								Reason: "SandboxMissing",
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"namespace:default ready_condition:false reason:SandboxMissing sandbox_template:my-template": 1,
			},
		},
		{
			name: "failed claim template not found",
			claims: []runtime.Object{
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-failed",
						Namespace: "default",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "missing-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
								Reason: "TemplateNotFound",
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"namespace:default ready_condition:false reason:TemplateNotFound sandbox_template:missing-template": 1,
			},
		},
		{
			name: "expired claim",
			claims: []runtime.Object{
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-expired",
						Namespace: "default",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "my-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
								Reason: "ClaimExpired",
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"namespace:default ready_condition:false reason:ClaimExpired sandbox_template:my-template": 1,
			},
		},
		{
			name: "claim without conditions",
			claims: []runtime.Object{
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-no-cond",
						Namespace: "default",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "my-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: nil,
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"namespace:default ready_condition:false reason:Unknown sandbox_template:my-template": 1,
			},
		},
		{
			name: "mixed claims",
			claims: []runtime.Object{
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-1",
						Namespace: "default",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "my-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
								Reason: "SandboxReady",
							},
						},
					},
				},
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-2",
						Namespace: "default",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "my-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
								Reason: "SandboxReady",
							},
						},
					},
				},
				&extensionsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim-3",
						Namespace: "test-ns",
					},
					Spec: extensionsv1alpha1.SandboxClaimSpec{
						TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
							Name: "other-template",
						},
					},
					Status: extensionsv1alpha1.SandboxClaimStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
								Reason: "SandboxMissing",
							},
						},
					},
				},
			},
			expectedCount: 2, // 2 series: (default, true, SandboxReady, my-template) and (test-ns, false, SandboxMissing, other-template)
			expectedLabels: map[string]int{
				"namespace:default ready_condition:true reason:SandboxReady sandbox_template:my-template":       2,
				"namespace:test-ns ready_condition:false reason:SandboxMissing sandbox_template:other-template": 1,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := newFakeClientWithClaims(tc.claims...).Build()
			collector := NewSandboxClaimCollector(fakeClient, logr.Discard())
			registry := prometheus.NewRegistry()
			registry.MustRegister(collector)
			count, err := testutil.GatherAndCount(registry, "agent_sandbox_claims")
			require.NoError(t, err)
			require.Equal(t, tc.expectedCount, count)
			metrics, err := registry.Gather()
			require.NoError(t, err)
			actualLabels := make(map[string]int)
			for _, mf := range metrics {
				if mf.GetName() == "agent_sandbox_claims" {
					for _, m := range mf.GetMetric() {
						labelStr := ""
						for _, l := range m.GetLabel() {
							labelStr += l.GetName() + ":" + l.GetValue() + " "
						}
						// Trim trailing space
						if len(labelStr) > 0 {
							labelStr = labelStr[:len(labelStr)-1]
						}
						actualLabels[labelStr] = int(m.GetGauge().GetValue())
					}
				}
			}
			require.Equal(t, tc.expectedLabels, actualLabels)
		})
	}
}
