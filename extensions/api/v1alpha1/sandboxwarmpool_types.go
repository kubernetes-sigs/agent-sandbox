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
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

// NOTE: json tags are required. Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

// SandboxWarmPoolSpec defines the desired state of SandboxWarmPool
type SandboxWarmPoolSpec struct {
	// Replicas is the desired number of sandboxes in the pool.
	// This field is controlled by an HPA if specified.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// PodTemplate describes the pod spec that will be used to create sandboxes in the warm pool.
	// +kubebuilder:validation:Required
	PodTemplate sandboxv1alpha1.PodTemplate `json:"podTemplate"`
}

// SandboxWarmPoolStatus defines the observed state of SandboxWarmPool
type SandboxWarmPoolStatus struct {
	// Replicas is the total number of sandboxes in the pool.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:resource:scope=Namespaced,shortName=swp
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// SandboxWarmPool is the Schema for the sandboxwarmpools API
type SandboxWarmPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of SandboxWarmPool
	// +required
	Spec SandboxWarmPoolSpec `json:"spec"`

	// status defines the observed state of SandboxWarmPool
	// +optional
	Status SandboxWarmPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxWarmPoolList contains a list of SandboxWarmPool
type SandboxWarmPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxWarmPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxWarmPool{}, &SandboxWarmPoolList{})
}
