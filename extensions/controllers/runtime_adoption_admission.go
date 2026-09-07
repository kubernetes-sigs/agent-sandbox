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
	"net/http"
	"reflect"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/runtimeadoption"
	"sigs.k8s.io/agent-sandbox/internal/utils"
)

// RuntimeAdoptionAdmissionOptions identifies the installed platform writers.
// Tenant opt-in remains SandboxWarmPool.spec.runtimeAdoption.
type RuntimeAdoptionAdmissionOptions struct {
	ControllerUsername        string
	RuntimeControllerUsername string
	AgentNamespace            string
	AllowedLabelDomains       []string
}

// RuntimeAdoptionAdmissionHandler protects ownership independently of the
// reconciler version. Sharing a service account does not let an older binary
// clear an attempt, overwrite held metadata, or publish pool-era readiness.
type RuntimeAdoptionAdmissionHandler struct {
	Reader  client.Reader
	Decoder admission.Decoder
	Scheme  *runtime.Scheme
	RuntimeAdoptionAdmissionOptions
}

func RegisterRuntimeAdoptionAdmission(mgr ctrl.Manager, options RuntimeAdoptionAdmissionOptions) error {
	if options.ControllerUsername == "" || options.RuntimeControllerUsername == "" || options.AgentNamespace == "" ||
		options.ControllerUsername == options.RuntimeControllerUsername {
		return errors.New("runtime adoption requires distinct configured controller identities and an agent namespace")
	}
	mgr.GetWebhookServer().Register(runtimeadoption.AdmissionPath, &admission.Webhook{Handler: &RuntimeAdoptionAdmissionHandler{
		Reader: mgr.GetAPIReader(), Decoder: admission.NewDecoder(mgr.GetScheme()), Scheme: mgr.GetScheme(),
		RuntimeAdoptionAdmissionOptions: options,
	}})
	return mgr.AddReadyzCheck("runtime-adoption-admission", mgr.GetWebhookServer().StartedChecker())
}

func (h *RuntimeAdoptionAdmissionHandler) Handle(ctx context.Context, request admission.Request) admission.Response {
	if h.Reader == nil || h.Decoder == nil || h.Scheme == nil {
		return admission.Errored(http.StatusInternalServerError, errors.New("runtime adoption admission is not initialized"))
	}
	var err error
	switch request.Resource.Group + "/" + request.Resource.Resource {
	case "agents.x-k8s.io/sandboxes":
		var before, after sandboxv1beta1.Sandbox
		if err = h.decodeObjects(request, &before, &after); err == nil {
			err = h.validateSandbox(ctx, request, &before, &after)
		}
	case "extensions.agents.x-k8s.io/sandboxclaims":
		var before, after extensionsv1beta1.SandboxClaim
		if err = h.decodeObjects(request, &before, &after); err == nil {
			err = h.validateClaim(ctx, request, &before, &after)
		}
	case "/pods":
		err = h.validatePodRequest(ctx, request)
	case "runtime.gatekeeper.sh/runtimepolicynodestatuses":
		err = h.validateNodeStatusRequest(ctx, request)
	default:
		err = errors.New("unsupported runtime adoption admission resource")
	}
	if err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("runtime adoption ownership and writer checks passed")
}

func (h *RuntimeAdoptionAdmissionHandler) decodeObjects(request admission.Request, before, after runtime.Object) error {
	if request.Operation != admissionv1.Create {
		if err := h.Decoder.DecodeRaw(request.OldObject, before); err != nil {
			return fmt.Errorf("decode previous object: %w", err)
		}
	}
	if request.Operation == admissionv1.Delete {
		return h.Decoder.DecodeRaw(request.OldObject, after)
	}
	return h.Decoder.Decode(request, after)
}

