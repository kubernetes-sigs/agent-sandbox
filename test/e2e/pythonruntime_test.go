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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

const sandboxManifest = `
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: sandbox-python-example
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: my-python-sandbox
      annotations:
        test: "yes"
    spec:
      containers:
      - name: python-sandbox
        image: %s/python-runtime-sandbox:%s
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8888
`

const templateManifest = `
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-sandbox-template
spec:
  podTemplate:
    metadata:
      labels:
        app: python-sandbox
        sandbox: codexec-python-sandbox
      annotations:
        test: "yes"
    spec:
      containers:
      - name: python-sandbox
        image: %s/python-runtime-sandbox:%s
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8888
`

const claimManifest = `
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: python-sandbox-claim
spec:
  sandboxTemplateRef:
    name: python-sandbox-template
`

const warmPoolManifest = `
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: python-warmpool
spec:
  replicas: 1
  sandboxTemplateRef:
    name: python-sandbox-template
`

// TestRunPythonRuntimeSandbox tests that we can run the Python runtime inside a standard Pod.
func TestRunPythonRuntimeSandbox(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	log := klog.FromContext(ctx)

	h := framework.NewTestContext(t)

	ns := fmt.Sprintf("python-runtime-sandbox-test-%d", time.Now().UnixNano())
	h.CreateTempNamespace(ctx, ns)

	startTime := time.Now()

	imageTag := os.Getenv("IMAGE_TAG")
	if imageTag == "" {
		imageTag = "latest"
	}
	imagePrefix := os.Getenv("IMAGE_PREFIX")
	if imagePrefix == "" {
		imagePrefix = "kind.local"
	}
	manifest := fmt.Sprintf(sandboxManifest, imagePrefix, imageTag)
	h.Apply(ctx, ns, manifest)

	// Pod and sandboxID have the same name
	sandboxID := types.NamespacedName{
		Namespace: ns,
		Name:      "sandbox-python-example",
	}

	// Wait for the pod to be ready
	if err := h.WaitForSandboxReady(ctx, sandboxID); err != nil {
		log.Error(err, "DEBUG: failed to wait for pod ready using WaitForObject")
		t.Fatalf("failed to wait for pod %s to be ready: %v", sandboxID.String(), err)
	}

	log.Info("Pod is ready", "podID", sandboxID.Name)

	// Run the tests on the pod
	runPodTests(ctx, t, h, sandboxID)

	duration := time.Since(startTime)
	log.Info("Test completed successfully", "duration", duration)
}

// TestRunPythonRuntimeSandboxClaim tests that we can run the Python runtime inside a Sandbox without a WarmPool.
func TestRunPythonRuntimeSandboxClaim(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	log := klog.FromContext(ctx)

	h := framework.NewTestContext(t)

	ns := fmt.Sprintf("python-sandbox-claim-test-%d", time.Now().UnixNano())
	h.CreateTempNamespace(ctx, ns)

	startTime := time.Now()

	imageTag := os.Getenv("IMAGE_TAG")
	if imageTag == "" {
		imageTag = "latest"
	}
	imagePrefix := os.Getenv("IMAGE_PREFIX")
	if imagePrefix == "" {
		imagePrefix = "kind.local"
	}
	manifest := fmt.Sprintf(templateManifest, imagePrefix, imageTag)
	h.Apply(ctx, ns, manifest)

	h.Apply(ctx, ns, claimManifest)

	sandboxID := types.NamespacedName{
		Namespace: ns,
		Name:      "python-sandbox-claim",
	}
	if err := h.WaitForSandboxReady(ctx, sandboxID); err != nil {
		log.Error(err, "DEBUG: failed to wait for pod ready using WaitForObject")
		t.Fatalf("failed to wait for pod %s to be ready: %v", sandboxID.String(), err)
	}

	log.Info("Sandbox is ready", "sandboxName", sandboxID.Name)

	// Run the tests on the pod
	runPodTests(ctx, t, h, sandboxID)

	duration := time.Since(startTime)
	log.Info("Test completed successfully", "duration", duration)
}

