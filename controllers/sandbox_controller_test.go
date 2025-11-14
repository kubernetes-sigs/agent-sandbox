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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeClient(initialObjs ...runtime.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}).
		WithRuntimeObjects(initialObjs...).
		Build()
}

func sandboxControllerRef(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "agents.x-k8s.io/v1alpha1",
		Kind:               "Sandbox",
		Name:               name,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func TestComputeReadyCondition(t *testing.T) {
	r := &SandboxReconciler{}

	testCases := []struct {
		name           string
		sandbox        *sandboxv1alpha1.Sandbox
		err            error
		svc            *corev1.Service
		pod            *corev1.Pod
		expectedStatus metav1.ConditionStatus
		expectedReason string
	}{
		{
			name: "all ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expectedStatus: metav1.ConditionTrue,
			expectedReason: "DependenciesReady",
		},
		{
			name: "error",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err:            errors.New("test error"),
			svc:            &corev1.Service{},
			pod:            &corev1.Pod{},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "ReconcilerError",
		},
		{
			name: "pod not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "pod running but not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "pod pending",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: &corev1.Service{},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "service not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err: nil,
			svc: nil,
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
		{
			name: "all not ready",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			err:            nil,
			svc:            nil,
			pod:            nil,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			condition := r.computeReadyCondition(tc.sandbox, tc.err, tc.svc, tc.pod)
			require.Equal(t, sandboxv1alpha1.SandboxConditionReady.String(), condition.Type)
			require.Equal(t, tc.sandbox.Generation, condition.ObservedGeneration)
			require.Equal(t, tc.expectedStatus, condition.Status)
			require.Equal(t, tc.expectedReason, condition.Reason)
		})
	}
}

func TestReconcile(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	testCases := []struct {
		name        string
		initialObjs []runtime.Object
		sandboxSpec sandboxv1alpha1.SandboxSpec
		wantStatus  sandboxv1alpha1.SandboxStatus
		wantObjs    []client.Object
	}{
		{
			name: "minimal sandbox spec with Pod and Service",
			// Input sandbox spec
			sandboxSpec: sandboxv1alpha1.SandboxSpec{
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
							},
						},
					},
				},
			},
			// Verify Sandbox status
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Service:       sandboxName,
				ServiceFQDN:   "sandbox-name.sandbox-ns.svc.cluster.local",
				Replicas:      1,
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=ab179450", // Pre-computed hash of "sandbox-name"
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "DependenciesNotReady",
						Message:            "Pod exists with phase: ; Service Exists",
					},
				},
			},
			wantObjs: []client.Object{
				// Verify Pod
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
							},
						},
					},
				},
				// Verify Service
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
						},
						ClusterIP: "None",
					},
				},
			},
		},
		{
			name: "sandbox spec with PVC, Pod, and Service",
			// Input sandbox spec
			sandboxSpec: sandboxv1alpha1.SandboxSpec{
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
							},
						},
					},
					ObjectMeta: sandboxv1alpha1.PodMetadata{
						Labels: map[string]string{
							"custom-label": "label-val",
						},
						Annotations: map[string]string{
							"custom-annotation": "anno-val",
						},
					},
				},
				VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
					{
						EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
							Name: "my-pvc",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									"storage": resource.MustParse("10Gi"),
								},
							},
						},
					},
				},
			},
			// Verify Sandbox status
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Service:       sandboxName,
				ServiceFQDN:   "sandbox-name.sandbox-ns.svc.cluster.local",
				Replicas:      1,
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=ab179450", // Pre-computed hash of "sandbox-name"
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "DependenciesNotReady",
						Message:            "Pod exists with phase: ; Service Exists",
					},
				},
			},
			wantObjs: []client.Object{
				// Verify Pod
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
							"custom-label":                      "label-val",
						},
						Annotations: map[string]string{
							"custom-annotation": "anno-val",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "my-pvc",
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: "my-pvc-sandbox-name",
										ReadOnly:  false,
									},
								},
							},
						},
					},
				},
				// Verify Service
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
						},
						ClusterIP: "None",
					},
				},
				// Verify PVC
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "my-pvc-sandbox-name",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								"storage": resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sb := &sandboxv1alpha1.Sandbox{}
			sb.Name = sandboxName
			sb.Namespace = sandboxNs
			sb.Generation = 1
			sb.Spec = tc.sandboxSpec
			r := SandboxReconciler{
				Client: newFakeClient(append(tc.initialObjs, sb)...),
				Scheme: Scheme,
			}

			_, err := r.Reconcile(t.Context(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
			})
			require.NoError(t, err)
			// Validate Sandbox status
			liveSandbox := &sandboxv1alpha1.Sandbox{}
			require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, liveSandbox))
			opts := []cmp.Option{
				cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime"),
			}
			if diff := cmp.Diff(tc.wantStatus, liveSandbox.Status, opts...); diff != "" {
				t.Fatalf("unexpected sandbox status (-want,+got):\n%s", diff)
			}
			// Validate the other objects from the "cluster" (fake client)
			for _, obj := range tc.wantObjs {
				liveObj := obj.DeepCopyObject().(client.Object)
				err = r.Get(t.Context(), types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, liveObj)
				require.NoError(t, err)
				require.Equal(t, obj, liveObj)
			}
		})
	}
}

