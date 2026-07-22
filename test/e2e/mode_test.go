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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestSandboxSuspendResumeCycleWithOperatingMode(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("my-sandbox-ns-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))
	// Create a Sandbox Object
	sandboxObj := simpleSandbox(ns.Name)
	sandboxObj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)
	svc := "my-sandbox"
	fqdn := fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name)
	selector := "agents.x-k8s.io/sandbox-name-hash=" + nameHash

	// runningStatus / suspendedStatus describe the expected Sandbox status at a
	// given spec generation. Suspended must persist across the whole lifecycle
	// (never disappear), toggling True<->False with its observedGeneration
	// tracking the spec generation.
	runningStatus := func(gen int64) sandboxv1beta1.SandboxStatus {
		return sandboxv1beta1.SandboxStatus{
			Service: svc, ServiceFQDN: fqdn, LabelSelector: selector,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1beta1.SandboxConditionSuspended),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gen,
					Reason:             sandboxv1beta1.SandboxReasonNotSuspended,
					Message:            "Sandbox is not suspended",
				},
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gen,
					Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
					Message:            "Pod is Ready; Service Exists",
				},
			},
		}
	}
	suspendedStatus := func(gen int64) sandboxv1beta1.SandboxStatus {
		return sandboxv1beta1.SandboxStatus{
			Service: svc, ServiceFQDN: fqdn, LabelSelector: selector,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1beta1.SandboxConditionSuspended),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gen,
					Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
					Message:            "Pod has been terminated. Sandbox is suspended",
				},
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gen,
					Reason:             sandboxv1beta1.SandboxReasonSuspended,
					Message:            "Sandbox is suspended",
				},
			},
		}
	}

	// 1. Initial running state at generation 1: Pod and Service exist.
	tc.MustWaitForObject(sandboxObj, predicates.SandboxHasStatus(runningStatus(1)))
	service := &corev1.Service{}
	service.Name = "my-sandbox"
	service.Namespace = ns.Name
	tc.MustExist(service)
	pod := &corev1.Pod{}
	pod.Name = "my-sandbox"
	pod.Namespace = ns.Name
	tc.MustExist(pod)

	lastTime := getSuspendedConditionTime(t, tc, sandboxObj)

	// 2. Drive multiple suspend/resume cycles. Each operatingMode patch bumps the
	// spec generation by one, so generations climb 1 -> 2 -> 3 -> 4 -> 5. Every
	// cycle asserts the Pod is deleted on suspend / recreated on resume while the
	// Service persists throughout.
	gen := int64(1)
	for cycle := 1; cycle <= 2; cycle++ {
		// Suspend: Pod is deleted, Service persists, Suspended -> True/PodTerminated.
		gen++
		time.Sleep(1 * time.Second) // Ensure LastTransitionTime advances
		framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1beta1.Sandbox) {
			obj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
		})
		tc.MustWaitForObject(sandboxObj, predicates.SandboxHasStatus(suspendedStatus(gen)))

		suspendTime := getSuspendedConditionTime(t, tc, sandboxObj)
		require.True(t, suspendTime.After(lastTime), "suspended LastTransitionTime must advance (suspendTime=%v, lastTime=%v)", suspendTime, lastTime)
		lastTime = suspendTime

		pod = &corev1.Pod{}
		pod.Name = "my-sandbox"
		pod.Namespace = ns.Name
		require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
		tc.MustMatchPredicates(service, predicates.NotDeleted())

		// Resume: Pod is recreated, Service persists, Suspended -> False/NotSuspended.
		gen++
		time.Sleep(1 * time.Second) // Ensure LastTransitionTime advances
		framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1beta1.Sandbox) {
			obj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
		})
		tc.MustWaitForObject(sandboxObj, predicates.SandboxHasStatus(runningStatus(gen)))

		resumeTime := getSuspendedConditionTime(t, tc, sandboxObj)
		require.True(t, resumeTime.After(lastTime), "resumed LastTransitionTime must advance (resumeTime=%v, lastTime=%v)", resumeTime, lastTime)
		lastTime = resumeTime

		pod = &corev1.Pod{}
		pod.Name = "my-sandbox"
		pod.Namespace = ns.Name
		tc.MustExist(pod)
		tc.MustMatchPredicates(service, predicates.NotDeleted())
	}
}

