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
<<<<<<< HEAD
	"context"
	"errors"
=======
>>>>>>> daba23d (Add a KEP to discuss the status field of Sandbox.)
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func newFakeClient(initialObjs ...runtime.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}).
		WithRuntimeObjects(initialObjs...).
		Build()
}

type alreadyExistsOnPodCreateClient struct {
	client.WithWatch
	pod *corev1.Pod
}

func (c *alreadyExistsOnPodCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		if c.pod != nil {
			pod := c.pod.DeepCopy()
			if err := c.WithWatch.Create(ctx, pod); err != nil && !k8serrors.IsAlreadyExists(err) {
				return err
			}
			c.pod = nil
		}
		return k8serrors.NewAlreadyExists(corev1.Resource("pods"), obj.GetName())
	}
	return c.WithWatch.Create(ctx, obj, opts...)
}

const sandboxUID = types.UID("test-sandbox-uid")

func sandboxControllerRef(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "agents.x-k8s.io/v1alpha1",
		Kind:               "Sandbox",
		Name:               name,
		UID:                sandboxUID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}
}

func TestComputeConditions(t *testing.T) {
	r := &SandboxReconciler{}

	gen := int64(1)
	sbWithRepl := func(replicas int32) *sandboxv1alpha1.Sandbox {
		return &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec:       sandboxv1alpha1.SandboxSpec{Replicas: ptr.To(replicas)},
		}
	}

	testCases := []struct {
		name               string
		sandbox            *sandboxv1alpha1.Sandbox
		svcsProvisioned    bool
		pvcsProvisioned    bool
		pod                *corev1.Pod
		expectedConditions []metav1.Condition
	}{
		{
			name:            "1. Provisioning - No dependencies",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: false,
			pvcsProvisioned: false,
			pod:             nil,
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "False", ObservedGeneration: gen, Reason: "SandboxInitializing", Message: "Provisioning dependencies"},
				{Type: "Suspended", Status: "Unknown", ObservedGeneration: gen, Reason: "PendingEvaluation", Message: "The suspension status has not yet been determined."},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxInitializing", Message: "Waiting for Sandbox to be provisioned"},
			},
		},
		{
			name:            "2. Provisioning - Partial dependencies (missing service)",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: false,
			pvcsProvisioned: true,
			pod:             nil,
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "False", ObservedGeneration: gen, Reason: "SandboxInitializing", Message: "Provisioning dependencies"},
				{Type: "Suspended", Status: "Unknown", ObservedGeneration: gen, Reason: "PendingEvaluation", Message: "The suspension status has not yet been determined."},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxInitializing", Message: "Waiting for Sandbox to be provisioned"},
			},
		},
		{
			name:            "3. Dependencies provisioned, Pod missing",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod:             nil,
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "Unknown", ObservedGeneration: gen, Reason: "PendingEvaluation", Message: "The suspension status has not yet been determined."},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "PodProvisioning", Message: "Pod is initializing"},
			},
		},
		{
			name:            "4. Pod Pending",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod:             &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is operational and not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "PodProvisioning", Message: "Pod is in phase: Pending"},
			},
		},
		{
			name:            "5. Pod Running but not Ready",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:  corev1.PodRunning,
					PodIPs: []corev1.PodIP{{IP: "10.244.0.1"}},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is operational and not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "PodProvisioning", Message: "Pod is Running but not Ready"},
			},
		},
		{
<<<<<<< HEAD
			name: "pod ready but no IP yet",
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
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "DependenciesNotReady",
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
=======
			name:            "6. Operational Sandbox - Fully Ready",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: true,
			pvcsProvisioned: true,
>>>>>>> 94fa3f1 (Add a KEP to discuss the status field of Sandbox.)
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is operational and not suspended"},
				{Type: "Ready", Status: "True", ObservedGeneration: gen, Reason: "SandboxReady", Message: "Sandbox is operational"},
			},
		},
		{
			name:            "7. Suspended by user - Pod still terminating",
			sandbox:         sbWithRepl(0),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "True", ObservedGeneration: gen, Reason: "UserSuspended", Message: "Sandbox has been suspended by the user"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxSuspended", Message: "Sandbox is suspended"},
			},
		},
		{
			name:            "8. Fully suspended - Pod deleted",
			sandbox:         sbWithRepl(0),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod:             nil,
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "True", ObservedGeneration: gen, Reason: "UserSuspended", Message: "Sandbox has been suspended by the user"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxSuspended", Message: "Sandbox is suspended"},
			},
		},
		{
			name:            "9. Resuming - Pod missing",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod:             nil,
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "Unknown", ObservedGeneration: gen, Reason: "PendingEvaluation", Message: "The suspension status has not yet been determined."},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "PodProvisioning", Message: "Pod is initializing"},
			},
		},
		{
			name:            "10. Unresponsive - Pod Status Unknown",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod:             &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodUnknown}},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is operational and not suspended"},
				{Type: "Ready", Status: "Unknown", ObservedGeneration: gen, Reason: "SandboxUnresponsive", Message: "Pod status is unknown"},
			},
		},
		{
			name:            "11. Pod Failed - Crash Loop",
			sandbox:         sbWithRepl(1),
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod:             &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is operational and not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "PodProvisioning", Message: "Pod is in phase: Failed"},
			},
		},
		{
			name:            "12. Suspended but missing dependencies",
			sandbox:         sbWithRepl(0),
			svcsProvisioned: false,
			pvcsProvisioned: false,
			pod:             nil,
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "False", ObservedGeneration: gen, Reason: "SandboxInitializing", Message: "Provisioning dependencies"},
				{Type: "Suspended", Status: "True", ObservedGeneration: gen, Reason: "UserSuspended", Message: "Sandbox has been suspended by the user"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxInitializing", Message: "Waiting for Sandbox to be provisioned"},
			},
		},
		{
<<<<<<< HEAD
			name: "13. Sandbox Deleting (DeletionTimestamp set)",
=======
			name: "13. User Initiated Termination (DeletionTimestamp set)",
>>>>>>> 84dd30e (Add a KEP to discuss the status field of Sandbox.)
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation:        gen,
					DeletionTimestamp: ptr.To(metav1.Now()),
				},
				Spec: sandboxv1alpha1.SandboxSpec{Replicas: ptr.To(int32(1))},
			},
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is operational and not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxDeleting", Message: "Sandbox is terminating"},
			},
		},
		{
			name: "14. Sandbox Expiration (Expired)",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Generation: gen},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: ptr.To(int32(1)),
					Lifecycle: sandboxv1alpha1.Lifecycle{
						ShutdownTime: ptr.To(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
					},
				},
			},
			svcsProvisioned: true,
			pvcsProvisioned: true,
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Initialized", Status: "True", ObservedGeneration: gen, Reason: "SandboxInitialized", Message: "Service and PVCs are provisioned"},
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is operational and not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxExpired", Message: "Sandbox has expired"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conditions := r.computeConditions(tc.sandbox, tc.svcsProvisioned, tc.pod, tc.pvcsProvisioned)
			opts := []cmp.Option{
				cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime"),
			}
			if diff := cmp.Diff(tc.expectedConditions, conditions, opts...); diff != "" {
				t.Fatalf("unexpected conditions (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestResolvePodName(t *testing.T) {
	testCases := []struct {
		name        string
		annotations map[string]string
		wantPodName string
	}{
		{
			name:        "no annotations",
			annotations: nil,
			wantPodName: "my-sandbox",
		},
		{
			name:        "annotation not present",
			annotations: map[string]string{"other": "value"},
			wantPodName: "my-sandbox",
		},
		{
			name:        "annotation present but empty",
			annotations: map[string]string{sandboxv1alpha1.SandboxPodNameAnnotation: ""},
			wantPodName: "my-sandbox",
		},
		{
			name:        "annotation present with warm pool pod name",
			annotations: map[string]string{sandboxv1alpha1.SandboxPodNameAnnotation: "warmpool-abc-xyz"},
			wantPodName: "warmpool-abc-xyz",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "my-sandbox",
					Namespace:   "default",
					Annotations: tc.annotations,
				},
			}
			got := resolvePodName(sandbox)
			require.Equal(t, tc.wantPodName, got)
		})
	}
}

