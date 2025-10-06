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
	"reflect"
	"sort"

	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	// These are for our PDB management
	pdbFinalizerName = "sandboxclaim.agents.x-k8s.io/pdb-cleanup"
	pdbName          = "sandbox-highly-available"
	pdbLabelKey      = "extensions.agents.x-k8s.io/sandbox-disruption-policy"
	pdbLabelValue    = "true"
)

// ErrTemplateNotFound is a sentinel error indicating a SandboxTemplate was not found.
var ErrTemplateNotFound = errors.New("SandboxTemplate not found")

// SandboxClaimReconciler reconciles a SandboxClaim object
type SandboxClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	claim := &extensionsv1alpha1.SandboxClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if k8errors.IsNotFound(err) {
			// Object not found, probably deleted. Nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get sandbox claim %q: %w", req.NamespacedName, err)
	}

	// Check if the object is being deleted
	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, claim)
	}

	// Object is not being deleted.
	originalClaimStatus := claim.Status.DeepCopy()

	// We create a custom error type to request a requeue from reconcileCreateOrUpdate
	var requeueErr *requeueError

	sandbox, err := r.reconcileCreateOrUpdate(ctx, claim)

	// Update claim status
	r.computeAndSetStatus(claim, sandbox, err)
	if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
		err = errors.Join(err, updateErr)
	}

	// Check if reconcileCreateOrUpdate requested an explicit requeue
	if errors.As(err, &requeueErr) {
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, err
}

// requeueError is a simple error type to signal that we need to requeue.
type requeueError struct {
	message string
}

func (e *requeueError) Error() string {
	return e.message
}

// reconcileDelete handles the cleanup when a SandboxClaim is deleted.
func (r *SandboxClaimReconciler) reconcileDelete(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// We only care if our finalizer is present.
	if controllerutil.ContainsFinalizer(claim, pdbFinalizerName) {
		logger.Info("Reconciling PDB deletion for deleted claim")

		// Run the PDB cleanup logic
		if err := r.reconcilePDBDeletion(ctx, claim); err != nil {
			// If cleanup fails, return error to retry.
			return ctrl.Result{}, err
		}

		// Cleanup successful, remove the finalizer
		logger.Info("PDB cleanup successful, removing finalizer")
		controllerutil.RemoveFinalizer(claim, pdbFinalizerName)
		if err := r.Update(ctx, claim); err != nil {
			logger.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}
	}

	// Object is cleaned up or has no finalizer. Stop reconciliation.
	return ctrl.Result{}, nil
}

// reconcileCreateOrUpdate handles the "normal" reconciliation loop for a SandboxClaim.
// This is called when the object is not being deleted.
func (r *SandboxClaimReconciler) reconcileCreateOrUpdate(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)

	// Template Logic
	template, err := r.getTemplate(ctx, claim)
	if err != nil {
		if k8errors.IsNotFound(err) {
			logger.Info("SandboxTemplate not found", "template", claim.Spec.TemplateRef.Name)
			return nil, ErrTemplateNotFound
		}
		logger.Error(err, "Failed to get SandboxTemplate", "template", claim.Spec.TemplateRef.Name)
		return nil, err
	}

	// PDB Finalizer Logic
	managePDB := template != nil && template.Spec.EnableDisruptionControl
	if managePDB && !controllerutil.ContainsFinalizer(claim, pdbFinalizerName) {
		logger.Info("Adding PDB finalizer to claim")
		controllerutil.AddFinalizer(claim, pdbFinalizerName)
		if err := r.Update(ctx, claim); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return nil, err
		}
		// Signal to the main Reconcile loop that we need to requeue
		return nil, &requeueError{"finalizer added"}
	}

	// Sandbox Logic Retrieval/Creation
	sandbox, err := r.getOrCreateSandbox(ctx, claim, template)
	if err != nil {
		// Error already logged by getOrCreateSandbox
		return nil, err
	}

	// PDB Creation Logic
	if managePDB {
		if pdbErr := r.reconcilePDB(ctx, claim); pdbErr != nil {
			err = errors.Join(err, pdbErr)
		}
	}

	return sandbox, err
}

func (r *SandboxClaimReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxClaimStatus, claim *extensionsv1alpha1.SandboxClaim) error {
	log := log.FromContext(ctx)

	sort.Slice(oldStatus.Conditions, func(i, j int) bool {
		return oldStatus.Conditions[i].Type < oldStatus.Conditions[j].Type
	})
	sort.Slice(claim.Status.Conditions, func(i, j int) bool {
		return claim.Status.Conditions[i].Type < claim.Status.Conditions[j].Type
	})

	if reflect.DeepEqual(oldStatus, &claim.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, claim); err != nil {
		log.Error(err, "Failed to update sandboxclaim status")
		return err
	}

	return nil
}

