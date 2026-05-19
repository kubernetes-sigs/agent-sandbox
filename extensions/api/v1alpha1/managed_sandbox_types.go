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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionType is a type of condition for a resource.
type ConditionType string

func (c ConditionType) String() string { return string(c) }

const (

	// ManagedSandboxConditionReady indicates readiness for Sandbox.
	ManagedSandboxConditionReady ConditionType = "Ready"

	// ManagedSandboxConditionFinished indicates the backing Pod reached a terminal phase.
	ManagedSandboxConditionFinished ConditionType = "Finished"
	// ManagedSandboxConditionCreated indicates whether the pod-agent has
	// successfully created the in-pod sandbox for this CR. Used by the
	// controller's CreateSandbox retry budget — `LastTransitionTime`
	// records the first failure and is compared against the budget.
	// Status=True once `CreateSandbox` succeeds.
	ManagedSandboxConditionCreated ConditionType = "Created"
)

// ManagedSandboxSpec defines the desired state of Sandbox.
type ManagedSandboxSpec struct {

	// image, when set, selects multi-tenant mode: the sandbox runs as a
	// bubblewrap-isolated tenant inside a shared pool pod whose base rootfs
	// is mounted from this OCI image reference.
	// +optional
	Image *SandboxImage `json:"image,omitempty"`

	// workspace controls how per-sandbox persistent state is mounted inside
	// the bubblewrap tenant. Ignored unless image is set.
	// +optional
	Workspace *SandboxWorkspace `json:"workspace,omitempty"`

	// Lifecycle defines when and how the sandbox should be shut down.
	// +optional
	Lifecycle `json:",inline"`

	// replicas is the number of desired replicas.
	// The only allowed values are 0 and 1.
	// Defaults to 1.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// service controls whether the controller should automatically create a
	// headless Service for this Sandbox.
	// When unset, the controller preserves existing Services for backward
	// compatibility but does not create new ones. Set to true to enable or false
	// to explicitly disable and remove the Service.
	//nolint:kubeapilinter
	//nolint:nobools // Enum not used to avoid duplicating the Service API; field is not expected to extend (issue #746).
	// +optional
	Service *bool `json:"service,omitempty"`
}

// ManagedSandboxImage selects the base OCI image whose rootfs becomes the lower
// layer of the bubblewrap overlay for a multi-tenant sandbox.
type SandboxImage struct {
	// reference is the OCI image reference (e.g. registry/repo:tag or @sha256:...).
	// The pool pod mounts this image read-only via the Kubernetes image volume
	// source (requires Kubernetes >= 1.33).
	// +required
	Reference string `json:"reference"`

	// pullPolicy mirrors corev1.PullPolicy for the image volume mount.
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// ManagedSandboxWorkspace describes per-sandbox persistent state inside the bwrap tenant.
type SandboxWorkspace struct {
	// mountPath is the path inside the sandbox where persistent state is mounted
	// as an overlay (or bind) source. Defaults to "/home".
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// size is the requested size for the per-sandbox persistent subpath.
	// Mapped to a subdirectory quota on the shared pool PVC. Optional; if unset,
	// no quota is enforced.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
}

// ManagedSandboxStatus defines the observed state of ManagedSandbox.
type ManagedSandboxStatus struct {
	// conditions defines the status conditions array
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// host records the pool pod that hosts this sandbox in multi-tenant mode.
	// Empty in legacy (podTemplate) mode.
	// +optional
	Host *SandboxHost `json:"host,omitempty"`

	// endpoints are the externally addressable URLs for this sandbox, populated
	// after the controller reconciles HTTPRoute(s) for it.
	// +optional
	// +listType=atomic
	Endpoints []SandboxEndpoint `json:"endpoints,omitempty"`

	// sshHost is the host to which an SSH client should connect for this
	// ManagedSandbox (an IP or a DNS name; always literal, never includes a port).
	// +optional
	SSHHost string `json:"sshHost,omitempty"`

	// sshPort is the TCP port on which the pod-agent's SSH server listens.
	// +optional
	SSHPort int32 `json:"sshPort,omitempty"`

	// sshSecretName names a Secret in the Sandbox's namespace that holds
	// the SSH session token under key "token". Owned by the Sandbox.
	// +optional
	SSHSecretName string `json:"sshSecretName,omitempty"`
}

// ManagedSandboxHost records the pool pod and persistent volume binding for a
// multi-tenant sandbox.
type SandboxHost struct {
	// podName is the name of the pool pod that runs the bubblewrap tenant.
	// +optional
	PodName string `json:"podName,omitempty"`

	// podUID is the UID of the pool pod, used to detect pod replacement.
	// +optional
	PodUID string `json:"podUID,omitempty"`

	// nodeName is the node the pool pod is scheduled on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// pvcName is the persistent volume claim that backs the pool pod's
	// /var/lib/sandboxes directory.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// pvUID is the UID of the bound PersistentVolume.
	// +optional
	PVUID string `json:"pvUID,omitempty"`
}

// ManagedSandboxEndpoint is one externally addressable URL exposed for a sandbox.
type SandboxEndpoint struct {
	// name distinguishes endpoints (e.g. "http", "ssh").
	// +required
	Name string `json:"name"`

	// url is the externally addressable URL.
	// +required
	URL string `json:"url"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:scope=Namespaced,shortName=sandbox
// ManagedSandbox is the Schema for the sandboxes API.
type ManagedSandbox struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Sandbox
	// +required
	Spec ManagedSandboxSpec `json:"spec"`

	// status defines the observed state of Sandbox
	// +optional
	Status ManagedSandboxStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ManagedSandboxList contains a list of ManagedSandbox.
type ManagedSandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedSandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ManagedSandbox{}, &ManagedSandboxList{})
}
