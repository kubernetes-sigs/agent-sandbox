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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseWatchNamespaces(t *testing.T) {
	tests := []struct {
		name     string
		flag     string
		env      string
		expected []string
	}{
		{
			name:     "empty flag and no env is cluster-scoped",
			expected: nil,
		},
		{
			name:     "single namespace",
			flag:     "team-a",
			expected: []string{"team-a"},
		},
		{
			name:     "multiple namespaces",
			flag:     "team-a,team-b,team-c",
			expected: []string{"team-a", "team-b", "team-c"},
		},
		{
			name:     "trims whitespace and drops empty entries",
			flag:     " team-a , , team-b ,",
			expected: []string{"team-a", "team-b"},
		},
		{
			name:     "deduplicates repeated namespaces",
			flag:     "team-a,team-a",
			expected: []string{"team-a"},
		},
		{
			name:     "deduplicates after trimming",
			flag:     "team-a, team-a ,team-b,team-b",
			expected: []string{"team-a", "team-b"},
		},
		{
			name:     "falls back to WATCH_NAMESPACE env var",
			env:      "team-a,team-b",
			expected: []string{"team-a", "team-b"},
		},
		{
			name:     "flag takes precedence over env var",
			flag:     "team-a",
			env:      "team-b",
			expected: []string{"team-a"},
		},
		{
			name:     "empty env var is cluster-scoped",
			env:      "",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Always set WATCH_NAMESPACE so the result does not depend on the
			// environment inherited from the test runner.
			t.Setenv("WATCH_NAMESPACE", tt.env)
			assert.Equal(t, tt.expected, parseWatchNamespaces(tt.flag))
		})
	}
}
