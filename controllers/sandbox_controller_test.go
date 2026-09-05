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
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func newFakeClient(initialObjs ...runtime.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&corev1.Pod{}, podSandboxNameHashIndex, podSandboxNameHashIndexer).
		WithRuntimeObjects(initialObjs...).
		Build()
}

const sandboxUID = types.UID("test-sandbox-uid")

func sandboxControllerRef(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         sandboxv1beta1.GroupVersion.String(),
		Kind:               sandboxv1beta1.SandboxKind,
		Name:               name,
		UID:                sandboxUID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}
}

func resizeResources(cpu, memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
	}
}

func inPlaceResizeSandbox(resources corev1.ResourceRequirements) *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "default", UID: sandboxUID, Generation: 7},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Resources: resources}}},
			}},
			ResourceResizePolicy: &sandboxv1beta1.ResourceResizePolicy{Type: sandboxv1beta1.ResourceResizePolicyInPlace},
		},
	}
}

func inPlaceResizePod(resources corev1.ResourceRequirements) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resize", Namespace: "default", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{sandboxControllerRef("resize")},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "workspace",
			Resources: resources,
			ResizePolicy: []corev1.ContainerResizePolicy{
				{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
				{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
			},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "workspace", RestartCount: 3, Resources: resources.DeepCopy(),
			}},
		},
	}
}

// hasExplicitRestartFreeResizePolicy reports whether a container explicitly
// declares a restart-free policy. An omitted policy is restart-free by
// Kubernetes defaulting, but does not satisfy tests verifying policy injection.
func hasExplicitRestartFreeResizePolicy(container corev1.Container, resourceName corev1.ResourceName) bool {
	for _, policy := range container.ResizePolicy {
		if policy.ResourceName == resourceName {
			return policy.RestartPolicy != corev1.RestartContainer
		}
	}
	return false
}

func TestReconcileInPlaceResourcesUsesStrategicResizeSubresourceWithoutRestart(t *testing.T) {
	current := resizeResources("1", "1Gi")
	desired := resizeResources("2", "2Gi")
	sandbox := inPlaceResizeSandbox(desired)
	pod := inPlaceResizePod(current)

	rawClient := newFakeClient()
	var subresource string
	var patched *corev1.Pod
	var patch client.Patch
	clientWithResize := interceptor.NewClient(rawClient, interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, name string, obj client.Object, gotPatch client.Patch, _ ...client.SubResourcePatchOption) error {
			subresource = name
			patched = obj.(*corev1.Pod).DeepCopy()
			patch = gotPatch
			return nil
		},
	})

	condition, err := (&SandboxReconciler{Client: clientWithResize}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionUnknown, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizePending, condition.Reason)
	assert.Equal(t, "resize", subresource)
	require.NotNil(t, patched)
	require.NotNil(t, patch)
	assert.Equal(t, types.StrategicMergePatchType, patch.Type(), "resize must merge containers by name instead of replacing the array")
	assert.Equal(t, types.UID("pod-uid"), patched.UID, "in-place resize must retain Pod identity")
	assert.Equal(t, int32(3), patched.Status.ContainerStatuses[0].RestartCount, "in-place resize must not restart the container")
	assert.True(t, patched.Spec.Containers[0].Resources.Requests.Cpu().Equal(resource.MustParse("2")))
	assert.True(t, patched.Spec.Containers[0].Resources.Limits.Memory().Equal(resource.MustParse("2Gi")))
	assert.Equal(t, current, pod.Spec.Containers[0].Resources, "resize submission must not mutate the observed Pod")
}

func TestReconcileInPlaceResourcesSkipsTerminatingPod(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
	pod := inPlaceResizePod(resizeResources("1", "1Gi"))
	deletionTime := metav1.Now()
	pod.DeletionTimestamp = &deletionTime

	patched := false
	clientWithResize := interceptor.NewClient(newFakeClient(), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			patched = true
			return nil
		},
	})

	condition, err := (&SandboxReconciler{Client: clientWithResize}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	assert.Nil(t, condition)
	assert.False(t, patched, "terminating Pods must not receive resize requests")
}

func TestSetResourceResizeConditionRetainsTerminalOutcomeUntilDisabled(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
	completed := resourceResizeCondition(
		sandbox,
		metav1.ConditionTrue,
		sandboxv1beta1.SandboxReasonResourceResizeCompleted,
		"CPU and memory resources were resized in place",
	)
	setResourceResizeCondition(sandbox, completed)
	setResourceResizeCondition(sandbox, nil)

	condition := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionResourceResize))
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeCompleted, condition.Reason)

	sandbox.Spec.ResourceResizePolicy = nil
	setResourceResizeCondition(sandbox, nil)
	assert.Nil(t, meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionResourceResize)))
}

func TestReconcileInPlaceResourcesRejectsRestartContainerPolicy(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
	pod := inPlaceResizePod(resizeResources("1", "1Gi"))
	pod.Spec.Containers[0].ResizePolicy[1].RestartPolicy = corev1.RestartContainer

	condition, err := (&SandboxReconciler{Client: newFakeClient()}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeUnsupported, condition.Reason)
	assert.Contains(t, condition.Message, "RestartContainer")
}

func TestReconcileInPlaceResourcesAllowsMissingResizePolicy(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
	pod := inPlaceResizePod(resizeResources("1", "1Gi"))
	pod.Spec.Containers[0].ResizePolicy = nil

	patched := false
	clientWithResize := interceptor.NewClient(newFakeClient(), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			patched = true
			return nil
		},
	})

	condition, err := (&SandboxReconciler{Client: clientWithResize}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.True(t, patched)
	assert.Equal(t, metav1.ConditionUnknown, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizePending, condition.Reason)
}

func TestResourceResizeTargetPreservesOmittedResources(t *testing.T) {
	tests := []struct {
		name           string
		removeResource func(*corev1.ResourceRequirements)
	}{
		{
			name: "CPU request",
			removeResource: func(resources *corev1.ResourceRequirements) {
				delete(resources.Requests, corev1.ResourceCPU)
			},
		},
		{
			name: "CPU limit",
			removeResource: func(resources *corev1.ResourceRequirements) {
				delete(resources.Limits, corev1.ResourceCPU)
			},
		},
		{
			name: "memory request",
			removeResource: func(resources *corev1.ResourceRequirements) {
				delete(resources.Requests, corev1.ResourceMemory)
			},
		},
		{
			name: "memory limit",
			removeResource: func(resources *corev1.ResourceRequirements) {
				delete(resources.Limits, corev1.ResourceMemory)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := resizeResources("1", "1Gi")
			desired := *current.DeepCopy()
			tt.removeResource(&desired)
			target, changed := resourceResizeTarget(current, desired)
			assert.False(t, changed)
			assert.Equal(t, current, target, "an omitted resource must preserve the admission-defaulted Pod value")
		})
	}
}

func TestReconcileInPlaceResourcesPreservesDefaultedRequest(t *testing.T) {
	current := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	}
	desired := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}
	sandbox := inPlaceResizeSandbox(desired)
	pod := inPlaceResizePod(current)

	var patched *corev1.Pod
	clientWithResize := interceptor.NewClient(newFakeClient(), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			patched = obj.(*corev1.Pod).DeepCopy()
			return nil
		},
	})

	condition, err := (&SandboxReconciler{Client: clientWithResize}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizePending, condition.Reason)
	require.NotNil(t, patched)
	assert.True(t, patched.Spec.Containers[0].Resources.Requests.Cpu().Equal(resource.MustParse("1")))
	assert.True(t, patched.Spec.Containers[0].Resources.Limits.Cpu().Equal(resource.MustParse("2")))
}

func TestReconcileInPlaceResourcesRetriesWhenPodDisappears(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
	pod := inPlaceResizePod(resizeResources("1", "1Gi"))
	clientWithResize := interceptor.NewClient(newFakeClient(), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			return k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod.Name)
		},
	})

	condition, err := (&SandboxReconciler{Client: clientWithResize}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.Error(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionUnknown, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizePending, condition.Reason)
}

func TestReconcileInPlaceResourcesReportsMissingResizeSubresourceAsUnsupported(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "method not supported",
			err:  k8serrors.NewMethodNotSupported(schema.GroupResource{Resource: "pods"}, "patch"),
		},
		{
			name: "endpoint not found",
			err: &k8serrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Reason: metav1.StatusReasonNotFound,
				Code:   http.StatusNotFound,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
			pod := inPlaceResizePod(resizeResources("1", "1Gi"))
			clientWithResize := interceptor.NewClient(newFakeClient(), interceptor.Funcs{
				SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
					return tt.err
				},
			})

			condition, err := (&SandboxReconciler{Client: clientWithResize}).reconcileInPlaceResources(context.Background(), sandbox, pod)
			require.NoError(t, err)
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionFalse, condition.Status)
			assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeUnsupported, condition.Reason)
			assert.Contains(t, condition.Message, "not supported by this cluster")
		})
	}
}

func TestReconcileChildResourcesKeepsReadyWhenResizeSubmissionRetries(t *testing.T) {
	desired := resizeResources("2", "2Gi")
	sandbox := inPlaceResizeSandbox(desired)
	sandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	pod := inPlaceResizePod(resizeResources("1", "1Gi"))
	pod.Labels = map[string]string{sandboxLabel: NameHash(sandbox.Name)}
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.1"}}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}

	clientWithResize := interceptor.NewClient(newFakeClient(sandbox, pod), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			return k8serrors.NewInternalError(errors.New("temporary resize failure"))
		},
	})
	reconciler := &SandboxReconciler{
		Client:        clientWithResize,
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	err := reconciler.reconcileChildResources(t.Context(), sandbox, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temporary resize failure")
	ready := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonDependenciesReady, ready.Reason)
	resize := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionResourceResize))
	require.NotNil(t, resize)
	assert.Equal(t, metav1.ConditionUnknown, resize.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizePending, resize.Reason)
}

func TestReconcileInPlaceResourcesDisabledDoesNotPatchPod(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
	sandbox.Spec.ResourceResizePolicy = nil
	pod := inPlaceResizePod(resizeResources("1", "1Gi"))

	condition, err := (&SandboxReconciler{Client: newFakeClient()}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	assert.Nil(t, condition)
	assert.True(t, pod.Spec.Containers[0].Resources.Requests.Cpu().Equal(resource.MustParse("1")), "Disabled must retain the running Pod resources")
}

func TestReconcileInPlaceResourcesProjectsTerminalPodFailure(t *testing.T) {
	desired := resizeResources("2", "2Gi")
	sandbox := inPlaceResizeSandbox(desired)
	pod := inPlaceResizePod(desired)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodResizePending, Status: corev1.ConditionFalse, Reason: corev1.PodReasonInfeasible, Message: "node capacity exhausted",
	}}

	condition, err := (&SandboxReconciler{Client: newFakeClient()}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeFailed, condition.Reason)
	assert.Equal(t, "node capacity exhausted", condition.Message)
}

func TestReconcileInPlaceResourcesSubmitsNewTargetWhilePreviousResizeIsPending(t *testing.T) {
	desired := resizeResources("2", "1Gi")
	sandbox := inPlaceResizeSandbox(desired)
	pod := inPlaceResizePod(resizeResources("4", "1Gi"))
	current := resizeResources("1", "1Gi")
	pod.Status.ContainerStatuses[0].Resources = current.DeepCopy()
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodResizePending, Status: corev1.ConditionFalse, Reason: corev1.PodReasonInfeasible, Message: "4 CPUs exceed node capacity",
	}}

	var patched *corev1.Pod
	clientWithResize := interceptor.NewClient(newFakeClient(), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			patched = obj.(*corev1.Pod).DeepCopy()
			return nil
		},
	})

	condition, err := (&SandboxReconciler{Client: clientWithResize}).reconcileInPlaceResources(t.Context(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionUnknown, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizePending, condition.Reason)
	require.NotNil(t, patched)
	assert.Equal(t, desired, patched.Spec.Containers[0].Resources)
}

