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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func TestSandboxWarmPoolConversion(t *testing.T) {
	src := &SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-warmpool",
			Namespace: "default",
			Labels: map[string]string{
				"foo": "bar",
			},
			Annotations: map[string]string{
				"baz":                                  "qux",
				v1alpha1SandboxWarmPoolStateAnnotation: "some-old-state",
			},
		},
		Spec: SandboxWarmPoolSpec{
			Replicas: 3,
			TemplateRef: SandboxTemplateRef{
				Name: "my-template",
			},
			UpdateStrategy: &SandboxWarmPoolUpdateStrategy{
				Type: RecreateSandboxWarmPoolUpdateStrategyType,
			},
		},
		Status: SandboxWarmPoolStatus{
			Replicas:      3,
			ReadyReplicas: 2,
			Selector:      "app=my-warmpool",
		},
	}

	// Convert to Hub (v1beta1)
	dst := &v1beta1.SandboxWarmPool{}
	if err := src.ConvertTo(dst); err != nil {
		t.Fatalf("failed to convert to v1beta1: %v", err)
	}

	// Verify src annotations and labels were not mutated during ConvertTo
	if val, ok := src.Annotations[v1alpha1SandboxWarmPoolStateAnnotation]; !ok || val != "some-old-state" {
		t.Errorf("src.Annotations was mutated during ConvertTo! expected 'some-old-state', got %q", val)
	}
	if len(src.Annotations) != 2 {
		t.Errorf("expected 2 annotations in src, got %d", len(src.Annotations))
	}
	if len(src.Labels) != 1 {
		t.Errorf("expected 1 label in src, got %d", len(src.Labels))
	}

	// Verify the marshaled state in dst does not contain the state annotation itself (no nesting)
	marshaledState := dst.Annotations[v1alpha1SandboxWarmPoolStateAnnotation]
	var stateObj SandboxWarmPool
	if err := json.Unmarshal([]byte(marshaledState), &stateObj); err != nil {
		t.Fatalf("failed to unmarshal state from dst: %v", err)
	}
	if _, ok := stateObj.Annotations[v1alpha1SandboxWarmPoolStateAnnotation]; ok {
		t.Errorf("dst.Annotations state nestedly contains the state annotation! causing exponential growth")
	}

	// Verify v1beta1 fields
	if dst.Spec.Replicas != 3 {
		t.Errorf("unexpected replicas: %d", dst.Spec.Replicas)
	}
	if dst.Spec.TemplateRef.Name != "my-template" {
		t.Errorf("unexpected template ref: %s", dst.Spec.TemplateRef.Name)
	}
	if dst.Spec.UpdateStrategy == nil || string(dst.Spec.UpdateStrategy.Type) != string(RecreateSandboxWarmPoolUpdateStrategyType) {
		t.Errorf("unexpected update strategy: %v", dst.Spec.UpdateStrategy)
	}
	if dst.Status.ReadyReplicas != 2 {
		t.Errorf("unexpected ready replicas: %d", dst.Status.ReadyReplicas)
	}

	// Convert back to Spoke (v1alpha1)
	roundTrip := &SandboxWarmPool{}
	if err := roundTrip.ConvertFrom(dst); err != nil {
		t.Fatalf("failed to convert back to v1alpha1: %v", err)
	}

	// Verify state annotation was stripped during ConvertFrom
	if _, ok := roundTrip.Annotations[v1alpha1SandboxWarmPoolStateAnnotation]; ok {
		t.Errorf("roundTrip.Annotations still contains the state annotation after ConvertFrom!")
	}

	// Verify round-trip preserves all fields
	if roundTrip.Spec.Replicas != src.Spec.Replicas {
		t.Errorf("roundtrip Replicas mismatch: expected %d, got %d", src.Spec.Replicas, roundTrip.Spec.Replicas)
	}
	if roundTrip.Spec.TemplateRef.Name != src.Spec.TemplateRef.Name {
		t.Errorf("roundtrip TemplateRef mismatch: expected %q, got %q", src.Spec.TemplateRef.Name, roundTrip.Spec.TemplateRef.Name)
	}
	if roundTrip.Spec.UpdateStrategy == nil || roundTrip.Spec.UpdateStrategy.Type != src.Spec.UpdateStrategy.Type {
		t.Errorf("roundtrip UpdateStrategy mismatch")
	}
	if roundTrip.Status.ReadyReplicas != src.Status.ReadyReplicas {
		t.Errorf("roundtrip ReadyReplicas mismatch: expected %d, got %d", src.Status.ReadyReplicas, roundTrip.Status.ReadyReplicas)
	}
	if roundTrip.Status.Selector != src.Status.Selector {
		t.Errorf("roundtrip Selector mismatch: expected %q, got %q", src.Status.Selector, roundTrip.Status.Selector)
	}
}
