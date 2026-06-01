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

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

func TestExecute_Success(t *testing.T) {
	server := NewProcessServer()
	ctx := context.Background()

	req := &pb.ExecuteRequest{
		Command: []string{"echo", "hello world"},
	}

	resp, err := server.Execute(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, int32(0), resp.ExitCode)
	assert.Equal(t, "hello world\n", resp.Stdout)
	assert.Equal(t, "", resp.Stderr)
}

func TestExecute_Failure(t *testing.T) {
	server := NewProcessServer()
	ctx := context.Background()

	// Command that outputs to stderr and exits with non-zero
	req := &pb.ExecuteRequest{
		Command: []string{"sh", "-c", "echo 'error message' >&2; exit 42"},
	}

	resp, err := server.Execute(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, int32(42), resp.ExitCode)
	assert.Equal(t, "", resp.Stdout)
	assert.Equal(t, "error message\n", resp.Stderr)
}

func TestExecute_EnvVars(t *testing.T) {
	server := NewProcessServer()
	ctx := context.Background()

	req := &pb.ExecuteRequest{
		Command: []string{"sh", "-c", "echo $MY_VAR"},
		EnvVars: map[string]string{
			"MY_VAR": "sandbox-rocks",
		},
	}

	resp, err := server.Execute(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, int32(0), resp.ExitCode)
	assert.Equal(t, "sandbox-rocks\n", resp.Stdout)
}
