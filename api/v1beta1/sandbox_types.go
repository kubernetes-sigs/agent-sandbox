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
//
// Terminology: a Sandbox has two distinct notions that are easy to confuse.
//   - "running" is a desired state expressed by the user via spec.operatingMode
//     (see SandboxOperatingModeRunning). It says the controller should create and
//     keep a backing Pod. It does not, by itself, mean the Pod is up yet.
//   - "readiness" is an observed state reported by the Ready condition
//     (see SandboxConditionReady). It becomes True only once the backing Pod is
//     actually Running and Ready with an assigned IP (and its Service exists, if
//     requested). Note there is deliberately no separate "Running" status
//     condition: whether the Pod is running is subsumed by Ready (an unready Pod
//     that is still starting reports Ready=False with reason DependenciesNotReady).
//
// Condition names and semantics follow the Kubernetes/Gateway API conventions for
// status conditions (abnormal-true vs. normal-true polarity, stable reason strings,
// observedGeneration) so consumers can reason about them uniformly. See the Gateway
// API condition guidelines as a model:
// https://gateway-api.sigs.k8s.io/geps/gep-1364/
type ConditionType string

func (c ConditionType) String() string { return string(c) }

const (
	// SandboxConditionSuspended reports progress of an administrative suspension.
	// It is set while operatingMode is Suspended: Status is True once the backing Pod
	// has been terminated (reason PodTerminated), and False while the Pod is still
	// terminating (reason PodNotTerminated).
	// Note: the controller does not currently remove this condition when the Sandbox is
	// resumed, so a stale Suspended condition may linger after operatingMode returns to
	// Running. Consumers should treat Ready as the authoritative signal and not infer the
	// live operating state from the mere presence of this condition.
	SandboxConditionSuspended ConditionType = "Suspended"
	// SandboxReasonSuspendedPodTerminated indicates that the pod has been terminated.
	SandboxReasonSuspendedPodTerminated = "PodTerminated"
	// SandboxReasonSuspendedPodNotTerminated indicates the pod has not been terminated yet.
	SandboxReasonSuspendedPodNotTerminated = "PodNotTerminated"

	// SandboxConditionReady summarizes whether the Sandbox is fully operational and
	// able to serve traffic. This is the observed "readiness" of the Sandbox, and is
	// distinct from the desired "running" state requested via spec.operatingMode: a
	// Sandbox with operatingMode Running is not Ready until its Pod actually comes up.
	// Status is True only when the backing Pod is in the Running phase with its Pod
	// Ready condition True and at least one pod IP assigned, and, when a Service is
	// required, that Service exists. A Service is required when it is explicitly
	// requested (see SandboxBlueprint.Service) or, for backward compatibility, when a
	// Service already exists even though one was not explicitly requested.
	// In all other states Status is False with a reason describing why: the Sandbox is
	// still provisioning (DependenciesNotReady), suspended (SandboxSuspended), its Pod
	// has reached a terminal phase (PodSucceeded/PodFailed), it has expired
	// (SandboxExpired), or the controller hit an error (ReconcilerError).
	SandboxConditionReady ConditionType = "Ready"
	// SandboxReasonDependenciesReady is the Ready=True reason: the Pod (and Service, if
	// requested) are provisioned and the Pod reports Ready with an assigned IP.
	SandboxReasonDependenciesReady = "DependenciesReady"
	// SandboxReasonDependenciesNotReady is a Ready=False reason: the Sandbox is expected
	// to be running but its underlying dependencies (Pod and/or Service) are not fully
	// provisioned or not yet reporting Ready.
	SandboxReasonDependenciesNotReady = "DependenciesNotReady"
	// SandboxReasonSuspended is a Ready=False reason: the Sandbox has been administratively
	// suspended (i.e., intentional action by the user to suspend the Sandbox).
	SandboxReasonSuspended = "SandboxSuspended"

	// SandboxConditionFinished reports that the backing Pod reached a terminal phase.
	// It is set (Status True) only after the Pod has Succeeded or Failed, with the reason
	// recording which; it is absent while the Pod is still running or does not exist.
	SandboxConditionFinished ConditionType = "Finished"
	// SandboxReasonPodSucceeded indicates the backing Pod completed successfully.
	SandboxReasonPodSucceeded = "PodSucceeded"
	// SandboxReasonPodFailed indicates the backing Pod completed unsuccessfully.
	SandboxReasonPodFailed = "PodFailed"

	// SandboxReasonExpired is a Ready=False reason: the Sandbox reached its shutdownTime
	// and its underlying resources were torn down (see Lifecycle).
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
	// SandboxTemplateRefHashLabel identifies which SandboxTemplate a Sandbox originated from.
	SandboxTemplateRefHashLabel = "agents.x-k8s.io/sandbox-template-ref-hash"
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
// It expresses intent ("running" vs. "suspended"), not observed status; whether the
// Sandbox has actually reached that state is reported by conditions (see
// SandboxConditionReady and SandboxConditionSuspended).
type SandboxOperatingMode string

const (
	// SandboxOperatingModeRunning indicates the Sandbox should be actively running:
	// the controller ensures a backing Pod (and Service, if requested) is created and
	// kept running. This is a desired-state declaration only; observed readiness is
	// reported separately by the Ready condition (see SandboxConditionReady), which
	// stays False until the Pod is actually Running and Ready.
	SandboxOperatingModeRunning SandboxOperatingMode = "Running"
	// SandboxOperatingModeSuspended indicates the Sandbox should be suspended: the
	// controller terminates the backing Pod while retaining the Sandbox object and its
	// volumes. Progress of the suspension is reported by the Suspended condition (see
	// SandboxConditionSuspended).
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

	// operatingMode specifies the desired operational state of the Sandbox:
	//   - Running (default): the controller keeps a backing Pod running.
	//   - Suspended: the controller terminates the backing Pod but retains the
	//     Sandbox object and its volumes so it can later be resumed.
	// This field declares intent only. The observed readiness of the Sandbox is
	// reported by the Ready condition, and the progress of a suspension by the
	// Suspended condition; a Sandbox in Running mode is not Ready until its Pod is
	// actually up (see SandboxConditionReady).
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
	// shutdownTime is the absolute time at which the Sandbox expires. When the current
	// time reaches shutdownTime, the controller tears down the underlying resources
	// (Pod and Service) and then applies shutdownPolicy to the Sandbox object itself.
	// If unset, the Sandbox never expires and lives until it is explicitly deleted.
	// +kubebuilder:validation:Format="date-time"
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// shutdownPolicy determines what happens to the Sandbox object itself when it expires
	// (i.e. when shutdownTime is reached). The underlying resources (Pod, Service) are
	// always deleted on expiry regardless of this policy; shutdownPolicy governs only the
	// Sandbox object:
	//   - Retain (default): the Sandbox object is kept after its resources are torn down.
	//     Its live status fields are cleared and a Ready=False condition with reason
	//     SandboxExpired is set so the expiry is observable.
	//   - Delete: the Sandbox object is deleted once its underlying resources are removed.
	// This field has no effect while shutdownTime is unset, since the Sandbox never expires.
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

	// service is the name of the headless Service created for this Sandbox. It is empty
	// when no Service exists for the Sandbox (for example when spec.service is false, or
	// unset with no pre-existing Service). See serviceFQDN for the fully qualified
	// in-cluster DNS name of this Service.
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
	// This field is populated only while a backing pod exists. It is cleared whenever
	// the pod is absent, for example when the Sandbox is suspended
	// (operatingMode: Suspended) or before the pod has been created.
	// +optional
	PodIPs []string `json:"podIPs,omitempty"`

	// nodeName is the name of the node where the underlying pod is scheduled.
	// Like podIPs, it is cleared whenever the pod is absent (e.g. while suspended).
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
