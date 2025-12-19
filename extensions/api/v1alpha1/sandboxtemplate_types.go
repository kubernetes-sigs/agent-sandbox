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

package v1alpha1

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

const (
	// SandboxIDLabel is the label key applied to the Pod to identify the owning Claim UID.
	// The SandboxClaim controller injects this label into the Pod
	// System-injected labels/annotations shouldn't be touched
	SandboxIDLabel = "agents.x-k8s.io/claim-uid"
)

// NetworkPolicySpec defines the desired state of the NetworkPolicy.
type NetworkPolicySpec struct {
	// Ingress is a list of ingress rules to be applied to the sandbox.
	// Traffic is allowed to the sandbox if it matches at least one rule.
	// If this list is empty, all ingress traffic is blocked (Default Deny).
	// +optional
	Ingress []networkingv1.NetworkPolicyIngressRule `json:"ingress,omitempty"`

	// Egress is a list of egress rules to be applied to the sandbox.
	// Traffic is allowed out of the sandbox if it matches at least one rule.
	// If this list is empty, all egress traffic is blocked (Default Deny).
	// +optional
	Egress []networkingv1.NetworkPolicyEgressRule `json:"egress,omitempty"`
}

// SandboxTemplateSpec defines the desired state of Sandbox
type SandboxTemplateSpec struct {
	// template is the object that describes the pod spec that will be used to create
	// an agent sandbox.
	// If AutomountServiceAccountToken is not specified in the PodSpec, it defaults to false
	// to ensure a secure-by-default environment.
	// +kubebuilder:validation:Required
	PodTemplate sandboxv1alpha1.PodTemplate `json:"podTemplate" protobuf:"bytes,3,opt,name=podTemplate"`

	// NetworkPolicy defines the network policy to be applied to the sandboxes
	// created from this template.
	// NOTE: This is a restricted subset of the standard Kubernetes NetworkPolicySpec.
	// Fields like 'PodSelector' and 'PolicyTypes' are intentionally excluded because
	// they are managed by the controller to ensure strict isolation and default-deny posture.
	// WARNING: This policy enforces a strict "Default Deny" ingress posture.
	// If your Pod uses sidecars (e.g., Istio proxy, monitoring agents) that listen
	// on their own ports, the NetworkPolicy will BLOCK traffic to them by default.
	// You MUST explicitly allow traffic to these sidecar ports using 'Ingress',
	// otherwise the sidecars may fail health checks.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
}

// SandboxTemplateStatus defines the observed state of Sandbox.
type SandboxTemplateStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sandboxtemplate
// SandboxTemplate is the Schema for the sandboxe template API
type SandboxTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Sandbox
	// +required
	Spec SandboxTemplateSpec `json:"spec"`

	// status defines the observed state of Sandbox
	// +optional
	Status SandboxTemplateStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SandboxTemplateList contains a list of Sandbox
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxTemplate{}, &SandboxTemplateList{})
}
