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
	"time"

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

func createPoolSandbox(poolName, namespace, poolNameHash, suffix string) *sandboxv1alpha1.Sandbox {
	replicas := int32(1)
	return &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              poolName + suffix,
			Namespace:         namespace,
			CreationTimestamp: metav1.Now(),
			Labels: map[string]string{
				warmPoolSandboxLabel: poolNameHash,
			},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: &replicas,
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

func createTemplate(namespace string) *extensionsv1alpha1.SandboxTemplate {
	return &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
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

	template := createTemplate(poolNamespace)

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

	poolNameHash := sandboxcontrollers.NameHash(poolName)
	scheme := newTestScheme()

	testCases := []struct {
		name             string
		initialObjs      []runtime.Object
		expectedReplicas int32
	}{
		{
			name:             "creates sandboxes when pool is empty",
			initialObjs:      []runtime.Object{template},
			expectedReplicas: replicas,
		},
		{
			name: "creates additional sandboxes when under-provisioned",
			initialObjs: []runtime.Object{
				template,
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-abc123"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "deletes excess sandboxes when over-provisioned",
			initialObjs: []runtime.Object{
				template,
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-abc123"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-def456"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-ghi789"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-jkl012"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "maintains correct replica count",
			initialObjs: []runtime.Object{
				template,
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-abc123"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-def456"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-ghi789"),
			},
			expectedReplicas: replicas,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(scheme).
					WithRuntimeObjects(tc.initialObjs...).
					Build(),
				Scheme: scheme,
			}

			ctx := context.Background()

			err := r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			err = r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			// Verify final state - count sandboxes with correct warm pool label
			list := &sandboxv1alpha1.SandboxList{}
			err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
			require.NoError(t, err)

			count := int32(0)
			for _, sb := range list.Items {
				if sb.Labels[warmPoolSandboxLabel] == poolNameHash {
					count++
				}
			}

			require.Equal(t, tc.expectedReplicas, count)
			require.Equal(t, tc.expectedReplicas, warmPool.Status.Replicas)

			expectedSelector := warmPoolSandboxLabel + "=" + poolNameHash
			require.Equal(t, expectedSelector, warmPool.Status.Selector, "Status.Selector mismatch")
		})
	}
}

func TestReconcilePoolControllerRef(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(2)

	template := createTemplate(poolNamespace)
	scheme := newTestScheme()

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

	poolNameHash := sandboxcontrollers.NameHash(poolName)

	createSandboxWithOwner := func(suffix string, ownerUID string) *sandboxv1alpha1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, suffix)
		if ownerUID != "" {
			sb.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
					Kind:       "SandboxWarmPool",
					Name:       poolName,
					UID:        types.UID(ownerUID),
					Controller: boolPtr(true),
				},
			}
		}
		return sb
	}

	createSandboxWithDifferentController := func(suffix string) *sandboxv1alpha1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, suffix)
		sb.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "other-controller",
				UID:        "other-uid-456",
				Controller: boolPtr(true),
			},
		}
		return sb
	}

	testCases := []struct {
		name             string
		initialObjs      []runtime.Object
		expectedReplicas int32
	}{
		{
			name: "adopts orphaned sandboxes with no controller reference",
			initialObjs: []runtime.Object{
				template,
				createSandboxWithOwner("-abc123", ""),
				createSandboxWithOwner("-def456", ""),
			},
			expectedReplicas: replicas,
		},
		{
			name: "includes sandboxes with correct controller reference",
			initialObjs: []runtime.Object{
				template,
				createSandboxWithOwner("-abc123", "warmpool-uid-123"),
				createSandboxWithOwner("-def456", "warmpool-uid-123"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "ignores sandboxes with different controller reference",
			initialObjs: []runtime.Object{
				template,
				createSandboxWithDifferentController("-abc123"),
				createSandboxWithDifferentController("-def456"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "handles mix of owned, orphaned, and foreign sandboxes",
			initialObjs: []runtime.Object{
				template,
				createSandboxWithOwner("-abc123", "warmpool-uid-123"),
				createSandboxWithOwner("-def456", ""),
				createSandboxWithDifferentController("-ghi789"),
			},
			expectedReplicas: replicas,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(scheme).
					WithRuntimeObjects(tc.initialObjs...).
					Build(),
				Scheme: scheme,
			}

			ctx := context.Background()

			err := r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			err = r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			list := &sandboxv1alpha1.SandboxList{}
			err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
			require.NoError(t, err)

			ownedCount := int32(0)
			for _, sb := range list.Items {
				if sb.Labels[warmPoolSandboxLabel] == poolNameHash {
					controllerRef := metav1.GetControllerOf(&sb)
					if controllerRef != nil && controllerRef.UID == warmPool.UID {
						ownedCount++
					}
				}
			}

			require.Equal(t, tc.expectedReplicas, ownedCount, "owned sandbox count mismatch")
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
	scheme := newTestScheme()

	t.Run("all created sandboxes have correct labels from template", func(t *testing.T) {
		template := &extensionsv1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      templateName,
				Namespace: poolNamespace,
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
				WithScheme(scheme).
				WithRuntimeObjects(template).
				Build(),
			Scheme: scheme,
		}

		expectedPoolNameHash := sandboxcontrollers.NameHash(poolName)

		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		list := &sandboxv1alpha1.SandboxList{}
		err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, list.Items, int(replicas))

		for _, sb := range list.Items {
			require.Equal(t, expectedPoolNameHash, sb.Labels[warmPoolSandboxLabel],
				"sandbox %s should have correct warm pool label", sb.Name)
			require.Equal(t, sandboxcontrollers.NameHash(templateName), sb.Labels[sandboxTemplateRefHash],
				"sandbox %s should have correct template ref label", sb.Name)

			// Verify pod template labels are propagated into the sandbox's pod template
			require.Equal(t, "2.0", sb.Spec.PodTemplate.ObjectMeta.Labels["version"])
			require.Equal(t, "from-podtemplate", sb.Spec.PodTemplate.ObjectMeta.Labels["pod-label"])

			// Verify pod template annotations
			require.Equal(t, "from-podtemplate", sb.Spec.PodTemplate.ObjectMeta.Annotations["pod-annotation"])
		}
	})
}

func TestReconcilePoolReadyReplicas(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(3)

	template := createTemplate(poolNamespace)
	scheme := newTestScheme()

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

	poolNameHash := sandboxcontrollers.NameHash(poolName)

	createSandboxWithReadyCondition := func(suffix string, ready metav1.ConditionStatus) *sandboxv1alpha1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, suffix)
		sb.Status.Conditions = []metav1.Condition{
			{
				Type:   string(sandboxv1alpha1.SandboxConditionReady),
				Status: ready,
			},
		}
		return sb
	}

	testCases := []struct {
		name                  string
		initialObjs           []runtime.Object
		expectedReadyReplicas int32
	}{
		{
			name: "no sandboxes ready",
			initialObjs: []runtime.Object{
				template,
				createSandboxWithReadyCondition("-abc123", metav1.ConditionFalse),
				createSandboxWithReadyCondition("-def456", metav1.ConditionUnknown),
				createSandboxWithReadyCondition("-ghi789", metav1.ConditionFalse),
			},
			expectedReadyReplicas: 0,
		},
		{
			name: "some sandboxes ready",
			initialObjs: []runtime.Object{
				template,
				createSandboxWithReadyCondition("-abc123", metav1.ConditionTrue),
				createSandboxWithReadyCondition("-def456", metav1.ConditionFalse),
				createSandboxWithReadyCondition("-ghi789", metav1.ConditionTrue),
			},
			expectedReadyReplicas: 2,
		},
		{
			name: "all sandboxes ready",
			initialObjs: []runtime.Object{
				template,
				createSandboxWithReadyCondition("-abc123", metav1.ConditionTrue),
				createSandboxWithReadyCondition("-def456", metav1.ConditionTrue),
				createSandboxWithReadyCondition("-ghi789", metav1.ConditionTrue),
			},
			expectedReadyReplicas: 3,
		},
		{
			name: "sandboxes with no ready condition",
			initialObjs: []runtime.Object{
				template,
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-abc123"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, "-def456"),
				createSandboxWithReadyCondition("-ghi789", metav1.ConditionTrue),
			},
			expectedReadyReplicas: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(scheme).
					WithRuntimeObjects(tc.initialObjs...).
					Build(),
				Scheme: scheme,
			}

			ctx := context.Background()

			err := r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)
			err = r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			require.Equal(t, tc.expectedReadyReplicas, warmPool.Status.ReadyReplicas)
		})
	}
}

