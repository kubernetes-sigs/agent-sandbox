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
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/agent-sandbox/internal/runtimeadoption"
	"sigs.k8s.io/agent-sandbox/internal/utils"
)

const (
	runtimeAdoptionRetryInterval = time.Second
	runtimeAdoptionLifetime      = 2 * time.Minute
	RuntimeAdoptionPoolUIDIndex  = ".status.runtimeAdoption.reservation.poolUID"
)

var errRuntimeAdoptionPending = errors.New("runtime adoption is pending")

//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/finalizers,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=namespaces;nodes,verbs=get
//+kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get
//+kubebuilder:rbac:groups=runtime.gatekeeper.sh,resources=runtimepolicynodestatuses,verbs=get;list;watch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get

func (r *SandboxClaimReconciler) getOrCreateRuntimeAdoptionSandbox(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, pool *extensionsv1beta1.SandboxWarmPool) (*sandboxv1beta1.Sandbox, error) {
	reader := r.authoritativeReader()
	fresh := &extensionsv1beta1.SandboxClaim{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(claim), fresh); err != nil {
		return nil, err
	}
	if fresh.UID != claim.UID || fresh.DeletionTimestamp != nil {
		return nil, errors.New("claim changed or was deleted before adoption")
	}
	*claim = *fresh
	if fresh.Status.RuntimeAdoption != nil {
		return nil, errRuntimeAdoptionPending
	}
	if assigned := fresh.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation]; assigned != "" ||
		fresh.Labels[extensionsv1beta1.DeprecatedAssignedSandboxNameLabel] != "" ||
		(fresh.Status.SandboxStatus.Name != "" && fresh.Status.SandboxStatus.Name != fresh.Name) {
		return nil, ErrRuntimeAdoptionUnavailable
	}
	var existing sandboxv1beta1.Sandbox
	if err := reader.Get(ctx, client.ObjectKey{Namespace: fresh.Namespace, Name: fresh.Name}, &existing); err == nil {
		if metav1.IsControlledBy(&existing, fresh) && !hasRuntimeAdoptionData(&existing) &&
			existing.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] == sandboxv1beta1.SandboxLaunchTypeCold {
			return &existing, nil
		}
		return nil, ErrRuntimeAdoptionUnavailable
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if !r.RuntimeAdoptionEnabled || pool.Spec.RuntimeAdoption.Mode != extensionsv1beta1.SandboxWarmPoolRuntimeAdoptionSamePolicyFirstClaim {
		r.runtimeAdoptionFallback(claim, "WarmAdoptionDisabled", "Qualified runtime adoption is disabled; using cold activation")
		return nil, nil
	}
	if err := runtimeadoption.AdmissionReady(ctx, reader, r.RuntimeAdoptionWebhookName); err != nil {
		r.runtimeAdoptionFallback(claim, "WarmAdoptionDisabled", "Protected adoption admission is unavailable; using cold activation")
		return nil, nil
	}
	if len(claim.Spec.Env) != 0 || len(claim.Spec.VolumeClaimTemplates) != 0 {
		r.runtimeAdoptionFallback(claim, "BlueprintMismatch", "Claim configuration requires cold activation")
		return nil, nil
	}
	var authoritativePool extensionsv1beta1.SandboxWarmPool
	if err := reader.Get(ctx, client.ObjectKeyFromObject(pool), &authoritativePool); err != nil {
		return nil, err
	}
	if authoritativePool.UID != pool.UID || authoritativePool.DeletionTimestamp != nil || authoritativePool.Spec.RuntimeAdoption == nil {
		return nil, errors.New("warm pool changed before runtime reservation")
	}
	pool = &authoritativePool
	template := &extensionsv1beta1.SandboxTemplate{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: pool.Spec.TemplateRef.Name}, template); err != nil {
		return nil, fmt.Errorf("strict adoption requires a current template: %w", err)
	}
	var namespace corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: claim.Namespace}, &namespace); err != nil {
		return nil, err
	}
	if namespace.UID == "" || namespace.DeletionTimestamp != nil || template.UID == "" || template.DeletionTimestamp != nil {
		return nil, errors.New("runtime adoption requires live namespace and template identities")
	}
	var candidates sandboxv1beta1.SandboxList
	if err := reader.List(ctx, &candidates, client.InNamespace(pool.Namespace), client.MatchingLabels{warmPoolSandboxLabel: sandboxcontrollers.NameHash(pool.Name)}); err != nil {
		return nil, err
	}
	// Labels locate candidates; every candidate is re-read and matched against
	// live controller references and signed origin before reserving it.
	for i := range candidates.Items {
		candidate := &sandboxv1beta1.Sandbox{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(&candidates.Items[i]), candidate); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if !runtimeCandidateForPool(candidate, pool) {
			continue
		}
		pod, err := runtimeAdoptionPod(ctx, reader, candidate)
		if err != nil {
			continue
		}
		evidence, err := runtimeadoption.ReadNodeEvidence(ctx, reader, pod, time.Now())
		if err != nil || !evidence.Qualified() {
			continue
		}
		initialization, origin, err := evidence.Initialization(candidate, pod)
		if err != nil || origin.NamespaceUID != string(namespace.UID) || initialization.PoolUID != pool.UID || initialization.TemplateUID != template.UID {
			continue
		}
		metadataDigest, err := runtimeadoption.MetadataDigest(&namespace, candidate, pod)
		if err != nil || string(metadataDigest) != origin.SourceMetadataDigest {
			continue
		}
		if err := r.reserveRuntimeAdoption(ctx, claim, candidate, pool, template, &namespace, pod, evidence, initialization); err != nil {
			return nil, err
		}
		return nil, errRuntimeAdoptionPending
	}
	r.runtimeAdoptionFallback(claim, "PoolOriginUnavailable", "No qualified first-claim pool execution is available; using cold activation")
	return nil, nil
}

