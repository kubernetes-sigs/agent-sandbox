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

// NOTE: json tags are required. Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

// StrategyType defines the type of upgrade strategy
type StrategyType string

const (
	// RollingUpdateStrategyType means the warm pool is updated in a rolling fashion
	RollingUpdateStrategyType StrategyType = "RollingUpdate"
)

// RollingUpdateStrategy defines the parameters for a rolling update strategy
type RollingUpdateStrategy struct {
	// The maximum number of sandboxes that can be scheduled above the desired number of replicas.
	// Value can be an absolute number (ex: 5) or a percentage of desired replicas (ex: 10%).
	// Defaults to 1.
	// +optional
	MaxSurge *int32 `json:"maxSurge,omitempty"`

	// The maximum number of sandboxes that can be unavailable during the update.
	// Value can be an absolute number (ex: 5) or a percentage of desired replicas (ex: 10%).
	// Defaults to 0.
	// +optional
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`
}

// SandboxWarmPoolStrategy defines the upgrade strategy for the warm pool
type SandboxWarmPoolStrategy struct {
	// Type of upgrade strategy. Currently only supports RollingUpdate.
	// +optional
	// +kubebuilder:default=RollingUpdate
	Type StrategyType `json:"type,omitempty"`

	// Rolling update config params. Present only if Type = RollingUpdate.
	// +optional
	RollingUpdate *RollingUpdateStrategy `json:"rollingUpdate,omitempty"`
}

// SandboxWarmPoolSpec defines the desired state of SandboxWarmPool
type SandboxWarmPoolSpec struct {
	// Replicas is the desired number of sandboxes in the pool.
	// This field is controlled by an HPA if specified.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// PodTemplate describes the pod spec that will be used to create sandboxes in the warm pool.
	// +kubebuilder:validation:Required
	PodTemplate PodTemplate `json:"podTemplate"`

	// Strategy defines the upgrade strategy for the warm pool.
	// +optional
	Strategy SandboxWarmPoolStrategy `json:"strategy,omitempty"`
}

// SandboxWarmPoolStatus defines the observed state of SandboxWarmPool
type SandboxWarmPoolStatus struct {
	// Replicas is the total number of sandboxes in the pool.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of sandboxes that are ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of sandboxes that are available (ready and not allocated).
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// Conditions represent the latest available observations of the warm pool's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyReplicas
// +kubebuilder:resource:scope=Namespaced,shortName=swp
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
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
