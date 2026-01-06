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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	poolLabel              = "agents.x-k8s.io/pool"
	sandboxTemplateRefHash = "agents.x-k8s.io/sandbox-template-ref-hash"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object.
//
// The reconciler maintains a pool of pre-warmed pods with PVCs ready for
// instant allocation. When the pool size is below the desired replicas,
// it creates new pods with their associated PVCs from volumeClaimTemplates.
//
// IMPORTANT: PVC Explosion Prevention
// -----------------------------------
// This controller watches PVCs it owns (Owns(&corev1.PersistentVolumeClaim{})).
// Without careful handling, this can cause a "PVC explosion" bug:
//
//  1. Reconcile runs, creates PVC (triggers watch event)
//  2. PVC watch triggers new reconcile BEFORE pod is created
//  3. New reconcile sees 0 pods, creates another PVC+pod
//  4. Infinite loop creating thousands of PVCs
//
// The fix: Before creating new pods, count owned PVCs. If ownedPVCs > currentPods,
// a creation is already in progress - skip creating more until the pod catches up.
// See reconcilePool() for implementation.
type SandboxWarmPoolReconciler struct {
	client.Client
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

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

	// Compute hash of the warm pool name for the pool label
	poolNameHash := sandboxcontrollers.NameHash(warmPool.Name)

	// List all pods with the pool label matching the warm pool name hash
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		poolLabel: poolNameHash,
	})

	if err := r.List(ctx, podList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     warmPool.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list pods")
		return err
	}

	// Filter pods by ownership and adopt orphans
	var activePods []corev1.Pod
	var allErrors error

	for _, pod := range podList.Items {
		// Skip pods that are being deleted
		if !pod.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}

		// Get the controller owner reference
		controllerRef := metav1.GetControllerOf(&pod)

		if controllerRef == nil {
			// Pod has no controller - adopt it
			log.Info("Adopting orphaned pod", "pod", pod.Name)
			if err := r.adoptPod(ctx, warmPool, &pod); err != nil {
				log.Error(err, "Failed to adopt pod", "pod", pod.Name)
				allErrors = errors.Join(allErrors, err)
				continue
			}
			activePods = append(activePods, pod)
		} else if controllerRef.UID == warmPool.UID {
			// Pod belongs to this warmpool - include it
			activePods = append(activePods, pod)
		} else {
			// Pod has a different controller - ignore it
			log.Info("Ignoring pod with different controller",
				"pod", pod.Name,
				"controller", controllerRef.Name,
				"controllerKind", controllerRef.Kind)
		}
	}

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(activePods))

	// PVC Explosion Prevention (see SandboxWarmPoolReconciler docs for full explanation)
	//
	// We must detect if a pod creation is already in progress. The sequence is:
	//   createPoolPod() -> Create PVC -> Create Pod
	//
	// If PVC creation triggers a reconcile before Pod creation completes,
	// we'd see ownedPVCs=1, currentPods=0, and incorrectly try to create another pod.
	//
	// Solution: If ownedPVCs > currentPods, creation is in progress - don't create more.
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     warmPool.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list PVCs")
		return err
	}

	// Count PVCs owned by this warm pool
	ownedPVCs := int32(0)
	for _, pvc := range pvcList.Items {
		if pvc.ObjectMeta.DeletionTimestamp.IsZero() {
			controllerRef := metav1.GetControllerOf(&pvc)
			if controllerRef != nil && controllerRef.UID == warmPool.UID {
				ownedPVCs++
			}
		}
	}

	creationInProgress := ownedPVCs > currentReplicas

	log.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
		"ownedPVCs", ownedPVCs,
		"creationInProgress", creationInProgress,
		"poolName", warmPool.Name,
		"poolNameHash", poolNameHash)

	// Update status replicas
	warmPool.Status.Replicas = currentReplicas

	// Calculate ready replicas
	readyReplicas := int32(0)
	for _, pod := range activePods {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				readyReplicas++
				break
			}
		}
	}
	warmPool.Status.ReadyReplicas = readyReplicas

	// Create new pods if we need more AND no creation is in progress
	if currentReplicas < desiredReplicas && !creationInProgress {
		podsToCreate := desiredReplicas - currentReplicas
		log.Info("Creating new pods", "count", podsToCreate)

		for i := int32(0); i < podsToCreate; i++ {
			if err := r.createPoolPod(ctx, warmPool, poolNameHash); err != nil {
				log.Error(err, "Failed to create pod")
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	// Delete excess pods if we have too many
	if currentReplicas > desiredReplicas {
		podsToDelete := currentReplicas - desiredReplicas
		log.Info("Deleting excess pods", "count", podsToDelete)

		// Sort active pods by creation timestamp (newest first)
		sort.Slice(activePods, func(i, j int) bool {
			return activePods[i].CreationTimestamp.After(activePods[j].CreationTimestamp.Time)
		})

		// Delete the first N active pods from the sorted list (newest first)
		for i := int32(0); i < podsToDelete && i < int32(len(activePods)); i++ {
			pod := &activePods[i]

			if err := r.Delete(ctx, pod); err != nil {
				log.Error(err, "Failed to delete pod", "pod", pod.Name)
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	return allErrors
}

// adoptPod sets this warmpool as the owner of an orphaned pod
func (r *SandboxWarmPoolReconciler) adoptPod(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, pod *corev1.Pod) error {
	if err := controllerutil.SetControllerReference(warmPool, pod, r.Scheme()); err != nil {
		return err
	}
	return r.Update(ctx, pod)
}

// createPoolPod creates a new pod for the warm pool, along with any required PVCs
func (r *SandboxWarmPoolReconciler) createPoolPod(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, poolNameHash string) error {
	log := log.FromContext(ctx)

	// Create labels for the pod
	podLabels := make(map[string]string)
	podLabels[poolLabel] = poolNameHash
	podLabels[sandboxTemplateRefHash] = sandboxcontrollers.NameHash(warmPool.Spec.TemplateRef.Name)

	// Try getting template
	var template *extensionsv1alpha1.SandboxTemplate
	var err error
	if template, err = r.getTemplate(ctx, warmPool); err != nil {
		log.Error(err, "Failed to get sandbox template for warm pool", "warmPoolName", warmPool.Name)
		return err
	}

	for k, v := range template.Spec.PodTemplate.ObjectMeta.Labels {
		podLabels[k] = v
	}

	// Create annotations for the pod
	podAnnotations := make(map[string]string)
	for k, v := range template.Spec.PodTemplate.ObjectMeta.Annotations {
		podAnnotations[k] = v
	}

	// Generate a unique suffix for this pod instance
	podSuffix := sandboxcontrollers.RandomSuffix(5)
	podName := fmt.Sprintf("%s-%s", warmPool.Name, podSuffix)

	// Create PVCs from volumeClaimTemplates
	var pvcVolumes []corev1.Volume
	for _, vctTemplate := range template.Spec.VolumeClaimTemplates {
		pvcName := fmt.Sprintf("%s-%s", vctTemplate.Name, podName)

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pvcName,
				Namespace:   warmPool.Namespace,
				Labels:      podLabels,
				Annotations: vctTemplate.Annotations,
			},
			Spec: vctTemplate.Spec,
		}

		// Copy labels from template
		if pvc.Labels == nil {
			pvc.Labels = make(map[string]string)
		}
		for k, v := range vctTemplate.Labels {
			pvc.Labels[k] = v
		}

		// Set controller reference so the PVC is owned by the SandboxWarmPool
		if err := ctrl.SetControllerReference(warmPool, pvc, r.Client.Scheme()); err != nil {
			return fmt.Errorf("SetControllerReference for PVC failed: %w", err)
		}

		// Create the PVC
		if err := r.Create(ctx, pvc); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				log.Error(err, "Failed to create PVC", "pvc", pvcName)
				return err
			}
		} else {
			log.Info("Created PVC for warm pool pod", "pvc", pvcName)
		}

		// Add volume reference for pod spec
		// Note: We don't wait for PVC binding - the scheduler will wait for it
		pvcVolumes = append(pvcVolumes, corev1.Volume{
			Name: vctTemplate.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
	}

	// Create the pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   warmPool.Namespace,
			Labels:      podLabels,
			Annotations: podAnnotations,
		},
		Spec: *template.Spec.PodTemplate.Spec.DeepCopy(),
	}

	// Add PVC volumes to pod spec
	pod.Spec.Volumes = append(pod.Spec.Volumes, pvcVolumes...)

	// Set controller reference so the Pod is owned by the SandboxWarmPool
	if err := ctrl.SetControllerReference(warmPool, pod, r.Client.Scheme()); err != nil {
		return fmt.Errorf("SetControllerReference for Pod failed: %w", err)
	}

	// Create the Pod
	if err := r.Create(ctx, pod); err != nil {
		log.Error(err, "Failed to create pod")
		return err
	}

	log.Info("Created new pool pod", "pod", pod.Name, "poolName", warmPool.Name, "poolNameHash", poolNameHash)
	return nil
}

// updateStatus updates the status of the SandboxWarmPool if it has changed
func (r *SandboxWarmPoolReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxWarmPoolStatus, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Check if status has changed
	if equality.Semantic.DeepEqual(oldStatus, &warmPool.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, warmPool); err != nil {
		log.Error(err, "Failed to update SandboxWarmPool status")
		return err
	}

	log.Info("Updated SandboxWarmPool status", "replicas", warmPool.Status.Replicas)
	return nil
}

func (r *SandboxWarmPoolReconciler) getTemplate(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) (*extensionsv1alpha1.SandboxTemplate, error) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: warmPool.Namespace,
			Name:      warmPool.Spec.TemplateRef.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
		if !k8errors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox template %q: %w", warmPool.Spec.TemplateRef.Name, err)
		}
		return nil, err
	}

	return template, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxWarmPool{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
