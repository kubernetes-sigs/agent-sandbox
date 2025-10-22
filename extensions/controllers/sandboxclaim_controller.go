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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	var err error
	var sandbox *v1alpha1.Sandbox
	var template *extensionsv1alpha1.SandboxTemplate

	// Try getting template
	if template, err = r.getTemplate(ctx, claim); err == nil || k8errors.IsNotFound(err) {
		// Try getting sandbox even if template is not found
		// It is possible that the template was deleted after the sandbox was created
		sandbox, err = r.getOrCreateSandbox(ctx, claim, template)
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
	sandbox.Spec.PodTemplate.Spec = template.Spec.PodTemplate.Spec
	sandbox.Spec.PodTemplate.ObjectMeta.Labels = template.Spec.PodTemplate.Labels
	sandbox.Spec.PodTemplate.ObjectMeta.Annotations = template.Spec.PodTemplate.Annotations
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

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxClaim{}).
		Owns(&sandboxv1alpha1.Sandbox{}).
		Complete(r)
}
