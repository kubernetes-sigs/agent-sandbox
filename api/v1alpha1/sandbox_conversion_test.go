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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func TestSandboxConversion(t *testing.T) {
	one := int32(1)
	zero := int32(0)

	tests := []struct {
		name                string
		replicas            *int32
		expectedMode        v1beta1.SandboxOperatingMode
		statusReplicas      int32
		expectedStatusRepls int32
	}{
		{
			name:                "Replicas is nil (default running)",
			replicas:            nil,
			expectedMode:        v1beta1.SandboxOperatingModeRunning,
			statusReplicas:      1,
			expectedStatusRepls: 1,
		},
		{
			name:                "Replicas is 1",
			replicas:            &one,
			expectedMode:        v1beta1.SandboxOperatingModeRunning,
			statusReplicas:      1,
			expectedStatusRepls: 1,
		},
		{
			name:                "Replicas is 0 (suspended)",
			replicas:            &zero,
			expectedMode:        v1beta1.SandboxOperatingModeSuspended,
			statusReplicas:      0,
			expectedStatusRepls: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := metav1.Now()
			policy := ShutdownPolicyDelete
			bTrue := true

			// Create src v1alpha1 Sandbox
			src := &Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						"foo": "bar",
					},
					Annotations: map[string]string{
						"baz":                          "qux",
						v1alpha1SandboxStateAnnotation: "some-old-state",
					},
				},
				Spec: SandboxSpec{
					PodTemplate: PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "agent",
									Image: "agent-image:latest",
								},
							},
						},
						ObjectMeta: PodMetadata{
							Labels: map[string]string{
								"pod-label": "value",
							},
						},
					},
					VolumeClaimTemplates: []PersistentVolumeClaimTemplate{
						{
							EmbeddedObjectMetadata: EmbeddedObjectMetadata{
								Name: "workspace",
							},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{
									corev1.ReadWriteOnce,
								},
							},
						},
					},
					Lifecycle: Lifecycle{
						ShutdownTime:   &now,
						ShutdownPolicy: &policy,
					},
					Replicas: tc.replicas,
					Service:  &bTrue,
				},
				Status: SandboxStatus{
					ServiceFQDN:   "my-sandbox.default.svc.cluster.local",
					LabelSelector: "sandbox=my-sandbox",
					PodIPs: []string{
						"10.244.0.5",
					},
					Replicas: tc.statusReplicas,
				},
			}

			// Convert to Hub (v1beta1)
			dst := &v1beta1.Sandbox{}
			if err := src.ConvertTo(dst); err != nil {
				t.Fatalf("failed to convert to v1beta1: %v", err)
			}

			// Verify src annotations and labels were not mutated during ConvertTo
			if val, ok := src.Annotations[v1alpha1SandboxStateAnnotation]; !ok || val != "some-old-state" {
				t.Errorf("src.Annotations was mutated during ConvertTo! expected 'some-old-state', got %q", val)
			}
			if len(src.Annotations) != 2 {
				t.Errorf("expected 2 annotations in src, got %d", len(src.Annotations))
			}
			if len(src.Labels) != 1 {
				t.Errorf("expected 1 label in src, got %d", len(src.Labels))
			}

			// Verify the marshaled state in dst does not contain the state annotation itself (no nesting)
			marshaledState := dst.Annotations[v1alpha1SandboxStateAnnotation]
			var stateObj Sandbox
			if err := json.Unmarshal([]byte(marshaledState), &stateObj); err != nil {
				t.Fatalf("failed to unmarshal state from dst: %v", err)
			}
			if _, ok := stateObj.Annotations[v1alpha1SandboxStateAnnotation]; ok {
				t.Errorf("dst.Annotations state nestedly contains the state annotation! causing exponential growth")
			}

			// Verify fields in v1beta1
			if dst.Spec.OperatingMode != tc.expectedMode {
				t.Errorf("expected OperatingMode %q, got %q", tc.expectedMode, dst.Spec.OperatingMode)
			}
			if dst.Spec.PodTemplate.Spec.Containers[0].Image != "agent-image:latest" {
				t.Errorf("expected image %q, got %q", "agent-image:latest", dst.Spec.PodTemplate.Spec.Containers[0].Image)
			}
			if dst.Spec.VolumeClaimTemplates[0].Name != "workspace" {
				t.Errorf("expected VolumeClaimTemplate name %q, got %q", "workspace", dst.Spec.VolumeClaimTemplates[0].Name)
			}
			if dst.Spec.Lifecycle.ShutdownPolicy == nil || string(*dst.Spec.Lifecycle.ShutdownPolicy) != string(ShutdownPolicyDelete) {
				t.Errorf("expected ShutdownPolicy %q, got %v", ShutdownPolicyDelete, dst.Spec.Lifecycle.ShutdownPolicy)
			}

			// Convert back to Spoke (v1alpha1)
			roundTrip := &Sandbox{}
			if err := roundTrip.ConvertFrom(dst); err != nil {
				t.Fatalf("failed to convert back to v1alpha1: %v", err)
			}

			// Verify state annotation was stripped during ConvertFrom
			if _, ok := roundTrip.Annotations[v1alpha1SandboxStateAnnotation]; ok {
				t.Errorf("roundTrip.Annotations still contains the state annotation after ConvertFrom!")
			}

			// Verify round-trip preserves fields losslessly
			if tc.replicas == nil {
				if roundTrip.Spec.Replicas != nil {
					t.Errorf("roundtrip Replicas mismatch: expected nil, got %v", *roundTrip.Spec.Replicas)
				}
			} else {
				if roundTrip.Spec.Replicas == nil || *roundTrip.Spec.Replicas != *tc.replicas {
					t.Errorf("roundtrip Replicas mismatch: expected %d, got %v", *tc.replicas, roundTrip.Spec.Replicas)
				}
			}

			if roundTrip.Status.Replicas != tc.expectedStatusRepls {
				t.Errorf("roundtrip Status.Replicas mismatch: expected %d, got %d", tc.expectedStatusRepls, roundTrip.Status.Replicas)
			}

			if roundTrip.Spec.PodTemplate.Spec.Containers[0].Image != src.Spec.PodTemplate.Spec.Containers[0].Image {
				t.Errorf("roundtrip PodTemplate Image mismatch: expected %q, got %q", src.Spec.PodTemplate.Spec.Containers[0].Image, roundTrip.Spec.PodTemplate.Spec.Containers[0].Image)
			}

			if len(roundTrip.Spec.VolumeClaimTemplates) != len(src.Spec.VolumeClaimTemplates) || roundTrip.Spec.VolumeClaimTemplates[0].Name != src.Spec.VolumeClaimTemplates[0].Name {
				t.Errorf("roundtrip VolumeClaimTemplates mismatch")
			}

			if roundTrip.Spec.Lifecycle.ShutdownPolicy == nil || *roundTrip.Spec.Lifecycle.ShutdownPolicy != *src.Spec.Lifecycle.ShutdownPolicy {
				t.Errorf("roundtrip ShutdownPolicy mismatch")
			}
		})
	}
}

