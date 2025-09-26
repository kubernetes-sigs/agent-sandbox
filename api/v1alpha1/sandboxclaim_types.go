/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

// SandboxClaimSpec defines the desired state of Sandbox
type SandboxClaimSpec struct {
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	TTLSeconds *int32 `json:"ttlSeconds,omitempty" protobuf:"varint,1,opt,name=ttlSeconds"`

	// Template refers to the SandboxTemplate to be used for creating a Sandbox
	// +kubebuilder:validation:Required
	TemplateName string `json:"templateName,omitempty" protobuf:"bytes,3,name=templateName"`
}

// SandboxClaimStatus defines the observed state of Sandbox.
type SandboxClaimStatus struct {
	SandboxName      string `json:"sandboxName,omitempty"`
	TemplateRevision string `json:"templateRevision,omitempty"`
	Hostname         string `json:"hostname,omitempty"`
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