func TestReconcileInPlaceResourcesProjectsPodResizeInProgress(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("2", "2Gi"))
	pod := inPlaceResizePod(resizeResources("2", "2Gi"))
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodResizeInProgress, Status: corev1.ConditionTrue, Reason: "Actuating", Message: "kubelet is resizing",
	}}

	condition, err := (&SandboxReconciler{Client: newFakeClient()}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionUnknown, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeInProgress, condition.Reason)
	assert.Equal(t, "kubelet is resizing", condition.Message)
}

func TestReconcileInPlaceResourcesCompletesOnlyAfterKubeletEnactsResources(t *testing.T) {
	desired := resizeResources("2", "2Gi")
	sandbox := inPlaceResizeSandbox(desired)
	sandbox.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionResourceResize), Status: metav1.ConditionUnknown,
		Reason: sandboxv1beta1.SandboxReasonResourceResizePending,
	}}
	pod := inPlaceResizePod(desired)
	pod.Status.ContainerStatuses[0].Resources = desired.DeepCopy()

	condition, err := (&SandboxReconciler{Client: newFakeClient()}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeCompleted, condition.Reason)
}

func TestReconcileInPlaceResourcesCompletesWithDefaultedRequest(t *testing.T) {
	desired := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}
	effective := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}
	sandbox := inPlaceResizeSandbox(desired)
	sandbox.Status.Conditions = []metav1.Condition{{
		Type: string(sandboxv1beta1.SandboxConditionResourceResize), Status: metav1.ConditionUnknown,
		Reason: sandboxv1beta1.SandboxReasonResourceResizePending,
	}}
	pod := inPlaceResizePod(effective)

	condition, err := (&SandboxReconciler{Client: newFakeClient()}).reconcileInPlaceResources(context.Background(), sandbox, pod)
	require.NoError(t, err)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeCompleted, condition.Reason)
}

func TestReconcileInPlaceResourcesCompletesWhenTerminalOutcomeNoLongerHasDrift(t *testing.T) {
	for _, reason := range []string{
		sandboxv1beta1.SandboxReasonResourceResizeFailed,
		sandboxv1beta1.SandboxReasonResourceResizeUnsupported,
	} {
		t.Run(reason, func(t *testing.T) {
			resources := resizeResources("1", "1Gi")
			sandbox := inPlaceResizeSandbox(resources)
			sandbox.Status.Conditions = []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionResourceResize), Status: metav1.ConditionFalse, Reason: reason,
			}}
			pod := inPlaceResizePod(resources)

			condition, err := (&SandboxReconciler{Client: newFakeClient()}).reconcileInPlaceResources(context.Background(), sandbox, pod)
			require.NoError(t, err)
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionTrue, condition.Status)
			assert.Equal(t, sandboxv1beta1.SandboxReasonResourceResizeCompleted, condition.Reason)
			assert.Equal(t, sandbox.Generation, condition.ObservedGeneration)
		})
	}
}

func TestEnsureRestartFreeResizePoliciesPreservesExplicitRestartPolicy(t *testing.T) {
	spec := corev1.PodSpec{Containers: []corev1.Container{{
		Name: "workspace",
		ResizePolicy: []corev1.ContainerResizePolicy{{
			ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer,
		}},
	}}}

	ensureRestartFreeResizePolicies(&spec)
	require.Len(t, spec.Containers[0].ResizePolicy, 2)
	assert.True(t, hasExplicitRestartFreeResizePolicy(spec.Containers[0], corev1.ResourceCPU))
	assert.False(t, hasExplicitRestartFreeResizePolicy(spec.Containers[0], corev1.ResourceMemory))
}

