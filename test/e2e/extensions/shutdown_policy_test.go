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

package extensions

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

const holdDeleteFinalizer = "e2e.agent-sandbox.dev/hold-delete"

// TestSandboxClaimDeleteForeground verifies that a SandboxClaim with
// ShutdownPolicy=DeleteForeground stays in the API with a deletionTimestamp
// until the underlying Sandbox and Pod are fully terminated.
func TestSandboxClaimDeleteForeground(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("claim-fg-delete-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Use a busybox container with a preStop hook that sleeps 10s.
	// This keeps the Pod in Terminating state long enough to observe
	// the Claim still existing with a deletionTimestamp (proving
	// the foreground cascade is blocking).
	// Note: pause:3.10 is used elsewhere in e2e tests, but it exits
	// immediately on SIGTERM and has no shell for preStop hooks.
	gracePeriod := int64(10)
	template := createTemplate(t, tc, ns.Name, "fg-delete-template", corev1.PodSpec{
		TerminationGracePeriodSeconds: &gracePeriod,
		Containers: []corev1.Container{{
			Name:    "busybox",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", "sleep infinity"},
			Lifecycle: &corev1.Lifecycle{
				PreStop: &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "sleep 3"}},
				},
			},
		}},
	})

	shutdownTime := metav1.NewTime(time.Now().Add(30 * time.Second)).Rfc3339Copy()
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fg-delete-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
			Lifecycle: &extensionsv1alpha1.Lifecycle{
				ShutdownPolicy: extensionsv1alpha1.ShutdownPolicyDeleteForeground,
				ShutdownTime:   &shutdownTime,
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for the claim and sandbox to become ready
	tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)

	sandboxID := types.NamespacedName{Name: claim.Name, Namespace: ns.Name}
	require.NoError(t, tc.WaitForSandboxReady(t.Context(), sandboxID))

	pod := &corev1.Pod{}
	pod.Name = claim.Name
	pod.Namespace = ns.Name
	tc.MustWaitForObject(pod, predicates.ReadyConditionIsTrue)
	t.Logf("Sandbox and Pod are ready, waiting for shutdown at %v", shutdownTime.Time)

	// After shutdown time, the claim should get a deletionTimestamp (foreground deletion)
	// but remain in the API until the Pod is fully gone.
	tc.MustWaitForObject(claim, predicates.HasDeletionTimestamp())
	t.Log("Claim has deletionTimestamp — foreground deletion in progress")

	// Wait for the Pod to get a deletionTimestamp via the GC cascade.
	// The preStop hook keeps the Pod in Terminating long enough to observe this.
	tc.MustWaitForObject(pod, predicates.HasDeletionTimestamp())
	tc.MustMatchPredicates(claim, predicates.HasDeletionTimestamp())
	t.Log("Both Claim and Pod have deletionTimestamp — foreground cascade confirmed")

	// Verify the full cascade completes: all owned resources are eventually gone
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	t.Log("Pod fully deleted")

	svc := &corev1.Service{}
	svc.Name = claim.Name
	svc.Namespace = ns.Name
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), svc))
	t.Log("Service fully deleted")

	sandbox := &sandboxv1alpha1.Sandbox{}
	sandbox.Name = claim.Name
	sandbox.Namespace = ns.Name
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), sandbox))
	t.Log("Sandbox fully deleted")

	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), claim))
	t.Log("Claim fully deleted")
}

func TestSandboxClaimTTLDeleteForegroundAfterFinished(t *testing.T) {
	tc := framework.NewTestContext(t)
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("claim-ttl-fg-delete-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := createTemplate(t, tc, ns.Name, "ttl-fg-delete-template", corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name:    "busybox",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", "exit 0"},
		}},
	})

	ttlAfterFinished := int32(5)
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ttl-fg-delete-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
			Lifecycle: &extensionsv1alpha1.Lifecycle{
				ShutdownPolicy:          extensionsv1alpha1.ShutdownPolicyDeleteForeground,
				TTLSecondsAfterFinished: &ttlAfterFinished,
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionFinished), sandboxv1alpha1.SandboxReasonPodSucceeded))
	framework.MustUpdateObject(tc.ClusterClient, pod, func(obj *corev1.Pod) {
		obj.Finalizers = append(obj.Finalizers, holdDeleteFinalizer)
	})

	sandbox := &sandboxv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}

	tc.MustWaitForObject(claim, predicates.HasDeletionTimestamp())
	tc.MustWaitForObject(pod, predicates.HasDeletionTimestamp())
	tc.MustWaitForObject(sandbox, predicates.HasDeletionTimestamp())

	framework.MustUpdateObject(tc.ClusterClient, pod, func(obj *corev1.Pod) {
		filtered := obj.Finalizers[:0]
		for _, finalizer := range obj.Finalizers {
			if finalizer != holdDeleteFinalizer {
				filtered = append(filtered, finalizer)
			}
		}
		obj.Finalizers = filtered
	})

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), svc))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), sandbox))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), claim))
}

