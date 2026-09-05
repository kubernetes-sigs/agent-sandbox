// Copyright 2026 The Kubernetes Authors.
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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// newInvalidFieldError constructs an API server-style "invalid" error for an
// unsupported field, mirroring the response a real kube-apiserver returns when
// it encounters a field it does not recognise in the submitted object.
func newInvalidFieldError(kind, name, fieldName string) error {
	return k8serrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: kind},
		name,
		field.ErrorList{field.Invalid(field.NewPath(fieldName), "", fmt.Sprintf("unknown field %q", fieldName))},
	)
}

// TestPodCreateRejectedUnsupportedFields verifies that when the API server
// rejects Pod creation because the Sandbox podTemplate contains fields not
// supported by the cluster version (e.g., bindMountOptions, evictionResponders
// introduced in newer Kubernetes releases), the reconciler surfaces the error
// through the Sandbox status conditions rather than silently ignoring it.
//
// This addresses review feedback from justinsb on PR #1547: new fields in
// corev1.PodSpec (added by the controller-runtime v0.25.0 / k8s 1.37 dep bump)
// are deep-copied from the Sandbox podTemplate into the Pod verbatim. On older
// clusters that don't recognise these fields, the API server rejects the Pod
// create. The reconciler must propagate that error.
func TestPodCreateRejectedUnsupportedFields(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-sandbox-unsupported",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "busybox",
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:             "data",
										MountPath:        "/data",
										BindMountOptions: []string{"rbind", "rw"},
									},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "data",
								VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								},
							},
						},
					},
				},
			},
		},
	}

	// Simulate an older API server that rejects Pod creation because of the
	// bindMountOptions field. The error message mirrors the real API server
	// response for unknown fields.
	inner := newFakeClient(sandbox)
	rejectingClient := interceptor.NewClient(inner, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return newInvalidFieldError("Pod", obj.GetName(), "spec.containers[0].volumeMounts[0].bindMountOptions")
			}
			return c.Create(ctx, obj, opts...)
		},
	})

	r := &SandboxReconciler{
		Client: rejectingClient,
		Scheme: Scheme,
		Tracer: asmetrics.NewNoOp(),
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      sandbox.Name,
		Namespace: sandbox.Namespace,
	}}

	// The reconciler must return an error so the workqueue requeues and the
	// operator can observe the failure.
	_, err := r.Reconcile(t.Context(), req)
	require.Error(t, err, "reconcile must return an error when Pod creation is rejected")
	require.Contains(t, err.Error(), "bindMountOptions",
		"error message should reference the unsupported field")

	// Even on error, the reconciler must update the Sandbox status so the
	// operator can see why the Pod failed to create.
	updatedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rejectingClient.Get(t.Context(), req.NamespacedName, updatedSandbox))

	ready := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, ready, "Ready condition must be set even when Pod creation fails")
	require.Equal(t, metav1.ConditionFalse, ready.Status,
		"Ready must be False when Pod creation is rejected")
}

// TestPodCreateRejectedEvictionResponders verifies that evictionResponders
// (a new corev1.PodSpec field from k8s 1.37) propagates a Pod create rejection
// into the Sandbox status, mirroring TestPodCreateRejectedUnsupportedFields
// for a different field.
func TestPodCreateRejectedEvictionResponders(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-sandbox-eviction",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "busybox",
							},
						},
						EvictionResponders: []corev1.EvictionResponder{
							{
								Name:     "custom-responder",
								Priority: ptr.To[int32](100),
							},
						},
					},
				},
			},
		},
	}

	inner := newFakeClient(sandbox)
	rejectingClient := interceptor.NewClient(inner, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return newInvalidFieldError("Pod", obj.GetName(), "spec.evictionResponders")
			}
			return c.Create(ctx, obj, opts...)
		},
	})

	r := &SandboxReconciler{
		Client: rejectingClient,
		Scheme: Scheme,
		Tracer: asmetrics.NewNoOp(),
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      sandbox.Name,
		Namespace: sandbox.Namespace,
	}}

	_, err := r.Reconcile(t.Context(), req)
	require.Error(t, err, "reconcile must return an error when Pod creation is rejected")
	require.Contains(t, err.Error(), "evictionResponders",
		"error message should reference the unsupported field")

	updatedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, rejectingClient.Get(t.Context(), req.NamespacedName, updatedSandbox))

	ready := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, ready, "Ready condition must be set even when Pod creation fails")
	require.Equal(t, metav1.ConditionFalse, ready.Status,
		"Ready must be False when Pod creation is rejected")
}
