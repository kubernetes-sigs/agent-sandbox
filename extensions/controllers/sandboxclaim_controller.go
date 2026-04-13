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
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	"sigs.k8s.io/agent-sandbox/internal/lifecycle"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

const ObservabilityAnnotation = "agents.x-k8s.io/controller-first-observed-at"
const immediateRequeueDelay = time.Millisecond

// ErrTemplateNotFound is a sentinel error indicating a SandboxTemplate was not found.
var ErrTemplateNotFound = errors.New("SandboxTemplate not found")

// ErrInvalidMetadata is a sentinel error indicating additionalPodMetadata was invalid.
var ErrInvalidMetadata = errors.New("invalid additionalPodMetadata")

// ErrSandboxNotOwned indicates the Sandbox exists but is not controlled by this claim.
var ErrSandboxNotOwned = errors.New("sandbox not owned by this claim")

var restrictedDomains = []string{"kubernetes.io", "k8s.io", "agents.x-k8s.io"}

// getWarmPoolPolicy returns the effective warm pool policy for a claim.
func getWarmPoolPolicy(claim *extensionsv1alpha1.SandboxClaim) extensionsv1alpha1.WarmPoolPolicy {
	if claim.Spec.WarmPool != nil {
		return *claim.Spec.WarmPool
	}
	return extensionsv1alpha1.WarmPoolPolicyDefault
}

// SandboxClaimReconciler reconciles a SandboxClaim object.
type SandboxClaimReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	WarmSandboxQueue        queue.SandboxQueue
	Recorder                events.EventRecorder
	Tracer                  asmetrics.Instrumenter
	MaxConcurrentReconciles int
	observedTimes           sync.Map
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims/finalizers,verbs=get;update;patch
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch;update
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Start of Reconcile loop for SandboxClaim", "request", req.NamespacedName)
	claim := &extensionsv1alpha1.SandboxClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if k8errors.IsNotFound(err) {
			logger.V(1).Info("SandboxClaim not found, ignoring", "request", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get sandbox claim %q: %w", req.NamespacedName, err)
	}

	// Unconditionally clean up legacy per-claim NetworkPolicies.
	// We log the error but do not block the main reconcile flow so
	// transient API issues don't prevent Sandbox adoption/creation.
	if err := r.cleanupLegacyNetworkPolicy(ctx, claim); err != nil {
		logger.Error(err, "Non-fatal error cleaning up legacy per-claim NetworkPolicy")
	}

	// Start Tracing Span
	ctx, end := r.Tracer.StartSpan(ctx, claim, "ReconcileSandboxClaim", nil)
	defer end()

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Initialize trace ID and observation time for active resources missing them.
	// Inline patch, no early return, to avoid forcing a second reconcile cycle.
	traceContext := r.Tracer.GetTraceContext(ctx)
	needObservabilityPatch := claim.Annotations[asmetrics.ObservabilityAnnotation] == ""
	needTraceContextPatch := traceContext != "" && (claim.Annotations[asmetrics.TraceContextAnnotation] == "")

	if needObservabilityPatch || needTraceContextPatch {
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Annotations == nil {
			claim.Annotations = make(map[string]string)
		}
		if needObservabilityPatch {
			key := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
			if val, ok := r.observedTimes.Load(key); ok {
				claim.Annotations[asmetrics.ObservabilityAnnotation] = val.(time.Time).Format(time.RFC3339Nano)
			} else {
				now := time.Now()
				claim.Annotations[asmetrics.ObservabilityAnnotation] = now.Format(time.RFC3339Nano)
				r.observedTimes.Store(key, now)
			}
		}
		if needTraceContextPatch {
			claim.Annotations[asmetrics.TraceContextAnnotation] = traceContext
		}
		if err := r.Patch(ctx, claim, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	originalClaimStatus := claim.Status.DeepCopy()

	// Check Expiration
	// We calculate this upfront to decide the flow.
	claimExpired, timeLeft := r.checkExpiration(claim)
	if claimExpired && !hasClaimExpiredCondition(claim.Status.Conditions) {
		meta.SetStatusCondition(&claim.Status.Conditions, r.computeReadyCondition(claim, nil, nil, true))
		if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
			logger.V(1).Info("Sandboxclaim UpdateStatus error encountered", "errors", updateErr, "request", req.NamespacedName)
			return ctrl.Result{}, updateErr
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, extensionsv1alpha1.ClaimExpiredReason, "Claim Expired", "Claim expired")
		}
		return ctrl.Result{RequeueAfter: immediateRequeueDelay}, nil
	}
	logger.V(1).Info("Expiration check", "isExpired", claimExpired, "timeLeft", timeLeft, "request", req.NamespacedName)

	// Handle "Delete" and "DeleteForeground" policies immediately.
	// If we delete the claim, we return immediately.
	// Continuing would try to update the status of a deleted object, causing a crash/error.
	if claimExpired && claim.Spec.Lifecycle != nil &&
		(claim.Spec.Lifecycle.ShutdownPolicy == extensionsv1alpha1.ShutdownPolicyDelete ||
			claim.Spec.Lifecycle.ShutdownPolicy == extensionsv1alpha1.ShutdownPolicyDeleteForeground) {

		policy := claim.Spec.Lifecycle.ShutdownPolicy
		logger.Info("Deleting Claim because time has expired", "shutdownPolicy", policy, "claim", claim.Name)
		if r.Recorder != nil {
			r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, extensionsv1alpha1.ClaimExpiredReason, "Deleting", fmt.Sprintf("Deleting Claim (ShutdownPolicy=%s)", policy))
		}

		deleteOpts := []client.DeleteOption{}
		if policy == extensionsv1alpha1.ShutdownPolicyDeleteForeground {
			deleteOpts = append(deleteOpts, client.PropagationPolicy(metav1.DeletePropagationForeground))
		}

		if err := r.Delete(ctx, claim, deleteOpts...); err != nil {
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
	r.computeAndSetStatus(claim, sandbox, reconcileErr, claimExpired)
	postExpiration, postTimeLeft := r.checkExpiration(claim)
	if postExpiration && !hasClaimExpiredCondition(claim.Status.Conditions) {
		meta.SetStatusCondition(&claim.Status.Conditions, r.computeReadyCondition(claim, sandbox, reconcileErr, true))
		if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
			errs := errors.Join(reconcileErr, updateErr)
			logger.V(1).Info("Sandboxclaim UpdateStatus error encountered", "errors", errs, "request", req.NamespacedName)
			return ctrl.Result{}, errs
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, extensionsv1alpha1.ClaimExpiredReason, "Claim Expired", "Claim expired")
		}
		return ctrl.Result{RequeueAfter: immediateRequeueDelay}, nil
	}

	if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
		errs := errors.Join(reconcileErr, updateErr)
		logger.V(1).Info("Sandboxclaim UpdateStatus error encountered", "errors", errs, "request", req.NamespacedName)
		return ctrl.Result{}, errs
	}

	r.recordCreationLatencyMetric(ctx, claim, originalClaimStatus, sandbox)

	// Determine Result
	var result ctrl.Result
	if !claimExpired {
		if postExpiration {
			result = ctrl.Result{RequeueAfter: immediateRequeueDelay}
		} else if postTimeLeft > 0 {
			result = ctrl.Result{RequeueAfter: postTimeLeft}
		}
	}

	// Suppress expected user errors (like missing templates) to avoid crash loops
	if errors.Is(reconcileErr, ErrTemplateNotFound) || errors.Is(reconcileErr, ErrInvalidMetadata) || errors.Is(reconcileErr, ErrSandboxNotOwned) {
		logger.V(1).Info("Sandboxclaim suppressed error(s) encountered", "error", reconcileErr, "request", req.NamespacedName)
		return result, nil
	}

	logger.V(1).Info("End of Reconcile loop SandboxClaim", "result", result, "error", reconcileErr, "request", req.NamespacedName)
	return result, reconcileErr
}

