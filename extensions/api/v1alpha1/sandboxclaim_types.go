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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

const (
	// ClaimExpiredReason is the reason used in conditions/events when a claim expires.
	ClaimExpiredReason = "ClaimExpired"
)

// ShutdownPolicy describes the policy for shutting down the underlying Sandbox when the SandboxClaim expires.
// +kubebuilder:validation:Enum=Delete;Retain
type ShutdownPolicy string

const (
	// ShutdownPolicyDelete deletes the SandboxClaim (and cascadingly the Sandbox) when expired.
	ShutdownPolicyDelete ShutdownPolicy = "Delete"

	// ShutdownPolicyRetain keeps the SandboxClaim when expired (Status will show Expired).
	// The underlying SandboxClaim resources (Sandbox, Pod, Service) are deleted to save resources,
	// but the SandboxClaim object itself remains.
	ShutdownPolicyRetain ShutdownPolicy = "Retain"
)

// Lifecycle defines the lifecycle management for the SandboxClaim.
type Lifecycle struct {
	// ShutdownTime is the absolute time when the SandboxClaim expires.
	// This time governs the lifecycle of the claim. It is not propagated to the
	// underlying Sandbox. Instead, the SandboxClaim controller enforces this
	// expiration by deleting the Sandbox resources when the time is reached.
	// If this field is omitted or set to nil, the SandboxClaim itself won't expire.
	// This implies unsetting a Sandbox's ShutdownTime via SandboxClaim isn't supported.
	// +kubebuilder:validation:Format="date-time"
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// ShutdownPolicy determines the behavior when the SandboxClaim expires.
	// +kubebuilder:default=Retain
	// +optional
	ShutdownPolicy ShutdownPolicy `json:"shutdownPolicy,omitempty"`
}

// SandboxTemmplateRef references a SandboxTemplate
type SandboxTemplateRef struct {
	// name of the SandboxTemplate
	// +kubebuilder:validation:Required
	Name string `json:"name,omitempty" protobuf:"bytes,1,name=name"`
}

// SandboxClaimSpec defines the desired state of Sandbox
type SandboxClaimSpec struct {
	// SandboxTemplateRefName - name of the SandboxTemplate to be used for creating a Sandbox
	// +kubebuilder:validation:Required
	TemplateRef SandboxTemplateRef `json:"sandboxTemplateRef,omitempty" protobuf:"bytes,3,name=sandboxTemplateRef"`

	// Lifecycle defines when and how the SandboxClaim should be shut down.
	// +optional
	Lifecycle *Lifecycle `json:"lifecycle,omitempty"`
}

// SandboxClaimStatus defines the observed state of Sandbox.
type SandboxClaimStatus struct {
	// Conditions represent the latest available observations of a Sandbox's current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" protobuf:"bytes,1,rep,name=conditions"`

	SandboxStatus SandboxStatus `json:"sandbox,omitempty" protobuf:"bytes,2,opt,name=sandboxStatus"`
}

type SandboxStatus struct {
	// SandboxName is the name of the Sandbox created from this claim
	// +optional
	Name string `json:"Name,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sandboxclaim
// SandboxClaim is the Schema for the sandbox Claim API
type SandboxClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Sandbox
	// +required
	Spec SandboxClaimSpec `json:"spec"`

	// status defines the observed state of Sandbox
	// +optional
	Status SandboxClaimStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SandboxList contains a list of Sandbox
type SandboxClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxClaim{}, &SandboxClaimList{})
}