func runtimeCandidateForPool(sandbox *sandboxv1beta1.Sandbox, pool *extensionsv1beta1.SandboxWarmPool) bool {
	owner := metav1.GetControllerOf(sandbox)
	return sandbox.UID != "" && sandbox.Namespace == pool.Namespace && sandbox.DeletionTimestamp == nil &&
		sandbox.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning &&
		utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind) &&
		owner.Name == pool.Name && owner.UID == pool.UID && !runtimeAdoptionStateProtected(sandbox)
}

func runtimeAdoptionPod(ctx context.Context, reader client.Reader, sandbox *sandboxv1beta1.Sandbox) (*corev1.Pod, error) {
	name := sandbox.Name
	if recorded := sandbox.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]; recorded != "" {
		name = recorded
	}
	var pod corev1.Pod
	if err := reader.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: name}, &pod); err != nil {
		return nil, err
	}
	if pod.UID == "" || !metav1.IsControlledBy(&pod, sandbox) || pod.DeletionTimestamp != nil {
		return nil, errors.New("runtime adoption Pod is missing or has another owner")
	}
	return &pod, nil
}

func (r *SandboxClaimReconciler) reserveRuntimeAdoption(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, pool *extensionsv1beta1.SandboxWarmPool, template *extensionsv1beta1.SandboxTemplate, namespace *corev1.Namespace, pod *corev1.Pod, evidence *runtimeadoption.NodeEvidence, initialization *sandboxv1beta1.RuntimeAdoptionInitialization) error {
	attemptID, err := runtimeadoption.NewID()
	if err != nil {
		return err
	}
	targetID, err := runtimeadoption.NewID()
	if err != nil {
		return err
	}
	intentDigest, err := runtimeadoption.ClaimIntentDigest(claim)
	if err != nil {
		return err
	}
	expires := time.Now().UTC().Add(runtimeAdoptionLifetime).Truncate(time.Second)
	if claim.Spec.Lifecycle != nil && claim.Spec.Lifecycle.ShutdownTime != nil && claim.Spec.Lifecycle.ShutdownTime.Before(&metav1.Time{Time: expires}) {
		expires = claim.Spec.Lifecycle.ShutdownTime.Time
	}
	if sandbox.Spec.ShutdownTime != nil && sandbox.Spec.ShutdownTime.Before(&metav1.Time{Time: expires}) {
		expires = sandbox.Spec.ShutdownTime.Time
	}
	expires = expires.UTC().Truncate(time.Second)
	if !time.Now().Before(expires) {
		return errors.New("claim or Sandbox expired before reservation")
	}
	reservation := sandboxv1beta1.RuntimeAdoptionReservation{
		WireVersion: sandboxv1beta1.RuntimeAdoptionWireVersion, AttemptID: attemptID,
		InitializationID: initialization.InitializationID, SourceActivationID: initialization.SourceActivationID, TargetActivationID: targetID,
		Namespace: namespace.Name, NamespaceUID: namespace.UID, ClaimName: claim.Name, ClaimUID: claim.UID,
		ClaimGeneration: claim.Generation, ClaimIntentDigest: intentDigest, SandboxUID: sandbox.UID, PoolUID: pool.UID, TemplateUID: template.UID,
		ExpiresAt: metav1.NewTime(expires),
	}
	if err := runtimeadoption.SetFinalizer(ctx, r.Client, claim, true); err != nil {
		return err
	}
	status := &extensionsv1beta1.SandboxClaimRuntimeAdoptionStatus{
		SandboxName: sandbox.Name, Reservation: reservation,
		PodRef:  sandboxv1beta1.RuntimeAdoptionObjectReference{Name: pod.Name, UID: pod.UID},
		NodeRef: sandboxv1beta1.RuntimeAdoptionObjectReference{Name: evidence.Node.Name, UID: evidence.Node.UID},
	}
	if err := runtimeadoption.PatchStatus(ctx, r.Client, claim, status); err != nil {
		return fmt.Errorf("persist claim adoption attempt: %w", err)
	}
	return r.acquireRuntimeReservation(ctx, claim, sandbox, initialization)
}

