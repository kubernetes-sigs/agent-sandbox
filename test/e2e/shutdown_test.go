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
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestSandboxShutdownTime(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace with unique name to avoid conflicts
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-shutdown-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))
	// Create a Sandbox Object
	sandboxObj := simpleSandbox(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)
	// Assert Sandbox object status reconciles as expected
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
			Replicas:      1,
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
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
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
					Reason:             sandboxv1alpha1.SandboxReasonReady,
					Message:            "Sandbox is operational",
				},
			},
		}),
	}
	require.NoError(t, tc.WaitForObject(t.Context(), sandboxObj, p...))
	// Assert Pod and Service objects exist
	pod := &corev1.Pod{}
	pod.Name = "my-sandbox"
	pod.Namespace = ns.Name
	tc.MustExist(pod)
	service := &corev1.Service{}
	service.Name = "my-sandbox"
	service.Namespace = ns.Name
	tc.MustExist(service)

	// Set a shutdown time that ends shortly, truncated to second-level precision (RFC3339) to match
	// the Kubernetes API's storage behavior.
	shutdown := metav1.NewTime(time.Now().Add(10 * time.Second)).Rfc3339Copy()
	framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1alpha1.Sandbox) {
		obj.Spec.ShutdownTime = &shutdown
	})

	// Wait for sandbox status to reflect new state
	p = []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			// Service/ServiceFQDN should be cleared from status when the Service is deleted
			Service:     "",
			ServiceFQDN: "",
			Replicas:    0,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 2,
					Reason:             sandboxv1alpha1.SandboxReasonSystemTermination,
					Message:            "Sandbox has expired",
				},
			},
		}),
	}
	require.NoError(t, tc.PollUntilObjectMatches(sandboxObj, p...))
	// Verify that the sandbox was shut down at or after the specified shutdownTime
	require.False(t, time.Now().Before(shutdown.Time))
	// Verify Pod and Service are deleted
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), service))
}

func TestSandboxRetainedExpiryPreservesFinishedCondition(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-retain-expiry-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	shutdown := metav1.NewTime(time.Now().Add(8 * time.Second)).Rfc3339Copy()
	policy := sandboxv1alpha1.ShutdownPolicyRetain
	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retain-finished-sandbox",
			Namespace: ns.Name,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "busybox",
						Image:   "busybox:1.36",
						Command: []string{"sh", "-c", "exit 0"},
					}},
				},
			},
			Lifecycle: sandboxv1alpha1.Lifecycle{
				ShutdownTime:   &shutdown,
				ShutdownPolicy: &policy,
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))

	tc.MustWaitForObject(sandbox, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionFinished), sandboxv1alpha1.SandboxReasonPodSucceeded))

	require.Eventually(t, func() bool {
		current := &sandboxv1alpha1.Sandbox{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, current); err != nil {
			return false
		}
		readyReasonMatches, err := predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionReady), sandboxv1alpha1.SandboxReasonExpired).Matches(current)
		if err != nil || !readyReasonMatches {
			return false
		}
		finishedReasonMatches, err := predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionFinished), sandboxv1alpha1.SandboxReasonPodSucceeded).Matches(current)
		if err != nil || !finishedReasonMatches {
			return false
		}
		return current.Status.Service == "" && current.Status.ServiceFQDN == "" && current.Status.Replicas == 0
	}, 60*time.Second, time.Second)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace}}
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), service))
}
