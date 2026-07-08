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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NOTE: json tags are required. Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

const (
	// TemplateRefField is the field used for indexing SandboxWarmPools by their template reference name.
	// Warning: This path must exactly match the JSON tag path of SandboxWarmPoolSpec.TemplateRef.Name.
	// If the JSON tags are changed, this constant must be updated to avoid indexer failures.
	TemplateRefField = ".spec.sandboxTemplateRef.name"
)

// SandboxTemplateRef references a SandboxTemplate.
type SandboxTemplateRef struct {
	// name of the SandboxTemplate
	// +required
	Name string `json:"name"`
}

// SandboxWarmPoolSpec defines the desired state of SandboxWarmPool.
type SandboxWarmPoolSpec struct {
	// replicas is the desired number of sandboxes in the pool.
	// This field is controlled by an HPA if specified.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// sandboxTemplateRef - name of the SandboxTemplate to be used for creating a Sandbox
	// Warning: Any change to the json tag "sandboxTemplateRef" must be synchronized with the TemplateRefField constant.
	// +required
	TemplateRef SandboxTemplateRef `json:"sandboxTemplateRef,omitempty"`

	// updateStrategy controls how the pool replaces its stale sandboxes. A sandbox is
	// considered stale when the effective SandboxBlueprint derived from the referenced
	// SandboxTemplate (or the sandboxTemplateRef name) changes; metadata-only edits
	// (annotations or labels) do not make a sandbox stale and never trigger replacement.
	// It applies only to sandboxes still owned by the pool (i.e. unclaimed). Once a sandbox
	// is claimed by a SandboxClaim, ownership transfers to the claim and the pool no longer
	// manages or replaces it.
	// Defaults to OnReplenish.
	// +optional
	UpdateStrategy *SandboxWarmPoolUpdateStrategy `json:"updateStrategy,omitempty"`
}

// SandboxWarmPoolUpdateStrategyType is a string enumeration type that enumerates
// all possible update strategies for the SandboxWarmPool controller.
// +kubebuilder:validation:Enum=Recreate;OnReplenish
type SandboxWarmPoolUpdateStrategyType string

const (
	// RecreateSandboxWarmPoolUpdateStrategyType deletes stale unclaimed sandboxes immediately
	// so the pool only holds fresh sandboxes matching the current template. Already-claimed
	// sandboxes are never touched.
	// Note: This applies to changes in the template's SandboxBlueprint only. Changes to annotations, labels, or template-level policies do not trigger recreate.
	RecreateSandboxWarmPoolUpdateStrategyType SandboxWarmPoolUpdateStrategyType = "Recreate"
	// OnReplenishSandboxWarmPoolUpdateStrategyType leaves stale unclaimed sandboxes in place.
	// A stale sandbox is only replaced with a fresh one when it is manually deleted, or when it
	// is claimed by a SandboxClaim (which removes it from the pool and triggers replenishment).
	// Already-claimed sandboxes are never touched.
	OnReplenishSandboxWarmPoolUpdateStrategyType SandboxWarmPoolUpdateStrategyType = "OnReplenish"
)

// SandboxWarmPoolUpdateStrategy defines the update strategy for the SandboxWarmPool.
type SandboxWarmPoolUpdateStrategy struct {
	// type indicates the type of the SandboxWarmPoolUpdateStrategy.
	// Default is OnReplenish.
	// +kubebuilder:default=OnReplenish
	// +optional
	Type SandboxWarmPoolUpdateStrategyType `json:"type,omitempty"`
}

// SandboxWarmPoolStatus defines the observed state of SandboxWarmPool.
type SandboxWarmPoolStatus struct {
	// replicas is the total number of sandboxes in the pool.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// readyReplicas is the total number of sandboxes in the pool that are in a ready state.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// selector is the label selector used to find the pods in the pool.
	// +optional
	Selector string `json:"selector,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:scope=Namespaced,shortName=swp
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:storageversion
// +kubebuilder:conversion:strategy=Webhook
// SandboxWarmPool is the Schema for the sandboxwarmpools API.
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
// SandboxWarmPoolList contains a list of SandboxWarmPool.
type SandboxWarmPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxWarmPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &SandboxWarmPool{}, &SandboxWarmPoolList{})
		return nil
	})
}