func (r *SandboxClaimReconciler) acquireRuntimeReservation(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, initialization *sandboxv1beta1.RuntimeAdoptionInitialization) error {
	reservation := &claim.Status.RuntimeAdoption.Reservation
	if sandbox.UID != reservation.SandboxUID || sandbox.DeletionTimestamp != nil || runtimeAdoptionStateProtected(sandbox) {
		return errors.New("candidate changed before exclusive reservation")
	}
	if err := runtimeadoption.SetFinalizer(ctx, r.Client, sandbox, true); err != nil {
		return err
	}
	status := &sandboxv1beta1.RuntimeAdoptionStatus{
		Initialization: initialization.DeepCopy(), Reservation: reservation.DeepCopy(),
		Consumed: &sandboxv1beta1.RuntimeAdoptionConsumption{AttemptID: reservation.AttemptID, ClaimUID: claim.UID, ConsumedTime: metav1.Now()},
	}
	if err := runtimeadoption.PatchStatus(ctx, r.Client, sandbox, status); err != nil {
		return fmt.Errorf("acquire Sandbox runtime reservation: %w", err)
	}
	return nil
}

func (r *SandboxClaimReconciler) runtimeAdoptionFallback(claim *extensionsv1beta1.SandboxClaim, reason, message string) {
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{Type: "RuntimeAdoption", Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: claim.Generation})
}

func (r *SandboxClaimReconciler) reconcileRuntimeAdoptionClaim(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (ctrl.Result, error) {
	fresh := &extensionsv1beta1.SandboxClaim{}
	if err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(claim), fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.UID != claim.UID || fresh.Status.RuntimeAdoption == nil {
		return ctrl.Result{}, errors.New("claim adoption identity changed")
	}
	claim = fresh
	oldStatus := claim.Status.DeepCopy()
	sandbox, err := r.advanceRuntimeAdoption(ctx, claim)
	if apierrors.IsNotFound(err) {
		// Missing API objects never prove that an observed runtime effect did
		// not happen. Keep this attempt and its finalizer for node recovery.
		err = fmt.Errorf("retained runtime identity is unavailable: %w", err)
	}
	r.computeAndSetStatus(claim, sandbox, err, false)
	if claim.Status.RuntimeAdoption.TerminalEvidenceDigest != "" {
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionFalse,
			Reason: "RuntimeAdoptionTerminated", Message: "Runtime confirmed termination; the consumed pool member cannot be reused", ObservedGeneration: claim.Generation})
	}
	if _, patchErr := r.updateStatus(ctx, oldStatus, claim); patchErr != nil {
		return ctrl.Result{}, errors.Join(err, patchErr)
	}
	if claim.Status.RuntimeAdoption.TerminalEvidenceDigest != "" && err == nil {
		return ctrl.Result{}, nil
	}
	if errors.Is(err, errRuntimeAdoptionPending) || errors.Is(err, runtimeadoption.ErrPending) {
		return ctrl.Result{RequeueAfter: runtimeAdoptionRetryInterval}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: runtimeAdoptionRetryInterval}, nil
}

