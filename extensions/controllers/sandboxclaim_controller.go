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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
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

// ErrWarmPoolNotFound is a sentinel error indicating a SandboxWarmPool was not found.
var ErrWarmPoolNotFound = errors.New("SandboxWarmPool not found")

// errAdoptionTriggeredRetry signals that warm-pool adoption was just completed for a
// sandbox and the claim must be requeued so a later pass observes the sandbox as
// controlled by this claim once the informer cache converges. It is a sentinel (not a
// generic error) so Reconcile can convert it into a bounded requeue instead
// of returning an error: an error would route through the exponential failure rate
// limiter, and because the same retry recurs each pass until the cache catches up the
// backoff compounds and, under concurrent claims, adoption tail latency balloons
// (#1107).
var errAdoptionTriggeredRetry = errors.New("triggered adoption completion, retry")

// adoptionCacheLagRequeueDelay is how long to wait before re-checking that a
// just-completed adoption is visible in the informer cache. Long enough to
// cover typical watch latency (so most claims converge in one extra pass) and
// to bound the rate of redundant adoption patches while the cache lags, but
// far below the multi-second exponential backoff it replaces.
const adoptionCacheLagRequeueDelay = 50 * time.Millisecond

var restrictedDomains = []string{"kubernetes.io", "k8s.io", "agents.x-k8s.io"}

var ErrCrossNamespaceAdoption = errors.New("cross-namespace adoption forbidden")

// ErrEnvVarsInjectionRejected is a sentinel error indicating environment variable injection was rejected.
var ErrEnvVarsInjectionRejected = errors.New("environment variable injection rejected")

// ErrVolumeClaimTemplatesDisallowed is a sentinel error indicating that volumeClaimTemplates are disallowed by the template.
var ErrVolumeClaimTemplatesDisallowed = errors.New("volume claim templates are disallowed by the template")

// ErrVolumeClaimTemplatesOverrideForbidden is a sentinel error indicating that overriding volume claim templates by name is forbidden.
var ErrVolumeClaimTemplatesOverrideForbidden = errors.New("overriding volume claim templates is forbidden by the template")

// ErrVolumeClaimTemplatesInvalid is a sentinel error indicating that the volumeClaimTemplates configuration is invalid.
var ErrVolumeClaimTemplatesInvalid = errors.New("invalid volume claim templates")

var suppressErrors = []error{
	ErrInvalidMetadata,
	ErrSandboxNotOwned,
	ErrEnvVarsInjectionRejected,
	ErrVolumeClaimTemplatesDisallowed,
	ErrVolumeClaimTemplatesOverrideForbidden,
	ErrVolumeClaimTemplatesInvalid,
}

// observedTimeEntry stores the first observed timestamp and the UID of the SandboxClaim.
// We store the UID to protect against stale data when a claim is deleted and a new one
// is created with the same name.
type observedTimeEntry struct {
	timestamp time.Time
	uid       types.UID
}

// observedTimeMap is a type-safe wrapper around sync.Map that only stores observedTimeEntry values.
type observedTimeMap struct {
	inner sync.Map
}

func (m *observedTimeMap) Load(key types.NamespacedName) (observedTimeEntry, bool) {
	val, ok := m.inner.Load(key)
	if !ok {
		return observedTimeEntry{}, false
	}
	return val.(observedTimeEntry), true
}

func (m *observedTimeMap) Store(key types.NamespacedName, entry observedTimeEntry) {
	m.inner.Store(key, entry)
}

func (m *observedTimeMap) Delete(key types.NamespacedName) {
	m.inner.Delete(key)
}

func (m *observedTimeMap) LoadOrStore(key types.NamespacedName, entry observedTimeEntry) (observedTimeEntry, bool) {
	actual, loaded := m.inner.LoadOrStore(key, entry)
	return actual.(observedTimeEntry), loaded
}

// triggeredAdoptionEntry records that completeAdoption already patched the
// named sandbox over to a claim (identified by UID), so cache-lag requeues
// can wait for the informer to converge without re-sending the patch.
type triggeredAdoptionEntry struct {
	uid     types.UID
	sandbox string
}

// triggeredAdoptionMap is a type-safe wrapper around sync.Map that only
// stores triggeredAdoptionEntry values.
type triggeredAdoptionMap struct {
	inner sync.Map
}

func (m *triggeredAdoptionMap) Load(key types.NamespacedName) (triggeredAdoptionEntry, bool) {
	val, ok := m.inner.Load(key)
	if !ok {
		return triggeredAdoptionEntry{}, false
	}
	return val.(triggeredAdoptionEntry), true
}

func (m *triggeredAdoptionMap) Store(key types.NamespacedName, entry triggeredAdoptionEntry) {
	m.inner.Store(key, entry)
}

func (m *triggeredAdoptionMap) Delete(key types.NamespacedName) {
	m.inner.Delete(key)
}

