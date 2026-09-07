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

package controllers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/runtimeadoption"
)

const (
	testCompanionUsername = "system:serviceaccount:agent-sandbox-system:agent-sandbox-controller"
	testRuntimeUsername   = "system:serviceaccount:gatekeeper-runtime-system:gatekeeper-runtime-controller"
	testAgentNamespace    = "gatekeeper-runtime-agent-system"
)

// The fake API applies the real admission handler to every controller patch.
// Runtime evidence comes from the shared, independently signed wire fixtures.
type runtimeProtocolFixture struct {
	t                        *testing.T
	ctx                      context.Context
	writer                   client.WithWatch
	handler                  *RuntimeAdoptionAdmissionHandler
	reconciler               *SandboxClaimReconciler
	scheme                   *runtime.Scheme
	namespace                *corev1.Namespace
	template                 *extensionsv1beta1.SandboxTemplate
	pool                     *extensionsv1beta1.SandboxWarmPool
	claim                    *extensionsv1beta1.SandboxClaim
	sandbox                  *sandboxv1beta1.Sandbox
	pod                      *corev1.Pod
	node                     runtimeadoption.NodeEvidence
	origin                   runtimeadoption.InitializationClaims
	initialization           *sandboxv1beta1.RuntimeAdoptionInitialization
	hold                     runtimeadoption.HoldClaims
	transfer                 runtimeadoption.Context
	observation              runtimeadoption.Observation
	conflictNextSandboxGrant bool
	writes                   []string
}

func protocolJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func readProtocolFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "runtimeadoption", "testdata", name))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, target))
}

func readProtocolClaims(t *testing.T, name string, target any) {
	t.Helper()
	var envelope runtimeadoption.Envelope
	readProtocolFixture(t, name, &envelope)
	require.NoError(t, json.Unmarshal(envelope.Payload, target))
}

func signProtocolClaims(t *testing.T, kind string, claims any) ([]byte, sandboxv1beta1.RuntimeAdoptionDigest) {
	t.Helper()
	payload := protocolJSON(t, claims)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	envelope := runtimeadoption.Envelope{Kind: kind, Algorithm: "Ed25519", KeyID: "fixture-key", Payload: payload,
		Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(kind+"\x00fixture-key\x00"), payload...)))}
	digest, err := runtimeadoption.Digest(runtimeadoption.EnvelopeDomain, envelope)
	require.NoError(t, err)
	return protocolJSON(t, envelope), digest
}

