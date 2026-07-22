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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const (
	sandboxTemplateRefHash          = sandboxv1beta1.SandboxTemplateRefHashLabel
	warmPoolSandboxLabel            = sandboxv1beta1.SandboxWarmPoolLabel
	sandboxCreateDeleteMaxBatchSize = 300
	autoscalerSafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	// sandboxWarmPoolLabelIndex is the cache field index over the warmPoolSandboxLabel
	// value on warm sandboxes, so reconcilePool's member lookup is O(pool members) instead
	// of O(sandboxes-in-namespace).
	sandboxWarmPoolLabelIndex = ".metadata.labels[" + warmPoolSandboxLabel + "]"

	// warmPoolReadinessGracePeriod is how long a pool sandbox may stay
	// non-Ready before the reconciler considers it stuck and replaces it.
	warmPoolReadinessGracePeriod = 5 * time.Minute

	// expectationsPendingRequeueDelay is the fallback requeue used when create
	// or delete work is skipped because previously issued writes have not been
	// observed by the informer cache yet. Watch events normally retrigger the
	// pool much sooner; this only guards against lost events.
	expectationsPendingRequeueDelay = 30 * time.Second

	// unschedulableRequeueDelay is the rate-limited retry interval for a pool
	// holding unschedulable sandboxes instead of churning delete/create (#1215).
	unschedulableRequeueDelay = time.Minute

	// Event reasons surfaced on the SandboxWarmPool when the pool cannot make
	// progress toward spec.replicas (and when progress resumes).
	reasonWarmPoolNotProgressing = "WarmPoolNotProgressing"
	reasonWarmPoolProgressing    = "WarmPoolProgressing"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object.
type SandboxWarmPoolReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	MaxBatchSize           int
	EnableWarmPoolEviction bool
	// Recorder emits pool-level Events (e.g. WarmPoolNotProgressing). May be
	// nil (tests); all uses are nil-guarded.
	Recorder events.EventRecorder

	// expectations tracks in-flight sandbox creations/deletions per pool so a
	// reconcile never re-creates toward the target off a cache that has not
	// observed its own previous writes (#1215). Access via exp().
	expectations *warmPoolExpectations
	expOnce      sync.Once

	// notProgressingMu guards notProgressing, the set of pools currently held
	// in a not-progressing state (used to emit transition events exactly once).
	notProgressingMu sync.Mutex
	notProgressing   map[types.NamespacedName]struct{}
}

