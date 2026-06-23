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
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/conversion"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	v1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const v1alpha1SandboxClaimStateAnnotation = "api.agents.x-k8s.io/v1alpha1-sandboxclaim-state"

// ConvertTo converts this SandboxClaim to the Hub version (v1beta1).
func (s *SandboxClaim) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.SandboxClaim)

	// Copy object metadata
	s.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)

	// Convert Spec
	if err := convertClaimSpecTo(&s.Spec, &dst.Spec, s.Name, s.Status.SandboxStatus.Name); err != nil {
		return err
	}

	// Convert Status
	if err := convertClaimStatusTo(&s.Status, &dst.Status); err != nil {
		return err
	}

	// Preserve the original v1alpha1 object state for lossless round-tripping
	if dst.Annotations == nil {
		dst.Annotations = make(map[string]string)
	}
	sCopy := s.DeepCopy()
	if sCopy.Annotations != nil {
		delete(sCopy.Annotations, v1alpha1SandboxClaimStateAnnotation)
	}
	stateJSON, err := json.Marshal(sCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal v1alpha1 SandboxClaim state: %w", err)
	}
	dst.Annotations[v1alpha1SandboxClaimStateAnnotation] = string(stateJSON)

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this SandboxClaim.
func (s *SandboxClaim) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.SandboxClaim)

	// Copy object metadata
	src.ObjectMeta.DeepCopyInto(&s.ObjectMeta)

	// Convert Spec
	if err := convertClaimSpecFrom(&src.Spec, &s.Spec); err != nil {
		return err
	}

	// Convert Status
	if err := convertClaimStatusFrom(&src.Status, &s.Status); err != nil {
		return err
	}

	// Restore original v1alpha1 state if present to ensure lossless conversion
	if stateJSON, ok := s.Annotations[v1alpha1SandboxClaimStateAnnotation]; ok {
		// Strip the state annotation so it doesn't leak to clients and get sent back on updates
		delete(s.Annotations, v1alpha1SandboxClaimStateAnnotation)

		var original SandboxClaim
		if err := json.Unmarshal([]byte(stateJSON), &original); err != nil {
			return fmt.Errorf("failed to unmarshal v1alpha1 SandboxClaim state: %w", err)
		}

		// If the warm pool ref in the hub hasn't changed (or matches the restored value),
		// we restore the original templateRef and warmpool fields.
		expectedWarmPoolRefName := ""
		if original.Spec.WarmPool != nil && original.Spec.WarmPool.IsSpecificPool() {
			expectedWarmPoolRefName = string(*original.Spec.WarmPool)
		} else {
			expectedWarmPoolRefName = original.Spec.TemplateRef.Name
		}

		if isWarmPoolRefMatching(src.Spec.WarmPoolRef.Name, expectedWarmPoolRefName, src.Status.SandboxStatus.Name) {
			s.Spec.TemplateRef = original.Spec.TemplateRef
			s.Spec.WarmPool = original.Spec.WarmPool
		} else {
			// The warm pool was updated in v1beta1, so we reflect it in v1alpha1
			policy := WarmPoolPolicy(src.Spec.WarmPoolRef.Name)
			s.Spec.WarmPool = &policy
			// We can't know the new template ref easily, so we keep the original one as a fallback
			s.Spec.TemplateRef = original.Spec.TemplateRef
		}
	}

	return nil
}

func isWarmPoolRefMatching(actualName, expectedName, sandboxName string) bool {
	if actualName == expectedName {
		return true
	}
	if actualName == fmt.Sprintf("shadow-pool-%s", expectedName) {
		return true
	}
	if sandboxName != "" && (actualName == sandboxName || actualName == stripRandomSuffix(sandboxName)) {
		return true
	}
	return false
}

func stripRandomSuffix(name string) string {
	if idx := strings.LastIndex(name, "-"); idx != -1 {
		return name[:idx]
	}
	return name
}

// Helper functions for SandboxClaim conversion

