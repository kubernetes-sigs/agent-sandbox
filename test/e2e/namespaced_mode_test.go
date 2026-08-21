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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	namespacedModeEnv                     = "NAMESPACED_MODE"
	namespacedModeWatchedNamespacesEnv    = "NAMESPACED_MODE_WATCHED_NAMESPACES"
	namespacedModeDefaultWatchedNamespace = "agent-sandbox-watched"
	namespacedModeUnwatchedNamespace      = "agent-sandbox-unwatched"
)

func TestNamespacedModeScope(t *testing.T) {
	if os.Getenv(namespacedModeEnv) != "true" {
		t.Skipf("set %s=true to run against a namespace-scoped controller", namespacedModeEnv)
	}

	tc := framework.NewTestContext(t)
	watchedNamespaces := namespacedModeWatchedNamespaces(t)
	for _, namespace := range append(watchedNamespaces, namespacedModeUnwatchedNamespace) {
		ns := &corev1.Namespace{}
		ns.Name = namespace
		tc.MustExist(ns)
	}

	unwatchedSandbox := simpleSandbox(namespacedModeUnwatchedNamespace)
	unwatchedSandbox.Name = "unwatched-sandbox"
	require.NoError(t, tc.CreateWithCleanup(t.Context(), unwatchedSandbox))

	for i, namespace := range watchedNamespaces {
		watchedSandbox := simpleSandbox(namespace)
		watchedSandbox.Name = "watched-sandbox"
		if i > 0 {
			watchedSandbox.Name += fmt.Sprintf("-%d", i)
		}
		require.NoError(t, tc.CreateWithCleanup(t.Context(), watchedSandbox))

		watchedPod := &corev1.Pod{}
		watchedPod.Name = watchedSandbox.Name
		watchedPod.Namespace = watchedSandbox.Namespace
		watchedService := &corev1.Service{}
		watchedService.Name = watchedSandbox.Name
		watchedService.Namespace = watchedSandbox.Namespace
		require.NoError(t, waitForObjects(t.Context(), tc, watchedPod, watchedService),
			"watched namespace %q was not reconciled", namespace)
	}

	unwatchedPod := &corev1.Pod{}
	unwatchedPod.Name = unwatchedSandbox.Name
	unwatchedPod.Namespace = unwatchedSandbox.Namespace
	unwatchedService := &corev1.Service{}
	unwatchedService.Name = unwatchedSandbox.Name
	unwatchedService.Namespace = unwatchedSandbox.Namespace
	require.ErrorIs(t,
		waitForUnexpectedObjects(t.Context(), tc, 10*time.Second, unwatchedPod, unwatchedService),
		context.DeadlineExceeded,
		"unwatched namespace was reconciled",
	)

	current := &sandboxv1beta1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{
		Name:      unwatchedSandbox.Name,
		Namespace: unwatchedSandbox.Namespace,
	}, current))
	require.Empty(t, current.Status, "unwatched Sandbox status was updated")
}

func namespacedModeWatchedNamespaces(t *testing.T) []string {
	t.Helper()
	value := os.Getenv(namespacedModeWatchedNamespacesEnv)
	if value == "" {
		value = namespacedModeDefaultWatchedNamespace
	}

	seen := map[string]struct{}{}
	var namespaces []string
	for namespace := range strings.SplitSeq(value, ",") {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			continue
		}
		if namespace == namespacedModeUnwatchedNamespace {
			t.Fatalf("%s must not contain control namespace %q", namespacedModeWatchedNamespacesEnv, namespace)
		}
		if _, exists := seen[namespace]; exists {
			continue
		}
		seen[namespace] = struct{}{}
		namespaces = append(namespaces, namespace)
	}
	require.NotEmpty(t, namespaces, "%s must contain at least one namespace", namespacedModeWatchedNamespacesEnv)
	return namespaces
}

func waitForObjects(ctx context.Context, tc *framework.TestContext, objects ...client.Object) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, framework.DefaultTimeout, true, func(ctx context.Context) (bool, error) {
		for _, obj := range objects {
			exists, err := objectExists(ctx, tc, obj)
			if err != nil || !exists {
				return false, err
			}
		}
		return true, nil
	})
}

func waitForUnexpectedObjects(ctx context.Context, tc *framework.TestContext, duration time.Duration, objects ...client.Object) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, duration, true, func(ctx context.Context) (bool, error) {
		for _, obj := range objects {
			exists, err := objectExists(ctx, tc, obj)
			if err != nil {
				return false, err
			}
			if exists {
				return false, fmt.Errorf("unexpected %T %s", obj, client.ObjectKeyFromObject(obj))
			}
		}
		return false, nil
	})
}

func objectExists(ctx context.Context, tc *framework.TestContext, obj client.Object) (bool, error) {
	err := tc.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking whether %T %s exists: %w", obj, client.ObjectKeyFromObject(obj), err)
}
