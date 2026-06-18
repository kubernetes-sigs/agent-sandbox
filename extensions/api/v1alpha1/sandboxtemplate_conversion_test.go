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
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	v1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func TestSandboxTemplateConversion(t *testing.T) {
	bTrue := true
	src := &SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-template",
			Namespace: "default",
			Labels: map[string]string{
				"foo": "bar",
			},
			Annotations: map[string]string{
				"baz":                                  "qux",
				v1alpha1SandboxTemplateStateAnnotation: "some-old-state",
			},
		},
		Spec: SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "my-agent",
							Image: "my-image:latest",
						},
					},
				},
			},
			VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
						Name: "data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
					},
				},
			},
			NetworkPolicy: &NetworkPolicySpec{
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{}, // empty ingress rule to verify it gets converted
				},
			},
			NetworkPolicyManagement: NetworkPolicyManagementManaged,
			EnvVarsInjectionPolicy:  EnvVarsInjectionPolicyAllowed,
			Service:                 &bTrue,
		},
	}

	// Convert to Hub (v1beta1)
	dst := &v1beta1.SandboxTemplate{}
	if err := src.ConvertTo(dst); err != nil {
		t.Fatalf("failed to convert to v1beta1: %v", err)
	}

	// Verify src annotations and labels were not mutated during ConvertTo
	if val, ok := src.Annotations[v1alpha1SandboxTemplateStateAnnotation]; !ok || val != "some-old-state" {
		t.Errorf("src.Annotations was mutated during ConvertTo! expected 'some-old-state', got %q", val)
	}
	if len(src.Annotations) != 2 {
		t.Errorf("expected 2 annotations in src, got %d", len(src.Annotations))
	}
	if len(src.Labels) != 1 {
		t.Errorf("expected 1 label in src, got %d", len(src.Labels))
	}

	// Verify the marshaled state in dst does not contain the state annotation itself (no nesting)
	marshaledState := dst.Annotations[v1alpha1SandboxTemplateStateAnnotation]
	var stateObj SandboxTemplate
	if err := json.Unmarshal([]byte(marshaledState), &stateObj); err != nil {
		t.Fatalf("failed to unmarshal state from dst: %v", err)
	}
	if _, ok := stateObj.Annotations[v1alpha1SandboxTemplateStateAnnotation]; ok {
		t.Errorf("dst.Annotations state nestedly contains the state annotation! causing exponential growth")
	}

	// Verify v1beta1 fields
	if dst.Spec.PodTemplate.Spec.Containers[0].Image != "my-image:latest" {
		t.Errorf("unexpected image: %s", dst.Spec.PodTemplate.Spec.Containers[0].Image)
	}
	if string(dst.Spec.EnvVarsInjectionPolicy) != string(EnvVarsInjectionPolicyAllowed) {
		t.Errorf("unexpected EnvVarsInjectionPolicy: %s", dst.Spec.EnvVarsInjectionPolicy)
	}

	// Convert back to Spoke (v1alpha1)
	roundTrip := &SandboxTemplate{}
	if err := roundTrip.ConvertFrom(dst); err != nil {
		t.Fatalf("failed to convert back to v1alpha1: %v", err)
	}

	// Verify state annotation was stripped during ConvertFrom
	if _, ok := roundTrip.Annotations[v1alpha1SandboxTemplateStateAnnotation]; ok {
		t.Errorf("roundTrip.Annotations still contains the state annotation after ConvertFrom!")
	}

	// Verify round-trip preserves all fields
	if roundTrip.Spec.PodTemplate.Spec.Containers[0].Image != src.Spec.PodTemplate.Spec.Containers[0].Image {
		t.Errorf("roundtrip PodTemplate Image mismatch: expected %q, got %q", src.Spec.PodTemplate.Spec.Containers[0].Image, roundTrip.Spec.PodTemplate.Spec.Containers[0].Image)
	}
	if roundTrip.Spec.EnvVarsInjectionPolicy != src.Spec.EnvVarsInjectionPolicy {
		t.Errorf("roundtrip EnvVarsInjectionPolicy mismatch: expected %q, got %q", src.Spec.EnvVarsInjectionPolicy, roundTrip.Spec.EnvVarsInjectionPolicy)
	}
	if roundTrip.Spec.NetworkPolicyManagement != src.Spec.NetworkPolicyManagement {
		t.Errorf("roundtrip NetworkPolicyManagement mismatch: expected %q, got %q", src.Spec.NetworkPolicyManagement, roundTrip.Spec.NetworkPolicyManagement)
	}
	if roundTrip.Spec.Service == nil || *roundTrip.Spec.Service != *src.Spec.Service {
		t.Errorf("roundtrip Service mismatch")
	}
}
