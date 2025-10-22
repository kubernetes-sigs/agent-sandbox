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
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Create a test scheme with extensions types registered
func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(scheme))
	utilruntime.Must(extensionsv1alpha1.AddToScheme(scheme))
	return scheme
}

func createPod(name, namespace, poolNameHash string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{poolLabel: poolNameHash},
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

func createPoolPod(poolName, namespace, poolNameHash, suffix string) *corev1.Pod {
	name := poolName + suffix
	return createPod(name, namespace, poolNameHash)
}

func createTemplate(name, namespace string) *extensionsv1alpha1.SandboxTemplate {
	return &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
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
}

func TestReconcilePool(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(3)

	// Create a SandboxTemplate
	template := createTemplate(templateName, poolNamespace)

	warmPool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			Replicas: replicas,
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: templateName,
			},
		},
	}

	// Compute the pool name hash
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	testCases := []struct {
		name             string
		initialObjs      []runtime.Object
		expectedReplicas int32
	}{
		{
			name:             "creates pods when pool is empty",
			initialObjs:      []runtime.Object{template},
			expectedReplicas: replicas,
		},
		{
			name: "creates additional pods when under-provisioned",
			initialObjs: []runtime.Object{
				template,
				createPoolPod(poolName, poolNamespace, poolNameHash, "abc123"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "deletes excess pods when over-provisioned",
			initialObjs: []runtime.Object{
				template,
				createPoolPod(poolName, poolNamespace, poolNameHash, "abc123"),
				createPoolPod(poolName, poolNamespace, poolNameHash, "def456"),
				createPoolPod(poolName, poolNamespace, poolNameHash, "ghi789"),
				createPoolPod(poolName, poolNamespace, poolNameHash, "jkl012"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "maintains correct replica count",
			initialObjs: []runtime.Object{
				template,
				createPoolPod(poolName, poolNamespace, poolNameHash, "abc123"),
				createPoolPod(poolName, poolNamespace, poolNameHash, "def456"),
				createPoolPod(poolName, poolNamespace, poolNameHash, "ghi789"),
			},
			expectedReplicas: replicas,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(newTestScheme()).
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
				if pod.Labels[poolLabel] == poolNameHash {
					count++
				}
			}

			require.Equal(t, tc.expectedReplicas, count)
			require.Equal(t, tc.expectedReplicas, warmPool.Status.Replicas)
		})
	}
}

func TestReconcilePoolControllerRef(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(2)

	// Create a SandboxTemplate
	template := createTemplate(templateName, poolNamespace)

	warmPool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
			UID:       "warmpool-uid-123",
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			Replicas: replicas,
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: templateName,
			},
		},
	}

	// Compute the pool name hash
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	createPodWithOwner := func(name string, ownerUID string) *corev1.Pod {
		pod := createPoolPod(poolName, poolNamespace, poolNameHash, name)
		if ownerUID != "" {
			pod.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
					Kind:       "SandboxWarmPool",
					Name:       poolName,
					UID:        types.UID(ownerUID),
					Controller: boolPtr(true),
				},
			}
		}
		return pod
	}

	createPodWithDifferentController := func(name string) *corev1.Pod {
		pod := createPoolPod(poolName, poolNamespace, poolNameHash, name)
		pod.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "other-controller",
				UID:        "other-uid-456",
				Controller: boolPtr(true),
			},
		}
		return pod
	}

	testCases := []struct {
		name             string
		initialObjs      []runtime.Object
		expectedReplicas int32
		expectedAdopted  int // number of pods that should be adopted
	}{
		{
			name: "adopts orphaned pods with no controller reference",
			initialObjs: []runtime.Object{
				template,
				createPodWithOwner("abc123", ""), // No owner reference
				createPodWithOwner("def456", ""), // No owner reference
			},
			expectedReplicas: replicas,
			expectedAdopted:  2,
		},
		{
			name: "includes pods with correct controller reference",
			initialObjs: []runtime.Object{
				template,
				createPodWithOwner("abc123", "warmpool-uid-123"),
				createPodWithOwner("def456", "warmpool-uid-123"),
			},
			expectedReplicas: replicas,
			expectedAdopted:  0,
		},
		{
			name: "ignores pods with different controller reference",
			initialObjs: []runtime.Object{
				template,
				createPodWithDifferentController("abc123"),
				createPodWithDifferentController("def456"),
			},
			expectedReplicas: replicas, // Should create 2 new pods
			expectedAdopted:  0,
		},
		{
			name: "handles mix of owned, orphaned, and foreign pods",
			initialObjs: []runtime.Object{
				template,
				createPodWithOwner("abc123", "warmpool-uid-123"), // Owned
				createPodWithOwner("def456", ""),                 // Orphaned - should adopt
				createPodWithDifferentController("ghi789"),       // Foreign - should ignore
			},
			expectedReplicas: replicas,
			expectedAdopted:  1,
		},
		{
			name: "adopts orphan and creates additional pod when under-provisioned",
			initialObjs: []runtime.Object{
				template,
				createPodWithOwner("abc123", ""), // Orphaned - should adopt
			},
			expectedReplicas: replicas, // 1 adopted + 1 created
			expectedAdopted:  1,
		},
		{
			name: "deletes excess owned pods but ignores foreign pods",
			initialObjs: []runtime.Object{
				template,
				createPodWithOwner("abc123", "warmpool-uid-123"),
				createPodWithOwner("def456", "warmpool-uid-123"),
				createPodWithOwner("ghi789", "warmpool-uid-123"),
				createPodWithDifferentController("jkl012"), // Should be ignored
			},
			expectedReplicas: replicas, // Should delete 1 owned pod
			expectedAdopted:  0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(newTestScheme()).
					WithRuntimeObjects(tc.initialObjs...).
					Build(),
			}

			ctx := context.Background()

			// Run reconcilePool
			err := r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			// Run again to ensure idempotency
			err = r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			// Verify final state
			list := &corev1.PodList{}
			err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
			require.NoError(t, err)

			// Count pods with correct pool label and owned by warmpool
			ownedCount := int32(0)
			adoptedCount := 0
			for _, pod := range list.Items {
				if pod.Labels[poolLabel] == poolNameHash {
					controllerRef := metav1.GetControllerOf(&pod)
					if controllerRef != nil && controllerRef.UID == warmPool.UID {
						ownedCount++
						// Check if this was originally an orphan (adopted)
						for _, initialObj := range tc.initialObjs {
							if initialPod, ok := initialObj.(*corev1.Pod); ok {
								if initialPod.Name == pod.Name && len(initialPod.OwnerReferences) == 0 {
									adoptedCount++
									break
								}
							}
						}
					}
				}
			}

			require.Equal(t, tc.expectedReplicas, ownedCount, "owned pod count mismatch")
			require.Equal(t, tc.expectedReplicas, warmPool.Status.Replicas, "status replicas mismatch")
		})
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestPoolLabelValueInIntegration(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(3)

	ctx := context.Background()

	t.Run("all created pods have correct pool label and sandbox template ref label", func(t *testing.T) {
		// Create a SandboxTemplate with labels and annotations
		template := &extensionsv1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      templateName,
				Namespace: poolNamespace,
				Labels: map[string]string{
					"app":     "test-app",
					"version": "1.0",
				},
				Annotations: map[string]string{
					"description": "test pod",
				},
			},
			Spec: extensionsv1alpha1.SandboxTemplateSpec{
				PodTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "test-image:latest",
							},
						},
					},
				},
			},
		}

		warmPool := &extensionsv1alpha1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      poolName,
				Namespace: poolNamespace,
				UID:       "warmpool-uid-123",
			},
			Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
				Replicas: replicas,
				TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
					Name: templateName,
				},
			},
		}

		r := SandboxWarmPoolReconciler{
			Client: fake.NewClientBuilder().
				WithScheme(newTestScheme()).
				WithRuntimeObjects(template).
				Build(),
		}

		// Calculate expected pool name hash
		expectedPoolNameHash := sandboxcontrollers.NameHash(poolName)

		// Reconcile
		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		// List all pods
		list := &corev1.PodList{}
		err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, list.Items, int(replicas))

		// Verify each pod has the correct labels
		for _, pod := range list.Items {
			require.Equal(t, expectedPoolNameHash, pod.Labels[poolLabel],
				"pod %s should have correct pool label (pool name hash)", pod.Name)
			require.Equal(t, sandboxcontrollers.NameHash(templateName), pod.Labels[sandboxTemplateRefHash],
				"pod %s should have correct sandbox template ref label", pod.Name)

			// Verify template labels are also present
			require.Equal(t, "test-app", pod.Labels["app"])
			require.Equal(t, "1.0", pod.Labels["version"])

			// Verify annotations
			require.Equal(t, "test pod", pod.Annotations["description"])
		}
	})
}
