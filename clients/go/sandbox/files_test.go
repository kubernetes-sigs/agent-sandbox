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
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Shared test helpers
// ---------------------------------------------------------------------------

// newReadyTestSandbox creates a Sandbox that's already "connected" to the given server URL.
func newReadyTestSandbox(serverURL string) *Sandbox {
	opts := Options{
		TemplateName:      "test-template",
		Namespace:         "default",
		APIURL:            serverURL,
		ServerPort:        8888,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Quiet:             true,
	}
	opts.setDefaults()

	k8s := &K8sHelper{Log: opts.Logger}
	opts.K8sHelper = k8s
	sb, err := New(context.Background(), opts)
	if err != nil {
		panic("newReadyTestSandbox: " + err.Error())
	}
	// Simulate being connected
	sb.connector.mu.Lock()
	sb.connector.baseURL = serverURL
	sb.connector.sandboxID = "test-claim-abc123"
	sb.connector.backoffScale = 0.001 // near-instant retries in tests
	sb.connector.mu.Unlock()
	sb.mu.Lock()
	sb.claimName = "test-claim-abc123"
	sb.mu.Unlock()
	return sb
}

// newUnreadyTestSandbox returns a Sandbox that has not been opened.
func newUnreadyTestSandbox() *Sandbox {
	opts := Options{
		TemplateName:      "test-template",
		Namespace:         "default",
		APIURL:            "http://localhost:19999",
		ServerPort:        8888,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Quiet:             true,
	}
	opts.setDefaults()

	k8s := &K8sHelper{Log: opts.Logger}
	opts.K8sHelper = k8s
	sb, err := New(context.Background(), opts)
	if err != nil {
		panic("newUnreadyTestSandbox: " + err.Error())
	}
	return sb
}

// --- Connection-level error retry helper ---

type failFirstTransport struct {
	inner     http.RoundTripper
	mu        sync.Mutex
	count     int
	failCount int
}

func (t *failFirstTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.count++
	n := t.count
	t.mu.Unlock()
	if n <= t.failCount {
		return nil, fmt.Errorf("connection refused (simulated)")
	}
	return t.inner.RoundTrip(req)
}

// --- Seek failure during retry helper ---

type failOnSecondSeek struct {
	io.Reader
	seekCount atomic.Int32
}

func (f *failOnSecondSeek) Seek(_ int64, _ int) (int64, error) {
	if f.seekCount.Add(1) > 1 {
		return 0, fmt.Errorf("seek failed (simulated)")
	}
	return 0, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOperations_NotReady(t *testing.T) {
	c := newUnreadyTestSandbox()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"Run", func() error { _, err := c.Run(context.Background(), "echo"); return err }},
		{"Write", func() error { return c.Write(context.Background(), "f.txt", []byte("d")) }},
		{"Read", func() error { _, err := c.Read(context.Background(), "f.txt"); return err }},
		{"List", func() error { _, err := c.List(context.Background(), "."); return err }},
		{"Exists", func() error { _, err := c.Exists(context.Background(), "f.txt"); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); !errors.Is(err, ErrNotReady) {
				t.Fatalf("expected ErrNotReady, got %v", err)
			}
		})
	}
}

func TestWrite_MultipartUpload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/upload") {
			t.Errorf("expected /upload, got %s", r.URL.Path)
		}

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("failed to parse Content-Type: %v", err)
		}
		if !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatalf("expected multipart content type, got %s", mediaType)
		}

		reader := multipart.NewReader(r.Body, params["boundary"])
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("failed to read multipart part: %v", err)
		}
		if part.FormName() != "file" {
			t.Errorf("expected form field 'file', got %q", part.FormName())
		}
		if part.FileName() != "test.txt" {
			t.Errorf("expected filename 'test.txt', got %q", part.FileName())
		}

		data, _ := io.ReadAll(part)
		if string(data) != "hello world" {
			t.Errorf("expected content 'hello world', got %q", string(data))
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	err := c.Write(context.Background(), "test.txt", []byte("hello world"))
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
}