func (r *SandboxClaimReconciler) advanceRuntimeAdoption(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (*sandboxv1beta1.Sandbox, error) {
	reader := r.authoritativeReader()
	attempt := claim.Status.RuntimeAdoption
	reservation := &attempt.Reservation
	if reservation.ClaimUID != claim.UID || reservation.ClaimName != claim.Name || reservation.Namespace != claim.Namespace || reservation.WireVersion != sandboxv1beta1.RuntimeAdoptionWireVersion {
		return nil, errors.New("claim reservation identity is invalid")
	}
	evidence, err := runtimeadoption.ReadNamedNodeEvidence(ctx, reader, attempt.NodeRef.Name, time.Now())
	if err != nil {
		return nil, err
	}
	if evidence.Node.UID != attempt.NodeRef.UID {
		return nil, errors.New("runtime adoption Node was replaced")
	}
	observation, observationErr := evidence.Observation(reservation)
	if observationErr != nil && !errors.Is(observationErr, runtimeadoption.ErrPending) {
		return nil, observationErr
	}
	var sandbox sandboxv1beta1.Sandbox
	if err := reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: attempt.SandboxName}, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			if observation != nil && len(observation.Termination) != 0 {
				return nil, r.completeRuntimeClaimTermination(ctx, claim, evidence, observation)
			}
			return nil, r.requestRuntimeTermination(ctx, claim, nil)
		}
		return nil, err
	}
	if sandbox.UID != reservation.SandboxUID {
		if observation != nil && len(observation.Termination) != 0 {
			return nil, r.completeRuntimeClaimTermination(ctx, claim, evidence, observation)
		}
		return nil, r.requestRuntimeTermination(ctx, claim, nil)
	}
	if observation != nil && observation.Phase == "Rejected" && len(observation.Termination) != 0 {
		if sandbox.Status.RuntimeAdoption != nil && sandbox.Status.RuntimeAdoption.Reservation != nil &&
			sandbox.Status.RuntimeAdoption.Reservation.AttemptID == reservation.AttemptID {
			return nil, errors.New("runtime rejected an attempt that has a Sandbox reservation")
		}
		return nil, r.completeRuntimeClaimTermination(ctx, claim, evidence, observation)
	}
	if sandbox.Status.RuntimeAdoption == nil || sandbox.Status.RuntimeAdoption.Reservation == nil {
		return nil, r.recoverUnacquiredRuntimeReservation(ctx, claim, &sandbox, evidence)
	}
	if !reflect.DeepEqual(*sandbox.Status.RuntimeAdoption.Reservation, *reservation) {
		return nil, r.requestRuntimeTermination(ctx, claim, nil)
	}
	if observation != nil && len(observation.Termination) != 0 {
		return nil, r.completeRuntimeTermination(ctx, claim, &sandbox, evidence, observation)
	}
	var namespace corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: claim.Namespace}, &namespace); err != nil {
		return nil, err
	}
	intentDigest, err := runtimeadoption.ClaimIntentDigest(claim)
	if err != nil {
		return nil, err
	}
	expired, _ := r.checkExpiration(claim)
	sandboxExpired := sandbox.Spec.ShutdownTime != nil && !time.Now().Before(sandbox.Spec.ShutdownTime.Time)
	cancel := claim.DeletionTimestamp != nil || sandbox.DeletionTimestamp != nil || namespace.DeletionTimestamp != nil || expired || sandboxExpired ||
		namespace.UID != reservation.NamespaceUID || claim.Generation != reservation.ClaimGeneration || intentDigest != reservation.ClaimIntentDigest ||
		attempt.TerminationRequestedTime != nil || sandbox.Status.RuntimeAdoption.TerminationRequestedTime != nil ||
		sandbox.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning
	if cancel {
		return nil, r.requestRuntimeTermination(ctx, claim, &sandbox)
	}
	pod, err := runtimeAdoptionPod(ctx, reader, &sandbox)
	if err != nil {
		if requestErr := r.requestRuntimeTermination(ctx, claim, &sandbox); requestErr != nil && !errors.Is(requestErr, errRuntimeAdoptionPending) {
			return nil, errors.Join(err, requestErr)
		}
		return nil, errRuntimeAdoptionPending
	}
	if pod.UID != attempt.PodRef.UID || pod.Name != attempt.PodRef.Name || pod.Spec.NodeName != attempt.NodeRef.Name {
		return nil, r.requestRuntimeTermination(ctx, claim, &sandbox)
	}
	if observation != nil && len(observation.Commit) != 0 {
		return r.completeRuntimeCommit(ctx, claim, &sandbox, pod, &namespace, evidence, observation)
	}
	if attempt.CommitDigest != "" || sandbox.Status.RuntimeAdoption.CommitDigest != "" {
		return nil, errRuntimeAdoptionPending
	}
	if !time.Now().Before(reservation.ExpiresAt.Time) || !evidence.Qualified() || !r.RuntimeAdoptionEnabled {
		return nil, r.requestRuntimeTermination(ctx, claim, &sandbox)
	}
	if observation == nil || len(observation.Hold) == 0 {
		return nil, errRuntimeAdoptionPending
	}
	_, holdDigest, err := evidence.VerifyHold(observation, reservation, sandbox.Status.RuntimeAdoption.Initialization, pod, true)
	if err != nil {
		return nil, err
	}
	if sandbox.Status.RuntimeAdoption.HoldEvidenceDigest == "" {
		status := sandbox.Status.RuntimeAdoption.DeepCopy()
		status.HoldEvidenceDigest = holdDigest
		if err := runtimeadoption.PatchStatus(ctx, r.Client, &sandbox, status); err != nil {
			return nil, err
		}
		return nil, errRuntimeAdoptionPending
	}
	if sandbox.Status.RuntimeAdoption.HoldEvidenceDigest != holdDigest {
		return nil, errors.New("runtime changed the reserved hold evidence")
	}
	if !metav1.IsControlledBy(&sandbox, claim) {
		if err := r.prepareRuntimeAdoptionTarget(ctx, claim, &sandbox); err != nil {
			return nil, err
		}
		return nil, errRuntimeAdoptionPending
	}
	if !sandboxcontrollers.SandboxPodMetadataMatches(ctx, &sandbox, pod) {
		return nil, errRuntimeAdoptionPending
	}
	metadataDigest, err := runtimeadoption.MetadataDigest(&namespace, &sandbox, pod)
	if err != nil {
		return nil, err
	}
	if sandbox.Status.RuntimeAdoption.TargetMetadataDigest == "" {
		status := sandbox.Status.RuntimeAdoption.DeepCopy()
		status.TargetMetadataDigest = metadataDigest
		if err := runtimeadoption.PatchStatus(ctx, r.Client, &sandbox, status); err != nil {
			return nil, err
		}
		return nil, errRuntimeAdoptionPending
	}
	if sandbox.Status.RuntimeAdoption.TargetMetadataDigest != metadataDigest {
		return nil, r.requestRuntimeTermination(ctx, claim, &sandbox)
	}
	if len(observation.Receipt) == 0 {
		return nil, errRuntimeAdoptionPending
	}
	context, receipt, receiptDigest, err := evidence.VerifyTransfer(observation, &sandbox, pod, true)
	if err != nil {
		return nil, err
	}
	grant := &sandboxv1beta1.RuntimeAdoptionGrant{ContextDigest: sandboxv1beta1.RuntimeAdoptionDigest(observation.ContextDigest),
		ReceiptDigest: receiptDigest, ExpiresAt: metav1.NewTime(receipt.ExpiresAt.UTC().Truncate(time.Second))}
	if attempt.Grant == nil || sandbox.Status.RuntimeAdoption.Grant == nil {
		if err := r.persistRuntimeGrant(ctx, claim, &sandbox, grant); err != nil {
			return nil, err
		}
		return nil, errRuntimeAdoptionPending
	}
	grant.GrantDigest = attempt.Grant.GrantDigest
	if !runtimeGrantsEqual(attempt.Grant, grant) {
		return nil, errors.New("held receipt changed after API grant authorization")
	}
	if len(observation.StartGrant) != 0 {
		grantDigest, err := evidence.VerifyStartGrant(observation, context, grant, true)
		if err != nil {
			return nil, err
		}
		if grant.GrantDigest != "" && grant.GrantDigest != grantDigest {
			return nil, errors.New("runtime changed the authorized start grant")
		}
		grant.GrantDigest = grantDigest
		if err := r.persistRuntimeGrant(ctx, claim, &sandbox, grant); err != nil {
			return nil, err
		}
	}
	return nil, errRuntimeAdoptionPending
}