func newRuntimeProtocolFixture(t *testing.T) *runtimeProtocolFixture {
	t.Helper()
	f := &runtimeProtocolFixture{t: t, ctx: context.Background(), scheme: newTestScheme()}
	readProtocolClaims(t, "adoption-pool-initialization-envelope.json", &f.origin)
	readProtocolFixture(t, "adoption-context.json", &f.transfer)
	r := f.transfer.Reservation
	f.namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.Namespace, UID: r.NamespaceUID}}
	f.template = &extensionsv1beta1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: r.Namespace, UID: r.TemplateUID}}
	f.template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"app": "agent"}},
		Spec: corev1.PodSpec{RuntimeClassName: new(runtimeadoption.StrictHandler), AutomountServiceAccountToken: new(false),
			Containers: []corev1.Container{{Name: "agent", Image: "example.invalid/agent@sha256:" + strings.Repeat("3", 64)}}}}
	f.pool = &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: r.Namespace, UID: r.PoolUID},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: f.template.Name},
			RuntimeAdoption: &extensionsv1beta1.SandboxWarmPoolRuntimeAdoption{Mode: extensionsv1beta1.SandboxWarmPoolRuntimeAdoptionSamePolicyFirstClaim}}}
	f.claim = &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: r.ClaimName, Namespace: r.Namespace, UID: r.ClaimUID, Generation: r.ClaimGeneration},
		Spec: extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: f.pool.Name},
			AdditionalPodMetadata: sandboxv1beta1.PodMetadata{Labels: map[string]string{"example.com/team": "claim"}}}}
	f.sandbox = createPoolSandbox(f.pool.Name, f.pool.Namespace, sandboxcontrollers.NameHash(f.pool.Name), f.template, "-member")
	f.sandbox.UID = r.SandboxUID
	f.sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(f.pool, extensionsv1beta1.GroupVersion.WithKind(extensionsv1beta1.SandboxWarmPoolKind))}
	f.sandbox.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
	f.pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: f.sandbox.Name, Namespace: r.Namespace, UID: types.UID(f.origin.Execution.PodUID),
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(f.sandbox, sandboxv1beta1.GroupVersion.WithKind("Sandbox"))}},
		Spec: *f.sandbox.Spec.PodTemplate.Spec.DeepCopy(), Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "agent", ContainerID: f.origin.Execution.ContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	f.pod.Spec.NodeName = "node-a"
	f.pod.ObjectMeta = sandboxcontrollers.ExpectedSandboxPodMetadata(f.ctx, f.sandbox, f.pod)
	f.node.Node = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.pod.Spec.NodeName, UID: types.UID(f.origin.Execution.NodeUID)}}
	f.node.Spec.NodeRef.Name, f.node.Spec.NodeRef.UID = f.node.Node.Name, string(f.node.Node.UID)
	f.node.Status.RuntimeIncarnation, f.node.Status.GadgetDigest = f.origin.Execution.RuntimeIncarnation, "test-gadget"
	f.node.Status.CapabilityProfileDigest = f.origin.CapabilityProfileDigest
	f.node.Status.Capabilities.AtomicSamePolicyTransfer = true
	f.node.Status.Capabilities.AdoptionHoldProfileDigest = f.transfer.HoldProfileDigest
	f.node.Status.SupportedSourceVersions = []string{sandboxv1beta1.RuntimeAdoptionWireVersion}
	f.node.Status.LifecycleKeyID = "fixture-key"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	f.node.Status.LifecyclePublicKey = base64.RawStdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	f.node.Status.Conditions = []metav1.Condition{{Type: "AgentReady", Status: metav1.ConditionTrue}, {Type: "PolicyStateReady", Status: metav1.ConditionTrue}}
	f.origin.Issuer, f.origin.IssuedAt = "gatekeeper-runtime-agent/"+string(f.node.Node.UID), time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	f.writer = fake.NewClientBuilder().WithScheme(f.scheme).WithObjects(f.namespace, f.template, f.pool, f.claim, f.sandbox, f.pod, &f.node.Node).
		WithStatusSubresource(f.claim, f.sandbox, f.pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if err := f.admitPatch(ctx, c, object, patch, ""); err != nil {
					return err
				}
				return c.Patch(ctx, object, patch, options...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
				if err := f.admitPatch(ctx, c, object, patch, subresource); err != nil {
					return err
				}
				return c.SubResource(subresource).Patch(ctx, object, patch, options...)
			},
		}).Build()
	f.handler = &RuntimeAdoptionAdmissionHandler{Reader: f.writer, Decoder: admission.NewDecoder(f.scheme), Scheme: f.scheme,
		RuntimeAdoptionAdmissionOptions: RuntimeAdoptionAdmissionOptions{ControllerUsername: testCompanionUsername, RuntimeControllerUsername: testRuntimeUsername,
			AgentNamespace: testAgentNamespace, AllowedLabelDomains: []string{"example.com"}}}
	f.restartController()
	f.reload()
	metadataDigest, err := runtimeadoption.MetadataDigest(f.namespace, f.sandbox, f.pod)
	require.NoError(t, err)
	f.origin.SourceMetadataDigest = string(metadataDigest)
	originBytes, _ := signProtocolClaims(t, "PoolInitialization", f.origin)
	f.node.Status.PoolInitializations = []runtimeadoption.InitializationObservation{{InitializationID: f.origin.InitializationID, SourceActivationID: f.origin.SourceActivationID,
		NamespaceUID: f.origin.NamespaceUID, SandboxUID: f.origin.SandboxUID, PoolUID: f.origin.PoolUID, TemplateUID: f.origin.TemplateUID,
		BlueprintDigest: f.origin.BlueprintDigest, ReceiptDigest: f.origin.SourceReceiptDigest, Envelope: originBytes}}
	f.node.Now = time.Now()
	f.initialization, _, err = f.node.Initialization(f.sandbox, f.pod)
	require.NoError(t, err)
	f.publish()
	return f
}