func (r *SandboxClaimReconciler) computeReadyCondition(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, err error) metav1.Condition {
	readyCondition := metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionReady),
		ObservedGeneration: claim.Generation,
		Status:             metav1.ConditionFalse,
	}

	// Reconciler errors take precedence. They are expected to be transient.
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			readyCondition.Reason = "TemplateNotFound"
			readyCondition.Message = fmt.Sprintf("SandboxTemplate %q not found", claim.Spec.TemplateRef.Name)
			return readyCondition
		}
		readyCondition.Reason = "ReconcilerError"
		readyCondition.Message = "Error seen: " + err.Error()
		return readyCondition
	}

	// Sanbox should be non-nil if err is nil
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == string(sandboxv1alpha1.SandboxConditionReady) {
			if condition.Status == metav1.ConditionTrue {
				readyCondition.Status = metav1.ConditionTrue
				readyCondition.Reason = "SandboxReady"
				readyCondition.Message = "Sandbox is ready"
				return readyCondition
			}
		}
	}

	readyCondition.Reason = "SandboxNotReady"
	readyCondition.Message = "Sandbox is not ready"
	return readyCondition
}

func (r *SandboxClaimReconciler) computeAndSetStatus(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, err error) {
	// compute and set overall Ready condition
	readyCondition := r.computeReadyCondition(claim, sandbox, err)
	meta.SetStatusCondition(&claim.Status.Conditions, readyCondition)

	if sandbox != nil {
		claim.Status.SandboxStatus.Name = sandbox.Name
	}
}

func (r *SandboxClaimReconciler) isControlledByClaim(sandbox *v1alpha1.Sandbox, claim *extensionsv1alpha1.SandboxClaim) bool {
	// Check if the existing sandbox is owned by this claim
	for _, ownerRef := range sandbox.OwnerReferences {
		if ownerRef.UID == claim.UID && ownerRef.Controller != nil && *ownerRef.Controller {
			return true
		}
	}
	return false
}

// tryAdoptPodFromPool attempts to find and adopt a pod from the warm pool
func (r *SandboxClaimReconciler) tryAdoptPodFromPool(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox) (*corev1.Pod, error) {
	log := log.FromContext(ctx)

	// List all pods with the podTemplateHashLabel matching the hash
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		sandboxTemplateRefHash: sandboxcontrollers.NameHash(claim.Spec.TemplateRef.Name),
	})

	if err := r.List(ctx, podList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     claim.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list pods from warm pool")
		return nil, err
	}

	// Filter out pods that are being deleted or already have a different controller
	filteredPods := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		// Skip pods that are being deleted
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		// Skip pods that already have a different controller
		controllerRef := metav1.GetControllerOf(&pod)
		if controllerRef != nil && controllerRef.Kind != "SandboxWarmPool" {
			log.Info("Ignoring pod with different controller, but this shouldn't happen because this pod shouldn't have template ref label",
				"pod", pod.Name,
				"controller", controllerRef.Name,
				"controllerKind", controllerRef.Kind)
			continue
		}

		filteredPods = append(filteredPods, pod)
	}
	podList.Items = filteredPods

	if len(podList.Items) == 0 {
		log.Info("No available pods in warm pool (all pods are being deleted, owned by other controllers, or pool is empty)")
		return nil, nil
	}

	// Sort pods by creation timestamp (oldest first)
	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].CreationTimestamp.Before(&podList.Items[j].CreationTimestamp)
	})

	// Get the first available pod
	pod := &podList.Items[0]
	log.Info("Adopting pod from warm pool", "pod", pod.Name)

	// Remove the pool labels
	delete(pod.Labels, poolLabel)
	delete(pod.Labels, sandboxTemplateRefHash)

	// Remove existing owner references (from SandboxWarmPool)
	pod.OwnerReferences = nil

	// Update the pod
	if err := r.Update(ctx, pod); err != nil {
		log.Error(err, "Failed to update adopted pod")
		return nil, err
	}

	log.Info("Successfully adopted pod from warm pool", "pod", pod.Name, "sandbox", sandbox.Name)
	return pod, nil
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
	sandbox.Spec.PodTemplate = template.Spec.PodTemplate

	replicas := int32(1)
	sandbox.Spec.Replicas = &replicas

	if template.Spec.EnableDisruptionControl {
		// 1. Inject the PDB label for the shared PDB to select
		if sandbox.Spec.PodTemplate.ObjectMeta.Labels == nil {
			sandbox.Spec.PodTemplate.ObjectMeta.Labels = make(map[string]string)
		}
		sandbox.Spec.PodTemplate.ObjectMeta.Labels[pdbLabelKey] = pdbLabelValue

		// 2. Inject the safe-to-evict annotation
		if sandbox.Spec.PodTemplate.ObjectMeta.Annotations == nil {
			sandbox.Spec.PodTemplate.ObjectMeta.Annotations = make(map[string]string)
		}
		sandbox.Spec.PodTemplate.ObjectMeta.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"] = "false"
	}

	if err := controllerutil.SetControllerReference(claim, sandbox, r.Scheme); err != nil {
		err = fmt.Errorf("failed to set controller reference for sandbox: %w", err)
		logger.Error(err, "Error creating sandbox for claim: %q", claim.Name)
		return nil, err
	}

	// Before creating the sandbox, try to adopt a pod from the warm pool
	adoptedPod, adoptErr := r.tryAdoptPodFromPool(ctx, claim, sandbox)
	if adoptErr != nil {
		logger.Error(adoptErr, "Failed to adopt pod from warm pool")
		return nil, adoptErr
	}

	if adoptedPod != nil {
		logger.Info("Adopted pod from warm pool for sandbox", "pod", adoptedPod.Name, "sandbox", sandbox.Name)
		if sandbox.Annotations == nil {
			sandbox.Annotations = make(map[string]string)
		}
		sandbox.Annotations[sandboxcontrollers.SanboxPodNameAnnotation] = adoptedPod.Name
	}

	if err := r.Create(ctx, sandbox); err != nil {
		err = fmt.Errorf("sandbox create error: %w", err)
		logger.Error(err, "Error creating sandbox for claim: %q", claim.Name)
		return nil, err
	}

	logger.Info("Created sandbox for claim", "claim", claim.Name)
	return sandbox, nil
}