func TestSandboxSuspensionWithTerminatingPod(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("my-sandbox-ns-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))
	// Create a Sandbox Object
	sandboxObj := simpleSandbox(ns.Name)
	sandboxObj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)
	// Wait for Sandbox to become Ready
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1beta1.SandboxConditionSuspended),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 1,
					Reason:             sandboxv1beta1.SandboxReasonNotSuspended,
					Message:            "Sandbox is not suspended",
				},
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
					Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
					Message:            "Pod is Ready; Service Exists",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandboxObj, p...)

	time1 := getSuspendedConditionTime(t, tc, sandboxObj)

	// Assert Pod exists
	pod := &corev1.Pod{}
	pod.Name = "my-sandbox"
	pod.Namespace = ns.Name
	tc.MustExist(pod)

	// 1. Add finalizer to the Pod to prevent complete deletion
	framework.MustUpdateObject(tc.ClusterClient, pod, func(obj *corev1.Pod) {
		obj.Finalizers = append(obj.Finalizers, "agents.x-k8s.io/test-hold")
	})

	// Strip the finalizer on teardown.
	t.Cleanup(func() {
		ctx := context.Background()
		p := &corev1.Pod{}
		if err := tc.Get(ctx, types.NamespacedName{Name: "my-sandbox", Namespace: ns.Name}, p); err != nil {
			if !apierrors.IsNotFound(err) {
				t.Logf("cleanup: failed to get Pod to strip test-hold finalizer: %v", err)
			}
			return
		}
		var kept []string
		removed := false
		for _, f := range p.Finalizers {
			if f == "agents.x-k8s.io/test-hold" {
				removed = true
				continue
			}
			kept = append(kept, f)
		}
		if !removed {
			return
		}
		p.Finalizers = kept
		if err := tc.Update(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: failed to remove test-hold finalizer: %v", err)
		}
	})

	// 2. Set Sandbox operating mode to suspended
	time.Sleep(1 * time.Second) // Ensure LastTransitionTime advances
	framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1beta1.Sandbox) {
		obj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	})

	// 3. Wait for Sandbox status to show PodTerminating (Suspended: False)
	p = []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1beta1.SandboxConditionSuspended),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 2,
					Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminating,
					Message:            "Pod is terminating. Sandbox is suspending",
				},
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 2,
					Reason:             sandboxv1beta1.SandboxReasonSuspended,
					Message:            "Sandbox is suspending",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandboxObj, p...)

	time2 := getSuspendedConditionTime(t, tc, sandboxObj)
	require.Equal(t, time1, time2, "LastTransitionTime must NOT change during False -> False transition (NotSuspended -> PodTerminating)")

	// Verify Pod still exists and has a DeletionTimestamp set
	livePod := &corev1.Pod{}
	livePod.Name = "my-sandbox"
	livePod.Namespace = ns.Name
	tc.MustExist(livePod)
	require.NotNil(t, livePod.DeletionTimestamp, "expected Pod to have DeletionTimestamp set")

	// 4. Remove finalizer from the Pod to let deletion finish
	framework.MustUpdateObject(tc.ClusterClient, livePod, func(obj *corev1.Pod) {
		var newFinalizers []string
		for _, f := range obj.Finalizers {
			if f != "agents.x-k8s.io/test-hold" {
				newFinalizers = append(newFinalizers, f)
			}
		}
		obj.Finalizers = newFinalizers
	})

	// 5. Wait for Sandbox status to show PodTerminated (Suspended: True)
	p = []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Type:               string(sandboxv1beta1.SandboxConditionSuspended),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 2,
					Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
					Message:            "Pod has been terminated. Sandbox is suspended",
				},
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 2,
					Reason:             sandboxv1beta1.SandboxReasonSuspended,
					Message:            "Sandbox is suspended",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandboxObj, p...)

	time3 := getSuspendedConditionTime(t, tc, sandboxObj)
	require.True(t, time3.After(time1), "LastTransitionTime must advance on False -> True transition (PodTerminating -> PodTerminated) (time3=%v, time1=%v)", time3, time1)

	// Verify Pod is completely gone
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), livePod))
}

