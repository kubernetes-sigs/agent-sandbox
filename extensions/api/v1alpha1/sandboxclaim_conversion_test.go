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

func TestSandboxClaimConversion(t *testing.T) {
	tests := []struct {
		name                string
		claimName           string
		warmPool            string
		templateName        string
		sandboxName         string
		expectedWarmPoolRef string
	}{
		{
			name:                "Cold start with specific warm pool policy",
			claimName:           "my-claim",
			warmPool:            "my-pool",
			templateName:        "my-template",
			sandboxName:         "my-claim",
			expectedWarmPoolRef: "my-pool",
		},
		{
			name:                "Cold start with none warm pool policy",
			claimName:           "my-claim",
			warmPool:            "none",
			templateName:        "my-template",
			sandboxName:         "my-claim",
			expectedWarmPoolRef: "shadow-pool-my-template",
		},
		{
			name:                "Cold start with default warm pool policy",
			claimName:           "my-claim",
			warmPool:            "default",
			templateName:        "my-template",
			sandboxName:         "my-claim",
			expectedWarmPoolRef: "shadow-pool-my-template",
		},
		{
			name:                "Warm start with specific warm pool policy",
			claimName:           "my-claim",
			warmPool:            "my-pool",
			templateName:        "my-template",
			sandboxName:         "my-pool-abcde",
			expectedWarmPoolRef: "my-pool",
		},
		{
			name:                "Warm start with default warm pool policy",
			claimName:           "my-claim",
			warmPool:            "default",
			templateName:        "my-template",
			sandboxName:         "my-pool-abcde",
			expectedWarmPoolRef: "my-pool",
		},
		{
			name:                "No sandbox created yet: fallback to warm pool name",
			claimName:           "my-claim",
			warmPool:            "my-pool",
			templateName:        "my-template",
			sandboxName:         "",
			expectedWarmPoolRef: "my-pool",
		},
		{
			name:                "No sandbox created yet and no specific warm pool: fallback to shadow-pool-my-template",
			claimName:           "my-claim",
			warmPool:            "",
			templateName:        "my-template",
			sandboxName:         "",
			expectedWarmPoolRef: "shadow-pool-my-template",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create src v1alpha1 SandboxClaim
			wpPolicy := WarmPoolPolicy(tc.warmPool)
			src := &SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.claimName,
					Namespace: "default",
					Labels: map[string]string{
						"foo": "bar",
					},
					Annotations: map[string]string{
						"baz":                               "qux",
						v1alpha1SandboxClaimStateAnnotation: "some-old-state",
					},
				},
				Spec: SandboxClaimSpec{
					TemplateRef: SandboxTemplateRef{
						Name: tc.templateName,
					},
				},
				Status: SandboxClaimStatus{
					SandboxStatus: SandboxStatus{
						Name: tc.sandboxName,
					},
				},
			}
			if tc.warmPool != "" {
				src.Spec.WarmPool = &wpPolicy
			}

			// Convert to Hub (v1beta1)
			dst := &v1beta1.SandboxClaim{}
			if err := src.ConvertTo(dst); err != nil {
				t.Fatalf("failed to convert to v1beta1: %v", err)
			}

			// Verify src annotations and labels were not mutated during ConvertTo
			if val, ok := src.Annotations[v1alpha1SandboxClaimStateAnnotation]; !ok || val != "some-old-state" {
				t.Errorf("src.Annotations was mutated during ConvertTo! expected 'some-old-state', got %q", val)
			}
			if len(src.Annotations) != 2 {
				t.Errorf("expected 2 annotations in src, got %d", len(src.Annotations))
			}
			if len(src.Labels) != 1 {
				t.Errorf("expected 1 label in src, got %d", len(src.Labels))
			}

			// Verify the marshaled state in dst does not contain the state annotation itself (no nesting)
			marshaledState := dst.Annotations[v1alpha1SandboxClaimStateAnnotation]
			var stateObj SandboxClaim
			if err := json.Unmarshal([]byte(marshaledState), &stateObj); err != nil {
				t.Fatalf("failed to unmarshal state from dst: %v", err)
			}
			if _, ok := stateObj.Annotations[v1alpha1SandboxClaimStateAnnotation]; ok {
				t.Errorf("dst.Annotations state nestedly contains the state annotation! causing exponential growth")
			}

			// Verify WarmPoolRef name in v1beta1
			if dst.Spec.WarmPoolRef.Name != tc.expectedWarmPoolRef {
				t.Errorf("expected WarmPoolRef.Name %q, got %q", tc.expectedWarmPoolRef, dst.Spec.WarmPoolRef.Name)
			}

			// Convert back to Spoke (v1alpha1)
			roundTrip := &SandboxClaim{}
			if err := roundTrip.ConvertFrom(dst); err != nil {
				t.Fatalf("failed to convert back to v1alpha1: %v", err)
			}

			// Verify state annotation was stripped during ConvertFrom
			if _, ok := roundTrip.Annotations[v1alpha1SandboxClaimStateAnnotation]; ok {
				t.Errorf("roundTrip.Annotations still contains the state annotation after ConvertFrom!")
			}

			// Verify round-trip preserves fields losslessly (due to state annotation preservation)
			if roundTrip.Spec.TemplateRef.Name != src.Spec.TemplateRef.Name {
				t.Errorf("roundtrip TemplateRef mismatch: expected %q, got %q", src.Spec.TemplateRef.Name, roundTrip.Spec.TemplateRef.Name)
			}
			if src.Spec.WarmPool != nil {
				if roundTrip.Spec.WarmPool == nil || *roundTrip.Spec.WarmPool != *src.Spec.WarmPool {
					t.Errorf("roundtrip WarmPool mismatch: expected %v, got %v", src.Spec.WarmPool, roundTrip.Spec.WarmPool)
				}
			} else {
				if roundTrip.Spec.WarmPool != nil && *roundTrip.Spec.WarmPool != WarmPoolPolicyDefault && *roundTrip.Spec.WarmPool != "" {
					t.Errorf("roundtrip WarmPool mismatch: expected nil or default, got %v", roundTrip.Spec.WarmPool)
				}
			}
		})
	}
}