func TestRead_ReturnsContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/download/") {
			t.Errorf("expected /download/ in path, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("file content here"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	data, err := c.Read(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if string(data) != "file content here" {
		t.Errorf("expected 'file content here', got %q", string(data))
	}
}

func TestOperations_URLEncodesSpecialChars(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		expected string
		callOp   func(*Sandbox, string) error
	}{
		{"Read_spaces", "path with spaces/file.txt", "path%20with%20spaces%2Ffile.txt", func(c *Sandbox, p string) error { _, err := c.Read(context.Background(), p); return err }},
		{"List_spaces", "path with spaces/dir", "path%20with%20spaces%2Fdir", func(c *Sandbox, p string) error { _, err := c.List(context.Background(), p); return err }},
		{"Exists_special", "file@special!.txt", "file%40special%21.txt", func(c *Sandbox, p string) error { _, err := c.Exists(context.Background(), p); return err }},
		{"Read_slashes", "subdir/nested/file.txt", "subdir%2Fnested%2Ffile.txt", func(c *Sandbox, p string) error { _, err := c.Read(context.Background(), p); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var receivedPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Use EscapedPath which always returns properly encoded form,
				// unlike RawPath which may be empty.
				receivedPath = r.URL.EscapedPath()
				switch {
				case strings.Contains(r.URL.Path, "/list/"):
					_ = json.NewEncoder(w).Encode([]FileEntry{})
				case strings.Contains(r.URL.Path, "/exists/"):
					_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
				default:
					_, _ = w.Write([]byte("ok"))
				}
			}))
			defer server.Close()

			c := newReadyTestSandbox(server.URL)
			if err := tc.callOp(c, tc.path); err != nil {
				t.Fatalf("%s() error: %v", tc.name, err)
			}
			if !strings.Contains(receivedPath, tc.expected) {
				t.Errorf("expected URL path containing %q, got %s", tc.expected, receivedPath)
			}
		})
	}
}

func TestList_ParsesEntries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]FileEntry{
			{Name: "file.txt", Size: 42, Type: "file", ModTime: 1700000000.0},
			{Name: "subdir", Size: 0, Type: "directory", ModTime: 1700000001.0},
		})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	entries, err := c.List(context.Background(), ".")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name != "file.txt" || entries[0].Size != 42 || entries[0].Type != "file" {
		t.Errorf("unexpected first entry: %+v", entries[0])
	}
	if entries[1].Name != "subdir" || entries[1].Type != "directory" {
		t.Errorf("unexpected second entry: %+v", entries[1])
	}
}

func TestList_EmptyDirectoryReturnsEmptySlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	entries, err := c.List(context.Background(), "empty")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if entries == nil {
		t.Fatal("expected non-nil slice for empty directory")
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestList_UnknownFileType_Filtered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]FileEntry{
			{Name: "good.txt", Size: 10, Type: FileTypeFile, ModTime: 1700000000.0},
			{Name: "link.txt", Size: 10, Type: "symlink", ModTime: 1700000000.0},
			{Name: "subdir", Size: 0, Type: FileTypeDirectory, ModTime: 1700000000.0},
		})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	entries, err := c.List(context.Background(), ".")
	if err != nil {
		t.Fatalf("List() should succeed with unknown types filtered, got: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (symlink filtered), got %d", len(entries))
	}
	if entries[0].Name != "good.txt" || entries[1].Name != "subdir" {
		t.Errorf("unexpected entries: %+v", entries)
	}
}

func TestList_AllUnknownTypes_ReturnsEmptySlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]FileEntry{
			{Name: "link1", Size: 10, Type: "symlink", ModTime: 1700000000.0},
			{Name: "pipe1", Size: 0, Type: "pipe", ModTime: 1700000000.0},
		})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	entries, err := c.List(context.Background(), ".")
	if err != nil {
		t.Fatalf("List() should succeed when all types are unknown, got: %v", err)
	}
	if entries == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (all filtered), got %d", len(entries))
	}
}

