// Copyright 2026 The Kubernetes Authors.
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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// networkPolicyProtectionFinalizer keeps a SandboxTemplate, and with it the
// managed NetworkPolicy it owns, from being removed while Sandboxes or Pods
// created from the template still carry its sandbox-template-ref-hash label.
// Without it, deleting the template garbage-collects the shared policy and the
// still-running sandboxes silently lose their network isolation.
const networkPolicyProtectionFinalizer = "extensions.agents.x-k8s.io/network-policy-protection"

// SandboxTemplateReconciler reconciles a SandboxTemplate object.
type SandboxTemplateReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Tracer   asmetrics.Instrumenter
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates/finalizers,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch;update
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update

func (r *SandboxTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the SandboxTemplate
	template := &extensionsv1beta1.SandboxTemplate{}
	if err := r.Get(ctx, req.NamespacedName, template); err != nil {
		if k8errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get sandbox template %q: %w", req.NamespacedName, err)
	}

	ctx, end := r.Tracer.StartSpan(ctx, template, "ReconcileSandboxTemplate", nil)
	defer end()

	if err := r.ensureTemplateRefHashLabel(ctx, template); err != nil {
		return ctrl.Result{}, err
	}

	if !template.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.releaseNetworkPolicyProtection(ctx, template)
	}

	// 2. Determine Scope and Desired State
	npName := template.Name + "-network-policy"
	npNamespace := template.Namespace

	management := template.Spec.NetworkPolicyManagement
	if management == "" {
		management = extensionsv1beta1.NetworkPolicyManagementManaged
	}

	// 3. Handle "Unmanaged" Opt-Out
	if management == extensionsv1beta1.NetworkPolicyManagementUnmanaged {
		existingNP := &networkingv1.NetworkPolicy{}
		err := r.Get(ctx, types.NamespacedName{Name: npName, Namespace: npNamespace}, existingNP)
		if err == nil {
			if !metav1.IsControlledBy(existingNP, template) {
				logger.Info("Skipping deletion of NetworkPolicy not owned by template", "name", npName)
				return ctrl.Result{}, nil
			}
			if err := r.Delete(ctx, existingNP); err != nil {
				logger.Error(err, "Failed to clean up unmanaged NetworkPolicy")
				return ctrl.Result{}, err
			}
			logger.Info("Deleted unmanaged NetworkPolicy", "name", existingNP.Name)
		} else if !k8errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get NetworkPolicy: %w", err)
		}
		// There is no managed policy left to protect.
		return ctrl.Result{}, r.removeNetworkPolicyProtection(ctx, template)
	}

	// Protect the managed policy before it exists, so that it can never be
	// garbage-collected ahead of the sandboxes it isolates.
	if err := r.ensureNetworkPolicyProtection(ctx, template); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Construct Desired NetworkPolicy Spec
	var desiredSpec networkingv1.NetworkPolicySpec
	if template.Spec.NetworkPolicy == nil {
		desiredSpec = buildDefaultNetworkPolicySpec(template.Name)
	} else {
		desiredSpec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					sandboxTemplateRefHash: SandboxTemplateRefHash(template.Name),
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: template.Spec.NetworkPolicy.Ingress,
			Egress:  template.Spec.NetworkPolicy.Egress,
		}
	}

	// 5. Reconcile Existing vs Desired
	existingNP := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: npName, Namespace: npNamespace}, existingNP)

	if err == nil {
		if !metav1.IsControlledBy(existingNP, template) {
			return ctrl.Result{}, fmt.Errorf("refusing to update NetworkPolicy %q as it is not controlled by SandboxTemplate %q", npName, template.Name)
		}
		// Policy exists: Semantic DeepEqual check for drift
		if equality.Semantic.DeepEqual(existingNP.Spec, desiredSpec) {
			return ctrl.Result{}, nil // Perfect match, O(1) efficiency.
		}

		existingNP.Spec = desiredSpec
		if err := r.Update(ctx, existingNP); err != nil {
			logger.Error(err, "Failed to update NetworkPolicy", "name", npName)
			return ctrl.Result{}, err
		}
		logger.Info("Successfully updated shared NetworkPolicy", "name", npName)
		return ctrl.Result{}, nil
	}

	if !k8errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get NetworkPolicy: %w", err)
	}

	// 6. Create New Policy
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: npName, Namespace: npNamespace},
		Spec:       desiredSpec,
	}

	if err := controllerutil.SetControllerReference(template, np, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, np); err != nil {
		logger.Error(err, "Failed to create NetworkPolicy", "name", npName)
		return ctrl.Result{}, err
	}

	logger.Info("Successfully created shared NetworkPolicy", "name", npName)
	return ctrl.Result{}, nil
}