// exp returns the reconciler's expectations tracker, lazily initializing it so
// zero-value construction (tests) and SetupWithManager both work.
func (r *SandboxWarmPoolReconciler) exp() *warmPoolExpectations {
	r.expOnce.Do(func() {
		if r.expectations == nil {
			r.expectations = newWarmPoolExpectations()
		}
	})
	return r.expectations
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/finalizers,verbs=get;update;patch
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update

// Reconcile implements the reconciliation loop for SandboxWarmPool.
func (r *SandboxWarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the SandboxWarmPool instance
	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	if err := r.Get(ctx, req.NamespacedName, warmPool); err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Info("SandboxWarmPool resource not found. Ignoring since object must be deleted")
			r.forgetPool(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get SandboxWarmPool")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !warmPool.DeletionTimestamp.IsZero() {
		logger.Info("SandboxWarmPool is being deleted")
		r.forgetPool(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Save old status for comparison
	oldStatus := warmPool.Status.DeepCopy()

	// Reconcile the pool (create or delete Sandboxes as needed)
	requeueAfter, err := r.reconcilePool(ctx, warmPool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update status if it has changed
	if err := r.updateStatus(ctx, oldStatus, warmPool); err != nil {
		logger.Error(err, "Failed to update SandboxWarmPool status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// forgetPool drops per-pool bookkeeping (expectations, not-progressing state)
// once a pool is gone or terminating.
func (r *SandboxWarmPoolReconciler) forgetPool(key types.NamespacedName) {
	r.exp().Forget(key)
	r.notProgressingMu.Lock()
	delete(r.notProgressing, key)
	r.notProgressingMu.Unlock()
}

// reconcilePool ensures the correct number of pre-allocated sandboxes exist in the pool.
// It returns an optional requeue delay (used when work was deliberately held
// back, e.g. unsatisfied expectations or unschedulable sandboxes) and any
// errors encountered.
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) (time.Duration, error) {
	logger := log.FromContext(ctx)

	poolKey := types.NamespacedName{Namespace: warmPool.Namespace, Name: warmPool.Name}
	var requeueAfter time.Duration

	// Compute hash of the warm pool name for the pool label
	poolNameHash := sandboxcontrollers.NameHash(warmPool.Name)

	// List all Sandbox CRs with the warm pool label
	sandboxList := &sandboxv1beta1.SandboxList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		warmPoolSandboxLabel: poolNameHash,
	})

	if err := r.List(ctx, sandboxList,
		client.InNamespace(warmPool.Namespace),
		client.MatchingFields{sandboxWarmPoolLabelIndex: poolNameHash},
	); err != nil {
		logger.Error(err, "Failed to list sandboxes")
		return 0, err
	}

	// Fetch template and compute hash once to avoid repeated expensive operations,
	// only currentSandboxBlueprintHash is used for staleness checks,
	// currentPodTemplateHash is kept as a value for DeprecatedSandboxPodTemplateHashLabel
	// for external consumer compatibility
	template, currentPodTemplateHash, currentSandboxBlueprintHash, tmplErr := r.fetchTemplateAndHash(ctx, warmPool)

	// Delete stale pods, filter pods by ownership and adopt orphans.
	// terminatingReplicas counts pool-owned sandboxes that are deleting (or
	// were deleted by us but not yet observed by the cache): they are not
	// active, but they still occupy capacity until fully gone.
	activeSandboxes, terminatingReplicas, allErrors := r.filterActiveSandboxes(ctx, poolKey, warmPool, sandboxList.Items, template, currentSandboxBlueprintHash, tmplErr)

	now := time.Now()
	var healthySandboxes []sandboxv1beta1.Sandbox
	unschedulableReplicas := int32(0)
	for _, sb := range activeSandboxes {
		if !isSandboxReady(&sb) && !sb.CreationTimestamp.IsZero() && now.Sub(sb.CreationTimestamp.Time) > warmPoolReadinessGracePeriod {
			// Deleting an unschedulable sandbox only produces an equally
			// unschedulable replacement: under a capacity shortfall the old
			// delete-and-replace behavior becomes an unbounded delete->create
			// loop (#1215). Hold the sandbox and retry on a rate-limited
			// requeue instead; the scheduler will place it when capacity
			// frees up.
			if r.isSandboxPodUnschedulable(ctx, &sb) {
				unschedulableReplicas++
				healthySandboxes = append(healthySandboxes, sb)
				continue
			}
			logger.Info("Deleting stuck warm pool sandbox",
				"sandbox", sb.Name,
				"age", now.Sub(sb.CreationTimestamp.Time).Round(time.Second))
			r.exp().ExpectDeletion(poolKey, sb.UID)
			if err := r.Delete(ctx, &sb); err != nil {
				r.exp().DeletionObserved(poolKey, sb.UID)
				logger.Error(err, "Failed to delete stuck sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				// The sandbox still exists; keep counting it as active so the
				// create path cannot overshoot spec.replicas.
				healthySandboxes = append(healthySandboxes, sb)
				continue
			}
			// Successfully deleted: it now occupies capacity as terminating
			// until the deletion is observed; the replacement is created on a
			// later reconcile once it no longer counts against the target.
			terminatingReplicas++
			continue
		}
		healthySandboxes = append(healthySandboxes, sb)
	}
	activeSandboxes = healthySandboxes

	desiredReplicas := int32(1)
	if warmPool.Spec.Replicas != nil {
		desiredReplicas = *warmPool.Spec.Replicas
	}
	currentReplicas := int32(len(activeSandboxes))
	// totalReplicas is the pool's whole live population: active plus
	// terminating-but-still-present. Creates are gated on this so the
	// population can never balloon past spec.replicas while deletes lag (#1215).
	totalReplicas := currentReplicas + terminatingReplicas

	logger.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
		"terminating", terminatingReplicas,
		"unschedulable", unschedulableReplicas,
		"poolName", warmPool.Name,
		"poolNameHash", poolNameHash)

	warmPool.Status.Replicas = currentReplicas
	warmPool.Status.Selector = labelSelector.String()

	// Calculate ready replicas by checking Sandbox Ready condition
	readyReplicas := int32(0)
	for i := range activeSandboxes {
		if isSandboxReady(&activeSandboxes[i]) {
			readyReplicas++
		}
	}
	warmPool.Status.ReadyReplicas = readyReplicas

	maxBatchSize := int32(r.MaxBatchSize)

	// Create new sandboxes if we need more.
	// Hard invariant: never create while the existing population (active,
	// including non-Ready, plus terminating-still-present) already covers
	// spec.replicas.
	if totalReplicas < desiredReplicas && tmplErr == nil {
		sandboxesToCreate := min(desiredReplicas-totalReplicas, maxBatchSize)

		sandboxCR, err := r.buildSandboxCR(warmPool, poolNameHash, template, currentPodTemplateHash, currentSandboxBlueprintHash)
		switch {
		case err != nil:
			logger.Error(err, "Failed to build sandbox CR blueprint")
			allErrors = errors.Join(allErrors, err)
		// TryExpectCreations atomically checks that every create and delete
		// this controller previously issued for the pool has been observed by
		// the informer cache, and records the new in-flight creates. If prior
		// writes are still unobserved the cached list above is stale and
		// creating against it would overshoot the target (the #1215 runaway),
		// so we skip and let the watch (or a fallback requeue) retrigger us.
		case !r.exp().TryExpectCreations(poolKey, int(sandboxesToCreate)):
			logger.Info("Skipping sandbox creation: waiting for in-flight creates/deletes to be observed",
				"poolName", warmPool.Name)
			requeueAfter = expectationsPendingRequeueDelay
		default:
			logger.Info("Creating new pool sandboxes", "count", sandboxesToCreate)
			// Parallel sandbox creation with adaptive slow-start batching (starts with 1 and doubles on success)
			successes, createErr := slowStartBatch(ctx, int(sandboxesToCreate), 1, func(_ int) error {
				return r.createPoolSandbox(ctx, warmPool, sandboxCR)
			})
			// Creates that never happened will never produce a watch event;
			// lower their expectations immediately so the pool is not blocked
			// until the expectations timeout.
			if lower := int(sandboxesToCreate) - successes; lower > 0 {
				r.exp().LowerCreations(poolKey, lower)
			}
			if createErr != nil {
				logger.Error(createErr, "Failed to create pool sandboxes")
				allErrors = errors.Join(allErrors, createErr)
			}
		}
	}

	// Delete excess sandboxes if we have too many. Like creates, excess
	// deletes are computed from the cached list, so they are skipped while
	// expectations are unsatisfied: a stale list could otherwise show a
	// phantom surplus and delete healthy sandboxes.
	if currentReplicas > desiredReplicas {
		if !r.exp().SatisfiedExpectations(poolKey) {
			logger.Info("Skipping excess sandbox deletion: waiting for in-flight creates/deletes to be observed",
				"poolName", warmPool.Name)
			requeueAfter = expectationsPendingRequeueDelay
		} else {
			sandboxesToDelete := min(currentReplicas-desiredReplicas, maxBatchSize)
			logger.Info("Deleting excess sandboxes", "count", sandboxesToDelete)

			// Prioritize deleting unready sandboxes before ready ones,
			// then newest first within each group.
			slices.SortFunc(activeSandboxes, func(a, b sandboxv1beta1.Sandbox) int {
				aReady := isSandboxReady(&a)
				bReady := isSandboxReady(&b)
				if aReady != bReady {
					if aReady {
						return 1 // a ready, b not ready -> b first (delete unready first)
					}
					return -1 // b ready, a not ready -> a first
				}
				return b.CreationTimestamp.Compare(a.CreationTimestamp.Time) // newest first
			})

			toDeleteCount := min(sandboxesToDelete, int32(len(activeSandboxes)))
			// Parallel sandbox deletion with adaptive slow-start batching (starts with 1 and doubles on success)
			_, deleteErr := slowStartBatch(ctx, int(toDeleteCount), 1, func(idx int) error {
				sb := &activeSandboxes[idx]
				r.exp().ExpectDeletion(poolKey, sb.UID)
				if err := r.deletePoolSandbox(ctx, sb); err != nil {
					r.exp().DeletionObserved(poolKey, sb.UID)
					return err
				}
				return nil
			})
			if deleteErr != nil {
				logger.Error(deleteErr, "Failed to delete pool sandboxes")
				allErrors = errors.Join(allErrors, deleteErr)
			}
		}
	}

	// Surface (and clear) the not-progressing signal. A pool with
	// unschedulable sandboxes past the readiness grace period cannot make
	// progress toward spec.replicas until cluster capacity frees up; degrade
	// visibly instead of churning.
	if unschedulableReplicas > 0 {
		r.setNotProgressing(warmPool, poolKey, true, fmt.Sprintf(
			"%d/%d sandboxes are unschedulable past the %s readiness grace period; holding them instead of replacing (replacements would be equally unschedulable)",
			unschedulableReplicas, desiredReplicas, warmPoolReadinessGracePeriod))
		if requeueAfter == 0 || unschedulableRequeueDelay < requeueAfter {
			requeueAfter = unschedulableRequeueDelay
		}
	} else {
		r.setNotProgressing(warmPool, poolKey, false, "")
	}

	if tmplErr != nil && !k8serrors.IsNotFound(tmplErr) {
		allErrors = errors.Join(allErrors, tmplErr)
	}

	return requeueAfter, allErrors
}

// setNotProgressing tracks the pool's not-progressing state and emits a
// transition Event: a Warning when the pool stops progressing and a Normal
// event once progress resumes. Repeated reconciles in the same state do not
// re-emit.
func (r *SandboxWarmPoolReconciler) setNotProgressing(warmPool *extensionsv1beta1.SandboxWarmPool, poolKey types.NamespacedName, notProgressing bool, message string) {
	r.notProgressingMu.Lock()
	_, was := r.notProgressing[poolKey]
	if notProgressing == was {
		r.notProgressingMu.Unlock()
		return
	}
	if notProgressing {
		if r.notProgressing == nil {
			r.notProgressing = make(map[types.NamespacedName]struct{})
		}
		r.notProgressing[poolKey] = struct{}{}
	} else {
		delete(r.notProgressing, poolKey)
	}
	r.notProgressingMu.Unlock()

	if r.Recorder == nil {
		return
	}
	if notProgressing {
		r.Recorder.Eventf(warmPool, nil, corev1.EventTypeWarning, reasonWarmPoolNotProgressing, "Reconciling", "%s", message)
	} else {
		r.Recorder.Eventf(warmPool, nil, corev1.EventTypeNormal, reasonWarmPoolProgressing, "Reconciling", "Warm pool is progressing again")
	}
}

// isSandboxPodUnschedulable reports whether the sandbox's backing pod is
// currently unschedulable (PodScheduled=False with reason Unschedulable).
// Missing pods or pods without a definitive PodScheduled=False/Unschedulable
// condition report false, preserving the delete-and-replace behavior for
// genuinely stuck sandboxes.
func (r *SandboxWarmPoolReconciler) isSandboxPodUnschedulable(ctx context.Context, sb *sandboxv1beta1.Sandbox) bool {
	// The backing pod normally shares the sandbox's name; a sandbox that
	// adopted a warm pod tracks the pod name in an annotation (same
	// resolution the sandbox controller uses).
	podName := sb.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]
	if podName == "" {
		podName = sb.Name
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: podName}, pod); err != nil {
		return false
	}
	if !pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled {
			return cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable
		}
	}
	return false
}

// adoptSandbox sets this warmpool as the owner of an orphaned sandbox.
func (r *SandboxWarmPoolReconciler) adoptSandbox(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool, sb *sandboxv1beta1.Sandbox) error {
	if err := controllerutil.SetControllerReference(warmPool, sb, r.Scheme); err != nil {
		return err
	}
	setWarmLaunchTypeLabelIfNeeded(sb)
	return r.Update(ctx, sb)
}

func setWarmLaunchTypeLabelIfNeeded(sb *sandboxv1beta1.Sandbox) bool {
	if sb.Labels == nil {
		sb.Labels = make(map[string]string)
	}
	if sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] == sandboxv1beta1.SandboxLaunchTypeWarm {
		return false
	}
	sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	return true
}

