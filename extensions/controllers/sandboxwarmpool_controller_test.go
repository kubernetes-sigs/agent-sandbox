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

package extensioncontrollers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func createSandbox(name, namespace, poolLabelValue string, ready bool) *sandboxv1alpha1.Sandbox {
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{poolLabel: poolLabelValue},
		},
	}
	if ready {
		sb.Status.Conditions = []metav1.Condition{
			{
				Type:   string(sandboxv1alpha1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
			},
		}
	}
	return sb
}

func TestReconcilePool(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	replicas := int32(3)

	warmPool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			Replicas: replicas,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			},
		},
	}

	poolLabelValue := (&SandboxWarmPoolReconciler{}).getPoolLabelValue(warmPool)

	createPoolSandbox := func(name string, ready bool) *sandboxv1alpha1.Sandbox {
		return createSandbox(name, poolNamespace, poolLabelValue, ready)
	}

	testCases := []struct {
		name             string
		initialObjs      []runtime.Object
		expectedReplicas int32
		expectedReady    int32
	}{
		{
			name:             "creates sandboxes when pool is empty",
			initialObjs:      []runtime.Object{},
			expectedReplicas: replicas,
			expectedReady:    0,
		},
		{
			name: "creates additional sandboxes when under-provisioned",
			initialObjs: []runtime.Object{
				createPoolSandbox("test-pool-abc", false),
			},
			expectedReplicas: replicas,
			expectedReady:    0,
		},
		{
			name: "deletes excess sandboxes when over-provisioned",
			initialObjs: []runtime.Object{
				createPoolSandbox("test-pool-abc", false),
				createPoolSandbox("test-pool-def", false),
				createPoolSandbox("test-pool-ghi", false),
				createPoolSandbox("test-pool-jkl", false),
			},
			expectedReplicas: replicas,
			expectedReady:    0,
		},
		{
			name: "counts ready replicas correctly",
			initialObjs: []runtime.Object{
				createPoolSandbox("test-pool-abc", true),
				createPoolSandbox("test-pool-def", false),
				createPoolSandbox("test-pool-ghi", true),
			},
			expectedReplicas: replicas,
			expectedReady:    2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(controllers.Scheme).
					WithRuntimeObjects(tc.initialObjs...).
					Build(),
			}

			ctx := context.Background()

			// Run reconcilePool twice: first to create/delete, second to update status
			err := r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			err = r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			// Verify final state
			list := &sandboxv1alpha1.SandboxList{}
			err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
			require.NoError(t, err)

			// Count sandboxes with correct pool label
			count := int32(0)
			for _, sb := range list.Items {
				if sb.Labels[poolLabel] == poolLabelValue {
					count++
				}
			}

			require.Equal(t, tc.expectedReplicas, count)
			require.Equal(t, tc.expectedReplicas, warmPool.Status.Replicas)
			require.Equal(t, tc.expectedReady, warmPool.Status.ReadyReplicas)
		})
	}
}