func (r *SandboxClaimReconciler) recoverUnacquiredRuntimeReservation(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, evidence *runtimeadoption.NodeEvidence) error {
	reservation := &claim.Status.RuntimeAdoption.Reservation
	if claim.DeletionTimestamp != nil || !time.Now().Before(reservation.ExpiresAt.Time) || claim.Status.RuntimeAdoption.TerminationRequestedTime != nil {
		return r.requestRuntimeTermination(ctx, claim, nil)
	}
	var pool extensionsv1beta1.SandboxWarmPool
	if err := r.authoritativeReader().Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.WarmPoolRef.Name}, &pool); err != nil {
		return err
	}
	if pool.UID != reservation.PoolUID || !runtimeCandidateForPool(sandbox, &pool) {
		return r.requestRuntimeTermination(ctx, claim, nil)
	}
	pod, err := runtimeAdoptionPod(ctx, r.authoritativeReader(), sandbox)
	if err != nil {
		return err
	}
	initialization, _, err := evidence.Initialization(sandbox, pod)
	if err != nil {
		return err
	}
	if initialization.InitializationID != reservation.InitializationID || initialization.SourceActivationID != reservation.SourceActivationID ||
		initialization.TemplateUID != reservation.TemplateUID || initialization.PoolUID != reservation.PoolUID || pod.UID != claim.Status.RuntimeAdoption.PodRef.UID {
		return errors.New("candidate initialization changed during reservation recovery")
	}
	if err := r.acquireRuntimeReservation(ctx, claim, sandbox, initialization); err != nil {
		return err
	}
	return errRuntimeAdoptionPending
}

func (r *SandboxClaimReconciler) persistRuntimeGrant(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, grant *sandboxv1beta1.RuntimeAdoptionGrant) error {
	if !runtimeGrantsEqual(claim.Status.RuntimeAdoption.Grant, grant) {
		status := claim.Status.RuntimeAdoption.DeepCopy()
		status.Grant = grant.DeepCopy()
		if err := runtimeadoption.PatchStatus(ctx, r.Client, claim, status); err != nil {
			return err
		}
	}
	if !runtimeGrantsEqual(sandbox.Status.RuntimeAdoption.Grant, grant) {
		status := sandbox.Status.RuntimeAdoption.DeepCopy()
		status.Grant = grant.DeepCopy()
		if err := runtimeadoption.PatchStatus(ctx, r.Client, sandbox, status); err != nil {
			return err
		}
	}
	return nil
}

func (r *SandboxClaimReconciler) requestRuntimeTermination(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox) error {
	when := claim.Status.RuntimeAdoption.TerminationRequestedTime
	if when == nil {
		now := metav1.Now()
		when = &now
		status := claim.Status.RuntimeAdoption.DeepCopy()
		status.TerminationRequestedTime = when
		if err := runtimeadoption.PatchStatus(ctx, r.Client, claim, status); err != nil {
			return err
		}
	}
	if sandbox != nil && sandbox.Status.RuntimeAdoption != nil && sandbox.Status.RuntimeAdoption.TerminationRequestedTime == nil {
		status := sandbox.Status.RuntimeAdoption.DeepCopy()
		status.TerminationRequestedTime = when.DeepCopy()
		if err := runtimeadoption.PatchStatus(ctx, r.Client, sandbox, status); err != nil {
			return err
		}
	}
	return errRuntimeAdoptionPending
}

