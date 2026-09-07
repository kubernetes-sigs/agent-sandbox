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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return bytes.TrimSpace(data)
}

func fixtureNode() *NodeEvidence {
	node := &NodeEvidence{Node: corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "node-uid-0001"}}, Now: time.Date(2026, 9, 6, 12, 0, 7, 0, time.UTC)}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	node.Status.LifecycleKeyID = "fixture-key"
	node.Status.LifecyclePublicKey = base64.RawStdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	node.Status.CapabilityProfileDigest = "sha256:" + strings.Repeat("7", 64)
	node.Status.RuntimeIncarnation = "runtime-incarnation-0001"
	node.Status.SupportedSourceVersions = []string{sandboxv1beta1.RuntimeAdoptionWireVersion}
	node.Status.Capabilities.AtomicSamePolicyTransfer = true
	node.Status.Capabilities.AdoptionHoldProfileDigest = "sha256:" + strings.Repeat("9", 64)
	return node
}

func TestCanonicalWireFixtures(t *testing.T) {
	var hashes map[string]string
	require.NoError(t, json.Unmarshal(fixtureBytes(t, "adoption-hashes.json"), &hashes))
	for _, tc := range []struct {
		file, key, domain string
		target            any
	}{
		{"adoption-reservation.json", "reservation", "runtime.gatekeeper.sh/agent-sandbox-adoption-reservation/v1", &sandboxv1beta1.RuntimeAdoptionReservation{}},
		{"adoption-context.json", "context", ContextDomain, &Context{}},
		{"adoption-execution.json", "stableExecution", "runtime.gatekeeper.sh/agent-sandbox-execution/v1", &Execution{}},
		{"adoption-source-readback.json", "heldReadback", "runtime.gatekeeper.sh/agent-sandbox-adoption-readback/v1", &Readback{}},
		{"adoption-commit-result.json", "commitResult", CommitDomain, &CommitResult{}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			require.NoError(t, decodeCanonical(fixtureBytes(t, tc.file), tc.target))
			digest, err := Digest(tc.domain, tc.target)
			require.NoError(t, err)
			require.Equal(t, hashes[tc.key], string(digest))
		})
	}
	for _, tc := range []struct {
		file, key, kind string
		claims          any
	}{
		{"adoption-pool-initialization-envelope.json", "poolInitialization", "PoolInitialization", &InitializationClaims{}},
		{"adoption-hold-envelope.json", "hold", "AdoptionHold", &HoldClaims{}},
		{"adoption-prepared-transfer-envelope.json", "preparedTransfer", "PreparedTransfer", &PreparedClaims{}},
		{"adoption-transfer-receipt-envelope.json", "transferReceipt", "TransferReceipt", &TransferReceiptClaims{}},
		{"adoption-start-grant-envelope.json", "startGrant", "AdoptionStartGrant", &TransferClaims{}},
		{"adoption-commit-envelope.json", "commitEnvelope", "AdoptionCommit", &CommitClaims{}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			digest, err := fixtureNode().Verify(fixtureBytes(t, tc.file), tc.kind, tc.claims)
			require.NoError(t, err)
			require.Equal(t, hashes[tc.key], string(digest))
		})
	}
}