func TestWrite_ExceedsMaxUploadSize(t *testing.T) {
	c := newReadyTestSandbox("http://unused")
	c.files.maxUpload = 10 // 10 bytes
	err := c.Write(context.Background(), "large.bin", make([]byte, 11))
	if err == nil {
		t.Fatal("expected error for content exceeding MaxUploadSize")
	}
	if !strings.Contains(err.Error(), "exceeds MaxUploadSize") {
		t.Errorf("expected MaxUploadSize error, got: %v", err)
	}
}

func TestWrite_ExactMaxUploadSize_Succeeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.files.maxUpload = 10
	err := c.Write(context.Background(), "exact.bin", make([]byte, 10))
	if err != nil {
		t.Fatalf("Write() should succeed at exact MaxUploadSize, got: %v", err)
	}
}

func TestExists_True(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	exists, err := c.Exists(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if !exists {
		t.Error("expected exists=true")
	}
}

func TestExists_False(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": false})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	exists, err := c.Exists(context.Background(), "nope.txt")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if exists {
		t.Error("expected exists=false")
	}
}

func TestRetry_ServerErrorThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	exists, err := c.Exists(context.Background(), "retry-test.txt")
	if err != nil {
		t.Fatalf("Exists() error after retries: %v", err)
	}
	if !exists {
		t.Error("expected exists=true after retry")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", got)
	}
}

func TestHTTPHeaders_AllSet(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.sandboxID = "my-claim"
	c.connector.namespace = "my-ns"
	c.connector.serverPort = 9999
	c.connector.mu.Unlock()

	_, err := c.Exists(context.Background(), "x")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}

	if headers.Get(headerSandboxID) != "my-claim" {
		t.Errorf("wrong %s: %s", headerSandboxID, headers.Get(headerSandboxID))
	}
	if headers.Get(headerSandboxNamespace) != "my-ns" {
		t.Errorf("wrong %s: %s", headerSandboxNamespace, headers.Get(headerSandboxNamespace))
	}
	if headers.Get(headerSandboxPort) != "9999" {
		t.Errorf("wrong %s: %s", headerSandboxPort, headers.Get(headerSandboxPort))
	}
}

func TestOperations_NonOKStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		operation string
		callOp    func(*Sandbox) error
	}{
		{"Run", http.StatusBadRequest, "run", func(c *Sandbox) error { _, err := c.Run(context.Background(), "bad"); return err }},
		{"Write", http.StatusForbidden, "write", func(c *Sandbox) error { return c.Write(context.Background(), "test.txt", []byte("data")) }},
		{"Read", http.StatusNotFound, "read", func(c *Sandbox) error { _, err := c.Read(context.Background(), "missing.txt"); return err }},
		{"List", http.StatusForbidden, "list", func(c *Sandbox) error { _, err := c.List(context.Background(), "."); return err }},
		{"Exists", http.StatusForbidden, "exists", func(c *Sandbox) error { _, err := c.Exists(context.Background(), "test.txt"); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("error response"))
			}))
			defer server.Close()

			c := newReadyTestSandbox(server.URL)
			err := tc.callOp(c)
			if err == nil {
				t.Fatal("expected error for non-OK status")
			}
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("expected HTTPError, got: %v", err)
			}
			if httpErr.StatusCode != tc.status {
				t.Errorf("expected status %d, got %d", tc.status, httpErr.StatusCode)
			}
			if httpErr.Operation != tc.operation {
				t.Errorf("expected operation %q, got %q", tc.operation, httpErr.Operation)
			}
		})
	}
}

