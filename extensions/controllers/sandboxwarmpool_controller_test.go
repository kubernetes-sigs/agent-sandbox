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
				PodTemplate: sandboxv1alpha1.PodTemplate{
					ObjectMeta: sandboxv1alpha1.PodMetadata{
						Labels: map[string]string{
							"pod-label": "from-podtemplate",
							"version":   "2.0",
						},
						Annotations: map[string]string{
							"pod-annotation": "from-podtemplate",
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

			// Verify labels from pod template
			require.Equal(t, "2.0", pod.Labels["version"])
			require.Equal(t, "from-podtemplate", pod.Labels["pod-label"])

			// Verify sandbox template labels are not propagated
			require.NotContains(t, pod.Labels, "app")

			// Verify annotations from pod template
			require.Equal(t, "from-podtemplate", pod.Annotations["pod-annotation"])

			// Verify sandbox template metadata annotations are not propagated
			require.NotContains(t, pod.Annotations, "description")
		}
	})
}

func TestReconcilePoolReadyReplicas(t *testing.T) {
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

	createPodWithReadyCondition := func(suffix string, ready corev1.ConditionStatus) *corev1.Pod {
		pod := createPoolPod(poolName, poolNamespace, poolNameHash, suffix)
		pod.Status.Conditions = []corev1.PodCondition{
			{
				Type:   corev1.PodReady,
				Status: ready,
			},
		}
		return pod
	}

	testCases := []struct {
		name                  string
		initialPods           []runtime.Object
		expectedReadyReplicas int32
	}{
		{
			name: "no pods ready",
			initialPods: []runtime.Object{
				template,
				createPodWithReadyCondition("abc123", corev1.ConditionFalse),
				createPodWithReadyCondition("def456", corev1.ConditionUnknown),
				createPodWithReadyCondition("ghi789", corev1.ConditionFalse),
			},
			expectedReadyReplicas: 0,
		},
		{
			name: "some pods ready",
			initialPods: []runtime.Object{
				template,
				createPodWithReadyCondition("abc123", corev1.ConditionTrue),
				createPodWithReadyCondition("def456", corev1.ConditionFalse),
				createPodWithReadyCondition("ghi789", corev1.ConditionTrue),
			},
			expectedReadyReplicas: 2,
		},
		{
			name: "all pods ready",
			initialPods: []runtime.Object{
				template,
				createPodWithReadyCondition("abc123", corev1.ConditionTrue),
				createPodWithReadyCondition("def456", corev1.ConditionTrue),
				createPodWithReadyCondition("ghi789", corev1.ConditionTrue),
			},
			expectedReadyReplicas: 3,
		},
		{
			name: "pods with no ready condition",
			initialPods: []runtime.Object{
				template,
				createPoolPod(poolName, poolNamespace, poolNameHash, "abc123"),
				createPoolPod(poolName, poolNamespace, poolNameHash, "def456"),
				createPodWithReadyCondition("ghi789", corev1.ConditionTrue),
			},
			expectedReadyReplicas: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(newTestScheme()).
					WithRuntimeObjects(tc.initialPods...).
					Build(),
			}

			ctx := context.Background()

			// Run reconcilePool twice to update status
			err := r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)
			err = r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			// Verify the ReadyReplicas status
			require.Equal(t, tc.expectedReadyReplicas, warmPool.Status.ReadyReplicas)
		})
	}
}

