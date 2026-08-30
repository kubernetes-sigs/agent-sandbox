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
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRunArgs(t *testing.T) {
	t.Run("splits the image from the command", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"sandboxd:latest", "echo", "hello"}, io.Discard)
		require.NoError(t, err)

		assert.Equal(t, "sandboxd:latest", opts.Image)
		assert.Equal(t, []string{"echo", "hello"}, opts.Command)
	})

	t.Run("passes flag-like command arguments through untouched", func(t *testing.T) {
		// FlagSet.Parse stops at the image, so -l and --color are the
		// sandboxed command's, not agtsbx's.
		opts, err := parseRunArgs([]string{"img", "ls", "-l", "--color=always"}, io.Discard)
		require.NoError(t, err)

		assert.Equal(t, []string{"ls", "-l", "--color=always"}, opts.Command)
	})

	t.Run("does not consume flags that appear after the image", func(t *testing.T) {
		// --runtime after the image belongs to the sandboxed command.
		opts, err := parseRunArgs([]string{"img", "mytool", "--runtime", "podman"}, io.Discard)
		require.NoError(t, err)

		assert.Equal(t, RuntimeAuto, opts.Runtime)
		assert.Equal(t, []string{"mytool", "--runtime", "podman"}, opts.Command)
	})

	t.Run("applies defaults", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"img", "true"}, io.Discard)
		require.NoError(t, err)

		assert.Equal(t, RuntimeAuto, opts.Runtime)
		assert.Equal(t, "default", opts.Namespace)
		assert.Equal(t, defaultGRPCPort, opts.GRPCPort)
		assert.Equal(t, defaultRESTPort, opts.RESTPort)
		assert.Equal(t, defaultReadyTimeout, opts.Timeout)
		// Sandboxes are ephemeral unless --keep is given.
		assert.False(t, opts.Keep)
	})

	t.Run("generates a unique prefixed name when none is given", func(t *testing.T) {
		first, err := parseRunArgs([]string{"img", "true"}, io.Discard)
		require.NoError(t, err)
		second, err := parseRunArgs([]string{"img", "true"}, io.Discard)
		require.NoError(t, err)

		// Concurrent runs must not collide on a container or object name.
		assert.True(t, strings.HasPrefix(first.Name, generatedNamePrefix))
		assert.NotEqual(t, first.Name, second.Name)
	})

	t.Run("honours an explicit name", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"--name", "mybox", "img", "true"}, io.Discard)
		require.NoError(t, err)
		assert.Equal(t, "mybox", opts.Name)
	})

	t.Run("collects repeated env flags in both spellings", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"-e", "A=1", "--env", "B=2", "img", "true"}, io.Discard)
		require.NoError(t, err)
		assert.Equal(t, []string{"A=1", "B=2"}, opts.Env)
	})

	t.Run("accepts short and long spellings of the other flags", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"-w", "/tmp", "-n", "agents", "-q", "img", "true"}, io.Discard)
		require.NoError(t, err)

		assert.Equal(t, "/tmp", opts.Workdir)
		assert.Equal(t, "agents", opts.Namespace)
		assert.True(t, opts.Quiet)
	})

	t.Run("parses a custom timeout and ports", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"--timeout", "45s", "--grpc-port", "1234", "--rest-port", "5678", "img", "true"}, io.Discard)
		require.NoError(t, err)

		assert.Equal(t, 45*time.Second, opts.Timeout)
		assert.Equal(t, 1234, opts.GRPCPort)
		assert.Equal(t, 5678, opts.RESTPort)
	})
}

func TestParseRunArgsErrors(t *testing.T) {
	testCases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "no arguments",
			args:    nil,
			wantErr: "an IMAGE is required",
		},
		{
			// Running the image alone would start sandboxd and appear to hang.
			name:    "image without a command",
			args:    []string{"sandboxd:latest"},
			wantErr: "a COMMAND is required",
		},
		{
			name:    "unknown runtime",
			args:    []string{"--runtime", "firecracker", "img", "true"},
			wantErr: `unknown runtime "firecracker"`,
		},
		{
			name:    "env forwarding an unset variable",
			args:    []string{"-e", "AGTSBX_DEFINITELY_UNSET", "img", "true"},
			wantErr: "not set in the environment",
		},
		{
			name:    "env with an empty key",
			args:    []string{"-e", "=value", "img", "true"},
			wantErr: "KEY=VALUE",
		},
		{
			name:    "non-positive timeout",
			args:    []string{"--timeout", "0s", "img", "true"},
			wantErr: "--timeout must be positive",
		},
		{
			name:    "port out of range",
			args:    []string{"--grpc-port", "70000", "img", "true"},
			wantErr: "--grpc-port must be between 1 and 65535",
		},
		{
			name:    "zero port",
			args:    []string{"--rest-port", "0", "img", "true"},
			wantErr: "--rest-port must be between 1 and 65535",
		},
		{
			// Worth catching before the engine reports it obscurely.
			name:    "identical sandboxd ports",
			args:    []string{"--grpc-port", "8080", "--rest-port", "8080", "img", "true"},
			wantErr: "must differ",
		},
		{
			name:    "unknown flag",
			args:    []string{"--nope", "img", "true"},
			wantErr: "usage",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRunArgs(tc.args, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			// Classified as usage so the CLI prints help, not a bare message.
			assert.ErrorIs(t, err, ErrUsage)
		})
	}
}

func TestEnvFlag(t *testing.T) {
	var env envFlag

	require.NoError(t, env.Set("A=1"))
	require.NoError(t, env.Set("B=with=equals"))
	assert.Equal(t, "A=1,B=with=equals", env.String())

	require.Error(t, env.Set("=emptykey"))
}

func TestEnvFlagForwardsFromTheEnvironment(t *testing.T) {
	// Passing a credential as "-e KEY" keeps it out of the command line and
	// the shell history, so the pass-through must actually resolve a value.
	t.Setenv("AGTSBX_TEST_TOKEN", "s3cret")

	var env envFlag
	require.NoError(t, env.Set("AGTSBX_TEST_TOKEN"))
	assert.Equal(t, envFlag{"AGTSBX_TEST_TOKEN=s3cret"}, env)

	// An unset variable must be reported rather than silently forwarded as
	// empty, which would leave the sandboxed agent unauthenticated.
	err := env.Set("AGTSBX_TEST_ABSENT")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not set in the environment")
}

func TestRunPrintsHelpOnRequest(t *testing.T) {
	var stdout, stderr strings.Builder

	code, err := Run(t.Context(), []string{"--help"}, IO{Stdout: &stdout, Stderr: &stderr})
	require.NoError(t, err)
	// Asking for help is not an error: the text goes to stdout and the exit
	// status is success.
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "Usage: agtsbx run")
	assert.Empty(t, stderr.String())
}

func TestRunRejectsBadUsage(t *testing.T) {
	var stdout, stderr strings.Builder

	code, err := Run(t.Context(), []string{"--runtime", "nope", "img", "true"}, IO{Stdout: &stdout, Stderr: &stderr})
	require.Error(t, err)
	assert.Equal(t, 1, code)
	require.ErrorIs(t, err, ErrUsage)
	// Guidance on stderr keeps stdout clean for the command's own output.
	assert.Contains(t, stderr.String(), "Usage: agtsbx run")
	assert.Empty(t, stdout.String())
}