func TestRetry_AllExhausted(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 2 * time.Minute // ensure timeout does not cut retries short
	c.connector.mu.Unlock()
	_, err := c.Exists(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	if got := attempts.Load(); got != int32(maxAttempts) {
		t.Errorf("expected %d attempts, got %d", maxAttempts, got)
	}
	errStr := err.Error()
	if !strings.Contains(errStr, fmt.Sprintf("%d attempts", maxAttempts)) {
		t.Errorf("expected error to mention %d attempts, got: %v", maxAttempts, err)
	}
	if !strings.Contains(errStr, "502") {
		t.Errorf("expected error to mention status 502, got: %v", err)
	}
}

func TestRetry_AllRetryableStatusCodes(t *testing.T) {
	for code := range retryableStatusCodes {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if attempts.Add(1) == 1 {
					w.WriteHeader(code)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
			}))
			defer server.Close()

			c := newReadyTestSandbox(server.URL)
			exists, err := c.Exists(context.Background(), "test.txt")
			if err != nil {
				t.Fatalf("Exists() should succeed after retry on %d: %v", code, err)
			}
			if !exists {
				t.Error("expected exists=true")
			}
			if got := attempts.Load(); got != 2 {
				t.Errorf("expected 2 attempts (1 failure + 1 success), got %d", got)
			}
		})
	}
}

func TestRetry_NonRetryableStatusNotRetried(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	_, err := c.Read(context.Background(), "missing.txt")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt for non-retryable status, got %d", got)
	}
}

func TestErrNotReady_WithLastError(t *testing.T) {
	c := newUnreadyTestSandbox()
	c.connector.mu.Lock()
	c.connector.lastError = fmt.Errorf("port-forward crashed")
	c.connector.mu.Unlock()

	_, err := c.Run(context.Background(), "echo hello")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
	if !strings.Contains(err.Error(), "port-forward crashed") {
		t.Errorf("expected error to contain lastError cause, got: %v", err)
	}
}

// --- MaxDownloadSize enforcement ---

func TestRead_ExceedsMaxDownloadSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data := make([]byte, 2048)
		_, _ = w.Write(data)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.files.maxDownload = 1024
	_, err := c.Read(context.Background(), "big.bin")
	if err == nil {
		t.Fatal("expected error for file exceeding MaxDownloadSize")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("expected size limit error, got: %v", err)
	}
}

func TestRead_ExactMaxDownloadSize_Succeeds(t *testing.T) {
	data := make([]byte, 1024)
	for i := range data {
		data[i] = 'A'
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.files.maxDownload = 1024
	result, err := c.Read(context.Background(), "exact.bin")
	if err != nil {
		t.Fatalf("Read() should succeed at exact MaxDownloadSize, got: %v", err)
	}
	if len(result) != 1024 {
		t.Errorf("expected 1024 bytes, got %d", len(result))
	}
}

func TestRead_OneOverMaxDownloadSize_Fails(t *testing.T) {
	data := make([]byte, 1025)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.files.maxDownload = 1024
	_, err := c.Read(context.Background(), "over.bin")
	if err == nil {
		t.Fatal("expected error for MaxDownloadSize+1")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("expected size limit error, got: %v", err)
	}
}

// --- Non-seekable body retry ---

func TestRetry_NonSeekableBody(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	// Wrap in a struct that only exposes io.Reader, hiding Seek.
	body := struct{ io.Reader }{strings.NewReader("payload")}
	resp, err := c.connector.SendRequest(context.Background(), http.MethodPost, "execute", body, "application/json", 0)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "non-seekable body") {
		t.Errorf("expected non-seekable body error, got: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt with non-seekable body, got %d", got)
	}
}

// --- Write rejects directory paths ---

func TestWrite_RejectsDirectoryPath(t *testing.T) {
	c := newReadyTestSandbox("http://unused")
	err := c.Write(context.Background(), "subdir/nested/file.txt", []byte("data"))
	if err == nil {
		t.Fatal("expected error for path with directory separators")
	}
	if !strings.Contains(err.Error(), "not a plain filename") {
		t.Errorf("expected 'not a plain filename' error, got: %v", err)
	}
}

// --- Write path validation ---

func TestWrite_InvalidPath(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"slash", "/"},
		{"double-slash", "///"},
		{"directory-path", "dir/file.txt"},
		{"absolute-path", "/tmp/file.txt"},
		{"relative-dot", "./file.txt"},
	}
	c := newReadyTestSandbox("http://unused")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Write(context.Background(), tc.path, []byte("data"))
			if err == nil {
				t.Fatal("expected error for invalid path")
			}
			if !strings.Contains(err.Error(), "not a plain filename") {
				t.Errorf("expected 'not a plain filename' error, got: %v", err)
			}
		})
	}
}

