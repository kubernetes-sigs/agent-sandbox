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
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func chromeSandbox() *sandboxv1alpha1.Sandbox {
	sandbox := &sandboxv1alpha1.Sandbox{}
	sandbox.Name = "chrome-sandbox"
	sandbox.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "chrome-sandbox",
					// might be nice to remove the IMAGE_TAG env var so this is easier to run from IDE
					Image:           fmt.Sprintf("kind.local/chrome-sandbox:%s", os.Getenv("IMAGE_TAG")),
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}
	return sandbox
}

// TestRunChromeSandbox tests that we can run Chrome inside a Sandbox,
// it also measures how long it takes for Chrome to start serving the CDP protocol.
func TestRunChromeSandbox(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("chrome-sandbox-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	startTime := time.Now()
	sandboxObj := chromeSandbox()
	sandboxObj.Namespace = ns.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))
	require.NoError(t, tc.WaitForObject(t.Context(), sandboxObj, predicates.ReadyConditionIsTrue))

	podID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      "chrome-sandbox",
	}
	podObj := &corev1.Pod{}
	podObj.Name = podID.Name
	podObj.Namespace = podID.Namespace
	// Wait for the pod to be ready
	require.NoError(t, tc.WaitForObject(t.Context(), podObj, predicates.ReadyConditionIsTrue))
	// Wait for chrome to be ready
	require.NoError(t, waitForChromeReady(t.Context(), tc, podID))
	duration := time.Since(startTime)
	t.Logf("Test took %s", duration)
}

func waitForChromeReady(ctx context.Context, tc *framework.TestContext, podID types.NamespacedName) error {
	tc.Helper()
	// Loop until we can query chrome for its version via the debug port
	pollDuration := 100 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled")
		default:
			// We have to port-forward in the loop because port-forward exits when it sees an error
			// https://github.com/kubernetes/kubectl/issues/1249
			portForwardCtx, portForwardCancel := context.WithCancel(ctx)
			if err := tc.PortForward(portForwardCtx, podID, 9222, 9222); err != nil {
				tc.Logf("failed to port forward: %s", err)
				portForwardCancel()
				time.Sleep(pollDuration)
				continue
			}

			u := "http://localhost:9222/json/version"
			info, err := getChromeInfo(ctx, u)
			portForwardCancel()
			if err != nil {
				tc.Logf("failed to get chrome info: %s", err)
				time.Sleep(pollDuration)
				continue
			}
			tc.Logf("Chrome is ready (%s). Response: %s", u, info)
			return nil
		}
	}
}

// getChromeInfo connects to the Chrome Debug Port and retrieves the version information.
// This is used to verify that Chrome is running inside the sandbox.
func getChromeInfo(ctx context.Context, u string) (string, error) {
	httpClient := &http.Client{}
	httpClient.Timeout = 200 * time.Millisecond

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Send the HTTP request
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error sending HTTP request to Chrome Debug Port: %w", err)
	}
	defer response.Body.Close()

	// Check for HTTP 200 OK
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("non-200 response from Chrome Debug Port: %d", response.StatusCode)
	}

	b, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body from Chrome Debug Port: %w", err)
	}

	return string(b), nil
}
