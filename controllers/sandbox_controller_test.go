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

func sandboxControllerRef(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "agents.x-k8s.io/v1alpha1",
		Kind:               "Sandbox",
		Name:               name,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
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
					Phase: corev1.PodRunning,
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
			name:            "6. Operational Sandbox - Fully Ready",
			sandbox:         sbWithRepl(1),
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
			name: "13. Sandbox Deleting (DeletionTimestamp set)",
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

func TestReconcile(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	testCases := []struct {
		name                 string
		initialObjs          []runtime.Object
		sandboxSpec          sandboxv1alpha1.SandboxSpec
		deletionTimestamp    *metav1.Time
		wantStatus           sandboxv1alpha1.SandboxStatus
		wantObjs             []client.Object
		wantDeletedObjs      []client.Object
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
		{
			name: "sandbox expired with retain policy",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
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
					ShutdownTime:   ptr.To(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
					ShutdownPolicy: ptr.To(sandboxv1alpha1.ShutdownPolicyRetain),
				},
			},
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1alpha1.SandboxConditionReady),
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonExpired,
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
			name: "sandbox expired with delete policy",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
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
					ShutdownTime:   ptr.To(metav1.NewTime(time.Now().Add(-30 * time.Minute))),
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
			name:              "Sandbox deleting (DeletionTimestamp set)",
			deletionTimestamp: ptr.To(metav1.Now()),
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
			wantStatus: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1alpha1.SandboxReasonDeleting,
						Message:            "Sandbox is terminating",
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
			if tc.deletionTimestamp != nil {
				sb.DeletionTimestamp = tc.deletionTimestamp
				sb.Finalizers = []string{"test-finalizer"}
			}
			sb.Spec = tc.sandboxSpec
			r := SandboxReconciler{
				Client: newFakeClient(append(tc.initialObjs, sb)...),
				Scheme: Scheme,
				Tracer: asmetrics.NewNoOp(),
			}

			_, err := r.Reconcile(t.Context(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
			})
			require.NoError(t, err)
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
						"custom-label":                      "label-val",
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
						sandboxv1alpha1.SandboxPodNameAnnotation: "adopted-pod-name",
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
						"custom-label":                      "label-val",
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
						sandboxv1alpha1.SandboxPodNameAnnotation: "non-existent-pod",
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
						sandboxv1alpha1.SandboxPodNameAnnotation: "annotated-pod-name",
						"other-annotation":                       "other-value",
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
				Tracer: asmetrics.NewNoOp(),
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
				if annotatedPod, exists := tc.sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; exists && annotatedPod != "" {
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
		name           string
		shutdownTime   *metav1.Time
		deletionPolicy sandboxv1alpha1.ShutdownPolicy
		wantExpired    bool
		wantRequeue    bool
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
			name:           "shutdown time in past - retain",
			shutdownTime:   ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Second))),
			deletionPolicy: sandboxv1alpha1.ShutdownPolicyRetain,
			wantExpired:    true,
			wantRequeue:    false,
		},
		{
			name:           "shutdown time in past - delete",
			shutdownTime:   ptr.To(metav1.NewTime(time.Now().Add(-1 * time.Minute))),
			deletionPolicy: sandboxv1alpha1.ShutdownPolicyDelete,
			wantExpired:    true,
			wantRequeue:    false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1alpha1.Sandbox{}
			sandbox.Spec.ShutdownTime = tc.shutdownTime
			if tc.deletionPolicy != "" {
				sandbox.Spec.ShutdownPolicy = ptr.To(tc.deletionPolicy)
			}
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