// checkExpiration calculates if the claim is expired and how much time is left.
func (r *SandboxClaimReconciler) checkExpiration(claim *extensionsv1alpha1.SandboxClaim) (bool, time.Duration) {
	if claim.Spec.Lifecycle == nil {
		return false, 0
	}

	finishedCondition := lifecycle.FinishedCondition(claim.Status.Conditions, string(v1alpha1.SandboxConditionFinished))
	return lifecycle.TimeLeft(time.Now(), claim.Spec.Lifecycle.ShutdownTime, claim.Spec.Lifecycle.TTLSecondsAfterFinished, finishedCondition)
}

// reconcileActive handles the creation and updates of running sandboxes.
func (r *SandboxClaimReconciler) reconcileActive(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling active claim", "claim", claim.Name)

	// Upfront validation of additional metadata to skip unnecessary processing
	if err := validateAdditionalPodMetadata(&claim.Spec.AdditionalPodMetadata); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMetadata, err)
	}

	// Fast path: try to find existing or adopt from warm pool before template lookup.
	sandbox, err := r.getOrCreateSandbox(ctx, claim, nil)
	logger.V(1).Info("getOrCreateSandbox result", "sandboxFound", sandbox != nil, "err", err, "claim", claim.Name)
	if err != nil {
		return nil, err
	}
	if sandbox != nil {
		// Found or adopted. Reconcile network policy (best effort, non blocking).
		logger.V(1).Info("Fast path: sandbox found or adopted, reconciling network policy", "claim", claim.Name)
		template, templateErr := r.getTemplate(ctx, claim)
		if templateErr != nil {
			logger.Error(templateErr, "failed to get template for network policy reconciliation (non-fatal)", "claim", claim.Name)

			// If we can't get the template but we have metadata to propagate, we should fail
			// to ensure consistency and enforce the "No Overrides" rule.
			if len(claim.Spec.AdditionalPodMetadata.Labels) > 0 || len(claim.Spec.AdditionalPodMetadata.Annotations) > 0 {
				return nil, fmt.Errorf("failed to get template for metadata propagation: %w", templateErr)
			}
		}

		if template != nil {

			// Check if metadata needs update
			var mergedMeta v1alpha1.PodMetadata
			template.Spec.PodTemplate.ObjectMeta.DeepCopyInto(&mergedMeta)

			// Preserve system-injected labels
			if mergedMeta.Labels == nil {
				mergedMeta.Labels = make(map[string]string)
			}
			mergedMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)
			mergedMeta.Labels[sandboxTemplateRefHash] = sandboxcontrollers.NameHash(template.Name)

			if err := mergePodMetadata(&mergedMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
				return nil, err
			}

			if !equality.Semantic.DeepEqual(&mergedMeta, &sandbox.Spec.PodTemplate.ObjectMeta) {
				logger.Info("Updating sandbox metadata to match claim", "claim", claim.Name, "sandbox", sandbox.Name)
				sandbox.Spec.PodTemplate.ObjectMeta = mergedMeta
				if err := r.Update(ctx, sandbox); err != nil {
					return nil, err
				}
			}
		}
		return sandbox, nil
	}

	// Cold path: no existing sandbox or warm pool candidate.
	// Need template to create from scratch.
	logger.V(1).Info("Cold path: no sandbox found, creating from template", "claim", claim.Name)
	template, templateErr := r.getTemplate(ctx, claim)
	if templateErr != nil && !k8errors.IsNotFound(templateErr) {
		return nil, templateErr
	}

	return r.createSandbox(ctx, claim, template)
}

