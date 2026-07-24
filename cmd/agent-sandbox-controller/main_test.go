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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestValidateWebhookConfiguration(t *testing.T) {
	tests := []struct {
		name            string
		enableWebhook   bool
		watchNamespaces []string
		expectedError   string
	}{
		{
			name:          "cluster-scoped webhook is allowed",
			enableWebhook: true,
		},
		{
			name:            "namespaced mode requires webhook disabled",
			enableWebhook:   true,
			watchNamespaces: []string{"team-a"},
			expectedError:   "--enable-webhook must be false when running in namespaced mode (--namespace)",
		},
		{
			name:            "namespaced mode without webhook is allowed",
			watchNamespaces: []string{"team-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWebhookConfiguration(tt.enableWebhook, tt.watchNamespaces)
			if tt.expectedError == "" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, tt.expectedError)
		})
	}
}

func TestValidateLeaderElectionNamespace(t *testing.T) {
	require.NoError(t, validateLeaderElectionNamespace(nil))
	require.EqualError(t, validateLeaderElectionNamespace(rest.ErrNotInCluster),
		"--leader-election-namespace must be set when running in namespaced mode outside a cluster")

	inClusterConfigErr := errors.New("service account token is unavailable")
	err := validateLeaderElectionNamespace(inClusterConfigErr)
	require.ErrorContains(t, err, "check in-cluster configuration for automatic namespace detection")
	assert.ErrorIs(t, err, inClusterConfigErr)
}

func TestParseWatchNamespaces(t *testing.T) {
	tests := []struct {
		name          string
		flag          string
		env           string
		expected      []string
		expectedError string
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
		{
			name:          "rejects flag with no namespaces",
			flag:          " , , ",
			env:           "team-a",
			expectedError: "--namespace must contain at least one non-empty namespace",
		},
		{
			name:          "rejects environment variable with no namespaces",
			env:           " , , ",
			expectedError: "WATCH_NAMESPACE must contain at least one non-empty namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Always set WATCH_NAMESPACE so the result does not depend on the
			// environment inherited from the test runner.
			t.Setenv("WATCH_NAMESPACE", tt.env)
			actual, err := parseWatchNamespaces(tt.flag)
			if tt.expectedError != "" {
				require.EqualError(t, err, tt.expectedError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual)
		})
	}
}
