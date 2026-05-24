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

//go:build integration

package sandbox

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

var (
	templateName     = flag.String("template-name", "python-sandbox-template", "SandboxTemplate name")
	gatewayName      = flag.String("gateway-name", "", "Gateway name for production mode")
	gatewayNamespace = flag.String("gateway-namespace", "", "Gateway namespace (defaults to --namespace)")
	apiURL           = flag.String("api-url", "", "Direct API URL")
	namespace        = flag.String("namespace", "default", "Kubernetes namespace")
	serverPort       = flag.Int("server-port", 8888, "Sandbox server port")
)

func TestIntegration_FullLifecycle(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" && *gatewayName == "" && *apiURL == "" {
		t.Skip("set INTEGRATION_TEST=1 or provide --gateway-name/--api-url to run integration tests")
	}

	gwNS := *gatewayNamespace
	if gwNS == "" {
		gwNS = *namespace
	}

	opts := Options{
		TemplateName:        *templateName,
		Namespace:           *namespace,
		GatewayName:         *gatewayName,
		GatewayNamespace:    gwNS,
		APIURL:              *apiURL,
		ServerPort:          *serverPort,
		SandboxReadyTimeout: 180 * time.Second,
	}

	client, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer client.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Log("Opening sandbox...")
	if err := client.Open(ctx); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Logf("Sandbox ready: claim=%s sandbox=%s pod=%s", client.ClaimName(), client.SandboxName(), client.PodName())

	t.Run("Run", func(t *testing.T) {
		result, err := client.Run(ctx, "echo 'Hello from the sandbox!'")
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if result.ExitCode != 0 {
			t.Errorf("expected exit_code=0, got %d (stderr=%s)", result.ExitCode, result.Stderr)
		}
		expected := "Hello from the sandbox!\n"
		if result.Stdout != expected {
			t.Errorf("expected stdout=%q, got %q", expected, result.Stdout)
		}
	})

	t.Run("WriteAndRead", func(t *testing.T) {
		content := []byte("This is a test file.")
		if err := client.Write(ctx, "test.txt", content); err != nil {
			t.Fatalf("Write() error: %v", err)
		}

		data, err := client.Read(ctx, "test.txt")
		if err != nil {
			t.Fatalf("Read() error: %v", err)
		}
		if string(data) != string(content) {
			t.Errorf("expected %q, got %q", string(content), string(data))
		}
	})

	t.Run("Exists", func(t *testing.T) {
		exists, err := client.Exists(ctx, "test.txt")
		if err != nil {
			t.Fatalf("Exists() error: %v", err)
		}
		if !exists {
			t.Error("expected test.txt to exist")
		}

		exists, err = client.Exists(ctx, "non_existent_file.txt")
		if err != nil {
			t.Fatalf("Exists() error: %v", err)
		}
		if exists {
			t.Error("expected non_existent_file.txt to not exist")
		}
	})

	t.Run("List", func(t *testing.T) {
		entries, err := client.List(ctx, ".")
		if err != nil {
			t.Fatalf("List() error: %v", err)
		}

		found := false
		for _, e := range entries {
			if e.Name == "test.txt" {
				found = true
				if e.Type != "file" {
					t.Errorf("expected type=file for test.txt, got %s", e.Type)
				}
				if e.Size != int64(len("This is a test file.")) {
					t.Errorf("expected size=%d, got %d", len("This is a test file."), e.Size)
				}
			}
		}
		if !found {
			t.Error("expected test.txt in directory listing")
		}
	})

	t.Log("Closing sandbox...")
}

func newIntegrationClient(t *testing.T) *Sandbox {
	t.Helper()
	if os.Getenv("INTEGRATION_TEST") == "" && *gatewayName == "" && *apiURL == "" {
		t.Skip("set INTEGRATION_TEST=1 or provide --gateway-name/--api-url to run integration tests")
	}
	gwNS := *gatewayNamespace
	if gwNS == "" {
		gwNS = *namespace
	}
	client, err := New(context.Background(), Options{
		TemplateName:        *templateName,
		Namespace:           *namespace,
		GatewayName:         *gatewayName,
		GatewayNamespace:    gwNS,
		APIURL:              *apiURL,
		ServerPort:          *serverPort,
		SandboxReadyTimeout: 180 * time.Second,
		Quiet:               true,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return client
}

func TestIntegration_ClientCleanupAndClose(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" && *gatewayName == "" && *apiURL == "" {
		t.Skip("set INTEGRATION_TEST=1 or provide --gateway-name/--api-url to run integration tests")
	}

	gwNS := *gatewayNamespace
	if gwNS == "" {
		gwNS = *namespace
	}

	// Create client with Cleanup = true
	opts := Options{
		TemplateName:        *templateName,
		Namespace:           *namespace,
		GatewayName:         *gatewayName,
		GatewayNamespace:    gwNS,
		APIURL:              *apiURL,
		ServerPort:          *serverPort,
		SandboxReadyTimeout: 180 * time.Second,
		Cleanup:             true, // Enable auto-cleanup!
	}

	client, err := NewClient(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Log("Creating sandbox...")
	sb, err := client.CreateSandbox(ctx, *templateName, *namespace)
	if err != nil {
		t.Fatalf("CreateSandbox() error: %v", err)
	}
	claimName := sb.ClaimName()
	t.Logf("Sandbox created: %s", claimName)

	// Verify it is active in cluster
	if err := client.k8s.verifyClaimExists(ctx, claimName, *namespace, client.tracer, client.svcName); err != nil {
		t.Fatalf("Expected claim to exist on cluster: %v", err)
	}

	t.Log("Closing client (should trigger parallel deletion of the sandbox)...")
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Client Close() error: %v", err)
	}

	// Verify it was successfully deleted from the cluster
	t.Log("Verifying claim was deleted from cluster...")
	start := time.Now()
	deleted := false
	for time.Since(start) < 30*time.Second {
		err := client.k8s.verifyClaimExists(ctx, claimName, *namespace, client.tracer, client.svcName)
		if err != nil && k8serrors.IsNotFound(err) {
			deleted = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !deleted {
		t.Errorf("expected claim %s to be deleted from cluster after client Close(), but it still exists", claimName)
	}
}