// SandboxClaimReconciler reconciles a SandboxClaim object.
type SandboxClaimReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	WarmSandboxQueue        queue.SandboxQueue
	Recorder                events.EventRecorder
	Tracer                  asmetrics.Instrumenter
	MaxConcurrentReconciles int
	observedTimes           observedTimeMap
	triggeredAdoptions      triggeredAdoptionMap
	AllowedLabelDomains     []string
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
	claim := &extensionsv1beta1.SandboxClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if k8errors.IsNotFound(err) {
			// Fallback cleanup to prevent memory leaks if the delete predicate was missed or a stale request is processed.
			r.observedTimes.Delete(req.NamespacedName)
			r.triggeredAdoptions.Delete(req.NamespacedName)
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
	var initialAttrs map[string]string
	if claim.Labels != nil {
		if val, ok := claim.Labels[v1beta1.CreatedByLabel]; ok && val != "" {
			initialAttrs = map[string]string{
				v1beta1.CreatedByLabel: asmetrics.NormalizeCreatedBy(val),
			}
		}
	}
	ctx, end := r.Tracer.StartSpan(ctx, claim, "ReconcileSandboxClaim", initialAttrs)
	defer end()

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Initialize trace ID and observation time for active resources missing them.
	if err := r.initializeAnnotations(ctx, claim); err != nil {
		return ctrl.Result{}, err
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
			r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, extensionsv1beta1.ClaimExpiredReason, "Claim Expired", "Claim expired")
		}
		return ctrl.Result{RequeueAfter: immediateRequeueDelay}, nil
	}
	logger.V(1).Info("Expiration check", "isExpired", claimExpired, "timeLeft", timeLeft, "request", req.NamespacedName)

	// Handle "Delete" and "DeleteForeground" policies immediately.
	// If we delete the claim, we return immediately.
	// Continuing would try to update the status of a deleted object, causing a crash/error.
	if claimExpired && claim.Spec.Lifecycle != nil &&
		(claim.Spec.Lifecycle.ShutdownPolicy == extensionsv1beta1.ShutdownPolicyDelete ||
			claim.Spec.Lifecycle.ShutdownPolicy == extensionsv1beta1.ShutdownPolicyDeleteForeground) {

		policy := claim.Spec.Lifecycle.ShutdownPolicy
		logger.Info("Deleting Claim because time has expired", "shutdownPolicy", policy, "claim", claim.Name)
		if r.Recorder != nil {
			r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, extensionsv1beta1.ClaimExpiredReason, "Deleting", fmt.Sprintf("Deleting Claim (ShutdownPolicy=%s)", policy))
		}

		deleteOpts := []client.DeleteOption{}
		if policy == extensionsv1beta1.ShutdownPolicyDeleteForeground {
			deleteOpts = append(deleteOpts, client.PropagationPolicy(metav1.DeletePropagationForeground))
		}

		if err := r.Delete(ctx, claim, deleteOpts...); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	// Manage Resources based on State
	var sandbox *v1beta1.Sandbox
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
			r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, extensionsv1beta1.ClaimExpiredReason, "Claim Expired", "Claim expired")
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

	// Requeue if dependency is missing, but don't return error to avoid log spam
	if errors.Is(reconcileErr, ErrWarmPoolNotFound) || errors.Is(reconcileErr, ErrTemplateNotFound) {
		if errors.Is(reconcileErr, ErrWarmPoolNotFound) {
			logger.V(1).Info("SandboxWarmPool not found yet, will retry", "warmPool", claim.Spec.WarmPoolRef.Name, "error", reconcileErr)
		} else {
			logger.V(1).Info("SandboxTemplate of the warmpool not found yet, will retry", "warmPool", claim.Spec.WarmPoolRef.Name, "error", reconcileErr)
		}

		// TODO: This 1-minute requeue creates a latency regression vs an immediate watch trigger.
		// Consider adding a lightweight SandboxTemplate -> claims map watch to reconcile promptly.
		requeueDelay := 1 * time.Minute
		if result.RequeueAfter > 0 && result.RequeueAfter < requeueDelay {
			requeueDelay = result.RequeueAfter
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	// Adoption was just triggered for a warm-pool sandbox. The sandbox is now patched
	// to us, but the informer cache may still show the warm-pool owner. Requeue
	// with a bounded delay to let the cache converge, WITHOUT returning an
	// error: returning an error would route through the exponential failure rate
	// limiter (and, under bursts, the shared 10qps bucket limiter), and because this
	// same retry recurs on each pass until the cache catches up the backoff compounds
	// (5ms*(2^k-1)) and adoption tail latency balloons (#1107). The nil error lets the
	// workqueue Forget the key, resetting the failure counter. Status is intentionally
	// not finalized with the sandbox on this pass (sandbox is nil here), preserving the
	// duplicate-adoption protection during cache lag.
	if errors.Is(reconcileErr, errAdoptionTriggeredRetry) {
		logger.V(4).Info("Adoption triggered; requeueing to let cache converge", "claim", claim.Name, "error", reconcileErr)
		requeueDelay := adoptionCacheLagRequeueDelay
		if result.RequeueAfter > 0 && result.RequeueAfter < requeueDelay {
			requeueDelay = result.RequeueAfter
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	// Suppress user configuration and validation errors to avoid crash loops
	if shouldSuppressError(reconcileErr) {
		logger.V(1).Info("Sandboxclaim suppressed error(s) encountered", "error", reconcileErr, "request", req.NamespacedName)
		return result, nil
	}

	logger.V(1).Info("End of Reconcile loop SandboxClaim", "result", result, "error", reconcileErr, "request", req.NamespacedName)
	return result, reconcileErr
}

// initializeAnnotations initializes trace ID and observation time for active resources missing them.
func (r *SandboxClaimReconciler) initializeAnnotations(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) error {
	traceContext := r.Tracer.GetTraceContext(ctx)
	needObservabilityPatch := claim.Annotations[asmetrics.ObservabilityAnnotation] == ""
	needTraceContextPatch := traceContext != "" && (claim.Annotations[asmetrics.TraceContextAnnotation] == "")

	if needObservabilityPatch || needTraceContextPatch {
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Annotations == nil {
			claim.Annotations = make(map[string]string)
		}
		if needObservabilityPatch {
			timestamp := r.getOrRecordObservedTime(claim)
			claim.Annotations[asmetrics.ObservabilityAnnotation] = timestamp.Format(time.RFC3339Nano)
		}
		if needTraceContextPatch {
			claim.Annotations[asmetrics.TraceContextAnnotation] = traceContext
		}
		if err := r.Patch(ctx, claim, patch); err != nil {
			return err
		}
	}
	return nil
}

// checkExpiration calculates if the claim is expired and how much time is left.
func (r *SandboxClaimReconciler) checkExpiration(claim *extensionsv1beta1.SandboxClaim) (bool, time.Duration) {
	if claim.Spec.Lifecycle == nil {
		return false, 0
	}

	finishedCondition := lifecycle.FinishedCondition(claim.Status.Conditions, string(v1beta1.SandboxConditionFinished))
	return lifecycle.TimeLeft(time.Now(), claim.Spec.Lifecycle.ShutdownTime, claim.Spec.Lifecycle.TTLSecondsAfterFinished, finishedCondition)
}

// reconcileActive handles the creation and updates of running sandboxes.
func (r *SandboxClaimReconciler) reconcileActive(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (*v1beta1.Sandbox, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling active claim", "claim", claim.Name)

	// Upfront validation of additional metadata to skip unnecessary processing
	if err := r.validateAdditionalPodMetadata(&claim.Spec.AdditionalPodMetadata); err != nil {
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
			logger.Error(templateErr, "failed to get template of the warmpool for network policy reconciliation (non-fatal)", "claim", claim.Name, "warmPool", claim.Spec.WarmPoolRef.Name)

			// If we can't get the template but we have metadata to propagate, we should fail
			// to ensure consistency and enforce the "No Overrides" rule.
			if len(claim.Spec.AdditionalPodMetadata.Labels) > 0 || len(claim.Spec.AdditionalPodMetadata.Annotations) > 0 {
				return nil, fmt.Errorf("failed to get template for metadata propagation: %w", templateErr)
			}
		}

		if template != nil {
			patch := client.MergeFrom(sandbox.DeepCopy())
			// Check if metadata needs update
			var mergedMeta v1beta1.PodMetadata
			template.Spec.PodTemplate.ObjectMeta.DeepCopyInto(&mergedMeta)

			// Preserve system-injected labels
			if mergedMeta.Labels == nil {
				mergedMeta.Labels = make(map[string]string)
			}
			templateHash := SandboxTemplateRefHash(template.Name)
			mergedMeta.Labels[extensionsv1beta1.SandboxIDLabel] = string(claim.UID)
			mergedMeta.Labels[sandboxTemplateRefHash] = templateHash
			// Sync the created-by label to the Pod template. If the claim does not have it,
			// we remove it to ensure consistency with cold starts and prevent stale label values.
			if val, ok := claim.Labels[v1beta1.CreatedByLabel]; ok && val != "" {
				mergedMeta.Labels[v1beta1.CreatedByLabel] = val
			} else {
				delete(mergedMeta.Labels, v1beta1.CreatedByLabel)
			}

			if err := r.mergePodMetadata(&mergedMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
				return nil, err
			}

			needsUpdate := !equality.Semantic.DeepEqual(&mergedMeta, &sandbox.Spec.PodTemplate.ObjectMeta)
			if sandbox.Labels[sandboxTemplateRefHash] != templateHash {
				if sandbox.Labels == nil {
					sandbox.Labels = make(map[string]string)
				}
				sandbox.Labels[sandboxTemplateRefHash] = templateHash
				needsUpdate = true
			}
			if val, ok := claim.Labels[v1beta1.CreatedByLabel]; ok && val != "" {
				if sandbox.Labels[v1beta1.CreatedByLabel] != val {
					if sandbox.Labels == nil {
						sandbox.Labels = make(map[string]string)
					}
					sandbox.Labels[v1beta1.CreatedByLabel] = val
					needsUpdate = true
				}
			} else {
				if _, exists := sandbox.Labels[v1beta1.CreatedByLabel]; exists {
					delete(sandbox.Labels, v1beta1.CreatedByLabel)
					needsUpdate = true
				}
			}

			if needsUpdate {
				logger.V(1).Info("Updating sandbox metadata to match claim", "claim", claim.Name, "sandbox", sandbox.Name)
				sandbox.Spec.PodTemplate.ObjectMeta = mergedMeta
				if updateErr := r.Patch(ctx, sandbox, patch); updateErr != nil {
					return sandbox, fmt.Errorf("failed to patch sandbox metadata for claim %q: %w", claim.Name, updateErr)
				}
			}
		}
		return sandbox, nil
	}

	// Cold path: no existing sandbox or warm pool candidate.
	// Need template to create from scratch.
	logger.V(1).Info("Cold path: no sandbox found, creating from template", "claim", claim.Name)
	template, templateErr := r.getTemplate(ctx, claim)
	if templateErr != nil {
		return nil, templateErr
	}

	return r.createSandbox(ctx, claim, template)
}

// reconcileExpired ensures the Sandbox is deleted for Retained claims.
func (r *SandboxClaimReconciler) reconcileExpired(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (*v1beta1.Sandbox, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling Expired claim", "claim", claim.Name)

	// Fall back to claim.Name when status is unset.
	statusName := claim.Name
	if claim.Status.SandboxStatus.Name != "" {
		statusName = claim.Status.SandboxStatus.Name
	}

	sandbox := &v1beta1.Sandbox{}
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

func (r *SandboxClaimReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1beta1.SandboxClaimStatus, claim *extensionsv1beta1.SandboxClaim) error {
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

	oldClaim := claim.DeepCopy()
	oldClaim.Status = *oldStatus

	patch := client.MergeFrom(oldClaim)

	if err := r.Status().Patch(ctx, claim, patch); err != nil {
		logger.Error(err, "Failed to patch sandboxclaim status")
		return err
	}

	logger.V(4).Info("Successfully patched sandboxclaim status",
		"name", claim.Name,
		"namespace", claim.Namespace,
		"observedGeneration", claim.Generation)
	return nil
}

func (r *SandboxClaimReconciler) computeReadyCondition(claim *extensionsv1beta1.SandboxClaim, sandbox *v1beta1.Sandbox, err error, isClaimExpired bool) metav1.Condition {
	if err != nil {
		reason := "ReconcilerError"
		if errors.Is(err, ErrTemplateNotFound) {
			reason = "TemplateNotFound"
			msg := strings.TrimSuffix(err.Error(), ": "+ErrTemplateNotFound.Error())
			return metav1.Condition{
				Type:               string(v1beta1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            msg,
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, ErrWarmPoolNotFound) {
			reason = "WarmPoolNotFound"
			return metav1.Condition{
				Type:               string(v1beta1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            fmt.Sprintf("SandboxWarmPool %q not found", claim.Spec.WarmPoolRef.Name),
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, errAdoptionTriggeredRetry) {
			// Benign retry signal, not a claim failure: adoption was patched and we
			// are only waiting for the informer cache to converge before finalizing.
			return metav1.Condition{
				Type:               string(v1beta1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             "AdoptionPending",
				Message:            "Warm-pool sandbox adoption triggered; waiting for cache to converge",
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, ErrInvalidMetadata) {
			reason = "InvalidMetadata"
			return metav1.Condition{
				Type:               string(v1beta1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            err.Error(),
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, ErrEnvVarsInjectionRejected) {
			reason = "EnvVarsInjectionRejected"
			return metav1.Condition{
				Type:               string(v1beta1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            err.Error(),
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, ErrSandboxNotOwned) {
			return metav1.Condition{
				Type:               string(v1beta1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             extensionsv1beta1.ClaimExpiredReason,
				Message:            fmt.Sprintf("Claim expired. %v; deletion skipped.", err),
				ObservedGeneration: claim.Generation,
			}
		}
		if errors.Is(err, ErrVolumeClaimTemplatesDisallowed) ||
			errors.Is(err, ErrVolumeClaimTemplatesOverrideForbidden) ||
			errors.Is(err, ErrVolumeClaimTemplatesInvalid) {
			return metav1.Condition{
				Type:               string(v1beta1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             "VolumeClaimTemplatesError",
				Message:            err.Error(),
				ObservedGeneration: claim.Generation,
			}
		}
		return metav1.Condition{
			Type:               string(v1beta1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "Error seen: " + err.Error(),
			ObservedGeneration: claim.Generation,
		}
	}

	if isClaimExpired {
		return metav1.Condition{
			Type:               string(v1beta1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             extensionsv1beta1.ClaimExpiredReason,
			Message:            "Claim expired. Sandbox cleanup initiated.",
			ObservedGeneration: claim.Generation,
		}
	}

	if sandbox == nil {
		// Only handle genuine missing sandbox here (expired case is handled above)
		return metav1.Condition{
			Type:               string(v1beta1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             "SandboxMissing",
			Message:            "Sandbox does not exist",
			ObservedGeneration: claim.Generation,
		}
	}

	// Check if Core Controller marked it as Expired
	if hasSandboxExpiredCondition(sandbox.Status.Conditions) {
		return metav1.Condition{
			Type:               string(v1beta1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             v1beta1.SandboxReasonExpired,
			Message:            "Underlying Sandbox resource has expired independently of the Claim.",
			ObservedGeneration: claim.Generation,
		}
	}

	// Forward the condition from Sandbox Status
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == string(v1beta1.SandboxConditionReady) {
			return condition
		}
	}

	return metav1.Condition{
		Type:               string(v1beta1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             "SandboxNotReady",
		Message:            "Sandbox is not ready",
		ObservedGeneration: claim.Generation,
	}
}

func (r *SandboxClaimReconciler) computeAndSetStatus(claim *extensionsv1beta1.SandboxClaim, sandbox *v1beta1.Sandbox, err error, isClaimExpired bool) {
	// A cache-lag adoption retry is a benign look-again, not a state change. If the
	// claim status was already finalized with a sandbox (the adoption pass itself, or
	// a controller restart racing a stale informer), leave the recorded Name/PodIPs
	// and existing conditions untouched instead of transiently wiping them.
	if sandbox == nil && errors.Is(err, errAdoptionTriggeredRetry) && claim.Status.SandboxStatus.Name != "" {
		return
	}
	readyCondition := r.computeReadyCondition(claim, sandbox, err, isClaimExpired)
	meta.SetStatusCondition(&claim.Status.Conditions, readyCondition)
	r.syncFinishedCondition(claim, sandbox, isClaimExpired)

	if sandbox != nil {
		claim.Status.SandboxStatus.Name = sandbox.Name
		claim.Status.SandboxStatus.PodIPs = sandbox.Status.PodIPs
	} else if err == nil || errors.Is(err, ErrSandboxNotOwned) {
		// Only clear bound sandbox identity when there is no error (sandbox legitimately deleted or unbound)
		// or when ownership verification fails. Never clear on transient lookup or patch errors, as wiping
		// status.sandbox.name forces a fallback to cold-start on the next reconcile retry.
		claim.Status.SandboxStatus.Name = ""
		claim.Status.SandboxStatus.PodIPs = nil
	}
}

func (r *SandboxClaimReconciler) syncFinishedCondition(claim *extensionsv1beta1.SandboxClaim, sandbox *v1beta1.Sandbox, isClaimExpired bool) {
	if sandbox != nil {
		finishedCondition := meta.FindStatusCondition(sandbox.Status.Conditions, string(v1beta1.SandboxConditionFinished))
		if finishedCondition != nil {
			meta.SetStatusCondition(&claim.Status.Conditions, *finishedCondition)
		} else {
			meta.RemoveStatusCondition(&claim.Status.Conditions, string(v1beta1.SandboxConditionFinished))
		}
		return
	}

	if !isClaimExpired {
		meta.RemoveStatusCondition(&claim.Status.Conditions, string(v1beta1.SandboxConditionFinished))
	}
}

// ensureClaimIdentityLabels sets SandboxIDLabel (= claim.UID) on the given label map,
// initializing it if nil. Used on both Sandbox.metadata.labels and
// Sandbox.spec.podTemplate.ObjectMeta.Labels so the platform informer can resolve
// sandbox→claim identity from top-level Sandbox events (KEP-0174 only propagates to
// pod template labels, not top-level Sandbox labels).
func ensureClaimIdentityLabels(labels map[string]string, claim *extensionsv1beta1.SandboxClaim) map[string]string {
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[extensionsv1beta1.SandboxIDLabel] = string(claim.UID)
	// Propagate created-by label from the claim if present. If absent, explicitly
	// delete it to synchronize removal or prevent stale propagation from warm sandboxes.
	if val, ok := claim.Labels[v1beta1.CreatedByLabel]; ok && val != "" {
		labels[v1beta1.CreatedByLabel] = val
	} else {
		delete(labels, v1beta1.CreatedByLabel)
	}
	return labels
}

func (r *SandboxClaimReconciler) getCandidate(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (*v1beta1.Sandbox, queue.SandboxKey, error) {
	logger := log.FromContext(ctx)

	namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(claim.Namespace, claim.Spec.WarmPoolRef.Name)

	var skipped []queue.SandboxKey
	var fallbackSandbox *v1beta1.Sandbox
	var fallbackKey queue.SandboxKey
	var adoptingFallback bool

	// Instantly returns unused keys the moment we find a valid/ready candidate!
	defer func() {
		for _, key := range skipped {
			r.WarmSandboxQueue.Add(namespacedWarmPoolName, key)
		}
		// If we parked a fallback sandbox but never ended up adopting it (due to error or adopting a ready one), requeue it.
		if fallbackSandbox != nil && !adoptingFallback {
			r.WarmSandboxQueue.Add(namespacedWarmPoolName, fallbackKey)
		}
	}()

	// Strategy helper to pick candidate using in-memory NodeSpread and FIFO tie-breaking
	pickSmart := func(keys []queue.SandboxKey) (queue.SandboxKey, bool) {
		namespaceKeys := keys

		if len(namespaceKeys) == 0 {
			return queue.SandboxKey{}, false
		}
		if len(namespaceKeys) == 1 {
			return namespaceKeys[0], true
		}

		// Group candidates into scheduled vs unscheduled
		var scheduledKeys []queue.SandboxKey
		var unscheduledKeys []queue.SandboxKey
		for _, key := range namespaceKeys {
			if key.NodeName != "" {
				scheduledKeys = append(scheduledKeys, key)
			} else {
				unscheduledKeys = append(unscheduledKeys, key)
			}
		}

		// NodeSpread strategy: spread workloads by round-robinning nodes.
		// We count the remaining warmpool sandboxes per node in the queue.
		// The node with the most remaining sandboxes has been selected the least.
		if len(scheduledKeys) > 0 {
			nodeCounts := make(map[string]int)
			for _, key := range scheduledKeys {
				nodeCounts[key.NodeName]++
			}

			maxCount := 0
			for _, count := range nodeCounts {
				if count > maxCount {
					maxCount = count
				}
			}

			var bestCandidates []queue.SandboxKey
			for _, key := range scheduledKeys {
				if nodeCounts[key.NodeName] == maxCount {
					bestCandidates = append(bestCandidates, key)
				}
			}

			// Ties (equal counts) are resolved using oldest first (first in the slice)
			return bestCandidates[0], true
		}

		// Fall back to oldest first (FIFO) for unscheduled keys
		return unscheduledKeys[0], true
	}

	for {
		adoptedKey, ok := r.WarmSandboxQueue.GetWithStrategy(namespacedWarmPoolName, pickSmart)
		if !ok {
			// No more candidates in our namespace. If we found an unready fallback sandbox, return it.
			if fallbackSandbox != nil {
				adoptingFallback = true
				return fallbackSandbox, fallbackKey, nil
			}
			return nil, queue.SandboxKey{}, nil
		}

		adopted := &v1beta1.Sandbox{}
		err := r.Get(ctx, client.ObjectKey{Namespace: adoptedKey.Namespace, Name: adoptedKey.Name}, adopted)
		if err != nil {
			if k8errors.IsNotFound(err) {
				// Ghost Pod detected: It was deleted from the cluster but was still in our queue.
				// Ignore it and instantly pop the next one.
				continue
			}
			// For real errors, put the key back in line and error out
			r.WarmSandboxQueue.Add(namespacedWarmPoolName, adoptedKey)
			return nil, queue.SandboxKey{}, err
		}

		if err := verifySandboxCandidate(adopted, claim); err != nil {
			logger.V(1).Info("sandbox candidate can't be adopted", "sandbox", adopted.Name, "warmPool", claim.Spec.WarmPoolRef.Name, "reason", err.Error())
			// If it is a good sandbox in the wrong namespace, put it back.
			// (Though pickSmart makes this impossible, we keep it for safety).
			if errors.Is(err, ErrCrossNamespaceAdoption) {
				skipped = append(skipped, adoptedKey)
			}
			continue
		}

		// Candidate is valid! Now check if it is Ready
		if isSandboxReady(adopted) {
			// Found a Ready sandbox! Adopt it immediately.
			return adopted, adoptedKey, nil
		}

		// Sandbox is valid but NOT Ready.
		// Keep the first unready sandbox we found as fallback.
		if fallbackSandbox == nil {
			fallbackSandbox = adopted
			fallbackKey = adoptedKey
		} else {
			// Push subsequent unready sandboxes to skipped so they go back to the queue
			skipped = append(skipped, adoptedKey)
		}
	}
}

func (r *SandboxClaimReconciler) adoptSandboxFromCandidates(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (*v1beta1.Sandbox, error) {
	logger := log.FromContext(ctx)
	namespacedWarmPoolNameForQueue := queue.GetNamespacedWarmPoolName(claim.Namespace, claim.Spec.WarmPoolRef.Name)

	// Keep trying until we successfully adopt a sandbox, or run out of candidates
	for range 3 {
		adopted, adoptedKey, err := r.getCandidate(ctx, claim)
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
			if wpName := getWarmPoolName(adopted); wpName != "" {
				poolName = wpName
			}

			logger.Info("Attempting sandbox adoption", "sandbox candidate", adopted.Name, "warm pool", poolName, "claim", claim.Name)

			// Update claim to record adoption (optimistic lock)
			if claim.Annotations == nil {
				claim.Annotations = make(map[string]string)
			}
			claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] = adopted.Name
			if err := r.Update(ctx, claim); err != nil {
				r.WarmSandboxQueue.Add(namespacedWarmPoolNameForQueue, adoptedKey)
				if k8errors.IsConflict(err) {
					// Conflict means someone else updated the claim. We fail and retry.
					return false, err
				}
				logger.Error(err, "Failed to update claim for adoption", "claim", claim.Name, "sandbox", adopted.Name)
				return false, err
			}

			// Call helper to complete adoption (patch sandbox)
			if err := r.completeAdoption(ctx, claim, adopted); err != nil {
				if k8errors.IsNotFound(err) {
					return false, nil
				}
				r.WarmSandboxQueue.Add(namespacedWarmPoolNameForQueue, adoptedKey)
				if k8errors.IsConflict(err) {
					return false, nil
				}
				logger.Error(err, "Failed to complete adoption for candidate sandbox", "sandbox candidate", adopted.Name, "claim", claim.Name)
				return false, err
			}

			logger.Info("Successfully adopted sandbox from warm pool", "sandbox", adopted.Name, "claim", claim.Name)

			// Record the completed adoption so a later pass that still sees the
			// stale warm-pool-owned view (informer cache lag) waits via the
			// bounded requeue instead of re-sending the adoption patch.
			r.triggeredAdoptions.Store(
				types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace},
				triggeredAdoptionEntry{uid: claim.UID, sandbox: adopted.Name},
			)

			if r.Recorder != nil {
				r.Recorder.Eventf(claim, nil, corev1.EventTypeNormal, "SandboxAdopted", "Adoption", "Adopted warm pool Sandbox %q", adopted.Name)
			}

			podCondition := "not_ready"
			if isSandboxReady(adopted) {
				podCondition = "ready"
			}
			templateName := r.resolveTemplateName(adopted)
			asmetrics.RecordSandboxClaimCreation(claim.Namespace, templateName, asmetrics.LaunchTypeWarm, poolName, podCondition, claim.Labels[v1beta1.CreatedByLabel])

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

func (r *SandboxClaimReconciler) completeAdoption(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, adopted *v1beta1.Sandbox) error {
	// Take a snapshot of the sandbox BEFORE we mutate it to generate a clean JSON Patch.
	originalAdopted := adopted.DeepCopy()

	templateHash := adopted.Labels[sandboxTemplateRefHash]

	// Remove warm pool labels so the sandbox no longer appears in warm pool queries
	delete(adopted.Labels, warmPoolSandboxLabel)
	delete(adopted.Labels, v1beta1.DeprecatedSandboxPodTemplateHashLabel)
	delete(adopted.Labels, v1beta1.SandboxTemplateHashLabel)
	if adopted.Labels == nil {
		adopted.Labels = make(map[string]string)
	}
	adopted.Labels[v1beta1.SandboxLaunchTypeLabel] = v1beta1.SandboxLaunchTypeWarm
	// Remove the warm pool's default eviction annotation so the adopted sandbox
	// is protected from autoscaler scale-downs now that it hosts active state.
	// Custom template-specified overrides (e.g. "false") are explicitly kept.
	if adopted.Spec.PodTemplate.ObjectMeta.Annotations != nil && adopted.Spec.PodTemplate.ObjectMeta.Annotations[warmPoolEvictionAnnotation] == "true" {
		delete(adopted.Spec.PodTemplate.ObjectMeta.Annotations, warmPoolEvictionAnnotation)
	}

	// Transfer ownership from SandboxWarmPool to SandboxClaim
	adopted.OwnerReferences = nil
	if err := controllerutil.SetControllerReference(claim, adopted, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference on adopted sandbox: %w", err)
	}

	// Propagate trace context from claim
	if adopted.Annotations == nil {
		adopted.Annotations = make(map[string]string)
	}

	// Ensure the adopted sandbox records its pod name before it can be observed Ready.
	if podName := adopted.Annotations[v1beta1.SandboxPodNameAnnotation]; podName != adopted.Name {
		adopted.Annotations[v1beta1.SandboxPodNameAnnotation] = adopted.Name
	}

	if traceContext, ok := claim.Annotations[asmetrics.TraceContextAnnotation]; ok {
		adopted.Annotations[asmetrics.TraceContextAnnotation] = traceContext
	}

	// Propagate claim identity labels for discovery and NetworkPolicy targeting.
	adopted.Labels = ensureClaimIdentityLabels(adopted.Labels, claim)
	adopted.Spec.PodTemplate.ObjectMeta.Labels = ensureClaimIdentityLabels(adopted.Spec.PodTemplate.ObjectMeta.Labels, claim)

	// Resolve the template hash and metadata used by reconcileActive.
	template, templateErr := r.getTemplate(ctx, claim)
	if templateHash == "" && template != nil {
		templateHash = SandboxTemplateRefHash(template.Name)
	} else if templateHash == "" && templateErr != nil {
		log.FromContext(ctx).V(1).Info("Unable to set template ref hash label during adoption because template lookup failed", "sandbox", adopted.Name, "claim", claim.Name, "error", templateErr.Error())
	}

	// Keep the template ref hash on the adopted sandbox's top-level labels so
	// discovery by template hash keeps working after adoption.
	if templateHash != "" {
		adopted.Labels[sandboxTemplateRefHash] = templateHash
	}

	if templateErr == nil && template != nil {
		var mergedMeta v1beta1.PodMetadata
		template.Spec.PodTemplate.ObjectMeta.DeepCopyInto(&mergedMeta)

		if mergedMeta.Labels == nil {
			mergedMeta.Labels = make(map[string]string)
		}
		mergedMeta.Labels[extensionsv1beta1.SandboxIDLabel] = string(claim.UID)
		if templateHash != "" {
			mergedMeta.Labels[sandboxTemplateRefHash] = templateHash
		}
		// Propagate created-by label to the Pod template during adoption. If absent,
		// explicitly delete it to ensure it is not kept from the pre-warmed sandbox.
		if val, ok := claim.Labels[v1beta1.CreatedByLabel]; ok && val != "" {
			mergedMeta.Labels[v1beta1.CreatedByLabel] = val
		} else {
			delete(mergedMeta.Labels, v1beta1.CreatedByLabel)
		}

		if err := r.mergePodMetadata(&mergedMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
			return err
		}

		// Force an exact match
		adopted.Spec.PodTemplate.ObjectMeta = mergedMeta
	} else {
		// Fallback (just in case template is somehow missing)
		if templateHash != "" {
			adopted.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash] = templateHash
		}

		if err := r.mergePodMetadata(&adopted.Spec.PodTemplate.ObjectMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
			return err
		}
	}

	if err := r.Patch(ctx, adopted, client.MergeFrom(originalAdopted)); err != nil {
		return err
	}

	return nil
}

// isSandboxReady checks if a sandbox has Ready=True condition.
func isSandboxReady(sb *v1beta1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(v1beta1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
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
func (r *SandboxClaimReconciler) validateAdditionalPodMetadata(claimMeta *v1beta1.PodMetadata) error {
	if claimMeta == nil {
		return nil
	}

	allowedDomains := r.AllowedLabelDomains
	if len(allowedDomains) == 0 {
		allowedDomains = []string{"sandbox.users.io"} // Secure default fallback
	}

	validate := func(key, value string, isLabel bool) error {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			kind := "annotation"
			if isLabel {
				kind = "label"
			}
			return fmt.Errorf("invalid %s key: %q: %s", kind, key, strings.Join(errs, "; "))
		}

		// Block spoofing of system components
		if isLabel && strings.EqualFold(key, "app") && strings.EqualFold(value, "sandbox-router") {
			return fmt.Errorf("restricted system label value: %q=%q is not allowed in AdditionalPodMetadata", key, value)
		}

		parts := strings.SplitN(key, "/", 2)
		domain := ""
		if len(parts) > 1 {
			domain = strings.ToLower(parts[0])
		} else if isLabel {
			return fmt.Errorf("label %q must have a domain prefix (e.g. 'sandbox.users.io/my-label') to prevent opting into unintended policy domains", key)
		}

		if isLabel {
			// Strict Allowlist for labels
			allowed := false
			for _, d := range allowedDomains {
				if domain == d || strings.HasSuffix(domain, "."+d) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("label domain %q is not in the allowlist", domain)
			}
		} else {
			// For annotations, we use the blocklist
			if isRestrictedDomain(domain) {
				return fmt.Errorf("restricted system domain: %q is not allowed in AdditionalPodMetadata", key)
			}
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
func (r *SandboxClaimReconciler) mergePodMetadata(templateMeta *v1beta1.PodMetadata, claimMeta *v1beta1.PodMetadata) error {
	if err := r.validateAdditionalPodMetadata(claimMeta); err != nil {
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

func (r *SandboxClaimReconciler) injectEnvs(logger logr.Logger, container *corev1.Container, envsToInject []extensionsv1beta1.EnvVar, policy extensionsv1beta1.EnvVarsInjectionPolicy, claimName string) error {
	if policy == extensionsv1beta1.EnvVarsInjectionPolicyAllowed && len(container.EnvFrom) > 0 {
		return fmt.Errorf("%w: container %q uses EnvFrom sources; Allowed policy cannot safely prevent overriding EnvFrom-provided variables", ErrEnvVarsInjectionRejected, container.Name)
	}

	for _, claimEnv := range envsToInject {
		existingIdx := -1
		for j, env := range container.Env {
			if env.Name == claimEnv.Name {
				existingIdx = j
				break
			}
		}

		if existingIdx >= 0 {
			if policy != extensionsv1beta1.EnvVarsInjectionPolicyOverrides {
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

func (r *SandboxClaimReconciler) createSandbox(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, template *extensionsv1beta1.SandboxTemplate) (*v1beta1.Sandbox, error) {
	logger := log.FromContext(ctx)

	if template == nil {
		logger.Error(ErrTemplateNotFound, "cannot create sandbox: template of the warmpool not found", "warmPool", claim.Spec.WarmPoolRef.Name)
		return nil, ErrTemplateNotFound
	}

	logger.Info("creating sandbox from template", "template", template.Name)
	sandbox := &v1beta1.Sandbox{
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
	sandbox.Annotations[v1beta1.SandboxTemplateRefAnnotation] = template.Name

	sandbox.Spec.SandboxBlueprint = *template.Spec.SandboxBlueprint.DeepCopy()
	// Merge volumeClaimTemplates from template and claim according to the template policy
	if len(claim.Spec.VolumeClaimTemplates) > 0 {
		resolvedVCTs, err := mergeVolumeClaimTemplates(
			template.Spec.VolumeClaimTemplates,
			claim.Spec.VolumeClaimTemplates,
			template.Spec.VolumeClaimTemplatesPolicy,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to merge volume claim templates: %w", err)
		}
		if len(resolvedVCTs) > 0 {
			sandbox.Spec.VolumeClaimTemplates = make([]v1beta1.PersistentVolumeClaimTemplate, len(resolvedVCTs))
			for i, vct := range resolvedVCTs {
				vct.DeepCopyInto(&sandbox.Spec.VolumeClaimTemplates[i])
			}
		}
	} else {
		// Validate the VolumeClaimTemplates from the SandboxTemplate.
		if err := validateVolumeClaimTemplates(template.Spec.VolumeClaimTemplates); err != nil {
			return nil, fmt.Errorf("invalid volume claim templates in template: %w", err)
		}
	}

	// Propagate claim identity labels for discovery and NetworkPolicy targeting.
	// Fork extension: also write SandboxIDLabel onto the top-level Sandbox metadata
	// (KEP-0174 only propagates to pod template labels; platform's informer reads
	// Sandbox.metadata.labels).
	templateHash := SandboxTemplateRefHash(template.Name)
	sandbox.Labels = ensureClaimIdentityLabels(sandbox.Labels, claim)
	sandbox.Labels[v1beta1.SandboxLaunchTypeLabel] = v1beta1.SandboxLaunchTypeCold
	sandbox.Labels[sandboxTemplateRefHash] = templateHash
	sandbox.Spec.PodTemplate.ObjectMeta.Labels = ensureClaimIdentityLabels(sandbox.Spec.PodTemplate.ObjectMeta.Labels, claim)
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash] = templateHash

	if err := r.mergePodMetadata(&sandbox.Spec.PodTemplate.ObjectMeta, &claim.Spec.AdditionalPodMetadata); err != nil {
		return nil, err
	}

	// Inject environment variables from the SandboxClaim
	if len(claim.Spec.Env) > 0 {
		if template.Spec.EnvVarsInjectionPolicy != extensionsv1beta1.EnvVarsInjectionPolicyAllowed && template.Spec.EnvVarsInjectionPolicy != extensionsv1beta1.EnvVarsInjectionPolicyOverrides {
			err := fmt.Errorf("%w: environment variable injection is not allowed by the template policy", ErrEnvVarsInjectionRejected)
			logger.Error(err, "Environment variable injection rejected", "claimName", claim.Name)
			return nil, err
		}

		// Group envs by container name for efficient lookup.
		envsByContainer := make(map[string][]extensionsv1beta1.EnvVar)
		defaultEnvs := []extensionsv1beta1.EnvVar{}
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
			var envsToInject []extensionsv1beta1.EnvVar
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

	asmetrics.RecordSandboxClaimCreation(claim.Namespace, template.Name, asmetrics.LaunchTypeCold, claim.Spec.WarmPoolRef.Name, "not_ready", claim.Labels[v1beta1.CreatedByLabel])

	return sandbox, nil
}

func mergeVolumeClaimTemplates(
	templateVCTs []v1beta1.PersistentVolumeClaimTemplate,
	claimVCTs []v1beta1.PersistentVolumeClaimTemplate,
	policy extensionsv1beta1.VolumeClaimTemplatesPolicy,
) ([]v1beta1.PersistentVolumeClaimTemplate, error) {
	if err := validateVolumeClaimTemplates(templateVCTs); err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}

	if len(claimVCTs) == 0 {
		return templateVCTs, nil
	}

	if err := validateVolumeClaimTemplates(claimVCTs); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}

	switch policy {
	case extensionsv1beta1.VolumeClaimTemplatesPolicyDisallowed, "":
		return nil, ErrVolumeClaimTemplatesDisallowed

	case extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed:
		// Check for any overrides (name match)
		templateMap := make(map[string]struct{}, len(templateVCTs))
		for _, vct := range templateVCTs {
			templateMap[vct.Name] = struct{}{}
		}
		for _, vct := range claimVCTs {
			if _, exists := templateMap[vct.Name]; exists {
				return nil, fmt.Errorf("%w: cannot override template volume %q", ErrVolumeClaimTemplatesOverrideForbidden, vct.Name)
			}
		}
		// Simply append claim VCTs to template VCTs
		merged := make([]v1beta1.PersistentVolumeClaimTemplate, 0, len(templateVCTs)+len(claimVCTs))
		merged = append(merged, templateVCTs...)
		merged = append(merged, claimVCTs...)
		return merged, nil

	case extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides:
		// Merge by Name: claim VCT replaces template VCT by name if they match, and new ones are appended.
		merged := make([]v1beta1.PersistentVolumeClaimTemplate, 0, len(templateVCTs)+len(claimVCTs))
		claimMap := make(map[string]v1beta1.PersistentVolumeClaimTemplate, len(claimVCTs))
		for _, vct := range claimVCTs {
			claimMap[vct.Name] = vct
		}

		// Keep template VCTs unless overridden by name
		for _, vct := range templateVCTs {
			if override, ok := claimMap[vct.Name]; ok {
				merged = append(merged, override)
				delete(claimMap, vct.Name)
			} else {
				merged = append(merged, vct)
			}
		}

		// Append any new volume templates introduced by the claim
		for _, vct := range claimVCTs {
			if _, exists := claimMap[vct.Name]; exists {
				merged = append(merged, vct)
			}
		}
		return merged, nil

	default:
		return nil, fmt.Errorf("unknown volume claim templates policy %q", policy)
	}
}

func validateVolumeClaimTemplates(vcts []v1beta1.PersistentVolumeClaimTemplate) error {
	names := make(map[string]struct{}, len(vcts))
	for i, vct := range vcts {
		if vct.Name == "" {
			return fmt.Errorf("%w: name at index %d is empty", ErrVolumeClaimTemplatesInvalid, i)
		}
		if _, exists := names[vct.Name]; exists {
			return fmt.Errorf("%w: duplicate name %q", ErrVolumeClaimTemplatesInvalid, vct.Name)
		}
		names[vct.Name] = struct{}{}
	}
	return nil
}

// migrateLegacyAssignedSandboxLabel migrates legacy assigned Sandbox name from label to annotation.
func (r *SandboxClaimReconciler) migrateLegacyAssignedSandboxLabel(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, sbName string) error {
	patch := client.MergeFrom(claim.DeepCopy())
	if claim.Annotations == nil {
		claim.Annotations = make(map[string]string)
	}
	claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] = sbName
	delete(claim.Labels, extensionsv1beta1.DeprecatedAssignedSandboxNameLabel)
	return r.Patch(ctx, claim, patch)
}

func (r *SandboxClaimReconciler) getOrCreateSandbox(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, _ *extensionsv1beta1.SandboxTemplate) (*v1beta1.Sandbox, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Executing getOrCreateSandbox", "claim", claim.Name)

	// Check if a previously adopted sandbox is recorded in claim status
	if statusName := claim.Status.SandboxStatus.Name; statusName != "" {
		logger.V(1).Info("Checking status for sandbox name", "claim.Status.SandboxStatus.Name", statusName, "claim", claim.Name)
		sandbox := &v1beta1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: statusName}, sandbox); err == nil {
			if metav1.IsControlledBy(sandbox, claim) {
				logger.V(4).Info("Found existing adopted sandbox from status", "claim.Status.SandboxStatus.Name", statusName, "claim", claim.Name)
				r.triggeredAdoptions.Delete(types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace})
				launchType := v1beta1.SandboxLaunchTypeCold
				if claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] == statusName ||
					claim.Labels[extensionsv1beta1.DeprecatedAssignedSandboxNameLabel] == statusName ||
					statusName != claim.Name {
					launchType = v1beta1.SandboxLaunchTypeWarm
				}
				if err := r.initializeSandboxLaunchTypeLabel(ctx, sandbox, launchType); err != nil {
					return nil, fmt.Errorf("failed to initialize launch type label on sandbox %q: %w", sandbox.Name, err)
				}
				return sandbox, nil
			}
		} else if !k8errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get sandbox %q from status: %w", statusName, err)
		}
	}

	// Check if a previously adopted sandbox is recorded in claim annotations or legacy labels
	var sbName string
	var fromLabel bool
	if claim.Annotations != nil {
		sbName = claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation]
	}
	if sbName == "" && claim.Labels != nil {
		sbName = claim.Labels[extensionsv1beta1.DeprecatedAssignedSandboxNameLabel]
		if sbName != "" {
			fromLabel = true
		}
	}

	if sbName != "" {
		logger.V(1).Info("Checking assigned sandbox name", "sandboxName", sbName, "fromLabel", fromLabel, "claim", claim.Name)
		sandbox := &v1beta1.Sandbox{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: sbName}, sandbox); err == nil {
			if metav1.IsControlledBy(sandbox, claim) {
				logger.V(4).Info("Found existing adopted sandbox", "sandbox", sbName, "claim", claim.Name)
				r.triggeredAdoptions.Delete(types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace})
				if fromLabel {
					if err := r.migrateLegacyAssignedSandboxLabel(ctx, claim, sbName); err != nil {
						logger.Error(err, "Failed to migrate legacy sandbox label to annotation (non-fatal)", "claim", claim.Name)
					} else {
						logger.Info("Successfully migrated legacy sandbox label to annotation", "claim", claim.Name)
					}
				}
				if err := r.initializeSandboxLaunchTypeLabel(ctx, sandbox, v1beta1.SandboxLaunchTypeWarm); err != nil {
					return nil, fmt.Errorf("failed to initialize launch type label on sandbox %q: %w", sandbox.Name, err)
				}
				return sandbox, nil
			}

			controllerRef := metav1.GetControllerOf(sandbox)
			if controllerRef != nil && controllerRef.Kind == "SandboxWarmPool" {
				// Still in warm pool. Try to complete adoption!
				logger.Info("Sandbox found in claim metadata still in warm pool, trying to complete adoption", "sandbox", sbName, "claim", claim.Name)
				if err := verifySandboxCandidate(sandbox, claim); err != nil {
					logger.Info("Sandbox recorded in claim metadata cannot be adopted, removing stale reference", "sandboxName", sbName, "fromLabel", fromLabel, "claim", claim.Name, "reason", err.Error())
					patch := client.MergeFrom(claim.DeepCopy())
					if fromLabel {
						delete(claim.Labels, extensionsv1beta1.DeprecatedAssignedSandboxNameLabel)
					} else {
						delete(claim.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation)
					}
					if err := r.Patch(ctx, claim, patch); err != nil {
						return nil, fmt.Errorf("failed to remove invalid sandbox reference: %w", err)
					}
				} else {
					// If we already sent the adoption patch for this exact claim+sandbox,
					// the cache just hasn't converged yet — keep waiting via the bounded
					// requeue without re-sending the (idempotent but redundant) patch.
					adoptionKey := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
					if prev, ok := r.triggeredAdoptions.Load(adoptionKey); ok && prev.uid == claim.UID && prev.sandbox == sbName {
						logger.V(4).Info("Adoption already triggered, waiting for cache to converge", "sandbox", sbName, "claim", claim.Name)
						return nil, fmt.Errorf("%w: sandbox %s", errAdoptionTriggeredRetry, sbName)
					}
					if err := r.completeAdoption(ctx, claim, sandbox); err != nil {
						if k8errors.IsNotFound(err) || k8errors.IsConflict(err) {
							logger.V(4).Info("Failed to complete adoption (conflict/notfound), falling through", "sandbox", sbName, "claim", claim.Name)
						} else {
							return nil, fmt.Errorf("failed to complete adoption of %q: %w", sbName, err)
						}
					} else {
						r.triggeredAdoptions.Store(adoptionKey, triggeredAdoptionEntry{uid: claim.UID, sandbox: sbName})
						if fromLabel {
							if err := r.migrateLegacyAssignedSandboxLabel(ctx, claim, sbName); err != nil {
								logger.Error(err, "Failed to migrate legacy sandbox label to annotation during adoption completion", "claim", claim.Name)
							} else {
								logger.Info("Successfully migrated legacy sandbox label to annotation during adoption completion", "claim", claim.Name)
							}
						}
						// Adoption was completed in-place (completeAdoption patched our controllerRef
						// and the Warm label). Signal a retry so a later pass observes the sandbox as
						// controlled by us once the cache converges. Returned as a sentinel so
						// Reconcile requeues immediately with a bounded delay rather than routing
						// through the exponential failure rate limiter (#1107).
						logger.Info("Triggered adoption completion for sandbox, requeueing", "sandbox", sbName, "claim", claim.Name)
						return nil, fmt.Errorf("%w: sandbox %s", errAdoptionTriggeredRetry, sbName)
					}
				}
			}
			logger.V(4).Info("Sandbox recorded in claim metadata belongs to another claim, falling through", "sandbox", sbName, "claim", claim.Name)
		} else if k8errors.IsNotFound(err) {
			logger.Info("Sandbox recorded in claim metadata not found, removing stale reference", "sandboxName", sbName, "claim", claim.Name)
			patch := client.MergeFrom(claim.DeepCopy())
			if fromLabel {
				delete(claim.Labels, extensionsv1beta1.DeprecatedAssignedSandboxNameLabel)
			} else {
				delete(claim.Annotations, extensionsv1beta1.AssignedSandboxNameAnnotation)
			}
			if err := r.Patch(ctx, claim, patch); err != nil {
				return nil, fmt.Errorf("failed to remove stale sandbox reference from claim metadata: %w", err)
			}
			logger.Info("Successfully removed stale sandbox reference from claim metadata", "sandbox", sbName, "claim", claim.Name)
		} else {
			return nil, fmt.Errorf("failed to get sandbox %q for sandbox name lookup: %w", sbName, err)
		}
	}

	// Try name-based lookup (sandbox created by createSandbox uses claim.Name)
	logger.V(1).Info("Trying name-based lookup for sandbox", "claim", claim.Name)
	sandbox := &v1beta1.Sandbox{
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
		logger.V(4).Info("sandbox already exists, skipping update", "name", sandbox.Name)
		if !metav1.IsControlledBy(sandbox, claim) {
			err := fmt.Errorf("sandbox %q is not controlled by claim %q. Please use a different claim name or delete the sandbox manually", sandbox.Name, claim.Name)
			logger.Error(err, "Sandbox controller mismatch")
			return nil, err
		}
		if err := r.initializeSandboxLaunchTypeLabel(ctx, sandbox, v1beta1.SandboxLaunchTypeCold); err != nil {
			return nil, fmt.Errorf("failed to initialize launch type label on sandbox %q: %w", sandbox.Name, err)
		}
		return sandbox, nil
	}

	// Implicit Cold Start Detection (Bypassing the Queue):
	// If len(claim.Spec.Env) > 0 or len(claim.Spec.VolumeClaimTemplates) > 0, the controller immediately bypasses the warm pool queue.
	if len(claim.Spec.Env) > 0 || len(claim.Spec.VolumeClaimTemplates) > 0 {
		logger.Info("Bypassing warm pool adoption because custom configuration is provided (env or volume claim templates)", "claim", claim.Name)
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

func (r *SandboxClaimReconciler) initializeSandboxLaunchTypeLabel(ctx context.Context, sandbox *v1beta1.Sandbox, launchType string) error {
	if sandbox.Labels != nil {
		if _, ok := sandbox.Labels[v1beta1.SandboxLaunchTypeLabel]; ok {
			return nil
		}
	}

	patch := client.MergeFrom(sandbox.DeepCopy())
	if sandbox.Labels == nil {
		sandbox.Labels = make(map[string]string)
	}
	sandbox.Labels[v1beta1.SandboxLaunchTypeLabel] = launchType
	return r.Patch(ctx, sandbox, patch)
}

func (r *SandboxClaimReconciler) getTemplate(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) (*extensionsv1beta1.SandboxTemplate, error) {
	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.WarmPoolRef.Name}, warmPool); err != nil {
		if k8errors.IsNotFound(err) {
			return nil, ErrWarmPoolNotFound
		}
		return nil, fmt.Errorf("failed to get sandbox warm pool %q: %w", claim.Spec.WarmPoolRef.Name, err)
	}

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      warmPool.Spec.TemplateRef.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
		if k8errors.IsNotFound(err) {
			return nil, fmt.Errorf(`SandboxTemplate %q not found: %w`, warmPool.Spec.TemplateRef.Name, ErrTemplateNotFound)
		}
		return nil, fmt.Errorf("failed to get sandbox template %q: %w", warmPool.Spec.TemplateRef.Name, err)
	}

	return template, nil
}

// resolveTemplateName safely extracts the SandboxTemplate name from the Sandbox annotations.
func (r *SandboxClaimReconciler) resolveTemplateName(sandbox *v1beta1.Sandbox) string {
	if sandbox != nil && sandbox.Annotations != nil && sandbox.Annotations[v1beta1.SandboxTemplateRefAnnotation] != "" {
		return sandbox.Annotations[v1beta1.SandboxTemplateRefAnnotation]
	}
	return "__unknown__"
}

// getOrRecordObservedTime stores the first time an object is seen by the controller in an in-memory
// map observedTimes for latency tracking. It returns the resolved timestamp for the object.
func (r *SandboxClaimReconciler) getOrRecordObservedTime(obj client.Object) time.Time {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}

	// Fast path: Entry already exists and UID matches
	if entry, ok := r.observedTimes.Load(key); ok {
		if entry.uid == obj.GetUID() {
			return entry.timestamp
		}
	}

	// Slow path: Entry missing or UID mismatched
	newEntry := observedTimeEntry{timestamp: time.Now(), uid: obj.GetUID()}
	actual, loaded := r.observedTimes.LoadOrStore(key, newEntry)
	if loaded {
		// Handle concurrent insertion: check if we need to overwrite due to UID mismatch
		if actual.uid != obj.GetUID() {
			r.observedTimes.Store(key, newEntry)
			return newEntry.timestamp
		}
		// UID matches, return the loaded timestamp
		return actual.timestamp
	}
	return newEntry.timestamp
}

// getTimingPredicate returns a predicate that stores the first time an object is seen by the
// controller, and cleans up the in-memory map entry when the object is deleted.
func (r *SandboxClaimReconciler) getTimingPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			r.getOrRecordObservedTime(e.Object)
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			r.getOrRecordObservedTime(e.ObjectNew)
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			key := types.NamespacedName{Name: e.Object.GetName(), Namespace: e.Object.GetNamespace()}
			entry, ok := r.observedTimes.Load(key)
			if ok && entry.uid == e.Object.GetUID() {
				r.observedTimes.Delete(key)
			}
			return true
		},
	}
}

// mapWarmPoolToClaims maps a SandboxWarmPool to a list of SandboxClaims that reference it.
func (r *SandboxClaimReconciler) mapWarmPoolToClaims(ctx context.Context, obj client.Object) []ctrl.Request {
	warmPool, ok := obj.(*extensionsv1beta1.SandboxWarmPool)
	if !ok {
		log.FromContext(ctx).Error(fmt.Errorf("unexpected object type %T", obj), "expected SandboxWarmPool in watch map function")
		return nil
	}
	var claims extensionsv1beta1.SandboxClaimList
	if err := r.List(ctx, &claims, client.InNamespace(warmPool.Namespace), client.MatchingFields{extensionsv1beta1.WarmPoolRefField: warmPool.Name}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list SandboxClaims for SandboxWarmPool", "namespace", warmPool.Namespace, "name", warmPool.Name)
		return nil
	}
	requests := make([]ctrl.Request, 0, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	r.MaxConcurrentReconciles = concurrentWorkers

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &extensionsv1beta1.SandboxClaim{}, extensionsv1beta1.WarmPoolRefField, func(rawObj client.Object) []string {
		claim, ok := rawObj.(*extensionsv1beta1.SandboxClaim)
		if !ok {
			return nil
		}
		if claim.Spec.WarmPoolRef.Name == "" {
			return nil
		}
		return []string{claim.Spec.WarmPoolRef.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1beta1.SandboxClaim{}, builder.WithPredicates(r.getTimingPredicate())).
		Owns(&v1beta1.Sandbox{}).
		Watches(&v1beta1.Sandbox{}, &sandboxEventHandler{sandboxQueue: r.WarmSandboxQueue}).
		Watches(&extensionsv1beta1.SandboxWarmPool{}, &warmPoolEventHandler{sandboxQueue: r.WarmSandboxQueue}).
		Watches(
			&extensionsv1beta1.SandboxWarmPool{},
			handler.EnqueueRequestsFromMapFunc(r.mapWarmPoolToClaims),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		// TODO: Keep a lightweight SandboxTemplate -> claims map watch to promptly reconcile
		// claims when a missing template is created, instead of relying on the 1-minute fallback.
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}

// cleanupLegacyNetworkPolicy cleans up any deprecated per-claim NetworkPolicies.
func (r *SandboxClaimReconciler) cleanupLegacyNetworkPolicy(ctx context.Context, claim *extensionsv1beta1.SandboxClaim) error {
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
func getLaunchType(sandbox *v1beta1.Sandbox) string {
	if sandbox == nil {
		return asmetrics.LaunchTypeUnknown
	}
	if sandbox.Labels[v1beta1.SandboxLaunchTypeLabel] == v1beta1.SandboxLaunchTypeWarm {
		return asmetrics.LaunchTypeWarm
	}
	return asmetrics.LaunchTypeCold
}

// recordClaimStartupLatency records the startup latency based on webhook annotation.
func (r *SandboxClaimReconciler) recordClaimStartupLatency(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, launchType string, templateName string, warmPoolName string) {
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
	asmetrics.RecordClaimStartupLatency(webhookSeenTime, launchType, templateName, warmPoolName)
}

// recordControllerStartupLatency records the controller startup latency based on observed time.
func (r *SandboxClaimReconciler) recordControllerStartupLatency(ctx context.Context, claim *extensionsv1beta1.SandboxClaim, launchType string, templateName string, warmPoolName string) {
	logger := log.FromContext(ctx)
	if observedTimeString := claim.Annotations[asmetrics.ObservabilityAnnotation]; observedTimeString != "" {
		key := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
		defer r.observedTimes.Delete(key)

		observedTime, err := time.Parse(time.RFC3339Nano, observedTimeString)
		if err != nil {
			logger.Error(err, "Failed to parse controller observation time", "value", observedTimeString)
			return
		}
		asmetrics.RecordClaimControllerStartupLatency(observedTime, launchType, templateName, warmPoolName)
	}
}

// recordSandboxCreationLatency records the sandbox creation latency.
func (r *SandboxClaimReconciler) recordSandboxCreationLatency(sandbox *v1beta1.Sandbox, launchType string, templateName string) {
	if sandbox == nil || sandbox.CreationTimestamp.IsZero() {
		return
	}
	sandboxReady := meta.FindStatusCondition(sandbox.Status.Conditions, string(v1beta1.SandboxConditionReady))
	if sandboxReady == nil || sandboxReady.Status != metav1.ConditionTrue || sandboxReady.LastTransitionTime.IsZero() {
		return
	}
	latency := sandboxReady.LastTransitionTime.Sub(sandbox.CreationTimestamp.Time)
	if latency >= 0 {
		asmetrics.RecordSandboxCreationLatency(latency, sandbox.Namespace, launchType, templateName)
	}
}

// recordCreationLatencyMetric detects and records transitions to Ready state.
func (r *SandboxClaimReconciler) recordCreationLatencyMetric(
	ctx context.Context,
	claim *extensionsv1beta1.SandboxClaim,
	oldStatus *extensionsv1beta1.SandboxClaimStatus,
	sandbox *v1beta1.Sandbox,
) {
	logger := log.FromContext(ctx)

	newStatus := &claim.Status
	newReady := meta.FindStatusCondition(newStatus.Conditions, string(v1beta1.SandboxConditionReady))
	if newReady == nil || newReady.Status != metav1.ConditionTrue {
		return
	}

	// Do not record creation metric if we have already seen the ready state.
	oldReady := meta.FindStatusCondition(oldStatus.Conditions, string(v1beta1.SandboxConditionReady))
	if oldReady != nil && oldReady.Status == metav1.ConditionTrue {
		// Already Ready before this reconcile; drain any entry re-added by a post-Ready UpdateFunc.
		key := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
		if entry, ok := r.observedTimes.Load(key); ok && entry.uid == claim.UID {
			r.observedTimes.Delete(key)
		}
		return
	}

	launchType := getLaunchType(sandbox)

	sandboxName := "none"
	if sandbox != nil {
		sandboxName = sandbox.Name
	}

	templateName := r.resolveTemplateName(sandbox)
	warmPoolName := claim.Spec.WarmPoolRef.Name

	logger.V(1).Info("SandboxClaim is marked as Ready", "claim", claim.Name, "sandbox", sandboxName, "duration", time.Since(claim.CreationTimestamp.Time))

	r.recordClaimStartupLatency(ctx, claim, launchType, templateName, warmPoolName)
	r.recordControllerStartupLatency(ctx, claim, launchType, templateName, warmPoolName)
	r.recordSandboxCreationLatency(sandbox, launchType, templateName)
}

func hasSandboxExpiredCondition(conditions []metav1.Condition) bool {
	readyCondition := meta.FindStatusCondition(conditions, string(v1beta1.SandboxConditionReady))
	return readyCondition != nil && readyCondition.Reason == v1beta1.SandboxReasonExpired
}

func hasClaimExpiredCondition(conditions []metav1.Condition) bool {
	readyCondition := meta.FindStatusCondition(conditions, string(v1beta1.SandboxConditionReady))
	return readyCondition != nil && readyCondition.Reason == extensionsv1beta1.ClaimExpiredReason
}

// sandboxEventHandler implements handler.EventHandler for the SandboxClaimReconciler.
type sandboxEventHandler struct {
	sandboxQueue queue.SandboxQueue
}

func (h *sandboxEventHandler) Create(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.Update(ctx, event.UpdateEvent{ObjectOld: &v1beta1.Sandbox{}, ObjectNew: e.Object}, q)
}

func (h *sandboxEventHandler) Update(ctx context.Context, e event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	newSandbox, ok := e.ObjectNew.(*v1beta1.Sandbox)
	if !ok {
		return
	}
	oldSandbox, ok := e.ObjectOld.(*v1beta1.Sandbox)
	if !ok {
		return
	}

	newAdoptable := isAdoptable(newSandbox) == nil
	oldAdoptable := isAdoptable(oldSandbox) == nil

	logger := log.FromContext(ctx)

	oldWarmPoolName := getWarmPoolName(oldSandbox)
	newWarmPoolName := getWarmPoolName(newSandbox)

	poolChanged := oldWarmPoolName != newWarmPoolName
	nodeScheduled := oldSandbox.Status.NodeName != newSandbox.Status.NodeName

	if (!oldAdoptable && newAdoptable) || (newAdoptable && poolChanged) || (newAdoptable && nodeScheduled) {
		// Add/update sandbox in the queue
		key := queue.SandboxKey{
			Namespace: newSandbox.Namespace,
			Name:      newSandbox.Name,
			NodeName:  newSandbox.Status.NodeName,
		}
		logger.V(1).Info("Adding/updating sandbox in warm pool queue", "warmPool", newWarmPoolName, "namespace", newSandbox.Namespace, "sandbox", key)
		if newWarmPoolName != "" {
			h.sandboxQueue.Add(queue.GetNamespacedWarmPoolName(newSandbox.Namespace, newWarmPoolName), key)
		}
	}
}

func (h *sandboxEventHandler) Generic(_ context.Context, _ event.GenericEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	// Generic events are not typically used for pod lifecycle changes we care about.
}

func verifySandboxCandidate(candidate *v1beta1.Sandbox, claim *extensionsv1beta1.SandboxClaim) error {
	if candidate.Namespace != claim.Namespace {
		return fmt.Errorf("%w: sandbox is in %q, claim is in %q", ErrCrossNamespaceAdoption, candidate.Namespace, claim.Namespace)
	}

	if err := isAdoptable(candidate); err != nil {
		return err
	}

	warmPoolName := getWarmPoolName(candidate)
	if warmPoolName == "" || warmPoolName != claim.Spec.WarmPoolRef.Name {
		return fmt.Errorf("incorrect warm pool, expected %v", claim.Spec.WarmPoolRef.Name)
	}
	return nil
}

func isAdoptable(candidate *v1beta1.Sandbox) error {
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
	if controllerRef == nil {
		return fmt.Errorf("sandbox %s/%s is unowned and cannot be safely adopted", candidate.Namespace, candidate.Name)
	}
	if controllerRef.APIVersion != extensionsv1beta1.GroupVersion.String() || controllerRef.Kind != "SandboxWarmPool" {
		return fmt.Errorf("sandbox %s/%s is not managed by warm pool. Controller: %v", candidate.Namespace, candidate.Name, controllerRef)
	}
	return nil
}

func (h *sandboxEventHandler) Delete(ctx context.Context, e event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	sandbox, ok := e.Object.(*v1beta1.Sandbox)
	if !ok {
		return
	}

	warmPoolName := getWarmPoolName(sandbox)

	if warmPoolName != "" {
		key := queue.SandboxKey{
			Namespace: sandbox.Namespace,
			Name:      sandbox.Name,
		}

		namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(sandbox.Namespace, warmPoolName)

		// Actively delete the Ghost Pod from the memory queue
		logger := log.FromContext(ctx)
		logger.V(1).Info("Removing deleted sandbox from warm pool queue", "namespace", sandbox.Namespace, "sandbox", key)
		h.sandboxQueue.RemoveItem(namespacedWarmPoolName, key)
	}
}

type warmPoolEventHandler struct {
	sandboxQueue queue.SandboxQueue
}

func (h *warmPoolEventHandler) Create(_ context.Context, _ event.CreateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}
func (h *warmPoolEventHandler) Update(_ context.Context, _ event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}
func (h *warmPoolEventHandler) Generic(_ context.Context, _ event.GenericEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *warmPoolEventHandler) Delete(ctx context.Context, e event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	warmPool, ok := e.Object.(*extensionsv1beta1.SandboxWarmPool)
	if !ok {
		return
	}

	namespacedWarmPoolName := queue.GetNamespacedWarmPoolName(warmPool.Namespace, warmPool.Name)
	logger := log.FromContext(ctx)
	logger.Info("SandboxWarmPool deleted, cleaning up memory queue", "namespace", warmPool.Namespace, "warmPool", warmPool.Name)

	// Actively drop the entire queue from memory
	h.sandboxQueue.RemoveQueue(namespacedWarmPoolName)
}

func getWarmPoolName(obj metav1.Object) string {
	if ctrl := metav1.GetControllerOf(obj); ctrl != nil && ctrl.Kind == "SandboxWarmPool" {
		return ctrl.Name
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "SandboxWarmPool" {
			return ref.Name
		}
	}
	return ""
}

func shouldSuppressError(err error) bool {
	for _, target := range suppressErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
