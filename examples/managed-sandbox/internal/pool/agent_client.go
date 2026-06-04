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

package pool

import (
	"context"
	"errors"
)

// AgentClient is the controller-side view of the pod-agent's gRPC API.
// The real implementation will use the moat agentsandbox protos; this
// interface exists so the controller can be developed and tested with a
// fake before the proto bindings are vendored.
type AgentClient interface {
	// CreateSandbox is idempotent in sandbox_uid: calling it twice with the
	// same uid returns the same handle.
	CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*SandboxHandle, error)
	// GetSandbox returns the current state of a sandbox. Returns ErrNotFound
	// if the agent does not know about this uid.
	GetSandbox(ctx context.Context, uid string) (*SandboxState, error)
	// DeleteSandbox is idempotent: deleting a non-existent sandbox returns nil.
	DeleteSandbox(ctx context.Context, uid string) error
	// ListSandboxes is used by the controller to reconcile drift after pool
	// pod restart.
	ListSandboxes(ctx context.Context) ([]SandboxState, error)
}

// ErrNotFound is returned by AgentClient methods when the requested sandbox
// is not known to the pod-agent.
var ErrNotFound = errors.New("pool: sandbox not found on pod-agent")

// CreateSandboxRequest carries the minimal fields needed to launch a tenant.
// More fields (egress policy, resource limits, mounts) will be added as the
// pod-agent gains support.
type CreateSandboxRequest struct {
	// UID is the Sandbox CR UID and the primary key for idempotency on
	// the pod-agent.
	UID string
	// Name is the namespaced name of the Sandbox CR, used for logs/debug only.
	Name string
	// ImageReference is the OCI reference of the base rootfs. Must match the
	// pool pod's mounted image; supplied for cross-checking only.
	ImageReference string
	// WorkspaceMountPath is the path inside the tenant where the persistent
	// upper layer is exposed (default "/home").
	WorkspaceMountPath string
	// SessionToken is a high-entropy secret used as the SSH password for
	// this tenant. Empty disables SSH on the pod-agent for this uid.
	SessionToken string
}

// SandboxHandle is what the pod-agent returns after CreateSandbox.
type SandboxHandle struct {
	UID   string
	State SandboxState
}

// SandboxState describes the live state of a tenant as observed by the
// pod-agent. It is intentionally small — anything durable lives in the
// Sandbox CR status, not in this struct.
type SandboxState struct {
	UID      string
	Phase    Phase
	ExitCode *int32
	Reason   string
	Message  string
}

// Phase enumerates the runtime states a tenant can be in.
type Phase string

const (
	PhaseUnknown  Phase = ""
	PhaseCreating Phase = "Creating"
	PhaseRunning  Phase = "Running"
	PhaseStopping Phase = "Stopping"
	PhaseStopped  Phase = "Stopped"
	PhaseFailed   Phase = "Failed"
)
