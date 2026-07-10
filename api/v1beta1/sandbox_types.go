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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ConditionType is a type of condition for a resource.
type ConditionType string

func (c ConditionType) String() string { return string(c) }

const (
	// SandboxConditionSuspended indicates the sandbox is administratively suspended.
	SandboxConditionSuspended ConditionType = "Suspended"
	// SandboxReasonSuspendedPodTerminated indicates that the pod has been terminated.
	SandboxReasonSuspendedPodTerminated = "PodTerminated"
	// SandboxReasonSuspendedPodNotTerminated indicates the pod has not been terminated yet.
	SandboxReasonSuspendedPodNotTerminated = "PodNotTerminated"

	// SandboxConditionReady indicates readiness for Sandbox.
	SandboxConditionReady ConditionType = "Ready"
	// SandboxReasonDependenciesReady indicates the sandbox is fully operational.
	SandboxReasonDependenciesReady = "DependenciesReady"
	// SandboxReasonDependenciesNotReady indicates the Sandbox is expected to be running
	// but its underlying dependencies are not fully provisioned or ready yet.
	SandboxReasonDependenciesNotReady = "DependenciesNotReady"
	// SandboxReasonSuspended indicates the Sandbox has been administratively suspended
	// (i.e., intentional action by the user to suspend the Sandbox).
	SandboxReasonSuspended = "SandboxSuspended"

	// SandboxConditionFinished indicates the backing Pod reached a terminal phase.
	SandboxConditionFinished ConditionType = "Finished"
	// SandboxReasonPodSucceeded indicates the backing Pod completed successfully.
	SandboxReasonPodSucceeded = "PodSucceeded"
	// SandboxReasonPodFailed indicates the backing Pod completed unsuccessfully.
	SandboxReasonPodFailed = "PodFailed"

	// SandboxReasonExpired indicates expired state for Sandbox.
	SandboxReasonExpired = "SandboxExpired"

	// SandboxPodNameAnnotation is the annotation used to track the pod name adopted from a warm pool.
	SandboxPodNameAnnotation = "agents.x-k8s.io/pod-name"
	// SandboxTemplateRefAnnotation is the annotation used to track the sandbox template ref.
	SandboxTemplateRefAnnotation = "agents.x-k8s.io/sandbox-template-ref"
	// SandboxLaunchTypeLabel is the label used to track whether the Sandbox was cold-created or originated from a warm pool.
	SandboxLaunchTypeLabel = "agents.x-k8s.io/launch-type"
	// CreatedByLabel is the label used to track which component created the resource (e.g. client, controller, etc.).
	CreatedByLabel = "agents.x-k8s.io/created-by"
	// SandboxLaunchTypeCold indicates the Sandbox was cold-created.
	SandboxLaunchTypeCold = "cold"
	// SandboxLaunchTypeWarm indicates the Sandbox was pre-provisioned by or adopted from a SandboxWarmPool.
	SandboxLaunchTypeWarm = "warm"
	// DeprecatedSandboxPodTemplateHashLabel is the label used to track the pod template hash.
	// Deprecated: Use SandboxTemplateHashLabel instead.
	DeprecatedSandboxPodTemplateHashLabel = "agents.x-k8s.io/sandbox-pod-template-hash"
	// SandboxTemplateHashLabel is the label used to track the blueprint hash.
	SandboxTemplateHashLabel = "agents.x-k8s.io/sandbox-template-hash"
	// SandboxPropagatedLabelsAnnotation is the annotation used to track the labels explicitly propagated from sandbox spec to pod.
	SandboxPropagatedLabelsAnnotation = "agents.x-k8s.io/propagated-labels"
	// SandboxPropagatedAnnotationsAnnotation is the annotation used to track the annotations explicitly propagated from sandbox spec to pod.
	SandboxPropagatedAnnotationsAnnotation = "agents.x-k8s.io/propagated-annotations"
	// SandboxAdoptableLabel is the label used to authorize a Sandbox to adopt an existing unowned resource.
	SandboxAdoptableLabel = "agents.x-k8s.io/adoptable"
	// SandboxWarmPoolLabel is the label used to track the warm pool that owns the Sandbox.
	SandboxWarmPoolLabel = "agents.x-k8s.io/warm-pool-sandbox"
)