func TestReconcile(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	testCases := []struct {
		name                 string
		initialObjs          []runtime.Object
		sandboxSpec          sandboxv1alpha1.SandboxSpec
<<<<<<< HEAD
		sandboxAnnotations   map[string]string
<<<<<<< HEAD
		reconcileCount       int
=======
=======
		deletionTimestamp    *metav1.Time
>>>>>>> 94fa3f1 (Add a KEP to discuss the status field of Sandbox.)
>>>>>>> daba23d (Add a KEP to discuss the status field of Sandbox.)
		wantStatus           sandboxv1alpha1.SandboxStatus
		wantObjs             []client.Object
		wantDeletedObjs      []client.Object
		wantSurvivingObjs    []client.Object
		expectSandboxDeleted bool
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
						Type:               string(sandboxv1alpha1.SandboxConditionInitialized),
						Status:             metav1.ConditionTrue,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonInitialized,
						Message:            "Service and PVCs are provisioned",
					},
					{
						Type:               string(sandboxv1alpha1.SandboxConditionSuspended),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonNotSuspended,
						Message:            "Sandbox is operational and not suspended",
					},
					{
						Type:               string(sandboxv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonPodProvisioning,
						Message:            "Pod is in phase: ",
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
							Name:        "my-pvc",
							Labels:      map[string]string{"custom-label": "label-val"},
							Annotations: map[string]string{"custom-annotation": "anno-val"},
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
						Type:               string(sandboxv1alpha1.SandboxConditionInitialized),
						Status:             metav1.ConditionTrue,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonInitialized,
						Message:            "Service and PVCs are provisioned",
					},
					{
						Type:               string(sandboxv1alpha1.SandboxConditionSuspended),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonNotSuspended,
						Message:            "Sandbox is operational and not suspended",
					},
					{
						Type:               string(sandboxv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonPodProvisioning,
						Message:            "Pod is in phase: ",
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
							"custom-annotation":                      "anno-val",
							"agents.x-k8s.io/propagated-labels":      "custom-label",
							"agents.x-k8s.io/propagated-annotations": "custom-annotation",
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
						Name:      "my-pvc-sandbox-name",
						Namespace: sandboxNs,
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
							"custom-label":                      "label-val",
						},
						Annotations:     map[string]string{"custom-annotation": "anno-val"},
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
		{
			name: "sandbox with existing pod propagates PodIPs",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": "ab179450",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
					Status: corev1.PodStatus{
						PodIPs: []corev1.PodIP{{IP: "10.244.0.5"}, {IP: "fd00::5"}},
						Phase:  corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			sandboxSpec: sandboxv1alpha1.SandboxSpec{
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Service:       sandboxName,
				ServiceFQDN:   "sandbox-name.sandbox-ns.svc.cluster.local",
				Replicas:      1,
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=ab179450",
				PodIPs:        []string{"10.244.0.5", "fd00::5"},
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             "DependenciesReady",
						Message:            "Pod is Ready; Service Exists",
					},
				},
			},
			wantObjs: []client.Object{
				// Verifying Service exists (Pod was verified indirectly via state, and owner reference is added in reconcilePod test suite)
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
			name:           "sandbox expired with retain policy",
			reconcileCount: 2,
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
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
				Lifecycle: sandboxv1alpha1.Lifecycle{
					ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
					ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyRetain),
				},
			},
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1alpha1.SandboxConditionReady),
						Status:             "False",
						ObservedGeneration: 1,
<<<<<<< HEAD
						Reason:             sandboxv1alpha1.SandboxReasonExpired,
=======
						Reason:             sandboxv1alpha1.SandboxReasonSystemTermination,
>>>>>>> 84dd30e (Add a KEP to discuss the status field of Sandbox.)
						Message:            "Sandbox has expired",
					},
				},
			},
			wantDeletedObjs: []client.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
		},
		{
			name:           "sandbox expired with retain policy deletes adopted warm pool pod",
			reconcileCount: 2,
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "warmpool-abc-xyz",
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
			sandboxAnnotations: map[string]string{
				sandboxv1alpha1.SandboxPodNameAnnotation: "warmpool-abc-xyz",
			},
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
				Lifecycle: sandboxv1alpha1.Lifecycle{
					ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
					ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyRetain),
				},
			},
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "SandboxExpired",
						Message:            "Sandbox has expired",
					},
				},
			},
			wantDeletedObjs: []client.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "warmpool-abc-xyz", Namespace: sandboxNs}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
		},
		{
			name:           "sandbox expired with delete policy",
			reconcileCount: 2,
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
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
				Lifecycle: sandboxv1alpha1.Lifecycle{
					ShutdownTime:   new(metav1.NewTime(time.Now().Add(-30 * time.Minute))),
					ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyDelete),
				},
			},
			wantDeletedObjs: []client.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
				&sandboxv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
			expectSandboxDeleted: true,
		},
		{
<<<<<<< HEAD
<<<<<<< HEAD
			name:           "sandbox expired skips deletion of pod owned by different controller",
			reconcileCount: 2,
=======
=======
>>>>>>> 91028f7 (Address comments.)
<<<<<<< HEAD
			name: "sandbox expired skips deletion of pod owned by different controller",
>>>>>>> daba23d (Add a KEP to discuss the status field of Sandbox.)
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "Deployment",
								Name:               "other-deployment",
								UID:                "other-uid",
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
=======
			name:              "user initiated termination (DeletionTimestamp set)",
=======
			name:              "Sandbox deleting (DeletionTimestamp set)",
>>>>>>> e3f32c1 (Address comments.)
			deletionTimestamp: ptr.To(metav1.Now()),
			sandboxSpec: sandboxv1alpha1.SandboxSpec{
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
>>>>>>> 94fa3f1 (Add a KEP to discuss the status field of Sandbox.)
							},
						},
					},
				},