// reconcileExpired ensures the Sandbox is deleted for Retained claims.
func (r *SandboxClaimReconciler) reconcileExpired(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling Expired claim", "claim", claim.Name)

	// Fall back to claim.Name when status is unset.
	statusName := claim.Name
	if claim.Status.SandboxStatus.Name != "" {
		statusName = claim.Status.SandboxStatus.Name
	}

	sandbox := &v1alpha1.Sandbox{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: statusName}, sandbox); err != nil {
		if k8errors.IsNotFound(err) {
			return nil, nil // Sandbox is gone, life is good.
		}
		return nil, err
	}

	// Verify ownership before delete action
	if !metav1.IsControlledBy(sandbox, claim) {
		logger.Info("Skipping deletion: Sandbox is not controlled by this claim", "sandbox", sandbox.Name, "claim", claim.Name)
		return nil, fmt.Errorf("%w: sandbox %q is not owned by claim %q", ErrSandboxNotOwned, sandbox.Name, claim.Name)
	}
	// Sandbox exists, delete it.
	if sandbox.DeletionTimestamp.IsZero() {
		logger.Info("Deleting Sandbox because Claim expired (Policy=Retain)", "sandbox", sandbox.Name, "claim", claim.Name)
		if err := r.Delete(ctx, sandbox); err != nil {
			return sandbox, fmt.Errorf("failed to delete expired sandbox: %w", err)
		}
	}
	return sandbox, nil
}

