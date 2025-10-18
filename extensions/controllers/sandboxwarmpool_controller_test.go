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

func createPod(name, namespace, poolLabelValue string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{poolLabel: poolLabelValue},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "test-image",
				},
			},
		},
	}
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

	poolLabelValue := warmPool.Name

	createPoolPod := func(name string) *corev1.Pod {
		return createPod(name, poolNamespace, poolLabelValue)
	}

	testCases := []struct {
		name             string
		initialObjs      []runtime.Object
		expectedReplicas int32
	}{
		{
			name:             "creates pods when pool is empty",
			initialObjs:      []runtime.Object{},
			expectedReplicas: replicas,
		},
		{
			name: "creates additional pods when under-provisioned",
			initialObjs: []runtime.Object{
				createPoolPod("test-pool-pod-0"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "deletes excess pods when over-provisioned",
			initialObjs: []runtime.Object{
				createPoolPod("test-pool-pod-0"),
				createPoolPod("test-pool-pod-1"),
				createPoolPod("test-pool-pod-2"),
				createPoolPod("test-pool-pod-3"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "maintains correct replica count",
			initialObjs: []runtime.Object{
				createPoolPod("test-pool-pod-0"),
				createPoolPod("test-pool-pod-1"),
				createPoolPod("test-pool-pod-2"),
			},
			expectedReplicas: replicas,
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
			list := &corev1.PodList{}
			err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
			require.NoError(t, err)

			// Count pods with correct pool label
			count := int32(0)
			for _, pod := range list.Items {
				if pod.Labels[poolLabel] == poolLabelValue {
					count++
				}
			}

			require.Equal(t, tc.expectedReplicas, count)
			require.Equal(t, tc.expectedReplicas, warmPool.Status.Replicas)
		})
	}
}
