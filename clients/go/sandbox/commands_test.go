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

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRun_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/execute") {
			t.Errorf("expected /execute, got %s", r.URL.Path)
		}
		if r.Header.Get(headerSandboxID) != "test-claim-abc123" {
			t.Errorf("wrong X-Sandbox-ID: %s", r.Header.Get(headerSandboxID))
		}
		if r.Header.Get(headerSandboxNamespace) != "default" {
			t.Errorf("wrong X-Sandbox-Namespace: %s", r.Header.Get(headerSandboxNamespace))
		}
		if r.Header.Get(headerSandboxPort) != "8888" {
			t.Errorf("wrong X-Sandbox-Port: %s", r.Header.Get(headerSandboxPort))
		}

		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 1 {
			t.Errorf("expected exactly 1 key in body, got %d: %v", len(body), body)
		}
		if body["command"] != "echo hello" {
			t.Errorf("expected command 'echo hello', got %q", body["command"])
		}

		_ = json.NewEncoder(w).Encode(ExecutionResult{
			Stdout:   "hello\n",
			Stderr:   "",
			ExitCode: 0,
		})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	result, err := c.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("expected stdout='hello\\n', got %q", result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit_code=0, got %d", result.ExitCode)
	}
	if result.Stderr != "" {
		t.Errorf("expected empty stderr, got %q", result.Stderr)
	}
}

func TestRun_ExitCodeDefault_EmptyJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	result, err := c.Run(context.Background(), "echo test")
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.ExitCode != -1 {
		t.Errorf("expected ExitCode=-1 for empty JSON, got %d", result.ExitCode)
	}
	if result.Stdout != "" {
		t.Errorf("expected empty Stdout for empty JSON, got %q", result.Stdout)
	}
	if result.Stderr != "" {
		t.Errorf("expected empty Stderr for empty JSON, got %q", result.Stderr)
	}
}

func TestContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.Run(ctx, "slow command")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded in error chain, got: %v", err)
	}
}

// --- Run POST body rewind on retry ---

func TestRun_DefaultNoRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	_, err := c.Run(context.Background(), "echo test")
	if err == nil {
		t.Fatal("expected error from Run with 502 response")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("Run should default to 1 attempt (no retry), got %d attempts", got)
	}
}

func TestRun_RetryOnServerError(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()

		n := attempts.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(ExecutionResult{Stdout: "ok", ExitCode: 0})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	// Run defaults to 1 attempt; explicitly opt in to retries.
	result, err := c.Run(context.Background(), "echo test", WithMaxAttempts(6))
	if err != nil {
		t.Fatalf("Run() error after retry: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit_code=0, got %d", result.ExitCode)
	}
	if result.Stdout != "ok" {
		t.Errorf("expected stdout='ok', got %q", result.Stdout)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
	// Verify the command payload arrived complete on both attempts.
	mu.Lock()
	defer mu.Unlock()
	for i, body := range bodies {
		if !strings.Contains(body, `"command":"echo test"`) {
			t.Errorf("attempt %d: expected command in body, got %q", i+1, body)
		}
	}
}

// --- WithTimeout per-call option (Run-specific) ---

func TestWithTimeout_LongerThanRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ExecutionResult{Stdout: "done", ExitCode: 0})
	}))
	defer srv.Close()

	c := newReadyTestSandbox(srv.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 50 * time.Millisecond // would timeout without override
	c.connector.mu.Unlock()
	ctx := context.Background()

	// Without WithTimeout, the 50ms RequestTimeout kills the 200ms request.
	_, err := c.Run(ctx, "slow")
	if err == nil {
		t.Fatal("expected timeout without WithTimeout")
	}

	// With WithTimeout(5s), the call overrides the short RequestTimeout.
	result, err := c.Run(ctx, "slow", WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("Run with WithTimeout(5s): %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
}
