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
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

const (
	MaxEnvelopeBytes = 192 * 1024
	MaxEvidenceAge   = 30 * time.Second
	StrictHandler    = "gatekeeper-runtime-strict"
	BlueprintVersion = "runtime.gatekeeper.sh/agent-sandbox-blueprint/v1"
)

var (
	NodeStatusGVK = schema.GroupVersionKind{Group: "runtime.gatekeeper.sh", Version: "v1alpha1", Kind: "RuntimePolicyNodeStatus"}
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	ErrPending    = errors.New("runtime adoption observation is pending")
	ErrDisabled   = errors.New("qualified same-policy adoption is unavailable")
)

type InitializationObservation struct {
	InitializationID   string `json:"initializationID"`
	SourceActivationID string `json:"sourceActivationID"`
	NamespaceUID       string `json:"namespaceUID"`
	SandboxUID         string `json:"sandboxUID"`
	PoolUID            string `json:"poolUID"`
	TemplateUID        string `json:"templateUID"`
	BlueprintDigest    string `json:"blueprintDigest"`
	ReceiptDigest      string `json:"receiptDigest"`
	Envelope           []byte `json:"envelope"`
}

type Observation struct {
	AttemptID          string `json:"attemptID"`
	ClaimUID           string `json:"claimUID"`
	SandboxUID         string `json:"sandboxUID"`
	SourceActivationID string `json:"sourceActivationID"`
	TargetActivationID string `json:"targetActivationID"`
	OwnerActivationID  string `json:"ownerActivationID"`
	Phase              string `json:"phase"`
	ContextDigest      string `json:"contextDigest,omitempty"`
	RequestDigest      string `json:"requestDigest"`
	Reason             string `json:"reason,omitempty"`
	Context            []byte `json:"context,omitempty"`
	Hold               []byte `json:"hold,omitempty"`
	PreparedTransfer   []byte `json:"preparedTransfer,omitempty"`
	Receipt            []byte `json:"receipt,omitempty"`
	StartGrant         []byte `json:"startGrant,omitempty"`
	Commit             []byte `json:"commit,omitempty"`
	Termination        []byte `json:"termination,omitempty"`
}

