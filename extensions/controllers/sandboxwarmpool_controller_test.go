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
	"encoding/json"
	"testing"
	"time"

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

func createPoolSandbox(poolName, namespace, poolNameHash string, template *extensionsv1alpha1.SandboxTemplate, suffix string) *sandboxv1alpha1.Sandbox {
	replicas := int32(1)
	templateRefHash := ""
	var podTemplateHash string
	var podSpec corev1.PodSpec

	if template != nil {
		templateRefHash = sandboxcontrollers.NameHash(template.Name)
		podSpec = *template.Spec.PodTemplate.Spec.DeepCopy()
		// If template has a version label, we could use it as part of the hash placeholder
		if v, ok := template.Spec.PodTemplate.ObjectMeta.Labels["version"]; ok {
			podTemplateHash = "pod-hash-" + v
		} else {
			specJSON, _ := json.Marshal(template.Spec.PodTemplate)
			podTemplateHash = sandboxcontrollers.NameHash(string(specJSON))
		}
	} else {
		// Fallback for tests that don't provide a template
		podSpec = corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "test-image",
				},
			},
		}
		specJSON, _ := json.Marshal(sandboxv1alpha1.PodTemplate{Spec: podSpec})
		podTemplateHash = sandboxcontrollers.NameHash(string(specJSON))
	}

	return &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              poolName + suffix,
			Namespace:         namespace,
			CreationTimestamp: metav1.Now(),
			Labels: map[string]string{
				warmPoolSandboxLabel:   poolNameHash,
				sandboxTemplateRefHash: templateRefHash,
				sandboxPodTemplateHash: podTemplateHash,
			},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: &replicas,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						warmPoolSandboxLabel:   poolNameHash,
						sandboxTemplateRefHash: templateRefHash,
						sandboxPodTemplateHash: podTemplateHash,
					},
				},
				Spec: podSpec,
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
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-abc123"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "deletes excess sandboxes when over-provisioned",
			initialObjs: []runtime.Object{
				template,
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-abc123"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-def456"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-ghi789"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-jkl012"),
			},
			expectedReplicas: replicas,
		},
		{
			name: "maintains correct replica count",
			initialObjs: []runtime.Object{
				template,
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-abc123"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-def456"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-ghi789"),
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
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, suffix)
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
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, suffix)
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

	createSandboxWithReadyCondition := func(suffix string, ready metav1.ConditionStatus) *sandboxv1alpha1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, suffix)
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
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-abc123"),
				createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-def456"),
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
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, suffix)
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

