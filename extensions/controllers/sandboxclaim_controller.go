// Copyright 2025 The Kubernetes Authors.
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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubectl/pkg/util/podutils"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

// TODO: These constants should be imported from the main controller package Issue #216
const (
	sandboxLabel = "agents.x-k8s.io/sandbox-name-hash"
)

// ErrTemplateNotFound is a sentinel error indicating a SandboxTemplate was not found.
var ErrTemplateNotFound = errors.New("SandboxTemplate not found")

// SandboxClaimReconciler reconciles a SandboxClaim object
type SandboxClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	claim := &extensionsv1alpha1.SandboxClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if k8errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get sandbox claim %q: %w", req.NamespacedName, err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Cache original status
	originalClaimStatus := claim.Status.DeepCopy()

	var err error
	var sandbox *v1alpha1.Sandbox
	var template *extensionsv1alpha1.SandboxTemplate

	// Check Expiration
	// The Claim is the source of truth for its own expiration logic.
	claimExpired := false
	if claim.Spec.Lifecycle != nil && claim.Spec.Lifecycle.ShutdownTime != nil {
		claimExpired = time.Now().After(claim.Spec.Lifecycle.ShutdownTime.Time)
	}

	// 1. Manage Sandbox Resources
	if !claimExpired {
		// Logic for active Claims
		if template, err = r.getTemplate(ctx, claim); err == nil || k8errors.IsNotFound(err) {
			// Ensure NetworkPolicy
			if npErr := r.reconcileNetworkPolicy(ctx, claim, template); npErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to reconcile network policy: %w", npErr)
			}
			sandbox, err = r.getOrCreateSandbox(ctx, claim, template)
		}
	} else {
		// Logic for expired Claims
		// We only check if the Sandbox exists to update status.
		// If Policy is Delete, we don't need to check Sandbox status.
		// We just delete the Claim immediately.
		if claim.Spec.Lifecycle != nil && claim.Spec.Lifecycle.ShutdownPolicy == extensionsv1alpha1.ShutdownPolicyDelete {
			log.Info("Deleting Claim because ShutdownPolicy=Delete and time has expired")
			if r.Recorder != nil {
				r.Recorder.Event(claim, corev1.EventTypeNormal, extensionsv1alpha1.ClaimExpiredReason, "Deleting Claim (ShutdownPolicy=Delete)")
			}
			// We ignore NotFound because if it's already gone.
			if delErr := r.Delete(ctx, claim); delErr != nil {
				return ctrl.Result{}, client.IgnoreNotFound(delErr)
			}
			return ctrl.Result{}, nil
		}

		// If we reached here, Policy is "Retain" (or nil).
		// Check Sandbox existence only to update the Status.
		existingSandbox := &v1alpha1.Sandbox{}
		if err = r.Get(ctx, req.NamespacedName, existingSandbox); err == nil {
			sandbox = existingSandbox
		} else if k8errors.IsNotFound(err) {
			err = nil
			sandbox = nil
		}
	}

	// 2. Update Status & Events
	r.computeAndSetStatus(claim, sandbox, err, claimExpired)

	// Emit event if we just transitioned to Expired state (Active -> Expired)
	if !hasExpiredCondition(originalClaimStatus.Conditions) && hasExpiredCondition(claim.Status.Conditions) {
		if r.Recorder != nil {
			r.Recorder.Event(claim, corev1.EventTypeNormal, "SandboxDeleted", "Underlying Sandbox expired and resources cleaned up")
		}
	}

	if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
		err = errors.Join(err, updateErr)
	}

	return ctrl.Result{}, err
}

func (r *SandboxClaimReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxClaimStatus, claim *extensionsv1alpha1.SandboxClaim) error {
	log := log.FromContext(ctx)

	sort.Slice(oldStatus.Conditions, func(i, j int) bool {
		return oldStatus.Conditions[i].Type < oldStatus.Conditions[j].Type
	})
	sort.Slice(claim.Status.Conditions, func(i, j int) bool {
		return claim.Status.Conditions[i].Type < claim.Status.Conditions[j].Type
	})

	if reflect.DeepEqual(oldStatus, &claim.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, claim); err != nil {
		log.Error(err, "Failed to update sandboxclaim status")
		return err
	}

	return nil
}

