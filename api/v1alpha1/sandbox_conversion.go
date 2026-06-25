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

	v1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

const v1alpha1SandboxStateAnnotation = "api.agents.x-k8s.io/v1alpha1-sandbox-state"

// ConvertTo converts this Sandbox to the Hub version (v1beta1).
func (s *Sandbox) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.Sandbox)

	// Copy object metadata
	s.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)

	// Convert Spec
	if err := ConvertSpecTo(&s.Spec, &dst.Spec); err != nil {
		return err
	}

	// Convert Status
	if err := ConvertStatusTo(&s.Status, &dst.Status); err != nil {
		return err
	}

	// Preserve the original v1alpha1 object state for lossless round-tripping
	if dst.Annotations == nil {
		dst.Annotations = make(map[string]string)
	}
	sCopy := s.DeepCopy()
	if sCopy.Annotations != nil {
		delete(sCopy.Annotations, v1alpha1SandboxStateAnnotation)
	}
	stateJSON, err := json.Marshal(sCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal v1alpha1 Sandbox state: %w", err)
	}
	dst.Annotations[v1alpha1SandboxStateAnnotation] = string(stateJSON)

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this Sandbox.
func (s *Sandbox) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.Sandbox)

	// Copy object metadata
	src.ObjectMeta.DeepCopyInto(&s.ObjectMeta)

	// Convert Spec
	if err := ConvertSpecFrom(&src.Spec, &s.Spec); err != nil {
		return err
	}

	// Convert Status
	if err := ConvertStatusFrom(&src.Status, &s.Status); err != nil {
		return err
	}

	// Set best-effort default for Status.Replicas based on OperatingMode.
	// This will be overridden by the restoration logic if the annotation exists.
	if src.Spec.OperatingMode == v1beta1.SandboxOperatingModeSuspended {
		s.Status.Replicas = 0
	} else {
		s.Status.Replicas = 1
	}

	// Restore original v1alpha1 state if present to ensure lossless conversion
	if stateJSON, ok := s.Annotations[v1alpha1SandboxStateAnnotation]; ok {
		// Strip the state annotation so it doesn't leak to clients and get sent back on updates
		delete(s.Annotations, v1alpha1SandboxStateAnnotation)

		var original Sandbox
		if err := json.Unmarshal([]byte(stateJSON), &original); err != nil {
			return fmt.Errorf("failed to unmarshal v1alpha1 Sandbox state: %w", err)
		}

		// Restore replicas field from original if OperatingMode matches original intent
		switch src.Spec.OperatingMode {
		case v1beta1.SandboxOperatingModeSuspended:
			zero := int32(0)
			s.Spec.Replicas = &zero
		case v1beta1.SandboxOperatingModeRunning:
			if original.Spec.Replicas == nil || *original.Spec.Replicas != 0 {
				s.Spec.Replicas = original.Spec.Replicas
			} else {
				one := int32(1)
				s.Spec.Replicas = &one
			}
		}

		// Restore Status replicas
		s.Status.Replicas = original.Status.Replicas
	}

	return nil
}

// Helper functions for Sandbox conversion

func ConvertSpecTo(src *SandboxSpec, dst *v1beta1.SandboxSpec) error {
	// PodTemplate
	if err := ConvertPodTemplateTo(&src.PodTemplate, &dst.PodTemplate); err != nil {
		return err
	}

	// VolumeClaimTemplates
	if src.VolumeClaimTemplates != nil {
		dst.VolumeClaimTemplates = make([]v1beta1.PersistentVolumeClaimTemplate, len(src.VolumeClaimTemplates))
		for i := range src.VolumeClaimTemplates {
			if err := ConvertPVCClaimTemplateTo(&src.VolumeClaimTemplates[i], &dst.VolumeClaimTemplates[i]); err != nil {
				return err
			}
		}
	} else {
		dst.VolumeClaimTemplates = nil
	}

	// Lifecycle
	if err := ConvertLifecycleTo(&src.Lifecycle, &dst.Lifecycle); err != nil {
		return err
	}

	// Replicas -> OperatingMode
	if src.Replicas != nil && *src.Replicas == 0 {
		dst.OperatingMode = v1beta1.SandboxOperatingModeSuspended
	} else {
		dst.OperatingMode = v1beta1.SandboxOperatingModeRunning
	}

	// Service
	dst.Service = src.Service

	return nil
}