// --- Write retry with multipart body rewind ---

func TestWrite_RetryOnServerError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the full body to verify it arrives complete on each attempt.
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("attempt %d: failed to get multipart reader: %v", attempts.Load()+1, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			t.Errorf("attempt %d: failed to read part: %v", attempts.Load()+1, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(part)
		if string(data) != "retry payload" {
			t.Errorf("attempt %d: expected 'retry payload', got %q", attempts.Load()+1, string(data))
		}

		n := attempts.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	err := c.Write(context.Background(), "test.txt", []byte("retry payload"))
	if err != nil {
		t.Fatalf("Write() error after retry: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
}

// --- ErrRetriesExhausted wrapping ---

func TestRetry_AllExhausted_WrapsErrRetriesExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 2 * time.Minute
	c.connector.mu.Unlock()
	_, err := c.Exists(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Errorf("expected ErrRetriesExhausted, got: %v", err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Errorf("expected HTTPError in error chain, got: %v", err)
	} else if httpErr.StatusCode != http.StatusBadGateway {
		t.Errorf("expected status 502 in HTTPError, got %d", httpErr.StatusCode)
	}
}

// --- JSON decode failure tests ---

func TestOperations_MalformedJSON(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
		callOp func(*Sandbox) error
	}{
		{"Run", "failed to decode run result", func(c *Sandbox) error { _, err := c.Run(context.Background(), "echo"); return err }},
		{"List", "failed to decode file listing", func(c *Sandbox) error { _, err := c.List(context.Background(), "."); return err }},
		{"Exists", "failed to decode exists response", func(c *Sandbox) error { _, err := c.Exists(context.Background(), "test.txt"); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("{invalid json"))
			}))
			defer server.Close()

			c := newReadyTestSandbox(server.URL)
			err := tc.callOp(c)
			if err == nil {
				t.Fatal("expected error for malformed JSON")
			}
			if !strings.Contains(err.Error(), tc.errMsg) {
				t.Errorf("expected %q error, got: %v", tc.errMsg, err)
			}
		})
	}
}

// --- percentEncode tests ---