func TestReconcilePodCreatesInPlaceSandboxWithRestartFreePolicies(t *testing.T) {
	sandbox := inPlaceResizeSandbox(resizeResources("1", "1Gi"))
	client := newFakeClient(sandbox)
	reconciler := &SandboxReconciler{Client: client, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

	pod, err := reconciler.reconcilePod(context.Background(), sandbox, NameHash(sandbox.Name), nil)
	require.NoError(t, err)
	require.NotNil(t, pod)
	require.True(t, hasExplicitRestartFreeResizePolicy(pod.Spec.Containers[0], corev1.ResourceCPU))
	require.True(t, hasExplicitRestartFreeResizePolicy(pod.Spec.Containers[0], corev1.ResourceMemory))
}

func TestComputeConditions(t *testing.T) {
	r := &SandboxReconciler{}

	gen := int64(1)
	sbWithMode := func(mode sandboxv1beta1.SandboxOperatingMode) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-sandbox",
				UID:        "test-uid",
				Generation: gen,
			},
			Spec: sandboxv1beta1.SandboxSpec{OperatingMode: mode},
		}
	}

	sbWithModeAndSvcReq := func(mode sandboxv1beta1.SandboxOperatingMode) *sandboxv1beta1.Sandbox {
		sb := sbWithMode(mode)
		sb.Spec.Service = new(true)
		return sb
	}

	// ownedPod stamps the controller ownerRef pointing at the fixture sandbox.
	// Conditions that mirror Pod state (Finished, PodScheduled) only trust a Pod
	// this Sandbox owns, so fixtures standing in for the real backing Pod need it.
	ownedPod := func(pod *corev1.Pod) *corev1.Pod {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: sandboxv1beta1.GroupVersion.String(),
			Kind:       sandboxv1beta1.SandboxKind,
			Name:       "test-sandbox",
			UID:        "test-uid",
			Controller: new(true),
		}}
		return pod
	}

	testCases := []struct {
		name               string
		sandbox            *sandboxv1beta1.Sandbox
		err                error
		svc                *corev1.Service
		pod                *corev1.Pod
		podErr             error
		expectedConditions []metav1.Condition
	}{
		{
			name:    "1. Provisioning - No dependencies",
			sandbox: sbWithModeAndSvcReq(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     nil,
			pod:     nil,
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod does not exist; Service does not exist"},
			},
		},
		{
			name:    "2. Provisioning - Partial dependencies (missing Pod)",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod:     nil,
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod does not exist; Service Exists"},
			},
		},
		{
			name:    "3. Pod Pending",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod:     ownedPod(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "Unknown", ObservedGeneration: gen, Reason: "PodSchedulingUnknown", Message: "Pod has not reported a PodScheduled condition yet"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod exists with phase: Pending; Service Exists"},
			},
		},
		{
			name:    "4. Pod Running but not Ready",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod: ownedPod(&corev1.Pod{
				Status: corev1.PodStatus{
					Phase:  corev1.PodRunning,
					PodIPs: []corev1.PodIP{{IP: "10.244.0.1"}},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			}),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "True", ObservedGeneration: gen, Reason: "PodScheduled"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod is Running but not Ready; Service Exists"},
			},
		},
		{
			name:    "5. Pod ready but no IP yet",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod: ownedPod(&corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "Unknown", ObservedGeneration: gen, Reason: "PodSchedulingUnknown", Message: "Pod has not reported a PodScheduled condition yet"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod is Ready but has no podIPs yet; Service Exists"},
			},
		},
		{
			name:    "6. Suspended by user - Pod still terminating",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeSuspended),
			svc:     &corev1.Service{},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-pod",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: sandboxv1beta1.GroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "test-sandbox",
							UID:        "test-uid",
							Controller: new(true),
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "PodTerminating", Message: "Pod is terminating. Sandbox is suspending"},
				{Type: "PodScheduled", Status: "Unknown", ObservedGeneration: gen, Reason: "PodSchedulingUnknown", Message: "Pod has not reported a PodScheduled condition yet"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxSuspended", Message: "Sandbox is suspending"},
			},
		},
		{
			name:    "6b. Suspended by user - Pod not owned",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeSuspended),
			svc:     &corev1.Service{},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-pod",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "PodNotOwned", Message: "Refused to delete pod because it is not owned by this sandbox"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxSuspended", Message: "Sandbox is suspending"},
			},
		},
		{
			name:    "6c. Suspended - owned pod present but delete failed stays terminating (not Unknown)",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeSuspended),
			svc:     &corev1.Service{},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-pod",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: sandboxv1beta1.GroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "test-sandbox",
							UID:        "test-uid",
							Controller: new(true),
						},
					},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			// reconcilePod returns the still-present pod alongside the delete error, so
			// we know the pod exists and must not report it as terminated/unknown.
			podErr: errors.New("failed to delete pod: boom"),
			err:    errors.New("failed to delete pod: boom"),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "PodTerminating", Message: "Pod is terminating. Sandbox is suspending"},
				{Type: "PodScheduled", Status: "Unknown", ObservedGeneration: gen, Reason: "PodSchedulingUnknown", Message: "Pod has not reported a PodScheduled condition yet"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "ReconcilerError", Message: "Error seen: failed to delete pod: boom"},
			},
		},
		{
			name:    "7. Fully suspended - Pod deleted",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeSuspended),
			svc:     &corev1.Service{},
			pod:     nil,
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "True", ObservedGeneration: gen, Reason: "PodTerminated", Message: "Pod has been terminated. Sandbox is suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxSuspended", Message: "Sandbox is suspended"},
			},
		},
		{
			name:    "7b. Suspended - pod reconcile failed reports Unknown",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeSuspended),
			svc:     &corev1.Service{},
			pod:     nil,
			// reconcilePod failed, so a nil pod does not prove the pod is gone.
			podErr: errors.New("pod get failed"),
			err:    errors.New("pod get failed"),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "Unknown", ObservedGeneration: gen, Reason: "PodStateUnknown", Message: "Pod state is unknown. Sandbox suspension cannot be confirmed"},
				{Type: "PodScheduled", Status: "Unknown", ObservedGeneration: gen, Reason: "PodSchedulingUnknown", Message: "Pod state is unknown. Pod scheduling cannot be determined"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "ReconcilerError", Message: "Error seen: pod get failed"},
			},
		},
		{
			// A failed pod lookup must not be mistaken for a confirmed absent pod:
			// PodScheduled reports Unknown so pruning keeps it, rather than removing
			// it and implying the sandbox has no backing pod.
			name:    "7c. Running - pod reconcile failed keeps PodScheduled as Unknown",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod:     nil,
			podErr:  errors.New("pod list failed"),
			err:     errors.New("pod list failed"),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "Unknown", ObservedGeneration: gen, Reason: "PodSchedulingUnknown", Message: "Pod state is unknown. Pod scheduling cannot be determined"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "ReconcilerError", Message: "Error seen: pod list failed"},
			},
		},
		{
			name: "8. Resuming - Pod missing",
			sandbox: func() *sandboxv1beta1.Sandbox {
				sb := sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning)
				sb.Status.Conditions = []metav1.Condition{{Type: "Suspended", Status: "True"}}
				return sb
			}(),
			svc: &corev1.Service{},
			pod: nil,
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod does not exist; Service Exists"},
			},
		},
		{
			name:    "9. Unresponsive - Pod Status Unknown",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod:     ownedPod(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodUnknown}}),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "Unknown", ObservedGeneration: gen, Reason: "PodSchedulingUnknown", Message: "Pod has not reported a PodScheduled condition yet"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod exists with phase: Unknown; Service Exists"},
			},
		},
		{
			name:    "10. Pod Failed",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox", Name: "test-sandbox", UID: "test-uid", Controller: new(true)},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
					},
				},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "Finished", Status: "True", ObservedGeneration: gen, Reason: "PodFailed", Message: "Pod failed"},
				{Type: "PodScheduled", Status: "True", ObservedGeneration: gen, Reason: "PodScheduled"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "PodFailed", Message: "Pod failed"},
			},
		},
		{
			name:    "11. Pod Succeeded",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox", Name: "test-sandbox", UID: "test-uid", Controller: new(true)},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
					},
				},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "Finished", Status: "True", ObservedGeneration: gen, Reason: "PodSucceeded", Message: "Pod completed successfully"},
				{Type: "PodScheduled", Status: "True", ObservedGeneration: gen, Reason: "PodScheduled"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "PodSucceeded", Message: "Pod completed successfully"},
			},
		},
		{
			name:    "11b. Foreign terminal pod does not drive Finished",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeSuspended),
			svc:     &corev1.Service{},
			// A Succeeded pod not owned by this Sandbox (occupying the name while
			// suspended) must not produce a Finished condition on this Sandbox.
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
			},
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "PodNotOwned", Message: "Refused to delete pod because it is not owned by this sandbox"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "SandboxSuspended", Message: "Sandbox is suspending"},
			},
		},
		{
			name:    "12. Reconciler error takes precedence",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			err:     errors.New("something went wrong"),
			svc:     nil,
			pod:     nil,
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "ReconcilerError", Message: "Error seen: something went wrong"},
			},
		},
		{
			name:    "13. Pod unschedulable - reason and message mirrored verbatim",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod: ownedPod(&corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{
						Type:    corev1.PodScheduled,
						Status:  corev1.ConditionFalse,
						Reason:  corev1.PodReasonUnschedulable,
						Message: "0/3 nodes are available: 3 Insufficient cpu.",
					},
				},
			}}),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "False", ObservedGeneration: gen, Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient cpu."},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod exists with phase: Pending; Service Exists"},
			},
		},
		{
			name:    "14. Pod scheduling gated - reason passed through",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod: ownedPod(&corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{
						Type:    corev1.PodScheduled,
						Status:  corev1.ConditionFalse,
						Reason:  corev1.PodReasonSchedulingGated,
						Message: "Scheduling is blocked due to non-empty scheduling gates",
					},
				},
			}}),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "False", ObservedGeneration: gen, Reason: "SchedulingGated", Message: "Scheduling is blocked due to non-empty scheduling gates"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod exists with phase: Pending; Service Exists"},
			},
		},
		{
			name:    "15. Pod not scheduled with empty reason - fallback reason keeps condition valid",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			svc:     &corev1.Service{},
			pod: ownedPod(&corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodScheduled,
						Status: corev1.ConditionFalse,
					},
				},
			}}),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "PodScheduled", Status: "False", ObservedGeneration: gen, Reason: "PodSchedulingUnknown"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "DependenciesNotReady", Message: "Pod exists with phase: Pending; Service Exists"},
			},
		},
		{
			name:    "16. Multiple owned Pods message excludes joined transient errors",
			sandbox: sbWithMode(sandboxv1beta1.SandboxOperatingModeRunning),
			err: errors.Join(
				errors.New("failed to reconcile PVC: temporary error"),
				&multipleSandboxPodsError{count: 2},
			),
			expectedConditions: []metav1.Condition{
				{Type: "Suspended", Status: "False", ObservedGeneration: gen, Reason: "NotSuspended", Message: "Sandbox is not suspended"},
				{Type: "Ready", Status: "False", ObservedGeneration: gen, Reason: "MultiplePods", Message: "multiple Pods (2) are controlled by this Sandbox; refusing to choose or create a Pod"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conditions := r.computeConditions(tc.sandbox, tc.err, tc.svc, tc.pod, tc.podErr)
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
			annotations: map[string]string{sandboxv1beta1.SandboxPodNameAnnotation: ""},
			wantPodName: "my-sandbox",
		},
		{
			name:        "annotation present with warm pool pod name",
			annotations: map[string]string{sandboxv1beta1.SandboxPodNameAnnotation: "warmpool-abc-xyz"},
			wantPodName: "warmpool-abc-xyz",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1beta1.Sandbox{
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
	nameHash := NameHash(sandboxName)
	testCases := []struct {
		name                 string
		initialObjs          []runtime.Object
		sandboxSpec          sandboxv1beta1.SandboxSpec
		sandboxAnnotations   map[string]string
		reconcileCount       int
		deletionTimestamp    *metav1.Time
		wantStatus           sandboxv1beta1.SandboxStatus
		wantObjs             []client.Object
		wantDeletedObjs      []client.Object
		wantSurvivingObjs    []client.Object
		expectSandboxDeleted bool
	}{
		{
			name: "minimal sandbox spec creates Pod but not Service by default",
			// Input sandbox spec
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
			}},
			},
			// Verify Sandbox status
			wantStatus: sandboxv1beta1.SandboxStatus{
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
				Conditions: []metav1.Condition{
					{
						Type:               "Suspended",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "NotSuspended",
						Message:            "Sandbox is not suspended",
					},
					{
						Type:               "PodScheduled",
						Status:             "Unknown",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonPodSchedulingUnknown,
						Message:            "Pod has not reported a PodScheduled condition yet",
					},
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonDependenciesNotReady,
						Message:            "Pod exists with phase: ",
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
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
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
		},
		{
			name: "minimal sandbox spec with Pod and Service",
			// Input sandbox spec
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
							},
						},
					},
				}},
			},
			// Verify Sandbox status
			wantStatus: sandboxv1beta1.SandboxStatus{
				Service:       sandboxName,
				ServiceFQDN:   "sandbox-name.sandbox-ns.svc.cluster.local",
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1beta1.SandboxConditionSuspended),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             "NotSuspended",
						Message:            "Sandbox is not suspended",
					},
					{
						Type:               string(sandboxv1beta1.SandboxConditionPodScheduled),
						Status:             metav1.ConditionUnknown,
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonPodSchedulingUnknown,
						Message:            "Pod has not reported a PodScheduled condition yet",
					},
					{
						Type:               string(sandboxv1beta1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonDependenciesNotReady,
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
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
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
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
						ClusterIP: "None",
					},
				},
			},
		},
		{
			name: "sandbox spec with PVC, Pod, and Service",
			// Input sandbox spec
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
							},
						},
					},
					ObjectMeta: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{
							"custom-label": "label-val",
						},
						Annotations: map[string]string{
							"custom-annotation": "anno-val",
						},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
					{
						EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{
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
				}},
			},
			// Verify Sandbox status
			wantStatus: sandboxv1beta1.SandboxStatus{
				Service:       sandboxName,
				ServiceFQDN:   "sandbox-name.sandbox-ns.svc.cluster.local",
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1beta1.SandboxConditionSuspended),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             "NotSuspended",
						Message:            "Sandbox is not suspended",
					},
					{
						Type:               string(sandboxv1beta1.SandboxConditionPodScheduled),
						Status:             metav1.ConditionUnknown,
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonPodSchedulingUnknown,
						Message:            "Pod has not reported a PodScheduled condition yet",
					},
					{
						Type:               string(sandboxv1beta1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonDependenciesNotReady,
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
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
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
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
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
							"agents.x-k8s.io/sandbox-name-hash":  nameHash,
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
						NodeName:   "node-1",
					},
					Status: corev1.PodStatus{
						PodIPs: []corev1.PodIP{{IP: "10.244.0.5"}, {IP: "fd00::5"}},
						Phase:  corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				}},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
				Service:       sandboxName,
				ServiceFQDN:   "sandbox-name.sandbox-ns.svc.cluster.local",
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
				PodIPs:        []string{"10.244.0.5", "fd00::5"},
				NodeName:      "node-1",
				Conditions: []metav1.Condition{
					{
						Type:               "Suspended",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "NotSuspended",
						Message:            "Sandbox is not suspended",
					},
					{
						Type:               "PodScheduled",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonPodScheduled,
					},
					{
						Type:               "Ready",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
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
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
						ClusterIP: "None",
					},
				},
			},
		},
		{
			name: "sandbox with existing pod carrying legacy tracking label propagates PodIPs when adoptable label is absent",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
					Status: corev1.PodStatus{
						PodIPs: []corev1.PodIP{{IP: "10.244.0.5"}, {IP: "fd00::5"}},
						Phase:  corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				}},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
				Service:       sandboxName,
				ServiceFQDN:   "sandbox-name.sandbox-ns.svc.cluster.local",
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
				PodIPs:        []string{"10.244.0.5", "fd00::5"},
				Conditions: []metav1.Condition{
					{
						Type:               "Suspended",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "NotSuspended",
						Message:            "Sandbox is not suspended",
					},
					{
						Type:               "PodScheduled",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonPodScheduled,
					},
					{
						Type:               "Ready",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
						Message:            "Pod is Ready; Service Exists",
					},
				},
			},
			wantObjs: []client.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
						ClusterIP: "None",
					},
				},
			},
		},
		{
			name: "sandbox with existing ready pod becomes Ready without Service by default",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sandboxName,
						Namespace: sandboxNs,
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":  nameHash,
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
						NodeName:   "node-2",
					},
					Status: corev1.PodStatus{
						PodIPs: []corev1.PodIP{{IP: "10.244.0.5"}},
						Phase:  corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			}},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
				PodIPs:        []string{"10.244.0.5"},
				NodeName:      "node-2",
				Conditions: []metav1.Condition{
					{
						Type:               "Suspended",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "NotSuspended",
						Message:            "Sandbox is not suspended",
					},
					{
						Type:               "PodScheduled",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonPodScheduled,
					},
					{
						Type:               "Ready",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
						Message:            "Pod is Ready",
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
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
			}}, Lifecycle: sandboxv1beta1.Lifecycle{
				ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
				ShutdownPolicy: ptr.To(sandboxv1beta1.ShutdownPolicyRetain),
			},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(sandboxv1beta1.SandboxConditionReady),
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonExpired,
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
				sandboxv1beta1.SandboxPodNameAnnotation: "warmpool-abc-xyz",
			},
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
			}}, Lifecycle: sandboxv1beta1.Lifecycle{
				ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
				ShutdownPolicy: ptr.To(sandboxv1beta1.ShutdownPolicyRetain),
			},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
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
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
						},
					},
				},
			}}, Lifecycle: sandboxv1beta1.Lifecycle{
				ShutdownTime:   new(metav1.NewTime(time.Now().Add(-30 * time.Minute))),
				ShutdownPolicy: ptr.To(sandboxv1beta1.ShutdownPolicyDelete),
			},
			},
			wantDeletedObjs: []client.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
				&sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
			expectSandboxDeleted: true,
		},
		{
			name:           "sandbox expired skips deletion of pod owned by different controller",
			reconcileCount: 2,
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
							},
						},
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
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			}}, Lifecycle: sandboxv1beta1.Lifecycle{
				ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
				ShutdownPolicy: ptr.To(sandboxv1beta1.ShutdownPolicyRetain),
			},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
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
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			}}, Lifecycle: sandboxv1beta1.Lifecycle{
				ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
				ShutdownPolicy: ptr.To(sandboxv1beta1.ShutdownPolicyRetain),
			},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
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
			name: "sandbox expired with no matching pod or service",
			sandboxSpec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			}}, Lifecycle: sandboxv1beta1.Lifecycle{
				ShutdownTime:   new(metav1.NewTime(time.Now().Add(-1 * time.Hour))),
				ShutdownPolicy: ptr.To(sandboxv1beta1.ShutdownPolicyRetain),
			},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
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
		},
		{
			// Regression: while the Pod is still terminating (kept alive here by a
			// finalizer), Suspended must be False/PodTerminating — not True — and
			// lastTransitionTime must not be stamped prematurely. This exercises the
			// real reconcilePod path (which the computeSuspendedCondition unit test
			// bypasses by forcing a non-nil pod).
			name: "suspend with a still-terminating pod reports Suspended=False/PodTerminating",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						Finalizers:      []string{"agents.x-k8s.io/test-hold"},
						Labels:          map[string]string{sandboxLabel: nameHash},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
			},
			sandboxSpec: sandboxv1beta1.SandboxSpec{
				OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					},
				},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
				LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
				Conditions: []metav1.Condition{
					{
						Type:               "Suspended",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             "PodTerminating",
						Message:            "Pod is terminating. Sandbox is suspending",
					},
					{
						Type:               "PodScheduled",
						Status:             "Unknown",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonPodSchedulingUnknown,
						Message:            "Pod has not reported a PodScheduled condition yet",
					},
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonSuspended,
						Message:            "Sandbox is suspending",
					},
				},
			},
			wantSurvivingObjs: []client.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs}},
			},
		},
		{
			name: "suspend with no pod reports Suspended=True/PodTerminated",
			sandboxSpec: sandboxv1beta1.SandboxSpec{
				OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					},
				},
			},
			wantStatus: sandboxv1beta1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Suspended",
						Status:             "True",
						ObservedGeneration: 1,
						Reason:             "PodTerminated",
						Message:            "Pod has been terminated. Sandbox is suspended",
					},
					{
						Type:               "Ready",
						Status:             "False",
						ObservedGeneration: 1,
						Reason:             sandboxv1beta1.SandboxReasonSuspended,
						Message:            "Sandbox is suspended",
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sb := &sandboxv1beta1.Sandbox{}
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
			for range reconcileCount {
				_, err = r.Reconcile(t.Context(), ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      sandboxName,
						Namespace: sandboxNs,
					},
				})
				require.NoError(t, err)
			}
			// Validate Sandbox status or deletion
			liveSandbox := &sandboxv1beta1.Sandbox{}
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
	sandboxObj := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "test-container",
					},
				},
			},
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					"custom-label": "label-val",
				},
				Annotations: map[string]string{
					"custom-annotation": "anno-val",
				},
			},
		}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	testCases := []struct {
		name                   string
		initialObjs            []runtime.Object
		sandbox                *sandboxv1beta1.Sandbox
		wantPod                *corev1.Pod
		expectErr              bool
		wantSandboxAnnotations map[string]string
		wantPodSurvives        string // if set, verify this pod still exists after reconcile
		// wantPodDeleting: reconcilePod is expected to return a still-terminating Pod
		// (non-nil) and the Pod should exist in the cluster with a DeletionTimestamp set.
		// Used for the suspend path, where the Pod is deleted but not yet gone.
		wantPodDeleting bool
	}{
		{
			name: "updates label and owner reference if Pod already exists",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							sandboxv1beta1.SandboxAdoptableLabel: "true",
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
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						"custom-label":                       "label-val",
						sandboxv1beta1.SandboxAdoptableLabel: "true",
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
			// Reconciling an existing pod must not write the pod name annotation on the Sandbox as it is no longer written by the controller.
			wantSandboxAnnotations: map[string]string{},
		},
		{
			name: "persists owner reference when adopting unowned pod whose labels are already correct",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":  nameHash,
							"custom-label":                       "label-val",
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
						Annotations: map[string]string{
							"custom-annotation":                      "anno-val",
							"agents.x-k8s.io/propagated-labels":      "custom-label",
							"agents.x-k8s.io/propagated-annotations": "custom-annotation",
						},
						// No OwnerReferences : simulates a pre-created pod whose
						// labels/annotations already match the sandbox spec exactly.
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
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
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						"custom-label":                       "label-val",
						sandboxv1beta1.SandboxAdoptableLabel: "true",
					},
					Annotations: map[string]string{
						"custom-annotation":                      "anno-val",
						"agents.x-k8s.io/propagated-labels":      "custom-label",
						"agents.x-k8s.io/propagated-annotations": "custom-annotation",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "adopts unowned pod carrying legacy tracking label when adoptable label is absent",
			initialObjs: []runtime.Object{
				&corev1.Pod{
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
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
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
					Containers: []corev1.Container{{Name: "test-container"}},
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
			// Reconciling a new pod must not write the pod name annotation on the Sandbox as it is no longer written by the controller.
			wantSandboxAnnotations: map[string]string{},
		},
		{
			name: "drops user-supplied system-reserved labels and annotations to prevent hijacking",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
					ObjectMeta: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{
							// Attacker attempts to hijack another Sandbox's routing label
							// and to spoof an extensions-prefixed system label.
							"agents.x-k8s.io/sandbox-name-hash":          "malicious-hijacked-hash",
							extensionsv1beta1.SandboxIDLabel:             "malicious-claim-uid",
							"extensions.agents.x-k8s.io/warm-pool-spoof": "evil",
							"custom-label": "label-val",
						},
						Annotations: map[string]string{
							"agents.x-k8s.io/pod-name":       "malicious-pod-name",
							asmetrics.TraceContextAnnotation: "spoofed-trace",
							"custom-annotation":              "anno-val",
						},
					},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						// System label is set by the controller, not the attacker's value.
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
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "scrubs stale system labels/annotations recorded by an older controller",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
							"custom-label":                      "label-val",
							// A system label an older controller propagated and recorded.
							"agents.x-k8s.io/evil": "x",
						},
						Annotations: map[string]string{
							"custom-annotation": "anno-val",
							// Older controller recorded system keys in the propagated lists.
							"agents.x-k8s.io/propagated-labels":      "custom-label,agents.x-k8s.io/evil",
							"agents.x-k8s.io/propagated-annotations": "custom-annotation,agents.x-k8s.io/pod-name,opentelemetry.io/trace-context",
							"agents.x-k8s.io/pod-name":               "leftover",
							asmetrics.TraceContextAnnotation:         "spoofed-trace",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
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
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "does not propagate system labels from Sandbox metadata to Pod",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						sandboxv1beta1.SandboxWarmPoolLabel: "pool-hash",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
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
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
		},
		{
			name: "does not propagate system labels from Sandbox PodTemplate to Pod",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{
							"custom-label":                      "label-val",
							sandboxv1beta1.SandboxWarmPoolLabel: "pool-hash",
						},
					},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
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
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
		},
		{
			name: "does not propagate template-ref-hash from Sandbox metadata to Pod",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
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
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
		},
		{
			name: "propagates warm pool label from Sandbox owner reference to Pod",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						sandboxv1beta1.SandboxWarmPoolLabel: NameHash("my-warm-pool"),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxWarmPoolKind,
							Name:       "my-warm-pool",
							UID:        "pool-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
						sandboxv1beta1.SandboxWarmPoolLabel: NameHash("my-warm-pool"),
						"custom-label":                      "label-val",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
		},
		{
			name: "removes warm pool label from Pod when Sandbox is no longer owned by SandboxWarmPool",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":  nameHash,
							sandboxv1beta1.SandboxWarmPoolLabel:  "pool-hash",
							"custom-label":                       "label-val",
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
						Annotations: map[string]string{
							"agents.x-k8s.io/propagated-labels": "custom-label",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						"custom-label":                       "label-val",
						sandboxv1beta1.SandboxAdoptableLabel: "true",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "adds warm pool label to existing Pod when Sandbox is owned by SandboxWarmPool",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":  nameHash,
							"custom-label":                       "label-val",
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
						Annotations: map[string]string{
							"agents.x-k8s.io/propagated-labels": "custom-label",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						sandboxv1beta1.SandboxWarmPoolLabel: NameHash("my-warm-pool"),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxWarmPoolKind,
							Name:       "my-warm-pool",
							UID:        "pool-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						sandboxv1beta1.SandboxWarmPoolLabel:  NameHash("my-warm-pool"),
						"custom-label":                       "label-val",
						sandboxv1beta1.SandboxAdoptableLabel: "true",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "propagates template-ref-hash label from Sandbox labels to new Pod",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxClaimKind,
							Name:       "my-claim",
							UID:        "claim-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":        nameHash,
						"custom-label":                             "label-val",
						sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
		},
		{
			name: "adds template-ref-hash label to existing Pod during reconciliation",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
							"custom-label":                      "label-val",
						},
						Annotations: map[string]string{
							"agents.x-k8s.io/propagated-labels": "custom-label",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxClaimKind,
							Name:       "my-claim",
							UID:        "claim-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":        nameHash,
						"custom-label":                             "label-val",
						sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
		},
		{
			name: "propagates claim UID label from Sandbox labels to new Pod",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						extensionsv1beta1.SandboxIDLabel: "claim-uid",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxClaimKind,
							Name:       "my-claim",
							UID:        "claim-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
						"custom-label":                      "label-val",
						extensionsv1beta1.SandboxIDLabel:    "claim-uid",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
			wantSandboxAnnotations: map[string]string{},
		},
		{
			name: "adds claim UID label to existing Pod during reconciliation",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
							"custom-label":                      "label-val",
						},
						Annotations: map[string]string{
							"agents.x-k8s.io/propagated-labels": "custom-label",
						},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						extensionsv1beta1.SandboxIDLabel: "claim-uid",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxClaimKind,
							Name:       "my-claim",
							UID:        "claim-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
						"custom-label":                      "label-val",
						extensionsv1beta1.SandboxIDLabel:    "claim-uid",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
			wantSandboxAnnotations: map[string]string{},
		},
		{
			name: "both warm-pool-sandbox and template-ref-hash coexist on Pod",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Labels: map[string]string{
						sandboxv1beta1.SandboxWarmPoolLabel:        NameHash("my-warm-pool"),
						sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxWarmPoolKind,
							Name:       "my-warm-pool",
							UID:        "pool-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":        nameHash,
						"custom-label":                             "label-val",
						sandboxv1beta1.SandboxWarmPoolLabel:        NameHash("my-warm-pool"),
						sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
					},
					Annotations: map[string]string{
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
			},
		},
		{
			name: "removes template-ref-hash label from Pod when Sandbox is not owned by extensions controller",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":        nameHash,
							"custom-label":                             "label-val",
							sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
						},
						Annotations: map[string]string{
							"agents.x-k8s.io/propagated-labels": "custom-label",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
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
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "removes template-ref-hash label from Pod when absent from Sandbox labels but still extensions-owned",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":        nameHash,
							"custom-label":                             "label-val",
							sandboxv1beta1.SandboxTemplateRefHashLabel: "da1fd924",
						},
						Annotations: map[string]string{
							"agents.x-k8s.io/propagated-labels": "custom-label",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: extensionsv1beta1.GroupVersion.String(),
							Kind:       extensionsv1beta1.SandboxClaimKind,
							Name:       "my-claim",
							UID:        "claim-uid",
							Controller: new(true),
						},
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"custom-label": "label-val"}},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
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
						"agents.x-k8s.io/propagated-labels": "custom-label",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			// Suspend deletes the owned Pod but keeps reporting it (as terminating)
			// until it is actually gone. The finalizer keeps the fake-client Pod alive
			// with a DeletionTimestamp after Delete, mimicking a real termination grace
			// period; reconcilePod returns the (still-present) Pod so the Suspended
			// condition can report PodTerminating.
			name: "marks owned pod for deletion when mode is Suspended (still terminating)",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Finalizers:      []string{"agents.x-k8s.io/test-hold"},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{
					OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
				},
			},
			wantPodDeleting: true,
		},
		{
			name: "no-op if mode is Suspended and pod does not exist",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
				},
				Spec: sandboxv1beta1.SandboxSpec{
					OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
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
						Labels: map[string]string{
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
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
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Annotations: map[string]string{
						sandboxv1beta1.SandboxPodNameAnnotation: "adopted-pod-name",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "test-container",
							},
						},
					},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "adopted-pod-name",
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						sandboxLabel:                         nameHash,
						sandboxv1beta1.SandboxAdoptableLabel: "true",
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
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1beta1.SandboxPodNameAnnotation: "victim-pod",
						"other-annotation":                      "keep-me",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{
					OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
				},
			},
			wantPod: &corev1.Pod{
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
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1beta1.SandboxPodNameAnnotation: "unowned-pod",
						"other-annotation":                      "keep-me",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{
					OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "unowned-pod",
					Namespace:       sandboxNs,
					ResourceVersion: "1",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c"}},
				},
			},
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
						Finalizers:      []string{"agents.x-k8s.io/test-hold"},
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c"}},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Annotations: map[string]string{
						sandboxv1beta1.SandboxPodNameAnnotation: "owned-pod",
						"other-annotation":                      "keep-me",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{
					OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
				},
			},
			wantPodDeleting: true,
			expectErr:       false,
			wantSandboxAnnotations: map[string]string{
				"other-annotation":                      "keep-me",
				sandboxv1beta1.SandboxPodNameAnnotation: "owned-pod",
			},
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
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1beta1.SandboxPodNameAnnotation: "foreign-pod",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod:                nil,
			expectErr:              true,
			wantSandboxAnnotations: map[string]string{},
		},
		{
			name: "refuses to delete unowned annotated pod and removes annotation when mode is Suspended",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "annotated-pod-name",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					Annotations: map[string]string{
						sandboxv1beta1.SandboxPodNameAnnotation: "annotated-pod-name",
						"other-annotation":                      "other-value",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{
					OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
				},
			},
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "annotated-pod-name",
					Namespace:       sandboxNs,
					ResourceVersion: "1",
				},
			},
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
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					ObjectMeta: sandboxv1beta1.PodMetadata{
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
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
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
		},
		{
			name: "refuses to adopt unowned pod that lacks pool authorization label",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "adopted-pod-name",
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "existing-container"}},
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
					Annotations: map[string]string{
						sandboxv1beta1.SandboxPodNameAnnotation: "adopted-pod-name",
					},
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				},
			},
			wantPod:                nil,
			expectErr:              true,
			wantSandboxAnnotations: map[string]string{sandboxv1beta1.SandboxPodNameAnnotation: "adopted-pod-name"},
		},
		{
			name:        "propagates and normalizes created-by label value go-client",
			initialObjs: []runtime.Object{},
			sandbox: func() *sandboxv1beta1.Sandbox {
				sb := sandboxObj.DeepCopy()
				sb.Labels = map[string]string{
					sandboxv1beta1.CreatedByLabel: "go-client",
				}
				return sb
			}(),
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
						"custom-label":                      "label-val",
						sandboxv1beta1.CreatedByLabel:       "go-client",
					},
					Annotations: map[string]string{
						"custom-annotation":                      "anno-val",
						"agents.x-k8s.io/propagated-labels":      "custom-label",
						"agents.x-k8s.io/propagated-annotations": "custom-annotation",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name:        "normalizes invalid created-by label to unknown",
			initialObjs: []runtime.Object{},
			sandbox: func() *sandboxv1beta1.Sandbox {
				sb := sandboxObj.DeepCopy()
				sb.Labels = map[string]string{
					sandboxv1beta1.CreatedByLabel: "invalid-user-value-which-is-too-long-or-custom",
				}
				return sb
			}(),
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "1",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
						"custom-label":                      "label-val",
						sandboxv1beta1.CreatedByLabel:       "unknown",
					},
					Annotations: map[string]string{
						"custom-annotation":                      "anno-val",
						"agents.x-k8s.io/propagated-labels":      "custom-label",
						"agents.x-k8s.io/propagated-annotations": "custom-annotation",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "updates and normalizes created-by label on existing Pod",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":  nameHash,
							"custom-label":                       "label-val",
							sandboxv1beta1.SandboxAdoptableLabel: "true",
							sandboxv1beta1.CreatedByLabel:        "controller",
						},
						Annotations: map[string]string{
							"custom-annotation":                      "anno-val",
							"agents.x-k8s.io/propagated-labels":      "custom-label",
							"agents.x-k8s.io/propagated-annotations": "custom-annotation",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			sandbox: func() *sandboxv1beta1.Sandbox {
				sb := sandboxObj.DeepCopy()
				sb.Labels = map[string]string{
					sandboxv1beta1.CreatedByLabel: "python-client",
				}
				return sb
			}(),
			wantPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						"custom-label":                       "label-val",
						sandboxv1beta1.SandboxAdoptableLabel: "true",
						sandboxv1beta1.CreatedByLabel:        "python-client",
					},
					Annotations: map[string]string{
						"custom-annotation":                      "anno-val",
						"agents.x-k8s.io/propagated-labels":      "custom-label",
						"agents.x-k8s.io/propagated-annotations": "custom-annotation",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
		{
			name: "removes created-by label from existing Pod when Sandbox lacks it",
			initialObjs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash":  nameHash,
							"custom-label":                       "label-val",
							sandboxv1beta1.SandboxAdoptableLabel: "true",
							sandboxv1beta1.CreatedByLabel:        "go-client",
						},
						Annotations: map[string]string{
							"custom-annotation":                      "anno-val",
							"agents.x-k8s.io/propagated-labels":      "custom-label",
							"agents.x-k8s.io/propagated-annotations": "custom-annotation",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
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
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						"custom-label":                       "label-val",
						sandboxv1beta1.SandboxAdoptableLabel: "true",
					},
					Annotations: map[string]string{
						"custom-annotation":                      "anno-val",
						"agents.x-k8s.io/propagated-labels":      "custom-label",
						"agents.x-k8s.io/propagated-annotations": "custom-annotation",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := tc.sandbox.DeepCopy()

			r := SandboxReconciler{
				Client:        newFakeClient(append(tc.initialObjs, sandbox)...),
				Scheme:        Scheme,
				Tracer:        asmetrics.NewNoOp(),
				ClusterDomain: "cluster.local",
			}

			pod, err := r.reconcilePod(t.Context(), sandbox, nameHash, nil)
			if tc.expectErr {
				require.Error(t, err)
				// Verify that any initially unowned Pod remains unowned (never adopted)
				for _, obj := range tc.initialObjs {
					if initialPod, ok := obj.(*corev1.Pod); ok {
						if len(initialPod.OwnerReferences) == 0 {
							livePod := &corev1.Pod{}
							err = r.Get(t.Context(), types.NamespacedName{Name: initialPod.Name, Namespace: initialPod.Namespace}, livePod)
							require.NoError(t, err)
							assert.Empty(t, livePod.OwnerReferences, "expected Pod %q to remain unowned after failed reconcile", livePod.Name)
						}
					}
				}
			} else {
				require.NoError(t, err)
			}
			if tc.wantPodDeleting {
				// reconcilePod returns the still-terminating Pod (pre-delete snapshot),
				// and the Pod still exists in the cluster marked for deletion.
				require.NotNil(t, pod, "expected reconcilePod to return the terminating pod")
				livePod := &corev1.Pod{}
				err = r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, livePod)
				require.NoError(t, err, "expected the terminating pod to still exist")
				require.NotNil(t, livePod.DeletionTimestamp, "expected the pod to be marked for deletion")
			} else {
				require.Equal(t, tc.wantPod, pod)

				// Validate the Pod from the "cluster" (fake client)
				if tc.wantPod != nil {
					livePod := &corev1.Pod{}
					err = r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, livePod)
					require.NoError(t, err)
					require.Equal(t, tc.wantPod, livePod)
				} else if !tc.expectErr {
					if tc.wantPodSurvives != "" {
						// Pod should still exist (ownership check blocked deletion)
						livePod := &corev1.Pod{}
						err = r.Get(t.Context(), types.NamespacedName{Name: tc.wantPodSurvives, Namespace: sandboxNs}, livePod)
						require.NoError(t, err, "expected pod %q to survive but it was deleted", tc.wantPodSurvives)
					} else {
						// When wantPod is nil and no error expected, verify pod doesn't exist
						livePod := &corev1.Pod{}
						podName := sandboxName
						if annotatedPod, exists := tc.sandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]; exists && annotatedPod != "" {
							podName = annotatedPod
						}
						err = r.Get(t.Context(), types.NamespacedName{Name: podName, Namespace: sandboxNs}, livePod)
						require.True(t, k8serrors.IsNotFound(err))
					}
				}
			}

			if tc.wantSandboxAnnotations != nil {
				liveSandbox := &sandboxv1beta1.Sandbox{}
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

func TestReconcilePodRecoversOwnedPodWhenTrackedPodIsMissing(t *testing.T) {
	const (
		sandboxName = "sandbox-name"
		sandboxNs   = "sandbox-ns"
		nameHash    = "name-hash"
		survivor    = "warm-pod-survivor"
	)

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
			Annotations: map[string]string{
				sandboxv1beta1.SandboxPodNameAnnotation: "warm-pod-missing",
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	ownedSurvivor := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            survivor,
			Namespace:       sandboxNs,
			Labels:          map[string]string{sandboxLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
	}

	r := &SandboxReconciler{
		Client:        newFakeClient(sandbox, ownedSurvivor),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	pod, err := r.reconcilePod(t.Context(), sandbox.DeepCopy(), nameHash, nil)
	require.NoError(t, err)
	require.NotNil(t, pod)
	assert.Equal(t, survivor, pod.Name)

	createdPod := &corev1.Pod{}
	err = r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, createdPod)
	require.True(t, k8serrors.IsNotFound(err), "must not create a second Pod when an owned survivor exists")

	liveSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sandbox), liveSandbox))
	assert.Empty(t, liveSandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation])
}

