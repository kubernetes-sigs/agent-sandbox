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
	// SandboxConditionReady indicates whether the Sandbox is ready to serve traffic.
	// The condition is True when both the backing Pod and the headless Service exist and
	// are healthy, OR when spec.replicas is 0 (in which case having no Pod is the intended
	// state and is not treated as an error).
	// The condition is False with reason ReconcilerError when the controller encounters an
	// error managing child resources, or with reason SandboxExpired when spec.shutdownTime
	// has passed.
	SandboxConditionReady ConditionType = "Ready"

	// SandboxConditionFinished indicates the backing Pod has reached a terminal phase.
	// The condition is True with reason PodSucceeded when the Pod phase is Succeeded,
	// or with reason PodFailed when the Pod phase is Failed.
	// This condition is absent while the Pod is still running.
	SandboxConditionFinished ConditionType = "Finished"

	// SandboxReasonExpired indicates the Sandbox has passed its spec.shutdownTime.
	SandboxReasonExpired = "SandboxExpired"
	// SandboxReasonPodSucceeded indicates the backing Pod completed successfully (phase=Succeeded).
	SandboxReasonPodSucceeded = "PodSucceeded"
	// SandboxReasonPodFailed indicates the backing Pod completed unsuccessfully (phase=Failed).
	SandboxReasonPodFailed = "PodFailed"

	// SandboxPodNameAnnotation is set on the Sandbox to record the name of its backing Pod.
	// This is necessary because the Pod name may differ from the Sandbox name when a
	// pre-warmed Pod is adopted from a SandboxWarmPool. The controller uses this annotation
	// on every reconcile to locate the correct Pod. If the annotated Pod no longer exists,
	// the annotation is cleared and the controller falls back to using the Sandbox name.
	// Do not set or modify this annotation manually.
	SandboxPodNameAnnotation = "agents.x-k8s.io/pod-name"

	// SandboxTemplateRefAnnotation is set on a Sandbox to record the name of the
	// SandboxTemplate it was created from. Used by the extensions controllers to
	// track template provenance.
	SandboxTemplateRefAnnotation = "agents.x-k8s.io/sandbox-template-ref"

	// SandboxPodTemplateHashLabel is set on Sandbox Pods to record a hash of the
	// podTemplate spec at the time the Pod was created. The SandboxWarmPool controller
	// compares this hash against the current template to detect stale pool pods and
	// decide whether to replace them according to the pool's updateStrategy.
	SandboxPodTemplateHashLabel = "agents.x-k8s.io/sandbox-pod-template-hash"

	// SandboxPropagatedLabelsAnnotation is set on the Pod by the controller to track
	// which label keys were propagated from spec.podTemplate.metadata.labels. Its value
	// is a sorted, comma-separated list of those keys. The controller uses this on every
	// reconcile to remove labels that have since been deleted from the podTemplate, while
	// leaving labels added by other sources (e.g. mutating webhooks) untouched.
	// Do not set or modify this annotation manually.
	SandboxPropagatedLabelsAnnotation = "agents.x-k8s.io/propagated-labels"

	// SandboxPropagatedAnnotationsAnnotation is set on the Pod by the controller to track
	// which annotation keys were propagated from spec.podTemplate.metadata.annotations.
	// Its value is a sorted, comma-separated list of those keys. The controller uses this
	// on every reconcile to remove annotations that have since been deleted from the
	// podTemplate, while leaving annotations added by other sources untouched.
	// Do not set or modify this annotation manually.
	SandboxPropagatedAnnotationsAnnotation = "agents.x-k8s.io/propagated-annotations"
)