func (r *SandboxClaimReconciler) computeReadyCondition(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, err error, isClaimExpired bool) metav1.Condition {
	if err != nil {
		reason := "ReconcilerError"
		if errors.Is(err, ErrTemplateNotFound) {
			reason = "TemplateNotFound"
			return metav1.Condition{
				Type:               string(sandboxv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            fmt.Sprintf("SandboxTemplate %q not found", claim.Spec.TemplateRef.Name),
				ObservedGeneration: claim.Generation,
			}
		}
		return metav1.Condition{
			Type:               string(sandboxv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "Error seen: " + err.Error(),
			ObservedGeneration: claim.Generation,
		}
	}

	if sandbox == nil {
		// If missing AND expired, that is the expected state for "Retain"
		if isClaimExpired {
			return metav1.Condition{
				Type:               string(sandboxv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             extensionsv1alpha1.ClaimExpiredReason,
				Message:            "Claim expired. Sandbox resources deleted.",
				ObservedGeneration: claim.Generation,
			}
		}

		// Otherwise, it's genuinely missing
		return metav1.Condition{
			Type:               string(sandboxv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             "SandboxMissing",
			Message:            "Sandbox does not exist",
			ObservedGeneration: claim.Generation,
		}
	}

	// Check if Core Controller marked it as Expired
	if isSandboxExpired(sandbox) {
		return metav1.Condition{
			Type:               string(sandboxv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             extensionsv1alpha1.ClaimExpiredReason,
			Message:            "Claim expired. Sandbox resources deleted.",
			ObservedGeneration: claim.Generation,
		}
	}

	// Forward the condition from Sandbox Status
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == string(sandboxv1alpha1.SandboxConditionReady) {
			return condition
		}
	}

	return metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             "SandboxNotReady",
		Message:            "Sandbox is not ready",
		ObservedGeneration: claim.Generation,
	}
}

func (r *SandboxClaimReconciler) computeAndSetStatus(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, err error, isClaimExpired bool) {
	readyCondition := r.computeReadyCondition(claim, sandbox, err, isClaimExpired)
	meta.SetStatusCondition(&claim.Status.Conditions, readyCondition)

	if sandbox != nil {
		claim.Status.SandboxStatus.Name = sandbox.Name
	}
}

// tryAdoptPodFromPool attempts to find and adopt a pod from the warm pool
func (r *SandboxClaimReconciler) tryAdoptPodFromPool(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox) (*corev1.Pod, error) {
	log := log.FromContext(ctx)

	// List all pods with the podTemplateHashLabel matching the hash
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		sandboxTemplateRefHash: sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name),
	})

	if err := r.List(ctx, podList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     claim.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list pods from warm pool")
		return nil, err
	}

	// Filter pods and create a slice of pointers for sorting
	candidates := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]

		// Skip pods that are being deleted
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		// Skip pods that already have a different controller
		if controllerRef := metav1.GetControllerOf(pod); controllerRef != nil && controllerRef.Kind != "SandboxWarmPool" {
			log.Info("Ignoring pod with different controller, but this shouldn't happen because this pod shouldn't have template ref label",
				"pod", pod.Name,
				"controller", controllerRef.Name,
				"controllerKind", controllerRef.Kind)
			continue
		}

		candidates = append(candidates, pod)
	}

	if len(candidates) == 0 {
		log.Info("No available pods in warm pool (all pods are being deleted, owned by other controllers, or pool is empty)")
		return nil, nil
	}

	// Sort pods using podutils.ByLogging to select the best available pod.
	sort.Sort(podutils.ByLogging(candidates))

	// Get the first available pod
	pod := candidates[0]
	log.Info("Adopting pod from warm pool", "pod", pod.Name)

	// Remove the pool labels
	delete(pod.Labels, poolLabel)
	delete(pod.Labels, sandboxTemplateRefHash)

	// Remove existing owner references (from SandboxWarmPool)
	pod.OwnerReferences = nil

	nameHash := sandboxcontrollers.NameHash(claim.Name)
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}

	pod.Labels[sandboxLabel] = nameHash

	// Label required by NetworkPolicy
	// We add the new label with the Claim UID for unique targeting.
	pod.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)

	// Update the pod
	if err := r.Update(ctx, pod); err != nil {
		log.Error(err, "Failed to update adopted pod")
		return nil, err
	}

	log.Info("Successfully adopted pod from warm pool", "pod", pod.Name, "sandbox", sandbox.Name)
	return pod, nil
}

