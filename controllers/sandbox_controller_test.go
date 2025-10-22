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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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
		name        string
		initialObjs []runtime.Object
		sandbox     *sandboxv1alpha1.Sandbox
		wantPod     *corev1.Pod
		expectErr   bool
	}{
		{
			name: "no-op if Pod already exists",
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
			wantPod: &corev1.Pod{ // Pod is not updated
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxReconciler{
				Client: newFakeClient(tc.initialObjs...),
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
			} else {
				livePod := &corev1.Pod{}
				err = r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, livePod)
				require.True(t, k8serrors.IsNotFound(err))
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

// TestTryAdoptPodFromPool tests the pod adoption mechanism from warm pool
func TestTryAdoptPodFromPool(t *testing.T) {
	sandboxName := "test-sandbox"
	sandboxNs := "test-ns"
	podTemplateHash := "testhash123"
	nameHash := NameHash(sandboxName)

	warmPoolOwnerRef := metav1.OwnerReference{
		APIVersion: "agents.x-k8s.io/v1alpha1",
		Kind:       "SandboxWarmPool",
		Name:       "warmpool",
		Controller: ptr.To(true),
	}

	testCases := []struct {
		name          string
		initialPods   []*corev1.Pod
		wantAdopted   bool
		wantPodName   string
		expectErr     bool
		validateAfter func(t *testing.T, adoptedPod *corev1.Pod)
	}{
		{
			name:        "no pods in warm pool",
			initialPods: []*corev1.Pod{},
			wantAdopted: false,
			expectErr:   false,
		},
		{
			name: "adopt single pod from warm pool",
			initialPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warmpool-pod-1",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						OwnerReferences:   []metav1.OwnerReference{warmPoolOwnerRef},
						CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
			},
			wantAdopted: true,
			wantPodName: "warmpool-pod-1",
			expectErr:   false,
			validateAfter: func(t *testing.T, adoptedPod *corev1.Pod) {
				require.NotNil(t, adoptedPod)
				// Verify pool labels were removed
				_, hasPoolLabel := adoptedPod.Labels[poolLabel]
				require.False(t, hasPoolLabel, "pool label should be removed")
				_, hasPodTemplateHashLabel := adoptedPod.Labels[podTemplateHashLabel]
				require.False(t, hasPodTemplateHashLabel, "podTemplateHash label should be removed")
				// Verify sandbox label was added
				require.Equal(t, nameHash, adoptedPod.Labels[sandboxLabel])
				// Verify owner reference was updated
				require.Len(t, adoptedPod.OwnerReferences, 1)
				require.Equal(t, "Sandbox", adoptedPod.OwnerReferences[0].Kind)
				require.Equal(t, sandboxName, adoptedPod.OwnerReferences[0].Name)
			},
		},
		{
			name: "adopt oldest pod when multiple available",
			initialPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warmpool-pod-newer",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						OwnerReferences:   []metav1.OwnerReference{warmPoolOwnerRef},
						CreationTimestamp: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warmpool-pod-oldest",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						OwnerReferences:   []metav1.OwnerReference{warmPoolOwnerRef},
						CreationTimestamp: metav1.NewTime(time.Now().Add(-15 * time.Minute)),
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
			},
			wantAdopted: true,
			wantPodName: "warmpool-pod-oldest",
			expectErr:   false,
		},
		{
			name: "skip pods being deleted",
			initialPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warmpool-pod-deleting",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						OwnerReferences:   []metav1.OwnerReference{warmPoolOwnerRef},
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{"test-finalizer"}, // Needed for fake client to accept deletionTimestamp
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
			},
			wantAdopted: false,
			expectErr:   false,
		},
		{
			name: "skip pods with different controller",
			initialPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-controller-pod",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "apps/v1",
								Kind:       "ReplicaSet",
								Name:       "other-controller",
								Controller: ptr.To(true),
							},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
			},
			wantAdopted: false,
			expectErr:   false,
		},
		{
			name: "adopt pod without owner reference",
			initialPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warmpool-pod-no-owner",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
			},
			wantAdopted: true,
			wantPodName: "warmpool-pod-no-owner",
			expectErr:   false,
		},
		{
			name: "skip first pod with different controller, adopt second valid pod",
			initialPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-controller-pod",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "apps/v1",
								Kind:       "Deployment",
								Name:       "other",
								Controller: ptr.To(true),
							},
						},
						CreationTimestamp: metav1.NewTime(time.Now().Add(-20 * time.Minute)),
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warmpool-pod-valid",
						Namespace: sandboxNs,
						Labels: map[string]string{
							podTemplateHashLabel: podTemplateHash,
						},
						OwnerReferences:   []metav1.OwnerReference{warmPoolOwnerRef},
						CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container"},
						},
					},
				},
			},
			wantAdopted: true,
			wantPodName: "warmpool-pod-valid",
			expectErr:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       "test-uid",
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: ptr.To(int32(1)),
				},
			}

			// Convert pods to runtime.Object
			initialObjs := make([]runtime.Object, len(tc.initialPods))
			for i, pod := range tc.initialPods {
				initialObjs[i] = pod
			}

			r := &SandboxReconciler{
				Client: newFakeClient(initialObjs...),
				Scheme: Scheme,
			}

			adoptedPod, err := r.tryAdoptPodFromPool(t.Context(), sandbox, podTemplateHash)

			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tc.wantAdopted {
				require.NotNil(t, adoptedPod, "expected a pod to be adopted")
				require.Equal(t, tc.wantPodName, adoptedPod.Name)

				// Verify the pod was updated in the client
				livePod := &corev1.Pod{}
				err = r.Get(t.Context(), types.NamespacedName{Name: adoptedPod.Name, Namespace: sandboxNs}, livePod)
				require.NoError(t, err)

				// Run additional validation if provided
				if tc.validateAfter != nil {
					tc.validateAfter(t, livePod)
				}
			} else {
				require.Nil(t, adoptedPod, "expected no pod to be adopted")
			}
		})
	}
}