// PodMetadata holds the labels and annotations to propagate to the sandbox Pod.
// Only labels and annotations are supported; other ObjectMeta fields are ignored.
//
// Changes to these fields are applied to the running Pod immediately via an in-place
// metadata update. The container is not restarted. Deleted keys are also removed from
// the Pod. Keys that were added to the Pod by external sources (e.g. mutating webhooks)
// are tracked separately and are never removed by the controller.
type PodMetadata struct {
	// labels to propagate to the sandbox Pod. Merged with controller-managed labels
	// already on the Pod. Changes are applied immediately to a running Pod without
	// restarting the container. Removing a key here also removes it from the Pod.
	// Labels set by external sources (e.g. mutating webhooks) are not affected.
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`

	// annotations to propagate to the sandbox Pod. Merged with controller-managed
	// annotations already on the Pod. Changes are applied immediately to a running Pod
	// without restarting the container. Removing a key here also removes it from the Pod.
	// Annotations set by external sources are not affected.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

// EmbeddedObjectMetadata holds the metadata fields used in PVC templates.
// Only name, labels, and annotations are used; other ObjectMeta fields are ignored.
type EmbeddedObjectMetadata struct {
	// name is used to construct the PVC name as {name}-{sandbox.name}.
	// For example, a template with name "data" in a Sandbox named "my-sandbox"
	// creates a PVC named "data-my-sandbox". Cannot be updated after the PVC
	// has been created.
	// +optional
	Name string `json:"name,omitempty" protobuf:"bytes,1,opt,name=name"`

	// labels to apply to the created PVC. Set at PVC creation time only.
	// Changes to this field have no effect on already-existing PVCs.
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,1,rep,name=labels"`

	// annotations to apply to the created PVC. Set at PVC creation time only.
	// Changes to this field have no effect on already-existing PVCs.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,2,rep,name=annotations"`
}

// PodTemplate defines the template for the sandbox Pod.
//
// The two sub-fields have intentionally different update semantics once a Pod is running:
//
//   - metadata (labels and annotations): propagated to the running Pod immediately via
//     an in-place update. The container is NOT restarted.
//
//   - spec: NOT applied to a running Pod. The new spec only takes effect after the
//     existing Pod is deleted. To apply a spec change, delete the Pod using the
//     selector in .status.selector or the name in the agents.x-k8s.io/pod-name
//     annotation on the Sandbox. The controller immediately recreates it.
type PodTemplate struct {
	// spec defines the desired state of the sandbox Pod.
	//
	// This field is used only when the Pod is first created or recreated after deletion.
	// Changes to this field have NO effect on a running Pod — the existing Pod continues
	// running with the spec it was originally created with.
	//
	// To apply a spec change to a running Sandbox, delete the backing Pod. The controller
	// will recreate it immediately using the updated spec. Use .status.selector or the
	// agents.x-k8s.io/pod-name annotation to identify the Pod — its name may differ from
	// the Sandbox name when a WarmPool pod was adopted.
	//
	// Volume priority: if a volume name in this spec matches a name in
	// spec.volumeClaimTemplates, the PVC-backed volume takes priority and replaces
	// the inline volume definition when the Pod is created.
	// +required
	Spec corev1.PodSpec `json:"spec" protobuf:"bytes,3,opt,name=spec"`

	// metadata defines labels and annotations to propagate to the sandbox Pod.
	// Unlike spec, changes to metadata are applied immediately to a running Pod
	// without restarting the container. Removed keys are cleaned up from the Pod.
	// Keys set by external sources (e.g. mutating webhooks) are not affected.
	// +optional
	ObjectMeta PodMetadata `json:"metadata" protobuf:"bytes,3,opt,name=metadata"`
}

// PersistentVolumeClaimTemplate defines the template for a PVC that the controller
// creates and manages on behalf of the Sandbox.
//
// PVC naming: the created PVC is named {metadata.name}-{sandbox.name}.
//
// Each entry drives PVC creation only — the controller never updates or deletes PVCs:
//   - Adding an entry provisions the PVC immediately but it is not mounted into the
//     running Pod until the Pod is next recreated.
//   - Removing an entry stops the volume from being injected into future Pods but does
//     NOT delete the existing PVC. The orphaned PVC must be deleted manually.
//   - Modifying an entry (e.g. storage class, capacity) has no effect on an
//     already-existing PVC since PVC specs are largely immutable after binding.
//   - If a PVC with the expected name exists but has no ownerReference, the controller
//     adopts it rather than creating a new one.
type PersistentVolumeClaimTemplate struct {
	// metadata for the PVC. The name field determines the PVC name as
	// {name}-{sandbox.name}. Labels and annotations are applied at creation time only
	// and have no effect on already-existing PVCs.
	// +optional
	EmbeddedObjectMetadata `json:"metadata" protobuf:"bytes,3,opt,name=metadata"`

	// spec defines the desired characteristics of the PVC (storage class, access modes,
	// capacity, etc.). Used only when the PVC is first created. Changes after the PVC
	// exists are silently ignored, as PVC specs are largely immutable after binding.
	// Every entry must have at least one access mode supported by the storage provisioner.
	// +required
	Spec corev1.PersistentVolumeClaimSpec `json:"spec" protobuf:"bytes,3,opt,name=spec"`
}