func TestReconcilePodPrefersOwnedPodOverStaleAdoptionTarget(t *testing.T) {
	const (
		sandboxName = "sandbox-name"
		sandboxNs   = "sandbox-ns"
		nameHash    = "name-hash"
		survivor    = "owned-survivor"
		staleTarget = "stale-adoption-target"
	)

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
			Annotations: map[string]string{
				sandboxv1beta1.SandboxPodNameAnnotation: staleTarget,
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	ownedSurvivor := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            survivor,
			Namespace:       sandboxNs,
			Labels:          map[string]string{sandboxLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
	}
	staleAdoptionTarget := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      staleTarget,
			Namespace: sandboxNs,
			Labels:    map[string]string{sandboxv1beta1.SandboxAdoptableLabel: "true"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
	}

	r := &SandboxReconciler{
		Client:        newFakeClient(sandbox, ownedSurvivor, staleAdoptionTarget),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	pod, err := r.reconcilePod(t.Context(), sandbox.DeepCopy(), nameHash, nil)
	require.NoError(t, err)
	require.NotNil(t, pod)
	assert.Equal(t, survivor, pod.Name)

	liveTarget := &corev1.Pod{}
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: staleTarget, Namespace: sandboxNs}, liveTarget))
	assert.Empty(t, liveTarget.OwnerReferences, "stale target must not be adopted when an owned Pod already exists")

	liveSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sandbox), liveSandbox))
	assert.Empty(t, liveSandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation])
}