func TestReconcilePod(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	nameHash := "name-hash"
	sandboxObj := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: ptr.To(int32(1)),
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{
						"custom-label": "label-val",
					},
					Annotations: map[string]string{
						"custom-annotation": "anno-val",
					},
				},
			},
		},
	}
	testCases := []struct {
		name                   string
		initialObjs            []runtime.Object
		sandbox                *sandboxv1alpha1.Sandbox
		wantPod                *corev1.Pod
		expectErr              bool
		wantSandboxAnnotations map[string]string
	}{
		{
			name: "updates label and owner reference if Pod already exists",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "foo",
							},
						},
					},
				},
			},
			sandbox: sandboxObj,
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "foo",
						},
					},
				},
			},
		},
		{
			name:    "reconcilePod creates a new Pod",
			sandbox: sandboxObj,
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
						"custom-label":                      "label-val",
					},
					Annotations: map[string]string{
						"custom-annotation": "anno-val",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
			},
		},
		{
			name: "delete pod if replicas is 0",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: ptr.To(int32(0))},
			},
			wantPod: nil,
		},
		{
			name: "no-op if replicas is 0 and pod does not exist",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: ptr.To(int32(0)),
				},
			},
			wantPod: nil,
		},
		{
			name: "adopts existing pod via annotation - pod gets label and owner reference",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "adopted-pod-name",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "existing-container",
							},
						},
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						SanboxPodNameAnnotation: "adopted-pod-name",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: ptr.To(int32(1)),
					PodTemplate: sandboxv1alpha1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "test-container",
								},
							},
						},
					},
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "adopted-pod-name",
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						sandboxLabel: nameHash,
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "existing-container",
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "does not change controller if Pod already has a different controller",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						// Add a controller reference to a different controller
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "Deployment",
								Name:               "some-other-controller",
								UID:                "some-other-uid",
								Controller:         ptr.To(true),
								BlockOwnerDeletion: ptr.To(true),
							},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "foo",
							},
						},
					},
				},
			},
			sandbox: sandboxObj,
			// The pod should still have the original controller reference
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
					},
					// Should still have the original controller reference
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "Deployment",
							Name:               "some-other-controller",
							UID:                "some-other-uid",
							Controller:         ptr.To(true),
							BlockOwnerDeletion: ptr.To(true),
						},
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "foo",
						},
					},
				},
			},
		},
		{
			name:        "error when annotated pod does not exist",
			initialObjs: []runtime.Object{},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						SanboxPodNameAnnotation: "non-existent-pod",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: ptr.To(int32(1)),
					PodTemplate: sandboxv1alpha1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "test-container",
								},
							},
						},
					},
				},
			},
			wantPod:   nil,
			expectErr: true,
		},
		{
			name: "remove pod name annotation when replicas is 0",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "annotated-pod-name",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						SanboxPodNameAnnotation: "annotated-pod-name",
						"other-annotation":      "other-value",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: ptr.To(int32(0)),
				},
			},
			wantPod:                nil,
			expectErr:              false,
			wantSandboxAnnotations: map[string]string{"other-annotation": "other-value"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxReconciler{
				Client: newFakeClient(append(tc.initialObjs, tc.sandbox)...),
				Scheme: Scheme,
			}

			pod, err := r.reconcilePod(t.Context(), tc.sandbox, nameHash)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantPod, pod)

			// Validate the Pod from the "cluster" (fake client)
			if tc.wantPod != nil {
				livePod := &corev1.Pod{}
				err = r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, livePod)
				require.NoError(t, err)
				require.Equal(t, tc.wantPod, livePod)
			} else if !tc.expectErr {
				// When wantPod is nil and no error expected, verify pod doesn't exist
				livePod := &corev1.Pod{}
				podName := sandboxName
				// Check if there's an annotation with a non-empty value
				if annotatedPod, exists := tc.sandbox.Annotations[SanboxPodNameAnnotation]; exists && annotatedPod != "" {
					podName = annotatedPod
				}
				err = r.Get(t.Context(), types.NamespacedName{Name: podName, Namespace: sandboxNs}, livePod)
				require.True(t, k8serrors.IsNotFound(err))
			}

			// Check if sandbox annotations were updated as expected
			if tc.wantSandboxAnnotations != nil {
				// Fetch the sandbox to see if annotations were updated
				liveSandbox := &sandboxv1alpha1.Sandbox{}
				err = r.Get(t.Context(), types.NamespacedName{Name: tc.sandbox.Name, Namespace: tc.sandbox.Namespace}, liveSandbox)
				require.NoError(t, err)

				// Check if the annotations match what we expect
				require.Equal(t, tc.wantSandboxAnnotations, liveSandbox.Annotations)
			}
		})
	}
}

