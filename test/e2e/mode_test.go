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

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestSandboxRunSuspendResumeLifecycle(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("my-sandbox-lifecycle-ns-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// 1. Create a Sandbox Object (defaults to Running mode)
	sandboxObj := simpleSandbox(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)

	// 2. Assert Sandbox becomes Ready
	pReady := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1, // Initial creation
					Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
					Message:            "Pod is Ready; Service Exists",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandboxObj, pReady...)

	// 3. Assert Pod and Service objects exist
	pod := &corev1.Pod{}
	pod.Name = "my-sandbox"
	pod.Namespace = ns.Name
	tc.MustExist(pod)

	service := &corev1.Service{}
	service.Name = "my-sandbox"
	service.Namespace = ns.Name
	tc.MustExist(service)

	// 4. Suspend the Sandbox
	t.Log("Suspending the sandbox")
	framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1beta1.Sandbox) {
		obj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	})

	// 5. Assert Sandbox becomes Suspended
	pSuspended := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash, // Should be retained from previous state
			PodIPs:        nil,                                             // Should be cleared
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 2, // First update
					Reason:             sandboxv1beta1.SandboxReasonSuspended,
					Message:            "Sandbox is suspended",
				},
				{
					Type:               string(sandboxv1beta1.SandboxConditionSuspended),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 2, // First update
					Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
					Message:            "Pod has been terminated. Sandbox is not operational.",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandboxObj, pSuspended...)

	// 6. Verify Pod is deleted and Service still exists
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	tc.MustExist(service)

	// 7. Resume the Sandbox
	t.Log("Resuming the sandbox")
	framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1beta1.Sandbox) {
		obj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	})

	// 8. Assert Sandbox becomes Ready again
	pResumed := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1beta1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 3, // Second update
					Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
					Message:            "Pod is Ready; Service Exists",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandboxObj, pResumed...)

	// 9. Verify Pod exists again
	tc.MustExist(pod)
}