func protocolAdmissionRequest(t *testing.T, before, after client.Object, username, subresource string) admission.Request {
	t.Helper()
	var resource metav1.GroupVersionResource
	switch after.(type) {
	case *sandboxv1beta1.Sandbox:
		resource = metav1.GroupVersionResource{Group: sandboxv1beta1.GroupVersion.Group, Version: "v1beta1", Resource: "sandboxes"}
	case *extensionsv1beta1.SandboxClaim:
		resource = metav1.GroupVersionResource{Group: extensionsv1beta1.GroupVersion.Group, Version: "v1beta1", Resource: "sandboxclaims"}
	case *corev1.Pod:
		resource = metav1.GroupVersionResource{Version: "v1", Resource: "pods"}
	default:
		resource = metav1.GroupVersionResource{Group: runtimeadoption.NodeStatusGVK.Group, Version: runtimeadoption.NodeStatusGVK.Version, Resource: "runtimepolicynodestatuses"}
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update, Resource: resource,
		Namespace: after.GetNamespace(), Name: after.GetName(), SubResource: subresource,
		UserInfo: authenticationv1.UserInfo{Username: username}, OldObject: runtime.RawExtension{Raw: protocolJSON(t, before)}, Object: runtime.RawExtension{Raw: protocolJSON(t, after)}}}
}

func (f *runtimeProtocolFixture) admitPatch(ctx context.Context, reader client.Reader, object client.Object, patch client.Patch, subresource string) error {
	before := object.DeepCopyObject().(client.Object)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(object), before); err != nil {
		return err
	}
	data, err := patch.Data(object)
	if err != nil {
		return err
	}
	var patched []byte
	if patch.Type() == types.JSONPatchType {
		decoded, err := jsonpatch.DecodePatch(data)
		if err != nil {
			return err
		}
		patched, err = decoded.Apply(protocolJSON(f.t, before))
		if err != nil {
			return err
		}
	} else {
		patched, err = jsonpatch.MergePatch(protocolJSON(f.t, before), data)
		if err != nil {
			return err
		}
	}
	after := object.DeepCopyObject().(client.Object)
	if err := json.Unmarshal(patched, after); err != nil {
		return err
	}
	request := protocolAdmissionRequest(f.t, before, after, testCompanionUsername, subresource)
	response := f.handler.Handle(ctx, request)
	if !response.Allowed {
		return fmt.Errorf("admission denied %s/%s: %s", request.Resource.Resource, subresource, response.Result.Message)
	}
	if sandbox, ok := after.(*sandboxv1beta1.Sandbox); ok && subresource == "status" && f.conflictNextSandboxGrant && sandbox.Status.RuntimeAdoption.Grant != nil {
		f.conflictNextSandboxGrant = false
		return apierrors.NewConflict(schema.GroupResource{Group: sandboxv1beta1.GroupVersion.Group, Resource: "sandboxes"}, sandbox.Name, errors.New("concurrent status writer"))
	}
	f.writes = append(f.writes, request.Resource.Resource+"/"+subresource)
	return nil
}

func (f *runtimeProtocolFixture) restartController() {
	f.reconciler = &SandboxClaimReconciler{Client: f.writer, APIReader: f.writer, Scheme: f.scheme, RuntimeAdoptionEnabled: true, AllowedLabelDomains: []string{"example.com"}}
}

func (f *runtimeProtocolFixture) reload() {
	f.t.Helper()
	for _, object := range []client.Object{f.claim, f.sandbox, f.pod} {
		require.NoError(f.t, f.writer.Get(f.ctx, client.ObjectKeyFromObject(object), object))
	}
}

func (f *runtimeProtocolFixture) publish() {
	f.t.Helper()
	f.node.Status.LastHeartbeatTime = &metav1.Time{Time: time.Now().UTC().Truncate(time.Second)}
	var object unstructured.Unstructured
	require.NoError(f.t, json.Unmarshal(protocolJSON(f.t, f.node), &object.Object))
	object.SetGroupVersionKind(runtimeadoption.NodeStatusGVK)
	object.SetName(f.node.Node.Name)
	before := &unstructured.Unstructured{}
	before.SetGroupVersionKind(runtimeadoption.NodeStatusGVK)
	err := f.writer.Get(f.ctx, client.ObjectKeyFromObject(&object), before)
	if apierrors.IsNotFound(err) {
		require.NoError(f.t, f.writer.Create(f.ctx, &object))
	} else {
		require.NoError(f.t, err)
		object.SetResourceVersion(before.GetResourceVersion())
		require.NoError(f.t, f.writer.Update(f.ctx, &object))
	}
}

func (f *runtimeProtocolFixture) reserve() {
	f.t.Helper()
	require.NoError(f.t, f.reconciler.reserveRuntimeAdoption(f.ctx, f.claim, f.sandbox, f.pool, f.template, f.namespace, f.pod, &f.node, f.initialization))
	f.reload()
}