func (r *SandboxClaimReconciler) getOrCreateSandbox(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, template *extensionsv1alpha1.SandboxTemplate) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		sandbox = nil
		if !k8errors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox %q: %w", claim.Name, err)
			return nil, err
		}
	}

	if sandbox != nil {
		logger.Info("sandbox already exists, skipping update", "name", sandbox.Name)
		if !r.isControlledByClaim(sandbox, claim) {
			err := fmt.Errorf("sandbox %q is not controlled by claim %q. Please use a different claim name or delete the sandbox manually", sandbox.Name, claim.Name)
			logger.Error(err, "Sandbox controller mismatch")
			return nil, err
		}
		return sandbox, nil
	}

	return r.createSandbox(ctx, claim, template)
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

// reconcilePDB ensures the shared PDB exists in the namespace.
func (r *SandboxClaimReconciler) reconcilePDB(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) error {
	logger := log.FromContext(ctx)
	pdb := &policyv1.PodDisruptionBudget{}
	pdbKey := types.NamespacedName{Name: pdbName, Namespace: claim.Namespace}

	err := r.Client.Get(ctx, pdbKey, pdb)
	if err != nil {
		if k8errors.IsNotFound(err) {
			// PDB does not exist, let's create it.
			logger.Info("Creating shared PodDisruptionBudget", "PDB.Name", pdbName, "PDB.Namespace", claim.Namespace)

			// This PDB will select ALL pods created by the core sandbox-controller
			// that have the disruption policy enabled.
			pdbToCreate := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pdbName,
					Namespace: claim.Namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							pdbLabelKey: pdbLabelValue,
						},
					},
				},
			}

			// We DO NOT set an owner ref, as this PDB is shared and its
			// lifecycle is managed by our finalizer.
			if err := r.Client.Create(ctx, pdbToCreate); err != nil {
				logger.Error(err, "Failed to create PDB")
				return err
			}
			return nil
		}
		// Some other error occurred when trying to Get the PDB
		return err
	}
	// PDB already exists, do nothing.
	return nil
}

// reconcilePDBDeletion handles cleanup of the shared PDB.
func (r *SandboxClaimReconciler) reconcilePDBDeletion(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) error {
	logger := log.FromContext(ctx)

	// List all other SandboxClaims in the same namespace
	claimList := &extensionsv1alpha1.SandboxClaimList{}
	if err := r.Client.List(ctx, claimList, client.InNamespace(claim.Namespace)); err != nil {
		logger.Error(err, "Failed to list SandboxClaims for PDB cleanup")
		return err
	}

	// Check if any *other* claims that require PDBs still exist.
	otherClaimsNeedPDB := false
	for _, otherClaim := range claimList.Items {
		if otherClaim.UID == claim.UID || !otherClaim.DeletionTimestamp.IsZero() {
			continue // Skip self or claims already being deleted
		}

		// We must check if the other claim also has PDB enabled
		if controllerutil.ContainsFinalizer(&otherClaim, pdbFinalizerName) {
			otherClaimsNeedPDB = true
			break
		}
	}

	if !otherClaimsNeedPDB {
		// This is the last claim that needs a PDB. Delete the PDB.
		logger.Info("Last SandboxClaim with disruption control deleted. Deleting PDB.", "PDB.Name", pdbName, "PDB.Namespace", claim.Namespace)
		pdbToDelete := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdbName,
				Namespace: claim.Namespace,
			},
		}

		if err := r.Client.Delete(ctx, pdbToDelete); err != nil {
			// Ignore "not found" errors, as it might already be gone.
			if !k8errors.IsNotFound(err) {
				logger.Error(err, "Failed to delete PDB")
				return err
			}
		}
	} else {
		logger.Info("Other SandboxClaims still require the PDB, not deleting.")
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxClaim{}).
		Owns(&sandboxv1alpha1.Sandbox{}).
		Complete(r)
}
