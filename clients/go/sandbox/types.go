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
	"maps"
	"time"
)

const (
	gatewayAPIGroup   = "gateway.networking.k8s.io"
	gatewayAPIVersion = "v1"
	gatewayPlural     = "gateways"

	// PodNameAnnotation is the deprecated annotation key on a Sandbox resource that identifies the name of the underlying pod.
	// Deprecated: New Sandboxes use their own name for the backing pod while non-empty legacy annotations may still be honored.
	PodNameAnnotation = "agents.x-k8s.io/pod-name"

	headerSandboxID        = "X-Sandbox-ID"
	headerSandboxNamespace = "X-Sandbox-Namespace"
	headerSandboxPort      = "X-Sandbox-Port"
	headerSandboxPodIP     = "X-Sandbox-Pod-IP"
	headerSandboxTimeout   = "X-Sandbox-Timeout"
	headerRequestID        = "X-Request-ID"
)

// Sentinel errors returned by the SDK.
var (
	ErrNotReady         = errors.New("sandbox is not ready")
	ErrTimeout          = errors.New("operation timed out")
	ErrClaimFailed      = errors.New("claim creation failed")
	ErrWarmPoolNotFound = errors.New("sandbox warm pool not found")
	ErrTemplateNotFound = errors.New("sandbox template not found")
	ErrPortForwardDied  = errors.New("port-forward connection lost")
	ErrNoSandboxService = errors.New("sandbox has no headless Service")
	ErrAlreadyOpen      = errors.New("sandbox is already open; call Close first")
	ErrOrphanedClaim    = errors.New("orphaned claim; call Close() to retry deletion")
	ErrRetriesExhausted = errors.New("retries exhausted")
	ErrSandboxDeleted   = errors.New("sandbox was deleted before becoming ready")
	ErrGatewayDeleted   = errors.New("gateway was deleted during address discovery")
	ErrResponseTooLarge = errors.New("response exceeded 16 MB limit")
	// ErrUnsupportedByRuntime is returned by operations the selected
	// runtime or connection cannot perform (e.g. Delete on the legacy
	// python-runtime, or Run with RuntimeSandboxd and APIURL).
	ErrUnsupportedByRuntime = errors.New("operation not supported by the sandbox runtime")
)

// HTTPError represents a non-OK HTTP response from the sandbox.
type HTTPError struct {
	StatusCode int
	Body       string
	Operation  string
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
	timeout     time.Duration
	maxAttempts int // 0 = use default (maxAttempts const); 1 = no retry
	env         map[string]string
	cwd         string
}

// WithTimeout sets the total timeout for a single operation, overriding
// the default RequestTimeout for that call.
func WithTimeout(d time.Duration) CallOption {
	return func(o *callOptions) {
		o.timeout = d
	}
}

// WithMaxAttempts sets the maximum number of attempts for an operation.
// Values ≤0 are ignored and the default is used (1 for Run, 6 for file
// operations).
//
//	result, err := client.Run(ctx, "cat /etc/hostname", sandbox.WithMaxAttempts(6))
func WithMaxAttempts(n int) CallOption {
	return func(o *callOptions) {
		if n > 0 {
			o.maxAttempts = n
		}
	}
}

// WithEnv sets environment variables for a single Run, merged over the
// sandbox's own environment (a key given here wins). Repeated calls
// accumulate. Run returns ErrUnsupportedByRuntime on the legacy
// python-runtime, which cannot carry them. Other operations ignore it.
//
//	result, err := client.Run(ctx, "echo $GREETING", sandbox.WithEnv(map[string]string{"GREETING": "hi"}))
func WithEnv(env map[string]string) CallOption {
	return func(o *callOptions) {
		if o.env == nil {
			o.env = make(map[string]string, len(env))
		}
		maps.Copy(o.env, env)
	}
}

// WithWorkingDir sets the working directory for a single Run, relative to the
// sandbox root (the default). sandboxd rejects a directory that resolves
// outside the root. Run returns ErrUnsupportedByRuntime on the legacy
// python-runtime. Other operations ignore it.
func WithWorkingDir(dir string) CallOption {
	return func(o *callOptions) {
		o.cwd = dir
	}
}

// ListOption configures Client.ListAllSandboxes.
type ListOption func(*listOptions)

type listOptions struct {
	labelSelector string
}

// WithLabelSelector restricts the listing to claims matching a Kubernetes
// label selector, for example "app=agent,tier!=dev". Pair it with
// Options.Labels, which stamps labels on the claims the client creates.
func WithLabelSelector(selector string) ListOption {
	return func(o *listOptions) {
		o.labelSelector = selector
	}
}

// Handle provides high-level interaction with a sandbox instance.
// Sandbox implements this interface; consumers should accept Handle
// in their APIs to enable testing with mocks. For sub-object access
// (Commands(), Files()), use the concrete *Sandbox type directly.
type Handle interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	Disconnect(ctx context.Context) error
	IsReady() bool

	Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error)
	Write(ctx context.Context, path string, content []byte, opts ...CallOption) error
	Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error)
	List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error)
	Exists(ctx context.Context, path string, opts ...CallOption) (bool, error)
}

// Info provides read-only access to sandbox identity metadata.
type Info interface {
	ClaimName() string
	SandboxName() string
	PodName() string
	PodIP() string
	Annotations() map[string]string
}

// ExecutionResult holds the result of a command execution in the sandbox.
type ExecutionResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Health is the sandboxd runtime's report from GET /v1/health.
type Health struct {
	Status string `json:"status"`
	// UptimeSeconds is how long sandboxd has been running.
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// Metadata is the non-sensitive, workload-scoped configuration sandboxd
// serves from GET /v1/metadata.
type Metadata struct {
	// Env holds the environment variables sandboxd exposes: only names
	// matching its --metadata-env-prefix, minus anything that looks like a
	// credential. It is never nil.
	Env map[string]string `json:"env"`
}

// FileType represents the type of a file entry.
type FileType string

const (
	FileTypeFile      FileType = "file"
	FileTypeDirectory FileType = "directory"
)

// FileEntry represents a file or directory entry in the sandbox. It is
// runtime-neutral: the SDK decodes the legacy wire format (mod_time as a
// float POSIX timestamp) and the sandboxd wire format (modified_at as an
// RFC 3339 string, plus mode) into this one shape.
type FileEntry struct {
	Name    string
	Size    int64
	Type    FileType
	ModTime time.Time
	// Mode holds octal permission bits (e.g. "0644"). Only populated by
	// the sandboxd runtime; empty on legacy.
	Mode string
}

// legacyFileEntry is the python-runtime wire format for a listing entry.
type legacyFileEntry struct {
	Name    string   `json:"name"`
	Size    int64    `json:"size"`
	Type    FileType `json:"type"`
	ModTime float64  `json:"mod_time"`
}

// sandboxdFileEntry is the sandboxd wire format for a listing entry, per
// packages/sandboxd/spec/filesystem/v1/filesystem.yaml.
type sandboxdFileEntry struct {
	Name       string   `json:"name"`
	Size       int64    `json:"size"`
	Type       FileType `json:"type"`
	ModifiedAt string   `json:"modified_at"`
	Mode       string   `json:"mode,omitempty"`
}

// sandboxdDirectoryListing wraps sandboxd listing entries.
type sandboxdDirectoryListing struct {
	Path    string              `json:"path"`
	Entries []sandboxdFileEntry `json:"entries"`
}

// sandboxdAPIError is the error body sandboxd returns on non-2xx responses.
type sandboxdAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
