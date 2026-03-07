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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)


// ErrTemplateNotFound is a sentinel error indicating a SandboxTemplate was not found.
var ErrTemplateNotFound = errors.New("SandboxTemplate not found")

// SandboxClaimReconciler reconciles a SandboxClaim object
type SandboxClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Tracer   asmetrics.Instrumenter
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

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

	// Start Tracing Span
	ctx, end := r.Tracer.StartSpan(ctx, claim, "ReconcileSandboxClaim", nil)
	defer end()

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Initialize trace ID for active resources missing an ID
	tc := r.Tracer.GetTraceContext(ctx)
	if tc != "" && (claim.Annotations == nil || claim.Annotations[asmetrics.TraceContextAnnotation] == "") {
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Annotations == nil {
			claim.Annotations = make(map[string]string)
		}
		claim.Annotations[asmetrics.TraceContextAnnotation] = tc
		if err := r.Patch(ctx, claim, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	originalClaimStatus := claim.Status.DeepCopy()

	// Check Expiration
	// We calculate this upfront to decide the flow.
	claimExpired, timeLeft := r.checkExpiration(claim)

	// Handle "Delete" Policy immediately
	// If we delete the claim, we return immediately.
	// Continuing would try to update the status of a deleted object, causing a crash/error.
	if claimExpired && claim.Spec.Lifecycle != nil && claim.Spec.Lifecycle.ShutdownPolicy == extensionsv1alpha1.ShutdownPolicyDelete {
		log.Info("Deleting Claim because ShutdownPolicy=Delete and time has expired")
		if r.Recorder != nil {
			r.Recorder.Event(claim, corev1.EventTypeNormal, extensionsv1alpha1.ClaimExpiredReason, "Deleting Claim (ShutdownPolicy=Delete)")
		}
		if err := r.Delete(ctx, claim); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	// Manage Resources based on State
	var sandbox *v1alpha1.Sandbox
	var reconcileErr error

	if claimExpired {
		// Policy=Retain (since Delete handled above)
		// Ensure Sandbox is deleted, but keep the Claim.
		sandbox, reconcileErr = r.reconcileExpired(ctx, claim)
	} else {
		// Ensure Sandbox exists and is configured.
		sandbox, reconcileErr = r.reconcileActive(ctx, claim)
	}

	// Update Status & Events
	r.computeAndSetStatus(ctx, claim, sandbox, reconcileErr, claimExpired)

	if !hasExpiredCondition(originalClaimStatus.Conditions) && hasExpiredCondition(claim.Status.Conditions) {
		if r.Recorder != nil {
			r.Recorder.Event(claim, corev1.EventTypeNormal, extensionsv1alpha1.ClaimExpiredReason, "Claim expired")
		}
	}

	if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, updateErr)
	}

	// Determine Result
	var result ctrl.Result
	if !claimExpired && timeLeft > 0 {
		result = ctrl.Result{RequeueAfter: timeLeft}
	}

	// Suppress expected user errors (like missing templates) to avoid crash loops
	if errors.Is(reconcileErr, ErrTemplateNotFound) {
		return result, nil
	}

	return result, reconcileErr
}

// checkExpiration calculates if the claim is expired and how much time is left.
func (r *SandboxClaimReconciler) checkExpiration(claim *extensionsv1alpha1.SandboxClaim) (bool, time.Duration) {
	if claim.Spec.Lifecycle == nil || claim.Spec.Lifecycle.ShutdownTime == nil {
		return false, 0
	}

	now := time.Now()
	expiry := claim.Spec.Lifecycle.ShutdownTime.Time

	if now.After(expiry) {
		return true, 0
	}

	return false, expiry.Sub(now)
}

// reconcileActive handles the creation and updates of running sandboxes.
func (r *SandboxClaimReconciler) reconcileActive(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	// Try getting template
	template, templateErr := r.getTemplate(ctx, claim)

	// If getting template failed with a real error (not just NotFound), fail fast.
	if templateErr != nil && !k8errors.IsNotFound(templateErr) {
		return nil, templateErr
	}

	// Only attempt network policy reconciliation if template was found.
	if templateErr == nil || k8errors.IsNotFound(templateErr) {
		// This ensures the firewall is up before the pod starts.
		if template != nil {
			if npErr := r.reconcileNetworkPolicy(ctx, claim, template); npErr != nil {
				return nil, fmt.Errorf("failed to reconcile network policy: %w", npErr)
			}
		}

		// Try getting sandbox even if template is not found
		// It is possible that the template was deleted after the sandbox was created
		sandbox, err := r.getOrCreateSandbox(ctx, claim, template)

		// Special Case: If we failed to create because template was missing, return that specific error
		if sandbox == nil && err == nil && templateErr != nil {
			return nil, ErrTemplateNotFound
		}
		return sandbox, err
	}

	return nil, templateErr
}

