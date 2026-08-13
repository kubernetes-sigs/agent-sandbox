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

// Verification script for the envd-sandbox example.
// Drives the envd REST API endpoints over plain HTTP.
//
// Run:
//
//	SANDBOX_BASE_URL=http://127.0.0.1:49983 go run test_client.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

type testResult struct {
	name    string
	passed  bool
	detail  string
	elapsed time.Duration
}

type testFunc struct {
	name string
	fn   func() (string, error)
}

func runTest(tf testFunc) testResult {
	start := time.Now()
	detail, err := tf.fn()
	elapsed := time.Since(start)
	if err != nil {
		return testResult{tf.name, false, err.Error(), elapsed}
	}
	return testResult{tf.name, true, fmt.Sprintf("%s (%.2fs)", detail, elapsed.Seconds()), elapsed}
}

func main() {
	baseURL := os.Getenv("SANDBOX_BASE_URL")
	if baseURL == "" {
		fmt.Fprintln(os.Stderr, "SANDBOX_BASE_URL is not set. Point it at a running envd sandbox pod,")
		fmt.Fprintln(os.Stderr, "e.g. via `kubectl port-forward pod/<name> 49983:49983` and export")
		fmt.Fprintln(os.Stderr, "SANDBOX_BASE_URL=http://127.0.0.1:49983")
		os.Exit(1)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	client := &http.Client{Timeout: 10 * time.Second}

	tests := []testFunc{
		{"health", func() (string, error) {
			resp, err := client.Get(baseURL + "/health")
			if err != nil {
				return "", fmt.Errorf("GET /health: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 204 {
				return "", fmt.Errorf("expected 204, got %d", resp.StatusCode)
			}
			return "204 No Content", nil
		}},
		{"init", func() (string, error) {
			body, err := json.Marshal(map[string]any{
				"envVars":     map[string]string{"HELLO": "envd"},
				"defaultUser": "user",
			})
			if err != nil {
				return "", fmt.Errorf("POST /init marshal: %w", err)
			}
			resp, err := client.Post(baseURL+"/init", "application/json", bytes.NewReader(body))
			if err != nil {
				return "", fmt.Errorf("POST /init: %w", err)
			}
			defer resp.Body.Close()
			// envd returns 204 No Content on success in --isnotfc mode.
			if resp.StatusCode != 204 {
				b, _ := io.ReadAll(resp.Body)
				return "", fmt.Errorf("init: expected 204, got %d: %s", resp.StatusCode, string(b))
			}
			return "init ok", nil
		}},
		{"files", func() (string, error) {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			if err := w.WriteField("path", "hello.txt"); err != nil {
				return "", fmt.Errorf("POST /files write path field: %w", err)
			}
			fw, err := w.CreateFormFile("file", "hello.txt")
			if err != nil {
				return "", fmt.Errorf("POST /files create form file: %w", err)
			}
			if _, err := fw.Write([]byte("hi from envd-sandbox")); err != nil {
				return "", fmt.Errorf("POST /files write file content: %w", err)
			}
			if err := w.Close(); err != nil {
				return "", fmt.Errorf("POST /files close multipart: %w", err)
			}

			resp, err := client.Post(baseURL+"/files", w.FormDataContentType(), &buf)
			if err != nil {
				return "", fmt.Errorf("POST /files upload: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				return "", fmt.Errorf("upload: expected 200, got %d: %s", resp.StatusCode, string(b))
			}

			resp2, err := client.Get(baseURL + "/files?path=hello.txt")
			if err != nil {
				return "", fmt.Errorf("GET /files download: %w", err)
			}
			defer resp2.Body.Close()
			if resp2.StatusCode != 200 {
				return "", fmt.Errorf("download: expected 200, got %d", resp2.StatusCode)
			}
			content, err := io.ReadAll(resp2.Body)
			if err != nil {
				return "", fmt.Errorf("GET /files read body: %w", err)
			}
			if string(content) != "hi from envd-sandbox" {
				return "", fmt.Errorf("content mismatch: %q", string(content))
			}
			return "round-trip ok", nil
		}},
		{"metrics", func() (string, error) {
			resp, err := client.Get(baseURL + "/metrics")
			if err != nil {
				return "", fmt.Errorf("GET /metrics: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				return "", fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(b))
			}
			var metrics map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
				return "", fmt.Errorf("GET /metrics body decode: %w", err)
			}
			if _, ok := metrics["ts"]; !ok {
				if _, ok := metrics["cpu_count"]; !ok {
					return "", fmt.Errorf("unexpected metrics: %v", metrics)
				}
			}
			keys := make([]string, 0, len(metrics))
			for k := range metrics {
				keys = append(keys, k)
			}
			return "keys=" + strings.Join(keys, ","), nil
		}},
	}

	fmt.Print("\n=== envd-sandbox verification: Go HTTP ===\n")

	var results []testResult
	for i, tf := range tests {
		fmt.Printf("[%d/%d] running %s...\n", i+1, len(tests), tf.name)
		r := runTest(tf)
		results = append(results, r)
		status := "PASS"
		if !r.passed {
			status = "FAIL"
		}
		fmt.Printf("         %s: %s\n", status, r.detail)
	}

	fmt.Println("\n=== Summary ===")
	failed := 0
	for _, r := range results {
		mark := "PASS"
		if !r.passed {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("  [%s] %s: %s\n", mark, r.name, r.detail)
	}

	if failed > 0 {
		fmt.Printf("\n%d test(s) failed.\n", failed)
		os.Exit(1)
	}
	fmt.Printf("\nAll %d test(s) passed.\n", len(results))
}