func (r *SandboxClaimReconciler) createSandbox(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, template *extensionsv1alpha1.SandboxTemplate) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)

	if template == nil {
		logger.Error(ErrTemplateNotFound, "cannot create sandbox")
		return nil, ErrTemplateNotFound
	}

	logger.Info("creating sandbox from template", "template", template.Name)
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
	}

	template.Spec.PodTemplate.DeepCopyInto(&sandbox.Spec.PodTemplate)
	// TODO: this is a workaround, remove replica assignment related issue #202
	replicas := int32(1)
	sandbox.Spec.Replicas = &replicas
	// Enforce a secure-by-default policy by disabling the automatic mounting
	// of the service account token, adhering to security best practices for
	// sandboxed environments.
	if sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken == nil {
		automount := false
		sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken = &automount
	}
	if sandbox.Spec.PodTemplate.ObjectMeta.Labels == nil {
		sandbox.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)

	// Sync lifecycle initially
	if desiredTime, desiredPolicy := calculateDesiredLifecycle(claim); desiredPolicy != nil || desiredTime != nil {
		sandbox.Spec.ShutdownTime = desiredTime
		sandbox.Spec.ShutdownPolicy = desiredPolicy
	}

	if err := controllerutil.SetControllerReference(claim, sandbox, r.Scheme); err != nil {
		err = fmt.Errorf("failed to set controller reference for sandbox: %w", err)
		logger.Error(err, "Error creating sandbox for claim: %q", claim.Name)
		return nil, err
	}

	// Before creating the sandbox, try to adopt a pod from the warm pool
	adoptedPod, adoptErr := r.tryAdoptPodFromPool(ctx, claim, sandbox)
	if adoptErr != nil {
		logger.Error(adoptErr, "Failed to adopt pod from warm pool")
		return nil, adoptErr
	}

	if adoptedPod != nil {
		logger.Info("Adopted pod from warm pool for sandbox", "pod", adoptedPod.Name, "sandbox", sandbox.Name)
		if sandbox.Annotations == nil {
			sandbox.Annotations = make(map[string]string)
		}
		sandbox.Annotations[sandboxcontrollers.SandboxPodNameAnnotation] = adoptedPod.Name
	}

	if err := r.Create(ctx, sandbox); err != nil {
		err = fmt.Errorf("sandbox create error: %w", err)
		logger.Error(err, "Error creating sandbox for claim: %q", claim.Name)
		return nil, err
	}

	logger.Info("Created sandbox for claim", "claim", claim.Name)

	if r.Recorder != nil {
		r.Recorder.Event(claim, corev1.EventTypeNormal, "SandboxProvisioned", fmt.Sprintf("Created Sandbox %q", sandbox.Name))
	}

	return sandbox, nil
}

func (r *SandboxClaimReconciler) getOrCreateSandbox(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, template *extensionsv1alpha1.SandboxTemplate) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		if !k8errors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox %q: %w", claim.Name, err)
			return nil, err
		}
		// Not Found -> Create
		return r.createSandbox(ctx, claim, template)
	}

	// Found -> Check for Control
	if !metav1.IsControlledBy(sandbox, claim) {
		err := fmt.Errorf("sandbox %q is not controlled by claim %q. Please use a different claim name or delete the sandbox manually", sandbox.Name, claim.Name)
		logger.Error(err, "Sandbox controller mismatch")
		return nil, err
	}

	// Check if we need to update Sandbox Spec based on Claim
	updatedSandbox := sandbox.DeepCopy()

	// Apply Lifecycle Logic
	syncLifecycle(claim, updatedSandbox)

	if !reflect.DeepEqual(sandbox.Spec.ShutdownTime, updatedSandbox.Spec.ShutdownTime) ||
		!reflect.DeepEqual(sandbox.Spec.ShutdownPolicy, updatedSandbox.Spec.ShutdownPolicy) {

		logger.Info("Updating Sandbox Lifecycle to match Claim", "Claim", claim.Name)
		if err := r.Update(ctx, updatedSandbox); err != nil {
			return nil, fmt.Errorf("failed to update sandbox lifecycle: %w", err)
		}
		return updatedSandbox, nil
	}

	return sandbox, nil
}

