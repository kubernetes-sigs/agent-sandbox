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

// NetworkPolicyManagement defines whether the controller automatically generates
// and manages a shared NetworkPolicy for this template.
type NetworkPolicyManagement string

const (
	// SandboxIDLabel is the label key applied to the Pod to identify the owning Claim UID.
	// The SandboxClaim controller injects this label into the Pod
	// System-injected labels/annotations shouldn't be touched
	SandboxIDLabel = "agents.x-k8s.io/claim-uid"

	// NetworkPolicyManagementManaged means the controller will ensure a shared NetworkPolicy exists.
	// This shared NetworkPolicy will be a user provide one or a default controller created policy.
	// This is the default behavior if the field is omitted.
	NetworkPolicyManagementManaged NetworkPolicyManagement = "Managed"

	// NetworkPolicyManagementUnmanaged means the controller will skip NetworkPolicy
	// creation entirely, allowing external systems (like Cilium) to manage networking.
	NetworkPolicyManagementUnmanaged NetworkPolicyManagement = "Unmanaged"
)

// NetworkPolicySpec defines the desired state of the NetworkPolicy.
type NetworkPolicySpec struct {
	// ingress is a list of ingress rules to be applied to the sandbox.
	// Traffic is allowed to the sandbox if it matches at least one rule.
	// If this list is empty, all ingress traffic is blocked (Default Deny).
	// +optional
	Ingress []networkingv1.NetworkPolicyIngressRule `json:"ingress,omitempty"`

	// egress is a list of egress rules to be applied to the sandbox.
	// Traffic is allowed out of the sandbox if it matches at least one rule.
	// If this list is empty, all egress traffic is blocked (Default Deny).
	// +optional
	Egress []networkingv1.NetworkPolicyEgressRule `json:"egress,omitempty"`
}

// SandboxTemplateSpec defines the desired state of Sandbox
type SandboxTemplateSpec struct {
	// podTemplate defines the object template that describes the pod spec that will be used to create
	// an agent sandbox.
	// If AutomountServiceAccountToken is not specified in the PodSpec, it defaults to false
	// to ensure a secure-by-default environment.
	// +required
	PodTemplate sandboxv1alpha1.PodTemplate `json:"podTemplate" protobuf:"bytes,3,opt,name=podTemplate"`

	// VolumeClaimTemplates is a list of claims that pods created from this template
	// are allowed to reference. When a SandboxClaim or SandboxWarmPool creates a sandbox
	// from this template, PVCs will be created from these templates.
	// +optional
	// +listType=atomic
	VolumeClaimTemplates []sandboxv1alpha1.PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty" protobuf:"bytes,4,rep,name=volumeClaimTemplates"`

	// networkPolicy defines the network policy to be applied to the sandboxes
	// created from this template. A single shared NetworkPolicy is created per Template.
	// Behavior is dictated by the NetworkPolicyManagement field:
	// - If Management is "Unmanaged": This field is completely ignored.
	// - If Management is "Managed" (default) and this field is omitted (nil): The controller
	//   automatically applies a strict Secure Default policy:
	//     * Ingress: Allow traffic only from the Sandbox Router.
	//     * Egress: Allow Public Internet only. Blocks internal IPs (RFC1918), Metadata Server, etc.
	// - If Management is "Managed" and this field is provided: The controller applies your custom rules.
	// Update Behavior:
	// Because the NetworkPolicy is shared at the template level, any updates to these rules
	// will be applied to the single shared policy object. The underlying Kubernetes CNI will then
	// dynamically enforce the updated rules across all existing and future sandboxes
	// referencing this template.
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

	// networkPolicyManagement defines whether the controller manages the NetworkPolicy.
	// Valid values are "Managed" (default) or "Unmanaged".
	// +kubebuilder:validation:Enum=Managed;Unmanaged
	// +kubebuilder:default=Managed
	// +optional
	NetworkPolicyManagement NetworkPolicyManagement `json:"networkPolicyManagement,omitempty"`
}

// SandboxTemplateStatus defines the observed state of Sandbox.
type SandboxTemplateStatus struct {
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sandboxtemplate
// SandboxTemplate is the Schema for the sandbox template API
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
