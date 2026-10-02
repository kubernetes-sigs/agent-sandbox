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

package agtsbx

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookPathFor returns a lookPathFunc that reports only the named executables
// as installed.
func lookPathFor(available ...string) lookPathFunc {
	return func(file string) (string, error) {
		if slices.Contains(available, file) {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
}

func TestSelectRuntime(t *testing.T) {
	testCases := []struct {
		name      string
		requested string
		available []string
		want      string
		wantErr   string
	}{
		{
			name:      "auto prefers docker when both engines are present",
			requested: RuntimeAuto,
			available: []string{"docker", "podman"},
			want:      RuntimeDocker,
		},
		{
			name:      "auto falls through to podman when docker is missing",
			requested: RuntimeAuto,
			available: []string{"podman"},
			want:      RuntimePodman,
		},
		{
			// Kubernetes is the terminal fallback, so auto always resolves.
			name:      "auto falls back to kubernetes with no container engine",
			requested: RuntimeAuto,
			available: nil,
			want:      RuntimeKubernetes,
		},
		{
			name:      "explicit docker is honoured",
			requested: RuntimeDocker,
			available: []string{"docker", "podman"},
			want:      RuntimeDocker,
		},
		{
			// An explicit choice is never silently downgraded: the error names
			// what the user actually asked for.
			name:      "explicit podman fails when podman is missing",
			requested: RuntimePodman,
			available: []string{"docker"},
			wantErr:   `runtime "podman" selected but the "podman" executable was not found`,
		},
		{
			name:      "explicit kubernetes needs no executable",
			requested: RuntimeKubernetes,
			available: nil,
			want:      RuntimeKubernetes,
		},
		{
			name:      "unknown runtime is rejected",
			requested: "firecracker",
			available: []string{"docker"},
			wantErr:   `unknown runtime "firecracker"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectRuntime(tc.requested, lookPathFor(tc.available...))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestIsKnownRuntime(t *testing.T) {
	for _, name := range knownRuntimes {
		assert.True(t, isKnownRuntime(name), "expected %q to be a known runtime", name)
	}
	assert.False(t, isKnownRuntime("gvisor"))
	assert.False(t, isKnownRuntime(""))
}

func TestNewBackendRejectsUnknownRuntime(t *testing.T) {
	_, err := newBackend("firecracker", backendOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown runtime "firecracker"`)
}

func TestNewBackendBuildsContainerBackends(t *testing.T) {
	for _, engine := range []string{RuntimeDocker, RuntimePodman} {
		backend, err := newBackend(engine, backendOptions{})
		require.NoError(t, err)
		// The backend name doubles as the executable name; a mismatch would
		// silently invoke the wrong engine.
		assert.Equal(t, engine, backend.Name())
	}
}