func (f *runtimeProtocolFixture) advance() (*sandboxv1beta1.Sandbox, error) {
	f.reload()
	sandbox, err := f.reconciler.advanceRuntimeAdoption(f.ctx, f.claim)
	f.reload()
	return sandbox, err
}

func (f *runtimeProtocolFixture) pending() {
	f.t.Helper()
	_, err := f.advance()
	require.ErrorIs(f.t, err, errRuntimeAdoptionPending)
}

func (f *runtimeProtocolFixture) evidence(operation string) runtimeadoption.EvidenceClaims {
	return runtimeadoption.EvidenceClaims{WireVersion: sandboxv1beta1.RuntimeAdoptionWireVersion, Issuer: f.origin.Issuer, KeyID: "fixture-key",
		IssuedAt: time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second), ExpiresAt: f.claim.Status.RuntimeAdoption.Reservation.ExpiresAt.Time,
		Nonce: "test-" + operation, Operation: operation}
}

func (f *runtimeProtocolFixture) publishHold() {
	f.t.Helper()
	readProtocolClaims(f.t, "adoption-hold-envelope.json", &f.hold)
	f.hold.EvidenceClaims, f.hold.Reservation = f.evidence("HoldTransfer"), f.claim.Status.RuntimeAdoption.Reservation
	_, originDigest := signProtocolClaims(f.t, "PoolInitialization", f.origin)
	f.hold.InitializationDigest = string(originDigest)
	f.hold.SourceReadback.VerifiedAt = f.hold.IssuedAt
	holdBytes, _ := signProtocolClaims(f.t, "AdoptionHold", f.hold)
	r := f.hold.Reservation
	f.observation = runtimeadoption.Observation{AttemptID: string(r.AttemptID), ClaimUID: string(r.ClaimUID), SandboxUID: string(r.SandboxUID),
		SourceActivationID: string(r.SourceActivationID), TargetActivationID: string(r.TargetActivationID), OwnerActivationID: string(r.SourceActivationID), Phase: "Held", Hold: holdBytes}
	f.node.Status.Adoptions = []runtimeadoption.Observation{f.observation}
	f.publish()
}

func (f *runtimeProtocolFixture) transferClaims(operation string, previous sandboxv1beta1.RuntimeAdoptionDigest) runtimeadoption.TransferClaims {
	r := f.claim.Status.RuntimeAdoption.Reservation
	return runtimeadoption.TransferClaims{EvidenceClaims: f.evidence(operation), AttemptID: string(r.AttemptID), SourceActivationID: string(r.SourceActivationID),
		TargetActivationID: string(r.TargetActivationID), ContextDigest: f.observation.ContextDigest, PreviousEvidenceDigest: string(previous)}
}

func (f *runtimeProtocolFixture) publishTransfer() {
	f.t.Helper()
	f.transfer.Reservation = f.claim.Status.RuntimeAdoption.Reservation
	f.transfer.SourceMetadataDigest = f.origin.SourceMetadataDigest
	f.transfer.TargetMetadataDigest = string(f.sandbox.Status.RuntimeAdoption.TargetMetadataDigest)
	readbackDigest, err := runtimeadoption.Digest("runtime.gatekeeper.sh/agent-sandbox-adoption-readback/v1", f.hold.SourceReadback)
	require.NoError(f.t, err)
	f.transfer.HeldBindingReadbackDigest = string(readbackDigest)
	f.observation.Context = protocolJSON(f.t, f.transfer)
	digest, err := runtimeadoption.Digest(runtimeadoption.ContextDomain, f.transfer)
	require.NoError(f.t, err)
	f.observation.ContextDigest = string(digest)
	var prepared runtimeadoption.PreparedClaims
	readProtocolClaims(f.t, "adoption-prepared-transfer-envelope.json", &prepared)
	prepared.TransferClaims = f.transferClaims("PrepareTransfer", f.sandbox.Status.RuntimeAdoption.HoldEvidenceDigest)
	var preparedDigest sandboxv1beta1.RuntimeAdoptionDigest
	f.observation.PreparedTransfer, preparedDigest = signProtocolClaims(f.t, "PreparedTransfer", prepared)
	var receipt runtimeadoption.TransferReceiptClaims
	readProtocolClaims(f.t, "adoption-transfer-receipt-envelope.json", &receipt)
	receipt.TransferClaims = f.transferClaims("ActivateTransfer", preparedDigest)
	receipt.Readback.ActivationID = string(f.transfer.Reservation.TargetActivationID)
	receipt.Readback.VerifiedAt = receipt.IssuedAt
	f.observation.Receipt, _ = signProtocolClaims(f.t, "TransferReceipt", receipt)
	f.observation.OwnerActivationID, f.observation.Phase = receipt.Readback.ActivationID, "Activated"
	f.node.Status.Adoptions = []runtimeadoption.Observation{f.observation}
	f.publish()
}