<<<<<<< HEAD
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
			sandboxSpec: sandboxv1alpha1.SandboxSpec{
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
				Lifecycle: sandboxv1alpha1.Lifecycle{
					ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
					ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyRetain),
				},
=======
>>>>>>> 94fa3f1 (Add a KEP to discuss the status field of Sandbox.)
			},
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
<<<<<<< HEAD
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "SandboxExpired",
						Message:            "Sandbox has expired",
					},
				},
			},
			// Pod should NOT be deleted (owned by other), Service SHOULD be deleted (owned by sandbox)
			wantDeletedObjs: []client.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
			wantSurvivingObjs: []client.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
		},
		{
			name:           "sandbox expired skips deletion of unowned pod",
			reconcileCount: 2,
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
						// No owner references
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
			sandboxSpec: sandboxv1alpha1.SandboxSpec{
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
				Lifecycle: sandboxv1alpha1.Lifecycle{
					ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
					ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyRetain),
				},
			},
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "SandboxExpired",
						Message:            "Sandbox has expired",
					},
				},
			},
			wantDeletedObjs: []client.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
			wantSurvivingObjs: []client.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
		},
		{
			name:           "sandbox expired with no matching pod or service",
			reconcileCount: 2,
			sandboxSpec: sandboxv1alpha1.SandboxSpec{
				PodTemplate: sandboxv1alpha1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
				Lifecycle: sandboxv1alpha1.Lifecycle{
					ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
					ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyRetain),
				},
			},
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "SandboxExpired",
						Message:            "Sandbox has expired",
