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
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// TestNamespacedModeWatchedNamespaceReconciled verifies that a sandbox created
// in a watched namespace is reconciled and reaches a Ready state.
func TestNamespacedModeWatchedNamespaceReconciled(t *testing.T) {
	if os.Getenv("NAMESPACED_MODE") != "true" {
		t.Skip("NAMESPACED_MODE not enabled; skipping namespaced mode test")
	}
	watchedNs := os.Getenv("WATCHED_NAMESPACE")
	if watchedNs == "" {
		t.Fatal("WATCHED_NAMESPACE must be set when NAMESPACED_MODE=true")
	}
	// Use the first namespace if multiple are provided.
	ns, _, _ := strings.Cut(watchedNs, ",")

	tc := framework.NewTestContext(t)

	sandboxObj := simpleSandbox(ns)
	sandboxObj.Name = fmt.Sprintf("ns-mode-watched-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
				},
			},
		}),
	}
	require.NoError(t, tc.WaitForObject(t.Context(), sandboxObj, p...))
}

// TestNamespacedModeUnwatchedNamespaceNotReconciled verifies that a sandbox
// created in a namespace not in the watch list is never reconciled.
func TestNamespacedModeUnwatchedNamespaceNotReconciled(t *testing.T) {
	if os.Getenv("NAMESPACED_MODE") != "true" {
		t.Skip("NAMESPACED_MODE not enabled; skipping namespaced mode test")
	}

	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("unwatched-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	sandboxObj := simpleSandbox(ns.Name)
	sandboxObj.Name = fmt.Sprintf("ns-mode-unwatched-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	// The controller should not reconcile this sandbox. Wait and verify no
	// Pod is created and the status stays empty.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxObj.Name,
			Namespace: ns.Name,
		},
	}
	err := tc.WaitForObject(ctx, pod)
	if err == nil {
		t.Fatal("expected no Pod to be created in unwatched namespace, but one appeared")
	}
	// Context deadline exceeded means the pod never appeared — this is the expected outcome.
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "expected timeout waiting for pod, got: %v", err)

	// Verify the sandbox status was never set (no reconciliation).
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: sandboxObj.Name, Namespace: ns.Name}, sandboxObj))
	if len(sandboxObj.Status.Conditions) > 0 {
		t.Errorf("sandbox in unwatched namespace has %d status conditions, want 0", len(sandboxObj.Status.Conditions))
	}
}