func ConvertSpecFrom(src *v1beta1.SandboxSpec, dst *SandboxSpec) error {
	// PodTemplate
	if err := ConvertPodTemplateFrom(&src.PodTemplate, &dst.PodTemplate); err != nil {
		return err
	}

	// VolumeClaimTemplates
	if src.VolumeClaimTemplates != nil {
		dst.VolumeClaimTemplates = make([]PersistentVolumeClaimTemplate, len(src.VolumeClaimTemplates))
		for i := range src.VolumeClaimTemplates {
			if err := ConvertPVCClaimTemplateFrom(&src.VolumeClaimTemplates[i], &dst.VolumeClaimTemplates[i]); err != nil {
				return err
			}
		}
	} else {
		dst.VolumeClaimTemplates = nil
	}

	// Lifecycle
	if err := ConvertLifecycleFrom(&src.Lifecycle, &dst.Lifecycle); err != nil {
		return err
	}

	// OperatingMode -> Replicas
	one := int32(1)
	zero := int32(0)
	if src.OperatingMode == v1beta1.SandboxOperatingModeSuspended {
		dst.Replicas = &zero
	} else {
		dst.Replicas = &one
	}

	// Service
	dst.Service = src.Service

	return nil
}

func ConvertStatusTo(src *SandboxStatus, dst *v1beta1.SandboxStatus) error {
	dst.ServiceFQDN = src.ServiceFQDN
	dst.Service = src.Service
	dst.Conditions = src.Conditions
	dst.LabelSelector = src.LabelSelector
	dst.PodIPs = src.PodIPs
	dst.NodeName = "" // NodeName is new in v1beta1 and does not exist in v1alpha1
	return nil
}

func ConvertStatusFrom(src *v1beta1.SandboxStatus, dst *SandboxStatus) error {
	dst.ServiceFQDN = src.ServiceFQDN
	dst.Service = src.Service
	dst.Conditions = src.Conditions
	dst.LabelSelector = src.LabelSelector
	dst.PodIPs = src.PodIPs
	return nil
}

func ConvertPodTemplateTo(src *PodTemplate, dst *v1beta1.PodTemplate) error {
	dst.Spec = src.Spec
	ConvertPodMetadataTo(&src.ObjectMeta, &dst.ObjectMeta)
	return nil
}

func ConvertPodTemplateFrom(src *v1beta1.PodTemplate, dst *PodTemplate) error {
	dst.Spec = src.Spec
	ConvertPodMetadataFrom(&src.ObjectMeta, &dst.ObjectMeta)
	return nil
}

func ConvertPodMetadataTo(src *PodMetadata, dst *v1beta1.PodMetadata) {
	dst.Labels = src.Labels
	dst.Annotations = src.Annotations
}

func ConvertPodMetadataFrom(src *v1beta1.PodMetadata, dst *PodMetadata) {
	dst.Labels = src.Labels
	dst.Annotations = src.Annotations
}

func ConvertPVCClaimTemplateTo(src *PersistentVolumeClaimTemplate, dst *v1beta1.PersistentVolumeClaimTemplate) error {
	dst.Spec = src.Spec
	ConvertEmbeddedMetadataTo(&src.EmbeddedObjectMetadata, &dst.EmbeddedObjectMetadata)
	return nil
}

func ConvertPVCClaimTemplateFrom(src *v1beta1.PersistentVolumeClaimTemplate, dst *PersistentVolumeClaimTemplate) error {
	dst.Spec = src.Spec
	ConvertEmbeddedMetadataFrom(&src.EmbeddedObjectMetadata, &dst.EmbeddedObjectMetadata)
	return nil
}

func ConvertEmbeddedMetadataTo(src *EmbeddedObjectMetadata, dst *v1beta1.EmbeddedObjectMetadata) {
	dst.Name = src.Name
	dst.Labels = src.Labels
	dst.Annotations = src.Annotations
}

func ConvertEmbeddedMetadataFrom(src *v1beta1.EmbeddedObjectMetadata, dst *EmbeddedObjectMetadata) {
	dst.Name = src.Name
	dst.Labels = src.Labels
	dst.Annotations = src.Annotations
}

func ConvertLifecycleTo(src *Lifecycle, dst *v1beta1.Lifecycle) error {
	dst.ShutdownTime = src.ShutdownTime
	if src.ShutdownPolicy != nil {
		policy := v1beta1.ShutdownPolicy(*src.ShutdownPolicy)
		dst.ShutdownPolicy = &policy
	} else {
		dst.ShutdownPolicy = nil
	}
	return nil
}

func ConvertLifecycleFrom(src *v1beta1.Lifecycle, dst *Lifecycle) error {
	dst.ShutdownTime = src.ShutdownTime
	if src.ShutdownPolicy != nil {
		policy := ShutdownPolicy(*src.ShutdownPolicy)
		dst.ShutdownPolicy = &policy
	} else {
		dst.ShutdownPolicy = nil
	}
	return nil
}
