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

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	poolLabel = "agents.x-k8s.io/pool"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object
type SandboxWarmPoolReconciler struct {
	client.Client
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the reconciliation loop for SandboxWarmPool
func (r *SandboxWarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the SandboxWarmPool instance
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	if err := r.Get(ctx, req.NamespacedName, warmPool); err != nil {
		if k8serrors.IsNotFound(err) {
			log.Info("SandboxWarmPool resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get SandboxWarmPool")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !warmPool.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("SandboxWarmPool is being deleted")
		return ctrl.Result{}, nil
	}

	// Save old status for comparison
	oldStatus := warmPool.Status.DeepCopy()

	// Reconcile the pool (create or delete Pods as needed)
	if err := r.reconcilePool(ctx, warmPool); err != nil {
		return ctrl.Result{}, err
	}

	// Update status if it has changed
	if err := r.updateStatus(ctx, oldStatus, warmPool); err != nil {
		log.Error(err, "Failed to update SandboxWarmPool status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcilePool ensures the correct number of pods exist in the pool
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Get the pool label value (pool-name-hash)
	poolLabelValue := warmPool.Name

	// List all pods with the pool label
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		poolLabel: poolLabelValue,
	})

	if err := r.List(ctx, podList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     warmPool.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list pods")
		return err
	}

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(podList.Items))

	log.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
		"poolLabel", poolLabelValue)

	// Update status replicas
	warmPool.Status.Replicas = currentReplicas

	var allErrors error

	// Create new pods if we need more
	if currentReplicas < desiredReplicas {
		podsToCreate := desiredReplicas - currentReplicas
		log.Info("Creating new pods", "count", podsToCreate)

		for i := int32(0); i < podsToCreate; i++ {
			// Generate deterministic name based on current count + index
			podName := fmt.Sprintf("%s-pod-%d", warmPool.Name, currentReplicas+i)

			if err := r.createPoolPodWithName(ctx, warmPool, poolLabelValue, podName); err != nil {
				if k8serrors.IsAlreadyExists(err) {
					// Another reconcile already created this - that's fine
					log.V(1).Info("Pod already exists", "pod", podName)
					continue
				}
				log.Error(err, "Failed to create pod")
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	// Delete excess pods if we have too many
	if currentReplicas > desiredReplicas {
		podsToDelete := currentReplicas - desiredReplicas
		log.Info("Deleting excess pods",
			"count", podsToDelete,
			"desired", desiredReplicas,
			"current", currentReplicas)

		// Delete pods with indices >= desiredReplicas (scale down from the top)
		for i := desiredReplicas; i < currentReplicas; i++ {
			podName := fmt.Sprintf("%s-pod-%d", warmPool.Name, i)
			
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: warmPool.Namespace,
				},
			}
			
			if err := r.Delete(ctx, pod); err != nil {
				if k8serrors.IsNotFound(err) {
					// Already deleted by another reconcile or doesn't exist - that's fine
					log.V(1).Info("Pod already deleted or not found", "pod", podName)
					continue
				}
				log.Error(err, "Failed to delete pod", "pod", podName)
				allErrors = errors.Join(allErrors, err)
			} else {
				log.Info("Deleted excess pod", "pod", podName)
			}
		}
	}

	return allErrors
}

// createPoolPod creates a new pod for the warm pool
func (r *SandboxWarmPoolReconciler) createPoolPodWithName(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, poolLabelValue string, podName string) error {
	log := log.FromContext(ctx)

	// Create labels for the pod
	podLabels := make(map[string]string)
	podLabels[poolLabel] = poolLabelValue

	// Copy labels from pod template
	for k, v := range warmPool.Spec.PodTemplate.ObjectMeta.Labels {
		podLabels[k] = v
	}

	// Create annotations for the pod
	podAnnotations := make(map[string]string)
	for k, v := range warmPool.Spec.PodTemplate.ObjectMeta.Annotations {
		podAnnotations[k] = v
	}

	// Create the pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   warmPool.Namespace,
			Labels:      podLabels,
			Annotations: podAnnotations,
		},
		Spec: warmPool.Spec.PodTemplate.Spec,
	}

	// Set controller reference so the Pod is owned by the SandboxWarmPool
	if err := ctrl.SetControllerReference(warmPool, pod, r.Client.Scheme()); err != nil {
		return fmt.Errorf("SetControllerReference for Pod failed: %w", err)
	}

	// Create the Pod
	if err := r.Create(ctx, pod); err != nil {
		log.Error(err, "Failed to create pod", "pod", podName)
		return err
	}

	log.Info("Created new pool pod", "pod", podName, "pool", poolLabelValue)
	return nil
}

// updateStatus updates the status of the SandboxWarmPool if it has changed
func (r *SandboxWarmPoolReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxWarmPoolStatus, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Check if status has changed
	if oldStatus.Replicas == warmPool.Status.Replicas {
		return nil
	}

	if err := r.Status().Update(ctx, warmPool); err != nil {
		log.Error(err, "Failed to update SandboxWarmPool status")
		return err
	}

	log.Info("Updated SandboxWarmPool status", "replicas", warmPool.Status.Replicas)
	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxWarmPool{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