func TestReconcilePodFailsClosedForMultipleOwnedPods(t *testing.T) {
	const (
		sandboxName = "sandbox-name"
		sandboxNs   = "sandbox-ns"
		nameHash    = "name-hash"
	)

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs, UID: sandboxUID},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	ownedPod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       sandboxNs,
				Labels:          map[string]string{sandboxLabel: nameHash},
				OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
		}
	}
	first := ownedPod("owned-pod-a")
	second := ownedPod("owned-pod-b")
	r := &SandboxReconciler{
		Client:        newFakeClient(sandbox, first, second),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	pod, err := r.reconcilePod(t.Context(), sandbox.DeepCopy(), nameHash, nil)
	require.Error(t, err)
	assert.Nil(t, pod)
	assert.Contains(t, err.Error(), "multiple Pods")

	createdPod := &corev1.Pod{}
	err = r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, createdPod)
	require.True(t, k8serrors.IsNotFound(err), "must not create another Pod while ownership is ambiguous")
	for _, name := range []string{first.Name, second.Name} {
		livePod := &corev1.Pod{}
		require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: name, Namespace: sandboxNs}, livePod))
	}
}

func TestReconcileChildResourcesSurfacesMultipleOwnedPods(t *testing.T) {
	const (
		sandboxName = "sandbox-name"
		sandboxNs   = "sandbox-ns"
	)
	nameHash := NameHash(sandboxName)
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       sandboxName,
			Namespace:  sandboxNs,
			UID:        sandboxUID,
			Generation: 3,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	ownedPod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       sandboxNs,
				Labels:          map[string]string{sandboxLabel: nameHash},
				OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
			},
		}
	}
	recorder := events.NewFakeRecorder(2)
	r := &SandboxReconciler{
		Client:        newFakeClient(sandbox, ownedPod("owned-pod-a"), ownedPod("owned-pod-b")),
		Scheme:        Scheme,
		Recorder:      recorder,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	require.NoError(t, r.reconcileChildResources(t.Context(), sandbox, nil))
	ready := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, sandboxv1beta1.SandboxReasonMultiplePods, ready.Reason)
	assert.Contains(t, ready.Message, "multiple Pods (2)")
	assert.Nil(t, sandbox.Status.PodIPs)
	assert.Empty(t, sandbox.Status.NodeName)

	service := &corev1.Service{}
	err := r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, service)
	require.True(t, k8serrors.IsNotFound(err), "must not create a routing Service for an ambiguous Pod mapping")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, corev1.EventTypeWarning)
		assert.Contains(t, event, sandboxv1beta1.SandboxReasonMultiplePods)
	default:
		t.Fatal("expected a warning Event for the Pod mapping conflict")
	}

	// The conflict is watch-driven and does not return an error or emit a new
	// Event on every reconcile while the Ready condition already reports it.
	require.NoError(t, r.reconcileChildResources(t.Context(), sandbox, nil))
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected duplicate Event: %s", event)
	default:
	}
}

