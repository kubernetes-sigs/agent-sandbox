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
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

// TestRunChromeSandbox tests that we can run Chrome inside a Sandbox,
// it also measures how long it takes for Chrome to start serving the CDP protocol.
func TestRunChromeSandbox(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	log := klog.FromContext(ctx)

	h := framework.NewTestContext(t)

	ns := fmt.Sprintf("chrome-sandbox-test-%d", time.Now().UnixNano())
	h.CreateTempNamespace(ctx, ns)

	startTime := time.Now()

	manifest := `
kind: Sandbox
apiVersion: agents.x-k8s.io/v1alpha1
metadata:
  name: chrome-sandbox
spec:
  podTemplate:
    spec:
      containers:
      - name: chrome-sandbox
        image: kind.local/chrome-sandbox:latest
        imagePullPolicy: IfNotPresent
`

	manifest = strings.ReplaceAll(manifest, ":latest", ":"+os.Getenv("IMAGE_TAG"))

	h.Apply(ctx, ns, manifest)

	sandboxID := types.NamespacedName{
		Namespace: ns,
		Name:      "chrome-sandbox",
	}

	h.WaitForSandboxReady(ctx, sandboxID)

	podID := types.NamespacedName{
		Namespace: ns,
		Name:      "chrome-sandbox",
	}

	// Wait for the pod to be ready
	{
		waitForPodReady := exec.CommandContext(ctx, "kubectl", "-n", ns, "wait", "pod/"+podID.Name, "--for=condition=Ready", "--timeout=60s")
		log.Info("waiting for pod to be ready", "command", waitForPodReady.String())
		waitForPodReady.Stdout = os.Stdout
		waitForPodReady.Stderr = os.Stderr
		if err := waitForPodReady.Run(); err != nil {
			t.Fatalf("failed to wait-for-pod-ready: %v", err)
		}
	}

	// Loop until we can query chrome for its version via the debug port
	for {
		if ctx.Err() != nil {
			t.Fatalf("context cancelled")
		}

		// We have to port-forward in the loop because port-forward exits when it sees an error
		// https://github.com/kubernetes/kubectl/issues/1249
		portForwardCtx, portForwardCancel := context.WithCancel(ctx)
		h.PortForward(portForwardCtx, podID, 9222, 9222)

		u := "http://localhost:9222/json/version"
		info, err := getChromeInfo(ctx, u)
		portForwardCancel()
		if err != nil {
			log.Error(err, "failed to get Chrome info")
			time.Sleep(100 * time.Millisecond)
			continue
		}
		log.Info("Chrome is ready", "url", u, "response", info)
		break
	}

	duration := time.Since(startTime)
	log.Info("Test completed successfully", "duration", duration)
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
