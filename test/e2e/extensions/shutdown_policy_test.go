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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	replicas := int32(0)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fg-delete-warmpool", Namespace: ns.Name},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	shutdownTime := metav1.NewTime(time.Now().Add(30 * time.Second)).Rfc3339Copy()
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fg-delete-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
			Lifecycle: &extensionsv1beta1.Lifecycle{
				ShutdownPolicy: extensionsv1beta1.ShutdownPolicyDeleteForeground,
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

	sandbox := &sandboxv1beta1.Sandbox{}
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

	replicas := int32(0)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ttl-fg-delete-warmpool", Namespace: ns.Name},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	ttlAfterFinished := int32(5)
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ttl-fg-delete-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
			Lifecycle: &extensionsv1beta1.Lifecycle{
				ShutdownPolicy:          extensionsv1beta1.ShutdownPolicyDeleteForeground,
				TTLSecondsAfterFinished: &ttlAfterFinished,
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1beta1.SandboxConditionFinished), sandboxv1beta1.SandboxReasonPodSucceeded))
	framework.MustUpdateObject(tc.ClusterClient, pod, func(obj *corev1.Pod) {
		obj.Finalizers = append(obj.Finalizers, holdDeleteFinalizer)
	})

	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}

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

// TestSandboxClaimPoolDefaultRetain verifies that a pool default cleans up a
// long-running sandbox without an SDK shutdown hook or an explicit claim lifecycle.
func TestSandboxClaimPoolDefaultRetain(t *testing.T) {
	tc := framework.NewTestContext(t)
	ctx := t.Context()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("claim-pool-ttl-%d", time.Now().UnixNano())}}
	require.NoError(t, tc.CreateWithCleanup(ctx, ns))
	template := createTemplate(t, tc, ns.Name, "pool-ttl-template", corev1.PodSpec{
		Containers: []corev1.Container{{Name: "pause", Image: "registry.k8s.io/pause:3.10"}},
	})
	replicas, ttl := int32(1), int32(30)
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-ttl", Namespace: ns.Name},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:      &replicas,
			TemplateRef:   extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
			ClaimDefaults: &extensionsv1beta1.SandboxClaimDefaults{TTLSecondsAfterCreated: &ttl},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(ctx, pool))
	// Prewarm before starting the claim's deadline so image pulls do not consume TTL.
	waitForWarmPoolSandboxReady(t, tc, ns.Name, pool)
	warm := &sandboxv1beta1.SandboxList{}
	require.NoError(t, tc.List(ctx, warm, client.InNamespace(ns.Name)))
	warmUIDs := make(map[types.UID]bool)
	for _, sb := range warm.Items {
		if metav1.IsControlledBy(&sb, pool) && isSandboxReady(&sb) {
			warmUIDs[sb.UID] = true
		}
	}
	require.NotEmpty(t, warmUIDs)
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "default-retain", Namespace: ns.Name},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name}},
	}
	require.NoError(t, tc.CreateWithCleanup(ctx, claim))
	claimUID := claim.UID
	waitForClaimReady(t, tc, claim)
	require.Nil(t, claim.Spec.Lifecycle, "the controller must not mutate user intent")
	require.NotNil(t, claim.Status.DefaultShutdownTime)
	deadline := claim.CreationTimestamp.Add(time.Duration(ttl) * time.Second)
	require.True(t, deadline.Equal(claim.Status.DefaultShutdownTime.Time))
	sb := &sandboxv1beta1.Sandbox{}
	key := types.NamespacedName{Namespace: ns.Name, Name: claim.Status.SandboxStatus.Name}
	require.NoError(t, tc.Get(ctx, key, sb))
	require.True(t, warmUIDs[sb.UID], "the claim must adopt the prewarmed Sandbox")
	pod := &corev1.Pod{}
	require.NoError(t, tc.Get(ctx, key, pod))
	require.True(t, metav1.IsControlledBy(pod, sb))
	// Observe actual running resources before expiry, not just a retained API record.
	require.Greater(t, time.Until(deadline), 3*time.Second)
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); {
		require.NoError(t, tc.Get(ctx, key, sb))
		require.True(t, sb.DeletionTimestamp.IsZero())
		require.NoError(t, tc.Get(ctx, key, pod))
		require.True(t, pod.DeletionTimestamp.IsZero())
		time.Sleep(200 * time.Millisecond)
	}
	tc.MustWaitForObject(claim, predicates.ConditionReasonEquals(string(sandboxv1beta1.SandboxConditionReady), extensionsv1beta1.ClaimExpiredReason))
	require.NoError(t, tc.WaitForObjectNotFound(ctx, sb))
	require.NoError(t, tc.WaitForObjectNotFound(ctx, pod))
	require.NoError(t, tc.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: claim.Name}, claim))
	require.Equal(t, claimUID, claim.UID)
	require.True(t, claim.DeletionTimestamp.IsZero(), "Retain preserves the Claim record")
	require.Nil(t, claim.Spec.Lifecycle)
	require.True(t, deadline.Equal(claim.Status.DefaultShutdownTime.Time), "the deadline must remain frozen")
}

func createTemplate(t *testing.T, tc *framework.TestContext, namespace, name string, podSpec corev1.PodSpec) *extensionsv1beta1.SandboxTemplate {
	t.Helper()
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: podSpec}}, NetworkPolicyManagement: extensionsv1beta1.NetworkPolicyManagementUnmanaged},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))
	return template
}
