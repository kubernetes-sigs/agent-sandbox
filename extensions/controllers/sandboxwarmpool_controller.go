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
	"math/rand"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	poolLabel              = "agents.x-k8s.io/pool"
	sandboxTemplateRefHash = "agents.x-k8s.io/sandbox-template-ref-hash"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object
type SandboxWarmPoolReconciler struct {
	client.Client
	MaxConcurrentWorkers int
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/finalizers,verbs=get;update;patch
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
	if !warmPool.DeletionTimestamp.IsZero() {
		log.Info("SandboxWarmPool is being deleted")
		return ctrl.Result{}, nil
	}

	// Save old status for comparison
	oldStatus := warmPool.Status.DeepCopy()

	// Reconcile the pool (create or delete Pods as needed)
	reconcileErr := r.reconcilePool(ctx, warmPool)

	// Update status if it has changed
	if err := r.updateStatus(ctx, oldStatus, warmPool); err != nil {
		reconcileErr = errors.Join(reconcileErr, err)
	}

	// Optimization 2: Fast retry with randomDelay for expected transient API errors during concurrent provisioning
	// Because the WarmPool provisions pods concurrently in large batches, it is expected
	// to occasionally hit APF rate limits (429) or transient conflicts (409).
	// We intercept these specific API errors and apply a fast, randomDelay retry to avoid
	// falling into the default controller-runtime exponential backoff queue (which can sleep up to 30s).
	if reconcileErr != nil {
		if k8serrors.IsTooManyRequests(reconcileErr) || k8serrors.IsConflict(reconcileErr) || k8serrors.IsServerTimeout(reconcileErr) {
			randomDelay := time.Duration(rand.Intn(400)+100) * time.Millisecond
			log.Info("Transient API error during concurrent provisioning, applying fast retry", "error", reconcileErr.Error())
			return ctrl.Result{RequeueAfter: randomDelay}, nil
		}
	}

	// All other unexpected errors fall through to the standard rate limiter
	return ctrl.Result{}, reconcileErr
}