func TestReconcilePool_TemplateUpdateRollout(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(2)

	testCases := []struct {
		name                 string
		strategy             extensionsv1alpha1.SandboxWarmPoolUpdateStrategyType
		expectedUpdatedImage bool
	}{
		{
			name:                 "Recreate strategy updates all pod images immediately",
			strategy:             extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
			expectedUpdatedImage: true,
		},
		{
			name:                 "OnReplenish strategy retains original pod images until manual deletion",
			strategy:             extensionsv1alpha1.OnReplenishSandboxWarmPoolUpdateStrategyType,
			expectedUpdatedImage: false,
		},
		{
			name:                 "Default strategy (empty string) behaves like OnReplenish and does not update all immediately",
			strategy:             "",
			expectedUpdatedImage: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create initial SandboxTemplate
			template := &extensionsv1alpha1.SandboxTemplate{
				TypeMeta: metav1.TypeMeta{
					APIVersion: extensionsv1alpha1.GroupVersion.String(),
					Kind:       "SandboxTemplate",
				},
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
									Image: "image-v1",
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
					UpdateStrategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
						Type: tc.strategy,
					},
				},
			}

			scheme := newTestScheme()
			r := SandboxWarmPoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(scheme).
					WithRuntimeObjects(template, warmPool).
					Build(),
				Scheme: scheme,
			}

			ctx := context.Background()

			// Initial reconciliation to create the sandboxes
			err := r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			// Get initial hash label
			template, initialHash, err := r.fetchTemplateAndHash(ctx, warmPool)
			require.NoError(t, err)

			// Verify sandboxes exist with initial image and hash
			sandboxes := &sandboxv1alpha1.SandboxList{}
			err = r.List(ctx, sandboxes, client.InNamespace(poolNamespace))
			require.NoError(t, err)
			require.Len(t, sandboxes.Items, int(replicas))
			for _, sb := range sandboxes.Items {
				require.Equal(t, "image-v1", sb.Spec.PodTemplate.Spec.Containers[0].Image)
				require.Equal(t, initialHash, sb.Labels[sandboxPodTemplateHash], "Sandbox should have initial template hash label")
			}

			// Update the SandboxTemplate content
			updatedTemplate := template.DeepCopy()
			updatedTemplate.Spec.PodTemplate.Spec.Containers[0].Image = "image-v2"
			err = r.Update(ctx, updatedTemplate)
			require.NoError(t, err)

			// Get new expected hash label
			_, updatedHash, err := r.fetchTemplateAndHash(ctx, warmPool)
			require.NoError(t, err)
			require.NotEqual(t, initialHash, updatedHash, "Hashes should differ after template update")

			// Reconcile again to trigger rollout (or lack thereof)
			err = r.reconcilePool(ctx, warmPool)
			require.NoError(t, err)

			// Verify state after update
			err = r.List(ctx, sandboxes, client.InNamespace(poolNamespace))
			require.NoError(t, err)
			require.Len(t, sandboxes.Items, int(replicas))

			if tc.expectedUpdatedImage {
				// For Recreate strategy, all should be updated
				for _, sb := range sandboxes.Items {
					require.Equal(t, "image-v2", sb.Spec.PodTemplate.Spec.Containers[0].Image, "Sandbox should have updated image")
					require.Equal(t, updatedHash, sb.Labels[sandboxPodTemplateHash], "Sandbox should have updated template hash label")
				}
				t.Log("Verified: All sandboxes updated immediately with Recreate strategy")
			} else {
				// For OnReplenish (default), all should still be v1
				for _, sb := range sandboxes.Items {
					require.Equal(t, "image-v1", sb.Spec.PodTemplate.Spec.Containers[0].Image, "Sandbox should retain original image")
					require.Equal(t, initialHash, sb.Labels[sandboxPodTemplateHash], "Sandbox should retain original template hash label")
				}
				t.Log("Verified: Sandboxes retained original image after update with OnReplenish strategy")

				// Now manually delete one sandbox to test replenishment
				sbToDelete := &sandboxes.Items[0]
				err = r.Delete(ctx, sbToDelete)
				require.NoError(t, err)

				// Reconcile to trigger replenishment
				err = r.reconcilePool(ctx, warmPool)
				require.NoError(t, err)

				// Verify that we have 2 sandboxes: one old (v1) and one new (v2)
				err = r.List(ctx, sandboxes, client.InNamespace(poolNamespace))
				require.NoError(t, err)
				require.Len(t, sandboxes.Items, int(replicas))

				v1Count, v2Count := 0, 0
				for _, sb := range sandboxes.Items {
					switch sb.Spec.PodTemplate.Spec.Containers[0].Image {
					case "image-v1":
						v1Count++
						require.Equal(t, initialHash, sb.Labels[sandboxPodTemplateHash])
					case "image-v2":
						v2Count++
						require.Equal(t, updatedHash, sb.Labels[sandboxPodTemplateHash])
					}
				}
				require.Equal(t, 1, v1Count, "Should have one remaining v1 sandbox")
				require.Equal(t, 1, v2Count, "Should have one newly created v2 sandbox")
				t.Log("Verified: New sandbox picking up updated template during replenishment in OnReplenish mode")
			}
		})
	}
}