// filterActiveSandboxes filters the list of sandboxes, deleting stale ones and adopting orphans.
// It returns the pool's active sandboxes plus the number of pool-owned
// terminating sandboxes: ones with a deletion timestamp, ones this controller
// deleted but whose deletion the cache has not observed yet, and ones deleted
// as stale in this pass. Terminating sandboxes are excluded from active (and
// so from Ready accounting), but still occupy capacity, so the create path
// must count them against spec.replicas (#1215).
func (r *SandboxWarmPoolReconciler) filterActiveSandboxes(ctx context.Context, poolKey types.NamespacedName, warmPool *extensionsv1beta1.SandboxWarmPool, sandboxes []sandboxv1beta1.Sandbox, template *extensionsv1beta1.SandboxTemplate, currentSandboxBlueprintHash string, tmplErr error) ([]sandboxv1beta1.Sandbox, int32, error) {
	logger := log.FromContext(ctx)
	var activeSandboxes []sandboxv1beta1.Sandbox
	terminatingReplicas := int32(0)
	var allErrors error

	vettedHashes := make(map[string]bool)

	// Determine the update strategy, defaulting to OnReplenish if not specified or unknown.
	var updateStrategyType extensionsv1beta1.SandboxWarmPoolUpdateStrategyType
	if warmPool.Spec.UpdateStrategy != nil {
		updateStrategyType = warmPool.Spec.UpdateStrategy.Type
	}

	var updateStrategy extensionsv1beta1.SandboxWarmPoolUpdateStrategyType
	switch updateStrategyType {
	case extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType:
		updateStrategy = extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType
	case extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType, "":
		updateStrategy = extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType
	default:
		logger.Info("Unknown update strategy, defaulting to OnReplenish", "strategy", updateStrategyType)
		updateStrategy = extensionsv1beta1.OnReplenishSandboxWarmPoolUpdateStrategyType
	}

	for _, sb := range sandboxes {
		controllerRef := metav1.GetControllerOf(&sb)
		isOrphan := controllerRef == nil
		isControlledByPool := controllerRef != nil && controllerRef.UID == warmPool.UID

		if !sb.DeletionTimestamp.IsZero() {
			// Terminating pool members are no longer active, but they still
			// occupy capacity until fully gone: count them so create gating
			// cannot balloon the population while deletes lag (#1215).
			if isControlledByPool {
				terminatingReplicas++
			}
			continue
		}

		// A sandbox this controller already deleted may still show up in the
		// (lagging) cache without a deletion timestamp; treat it as
		// terminating, not active.
		if isControlledByPool && r.exp().IsPendingDeletion(poolKey, sb.UID) {
			terminatingReplicas++
			continue
		}

		if !isOrphan && !isControlledByPool {
			logger.Info("Ignoring sandbox with different controller", "sandbox", sb.Name, "controller", controllerRef.Name)
			continue
		}

		if tmplErr == nil && (updateStrategy == extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType || isOrphan) {
			if r.isSandboxStale(ctx, &sb, template, currentSandboxBlueprintHash, vettedHashes) {
				logger.Info("Deleting stale sandbox", "sandbox", sb.Name, "isOrphan", isOrphan)
				// Only pool-owned sandboxes get deletion expectations: the
				// watch handler can only map owned delete events back to the
				// pool, and only owned sandboxes count against the target.
				if isControlledByPool {
					r.exp().ExpectDeletion(poolKey, sb.UID)
				}
				if err := r.Delete(ctx, &sb); err != nil {
					if isControlledByPool {
						r.exp().DeletionObserved(poolKey, sb.UID)
					}
					logger.Error(err, "Failed to delete stale sandbox", "sandbox", sb.Name)
					allErrors = errors.Join(allErrors, err)
				} else if isControlledByPool {
					terminatingReplicas++
				}
				continue
			}
		}

		if isControlledByPool && setWarmLaunchTypeLabelIfNeeded(&sb) {
			if err := r.Update(ctx, &sb); err != nil {
				logger.Error(err, "Failed to update sandbox launch type label", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				continue
			}
		}

		if isOrphan {
			logger.Info("Adopting orphaned sandbox", "sandbox", sb.Name)
			if err := r.adoptSandbox(ctx, warmPool, &sb); err != nil {
				logger.Error(err, "Failed to adopt sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				continue
			}
		}

		activeSandboxes = append(activeSandboxes, sb)
	}
	return activeSandboxes, terminatingReplicas, allErrors
}

// computePodTemplateHash computes a hash of the sandbox template's Spec.PodTemplate.
func computePodTemplateHash(template *extensionsv1beta1.SandboxTemplate) (string, error) {
	specJSON, err := json.Marshal(template.Spec.PodTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to marshal pod template for hashing: %w", err)
	}
	return sandboxcontrollers.NameHash(string(specJSON)), nil
}

// computeSandboxBlueprintHash computes a hash of the sandbox template's Spec.SandboxBlueprint.
func computeSandboxBlueprintHash(template *extensionsv1beta1.SandboxTemplate) (string, error) {
	specJSON, err := json.Marshal(template.Spec.SandboxBlueprint)
	if err != nil {
		return "", fmt.Errorf("failed to marshal sandbox blueprint for hashing: %w", err)
	}
	return sandboxcontrollers.NameHash(string(specJSON)), nil
}

// fetchTemplateAndHash fetches the sandbox template and computes its hash.
func (r *SandboxWarmPoolReconciler) fetchTemplateAndHash(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) (*extensionsv1beta1.SandboxTemplate, string, string, error) {
	logger := log.FromContext(ctx)
	template, tmplErr := r.getTemplate(ctx, warmPool)
	var currentPodTemplateHash, currentSandboxBlueprintHash string
	if tmplErr == nil {
		currentPodTemplateHash, tmplErr = computePodTemplateHash(template)
	}
	if tmplErr == nil {
		currentSandboxBlueprintHash, tmplErr = computeSandboxBlueprintHash(template)
	}

	if tmplErr != nil {
		logger.Error(tmplErr, "Failed to get sandbox template and hash", "templateRef", warmPool.Spec.TemplateRef.Name)
	}
	return template, currentPodTemplateHash, currentSandboxBlueprintHash, tmplErr
}

// buildSandboxCR constructs the base Sandbox CR (with pod template and volume claim templates) for the warm pool.
func (r *SandboxWarmPoolReconciler) buildSandboxCR(
	warmPool *extensionsv1beta1.SandboxWarmPool,
	poolNameHash string,
	template *extensionsv1beta1.SandboxTemplate,
	currentPodTemplateHash string,
	currentSandboxBlueprintHash string,
) (*sandboxv1beta1.Sandbox, error) {
	sandboxLabels := map[string]string{
		warmPoolSandboxLabel:                                 poolNameHash,
		sandboxTemplateRefHash:                               SandboxTemplateRefHash(warmPool.Spec.TemplateRef.Name),
		sandboxv1beta1.SandboxLaunchTypeLabel:                sandboxv1beta1.SandboxLaunchTypeWarm,
		sandboxv1beta1.DeprecatedSandboxPodTemplateHashLabel: currentPodTemplateHash,
		sandboxv1beta1.SandboxTemplateHashLabel:              currentSandboxBlueprintHash,
		sandboxv1beta1.CreatedByLabel:                        "controller",
	}

	// Build annotations for the Sandbox CR
	sandboxAnnotations := map[string]string{
		sandboxv1beta1.SandboxTemplateRefAnnotation: warmPool.Spec.TemplateRef.Name,
	}

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", warmPool.Name),
			Namespace:    warmPool.Namespace,
			Labels:       sandboxLabels,
			Annotations:  sandboxAnnotations,
		},
		// Deep-copy the entire shared blueprint
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy(),
		},
	}

	// Propagate pool and template labels to pod template for consistency and targeting
	if sandbox.Spec.PodTemplate.ObjectMeta.Labels == nil {
		sandbox.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
	}
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[warmPoolSandboxLabel] = poolNameHash
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxTemplateRefHash] = SandboxTemplateRefHash(warmPool.Spec.TemplateRef.Name)
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.DeprecatedSandboxPodTemplateHashLabel] = currentPodTemplateHash
	sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.SandboxTemplateHashLabel] = currentSandboxBlueprintHash

	// Respect the template's custom eviction annotation if explicitly specified.
	// Only apply the default eviction behavior if the annotation is not defined.
	if _, exists := sandbox.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation]; !exists {
		if r.EnableWarmPoolEviction {
			if sandbox.Spec.PodTemplate.ObjectMeta.Annotations == nil {
				sandbox.Spec.PodTemplate.ObjectMeta.Annotations = make(map[string]string)
			}
			sandbox.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation] = "true"
		}
	}

	// Apply secure defaults to the sandbox pod spec
	ApplySandboxSecureDefaults(template, &sandbox.Spec.PodTemplate.Spec)

	// Set controller reference so the Sandbox is owned by the SandboxWarmPool
	if err := ctrl.SetControllerReference(warmPool, sandbox, r.Scheme); err != nil {
		return nil, fmt.Errorf("SetControllerReference for Sandbox failed: %w", err)
	}

	return sandbox, nil
}

