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
	"slices"
	"strings"
	"sync"
	"time"

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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

const ObservabilityAnnotation = "agents.x-k8s.io/controller-first-observed-at"

// ErrTemplateNotFound is a sentinel error indicating a SandboxTemplate was not found.
var ErrTemplateNotFound = errors.New("SandboxTemplate not found")

// ErrInvalidMetadata is a sentinel error indicating additionalPodMetadata was invalid.
var ErrInvalidMetadata = errors.New("invalid additionalPodMetadata")

// getWarmPoolPolicy returns the effective warm pool policy for a claim.
func getWarmPoolPolicy(claim *extensionsv1alpha1.SandboxClaim) extensionsv1alpha1.WarmPoolPolicy {
	if claim.Spec.WarmPool != nil {
		return *claim.Spec.WarmPool
	}
	return extensionsv1alpha1.WarmPoolPolicyDefault
}

// SandboxClaimReconciler reconciles a SandboxClaim object
type SandboxClaimReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
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
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
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

	// Start Tracing Span
	ctx, end := r.Tracer.StartSpan(ctx, claim, "ReconcileSandboxClaim", nil)
	defer end()

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Initialize trace ID and observation time for active resources missing them.
	// Inline patch, no early return, to avoid forcing a second reconcile cycle.
	traceContext := r.Tracer.GetTraceContext(ctx)
	needObservabilityPatch := claim.Annotations[ObservabilityAnnotation] == ""
	needTraceContextPatch := traceContext != "" && (claim.Annotations[asmetrics.TraceContextAnnotation] == "")

	if needObservabilityPatch || needTraceContextPatch {
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Annotations == nil {
			claim.Annotations = make(map[string]string)
		}
		if needObservabilityPatch {
			key := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
			if val, ok := r.observedTimes.Load(key); ok {
				claim.Annotations[ObservabilityAnnotation] = val.(time.Time).Format(time.RFC3339Nano)
			} else {
				now := time.Now()
				claim.Annotations[ObservabilityAnnotation] = now.Format(time.RFC3339Nano)
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

	if !hasExpiredCondition(originalClaimStatus.Conditions) && hasExpiredCondition(claim.Status.Conditions) {
		if r.Recorder != nil {
			r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, extensionsv1alpha1.ClaimExpiredReason, "Claim Expired", "Claim expired")
		}
	}

	if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
		errs := errors.Join(reconcileErr, updateErr)
		logger.V(1).Info("Sandboxclaim UpdateStatus error encountered", "errors", errs, "request", req.NamespacedName)
		return ctrl.Result{}, errs
	}

	r.recordCreationLatencyMetric(ctx, claim, originalClaimStatus, sandbox)

	// Determine Result
	var result ctrl.Result
	if !claimExpired && timeLeft > 0 {
		result = ctrl.Result{RequeueAfter: timeLeft}
	}

	// Suppress expected user errors (like missing templates) to avoid crash loops
	if errors.Is(reconcileErr, ErrTemplateNotFound) || errors.Is(reconcileErr, ErrInvalidMetadata) {
		logger.V(1).Info("Sandboxclaim suppressed error(s) encountered", "error", reconcileErr, "request", req.NamespacedName)
		return result, nil
	}

	logger.V(1).Info("End of Reconcile loop SandboxClaim", "result", result, "error", reconcileErr, "request", req.NamespacedName)
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
			if npErr := r.reconcileNetworkPolicy(ctx, claim, template); npErr != nil {
				logger.Error(npErr, "network policy reconcile failed after adoption (non-fatal)", "claim", claim.Name)
			}

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
	if templateErr != nil {
		return nil, ErrTemplateNotFound
	}

	if npErr := r.reconcileNetworkPolicy(ctx, claim, template); npErr != nil {
		return nil, fmt.Errorf("failed to reconcile network policy: %w", npErr)
	}

	return r.createSandbox(ctx, claim, template)
}

