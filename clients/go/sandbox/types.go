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
	"errors"
	"fmt"
	"time"
)

const (
	gatewayAPIGroup   = "gateway.networking.k8s.io"
	gatewayAPIVersion = "v1"
	gatewayPlural     = "gateways"

	// PodNameAnnotation is the annotation key on a Sandbox resource that
	// identifies the name of the underlying pod.
	PodNameAnnotation = "agents.x-k8s.io/pod-name"

	headerSandboxID        = "X-Sandbox-ID"
	headerSandboxNamespace = "X-Sandbox-Namespace"
	headerSandboxPort      = "X-Sandbox-Port"
	headerRequestID        = "X-Request-ID"
)

// Sentinel errors returned by the SDK. Use errors.Is to check for specific
// failure modes in application code.
var (
	// ErrNotReady indicates the client has not completed Open or has lost connectivity.
	ErrNotReady = errors.New("client is not ready")
	// ErrTimeout indicates an operation exceeded its deadline.
	ErrTimeout = errors.New("operation timed out")
	// ErrClaimFailed indicates the SandboxClaim could not be created.
	ErrClaimFailed = errors.New("claim creation failed")
	// ErrPortForwardDied indicates the port-forward tunnel dropped unexpectedly.
	ErrPortForwardDied = errors.New("port-forward connection lost")
	// ErrAlreadyOpen indicates Open was called on an already-open client.
	ErrAlreadyOpen = errors.New("client is already open; call Close first")
	// ErrOrphanedClaim indicates Close failed to delete the claim; retry with Close.
	ErrOrphanedClaim = errors.New("orphaned claim; call Close() to retry deletion")
	// ErrRetriesExhausted indicates all HTTP retry attempts failed.
	ErrRetriesExhausted = errors.New("retries exhausted")
	// ErrSandboxDeleted indicates the sandbox was removed before becoming ready.
	ErrSandboxDeleted = errors.New("sandbox was deleted before becoming ready")
	// ErrGatewayDeleted indicates the gateway was removed during address discovery.
	ErrGatewayDeleted = errors.New("gateway was deleted during address discovery")
)

// HTTPError represents a non-OK HTTP response from the sandbox.
// Use errors.As to inspect the status code programmatically.
type HTTPError struct {
	// StatusCode is the HTTP status code returned by the sandbox server.
	StatusCode int
	// Body is the (possibly truncated) response body.
	Body string
	// Operation is the SDK method that triggered the request (e.g. "Run", "Read").
	Operation string
}

func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > 256 {
		body = body[:256] + "... [truncated]"
	}
	return fmt.Sprintf("%s returned status %d: %s", e.Operation, e.StatusCode, body)
}

// CallOption configures per-call behavior for SDK operations.
type CallOption func(*callOptions)

type callOptions struct {
	timeout time.Duration
}

// WithTimeout sets the total timeout for a single operation, overriding
// the client's RequestTimeout for that call. If the caller's context
// already has a shorter deadline, the context deadline takes precedence.
func WithTimeout(d time.Duration) CallOption {
	return func(o *callOptions) {
		o.timeout = d
	}
}

// Client provides high-level interaction with an agent-sandbox instance.
// SandboxClient implements this interface; consumers should accept Client
// in their APIs to enable testing with mocks.
type Client interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	IsReady() bool
	Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error)
	Write(ctx context.Context, path string, content []byte, opts ...CallOption) error
	Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error)
	List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error)
	Exists(ctx context.Context, path string, opts ...CallOption) (bool, error)
}

// SandboxInfo provides read-only access to sandbox identity metadata.
// These accessors are on the concrete SandboxClient rather than the Client
// interface so that adding new accessors is not a breaking change for
// mock/fake implementors.
type SandboxInfo interface {
	ClaimName() string
	SandboxName() string
	PodName() string
	Annotations() map[string]string
}

var (
	_ Client      = (*SandboxClient)(nil)
	_ SandboxInfo = (*SandboxClient)(nil)
)

// ExecutionResult holds the result of a command execution in the sandbox.
type ExecutionResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// FileType represents the type of a file entry.
// List() rejects types other than FileTypeFile and FileTypeDirectory.
type FileType string

const (
	FileTypeFile      FileType = "file"
	FileTypeDirectory FileType = "directory"
)

// FileEntry represents a file or directory entry in the sandbox.
type FileEntry struct {
	Name    string   `json:"name"`
	Size    int64    `json:"size"`
	Type    FileType `json:"type"`
	ModTime float64  `json:"mod_time"`
}