func (h *RuntimeAdoptionAdmissionHandler) validateFinalizer(request admission.Request, before, after client.Object, protected bool, terminal sandboxv1beta1.RuntimeAdoptionDigest) error {
	oldFinalizer, newFinalizer := runtimeAdoptionHasFinalizer(before), runtimeAdoptionHasFinalizer(after)
	if request.Operation == admissionv1.Delete {
		if protected && !oldFinalizer && terminal == "" {
			return errors.New("uncertain runtime ownership must retain its protection finalizer")
		}
		return nil
	}
	if oldFinalizer != newFinalizer {
		if request.UserInfo.Username != h.ControllerUsername {
			return errors.New("only the companion controller may change runtime adoption finalizers")
		}
		if oldFinalizer && protected && terminal == "" {
			return errors.New("runtime adoption finalization requires a retained terminal observation")
		}
	}
	if protected && !newFinalizer && terminal == "" {
		return errors.New("runtime adoption requires a protection finalizer")
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) validateClaim(ctx context.Context, request admission.Request, before, after *extensionsv1beta1.SandboxClaim) error {
	terminal := sandboxv1beta1.RuntimeAdoptionDigest("")
	if before.Status.RuntimeAdoption != nil {
		terminal = before.Status.RuntimeAdoption.TerminalEvidenceDigest
	}
	if err := h.validateFinalizer(request, before, after, before.Status.RuntimeAdoption != nil || after.Status.RuntimeAdoption != nil, terminal); err != nil {
		return err
	}
	if request.Operation == admissionv1.Delete {
		return nil
	}
	if !reflect.DeepEqual(before.Status.RuntimeAdoption, after.Status.RuntimeAdoption) {
		if request.UserInfo.Username != h.ControllerUsername || request.SubResource != "status" || request.Operation != admissionv1.Update {
			return errors.New("claim runtime adoption status is owned by the companion controller")
		}
		if err := h.validateClaimRuntimeStatus(ctx, before, after); err != nil {
			return err
		}
	}
	if after.Status.RuntimeAdoption != nil && adoptionReadyChanged(before.Status.Conditions, after.Status.Conditions) {
		var sandbox sandboxv1beta1.Sandbox
		if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: after.Namespace, Name: after.Status.RuntimeAdoption.SandboxName}, &sandbox); err != nil {
			return err
		}
		if !runtimeAdoptionVerified(after, &sandbox, time.Now()) {
			return errors.New("claim Ready requires current verification for this exact attempt")
		}
	}
	if status := after.Status.RuntimeAdoption; status != nil &&
		(after.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] != "" || after.Labels[extensionsv1beta1.DeprecatedAssignedSandboxNameLabel] != "" ||
			after.Status.SandboxStatus.Name != "" && after.Status.SandboxStatus.Name != status.SandboxName) {
		return errors.New("a reserved claim cannot use legacy assignment or identify another Sandbox")
	}
	if after.Status.RuntimeAdoption == nil {
		for _, name := range []string{after.Status.SandboxStatus.Name, after.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation], after.Labels[extensionsv1beta1.DeprecatedAssignedSandboxNameLabel]} {
			if name == "" {
				continue
			}
			var sandbox sandboxv1beta1.Sandbox
			if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: after.Namespace, Name: name}, &sandbox); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return err
			}
			strict, err := strictRuntimeSandbox(ctx, h.Reader, &sandbox)
			if err != nil {
				return err
			}
			if hasRuntimeAdoptionData(&sandbox) || strict && (sandbox.Name != after.Name || !metav1.IsControlledBy(&sandbox, after) || sandbox.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] != sandboxv1beta1.SandboxLaunchTypeCold) {
				return errors.New("legacy claim assignment cannot adopt a strict pool execution")
			}
		}
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) validateSandbox(ctx context.Context, request admission.Request, before, after *sandboxv1beta1.Sandbox) error {
	terminal := sandboxv1beta1.RuntimeAdoptionDigest("")
	if before.Status.RuntimeAdoption != nil {
		terminal = before.Status.RuntimeAdoption.TerminalEvidenceDigest
	}
	protected := runtimeAdoptionStateProtected(before) || runtimeAdoptionStateProtected(after)
	if err := h.validateFinalizer(request, before, after, protected, terminal); err != nil {
		return err
	}
	if request.Operation == admissionv1.Delete {
		return nil
	}
	if !reflect.DeepEqual(before.Status.RuntimeAdoption, after.Status.RuntimeAdoption) {
		if request.UserInfo.Username != h.ControllerUsername || request.SubResource != "status" || request.Operation != admissionv1.Update {
			return errors.New("sandbox runtime adoption status is owned by the companion controller")
		}
		if err := h.validateSandboxRuntimeStatus(ctx, before, after); err != nil {
			return err
		}
	}
	if !reflect.DeepEqual(before.Status.RuntimeActivationVerification, after.Status.RuntimeActivationVerification) {
		if request.UserInfo.Username != h.RuntimeControllerUsername || request.SubResource != "status" || request.Operation != admissionv1.Update {
			return errors.New("runtime activation verification is owned by the runtime readiness controller")
		}
		if verification := after.Status.RuntimeActivationVerification; verification != nil && meta.IsStatusConditionTrue(verification.Conditions, "Verified") {
			claim, err := h.claimForSandbox(ctx, after)
			if err != nil {
				return err
			}
			if !runtimeAdoptionVerified(claim, after, time.Now()) {
				return errors.New("runtime verification does not match the current committed claim")
			}
		}
	}
	if protected && adoptionReadyChanged(before.Status.Conditions, after.Status.Conditions) {
		claim, err := h.claimForSandbox(ctx, after)
		if err != nil {
			return err
		}
		if !runtimeAdoptionVerified(claim, after, time.Now()) {
			return errors.New("sandbox Ready cannot reuse pre-adoption readiness")
		}
	}
	if request.SubResource == "status" {
		return nil
	}
	if before.Status.RuntimeAdoption != nil && before.Status.RuntimeAdoption.Reservation != nil {
		// Workload blueprints remain immutable for the original task's life.
		oldBlueprint, newBlueprint := before.Spec.SandboxBlueprint.DeepCopy(), after.Spec.SandboxBlueprint.DeepCopy()
		oldBlueprint.PodTemplate.ObjectMeta, newBlueprint.PodTemplate.ObjectMeta = sandboxv1beta1.PodMetadata{}, sandboxv1beta1.PodMetadata{}
		if !reflect.DeepEqual(oldBlueprint, newBlueprint) {
			return errors.New("a reserved runtime blueprint cannot be changed")
		}
		if !sameAdmissionMetadata(before, after) || !reflect.DeepEqual(before.Spec.PodTemplate.ObjectMeta, after.Spec.PodTemplate.ObjectMeta) {
			if request.UserInfo.Username != h.ControllerUsername || before.Status.RuntimeAdoption.TargetMetadataDigest != "" || before.Status.RuntimeAdoption.TerminationRequestedTime != nil {
				return errors.New("reserved metadata requires the pending held target transition")
			}
			claim, err := h.claimForSandbox(ctx, before)
			if err != nil {
				return err
			}
			attempt, err := h.readAdmissionAttempt(ctx, claim, before)
			if err != nil {
				return err
			}
			_, holdDigest, err := attempt.evidence.VerifyHold(attempt.observation, before.Status.RuntimeAdoption.Reservation, before.Status.RuntimeAdoption.Initialization, attempt.pod, true)
			if err != nil || before.Status.RuntimeAdoption.HoldEvidenceDigest != holdDigest {
				return errors.Join(errors.New("ownership transfer requires the exact retained hold"), err)
			}
			expected := before.DeepCopy()
			reconciler := SandboxClaimReconciler{APIReader: h.Reader, Scheme: h.Scheme, AllowedLabelDomains: h.AllowedLabelDomains}
			if err := reconciler.PrepareRuntimeAdoptionTarget(ctx, claim, expected); err != nil {
				return err
			}
			if !sameAdmissionMetadata(expected, after) || !reflect.DeepEqual(expected.Spec.PodTemplate.ObjectMeta, after.Spec.PodTemplate.ObjectMeta) {
				return errors.New("reserved metadata differs from the authenticated target transformation")
			}
		}
		return nil
	}
	strict, err := strictRuntimeSandbox(ctx, h.Reader, after)
	if err != nil {
		return err
	}
	if request.Operation == admissionv1.Update {
		oldStrict, err := strictRuntimeSandbox(ctx, h.Reader, before)
		if err != nil {
			return err
		}
		strict = strict || oldStrict
	}
	if strict {
		oldOwner, newOwner := metav1.GetControllerOf(before), metav1.GetControllerOf(after)
		oldPool := utils.MatchesGroupKind(oldOwner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind)
		newPool := utils.MatchesGroupKind(newOwner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind)
		if request.Operation == admissionv1.Create && newPool {
			if request.UserInfo.Username != h.ControllerUsername {
				return errors.New("strict pool origin requires a controller-created Sandbox")
			}
			var pool extensionsv1beta1.SandboxWarmPool
			if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: after.Namespace, Name: newOwner.Name}, &pool); err != nil {
				return err
			}
			if pool.UID != newOwner.UID || pool.DeletionTimestamp != nil {
				return errors.New("strict pool origin requires the live pool UID")
			}
		}
		if request.Operation == admissionv1.Update && (oldPool || newPool) && !reflect.DeepEqual(before.OwnerReferences, after.OwnerReferences) {
			return errors.New("strict pool ownership changes require a protected runtime reservation")
		}
	}
	return nil
}