// createPoolSandbox creates a full Sandbox CR for the warm pool using a pre-built sandboxCR.
func (r *SandboxWarmPoolReconciler) createPoolSandbox(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool, sandboxCR *sandboxv1beta1.Sandbox) error {
	logger := log.FromContext(ctx)
	sandbox := sandboxCR.DeepCopy()
	if err := r.Create(ctx, sandbox); err != nil {
		logger.Error(err, "Failed to create pool sandbox")
		return err
	}

	logger.Info("Created new pool sandbox", "sandbox", sandbox.Name, "poolName", warmPool.Name)
	return nil
}

// deletePoolSandbox deletes a Sandbox CR from the warm pool. Ignores not found errors to not abort the batch deletion if some sandboxes are already deleted.
func (r *SandboxWarmPoolReconciler) deletePoolSandbox(ctx context.Context, sb *sandboxv1beta1.Sandbox) error {
	logger := log.FromContext(ctx)
	if err := r.Delete(ctx, sb); err != nil && client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to delete sandbox", "sandbox", sb.Name, "namespace", sb.Namespace)
		return err
	}
	return nil
}

// updateStatus updates the status of the SandboxWarmPool if it has changed.
func (r *SandboxWarmPoolReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1beta1.SandboxWarmPoolStatus, warmPool *extensionsv1beta1.SandboxWarmPool) error {
	logger := log.FromContext(ctx)

	// Check if status has changed
	if equality.Semantic.DeepEqual(oldStatus, &warmPool.Status) {
		return nil
	}

	oldWarmPool := warmPool.DeepCopy()
	oldWarmPool.Status = *oldStatus
	patch := client.MergeFrom(oldWarmPool)

	if err := r.Status().Patch(ctx, warmPool, patch); err != nil {
		return fmt.Errorf("failed to update SandboxWarmPool status: %w", err)
	}

	logger.Info("Updated SandboxWarmPool status", "replicas", warmPool.Status.Replicas, "readyReplicas", warmPool.Status.ReadyReplicas)
	return nil
}