func TestSandboxExpiry(t *testing.T) {
	testCases := []struct {
		name         string
		shutdownTime *metav1.Time
		wantExpired  bool
		wantRequeue  bool
	}{
		{
			name:         "nil shutdown time",
			shutdownTime: nil,
			wantExpired:  false,
			wantRequeue:  false,
		},
		{
			name:         "shutdown time in future",
			shutdownTime: ptr.To(metav1.NewTime(time.Now().Add(2 * time.Hour))),
			wantExpired:  false,
			wantRequeue:  true,
		},
		{
			name:         "shutdown time in past",
			shutdownTime: ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Second))),
			wantExpired:  true,
			wantRequeue:  false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.Sandbox{}
			sandbox.Spec.ShutdownTime = tc.shutdownTime
			expired, requeueAfter := checkSandboxExpiry(sandbox)
			require.Equal(t, tc.wantExpired, expired)
			if tc.wantRequeue {
				require.Greater(t, requeueAfter, time.Duration(0))
			} else {
				require.Equal(t, time.Duration(0), requeueAfter)
			}
		})
	}
}

func TestSandboxCreationLatencyMetric(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	sb := &sandboxv1alpha1.Sandbox{}
	sb.Name = sandboxName
	sb.Namespace = sandboxNs
	sb.Generation = 1
	sb.CreationTimestamp = metav1.NewTime(time.Now())
	sb.Spec = sandboxv1alpha1.SandboxSpec{
		PodTemplate: sandboxv1alpha1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "test-container",
					},
				},
			},
		},
	}

	r := SandboxReconciler{
		Client: newFakeClient(sb),
		Scheme: Scheme,
	}

	_, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandboxName,
			Namespace: sandboxNs,
		},
	})
	require.NoError(t, err)

	// get pod and mark it ready
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		},
	}
	require.NoError(t, r.Status().Update(t.Context(), pod))

	_, err = r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandboxName,
			Namespace: sandboxNs,
		},
	})
	require.NoError(t, err)

	// Validate Sandbox status
	liveSandbox := &sandboxv1alpha1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, liveSandbox))
	require.True(t, meta.IsStatusConditionTrue(liveSandbox.Status.Conditions, "Ready"))
	require.NotNil(t, liveSandbox.Annotations)
	require.NotNil(t, liveSandbox.Annotations[readinessObserved])

	// Check metric
	expected := `
	# HELP sandbox_creation_latency Time taken from sandbox creation to sandbox ready in milliseconds
	# TYPE sandbox_creation_latency histogram
	sandbox_creation_latency_bucket{le="50"} 1

	sandbox_creation_latency_bucket{le="100"} 1
	sandbox_creation_latency_bucket{le="200"} 1
	sandbox_creation_latency_bucket{le="300"} 1
	sandbox_creation_latency_bucket{le="500"} 1
	sandbox_creation_latency_bucket{le="700"} 1
	sandbox_creation_latency_bucket{le="1000"} 1
	sandbox_creation_latency_bucket{le="1500"} 1
	sandbox_creation_latency_bucket{le="2000"} 1
	sandbox_creation_latency_bucket{le="3000"} 1
	sandbox_creation_latency_bucket{le="4500"} 1
	sandbox_creation_latency_bucket{le="6000"} 1
	sandbox_creation_latency_bucket{le="9000"} 1
	sandbox_creation_latency_bucket{le="12000"} 1
	sandbox_creation_latency_bucket{le="18000"} 1
	sandbox_creation_latency_bucket{le="30000"} 1
	sandbox_creation_latency_bucket{le="+Inf"} 1
	sandbox_creation_latency_count 1
	`
	err = testutil.CollectAndCompare(sandboxCreationLatency, strings.NewReader(expected), "sandbox_creation_latency")
	// We ignore the error because the sum is not deterministic
	if err != nil && !strings.Contains(err.Error(), "sandbox_creation_latency_sum") {
		require.NoError(t, err)
	}
}