func convertClaimSpecTo(src *SandboxClaimSpec, dst *v1beta1.SandboxClaimSpec, claimName, sandboxName string) error {
	// Lifecycle
	if src.Lifecycle != nil {
		dst.Lifecycle = &v1beta1.Lifecycle{
			ShutdownTime:            src.Lifecycle.ShutdownTime,
			TTLSecondsAfterFinished: src.Lifecycle.TTLSecondsAfterFinished,
			ShutdownPolicy:          v1beta1.ShutdownPolicy(src.Lifecycle.ShutdownPolicy),
		}
	} else {
		dst.Lifecycle = nil
	}

	// WarmPool / TemplateRef -> WarmPoolRef
	if src.WarmPool != nil && src.WarmPool.IsSpecificPool() {
		dst.WarmPoolRef = v1beta1.SandboxWarmPoolRef{
			Name: string(*src.WarmPool),
		}
	} else {
		// none or default warm pool policy
		if sandboxName != "" && claimName != sandboxName {
			// Warm start
			dst.WarmPoolRef = v1beta1.SandboxWarmPoolRef{
				Name: stripRandomSuffix(sandboxName),
			}
		} else {
			// Cold start or no sandbox created yet
			dst.WarmPoolRef = v1beta1.SandboxWarmPoolRef{
				Name: fmt.Sprintf("shadow-pool-%s", src.TemplateRef.Name),
			}
		}
	}

	// AdditionalPodMetadata
	if err := convertPodMetadataToClaim(&src.AdditionalPodMetadata, &dst.AdditionalPodMetadata); err != nil {
		return err
	}

	// Env
	if src.Env != nil {
		dst.Env = make([]v1beta1.EnvVar, len(src.Env))
		for i := range src.Env {
			dst.Env[i] = v1beta1.EnvVar{
				Name:          src.Env[i].Name,
				Value:         src.Env[i].Value,
				ContainerName: src.Env[i].ContainerName,
			}
		}
	} else {
		dst.Env = nil
	}

	return nil
}

func convertClaimSpecFrom(src *v1beta1.SandboxClaimSpec, dst *SandboxClaimSpec) error {
	// Lifecycle
	if src.Lifecycle != nil {
		dst.Lifecycle = &Lifecycle{
			ShutdownTime:            src.Lifecycle.ShutdownTime,
			TTLSecondsAfterFinished: src.Lifecycle.TTLSecondsAfterFinished,
			ShutdownPolicy:          ShutdownPolicy(src.Lifecycle.ShutdownPolicy),
		}
	} else {
		dst.Lifecycle = nil
	}

	// WarmPoolRef -> WarmPool / TemplateRef
	if templateName, ok := strings.CutPrefix(src.WarmPoolRef.Name, "shadow-pool-"); ok {
		dst.TemplateRef = SandboxTemplateRef{
			Name: templateName,
		}
		policy := WarmPoolPolicyDefault
		dst.WarmPool = &policy
	} else {
		policy := WarmPoolPolicy(src.WarmPoolRef.Name)
		dst.WarmPool = &policy
		dst.TemplateRef = SandboxTemplateRef{
			Name: src.WarmPoolRef.Name,
		}
	}

	// AdditionalPodMetadata
	if err := convertPodMetadataFromClaim(&src.AdditionalPodMetadata, &dst.AdditionalPodMetadata); err != nil {
		return err
	}

	// Env
	if src.Env != nil {
		dst.Env = make([]EnvVar, len(src.Env))
		for i := range src.Env {
			dst.Env[i] = EnvVar{
				Name:          src.Env[i].Name,
				Value:         src.Env[i].Value,
				ContainerName: src.Env[i].ContainerName,
			}
		}
	} else {
		dst.Env = nil
	}

	return nil
}

func convertClaimStatusTo(src *SandboxClaimStatus, dst *v1beta1.SandboxClaimStatus) error {
	dst.Conditions = src.Conditions
	dst.SandboxStatus = v1beta1.SandboxStatus{
		Name:   src.SandboxStatus.Name,
		PodIPs: src.SandboxStatus.PodIPs,
	}
	return nil
}

func convertClaimStatusFrom(src *v1beta1.SandboxClaimStatus, dst *SandboxClaimStatus) error {
	dst.Conditions = src.Conditions
	dst.SandboxStatus = SandboxStatus{
		Name:   src.SandboxStatus.Name,
		PodIPs: src.SandboxStatus.PodIPs,
	}
	return nil
}

func convertPodMetadataToClaim(src *sandboxv1alpha1.PodMetadata, dst *sandboxv1beta1.PodMetadata) error {
	dst.Labels = src.Labels
	dst.Annotations = src.Annotations
	return nil
}

func convertPodMetadataFromClaim(src *sandboxv1beta1.PodMetadata, dst *sandboxv1alpha1.PodMetadata) error {
	dst.Labels = src.Labels
	dst.Annotations = src.Annotations
	return nil
}
