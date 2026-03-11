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

	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	// poolLabel is used by the claim controller's legacy pod-adoption path.
	poolLabel              = "agents.x-k8s.io/pool"
	sandboxTemplateRefHash = "agents.x-k8s.io/sandbox-template-ref-hash"
	warmPoolSandboxLabel   = "agents.x-k8s.io/warm-pool-sandbox"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object
type SandboxWarmPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete

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

	// Reconcile the pool (create or delete Sandboxes as needed)
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

// reconcilePool ensures the correct number of pre-allocated sandboxes exist in the pool
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Compute hash of the warm pool name for the pool label
	poolNameHash := sandboxcontrollers.NameHash(warmPool.Name)

	// List all Sandbox CRs with the warm pool label
	sandboxList := &sandboxv1alpha1.SandboxList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		warmPoolSandboxLabel: poolNameHash,
	})

	if err := r.List(ctx, sandboxList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     warmPool.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list sandboxes")
		return err
	}

	// Filter sandboxes by ownership
	var activeSandboxes []sandboxv1alpha1.Sandbox
	var allErrors error

	for _, sb := range sandboxList.Items {
		if !sb.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}

		controllerRef := metav1.GetControllerOf(&sb)

		if controllerRef == nil {
			// Orphaned sandbox - adopt it
			log.Info("Adopting orphaned sandbox", "sandbox", sb.Name)
			if err := r.adoptSandbox(ctx, warmPool, &sb); err != nil {
				log.Error(err, "Failed to adopt sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				continue
			}
			activeSandboxes = append(activeSandboxes, sb)
		} else if controllerRef.UID == warmPool.UID {
			activeSandboxes = append(activeSandboxes, sb)
		} else {
			log.Info("Ignoring sandbox with different controller",
				"sandbox", sb.Name,
				"controller", controllerRef.Name,
				"controllerKind", controllerRef.Kind)
		}
	}

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(activeSandboxes))

	log.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
		"poolName", warmPool.Name,
		"poolNameHash", poolNameHash)

	warmPool.Status.Replicas = currentReplicas

	// Calculate ready replicas by checking Sandbox Ready condition
	readyReplicas := int32(0)
	for _, sb := range activeSandboxes {
		for _, cond := range sb.Status.Conditions {
			if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
				readyReplicas++
				break
			}
		}
	}
	warmPool.Status.ReadyReplicas = readyReplicas

	// Create new sandboxes if we need more
	if currentReplicas < desiredReplicas {
		sandboxesToCreate := desiredReplicas - currentReplicas
		log.Info("Creating new pool sandboxes", "count", sandboxesToCreate)

		for i := int32(0); i < sandboxesToCreate; i++ {
			if err := r.createPoolSandbox(ctx, warmPool, poolNameHash); err != nil {
				log.Error(err, "Failed to create pool sandbox")
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	// Delete excess sandboxes if we have too many
	if currentReplicas > desiredReplicas {
		sandboxesToDelete := currentReplicas - desiredReplicas
		log.Info("Deleting excess sandboxes", "count", sandboxesToDelete)

		sort.Slice(activeSandboxes, func(i, j int) bool {
			return activeSandboxes[i].CreationTimestamp.After(activeSandboxes[j].CreationTimestamp.Time)
		})

		for i := int32(0); i < sandboxesToDelete && i < int32(len(activeSandboxes)); i++ {
			sb := &activeSandboxes[i]
			if err := r.Delete(ctx, sb); err != nil {
				log.Error(err, "Failed to delete sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	return allErrors
}

// adoptSandbox sets this warmpool as the owner of an orphaned sandbox
func (r *SandboxWarmPoolReconciler) adoptSandbox(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, sb *sandboxv1alpha1.Sandbox) error {
	if err := controllerutil.SetControllerReference(warmPool, sb, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, sb)
}

// createPoolSandbox creates a full Sandbox CR (with pod template, service, etc.) for the warm pool
func (r *SandboxWarmPoolReconciler) createPoolSandbox(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, poolNameHash string) error {
	log := log.FromContext(ctx)

	// Try getting template
	var template *extensionsv1alpha1.SandboxTemplate
	var err error
	if template, err = r.getTemplate(ctx, warmPool); err != nil {
		log.Error(err, "Failed to get sandbox template for warm pool", "warmPoolName", warmPool.Name)
		return err
	}

	// Build labels for the Sandbox CR
	sandboxLabels := map[string]string{
		warmPoolSandboxLabel:   poolNameHash,
		sandboxTemplateRefHash: sandboxcontrollers.NameHash(warmPool.Spec.TemplateRef.Name),
	}

	// Copy template pod labels into sandbox pod template
	podLabels := make(map[string]string)
	for k, v := range template.Spec.PodTemplate.ObjectMeta.Labels {
		podLabels[k] = v
	}

	podAnnotations := make(map[string]string)
	for k, v := range template.Spec.PodTemplate.ObjectMeta.Annotations {
		podAnnotations[k] = v
	}

	replicas := int32(1)
	automount := false

	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", warmPool.Name),
			Namespace:    warmPool.Namespace,
			Labels:       sandboxLabels,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: &replicas,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: *template.Spec.PodTemplate.Spec.DeepCopy(),
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
			},
		},
	}

	// Enforce secure-by-default
	if sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken == nil {
		sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken = &automount
	}

	// Set controller reference so the Sandbox is owned by the SandboxWarmPool
	if err := ctrl.SetControllerReference(warmPool, sandbox, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference for Sandbox failed: %w", err)
	}

	if err := r.Create(ctx, sandbox); err != nil {
		log.Error(err, "Failed to create pool sandbox")
		return err
	}

	log.Info("Created new pool sandbox", "sandbox", sandbox.Name, "poolName", warmPool.Name)
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
		if !k8serrors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox template %q: %w", warmPool.Spec.TemplateRef.Name, err)
		}
		return nil, err
	}

	return template, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxWarmPool{}).
		Owns(&sandboxv1alpha1.Sandbox{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}
