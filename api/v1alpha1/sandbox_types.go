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

const (
	// SandboxConditionReady indicates readiness for Sandbox
	SandboxConditionReady ConditionType = "Ready"
)

type PodMetadata struct {
	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`

	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

type EmbeddedObjectMetadata struct {
	// Name must be unique within a namespace. Is required when creating resources, although
	// some resources may allow a client to request the generation of an appropriate name
	// automatically. Name is primarily intended for creation idempotence and configuration
	// definition.
	// Cannot be updated.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names#names
	// +optional
	Name string `json:"name,omitempty" protobuf:"bytes,1,opt,name=name"`

	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`

	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

type PodTemplate struct {
	// Spec is the Pod's spec
	// +kubebuilder:validation:Required
	Spec corev1.PodSpec `json:"spec" protobuf:"bytes,3,opt,name=spec"`

	// Metadata is the Pod's metadata. Only labels and annotations are used.
	// +kubebuilder:validation:Optional
	ObjectMeta PodMetadata `json:"metadata" protobuf:"bytes,3,opt,name=metadata"`
}

type PersistentVolumeClaimTemplate struct {
	// Metadata is the PVC's metadata.
	// +kubebuilder:validation:Optional
	EmbeddedObjectMetadata `json:"metadata" protobuf:"bytes,3,opt,name=metadata"`

	// Spec is the PVC's spec
	// +kubebuilder:validation:Required
	Spec corev1.PersistentVolumeClaimSpec `json:"spec" protobuf:"bytes,3,opt,name=spec"`
}

// SandboxSpec defines the desired state of Sandbox
type SandboxSpec struct {
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// PodTemplate describes the pod spec that will be used to create an agent sandbox.
	// +kubebuilder:validation:Required
	PodTemplate PodTemplate `json:"podTemplate" protobuf:"bytes,3,opt,name=podTemplate"`

	// VolumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.
	// Every claim in this list must have at least one matching access mode with a provisioner volume.
	// +optional
	// +kubebuilder:validation:Optional
	VolumeClaimTemplates []PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty" protobuf:"bytes,4,rep,name=volumeClaimTemplates"`

	// ShutdownTime - Absolute time when the sandbox is deleted.
	// If a time in the past is provided, the sandbox will be deleted immediately.
	// +kubebuilder:validation:Format="date-time"
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// Replicas is the number of desired replicas.
	// The only allowed values are 0 and 1.
	// Defaults to 1.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
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

	// Replicas is the number of actual replicas.
	Replicas int32 `json:"replicas"`

	// LabelSelector is the label selector for pods.
	// +optional
	LabelSelector string `json:"selector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
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