func (r *SandboxClaimReconciler) completeRuntimeTermination(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, evidence *runtimeadoption.NodeEvidence, observation *runtimeadoption.Observation) error {
	digest, err := evidence.VerifyTermination(observation, &claim.Status.RuntimeAdoption.Reservation)
	if err != nil {
		return err
	}
	if sandbox.Status.RuntimeAdoption.TerminalEvidenceDigest != digest {
		status := sandbox.Status.RuntimeAdoption.DeepCopy()
		status.TerminalEvidenceDigest = digest
		if err := runtimeadoption.PatchStatus(ctx, r.Client, sandbox, status); err != nil {
			return err
		}
	}
	if claim.Status.RuntimeAdoption.TerminalEvidenceDigest != digest {
		status := claim.Status.RuntimeAdoption.DeepCopy()
		status.TerminalEvidenceDigest = digest
		if err := runtimeadoption.PatchStatus(ctx, r.Client, claim, status); err != nil {
			return err
		}
	}
	if err := runtimeadoption.SetFinalizer(ctx, r.Client, sandbox, false); err != nil {
		return err
	}
	if sandbox.DeletionTimestamp == nil {
		if err := r.Delete(ctx, sandbox, client.Preconditions{UID: &sandbox.UID, ResourceVersion: &sandbox.ResourceVersion}); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return runtimeadoption.SetFinalizer(ctx, r.Client, claim, false)
}

func (r *SandboxClaimReconciler) completeRuntimeClaimTermination(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, evidence *runtimeadoption.NodeEvidence, observation *runtimeadoption.Observation) error {
	digest, err := evidence.VerifyTermination(observation, &claim.Status.RuntimeAdoption.Reservation)
	unacquired := err != nil
	if err != nil {
		digest, err = evidence.VerifyRejection(observation, &claim.Status.RuntimeAdoption.Reservation)
	}
	if err != nil {
		return err
	}
	if claim.Status.RuntimeAdoption.TerminalEvidenceDigest != digest {
		status := claim.Status.RuntimeAdoption.DeepCopy()
		status.TerminalEvidenceDigest = digest
		if err := runtimeadoption.PatchStatus(ctx, r.Client, claim, status); err != nil {
			return err
		}
	}
	if unacquired {
		// A crash can leave only the Sandbox finalizer, before the exclusive
		// reservation write. A winning reservation changes the version and
		// prevents this cleanup from removing its protection.
		var sandbox sandboxv1beta1.Sandbox
		if err := r.authoritativeReader().Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Status.RuntimeAdoption.SandboxName}, &sandbox); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
		} else if sandbox.UID == claim.Status.RuntimeAdoption.Reservation.SandboxUID && !runtimeAdoptionStateProtected(&sandbox) {
			if err := runtimeadoption.SetFinalizer(ctx, r.Client, &sandbox, false); err != nil {
				return err
			}
		}
	}
	return runtimeadoption.SetFinalizer(ctx, r.Client, claim, false)
}

func (r *SandboxClaimReconciler) recoverEmptyRuntimePreparation(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) error {
	if claim.Status.RuntimeAdoption != nil || !runtimeAdoptionHasFinalizer(claim) {
		return nil
	}
	var fresh extensionsv1beta1.SandboxClaim
	if err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(claim), &fresh); err != nil {
		return err
	}
	if fresh.UID != claim.UID {
		return errors.New("claim changed during runtime preparation recovery")
	}
	if fresh.Status.RuntimeAdoption == nil {
		// The claim attempt write also carries UID and resourceVersion. It
		// cannot land after this removal using a previously admitted version.
		if err := runtimeadoption.SetFinalizer(ctx, r.Client, &fresh, false); err != nil {
			return err
		}
	}
	*claim = fresh
	return nil
}

func (r *SandboxClaimReconciler) completeRuntimeCommit(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod, namespace *corev1.Namespace, evidence *runtimeadoption.NodeEvidence, observation *runtimeadoption.Observation) (*sandboxv1beta1.Sandbox, error) {
	grant := claim.Status.RuntimeAdoption.Grant
	if grant == nil || grant.GrantDigest == "" || !runtimeGrantsEqual(grant, sandbox.Status.RuntimeAdoption.Grant) || !metav1.IsControlledBy(sandbox, claim) {
		return nil, errors.New("commit has no matching durable grant and owner")
	}
	context, _, receiptDigest, err := evidence.VerifyTransfer(observation, sandbox, pod, false)
	if err != nil || receiptDigest != grant.ReceiptDigest {
		return nil, errors.Join(errors.New("commit has no matching held receipt"), err)
	}
	grantDigest, err := evidence.VerifyStartGrant(observation, context, grant, false)
	if err != nil || grantDigest != grant.GrantDigest {
		return nil, errors.Join(errors.New("commit has no matching signed grant"), err)
	}
	commit, _, err := evidence.VerifyCommit(observation, context, grant)
	if err != nil {
		return nil, err
	}
	metadataDigest, err := runtimeadoption.MetadataDigest(namespace, sandbox, pod)
	if err != nil || metadataDigest != sandbox.Status.RuntimeAdoption.TargetMetadataDigest {
		return nil, r.requestRuntimeTermination(ctx, claim, sandbox)
	}
	digest := sandboxv1beta1.RuntimeAdoptionDigest(commit.ResultDigest)
	if sandbox.Status.RuntimeAdoption.CommitDigest != "" && sandbox.Status.RuntimeAdoption.CommitDigest != digest {
		return nil, errors.New("runtime changed the confirmed commit identity")
	}
	if sandbox.Status.RuntimeAdoption.CommitDigest == "" {
		status := sandbox.Status.RuntimeAdoption.DeepCopy()
		status.CommitDigest = digest
		if err := runtimeadoption.PatchStatus(ctx, r.Client, sandbox, status); err != nil {
			return nil, err
		}
	}
	if claim.Status.RuntimeAdoption.CommitDigest != "" && claim.Status.RuntimeAdoption.CommitDigest != digest {
		return nil, errors.New("claim already records another commit")
	}
	if claim.Status.RuntimeAdoption.CommitDigest == "" {
		status := claim.Status.RuntimeAdoption.DeepCopy()
		status.CommitDigest = digest
		if err := runtimeadoption.PatchStatus(ctx, r.Client, claim, status); err != nil {
			return nil, err
		}
		log.FromContext(ctx).Info("Runtime adoption committed", "claim", claim.Name, "sandbox", sandbox.Name, "attemptID", status.Reservation.AttemptID)
	}
	verification := sandbox.Status.RuntimeActivationVerification
	if !runtimeAdoptionVerified(claim, sandbox, time.Now()) || verification.PodUID != pod.UID || verification.NodeUID != evidence.Node.UID ||
		verification.ContainerID != strings.TrimPrefix(context.Execution.ContainerID, "containerd://") || uint64(verification.TaskStartTime) != context.Execution.TaskStartTime ||
		verification.RuntimeIncarnation != evidence.Status.RuntimeIncarnation || uint64(verification.PolicyEpoch) != context.PolicyEpoch ||
		len(pod.Status.ContainerStatuses) != 1 || strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "containerd://") != verification.ContainerID {
		return sandbox, errRuntimeAdoptionPending
	}
	return sandbox, nil
}