func TestReconcilePoolWithVolumeClaimTemplates(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(2)

	// Create a SandboxTemplate with volumeClaimTemplates
	templateWithPVC := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: poolNamespace,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "test-image",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/data",
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
						Name: "data",
						Labels: map[string]string{
							"pvc-label": "test-value",
						},
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: *mustParseQuantity("1Gi"),
							},
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

	t.Run("creates PVCs from volumeClaimTemplates", func(t *testing.T) {
		r := SandboxWarmPoolReconciler{
			Client: fake.NewClientBuilder().
				WithScheme(newTestScheme()).
				WithRuntimeObjects(templateWithPVC).
				Build(),
		}

		ctx := context.Background()

		// Run reconcilePool
		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		// Verify pods were created
		podList := &corev1.PodList{}
		err = r.List(ctx, podList, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, podList.Items, int(replicas))

		// Verify PVCs were created
		pvcList := &corev1.PersistentVolumeClaimList{}
		err = r.List(ctx, pvcList, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, pvcList.Items, int(replicas), "expected one PVC per pod")

		// Verify each PVC
		for _, pvc := range pvcList.Items {
			// Verify PVC is owned by the warm pool
			controllerRef := metav1.GetControllerOf(&pvc)
			require.NotNil(t, controllerRef)
			require.Equal(t, warmPool.UID, controllerRef.UID)

			// Verify PVC has correct labels (pool label + template labels)
			require.Equal(t, sandboxcontrollers.NameHash(poolName), pvc.Labels[poolLabel])
			require.Equal(t, "test-value", pvc.Labels["pvc-label"])

			// Verify PVC spec
			require.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes)
		}

		// Verify each pod has PVC volume attached
		for _, pod := range podList.Items {
			var foundPVCVolume bool
			for _, vol := range pod.Spec.Volumes {
				if vol.PersistentVolumeClaim != nil {
					foundPVCVolume = true
					// Verify volume name matches template
					require.Equal(t, "data", vol.Name)
					break
				}
			}
			require.True(t, foundPVCVolume, "pod should have PVC volume attached")
		}
	})

	t.Run("cleans up orphaned PVCs and creates new pods in same reconcile", func(t *testing.T) {
		poolNameHash := sandboxcontrollers.NameHash(poolName)

		// Create an orphaned PVC (simulating partial failure where PVC exists but pod doesn't)
		orphanedPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data-test-pool-abc12",
				Namespace: poolNamespace,
				Labels: map[string]string{
					poolLabel: poolNameHash,
				},
				Annotations: map[string]string{
					sandboxcontrollers.WarmPoolPodNameAnnotation: "test-pool-abc12", // Pod doesn't exist
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
						Kind:       "SandboxWarmPool",
						Name:       poolName,
						UID:        warmPool.UID,
						Controller: boolPtr(true),
					},
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		}

		r := SandboxWarmPoolReconciler{
			Client: fake.NewClientBuilder().
				WithScheme(newTestScheme()).
				WithRuntimeObjects(templateWithPVC, orphanedPVC).
				Build(),
		}

		ctx := context.Background()

		// Run reconcilePool - should clean up orphan AND create new pods in same reconcile
		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		// Verify pods were created
		podList := &corev1.PodList{}
		err = r.List(ctx, podList, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, podList.Items, int(warmPool.Spec.Replicas), "should create pods")

		// Verify old orphaned PVC was deleted and new PVCs were created
		pvcList := &corev1.PersistentVolumeClaimList{}
		err = r.List(ctx, pvcList, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, pvcList.Items, int(warmPool.Spec.Replicas), "should have one PVC per pod")

		// Verify the orphaned PVC is gone (none should have the old name)
		for _, pvc := range pvcList.Items {
			require.NotEqual(t, "data-test-pool-abc12", pvc.Name, "orphaned PVC should be deleted")
		}
	})

	t.Run("deletes PVCs when scaling down pods", func(t *testing.T) {
		poolNameHash := sandboxcontrollers.NameHash(poolName)

		// Create existing pod with its PVC
		existingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-existing",
				Namespace: poolNamespace,
				Labels: map[string]string{
					poolLabel: poolNameHash,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
						Kind:       "SandboxWarmPool",
						Name:       poolName,
						UID:        warmPool.UID,
						Controller: boolPtr(true),
					},
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test", Image: "test"}},
			},
		}

		existingPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data-test-pool-existing",
				Namespace: poolNamespace,
				Labels: map[string]string{
					poolLabel: poolNameHash,
				},
				Annotations: map[string]string{
					sandboxcontrollers.WarmPoolPodNameAnnotation: "test-pool-existing", // Matches pod name
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
						Kind:       "SandboxWarmPool",
						Name:       poolName,
						UID:        warmPool.UID,
						Controller: boolPtr(true),
					},
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		}

		// Warm pool with 0 replicas (scale down)
		scaledDownPool := warmPool.DeepCopy()
		scaledDownPool.Spec.Replicas = 0

		r := SandboxWarmPoolReconciler{
			Client: fake.NewClientBuilder().
				WithScheme(newTestScheme()).
				WithRuntimeObjects(templateWithPVC, existingPod, existingPVC).
				Build(),
		}

		ctx := context.Background()

		// Run reconcilePool - should delete pod and its PVC
		err := r.reconcilePool(ctx, scaledDownPool)
		require.NoError(t, err)

		// Verify the pod was deleted
		podList := &corev1.PodList{}
		err = r.List(ctx, podList, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, podList.Items, 0, "pod should be deleted during scale-down")

		// Verify the PVC was also deleted
		pvcList := &corev1.PersistentVolumeClaimList{}
		err = r.List(ctx, pvcList, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, pvcList.Items, 0, "PVC should be deleted during scale-down")
	})
}

func mustParseQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