func (r *SandboxClaimReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxClaimStatus, claim *extensionsv1alpha1.SandboxClaim) error {
	logger := log.FromContext(ctx)

	slices.SortFunc(oldStatus.Conditions, func(a, b metav1.Condition) int {
		if a.Type < b.Type {
			return -1
		}
		return 1
	})
	slices.SortFunc(claim.Status.Conditions, func(a, b metav1.Condition) int {
		if a.Type < b.Type {
			return -1
		}
		return 1
	})

	if equality.Semantic.DeepEqual(oldStatus, &claim.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, claim); err != nil {
		logger.Error(err, "Failed to update sandboxclaim status")
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
				Type:               string(v1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            fmt.Sprintf("SandboxTemplate %q not found", claim.Spec.TemplateRef.Name),
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, ErrInvalidMetadata) {
			reason = "InvalidMetadata"
			return metav1.Condition{
				Type:               string(v1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            err.Error(),
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, ErrSandboxNotOwned) {
			return metav1.Condition{
				Type:               string(v1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             extensionsv1alpha1.ClaimExpiredReason,
				Message:            fmt.Sprintf("Claim expired. %v; deletion skipped.", err),
				ObservedGeneration: claim.Generation,
			}
		}
		return metav1.Condition{
			Type:               string(v1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "Error seen: " + err.Error(),
			ObservedGeneration: claim.Generation,
		}
	}

	if isClaimExpired {
		return metav1.Condition{
			Type:               string(v1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             extensionsv1alpha1.ClaimExpiredReason,
			Message:            "Claim expired. Sandbox resources deleted.",
			ObservedGeneration: claim.Generation,
		}
	}

	if sandbox == nil {
		// Only handle genuine missing sandbox here (expired case is handled above)
		return metav1.Condition{
			Type:               string(v1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             "SandboxMissing",
			Message:            "Sandbox does not exist",
			ObservedGeneration: claim.Generation,
		}
	}

	// Check if Core Controller marked it as Expired
	if hasSandboxExpiredCondition(sandbox.Status.Conditions) {
		return metav1.Condition{
			Type:               string(v1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.SandboxReasonExpired,
			Message:            "Underlying Sandbox resource has expired independently of the Claim.",
			ObservedGeneration: claim.Generation,
		}
	}

	// Forward the condition from Sandbox Status
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == string(v1alpha1.SandboxConditionReady) {
			return condition
		}
	}

	return metav1.Condition{
		Type:               string(v1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             "SandboxNotReady",
		Message:            "Sandbox is not ready",
		ObservedGeneration: claim.Generation,
	}
}

func (r *SandboxClaimReconciler) computeAndSetStatus(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, err error, isClaimExpired bool) {
	readyCondition := r.computeReadyCondition(claim, sandbox, err, isClaimExpired)
	meta.SetStatusCondition(&claim.Status.Conditions, readyCondition)
	r.syncFinishedCondition(claim, sandbox, isClaimExpired)

	if sandbox != nil {
		claim.Status.SandboxStatus.Name = sandbox.Name
		claim.Status.SandboxStatus.PodIPs = sandbox.Status.PodIPs
	} else {
		claim.Status.SandboxStatus.Name = ""
		claim.Status.SandboxStatus.PodIPs = nil
	}
}

func (r *SandboxClaimReconciler) syncFinishedCondition(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, isClaimExpired bool) {
	if sandbox != nil {
		finishedCondition := meta.FindStatusCondition(sandbox.Status.Conditions, string(v1alpha1.SandboxConditionFinished))
		if finishedCondition != nil {
			meta.SetStatusCondition(&claim.Status.Conditions, *finishedCondition)
		} else {
			meta.RemoveStatusCondition(&claim.Status.Conditions, string(v1alpha1.SandboxConditionFinished))
		}
		return
	}

	if !isClaimExpired {
		meta.RemoveStatusCondition(&claim.Status.Conditions, string(v1alpha1.SandboxConditionFinished))
	}
}

func (r *SandboxClaimReconciler) getCandidate(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, templateHash string) (*v1alpha1.Sandbox, queue.SandboxKey, error) {
	logger := log.FromContext(ctx)
	policy := getWarmPoolPolicy(claim)

	var skipped []queue.SandboxKey
	// Instantly returns unused keys the moment we find a valid candidate!
	defer func() {
		for _, key := range skipped {
			r.WarmSandboxQueue.Add(templateHash, key)
		}
	}()

	for {
		adoptedKey, ok := r.WarmSandboxQueue.Get(templateHash)
		if !ok {
			return nil, queue.SandboxKey{}, nil
		}

		// 1. Hand the Kubernetes client the empty bucket
		adopted := &v1alpha1.Sandbox{}

		// 2. Fetch from the Informer Cache
		err := r.Get(ctx, client.ObjectKey{Namespace: adoptedKey.Namespace, Name: adoptedKey.Name}, adopted)
		if err != nil {
			if k8errors.IsNotFound(err) {
				// Ghost Pod detected: It was deleted from the cluster but was still in our queue.
				// Ignore it and instantly pop the next one.
				continue
			}
			// For real errors, put the key back in line and error out
			r.WarmSandboxQueue.Add(templateHash, adoptedKey)
			return nil, queue.SandboxKey{}, err
		}

		if err := verifySandboxCandidate(adopted, claim); err != nil {
			logger.V(1).Info("sandbox candidate can't be adopted for template", "sandbox", adopted.Name, "templateHash", templateHash, "reason", err.Error())
			continue
		}

		if policy.IsSpecificPool() {
			specificPoolHash := sandboxcontrollers.NameHash(string(policy))
			if adopted.Labels[warmPoolSandboxLabel] != specificPoolHash {
				skipped = append(skipped, adoptedKey) // Save to skip list for the defer loop
				continue
			}
		}

		// Valid candidate found
		return adopted, adoptedKey, nil
	}
}

func (r *SandboxClaimReconciler) adoptSandboxFromCandidates(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	templateHash := sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name)

	// Keep trying until we successfully adopt a sandbox, or run out of candidates
	for range 3 {
		adopted, adoptedKey, err := r.getCandidate(ctx, claim, templateHash)
		if err != nil {
			return nil, err
		}
		if adopted == nil {
			logger.Info("Failed to adopt any sandbox after checking all candidates", "claim", claim.Name)
			return nil, nil // Warm pool is truly empty, fall completely to cold start
		}

		// Wrap the API logic in a closure
		success, err := func() (bool, error) {
			poolName := "none"
			if controllerRef := metav1.GetControllerOf(adopted); controllerRef != nil {
				poolName = controllerRef.Name
			}

			logger.Info("Attempting sandbox adoption", "sandbox candidate", adopted.Name, "warm pool", poolName, "claim", claim.Name)

			// Take a snapshot of the pod BEFORE we mutate it to generate a clean JSON Patch.
			originalAdopted := adopted.DeepCopy()

			// Remove warm pool labels so the sandbox no longer appears in warm pool queries
			delete(adopted.Labels, warmPoolSandboxLabel)
			delete(adopted.Labels, sandboxTemplateRefHash)
			delete(adopted.Labels, v1alpha1.SandboxPodTemplateHashLabel)

			// Transfer ownership from SandboxWarmPool to SandboxClaim
			adopted.OwnerReferences = nil
			if err := controllerutil.SetControllerReference(claim, adopted, r.Scheme); err != nil {
				r.WarmSandboxQueue.Add(templateHash, adoptedKey)
				return false, fmt.Errorf("failed to set controller reference on adopted sandbox: %w", err)
			}

			// Propagate trace context from claim
			if adopted.Annotations == nil {
				adopted.Annotations = make(map[string]string)
			}

			// Ensure the adopted sandbox records its pod name before it can be observed Ready.
			if podName := adopted.Annotations[v1alpha1.SandboxPodNameAnnotation]; podName != adopted.Name {
				if podName != "" {
					logger.Info("Correcting adopted sandbox pod-name annotation", "sandbox", adopted.Name, "oldPodName", podName, "newPodName", adopted.Name)
				}
				adopted.Annotations[v1alpha1.SandboxPodNameAnnotation] = adopted.Name
			}

			if traceContext, ok := claim.Annotations[asmetrics.TraceContextAnnotation]; ok {
				adopted.Annotations[asmetrics.TraceContextAnnotation] = traceContext
			}

			// Add sandbox ID label to pod template for NetworkPolicy targeting
			if adopted.Spec.PodTemplate.ObjectMeta.Labels == nil {
				adopted.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
			}
			adopted.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)

			// Fetch the template to construct the mergedMeta that reconcileActive will build.
			template, templateErr := r.getTemplate(ctx, claim)
			if templateErr == nil && template != nil {
				var mergedMeta v1alpha1.PodMetadata
				template.Spec.PodTemplate.ObjectMeta.DeepCopyInto(&mergedMeta)

				if mergedMeta.Labels == nil {
					mergedMeta.Labels = make(map[string]string)
				}
				mergedMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)
				mergedMeta.Labels[sandboxTemplateRefHash] = templateHash

				if err := mergePodMetadata(&mergedMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
					// Adoption hasn't been patched yet. Put the sandbox back to avoid draining the pool!
					r.WarmSandboxQueue.Add(templateHash, adoptedKey)
					logger.Error(err, "Failed to merge pod metadata for adoption candidate sandbox", "sandbox candidate", adopted.Name, "claim", claim.Name)
					return false, err
				}

				// Force an exact match
				adopted.Spec.PodTemplate.ObjectMeta = mergedMeta
			} else {
				// Fallback (just in case template is somehow missing)
				adopted.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash] = templateHash

				if err := mergePodMetadata(&adopted.Spec.PodTemplate.ObjectMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
					r.WarmSandboxQueue.Add(templateHash, adoptedKey)
					logger.Error(err, "Failed to merge pod metadata for fallback adoption candidate sandbox", "sandbox candidate", adopted.Name, "claim", claim.Name)
					return false, err
				}
			}

			if err := r.Patch(ctx, adopted, client.MergeFrom(originalAdopted)); err != nil {
				if k8errors.IsNotFound(err) {
					return false, nil
				}

				r.WarmSandboxQueue.Add(templateHash, adoptedKey)

				if k8errors.IsConflict(err) {
					// Patch conflicts are not expected here in the common case, but they can still legitimately occur.
					return false, nil
				}

				logger.Error(err, "Failed to patch adoption candidate sandbox", "sandbox candidate", adopted.Name, "claim", claim.Name)
				return false, err
			}

			logger.Info("Successfully adopted sandbox from warm pool", "sandbox", adopted.Name, "claim", claim.Name)

			if r.Recorder != nil {
				r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, "SandboxAdopted", "Adoption", "Adopted warm pool Sandbox %q", adopted.Name)
			}

			podCondition := "not_ready"
			if isSandboxReady(adopted) {
				podCondition = "ready"
			}
			asmetrics.RecordSandboxClaimCreation(claim.Namespace, claim.Spec.TemplateRef.Name, asmetrics.LaunchTypeWarm, poolName, podCondition)

			return true, nil
		}()

		if err != nil {
			return nil, err
		}

		if success {
			return adopted, nil
		}
	}

	logger.Info("Failed to adopt sandbox after max retries", "claim", claim.Name)
	return nil, nil
}

// isSandboxReady checks if a sandbox has Ready=True condition.
func isSandboxReady(sb *v1alpha1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(v1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func isRestrictedDomain(domain string) bool {
	for _, d := range restrictedDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// validateAdditionalPodMetadata checks claimMeta for invalid domain or label values upfront.
func validateAdditionalPodMetadata(claimMeta *v1alpha1.PodMetadata) error {
	if claimMeta == nil {
		return nil
	}

	validate := func(key, value string, isLabel bool) error {
		// Check restricted domains
		parts := strings.SplitN(key, "/", 2)
		domain := ""
		if len(parts) > 1 {
			domain = strings.ToLower(parts[0])
		}
		if isRestrictedDomain(domain) {
			return fmt.Errorf("restricted system domain: %q is not allowed in AdditionalPodMetadata", key)
		}

		// Validate label values (annotations have less restrictions)
		if isLabel {
			if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
				return fmt.Errorf("invalid label value: %q does not match allowed pattern: %s", value, strings.Join(errs, "; "))
			}
		}
		return nil
	}

	for k, v := range claimMeta.Labels {
		if err := validate(k, v, true); err != nil {
			return fmt.Errorf("failed to validate label %q: %w", k, err)
		}
	}

	for k, v := range claimMeta.Annotations {
		if err := validate(k, v, false); err != nil {
			return fmt.Errorf("failed to validate annotation %q: %w", k, err)
		}
	}

	return nil
}

// mergePodMetadata merges labels and annotations from claimMeta into templateMeta,
// rejecting overrides with different values.
func mergePodMetadata(templateMeta *v1alpha1.PodMetadata, claimMeta *v1alpha1.PodMetadata) error {
	if err := validateAdditionalPodMetadata(claimMeta); err != nil {
		return err
	}

	// Check for overrides in labels
	for k, v := range claimMeta.Labels {
		if tv, ok := templateMeta.Labels[k]; ok && tv != v {
			return fmt.Errorf("metadata override conflict: label %q is defined in template with value %q, but claim requests %q", k, tv, v)
		}
	}

	// Check for overrides in annotations
	for k, v := range claimMeta.Annotations {
		if tv, ok := templateMeta.Annotations[k]; ok && tv != v {
			return fmt.Errorf("metadata override conflict: annotation %q is defined in template with value %q, but claim requests %q", k, tv, v)
		}
	}

	// Merge labels
	if len(claimMeta.Labels) > 0 {
		if templateMeta.Labels == nil {
			templateMeta.Labels = make(map[string]string)
		}
		maps.Copy(templateMeta.Labels, claimMeta.Labels)
	}

	// Merge annotations
	if len(claimMeta.Annotations) > 0 {
		if templateMeta.Annotations == nil {
			templateMeta.Annotations = make(map[string]string)
		}
		maps.Copy(templateMeta.Annotations, claimMeta.Annotations)
	}

	return nil
}

// injectEnvs is a helper to inject/override a set of environment variables in a container.
func (r *SandboxClaimReconciler) injectEnvs(logger logr.Logger, container *corev1.Container, envsToInject []extensionsv1alpha1.EnvVar, policy extensionsv1alpha1.EnvVarsInjectionPolicy, claimName string) error {
	for _, claimEnv := range envsToInject {
		existingIdx := -1
		for j, env := range container.Env {
			if env.Name == claimEnv.Name {
				existingIdx = j
				break
			}
		}

		if existingIdx >= 0 {
			if policy != extensionsv1alpha1.EnvVarsInjectionPolicyOverrides {
				err := fmt.Errorf("environment variable override is not allowed by the template policy for variable %q", claimEnv.Name)
				logger.Error(err, "Environment variable override rejected", "claimName", claimName, "envName", claimEnv.Name)
				return err
			}
			logger.Info("Overriding existing environment variable", "envName", claimEnv.Name, "container", container.Name)
			container.Env[existingIdx] = corev1.EnvVar{Name: claimEnv.Name, Value: claimEnv.Value}
		} else {
			logger.Info("Appending new environment variable", "envName", claimEnv.Name, "container", container.Name)
			container.Env = append(container.Env, corev1.EnvVar{Name: claimEnv.Name, Value: claimEnv.Value})
		}
	}
	return nil
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
	if traceContext, ok := claim.Annotations[asmetrics.TraceContextAnnotation]; ok {
		sandbox.Annotations[asmetrics.TraceContextAnnotation] = traceContext
	}

	// Track the sandbox template ref to be used by metrics collector
	sandbox.Annotations[v1alpha1.SandboxTemplateRefAnnotation] = template.Name

	template.Spec.PodTemplate.DeepCopyInto(&sandbox.Spec.PodTemplate)

	if sandbox.Spec.PodTemplate.ObjectMeta.Labels == nil {
		sandbox.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash] = sandboxcontrollers.NameHash(template.Name)

	if err := mergePodMetadata(&sandbox.Spec.PodTemplate.ObjectMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
		return nil, err
	}

	// Inject environment variables from the SandboxClaim
	if len(claim.Spec.Env) > 0 {
		if template.Spec.EnvVarsInjectionPolicy != extensionsv1alpha1.EnvVarsInjectionPolicyAllowed && template.Spec.EnvVarsInjectionPolicy != extensionsv1alpha1.EnvVarsInjectionPolicyOverrides {
			err := fmt.Errorf("environment variable injection is not allowed by the template policy")
			logger.Error(err, "Environment variable injection rejected", "claimName", claim.Name)
			return nil, err
		}

		// Group envs by container name for efficient lookup.
		envsByContainer := make(map[string][]extensionsv1alpha1.EnvVar)
		defaultEnvs := []extensionsv1alpha1.EnvVar{}
		for _, env := range claim.Spec.Env {
			if env.ContainerName == "" {
				defaultEnvs = append(defaultEnvs, env)
			} else {
				envsByContainer[env.ContainerName] = append(envsByContainer[env.ContainerName], env)
			}
		}

		// Validate that all targeted containers exist.
		allContainerNames := make(map[string]struct{})
		for _, c := range sandbox.Spec.PodTemplate.Spec.InitContainers {
			allContainerNames[c.Name] = struct{}{}
		}
		for _, c := range sandbox.Spec.PodTemplate.Spec.Containers {
			allContainerNames[c.Name] = struct{}{}
		}
		for containerName := range envsByContainer {
			if _, ok := allContainerNames[containerName]; !ok {
				err := fmt.Errorf("target container %q not found in template", containerName)
				// To provide a more helpful error, we find which env var caused it.
				for _, e := range envsByContainer[containerName] {
					err = fmt.Errorf("target container %q not found in template for environment variable %q", containerName, e.Name)
					break
				}
				logger.Error(err, "Environment variable injection rejected: container not found", "claimName", claim.Name)
				return nil, err
			}
		}

		// Inject into init containers
		for i := range sandbox.Spec.PodTemplate.Spec.InitContainers {
			container := &sandbox.Spec.PodTemplate.Spec.InitContainers[i]
			if envs, ok := envsByContainer[container.Name]; ok {
				if err := r.injectEnvs(logger, container, envs, template.Spec.EnvVarsInjectionPolicy, claim.Name); err != nil {
					return nil, err
				}
			}
		}

		// Inject into regular containers
		for i := range sandbox.Spec.PodTemplate.Spec.Containers {
			container := &sandbox.Spec.PodTemplate.Spec.Containers[i]
			var envsToInject []extensionsv1alpha1.EnvVar
			if envs, ok := envsByContainer[container.Name]; ok {
				envsToInject = append(envsToInject, envs...)
			}
			if i == 0 { // Default envs go to the first main container
				envsToInject = append(envsToInject, defaultEnvs...)
			}
			if len(envsToInject) > 0 {
				if err := r.injectEnvs(logger, container, envsToInject, template.Spec.EnvVarsInjectionPolicy, claim.Name); err != nil {
					return nil, err
				}
			}
		}
	}

	// TODO: this is a workaround, remove replica assignment related issue #202
	replicas := int32(1)
	sandbox.Spec.Replicas = &replicas

	// Apply secure defaults to the sandbox pod spec
	ApplySandboxSecureDefaults(template, &sandbox.Spec.PodTemplate.Spec)

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

	logger.Info("Created sandbox for claim", "claim", claim.Name, "sandbox", sandbox.Name, "isReady", false, "duration", time.Since(claim.CreationTimestamp.Time))

	if r.Recorder != nil {
		r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, "SandboxProvisioned", "Provisioning", "Created Sandbox %q", sandbox.Name)
	}

	asmetrics.RecordSandboxClaimCreation(claim.Namespace, claim.Spec.TemplateRef.Name, asmetrics.LaunchTypeCold, "none", "not_ready")

	return sandbox, nil
}

func (r *SandboxClaimReconciler) getOrCreateSandbox(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, _ *extensionsv1alpha1.SandboxTemplate) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Executing getOrCreateSandbox", "claim", claim.Name)

	// Check if a previously adopted sandbox is recorded in claim status
	if statusName := claim.Status.SandboxStatus.Name; statusName != "" {
		logger.V(1).Info("Checking status for sandbox name", "claim.Status.SandboxStatus.Name", statusName, "claim", claim.Name)
		sandbox := &v1alpha1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: statusName}, sandbox); err == nil {
			if metav1.IsControlledBy(sandbox, claim) {
				logger.Info("Found existing adopted sandbox from status", "claim.Status.SandboxStatus.Name", statusName, "claim", claim.Name)
				return sandbox, nil
			}
		} else if !k8errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get sandbox %q from status: %w", statusName, err)
		}
	}

	// Try name-based lookup (sandbox created by createSandbox uses claim.Name)
	logger.V(1).Info("Trying name-based lookup for sandbox", "claim", claim.Name)
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		sandbox = nil
		if !k8errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get sandbox %q: %w", claim.Name, err)
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

	policy := getWarmPoolPolicy(claim)

	// Preserve HEAD's new Env validation feature!
	if policy != extensionsv1alpha1.WarmPoolPolicyNone && len(claim.Spec.Env) > 0 {
		err := fmt.Errorf("custom environment variables are not supported when using a warm pool")
		logger.Error(err, "Invalid configuration", "claim", claim.Name)
		return nil, err
	}

	if policy == extensionsv1alpha1.WarmPoolPolicyNone {
		logger.Info("Skipping warm pool adoption based on warmpool policy", "claim", claim.Name, "warmpool", policy)
		return nil, nil
	}

	// Go to the custom queue instead of standard r.List()
	adopted, err := r.adoptSandboxFromCandidates(ctx, claim)
	if err != nil {
		return nil, err
	}
	if adopted != nil {
		return adopted, nil
	}

	// No warm pool sandbox available; caller decides whether to create
	return nil, nil
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

