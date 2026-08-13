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

import (
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/conversion"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	v1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const v1alpha1SandboxTemplateStateAnnotation = "api.agents.x-k8s.io/v1alpha1-sandboxtemplate-state"

// ConvertTo converts this SandboxTemplate to the Hub version (v1beta1).
func (s *SandboxTemplate) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.SandboxTemplate)

	// Copy object metadata
	s.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)

	// Convert Spec
	if err := convertTemplateSpecTo(&s.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("convert spec to v1beta1: %w", err)
	}

	// Restore v1beta1-only VolumeClaimTemplatesPolicy if present in annotations
	if policy, ok := s.Annotations["api.agents.x-k8s.io/v1beta1-volume-claim-templates-policy"]; ok {
		switch v1beta1.VolumeClaimTemplatesPolicy(policy) {
		case v1beta1.VolumeClaimTemplatesPolicyDisallowed, v1beta1.VolumeClaimTemplatesPolicyAllowed, v1beta1.VolumeClaimTemplatesPolicyOverrides:
			dst.Spec.VolumeClaimTemplatesPolicy = v1beta1.VolumeClaimTemplatesPolicy(policy)
		default:
			return fmt.Errorf("invalid VolumeClaimTemplatesPolicy annotation value: %q", policy)
		}
		if dst.Annotations != nil {
			delete(dst.Annotations, "api.agents.x-k8s.io/v1beta1-volume-claim-templates-policy")
		}
	}

	// Preserve the original v1alpha1 object state for lossless round-tripping
	if dst.Annotations == nil {
		dst.Annotations = make(map[string]string)
	}
	sCopy := s.DeepCopy()
	if sCopy.Annotations != nil {
		delete(sCopy.Annotations, v1alpha1SandboxTemplateStateAnnotation)
	}
	stateJSON, err := json.Marshal(sCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal v1alpha1 SandboxTemplate state: %w", err)
	}
	dst.Annotations[v1alpha1SandboxTemplateStateAnnotation] = string(stateJSON)

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this SandboxTemplate.
func (s *SandboxTemplate) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.SandboxTemplate)

	// Copy object metadata
	src.ObjectMeta.DeepCopyInto(&s.ObjectMeta)

	// Convert Spec
	if err := convertTemplateSpecFrom(&src.Spec, &s.Spec); err != nil {
		return fmt.Errorf("convert spec from v1beta1: %w", err)
	}

	// Strip the state annotation if present so it doesn't leak to clients and get sent back on updates
	delete(s.Annotations, v1alpha1SandboxTemplateStateAnnotation)

	// Preserve v1beta1-only VolumeClaimTemplatesPolicy for round-tripping
	if src.Spec.VolumeClaimTemplatesPolicy != "" {
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		s.Annotations["api.agents.x-k8s.io/v1beta1-volume-claim-templates-policy"] = string(src.Spec.VolumeClaimTemplatesPolicy)
	} else if s.Annotations != nil {
		delete(s.Annotations, "api.agents.x-k8s.io/v1beta1-volume-claim-templates-policy")
	}

	return nil
}

// Helper functions for SandboxTemplate conversion

func convertTemplateSpecTo(src *SandboxTemplateSpec, dst *v1beta1.SandboxTemplateSpec) error {
	// PodTemplate
	if err := sandboxv1alpha1.ConvertPodTemplateTo(&src.PodTemplate, &dst.PodTemplate); err != nil {
		return fmt.Errorf("convert podTemplate to v1beta1: %w", err)
	}

	// VolumeClaimTemplates
	if src.VolumeClaimTemplates != nil {
		dst.VolumeClaimTemplates = make([]sandboxv1beta1.PersistentVolumeClaimTemplate, len(src.VolumeClaimTemplates))
		for i := range src.VolumeClaimTemplates {
			if err := sandboxv1alpha1.ConvertPVCClaimTemplateTo(&src.VolumeClaimTemplates[i], &dst.VolumeClaimTemplates[i]); err != nil {
				return fmt.Errorf("convert volumeClaimTemplates[%d] to v1beta1: %w", i, err)
			}
		}
	} else {
		dst.VolumeClaimTemplates = nil
	}

	// NetworkPolicy
	if src.NetworkPolicy != nil {
		dst.NetworkPolicy = &v1beta1.NetworkPolicySpec{
			Ingress: src.NetworkPolicy.Ingress,
			Egress:  src.NetworkPolicy.Egress,
		}
	} else {
		dst.NetworkPolicy = nil
	}

	// NetworkPolicyManagement
	dst.NetworkPolicyManagement = v1beta1.NetworkPolicyManagement(src.NetworkPolicyManagement)

	// EnvVarsInjectionPolicy
	dst.EnvVarsInjectionPolicy = v1beta1.EnvVarsInjectionPolicy(src.EnvVarsInjectionPolicy)

	// Service
	dst.Service = src.Service

	return nil
}

func convertTemplateSpecFrom(src *v1beta1.SandboxTemplateSpec, dst *SandboxTemplateSpec) error {
	// PodTemplate
	if err := sandboxv1alpha1.ConvertPodTemplateFrom(&src.PodTemplate, &dst.PodTemplate); err != nil {
		return fmt.Errorf("convert podTemplate from v1beta1: %w", err)
	}

	// VolumeClaimTemplates
	if src.VolumeClaimTemplates != nil {
		dst.VolumeClaimTemplates = make([]sandboxv1alpha1.PersistentVolumeClaimTemplate, len(src.VolumeClaimTemplates))
		for i := range src.VolumeClaimTemplates {
			if err := sandboxv1alpha1.ConvertPVCClaimTemplateFrom(&src.VolumeClaimTemplates[i], &dst.VolumeClaimTemplates[i]); err != nil {
				return fmt.Errorf("convert volumeClaimTemplates[%d] from v1beta1: %w", i, err)
			}
		}
	} else {
		dst.VolumeClaimTemplates = nil
	}

	// NetworkPolicy
	if src.NetworkPolicy != nil {
		dst.NetworkPolicy = &NetworkPolicySpec{
			Ingress: src.NetworkPolicy.Ingress,
			Egress:  src.NetworkPolicy.Egress,
		}
	} else {
		dst.NetworkPolicy = nil
	}

	// NetworkPolicyManagement
	dst.NetworkPolicyManagement = NetworkPolicyManagement(src.NetworkPolicyManagement)

	// EnvVarsInjectionPolicy
	dst.EnvVarsInjectionPolicy = EnvVarsInjectionPolicy(src.EnvVarsInjectionPolicy)

	// Service
	dst.Service = src.Service

	return nil
}