func (r *SandboxWarmPoolReconciler) getTemplate(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) (*extensionsv1beta1.SandboxTemplate, error) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: warmPool.Namespace,
			Name:      warmPool.Spec.TemplateRef.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
		if !k8serrors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox template %q: %w", warmPool.Spec.TemplateRef.Name, err)
		}
		return nil, err
	}

	return template, nil
}

// isSandboxStale checks if the sandbox version matches the current template.
// It uses a cache (vettedHashes) to avoid repeated expensive DeepEqual calls
// for sandboxes with the same hash.
func (r *SandboxWarmPoolReconciler) isSandboxStale(
	ctx context.Context,
	sandbox *sandboxv1beta1.Sandbox,
	template *extensionsv1beta1.SandboxTemplate,
	currentSandboxBlueprintHash string,
	vettedHashes map[string]bool,
) bool {
	sandboxHash := sandbox.Labels[sandboxv1beta1.SandboxTemplateHashLabel]

	// If the templateRefHash doesn't match, it's stale.
	if sandbox.Labels[sandboxTemplateRefHash] != SandboxTemplateRefHash(template.Name) {
		return true
	}

	// Check if the sandbox is unowned (orphaned).
	controllerRef := metav1.GetControllerOf(sandbox)
	isOrphan := controllerRef == nil
	if isOrphan {
		// Always perform full semantic comparison for orphans.
		return !r.compareSandboxBlueprint(template, &sandbox.Spec.SandboxBlueprint)
	}

	// If hashes match, it's fresh.
	if sandboxHash != "" && sandboxHash == currentSandboxBlueprintHash {
		return false
	}

	// If currentSandboxBlueprintHash is empty, it means we failed to compute it.
	// In this case, we should log an error and treat it as NOT stale to avoid
	// mass-deleting existing sandboxes due to a marshal failure.
	if currentSandboxBlueprintHash == "" {
		log.FromContext(ctx).Error(nil, "currentSandboxBlueprintHash is empty, skipping staleness check", "sandbox", sandbox.Name)
		return false
	}

	// Check if we've already evaluated this specific old version.
	if sandboxHash != "" {
		if isStale, found := vettedHashes[sandboxHash]; found {
			return isStale
		}
	}

	// Perform a semantic comparison of the sandbox blueprint.
	// We normalize the pod spec by applying the same secure defaults
	// used during creation to avoid false positives from controller-injected fields.
	isStale := !r.compareSandboxBlueprint(template, &sandbox.Spec.SandboxBlueprint)

	// Save the result for the next sandbox with this same hash.
	if sandboxHash != "" {
		vettedHashes[sandboxHash] = isStale
	}

	return isStale
}