func TestSandboxClaimTTLAfterFinished(t *testing.T) {
	testCtx := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("claim-ttl-after-finished-%d", time.Now().UnixNano())
	require.NoError(t, testCtx.CreateWithCleanup(t.Context(), ns))

	template := createTemplate(t, testCtx, ns.Name, "ttl-after-finished-template", corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name:    "busybox",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", "exit 0"},
		}},
	})

	ttlAfterFinished := int32(2)
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ttl-after-finished-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
			Lifecycle: &extensionsv1alpha1.Lifecycle{
				ShutdownPolicy:          extensionsv1alpha1.ShutdownPolicyDelete,
				TTLSecondsAfterFinished: &ttlAfterFinished,
			},
		},
	}
	require.NoError(t, testCtx.CreateWithCleanup(t.Context(), claim))

	podKey := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
	require.Eventually(t, func() bool {
		pod := &corev1.Pod{}
		if err := testCtx.Get(t.Context(), podKey, pod); err != nil {
			return false
		}
		return pod.Status.Phase == corev1.PodSucceeded
	}, 60*time.Second, time.Second)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	sandbox := &sandboxv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}

	require.NoError(t, testCtx.WaitForObjectNotFound(t.Context(), claim))
	require.NoError(t, testCtx.WaitForObjectNotFound(t.Context(), sandbox))
	require.NoError(t, testCtx.WaitForObjectNotFound(t.Context(), pod))
	require.NoError(t, testCtx.WaitForObjectNotFound(t.Context(), svc))
}

func TestSandboxClaimExpiryUsesEarlierOfShutdownTimeAndTTL(t *testing.T) {
	tc := framework.NewTestContext(t)
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("claim-earlier-of-expiry-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := createTemplate(t, tc, ns.Name, "earlier-of-template", corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name:    "busybox",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", "exit 0"},
		}},
	})

	shutdownTime := metav1.NewTime(time.Now().Add(30 * time.Second)).Rfc3339Copy()
	ttlAfterFinished := int32(120)
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "earlier-of-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
			Lifecycle: &extensionsv1alpha1.Lifecycle{
				ShutdownPolicy:          extensionsv1alpha1.ShutdownPolicyDelete,
				ShutdownTime:            &shutdownTime,
				TTLSecondsAfterFinished: &ttlAfterFinished,
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionFinished), sandboxv1alpha1.SandboxReasonPodSucceeded))
	require.Never(t, func() bool {
		current := &extensionsv1alpha1.SandboxClaim{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, current); err != nil {
			return true
		}
		return current.DeletionTimestamp != nil
	}, 5*time.Second, 250*time.Millisecond)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	sandbox := &sandboxv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}

	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), claim))
	require.False(t, time.Now().Before(shutdownTime.Time), "claim deleted before shutdownTime elapsed")
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), sandbox))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), svc))
}

func TestSandboxClaimFinishedWithoutTTLIsRetained(t *testing.T) {
	tc := framework.NewTestContext(t)
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("claim-finished-no-ttl-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := createTemplate(t, tc, ns.Name, "finished-no-ttl-template", corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name:    "busybox",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", "exit 0"},
		}},
	})

	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "finished-no-ttl-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
			Lifecycle: &extensionsv1alpha1.Lifecycle{
				ShutdownPolicy: extensionsv1alpha1.ShutdownPolicyDelete,
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionFinished), sandboxv1alpha1.SandboxReasonPodSucceeded))
	require.Never(t, func() bool {
		current := &extensionsv1alpha1.SandboxClaim{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, current); err != nil {
			return true
		}
		return current.DeletionTimestamp != nil
	}, 5*time.Second, 250*time.Millisecond)
	tc.MustExist(&sandboxv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}})
}

func TestSandboxClaimTTLZeroRetainPreservesFinishedConditionDuringCleanup(t *testing.T) {
	tc := framework.NewTestContext(t)
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("claim-ttl-zero-retain-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := createTemplate(t, tc, ns.Name, "ttl-zero-retain-template", corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name:    "busybox",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c", "sleep 2; exit 0"},
		}},
	})

	ttlAfterFinished := int32(0)
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ttl-zero-retain-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
			Lifecycle: &extensionsv1alpha1.Lifecycle{
				ShutdownPolicy:          extensionsv1alpha1.ShutdownPolicyRetain,
				TTLSecondsAfterFinished: &ttlAfterFinished,
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	sandbox := &sandboxv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	require.Eventually(t, func() bool {
		return tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox) == nil
	}, 60*time.Second, time.Second)
	framework.MustUpdateObject(tc.ClusterClient, sandbox, func(obj *sandboxv1alpha1.Sandbox) {
		obj.Finalizers = append(obj.Finalizers, holdDeleteFinalizer)
	})

	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionReady), extensionsv1alpha1.ClaimExpiredReason))
	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionFinished), sandboxv1alpha1.SandboxReasonPodSucceeded))
	tc.MustWaitForObject(sandbox, predicates.HasDeletionTimestamp())

	framework.MustUpdateObject(tc.ClusterClient, sandbox, func(obj *sandboxv1alpha1.Sandbox) {
		filtered := obj.Finalizers[:0]
		for _, finalizer := range obj.Finalizers {
			if finalizer != holdDeleteFinalizer {
				filtered = append(filtered, finalizer)
			}
		}
		obj.Finalizers = filtered
	})

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), svc))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), sandbox))

	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionReady), extensionsv1alpha1.ClaimExpiredReason))
	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1alpha1.SandboxConditionFinished), sandboxv1alpha1.SandboxReasonPodSucceeded))
	tc.MustMatchPredicates(claim, predicates.NotDeleted())
}

func createTemplate(t *testing.T, tc *framework.TestContext, namespace, name string, podSpec corev1.PodSpec) *extensionsv1alpha1.SandboxTemplate {
	t.Helper()
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			NetworkPolicyManagement: extensionsv1alpha1.NetworkPolicyManagementUnmanaged,
			PodTemplate:             sandboxv1alpha1.PodTemplate{Spec: podSpec},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))
	return template
}