func TestReconcilePool_TemplateRefUpdate_SameSpec(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName1 := "test-template-1"
	templateName2 := "test-template-2"
	replicas := int32(2)

	// Create initial SandboxTemplate
	template1 := &extensionsv1alpha1.SandboxTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: extensionsv1alpha1.GroupVersion.String(),
			Kind:       "SandboxTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName1,
			Namespace: poolNamespace,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "image-v1",
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
				Name: templateName1,
			},
			UpdateStrategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
				Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
			},
		},
	}

	scheme := newTestScheme()
	r := SandboxWarmPoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(template1, warmPool).
			Build(),
		Scheme: scheme,
	}

	ctx := context.Background()

	// Initial reconcile
	err := r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)

	sandboxes := &sandboxv1alpha1.SandboxList{}
	err = r.List(ctx, sandboxes, client.InNamespace(poolNamespace))
	require.NoError(t, err)
	require.Len(t, sandboxes.Items, int(replicas))

	initialSandboxNames := make(map[string]bool)
	for _, sb := range sandboxes.Items {
		initialSandboxNames[sb.Name] = true
	}

	// Create new SandboxTemplate with SAME spec
	template2 := &extensionsv1alpha1.SandboxTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: extensionsv1alpha1.GroupVersion.String(),
			Kind:       "SandboxTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName2,
			Namespace: poolNamespace,
		},
		Spec: *template1.Spec.DeepCopy(),
	}
	err = r.Create(ctx, template2)
	require.NoError(t, err)

	// Update WarmPool to point to template2
	warmPool.Spec.TemplateRef.Name = templateName2
	err = r.Update(ctx, warmPool)
	require.NoError(t, err)

	// Reconcile again to trigger rollout
	err = r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)

	// Verify state after update
	err = r.List(ctx, sandboxes, client.InNamespace(poolNamespace))
	require.NoError(t, err)
	require.Len(t, sandboxes.Items, int(replicas))

	for _, sb := range sandboxes.Items {
		// Sandboxes should be recreated (new names) because TemplateRef changed
		require.False(t, initialSandboxNames[sb.Name], "Sandbox should have been recreated with new name")
		require.Equal(t, sandboxcontrollers.NameHash(templateName2), sb.Labels[sandboxTemplateRefHash], "Sandbox should have updated template ref hash label")
		// The pod spec is identical, so the image remains image-v1
		require.Equal(t, "image-v1", sb.Spec.PodTemplate.Spec.Containers[0].Image, "Sandbox should retain original image since spec is identical")
	}
}

func TestFindWarmPoolsForTemplate(t *testing.T) {
	namespace := "default"
	templateName := "test-template"

	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: namespace,
		},
	}

	wp1 := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-1",
			Namespace: namespace,
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: templateName,
			},
		},
	}

	wp2 := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-2",
			Namespace: namespace,
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "other-template",
			},
		},
	}

	scheme := newTestScheme()
	r := SandboxWarmPoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithIndex(&extensionsv1alpha1.SandboxWarmPool{}, extensionsv1alpha1.TemplateRefField, func(rawObj client.Object) []string {
				wp := rawObj.(*extensionsv1alpha1.SandboxWarmPool)
				return []string{wp.Spec.TemplateRef.Name}
			}).
			WithRuntimeObjects(wp1, wp2).
			Build(),
		Scheme: scheme,
	}

	requests := r.findWarmPoolsForTemplate(context.Background(), template)

	require.Len(t, requests, 1)
	require.Equal(t, "pool-1", requests[0].Name)
	require.Equal(t, namespace, requests[0].Namespace)
}

