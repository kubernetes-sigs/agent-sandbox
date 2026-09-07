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
	"context"
	"errors"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/runtimeadoption"
)

func (h *RuntimeAdoptionAdmissionHandler) validateClaimRuntimeStatus(ctx context.Context, before, after *extensionsv1beta1.SandboxClaim) error {
	oldStatus, status := before.Status.RuntimeAdoption, after.Status.RuntimeAdoption
	if status == nil {
		return errors.New("a claim attempt cannot be erased or replaced")
	}
	var sandbox sandboxv1beta1.Sandbox
	sandboxErr := h.Reader.Get(ctx, client.ObjectKey{Namespace: after.Namespace, Name: status.SandboxName}, &sandbox)
	if oldStatus == nil {
		if sandboxErr != nil {
			return sandboxErr
		}
		if status.Grant != nil || status.CommitDigest != "" || status.TerminationRequestedTime != nil || status.TerminalEvidenceDigest != "" {
			return errors.New("a new claim attempt must precede hold and release authorization")
		}
		_, err := h.validateReservationOrigin(ctx, after, &sandbox)
		return err
	}
	if oldStatus.SandboxName != status.SandboxName || oldStatus.PodRef != status.PodRef || oldStatus.NodeRef != status.NodeRef ||
		!reflect.DeepEqual(oldStatus.Reservation, status.Reservation) || !grantExtends(oldStatus.Grant, status.Grant) ||
		!digestExtends(oldStatus.CommitDigest, status.CommitDigest) || !digestExtends(oldStatus.TerminalEvidenceDigest, status.TerminalEvidenceDigest) ||
		!timeExtends(oldStatus.TerminationRequestedTime, status.TerminationRequestedTime) {
		return errors.New("claim attempt identity and retained outcomes are immutable")
	}
	if oldStatus.TerminalEvidenceDigest != status.TerminalEvidenceDigest {
		digest, unacquired, err := h.terminalForClaim(ctx, after)
		if err != nil || digest != status.TerminalEvidenceDigest {
			return errors.Join(errors.New("claim terminal digest does not match authenticated node evidence"), err)
		}
		if unacquired && sandboxErr == nil && sandbox.UID == status.Reservation.SandboxUID && sandbox.Status.RuntimeAdoption != nil &&
			sandbox.Status.RuntimeAdoption.Reservation != nil && sandbox.Status.RuntimeAdoption.Reservation.AttemptID == status.Reservation.AttemptID {
			return errors.New("an acquired reservation cannot be finalized as an unacquired attempt")
		}
	}
	if runtimeGrantsEqual(oldStatus.Grant, status.Grant) && oldStatus.CommitDigest == status.CommitDigest {
		return nil
	}
	if sandboxErr != nil {
		return sandboxErr
	}
	if status.TerminationRequestedTime != nil || status.TerminalEvidenceDigest != "" {
		return errors.New("a cancelling claim cannot publish new release authorization")
	}
	attempt, err := h.readAdmissionAttempt(ctx, after, &sandbox)
	if err != nil {
		return err
	}
	if !runtimeGrantsEqual(oldStatus.Grant, status.Grant) {
		if err := h.validateGrant(ctx, after, &sandbox, attempt, status.Grant); err != nil {
			return err
		}
	}
	if oldStatus.CommitDigest != status.CommitDigest {
		if sandbox.Status.RuntimeAdoption.CommitDigest != status.CommitDigest {
			return errors.New("claim commit must follow the matching Sandbox commit")
		}
		return validateAdmissionCommit(attempt, &sandbox, status.Grant, status.CommitDigest)
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) validateSandboxRuntimeStatus(ctx context.Context, before, after *sandboxv1beta1.Sandbox) error {
	oldStatus, status := before.Status.RuntimeAdoption, after.Status.RuntimeAdoption
	if status == nil || status.Reservation == nil || status.Initialization == nil || status.Consumed == nil {
		return errors.New("sandbox runtime adoption retains initialization, reservation and consumed eligibility")
	}
	claim, err := h.claimForSandbox(ctx, after)
	if err != nil {
		return err
	}
	if oldStatus == nil || oldStatus.Reservation == nil {
		if oldStatus != nil && (runtimeAdoptionProtected(before) || !reflect.DeepEqual(oldStatus.Initialization, status.Initialization)) {
			return errors.New("previous runtime initialization or eligibility cannot be replaced")
		}
		if status.HoldEvidenceDigest != "" || status.TargetMetadataDigest != "" || status.Grant != nil || status.CommitDigest != "" ||
			status.TerminationRequestedTime != nil || status.TerminalEvidenceDigest != "" ||
			status.Consumed.AttemptID != status.Reservation.AttemptID || status.Consumed.ClaimUID != claim.UID ||
			status.Consumed.ConsumedTime.IsZero() || status.Consumed.ConsumedTime.After(time.Now()) {
			return errors.New("sandbox acquisition must precede runtime hold and metadata changes")
		}
		initialization, err := h.validateReservationOrigin(ctx, claim, before)
		if err != nil || !reflect.DeepEqual(initialization, status.Initialization) {
			return errors.Join(errors.New("sandbox origin differs from the verified initialization"), err)
		}
		return nil
	}
	if !reflect.DeepEqual(oldStatus.Reservation, status.Reservation) || !reflect.DeepEqual(oldStatus.Initialization, status.Initialization) ||
		!reflect.DeepEqual(oldStatus.Consumed, status.Consumed) || !grantExtends(oldStatus.Grant, status.Grant) ||
		!digestExtends(oldStatus.HoldEvidenceDigest, status.HoldEvidenceDigest) || !digestExtends(oldStatus.TargetMetadataDigest, status.TargetMetadataDigest) ||
		!digestExtends(oldStatus.CommitDigest, status.CommitDigest) || !digestExtends(oldStatus.TerminalEvidenceDigest, status.TerminalEvidenceDigest) ||
		!timeExtends(oldStatus.TerminationRequestedTime, status.TerminationRequestedTime) {
		return errors.New("sandbox runtime identity, consumption and retained evidence are immutable")
	}
	if oldStatus.TerminalEvidenceDigest != status.TerminalEvidenceDigest {
		digest, unacquired, err := h.terminalForClaim(ctx, claim)
		if err != nil || unacquired || digest != status.TerminalEvidenceDigest {
			return errors.Join(errors.New("sandbox finalization requires original-root destruction evidence"), err)
		}
	}
	progress := oldStatus.HoldEvidenceDigest != status.HoldEvidenceDigest || oldStatus.TargetMetadataDigest != status.TargetMetadataDigest ||
		!runtimeGrantsEqual(oldStatus.Grant, status.Grant) || oldStatus.CommitDigest != status.CommitDigest
	if !progress {
		return nil
	}
	if status.TerminationRequestedTime != nil || status.TerminalEvidenceDigest != "" {
		return errors.New("a cancelling Sandbox cannot advance release authorization")
	}
	attempt, err := h.readAdmissionAttempt(ctx, claim, after)
	if err != nil {
		return err
	}
	if oldStatus.HoldEvidenceDigest != status.HoldEvidenceDigest {
		if err := h.currentIntent(claim, after, attempt.namespace); err != nil {
			return err
		}
		_, holdDigest, err := attempt.evidence.VerifyHold(attempt.observation, status.Reservation, status.Initialization, attempt.pod, true)
		if err != nil || !attempt.evidence.Qualified() || holdDigest != status.HoldEvidenceDigest {
			return errors.Join(errors.New("sandbox hold digest does not match qualified node evidence"), err)
		}
	}
	if oldStatus.TargetMetadataDigest != status.TargetMetadataDigest {
		if oldStatus.HoldEvidenceDigest == "" {
			return errors.New("metadata acknowledgement requires a previously persisted hold")
		}
		if err := h.validateTargetMetadata(ctx, claim, after, attempt); err != nil {
			return err
		}
	}
	if !runtimeGrantsEqual(oldStatus.Grant, status.Grant) {
		if !runtimeGrantsEqual(claim.Status.RuntimeAdoption.Grant, status.Grant) || oldStatus.TargetMetadataDigest == "" {
			return errors.New("sandbox grant must follow the matching claim grant and target metadata")
		}
		if err := h.validateGrant(ctx, claim, after, attempt, status.Grant); err != nil {
			return err
		}
	}
	if oldStatus.CommitDigest != status.CommitDigest {
		if !runtimeGrantsEqual(claim.Status.RuntimeAdoption.Grant, status.Grant) {
			return errors.New("commit requires matching durable grants")
		}
		return validateAdmissionCommit(attempt, after, status.Grant, status.CommitDigest)
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) validateReservationOrigin(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox) (*sandboxv1beta1.RuntimeAdoptionInitialization, error) {
	status := claim.Status.RuntimeAdoption
	if status == nil {
		return nil, errors.New("claim has no durable attempt")
	}
	reservation := &status.Reservation
	digest, err := runtimeadoption.ClaimIntentDigest(claim)
	if err != nil {
		return nil, err
	}
	if reservation.WireVersion != sandboxv1beta1.RuntimeAdoptionWireVersion || reservation.ClaimUID != claim.UID || reservation.ClaimName != claim.Name ||
		reservation.Namespace != claim.Namespace || reservation.SandboxUID != sandbox.UID || reservation.ClaimGeneration != claim.Generation ||
		reservation.ClaimIntentDigest != digest || reservation.SourceActivationID == reservation.TargetActivationID ||
		!runtimeAdoptionHasFinalizer(claim) || claim.DeletionTimestamp != nil || status.TerminationRequestedTime != nil ||
		!time.Now().Before(reservation.ExpiresAt.Time) || reservation.ExpiresAt.After(time.Now().Add(runtimeAdoptionLifetime)) ||
		len(claim.Spec.Env) != 0 || len(claim.Spec.VolumeClaimTemplates) != 0 ||
		claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] != "" || claim.Labels[extensionsv1beta1.DeprecatedAssignedSandboxNameLabel] != "" {
		return nil, errors.New("reservation does not match the unassigned live claim intent")
	}
	var pool extensionsv1beta1.SandboxWarmPool
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.WarmPoolRef.Name}, &pool); err != nil {
		return nil, err
	}
	if pool.UID != reservation.PoolUID || pool.DeletionTimestamp != nil || pool.Spec.RuntimeAdoption == nil ||
		pool.Spec.RuntimeAdoption.Mode != extensionsv1beta1.SandboxWarmPoolRuntimeAdoptionSamePolicyFirstClaim || !runtimeCandidateForPool(sandbox, &pool) {
		return nil, errors.New("reservation requires an unused member of the live opted-in pool")
	}
	var template extensionsv1beta1.SandboxTemplate
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: pool.Spec.TemplateRef.Name}, &template); err != nil {
		return nil, err
	}
	if template.UID != reservation.TemplateUID || template.DeletionTimestamp != nil || !runtimeBlueprintMatchesTemplate(sandbox, &template) {
		return nil, errors.New("reservation requires the original current template blueprint")
	}
	var namespace corev1.Namespace
	if err := h.Reader.Get(ctx, client.ObjectKey{Name: claim.Namespace}, &namespace); err != nil {
		return nil, err
	}
	if namespace.UID != reservation.NamespaceUID || namespace.DeletionTimestamp != nil {
		return nil, errors.New("reservation namespace identity changed")
	}
	pod, err := runtimeAdoptionPod(ctx, h.Reader, sandbox)
	if err != nil {
		return nil, err
	}
	if pod.Name != status.PodRef.Name || pod.UID != status.PodRef.UID || pod.Spec.NodeName != status.NodeRef.Name {
		return nil, errors.New("reservation Pod locator differs from live execution")
	}
	evidence, err := runtimeadoption.ReadNodeEvidence(ctx, h.Reader, pod, time.Now())
	if err != nil {
		return nil, err
	}
	if !evidence.Qualified() || evidence.Node.UID != status.NodeRef.UID {
		return nil, errors.New("reservation requires qualified evidence from the original Node")
	}
	initialization, origin, err := evidence.Initialization(sandbox, pod)
	if err != nil {
		return nil, err
	}
	metadataDigest, err := runtimeadoption.MetadataDigest(&namespace, sandbox, pod)
	if err != nil || origin.NamespaceUID != string(namespace.UID) || initialization.PoolUID != pool.UID || initialization.TemplateUID != template.UID ||
		initialization.InitializationID != reservation.InitializationID || initialization.SourceActivationID != reservation.SourceActivationID ||
		origin.SourceMetadataDigest != string(metadataDigest) {
		return nil, errors.Join(errors.New("reservation does not match authenticated unused pool origin"), err)
	}
	return initialization, nil
}