// TestReconcilePodWithWarmPool tests the full reconcilePod flow with warm pool adoption
func TestReconcilePodWithWarmPool(t *testing.T) {
	sandboxName := "test-sandbox"
	sandboxNs := "test-ns"
	nameHash := NameHash(sandboxName)

	// Create a sandbox with a specific pod template
	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       "test-uid",
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: ptr.To(int32(1)),
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:latest",
						},
					},
				},
			},
		},
	}

	// Compute the expected hash for this template
	expectedHash, err := hashPodTemplate(sandbox.Spec.PodTemplate)
	require.NoError(t, err)

	// Create a warm pool pod with the matching hash
	warmPoolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warmpool-pod-123",
			Namespace: sandboxNs,
			Labels: map[string]string{
				podTemplateHashLabel: expectedHash,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "agents.x-k8s.io/v1alpha1",
					Kind:       "SandboxWarmPool",
					Name:       "warmpool",
					Controller: ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "nginx:latest",
				},
			},
		},
	}

	r := &SandboxReconciler{
		Client: newFakeClient(warmPoolPod),
		Scheme: Scheme,
	}

	// Reconcile the pod - should adopt from warm pool
	adoptedPod, err := r.reconcilePod(t.Context(), sandbox, nameHash)
	require.NoError(t, err)
	require.NotNil(t, adoptedPod)
	require.Equal(t, "warmpool-pod-123", adoptedPod.Name)

	// Verify the pod was properly adopted
	livePod := &corev1.Pod{}
	err = r.Get(t.Context(), types.NamespacedName{Name: "warmpool-pod-123", Namespace: sandboxNs}, livePod)
	require.NoError(t, err)

	// Verify labels were updated
	require.Equal(t, nameHash, livePod.Labels[sandboxLabel])
	_, hasPoolLabel := livePod.Labels[poolLabel]
	require.False(t, hasPoolLabel)
	_, hasPodTemplateHashLabel := livePod.Labels[podTemplateHashLabel]
	require.False(t, hasPodTemplateHashLabel)

	// Verify owner reference was updated
	require.Len(t, livePod.OwnerReferences, 1)
	require.Equal(t, "Sandbox", livePod.OwnerReferences[0].Kind)
	require.Equal(t, sandboxName, livePod.OwnerReferences[0].Name)
}