func TestSandboxConversionFromHub(t *testing.T) {
	// Test conversion of a v1beta1 Sandbox created without v1alpha1 state annotation (e.g. created directly via v1beta1 API)
	tests := []struct {
		name             string
		mode             v1beta1.SandboxOperatingMode
		expectedReplicas int32
	}{
		{
			name:             "Hub Running mode",
			mode:             v1beta1.SandboxOperatingModeRunning,
			expectedReplicas: 1,
		},
		{
			name:             "Hub Suspended mode",
			mode:             v1beta1.SandboxOperatingModeSuspended,
			expectedReplicas: 0,
		},
		{
			name:             "Hub empty mode (defaults to Running/1)",
			mode:             "",
			expectedReplicas: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := &v1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-sandbox",
					Namespace: "default",
				},
				Spec: v1beta1.SandboxSpec{
					OperatingMode: tc.mode,
				},
			}

			dst := &Sandbox{}
			if err := dst.ConvertFrom(src); err != nil {
				t.Fatalf("failed to convert from v1beta1: %v", err)
			}

			if dst.Spec.Replicas == nil {
				t.Fatalf("expected Replicas to be non-nil, got nil")
			}
			if *dst.Spec.Replicas != tc.expectedReplicas {
				t.Errorf("expected Replicas to be %d, got %d", tc.expectedReplicas, *dst.Spec.Replicas)
			}
			if dst.Status.Replicas != tc.expectedReplicas {
				t.Errorf("expected Status.Replicas to be %d, got %d", tc.expectedReplicas, dst.Status.Replicas)
			}
		})
	}
}