func (f *runtimeProtocolFixture) publishStartGrant() {
	f.t.Helper()
	grant := f.transferClaims("AuthorizeCommit", f.claim.Status.RuntimeAdoption.Grant.ReceiptDigest)
	f.observation.StartGrant, _ = signProtocolClaims(f.t, "AdoptionStartGrant", grant)
	f.node.Status.Adoptions = []runtimeadoption.Observation{f.observation}
	f.publish()
}

func (f *runtimeProtocolFixture) publishCommit() {
	f.t.Helper()
	var commit runtimeadoption.CommitClaims
	readProtocolClaims(f.t, "adoption-commit-envelope.json", &commit)
	grant := f.claim.Status.RuntimeAdoption.Grant
	commit.TransferClaims = f.transferClaims("CommitTransfer", grant.GrantDigest)
	commit.Result.AttemptID, commit.Result.ContextDigest = string(f.transfer.Reservation.AttemptID), string(grant.ContextDigest)
	commit.Result.SourceActivationID, commit.Result.TargetActivationID = string(f.transfer.Reservation.SourceActivationID), string(f.transfer.Reservation.TargetActivationID)
	commit.Result.GrantDigest, commit.Result.ReceiptDigest = string(grant.GrantDigest), string(grant.ReceiptDigest)
	commit.Result.ConsumedAt, commit.Result.CommittedAt = commit.IssuedAt.Add(time.Second), commit.IssuedAt.Add(2*time.Second)
	digest, err := runtimeadoption.Digest(runtimeadoption.CommitDomain, commit.Result)
	require.NoError(f.t, err)
	commit.ResultDigest = string(digest)
	commit.Readback.ActivationID, commit.Readback.VerifiedAt = commit.Result.TargetActivationID, commit.Result.CommittedAt
	f.observation.Commit, _ = signProtocolClaims(f.t, "AdoptionCommit", commit)
	f.observation.Phase = "Committed"
	f.node.Status.Adoptions = []runtimeadoption.Observation{f.observation}
	f.publish()
}

