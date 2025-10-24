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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
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

	// Compute the pool name hash
	poolNameHash := NameHash(poolName)

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
				createPoolPod(poolName, poolNamespace, poolNameHash, "abc123"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "deletes excess pods when over-provisioned",
			initialObjs: []runtime.Object{
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
	replicas := int32(2)

	warmPool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
			UID:       "warmpool-uid-123",
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

	// Compute the pool name hash
	poolNameHash := NameHash(poolName)

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
				createPodWithOwner("abc123", ""), // No owner reference
				createPodWithOwner("def456", ""), // No owner reference
			},
			expectedReplicas: replicas,
			expectedAdopted:  2,
		},
		{
			name: "includes pods with correct controller reference",
			initialObjs: []runtime.Object{
				createPodWithOwner("abc123", "warmpool-uid-123"),
				createPodWithOwner("def456", "warmpool-uid-123"),
			},
			expectedReplicas: replicas,
			expectedAdopted:  0,
		},
		{
			name: "ignores pods with different controller reference",
			initialObjs: []runtime.Object{
				createPodWithDifferentController("abc123"),
				createPodWithDifferentController("def456"),
			},
			expectedReplicas: replicas, // Should create 2 new pods
			expectedAdopted:  0,
		},
		{
			name: "handles mix of owned, orphaned, and foreign pods",
			initialObjs: []runtime.Object{
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
				createPodWithOwner("abc123", ""), // Orphaned - should adopt
			},
			expectedReplicas: replicas, // 1 adopted + 1 created
			expectedAdopted:  1,
		},
		{
			name: "deletes excess owned pods but ignores foreign pods",
			initialObjs: []runtime.Object{
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

func TestHashPodTemplate(t *testing.T) {
	testCases := []struct {
		name          string
		podTemplate1  sandboxv1alpha1.PodTemplate
		podTemplate2  sandboxv1alpha1.PodTemplate
		shouldBeEqual bool
	}{
		{
			name: "identical templates with full resource specs produce same hash even if field order differs",
			podTemplate1: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image:v1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("128Mi"),
									corev1.ResourceCPU:    resource.MustParse("250m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("256Mi"),
									corev1.ResourceCPU:    resource.MustParse("500m"),
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "ENV_VAR_1",
									Value: "value1",
								},
								{
									Name:  "ENV_VAR_2",
									Value: "value2",
								},
							},
							Command: []string{"/bin/sh", "-c", "sleep 3600"},
						},
					},
				},
			},
			podTemplate2: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("250m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
							Command: []string{"/bin/sh", "-c", "sleep 3600"},
							Env: []corev1.EnvVar{
								{
									Name:  "ENV_VAR_1",
									Value: "value1",
								},
								{
									Name:  "ENV_VAR_2",
									Value: "value2",
								},
							},
							Image: "test-image:v1",
						},
					},
				},
			},
			shouldBeEqual: true,
		},
		{
			name: "different images produce different hash",
			podTemplate1: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image:v1",
						},
					},
				},
			},
			podTemplate2: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image:v2",
						},
					},
				},
			},
			shouldBeEqual: false,
		},
		{
			name: "different container names produce different hash",
			podTemplate1: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "container-1",
							Image: "test-image",
						},
					},
				},
			},
			podTemplate2: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "container-2",
							Image: "test-image",
						},
					},
				},
			},
			shouldBeEqual: false,
		},
		{
			name: "different labels produce same hash",
			podTemplate1: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						"app": "test1",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			},
			podTemplate2: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						"app": "test2",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			},
			shouldBeEqual: true,
		},
		{
			name: "different annotations produce same hash",
			podTemplate1: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Annotations: map[string]string{
						"key": "value1",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			},
			podTemplate2: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Annotations: map[string]string{
						"key": "value2",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
						},
					},
				},
			},
			shouldBeEqual: true,
		},
		{
			name: "different resource limits produce different hash",
			podTemplate1: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("100m"),
								},
							},
						},
					},
				},
			},
			podTemplate2: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("200m"),
								},
							},
						},
					},
				},
			},
			shouldBeEqual: false,
		},
		{
			name: "different number of containers produce different hash",
			podTemplate1: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "container-1",
							Image: "test-image",
						},
					},
				},
			},
			podTemplate2: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "container-1",
							Image: "test-image",
						},
						{
							Name:  "container-2",
							Image: "test-image",
						},
					},
				},
			},
			shouldBeEqual: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hash1, err := hashPodTemplate(tc.podTemplate1)
			require.NoError(t, err)
			require.NotEmpty(t, hash1)

			hash2, err := hashPodTemplate(tc.podTemplate2)
			require.NoError(t, err)
			require.NotEmpty(t, hash2)

			if tc.shouldBeEqual {
				require.Equal(t, hash1, hash2, "hashes should be equal for identical templates")
			} else {
				require.NotEqual(t, hash1, hash2, "hashes should be different for different templates")
			}
		})
	}
}