func sameAdmissionMetadata(before, after metav1.Object) bool {
	return before.GetName() == after.GetName() && before.GetNamespace() == after.GetNamespace() && before.GetUID() == after.GetUID() &&
		reflect.DeepEqual(before.GetLabels(), after.GetLabels()) && reflect.DeepEqual(before.GetAnnotations(), after.GetAnnotations()) &&
		reflect.DeepEqual(before.GetOwnerReferences(), after.GetOwnerReferences())
}

func adoptionReadyChanged(before, after []metav1.Condition) bool {
	condition := meta.FindStatusCondition(after, string(sandboxv1beta1.SandboxConditionReady))
	return condition != nil && condition.Status == metav1.ConditionTrue && !reflect.DeepEqual(condition, meta.FindStatusCondition(before, string(sandboxv1beta1.SandboxConditionReady)))
}

func (h *RuntimeAdoptionAdmissionHandler) claimForSandbox(ctx context.Context, sandbox *sandboxv1beta1.Sandbox) (*extensionsv1beta1.SandboxClaim, error) {
	if sandbox.Status.RuntimeAdoption == nil || sandbox.Status.RuntimeAdoption.Reservation == nil {
		return nil, errors.New("sandbox has no protected claim reservation")
	}
	reservation := sandbox.Status.RuntimeAdoption.Reservation
	var claim extensionsv1beta1.SandboxClaim
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: reservation.Namespace, Name: reservation.ClaimName}, &claim); err != nil {
		return nil, err
	}
	if claim.UID != reservation.ClaimUID || claim.Status.RuntimeAdoption == nil || !reflect.DeepEqual(claim.Status.RuntimeAdoption.Reservation, *reservation) {
		return nil, errors.New("sandbox reservation does not match its live claim")
	}
	return &claim, nil
}