func TestReconcilePoolGCStuckSandboxes(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(2)

	template := createTemplate(poolNamespace)
	scheme := newTestScheme()

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

	poolNameHash := sandboxcontrollers.NameHash(poolName)

	createSandboxWithAge := func(suffix string, ready metav1.ConditionStatus, age time.Duration) *sandboxv1alpha1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, suffix)
		sb.CreationTimestamp = metav1.Time{Time: time.Now().Add(-age)}
		sb.Status.Conditions = []metav1.Condition{
			{
				Type:   string(sandboxv1alpha1.SandboxConditionReady),
				Status: ready,
			},
		}
		return sb
	}

	t.Run("deletes non-ready sandbox older than grace period", func(t *testing.T) {
		r := SandboxWarmPoolReconciler{
			Client: fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(
					template,
					createSandboxWithAge("-stuck", metav1.ConditionFalse, 10*time.Minute),
					createSandboxWithAge("-healthy", metav1.ConditionTrue, 10*time.Minute),
				).
				Build(),
			Scheme: scheme,
		}

		ctx := context.Background()
		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		// The stuck sandbox should be deleted and replaced
		list := &sandboxv1alpha1.SandboxList{}
		err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)

		// Should have: 1 healthy (kept) + 1 newly created replacement = 2
		poolCount := int32(0)
		for _, sb := range list.Items {
			if sb.Labels[warmPoolSandboxLabel] == poolNameHash {
				poolCount++
			}
		}
		require.Equal(t, replicas, poolCount)
	})

	t.Run("keeps non-ready sandbox within grace period", func(t *testing.T) {
		r := SandboxWarmPoolReconciler{
			Client: fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(
					template,
					createSandboxWithAge("-starting", metav1.ConditionFalse, 2*time.Minute),
					createSandboxWithAge("-healthy", metav1.ConditionTrue, 10*time.Minute),
				).
				Build(),
			Scheme: scheme,
		}

		ctx := context.Background()
		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		// Both should be kept (one healthy, one still within grace period)
		list := &sandboxv1alpha1.SandboxList{}
		err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)

		poolCount := int32(0)
		for _, sb := range list.Items {
			if sb.Labels[warmPoolSandboxLabel] == poolNameHash {
				poolCount++
			}
		}
		require.Equal(t, replicas, poolCount)
		require.Equal(t, replicas, warmPool.Status.Replicas)
	})
}