// syncLifecycle applies the Claim's lifecycle intent to the Sandbox.
func syncLifecycle(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox) {
	// 1. Handle Policy (Always has a default, so we always enforce it)
	desiredPolicy := sandboxv1alpha1.ShutdownPolicyRetain // Default
	if claim.Spec.Lifecycle != nil && claim.Spec.Lifecycle.ShutdownPolicy != "" {
		desiredPolicy = sandboxv1alpha1.ShutdownPolicy(claim.Spec.Lifecycle.ShutdownPolicy)
	}
	sandbox.Spec.ShutdownPolicy = &desiredPolicy

	// 2. Handle Time
	// If Claim has a time, enforce it.
	// If Claim is nil, do nothing (preserve Sandbox's existing time).
	if claim.Spec.Lifecycle != nil && claim.Spec.Lifecycle.ShutdownTime != nil {
		t := *claim.Spec.Lifecycle.ShutdownTime
		sandbox.Spec.ShutdownTime = &t
	}
}

// calculateDesiredLifecycle computes the desired lifecycle pointers based on the Claim.
func calculateDesiredLifecycle(claim *extensionsv1alpha1.SandboxClaim) (*metav1.Time, *sandboxv1alpha1.ShutdownPolicy) {
	if claim.Spec.Lifecycle == nil {
		// If Claim has no lifecycle, we assume default (nil time, Retain policy)
		retain := sandboxv1alpha1.ShutdownPolicyRetain
		return nil, &retain
	}

	var desiredTime *metav1.Time
	if claim.Spec.Lifecycle.ShutdownTime != nil {
		t := *claim.Spec.Lifecycle.ShutdownTime
		desiredTime = &t
	}

	// Map Claim policy (value) to Sandbox policy (pointer)
	var desiredPolicy *sandboxv1alpha1.ShutdownPolicy
	if claim.Spec.Lifecycle.ShutdownPolicy != "" {
		p := sandboxv1alpha1.ShutdownPolicy(claim.Spec.Lifecycle.ShutdownPolicy)
		desiredPolicy = &p
	} else {
		p := sandboxv1alpha1.ShutdownPolicyRetain
		desiredPolicy = &p
	}

	return desiredTime, desiredPolicy
}

func (r *SandboxClaimReconciler) getTemplate(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*extensionsv1alpha1.SandboxTemplate, error) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Spec.TemplateRef.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
		if !k8errors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox template %q: %w", claim.Spec.TemplateRef.Name, err)
		}
		return nil, err
	}

	return template, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxClaim{}).
		Owns(&sandboxv1alpha1.Sandbox{}).
		Complete(r)
}

// reconcileNetworkPolicy ensures a NetworkPolicy exists for the claimed Sandbox.
func (r *SandboxClaimReconciler) reconcileNetworkPolicy(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, template *extensionsv1alpha1.SandboxTemplate) error {
	logger := log.FromContext(ctx)

	// 1. Cleanup Check: If missing, delete existing policy
	if template == nil || template.Spec.NetworkPolicy == nil {
		existingNP := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      claim.Name + "-network-policy",
				Namespace: claim.Namespace,
			},
		}
		if err := r.Delete(ctx, existingNP); err != nil {
			if !k8errors.IsNotFound(err) {
				logger.Error(err, "Failed to clean up disabled NetworkPolicy")
				return err
			}
		} else {
			logger.Info("Deleted disabled NetworkPolicy", "name", existingNP.Name)
		}
		return nil
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name + "-network-policy",
			Namespace: claim.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{
				extensionsv1alpha1.SandboxIDLabel: string(claim.UID),
			},
		}
		np.Spec.PolicyTypes = []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		}

		templateNP := template.Spec.NetworkPolicy

		if len(templateNP.Ingress) > 0 {
			np.Spec.Ingress = templateNP.Ingress
		}

		if len(templateNP.Egress) > 0 {
			np.Spec.Egress = templateNP.Egress
		}

		return controllerutil.SetControllerReference(claim, np, r.Scheme)
	})

	if err != nil {
		logger.Error(err, "Failed to create or update NetworkPolicy for claim")
		return err
	}

	logger.Info("Successfully reconciled NetworkPolicy for claim", "NetworkPolicy.Name", np.Name)
	return nil
}

// isSandboxExpired checks the Sandbox status condition set by the Core Controller
func isSandboxExpired(sandbox *v1alpha1.Sandbox) bool {
	return hasExpiredCondition(sandbox.Status.Conditions)
}

// hasExpiredCondition Helper to check if conditions list contains the expired reason
func hasExpiredCondition(conditions []metav1.Condition) bool {
	for _, cond := range conditions {
		if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) {
			if cond.Reason == extensionsv1alpha1.ClaimExpiredReason || cond.Reason == sandboxv1alpha1.SandboxReasonExpired {
				return true
			}
		}
	}
	return false
}