func TestPercentEncode(t *testing.T) {
	cases := []struct {
		input, expected string
	}{
		{"simple.txt", "simple.txt"},
		{"path with spaces/file.txt", "path%20with%20spaces%2Ffile.txt"},
		{"file@name!.txt", "file%40name%21.txt"},
		{"a+b=c&d", "a%2Bb%3Dc%26d"},
		{"/home/user/file.txt", "%2Fhome%2Fuser%2Ffile.txt"},
		{"safe-chars_ok.~txt", "safe-chars_ok.~txt"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := percentEncode(tc.input)
			if got != tc.expected {
				t.Errorf("percentEncode(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// --- Mid-retry port-forward death detection ---

func TestDoRequest_DetectsDeathMidRetry(t *testing.T) {
	var attempts atomic.Int32
	var client *Sandbox
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			// Simulate port-forward death before returning the first response.
			// This happens synchronously in the handler, so by the time the
			// client receives the 502, baseURL is already cleared.
			client.connector.mu.Lock()
			client.connector.baseURL = ""
			client.connector.lastError = fmt.Errorf("port-forward crashed")
			client.connector.mu.Unlock()
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	client = newReadyTestSandbox(server.URL)
	_, err := client.Exists(context.Background(), "test.txt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady after mid-retry death, got: %v", err)
	}
	if !strings.Contains(err.Error(), "port-forward crashed") {
		t.Errorf("expected lastError in error, got: %v", err)
	}
}

func TestRetry_ConnectionErrorThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)

	// Wrap transport to fail on first attempt with a connection error.
	c.connector.httpClient.Transport = &failFirstTransport{
		inner:     c.connector.httpClient.Transport,
		failCount: 1,
	}

	exists, err := c.Exists(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("expected success after connection retry, got: %v", err)
	}
	if !exists {
		t.Error("expected exists=true")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 server-side attempt (after retry), got %d", got)
	}
}

// --- backoff tests ---

func TestBackoffDuration(t *testing.T) {
	c := newUnreadyTestSandbox()

	// Attempt 0 should be immediate (0 duration) — first retry has no backoff.
	d0 := c.connector.backoff(0)
	if d0 != 0 {
		t.Errorf("attempt 0: expected immediate retry (0), got %v", d0)
	}

	// All subsequent attempts should be positive and capped.
	for attempt := 1; attempt < 20; attempt++ {
		d := c.connector.backoff(attempt)
		if d <= 0 {
			t.Errorf("attempt %d: backoff must be positive, got %v", attempt, d)
		}
		// Maximum possible: maxBackoff + maxBackoff/4 (jitter < d/4).
		if d > maxBackoff+maxBackoff/4 {
			t.Errorf("attempt %d: backoff %v exceeds cap with jitter", attempt, d)
		}
	}

	// Attempt 1 should be around baseBackoff (500ms) +/- 25% jitter.
	d1 := c.connector.backoff(1)
	expected1 := baseBackoff
	if d1 < expected1*3/4 || d1 >= expected1*5/4 {
		t.Errorf("attempt 1: backoff %v outside expected range [%v, %v)", d1, expected1*3/4, expected1*5/4)
	}

	// Attempt 2 should be around 2*baseBackoff (1s) +/- 25% jitter.
	d2 := c.connector.backoff(2)
	expected2 := 2 * baseBackoff
	if d2 < expected2*3/4 || d2 >= expected2*5/4 {
		t.Errorf("attempt 2: backoff %v outside expected range [%v, %v)", d2, expected2*3/4, expected2*5/4)
	}
}

func TestBackoff_ScaleReducesDuration(t *testing.T) {
	c := newUnreadyTestSandbox()
	c.connector.mu.Lock()
	c.connector.backoffScale = 0.001
	c.connector.mu.Unlock()

	for attempt := 0; attempt < 6; attempt++ {
		d := c.connector.backoff(attempt)
		if d > 50*time.Millisecond {
			t.Errorf("attempt %d: scaled backoff %v should be very small", attempt, d)
		}
	}
}

func TestBackoff_ExtremelySmallScale_NoPanic(t *testing.T) {
	c := newUnreadyTestSandbox()
	c.connector.mu.Lock()
	c.connector.backoffScale = 0.0000001 // produces sub-nanosecond durations where d/2 truncates to 0
	c.connector.mu.Unlock()

	// Attempt 0 is always immediate.
	if d := c.connector.backoff(0); d != 0 {
		t.Errorf("attempt 0: expected 0, got %v", d)
	}
	for attempt := 1; attempt < 10; attempt++ {
		d := c.connector.backoff(attempt)
		if d <= 0 {
			t.Errorf("attempt %d: backoff must be positive, got %v", attempt, d)
		}
	}
}

// --- Seek failure during retry ---

func TestRetry_SeekFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	body := &failOnSecondSeek{Reader: strings.NewReader("payload")}
	resp, err := c.connector.SendRequest(context.Background(), http.MethodPost, "execute", body, "application/json", 0)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "failed to reset request body") {
		t.Errorf("expected seek failure error, got: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 server hit before seek failure, got %d", got)
	}
}

// --- Connection error exhaustion ---

func TestRetry_ConnectionErrorExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("should not reach server")
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.httpClient.Transport = &failFirstTransport{
		inner:     c.connector.httpClient.Transport,
		failCount: maxAttempts, // fail all attempts
	}

	_, err := c.Exists(context.Background(), "test.txt")
	if err == nil {
		t.Fatal("expected error after all connection retries exhausted")
	}
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Errorf("expected ErrRetriesExhausted, got: %v", err)
	}
}

// --- Concurrent Close during retry ---

