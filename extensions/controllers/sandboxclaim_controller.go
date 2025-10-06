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

	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	// These are for our PDB management
	pdbFinalizerName = "sandboxclaim.agents.x-k8s.io/pdb-cleanup"
	pdbName          = "sandbox-highly-available"
	pdbLabelKey      = "sandbox-disruption-policy"
	pdbLabelValue    = "HighlyAvailable"
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
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	claim := &extensionsv1alpha1.SandboxClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if k8errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get sandbox claim %q: %w", req.NamespacedName, err)
	}
	originalClaimStatus := claim.Status.DeepCopy()

	template, err := r.getTemplate(ctx, claim)
	if err != nil && !k8errors.IsNotFound(err) {
		// This is a real error, update status and requeue
		r.computeAndSetStatus(claim, nil, err)
		// We can't update status if the claim is not found, but we also don't need to.
		if !k8errors.IsNotFound(err) {
			if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
				err = errors.Join(err, updateErr)
			}
		}
		return ctrl.Result{}, err
	}
	if k8errors.IsNotFound(err) {
		// Template not found, set status and stop.
		r.computeAndSetStatus(claim, nil, ErrTemplateNotFound)
		if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
			err = errors.Join(err, updateErr)
		}
		return ctrl.Result{}, err
	}

	// Determine if we should manage PDBs for this claim
	managePDB := template != nil && template.Spec.EnableDisruptionControl

	if !claim.DeletionTimestamp.IsZero() {
		// This logic only runs if the claim is being deleted
		if managePDB && controllerutil.ContainsFinalizer(claim, pdbFinalizerName) {
			logger.Info("Reconciling PDB deletion for deleted claim")
			if err := r.reconcilePDBDeletion(ctx, claim); err != nil {
				return ctrl.Result{}, err
			}

			// Cleanup successful, remove the finalizer
			controllerutil.RemoveFinalizer(claim, pdbFinalizerName)
			if err := r.Update(ctx, claim); err != nil {
				logger.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// If not deleting, add the finalizer if it's needed and missing
	if managePDB && !controllerutil.ContainsFinalizer(claim, pdbFinalizerName) {
		logger.Info("Adding PDB finalizer to claim")
		controllerutil.AddFinalizer(claim, pdbFinalizerName)
		if err := r.Update(ctx, claim); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil // Requeue to process again with the finalizer
	}

	// cache the original status from sandboxclaim
	var sandbox *v1alpha1.Sandbox

	// Try getting or creating the sandbox
	// We already fetched the template, so we pass it in.
	sandbox, err = r.getOrCreateSandbox(ctx, claim, template)

	if err == nil { // Only reconcile children if sandbox was found or created
		// Reconcile PDB
		if managePDB {
			if pdbErr := r.reconcilePDB(ctx, claim); pdbErr != nil {
				err = errors.Join(err, pdbErr)
			}
		}
	}

	// Update claim status
	r.computeAndSetStatus(claim, sandbox, err)
	if updateErr := r.updateStatus(ctx, originalClaimStatus, claim); updateErr != nil {
		err = errors.Join(err, updateErr)
	}

	return ctrl.Result{}, err
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
	replicas := int32(1)
	sandbox.Spec.Replicas = &replicas

	sandbox.Spec.PodTemplate.Spec = template.Spec.PodTemplate.Spec
	sandbox.Spec.PodTemplate.ObjectMeta.Labels = template.Spec.PodTemplate.Labels
	sandbox.Spec.PodTemplate.ObjectMeta.Annotations = template.Spec.PodTemplate.Annotations

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
							// This label is injected into the PodTemplate by createSandbox
							// and then propagated to the Pod by the sandbox-controller.
						},
					},
				},
			}

			// The PDB selector will target this common label.
			pdbToCreate.Spec.Selector.MatchLabels = map[string]string{
				pdbLabelKey: pdbLabelValue,
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
		template, err := r.getTemplate(ctx, &otherClaim)
		if err == nil && template != nil && template.Spec.EnableDisruptionControl {
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
