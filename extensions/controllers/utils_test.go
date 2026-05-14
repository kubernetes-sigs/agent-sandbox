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
	"testing"
)

func TestSandboxTemplateRefHash(t *testing.T) {
	testCases := []struct {
		desc            string
		templateRefName string
	}{
		{
			desc:            "simple template ref name",
			templateRefName: "sandbox-name",
		},
		{
			desc:            "a different template ref name",
			templateRefName: "other-template",
		},
		{
			desc:            "empty template ref name",
			templateRefName: "",
		},
	}

	results := make(map[string]string)

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			hash1 := SandboxTemplateRefHash(tc.templateRefName)
			hash2 := SandboxTemplateRefHash(tc.templateRefName)

			if len(hash1) != 8 {
				t.Errorf("SandboxTemplateRefHash(%q) length = %d, want 8", tc.templateRefName, len(hash1))
			}

			if hash1 != hash2 {
				t.Errorf("SandboxTemplateRefHash(%q) is not deterministic: %q != %q", tc.templateRefName, hash1, hash2)
			}

			results[tc.desc] = hash1
		})
	}

	// Check that different inputs produce different hashes
	for descA, resultA := range results {
		for descB, resultB := range results {
			if descA == descB {
				continue
			}

			if resultA == resultB {
				t.Errorf("SandboxTemplateRefHash produced same hash for different inputs: case '%q' and case '%q'", descA, descB)
			}
		}
	}
}
