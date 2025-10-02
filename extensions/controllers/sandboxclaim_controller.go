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

	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

// SandboxClaimReconciler reconciles a SandboxClaim object
type SandboxClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch

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

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// cache the original status from sandboxclaim
	originalClaimStatus := claim.Status.DeepCopy()
	var combinedErrors error

	// Try getting template and sandbox, collect errors if any
	template, err := r.getTemplate(ctx, claim)
	// We ignore the not found error and handle it via template being nil
	if k8errors.IsNotFound(err) {
		template = nil
		err = nil
	}
	combinedErrors = errors.Join(combinedErrors, err)
	sandbox, err := r.getSandbox(ctx, claim)
	if k8errors.IsNotFound(err) {
		// We ignore the not found error and handle it via sandbox being nil
		sandbox = nil
		err = nil
	}
	combinedErrors = errors.Join(combinedErrors, err)

	if combinedErrors == nil {
		if template == nil {
			err := fmt.Errorf("sandboxtemplate %q not found", claim.Spec.TemplateRef.Name)
			logger.Error(err, "Missing sandbox template")
			combinedErrors = errors.Join(combinedErrors, err)
		}
		if sandbox != nil {
			logger.Info("sandbox already exists, skipping update", "name", sandbox.Name)
			if !r.isControlledByClaim(sandbox, claim) {
				err := fmt.Errorf("sandbox %q is not controlled by claim %q", sandbox.Name, claim.Name)
				logger.Error(err, "Sandbox controller mismatch")
				combinedErrors = errors.Join(combinedErrors, err)
			}
		} else {
			sandbox, err = r.createSandbox(ctx, claim, template)
			combinedErrors = errors.Join(combinedErrors, err)
		}
	}

	// Update claim status
	r.computeAndSetStatus(claim, sandbox, template, combinedErrors)
	if err := r.updateStatus(ctx, originalClaimStatus, claim); err != nil {
		combinedErrors = errors.Join(combinedErrors, err)
	}

	return ctrl.Result{}, combinedErrors
}

func (r *SandboxClaimReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxClaimStatus, claim *extensionsv1alpha1.SandboxClaim) error {
	log := log.FromContext(ctx)

	if reflect.DeepEqual(oldStatus, &claim.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, claim); err != nil {
		log.Error(err, "Failed to update sandboxclaim status")
		return err
	}

	return nil
}

func (r *SandboxClaimReconciler) computeReadyCondition(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, template *extensionsv1alpha1.SandboxTemplate, err error) metav1.Condition {
	readyCondition := metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionReady),
		ObservedGeneration: claim.Generation,
		Status:             metav1.ConditionFalse,
		Reason:             "MissingSandbox",
		Message:            "Sandbox not found",
	}

	// Reconciler errors take precedence. They are expected to be transient.
	if err != nil {
		readyCondition.Reason = "ReconcilerError"
		readyCondition.Message = "Error seen: " + err.Error()
		return readyCondition
	}

	if sandbox == nil && template == nil {
		readyCondition.Reason = "TemplateNotFound"
		readyCondition.Message = fmt.Sprintf("SandboxTemplate %q not found", claim.Spec.TemplateRef.Name)
		return readyCondition
	}

	if sandbox != nil {
		sandboxReady := false
		for _, condition := range sandbox.Status.Conditions {
			if condition.Type == string(sandboxv1alpha1.SandboxConditionReady) {
				if condition.Status == metav1.ConditionTrue {
					sandboxReady = true
				}
				break
			}
		}
		if sandboxReady {
			readyCondition.Status = metav1.ConditionTrue
			readyCondition.Reason = "SandboxReady"
			readyCondition.Message = "Sandbox is ready"
		} else {
			readyCondition.Reason = "SandboxNotReady"
			readyCondition.Message = "Sandbox is not ready"
		}
	}

	return readyCondition
}

func (r *SandboxClaimReconciler) computeAndSetStatus(claim *extensionsv1alpha1.SandboxClaim, sandbox *v1alpha1.Sandbox, template *extensionsv1alpha1.SandboxTemplate, err error) {
	// compute and set overall Ready condition
	readyCondition := r.computeReadyCondition(claim, sandbox, template, err)
	meta.SetStatusCondition(&claim.Status.Conditions, readyCondition)

	if sandbox != nil {
		claim.Status.SandboxStatus.Name = sandbox.Name
		claim.Status.SandboxStatus.ServiceFQDN = sandbox.Status.ServiceFQDN
		claim.Status.SandboxStatus.Service = sandbox.Status.Service
	}
}

func (r *SandboxClaimReconciler) isControlledByClaim(sandbox *v1alpha1.Sandbox, claim *extensionsv1alpha1.SandboxClaim) bool {
	// Check if the existing sandbox is owned by this claim
	controlledBy := false
	for _, ownerRef := range sandbox.OwnerReferences {
		if ownerRef.UID == claim.UID && ownerRef.Controller != nil && *ownerRef.Controller {
			controlledBy = true
			break
		}
	}
	return controlledBy
}

func (r *SandboxClaimReconciler) createSandbox(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, template *extensionsv1alpha1.SandboxTemplate) (*v1alpha1.Sandbox, error) {
	logger := log.FromContext(ctx)
	if template == nil {
		err := fmt.Errorf("sandboxtemplate not found")
		logger.Error(err, "cannot create sandbox")
		// returning nil error here since computeStatus will surface this in ready condition
		return nil, nil
	}

	logger.Info("creating sandbox from template", "template", template.Name)
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
	}
	sandbox.Spec.PodTemplate.Spec = template.Spec.PodTemplate.Spec
	sandbox.Spec.PodTemplate.ObjectMeta.Labels = template.Spec.PodTemplate.Labels
	sandbox.Spec.PodTemplate.ObjectMeta.Annotations = template.Spec.PodTemplate.Annotations
	if err := controllerutil.SetControllerReference(claim, sandbox, r.Scheme); err != nil {
		err := fmt.Errorf("failed to set controller reference for sandbox: %w", err)
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

func (r *SandboxClaimReconciler) getSandbox(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*v1alpha1.Sandbox, error) {
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
	}
	var err error
	if err = r.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		sandbox = nil
		if !k8errors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox %q: %w", claim.Name, err)
		}
	}

	return sandbox, err
}

func (r *SandboxClaimReconciler) getTemplate(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim) (*extensionsv1alpha1.SandboxTemplate, error) {
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Spec.TemplateRef.Name,
		},
	}
	var err error
	if err = r.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
		template = nil
		if !k8errors.IsNotFound(err) {
			err = fmt.Errorf("failed to get sandbox template %q: %w", claim.Spec.TemplateRef.Name, err)
		}
	}

	return template, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxClaim{}).
		Watches(&sandboxv1alpha1.Sandbox{}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
