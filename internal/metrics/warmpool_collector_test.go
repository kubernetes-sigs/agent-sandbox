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

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func ownerRefTo(pool *extensionsv1beta1.SandboxWarmPool) metav1.OwnerReference {
	isController := true
	return metav1.OwnerReference{
		APIVersion: extensionsv1beta1.GroupVersion.String(),
		Kind:       "SandboxWarmPool",
		Name:       pool.Name,
		UID:        pool.UID,
		Controller: &isController,
	}
}

func TestWarmPoolCollector(t *testing.T) {
	pool1 := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-1",
			Namespace: "default",
			UID:       "uid-1",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{
				Name: "template-1",
			},
		},
	}

	pool2 := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-2",
			Namespace: "kube-system",
			UID:       "uid-2",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{
				Name: "template-2",
			},
		},
	}

	testCases := []struct {
		name           string
		objects        []runtime.Object
		expectedCount  int
		expectedLabels map[string]int
	}{
		{
			name: "empty pool",
			objects: []runtime.Object{
				pool1,
			},
			expectedCount: 4,
			expectedLabels: map[string]int{
				"namespace:default sandbox_status:failed sandbox_template:template-1 warmpool_name:pool-1":    0,
				"namespace:default sandbox_status:pending sandbox_template:template-1 warmpool_name:pool-1":   0,
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":     0,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1": 0,
			},
		},
		{
			name: "single ready sandbox in one pool",
			objects: []runtime.Object{
				pool1,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-1",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool1)},
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
			expectedCount: 4,
			expectedLabels: map[string]int{
				"namespace:default sandbox_status:failed sandbox_template:template-1 warmpool_name:pool-1":    0,
				"namespace:default sandbox_status:pending sandbox_template:template-1 warmpool_name:pool-1":   0,
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":     1,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1": 0,
			},
		},
		{
			name: "mixed statuses across pools and namespaces",
			objects: []runtime.Object{
				pool1,
				pool2,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-1",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool1)},
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
						Name:      "sb-2",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool1)},
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
						Name:      "sb-3",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool1)},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: nil,
					},
				},
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-4",
						Namespace: "kube-system",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool2)},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionFinished),
								Status: metav1.ConditionTrue,
								Reason: sandboxv1beta1.SandboxReasonPodFailed,
							},
						},
					},
				},
			},
			expectedCount: 8,
			expectedLabels: map[string]int{
				"namespace:default sandbox_status:failed sandbox_template:template-1 warmpool_name:pool-1":        0,
				"namespace:default sandbox_status:pending sandbox_template:template-1 warmpool_name:pool-1":       1,
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":         2,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1":     0,
				"namespace:kube-system sandbox_status:failed sandbox_template:template-2 warmpool_name:pool-2":    1,
				"namespace:kube-system sandbox_status:pending sandbox_template:template-2 warmpool_name:pool-2":   0,
				"namespace:kube-system sandbox_status:ready sandbox_template:template-2 warmpool_name:pool-2":     0,
				"namespace:kube-system sandbox_status:succeeded sandbox_template:template-2 warmpool_name:pool-2": 0,
			},
		},
		{
			name: "terminal-beats-ready edge case",
			objects: []runtime.Object{
				pool1,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-1",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool1)},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(sandboxv1beta1.SandboxConditionFinished),
								Status: metav1.ConditionTrue,
								Reason: sandboxv1beta1.SandboxReasonPodSucceeded,
							},
							{
								Type:   string(sandboxv1beta1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedCount: 4,
			expectedLabels: map[string]int{
				"namespace:default sandbox_status:failed sandbox_template:template-1 warmpool_name:pool-1":    0,
				"namespace:default sandbox_status:pending sandbox_template:template-1 warmpool_name:pool-1":   0,
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":     0,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1": 1,
			},
		},
		{
			name: "no conditions at all",
			objects: []runtime.Object{
				pool1,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-1",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool1)},
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: nil,
					},
				},
			},
			expectedCount: 4,
			expectedLabels: map[string]int{
				"namespace:default sandbox_status:failed sandbox_template:template-1 warmpool_name:pool-1":    0,
				"namespace:default sandbox_status:pending sandbox_template:template-1 warmpool_name:pool-1":   1,
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":     0,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1": 0,
			},
		},
		{
			name: "orphaned sandbox",
			objects: []runtime.Object{
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-1",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(&extensionsv1beta1.SandboxWarmPool{
							ObjectMeta: metav1.ObjectMeta{
								Name: "pool-that-does-not-exist",
							},
						})},
					},
				},
			},
			expectedCount:  0,
			expectedLabels: map[string]int{},
		},
		{
			name: "template annotation override",
			objects: []runtime.Object{
				pool1,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-1",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(pool1)},
						Annotations: map[string]string{
							sandboxv1beta1.SandboxTemplateRefAnnotation: "template-override",
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
			expectedCount: 5,
			expectedLabels: map[string]int{
				"namespace:default sandbox_status:failed sandbox_template:template-1 warmpool_name:pool-1":       0,
				"namespace:default sandbox_status:pending sandbox_template:template-1 warmpool_name:pool-1":      0,
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":        0,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1":    0,
				"namespace:default sandbox_status:ready sandbox_template:template-override warmpool_name:pool-1": 1,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := newFakeClient(tc.objects...).Build()
			collector := NewWarmPoolCollector(fakeClient, logr.Discard())
			reg := prometheus.NewRegistry()
			reg.MustRegister(collector)
			count, err := testutil.GatherAndCount(reg, "agent_sandbox_warmpool_size")
			require.NoError(t, err)
			require.Equal(t, tc.expectedCount, count)

			metrics, err := reg.Gather()
			require.NoError(t, err)
			actualLabels := make(map[string]int)
			for _, mf := range metrics {
				if mf.GetName() == "agent_sandbox_warmpool_size" {
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