=======
						Type:               string(sandboxv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonDeleting,
						Message:            "Sandbox is terminating",
>>>>>>> 94fa3f1 (Add a KEP to discuss the status field of Sandbox.)
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
			sb.UID = sandboxUID
			sb.Generation = 1
			if tc.deletionTimestamp != nil {
				sb.DeletionTimestamp = tc.deletionTimestamp
				sb.Finalizers = []string{"test-finalizer"}
			}
			sb.Spec = tc.sandboxSpec
			if tc.sandboxAnnotations != nil {
				sb.Annotations = tc.sandboxAnnotations
			}
			r := SandboxReconciler{
				Client:        newFakeClient(append(tc.initialObjs, sb)...),
				Scheme:        Scheme,
				Tracer:        asmetrics.NewNoOp(),
				ClusterDomain: "cluster.local",
			}

			reconcileCount := tc.reconcileCount
			if reconcileCount == 0 {
				reconcileCount = 1
			}
			var err error
			for i := 0; i < reconcileCount; i++ {
				_, err = r.Reconcile(t.Context(), ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      sandboxName,
						Namespace: sandboxNs,
					},
				})
				require.NoError(t, err)
			}
			// Validate Sandbox status or deletion
			liveSandbox := &sandboxv1alpha1.Sandbox{}
			err = r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, liveSandbox)
			if tc.expectSandboxDeleted {
				require.True(t, k8serrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				opts := []cmp.Option{
					cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime"),
				}
				if diff := cmp.Diff(tc.wantStatus, liveSandbox.Status, opts...); diff != "" {
					t.Fatalf("unexpected sandbox status (-want,+got):\n%s", diff)
				}
			}
			// Validate the other objects from the "cluster" (fake client)
			for _, obj := range tc.wantObjs {
				liveObj := obj.DeepCopyObject().(client.Object)
				err = r.Get(t.Context(), types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, liveObj)
				require.NoError(t, err)
				require.Equal(t, obj, liveObj)
			}
			for _, obj := range tc.wantDeletedObjs {
				liveObj := obj.DeepCopyObject().(client.Object)
				err = r.Get(t.Context(), types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, liveObj)
				require.True(t, k8serrors.IsNotFound(err))
			}
			for _, obj := range tc.wantSurvivingObjs {
				liveObj := obj.DeepCopyObject().(client.Object)
				err = r.Get(t.Context(), types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, liveObj)
				require.NoError(t, err, "expected object %q/%q to survive but it was deleted or not found",
					obj.GetNamespace(), obj.GetName())
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
			UID:       sandboxUID,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: new(int32(1)),
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
		wrapClient             func(client.WithWatch) client.WithWatch
		sandbox                *sandboxv1alpha1.Sandbox
		wantPod                *corev1.Pod
		expectErr              bool
		wantSandboxAnnotations map[string]string
		wantPodSurvives        string // if set, verify this pod still exists after reconcile
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
						"custom-label":                      "label-val",
					},
					Annotations: map[string]string{
						"custom-annotation":                      "anno-val",
						"agents.x-k8s.io/propagated-labels":      "custom-label",
						"agents.x-k8s.io/propagated-annotations": "custom-annotation",
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
			wantSandboxAnnotations: map[string]string{
				sandboxv1alpha1.SandboxPodNameAnnotation: sandboxName,
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
						"custom-annotation":                      "anno-val",
						"agents.x-k8s.io/propagated-labels":      "custom-label",
						"agents.x-k8s.io/propagated-annotations": "custom-annotation",
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
			wantSandboxAnnotations: map[string]string{
				sandboxv1alpha1.SandboxPodNameAnnotation: sandboxName,
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
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(0)),
				},
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
					Replicas: new(int32(0)),
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
					UID:       sandboxUID,
					Annotations: map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "adopted-pod-name",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(1)),
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
			name: "refuses to modify pod owned by a different controller",
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
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
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
			sandbox:   sandboxObj,
			wantPod:   nil,
			expectErr: true,
		},
		{
			name: "refuses pod created by another controller between get and create",
			wrapClient: func(c client.WithWatch) client.WithWatch {
				return &alreadyExistsOnPodCreateClient{
					WithWatch: c,
					pod: &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:            sandboxName,
							Namespace:       sandboxNs,
							ResourceVersion: "1",
							OwnerReferences: []metav1.OwnerReference{
								{
									APIVersion:         "apps/v1",
									Kind:               "Deployment",
									Name:               "some-other-controller",
									UID:                "some-other-uid",
									Controller:         new(true),
									BlockOwnerDeletion: new(true),
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
				}
			},
			sandbox:   sandboxObj,
			wantPod:   nil,
			expectErr: true,
		},
		{
			name: "creates pod with volumeClaimTemplate volumes replacing conflicting pod template volumes",
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(1)),
					PodTemplate: sandboxv1alpha1.PodTemplate{
						ObjectMeta: sandboxv1alpha1.PodMetadata{
							Annotations: map[string]string{
								"agents.x-k8s.io/template": "default",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "test-container",
									VolumeMounts: []corev1.VolumeMount{
										{Name: "data", MountPath: "/data"},
									},
								},
							},
							Volumes: []corev1.Volume{
								{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
								{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"}}}},
							},
						},
					},
					VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
						{
							EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{Name: "data"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							},
						},
					},
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						sandboxLabel: nameHash,
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/template":               "default",
						"agents.x-k8s.io/propagated-annotations": "agents.x-k8s.io/template",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data"},
							},
						},
					},
					Volumes: []corev1.Volume{
						// config preserved, data replaced by PVC
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"}}}},
						{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-" + sandboxName}}},
					},
				},
			},
			wantSandboxAnnotations: map[string]string{
				sandboxv1alpha1.SandboxPodNameAnnotation: sandboxName,
			},
		},
		{
			name:        "annotated pod missing with replicas=1: clears annotation and recreates pod",
			initialObjs: []runtime.Object{},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Annotations: map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "non-existent-pod",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(1)),
					PodTemplate: sandboxv1alpha1.PodTemplate{
						ObjectMeta: sandboxv1alpha1.PodMetadata{
							Annotations: map[string]string{
								"agents.x-k8s.io/template": "default",
							},
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
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						sandboxLabel: nameHash,
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/template":               "default",
						"agents.x-k8s.io/propagated-annotations": "agents.x-k8s.io/template",
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
			expectErr: false,
			wantSandboxAnnotations: map[string]string{
				sandboxv1alpha1.SandboxPodNameAnnotation: sandboxName,
			},
		},
		{
			name:        "annotated pod missing with replicas=0: clears annotation and does not recreate pod",
			initialObjs: []runtime.Object{},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Annotations: map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "non-existent-pod",
						"other-annotation":                       "keep-me",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32),
				},
			},
			wantPod:   nil,
			expectErr: false,
			wantSandboxAnnotations: map[string]string{
				"other-annotation": "keep-me",
			},
		},
		{
			name: "refuses to delete annotated pod owned by a different controller",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "victim-pod",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "Deployment",
								Name:               "other-deployment",
								UID:                "other-uid",
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c"}},
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "victim-pod",
						"other-annotation":                       "keep-me",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(0)),
				},
			},
			wantPod:                nil,
			expectErr:              false,
			wantSandboxAnnotations: map[string]string{"other-annotation": "keep-me"},
			wantPodSurvives:        "victim-pod",
		},
		{
			name: "refuses to delete annotated pod with no controller reference",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "unowned-pod",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c"}},
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "unowned-pod",
						"other-annotation":                       "keep-me",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(0)),
				},
			},
			wantPod:                nil,
			expectErr:              false,
			wantSandboxAnnotations: map[string]string{"other-annotation": "keep-me"},
			wantPodSurvives:        "unowned-pod",
		},
		{
			name: "deletes annotated pod owned by this sandbox",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "owned-pod",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c"}},
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Annotations: map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "owned-pod",
						"other-annotation":                       "keep-me",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(0)),
				},
			},
			wantPod:                nil,
			expectErr:              false,
			wantSandboxAnnotations: map[string]string{"other-annotation": "keep-me"},
		},
		{
			name: "refuses to adopt annotated pod owned by a different controller",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "foreign-pod",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "Deployment",
								Name:               "other-deployment",
								UID:                "other-uid",
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c"}},
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1alpha1.SandboxPodNameAnnotation: "foreign-pod",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(1)),
					PodTemplate: sandboxv1alpha1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "test-container"}},
						},
					},
				},
			},
			wantPod:                nil,
			expectErr:              true,
			wantSandboxAnnotations: map[string]string{},
		},
		{
			name: "refuses to delete unowned annotated pod and removes annotation when replicas is 0",
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
						sandboxv1alpha1.SandboxPodNameAnnotation: "annotated-pod-name",
						"other-annotation":                       "other-value",
					},
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(0)),
				},
			},
			wantPod:                nil,
			expectErr:              false,
			wantSandboxAnnotations: map[string]string{"other-annotation": "other-value"},
			wantPodSurvives:        "annotated-pod-name",
		},
		{
			name: "reconcilePod deletes label and annotation removed from sandbox",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							sandboxLabel:                   nameHash,
							"remove-label":                 "value",
							"keep-label":                   "value",
							"agents.x-k8s.io/system-label": "value",
						},
						Annotations: map[string]string{
							"remove-annotation":                      "value",
							"keep-annotation":                        "value",
							"kubernetes.io/system-annotation":        "value",
							"agents.x-k8s.io/propagated-labels":      "remove-label,keep-label",
							"agents.x-k8s.io/propagated-annotations": "remove-annotation,keep-annotation",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			sandbox: &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					Replicas: new(int32(1)),
					PodTemplate: sandboxv1alpha1.PodTemplate{
						ObjectMeta: sandboxv1alpha1.PodMetadata{
							Labels: map[string]string{
								"keep-label": "value",
							},
							Annotations: map[string]string{
								"keep-annotation": "value",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "test-container"}},
						},
					},
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						sandboxLabel:                   nameHash,
						"keep-label":                   "value",
						"agents.x-k8s.io/system-label": "value",
					},
					Annotations: map[string]string{
						"keep-annotation":                        "value",
						"kubernetes.io/system-annotation":        "value",
						"agents.x-k8s.io/propagated-labels":      "keep-label",
						"agents.x-k8s.io/propagated-annotations": "keep-annotation",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
			wantSandboxAnnotations: map[string]string{
				sandboxv1alpha1.SandboxPodNameAnnotation: sandboxName,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := tc.sandbox.DeepCopy()

			fakeClient := newFakeClient(append(tc.initialObjs, sandbox)...)
			if tc.wrapClient != nil {
				fakeClient = tc.wrapClient(fakeClient)
			}

			r := SandboxReconciler{
				Client:        fakeClient,
				Scheme:        Scheme,
				Tracer:        asmetrics.NewNoOp(),
				ClusterDomain: "cluster.local",
			}

			pod, err := r.reconcilePod(t.Context(), sandbox, nameHash)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			// Validate the Pod from the "cluster" (fake client)
			if tc.wantPod != nil {
				require.NotNil(t, pod, "expected pod to be returned")
				livePod := &corev1.Pod{}
				err = r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, livePod)
				require.NoError(t, err)
				require.Equal(t, tc.wantPod, livePod)
			} else if !tc.expectErr {
<<<<<<< HEAD
				if tc.wantPodSurvives != "" {
					// Pod should still exist (ownership check blocked deletion)
					livePod := &corev1.Pod{}
					err = r.Get(t.Context(), types.NamespacedName{Name: tc.wantPodSurvives, Namespace: sandboxNs}, livePod)
					require.NoError(t, err, "expected pod %q to survive but it was deleted", tc.wantPodSurvives)
				} else {
					// When wantPod is nil and no error expected, verify pod doesn't exist
					livePod := &corev1.Pod{}
					podName := sandboxName
					if annotatedPod, exists := tc.sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; exists && annotatedPod != "" {
						podName = annotatedPod
					}
					err = r.Get(t.Context(), types.NamespacedName{Name: podName, Namespace: sandboxNs}, livePod)
					require.True(t, k8serrors.IsNotFound(err))
=======
				// When wantPod is nil and no error expected, verify pod doesn't exist
				livePod := &corev1.Pod{}
				podName := sandboxName
				// Check if there's an annotation with a non-empty value
				if annotatedPod, exists := tc.sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; exists && annotatedPod != "" {
					podName = annotatedPod
>>>>>>> 94fa3f1 (Add a KEP to discuss the status field of Sandbox.)
				}
			}

			if tc.wantSandboxAnnotations != nil {
				liveSandbox := &sandboxv1alpha1.Sandbox{}
				err = r.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, liveSandbox)
				require.NoError(t, err)
				if len(tc.wantSandboxAnnotations) == 0 {
					require.Empty(t, liveSandbox.Annotations)
				} else {
					require.Equal(t, tc.wantSandboxAnnotations, liveSandbox.Annotations)
				}
			}
		})
	}
}

