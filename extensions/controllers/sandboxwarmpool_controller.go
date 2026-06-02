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
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const (
	sandboxTemplateRefHash          = "agents.x-k8s.io/sandbox-template-ref-hash"
	warmPoolSandboxLabel            = "agents.x-k8s.io/warm-pool-sandbox"
	sandboxCreateDeleteMaxBatchSize = 300
	warmPoolEvictionAnnotation      = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object.
type SandboxWarmPoolReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	MaxBatchSize           int
	EnableWarmPoolEviction bool
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/finalizers,verbs=get;update;patch
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the reconciliation loop for SandboxWarmPool.
func (r *SandboxWarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the SandboxWarmPool instance
	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	if err := r.Get(ctx, req.NamespacedName, warmPool); err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Info("SandboxWarmPool resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get SandboxWarmPool")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !warmPool.DeletionTimestamp.IsZero() {
		logger.Info("SandboxWarmPool is being deleted")
		return ctrl.Result{}, nil
	}

	// Save old status for comparison
	oldStatus := warmPool.Status.DeepCopy()

	// Reconcile the pool (create or delete Sandboxes as needed)
	if err := r.reconcilePool(ctx, warmPool); err != nil {
		return ctrl.Result{}, err
	}

	// Update status if it has changed
	if err := r.updateStatus(ctx, oldStatus, warmPool); err != nil {
		logger.Error(err, "Failed to update SandboxWarmPool status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcilePool ensures the correct number of pre-allocated sandboxes exist in the pool.
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) error {
	logger := log.FromContext(ctx)

	// Compute hash of the warm pool name for the pool label
	poolNameHash := sandboxcontrollers.NameHash(warmPool.Name)

	// List all Sandbox CRs with the warm pool label
	sandboxList := &sandboxv1beta1.SandboxList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		warmPoolSandboxLabel: poolNameHash,
	})

	if err := r.List(ctx, sandboxList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     warmPool.Namespace,
	}); err != nil {
		logger.Error(err, "Failed to list sandboxes")
		return err
	}

	// Fetch template and compute hash once to avoid repeated expensive operations
	template, currentPodTemplateHash, tmplErr := r.fetchTemplateAndHash(ctx, warmPool)

	// Delete stale pods, filter pods by ownership and adopt orphans
	activeSandboxes, allErrors := r.filterActiveSandboxes(ctx, warmPool, sandboxList.Items, template, currentPodTemplateHash, tmplErr)

	const warmPoolReadinessGracePeriod = 5 * time.Minute

	now := time.Now()
	var healthySandboxes []sandboxv1beta1.Sandbox
	for _, sb := range activeSandboxes {
		if !isSandboxReady(&sb) && !sb.CreationTimestamp.IsZero() && now.Sub(sb.CreationTimestamp.Time) > warmPoolReadinessGracePeriod {
			logger.Info("Deleting stuck warm pool sandbox",
				"sandbox", sb.Name,
				"age", now.Sub(sb.CreationTimestamp.Time).Round(time.Second))
			if err := r.Delete(ctx, &sb); err != nil {
				logger.Error(err, "Failed to delete stuck sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
			}
			continue
		}
		healthySandboxes = append(healthySandboxes, sb)
	}
	activeSandboxes = healthySandboxes

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(activeSandboxes))

	logger.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
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

	// Create new sandboxes if we need more
	if currentReplicas < desiredReplicas && tmplErr == nil {
		sandboxesToCreate := min(desiredReplicas-currentReplicas, maxBatchSize)
		logger.Info("Creating new pool sandboxes", "count", sandboxesToCreate)

		sandboxCR, err := r.buildSandboxCR(warmPool, poolNameHash, template, currentPodTemplateHash)
		if err != nil {
			logger.Error(err, "Failed to build sandbox CR blueprint")
			allErrors = errors.Join(allErrors, err)
		} else {
			// Parallel sandbox creation with adaptive slow-start batching (starts with 1 and doubles on success)
			_, createErr := slowStartBatch(ctx, int(sandboxesToCreate), 1, func(_ int) error {
				return r.createPoolSandbox(ctx, warmPool, sandboxCR)
			})
			if createErr != nil {
				logger.Error(createErr, "Failed to create pool sandboxes")
				allErrors = errors.Join(allErrors, createErr)
			}
		}
	}

	// Delete excess sandboxes if we have too many
	if currentReplicas > desiredReplicas {
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
			return r.deletePoolSandbox(ctx, &activeSandboxes[idx])
		})
		if deleteErr != nil {
			logger.Error(deleteErr, "Failed to delete pool sandboxes")
			allErrors = errors.Join(allErrors, deleteErr)
		}
	}

	if tmplErr != nil && !k8serrors.IsNotFound(tmplErr) {
		allErrors = errors.Join(allErrors, tmplErr)
	}

	return allErrors
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
func (r *SandboxWarmPoolReconciler) filterActiveSandboxes(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool, sandboxes []sandboxv1beta1.Sandbox, template *extensionsv1beta1.SandboxTemplate, currentPodTemplateHash string, tmplErr error) ([]sandboxv1beta1.Sandbox, error) {
	logger := log.FromContext(ctx)
	var activeSandboxes []sandboxv1beta1.Sandbox
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
		if !sb.DeletionTimestamp.IsZero() {
			continue
		}

		controllerRef := metav1.GetControllerOf(&sb)
		isOrphan := controllerRef == nil
		isControlledByPool := controllerRef != nil && controllerRef.UID == warmPool.UID

		if !isOrphan && !isControlledByPool {
			logger.Info("Ignoring sandbox with different controller", "sandbox", sb.Name, "controller", controllerRef.Name)
			continue
		}

		if tmplErr == nil && (updateStrategy == extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType || isOrphan) {
			if r.isSandboxStale(ctx, &sb, template, currentPodTemplateHash, vettedHashes) {
				logger.Info("Deleting stale sandbox", "sandbox", sb.Name, "isOrphan", isOrphan)
				if err := r.Delete(ctx, &sb); err != nil {
					logger.Error(err, "Failed to delete stale sandbox", "sandbox", sb.Name)
					allErrors = errors.Join(allErrors, err)
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
	return activeSandboxes, allErrors
}

// computePodTemplateHash computes a hash of the sandbox template's Spec.PodTemplate.
func computePodTemplateHash(template *extensionsv1beta1.SandboxTemplate) (string, error) {
	specJSON, err := json.Marshal(template.Spec.PodTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to marshal pod template for hashing: %w", err)
	}
	return sandboxcontrollers.NameHash(string(specJSON)), nil
}

// fetchTemplateAndHash fetches the sandbox template and computes its hash.
func (r *SandboxWarmPoolReconciler) fetchTemplateAndHash(ctx context.Context, warmPool *extensionsv1beta1.SandboxWarmPool) (*extensionsv1beta1.SandboxTemplate, string, error) {
	logger := log.FromContext(ctx)
	template, tmplErr := r.getTemplate(ctx, warmPool)
	var currentPodTemplateHash string
	if tmplErr == nil {
		currentPodTemplateHash, tmplErr = computePodTemplateHash(template)
	}

	if tmplErr != nil {
		logger.Error(tmplErr, "Failed to get sandbox template and hash", "templateRef", warmPool.Spec.TemplateRef.Name)
	}
	return template, currentPodTemplateHash, tmplErr
}

// buildSandboxCR constructs the base Sandbox CR (with pod template and volume claim templates) for the warm pool.
func (r *SandboxWarmPoolReconciler) buildSandboxCR(warmPool *extensionsv1beta1.SandboxWarmPool, poolNameHash string, template *extensionsv1beta1.SandboxTemplate, currentPodTemplateHash string) (*sandboxv1beta1.Sandbox, error) {
	sandboxLabels := map[string]string{
		warmPoolSandboxLabel:                       poolNameHash,
		sandboxTemplateRefHash:                     SandboxTemplateRefHash(warmPool.Spec.TemplateRef.Name),
		sandboxv1beta1.SandboxLaunchTypeLabel:      sandboxv1beta1.SandboxLaunchTypeWarm,
		sandboxv1beta1.SandboxPodTemplateHashLabel: currentPodTemplateHash,
	}

	// Build annotations for the Sandbox CR
	sandboxAnnotations := map[string]string{
		sandboxv1beta1.SandboxTemplateRefAnnotation: warmPool.Spec.TemplateRef.Name,
	}

	// Copy template pod labels into sandbox pod template
	podLabels := make(map[string]string)
	maps.Copy(podLabels, template.Spec.PodTemplate.ObjectMeta.Labels)
	// Propagate pool and template labels to pod template for consistency and targeting
	podLabels[warmPoolSandboxLabel] = poolNameHash
	podLabels[sandboxTemplateRefHash] = SandboxTemplateRefHash(warmPool.Spec.TemplateRef.Name)
	podLabels[sandboxv1beta1.SandboxPodTemplateHashLabel] = currentPodTemplateHash

	podAnnotations := make(map[string]string)
	maps.Copy(podAnnotations, template.Spec.PodTemplate.ObjectMeta.Annotations)

	// Respect the template's custom eviction annotation if explicitly specified.
	// Only apply the default eviction behavior if the annotation is not defined.
	if _, exists := template.Spec.PodTemplate.ObjectMeta.Annotations[warmPoolEvictionAnnotation]; !exists {
		if r.EnableWarmPoolEviction {
			podAnnotations[warmPoolEvictionAnnotation] = "true"
		}
	}

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", warmPool.Name),
			Namespace:    warmPool.Namespace,
			Labels:       sandboxLabels,
			Annotations:  sandboxAnnotations,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			Service: template.Spec.Service,
			PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: *template.Spec.PodTemplate.Spec.DeepCopy(),
				ObjectMeta: sandboxv1beta1.PodMetadata{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
			},
		},
	}

	// Copy volumeClaimTemplates from template to sandbox
	if len(template.Spec.VolumeClaimTemplates) > 0 {
		sandbox.Spec.VolumeClaimTemplates = make([]sandboxv1beta1.PersistentVolumeClaimTemplate, len(template.Spec.VolumeClaimTemplates))
		for i, vct := range template.Spec.VolumeClaimTemplates {
			vct.DeepCopyInto(&sandbox.Spec.VolumeClaimTemplates[i])
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

	patch := &extensionsv1beta1.SandboxWarmPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       "SandboxWarmPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      warmPool.Name,
			Namespace: warmPool.Namespace,
		},
		Status: warmPool.Status,
	}

	if err := r.Status().Patch(ctx, patch, client.Apply, client.FieldOwner("warmpool-controller"), client.ForceOwnership); err != nil { //nolint:staticcheck // SA1019: client.Apply requires generated apply configurations
		logger.Error(err, "Failed to apply SandboxWarmPool status via SSA")
		return err
	}

	logger.Info("Updated SandboxWarmPool status", "replicas", warmPool.Status.Replicas)
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
	currentPodTemplateHash string,
	vettedHashes map[string]bool,
) bool {
	sandboxHash := sandbox.Labels[sandboxv1beta1.SandboxPodTemplateHashLabel]

	// If the templateRefHash doesn't match, it's stale.
	if sandbox.Labels[sandboxTemplateRefHash] != SandboxTemplateRefHash(template.Name) {
		return true
	}

	// Check if the sandbox is unowned (orphaned).
	controllerRef := metav1.GetControllerOf(sandbox)
	isOrphan := controllerRef == nil
	if isOrphan {
		// Always perform full semantic comparison for orphans.
		return !r.comparePodSpecs(template, &sandbox.Spec.PodTemplate.Spec)
	}

	// If hashes match, it's fresh.
	if sandboxHash != "" && sandboxHash == currentPodTemplateHash {
		return false
	}

	// If currentPodTemplateHash is empty, it means we failed to compute it.
	// In this case, we should log an error and treat it as NOT stale to avoid
	// mass-deleting existing sandboxes due to a marshal failure.
	if currentPodTemplateHash == "" {
		log.FromContext(ctx).Error(nil, "currentPodTemplateHash is empty, skipping staleness check", "sandbox", sandbox.Name)
		return false
	}

	// Check if we've already evaluated this specific old version.
	if sandboxHash != "" {
		if isStale, found := vettedHashes[sandboxHash]; found {
			return isStale
		}
	}

	// Perform Semantic DeepEqual on the Pod Spec only.
	// We normalize the pod spec by applying the same secure defaults
	// used during creation to avoid false positives from controller-injected fields.
	isStale := !r.comparePodSpecs(template, &sandbox.Spec.PodTemplate.Spec)

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

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	if r.MaxBatchSize <= 0 {
		r.MaxBatchSize = sandboxCreateDeleteMaxBatchSize
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &extensionsv1beta1.SandboxWarmPool{}, extensionsv1beta1.TemplateRefField, func(rawObj client.Object) []string {
		wp := rawObj.(*extensionsv1beta1.SandboxWarmPool)
		if wp.Spec.TemplateRef.Name == "" {
			return nil
		}
		return []string{wp.Spec.TemplateRef.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1beta1.SandboxWarmPool{}).
		Owns(&sandboxv1beta1.Sandbox{}).
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
