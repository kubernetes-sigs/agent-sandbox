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
	"sort"
	"time"

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

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	sandboxTemplateRefHash = "agents.x-k8s.io/sandbox-template-ref-hash"
	warmPoolSandboxLabel   = "agents.x-k8s.io/warm-pool-sandbox"

	// sandboxPodTemplateHash tracks the specific version/content of the template.
	sandboxPodTemplateHash = "agents.x-k8s.io/sandbox-pod-template-hash"

	// templateRefField is the field used for indexing SandboxWarmPools by their template reference name.
	templateRefField = ".spec.templateRef.Name"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object
type SandboxWarmPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/finalizers,verbs=get;update;patch
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

	// Fetch template and compute hash once to avoid repeated expensive operations
	template, currentPodTemplateHash, tmplErr := r.fetchTemplateAndHash(ctx, warmPool)

	// Delete stale pods, filter pods by ownership and adopt orphans
	activeSandboxes, allErrors := r.filterActiveSandboxes(ctx, warmPool, sandboxList.Items, template, currentPodTemplateHash, tmplErr)

	const warmPoolReadinessGracePeriod = 5 * time.Minute

	now := time.Now()
	var healthySandboxes []sandboxv1alpha1.Sandbox
	for _, sb := range activeSandboxes {
		if !isSandboxReady(&sb) && !sb.CreationTimestamp.IsZero() && now.Sub(sb.CreationTimestamp.Time) > warmPoolReadinessGracePeriod {
			log.Info("Deleting stuck warm pool sandbox",
				"sandbox", sb.Name,
				"age", now.Sub(sb.CreationTimestamp.Time).Round(time.Second))
			if err := r.Delete(ctx, &sb); err != nil {
				log.Error(err, "Failed to delete stuck sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
			}
			continue
		}
		healthySandboxes = append(healthySandboxes, sb)
	}
	activeSandboxes = healthySandboxes

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(activeSandboxes))

	log.Info("Pool status",
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

	// Create new sandboxes if we need more
	if currentReplicas < desiredReplicas {
		if tmplErr != nil {
			return errors.Join(allErrors, tmplErr)
		}

		sandboxesToCreate := desiredReplicas - currentReplicas
		log.Info("Creating new pool sandboxes", "count", sandboxesToCreate)

		for i := int32(0); i < sandboxesToCreate; i++ {
			if err := r.createPoolSandbox(ctx, warmPool, poolNameHash, template, currentPodTemplateHash); err != nil {
				log.Error(err, "Failed to create pool sandbox")
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	// Delete excess sandboxes if we have too many
	if currentReplicas > desiredReplicas {
		sandboxesToDelete := currentReplicas - desiredReplicas
		log.Info("Deleting excess sandboxes", "count", sandboxesToDelete)

		// Prioritize deleting unready sandboxes before ready ones,
		// then newest first within each group.
		sort.Slice(activeSandboxes, func(i, j int) bool {
			iReady := isSandboxReady(&activeSandboxes[i])
			jReady := isSandboxReady(&activeSandboxes[j])
			if iReady != jReady {
				return !iReady // unready first
			}
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

// filterActiveSandboxes filters the list of sandboxes, deleting stale ones and adopting orphans
func (r *SandboxWarmPoolReconciler) filterActiveSandboxes(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, sandboxes []sandboxv1alpha1.Sandbox, template *extensionsv1alpha1.SandboxTemplate, currentPodTemplateHash string, tmplErr error) ([]sandboxv1alpha1.Sandbox, error) {
	log := log.FromContext(ctx)
	var activeSandboxes []sandboxv1alpha1.Sandbox
	var allErrors error

	vettedHashes := make(map[string]bool)

	updateStrategy := warmPool.Spec.UpdateStrategy.Type
	if updateStrategy == "" {
		updateStrategy = extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType
	}

	for _, sb := range sandboxes {
		if !sb.DeletionTimestamp.IsZero() {
			continue
		}

		controllerRef := metav1.GetControllerOf(&sb)
		isOrphan := controllerRef == nil
		isControlledByPool := controllerRef != nil && controllerRef.UID == warmPool.UID

		if !isOrphan && !isControlledByPool {
			log.Info("Ignoring sandbox with different controller", "sandbox", sb.Name, "controller", controllerRef.Name)
			continue
		}

		if updateStrategy == extensionsv1alpha1.RecreateSandboxWarmPoolUpdateStrategyType && tmplErr == nil && r.isSandboxStale(&sb, template, currentPodTemplateHash, vettedHashes) {
			log.Info("Deleting stale sandbox from pool", "sandbox", sb.Name)
			if err := r.Delete(ctx, &sb); err != nil {
				log.Error(err, "Failed to delete stale sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
			}
			continue
		}

		if isOrphan {
			log.Info("Adopting orphaned sandbox", "sandbox", sb.Name)
			if err := r.adoptSandbox(ctx, warmPool, &sb); err != nil {
				log.Error(err, "Failed to adopt sandbox", "sandbox", sb.Name)
				allErrors = errors.Join(allErrors, err)
				continue
			}
		}

		activeSandboxes = append(activeSandboxes, sb)
	}
	return activeSandboxes, allErrors
}

// fetchTemplateAndHash fetches the sandbox template and computes its hash.
func (r *SandboxWarmPoolReconciler) fetchTemplateAndHash(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) (*extensionsv1alpha1.SandboxTemplate, string, error) {
	log := log.FromContext(ctx)
	template, tmplErr := r.getTemplate(ctx, warmPool)
	var currentPodTemplateHash string
	if tmplErr == nil {
		currentPodTemplateHash = r.computePodTemplateHash(template)
	} else {
		log.Error(tmplErr, "Failed to get sandbox template", "templateRef", warmPool.Spec.TemplateRef.Name)
	}
	return template, currentPodTemplateHash, tmplErr
}

// createPoolSandbox creates a full Sandbox CR (with pod template, service, etc.) for the warm pool
func (r *SandboxWarmPoolReconciler) createPoolSandbox(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, poolNameHash string, template *extensionsv1alpha1.SandboxTemplate, currentPodTemplateHash string) error {
	log := log.FromContext(ctx)

	// Build labels for the Sandbox CR
	sandboxLabels := map[string]string{
		warmPoolSandboxLabel:   poolNameHash,
		sandboxTemplateRefHash: sandboxcontrollers.NameHash(warmPool.Spec.TemplateRef.Name),
		sandboxPodTemplateHash: currentPodTemplateHash,
	}

	// Copy template pod labels into sandbox pod template
	podLabels := make(map[string]string)
	for k, v := range template.Spec.PodTemplate.ObjectMeta.Labels {
		podLabels[k] = v
	}
	// Propagate pool and template labels to pod template for consistency and targeting
	podLabels[warmPoolSandboxLabel] = poolNameHash
	podLabels[sandboxTemplateRefHash] = sandboxcontrollers.NameHash(warmPool.Spec.TemplateRef.Name)
	podLabels[sandboxPodTemplateHash] = currentPodTemplateHash

	podAnnotations := make(map[string]string)
	for k, v := range template.Spec.PodTemplate.ObjectMeta.Annotations {
		podAnnotations[k] = v
	}

	replicas := int32(1)

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

	// Enforce a secure-by-default policy by disabling the automatic mounting
	// of the service account token for warm pool sandboxes.
	if sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken == nil {
		automount := false
		sandbox.Spec.PodTemplate.Spec.AutomountServiceAccountToken = &automount
	}

	// Enforce DNS secure-by-default for warm pool sandboxes.
	management := template.Spec.NetworkPolicyManagement
	isManaged := management == "" || management == extensionsv1alpha1.NetworkPolicyManagementManaged
	isSecureByDefault := isManaged && template.Spec.NetworkPolicy == nil
	if isSecureByDefault && sandbox.Spec.PodTemplate.Spec.DNSPolicy == "" {
		sandbox.Spec.PodTemplate.Spec.DNSPolicy = corev1.DNSNone
		sandbox.Spec.PodTemplate.Spec.DNSConfig = &corev1.PodDNSConfig{
			Nameservers: []string{"8.8.8.8", "1.1.1.1"},
		}
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

// computePodTemplateHash computes a hash of the sandbox template's PodTemplate.
func (r *SandboxWarmPoolReconciler) computePodTemplateHash(template *extensionsv1alpha1.SandboxTemplate) string {
	data := struct {
		Name        string                      `json:"name"`
		PodTemplate sandboxv1alpha1.PodTemplate `json:"podTemplate"`
	}{
		Name:        template.Name,
		PodTemplate: template.Spec.PodTemplate,
	}
	specJSON, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	return sandboxcontrollers.NameHash(string(specJSON))
}

// isSandboxStale checks if the sandbox version matches the current template.
// It uses a cache (vettedHashes) to avoid repeated expensive DeepEqual calls
// for sandboxes with the same hash.
func (r *SandboxWarmPoolReconciler) isSandboxStale(
	sandbox *sandboxv1alpha1.Sandbox,
	template *extensionsv1alpha1.SandboxTemplate,
	currentTemplateHash string,
	vettedHashes map[string]bool,
) bool {
	sandboxHash := sandbox.Labels[sandboxPodTemplateHash]

	// If hashes match, it's fresh.
	if sandboxHash == currentTemplateHash {
		return false
	}

	// Check if we've already evaluated this specific old version.
	if isStale, found := vettedHashes[sandboxHash]; found {
		return isStale
	}

	// Perform Semantic DeepEqual on the Pod Spec only.
	// We ignore metadata/labels here to avoid false positives from runtime labels.
	isStale := !equality.Semantic.DeepEqual(&template.Spec.PodTemplate.Spec, &sandbox.Spec.PodTemplate.Spec)

	// Save the result for the next sandbox with this same hash.
	vettedHashes[sandboxHash] = isStale

	return isStale
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &extensionsv1alpha1.SandboxWarmPool{}, templateRefField, func(rawObj client.Object) []string {
		wp := rawObj.(*extensionsv1alpha1.SandboxWarmPool)
		if wp.Spec.TemplateRef.Name == "" {
			return nil
		}
		return []string{wp.Spec.TemplateRef.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxWarmPool{}).
		Owns(&sandboxv1alpha1.Sandbox{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Watches(
			&extensionsv1alpha1.SandboxTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.findWarmPoolsForTemplate),
		).
		Complete(r)
}

// findWarmPoolsForTemplate returns a list of reconcile.Requests for all SandboxWarmPools that reference the template.
func (r *SandboxWarmPoolReconciler) findWarmPoolsForTemplate(ctx context.Context, obj client.Object) []reconcile.Request {
	template, ok := obj.(*extensionsv1alpha1.SandboxTemplate)
	if !ok {
		return nil
	}

	warmPools := &extensionsv1alpha1.SandboxWarmPoolList{}
	if err := r.List(ctx, warmPools, client.InNamespace(template.Namespace), client.MatchingFields{templateRefField: template.Name}); err != nil {
		return nil
	}

	var requests []reconcile.Request
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
