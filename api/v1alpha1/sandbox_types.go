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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionType is a type of condition for a resource.
type ConditionType string

func (c ConditionType) String() string { return string(c) }

type TTLPolicyType string

func (c TTLPolicyType) String() string { return string(c) }

const (
	// SandboxConditionReady indicates readiness for Sandbox
	SandboxConditionReady ConditionType = "Ready"

	// TTL policy
	TTLPolicyOnCreate TTLPolicyType = "onCreate"
	TTLPolicyOnReady  TTLPolicyType = "onReady"
	TTLPolicyOnEnable TTLPolicyType = "onEnable"
	TTLPolicyNever    TTLPolicyType = "never"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

type TTLConfig struct {
	// Seconds sets after how many seconds should the sandbox be deleted
	Seconds int32 `json:"seconds,omitempty"`

	// StartPolicy indicated when the count down for shutdown should start
	// onCreate - TTL starts from sandbox creation
	// onReady - TTL starts from sandbox ready
	// onEnable - When this is set and .status.shutdownAt is nil
	// never - TTL is disabled
	// +kubebuilder:validation:Enum=onCreate;onReady;onEnable;disable
	StartPolicy TTLPolicyType `json:"startPolicy,omitempty"`

	//ShutdownAt - Absolute time when the sandbox is deleted.
	// setting this would override StartPolicy and Seconds
	ShutdownAt string `json:"shutdownAt,omitempty"`
}

// SandboxSpec defines the desired state of Sandbox
type SandboxSpec struct {
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// PodTemplate describes the pod spec that will be used to create an agent sandbox.
	// +kubebuilder:validation:Required
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate" protobuf:"bytes,3,opt,name=podTemplate"`

	// +optional
	TTL *TTLConfig `json:"ttl,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// FQDN that is valid for default cluster settings
	// Limitation: Hardcoded to the domain .cluster.local
	// e.g. sandbox-example.default.svc.cluster.local
	ServiceFQDN string `json:"serviceFQDN,omitempty"`

	// e.g. sandbox-example
	Service string `json:"service,omitempty"`

	// status conditions array
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// FirstReadyTime - when did the sandbox become ready first
	FirstReadyTime *metav1.Time `json:"firstReadyTime,omitempty"`

	// ShutdownAt - when will the sandbox be deleted
	ShutdownAt *metav1.Time `json:"ttlShutdownAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sandbox
// Sandbox is the Schema for the sandboxes API
type Sandbox struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Sandbox
	// +required
	Spec SandboxSpec `json:"spec"`

	// status defines the observed state of Sandbox
	// +optional
	Status SandboxStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SandboxList contains a list of Sandbox
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