type runtimeAdmissionAttempt struct {
	pod         *corev1.Pod
	namespace   *corev1.Namespace
	evidence    *runtimeadoption.NodeEvidence
	observation *runtimeadoption.Observation
}

func (h *RuntimeAdoptionAdmissionHandler) readAdmissionAttempt(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox) (*runtimeAdmissionAttempt, error) {
	status := claim.Status.RuntimeAdoption
	if status == nil || sandbox.Status.RuntimeAdoption == nil || !reflect.DeepEqual(sandbox.Status.RuntimeAdoption.Reservation, &status.Reservation) ||
		sandbox.UID != status.Reservation.SandboxUID || !runtimeAdoptionHasFinalizer(claim) || !runtimeAdoptionHasFinalizer(sandbox) {
		return nil, errors.New("both protected reservations and finalizers are required")
	}
	pod, err := runtimeAdoptionPod(ctx, h.Reader, sandbox)
	if err != nil {
		return nil, err
	}
	if pod.Name != status.PodRef.Name || pod.UID != status.PodRef.UID || pod.Spec.NodeName != status.NodeRef.Name {
		return nil, errors.New("retained Pod or Node identity changed")
	}
	var namespace corev1.Namespace
	if err := h.Reader.Get(ctx, client.ObjectKey{Name: claim.Namespace}, &namespace); err != nil {
		return nil, err
	}
	if namespace.UID != status.Reservation.NamespaceUID {
		return nil, errors.New("reserved namespace was replaced")
	}
	evidence, err := runtimeadoption.ReadNodeEvidence(ctx, h.Reader, pod, time.Now())
	if err != nil {
		return nil, err
	}
	if evidence.Node.UID != status.NodeRef.UID {
		return nil, errors.New("reserved Node was replaced")
	}
	observation, err := evidence.Observation(&status.Reservation)
	if err != nil {
		return nil, err
	}
	return &runtimeAdmissionAttempt{pod: pod, namespace: &namespace, evidence: evidence, observation: observation}, nil
}

func (h *RuntimeAdoptionAdmissionHandler) currentIntent(claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, namespace *corev1.Namespace) error {
	reservation := &claim.Status.RuntimeAdoption.Reservation
	digest, err := runtimeadoption.ClaimIntentDigest(claim)
	if err != nil {
		return err
	}
	if claim.UID != reservation.ClaimUID || claim.Generation != reservation.ClaimGeneration || digest != reservation.ClaimIntentDigest ||
		claim.DeletionTimestamp != nil || sandbox.DeletionTimestamp != nil || namespace.DeletionTimestamp != nil ||
		claim.Status.RuntimeAdoption.TerminationRequestedTime != nil || sandbox.Status.RuntimeAdoption.TerminationRequestedTime != nil ||
		sandbox.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning || !time.Now().Before(reservation.ExpiresAt.Time) ||
		claim.Spec.Lifecycle != nil && claim.Spec.Lifecycle.ShutdownTime != nil && !time.Now().Before(claim.Spec.Lifecycle.ShutdownTime.Time) ||
		sandbox.Spec.ShutdownTime != nil && !time.Now().Before(sandbox.Spec.ShutdownTime.Time) {
		return errors.New("current claim intent no longer authorizes this adoption")
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) validateTargetMetadata(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sandbox *sandboxv1beta1.Sandbox, attempt *runtimeAdmissionAttempt) error {
	if err := h.currentIntent(claim, sandbox, attempt.namespace); err != nil {
		return err
	}
	if !metav1.IsControlledBy(sandbox, claim) || !sandboxcontrollers.SandboxPodMetadataMatches(ctx, sandbox, attempt.pod) {
		return errors.New("target metadata has not reached the retained Pod")
	}
	digest, err := runtimeadoption.MetadataDigest(attempt.namespace, sandbox, attempt.pod)
	if err != nil || digest != sandbox.Status.RuntimeAdoption.TargetMetadataDigest {
		return errors.Join(errors.New("actual target metadata differs from the protected digest"), err)
	}
	return nil
}