func runtimeBlueprintMatchesTemplate(sandbox *sandboxv1beta1.Sandbox, template *extensionsv1beta1.SandboxTemplate) bool {
	actual, expected := sandbox.Spec.SandboxBlueprint.DeepCopy(), template.Spec.SandboxBlueprint.DeepCopy()
	actual.PodTemplate.ObjectMeta, expected.PodTemplate.ObjectMeta = sandboxv1beta1.PodMetadata{}, sandboxv1beta1.PodMetadata{}
	ApplySandboxSecureDefaults(template, &expected.PodTemplate.Spec)
	return reflect.DeepEqual(actual, expected)
}

func (h *RuntimeAdoptionAdmissionHandler) validateGrant(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, attempt *runtimeAdmissionAttempt, grant *sandboxv1beta1.RuntimeAdoptionGrant) error {
	if grant == nil || !attempt.evidence.Qualified() {
		return errors.New("grant requires a qualified runtime")
	}
	if err := h.validateTargetMetadata(ctx, claim, sandbox, attempt); err != nil {
		return err
	}
	context, receipt, receiptDigest, err := attempt.evidence.VerifyTransfer(attempt.observation, sandbox, attempt.pod, true)
	if err != nil {
		return err
	}
	if grant.ContextDigest != sandboxv1beta1.RuntimeAdoptionDigest(attempt.observation.ContextDigest) || grant.ReceiptDigest != receiptDigest || !grant.ExpiresAt.Time.Equal(receipt.ExpiresAt) {
		return errors.New("API grant does not identify the current held transfer receipt and deadline")
	}
	if grant.GrantDigest != "" {
		digest, err := attempt.evidence.VerifyStartGrant(attempt.observation, context, grant, true)
		if err != nil || digest != grant.GrantDigest {
			return errors.Join(errors.New("API grant does not identify the signed runtime grant"), err)
		}
	}
	return nil
}

