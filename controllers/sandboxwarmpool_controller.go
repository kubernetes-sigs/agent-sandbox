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

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	poolLabel               = "agents.x-k8s.io/pool"
	podTemplateHashLabel    = "agents.x-k8s.io/pod-template-hash"
	conditionTypeInProgress = "InProgress"
	conditionTypeCurrent    = "Current"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object
type SandboxWarmPoolReconciler struct {
	client.Client
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the reconciliation loop for SandboxWarmPool
func (r *SandboxWarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the SandboxWarmPool instance
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	if err := r.Get(ctx, req.NamespacedName, warmPool); err != nil {
		if k8serrors.IsNotFound(err) {
			log.Info("sandboxwarmpool resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !warmPool.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("SandboxWarmPool is being deleted")
		return ctrl.Result{}, nil
	}

	oldStatus := warmPool.Status.DeepCopy()

	// Compute pod template hash and update labels
	if err := r.updatePodTemplateHashLabel(ctx, warmPool); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile the pool
	if err := r.reconcilePool(ctx, warmPool); err != nil {
		return ctrl.Result{}, err
	}

	// Update status if changed
	if err := r.updateStatus(ctx, oldStatus, warmPool); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// updatePodTemplateHashLabel computes the hash of the pod template and updates the SandboxWarmPool labels
func (r *SandboxWarmPoolReconciler) updatePodTemplateHashLabel(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Compute hash from the pod template
	hash := ComputePodTemplateHash(&warmPool.Spec.PodTemplate)

	// Check if label needs to be updated
	if warmPool.Labels == nil {
		warmPool.Labels = make(map[string]string)
	}

	if warmPool.Labels[podTemplateHashLabel] == hash {
		// No update needed
		return nil
	}

	// Update the label
	warmPool.Labels[podTemplateHashLabel] = hash

	if err := r.Update(ctx, warmPool); err != nil {
		log.Error(err, "Failed to update SandboxWarmPool labels")
		return fmt.Errorf("failed to update SandboxWarmPool labels: %w", err)
	}

	log.Info("Updated pod-template-hash label", "hash", hash)
	return nil
}

// reconcilePool ensures the correct number of sandboxes exist in the pool
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Get the pool label value (pool-name-hash)
	poolLabelValue := r.getPoolLabelValue(warmPool)

	// List all sandboxes with the pool label
	sandboxList := &sandboxv1alpha1.SandboxList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		poolLabel: poolLabelValue,
	})
	if err := r.List(ctx, sandboxList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     warmPool.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list sandboxes")
		return err
	}

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(sandboxList.Items))
	readyReplicas := r.countReadySandboxes(sandboxList.Items)

	// Update status replicas and readyReplicas
	warmPool.Status.Replicas = currentReplicas
	warmPool.Status.ReadyReplicas = readyReplicas

	var allErrors error

	// Create new sandboxes if we need more
	if currentReplicas < desiredReplicas {
		sandboxesToCreate := desiredReplicas - currentReplicas
		log.Info("Creating new sandboxes", "count", sandboxesToCreate, "desired", desiredReplicas, "current", currentReplicas)

		for i := int32(0); i < sandboxesToCreate; i++ {
			if err := r.createPoolSandbox(ctx, warmPool, poolLabelValue); err != nil {
				log.Error(err, "Failed to create sandbox")
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	// Delete excess sandboxes if we have too many
	if currentReplicas > desiredReplicas {
		sandboxesToDelete := currentReplicas - desiredReplicas
		log.Info("Deleting excess sandboxes", "count", sandboxesToDelete, "desired", desiredReplicas, "current", currentReplicas)

		for i := int32(0); i < sandboxesToDelete && i < int32(len(sandboxList.Items)); i++ {
			sandbox := &sandboxList.Items[i]
			if err := r.Delete(ctx, sandbox); err != nil && !k8serrors.IsNotFound(err) {
				log.Error(err, "Failed to delete sandbox", "sandbox", sandbox.Name)
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	// Update conditions based on the pool state
	r.updateConditions(warmPool, allErrors)

	return allErrors
}

// createPoolSandbox creates a new sandbox for the warm pool
func (r *SandboxWarmPoolReconciler) createPoolSandbox(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, poolLabelValue string) error {
	log := log.FromContext(ctx)

	// Generate a unique name for the sandbox
	sandboxName := fmt.Sprintf("%s-%s", warmPool.Name, rand.String(5))

	// Create labels for the sandbox
	sandboxLabels := make(map[string]string)
	sandboxLabels[poolLabel] = poolLabelValue

	// Copy labels from pod template
	for k, v := range warmPool.Spec.PodTemplate.ObjectMeta.Labels {
		sandboxLabels[k] = v
	}

	// Create annotations for the sandbox
	sandboxAnnotations := make(map[string]string)
	for k, v := range warmPool.Spec.PodTemplate.ObjectMeta.Annotations {
		sandboxAnnotations[k] = v
	}

	// Create the sandbox
	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sandboxName,
			Namespace:   warmPool.Namespace,
			Labels:      sandboxLabels,
			Annotations: sandboxAnnotations,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			PodTemplate: warmPool.Spec.PodTemplate,
		},
	}

	sandbox.SetGroupVersionKind(sandboxv1alpha1.GroupVersion.WithKind("Sandbox"))
	if err := ctrl.SetControllerReference(warmPool, sandbox, r.Client.Scheme()); err != nil {
		return fmt.Errorf("SetControllerReference for Sandbox failed: %w", err)
	}

	if err := r.Create(ctx, sandbox, client.FieldOwner("sandboxwarmpool-controller")); err != nil {
		log.Error(err, "Failed to create sandbox", "sandbox", sandboxName)
		return err
	}

	log.Info("Created new pool sandbox", "sandbox", sandboxName)
	return nil
}

// getPoolLabelValue generates the pool label value: <pool-name-hash>
func (r *SandboxWarmPoolReconciler) getPoolLabelValue(warmPool *extensionsv1alpha1.SandboxWarmPool) string {
	hash := NameHash(warmPool.Name)
	return fmt.Sprintf("%s-%s", warmPool.Name, hash)
}

// countReadySandboxes counts the number of ready sandboxes in the list
func (r *SandboxWarmPoolReconciler) countReadySandboxes(sandboxes []sandboxv1alpha1.Sandbox) int32 {
	count := int32(0)
	for _, sandbox := range sandboxes {
		for _, condition := range sandbox.Status.Conditions {
			if condition.Type == string(sandboxv1alpha1.SandboxConditionReady) && condition.Status == metav1.ConditionTrue {
				count++
				break
			}
		}
	}
	return count
}

// updateConditions updates the status conditions of the SandboxWarmPool
func (r *SandboxWarmPoolReconciler) updateConditions(warmPool *extensionsv1alpha1.SandboxWarmPool, reconcileErr error) {
	if reconcileErr != nil {
		// Set InProgress condition if there was an error
		meta.SetStatusCondition(&warmPool.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInProgress,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: warmPool.Generation,
			Reason:             "ReconcileError",
			Message:            fmt.Sprintf("Error during reconciliation: %v", reconcileErr),
		})
		meta.SetStatusCondition(&warmPool.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCurrent,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: warmPool.Generation,
			Reason:             "ReconcileError",
			Message:            fmt.Sprintf("Error during reconciliation: %v", reconcileErr),
		})
		return
	}

	// Check if all replicas are ready
	if warmPool.Status.ReadyReplicas == warmPool.Spec.Replicas {
		// Pool is current
		meta.SetStatusCondition(&warmPool.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCurrent,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: warmPool.Generation,
			Reason:             "PoolReady",
			Message:            fmt.Sprintf("All %d replicas are ready", warmPool.Spec.Replicas),
		})
		meta.SetStatusCondition(&warmPool.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInProgress,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: warmPool.Generation,
			Reason:             "PoolReady",
			Message:            fmt.Sprintf("All %d replicas are ready", warmPool.Spec.Replicas),
		})
	} else {
		// Pool is in progress
		meta.SetStatusCondition(&warmPool.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInProgress,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: warmPool.Generation,
			Reason:             "PoolScaling",
			Message:            fmt.Sprintf("Pool is scaling: %d/%d replicas ready", warmPool.Status.ReadyReplicas, warmPool.Spec.Replicas),
		})
		meta.SetStatusCondition(&warmPool.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCurrent,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: warmPool.Generation,
			Reason:             "PoolScaling",
			Message:            fmt.Sprintf("Pool is scaling: %d/%d replicas ready", warmPool.Status.ReadyReplicas, warmPool.Spec.Replicas),
		})
	}
}