func (r *SandboxClaimReconciler) getTimingPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			key := types.NamespacedName{Name: e.Object.GetName(), Namespace: e.Object.GetNamespace()}
			r.observedTimes.LoadOrStore(key, time.Now())
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			key := types.NamespacedName{Name: e.ObjectNew.GetName(), Namespace: e.ObjectNew.GetNamespace()}
			r.observedTimes.LoadOrStore(key, time.Now())
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			key := types.NamespacedName{Name: e.Object.GetName(), Namespace: e.Object.GetNamespace()}
			r.observedTimes.Delete(key)
			return true
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	r.MaxConcurrentReconciles = concurrentWorkers

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxClaim{}, builder.WithPredicates(r.getTimingPredicate())).
		Owns(&v1alpha1.Sandbox{}).
		Watches(&v1alpha1.Sandbox{}, &sandboxEventHandler{sandboxQueue: r.WarmSandboxQueue}).
		Watches(&extensionsv1alpha1.SandboxTemplate{}, &templateEventHandler{sandboxQueue: r.WarmSandboxQueue}).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}

// cleanupLegacyNetworkPolicy cleans up any deprecated per-claim NetworkPolicies.
func (r *SandboxClaimReconciler) cleanupLegacyNetworkPolicy(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) error {
	logger := log.FromContext(ctx)
	npKey := types.NamespacedName{Name: claim.Name + "-network-policy", Namespace: claim.Namespace}

	existingNP := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, npKey, existingNP); err == nil {

		// Verify this policy was actually created by this controller
		// before deleting it. We check if the SandboxClaim is the controller.
		controllerRef := metav1.GetControllerOf(existingNP)
		isControlledByClaim := controllerRef != nil && controllerRef.UID == claim.UID && controllerRef.Kind == "SandboxClaim"

		if !isControlledByClaim {
			// A user manually created a policy with our reserved name. We should not delete it, but log a warning so it can be resolved.
			logger.V(1).Info("Found NetworkPolicy with reserved name, but it is not controlled by this claim. Skipping deletion.", "name", existingNP.Name)
			return nil
		}

		// Use client.IgnoreNotFound to prevent benign race conditions
		// if the object is deleted between our Get and Delete calls.
		if deleteErr := r.Delete(ctx, existingNP); client.IgnoreNotFound(deleteErr) != nil {
			logger.Error(deleteErr, "Failed to clean up deprecated per-claim NetworkPolicy")
			return deleteErr
		}
		logger.Info("Cleaned up deprecated per-claim NetworkPolicy in favor of shared Template policy", "name", existingNP.Name)
	} else if !k8errors.IsNotFound(err) {
		logger.Error(err, "Failed to check cache for deprecated per-claim NetworkPolicy")
		return err
	}

	return nil
}

