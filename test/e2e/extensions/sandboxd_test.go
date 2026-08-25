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
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// sandboxdManifest runs sandboxd as the sole container. It binds 0.0.0.0
// (the daemon default), so the kubelet httpGet readiness probe reaches
// /v1/health and the test reaches both ports via port-forward. The API
// behaves the same in any deployment topology; note that exec'd commands
// always run in the container hosting sandboxd, regardless of what other
// containers share the pod.
const sandboxdManifest = `
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: sandbox-sandboxd-example
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: my-sandboxd-sandbox
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        fsGroup: 1000
      containers:
      - name: sandboxd
        image: %ssandboxd:%s
        imagePullPolicy: IfNotPresent
        securityContext:
          runAsUser: 1000
          runAsGroup: 1000
          runAsNonRoot: true
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
        ports:
        - containerPort: 8080
        - containerPort: 9090
        readinessProbe:
          httpGet:
            path: /v1/health
            port: 8080
`

// TestRunSandboxdSandbox runs sandboxd in a Pod and exercises both surfaces:
// the REST filesystem (health + PUT/GET round-trip) and the gRPC
// ProcessService (Execute), reaching the loopback-bound daemon via
// port-forward directly to the pod.
func TestRunSandboxdSandbox(testingT *testing.T) {
	ctx := testingT.Context()

	testContext := framework.NewTestContext(testingT)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandboxd-sandbox-test-%d", time.Now().UnixNano())
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), ns))

	startTime := time.Now()

	manifest := fmt.Sprintf(sandboxdManifest, getImagePrefix(), getImageTag())
	sandboxObj, err := sandboxFromManifest(manifest)
	require.NoError(testingT, err)
	sandboxObj.Namespace = ns.Name
	require.NoError(testingT, testContext.CreateWithCleanup(testingT.Context(), sandboxObj))
	testContext.MustWaitForObject(sandboxObj, predicates.ReadyConditionIsTrue)

	sandboxID := types.NamespacedName{Namespace: ns.Name, Name: "sandbox-sandboxd-example"}

	podObj := &corev1.Pod{}
	podObj.Name = sandboxID.Name
	podObj.Namespace = sandboxID.Namespace
	testContext.MustWaitForObject(podObj, predicates.ReadyConditionIsTrue)

	testingT.Logf("Pod is ready: %s", sandboxID.Name)
	require.NoError(testingT, runSandboxdPodTests(ctx, testingT, testContext, sandboxID))

	testingT.Logf("Test completed successfully: duration - %s", time.Since(startTime))
}

func runSandboxdPodTests(ctx context.Context, testingT *testing.T, testContext *framework.TestContext, podID types.NamespacedName) error {
	testContext.Helper()
	pollDuration := 200 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled")
		default:
			pfCtx, pfCancel := context.WithCancel(ctx)
			if err := testContext.PortForward(pfCtx, podID, 8080, 8080); err != nil {
				testingT.Logf("REST port forward failed: %s", err)
				pfCancel()
				time.Sleep(pollDuration)
				continue
			}
			if err := testContext.PortForward(pfCtx, podID, 9090, 9090); err != nil {
				testingT.Logf("gRPC port forward failed: %s", err)
				pfCancel()
				time.Sleep(pollDuration)
				continue
			}

			if err := checkSandboxdHealth(ctx); err != nil {
				testingT.Logf("health check failed: %s", err)
				pfCancel()
				time.Sleep(pollDuration)
				continue
			}
			if err := checkSandboxdFileRoundTrip(ctx); err != nil {
				testingT.Logf("filesystem round-trip failed: %s", err)
				pfCancel()
				time.Sleep(pollDuration)
				continue
			}
			if err := checkSandboxdExecute(ctx); err != nil {
				testingT.Logf("gRPC execute failed: %s", err)
				pfCancel()
				time.Sleep(pollDuration)
				continue
			}
			pfCancel()
			testingT.Logf("sandboxd REST + gRPC checks passed.")
			return nil
		}
	}
}

func checkSandboxdHealth(ctx context.Context) error {
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8080/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("non-200 from /v1/health: %d", resp.StatusCode)
	}
	return nil
}

func checkSandboxdFileRoundTrip(ctx context.Context) error {
	client := &http.Client{Timeout: 5 * time.Second}
	const want = "hello from e2e"

	// PUT
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://localhost:8080/v1/files/notes/hello.txt", strings.NewReader(want))
	if err != nil {
		return err
	}
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putResp, err := client.Do(putReq)
	if err != nil {
		return err
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("PUT returned %d, want 204", putResp.StatusCode)
	}

	// GET
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://localhost:8080/v1/files/notes/hello.txt", nil)
	if err != nil {
		return err
	}
	getResp, err := client.Do(getReq)
	if err != nil {
		return err
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET returned %d, want 200", getResp.StatusCode)
	}
	body, err := io.ReadAll(getResp.Body)
	if err != nil {
		return err
	}
	if string(body) != want {
		return fmt.Errorf("GET returned %q, want %q", body, want)
	}
	return nil
}

func checkSandboxdExecute(ctx context.Context) error {
	conn, err := grpc.NewClient("localhost:9090", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	client := processv1.NewProcessServiceClient(conn)
	execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.Execute(execCtx, &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "echo hello world"}},
	})
	if err != nil {
		return fmt.Errorf("Execute RPC failed: %w", err)
	}
	if resp.GetExitCode() != 0 {
		return fmt.Errorf("unexpected exit code %d (stderr: %s)", resp.GetExitCode(), resp.GetStderr())
	}
	if got := string(resp.GetStdout()); got != "hello world\n" {
		return fmt.Errorf("unexpected stdout %q", got)
	}
	return nil
}