func TestSandboxOwnedPodsRequiresOwnerUID(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{UID: sandboxUID}}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "owned", OwnerReferences: []metav1.OwnerReference{sandboxControllerRef("sandbox")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "old-incarnation", OwnerReferences: []metav1.OwnerReference{{UID: "old-sandbox-uid", Controller: new(true)}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "unowned"}},
	}

	owned := sandboxOwnedPods(pods, sandbox)
	require.Len(t, owned, 1)
	assert.Equal(t, "owned", owned[0].Name)
}

func TestReconcilePodWaitsForOwnedTerminatingPod(t *testing.T) {
	const (
		sandboxName = "sandbox-name"
		sandboxNs   = "sandbox-ns"
		nameHash    = "name-hash"
	)

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs, UID: sandboxUID},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	deletionTime := metav1.Now()
	terminatingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "terminating-owned-pod",
			Namespace:         sandboxNs,
			Labels:            map[string]string{sandboxLabel: nameHash},
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
			Finalizers:        []string{"agents.x-k8s.io/test-hold"},
			DeletionTimestamp: &deletionTime,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
	}
	r := &SandboxReconciler{
		Client:        newFakeClient(sandbox, terminatingPod),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	pod, err := r.reconcilePod(t.Context(), sandbox.DeepCopy(), nameHash, nil)
	require.NoError(t, err)
	require.NotNil(t, pod)
	assert.Equal(t, terminatingPod.Name, pod.Name)

	createdPod := &corev1.Pod{}
	err = r.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, createdPod)
	require.True(t, k8serrors.IsNotFound(err), "must wait for the owned terminating Pod instead of overlapping it")
}

func TestServicePortsForSandboxReturnsNilWithoutContainerPorts(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "main",
						}},
					},
				},
			},
		},
	}

	require.Nil(t, servicePortsForSandbox(sandbox))
}

func TestReconcileService(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	nameHash := "name-hash"
	sandboxObj := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(true)}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
	}
	sandboxWithPodSpec := func(podSpec corev1.PodSpec) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sandboxName,
				Namespace: sandboxNs,
				UID:       sandboxUID,
			},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					Service: new(true),
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: podSpec,
					},
				},
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
			},
		}
	}
	sandboxWithContainers := func(containers ...corev1.Container) *sandboxv1beta1.Sandbox {
		return sandboxWithPodSpec(corev1.PodSpec{Containers: containers})
	}
	sandboxWithPorts := func(containerPorts ...corev1.ContainerPort) *sandboxv1beta1.Sandbox {
		return sandboxWithContainers(corev1.Container{
			Name:  "main",
			Ports: containerPorts,
		})
	}
	sandboxWithNilServiceAndPorts := func(containerPorts ...corev1.ContainerPort) *sandboxv1beta1.Sandbox {
		sandbox := sandboxWithPorts(containerPorts...)
		sandbox.Spec.Service = nil
		return sandbox
	}
	servicePortWithName := func(port int32, protocol corev1.Protocol, name string) corev1.ServicePort {
		return corev1.ServicePort{
			Name:       name,
			Protocol:   protocol,
			Port:       port,
			TargetPort: intstr.FromInt32(port),
		}
	}
	servicePort := func(port int32, protocol corev1.Protocol) corev1.ServicePort {
		return servicePortWithName(port, protocol, fmt.Sprintf("p-%d-%s", port, strings.ToLower(string(protocol))))
	}
	alwaysRestart := corev1.ContainerRestartPolicyAlways
	appProtocolHTTP := "http"

	testCases := []struct {
		name                  string
		initialObjs           []runtime.Object
		sandbox               *sandboxv1beta1.Sandbox
		wantService           *corev1.Service
		expectErr             bool
		errContains           string // substring that must appear in the error
		wantNilService        bool
		wantServiceDeleted    bool
		wantStatusService     string
		wantStatusServiceFQDN string
	}{
		{
			name:    "creates a new headless service when none exists and service is true",
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
			name: "creates a new headless service with container ports when service is true",
			sandbox: sandboxWithPorts(corev1.ContainerPort{
				ContainerPort: 8080,
			}),
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
					Ports: []corev1.ServicePort{
						servicePort(8080, corev1.ProtocolTCP),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "creates a new headless service with native sidecar container ports",
			sandbox: sandboxWithPodSpec(corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "main",
				}},
				InitContainers: []corev1.Container{
					{
						Name: "setup",
						Ports: []corev1.ContainerPort{{
							Name:          "setup",
							ContainerPort: 7070,
						}},
					},
					{
						Name:          "proxy",
						RestartPolicy: &alwaysRestart,
						Ports: []corev1.ContainerPort{{
							Name:          "metrics",
							ContainerPort: 15020,
						}},
					},
				},
			}),
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
					Ports: []corev1.ServicePort{
						servicePortWithName(15020, corev1.ProtocolTCP, "metrics"),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "creates a new headless service with sorted unique container ports",
			sandbox: sandboxWithPorts(
				corev1.ContainerPort{ContainerPort: 9090, Protocol: corev1.ProtocolUDP},
				corev1.ContainerPort{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
				corev1.ContainerPort{Name: "http", ContainerPort: 8080},
				corev1.ContainerPort{ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
				corev1.ContainerPort{ContainerPort: 0, Protocol: corev1.ProtocolTCP},
			),
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
					Ports: []corev1.ServicePort{
						servicePortWithName(8080, corev1.ProtocolTCP, "http"),
						servicePort(9090, corev1.ProtocolTCP),
						servicePort(9090, corev1.ProtocolUDP),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "uses the first container port name when duplicate names are reused",
			sandbox: sandboxWithContainers(
				corev1.Container{
					Name: "main",
					Ports: []corev1.ContainerPort{{
						Name:          "http",
						ContainerPort: 9090,
					}},
				},
				corev1.Container{
					Name: "sidecar",
					Ports: []corev1.ContainerPort{{
						Name:          "http",
						ContainerPort: 8080,
					}},
				},
			),
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
					Ports: []corev1.ServicePort{
						servicePort(8080, corev1.ProtocolTCP),
						servicePortWithName(9090, corev1.ProtocolTCP, "http"),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "adjusts generated service port name when it conflicts with an explicit name",
			sandbox: sandboxWithPorts(
				corev1.ContainerPort{Name: "p-8080-tcp", ContainerPort: 9090},
				corev1.ContainerPort{ContainerPort: 8080},
			),
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
					Ports: []corev1.ServicePort{
						servicePortWithName(8080, corev1.ProtocolTCP, "p-8080-tcp-2"),
						servicePortWithName(9090, corev1.ProtocolTCP, "p-8080-tcp"),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "uses existing service owned by this sandbox when service is true",
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
			name: "repairs selector and label drift on service owned by this sandbox when service is true",
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
						Ports: []corev1.ServicePort{
							servicePort(9090, corev1.ProtocolTCP),
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
			name: "repairs port drift on service owned by this sandbox when service is true",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
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
						Ports: []corev1.ServicePort{
							servicePort(9090, corev1.ProtocolTCP),
						},
					},
				},
			},
			sandbox: sandboxWithPorts(corev1.ContainerPort{
				ContainerPort: 8080,
			}),
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
					Ports: []corev1.ServicePort{
						servicePort(8080, corev1.ProtocolTCP),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "preserves unmanaged service port fields when controlled fields match",
			initialObjs: []runtime.Object{
				&corev1.Service{
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
						Ports: []corev1.ServicePort{{
							Name:        "p-8080-tcp",
							Protocol:    corev1.ProtocolTCP,
							Port:        8080,
							TargetPort:  intstr.FromInt32(8080),
							AppProtocol: &appProtocolHTTP,
						}},
					},
				},
			},
			sandbox: sandboxWithPorts(corev1.ContainerPort{
				ContainerPort: 8080,
			}),
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
					Ports: []corev1.ServicePort{{
						Name:        "p-8080-tcp",
						Protocol:    corev1.ProtocolTCP,
						Port:        8080,
						TargetPort:  intstr.FromInt32(8080),
						AppProtocol: &appProtocolHTTP,
					}},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},

		{
			name: "refuses to use service owned by a different controller when service is true",
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
			name: "adopts unowned service and sets controller reference when service is true",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
					},
				},
			},
			sandbox: sandboxWithPorts(corev1.ContainerPort{
				ContainerPort: 8080,
			}),
			wantService: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:            sandboxName,
					Namespace:       sandboxNs,
					ResourceVersion: "2",
					Labels: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						sandboxv1beta1.SandboxAdoptableLabel: "true",
					},
					OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"agents.x-k8s.io/sandbox-name-hash": nameHash,
					},
					Ports: []corev1.ServicePort{
						servicePort(8080, corev1.ProtocolTCP),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "adopts unowned headless service and clears existing ports when sandbox has none",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
					},
					Spec: corev1.ServiceSpec{
						ClusterIP: "None",
						Ports: []corev1.ServicePort{
							servicePort(9090, corev1.ProtocolTCP),
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
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						sandboxv1beta1.SandboxAdoptableLabel: "true",
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
		{
			name: "refuses to adopt unowned service with non-headless ClusterIP when service is true",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
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
			name: "adopts unowned headless service and overwrites wrong selector when service is true",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
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
						"agents.x-k8s.io/sandbox-name-hash":  nameHash,
						sandboxv1beta1.SandboxAdoptableLabel: "true",
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
		{
			name: "adopts unowned headless service carrying legacy tracking label when adoptable label is absent",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
					},
					Spec: corev1.ServiceSpec{
						ClusterIP: "None",
						Selector: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
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
		{
			name: "does not create service when service is nil",
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{},
			},
			wantNilService:        true,
			wantStatusService:     "",
			wantStatusServiceFQDN: "",
		},
		{
			name: "preserves and reconciles owned service when service is nil",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
						OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
					},
					Spec: corev1.ServiceSpec{
						ClusterIP: "None",
						Ports: []corev1.ServicePort{
							servicePort(9090, corev1.ProtocolTCP),
						},
					},
				},
			},
			sandbox: sandboxWithNilServiceAndPorts(corev1.ContainerPort{
				ContainerPort: 8080,
			}),
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
					Ports: []corev1.ServicePort{
						servicePort(9090, corev1.ProtocolTCP),
					},
				},
			},
			wantStatusService:     sandboxName,
			wantStatusServiceFQDN: sandboxName + "." + sandboxNs + ".svc.cluster.local",
		},
		{
			name: "ignores unowned service when service is nil",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
					Spec: corev1.ServiceSpec{
						ClusterIP: "None",
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{},
			},
			wantNilService:        true,
			wantStatusService:     "",
			wantStatusServiceFQDN: "",
		},
		{
			name: "deletes owned service when service is explicitly false",
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
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(false)}},
			},
			wantNilService:        true,
			wantServiceDeleted:    true,
			wantStatusService:     "",
			wantStatusServiceFQDN: "",
		},
		{
			name: "ignores unowned service when service is explicitly false",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
				},
			},
			sandbox: &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sandboxName,
					Namespace: sandboxNs,
					UID:       sandboxUID,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(false)}},
			},
			wantNilService:        true,
			wantStatusService:     "",
			wantStatusServiceFQDN: "",
		},
		{
			name: "refuses to adopt unowned service that lacks pool authorization label",
			initialObjs: []runtime.Object{
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:            sandboxName,
						Namespace:       sandboxNs,
						ResourceVersion: "1",
					},
				},
			},
			sandbox:     sandboxObj,
			wantService: nil,
			expectErr:   true,
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
				// Verify that any initially unowned Service remains unowned (never adopted)
				for _, obj := range tc.initialObjs {
					if initialSvc, ok := obj.(*corev1.Service); ok {
						if len(initialSvc.OwnerReferences) == 0 {
							liveSvc := &corev1.Service{}
							err = r.Get(t.Context(), types.NamespacedName{Name: initialSvc.Name, Namespace: initialSvc.Namespace}, liveSvc)
							require.NoError(t, err)
							assert.Empty(t, liveSvc.OwnerReferences, "expected Service %q to remain unowned after failed reconcile", liveSvc.Name)
						}
					}
				}
			} else {
				require.NoError(t, err)
				if tc.wantNilService {
					require.Nil(t, svc)
				} else {
					require.NotNil(t, svc)
				}
			}

			// Verify status was set correctly
			if !tc.expectErr {
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
			} else if tc.wantServiceDeleted {
				liveSvc := &corev1.Service{}
				err = r.Get(t.Context(), types.NamespacedName{
					Name: sandboxName, Namespace: sandboxNs,
				}, liveSvc)
				require.True(t, k8serrors.IsNotFound(err), "expected service to be deleted but it still exists")
			}
		})
	}
}

