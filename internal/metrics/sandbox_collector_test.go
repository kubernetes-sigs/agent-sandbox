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

// nolint:revive
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
)

func newFakeClient(objects ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...)
}

func TestSandboxCollector(t *testing.T) {
	testCases := []struct {
		name           string
		sandboxes      []runtime.Object
		expectedCount  int
		expectedLabels map[string]int
	}{
		{
			name: "single ready cold unknown sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-1",
						Namespace: "default",
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"claimed:false expired:false launch_type:cold namespace:default ready_condition:true sandbox_template:unknown": 1,
			},
		},
		{
			name: "missing ready condition",
			sandboxes: []runtime.Object{
				&sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-missing",
						Namespace: "default",
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: nil,
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"claimed:false expired:false launch_type:cold namespace:default ready_condition:false sandbox_template:unknown": 1,
			},
		},
		{
			name: "mixed sandboxes",
			sandboxes: []runtime.Object{
				&sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-1",
						Namespace: "default",
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
				&sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-2",
						Namespace: "test-ns",
						Annotations: map[string]string{
							sandboxv1alpha1.SandboxPodNameAnnotation:     "adopted-pod",
							sandboxv1alpha1.SandboxTemplateRefAnnotation: "my-template",
						},
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
								Reason: sandboxv1alpha1.SandboxReasonExpired,
							},
						},
					},
				},
				&sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-3",
						Namespace: "default",
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
							},
						},
					},
				},
				&sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-4",
						Namespace: "default",
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
							},
						},
					},
				},
			},
			expectedCount: 3, // We expect 3 distinct metric series for the 4 sandboxes
			expectedLabels: map[string]int{
				"claimed:false expired:false launch_type:cold namespace:default ready_condition:true sandbox_template:unknown":     1,
				"claimed:false expired:true launch_type:warm namespace:test-ns ready_condition:false sandbox_template:my-template": 1,
				"claimed:false expired:false launch_type:cold namespace:default ready_condition:false sandbox_template:unknown":    2,
			},
		},
		{
			name: "claimed sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-claimed",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "agents.x-k8s.io/v1alpha1",
								Kind:       "SandboxClaim",
								Name:       "my-claim",
								UID:        "1234",
								Controller: new(true),
							},
						},
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"claimed:true expired:false launch_type:cold namespace:default ready_condition:true sandbox_template:unknown": 1,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := newFakeClient(tc.sandboxes...).Build()
			collector := NewSandboxCollector(fakeClient, logr.Discard())
			registry := prometheus.NewRegistry()
			registry.MustRegister(collector)
			count, err := testutil.GatherAndCount(registry, "agent_sandboxes")
			require.NoError(t, err)
			require.Equal(t, tc.expectedCount, count)
			metrics, err := registry.Gather()
			require.NoError(t, err)
			actualLabels := make(map[string]int)
			for _, mf := range metrics {
				if mf.GetName() == "agent_sandboxes" {
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