func runtimeAdoptionVerified(claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, now time.Time) bool {
	if claim == nil || sandbox == nil || claim.Status.RuntimeAdoption == nil || sandbox.Status.RuntimeAdoption == nil || sandbox.Status.RuntimeActivationVerification == nil {
		return false
	}
	claimStatus, status, verification := claim.Status.RuntimeAdoption, sandbox.Status.RuntimeAdoption, sandbox.Status.RuntimeActivationVerification
	reservation := &claimStatus.Reservation
	intentDigest, err := runtimeadoption.ClaimIntentDigest(claim)
	return err == nil && reservation.ClaimUID == claim.UID && reservation.ClaimName == claim.Name && reservation.ClaimGeneration == claim.Generation &&
		reservation.ClaimIntentDigest == intentDigest && reservation.SandboxUID == sandbox.UID && reservation.Namespace == claim.Namespace &&
		reflect.DeepEqual(status.Reservation, reservation) && metav1.IsControlledBy(sandbox, claim) &&
		claimStatus.TerminationRequestedTime == nil && status.TerminationRequestedTime == nil && status.TerminalEvidenceDigest == "" &&
		claimStatus.CommitDigest != "" && claimStatus.CommitDigest == status.CommitDigest && status.CommitDigest == verification.CommitDigest &&
		claimStatus.Grant != nil && status.Grant != nil && runtimeGrantsEqual(claimStatus.Grant, status.Grant) && status.Grant.GrantDigest != "" &&
		verification.AttemptID == reservation.AttemptID && verification.ClaimUID == claim.UID && verification.TargetActivationID == reservation.TargetActivationID &&
		verification.ContextDigest == status.Grant.ContextDigest && verification.ReceiptDigest != "" && verification.PodUID == claimStatus.PodRef.UID &&
		verification.NodeUID == claimStatus.NodeRef.UID && verification.ContainerID != "" && verification.TaskStartTime > 0 && verification.RuntimeIncarnation != "" &&
		verification.ValidUntil.After(now) && meta.IsStatusConditionTrue(verification.Conditions, "Verified") &&
		claim.DeletionTimestamp == nil && sandbox.DeletionTimestamp == nil
}

func runtimeGrantsEqual(before, after *sandboxv1beta1.RuntimeAdoptionGrant) bool {
	if before == nil || after == nil {
		return before == after
	}
	// API timestamps decode in the process's local timezone. Equality must
	// compare the instant rather than the time.Location pointer.
	return before.ContextDigest == after.ContextDigest && before.ReceiptDigest == after.ReceiptDigest &&
		before.GrantDigest == after.GrantDigest && before.ExpiresAt.Equal(&after.ExpiresAt)
}