func TestDoRequest_ConcurrentClose(t *testing.T) {
	firstHit := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case firstHit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 30 * time.Second
	c.connector.backoffScale = 0.1 // moderate backoff so clearing URL wins the race
	c.connector.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Exists(context.Background(), "test.txt")
		errCh <- err
	}()

	// Wait for the first request to arrive, then clear baseURL.
	<-firstHit
	c.connector.mu.Lock()
	c.connector.baseURL = ""
	c.connector.mu.Unlock()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady after concurrent close, got: %v", err)
	}
}

// --- Request ID header ---

func TestDoRequest_SendsRequestID(t *testing.T) {
	var receivedReqID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedReqID = r.Header.Get(headerRequestID)
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	_, err := c.Exists(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if receivedReqID == "" {
		t.Error("expected non-empty X-Request-ID header")
	}
	if len(receivedReqID) != 16 { // 8 bytes hex-encoded
		t.Errorf("expected 16-char request ID, got %q", receivedReqID)
	}
}

func TestRead_BodyReadError(t *testing.T) {
	// Server claims a large body but closes the connection after a few bytes,
	// causing io.ReadAll to fail with an unexpected EOF.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("hijacking not supported")
		}
		conn, bufrw, _ := hj.Hijack()
		_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100000\r\n\r\nshort")
		_ = bufrw.Flush()
		_ = conn.Close()
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	_, err := c.Read(context.Background(), "test.txt")
	if err == nil {
		t.Fatal("expected error from truncated response body")
	}
	if !strings.Contains(err.Error(), "failed to read file content") &&
		!strings.Contains(err.Error(), "read") {
		t.Errorf("expected read-related error, got: %v", err)
	}
}

// --- Per-attempt timeout tests ---

func TestPerAttemptTimeout_RetriesOnSlowServer(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			// First attempt: block longer than PerAttemptTimeout.
			time.Sleep(500 * time.Millisecond)
			return
		}
		// Second attempt: respond immediately.
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 5 * time.Second
	c.connector.perAttemptTimeout = 100 * time.Millisecond // Very short: triggers timeout on slow first attempt.
	c.connector.mu.Unlock()

	exists, err := c.Exists(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if !exists {
		t.Error("expected exists=true after per-attempt timeout retry")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("expected 2 attempts (first timed out, second succeeded), got %d", got)
	}
}

func TestRequestID_StableAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var reqIDs []string
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqIDs = append(reqIDs, r.Header.Get(headerRequestID))
		mu.Unlock()
		n := attempts.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	_, err := c.Exists(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqIDs) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(reqIDs))
	}
	for i := 1; i < len(reqIDs); i++ {
		if reqIDs[i] != reqIDs[0] {
			t.Errorf("request ID changed between attempt 1 and %d: %q vs %q", i+1, reqIDs[0], reqIDs[i])
		}
	}
}

// --- RequestTimeout auto-application ---

func TestRequestTimeout_AutoApplied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 200 * time.Millisecond
	c.connector.mu.Unlock()

	start := time.Now()
	_, err := c.Exists(context.Background(), "test.txt")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from RequestTimeout")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected failure within ~200ms (RequestTimeout), took %s — timeout not applied", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded in error chain, got: %v", err)
	}
}

// --- PerAttemptTimeout vs RequestTimeout precedence ---

func TestPerAttemptTimeout_BoundedByRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 300 * time.Millisecond
	c.connector.perAttemptTimeout = 5 * time.Second // much longer than RequestTimeout
	c.connector.mu.Unlock()

	start := time.Now()
	_, err := c.Exists(context.Background(), "test.txt")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error")
	}
	// Must be bounded by RequestTimeout (300ms), not PerAttemptTimeout (5s).
	if elapsed > 1*time.Second {
		t.Fatalf("expected failure within ~300ms (RequestTimeout), took %s — PerAttemptTimeout leaked", elapsed)
	}
}

// --- Write trailing-slash path ---

