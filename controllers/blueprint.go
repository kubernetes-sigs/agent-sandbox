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
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// BlueprintHash is the canonical content hash of a SandboxBlueprint: the
// NameHash of its JSON encoding. It is the value tracked in the
// SandboxTemplateHashLabel on sandboxes and pinned in spec.blueprintRef, so
// every producer and consumer of blueprint identity must use this function.
func BlueprintHash(blueprint *sandboxv1beta1.SandboxBlueprint) (string, error) {
	specJSON, err := json.Marshal(blueprint)
	if err != nil {
		return "", fmt.Errorf("failed to marshal sandbox blueprint for hashing: %w", err)
	}
	return NameHash(string(specJSON)), nil
}

// ApplySandboxSecureDefaults applies the controller's "Secure by Default" logic to a PodSpec.
func ApplySandboxSecureDefaults(template *extensionsv1beta1.SandboxTemplate, spec *corev1.PodSpec) {
	// Enforce a secure-by-default policy by disabling the automatic mounting
	// of the service account token, adhering to security best practices for
	// sandboxed environments.
	if spec.AutomountServiceAccountToken == nil {
		automount := false
		spec.AutomountServiceAccountToken = &automount
	}

	// Determine if we are in "Secure By Default" mode
	management := template.Spec.NetworkPolicyManagement
	isManaged := management == "" || management == extensionsv1beta1.NetworkPolicyManagementManaged
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

// resolveBlueprintRef hydrates a blueprintRef Sandbox's blueprint fields
// (podTemplate.spec, volumeClaimTemplates, service) in memory from its
// SandboxTemplate, applying the same secure defaults the extensions
// controllers apply when they embed a blueprint. The hydrated fields exist
// only on this reconcile's in-memory object: every write the reconciler
// issues against the Sandbox is a MergeFrom patch whose base is a post-
// hydration deep copy, so the resolved blueprint is never persisted.
//
// Resolution fails when the template is missing or its blueprint no longer
// hashes to the pinned blueprintHash: building a pod from a drifted template
// would silently produce a different workload than the one this Sandbox was
// created for. The caller decides how much of the reconcile that failure
// blocks (creation must be blocked; an already-running pod must not be).
//
// No-op for embedded-blueprint sandboxes (blueprintRef unset).
func (r *SandboxReconciler) resolveBlueprintRef(ctx context.Context, sandbox *sandboxv1beta1.Sandbox) error {
	ref := sandbox.Spec.BlueprintRef
	if ref == nil {
		return nil
	}

	template := &extensionsv1beta1.SandboxTemplate{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: ref.Name}, template); err != nil {
		return fmt.Errorf("resolve blueprintRef: get SandboxTemplate %q: %w", ref.Name, err)
	}
	if template.Spec.PodTemplate.Spec == nil {
		return fmt.Errorf("resolve blueprintRef: SandboxTemplate %q has no podTemplate.spec", ref.Name)
	}
	hash, err := BlueprintHash(&template.Spec.SandboxBlueprint)
	if err != nil {
		return fmt.Errorf("resolve blueprintRef: hash blueprint of SandboxTemplate %q: %w", ref.Name, err)
	}
	if hash != ref.BlueprintHash {
		return fmt.Errorf("resolve blueprintRef: SandboxTemplate %q blueprint hash %s does not match the Sandbox's pinned hash %s (template changed since the Sandbox was created)", ref.Name, hash, ref.BlueprintHash)
	}

	blueprint := template.Spec.SandboxBlueprint.DeepCopy()
	sandbox.Spec.PodTemplate.Spec = blueprint.PodTemplate.Spec
	sandbox.Spec.VolumeClaimTemplates = blueprint.VolumeClaimTemplates
	sandbox.Spec.Service = blueprint.Service
	ApplySandboxSecureDefaults(template, sandbox.Spec.PodTemplate.Spec)
	return nil
}

// blueprintUnresolved reports whether the sandbox declares a blueprintRef
// whose blueprint has not been hydrated onto the in-memory object.
func blueprintUnresolved(sandbox *sandboxv1beta1.Sandbox) bool {
	return sandbox.Spec.BlueprintRef != nil && sandbox.Spec.PodTemplate.Spec == nil
}
