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
	intstr "k8s.io/apimachinery/pkg/util/intstr"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// Important: Run "make" to regenerate code after modifying this file

// SandboxSpec defines the desired state of Sandbox
type SandboxSpec struct {
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// PodTemplate describes the pod spec that will be used to create an agent sandbox.
	// +kubebuilder:validation:Required
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate" protobuf:"bytes,3,opt,name=podTemplate"`

	// Networking describes optional external exposure and/or references to
	// externally managed routes for this sandbox. If omitted, only the default
	// headless Service (for internal discovery) is used.
	// +optional
	Networking *NetworkingSpec `json:"networking,omitempty"`

	// Pause, when true, indicates the sandbox should be stopped.
	// The controller ensures no Pod is running, while preserving the Sandbox
	// object and persistent state. When false, the controller ensures the Pod
	// is running as specified by PodTemplate.
	// +optional
	Pause *bool `json:"pause,omitempty"`

	// Schedule provides time-based lifecycle controls for the sandbox.
	// +optional
	Schedule *Schedule `json:"schedule,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// Conditions represent the latest available observations of the sandbox's state.
	// Common conditions may include Ready, Stopped, Resuming, Scheduled, NetworkingReady.
	// +optional
	Conditions []Condition `json:"conditions,omitempty"`

	// Networking exposes observed reachability information, such as URLs, IPs
	// and ports, derived from Services/Routes referenced by spec.networking.
	// +optional
	Networking []NetworkingStatus `json:"networking,omitempty"`
}

// Schedule defines time-based lifecycle behavior for a sandbox.
type Schedule struct {
	// ShutdownTime is the time to stop the sandbox, in RFC3339 format
	// (e.g. 2025-07-01T00:00:00Z). When reached, the controller should ensure
	// the sandbox is paused.
	// +kubebuilder:validation:Format=date-time
	// +optional
	ShutdownTime string `json:"shutdownTime,omitempty"`
}

// Condition is a wrapper around metav1.Condition for CRD compatibility and
// future extension.
type Condition struct {
	metav1.Condition `json:",inline"`
}

// NetworkingSpec describes optional exposure configuration and external route refs.
type NetworkingSpec struct {
	// Service describes an optional Service to create for exposure.
	// If omitted, only the internal headless Service is maintained for discovery.
	// +optional
	Service *ServiceSpec `json:"service,omitempty"`

	// RouteRefs references externally managed routing resources (e.g., Gateway API
	// HTTPRoute/TCPRoute or Ingress) that target the sandbox Service.
	// The controller may read these for observability but does not create them.
	// +optional
	RouteRefs []RouteRef `json:"routeRefs,omitempty"`
}

// ServiceSpec is a minimal, opinion-neutral subset to describe a Service exposure.
type ServiceSpec struct {
	// Type determines exposure level. ClusterIP for in-cluster, NodePort/LoadBalancer for external.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`

	// Ports to expose on the Service.
	// +optional
	Ports []ServicePort `json:"ports,omitempty"`
}

// ServicePort describes a single Service port mapping.
type ServicePort struct {
	// Name should align with the container port name when TargetPort is omitted.
	// +optional
	Name string `json:"name,omitempty"`

	// Port is the Service port.
	Port int32 `json:"port"`

	// TargetPort is the target Pod port. If omitted and Name is set, the controller
	// may resolve the container port by name.
	// +optional
	TargetPort intstr.IntOrString `json:"targetPort,omitempty"`

	// Protocol defaults to TCP.
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// RouteRef references an external routing resource in the same namespace.
type RouteRef struct {
	// Kind of the route resource (e.g., HTTPRoute, TCPRoute, Ingress).
	Kind string `json:"kind"`
	// Name of the referenced route resource.
	Name string `json:"name"`
}

// NetworkingStatus captures observed reachability information.
type NetworkingStatus struct {
	// Name associates with a port/name when applicable.
	// +optional
	Name string `json:"name,omitempty"`
	// URL is an externally reachable URL if known (e.g., from a Route/Ingress).
	// +optional
	URL string `json:"url,omitempty"`
	// IP is an external IP if assigned (e.g., LoadBalancer ingress).
	// +optional
	IP string `json:"ip,omitempty"`
	// Port is the externally reachable port.
	// +optional
	Port int32 `json:"port,omitempty"`
	// Ready indicates whether networking is considered ready.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// Message provides human-readable details.
	// +optional
	Message string `json:"message,omitempty"`
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