// getLaunchType determines the launch type based on the sandbox state.
func getLaunchType(sandbox *v1alpha1.Sandbox) string {
	if sandbox == nil {
		return asmetrics.LaunchTypeUnknown
	}
	if sandbox.Annotations[v1alpha1.SandboxPodNameAnnotation] != "" {
		return asmetrics.LaunchTypeWarm
	}
	return asmetrics.LaunchTypeCold
}

// recordClaimStartupLatency records the startup latency based on webhook annotation.
func (r *SandboxClaimReconciler) recordClaimStartupLatency(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, launchType string) {
	logger := log.FromContext(ctx)
	webhookSeenTimeStr := claim.Annotations[asmetrics.WebhookAnnotation]
	if webhookSeenTimeStr == "" {
		logger.V(1).Info("Webhook first seen annotation missing, skipping ClaimStartupLatency metric", "claim", claim.Name)
		return
	}
	webhookSeenTime, err := time.Parse(time.RFC3339Nano, webhookSeenTimeStr)
	if err != nil {
		logger.Error(err, "Failed to parse webhook first seen time", "value", webhookSeenTimeStr)
		return
	}
	duration := time.Since(webhookSeenTime)
	if duration < 0 {
		logger.Error(errors.New("negative duration"), "Webhook seen time is in the future", "duration", duration, "webhookSeenTime", webhookSeenTime)
		return
	}
	asmetrics.RecordClaimStartupLatency(webhookSeenTime, launchType, claim.Spec.TemplateRef.Name)
}