func TestReconcilePoolVolumeClaimTemplates(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(2)

	scheme := newTestScheme()
	ctx := context.Background()
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	t.Run("volumeClaimTemplates are copied from template to sandbox", func(t *testing.T) {
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
							},
						},
					},
				},
				VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
					{
						EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
							Name: "data-vol",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("10Gi"),
								},
							},
						},
					},
					{
						EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
							Name: "cache-vol",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("5Gi"),
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
				UID:       "warmpool-uid-vct",
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
				WithScheme(scheme).
				WithRuntimeObjects(templateWithPVC).
				Build(),
			Scheme: scheme,
		}

		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		list := &sandboxv1alpha1.SandboxList{}
		err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, list.Items, int(replicas))

		for _, sb := range list.Items {
			require.Equal(t, poolNameHash, sb.Labels[warmPoolSandboxLabel],
				"sandbox %s should have correct warm pool label", sb.Name)

			// Verify volumeClaimTemplates were copied
			require.Len(t, sb.Spec.VolumeClaimTemplates, 2,
				"sandbox %s should have 2 volumeClaimTemplates", sb.Name)
			require.Equal(t, "data-vol", sb.Spec.VolumeClaimTemplates[0].Name)
			require.Equal(t, resource.MustParse("10Gi"),
				sb.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage])
			require.Equal(t, "cache-vol", sb.Spec.VolumeClaimTemplates[1].Name)
			require.Equal(t, resource.MustParse("5Gi"),
				sb.Spec.VolumeClaimTemplates[1].Spec.Resources.Requests[corev1.ResourceStorage])
		}
	})

	t.Run("template without volumeClaimTemplates produces sandbox with no VCTs", func(t *testing.T) {
		templateNoPVC := &extensionsv1alpha1.SandboxTemplate{
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
				UID:       "warmpool-uid-novct",
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
				WithScheme(scheme).
				WithRuntimeObjects(templateNoPVC).
				Build(),
			Scheme: scheme,
		}

		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		list := &sandboxv1alpha1.SandboxList{}
		err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, list.Items, int(replicas))

		for _, sb := range list.Items {
			require.Empty(t, sb.Spec.VolumeClaimTemplates,
				"sandbox %s should have no volumeClaimTemplates", sb.Name)
		}
	})

	t.Run("volumeClaimTemplates are deep-copied not shared", func(t *testing.T) {
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
							},
						},
					},
				},
				VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
					{
						EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
							Name: "workspace",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("20Gi"),
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
				UID:       "warmpool-uid-deepcopy",
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
				WithScheme(scheme).
				WithRuntimeObjects(templateWithPVC).
				Build(),
			Scheme: scheme,
		}

		err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		list := &sandboxv1alpha1.SandboxList{}
		err = r.List(ctx, list, &client.ListOptions{Namespace: poolNamespace})
		require.NoError(t, err)
		require.Len(t, list.Items, int(replicas))

		// Mutate the first sandbox's VCT and verify the second is unaffected
		list.Items[0].Spec.VolumeClaimTemplates[0].Name = "mutated"
		require.Equal(t, "workspace", list.Items[1].Spec.VolumeClaimTemplates[0].Name,
			"second sandbox should not be affected by mutating the first")

		// Also verify template is unaffected
		require.Equal(t, "workspace", templateWithPVC.Spec.VolumeClaimTemplates[0].Name,
			"template should not be affected by mutating a sandbox's VCT")
	})
}
