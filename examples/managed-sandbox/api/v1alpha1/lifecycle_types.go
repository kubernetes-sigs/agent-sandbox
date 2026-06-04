// Copyright 2026 The Kubernetes Authors.
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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ShutdownPolicy describes the policy for shutting down a managed sandbox.
// +kubebuilder:validation:Enum=Delete;Retain
type ShutdownPolicy string

const (
	// ShutdownPolicyDelete deletes backing resources when the sandbox expires.
	ShutdownPolicyDelete ShutdownPolicy = "Delete"

	// ShutdownPolicyRetain keeps the API object when the sandbox expires.
	ShutdownPolicyRetain ShutdownPolicy = "Retain"
)

// Lifecycle defines optional lifetime controls for the managed sandbox example.
type Lifecycle struct {
	// shutdownTime is the absolute time when the sandbox expires.
	// +kubebuilder:validation:Format="date-time"
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// ttlSecondsAfterFinished limits how long a finished sandbox is retained.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// shutdownPolicy determines the behavior when the sandbox expires.
	// +kubebuilder:default=Retain
	// +optional
	ShutdownPolicy ShutdownPolicy `json:"shutdownPolicy,omitempty"`
}