func TestCheckOwnership(t *testing.T) {
	sandboxName := "test-sandbox"
	sandboxUID := types.UID("sandbox-uid-123")

	sandbox := &sandboxv1beta1.Sandbox{
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
		APIVersion:         sandboxv1beta1.GroupVersion.String(),
		Kind:               sandboxv1beta1.SandboxKind,
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
	nameHash := NameHash(sandboxName)

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
			{
				EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: pvcTemplateName},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			},
		}},
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
								APIVersion:         sandboxv1beta1.GroupVersion.String(),
								Kind:               sandboxv1beta1.SandboxKind,
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
						Labels: map[string]string{
							sandboxv1beta1.SandboxAdoptableLabel: "true",
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "adopts unowned PVC carrying legacy tracking label when adoptable label is absent",
			initialObjs: []runtime.Object{
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: sandboxNs,
						Labels: map[string]string{
							"agents.x-k8s.io/sandbox-name-hash": nameHash,
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "refuses to adopt unowned PVC that lacks pool authorization label",
			initialObjs: []runtime.Object{
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: sandboxNs,
					},
				},
			},
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := SandboxReconciler{
				Client: newFakeClient(append(tc.initialObjs, sandbox)...),
				Scheme: Scheme,
				Tracer: asmetrics.NewNoOp(),
			}

			err := r.reconcilePVCs(t.Context(), sandbox, nameHash)
			if tc.expectErr {
				require.Error(t, err)
				if tc.errContains != "" {
					require.Contains(t, err.Error(), tc.errContains)
				}
				// Verify that any initially unowned PVC remains unowned (never adopted)
				for _, obj := range tc.initialObjs {
					if initialPVC, ok := obj.(*corev1.PersistentVolumeClaim); ok {
						if len(initialPVC.OwnerReferences) == 0 {
							livePVC := &corev1.PersistentVolumeClaim{}
							err = r.Get(t.Context(), types.NamespacedName{Name: initialPVC.Name, Namespace: initialPVC.Namespace}, livePVC)
							require.NoError(t, err)
							assert.Empty(t, livePVC.OwnerReferences, "expected PVC %q to remain unowned after failed reconcile", livePVC.Name)
						}
					}
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
		deletionPolicy sandboxv1beta1.ShutdownPolicy
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
			deletionPolicy: sandboxv1beta1.ShutdownPolicyRetain,
			wantExpired:    true,
			wantRequeue:    0,
		},
		{
			name:           "shutdown time in past - delete",
			shutdownTime:   new(metav1.NewTime(now.Add(-1 * time.Minute))),
			deletionPolicy: sandboxv1beta1.ShutdownPolicyDelete,
			wantExpired:    true,
			wantRequeue:    0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := &sandboxv1beta1.Sandbox{}
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

// TestReconcileChildResourcesSuspendedForeignPodDoesNotLeakIPOrNodeName verifies
// that when a Sandbox is suspended and a Pod with its name exists but is owned by a
// different controller, reconcilePod surfaces that Pod (so the Suspended condition
// can report PodNotOwned) but its runtime status (PodIPs, NodeName) must NOT leak
// into the Sandbox's status.
func TestReconcileChildResourcesSuspendedForeignPodDoesNotLeakIPOrNodeName(t *testing.T) {
	sandboxName := "sandbox-unowned"
	sandboxNs := "default"
	nameHash := NameHash(sandboxName)

	sandboxObj := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: sandboxNs, UID: "sandbox-uid-123"},
		Spec: sandboxv1beta1.SandboxSpec{
			// Suspended so reconcilePod surfaces the foreign pod (non-nil). In Running
			// mode a foreign pod returns nil+err and never reaches the ownership guard.
			OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test"}}},
				},
			},
		},
	}

	foreignPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			Labels:    map[string]string{sandboxLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "other-rs", UID: "other-uid-999", Controller: new(true)},
			},
		},
		Spec:   corev1.PodSpec{NodeName: "node-foreign", Containers: []corev1.Container{{Name: "test"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "192.168.1.100"}}},
	}

	r := &SandboxReconciler{
		Client:        newFakeClient(sandboxObj, foreignPod),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	// Refusing to delete a foreign pod is a steady state, not an error.
	require.NoError(t, r.reconcileChildResources(t.Context(), sandboxObj, nil))

	assert.Nil(t, sandboxObj.Status.PodIPs, "foreign pod IPs must NOT leak into sandbox status")
	assert.Empty(t, sandboxObj.Status.NodeName, "foreign pod NodeName must NOT leak into sandbox status")
	assert.Equal(t, sandboxLabel+"="+nameHash, sandboxObj.Status.LabelSelector, "LabelSelector must be set for any non-nil pod (including foreign pods)")

	// Confirm we actually hit the foreign-pod path (guards against silently
	// regressing to the pod==nil clearing, which would pass the asserts above for
	// the wrong reason).
	cond := meta.FindStatusCondition(sandboxObj.Status.Conditions, string(sandboxv1beta1.SandboxConditionSuspended))
	require.NotNil(t, cond)
	assert.Equal(t, sandboxv1beta1.SandboxReasonSuspendedPodNotOwned, cond.Reason)
}

// TestPodScheduledConditionRemovedWithPod verifies the PodScheduled condition
// mirrors the backing pod's scheduling state while the pod exists and is
// removed from status once the pod is gone (here via suspension), rather than
// lingering or flipping to a misleading False.
func TestPodScheduledConditionRemovedWithPod(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "podscheduled-sandbox",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
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
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodScheduled,
					Status:  corev1.ConditionFalse,
					Reason:  corev1.PodReasonUnschedulable,
					Message: "0/3 nodes are available: 3 Insufficient cpu.",
				},
			},
		},
	}

	r := &SandboxReconciler{
		Client: newFakeClient(sandbox, pod),
		Scheme: Scheme,
		Tracer: asmetrics.NewNoOp(),
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}}

	_, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)

	updatedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
	scheduledCondition := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionPodScheduled))
	require.NotNil(t, scheduledCondition)
	require.Equal(t, metav1.ConditionFalse, scheduledCondition.Status)
	require.Equal(t, string(corev1.PodReasonUnschedulable), scheduledCondition.Reason)
	require.Equal(t, pod.Status.Conditions[0].Message, scheduledCondition.Message)

	// Suspend the sandbox; the pod is deleted and the mirrored condition
	// must be removed along with it.
	updatedSandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	require.NoError(t, r.Update(t.Context(), updatedSandbox))

	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	// Second pass observes the deleted pod.
	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)

	require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
	require.Nil(t, meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionPodScheduled)))
	require.NotNil(t, meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionSuspended)))
}

// TestPodScheduledConditionUnknownWhenPodLookupFails verifies that a failed Pod
// lookup is not mistaken for a confirmed absent Pod: PodScheduled must report
// Unknown and survive pruning, rather than being removed and implying the
// Sandbox has no backing Pod.
func TestPodScheduledConditionUnknownWhenPodLookupFails(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "podscheduled-lookup-fail",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandbox.Name,
			Namespace:       sandbox.Namespace,
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandbox.Name)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
		},
	}

	// failPodGet toggles Pod Get failures on so the first reconcile can establish
	// the condition and the second can observe the lookup failure.
	failPodGet := false
	inner := newFakeClient(sandbox, pod)
	fc := interceptor.NewClient(inner, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod && failPodGet {
				return k8serrors.NewInternalError(errors.New("pod get failed"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r := &SandboxReconciler{
		Client: fc,
		Scheme: Scheme,
		Tracer: asmetrics.NewNoOp(),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}}

	_, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)

	updatedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
	scheduled := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionPodScheduled))
	require.NotNil(t, scheduled)
	require.Equal(t, metav1.ConditionTrue, scheduled.Status)

	// Now make the Pod lookup fail. The condition must be retained as Unknown,
	// not pruned as it would be for a genuinely absent pod.
	failPodGet = true
	_, err = r.Reconcile(t.Context(), req)
	require.Error(t, err, "reconcile must surface the pod lookup failure")

	require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
	scheduled = meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionPodScheduled))
	require.NotNil(t, scheduled, "PodScheduled must be retained when the pod lookup fails")
	require.Equal(t, metav1.ConditionUnknown, scheduled.Status)
	require.Equal(t, sandboxv1beta1.SandboxReasonPodSchedulingUnknown, scheduled.Reason)
}