type PodMetadata struct {
	// labels defines the map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

type EmbeddedObjectMetadata struct {
	// name must be unique within a namespace. Is required when creating resources, although
	// some resources may allow a client to request the generation of an appropriate name
	// automatically. Name is primarily intended for creation idempotence and configuration
	// definition.
	// Cannot be updated.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names#names
	// +optional
	Name string `json:"name,omitempty"`

	// labels defines the map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

type PodTemplate struct {
	// spec is the Pod's spec
	// +required
	Spec corev1.PodSpec `json:"spec"`

	// metadata is the Pod's metadata. Only labels and annotations are used.
	// +optional
	ObjectMeta PodMetadata `json:"metadata"`
}

type PersistentVolumeClaimTemplate struct {
	// metadata is the PVC's metadata.
	// +optional
	EmbeddedObjectMetadata `json:"metadata"`

	// spec is the PVC's spec
	// +required
	Spec corev1.PersistentVolumeClaimSpec `json:"spec"`
}

// SandboxOperatingMode defines the desired operational state of the Sandbox.
type SandboxOperatingMode string

const (
	// SandboxOperatingModeRunning indicates the sandbox should be actively running.
	SandboxOperatingModeRunning SandboxOperatingMode = "Running"
	// SandboxOperatingModeSuspended indicates the sandbox should be suspended.
	SandboxOperatingModeSuspended SandboxOperatingMode = "Suspended"
)

// NOTE: When adding, removing, or renaming a field in SandboxBlueprint,
// also update compareSandboxBlueprint() in extensions/controllers/sandboxwarmpool_controller.go
// so the SandboxWarmPool staleness check accounts for it. A field left out of that comparison
// is not tracked for drift, so warm sandboxes will not be detected as stale when it changes.

// SandboxBlueprint defines the configuration shared between Sandbox and SandboxTemplate.
// It deliberately excludes runtime-only fields (operatingMode, lifecycle).
type SandboxBlueprint struct {
	// podTemplate describes the pod that will be created in the sandbox.
	// Note: When provisioned via a SandboxTemplate (such as by a SandboxClaim or SandboxWarmPool),
	// if AutomountServiceAccountToken is not specified in the PodSpec, the controller defaults it
	// to false to ensure a secure-by-default environment.
	// +required
	PodTemplate PodTemplate `json:"podTemplate"`

	// volumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.
	// When creating a sandbox, PVCs will be created from these templates.
	// Every claim in this list must have at least one matching access mode with a provisioner volume.
	// NOTE: This list is atomic. Updates to this field will replace the entire list rather than merging with existing entries.
	// +optional
	// +listType=atomic
	VolumeClaimTemplates []PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`

	// service controls whether the controller should automatically create a
	// headless Service for the Sandbox workload.
	// When unset, the controller preserves existing Services for backward
	// compatibility but does not create new ones. Set to true to enable or false
	// to explicitly disable and remove the Service.
	//nolint:kubeapilinter // Enum not used to avoid duplicating the Service API; field is not expected to extend (issue #746).
	// +optional
	Service *bool `json:"service,omitempty"`
}

// SandboxSpec defines the desired state of Sandbox.
type SandboxSpec struct {
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// SandboxBlueprint defines the workload configuration shared with SandboxTemplate.
	// NOTE: Once a field is added here, it is promoted to both Sandbox and SandboxTemplate.
	// Since moving fields out is breaking, if unsure whether a new field should be shared,
	// define it in SandboxSpec (or SandboxTemplateSpec) first and promote it here later.
	SandboxBlueprint `json:",inline"`

	// Lifecycle defines when and how the sandbox should be shut down.
	// +optional
	Lifecycle `json:",inline"`

	// operatingMode specifies the desired operational state of the Sandbox.
	// Defaults to Running if not specified.
	// +kubebuilder:default=Running
	// +kubebuilder:validation:Enum=Running;Suspended
	// +optional
	OperatingMode SandboxOperatingMode `json:"operatingMode,omitempty"`
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
	// The domain defaults to cluster.local but is configurable via the controller's --cluster-domain flag.
	// +optional
	ServiceFQDN string `json:"serviceFQDN,omitempty"`

	// service is a sandbox-example
	// +optional
	Service string `json:"service,omitempty"`

	// conditions defines the status conditions array
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// selector is the label selector for pods.
	// +optional
	LabelSelector string `json:"selector,omitempty"`

	// podIPs are the IP addresses of the underlying pod.
	// A pod may have multiple IPs in dual-stack clusters.
	// +optional
	PodIPs []string `json:"podIPs,omitempty"`

	// nodeName is the name of the node where the underlying pod is scheduled.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sandbox
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:storageversion
// +kubebuilder:conversion:strategy=Webhook
// Sandbox is the Schema for the sandboxes API.
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

// SandboxList contains a list of Sandbox.
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Sandbox{}, &SandboxList{})
		return nil
	})
}