// comparePodSpecs checks if the pod spec in the sandbox is semantically equal to the template,
// normalizing for fields that the controller populates by default.
func (r *SandboxWarmPoolReconciler) comparePodSpecs(template *extensionsv1beta1.SandboxTemplate, actualSandboxSpec *corev1.PodSpec) bool {
	// Create what the sandbox SHOULD look like if it were created from the current template.
	expectedSpec := template.Spec.PodTemplate.Spec.DeepCopy()
	ApplySandboxSecureDefaults(template, expectedSpec)

	// Compare the actual sandbox spec to the expected "perfect" spec.
	// Since both have now undergone the exact same defaulting logic,
	// any remaining difference is a TRUE template drift.
	return equality.Semantic.DeepEqual(expectedSpec, actualSandboxSpec)
}

// compareVolumeClaimTemplates checks if the volume claim templates in the sandbox are equal to the template.
// Only each entry's name and spec are compared, as changes in metadata (like labels, annotations) are not tracked for staleness.
// Note: Comparison is index-based (order-sensitive) to stay consistent with computeSandboxBlueprintHash (+listType=atomic).
// Making this comparison order-independent without also sorting the templates in computeSandboxBlueprintHash
// would cause reordered warm sandboxes to fail the hash label check on every reconcile.
func (r *SandboxWarmPoolReconciler) compareVolumeClaimTemplates(template *extensionsv1beta1.SandboxTemplate, actualVCTs []sandboxv1beta1.PersistentVolumeClaimTemplate) bool {
	if len(template.Spec.SandboxBlueprint.VolumeClaimTemplates) != len(actualVCTs) {
		return false
	}

	for i, tmplVCT := range template.Spec.SandboxBlueprint.VolumeClaimTemplates {
		actualVCT := actualVCTs[i]
		if tmplVCT.Name != actualVCT.Name || !equality.Semantic.DeepEqual(tmplVCT.Spec, actualVCT.Spec) {
			return false
		}
	}

	return true
}