func TestEvidenceRejectsSubstitutionAndNoncanonicalEncoding(t *testing.T) {
	original := fixtureBytes(t, "adoption-hold-envelope.json")
	for _, tc := range []struct {
		name string
		edit func(*NodeEvidence, []byte) []byte
	}{
		{"another current key", func(n *NodeEvidence, data []byte) []byte { n.Status.LifecycleKeyID = "replacement-key"; return data }},
		{"trailing whitespace", func(_ *NodeEvidence, data []byte) []byte { return append(data, '\n') }},
		{"unknown envelope field", func(_ *NodeEvidence, data []byte) []byte {
			return bytes.Replace(data, []byte(`{"kind":`), []byte(`{"untrusted":true,"kind":`), 1)
		}},
		{"duplicate key", func(_ *NodeEvidence, data []byte) []byte {
			return bytes.Replace(data, []byte(`{"kind":`), []byte(`{"kind":"AdoptionHold","kind":`), 1)
		}},
		{"different claimant", func(_ *NodeEvidence, data []byte) []byte {
			return bytes.ReplaceAll(data, []byte("claim-uid-0001"), []byte("claim-uid-0002"))
		}},
		{"oversize evidence", func(_ *NodeEvidence, _ []byte) []byte { return bytes.Repeat([]byte("x"), MaxEnvelopeBytes+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := fixtureNode()
			_, err := node.Verify(tc.edit(node, append([]byte(nil), original...)), "AdoptionHold", &HoldClaims{})
			require.Error(t, err)
		})
	}
}

func signFixture(t *testing.T, kind string, claims any) ([]byte, sandboxv1beta1.RuntimeAdoptionDigest) {
	t.Helper()
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	input := append([]byte(kind+"\x00fixture-key\x00"), payload...)
	envelope := Envelope{Kind: kind, Algorithm: "Ed25519", KeyID: "fixture-key", Payload: payload, Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, input))}
	data, err := json.Marshal(envelope)
	require.NoError(t, err)
	digest, err := Digest(EnvelopeDomain, envelope)
	require.NoError(t, err)
	return data, digest
}

func fixtureClaims(t *testing.T, name string, target any) {
	t.Helper()
	var envelope Envelope
	require.NoError(t, decodeCanonical(fixtureBytes(t, name), &envelope))
	require.NoError(t, decodeCanonical(envelope.Payload, target))
}

