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

package snapshots

import (
	"errors"
	"time"
)

const (
	PodSnapshotAPIGroup      = "podsnapshot.gke.io"
	PodSnapshotAPIVersion    = "v1"
	PodSnapshotPlural        = "podsnapshots"
	PodSnapshotTriggerPlural = "podsnapshotmanualtriggers"
	PodSnapshotTriggerKind   = "PodSnapshotManualTrigger"

	SandboxAPIGroup   = "agents.x-k8s.io"
	SandboxAPIVersion = "v1beta1"
	SandboxPlural     = "sandboxes"

	SandboxNameHashLabel    = "agents.x-k8s.io/sandbox-name-hash"
	PodSnapshotPodAnnotation = "podsnapshot.gke.io/origin-pod"
)

const (
	SuccessCode = 0
	ErrorCode   = 1
)

var (
	ErrCRDNotInstalled = errors.New("Pod Snapshot Controller is not ready; install the PodSnapshotPolicy CRD before using this client")
	ErrSnapshotTimeout = errors.New("snapshot creation timed out")
	ErrSnapshotFailed  = errors.New("snapshot creation failed")
)

// SnapshotResponse is the result of a snapshot creation operation.
type SnapshotResponse struct {
	Success           bool
	TriggerName       string
	SnapshotUID       string
	SnapshotTimestamp time.Time
	ErrorReason       string
	ErrorCode         int
}

// SnapshotDetail holds information about a single snapshot.
type SnapshotDetail struct {
	SnapshotUID       string
	SourcePod         string
	CreationTimestamp time.Time
	Status            string
}

// ListSnapshotResult is the result of a list snapshots operation.
type ListSnapshotResult struct {
	Success     bool
	Snapshots   []SnapshotDetail
	ErrorReason string
	ErrorCode   int
}

// DeleteSnapshotResult is the result of a delete snapshots operation.
type DeleteSnapshotResult struct {
	Success          bool
	DeletedSnapshots []string
	ErrorReason      string
	ErrorCode        int
}

// SnapshotFilter controls which snapshots are returned by List.
type SnapshotFilter struct {
	// ReadyOnly excludes snapshots that are not yet in the Ready state.
	ReadyOnly bool
	// GroupingLabels are additional label selectors applied on top of the
	// mandatory sandbox-name-hash selector.
	GroupingLabels map[string]string
}

// SuspendResponse is the result of a suspend operation.
type SuspendResponse struct {
	Success          bool
	SnapshotResponse *SnapshotResponse
	ErrorReason      string
	ErrorCode        int
}

// ResumeResponse is the result of a resume operation.
type ResumeResponse struct {
	Success              bool
	RestoredFromSnapshot bool
	SnapshotUID          string
	ErrorReason          string
	ErrorCode            int
}

// RestoreCheckResult is the result of an is-restored-from-snapshot check.
type RestoreCheckResult struct {
	Success     bool
	ErrorReason string
	ErrorCode   int
}

// snapshotResult is the internal result returned by waitForSnapshotCompleted.
type snapshotResult struct {
	SnapshotUID       string
	SnapshotTimestamp time.Time
}