// SandboxSpec defines the desired state of Sandbox.
type SandboxSpec struct {
	// podTemplate describes the Pod that the controller will create for this Sandbox.
	//
	// podTemplate.metadata (labels/annotations): changes are propagated immediately to
	// the running Pod without restarting the container. Removed keys are also cleaned up.
	// Keys added by external sources (e.g. mutating webhooks) are not affected.
	//
	// podTemplate.spec: changes do NOT affect a running Pod. The updated spec is only
	// used when the Pod is next created or recreated. To apply a spec change, delete the
	// Pod identified by .status.selector or the agents.x-k8s.io/pod-name annotation.
	// Do not assume the Pod name matches the Sandbox name — a WarmPool-adopted Pod keeps
	// its original name.
	// +required
	PodTemplate PodTemplate `json:"podTemplate" protobuf:"bytes,3,opt,name=podTemplate"`

	// volumeClaimTemplates is a list of PVC templates. For each entry the controller
	// creates one PVC named {template.metadata.name}-{sandbox.name} before creating the
	// Pod. These PVCs are automatically injected as volumes into the Pod. If a volume
	// name in podTemplate.spec.volumes matches a template name here, the PVC-backed
	// volume takes priority and silently replaces the inline definition.
	//
	// Scale-down / scale-up: when replicas is set to 0 the Pod is deleted but all PVCs
	// are preserved. When replicas is set back to 1 the same PVCs are re-attached to the
	// new Pod, restoring the full filesystem context in every mounted path. This is the
	// primary mechanism for hibernating and resuming an agent while retaining its state.
	//
	// Mutability after creation:
	//   - Adding an entry: the PVC is provisioned immediately but is not mounted until
	//     the Pod is next recreated.
	//   - Removing an entry: the PVC is NOT deleted. It will no longer be mounted in
	//     future Pods. Delete it manually to reclaim storage.
	//   - Modifying an entry: ignored for existing PVCs (PVC specs are immutable after
	//     binding). Only affects newly created PVCs.
	//   - Unowned PVCs with a matching name are adopted by the controller.
	// +optional
	// +listType=atomic
	VolumeClaimTemplates []PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty" protobuf:"bytes,4,rep,name=volumeClaimTemplates"`

	// Lifecycle defines when and how the sandbox should be shut down.
	// +optional
	Lifecycle `json:",inline"`

	// replicas is the desired number of running Pod replicas. Only 0 and 1 are allowed.
	// Defaults to 1.
	//
	// replicas: 1 (default)
	// The controller ensures a Pod and a headless Service exist. If neither exists they
	// are created from podTemplate and the headless Service spec respectively. The Sandbox
	// Ready condition reflects the health of both resources.
	//
	// replicas: 0
	// The controller deletes the owned Pod. The headless Service is kept. All PVCs from
	// volumeClaimTemplates are preserved — their data is retained. While replicas is 0
	// the Sandbox Ready condition is set to True because the absence of a Pod is the
	// intended state, not an error. If the Pod already has a DeletionTimestamp the
	// controller waits for it to terminate rather than re-issuing a delete.
	//
	// Restoring to replicas: 1 after scale-down
	// The controller creates a new Pod and re-attaches all existing PVCs. The filesystem
	// context in every mounted path is fully restored, allowing an agent to resume exactly
	// where it left off. This makes replicas a lightweight save/restore mechanism for
	// agent filesystem state. Future enhancements (memory snapshots, root filesystem
	// archival) are planned to complement this.
	//
	// Ownership: the controller only deletes a Pod it owns. If the Pod is owned by a
	// different controller or has no owner, the delete is skipped and a warning is logged.
	// The Sandbox may still report Ready=True because the intended replica count is 0.
	//
	// This field is also exposed via the /scale subresource, so kubectl scale and
	// horizontal pod autoscalers work against it.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// ShutdownPolicy describes the policy for the Sandbox object itself when it expires.
// In both cases the backing Pod and Service are always deleted on expiry.
// PVCs are never deleted on expiry and must be cleaned up manually.
// +kubebuilder:validation:Enum=Delete;Retain
type ShutdownPolicy string

const (
	// ShutdownPolicyDelete deletes the Sandbox object itself after the Pod and Service
	// are removed on expiry. PVCs are not deleted and must be cleaned up manually.
	ShutdownPolicyDelete ShutdownPolicy = "Delete"

	// ShutdownPolicyRetain keeps the Sandbox object when it expires. The backing Pod
	// and Service are still deleted. The Sandbox Ready condition is set to False with
	// reason SandboxExpired so the expiry is visible in the resource status.
	// PVCs are not deleted and must be cleaned up manually.
	ShutdownPolicyRetain ShutdownPolicy = "Retain"
)

// Lifecycle defines the expiry and shutdown behavior for the Sandbox.
type Lifecycle struct {
	// shutdownTime is the absolute UTC time at which the Sandbox expires.
	// When this time is reached the controller deletes the backing Pod and Service.
	// PVCs from volumeClaimTemplates are never deleted on expiry; they must be cleaned
	// up manually. The Sandbox object is deleted or retained based on shutdownPolicy.
	// Once expired the Ready condition is set to False with reason SandboxExpired and
	// live-resource status fields (podIPs, replicas) are cleared.
	// +kubebuilder:validation:Format="date-time"
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// shutdownPolicy controls whether the Sandbox object itself is deleted on expiry.
	// The backing Pod and Service are always deleted regardless of this setting.
	// PVCs are never deleted on expiry and must be cleaned up manually.
	//
	// Delete: the Sandbox object is deleted after child resources are removed.
	// Retain (default): the Sandbox object is kept. Its Ready condition is set to False
	// with reason SandboxExpired, making the expiry visible in the resource status.
	// +kubebuilder:default=Retain
	// +optional
	ShutdownPolicy *ShutdownPolicy `json:"shutdownPolicy,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// serviceFQDN is the fully-qualified domain name of the headless Service for this
	// Sandbox, in the form {name}.{namespace}.svc.{clusterDomain}. The cluster domain
	// defaults to cluster.local and can be overridden via the controller's
	// --cluster-domain flag. This field is empty when the Service does not exist.
	// +optional
	ServiceFQDN string `json:"serviceFQDN,omitempty"`

	// service is the name of the headless Service created for this Sandbox. The Service
	// selects the backing Pod using an internal name-hash label and provides a stable DNS
	// name regardless of Pod restarts. Empty when the Service does not exist.
	// +optional
	Service string `json:"service,omitempty"`

	// conditions is the list of status conditions for this Sandbox.
	//
	// Ready=True: Pod is running and ready and the Service exists, OR replicas is 0.
	// Ready=False/ReconcilerError: controller encountered an error managing child resources.
	// Ready=False/SandboxExpired: shutdownTime has passed.
	//
	// Finished=True/PodSucceeded: backing Pod phase is Succeeded.
	// Finished=True/PodFailed: backing Pod phase is Failed.
	// Finished is absent while the Pod is running or does not exist.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// replicas is the number of currently running Pod replicas (0 or 1). Reflects the
	// observed state. It is 0 whenever the Pod does not exist, including during
	// termination, before the first Pod is created, or while spec.replicas is 0.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// selector is the label selector that identifies the Pod(s) belonging to this Sandbox.
	// Because the Pod name may differ from the Sandbox name (e.g. after WarmPool adoption),
	// this is the reliable way to locate the backing Pod. Use it with kubectl to trigger
	// a Pod recreation and apply podTemplate.spec changes:
	//
	//   kubectl delete pod -l <selector> -n <namespace>
	//
	// Empty when spec.replicas is 0.
	// +optional
	LabelSelector string `json:"selector,omitempty"`

	// podIPs are the IP addresses of the backing Pod. A Pod may have more than one IP in
	// dual-stack clusters (one IPv4, one IPv6). Empty when no Pod exists or when the Pod
	// has not yet been assigned an IP address.
	// +optional
	PodIPs []string `json:"podIPs,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:scope=Namespaced,shortName=sandbox
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
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
