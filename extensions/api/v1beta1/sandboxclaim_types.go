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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

const (
	// ClaimExpiredReason is the reason used in conditions/events when a claim expires.
	ClaimExpiredReason = "ClaimExpired"

	// DeprecatedAssignedSandboxNameLabel is the legacy label key applied to the claim to identify the adopted Sandbox name.
	// Deprecated: Use AssignedSandboxNameAnnotation instead.
	DeprecatedAssignedSandboxNameLabel = "agents.x-k8s.io/sandbox-name"

	// AssignedSandboxNameAnnotation is the annotation key applied to the claim to identify the adopted Sandbox Name.
	AssignedSandboxNameAnnotation = "agents.x-k8s.io/sandbox-name"

	// WarmPoolRefField is the field used for indexing SandboxClaims by their warm pool reference name.
	WarmPoolRefField = ".spec.warmPoolRef.name"
)

// ShutdownPolicy describes the policy for shutting down the underlying Sandbox when the SandboxClaim expires.
// +kubebuilder:validation:Enum=Delete;DeleteForeground;Retain
type ShutdownPolicy string

const (
	// ShutdownPolicyDelete deletes the SandboxClaim (and cascadingly the Sandbox) when expired.
	ShutdownPolicyDelete ShutdownPolicy = "Delete"

	// ShutdownPolicyDeleteForeground deletes the SandboxClaim when expired using foreground
	// cascade deletion. The claim remains in the API (with a deletionTimestamp) until its
	// underlying Sandbox and Pod are fully terminated. This allows external systems to observe
	// shutdown progress by checking whether the claim still exists.
	ShutdownPolicyDeleteForeground ShutdownPolicy = "DeleteForeground"

	// ShutdownPolicyRetain keeps the SandboxClaim when expired (Status will show Expired).
	// The underlying SandboxClaim resources (Sandbox, Pod, Service) are deleted to save resources,
	// but the SandboxClaim object itself remains.
	ShutdownPolicyRetain ShutdownPolicy = "Retain"
)

// Lifecycle defines the lifecycle management for the SandboxClaim.
type Lifecycle struct {
	// shutdownTime is the absolute time when the SandboxClaim expires.
	// This time governs the lifecycle of the claim. It is not propagated to the
	// underlying Sandbox. Instead, the SandboxClaim controller enforces this
	// expiration by deleting the Sandbox resources when the time is reached.
	// If this field is omitted or set to nil, the SandboxClaim itself won't expire.
	// This implies unsetting a Sandbox's ShutdownTime via SandboxClaim isn't supported.
	// +kubebuilder:validation:Format="date-time"
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// ttlSecondsAfterFinished limits how long a finished claim is retained.
	// The timer starts from the mirrored Finished condition's LastTransitionTime.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// shutdownPolicy determines the behavior when the SandboxClaim expires.
	// +kubebuilder:default=Retain
	// +optional
	ShutdownPolicy ShutdownPolicy `json:"shutdownPolicy,omitempty"`
}

// SandboxWarmPoolRef references a SandboxWarmPool.
type SandboxWarmPoolRef struct {
	// name of the SandboxWarmPool
	// +required
	Name string `json:"name"`
}

// EnvVar represents a custom environment variable key-value pair.
type EnvVar struct {
	// name of the environment variable.
	// +required
	Name string `json:"name"`

	// value of the environment variable.
	// +required
	Value string `json:"value"`

	// containerName specifies the target container for the environment variable.
	// If not specified, it defaults to the first container defined in the template.
	// +optional
	ContainerName string `json:"containerName,omitempty"`
}

// SandboxClaimSpec defines the desired state of Sandbox.
type SandboxClaimSpec struct {
	// warmPoolRef targets the specific pre-warmed infrastructure pool to check out from.
	// +required
	WarmPoolRef SandboxWarmPoolRef `json:"warmPoolRef"`

	// lifecycle defines when and how the SandboxClaim should be shut down.
	// +optional
	Lifecycle *Lifecycle `json:"lifecycle,omitempty"`

	// additionalPodMetadata defines the labels and annotations to be propagated to the Sandbox Pod.
	// Label values are limited to 63 characters and must match Kubernetes label value patterns.
	// +optional
	AdditionalPodMetadata sandboxv1beta1.PodMetadata `json:"additionalPodMetadata,omitempty"`

	// env is a list of environment variables to inject into the sandbox.
	// Please note adding this field means the Sandbox will always be cold-started from the
	// template of the warmpool.
	// +listType=atomic
	// +optional
	Env []EnvVar `json:"env,omitempty"`
}

// SandboxClaimStatus defines the observed state of Sandbox.
type SandboxClaimStatus struct {
	// conditions represent the latest available observations of a Sandbox's current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// sandbox defines the state of Sandbox
	// +optional
	SandboxStatus SandboxStatus `json:"sandbox,omitempty"`
}

type SandboxStatus struct {
	// name is the name of the Sandbox created from this claim
	// +optional
	Name string `json:"name,omitempty"`

	// podIPs are the IP addresses of the underlying pod.
	// A pod may have multiple IPs in dual-stack clusters.
	// +optional
	PodIPs []string `json:"podIPs,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sandboxclaim
// +kubebuilder:storageversion
// +kubebuilder:conversion:strategy=Webhook
// SandboxClaim is the Schema for the sandbox Claim API.
type SandboxClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Sandbox
	// +required
	Spec SandboxClaimSpec `json:"spec"`

	// status defines the observed state of Sandbox
	// +optional
	Status SandboxClaimStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// SandboxList contains a list of Sandbox.
type SandboxClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxClaim{}, &SandboxClaimList{})
}