// reconcileExpired ensures the Sandbox is deleted for Retained claims.
func (r *SandboxClaimReconciler) reconcileExpired(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling Expired claim", "claim", claim.Name)
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
		logger.Info("Deleting Sandbox because Claim expired (Policy=Retain)", "Sandbox", sandbox.Name, "claim", claim.Name)
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
	if isSandboxExpired(sandbox) {
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

	if sandbox != nil {
		claim.Status.SandboxStatus.Name = sandbox.Name
		claim.Status.SandboxStatus.PodIPs = sandbox.Status.PodIPs
	} else {
		claim.Status.SandboxStatus.Name = ""
		claim.Status.SandboxStatus.PodIPs = nil
	}
}

// adoptSandboxFromCandidates picks the best candidate and transfers ownership to the claim.
func (r *SandboxClaimReconciler) adoptSandboxFromCandidates(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, candidates []*v1alpha1.Sandbox) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)

	// Sort: ready sandboxes first, then by creation time (oldest first)
	slices.SortFunc(candidates, func(a, b *v1alpha1.Sandbox) int {
		aReady := isSandboxReady(a)
		bReady := isSandboxReady(b)
		if aReady != bReady {
			if aReady {
				return -1 // a ready, b not ready -> a first
			}
			return 1 // b ready, a not ready -> b first
		}
		return a.CreationTimestamp.Time.Compare(b.CreationTimestamp.Time)
	})

	if len(candidates) == 0 {
		logger.Info("No warm pool candidates available, falling through to cold start", "claim", claim.Name)
		return nil, nil
	}

	// Determine the search range for collision avoidance.
	n := len(candidates)
	workerCount := r.MaxConcurrentReconciles
	if workerCount <= 0 {
		workerCount = 1
	}
	searchWindow := min(n, workerCount)

	// Compute a starting index deterministic to this specific Claim UID.
	hashValue := sandboxcontrollers.GetNumericHash(string(claim.UID))
	startIndex := int(hashValue % uint32(searchWindow))

	// Iterate through the entire list starting from the hashed offset.
	for i := 0; i < n; i++ {
		currIndex := (startIndex + i) % n
		adopted := candidates[currIndex]

		// Extract pool name from owner reference before clearing
		poolName := "none"
		if controllerRef := metav1.GetControllerOf(adopted); controllerRef != nil {
			poolName = controllerRef.Name
		}

		logger.Info("Attempting sandbox adoption", "sandbox candidate", adopted.Name, "warm pool", poolName, "claim", claim.Name)

		// Remove warm pool labels so the sandbox no longer appears in warm pool queries
		delete(adopted.Labels, warmPoolSandboxLabel)
		delete(adopted.Labels, sandboxTemplateRefHash)
		delete(adopted.Labels, v1alpha1.SandboxPodTemplateHashLabel)

		// Transfer ownership from SandboxWarmPool to SandboxClaim
		adopted.OwnerReferences = nil
		if err := controllerutil.SetControllerReference(claim, adopted, r.Scheme); err != nil {
			return nil, fmt.Errorf("failed to set controller reference on adopted sandbox: %w", err)
		}

		// Propagate trace context from claim
		if adopted.Annotations == nil {
			adopted.Annotations = make(map[string]string)
		}
		if traceContext, ok := claim.Annotations[asmetrics.TraceContextAnnotation]; ok {
			adopted.Annotations[asmetrics.TraceContextAnnotation] = traceContext
		}

		// Add sandbox ID label to pod template for NetworkPolicy targeting
		if adopted.Spec.PodTemplate.ObjectMeta.Labels == nil {
			adopted.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
		}
		adopted.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1alpha1.SandboxIDLabel] = string(claim.UID)
		adopted.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash] = sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name)

		// Merge metadata from claim
		if err := mergePodMetadata(&adopted.Spec.PodTemplate.ObjectMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
			return nil, err
		}

		// Update uses optimistic concurrency (resourceVersion) so concurrent
		// claims racing to adopt the same sandbox will conflict and retry.
		if err := r.Update(ctx, adopted); err != nil {
			if k8errors.IsConflict(err) || k8errors.IsNotFound(err) {
				// Another worker adopted this sandbox while we were processing; try next candidate.
				continue
			}
			logger.Error(err, "Failed to update adoption candidate sandbox", "sandbox candidate", adopted.Name, "claim", claim.Name)
			return nil, err
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

		return adopted, nil
	}

	logger.Info("Failed to adopt any sandbox after checking all candidates", "claim", claim.Name)
	return nil, nil // Return nil, nil to fall completely to cold start
}