// recordControllerStartupLatency records the controller startup latency based on observed time.
func (r *SandboxClaimReconciler) recordControllerStartupLatency(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, launchType string) {
	logger := log.FromContext(ctx)
	if observedTimeString := claim.Annotations[asmetrics.ObservabilityAnnotation]; observedTimeString != "" {
		key := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
		defer r.observedTimes.Delete(key)

		observedTime, err := time.Parse(time.RFC3339Nano, observedTimeString)
		if err != nil {
			logger.Error(err, "Failed to parse controller observation time", "value", observedTimeString)
			return
		}
		asmetrics.RecordClaimControllerStartupLatency(observedTime, launchType, claim.Spec.TemplateRef.Name)
	}
}

// recordSandboxCreationLatency records the sandbox creation latency.
func (r *SandboxClaimReconciler) recordSandboxCreationLatency(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, launchType string) {
	if sandbox == nil || sandbox.CreationTimestamp.IsZero() {
		return
	}
	sandboxReady := meta.FindStatusCondition(sandbox.Status.Conditions, string(v1alpha1.SandboxConditionReady))
	if sandboxReady == nil || sandboxReady.Status != metav1.ConditionTrue || sandboxReady.LastTransitionTime.IsZero() {
		return
	}
	latency := sandboxReady.LastTransitionTime.Sub(sandbox.CreationTimestamp.Time)
	if latency >= 0 {
		asmetrics.RecordSandboxCreationLatency(latency, sandbox.Namespace, launchType, claim.Spec.TemplateRef.Name)
	}
}