func TestHashPodTemplateProperties(t *testing.T) {
	t.Run("hash is deterministic", func(t *testing.T) {
		podTemplate := sandboxv1alpha1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
					},
				},
			},
		}

		// Generate hash multiple times
		hashes := make([]string, 10)
		for i := 0; i < 10; i++ {
			hash, err := hashPodTemplate(podTemplate)
			require.NoError(t, err)
			hashes[i] = hash
		}

		// All hashes should be identical
		for i := 1; i < len(hashes); i++ {
			require.Equal(t, hashes[0], hashes[i], "hash should be deterministic")
		}
	})

	t.Run("hash is base36 encoded", func(t *testing.T) {
		podTemplate := sandboxv1alpha1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
					},
				},
			},
		}

		hash, err := hashPodTemplate(podTemplate)
		require.NoError(t, err)
		require.NotEmpty(t, hash)

		// Verify it's valid base36 (only contains 0-9, a-z)
		for _, char := range hash {
			require.True(t, (char >= '0' && char <= '9') || (char >= 'a' && char <= 'z'),
				"hash should only contain base36 characters (0-9, a-z)")
		}
	})

	t.Run("hash is reasonable length", func(t *testing.T) {
		podTemplate := sandboxv1alpha1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
					},
				},
			},
		}

		hash, err := hashPodTemplate(podTemplate)
		require.NoError(t, err)
		require.NotEmpty(t, hash)

		// FNV-1a 64-bit hash in base36 should be at most 13 characters
		// (2^64 - 1 in base36 is "3w5e11264sgsf")
		require.LessOrEqual(t, len(hash), 13, "hash should be reasonably short")
	})
}

func TestPoolLabelValueInIntegration(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	replicas := int32(3)

	ctx := context.Background()

	t.Run("all created pods have correct pool label and pod template hash label", func(t *testing.T) {
		warmPool := &extensionsv1alpha1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      poolName,
				Namespace: poolNamespace,
				UID:       "warmpool-uid-123",
			},
			Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
				Replicas: replicas,
				PodTemplate: sandboxv1alpha1.PodTemplate{
					ObjectMeta: sandboxv1alpha1.PodMetadata{
						Labels: map[string]string{
							"app":     "test-app",
							"version": "1.0",
						},
						Annotations: map[string]string{
							"description": "test pod",
						},
					},
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

		r := SandboxWarmPoolReconciler{
			Client: fake.NewClientBuilder().
				WithScheme(newTestScheme()).
				Build(),
		}

		// Calculate expected hashes
		expectedPoolNameHash := NameHash(poolName)
		expectedPodTemplateHash, err := hashPodTemplate(warmPool.Spec.PodTemplate)
		require.NoError(t, err)

		// Reconcile
		err = r.reconcilePool(ctx, warmPool)
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
			require.Equal(t, expectedPodTemplateHash, pod.Labels[podTemplateHashLabel],
				"pod %s should have correct pod template hash label", pod.Name)

			// Verify template labels are also present
			require.Equal(t, "test-app", pod.Labels["app"])
			require.Equal(t, "1.0", pod.Labels["version"])

			// Verify annotations
			require.Equal(t, "test pod", pod.Annotations["description"])
		}
	})
}