func (r *SandboxTemplateReconciler) ensureTemplateRefHashLabel(ctx context.Context, template *extensionsv1beta1.SandboxTemplate) error {
	if !template.DeletionTimestamp.IsZero() {
		return nil
	}

	expectedHash := SandboxTemplateRefHash(template.Name)
	if template.Labels[sandboxTemplateRefHash] == expectedHash {
		return nil
	}

	original := template.DeepCopy()
	if template.Labels == nil {
		template.Labels = make(map[string]string)
	}
	template.Labels[sandboxTemplateRefHash] = expectedHash

	if err := r.Patch(ctx, template, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("failed to label sandbox template %q with template ref hash: %w", client.ObjectKeyFromObject(template), err)
	}
	return nil
}

// ensureNetworkPolicyProtection adds networkPolicyProtectionFinalizer to a
// template whose NetworkPolicy is managed.
func (r *SandboxTemplateReconciler) ensureNetworkPolicyProtection(ctx context.Context, template *extensionsv1beta1.SandboxTemplate) error {
	if controllerutil.ContainsFinalizer(template, networkPolicyProtectionFinalizer) {
		return nil
	}
	original := template.DeepCopy()
	controllerutil.AddFinalizer(template, networkPolicyProtectionFinalizer)
	if err := r.Patch(ctx, template, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("failed to add finalizer to sandbox template %q: %w", client.ObjectKeyFromObject(template), err)
	}
	return nil
}

// removeNetworkPolicyProtection drops networkPolicyProtectionFinalizer.
func (r *SandboxTemplateReconciler) removeNetworkPolicyProtection(ctx context.Context, template *extensionsv1beta1.SandboxTemplate) error {
	if !controllerutil.ContainsFinalizer(template, networkPolicyProtectionFinalizer) {
		return nil
	}
	original := template.DeepCopy()
	controllerutil.RemoveFinalizer(template, networkPolicyProtectionFinalizer)
	if err := r.Patch(ctx, template, client.MergeFrom(original)); err != nil {
		if k8errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to remove finalizer from sandbox template %q: %w", client.ObjectKeyFromObject(template), err)
	}
	return nil
}

// releaseNetworkPolicyProtection lets a deleted template go once nothing in
// its namespace still relies on the template's NetworkPolicy. Until then the
// finalizer stays, the owned policy is not garbage-collected, and the
// Sandbox/Pod watches below requeue the template when the last one is gone.
func (r *SandboxTemplateReconciler) releaseNetworkPolicyProtection(ctx context.Context, template *extensionsv1beta1.SandboxTemplate) error {
	if !controllerutil.ContainsFinalizer(template, networkPolicyProtectionFinalizer) {
		return nil
	}

	selector := client.MatchingLabels{sandboxTemplateRefHash: SandboxTemplateRefHash(template.Name)}
	inNamespace := client.InNamespace(template.Namespace)

	sandboxes := &sandboxv1beta1.SandboxList{}
	if err := r.List(ctx, sandboxes, inNamespace, selector, client.Limit(1)); err != nil {
		return fmt.Errorf("failed to list sandboxes for sandbox template %q: %w", client.ObjectKeyFromObject(template), err)
	}
	if len(sandboxes.Items) > 0 {
		log.FromContext(ctx).Info("Keeping NetworkPolicy of deleted SandboxTemplate while sandboxes still use it",
			"template", template.Name, "sandbox", sandboxes.Items[0].Name)
		return nil
	}

	// A Pod outlives its Sandbox while it terminates and can still send
	// traffic during its grace period.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, inNamespace, selector, client.Limit(1)); err != nil {
		return fmt.Errorf("failed to list pods for sandbox template %q: %w", client.ObjectKeyFromObject(template), err)
	}
	if len(pods.Items) > 0 {
		log.FromContext(ctx).Info("Keeping NetworkPolicy of deleted SandboxTemplate while pods still use it",
			"template", template.Name, "pod", pods.Items[0].Name)
		return nil
	}

	return r.removeNetworkPolicyProtection(ctx, template)
}

