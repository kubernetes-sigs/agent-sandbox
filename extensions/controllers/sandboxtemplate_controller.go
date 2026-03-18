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

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// SandboxTemplateReconciler reconciles a SandboxTemplate object
type SandboxTemplateReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Tracer   asmetrics.Instrumenter
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *SandboxTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	if err := r.Get(ctx, req.NamespacedName, template); err != nil {
		if k8errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get sandbox template %q: %w", req.NamespacedName, err)
	}

	ctx, end := r.Tracer.StartSpan(ctx, template, "ReconcileSandboxTemplate", nil)
	defer end()

	if !template.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 2. Determine Scope and Desired State
	npName := template.Name + "-network-policy"
	npNamespace := template.Namespace

	management := template.Spec.NetworkPolicyManagement
	if management == "" {
		management = sandboxv1alpha1.NetworkPolicyManagementManaged
	}

	// 3. Handle "Unmanaged" Opt-Out
	if management == sandboxv1alpha1.NetworkPolicyManagementUnmanaged {
		existingNP := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: npName, Namespace: npNamespace},
		}
		if err := r.Delete(ctx, existingNP); err != nil && !k8errors.IsNotFound(err) {
			logger.Error(err, "Failed to clean up unmanaged NetworkPolicy")
			return ctrl.Result{}, err
		} else if err == nil {
			logger.Info("Deleted unmanaged NetworkPolicy", "name", existingNP.Name)
		}
		return ctrl.Result{}, nil
	}

	// 4. Construct Desired NetworkPolicy Spec
	var desiredSpec networkingv1.NetworkPolicySpec
	if template.Spec.NetworkPolicy == nil {
		desiredSpec = buildDefaultNetworkPolicySpec(template.Name)
	} else {
		desiredSpec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					sandboxTemplateLabel: sandboxcontrollers.NameHash(template.Name),
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

// buildDefaultNetworkPolicySpec generates the "Secure by Default" network policy.
func buildDefaultNetworkPolicySpec(templateName string) networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{
				sandboxTemplateLabel: sandboxcontrollers.NameHash(templateName),
			},
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		// 1. INGRESS: Allow traffic only from the Sandbox Router
		Ingress: []networkingv1.NetworkPolicyIngressRule{
			{
				From: []networkingv1.NetworkPolicyPeer{
					{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "sandbox-router",
							},
						},
					},
				},
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
								"fc00::/7", // Block IPv6 Unique Local Addresses (Internal)
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
		For(&extensionsv1alpha1.SandboxTemplate{}).
		Owns(&networkingv1.NetworkPolicy{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}