// TestSandboxPodDeletionKeepsSuspendedFalse verifies that a Pod disruption while
// the Sandbox is Running (e.g. a crash/eviction, not a suspend) does NOT flip the
// Suspended condition.
func TestSandboxPodDeletionKeepsSuspendedFalse(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("my-sandbox-ns-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	sandboxObj := simpleSandbox(ns.Name)
	sandboxObj.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)
	runningReady := sandboxv1beta1.SandboxStatus{
		Service:       "my-sandbox",
		ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
		LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
		Conditions: []metav1.Condition{
			{
				Type:               string(sandboxv1beta1.SandboxConditionSuspended),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: 1,
				Reason:             sandboxv1beta1.SandboxReasonNotSuspended,
				Message:            "Sandbox is not suspended",
			},
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
				Message:            "Pod is Ready; Service Exists",
			},
		},
	}
	tc.MustWaitForObject(sandboxObj, predicates.SandboxHasStatus(runningReady))

	time1 := getSuspendedConditionTime(t, tc, sandboxObj)

	// Capture the current Pod and delete it directly (simulating a crash/eviction).
	pod := &corev1.Pod{}
	pod.Name = "my-sandbox"
	pod.Namespace = ns.Name
	tc.MustExist(pod)
	oldPodUID := pod.UID
	require.NoError(t, tc.Delete(t.Context(), pod))

	// The controller must recreate the Pod (new UID), proving the disruption happened.
	podKey := types.NamespacedName{Name: "my-sandbox", Namespace: ns.Name}
	require.Eventually(t, func() bool {
		p := &corev1.Pod{}
		if err := tc.Get(t.Context(), podKey, p); err != nil {
			return false
		}
		return p.UID != "" && p.UID != oldPodUID
	}, 2*time.Minute, 2*time.Second, "controller did not recreate the pod after deletion")

	// Suspended must still be False/NotSuspended at generation 1 (a pod restart is
	// not a suspend), and the Sandbox must recover to Ready.
	tc.MustWaitForObject(sandboxObj, predicates.SandboxHasStatus(runningReady))

	time2 := getSuspendedConditionTime(t, tc, sandboxObj)
	require.Equal(t, time1, time2, "Suspended LastTransitionTime must not change during a pod restart/disruption")
}

func getSuspendedConditionTime(t *testing.T, tc *framework.TestContext, sandboxObj *sandboxv1beta1.Sandbox) time.Time {
	liveSandbox := &sandboxv1beta1.Sandbox{}
	err := tc.Get(tc.Context(), types.NamespacedName{Name: sandboxObj.Name, Namespace: sandboxObj.Namespace}, liveSandbox)
	require.NoError(t, err)

	cond := meta.FindStatusCondition(liveSandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionSuspended))
	require.NotNil(t, cond, "Suspended condition must be present in Sandbox status")
	require.False(t, cond.LastTransitionTime.IsZero(), "LastTransitionTime must be nonzero")
	return cond.LastTransitionTime.Time
}

func TestSandboxSuspensionBlockedByUnownedPodConflict(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sb-unowned-e2e-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	sandboxName := "my-sandbox-conflict"

	// 1. Pre-create a Pod named "my-sandbox-conflict" in the namespace, but without any controller references.
	// This represents the "unowned" naming conflict.
	unownedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns.Name,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), unownedPod))

	// 2. Create the Sandbox object with operatingMode = Suspended and Service = true.
	serviceReq := true
	sandboxObj := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns.Name,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				Service: &serviceReq,
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "pause",
								Image: "registry.k8s.io/pause:3.10",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)
	svc := "my-sandbox-conflict"
	fqdn := fmt.Sprintf("my-sandbox-conflict.%s.svc.cluster.local", ns.Name)
	selector := "agents.x-k8s.io/sandbox-name-hash=" + nameHash

	expectedStatus := sandboxv1beta1.SandboxStatus{
		Service: svc, ServiceFQDN: fqdn, LabelSelector: selector,
		Conditions: []metav1.Condition{
			{
				Type:               string(sandboxv1beta1.SandboxConditionSuspended),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: 1,
				Reason:             sandboxv1beta1.SandboxReasonSuspendedPodNotOwned,
				Message:            "Refused to delete pod because it is not owned by this sandbox",
			},
			{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: 1,
				Reason:             sandboxv1beta1.SandboxReasonSuspended,
				Message:            "Sandbox is suspending",
			},
		},
	}
	tc.MustWaitForObject(sandboxObj, predicates.SandboxHasStatus(expectedStatus))

	// 4. Verify that the unowned Pod still exists in the namespace (i.e. was not deleted/hijacked).
	livePod := &corev1.Pod{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: sandboxName, Namespace: ns.Name}, livePod))
}
