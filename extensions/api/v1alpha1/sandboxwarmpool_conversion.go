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

	"sigs.k8s.io/controller-runtime/pkg/conversion"

	v1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const v1alpha1SandboxWarmPoolStateAnnotation = "api.agents.x-k8s.io/v1alpha1-sandboxwarmpool-state"

// ConvertTo converts this SandboxWarmPool to the Hub version (v1beta1).
func (s *SandboxWarmPool) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.SandboxWarmPool)

	// Copy object metadata
	s.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)

	// Convert Spec
	if err := convertWarmPoolSpecTo(&s.Spec, &dst.Spec); err != nil {
		return err
	}

	// Convert Status
	if err := convertWarmPoolStatusTo(&s.Status, &dst.Status); err != nil {
		return err
	}

	// Preserve the original v1alpha1 object state for lossless round-tripping
	if dst.Annotations == nil {
		dst.Annotations = make(map[string]string)
	}
	sCopy := s.DeepCopy()
	if sCopy.Annotations != nil {
		delete(sCopy.Annotations, v1alpha1SandboxWarmPoolStateAnnotation)
	}
	stateJSON, err := json.Marshal(sCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal v1alpha1 SandboxWarmPool state: %w", err)
	}
	dst.Annotations[v1alpha1SandboxWarmPoolStateAnnotation] = string(stateJSON)

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this SandboxWarmPool.
func (s *SandboxWarmPool) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.SandboxWarmPool)

	// Copy object metadata
	src.ObjectMeta.DeepCopyInto(&s.ObjectMeta)

	// Convert Spec
	if err := convertWarmPoolSpecFrom(&src.Spec, &s.Spec); err != nil {
		return err
	}

	// Convert Status
	if err := convertWarmPoolStatusFrom(&src.Status, &s.Status); err != nil {
		return err
	}

	// Restore original v1alpha1 state if present to ensure lossless conversion
	if stateJSON, ok := s.Annotations[v1alpha1SandboxWarmPoolStateAnnotation]; ok {
		// Strip the state annotation so it doesn't leak to clients and get sent back on updates
		delete(s.Annotations, v1alpha1SandboxWarmPoolStateAnnotation)

		var original SandboxWarmPool
		if err := json.Unmarshal([]byte(stateJSON), &original); err != nil {
			return fmt.Errorf("failed to unmarshal v1alpha1 SandboxWarmPool state: %w", err)
		}
		// All fields map 1:1, kept for consistency/robustness.
	}

	return nil
}

// Helper functions for SandboxWarmPool conversion

func convertWarmPoolSpecTo(src *SandboxWarmPoolSpec, dst *v1beta1.SandboxWarmPoolSpec) error {
	dst.Replicas = src.Replicas
	dst.TemplateRef = v1beta1.SandboxTemplateRef{
		Name: src.TemplateRef.Name,
	}

	if src.UpdateStrategy != nil {
		dst.UpdateStrategy = &v1beta1.SandboxWarmPoolUpdateStrategy{
			Type: v1beta1.SandboxWarmPoolUpdateStrategyType(src.UpdateStrategy.Type),
		}
	} else {
		dst.UpdateStrategy = nil
	}

	return nil
}

func convertWarmPoolSpecFrom(src *v1beta1.SandboxWarmPoolSpec, dst *SandboxWarmPoolSpec) error {
	dst.Replicas = src.Replicas
	dst.TemplateRef = SandboxTemplateRef{
		Name: src.TemplateRef.Name,
	}

	if src.UpdateStrategy != nil {
		dst.UpdateStrategy = &SandboxWarmPoolUpdateStrategy{
			Type: SandboxWarmPoolUpdateStrategyType(src.UpdateStrategy.Type),
		}
	} else {
		dst.UpdateStrategy = nil
	}

	return nil
}

func convertWarmPoolStatusTo(src *SandboxWarmPoolStatus, dst *v1beta1.SandboxWarmPoolStatus) error {
	dst.Replicas = src.Replicas
	dst.ReadyReplicas = src.ReadyReplicas
	dst.Selector = src.Selector
	return nil
}

func convertWarmPoolStatusFrom(src *v1beta1.SandboxWarmPoolStatus, dst *SandboxWarmPoolStatus) error {
	dst.Replicas = src.Replicas
	dst.ReadyReplicas = src.ReadyReplicas
	dst.Selector = src.Selector
	return nil
}
