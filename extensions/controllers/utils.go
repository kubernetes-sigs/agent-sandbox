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

package controllers

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

// ApplySandboxSecureDefaults applies the controller's "Secure by Default" logic to a PodSpec.
func ApplySandboxSecureDefaults(template *extensionsv1alpha1.SandboxTemplate, spec *corev1.PodSpec) {
	// Enforce a secure-by-default policy by disabling the automatic mounting
	// of the service account token, adhering to security best practices for
	// sandboxed environments.
	if spec.AutomountServiceAccountToken == nil {
		automount := false
		spec.AutomountServiceAccountToken = &automount
	}

	// Determine if we are in "Secure By Default" mode
	management := template.Spec.NetworkPolicyManagement
	isManaged := management == "" || management == extensionsv1alpha1.NetworkPolicyManagementManaged
	isSecureByDefault := isManaged && template.Spec.NetworkPolicy == nil

	// To prevent internal DNS enumeration while still allowing public domain resolution,
	// we explicitly override the Pod's DNS config to use external public resolvers.
	// We only inject this if using the strict "Secure by Default" policy. If the user
	// provides custom rules or is Unmanaged, we leave DNS alone for air-gapped/proxy compatibility.
	if isSecureByDefault && spec.DNSPolicy == "" {
		spec.DNSPolicy = corev1.DNSNone
		spec.DNSConfig = &corev1.PodDNSConfig{
			Nameservers: []string{"8.8.8.8", "1.1.1.1"}, // Google & Cloudflare public DNS
		}
	}
}

// computePodTemplateHash computes a hash of the sandbox template's Spec.PodTemplate.
func computePodTemplateHash(template *extensionsv1alpha1.SandboxTemplate) (string, error) {
	specJSON, err := json.Marshal(template.Spec.PodTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to marshal pod template for hashing: %w", err)
	}
	return sandboxcontrollers.NameHash(string(specJSON)), nil
}
