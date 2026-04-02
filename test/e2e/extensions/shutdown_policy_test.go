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
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fg-delete-template",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			NetworkPolicyManagement: extensionsv1alpha1.NetworkPolicyManagementUnmanaged,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &gracePeriod,
					Containers: []corev1.Container{
						{
							Name:    "busybox",
							Image:   "busybox:1.36",
							Command: []string{"sh", "-c", "sleep infinity"},
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"sh", "-c", "sleep 3"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	shutdownTime := metav1.NewTime(time.Now().Add(30 * time.Second)).Rfc3339Copy()
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fg-delete-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "fg-delete-template"},
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