func (r *SandboxClaimReconciler) prepareRuntimeAdoptionTarget(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox) error {
	before := sandbox.DeepCopy()
	if err := r.PrepareRuntimeAdoptionTarget(ctx, claim, sandbox); err != nil {
		return err
	}
	return r.Patch(ctx, sandbox, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// PrepareRuntimeAdoptionTarget is shared with admission so an old controller
// cannot submit a different ownership or metadata transformation under the same
// service-account identity. It changes no running workload specification.
func (r *SandboxClaimReconciler) PrepareRuntimeAdoptionTarget(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox) error {
	status := sandbox.Status.RuntimeAdoption
	if status == nil || status.Reservation == nil || status.HoldEvidenceDigest == "" || status.TerminationRequestedTime != nil ||
		!reflect.DeepEqual(*status.Reservation, claim.Status.RuntimeAdoption.Reservation) {
		return errors.New("target metadata requires the matching verified hold")
	}
	var pool extensionsv1beta1.SandboxWarmPool
	if err := r.authoritativeReader().Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.WarmPoolRef.Name}, &pool); err != nil {
		return err
	}
	if pool.UID != status.Reservation.PoolUID || pool.DeletionTimestamp != nil {
		return errors.New("pool identity changed before target preparation")
	}
	var template extensionsv1beta1.SandboxTemplate
	if err := r.authoritativeReader().Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: pool.Spec.TemplateRef.Name}, &template); err != nil {
		return err
	}
	if template.UID != status.Reservation.TemplateUID || template.DeletionTimestamp != nil || len(claim.Spec.Env) != 0 || len(claim.Spec.VolumeClaimTemplates) != 0 {
		return errors.New("strict template identity or claim blueprint changed")
	}
	if err := r.validateAdditionalPodMetadata(&claim.Spec.AdditionalPodMetadata); err != nil {
		return err
	}
	var metadata sandboxv1beta1.PodMetadata
	template.Spec.PodTemplate.ObjectMeta.DeepCopyInto(&metadata)
	templateHash := SandboxTemplateRefHash(template.Name)
	metadata.Labels = ensureClaimIdentityLabels(metadata.Labels, claim)
	metadata.Labels[sandboxTemplateRefHash] = templateHash
	if err := r.mergePodMetadata(&metadata, &claim.Spec.AdditionalPodMetadata); err != nil {
		return err
	}
	sandbox.Spec.PodTemplate.ObjectMeta = metadata
	sandbox.Labels = ensureClaimIdentityLabels(sandbox.Labels, claim)
	for _, key := range []string{warmPoolSandboxLabel, sandboxv1beta1.DeprecatedSandboxPodTemplateHashLabel, sandboxv1beta1.SandboxTemplateHashLabel} {
		delete(sandbox.Labels, key)
	}
	sandbox.Labels[sandboxTemplateRefHash] = templateHash
	sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	if trace, ok := claim.Annotations[asmetrics.TraceContextAnnotation]; ok {
		if sandbox.Annotations == nil {
			sandbox.Annotations = map[string]string{}
		}
		sandbox.Annotations[asmetrics.TraceContextAnnotation] = trace
	}
	sandbox.OwnerReferences = nil
	return controllerutil.SetControllerReference(claim, sandbox, r.Scheme)
}

func strictRuntimeSandbox(ctx context.Context, reader client.Reader, sandbox *sandboxv1beta1.Sandbox) (bool, error) {
	if sandbox.Spec.PodTemplate.Spec.RuntimeClassName == nil {
		return false, nil
	}
	if *sandbox.Spec.PodTemplate.Spec.RuntimeClassName == runtimeadoption.StrictHandler {
		return true, nil
	}
	var runtimeClass nodev1.RuntimeClass
	if err := reader.Get(ctx, client.ObjectKey{Name: *sandbox.Spec.PodTemplate.Spec.RuntimeClassName}, &runtimeClass); err != nil {
		return false, err
	}
	return runtimeClass.Handler == runtimeadoption.StrictHandler, nil
}

func runtimeAdoptionPoolUIDIndexer(object client.Object) []string {
	sandbox, ok := object.(*sandboxv1beta1.Sandbox)
	if !ok || sandbox.Status.RuntimeAdoption == nil || sandbox.Status.RuntimeAdoption.Reservation == nil {
		return nil
	}
	return []string{string(sandbox.Status.RuntimeAdoption.Reservation.PoolUID)}
}

func (r *SandboxClaimReconciler) mapReservedSandboxToClaim(_ context.Context, object client.Object) []ctrl.Request {
	sandbox, ok := object.(*sandboxv1beta1.Sandbox)
	if !ok || sandbox.Status.RuntimeAdoption == nil || sandbox.Status.RuntimeAdoption.Reservation == nil {
		return nil
	}
	reservation := sandbox.Status.RuntimeAdoption.Reservation
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: reservation.Namespace, Name: reservation.ClaimName}}}
}

func runtimeAdoptionHasFinalizer(object client.Object) bool {
	return slices.Contains(object.GetFinalizers(), sandboxv1beta1.RuntimeAdoptionFinalizer)
}

func (r *SandboxWarmPoolReconciler) mapReservedSandboxToPool(ctx context.Context, object client.Object) []ctrl.Request {
	sandbox, ok := object.(*sandboxv1beta1.Sandbox)
	if !ok || sandbox.Status.RuntimeAdoption == nil || sandbox.Status.RuntimeAdoption.Reservation == nil {
		return nil
	}
	var pools extensionsv1beta1.SandboxWarmPoolList
	if err := r.List(ctx, &pools, client.InNamespace(sandbox.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "Unable to locate reserved Sandbox pool")
		return nil
	}
	for _, pool := range pools.Items {
		if pool.UID == sandbox.Status.RuntimeAdoption.Reservation.PoolUID {
			return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(&pool)}}
		}
	}
	return nil
}
