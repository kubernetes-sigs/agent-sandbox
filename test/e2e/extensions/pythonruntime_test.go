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

package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
	"sigs.k8s.io/yaml"
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
        image: %spython-runtime-sandbox:%s
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
        image: %spython-runtime-sandbox:%s
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
  replicas: 2
  sandboxTemplateRef:
    name: python-sandbox-template
`

func sandboxFromManifest(manifest string) (*sandboxv1alpha1.Sandbox, error) {
	sandbox := &sandboxv1alpha1.Sandbox{}
	if err := yaml.Unmarshal([]byte(manifest), sandbox); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Sandbox: %w", err)
	}
	return sandbox, nil
}

func sandboxTemplateFromManifest(manifest string) (*extensionsv1alpha1.SandboxTemplate, error) {
	template := &extensionsv1alpha1.SandboxTemplate{}
	if err := yaml.Unmarshal([]byte(manifest), template); err != nil {
		return nil, fmt.Errorf("failed to unmarshal SandboxTemplate: %w", err)
	}
	return template, nil
}

func sandboxClaimFromManifest(manifest string) (*extensionsv1alpha1.SandboxClaim, error) {
	claim := &extensionsv1alpha1.SandboxClaim{}
	if err := yaml.Unmarshal([]byte(manifest), claim); err != nil {
		return nil, fmt.Errorf("failed to unmarshal SandboxClaim: %w", err)
	}
	return claim, nil
}

func sandboxWarmpoolFromManifest(manifest string) (*extensionsv1alpha1.SandboxWarmPool, error) {
	warmpool := &extensionsv1alpha1.SandboxWarmPool{}
	if err := yaml.Unmarshal([]byte(manifest), warmpool); err != nil {
		return nil, fmt.Errorf("failed to unmarshal SandboxWarmPool: %w", err)
	}
	return warmpool, nil
}

func getImageTag() string {
	imageTag := os.Getenv("IMAGE_TAG")
	if imageTag == "" {
		imageTag = "latest"
	}
	return imageTag
}

func getImagePrefix() string {
	imagePrefix := os.Getenv("IMAGE_PREFIX")
	if imagePrefix == "" {
		imagePrefix = "kind.local/"
	}
	return imagePrefix
}

// TestRunPythonRuntimeSandbox tests that we can run the Python runtime inside a standard Pod.
func TestRunPythonRuntimeSandbox(testingT *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	testContext := framework.NewTestContext(testingT)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("python-runtime-sandbox-test-%d", time.Now().UnixNano())
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), ns))

	startTime := time.Now()

	// Apply python runtime sandbox manifest
	manifest := fmt.Sprintf(sandboxManifest, getImagePrefix(), getImageTag())
	sandboxObj, err := sandboxFromManifest(manifest)
	require.NoError(testingT, err)
	sandboxObj.Namespace = ns.Name
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), sandboxObj))
	require.NoError(testingT, testContext.WaitForObject(testingT.Context(), sandboxObj, predicates.ReadyConditionIsTrue))

	// Pod and sandboxID have the same name
	sandboxID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      "sandbox-python-example",
	}

	podObj := &corev1.Pod{}
	podObj.Name = sandboxID.Name
	podObj.Namespace = sandboxID.Namespace

	// Wait for the pod to be ready
	require.NoError(testingT, testContext.WaitForObject(testingT.Context(), podObj, predicates.ReadyConditionIsTrue))

	testingT.Logf("Pod is ready: podID - %s", sandboxID.Name)
	// Run the tests on the pod
	require.NoError(testingT, runPodTests(ctx, testingT, testContext, sandboxID))

	duration := time.Since(startTime)
	testingT.Logf("Test completed successfully: duration - %s", duration)
}

// TestRunPythonRuntimeSandboxClaim tests that we can run the Python runtime inside a Sandbox without a WarmPool.
func TestRunPythonRuntimeSandboxClaim(testingT *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	testContext := framework.NewTestContext(testingT)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("python-sandbox-claim-test-%d", time.Now().UnixNano())
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), ns))

	startTime := time.Now()

	// Apply python runtime sandbox template and claim manifests
	manifest := fmt.Sprintf(templateManifest, getImagePrefix(), getImageTag())
	sandboxTemplate, err := sandboxTemplateFromManifest(manifest)
	require.NoError(testingT, err)
	sandboxTemplate.Namespace = ns.Name
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), sandboxTemplate))

	sandboxClaim, err := sandboxClaimFromManifest(claimManifest)
	require.NoError(testingT, err)
	sandboxClaim.Namespace = ns.Name
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), sandboxClaim))

	sandboxID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      "python-sandbox-claim",
	}

	podObj := &corev1.Pod{}
	podObj.Name = sandboxID.Name
	podObj.Namespace = sandboxID.Namespace

	// Wait for the pod to be ready
	require.NoError(testingT, testContext.WaitForObject(testingT.Context(), podObj, predicates.ReadyConditionIsTrue))

	testingT.Logf("Sandbox is ready: sandboxName - %s", sandboxID.Name)

	// Run the tests on the pod
	require.NoError(testingT, runPodTests(ctx, testingT, testContext, sandboxID))

	duration := time.Since(startTime)
	testingT.Logf("Test completed successfully: duration %s", duration)
}

// TestRunPythonRuntimeSandboxWarmpool tests that we can run the Python runtime inside a Sandbox.
func TestRunPythonRuntimeSandboxWarmpool(testingT *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	testContext := framework.NewTestContext(testingT)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("python-sandbox-warmpool-test-%d", time.Now().UnixNano())
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), ns))

	startTime := time.Now()

	// Apply python runtime sandbox template, warmpool manifests
	manifest := fmt.Sprintf(templateManifest, getImagePrefix(), getImageTag())
	sandboxTemplate, err := sandboxTemplateFromManifest(manifest)
	require.NoError(testingT, err)
	sandboxTemplate.Namespace = ns.Name
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), sandboxTemplate))

	sandboxWarmpool, err := sandboxWarmpoolFromManifest(warmPoolManifest)
	require.NoError(testingT, err)
	sandboxWarmpool.Namespace = ns.Name
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), sandboxWarmpool))

	sandboxWarmpoolID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      "python-warmpool",
	}

	// Wait for the warmpool to be ready
	require.NoError(testingT, testContext.WaitForWarmPoolReady(testingT.Context(), sandboxWarmpoolID))

	// Apply python runtime sandbox claim manifest
	sandboxClaim, err := sandboxClaimFromManifest(claimManifest)
	require.NoError(testingT, err)
	sandboxClaim.Namespace = ns.Name
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), sandboxClaim))

	sandboxID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      "python-sandbox-claim",
	}

	require.NoError(testingT, testContext.WaitForSandboxReady(testingT.Context(), sandboxID))

	// Get the SandboxClaim to extract the sandbox name
	sandbox, err := testContext.GetSandbox(ctx, sandboxID)
	require.NoError(testingT, err)

	sandboxName, _, err := unstructured.NestedString(sandbox.Object, "metadata", "annotations", "agents.x-k8s.io/pod-name")
	require.NoError(testingT, err)
	testingT.Logf("DEBUG: Extracted SandboxName from Sandbox: sandboxName - %s", sandboxName)

	podID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      sandboxName,
	}

	// Run the tests on the pod
	require.NoError(testingT, runPodTests(ctx, testingT, testContext, podID))

	duration := time.Since(startTime)
	testingT.Logf("Test completed successfully: duration-%s", duration)
}

// runPodTests runs the health check, root endpoint, and execute endpoint tests on the given pod.
func runPodTests(ctx context.Context, testingT *testing.T, testContext *framework.TestContext, podID types.NamespacedName) error {
	testContext.Helper()
	pollDuration := 200 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			testingT.Logf("Context cancelled, exiting runPodTests")
			return fmt.Errorf("context cancelled")
		default:
			testingT.Logf("Attempting port forward and checks...")

			// Port forward for health check
			portForwardCtx, portForwardCancel := context.WithCancel(ctx)
			if err := testContext.PortForward(portForwardCtx, podID, 8888, 8888); err != nil {
				testingT.Logf("Failed to port forward for health check: %s", err)
				portForwardCancel()
				time.Sleep(pollDuration)
				continue
			}
			testingT.Logf("Port forward for health check established.")

			// Perform health check
			healthURL := "http://localhost:8888/"
			err := checkHealth(ctx, healthURL)

			if err != nil {
				testingT.Logf("Failed to get health check: %s", err)
				portForwardCancel()
				time.Sleep(pollDuration)
				continue
			}
			testingT.Logf("Health check successful: url - %s", healthURL)

			// Perform execute check
			executeURL := "http://localhost:8888/execute"
			err = checkExecute(ctx, executeURL)
			portForwardCancel()

			if err != nil {
				testingT.Logf("failed to verify execute endpoint: %v", err)
				portForwardCancel()
				time.Sleep(pollDuration)
				continue
			}
			testingT.Logf("Execute endpoint check successful: url - %s", executeURL)

			// Both checks passed
			testingT.Logf("Both health and execute checks passed.")
			return nil
		}
	}
}

// checkHealth connects to the Python server health check endpoint.
func checkHealth(ctx context.Context, url string) error {
	httpClient := &http.Client{}
	httpClient.Timeout = 200 * time.Millisecond

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
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
func checkExecute(ctx context.Context, url string) error {
	httpClient := &http.Client{}
	httpClient.Timeout = 5 * time.Second // Increased timeout for execute

	payload := `{"command": "echo 'hello world'"}`
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(payload))
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

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("error reading response body from execute endpoint: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to parse JSON response: %w", err)
	}

	stdout, ok := result["stdout"].(string)
	if !ok {
		return fmt.Errorf("stdout field not found or not a string in response: %s", string(body))
	}

	if stdout != "hello world\n" {
		return fmt.Errorf("unexpected stdout in response: got %q, want %q", stdout, "hello world\n")
	}
	return nil
}