type NodeEvidence struct {
	Node corev1.Node `json:"-"`
	Spec struct {
		NodeRef struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"nodeRef"`
	} `json:"spec"`
	Status struct {
		LastHeartbeatTime       *metav1.Time                `json:"lastHeartbeatTime"`
		RuntimeIncarnation      string                      `json:"runtimeIncarnation"`
		GadgetDigest            string                      `json:"gadgetDigest"`
		LifecycleKeyID          string                      `json:"lifecycleKeyID"`
		LifecyclePublicKey      string                      `json:"lifecyclePublicKey"`
		CapabilityProfileDigest string                      `json:"capabilityProfileDigest"`
		SupportedSourceVersions []string                    `json:"supportedSourceVersions"`
		Conditions              []metav1.Condition          `json:"conditions"`
		PoolInitializations     []InitializationObservation `json:"poolInitializations"`
		Adoptions               []Observation               `json:"adoptions"`
		Capabilities            struct {
			AtomicSamePolicyTransfer  bool   `json:"atomicSamePolicyTransfer"`
			AdoptionHoldProfileDigest string `json:"adoptionHoldProfileDigest"`
		} `json:"capabilities"`
	} `json:"status"`
	Now time.Time `json:"-"`
}

// ReadNodeEvidence always uses an API reader, never informer snapshots. A node's
// status key is trusted only because admission restricts that status writer to
// the bound agent service account on this exact Node UID.
func ReadNodeEvidence(ctx context.Context, reader client.Reader, pod *corev1.Pod, now time.Time) (*NodeEvidence, error) {
	if pod.Spec.NodeName == "" {
		return nil, ErrPending
	}
	return ReadNamedNodeEvidence(ctx, reader, pod.Spec.NodeName, now)
}

func ReadNamedNodeEvidence(ctx context.Context, reader client.Reader, nodeName string, now time.Time) (*NodeEvidence, error) {
	var node corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return nil, fmt.Errorf("read adoption Node: %w", err)
	}
	status := &unstructured.Unstructured{}
	status.SetGroupVersionKind(NodeStatusGVK)
	if err := reader.Get(ctx, client.ObjectKey{Name: node.Name}, status); err != nil {
		return nil, fmt.Errorf("read runtime adoption evidence: %w", err)
	}
	data, err := json.Marshal(status.Object)
	if err != nil {
		return nil, err
	}
	var evidence NodeEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		return nil, fmt.Errorf("decode runtime adoption evidence: %w", err)
	}
	evidence.Node, evidence.Now = node, now
	if status.GetDeletionTimestamp() != nil || node.DeletionTimestamp != nil || evidence.Spec.NodeRef.Name != node.Name ||
		evidence.Spec.NodeRef.UID != string(node.UID) || node.UID == "" || evidence.Status.LastHeartbeatTime == nil ||
		evidence.Status.LastHeartbeatTime.After(now) || now.Sub(evidence.Status.LastHeartbeatTime.Time) >= MaxEvidenceAge ||
		evidence.Status.RuntimeIncarnation == "" || evidence.Status.GadgetDigest == "" ||
		!digestPattern.MatchString(evidence.Status.CapabilityProfileDigest) ||
		!meta.IsStatusConditionTrue(evidence.Status.Conditions, "AgentReady") ||
		!meta.IsStatusConditionTrue(evidence.Status.Conditions, "PolicyStateReady") {
		return nil, errors.New("runtime adoption evidence is stale, unhealthy, or belongs to another Node")
	}
	if len(evidence.Status.PoolInitializations) > 256 || len(evidence.Status.Adoptions) > 256 {
		return nil, errors.New("runtime adoption evidence exceeds the bounded inventory")
	}
	return &evidence, nil
}

func (n *NodeEvidence) Qualified() bool {
	return n != nil && n.Status.Capabilities.AtomicSamePolicyTransfer &&
		digestPattern.MatchString(n.Status.Capabilities.AdoptionHoldProfileDigest) &&
		slices.Contains(n.Status.SupportedSourceVersions, sandboxv1beta1.RuntimeAdoptionWireVersion)
}

func (n *NodeEvidence) Initialization(sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod) (*sandboxv1beta1.RuntimeAdoptionInitialization, *InitializationClaims, error) {
	var selected *InitializationObservation
	for i := range n.Status.PoolInitializations {
		item := &n.Status.PoolInitializations[i]
		if item.SandboxUID == string(sandbox.UID) {
			if selected != nil {
				return nil, nil, errors.New("runtime reported multiple origins for one Sandbox")
			}
			selected = item
		}
	}
	if selected == nil {
		return nil, nil, ErrPending
	}
	var claims InitializationClaims
	if _, err := n.Verify(selected.Envelope, "PoolInitialization", &claims); err != nil {
		return nil, nil, err
	}
	if claims.WireVersion != sandboxv1beta1.RuntimeAdoptionWireVersion || !claims.FirstClaimOnly ||
		claims.Issuer != n.issuer() || claims.KeyID != n.Status.LifecycleKeyID ||
		claims.IssuedAt.IsZero() || claims.IssuedAt.After(n.Now) ||
		claims.SandboxUID != selected.SandboxUID || claims.NamespaceUID != selected.NamespaceUID ||
		claims.PoolUID != selected.PoolUID || claims.TemplateUID != selected.TemplateUID ||
		claims.InitializationID != selected.InitializationID || claims.SourceActivationID != selected.SourceActivationID ||
		claims.BlueprintDigest != selected.BlueprintDigest || claims.SourceReceiptDigest != selected.ReceiptDigest ||
		!digestPattern.MatchString(claims.BlueprintDigest) || claims.BlueprintVersion != BlueprintVersion ||
		claims.CapabilityProfileDigest != n.Status.CapabilityProfileDigest || claims.RuntimeHandler != StrictHandler ||
		!n.executionMatches(claims.Execution, pod) {
		return nil, nil, errors.New("pool initialization does not match current authenticated identity")
	}
	return &sandboxv1beta1.RuntimeAdoptionInitialization{
		InitializationID:   sandboxv1beta1.RuntimeAdoptionID(claims.InitializationID),
		SourceActivationID: sandboxv1beta1.RuntimeAdoptionID(claims.SourceActivationID),
		PoolUID:            types.UID(claims.PoolUID), TemplateUID: types.UID(claims.TemplateUID),
		BlueprintDigest: sandboxv1beta1.RuntimeAdoptionDigest(claims.BlueprintDigest),
		ReceiptDigest:   sandboxv1beta1.RuntimeAdoptionDigest(claims.SourceReceiptDigest),
		Envelope:        append([]byte(nil), selected.Envelope...),
	}, &claims, nil
}

func (n *NodeEvidence) Observation(reservation *sandboxv1beta1.RuntimeAdoptionReservation) (*Observation, error) {
	var selected *Observation
	for i := range n.Status.Adoptions {
		item := &n.Status.Adoptions[i]
		if item.AttemptID == string(reservation.AttemptID) {
			if selected != nil {
				return nil, errors.New("runtime reported duplicate adoption attempts")
			}
			selected = item
		}
	}
	if selected == nil {
		return nil, ErrPending
	}
	if selected.ClaimUID != string(reservation.ClaimUID) || selected.SandboxUID != string(reservation.SandboxUID) ||
		selected.SourceActivationID != string(reservation.SourceActivationID) || selected.TargetActivationID != string(reservation.TargetActivationID) {
		return nil, errors.New("runtime observation belongs to a different attempt owner")
	}
	if selected.OwnerActivationID == "" {
		return nil, ErrPending
	}
	if selected.OwnerActivationID != string(reservation.SourceActivationID) && selected.OwnerActivationID != string(reservation.TargetActivationID) {
		return nil, errors.New("runtime observation belongs to a different attempt owner")
	}
	return selected, nil
}

func (n *NodeEvidence) Verify(data []byte, kind string, claims any) (sandboxv1beta1.RuntimeAdoptionDigest, error) {
	var envelope Envelope
	if err := decodeCanonical(data, &envelope); err != nil {
		return "", err
	}
	if envelope.Kind != kind || envelope.Algorithm != "Ed25519" || envelope.KeyID == "" || envelope.KeyID != n.Status.LifecycleKeyID {
		return "", errors.New("runtime evidence has the wrong kind, algorithm, or current key")
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(n.Status.LifecyclePublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return "", errors.New("runtime evidence has an invalid verification key")
	}
	signature, err := base64.RawStdEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return "", errors.New("runtime evidence has an invalid signature encoding")
	}
	input := []byte(envelope.Kind + "\x00" + envelope.KeyID + "\x00")
	input = append(input, envelope.Payload...)
	if !ed25519.Verify(ed25519.PublicKey(key), input, signature) {
		return "", errors.New("runtime evidence signature verification failed")
	}
	if err := decodeCanonical(envelope.Payload, claims); err != nil {
		return "", err
	}
	return Digest(EnvelopeDomain, envelope)
}

func decodeCanonical(data []byte, target any) error {
	if len(data) == 0 || len(data) > MaxEnvelopeBytes {
		return errors.New("runtime evidence is missing or exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode runtime evidence: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("runtime evidence contains trailing JSON")
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, data) {
		return errors.New("runtime evidence is not canonically encoded")
	}
	return nil
}

func (n *NodeEvidence) issuer() string { return "gatekeeper-runtime-agent/" + string(n.Node.UID) }

func (n *NodeEvidence) executionMatches(execution Execution, pod *corev1.Pod) bool {
	if execution.PodUID != string(pod.UID) || execution.NodeUID != string(n.Node.UID) ||
		execution.RuntimeIncarnation != n.Status.RuntimeIncarnation || execution.TaskStartTime == 0 ||
		execution.ManagedRootCgroupID == 0 || execution.PID <= 0 || execution.MountNamespaceID == 0 ||
		execution.NetworkNamespaceID == 0 || !digestPattern.MatchString(execution.OCIConfigDigest) ||
		len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 ||
		len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Running == nil {
		return false
	}
	container := pod.Status.ContainerStatuses[0]
	return container.Name == pod.Spec.Containers[0].Name && container.RestartCount == 0 &&
		strings.TrimPrefix(container.ContainerID, "containerd://") == strings.TrimPrefix(execution.ContainerID, "containerd://")
}

func (n *NodeEvidence) checkEvidence(e EvidenceClaims, operation string, authorize bool) error {
	if e.WireVersion != sandboxv1beta1.RuntimeAdoptionWireVersion || e.Issuer != n.issuer() ||
		e.KeyID != n.Status.LifecycleKeyID || e.Operation != operation || e.Nonce == "" ||
		e.IssuedAt.IsZero() || e.IssuedAt.After(n.Now) || !e.ExpiresAt.After(e.IssuedAt) ||
		(authorize && !n.Now.Before(e.ExpiresAt)) {
		return errors.New("runtime evidence identity, operation, or validity interval is invalid")
	}
	return nil
}

func (n *NodeEvidence) VerifyHold(observation *Observation, reservation *sandboxv1beta1.RuntimeAdoptionReservation, initialization *sandboxv1beta1.RuntimeAdoptionInitialization, pod *corev1.Pod, authorize bool) (*HoldClaims, sandboxv1beta1.RuntimeAdoptionDigest, error) {
	if initialization == nil || reservation == nil || observation == nil {
		return nil, "", errors.New("hold evidence is missing durable source identity")
	}
	var hold HoldClaims
	digest, err := n.Verify(observation.Hold, "AdoptionHold", &hold)
	if err != nil {
		return nil, "", err
	}
	if err := n.checkEvidence(hold.EvidenceClaims, "HoldTransfer", authorize); err != nil {
		return nil, "", err
	}
	var origin InitializationClaims
	initializationDigest, err := n.Verify(initialization.Envelope, "PoolInitialization", &origin)
	if err != nil {
		return nil, "", err
	}
	if !reflect.DeepEqual(hold.Reservation, *reservation) || hold.InitializationDigest != string(initializationDigest) ||
		!digestPattern.MatchString(hold.SourceReceiptDigest) || hold.CapabilityProfileDigest != n.Status.CapabilityProfileDigest ||
		hold.HoldProfileDigest != n.Status.Capabilities.AdoptionHoldProfileDigest ||
		!digestPattern.MatchString(hold.RuntimeHoldReadbackDigest) ||
		!n.executionMatches(hold.Execution, pod) || hold.Execution != origin.Execution || hold.SourceTask != origin.SourceTask ||
		!heldTaskMatchesSource(hold.SourceTask, hold.HeldTask) ||
		hold.SourceReadback.ActivationID != string(reservation.SourceActivationID) || hold.SourceReadback.StrictRootState != "Held" ||
		hold.SourceReadback.Task != origin.SourceTask || hold.SourceReadback.RuntimeIncarnation != n.Status.RuntimeIncarnation ||
		!hold.ExpiresAt.Equal(reservation.ExpiresAt.Time) {
		return nil, "", errors.New("hold evidence does not match the reserved source execution")
	}
	return &hold, digest, nil
}

func heldTaskMatchesSource(source, held Task) bool {
	if held.ExecutableDevice == 0 || held.ExecutableInode == 0 {
		return false
	}
	// The stopped runc init's first exec changes its executable legitimately.
	// All other task identity fields must remain the retained source identity.
	source.ExecutableDevice, source.ExecutableInode = held.ExecutableDevice, held.ExecutableInode
	return source == held
}

func (n *NodeEvidence) VerifyTransfer(observation *Observation, sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod, authorize bool) (*Context, *TransferReceiptClaims, sandboxv1beta1.RuntimeAdoptionDigest, error) {
	adoption := sandbox.Status.RuntimeAdoption
	if adoption == nil || adoption.Reservation == nil || adoption.Initialization == nil {
		return nil, nil, "", errors.New("sandbox is missing durable adoption identity")
	}
	hold, holdDigest, err := n.VerifyHold(observation, adoption.Reservation, adoption.Initialization, pod, authorize)
	if err != nil {
		return nil, nil, "", err
	}
	var context Context
	if err := decodeCanonical(observation.Context, &context); err != nil {
		return nil, nil, "", err
	}
	contextDigest, err := Digest(ContextDomain, context)
	if err != nil {
		return nil, nil, "", err
	}
	var origin InitializationClaims
	if _, err := n.Verify(adoption.Initialization.Envelope, "PoolInitialization", &origin); err != nil {
		return nil, nil, "", err
	}
	readbackDigest, err := Digest("runtime.gatekeeper.sh/agent-sandbox-adoption-readback/v1", hold.SourceReadback)
	if err != nil {
		return nil, nil, "", err
	}
	pinsDigest, err := Digest("runtime.gatekeeper.sh/agent-sandbox-configuration-pins/v1", origin.ConfigurationPins)
	if err != nil {
		return nil, nil, "", err
	}
	if context.WireVersion != sandboxv1beta1.RuntimeAdoptionWireVersion || !reflect.DeepEqual(context.Reservation, *adoption.Reservation) ||
		context.BlueprintVersion != origin.BlueprintVersion || context.BlueprintDigest != origin.BlueprintDigest ||
		!bytes.Equal(context.Images, origin.Images) || context.ConfigurationPinDigest != string(pinsDigest) ||
		context.SourceMetadataDigest != origin.SourceMetadataDigest || context.TargetMetadataDigest != string(adoption.TargetMetadataDigest) ||
		!bytes.Equal(context.SourceSubject, origin.SourceSubject) || context.SourceSubjectDigest != origin.SourceSubjectDigest ||
		!bytes.Equal(context.OrderedRevisions, origin.OrderedRevisions) || context.RevisionSetHash != origin.RevisionSetHash ||
		context.FailurePolicy != origin.FailurePolicy || !bytes.Equal(context.RequiredCapabilities, origin.RequiredCapabilities) ||
		context.Execution != hold.Execution || context.SourceTask != hold.SourceTask || context.HeldTask != hold.HeldTask ||
		context.RuntimeClassUID != origin.RuntimeClassUID || context.RuntimeClassName != origin.RuntimeClassName || context.RuntimeHandler != StrictHandler ||
		context.SourceReceiptDigest != hold.SourceReceiptDigest || context.CapabilityProfileDigest != n.Status.CapabilityProfileDigest ||
		context.HoldProfileDigest != n.Status.Capabilities.AdoptionHoldProfileDigest || context.HeldBindingReadbackDigest != string(readbackDigest) ||
		context.RuntimeHoldReadbackDigest != hold.RuntimeHoldReadbackDigest ||
		observation.ContextDigest != string(contextDigest) || !digestPattern.MatchString(context.TargetSubjectDigest) {
		return nil, nil, "", errors.New("prepared context differs from the retained source or final target identity")
	}
	var prepared PreparedClaims
	preparedDigest, err := n.Verify(observation.PreparedTransfer, "PreparedTransfer", &prepared)
	if err != nil {
		return nil, nil, "", err
	}
	if err := n.checkTransfer(prepared.TransferClaims, context.Reservation, contextDigest, holdDigest, "PrepareTransfer", authorize); err != nil {
		return nil, nil, "", err
	}
	var receipt TransferReceiptClaims
	receiptDigest, err := n.Verify(observation.Receipt, "TransferReceipt", &receipt)
	if err != nil {
		return nil, nil, "", err
	}
	if err := n.checkTransfer(receipt.TransferClaims, context.Reservation, contextDigest, preparedDigest, "ActivateTransfer", authorize); err != nil {
		return nil, nil, "", err
	}
	if receipt.Readback.ActivationID != string(context.Reservation.TargetActivationID) || receipt.Readback.StrictRootState != "Held" ||
		receipt.Readback.Task != context.SourceTask || receipt.Readback.RuntimeIncarnation != n.Status.RuntimeIncarnation ||
		receipt.Readback.RevisionSetHash != context.RevisionSetHash || prepared.Generation == 0 || receipt.Readback.Generation != prepared.Generation ||
		!receipt.ExpiresAt.Equal(prepared.ExpiresAt) || !prepared.ExpiresAt.Equal(hold.ExpiresAt) {
		return nil, nil, "", errors.New("transfer receipt does not prove the held successor binding")
	}
	return &context, &receipt, receiptDigest, nil
}

func (n *NodeEvidence) VerifyStartGrant(observation *Observation, context *Context, grant *sandboxv1beta1.RuntimeAdoptionGrant, authorize bool) (sandboxv1beta1.RuntimeAdoptionDigest, error) {
	var claims TransferClaims
	digest, err := n.Verify(observation.StartGrant, "AdoptionStartGrant", &claims)
	if err != nil {
		return "", err
	}
	if err := n.checkTransfer(claims, context.Reservation, grant.ContextDigest, grant.ReceiptDigest, "AuthorizeCommit", authorize); err != nil {
		return "", err
	}
	if !claims.ExpiresAt.Equal(grant.ExpiresAt.Time) {
		return "", errors.New("start grant deadline differs from the API authorization")
	}
	return digest, nil
}

func (n *NodeEvidence) VerifyCommit(observation *Observation, context *Context, grant *sandboxv1beta1.RuntimeAdoptionGrant) (*CommitClaims, sandboxv1beta1.RuntimeAdoptionDigest, error) {
	var commit CommitClaims
	digest, err := n.Verify(observation.Commit, "AdoptionCommit", &commit)
	if err != nil {
		return nil, "", err
	}
	// This reads a retained outcome. Expired authorization cannot cause a new
	// release, but it must not hide a release already confirmed by the runtime.
	if err := n.checkTransfer(commit.TransferClaims, context.Reservation, grant.ContextDigest, grant.GrantDigest, "CommitTransfer", false); err != nil {
		return nil, "", err
	}
	result := commit.Result
	resultDigest, err := Digest(CommitDomain, result)
	if err != nil {
		return nil, "", err
	}
	if result.AttemptID != string(context.Reservation.AttemptID) || result.ContextDigest != string(grant.ContextDigest) ||
		result.SourceActivationID != string(context.Reservation.SourceActivationID) || result.TargetActivationID != string(context.Reservation.TargetActivationID) ||
		result.GrantDigest != string(grant.GrantDigest) || result.ReceiptDigest != string(grant.ReceiptDigest) ||
		result.Execution != context.Execution || result.PolicyEpoch != context.PolicyEpoch ||
		!bytes.Equal(result.OrderedRevisions, context.OrderedRevisions) || result.RevisionSetHash != context.RevisionSetHash ||
		result.ReleaseOutcome != "Released" || result.ConsumedAt.IsZero() || result.CommittedAt.Before(result.ConsumedAt) || result.CommittedAt.After(n.Now) ||
		!result.ConsumedAt.Before(grant.ExpiresAt.Time) || commit.ResultDigest != string(resultDigest) ||
		commit.Readback.ActivationID != result.TargetActivationID || commit.Readback.StrictRootState != "Active" ||
		commit.Readback.Task != context.SourceTask || commit.Readback.RuntimeIncarnation != n.Status.RuntimeIncarnation ||
		commit.Readback.RevisionSetHash != context.RevisionSetHash {
		return nil, "", errors.New("commit observation does not identify a confirmed release for the authorized grant")
	}
	return &commit, digest, nil
}

func (n *NodeEvidence) VerifyTermination(observation *Observation, reservation *sandboxv1beta1.RuntimeAdoptionReservation) (sandboxv1beta1.RuntimeAdoptionDigest, error) {
	var terminal Termination
	if err := decodeCanonical(observation.Termination, &terminal); err != nil {
		return "", err
	}
	if terminal.AttemptID != string(reservation.AttemptID) || terminal.OwnerActivationID != observation.OwnerActivationID ||
		terminal.Phase != "Terminated" || !terminal.Terminated || !terminal.RootDestroyed || terminal.Unacquired {
		return "", errors.New("runtime has not confirmed termination and original root destruction")
	}
	return Digest("runtime.gatekeeper.sh/agent-sandbox-termination-result/v1", terminal)
}

// VerifyRejection proves the node durably fenced an attempt before acquisition.
// It authorizes claim finalization only, never destruction of the source or a
// Sandbox that another attempt has reserved.
func (n *NodeEvidence) VerifyRejection(observation *Observation, reservation *sandboxv1beta1.RuntimeAdoptionReservation) (sandboxv1beta1.RuntimeAdoptionDigest, error) {
	var rejected Termination
	if err := decodeCanonical(observation.Termination, &rejected); err != nil {
		return "", err
	}
	if rejected.AttemptID != string(reservation.AttemptID) || rejected.OwnerActivationID != string(reservation.SourceActivationID) ||
		rejected.OwnerActivationID != observation.OwnerActivationID || rejected.Phase != "Rejected" || observation.Phase != "Rejected" ||
		!rejected.Unacquired || rejected.Terminated || rejected.RootDestroyed {
		return "", errors.New("runtime has not fenced an unacquired attempt")
	}
	return Digest("runtime.gatekeeper.sh/agent-sandbox-termination-result/v1", rejected)
}

func (n *NodeEvidence) checkTransfer(e TransferClaims, reservation sandboxv1beta1.RuntimeAdoptionReservation, contextDigest, previousDigest sandboxv1beta1.RuntimeAdoptionDigest, operation string, authorize bool) error {
	if err := n.checkEvidence(e.EvidenceClaims, operation, authorize); err != nil {
		return err
	}
	if e.AttemptID != string(reservation.AttemptID) || e.SourceActivationID != string(reservation.SourceActivationID) ||
		e.TargetActivationID != string(reservation.TargetActivationID) || e.ContextDigest != string(contextDigest) ||
		e.PreviousEvidenceDigest != string(previousDigest) || (authorize && e.ExpiresAt.After(reservation.ExpiresAt.Time)) {
		return errors.New("runtime evidence does not match the prepared attempt and previous evidence")
	}
	return nil
}