// reconcileExpired ensures the Sandbox is deleted for Retained claims.
func (r *SandboxClaimReconciler) reconcileExpired(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	log := log.FromContext(ctx)
	sandbox := &v1alpha1.Sandbox{}

	// Check if Sandbox exists
	if err := r.Get(ctx, client.ObjectKeyFromObject(claim), sandbox); err != nil {
		if k8errors.IsNotFound(err) {
			return nil, nil // Sandbox is gone, life is good.
		}
		return nil, err
	}

	// Sandbox exists, delete it.
	if sandbox.DeletionTimestamp.IsZero() {
		log.Info("Deleting Sandbox because Claim expired (Policy=Retain)", "Sandbox", sandbox.Name)
		if err := r.Delete(ctx, sandbox); err != nil {
			return sandbox, fmt.Errorf("failed to delete expired sandbox: %w", err)
		}
	}

	return sandbox, nil
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

	if isClaimExpired {
		return metav1.Condition{
			Type:               string(sandboxv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             extensionsv1alpha1.ClaimExpiredReason,
			Message:            "Claim expired. Sandbox resources deleted.",
			ObservedGeneration: claim.Generation,
		}
	}

	if sandbox == nil {
		// Only handle genuine missing sandbox here (expired case is handled above)
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
			Reason:             sandboxv1alpha1.SandboxReasonExpired,
			Message:            "Underlying Sandbox resource has expired independently of the Claim.",
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

func (r *SandboxClaimReconciler) computeAndSetStatus(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, err error, isClaimExpired bool) {
	logger := log.FromContext(ctx)
	readyCondition := r.computeReadyCondition(claim, sandbox, err, isClaimExpired)
	meta.SetStatusCondition(&claim.Status.Conditions, readyCondition)

	if sandbox != nil {
		claim.Status.SandboxStatus.Name = sandbox.Name

		// Look up the pod IP if the sandbox is ready
		if readyCondition.Status == metav1.ConditionTrue {
			podName := sandbox.Name
			if ann, ok := sandbox.Annotations[sandboxcontrollers.SandboxPodNameAnnotation]; ok && ann != "" {
				podName = ann
			}
			pod := &corev1.Pod{}
			if podErr := r.Get(ctx, client.ObjectKey{Name: podName, Namespace: sandbox.Namespace}, pod); podErr == nil {
				claim.Status.SandboxStatus.PodIP = pod.Status.PodIP
			} else {
				logger.V(1).Info("Could not look up pod IP for status", "pod", podName, "error", podErr)
			}
		}
	}
}

// tryAdoptSandboxFromPool attempts to find and adopt a pre-allocated Sandbox from the warm pool.
// Returns the adopted Sandbox if successful, nil if no candidates, or an error.
func (r *SandboxClaimReconciler) tryAdoptSandboxFromPool(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	log := log.FromContext(ctx)

	// List all Sandbox CRs with the template ref hash matching the claim's template
	sandboxList := &v1alpha1.SandboxList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		sandboxTemplateRefHash: sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name),
		warmPoolSandboxLabel:   "", // presence check done via selector below
	})
	// We need sandboxes that have BOTH the template ref hash AND the warm pool label.
	// Since we can't do "exists" with SelectorFromSet, list by template ref and filter.
	labelSelector = labels.SelectorFromSet(labels.Set{
		sandboxTemplateRefHash: sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name),
	})

	if err := r.List(ctx, sandboxList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     claim.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list sandboxes from warm pool")
		return nil, err
	}

	// Filter to only warm pool sandboxes that are ready
	var candidates []*v1alpha1.Sandbox
	for i := range sandboxList.Items {
		sb := &sandboxList.Items[i]

		// Must have the warm pool label
		if _, ok := sb.Labels[warmPoolSandboxLabel]; !ok {
			continue
		}

		// Skip sandboxes being deleted
		if !sb.DeletionTimestamp.IsZero() {
			continue
		}

		// Skip sandboxes not owned by a SandboxWarmPool
		controllerRef := metav1.GetControllerOf(sb)
		if controllerRef != nil && controllerRef.Kind != "SandboxWarmPool" {
			continue
		}

		// Prefer ready sandboxes
		candidates = append(candidates, sb)
	}

	if len(candidates) == 0 {
		log.Info("No available sandboxes in warm pool")
		return nil, nil
	}

	// Sort: ready sandboxes first, then by creation time (oldest first)
	sort.Slice(candidates, func(i, j int) bool {
		iReady := isSandboxReady(candidates[i])
		jReady := isSandboxReady(candidates[j])
		if iReady != jReady {
			return iReady
		}
		return candidates[i].CreationTimestamp.Before(&candidates[j].CreationTimestamp)
	})

	adopted := candidates[0]
	log.Info("Adopting sandbox from warm pool", "sandbox", adopted.Name)

	// Remove warm pool labels
	delete(adopted.Labels, warmPoolSandboxLabel)
	delete(adopted.Labels, sandboxTemplateRefHash)

	// Remove existing owner references (from SandboxWarmPool)
	adopted.OwnerReferences = nil

	// Set the claim as controller owner
	if err := controllerutil.SetControllerReference(claim, adopted, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference on adopted sandbox: %w", err)
	}

	// Propagate trace context from claim
	if adopted.Annotations == nil {
		adopted.Annotations = make(map[string]string)
	}
	if tc, ok := claim.Annotations[asmetrics.TraceContextAnnotation]; ok {
		adopted.Annotations[asmetrics.TraceContextAnnotation] = tc
	}

	// Add sandbox ID label to pod template for NetworkPolicy targeting
	if adopted.Spec.PodTemplate.ObjectMeta.Labels == nil {
		adopted.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	adopted.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)

	if err := r.Update(ctx, adopted); err != nil {
		log.Error(err, "Failed to update adopted sandbox")
		return nil, err
	}

	log.Info("Successfully adopted sandbox from warm pool", "sandbox", adopted.Name, "claim", claim.Name)

	if r.Recorder != nil {
		r.Recorder.Event(claim, corev1.EventTypeNormal, "SandboxAdopted", fmt.Sprintf("Adopted warm pool Sandbox %q", adopted.Name))
	}

	return adopted, nil
}