func TestReconcileService(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	nameHash := "name-hash"
	sandboxObj := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: new(int32(1)),
		},
	}

	testCases := []struct {
		name                  string
		initialObjs           []runtime.Object
		sandbox               *sandboxv1alpha1.Sandbox
		wantService           *corev1.Service
		expectErr             bool
		errContains           string // substring that must appear in the error
		wantStatusService     string
		wantStatusServiceFQDN string
	}{
		{
			name:    "creates a new headless service when none exists",
			sandbox: sandboxObj,
			wantService: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						sandboxLabel: nameHash,
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "None",
					Selector: map[string]string{
						sandboxLabel: nameHash,
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "uses existing service owned by this sandbox",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
			sandbox:               sandboxObj,
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},

		{
			name: "repairs selector and label drift on service owned by this sandbox",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"keep": "me",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{
							"app": "something-else",
						},
					},
				},
			},
			sandbox: sandboxObj,
			wantService: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"keep":       "me",
						sandboxLabel: nameHash,
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						sandboxLabel: nameHash,
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},

		{
			name: "refuses to use service owned by a different controller",

			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "Deployment",
								Name:               "some-other-controller",
								UID:                "some-other-uid",
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						},
					},
				},
			},
			sandbox:     sandboxObj,
			wantService: nil,
			expectErr:   true,
		},
		{
			name: "adopts unowned service and sets controller reference",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
				},
			},
			sandbox: sandboxObj,
			wantService: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "refuses to adopt unowned service with non-headless ClusterIP",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.ServiceSpec{
						ClusterIP: "10.96.0.100",
					},
				},
			},
			sandbox:     sandboxObj,
			wantService: nil,
			expectErr:   true,
			errContains: "immutable",
		},
		{
			name: "adopts unowned headless service and overwrites wrong selector",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.ServiceSpec{
						ClusterIP: "None",
						Selector: map[string]string{
							"app": "something-else",
						},
					},
				},
			},
			sandbox: sandboxObj,
			wantService: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "None",
					Selector: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxReconciler{
				Client:        newFakeClient(append(tc.initialObjs, tc.sandbox)...),
				Scheme:        Scheme,
				Tracer:        asmetrics.NewNoOp(),
				ClusterDomain: "cluster.local",
			}

			svc, err := r.reconcileService(t.Context(), tc.sandbox, nameHash)
			if tc.expectErr {
				require.Error(t, err)
				require.Nil(t, svc)
				if tc.errContains != "" {
					require.Contains(t, err.Error(), tc.errContains)
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, svc)
			}

			// Verify status was set correctly
			if tc.wantStatusService != "" {
				require.Equal(t, tc.wantStatusService, tc.sandbox.Status.Service)
				require.Equal(t, tc.wantStatusServiceFQDN, tc.sandbox.Status.ServiceFQDN)
			}

			// Verify the live service in the fake client matches expected state
			if tc.wantService != nil {
				liveSvc := &corev1.Service{}
				err = r.Get(t.Context(), types.NamespacedName{
					Name: sandboxName, Namespace: sandboxNs,
				}, liveSvc)
				require.NoError(t, err)
				if diff := cmp.Diff(tc.wantService, liveSvc, cmpopts.IgnoreFields(metav1.TypeMeta{}, "APIVersion", "Kind")); diff != "" {
					t.Errorf("live service mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestCheckOwnership(t *testing.T) {
	sandboxName := "test-sandbox"
	sandboxUID := types.UID("sandbox-uid-123")

	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: sandboxName,
			UID:  sandboxUID,
		},
	}

	otherOwnerRef := metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               "Deployment",
		Name:               "other-controller",
		UID:                "other-uid",
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}

	sandboxOwnerRef := metav1.OwnerReference{
		APIVersion:         "agents.x-k8s.io/v1alpha1",
		Kind:               "Sandbox",
		Name:               sandboxName,
		UID:                sandboxUID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}

	testCases := []struct {
		name              string
		obj               client.Object
		wantOwnership     resourceOwnership
		wantControllerRef *metav1.OwnerReference
	}{
		{
			name: "pod owned by sandbox",
			obj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					OwnerReferences: []metav1.OwnerReference{sandboxOwnerRef},
				},
			},
			wantOwnership:     resourceOwnedBySandbox,
			wantControllerRef: &sandboxOwnerRef,
		},
		{
			name: "pod with no owner",
			obj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "unowned-pod",
				},
			},
			wantOwnership:     resourceUnowned,
			wantControllerRef: nil,
		},
		{
			name: "pod owned by different controller",
			obj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "foreign-pod",
					OwnerReferences: []metav1.OwnerReference{otherOwnerRef},
				},
			},
			wantOwnership:     resourceOwnedByOther,
			wantControllerRef: &otherOwnerRef,
		},
		{
			name: "service owned by sandbox",
			obj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-service",
					OwnerReferences: []metav1.OwnerReference{sandboxOwnerRef},
				},
			},
			wantOwnership:     resourceOwnedBySandbox,
			wantControllerRef: &sandboxOwnerRef,
		},
		{
			name: "service with no owner",
			obj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "unowned-service",
				},
			},
			wantOwnership:     resourceUnowned,
			wantControllerRef: nil,
		},
		{
			name: "service owned by different controller",
			obj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "foreign-service",
					OwnerReferences: []metav1.OwnerReference{otherOwnerRef},
				},
			},
			wantOwnership:     resourceOwnedByOther,
			wantControllerRef: &otherOwnerRef,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ownership, controllerRef := checkOwnership(tc.obj, sandbox)
			require.Equal(t, tc.wantOwnership, ownership)
			require.Equal(t, tc.wantControllerRef, controllerRef)
		})
	}
}

