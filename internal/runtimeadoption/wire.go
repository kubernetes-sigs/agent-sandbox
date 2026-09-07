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

package runtimeadoption

import (
	"encoding/json"
	"time"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// These field orders are part of the versioned canonical contract. Opaque
// policy descriptors remain signed bytes: the node's policy resolver validates
// their contents, while the companion compares their exact identities.
type Envelope struct {
	Kind      string          `json:"kind"`
	Algorithm string          `json:"algorithm"`
	KeyID     string          `json:"keyID"`
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

type Execution struct {
	ContainerID         string `json:"containerID"`
	PID                 int32  `json:"pid"`
	TaskStartTime       uint64 `json:"taskStartTime"`
	ManagedRootCgroupID uint64 `json:"managedRootCgroupID"`
	ManagedRootPath     string `json:"managedRootPath"`
	MountNamespaceID    uint64 `json:"mountNamespaceID"`
	NetworkNamespaceID  uint64 `json:"networkNamespaceID"`
	OCIConfigDigest     string `json:"ociConfigDigest"`
	PodUID              string `json:"podUID"`
	NodeUID             string `json:"nodeUID"`
	RuntimeIncarnation  string `json:"runtimeIncarnation"`
}

type Task struct {
	ContainerID         string `json:"containerID"`
	PID                 int32  `json:"pid"`
	TaskStartTime       uint64 `json:"taskStartTime"`
	ManagedRootCgroupID uint64 `json:"managedRootCgroupID"`
	ManagedRootPath     string `json:"managedRootPath,omitempty"`
	MountNamespaceID    uint64 `json:"mountNamespaceID,omitempty"`
	ExecutableDevice    uint64 `json:"executableDevice,omitempty"`
	ExecutableInode     uint64 `json:"executableInode,omitempty"`
	OCIConfigDigest     string `json:"ociConfigDigest,omitempty"`
	NetworkIdentity     string `json:"networkIdentity,omitempty"`
	GuestContainerID    string `json:"guestContainerID,omitempty"`
}

type Readback struct {
	ActivationID       string    `json:"activationID"`
	RuntimeIncarnation string    `json:"runtimeIncarnation"`
	Task               Task      `json:"task"`
	Generation         uint64    `json:"generation"`
	RevisionSetHash    string    `json:"revisionSetHash"`
	StrictRootState    string    `json:"strictRootState"`
	LinkIDs            []uint32  `json:"linkIDs,omitempty"`
	ProgramIDs         []uint32  `json:"programIDs,omitempty"`
	MapIDs             []uint32  `json:"mapIDs,omitempty"`
	AttachmentPoints   []string  `json:"attachmentPoints,omitempty"`
	VerifiedAt         time.Time `json:"verifiedAt"`
}

type InitializationClaims struct {
	WireVersion             string          `json:"wireVersion"`
	Issuer                  string          `json:"issuer"`
	KeyID                   string          `json:"keyID"`
	IssuedAt                time.Time       `json:"issuedAt"`
	InitializationID        string          `json:"initializationID"`
	FirstClaimOnly          bool            `json:"firstClaimOnly"`
	SourceActivationID      string          `json:"sourceActivationID"`
	SourceReceiptDigest     string          `json:"sourceReceiptDigest"`
	SourceMetadataDigest    string          `json:"sourceMetadataDigest"`
	NamespaceUID            string          `json:"namespaceUID"`
	SandboxUID              string          `json:"sandboxUID"`
	PoolUID                 string          `json:"poolUID"`
	TemplateUID             string          `json:"templateUID"`
	BlueprintVersion        string          `json:"blueprintVersion"`
	BlueprintDigest         string          `json:"blueprintDigest"`
	Images                  json.RawMessage `json:"images"`
	ConfigurationPins       json.RawMessage `json:"configurationPins"`
	SourceSubject           json.RawMessage `json:"sourceSubject"`
	SourceSubjectDigest     string          `json:"sourceSubjectDigest"`
	PolicyEpoch             uint64          `json:"policyEpoch"`
	OrderedRevisions        json.RawMessage `json:"orderedRevisions"`
	RevisionSetHash         string          `json:"revisionSetHash"`
	FailurePolicy           string          `json:"failurePolicy"`
	RequiredCapabilities    json.RawMessage `json:"requiredCapabilities"`
	Execution               Execution       `json:"execution"`
	SourceTask              Task            `json:"sourceTask"`
	RuntimeClassUID         string          `json:"runtimeClassUID"`
	RuntimeClassName        string          `json:"runtimeClassName"`
	RuntimeHandler          string          `json:"runtimeHandler"`
	CapabilityProfileDigest string          `json:"capabilityProfileDigest"`
}

type EvidenceClaims struct {
	WireVersion string    `json:"wireVersion"`
	Issuer      string    `json:"issuer"`
	KeyID       string    `json:"keyID"`
	IssuedAt    time.Time `json:"issuedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Nonce       string    `json:"nonce"`
	Operation   string    `json:"operation"`
}

type HoldClaims struct {
	EvidenceClaims
	Reservation               sandboxv1beta1.RuntimeAdoptionReservation `json:"reservation"`
	InitializationDigest      string                                    `json:"initializationDigest"`
	SourceReceiptDigest       string                                    `json:"sourceReceiptDigest"`
	Execution                 Execution                                 `json:"execution"`
	SourceTask                Task                                      `json:"sourceTask"`
	HeldTask                  Task                                      `json:"heldTask"`
	SourceReadback            Readback                                  `json:"sourceReadback"`
	CapabilityProfileDigest   string                                    `json:"capabilityProfileDigest"`
	HoldProfileDigest         string                                    `json:"holdProfileDigest"`
	RuntimeHoldReadbackDigest string                                    `json:"runtimeHoldReadbackDigest"`
}

type TransferClaims struct {
	EvidenceClaims
	AttemptID              string `json:"attemptID"`
	SourceActivationID     string `json:"sourceActivationID"`
	TargetActivationID     string `json:"targetActivationID"`
	ContextDigest          string `json:"contextDigest"`
	PreviousEvidenceDigest string `json:"previousEvidenceDigest"`
}

type PreparedClaims struct {
	TransferClaims
	Generation uint64 `json:"generation"`
}

type TransferReceiptClaims struct {
	TransferClaims
	Readback Readback `json:"readback"`
}

type Context struct {
	WireVersion               string                                    `json:"wireVersion"`
	Reservation               sandboxv1beta1.RuntimeAdoptionReservation `json:"reservation"`
	BlueprintVersion          string                                    `json:"blueprintVersion"`
	BlueprintDigest           string                                    `json:"blueprintDigest"`
	Images                    json.RawMessage                           `json:"images"`
	ConfigurationPinDigest    string                                    `json:"configurationPinDigest"`
	SourceMetadataDigest      string                                    `json:"sourceMetadataDigest"`
	TargetMetadataDigest      string                                    `json:"targetMetadataDigest"`
	SourceSubject             json.RawMessage                           `json:"sourceSubject"`
	TargetSubject             json.RawMessage                           `json:"targetSubject"`
	SourceSubjectDigest       string                                    `json:"sourceSubjectDigest"`
	TargetSubjectDigest       string                                    `json:"targetSubjectDigest"`
	PolicyEpoch               uint64                                    `json:"policyEpoch"`
	OrderedRevisions          json.RawMessage                           `json:"orderedRevisions"`
	RevisionSetHash           string                                    `json:"revisionSetHash"`
	FailurePolicy             string                                    `json:"failurePolicy"`
	RequiredCapabilities      json.RawMessage                           `json:"requiredCapabilities"`
	Execution                 Execution                                 `json:"execution"`
	SourceTask                Task                                      `json:"sourceTask"`
	HeldTask                  Task                                      `json:"heldTask"`
	RuntimeClassUID           string                                    `json:"runtimeClassUID"`
	RuntimeClassName          string                                    `json:"runtimeClassName"`
	RuntimeHandler            string                                    `json:"runtimeHandler"`
	SourceReceiptDigest       string                                    `json:"sourceReceiptDigest"`
	CapabilityProfileDigest   string                                    `json:"capabilityProfileDigest"`
	HoldProfileDigest         string                                    `json:"holdProfileDigest"`
	HeldBindingReadbackDigest string                                    `json:"heldBindingReadbackDigest"`
	RuntimeHoldReadbackDigest string                                    `json:"runtimeHoldReadbackDigest"`
}

type CommitResult struct {
	AttemptID          string          `json:"attemptID"`
	ContextDigest      string          `json:"contextDigest"`
	SourceActivationID string          `json:"sourceActivationID"`
	TargetActivationID string          `json:"targetActivationID"`
	GrantDigest        string          `json:"grantDigest"`
	ReceiptDigest      string          `json:"receiptDigest"`
	ConsumedAt         time.Time       `json:"consumedAt"`
	CommittedAt        time.Time       `json:"committedAt"`
	Execution          Execution       `json:"execution"`
	PolicyEpoch        uint64          `json:"policyEpoch"`
	OrderedRevisions   json.RawMessage `json:"orderedRevisions"`
	RevisionSetHash    string          `json:"revisionSetHash"`
	ReleaseOutcome     string          `json:"releaseOutcome"`
}

type CommitClaims struct {
	TransferClaims
	Result       CommitResult `json:"result"`
	ResultDigest string       `json:"resultDigest"`
	Readback     Readback     `json:"readback"`
}

type Termination struct {
	AttemptID         string `json:"attemptID"`
	OwnerActivationID string `json:"ownerActivationID"`
	Phase             string `json:"phase"`
	Terminated        bool   `json:"terminated"`
	RootDestroyed     bool   `json:"rootDestroyed"`
	Unacquired        bool   `json:"unacquired,omitempty"`
}
