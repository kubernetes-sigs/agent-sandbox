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
	"context"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// terminatingAt returns a deletion timestamp for objects that are being
// deleted; the fake client requires a finalizer alongside it.
func terminatingAt() *metav1.Time {
	now := metav1.Now()
	return &now
}

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

// legacyOwnerRefTo mimics an owner reference written by a pre-v1beta1 pool
// controller, which an in-place upgrade leaves in place.
func legacyOwnerRefTo(pool *extensionsv1beta1.SandboxWarmPool) metav1.OwnerReference {
	ref := ownerRefTo(pool)
	ref.APIVersion = "extensions.agents.x-k8s.io/v1alpha1"
	return ref
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
			// A pool controller from before v1beta1 wrote v1alpha1 owner
			// references; an in-place upgrade keeps them, and the pool must
			// not report as empty because of the stale apiVersion.
			name: "legacy v1alpha1-owned warmpool sandbox",
			objects: []runtime.Object{
				pool1,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-legacy",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{legacyOwnerRefTo(pool1)},
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
			// pool1 is present so the lookup miss (not the no-pools
			// short-circuit) is what skips the sandbox.
			name: "orphaned sandbox",
			objects: []runtime.Object{
				pool1,
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
			expectedCount: 4,
			expectedLabels: map[string]int{
				"namespace:default sandbox_status:failed sandbox_template:template-1 warmpool_name:pool-1":    0,
				"namespace:default sandbox_status:pending sandbox_template:template-1 warmpool_name:pool-1":   0,
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":     0,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1": 0,
			},
		},
		{
			// A pool deleted and recreated under the same name leaves behind
			// sandboxes whose owner UID no longer matches; they belong to the
			// old pool and must not be attributed to the new one.
			name: "stale owner UID from recreated pool",
			objects: []runtime.Object{
				pool1,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sb-stale",
						Namespace: "default",
						Labels: map[string]string{
							sandboxv1beta1.SandboxWarmPoolLabel: "",
						},
						OwnerReferences: []metav1.OwnerReference{ownerRefTo(&extensionsv1beta1.SandboxWarmPool{
							ObjectMeta: metav1.ObjectMeta{
								Name: pool1.Name,
								UID:  "uid-old",
							},
						})},
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
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":     0,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1": 0,
			},
		},
		{
			// The sandbox reconciler leaves conditions untouched once deletion
			// starts, so a terminating sandbox still reads Ready=True.
			name: "terminating sandbox is not counted",
			objects: []runtime.Object{
				pool1,
				&sandboxv1beta1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "sb-terminating",
						Namespace:         "default",
						DeletionTimestamp: terminatingAt(),
						Finalizers:        []string{"test.agents.x-k8s.io/hold"},
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
				"namespace:default sandbox_status:ready sandbox_template:template-1 warmpool_name:pool-1":     0,
				"namespace:default sandbox_status:succeeded sandbox_template:template-1 warmpool_name:pool-1": 0,
			},
		},
		{
			name: "terminating pool is not reported",
			objects: []runtime.Object{
				&extensionsv1beta1.SandboxWarmPool{
					ObjectMeta: metav1.ObjectMeta{
						Name:              pool1.Name,
						Namespace:         pool1.Namespace,
						UID:               pool1.UID,
						DeletionTimestamp: terminatingAt(),
						Finalizers:        []string{"test.agents.x-k8s.io/hold"},
					},
					Spec: pool1.Spec,
				},
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

// TestWarmPoolCollectorSkipsSandboxListWithoutPools pins the short-circuit:
// with no warm pools there is nothing to attribute sandboxes to, so the
// collector must not scan the Sandbox cache on every scrape.
func TestWarmPoolCollectorSkipsSandboxListWithoutPools(t *testing.T) {
	labeledSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sb-1",
			Namespace: "default",
			Labels: map[string]string{
				sandboxv1beta1.SandboxWarmPoolLabel: "",
			},
		},
	}
	// Collect runs on a prometheus goroutine, so the interceptor only records
	// the call; failing from there would hang Gather instead of failing the test.
	var listedSandboxes atomic.Bool
	fakeClient := newFakeClient(labeledSandbox).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*sandboxv1beta1.SandboxList); ok {
					listedSandboxes.Store(true)
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewWarmPoolCollector(fakeClient, logr.Discard()))
	count, err := testutil.GatherAndCount(reg, "agent_sandbox_warmpool_size")
	require.NoError(t, err)
	require.Equal(t, 0, count)
	require.False(t, listedSandboxes.Load(), "sandboxes were listed although no warm pools exist")
}