func TestReconcilePVCs(t *testing.T) {
	sandboxName := "test-sandbox"
	sandboxNs := "test-ns"
	sandboxUID := types.UID("sandbox-uid-123")
	otherUID := types.UID("other-uid-456")
	pvcTemplateName := "data"
	pvcName := pvcTemplateName + "-" + sandboxName // "data-test-sandbox"

	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{Name: pvcTemplateName},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
	}

	testCases := []struct {
		name        string
		initialObjs []runtime.Object
		expectErr   bool
		errContains string
	}{
		{
			name:      "creates new PVC when none exists",
			expectErr: false,
		},
		{
			name: "uses existing PVC owned by this sandbox",
			initialObjs: []runtime.Object{
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: sandboxNs,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "agents.x-k8s.io/v1alpha1",
								Kind:               "Sandbox",
								Name:               sandboxName,
								UID:                sandboxUID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "refuses PVC owned by a different controller",
			initialObjs: []runtime.Object{
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: sandboxNs,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "Deployment",
								Name:               "other-controller",
								UID:                otherUID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						},
					},
				},
			},
			expectErr:   true,
			errContains: "is owned by",
		},
		{
			name: "adopts unowned PVC",
			initialObjs: []runtime.Object{
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: sandboxNs,
						// No owner references.
					},
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxReconciler{
				Client: newFakeClient(append(tc.initialObjs, sandbox)...),
				Scheme: Scheme,
				Tracer: asmetrics.NewNoOp(),
			}

			err := r.reconcilePVCs(t.Context(), sandbox, NameHash(sandboxName))
			if tc.expectErr {
				require.Error(t, err)
				if tc.errContains != "" {
					require.Contains(t, err.Error(), tc.errContains)
				}
				return
			}

			require.NoError(t, err)

			// Verify PVC exists and is owned by the sandbox.
			livePVC := &corev1.PersistentVolumeClaim{}
			err = r.Get(t.Context(), types.NamespacedName{Name: pvcName, Namespace: sandboxNs}, livePVC)
			require.NoError(t, err)
			ownerRef := metav1.GetControllerOf(livePVC)
			require.NotNil(t, ownerRef, "PVC should have a controller owner reference")
			require.Equal(t, sandboxUID, ownerRef.UID, "PVC controller reference UID should match sandbox UID")
		})
	}
}

