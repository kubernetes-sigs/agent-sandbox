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
	"hash/fnv"
	"reflect"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	sandboxLabel = "agents.x-k8s.io/sandbox-name-hash"
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
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

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

		if err == nil { // Only reconcile NetworkPolicy if sandbox creation was successful
			if npErr := r.reconcileNetworkPolicy(ctx, claim, template); npErr != nil {
				// Join the error so it gets reported in the status
				err = errors.Join(err, npErr)
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
	sandbox.Spec.PodTemplate.Spec = template.Spec.PodTemplate.Spec
	sandbox.Spec.PodTemplate.ObjectMeta.Labels = template.Spec.PodTemplate.Labels
	sandbox.Spec.PodTemplate.ObjectMeta.Annotations = template.Spec.PodTemplate.Annotations
	if err := controllerutil.SetControllerReference(claim, sandbox, r.Scheme); err != nil {
		err = fmt.Errorf("failed to set controller reference for sandbox: %w", err)
		logger.Error(err, "Error setting controller reference for sandbox", "claim", claim.Name)
		return nil, err
	}

	if err := r.Create(ctx, sandbox); err != nil {
		err = fmt.Errorf("sandbox create error: %w", err)
		logger.Error(err, "Error creating sandbox for claim", "claim", claim.Name)
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

// reconcileNetworkPolicy ensures a NetworkPolicy exists for the claimed Sandbox,
// translating the rules from the SandboxTemplate.
func (r *SandboxClaimReconciler) reconcileNetworkPolicy(ctx context.Context, claim *extensionsv1alpha1.SandboxClaim, template *extensionsv1alpha1.SandboxTemplate) error {
	logger := log.FromContext(ctx)

	// If the template doesn't exist or network policy is disabled, we do nothing.
	if template == nil || template.Spec.NetworkPolicy == nil || !template.Spec.NetworkPolicy.Enabled {
		logger.V(1).Info("Network policy not enabled for this template, skipping.")
		// TODO: Add logic here to delete an existing NetworkPolicy if the template is changed to disabled=false.
		return nil
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name + "-network-policy", // A unique name for the policy
			Namespace: claim.Namespace,
		},
	}

	// CreateOrUpdate will create the policy if it doesn't exist, or update it
	// if the template has changed.
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		nameHash := NameHash(claim.Name)
		podSelector := metav1.LabelSelector{
			MatchLabels: map[string]string{
				sandboxLabel: nameHash,
			},
		}

		var ingressRules []networkingv1.NetworkPolicyIngressRule
		templateNP := template.Spec.NetworkPolicy

		hasIngressSources := templateNP.IngressControllerSelectors != nil ||
			len(templateNP.IngressFromIPBlocks) > 0 ||
			len(templateNP.AdditionalIngressRules) > 0

		if hasIngressSources {
			var ingressPeers []networkingv1.NetworkPolicyPeer
			if sel := templateNP.IngressControllerSelectors; sel != nil {
				peer := networkingv1.NetworkPolicyPeer{}
				if sel.NamespaceSelector != nil {
					peer.NamespaceSelector = &metav1.LabelSelector{MatchLabels: sel.NamespaceSelector}
				}
				if sel.PodSelector != nil {
					peer.PodSelector = &metav1.LabelSelector{MatchLabels: sel.PodSelector}
				}
				ingressPeers = append(ingressPeers, peer)
			}
			for _, block := range templateNP.IngressFromIPBlocks {
				ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{
					IPBlock: &networkingv1.IPBlock{CIDR: block.CIDR},
				})
			}
			for _, rule := range templateNP.AdditionalIngressRules {
				peer := networkingv1.NetworkPolicyPeer{}
				if rule.InNamespaceSelector != nil {
					peer.NamespaceSelector = &metav1.LabelSelector{MatchLabels: rule.InNamespaceSelector}
				}
				if rule.FromPodSelector != nil {
					peer.PodSelector = &metav1.LabelSelector{MatchLabels: rule.FromPodSelector}
				}
				ingressPeers = append(ingressPeers, peer)
			}

			var ingressPorts []networkingv1.NetworkPolicyPort
			if len(template.Spec.PodTemplate.Spec.Containers) > 0 {
				for _, container := range template.Spec.PodTemplate.Spec.Containers {
					for _, port := range container.Ports {
						p := port.ContainerPort
						proto := corev1.ProtocolTCP
						if port.Protocol != "" {
							proto = port.Protocol
						}
						ingressPorts = append(ingressPorts, networkingv1.NetworkPolicyPort{
							Protocol: &proto,
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: p},
						})
					}
				}
			}
			ingressRules = append(ingressRules, networkingv1.NetworkPolicyIngressRule{
				From:  ingressPeers,
				Ports: ingressPorts,
			})
		}

		var egressRules []networkingv1.NetworkPolicyEgressRule
		dnsPort53 := intstr.FromInt(53)
		protoUDP := corev1.ProtocolUDP
		protoTCP := corev1.ProtocolTCP
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &protoUDP, Port: &dnsPort53},
				{Protocol: &protoTCP, Port: &dnsPort53},
			},
		})

		for _, rule := range templateNP.AdditionalEgressRules {
			var egressPeers []networkingv1.NetworkPolicyPeer
			var egressPorts []networkingv1.NetworkPolicyPort
			if rule.ToIPBlock != nil {
				egressPeers = append(egressPeers, networkingv1.NetworkPolicyPeer{
					IPBlock: &networkingv1.IPBlock{CIDR: rule.ToIPBlock.CIDR, Except: rule.ToIPBlock.Except},
				})
			} else if rule.ToPodSelector != nil {
				peer := networkingv1.NetworkPolicyPeer{}
				if rule.InNamespaceSelector != nil {
					peer.NamespaceSelector = &metav1.LabelSelector{MatchLabels: rule.InNamespaceSelector}
				}
				peer.PodSelector = &metav1.LabelSelector{MatchLabels: rule.ToPodSelector}
				egressPeers = append(egressPeers, peer)
			}
			for _, p := range rule.Ports {
				portNum := intstr.FromInt(int(*p.Port))
				proto := corev1.ProtocolTCP
				if p.Protocol != nil {
					proto = *p.Protocol
				}
				egressPorts = append(egressPorts, networkingv1.NetworkPolicyPort{
					Protocol: &proto,
					Port:     &portNum,
				})
			}
			egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
				To:    egressPeers,
				Ports: egressPorts,
			})
		}

		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: podSelector,
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingressRules,
			Egress:  egressRules,
		}

		return controllerutil.SetControllerReference(claim, np, r.Scheme)
	})

	if err != nil {
		logger.Error(err, "Failed to create or update NetworkPolicy for claim")
		return err
	}

	logger.Info("Successfully reconciled NetworkPolicy for claim", "NetworkPolicy.Name", np.Name)
	return nil
}

// NameHash generates an FNV-1a hash from a string and returns
// it as a fixed-length hexadecimal string.
func NameHash(objectName string) string {
	h := fnv.New32a()
	h.Write([]byte(objectName))
	hashValue := h.Sum32()
	return fmt.Sprintf("%08x", hashValue)
}
