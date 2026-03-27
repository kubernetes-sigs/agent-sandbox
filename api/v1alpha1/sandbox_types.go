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
	// SandboxConditionInitialized represents the first-time setup of the sandbox dependencies
	SandboxConditionInitialized ConditionType = "Initialized"
	// SandboxReasonInitializing indicates the sandbox dependencies are being provisioned
	SandboxReasonInitializing = "SandboxInitializing"
	// SandboxReasonInitialized indicates the first-time setup of the sandbox is complete.
	SandboxReasonInitialized = "SandboxInitialized"

	// SandboxConditionSuspended indicates the sandbox is administratively suspended
	SandboxConditionSuspended ConditionType = "Suspended"
	// SandboxReasonPendingEvaluation indicates the reason hasn't been evaluated yet by the controller.
	SandboxReasonPendingEvaluation = "PendingEvaluation"
	// SandboxReasonUserSuspended indicates the sandbox has been suspended by the user
	SandboxReasonUserSuspended = "UserSuspended"
	// SandboxReasonNotSuspended indicates the sandbox is operational and not suspended
	SandboxReasonNotSuspended = "NotSuspended"

	// SandboxConditionReady indicates readiness for Sandbox
	SandboxConditionReady ConditionType = "Ready"
	// SandboxReasonReady indicates the sandbox is fully operational
	SandboxReasonReady = "SandboxReady"
	// SandboxReasonPodProvisioning indicates the sandbox pod is being created or starting
	SandboxReasonPodProvisioning = "PodProvisioning"
	// SandboxReasonSuspended indicates the sandbox is suspended and not ready
	SandboxReasonSuspended = "SandboxSuspended"
	// SandboxReasonUnresponsive indicates the sandbox pod is in an unknown state
	SandboxReasonUnresponsive = "SandboxUnresponsive"
	// SandboxReasonExpired indicates the sandbox is being terminated due to expiration.
	SandboxReasonExpired = "SandboxExpired"
	// SandboxReasonDeleting indicates the sandbox is being terminated due to deletion.
	SandboxReasonDeleting = "SandboxDeleting"

	// SandboxPodNameAnnotation is the annotation used to track the pod name adopted from a warm pool.
	SandboxPodNameAnnotation = "agents.x-k8s.io/pod-name"
	// SandboxTemplateRefAnnotation is the annotation used to track the sandbox template ref.
	SandboxTemplateRefAnnotation = "agents.x-k8s.io/sandbox-template-ref"
)

type PodMetadata struct {
	// labels defines the map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`

	// annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

type EmbeddedObjectMetadata struct {
	// name must be unique within a namespace. Is required when creating resources, although
	// some resources may allow a client to request the generation of an appropriate name
	// automatically. Name is primarily intended for creation idempotence and configuration
	// definition.
	// Cannot be updated.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names#names
	// +optional
	Name string `json:"name,omitempty" protobuf:"bytes,1,opt,name=name"`

	// labels defines the map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`

	// annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

type PodTemplate struct {
	// spec is the Pod's spec
	// +required
	Spec corev1.PodSpec `json:"spec" protobuf:"bytes,3,opt,name=spec"`

	// metadata is the Pod's metadata. Only labels and annotations are used.
	// +optional
	ObjectMeta PodMetadata `json:"metadata" protobuf:"bytes,3,opt,name=metadata"`
}

type PersistentVolumeClaimTemplate struct {
	// metadata is the PVC's metadata.
	// +optional
	EmbeddedObjectMetadata `json:"metadata" protobuf:"bytes,3,opt,name=metadata"`

	// spec is the PVC's spec
	// +required
	Spec corev1.PersistentVolumeClaimSpec `json:"spec" protobuf:"bytes,3,opt,name=spec"`
}

// SandboxSpec defines the desired state of Sandbox
type SandboxSpec struct {
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// podTemplate describes the pod spec that will be used to create an agent sandbox.
	// +required
	PodTemplate PodTemplate `json:"podTemplate" protobuf:"bytes,3,opt,name=podTemplate"`

	// volumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.
	// Every claim in this list must have at least one matching access mode with a provisioner volume.
	// +optional
	VolumeClaimTemplates []PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty" protobuf:"bytes,4,rep,name=volumeClaimTemplates"`

	// Lifecycle defines when and how the sandbox should be shut down.
	// +optional
	Lifecycle `json:",inline"`

	// replicas is the number of desired replicas.
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
	// shutdownTime is the absolute time when the sandbox expires.
	// +kubebuilder:validation:Format="date-time"
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// shutdownPolicy determines if the Sandbox resource itself should be deleted when it expires.
	// Underlying resources(Pods, Services) are always deleted on expiry.
	// +kubebuilder:default=Retain
	// +optional
	ShutdownPolicy *ShutdownPolicy `json:"shutdownPolicy,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// serviceFQDN that is valid for default cluster settings
	// Limitation: Hardcoded to the domain .cluster.local
	// e.g. sandbox-example.default.svc.cluster.local
	// +optional
	ServiceFQDN string `json:"serviceFQDN,omitempty"`

	// service is a sandbox-example
	// +optional
	Service string `json:"service,omitempty"`

	// conditions defines the status conditions array
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// replicas is the number of actual replicas.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// selector is the label selector for pods.
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