func liveFixture(t *testing.T) (*NodeEvidence, *sandboxv1beta1.Sandbox, *corev1.Pod, *Observation) {
	t.Helper()
	node := fixtureNode()
	var origin InitializationClaims
	fixtureClaims(t, "adoption-pool-initialization-envelope.json", &origin)
	origin.Issuer = node.issuer()
	originBytes, originDigest := signFixture(t, "PoolInitialization", origin)
	var hold HoldClaims
	fixtureClaims(t, "adoption-hold-envelope.json", &hold)
	hold.Issuer, hold.InitializationDigest = node.issuer(), string(originDigest)
	holdBytes, holdDigest := signFixture(t, "AdoptionHold", hold)
	var prepared PreparedClaims
	fixtureClaims(t, "adoption-prepared-transfer-envelope.json", &prepared)
	prepared.Issuer, prepared.PreviousEvidenceDigest = node.issuer(), string(holdDigest)
	preparedBytes, preparedDigest := signFixture(t, "PreparedTransfer", prepared)
	var receipt TransferReceiptClaims
	fixtureClaims(t, "adoption-transfer-receipt-envelope.json", &receipt)
	receipt.Issuer, receipt.PreviousEvidenceDigest = node.issuer(), string(preparedDigest)
	receiptBytes, receiptDigest := signFixture(t, "TransferReceipt", receipt)
	var grant TransferClaims
	fixtureClaims(t, "adoption-start-grant-envelope.json", &grant)
	grant.Issuer, grant.PreviousEvidenceDigest = node.issuer(), string(receiptDigest)
	grantBytes, grantDigest := signFixture(t, "AdoptionStartGrant", grant)
	var commit CommitClaims
	fixtureClaims(t, "adoption-commit-envelope.json", &commit)
	commit.Issuer, commit.PreviousEvidenceDigest = node.issuer(), string(grantDigest)
	commit.Result.GrantDigest, commit.Result.ReceiptDigest = string(grantDigest), string(receiptDigest)
	resultDigest, err := Digest(CommitDomain, commit.Result)
	require.NoError(t, err)
	commit.ResultDigest = string(resultDigest)
	commitBytes, _ := signFixture(t, "AdoptionCommit", commit)
	initialization := sandboxv1beta1.RuntimeAdoptionInitialization{
		InitializationID: hold.Reservation.InitializationID, SourceActivationID: hold.Reservation.SourceActivationID,
		PoolUID: hold.Reservation.PoolUID, TemplateUID: hold.Reservation.TemplateUID, BlueprintDigest: sandboxv1beta1.RuntimeAdoptionDigest(origin.BlueprintDigest),
		ReceiptDigest: sandboxv1beta1.RuntimeAdoptionDigest(origin.SourceReceiptDigest), Envelope: originBytes,
	}
	var context Context
	require.NoError(t, decodeCanonical(fixtureBytes(t, "adoption-context.json"), &context))
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "agents", UID: hold.Reservation.SandboxUID},
		Status: sandboxv1beta1.SandboxStatus{RuntimeAdoption: &sandboxv1beta1.RuntimeAdoptionStatus{Initialization: &initialization,
			Reservation: hold.Reservation.DeepCopy(), HoldEvidenceDigest: holdDigest, TargetMetadataDigest: sandboxv1beta1.RuntimeAdoptionDigest(context.TargetMetadataDigest),
			Grant: &sandboxv1beta1.RuntimeAdoptionGrant{ContextDigest: sandboxv1beta1.RuntimeAdoptionDigest(grant.ContextDigest), ReceiptDigest: receiptDigest, GrantDigest: grantDigest, ExpiresAt: metav1.NewTime(grant.ExpiresAt)},
		}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "agents", UID: types.UID(hold.Execution.PodUID)},
		Spec:   corev1.PodSpec{NodeName: node.Node.Name, Containers: []corev1.Container{{Name: "agent"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", ContainerID: hold.Execution.ContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	observation := &Observation{AttemptID: string(hold.Reservation.AttemptID), ClaimUID: string(hold.Reservation.ClaimUID), SandboxUID: string(hold.Reservation.SandboxUID),
		SourceActivationID: string(hold.Reservation.SourceActivationID), TargetActivationID: string(hold.Reservation.TargetActivationID), OwnerActivationID: string(hold.Reservation.TargetActivationID),
		ContextDigest: grant.ContextDigest, Context: fixtureBytes(t, "adoption-context.json"), Hold: holdBytes, PreparedTransfer: preparedBytes, Receipt: receiptBytes, StartGrant: grantBytes, Commit: commitBytes}
	return node, sandbox, pod, observation
}

func TestTransferAcceptsRefreshedSourceReceiptAndRetainsCommit(t *testing.T) {
	node, sandbox, pod, observation := liveFixture(t)
	hold, _, err := node.VerifyHold(observation, sandbox.Status.RuntimeAdoption.Reservation, sandbox.Status.RuntimeAdoption.Initialization, pod, true)
	require.NoError(t, err)
	require.NotEqual(t, string(sandbox.Status.RuntimeAdoption.Initialization.ReceiptDigest), hold.SourceReceiptDigest)
	require.NotEqual(t, hold.SourceTask.ExecutableInode, hold.HeldTask.ExecutableInode, "initial exec legitimately changes the live executable")
	context, _, digest, err := node.VerifyTransfer(observation, sandbox, pod, true)
	require.NoError(t, err)
	require.Equal(t, sandbox.Status.RuntimeAdoption.Grant.ReceiptDigest, digest)
	grantDigest, err := node.VerifyStartGrant(observation, context, sandbox.Status.RuntimeAdoption.Grant, true)
	require.NoError(t, err)
	require.Equal(t, sandbox.Status.RuntimeAdoption.Grant.GrantDigest, grantDigest)
	_, _, err = node.VerifyCommit(observation, context, sandbox.Status.RuntimeAdoption.Grant)
	require.NoError(t, err)
	node.Now = node.Now.Add(2 * time.Minute)
	_, _, _, err = node.VerifyTransfer(observation, sandbox, pod, true)
	require.Error(t, err, "expired evidence cannot authorize release")
	context, _, _, err = node.VerifyTransfer(observation, sandbox, pod, false)
	require.NoError(t, err, "historical outcome remains readable")
	_, _, err = node.VerifyCommit(observation, context, sandbox.Status.RuntimeAdoption.Grant)
	require.NoError(t, err)
}

func TestTransferRejectsChangedExecutionOrMetadata(t *testing.T) {
	for _, name := range []string{"claim UID", "Pod UID", "container restart", "container replacement", "runtime incarnation", "target metadata", "hold profile"} {
		t.Run(name, func(t *testing.T) {
			node, sandbox, pod, observation := liveFixture(t)
			switch name {
			case "claim UID":
				sandbox.Status.RuntimeAdoption.Reservation.ClaimUID = "replacement-claim"
			case "Pod UID":
				pod.UID = "replacement-pod"
			case "container restart":
				pod.Status.ContainerStatuses[0].RestartCount = 1
			case "container replacement":
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
			case "runtime incarnation":
				node.Status.RuntimeIncarnation = "replacement-runtime"
			case "target metadata":
				sandbox.Status.RuntimeAdoption.TargetMetadataDigest = sandboxv1beta1.RuntimeAdoptionDigest("sha256:" + strings.Repeat("e", 64))
			case "hold profile":
				node.Status.Capabilities.AdoptionHoldProfileDigest = "sha256:" + strings.Repeat("e", 64)
			}
			_, _, _, err := node.VerifyTransfer(observation, sandbox, pod, true)
			require.Error(t, err)
		})
	}
}

func TestTerminalEvidenceSeparatesUnacquiredRejection(t *testing.T) {
	node, sandbox, _, observation := liveFixture(t)
	reservation := sandbox.Status.RuntimeAdoption.Reservation
	observation.OwnerActivationID, observation.Phase = string(reservation.SourceActivationID), "Rejected"
	termination := Termination{AttemptID: string(reservation.AttemptID), OwnerActivationID: string(reservation.SourceActivationID), Phase: "Rejected", Unacquired: true}
	var err error
	observation.Termination, err = json.Marshal(termination)
	require.NoError(t, err)
	_, err = node.VerifyRejection(observation, reservation)
	require.NoError(t, err)
	_, err = node.VerifyTermination(observation, reservation)
	require.Error(t, err)
	termination.RootDestroyed = true
	observation.Termination, err = json.Marshal(termination)
	require.NoError(t, err)
	_, err = node.VerifyRejection(observation, reservation)
	require.Error(t, err)
}

func TestReadEvidenceRejectsStaleAndReplacementNode(t *testing.T) {
	for _, name := range []string{"fresh", "stale", "future", "replacement", "unhealthy"} {
		t.Run(name, func(t *testing.T) {
			node := fixtureNode()
			node.Spec.NodeRef.Name, node.Spec.NodeRef.UID = node.Node.Name, string(node.Node.UID)
			node.Status.LastHeartbeatTime = &metav1.Time{Time: node.Now}
			node.Status.GadgetDigest = "gadget-digest"
			node.Status.Conditions = []metav1.Condition{{Type: "AgentReady", Status: metav1.ConditionTrue}, {Type: "PolicyStateReady", Status: metav1.ConditionTrue}}
			switch name {
			case "stale":
				node.Status.LastHeartbeatTime = &metav1.Time{Time: node.Now.Add(-MaxEvidenceAge)}
			case "future":
				node.Status.LastHeartbeatTime = &metav1.Time{Time: node.Now.Add(time.Second)}
			case "replacement":
				node.Spec.NodeRef.UID = "replacement-node"
			case "unhealthy":
				node.Status.Conditions = nil
			}
			data, err := json.Marshal(node)
			require.NoError(t, err)
			var status unstructured.Unstructured
			require.NoError(t, json.Unmarshal(data, &status.Object))
			status.SetGroupVersionKind(NodeStatusGVK)
			status.SetName(node.Node.Name)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&node.Node, &status).Build()
			_, err = ReadNamedNodeEvidence(context.Background(), reader, node.Node.Name, node.Now)
			if name == "fresh" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