// isSandboxReady checks if a sandbox has Ready=True condition
func isSandboxReady(sb *v1alpha1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
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

	// Propagate the trace context annotation to the Sandbox resource
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string)
	}
	if tc, ok := claim.Annotations[asmetrics.TraceContextAnnotation]; ok {
		sandbox.Annotations[asmetrics.TraceContextAnnotation] = tc
	}

	template.Spec.PodTemplate.DeepCopyInto(&sandbox.Spec.PodTemplate)
	replicas := int32(1)
	sandbox.Spec.Replicas = &replicas
	if sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken == nil {
		automount := false
		sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken = &automount
	}
	if sandbox.Spec.PodTemplate.ObjectMeta.Labels == nil {
		sandbox.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)

	if err := controllerutil.SetControllerReference(claim, sandbox, r.Scheme); err != nil {
		err = fmt.Errorf("failed to set controller reference for sandbox: %w", err)
		logger.Error(err, "Error creating sandbox for claim", "claimName", claim.Name)
		return nil, err
	}

	if err := r.Create(ctx, sandbox); err != nil {
		err = fmt.Errorf("sandbox create error: %w", err)
		logger.Error(err, "Error creating sandbox for claim", "claimName", claim.Name)
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
		sandbox = nil
		if !k8errors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox %q: %w", claim.Name, err)
			return nil, err
		}
	}

	if sandbox != nil {
		logger.Info("sandbox already exists, skipping update", "name", sandbox.Name)
		if !metav1.IsControlledBy(sandbox, claim) {
			err := fmt.Errorf("sandbox %q is not controlled by claim %q. Please use a different claim name or delete the sandbox manually", sandbox.Name, claim.Name)
			logger.Error(err, "Sandbox controller mismatch")
			return nil, err
		}
		return sandbox, nil
	}

	// Try to adopt a pre-allocated Sandbox from the warm pool first (single reconcile cycle)
	adopted, adoptErr := r.tryAdoptSandboxFromPool(ctx, claim)
	if adoptErr != nil {
		logger.Error(adoptErr, "Failed to adopt sandbox from warm pool, falling back to creation")
		// Fall through to create a new sandbox
	}
	if adopted != nil {
		return adopted, nil
	}

	// No warm pool sandbox available — create a new one
	return r.createSandbox(ctx, claim, template)
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