func TestSandboxExpiry(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

	testCases := []struct {
		name           string
		shutdownTime   *metav1.Time
		deletionPolicy sandboxv1alpha1.ShutdownPolicy
		wantExpired    bool
		wantRequeue    time.Duration
	}{
		{
			name:         "nil shutdown time",
			shutdownTime: nil,
			wantExpired:  false,
			wantRequeue:  0,
		},
		{
			name:         "shutdown time in future",
			shutdownTime: new(metav1.NewTime(now.Add(2 * time.Hour))),
			wantExpired:  false,
			wantRequeue:  2 * time.Hour,
		},
		{
			name:         "shutdown time at current time expires immediately",
			shutdownTime: new(metav1.NewTime(now)),
			wantExpired:  true,
			wantRequeue:  0,
		},
		{
			name:         "shutdown time shortly in future uses minimum requeue",
			shutdownTime: new(metav1.NewTime(now.Add(500 * time.Millisecond))),
			wantExpired:  false,
			wantRequeue:  2 * time.Second,
		},
		{
			name:           "shutdown time in past - retain",
			shutdownTime:   new(metav1.NewTime(now.Add(-10 * time.Second))),
			deletionPolicy: sandboxv1alpha1.ShutdownPolicyRetain,
			wantExpired:    true,
			wantRequeue:    0,
		},
		{
			name:           "shutdown time in past - delete",
			shutdownTime:   new(metav1.NewTime(now.Add(-1 * time.Minute))),
			deletionPolicy: sandboxv1alpha1.ShutdownPolicyDelete,
			wantExpired:    true,
			wantRequeue:    0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.Sandbox{}
			sandbox.Spec.ShutdownTime = tc.shutdownTime
			if tc.deletionPolicy != "" {
				sandbox.Spec.ShutdownPolicy = new(tc.deletionPolicy)
			}
			expired, requeueAfter := checkSandboxExpiry(sandbox, now)
			require.Equal(t, tc.wantExpired, expired)
			require.Equal(t, tc.wantRequeue, requeueAfter)
		})
	}
}