// compareSandboxBlueprint checks if the sandbox blueprint in the sandbox is semantically equal to the template,
// ignoring metadata differences and only comparing the fields that are relevant for staleness detection.
func (r *SandboxWarmPoolReconciler) compareSandboxBlueprint(template *extensionsv1beta1.SandboxTemplate, actualSandboxSpec *sandboxv1beta1.SandboxBlueprint) bool {
	return r.comparePodSpecs(template, &actualSandboxSpec.PodTemplate.Spec) &&
		r.compareVolumeClaimTemplates(template, actualSandboxSpec.VolumeClaimTemplates) &&
		equality.Semantic.DeepEqual(template.Spec.Service, actualSandboxSpec.Service)
}

// sandboxWarmPoolLabelIndexer extracts the warmPoolSandboxLabel value for the
// sandboxWarmPoolLabelIndex cache field index. Shared with tests so fake clients
// register the same index the manager does.
func sandboxWarmPoolLabelIndexer(obj client.Object) []string {
	if v, ok := obj.GetLabels()[warmPoolSandboxLabel]; ok {
		return []string{v}
	}
	return nil
}

// sandboxTemplateRefNameIndexer extracts the template reference name for the
// TemplateRefField cache field index. Shared with tests so fake clients
// register the same index the manager does.
func sandboxTemplateRefNameIndexer(obj client.Object) []string {
	wp := obj.(*extensionsv1beta1.SandboxWarmPool)
	if wp.Spec.TemplateRef.Name == "" {
		return nil
	}
	return []string{wp.Spec.TemplateRef.Name}
}