func TestRuntimeAdoptionOrdersMetadataAndRecoversPartialGrant(t *testing.T) {
	f := newRuntimeProtocolFixture(t)
	f.reserve()
	require.Equal(t, []string{"sandboxclaims/", "sandboxclaims/status", "sandboxes/", "sandboxes/status"}, f.writes)
	f.pending()
	require.True(t, metav1.IsControlledBy(f.sandbox, f.pool))
	require.Error(t, f.reconciler.prepareRuntimeAdoptionTarget(f.ctx, f.claim, f.sandbox))
	f.publishHold()
	f.pending()
	require.NotEmpty(t, f.sandbox.Status.RuntimeAdoption.HoldEvidenceDigest)
	require.True(t, metav1.IsControlledBy(f.sandbox, f.pool))
	f.pending()
	require.True(t, metav1.IsControlledBy(f.sandbox, f.claim))
	require.Empty(t, f.pod.Labels["example.com/team"])
	f.pending()
	require.Empty(t, f.sandbox.Status.RuntimeAdoption.TargetMetadataDigest)
	require.Nil(t, f.claim.Status.RuntimeAdoption.Grant)
	before := f.pod.DeepCopy()
	f.pod.ObjectMeta = sandboxcontrollers.ExpectedSandboxPodMetadata(f.ctx, f.sandbox, f.pod)
	require.NoError(t, f.writer.Patch(f.ctx, f.pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	f.pending()
	require.NotEmpty(t, f.sandbox.Status.RuntimeAdoption.TargetMetadataDigest)
	f.publishTransfer()
	f.conflictNextSandboxGrant = true
	_, err := f.advance()
	require.True(t, apierrors.IsConflict(err), "second grant write should lose the injected race: %v", err)
	require.NotNil(t, f.claim.Status.RuntimeAdoption.Grant)
	require.Nil(t, f.sandbox.Status.RuntimeAdoption.Grant)
	f.restartController()
	f.pending()
	require.Equal(t, f.claim.Status.RuntimeAdoption.Grant, f.sandbox.Status.RuntimeAdoption.Grant)
	f.publishStartGrant()
	f.pending()
	require.NotEmpty(t, f.claim.Status.RuntimeAdoption.Grant.GrantDigest)
	require.Equal(t, f.claim.Status.RuntimeAdoption.Grant, f.sandbox.Status.RuntimeAdoption.Grant)
	f.publishCommit()
	f.pending()
	require.NotEmpty(t, f.claim.Status.RuntimeAdoption.CommitDigest)
	require.Equal(t, f.claim.Status.RuntimeAdoption.CommitDigest, f.sandbox.Status.RuntimeAdoption.CommitDigest)
	require.False(t, runtimeAdoptionVerified(f.claim, f.sandbox, time.Now()))
	f.sandbox.Status.RuntimeActivationVerification = &sandboxv1beta1.RuntimeActivationVerification{
		AttemptID: f.transfer.Reservation.AttemptID, ClaimUID: f.claim.UID, PodUID: f.pod.UID, NodeUID: f.node.Node.UID,
		ContainerID: strings.TrimPrefix(f.transfer.Execution.ContainerID, "containerd://"), TaskStartTime: int64(f.transfer.Execution.TaskStartTime),
		RuntimeIncarnation: f.node.Status.RuntimeIncarnation, TargetActivationID: f.transfer.Reservation.TargetActivationID,
		ReceiptDigest: "sha256:" + sandboxv1beta1.RuntimeAdoptionDigest(strings.Repeat("d", 64)), ContextDigest: f.sandbox.Status.RuntimeAdoption.Grant.ContextDigest,
		CommitDigest: f.sandbox.Status.RuntimeAdoption.CommitDigest, PolicyEpoch: int64(f.transfer.PolicyEpoch), ValidUntil: metav1.NewTime(time.Now().Add(time.Minute)),
		Conditions: []metav1.Condition{{Type: "Verified", Status: metav1.ConditionTrue}},
	}
	require.NoError(t, f.writer.Status().Update(f.ctx, f.sandbox))
	sandbox, err := f.advance()
	require.NoError(t, err)
	f.reconciler.computeAndSetStatus(f.claim, sandbox, err, false)
	require.True(t, meta.IsStatusConditionTrue(f.claim.Status.Conditions, "Ready"))

	f.pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
	require.NoError(t, f.writer.Status().Update(f.ctx, f.pod))
	sandbox, err = f.advance()
	require.Error(t, err)
	f.reconciler.computeAndSetStatus(f.claim, sandbox, err, false)
	require.False(t, meta.IsStatusConditionTrue(f.claim.Status.Conditions, "Ready"))
}

func TestRuntimeRejectionFinalizesOnlyUnacquiredClaim(t *testing.T) {
	for _, acquired := range []bool{false, true} {
		t.Run(fmt.Sprintf("winner-acquired-%t", acquired), func(t *testing.T) {
			f := newRuntimeProtocolFixture(t)
			f.reserve()
			winner := f.sandbox.DeepCopy()
			if !acquired {
				// Crash after the finalizer write but before reservation CAS.
				f.sandbox.Status.RuntimeAdoption = nil
				require.NoError(t, f.writer.Status().Update(f.ctx, f.sandbox))
			}
			loser := f.claim.DeepCopy()
			loser.Name, loser.UID, loser.ResourceVersion = "losing-claim", "losing-claim-uid", ""
			loser.Status.RuntimeAdoption.Reservation.ClaimName, loser.Status.RuntimeAdoption.Reservation.ClaimUID = loser.Name, loser.UID
			loser.Status.RuntimeAdoption.Reservation.AttemptID = "losing-attempt-0001"
			loser.Status.RuntimeAdoption.Reservation.TargetActivationID = "losing-target-0001"
			intent, err := runtimeadoption.ClaimIntentDigest(loser)
			require.NoError(t, err)
			loser.Status.RuntimeAdoption.Reservation.ClaimIntentDigest = intent
			require.NoError(t, f.writer.Create(f.ctx, loser))
			r := loser.Status.RuntimeAdoption.Reservation
			f.node.Status.Adoptions = []runtimeadoption.Observation{{AttemptID: string(r.AttemptID), ClaimUID: string(r.ClaimUID), SandboxUID: string(r.SandboxUID),
				SourceActivationID: string(r.SourceActivationID), TargetActivationID: string(r.TargetActivationID), OwnerActivationID: string(r.SourceActivationID), Phase: "Rejected",
				Termination: protocolJSON(t, runtimeadoption.Termination{AttemptID: string(r.AttemptID), OwnerActivationID: string(r.SourceActivationID), Phase: "Rejected", Unacquired: true})}}
			f.publish()
			_, err = f.reconciler.advanceRuntimeAdoption(f.ctx, loser)
			require.NoError(t, err)
			require.NoError(t, f.writer.Get(f.ctx, client.ObjectKeyFromObject(loser), loser))
			require.NotEmpty(t, loser.Status.RuntimeAdoption.TerminalEvidenceDigest)
			require.NotContains(t, loser.Finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
			require.NoError(t, f.writer.Get(f.ctx, client.ObjectKeyFromObject(f.sandbox), f.sandbox))
			require.Nil(t, f.sandbox.DeletionTimestamp)
			if acquired {
				require.Equal(t, winner.Status.RuntimeAdoption, f.sandbox.Status.RuntimeAdoption)
				require.Equal(t, winner.OwnerReferences, f.sandbox.OwnerReferences)
				require.Contains(t, f.sandbox.Finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
			} else {
				require.Nil(t, f.sandbox.Status.RuntimeAdoption)
				require.NotContains(t, f.sandbox.Finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
			}
		})
	}
}

func TestRuntimeTerminalRecoveryAfterSandboxDeletion(t *testing.T) {
	f := newRuntimeProtocolFixture(t)
	f.reserve()
	r := f.claim.Status.RuntimeAdoption.Reservation
	f.node.Status.Adoptions = []runtimeadoption.Observation{{AttemptID: string(r.AttemptID), ClaimUID: string(r.ClaimUID), SandboxUID: string(r.SandboxUID),
		SourceActivationID: string(r.SourceActivationID), TargetActivationID: string(r.TargetActivationID), OwnerActivationID: string(r.SourceActivationID), Phase: "Terminated",
		Termination: protocolJSON(t, runtimeadoption.Termination{AttemptID: string(r.AttemptID), OwnerActivationID: string(r.SourceActivationID), Phase: "Terminated", Terminated: true, RootDestroyed: true})}}
	f.publish()
	// The first reconciliation removed the Sandbox, then crashed before the
	// final claim write. Its retained node outcome must finish that claim.
	f.sandbox.Finalizers = nil
	require.NoError(t, f.writer.Update(f.ctx, f.sandbox))
	require.NoError(t, f.writer.Delete(f.ctx, f.sandbox))
	f.restartController()
	_, err := f.reconciler.advanceRuntimeAdoption(f.ctx, f.claim)
	require.NoError(t, err)
	require.NoError(t, f.writer.Get(f.ctx, client.ObjectKeyFromObject(f.claim), f.claim))
	require.NotEmpty(t, f.claim.Status.RuntimeAdoption.TerminalEvidenceDigest)
	require.NotContains(t, f.claim.Finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
	var sandboxes sandboxv1beta1.SandboxList
	require.NoError(t, f.writer.List(f.ctx, &sandboxes))
	require.Empty(t, sandboxes.Items)
}

func TestRuntimePreparationRecoveryUsesCurrentClaim(t *testing.T) {
	for _, acquired := range []bool{false, true} {
		t.Run(fmt.Sprintf("attempt-exists-%t", acquired), func(t *testing.T) {
			f := newRuntimeProtocolFixture(t)
			require.NoError(t, runtimeadoption.SetFinalizer(f.ctx, f.writer, f.claim, true))
			stale := f.claim.DeepCopy()
			if acquired {
				f.reserve()
			}
			require.NoError(t, f.reconciler.recoverEmptyRuntimePreparation(f.ctx, stale))
			if acquired {
				require.NotNil(t, stale.Status.RuntimeAdoption)
				require.Contains(t, stale.Finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
				require.ErrorIs(t, f.reconciler.retryAdoptionAnnotation(f.ctx, stale, "legacy-candidate"), ErrRuntimeAdoptionUnavailable)
				require.Empty(t, stale.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
			} else {
				require.Nil(t, stale.Status.RuntimeAdoption)
				require.NotContains(t, stale.Finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
			}
		})
	}
}

func TestRuntimeAdoptionRequiresAdmissionAndQualifiedCapability(t *testing.T) {
	for _, name := range []string{"disabled platform", "missing admission", "unqualified node"} {
		t.Run(name, func(t *testing.T) {
			f := newRuntimeProtocolFixture(t)
			f.reconciler.RuntimeAdoptionWebhookName = runtimeadoption.DefaultWebhookConfigurationName
			switch name {
			case "disabled platform":
				f.reconciler.RuntimeAdoptionEnabled = false
			case "unqualified node":
				configuration := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: runtimeadoption.DefaultWebhookConfigurationName},
					Webhooks: []admissionregistrationv1.ValidatingWebhook{{Name: runtimeadoption.AdmissionWebhookName, Rules: runtimeadoption.AdmissionRules(),
						FailurePolicy: new(admissionregistrationv1.Fail), MatchPolicy: new(admissionregistrationv1.Equivalent), SideEffects: new(admissionregistrationv1.SideEffectClassNone), AdmissionReviewVersions: []string{"v1"},
						ClientConfig: admissionregistrationv1.WebhookClientConfig{CABundle: []byte("test-ca"), Service: &admissionregistrationv1.ServiceReference{Name: "admission", Namespace: "agent-sandbox-system", Path: new(runtimeadoption.AdmissionPath)}}}}}
				require.NoError(t, f.writer.Create(f.ctx, configuration))
				f.node.Status.Capabilities.AtomicSamePolicyTransfer = false
				f.publish()
			}
			sandbox, err := f.reconciler.getOrCreateRuntimeAdoptionSandbox(f.ctx, f.claim, f.pool)
			require.NoError(t, err)
			require.Nil(t, sandbox, "caller must use the cold creation path")
			require.Nil(t, f.claim.Status.RuntimeAdoption)
			require.True(t, meta.IsStatusConditionFalse(f.claim.Status.Conditions, "RuntimeAdoption"))
			require.Empty(t, f.writes)
		})
	}
}

func TestStrictRuntimeCannotUseLegacyRecoveryPaths(t *testing.T) {
	for _, name := range []string{"complete", "conflict", "status", "annotation", "legacy label", "same name", "already exists"} {
		for _, runtimeClassName := range []string{runtimeadoption.StrictHandler, "strict-alias"} {
			t.Run(name+"/"+runtimeClassName, func(t *testing.T) {
				f := newRuntimeProtocolFixture(t)
				f.pool.Spec.RuntimeAdoption = nil
				require.NoError(t, f.writer.Update(f.ctx, f.pool))
				if runtimeClassName != runtimeadoption.StrictHandler {
					require.NoError(t, f.writer.Create(f.ctx, &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: runtimeClassName}, Handler: runtimeadoption.StrictHandler}))
				}
				f.sandbox.Spec.PodTemplate.Spec.RuntimeClassName = &runtimeClassName
				f.sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
				f.sandbox.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(f.claim, extensionsv1beta1.GroupVersion.WithKind("SandboxClaim"))}
				require.NoError(t, f.writer.Update(f.ctx, f.sandbox))
				if name == "same name" || name == "already exists" {
					require.NoError(t, f.writer.Delete(f.ctx, f.sandbox))
					f.sandbox.Name, f.sandbox.ResourceVersion = f.claim.Name, ""
					require.NoError(t, f.writer.Create(f.ctx, f.sandbox))
				}
				var err error
				switch name {
				case "complete":
					err = f.reconciler.completeAdoption(f.ctx, f.claim, f.sandbox)
				case "conflict":
					_, err = f.reconciler.resolveAdoptionCompletion(f.ctx, f.claim, f.sandbox.Name)
				case "already exists":
					_, err = f.reconciler.createSandbox(f.ctx, f.claim, f.template)
				default:
					switch name {
					case "status":
						f.claim.Status.SandboxStatus.Name = f.sandbox.Name
					case "annotation":
						f.claim.Annotations = map[string]string{extensionsv1beta1.AssignedSandboxNameAnnotation: f.sandbox.Name}
					case "legacy label":
						f.claim.Labels = map[string]string{extensionsv1beta1.DeprecatedAssignedSandboxNameLabel: f.sandbox.Name}
					}
					_, err = f.reconciler.getOrCreateSandbox(f.ctx, f.claim, f.template)
				}
				require.ErrorIs(t, err, ErrRuntimeAdoptionUnavailable)
			})
		}
	}
}