// isSandboxReady checks if a sandbox has Ready=True condition
func isSandboxReady(sb *v1alpha1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(v1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
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
		if domain == "kubernetes.io" || domain == "k8s.io" || strings.HasSuffix(domain, ".agents.x-k8s.io") || domain == "agents.x-k8s.io" {
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
		for k, v := range claimMeta.Labels {
			templateMeta.Labels[k] = v
		}
	}

	// Merge annotations
	if len(claimMeta.Annotations) > 0 {
		if templateMeta.Annotations == nil {
			templateMeta.Annotations = make(map[string]string)
		}
		for k, v := range claimMeta.Annotations {
			templateMeta.Annotations[k] = v
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

	// Single List: ownership guard + adoption candidate scan.
	// This queries the informer cache (not the API server), so it's fast.
	logger.V(1).Info("Listing sandbox adoption candidates", "claim", claim.Name)
	allSandboxes := &v1alpha1.SandboxList{}
	if err := r.List(ctx, allSandboxes, client.InNamespace(claim.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}

	policy := getWarmPoolPolicy(claim)
	templateHash := sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name)
	var adoptionCandidates []*v1alpha1.Sandbox

	for i := range allSandboxes.Items {
		sb := &allSandboxes.Items[i]
		if !sb.DeletionTimestamp.IsZero() {
			continue
		}

		// Ownership guard: if this claim already owns a sandbox, return it
		if metav1.IsControlledBy(sb, claim) {
			logger.Info("Found existing owned sandbox", "sandbox", sb.Name, "claim", claim.Name)
			return sb, nil
		}

		// Skip warm pool adoption entirely if policy is "none"
		if policy == extensionsv1alpha1.WarmPoolPolicyNone {
			continue
		}

		// Collect adoption candidates from warm pool
		if _, ok := sb.Labels[warmPoolSandboxLabel]; !ok {
			continue
		}
		if sb.Labels[sandboxTemplateRefHash] != templateHash {
			continue
		}
		controllerRef := metav1.GetControllerOf(sb)
		if controllerRef != nil && controllerRef.Kind != "SandboxWarmPool" {
			continue
		}

		// If a specific pool is requested, only consider sandboxes from that pool
		if policy.IsSpecificPool() {
			specificPoolHash := sandboxcontrollers.NameHash(string(policy))
			if sb.Labels[warmPoolSandboxLabel] != specificPoolHash {
				continue
			}
		}

		adoptionCandidates = append(adoptionCandidates, sb)
	}

	if policy == extensionsv1alpha1.WarmPoolPolicyNone {
		logger.Info("Skipping warm pool adoption based on warmpool policy", "claim", claim.Name, "warmpool", policy)
		return nil, nil
	}

	// Try to adopt from warm pool
	if len(adoptionCandidates) > 0 {
		logger.V(1).Info("Found warm pool adoption candidates", "count", len(adoptionCandidates), "claim", claim.Name, "warmpool", policy)
		adopted, err := r.adoptSandboxFromCandidates(ctx, claim, adoptionCandidates)
		if err != nil {
			return nil, err
		}
		if adopted != nil {
			return adopted, nil
		}
	} else if policy.IsSpecificPool() {
		logger.Info("No available sandboxes in specified warm pool", "warmPool", string(policy), "claim", claim.Name)
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
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	r.MaxConcurrentReconciles = concurrentWorkers

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxClaim{}, builder.WithPredicates(r.getTimingPredicate())).
		Owns(&v1alpha1.Sandbox{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}

// reconcileNetworkPolicy ensures a NetworkPolicy exists for the claimed Sandbox.
func (r *SandboxClaimReconciler) reconcileNetworkPolicy(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, template *extensionsv1alpha1.SandboxTemplate) error {
	logger := log.FromContext(ctx)

	// Skip if the template opts out of managed network policies
	if template != nil && template.Spec.NetworkPolicyManagement == extensionsv1alpha1.NetworkPolicyManagementUnmanaged {
		return nil
	}

	// Cleanup Check: If missing, delete existing policy
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

		np.Spec.Ingress = templateNP.Ingress
		np.Spec.Egress = templateNP.Egress

		return controllerutil.SetControllerReference(claim, np, r.Scheme)
	})

	if err != nil {
		logger.Error(err, "Failed to create or update NetworkPolicy for claim")
		return err
	}

	logger.Info("Successfully reconciled NetworkPolicy for claim", "NetworkPolicy.Name", np.Name)
	return nil
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

	launchType := asmetrics.LaunchTypeCold
	// This is unlikely to happen; here for completeness only.
	if sandbox == nil {
		launchType = asmetrics.LaunchTypeUnknown
	} else if sandbox.Annotations[v1alpha1.SandboxPodNameAnnotation] != "" {
		// Existence of the SandboxPodNameAnnotation implies the pod was adopted from a warm pool.
		launchType = asmetrics.LaunchTypeWarm
	}

	sandboxName := "none"
	if sandbox != nil {
		sandboxName = sandbox.Name
	}
	logger.V(1).Info("SandboxClaim is marked as Ready", "claim", claim.Name, "sandbox", sandboxName, "duration", time.Since(claim.CreationTimestamp.Time))

	// SandboxClaim doesn't react to TemplateRef updates currently, so we don't need to handle the
	// startup latency when the TemplateRef is updated.
	asmetrics.RecordClaimStartupLatency(claim.CreationTimestamp.Time, launchType, claim.Spec.TemplateRef.Name)

	// Record controller startup latency
	if observedTimeString := claim.Annotations[ObservabilityAnnotation]; observedTimeString != "" {
		observedTime, err := time.Parse(time.RFC3339Nano, observedTimeString)
		if err != nil {
			logger.Error(err, "Failed to parse controller observation time", "value", observedTimeString)
		} else {
			asmetrics.RecordClaimControllerStartupLatency(observedTime, launchType, claim.Spec.TemplateRef.Name)
			// Clean up map entry after success
			r.observedTimes.Delete(types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace})
		}
	}

	// For cold launches, also record the time from Sandbox creation to Ready state to capture controller overhead.
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

// isSandboxExpired checks the Sandbox status condition set by the Core Controller
func isSandboxExpired(sandbox *v1alpha1.Sandbox) bool {
	return hasExpiredCondition(sandbox.Status.Conditions)
}

// hasExpiredCondition Helper to check if conditions list contains the expired reason
func hasExpiredCondition(conditions []metav1.Condition) bool {
	for _, cond := range conditions {
		if cond.Type == string(v1alpha1.SandboxConditionReady) {
			if cond.Reason == extensionsv1alpha1.ClaimExpiredReason || cond.Reason == v1alpha1.SandboxReasonExpired {
				return true
			}
		}
	}
	return false
}
