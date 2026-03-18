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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionType is a type of condition for a resource.
type ConditionType string

func (c ConditionType) String() string { return string(c) }

const (
	// SandboxConditionReady indicates readiness for Sandbox
	SandboxConditionReady ConditionType = "Ready"

	// SandboxReasonExpired indicates expired state for Sandbox
	SandboxReasonExpired = "SandboxExpired"

	// NetworkPolicyManagementManaged means the controller will ensure a NetworkPolicy exists.
	// This NetworkPolicy will be a user provide one or a default controller created policy.
	// This is the default behavior if the field is omitted.
	NetworkPolicyManagementManaged NetworkPolicyManagement = "Managed"

	// NetworkPolicyManagementUnmanaged means the controller will skip NetworkPolicy
	// creation entirely, allowing external systems (like Cilium) to manage networking.
	NetworkPolicyManagementUnmanaged NetworkPolicyManagement = "Unmanaged"
)

// NetworkPolicyManagement defines whether the controller automatically generates
// and manages a NetworkPolicy for this sandbox.
type NetworkPolicyManagement string

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

	// Lifecycle defines when and how the sandbox should be shut down.
	// +optional
	Lifecycle `json:",inline"`

	// NetworkPolicy defines the network policy to be applied to the sandbox.
	// Behavior is dictated by the NetworkPolicyManagement field:
	// - If Management is "Unmanaged": This field is completely ignored.
	// - If Management is "Managed" (default) and this field is omitted (nil): The controller
	//   automatically applies a strict Secure Default policy:
	//     * Ingress: Allow traffic only from the Sandbox Router.
	//     * Egress: Allow Public Internet only. Blocks internal IPs (RFC1918), Metadata Server, etc.
	// - If Management is "Managed" and this field is provided: The controller applies your custom rules.
	// WARNING: This policy enforces a strict "Default Deny" ingress posture.
	// If your Pod uses sidecars (e.g., Istio proxy, monitoring agents) that listen
	// on their own ports, the NetworkPolicy will BLOCK traffic to them by default.
	// You MUST explicitly allow traffic to these sidecar ports using 'Ingress',
	// otherwise the sidecars may fail health checks.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// NetworkPolicyManagement defines whether the controller manages the NetworkPolicy.
	// Valid values are "Managed" (default) or "Unmanaged".
	// +kubebuilder:validation:Enum=Managed;Unmanaged
	// +kubebuilder:default=Managed
	// +optional
	NetworkPolicyManagement NetworkPolicyManagement `json:"networkPolicyManagement,omitempty"`

	// Replicas is the number of desired replicas.
	// The only allowed values are 0 and 1.
	// Defaults to 1.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// ShutdownPolicy describes the policy for deleting the Sandbox when it expires.
// +kubebuilder:validation:Enum=Delete;Retain
type ShutdownPolicy string

const (
	// ShutdownPolicyDelete deletes the Sandbox when expired.
	ShutdownPolicyDelete ShutdownPolicy = "Delete"

	// ShutdownPolicyRetain keeps the Sandbox when expired (Status will show Expired).
	ShutdownPolicyRetain ShutdownPolicy = "Retain"
)

// Lifecycle defines the lifecycle management for the Sandbox.
type Lifecycle struct {
	// ShutdownTime is the absolute time when the sandbox expires.
	// +kubebuilder:validation:Format="date-time"
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// ShutdownPolicy determines if the Sandbox resource itself should be deleted when it expires.
	// Underlying resources(Pods, Services) are always deleted on expiry.
	// +kubebuilder:default=Retain
	// +optional
	ShutdownPolicy *ShutdownPolicy `json:"shutdownPolicy,omitempty"`
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

// +genclient
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