// TestSuspendedConditionUnknownWhenPodLookupFails pins the pod error reaching
// the Pod-derived conditions. reconcileChildResources reuses err for both the
// Pod and the Service (the Service's := only introduces svc), so passing it
// straight through hands computeSuspendedCondition the Service error and makes
// its pod-state-unknown branch unreachable: a suspended Sandbox whose Pod could
// not be read then reports Suspended=True/PodTerminated, claiming the Pod is
// gone when its state is simply unknown.
func TestSuspendedConditionUnknownWhenPodLookupFails(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "suspended-lookup-fail",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container"}},
					},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
		},
	}

	fc := interceptor.NewClient(newFakeClient(sandbox), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return k8serrors.NewInternalError(errors.New("pod get failed"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r := &SandboxReconciler{
		Client: fc,
		Scheme: Scheme,
		Tracer: asmetrics.NewNoOp(),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}}

	_, err := r.Reconcile(t.Context(), req)
	require.Error(t, err, "reconcile must surface the pod lookup failure")

	updatedSandbox := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
	suspended := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionSuspended))
	require.NotNil(t, suspended)
	require.Equal(t, metav1.ConditionUnknown, suspended.Status,
		"the pod lookup failed, so suspension cannot be confirmed")
	require.Equal(t, sandboxv1beta1.SandboxReasonSuspendedPodStateUnknown, suspended.Reason)
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
			finishedReason: sandboxv1beta1.SandboxReasonPodSucceeded,
		},
		{
			name:           "failed pod",
			phase:          corev1.PodFailed,
			finishedReason: sandboxv1beta1.SandboxReasonPodFailed,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			shutdownTime := metav1.NewTime(time.Now().Add(time.Hour))
			sandbox := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "ttl-finished-sandbox",
					Namespace:  "default",
					UID:        sandboxUID,
					Generation: 1,
				},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(true),
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "test-container"}},
						},
					}}, Lifecycle: sandboxv1beta1.Lifecycle{
					ShutdownTime:   &shutdownTime,
					ShutdownPolicy: ptr.To(sandboxv1beta1.ShutdownPolicyRetain),
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

			updatedSandbox := &sandboxv1beta1.Sandbox{}
			require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
			finishedCondition := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
			require.NotNil(t, finishedCondition)
			require.Equal(t, tc.finishedReason, finishedCondition.Reason)
			require.NotNil(t, meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionPodScheduled)))
			require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &corev1.Pod{}))
			require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, &corev1.Service{}))

			expiredShutdownTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
			updatedSandbox.Spec.ShutdownTime = &expiredShutdownTime
			require.NoError(t, r.Update(t.Context(), updatedSandbox))

			result, err = r.Reconcile(t.Context(), req)
			require.NoError(t, err)
			require.Greater(t, result.RequeueAfter, time.Duration(0))

			require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedSandbox))
			readyCondition := meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
			require.NotNil(t, readyCondition)
			require.Equal(t, sandboxv1beta1.SandboxReasonExpired, readyCondition.Reason)
			finishedCondition = meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
			require.NotNil(t, finishedCondition)
			require.Equal(t, tc.finishedReason, finishedCondition.Reason)
			// Expiry removes the mirrored PodScheduled condition while
			// Finished is preserved.
			require.Nil(t, meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionPodScheduled)))
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
			readyCondition = meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
			require.NotNil(t, readyCondition)
			require.Equal(t, sandboxv1beta1.SandboxReasonExpired, readyCondition.Reason)
			finishedCondition = meta.FindStatusCondition(updatedSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
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
			r := &SandboxReconciler{
				ClusterDomain: tc.clusterDomain,
			}
			sandbox := &sandboxv1beta1.Sandbox{}
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

// TestSandboxReconcile_ConditionsDoNotAccumulate verifies that reconciling a
// ready sandbox many times does not grow the conditions slice. A bug
// that appends instead of upserts the Ready condition will cause unbounded
// status growth.
func TestSandboxReconcile_ConditionsDoNotAccumulate(t *testing.T) {
	sbName := "no-grow-sandbox"
	sbNs := "default"
	nameHash := NameHash(sbName)

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: sbName, Namespace: sbNs,
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}}, OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: sbName, Namespace: sbNs,
			Labels:          map[string]string{sandboxLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sbName)},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "img"}},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionTrue,
				},
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: sbName, Namespace: sbNs,
			Labels:          map[string]string{sandboxLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sbName)},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  map[string]string{sandboxLabel: nameHash},
		},
	}

	fc := newFakeClient(sandbox, pod, svc)
	r := &SandboxReconciler{
		Client: fc,
		Scheme: Scheme,
		Tracer: asmetrics.NewNoOp(),
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sbName, Namespace: sbNs}}

	const iters = 20
	for i := range iters {
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err, "reconcile iteration %d", i)
	}

	var got sandboxv1beta1.Sandbox
	require.NoError(t, fc.Get(ctx, types.NamespacedName{Name: sbName, Namespace: sbNs}, &got))
	// Steady state for a running, ready sandbox: Suspended, PodScheduled, Ready.
	require.Len(t, got.Status.Conditions, 3,
		"conditions slice must not grow across %d reconcile iterations — controller must upsert not append", iters)
}

type mockTracer struct {
	asmetrics.Instrumenter
	capturedAttrs map[string]string
}

func (m *mockTracer) StartSpan(ctx context.Context, _ metav1.Object, _ string, attrs map[string]string) (context.Context, func()) {
	if len(attrs) > 0 {
		m.capturedAttrs = attrs
	}
	return ctx, func() {}
}

func (m *mockTracer) GetTraceContext(_ context.Context) string {
	return ""
}

func (m *mockTracer) IsRecording(_ context.Context) bool {
	return true
}

func (m *mockTracer) AddEvent(_ context.Context, _ string, _ map[string]string) {}

func TestReconcile_TracingNormalization(t *testing.T) {
	sbName := "tracing-test-sandbox"
	sbNs := "default"
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbName,
			Namespace: sbNs,
			UID:       "uid-1",
			Labels: map[string]string{
				sandboxv1beta1.CreatedByLabel: "invalid-value",
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test-container", Image: "nginx"}},
					},
				},
			},
		},
	}

	fc := newFakeClient(sandbox)
	mt := &mockTracer{}
	r := &SandboxReconciler{
		Client:        fc,
		Scheme:        Scheme,
		Tracer:        mt,
		ClusterDomain: "cluster.local",
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sbName, Namespace: sbNs}}

	var sb sandboxv1beta1.Sandbox
	require.NoError(t, fc.Get(ctx, req.NamespacedName, &sb))

	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	require.NotNil(t, mt.capturedAttrs)
	require.Equal(t, "unknown", mt.capturedAttrs[sandboxv1beta1.CreatedByLabel], "created-by label must be normalized in span attributes")
}

func TestNameHash_Correctness(t *testing.T) {
	// Verify the fast hex encoding produces the same output as the
	// reference implementation (fmt.Sprintf("%08x", ...)).
	cases := []string{
		"",
		"a",
		"my-sandbox",
		"test-template-custom",
		"pool",
		"sandbox-name-with-a-very-long-label-value",
	}

	// Supplement with 100 randomized DNS-label-shaped strings so bit
	// manipulation is exercised across a broader input distribution.
	// Seeded for reproducibility.
	rng := rand.New(rand.NewPCG(42, 0))
	const dnsLabelChars = "abcdefghijklmnopqrstuvwxyz0123456789-"
	for range 100 {
		n := rng.IntN(63) + 1 // length in [1, 63]
		var buf [63]byte
		for i := range n {
			buf[i] = dnsLabelChars[rng.IntN(len(dnsLabelChars))]
		}
		cases = append(cases, string(buf[:n]))
	}

	for _, name := range cases {
		got := NameHash(name)
		if len(got) != 8 {
			t.Errorf("NameHash(%q) length = %d, want 8", name, len(got))
		}
		// Verify all chars are lowercase hex digits.
		for i, c := range got {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Errorf("NameHash(%q)[%d] = %c, want hex digit", name, i, c)
			}
		}
		// Cross-check against GetNumericHash.
		want := fmt.Sprintf("%08x", GetNumericHash(name))
		if got != want {
			t.Errorf("NameHash(%q) = %q, want %q", name, got, want)
		}
	}
}

func BenchmarkNameHashNew(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		_ = NameHash("my-sandbox-name")
	}
}

func BenchmarkNameHashOld(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		_ = fmt.Sprintf("%08x", GetNumericHash("my-sandbox-name"))
	}
}

// TestReconcileCoalescesNodeNameStatusWrite verifies that a status change
// consisting only of the scheduled pod's node name is not written in its own
// API request: the node name rides along with the next status write instead,
// normally the Ready transition.
func TestReconcileCoalescesNodeNameStatusWrite(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}}

	sb := &sandboxv1beta1.Sandbox{}
	sb.Name = sandboxName
	sb.Namespace = sandboxNs
	sb.UID = sandboxUID
	sb.Generation = 1
	sb.Spec = sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
		PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container"}}},
		},
	}}
	r := &SandboxReconciler{
		Client:        newFakeClient(sb),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}

	// Initial reconcile: creates the pod and writes the initial status.
	_, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	beforeBind := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, beforeBind))
	require.Empty(t, beforeBind.Status.NodeName)

	// The pod reports Pending (its state from creation until it runs) and
	// the sandbox status reflects that.
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, pod))
	pod.Status.Phase = corev1.PodPending
	require.NoError(t, r.Status().Update(t.Context(), pod))
	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	beforeBind = &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, beforeBind))

	// Scheduler binds the pod; nothing else about the sandbox changes, so no
	// status write should happen.
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, pod))
	pod.Spec.NodeName = "node-1"
	require.NoError(t, r.Update(t.Context(), pod))

	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	live := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, live))
	assert.Empty(t, live.Status.NodeName, "node-name-only change should not be written on its own")
	assert.Equal(t, beforeBind.ResourceVersion, live.ResourceVersion, "no status write expected for a node-name-only change")

	// Pod becomes Ready: a single status write carries the node name, the
	// pod IPs and the Ready condition together.
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.8"}}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, r.Status().Update(t.Context(), pod))

	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, live))
	assert.Equal(t, "node-1", live.Status.NodeName)
	assert.Equal(t, []string{"10.0.0.8"}, live.Status.PodIPs)
	readyCondition := meta.FindStatusCondition(live.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, readyCondition)
	assert.Equal(t, metav1.ConditionTrue, readyCondition.Status)

	// Once the sandbox is Ready the deferral no longer applies: a node
	// change with no condition change (impossible in practice, but cheap to
	// guard) is written through rather than leaving a Ready sandbox with a
	// stale node name.
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, pod))
	pod.Spec.NodeName = "node-2"
	require.NoError(t, r.Update(t.Context(), pod))

	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, live))
	assert.Equal(t, "node-2", live.Status.NodeName, "node changes on a Ready sandbox must be written immediately")
}