func TestSandboxShutdownExpiryUsesTwoPassAndPreservesFinishedCondition(t *testing.T) {
	testCases := []struct {
		name           string
		phase          corev1.PodPhase
		finishedReason string
	}{
		{
			name:           "succeeded pod",
			phase:          corev1.PodSucceeded,
			finishedReason: sandboxv1alpha1.SandboxReasonPodSucceeded,
		},
		{
			name:           "failed pod",
			phase:          corev1.PodFailed,
			finishedReason: sandboxv1alpha1.SandboxReasonPodFailed,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			shutdownTime := metav1.NewTime(time.Now().Add(time.Hour))
			sandbox := &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "ttl-finished-sandbox",
					Namespace:  "default",
					UID:        sandboxUID,
					Generation: 1,
				},
				Spec: sandboxv1alpha1.SandboxSpec{
					PodTemplate: sandboxv1alpha1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "test-container"}},
						},
					},
					Lifecycle: sandboxv1alpha1.Lifecycle{
						ShutdownTime:   &shutdownTime,
						ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyRetain),
					},
				},
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandbox.Name,
					Namespace:       sandbox.Namespace,
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandbox.Name)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
				Status: corev1.PodStatus{Phase: tc.phase},
			}

			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandbox.Name,
					Namespace:       sandbox.Namespace,
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandbox.Name)},
				},
				Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
			}

			r := &SandboxReconciler{
				Client: newFakeClient(sandbox, pod, service),
				Scheme: Scheme,
				Tracer: asmetrics.NewNoOp(),
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}}

			result, err := r.Reconcile(t.Context(), req)
			require.NoError(t, err)
			require.Greater(t, result.RequeueAfter, time.Duration(0))

			updatedSandbox := &sandboxv1alpha1.Sandbox{}
			require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
			finishedCondition := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionFinished))
			require.NotNil(t, finishedCondition)
			require.Equal(t, tc.finishedReason, finishedCondition.Reason)
			require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &corev1.Pod{}))
			require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, &corev1.Service{}))

			expiredShutdownTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
			updatedSandbox.Spec.ShutdownTime = &expiredShutdownTime
			require.NoError(t, r.Update(t.Context(), updatedSandbox))

			result, err = r.Reconcile(t.Context(), req)
			require.NoError(t, err)
			require.Greater(t, result.RequeueAfter, time.Duration(0))

			require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
			readyCondition := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionReady))
			require.NotNil(t, readyCondition)
			require.Equal(t, sandboxv1alpha1.SandboxReasonExpired, readyCondition.Reason)
			finishedCondition = meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionFinished))
			require.NotNil(t, finishedCondition)
			require.Equal(t, tc.finishedReason, finishedCondition.Reason)
			require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &corev1.Pod{}))
			require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, &corev1.Service{}))

			result, err = r.Reconcile(t.Context(), req)
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)

			err = r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &corev1.Pod{})
			require.True(t, k8serrors.IsNotFound(err))
			err = r.Get(t.Context(), types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, &corev1.Service{})
			require.True(t, k8serrors.IsNotFound(err))

			require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
			readyCondition = meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionReady))
			require.NotNil(t, readyCondition)
			require.Equal(t, sandboxv1alpha1.SandboxReasonExpired, readyCondition.Reason)
			finishedCondition = meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionFinished))
			require.NotNil(t, finishedCondition)
			require.Equal(t, tc.finishedReason, finishedCondition.Reason)
		})
	}
}

func TestSetServiceStatusCustomDomain(t *testing.T) {
	testCases := []struct {
		name          string
		clusterDomain string
		wantFQDN      string
	}{
		{
			name:          "default cluster.local domain",
			clusterDomain: "cluster.local",
			wantFQDN:      "my-svc.my-ns.svc.cluster.local",
		},
		{
			name:          "custom cluster domain",
			clusterDomain: "custom.domain",
			wantFQDN:      "my-svc.my-ns.svc.custom.domain",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &SandboxReconciler{ClusterDomain: tc.clusterDomain}
			sandbox := &sandboxv1alpha1.Sandbox{}
			service := &corev1.Service{}
			service.Name = "my-svc"
			service.Namespace = "my-ns"

			r.setServiceStatus(sandbox, service)

			require.Equal(t, "my-svc", sandbox.Status.Service)
			require.Equal(t, tc.wantFQDN, sandbox.Status.ServiceFQDN)
		})
	}
}

func TestMergeVolumeClaimVolumes(t *testing.T) {
	pvcVol := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: "data-my-pod",
			},
		},
	}

	t.Run("replaces conflicting volume", func(t *testing.T) {
		existing := []corev1.Volume{
			{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		}

		result := MergeVolumeClaimVolumes(existing, []corev1.Volume{pvcVol})

		require.Len(t, result, 2)
		// config preserved
		require.Equal(t, "config", result[0].Name)
		require.NotNil(t, result[0].ConfigMap)
		// data replaced by PVC
		require.Equal(t, "data", result[1].Name)
		require.NotNil(t, result[1].PersistentVolumeClaim)
	})

	t.Run("appends when no conflict", func(t *testing.T) {
		existing := []corev1.Volume{
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		}

		result := MergeVolumeClaimVolumes(existing, []corev1.Volume{pvcVol})

		require.Len(t, result, 2)
		require.Equal(t, "config", result[0].Name)
		require.Equal(t, "data", result[1].Name)
	})

	t.Run("no-op when pvcVolumes is empty", func(t *testing.T) {
		existing := []corev1.Volume{
			{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}

		result := MergeVolumeClaimVolumes(existing, nil)

		require.Len(t, result, 1)
		require.Equal(t, "data", result[0].Name)
		require.NotNil(t, result[0].EmptyDir)
	})
}
