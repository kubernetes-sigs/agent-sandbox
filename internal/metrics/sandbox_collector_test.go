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

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func newFakeClient(objects ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = sandboxv1beta1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...)
}

func TestSandboxCollector(t *testing.T) {
	trueVal := true
	testCases := []struct {
		name           string
		sandboxes      []runtime.Object
		expectedCount  int
		expectedLabels map[string]int
	}{
		{
			name: "single ready cold unknown sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-1",
						Namespace: "default",
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:None ready_condition:true sandbox_template:unknown": 1,
			},
		},
		{
			name: "missing ready condition",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-missing",
						Namespace: "default",
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: nil,
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:None ready_condition:false sandbox_template:unknown": 1,
			},
		},
		{
			name: "cold launch label with pod name annotation remains cold",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-cold",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeCold,
						},
						Annotations: map[string]string{
							sandboxv1beta1.SandboxPodNameAnnotation: "sandbox-cold",
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:None ready_condition:false sandbox_template:unknown": 1,
			},
		},
		{
			name: "warm launch label reports warm",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-warm",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:warm namespace:default owned_by:None ready_condition:false sandbox_template:unknown": 1,
			},
		},
		{
			name: "mixed sandboxes",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-1",
						Namespace: "default",
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-2",
						Namespace: "test-ns",
						Labels: map[string]string{
							sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
						},
						Annotations: map[string]string{
							sandboxv1beta1.SandboxPodNameAnnotation:     "adopted-pod",
							sandboxv1beta1.SandboxTemplateRefAnnotation: "my-template",
						},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
								Reason: sandboxv1beta1.SandboxReasonExpired,
							},
						},
					},
				},
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-3",
						Namespace: "default",
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
							},
						},
					},
				},
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-4",
						Namespace: "default",
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
							},
						},
					},
				},
			},
			expectedCount: 3, // We expect 3 distinct metric series for the 4 sandboxes
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:None ready_condition:true sandbox_template:unknown":     1,
				"created_by:unknown expired:true launch_type:warm namespace:test-ns owned_by:None ready_condition:false sandbox_template:my-template": 1,
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:None ready_condition:false sandbox_template:unknown":    2,
			},
		},
		{
			name: "claimed sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-claimed",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: extensionsv1beta1.GroupVersion.String(),
								Kind:       "SandboxClaim",
								Name:       "my-claim",
								UID:        "1234",
								Controller: &trueVal,
							},
						},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:SandboxClaim ready_condition:true sandbox_template:unknown": 1,
			},
		},
		{
			name: "warmpool sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-warmpool",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: extensionsv1beta1.GroupVersion.String(),
								Kind:       "SandboxWarmPool",
								Name:       "my-warmpool",
								UID:        "5678",
								Controller: &trueVal,
							},
						},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:SandboxWarmPool ready_condition:true sandbox_template:unknown": 1,
			},
		},
		{
			// Owner references written by a pre-v1beta1 pool controller keep
			// the v1alpha1 apiVersion after an in-place upgrade; the owned_by
			// label must still attribute the sandbox to its warm pool.
			name: "legacy v1alpha1-owned warmpool sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-warmpool-legacy",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
								Kind:       "SandboxWarmPool",
								Name:       "my-warmpool",
								UID:        "9012",
								Controller: &trueVal,
							},
						},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:SandboxWarmPool ready_condition:true sandbox_template:unknown": 1,
			},
		},
		{
			name: "client-created sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-client",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.CreatedByLabel: "go-client",
						},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:go-client expired:false launch_type:cold namespace:default owned_by:None ready_condition:true sandbox_template:unknown": 1,
			},
		},
		{
			name: "python client created sandbox",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-python-client",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.CreatedByLabel: "python-client",
						},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:python-client expired:false launch_type:cold namespace:default owned_by:None ready_condition:true sandbox_template:unknown": 1,
			},
		},
		{
			name: "untrusted created_by label normalized to unknown",
			sandboxes: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-untrusted",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.CreatedByLabel: "hacker-client",
						},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedLabels: map[string]int{
				"created_by:unknown expired:false launch_type:cold namespace:default owned_by:None ready_condition:true sandbox_template:unknown": 1,
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