func TestComparePodSpecsNormalization(t *testing.T) {
	falseVal := false
	trueVal := true

	tests := []struct {
		name           string
		templateSpec   corev1.PodSpec
		actualSpec     corev1.PodSpec
		secureByDef    bool
		expectedResult bool // true if they should be considered equal
	}{
		{
			name: "Identical specs should match",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test", Image: "img"}},
			},
			actualSpec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test", Image: "img"}},
			},
			secureByDef:    true,
			expectedResult: true,
		},
		{
			name: "AutomountServiceAccountToken nil in template vs false in actual should match",
			templateSpec: corev1.PodSpec{
				AutomountServiceAccountToken: nil,
			},
			actualSpec: corev1.PodSpec{
				AutomountServiceAccountToken: &falseVal,
			},
			secureByDef:    true,
			expectedResult: true,
		},
		{
			name: "AutomountServiceAccountToken true in template vs false in actual should NOT match (drift)",
			templateSpec: corev1.PodSpec{
				AutomountServiceAccountToken: &trueVal,
			},
			actualSpec: corev1.PodSpec{
				AutomountServiceAccountToken: &falseVal,
			},
			secureByDef:    true,
			expectedResult: false,
		},
		{
			name: "DNSPolicy empty in template vs DNSNone in actual (SecureByDefault) should match",
			templateSpec: corev1.PodSpec{
				DNSPolicy: "",
			},
			actualSpec: corev1.PodSpec{
				DNSPolicy: corev1.DNSNone,
				DNSConfig: &corev1.PodDNSConfig{
					Nameservers: []string{"8.8.8.8", "1.1.1.1"},
				},
			},
			secureByDef:    true,
			expectedResult: true,
		},
		{
			name: "DNSPolicy drift from Default to ClusterFirst should NOT match",
			templateSpec: corev1.PodSpec{
				DNSPolicy: corev1.DNSClusterFirst,
			},
			actualSpec: corev1.PodSpec{
				DNSPolicy: corev1.DNSDefault,
			},
			secureByDef:    false,
			expectedResult: false,
		},
	}

	r := &SandboxWarmPoolReconciler{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := &extensionsv1alpha1.SandboxTemplate{
				Spec: extensionsv1alpha1.SandboxTemplateSpec{
					PodTemplate: sandboxv1alpha1.PodTemplate{
						Spec: tt.templateSpec,
					},
				},
			}
			if tt.secureByDef {
				template.Spec.NetworkPolicyManagement = extensionsv1alpha1.NetworkPolicyManagementManaged
			} else {
				template.Spec.NetworkPolicyManagement = extensionsv1alpha1.NetworkPolicyManagementUnmanaged
			}

			// We need to apply the SAME defaults to the 'actual' spec in the test
			// if we want to simulate a sandbox that was created with those defaults.
			actualSpecCopy := tt.actualSpec.DeepCopy()
			// Only apply if it's NOT a drift test case where we WANT them to be different
			if tt.expectedResult {
				ApplySandboxSecureDefaults(template, actualSpecCopy)
			}

			result := r.comparePodSpecs(template, actualSpecCopy)
			if result != tt.expectedResult {
				t.Errorf("comparePodSpecs() = %v, want %v", result, tt.expectedResult)
			}
		})
	}
}

func TestReconcilePool_TemplateUpdate_DNSPolicy(t *testing.T) {
	poolName := "test-pool"
	poolNamespace := "default"
	templateName := "test-template"
	replicas := int32(2)

	ctx := context.Background()
	scheme := newTestScheme()

	// Create initial SandboxTemplate with default DNS
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: poolNamespace,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			NetworkPolicyManagement: extensionsv1alpha1.NetworkPolicyManagementUnmanaged,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "img"},
					},
					DNSPolicy: corev1.DNSDefault,
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
			UpdateStrategy: &extensionsv1alpha1.SandboxWarmPoolUpdateStrategy{
				Type: extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType,
			},
		},
	}

	r := SandboxWarmPoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(template, warmPool).
			Build(),
		Scheme: scheme,
	}

	// Initial reconcile to create sandboxes
	err := r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)

	// Verify initial state
	sandboxes := &sandboxv1alpha1.SandboxList{}
	err = r.List(ctx, sandboxes, client.InNamespace(poolNamespace))
	require.NoError(t, err)
	require.Len(t, sandboxes.Items, int(replicas))
	for _, sb := range sandboxes.Items {
		require.Equal(t, corev1.DNSDefault, sb.Spec.PodTemplate.Spec.DNSPolicy)
	}

	// Update SandboxTemplate to change DNSPolicy
	updatedTemplate := template.DeepCopy()
	updatedTemplate.Spec.PodTemplate.Spec.DNSPolicy = corev1.DNSClusterFirst
	err = r.Update(ctx, updatedTemplate)
	require.NoError(t, err)

	// Reconcile again, should trigger rollout (deletion and recreation)
	err = r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)

	// Verify that sandboxes now have the updated DNSPolicy
	err = r.List(ctx, sandboxes, client.InNamespace(poolNamespace))
	require.NoError(t, err)
	require.Len(t, sandboxes.Items, int(replicas))
	for _, sb := range sandboxes.Items {
		require.Equal(t, corev1.DNSClusterFirst, sb.Spec.PodTemplate.Spec.DNSPolicy, "Sandbox should have updated DNSPolicy")
	}
}