func TestSandboxClaimConversionFromHub(t *testing.T) {
	// Test conversion of a v1beta1 SandboxClaim created without v1alpha1 state annotation (e.g. created directly via v1beta1 API)
	tests := []struct {
		name                string
		warmPoolRefName     string
		expectedWarmPool    string
		expectedTemplateRef string
	}{
		{
			name:                "WarmPoolRef is a specific pool",
			warmPoolRefName:     "my-pool",
			expectedWarmPool:    "my-pool",
			expectedTemplateRef: "my-pool",
		},
		{
			name:                "WarmPoolRef is empty",
			warmPoolRefName:     "",
			expectedWarmPool:    "",
			expectedTemplateRef: "",
		},
		{
			name:                "WarmPoolRef is a synthetic shadow pool",
			warmPoolRefName:     "shadow-pool-my-template",
			expectedWarmPool:    "default",
			expectedTemplateRef: "my-template",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := &v1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-claim",
					Namespace: "default",
				},
				Spec: v1beta1.SandboxClaimSpec{
					WarmPoolRef: v1beta1.SandboxWarmPoolRef{
						Name: tc.warmPoolRefName,
					},
				},
			}

			dst := &SandboxClaim{}
			if err := dst.ConvertFrom(src); err != nil {
				t.Fatalf("failed to convert from v1beta1: %v", err)
			}

			if dst.Spec.WarmPool == nil {
				t.Fatalf("expected WarmPool to be non-nil, got nil")
			}
			if string(*dst.Spec.WarmPool) != tc.expectedWarmPool {
				t.Errorf("expected WarmPool %q, got %q", tc.expectedWarmPool, string(*dst.Spec.WarmPool))
			}
			if dst.Spec.TemplateRef.Name != tc.expectedTemplateRef {
				t.Errorf("expected TemplateRef.Name %q, got %q", tc.expectedTemplateRef, dst.Spec.TemplateRef.Name)
			}
		})
	}
}