// TestRunPythonRuntimeSandboxWarmpool tests that we can run the Python runtime inside a Sandbox.
func TestRunPythonRuntimeSandboxWarmpool(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	log := klog.FromContext(ctx)

	h := framework.NewTestContext(t)

	ns := fmt.Sprintf("python-sandbox-warmpool-test-%d", time.Now().UnixNano())
	h.CreateTempNamespace(ctx, ns)

	startTime := time.Now()

	imageTag := os.Getenv("IMAGE_TAG")
	if imageTag == "" {
		imageTag = "latest"
	}
	imagePrefix := os.Getenv("IMAGE_PREFIX")
	if imagePrefix == "" {
		imagePrefix = "kind.local"
	}
	manifest := fmt.Sprintf(templateManifest, imagePrefix, imageTag)
	h.Apply(ctx, ns, manifest)

	h.Apply(ctx, ns, warmPoolManifest)
	sandboxWarmpoolID := types.NamespacedName{
		Namespace: ns,
		Name:      "python-warmpool",
	}
	// Wait for the warmpool to be ready
	if err := h.WaitForWarmPoolReady(ctx, sandboxWarmpoolID, 1); err != nil {
		log.Error(err, "DEBUG: failed to wait for pod ready using WaitForObject")
		t.Fatalf("failed to wait for pod %s to be ready: %v", sandboxWarmpoolID.String(), err)
	}

	h.Apply(ctx, ns, claimManifest)
	sandboxID := types.NamespacedName{
		Namespace: ns,
		Name:      "python-sandbox-claim",
	}

	if err := h.WaitForSandboxReady(ctx, sandboxID); err != nil {
		log.Error(err, "DEBUG: failed to wait for pod ready using WaitForObject")
		t.Fatalf("failed to wait for pod %s to be ready: %v", sandboxID.String(), err)
	}

	// Get the SandboxClaim to extract the sandbox name
	sandbox := h.GetSandbox(ctx, sandboxID)
	if sandbox == nil {
		log.Error(nil, "failed to get sandbox", sandboxID.String())
		t.Fatalf("Failed to get Sandbox %s after it was bound", sandboxID.String())
	}

	sandboxName, found, err := unstructured.NestedString(sandbox.Object, "metadata", "annotations", "agents.x-k8s.io/pod-name")
	if err != nil || !found || sandboxName == "" {
		t.Fatalf("Failed to extract annotations sandboxName from bound Sandbox %+v: found=%v, err=%v, value=%s",
			sandbox.Object, found, err, sandboxName)
	}
	log.Info("DEBUG: Extracted SandboxName from Sandbox", "sandboxName", sandboxName)

	podID := types.NamespacedName{
		Namespace: ns,
		Name:      sandboxName,
	}

	// Run the tests on the pod
	runPodTests(ctx, t, h, podID)

	duration := time.Since(startTime)
	log.Info("Test completed successfully", "duration", duration)
}

// runPodTests runs the health check, root endpoint, and execute endpoint tests on the given pod.
func runPodTests(ctx context.Context, t *testing.T, h *framework.TestContext, podID types.NamespacedName) {
	log := klog.FromContext(ctx)

	// Get the template to check the runtime
	templateID := types.NamespacedName{Namespace: podID.Namespace, Name: "python-sandbox-template"}
	template := h.GetSandboxTemplate(ctx, templateID)
	runtimeClassName, found, err := unstructured.NestedString(template.Object, "spec", "podTemplate", "spec", "runtimeClassName")
	if err != nil {
		t.Fatalf("Failed to get runtimeClassName from template: %v", err)
	}
	if found && runtimeClassName == "gvisor" {
		log.Info("Skipping PortForward tests for gvisor runtime")
		return
	}

	// Loop until we can query the python server for its health
	for {
		if ctx.Err() != nil {
			t.Fatalf("context cancelled")
		}

		portForwardCtx, portForwardCancel := context.WithCancel(ctx)
		h.PortForward(portForwardCtx, podID, 8888, 8888)

		u := "http://localhost:8888/"
		err := checkHealth(ctx, u)
		portForwardCancel()

		if err != nil {
			log.Error(err, "failed to get health check")
			// time.Sleep(100 * time.Millisecond)
			continue
		}

		log.Info("Python server is ready", "url", u)

		break
	}

	// Test execute endpoint
	{
		portForwardCtx, portForwardCancel := context.WithCancel(ctx)
		h.PortForward(portForwardCtx, podID, 8888, 8888)
		u := "http://localhost:8888/execute"
		err := checkExecute(ctx, u)
		portForwardCancel()
		if err != nil {
			t.Fatalf("failed to verify execute endpoint: %v", err)
		}
		log.Info("Execute endpoint check successful", "url", u)
	}
}

// checkHealth connects to the Python server health check endpoint.
func checkHealth(ctx context.Context, u string) error {
	httpClient := &http.Client{}
	httpClient.Timeout = 200 * time.Millisecond

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Send the HTTP request
	response, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending HTTP request to health check: %w", err)
	}
	defer response.Body.Close()

	// Check for HTTP 200 OK
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("non-200 response from health check: %d", response.StatusCode)
	}

	_, err = io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("error reading response body from health check: %w", err)
	}
	return nil
}

// checkExecute connects to the Python server execute endpoint.
func checkExecute(ctx context.Context, u string) error {
	httpClient := &http.Client{}
	httpClient.Timeout = 5 * time.Second // Increased timeout for execute

	payload := `{"command": "echo 'hello world'"}`
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send the HTTP request
	response, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending HTTP request to execute endpoint: %w", err)
	}
	defer response.Body.Close()

	// Check for HTTP 200 OK
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("non-200 response from execute endpoint: %d", response.StatusCode)
	}

	b, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("error reading response body from execute endpoint: %w", err)
	}

	// Basic check for stdout - more robust JSON parsing could be added if needed
	bodyStr := string(b)
	if !strings.Contains(bodyStr, `"stdout":"hello world\n"`) {
		return fmt.Errorf("unexpected response from execute endpoint: %s", bodyStr)
	}
	return nil
}