// reconcilePool ensures the correct number of pods exist in the pool
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Compute hash of the warm pool name for the pool label
	poolNameHash := sandboxcontrollers.NameHash(warmPool.Name)

	// List all pods with the pool label matching the warm pool name hash
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{poolLabel: poolNameHash}),
		Namespace:     warmPool.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list pods")
		return err
	}

	// Filter pods by ownership and adopt orphans
	var activePods []*corev1.Pod
	var orphanedPods []*corev1.Pod

	for i := range podList.Items {
		// Skip pods that are being deleted
		pod := &podList.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		// Get the controller owner reference
		controllerRef := metav1.GetControllerOf(pod)
		if controllerRef == nil {
			orphanedPods = append(orphanedPods, pod)
		} else if controllerRef.UID == warmPool.UID {
			activePods = append(activePods, pod)
		}
	}

	var allErrors error

	// Adopt orphans concurrently
	if len(orphanedPods) > 0 {
		var adoptEg errgroup.Group
		adoptEg.SetLimit(r.MaxConcurrentWorkers)
		for _, pod := range orphanedPods {
			p := pod
			adoptEg.Go(func() error {
				return r.adoptPod(ctx, warmPool, p)
			})
			activePods = append(activePods, p)
		}
		if err := adoptEg.Wait(); err != nil {
			allErrors = errors.Join(allErrors, err)
		}
	}

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(activePods))

	log.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
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

	// Optimization: Provision pods concurrently to meet target replicas rapidly
	if currentReplicas < desiredReplicas {
		podsToCreate := desiredReplicas - currentReplicas
		log.Info("Creating new pods concurrently", "count", podsToCreate)

		// fetch template once
		template, err := r.getTemplate(ctx, warmPool)
		if err != nil {
			return errors.Join(allErrors, err)
		}

		var eg errgroup.Group
		eg.SetLimit(r.MaxConcurrentWorkers) // Fire up to MaxConcurrentWorkers creation requests in parallel

		for i := int32(0); i < podsToCreate; i++ {
			eg.Go(func() error {
				// Pass the template in so we don't fetch it 150 times
				return r.createPoolPod(ctx, warmPool, poolNameHash, template)
			})
		}

		if err := eg.Wait(); err != nil {
			allErrors = errors.Join(allErrors, err)
		}
	}

	// Optimization: Delete excess pods concurrently
	if currentReplicas > desiredReplicas {
		podsToDelete := currentReplicas - desiredReplicas
		log.Info("Deleting excess pods concurrently", "count", podsToDelete)

		// Sort active pods by creation timestamp (newest first)
		sort.Slice(activePods, func(i, j int) bool {
			if activePods[i].CreationTimestamp.Equal(&activePods[j].CreationTimestamp) {
				return activePods[i].Name < activePods[j].Name
			}
			return activePods[i].CreationTimestamp.After(activePods[j].CreationTimestamp.Time)
		})

		var eg errgroup.Group
		eg.SetLimit(r.MaxConcurrentWorkers)

		for i := int32(0); i < podsToDelete && i < int32(len(activePods)); i++ {
			pod := activePods[i]

			// Capture loop variable for the goroutine
			p := pod
			eg.Go(func() error {
				if err := r.Delete(ctx, p); err != nil {
					log.Error(err, "Failed to delete pod", "pod", p.Name)
					return err
				}
				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			allErrors = errors.Join(allErrors, err)
		}
	}

	return allErrors
}

// adoptPod sets this warmpool as the owner of an orphaned pod
func (r *SandboxWarmPoolReconciler) adoptPod(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, pod *corev1.Pod) error {
	// Use MergePatch instead of Update to eliminate "Object Modified" conflicts
	// if the pod status changes during adoption.
	patchBase := pod.DeepCopy()
	if err := controllerutil.SetControllerReference(warmPool, pod, r.Scheme()); err != nil {
		return err
	}
	return r.Patch(ctx, pod, client.MergeFrom(patchBase))
}

// createPoolPod creates a new pod for the warm pool
func (r *SandboxWarmPoolReconciler) createPoolPod(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, poolNameHash string, template *extensionsv1alpha1.SandboxTemplate) error {
	log := log.FromContext(ctx)

	// Create labels for the pod
	podLabels := make(map[string]string)
	podLabels[poolLabel] = poolNameHash
	podLabels[sandboxTemplateRefHash] = sandboxcontrollers.NameHash(warmPool.Spec.TemplateRef.Name)

	for k, v := range template.Spec.PodTemplate.ObjectMeta.Labels {
		podLabels[k] = v
	}

	// Create annotations for the pod
	podAnnotations := make(map[string]string)
	for k, v := range template.Spec.PodTemplate.ObjectMeta.Annotations {
		podAnnotations[k] = v
	}

	// Create the pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", warmPool.Name),
			Namespace:    warmPool.Namespace,
			Labels:       podLabels,
			Annotations:  podAnnotations,
		},
		Spec: template.Spec.PodTemplate.Spec,
	}

	// Set controller reference so the Pod is owned by the SandboxWarmPool
	if err := ctrl.SetControllerReference(warmPool, pod, r.Scheme()); err != nil {
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

	patch := &extensionsv1alpha1.SandboxWarmPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: extensionsv1alpha1.GroupVersion.String(),
			Kind:       "SandboxWarmPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      warmPool.Name,
			Namespace: warmPool.Namespace,
		},
		Status: warmPool.Status,
	}

	// Send the Server-Side Apply request to update the status subresource
	if err := r.Status().Patch(ctx, patch, client.Apply, client.FieldOwner("warmpool-controller"), client.ForceOwnership); err != nil {
		log.Error(err, "Failed to apply SandboxWarmPool status via SSA")
		return err
	}

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
		if !k8serrors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox template %q: %w", warmPool.Spec.TemplateRef.Name, err)
		}
		return nil, err
	}

	return template, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	r.MaxConcurrentWorkers = concurrentWorkers
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxWarmPool{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}