func validateAdmissionCommit(attempt *runtimeAdmissionAttempt, sandbox *sandboxv1beta1.Sandbox, grant *sandboxv1beta1.RuntimeAdoptionGrant, digest sandboxv1beta1.RuntimeAdoptionDigest) error {
	if grant == nil || grant.GrantDigest == "" {
		return errors.New("commit requires retained start authorization")
	}
	context, _, receiptDigest, err := attempt.evidence.VerifyTransfer(attempt.observation, sandbox, attempt.pod, false)
	if err != nil || grant.ReceiptDigest != receiptDigest {
		return errors.Join(errors.New("commit held receipt changed"), err)
	}
	grantDigest, err := attempt.evidence.VerifyStartGrant(attempt.observation, context, grant, false)
	if err != nil || grant.GrantDigest != grantDigest {
		return errors.Join(errors.New("commit grant changed"), err)
	}
	commit, _, err := attempt.evidence.VerifyCommit(attempt.observation, context, grant)
	if err != nil || commit.ResultDigest != string(digest) {
		return errors.Join(errors.New("commit digest does not match confirmed release evidence"), err)
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) terminalForClaim(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (sandboxv1beta1.RuntimeAdoptionDigest, bool, error) {
	status := claim.Status.RuntimeAdoption
	evidence, err := runtimeadoption.ReadNamedNodeEvidence(ctx, h.Reader, status.NodeRef.Name, time.Now())
	if err != nil {
		return "", false, err
	}
	if evidence.Node.UID != status.NodeRef.UID {
		return "", false, errors.New("terminal observation belongs to a replacement Node")
	}
	observation, err := evidence.Observation(&status.Reservation)
	if err != nil {
		return "", false, err
	}
	digest, err := evidence.VerifyTermination(observation, &status.Reservation)
	if err == nil {
		return digest, false, nil
	}
	digest, err = evidence.VerifyRejection(observation, &status.Reservation)
	return digest, true, err
}

func digestExtends(before, after sandboxv1beta1.RuntimeAdoptionDigest) bool {
	return before == "" || before == after
}

func timeExtends(before, after *metav1.Time) bool {
	return before == nil && (after == nil || !after.IsZero() && !after.After(time.Now())) || before != nil && before.Equal(after)
}

func grantExtends(before, after *sandboxv1beta1.RuntimeAdoptionGrant) bool {
	if before == nil {
		return true
	}
	return after != nil && before.ContextDigest == after.ContextDigest && before.ReceiptDigest == after.ReceiptDigest &&
		before.ExpiresAt.Equal(&after.ExpiresAt) && digestExtends(before.GrantDigest, after.GrantDigest)
}