// warmPoolControllerKey resolves the SandboxWarmPool that controls obj, if any.
func warmPoolControllerKey(obj client.Object) (types.NamespacedName, bool) {
	controllerRef := metav1.GetControllerOf(obj)
	if controllerRef == nil {
		return types.NamespacedName{}, false
	}
	gv, err := schema.ParseGroupVersion(controllerRef.APIVersion)
	if err != nil || gv.Group != extensionsv1beta1.GroupVersion.Group || controllerRef.Kind != "SandboxWarmPool" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: controllerRef.Name}, true
}

// warmPoolSandboxEventHandler wraps the standard enqueue-for-owner handler so
// the expectations tracker observes owned sandbox add/delete events before the
// owning pool is enqueued. This ordering guarantees that by the time a
// reconcile triggered by one of our own writes runs, the corresponding
// expectation has already been lowered.
type warmPoolSandboxEventHandler struct {
	handler.EventHandler
	expectations *warmPoolExpectations
}

func (h *warmPoolSandboxEventHandler) Create(ctx context.Context, evt event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if key, ok := warmPoolControllerKey(evt.Object); ok {
		h.expectations.CreationObserved(key)
	}
	h.EventHandler.Create(ctx, evt, q)
}

func (h *warmPoolSandboxEventHandler) Delete(ctx context.Context, evt event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if key, ok := warmPoolControllerKey(evt.Object); ok {
		h.expectations.DeletionObserved(key, evt.Object.GetUID())
	}
	h.EventHandler.Delete(ctx, evt, q)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	if r.MaxBatchSize <= 0 {
		r.MaxBatchSize = sandboxCreateDeleteMaxBatchSize
	}

	// Index sandboxes by the warm pool label value
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &sandboxv1beta1.Sandbox{},
		sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer); err != nil {
		return fmt.Errorf("failed to index sandboxes by warm pool label: %w", err)
	}

	// Index warm pools by the template reference name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &extensionsv1beta1.SandboxWarmPool{},
		extensionsv1beta1.TemplateRefField, sandboxTemplateRefNameIndexer); err != nil {
		return fmt.Errorf("failed to index warm pools by template reference name: %w", err)
	}

	// Equivalent to Owns(&Sandbox{}), plus expectation observation on
	// add/delete events (see warmPoolSandboxEventHandler).
	sandboxHandler := &warmPoolSandboxEventHandler{
		EventHandler: handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(),
			&extensionsv1beta1.SandboxWarmPool{}, handler.OnlyControllerOwner()),
		expectations: r.exp(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1beta1.SandboxWarmPool{}).
		Watches(&sandboxv1beta1.Sandbox{}, sandboxHandler).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Watches(
			&extensionsv1beta1.SandboxTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.findWarmPoolsForTemplate),
		).
		Complete(r)
}

// findWarmPoolsForTemplate returns a list of reconcile.Requests for all SandboxWarmPools that reference the template.
func (r *SandboxWarmPoolReconciler) findWarmPoolsForTemplate(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	template, ok := obj.(*extensionsv1beta1.SandboxTemplate)
	if !ok {
		return nil
	}

	warmPools := &extensionsv1beta1.SandboxWarmPoolList{}
	if err := r.List(ctx, warmPools, client.InNamespace(template.Namespace), client.MatchingFields{extensionsv1beta1.TemplateRefField: template.Name}); err != nil {
		logger.Error(err, "Failed to list warm pools for template", "template", template.Name)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(warmPools.Items))
	for _, wp := range warmPools.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      wp.Name,
				Namespace: wp.Namespace,
			},
		})
	}
	return requests
}

// slowStartBatch is a helper that runs a given function fn multiple times in parallel batches.
// It starts with initialBatchSize, and doubles the batch size for each successful batch.
// If any execution of fn returns an error, it stops and returns the first encountered error.
func slowStartBatch(ctx context.Context, count int, initialBatchSize int, fn func(int) error) (int, error) {
	remaining := count
	successes := 0

	for batchSize := min(remaining, initialBatchSize); batchSize > 0; batchSize = min(2*batchSize, remaining) {
		if ctx.Err() != nil {
			return successes, ctx.Err()
		}

		eg, _ := errgroup.WithContext(ctx)
		var batchSuccesses atomic.Int64

		for i := 0; i < batchSize; i++ {
			index := successes + i
			eg.Go(func() error {
				if err := fn(index); err != nil {
					return err
				}
				batchSuccesses.Add(1)
				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			successes += int(batchSuccesses.Load())
			return successes, err
		}

		successes += int(batchSuccesses.Load())
		remaining -= batchSize
	}

	return successes, nil
}
