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

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestInPlaceResourceResizePreservesPodIdentity(t *testing.T) {
	tc := framework.NewTestContext(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: fmt.Sprintf("sandbox-resize-test-%d", time.Now().UnixNano()),
	}}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), namespace))

	sandbox := simpleSandbox(namespace.Name)
	sandbox.Spec.ResourceResizePolicy = &sandboxv1beta1.ResourceResizePolicy{
		Type: sandboxv1beta1.ResourceResizePolicyInPlace,
	}
	sandbox.Spec.PodTemplate.Spec.Containers[0].Resources = resizeResources("10m", "32Mi")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))
	tc.MustWaitForObject(sandbox, predicates.ReadyConditionIsTrue)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace}}
	tc.MustWaitForObject(pod, predicates.ReadyConditionIsTrue)
	before := &corev1.Pod{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, before))
	require.Len(t, before.Status.ContainerStatuses, 1)
	beforeUID := before.UID
	beforeRestartCount := before.Status.ContainerStatuses[0].RestartCount

	desiredResources := resizeResources("20m", "64Mi")
	framework.MustUpdateObject(tc.ClusterClient, sandbox, func(current *sandboxv1beta1.Sandbox) {
		current.Spec.PodTemplate.Spec.Containers[0].Resources = desiredResources
	})

	require.NoError(t, tc.WaitForObject(t.Context(), sandbox,
		&predicates.StatusPredicate{
			MatchType:   string(sandboxv1beta1.SandboxConditionResourceResize),
			MatchStatus: metav1.ConditionTrue,
		},
		predicates.ConditionReasonEquals(
			string(sandboxv1beta1.SandboxConditionResourceResize),
			sandboxv1beta1.SandboxReasonResourceResizeCompleted,
		),
	))

	after := &corev1.Pod{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, after))
	require.Equal(t, beforeUID, after.UID, "in-place resize must not replace the Pod")
	require.Len(t, after.Status.ContainerStatuses, 1)
	require.Equal(t, beforeRestartCount, after.Status.ContainerStatuses[0].RestartCount,
		"in-place resize must not restart the container")
	require.True(t, equalResizeResources(desiredResources, after.Spec.Containers[0].Resources),
		"Pod spec did not receive the requested CPU and memory")
	require.NotNil(t, after.Status.ContainerStatuses[0].Resources)
	require.True(t, equalResizeResources(desiredResources, *after.Status.ContainerStatuses[0].Resources),
		"kubelet did not apply the requested CPU and memory")
}

func resizeResources(cpu, memory string) corev1.ResourceRequirements {
	resources := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
	}
	return corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()}
}

func equalResizeResources(want, got corev1.ResourceRequirements) bool {
	for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		wantRequest, gotRequest := want.Requests[resourceName], got.Requests[resourceName]
		wantLimit, gotLimit := want.Limits[resourceName], got.Limits[resourceName]
		if wantRequest.Cmp(gotRequest) != 0 || wantLimit.Cmp(gotLimit) != 0 {
			return false
		}
	}
	return true
}