func TestWrite_TrailingSlashPath(t *testing.T) {
	c := newReadyTestSandbox("http://unused")
	// "some/dir/" contains directory separators and should be rejected.
	err := c.Write(context.Background(), "some/dir/", []byte("data"))
	if err == nil {
		t.Fatal("expected error for path with directory separators")
	}
	if !strings.Contains(err.Error(), "not a plain filename") {
		t.Errorf("expected 'not a plain filename' error, got: %v", err)
	}
}

// --- Per-attempt timeout vs body read ---

func TestDoRequest_BodyReadableAfterPerAttemptTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Write first byte immediately so headers are sent and SendRequest returns.
		_, _ = w.Write([]byte("h"))
		w.(http.Flusher).Flush()
		// Remaining bytes arrive slowly, exceeding PerAttemptTimeout.
		for _, b := range []byte("ello") {
			time.Sleep(60 * time.Millisecond)
			_, _ = w.Write([]byte{b})
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.perAttemptTimeout = 100 * time.Millisecond
	c.connector.requestTimeout = 5 * time.Second
	c.connector.mu.Unlock()

	resp, err := c.connector.SendRequest(context.Background(), http.MethodGet, "download/test.txt", nil, "", 0)
	if err != nil {
		t.Fatalf("SendRequest error: %v", err)
	}

	// Body read takes ~240ms, longer than PerAttemptTimeout (100ms).
	// If the per-attempt timer were not stopped, context would cancel and this would fail.
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("body read should succeed (governed by RequestTimeout, not PerAttemptTimeout): %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("expected 'hello', got %q", string(data))
	}
}

// --- WithTimeout per-call option ---

func TestWithTimeout_OverridesRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer srv.Close()

	c := newReadyTestSandbox(srv.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 5 * time.Second
	c.connector.mu.Unlock()
	ctx := context.Background()

	// Without WithTimeout, the 5s RequestTimeout allows the 200ms delay.
	exists, err := c.Exists(ctx, "test.txt")
	if err != nil {
		t.Fatalf("Exists without WithTimeout: %v", err)
	}
	if !exists {
		t.Error("expected exists=true")
	}

	// With WithTimeout(50ms), the call should timeout before the 200ms response.
	_, err = c.Exists(ctx, "test.txt", WithTimeout(50*time.Millisecond))
	if err == nil {
		t.Fatal("expected timeout error with WithTimeout(50ms)")
	}
}

func TestWithTimeout_CallerContextWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
	}))
	defer srv.Close()

	c := newReadyTestSandbox(srv.URL)
	// Caller's context has a 50ms deadline, tighter than WithTimeout(5s).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Exists(ctx, "test.txt", WithTimeout(5*time.Second))
	if err == nil {
		t.Fatal("expected caller's context deadline to take precedence")
	}
}

// --- WithMaxAttempts tests ---

func TestWithMaxAttempts_SingleAttempt(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	_, err := c.Exists(context.Background(), "test.txt", WithMaxAttempts(1))
	if err == nil {
		t.Fatal("expected error with single attempt on 502")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt with WithMaxAttempts(1), got %d", got)
	}
}

func TestWithMaxAttempts_InvalidUsesDefault(t *testing.T) {
	for _, n := range []int{0, -5} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer server.Close()

			c := newReadyTestSandbox(server.URL)
			_, err := c.Exists(context.Background(), "test.txt", WithMaxAttempts(n))
			if err == nil {
				t.Fatal("expected error")
			}
			if got := attempts.Load(); got != int32(maxAttempts) {
				t.Errorf("WithMaxAttempts(%d) should use default (%d attempts), got %d", n, maxAttempts, got)
			}
		})
	}
}

func TestWithMaxAttempts_CustomCount(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("down"))
	}))
	defer server.Close()

	c := newReadyTestSandbox(server.URL)
	c.connector.mu.Lock()
	c.connector.requestTimeout = 2 * time.Minute // ensure timeout does not cut retries short
	c.connector.mu.Unlock()
	_, err := c.Exists(context.Background(), "test.txt", WithMaxAttempts(3))
	if err == nil {
		t.Fatal("expected error after 3 attempts")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected exactly 3 attempts with WithMaxAttempts(3), got %d", got)
	}
}