// mapTemplateUserToDeletingTemplates maps a Sandbox or Pod carrying a
// sandbox-template-ref-hash label to the deleting, still-protected templates
// in its namespace with that hash.
func (r *SandboxTemplateReconciler) mapTemplateUserToDeletingTemplates(ctx context.Context, obj client.Object) []reconcile.Request {
	hash := obj.GetLabels()[sandboxTemplateRefHash]
	if hash == "" {
		return nil
	}
	templates := &extensionsv1beta1.SandboxTemplateList{}
	if err := r.List(ctx, templates, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list sandbox templates", "namespace", obj.GetNamespace())
		return nil
	}
	var requests []reconcile.Request
	for i := range templates.Items {
		t := &templates.Items[i]
		if t.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(t, networkPolicyProtectionFinalizer) {
			continue
		}
		if SandboxTemplateRefHash(t.Name) != hash {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(t)})
	}
	return requests
}

// templateUserReleasedPredicate passes the events after which an object may
// no longer hold a template's NetworkPolicy: deletion, and an update that
// changes its sandbox-template-ref-hash label.
func templateUserReleasedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetLabels()[sandboxTemplateRefHash] != e.ObjectNew.GetLabels()[sandboxTemplateRefHash]
		},
	}
}

// buildDefaultNetworkPolicySpec generates the "Secure by Default" network policy.
func buildDefaultNetworkPolicySpec(templateName string) networkingv1.NetworkPolicySpec {
	peers := []networkingv1.NetworkPolicyPeer{
		{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": "agent-sandbox-system",
				},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "sandbox-router",
				},
			},
		},
	}

	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{
				sandboxTemplateRefHash: SandboxTemplateRefHash(templateName),
			},
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		// 1. INGRESS: Allow traffic only from the Sandbox Router
		Ingress: []networkingv1.NetworkPolicyIngressRule{
			{
				From: peers,
			},
		},
		// 2. EGRESS: Secure Default Configuration
		Egress: []networkingv1.NetworkPolicyEgressRule{
			// Public Internet Access (Strict Isolation)
			// This rule allows all ports to PUBLIC IPs, but explicitly blocks private LAN ranges.
			// NOTE: This intentionally blocks internal cluster DNS (CoreDNS) by default to prevent
			// agents from probing for service discovery and leaking internal service names.
			{
				To: []networkingv1.NetworkPolicyPeer{
					{
						IPBlock: &networkingv1.IPBlock{
							CIDR: "0.0.0.0/0",
							Except: []string{
								"10.0.0.0/8",     // Block Private Class A (Cluster/VPC Network)
								"172.16.0.0/12",  // Block Private Class B
								"192.168.0.0/16", // Block Private Class C
								"169.254.0.0/16", // Block Link-Local (Metadata Server)
							},
						},
					},
					{
						IPBlock: &networkingv1.IPBlock{
							CIDR: "::/0", // IPv6 Catch-all
							Except: []string{
								"fc00::/7",  // Block IPv6 Unique Local Addresses (Internal)
								"fe80::/10", // Block IPv6 Link-Local
							},
						},
					},
				},
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxTemplateReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1beta1.SandboxTemplate{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&sandboxv1beta1.Sandbox{},
			handler.EnqueueRequestsFromMapFunc(r.mapTemplateUserToDeletingTemplates),
			builder.WithPredicates(templateUserReleasedPredicate())).
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapTemplateUserToDeletingTemplates),
			builder.WithPredicates(templateUserReleasedPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}