// recordCreationLatencyMetric detects and records transitions to Ready state.
func (r *SandboxClaimReconciler) recordCreationLatencyMetric(
	ctx context.Context,
	claim *extensionsv1alpha1.SandboxClaim,
	oldStatus *extensionsv1alpha1.SandboxClaimStatus,
	sandbox *v1alpha1.Sandbox,
) {
	logger := log.FromContext(ctx)

	newStatus := &claim.Status
	newReady := meta.FindStatusCondition(newStatus.Conditions, string(v1alpha1.SandboxConditionReady))
	if newReady == nil || newReady.Status != metav1.ConditionTrue {
		return
	}

	// Do not record creation metric if we have already seen the ready state.
	oldReady := meta.FindStatusCondition(oldStatus.Conditions, string(v1alpha1.SandboxConditionReady))
	if oldReady != nil && oldReady.Status == metav1.ConditionTrue {
		return
	}

	launchType := getLaunchType(sandbox)

	sandboxName := "none"
	if sandbox != nil {
		sandboxName = sandbox.Name
	}
	logger.V(1).Info("SandboxClaim is marked as Ready", "claim", claim.Name, "sandbox", sandboxName, "duration", time.Since(claim.CreationTimestamp.Time))

	r.recordClaimStartupLatency(ctx, claim, launchType)
	r.recordControllerStartupLatency(ctx, claim, launchType)
	r.recordSandboxCreationLatency(claim, sandbox, launchType)
}

func hasSandboxExpiredCondition(conditions []metav1.Condition) bool {
	readyCondition := meta.FindStatusCondition(conditions, string(v1alpha1.SandboxConditionReady))
	return readyCondition != nil && readyCondition.Reason == v1alpha1.SandboxReasonExpired
}

func hasClaimExpiredCondition(conditions []metav1.Condition) bool {
	readyCondition := meta.FindStatusCondition(conditions, string(v1alpha1.SandboxConditionReady))
	return readyCondition != nil && readyCondition.Reason == extensionsv1alpha1.ClaimExpiredReason
}

// sandboxEventHandler implements handler.EventHandler for the SandboxClaimReconciler.
type sandboxEventHandler struct {
	sandboxQueue queue.SandboxQueue
}

func (h *sandboxEventHandler) Create(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.Update(ctx, event.UpdateEvent{ObjectOld: &v1alpha1.Sandbox{}, ObjectNew: e.Object}, q)
}

func (h *sandboxEventHandler) Update(ctx context.Context, e event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	newSandbox, ok := e.ObjectNew.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	oldSandbox, ok := e.ObjectOld.(*v1alpha1.Sandbox)
	if !ok {
		return
	}

	newAdoptable := isAdoptable(newSandbox) == nil
	oldAdoptable := isAdoptable(oldSandbox) == nil

	logger := log.FromContext(ctx)

	hashChanged := oldSandbox.Labels[sandboxTemplateRefHash] != newSandbox.Labels[sandboxTemplateRefHash]

	if (!oldAdoptable && newAdoptable) || (newAdoptable && hashChanged) {
		// Add sandbox only on transition to adoptable.
		key := queue.SandboxKey{
			Namespace: newSandbox.Namespace,
			Name:      newSandbox.Name,
		}
		logger.V(1).Info("Adding sandbox to warm pool queue", "templateRefHash", newSandbox.Labels[sandboxTemplateRefHash], "sandbox", key)
		h.sandboxQueue.Add(newSandbox.Labels[sandboxTemplateRefHash], key)
	}
}

func (h *sandboxEventHandler) Generic(_ context.Context, _ event.GenericEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	// Generic events are not typically used for pod lifecycle changes we care about.
}

func verifySandboxCandidate(candidate *v1alpha1.Sandbox, claim *extensionsv1alpha1.SandboxClaim) error {
	if err := isAdoptable(candidate); err != nil {
		return err
	}

	templateHash := sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name)
	if candidate.Labels[sandboxTemplateRefHash] != templateHash {
		return fmt.Errorf("incorrect template hash, expected %v, got %v", templateHash, candidate.Labels[sandboxTemplateRefHash])
	}
	return nil
}

func isAdoptable(candidate *v1alpha1.Sandbox) error {
	if !candidate.DeletionTimestamp.IsZero() {
		return fmt.Errorf("sandbox is deleted")
	}
	if _, ok := candidate.Labels[warmPoolSandboxLabel]; !ok {
		return fmt.Errorf("sandbox is missing the warm pool sandbox label")
	}
	if _, ok := candidate.Labels[sandboxTemplateRefHash]; !ok {
		return fmt.Errorf("sandbox is missing the sandbox template ref hash label")
	}

	controllerRef := metav1.GetControllerOf(candidate)
	if controllerRef != nil && controllerRef.Kind != "SandboxWarmPool" {
		return fmt.Errorf("sandbox is not managed by warm pool. Controller: %v", controllerRef)
	}
	return nil
}

func (h *sandboxEventHandler) Delete(ctx context.Context, e event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	sandbox, ok := e.Object.(*v1alpha1.Sandbox)
	if !ok {
		return
	}

	// Grab the hash to find which queue this pod lived in
	templateHash := sandbox.Labels[sandboxTemplateRefHash]

	if templateHash != "" {
		key := queue.SandboxKey{
			Namespace: sandbox.Namespace,
			Name:      sandbox.Name,
		}

		// Actively delete the Ghost Pod from the memory queue
		logger := log.FromContext(ctx)
		logger.V(1).Info("Removing deleted sandbox from warm pool queue", "sandbox", key)
		h.sandboxQueue.RemoveItem(templateHash, key)
	}
}

type templateEventHandler struct {
	sandboxQueue queue.SandboxQueue
}

func (h *templateEventHandler) Create(_ context.Context, _ event.CreateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}
func (h *templateEventHandler) Update(_ context.Context, _ event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}
func (h *templateEventHandler) Generic(_ context.Context, _ event.GenericEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *templateEventHandler) Delete(ctx context.Context, e event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	template, ok := e.Object.(*extensionsv1alpha1.SandboxTemplate)
	if !ok {
		return
	}

	templateHash := sandboxcontrollers.NameHash(template.Name)
	logger := log.FromContext(ctx)
	logger.Info("SandboxTemplate deleted, cleaning up memory queue", "template", template.Name, "hash", templateHash)

	// Actively drop the entire queue from memory
	h.sandboxQueue.RemoveQueue(templateHash)
}