// updateStatus updates the status of the SandboxWarmPool if it has changed
func (r *SandboxWarmPoolReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxWarmPoolStatus, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Check if status has changed
	if oldStatus.Replicas == warmPool.Status.Replicas &&
		oldStatus.ReadyReplicas == warmPool.Status.ReadyReplicas &&
		conditionsEqual(oldStatus.Conditions, warmPool.Status.Conditions) {
		return nil
	}

	if err := r.Status().Update(ctx, warmPool); err != nil {
		log.Error(err, "Failed to update SandboxWarmPool status")
		return err
	}

	return nil
}

// conditionsEqual checks if two condition slices are equal
func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]metav1.Condition)
	for _, cond := range a {
		aMap[cond.Type] = cond
	}

	for _, cond := range b {
		aCond, ok := aMap[cond.Type]
		if !ok || aCond.Status != cond.Status || aCond.Reason != cond.Reason {
			return false
		}
	}

	return true
}

// ComputePodTemplateHash computes a hash of the pod template
func ComputePodTemplateHash(podTemplate interface{}) string {
	// For now, use a simple hash based on the pod template spec
	// In production, you might want a more sophisticated hashing mechanism
	// that serializes the entire spec and computes a hash
	return NameHash(fmt.Sprintf("%v", podTemplate))
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxWarmPool{}).
		Owns(&sandboxv1alpha1.Sandbox{}).
		Complete(r)
}
